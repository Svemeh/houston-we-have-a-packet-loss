package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	satellite "github.com/joshuaferrara/go-satellite"
)

// Where the dish sits. Comes from the environment or the dashboard, so a real
// position never lands in the repo. Altitude is deliberately absent: at 550 km
// slant range a couple of hundred metres moves a look angle by ~0.02°, well
// under the 0.1° we round to on the wire.
type Observer struct {
	LatitudeDeg  float64 `json:"latitude_deg"`
	LongitudeDeg float64 `json:"longitude_deg"`
}

func (observer Observer) Validate() error {
	if math.IsNaN(observer.LatitudeDeg) || math.IsNaN(observer.LongitudeDeg) ||
		observer.LatitudeDeg < -90 || observer.LatitudeDeg > 90 ||
		observer.LongitudeDeg < -180 || observer.LongitudeDeg > 180 {
		return fmt.Errorf("position out of range: %.4f, %.4f", observer.LatitudeDeg, observer.LongitudeDeg)
	}
	return nil
}

const (
	envLatitude  = "HOUSTON_LAT"
	envLongitude = "HOUSTON_LON"
	envFilePath  = ".env"

	// Where a location came from. The environment wins over the dashboard, so
	// the dashboard can't change one that was set there.
	LocationFromEnv      = "env"
	LocationFromSettings = "settings"
)

// LoadObserver finds the dish's position: real environment variables first,
// then .env, then the location saved from the dashboard. source is "" when
// none of them has one; the app runs anyway and the dashboard asks for it.
// Coordinates that are set but wrong are still an error.
//
// The dish knows its own position, but refuses to hand it over unless
// location_request_mode is set to LOCAL — a config write that needs SpaceX's
// signing credentials. So the user has to supply it.
func LoadObserver(settingsPath string) (observer Observer, source string, err error) {
	loadEnvFile(envFilePath)

	latitude, haveLatitude, err := optionalFloat(envLatitude)
	if err != nil {
		return Observer{}, "", err
	}
	longitude, haveLongitude, err := optionalFloat(envLongitude)
	if err != nil {
		return Observer{}, "", err
	}
	switch {
	case haveLatitude && haveLongitude:
		observer = Observer{LatitudeDeg: latitude, LongitudeDeg: longitude}
		if err := observer.Validate(); err != nil {
			return Observer{}, "", fmt.Errorf("%s/%s: %w", envLatitude, envLongitude, err)
		}
		return observer, LocationFromEnv, nil
	case haveLatitude || haveLongitude:
		return Observer{}, "", fmt.Errorf("set both %s and %s, or neither", envLatitude, envLongitude)
	}

	settings, err := LoadSettings(settingsPath)
	if err != nil {
		log.Printf("sky: ignoring %s: %v", settingsPath, err)
		return Observer{}, "", nil
	}
	if settings.Location == nil {
		return Observer{}, "", nil
	}
	if err := settings.Location.Validate(); err != nil {
		log.Printf("sky: ignoring location in %s: %v", settingsPath, err)
		return Observer{}, "", nil
	}
	return *settings.Location, LocationFromSettings, nil
}

// optionalFloat reads a number from the environment; found is false when the
// variable is unset or empty.
func optionalFloat(key string) (value float64, found bool, err error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, false, nil
	}
	value, err = strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false, fmt.Errorf("%s=%q is not a number", key, raw)
	}
	return value, true, nil
}

// loadEnvFile copies KEY=value lines into the process environment without
// overwriting anything already set there, so a real env var always wins.
func loadEnvFile(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, alreadySet := os.LookupEnv(key); !alreadySet {
			os.Setenv(key, value)
		}
	}
}

// SkyObject is one satellite as seen from the dish. Angles are degrees:
// azimuth clockwise from true north, elevation up from the horizon.
type SkyObject struct {
	Name         string  `json:"name"`
	AzimuthDeg   float64 `json:"az"`
	ElevationDeg float64 `json:"el"`
	RangeKm      float64 `json:"range_km"`
	InCone       bool    `json:"in_cone"`
}

// SkySnapshot is everything above the horizon at one instant, plus enough
// context for the client to say honestly how much of it is guesswork.
type SkySnapshot struct {
	Timestamp time.Time `json:"timestamp"`
	// LocationSet is false until a position is known; nothing is propagated
	// before then, and the sky view asks for one.
	LocationSet      bool        `json:"location_set"`
	Objects          []SkyObject `json:"objects"`
	InConeCount      int         `json:"in_cone_count"`
	TrackedCount     int         `json:"tracked_count"`      // elements that can reach our latitude
	CatalogCount     int         `json:"catalog_count"`      // elements in the last fetch
	ElementsAgeS     int64       `json:"elements_age_s"`     // -1 when we have none
	ElementsAreStale bool        `json:"elements_are_stale"` // older than TLEStaleAfter
	ConeHalfAngleDeg float64     `json:"cone_half_angle_deg"`
	ConeAzimuthDeg   float64     `json:"cone_azimuth_deg"`
	ConeElevationDeg float64     `json:"cone_elevation_deg"`
	ServiceFloorDeg  float64     `json:"service_floor_deg"`
	// ConeIsAssumed is true while we're pointing the cone at zenith because the
	// dish hasn't told us where it's actually aimed.
	ConeIsAssumed bool `json:"cone_is_assumed"`
}

type trackedSatellite struct {
	name       string
	noradID    int
	propagator satellite.Satellite
}

// SkyTracker holds the current element set, propagates it on a tick, and fans
// each snapshot out to connected browsers.
type SkyTracker struct {
	httpClient *http.Client

	mutex             sync.RWMutex
	observer          Observer
	locationSource    string // "" until a position is known
	elements          []trackedSatellite
	catalogCount      int
	elementsFetchedAt time.Time
	subscribers       map[chan SkySnapshot]struct{}

	// Boresight as last reported by the dish. Written by pollLoop, read on
	// every tick. haveBoresight stays false until the first successful poll,
	// and on firmware too old to report it.
	coneAzimuthDeg   float64
	coneElevationDeg float64
	haveBoresight    bool
}

func NewSkyTracker(observer Observer, locationSource string) *SkyTracker {
	return &SkyTracker{
		observer:       observer,
		locationSource: locationSource,
		httpClient:     &http.Client{Timeout: TLEFetchTimeout},
		subscribers:    make(map[chan SkySnapshot]struct{}),
		// Until the dish reports in, aim straight up and say so.
		coneElevationDeg: 90,
	}
}

// SetBoresight records where the dish says it is aimed. Called from pollLoop
// on every successful poll; the values only change if the dish is physically
// moved, so this is almost always a no-op write.
func (tracker *SkyTracker) SetBoresight(azimuthDeg, elevationDeg float64) {
	tracker.mutex.Lock()
	defer tracker.mutex.Unlock()
	tracker.coneAzimuthDeg = azimuthDeg
	tracker.coneElevationDeg = elevationDeg
	tracker.haveBoresight = true
}

// Location returns the current position and where it came from; source is ""
// when there is none yet.
func (tracker *SkyTracker) Location() (observer Observer, source string) {
	tracker.mutex.RLock()
	defer tracker.mutex.RUnlock()
	return tracker.observer, tracker.locationSource
}

// SetObserver moves the observer, then re-filters the cached elements for the
// new latitude so the sky view updates without waiting for a download.
func (tracker *SkyTracker) SetObserver(observer Observer, source string) {
	tracker.mutex.Lock()
	tracker.observer = observer
	tracker.locationSource = source
	tracker.mutex.Unlock()

	if err := tracker.loadCachedElements(); err != nil {
		log.Printf("sky: no usable element cache yet (%v)", err)
	}
}

// filterLatitude is the latitude the element prefilter works against. With no
// location yet it's the equator, which keeps every satellite.
func (tracker *SkyTracker) filterLatitude() float64 {
	tracker.mutex.RLock()
	defer tracker.mutex.RUnlock()
	return tracker.observer.LatitudeDeg
}

// angularSeparation is the great-circle angle between two directions in the
// sky, in degrees — elevation plays the part of latitude, azimuth longitude.
func angularSeparation(azimuthA, elevationA, azimuthB, elevationB float64) float64 {
	elevationARad := elevationA * satellite.DEG2RAD
	elevationBRad := elevationB * satellite.DEG2RAD
	azimuthDeltaRad := (azimuthA - azimuthB) * satellite.DEG2RAD
	cosSeparation := math.Sin(elevationARad)*math.Sin(elevationBRad) +
		math.Cos(elevationARad)*math.Cos(elevationBRad)*math.Cos(azimuthDeltaRad)
	return math.Acos(math.Max(-1, math.Min(1, cosSeparation))) * satellite.RAD2DEG
}

// Run drives both the element refresh and the propagation tick until ctx ends.
func (tracker *SkyTracker) Run(ctx context.Context) {
	go tracker.refreshLoop(ctx)

	ticker := time.NewTicker(SkyTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !tracker.hasSubscribers() {
				continue
			}
			tracker.broadcast(tracker.Snapshot(time.Now()))
		}
	}
}

// refreshLoop keeps the element set current without annoying Celestrak. They
// publish every couple of hours and serve each IP one copy per cycle, so we
// seed from the on-disk cache first and only go to the network once what we
// have is genuinely old.
func (tracker *SkyTracker) refreshLoop(ctx context.Context) {
	if err := tracker.loadCachedElements(); err != nil {
		log.Printf("sky: no usable element cache (%v)", err)
	}

	for {
		waitBeforeNext := TLERefreshRetryInterval
		if age := tracker.elementsAge(); age >= 0 && age < TLERefreshInterval {
			waitBeforeNext = TLERefreshInterval - age // still fresh, don't ask
		} else if err := tracker.RefreshElements(ctx); err != nil {
			log.Printf("sky: element fetch failed: %v", err)
		} else {
			waitBeforeNext = TLERefreshInterval
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(waitBeforeNext):
		}
	}
}

// loadCachedElements seeds from the last downloaded copy, so a restart costs
// no network at all. Celestrak will not serve a second copy within an update
// cycle, which makes this mandatory rather than an optimisation.
func (tracker *SkyTracker) loadCachedElements() error {
	file, err := os.Open(ElementCachePath)
	if err != nil {
		return err
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return err
	}
	tracked, catalogCount, err := parseElementStream(file, tracker.filterLatitude())
	if err != nil {
		return err
	}
	if len(tracked) == 0 {
		return fmt.Errorf("cache holds no usable elements")
	}

	tracker.storeElements(tracked, catalogCount, fileInfo.ModTime())
	log.Printf("sky: seeded %d satellites from %s (%s old)", len(tracked), ElementCachePath,
		time.Since(fileInfo.ModTime()).Round(time.Minute))
	return nil
}

// RefreshElements pulls the current Starlink element set, writes it to the
// cache, and keeps only the satellites whose orbits can reach our latitude.
// On failure the previous set is left in place — stale elements beat none.
func (tracker *SkyTracker) RefreshElements(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, CelestrakStarlinkURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", CelestrakUserAgent)
	request.Header.Set("Accept", "*/*")

	response, err := tracker.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	// Celestrak answers 403 with a plain-text note when we already hold the
	// current set. That's a courtesy, not a failure: keep what we have and
	// treat the cache as re-validated so we back off a full cycle.
	if response.StatusCode == http.StatusForbidden {
		note, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		if strings.Contains(string(note), "has not updated") {
			if tracker.elementsAge() < 0 {
				return fmt.Errorf("celestrak has nothing new and no elements are loaded")
			}
			log.Print("sky: elements already current, nothing to download")
			tracker.markElementsRevalidated()
			return nil
		}
		return fmt.Errorf("celestrak refused: %s", strings.TrimSpace(string(note)))
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("celestrak returned %s", response.Status)
	}

	rawElements, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	latitude := tracker.filterLatitude()
	tracked, catalogCount, err := parseElementStream(bytes.NewReader(rawElements), latitude)
	if err != nil {
		return err
	}
	if len(tracked) == 0 {
		return fmt.Errorf("no usable elements in response (%d parsed)", catalogCount)
	}

	// Only cache what parsed, so a truncated download can't poison the restart.
	if err := os.MkdirAll(filepath.Dir(ElementCachePath), 0o755); err != nil {
		log.Printf("sky: could not create %s: %v", filepath.Dir(ElementCachePath), err)
	} else if err := os.WriteFile(ElementCachePath, rawElements, 0o644); err != nil {
		log.Printf("sky: could not write %s: %v", ElementCachePath, err)
	}
	tracker.storeElements(tracked, catalogCount, time.Now())

	log.Printf("sky: %d of %d satellites can reach %.2f° latitude", len(tracked), catalogCount, math.Abs(latitude))
	return nil
}

func (tracker *SkyTracker) storeElements(tracked []trackedSatellite, catalogCount int, fetchedAt time.Time) {
	tracker.mutex.Lock()
	defer tracker.mutex.Unlock()
	tracker.elements = tracked
	tracker.catalogCount = catalogCount
	tracker.elementsFetchedAt = fetchedAt
}

// markElementsRevalidated resets the clock after Celestrak confirms our copy is
// still the current one, so we wait a full interval before asking again.
func (tracker *SkyTracker) markElementsRevalidated() {
	tracker.mutex.Lock()
	defer tracker.mutex.Unlock()
	if len(tracker.elements) == 0 {
		return // nothing to revalidate; keep retrying on the short interval
	}
	tracker.elementsFetchedAt = time.Now()
}

// elementsAge returns how old the current set is, or -1 when there isn't one.
func (tracker *SkyTracker) elementsAge() time.Duration {
	tracker.mutex.RLock()
	defer tracker.mutex.RUnlock()
	if tracker.elementsFetchedAt.IsZero() {
		return -1
	}
	return time.Since(tracker.elementsFetchedAt)
}

type ommRecord struct {
	ObjectName      string  `json:"OBJECT_NAME"`
	NoradCatID      int     `json:"NORAD_CAT_ID"`
	Epoch           string  `json:"EPOCH"`
	MeanMotion      float64 `json:"MEAN_MOTION"`
	Eccentricity    float64 `json:"ECCENTRICITY"`
	Inclination     float64 `json:"INCLINATION"`
	RAOfAscNode     float64 `json:"RA_OF_ASC_NODE"`
	ArgOfPericenter float64 `json:"ARG_OF_PERICENTER"`
	MeanAnomaly     float64 `json:"MEAN_ANOMALY"`
	BStar           float64 `json:"BSTAR"`
	MeanMotionDot   float64 `json:"MEAN_MOTION_DOT"`
	MeanMotionDDot  float64 `json:"MEAN_MOTION_DDOT"`
}

// parseElementStream reads Celestrak's OMM JSON, dropping satellites whose
// inclination puts them permanently below our horizon. Returns the kept set
// and how many records were seen in total.
func parseElementStream(reader io.Reader, observerLatitudeDeg float64) ([]trackedSatellite, int, error) {
	var records []ommRecord
	if err := json.NewDecoder(reader).Decode(&records); err != nil {
		return nil, 0, err
	}

	var tracked []trackedSatellite
	for _, record := range records {
		if !canReachLatitude(record.Inclination, observerLatitudeDeg) {
			continue
		}
		line1, line2, err := ommToTLE(record)
		if err != nil {
			continue
		}
		propagator := satellite.TLEToSat(line1, line2, satellite.GravityWGS84)
		if propagator.Error == 0 {
			tracked = append(tracked, trackedSatellite{name: record.ObjectName, noradID: record.NoradCatID, propagator: propagator})
		}
	}
	return tracked, len(records), nil
}

// ommToTLE formats OMM elements as TLE lines for go-satellite, which only
// takes TLEs. The catalog number is 00000 since SGP4 doesn't use it and real
// IDs no longer fit in five digits. Values that wouldn't fit a TLE column are
// rejected, because go-satellite exits the process on an unparsable field.
func ommToTLE(record ommRecord) (line1, line2 string, err error) {
	epoch, err := time.Parse("2006-01-02T15:04:05.999999", record.Epoch)
	if err != nil {
		return "", "", err
	}
	for _, value := range []float64{record.MeanMotion, record.Eccentricity, record.Inclination,
		record.RAOfAscNode, record.ArgOfPericenter, record.MeanAnomaly,
		record.BStar, record.MeanMotionDot, record.MeanMotionDDot} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return "", "", fmt.Errorf("non-finite element")
		}
	}
	eccentricityDigits := int(math.Round(record.Eccentricity * 1e7))
	ndot := strings.Replace(fmt.Sprintf("%+.8f", record.MeanMotionDot), "0.", ".", 1)
	ndot = strings.Replace(ndot, "+", " ", 1)
	if record.MeanMotion <= 0 || record.MeanMotion >= 100 ||
		eccentricityDigits < 0 || eccentricityDigits > 9999999 ||
		record.Inclination < 0 || record.Inclination > 180 || len(ndot) != 10 {
		return "", "", fmt.Errorf("element out of TLE range")
	}
	nddot, err := tleExponent(record.MeanMotionDDot)
	if err != nil {
		return "", "", err
	}
	bstar, err := tleExponent(record.BStar)
	if err != nil {
		return "", "", err
	}

	hour, minute, second := epoch.Clock()
	secondOfDay := float64(hour*3600+minute*60+second) + float64(epoch.Nanosecond())/1e9
	epochDays := float64(epoch.YearDay()) + secondOfDay/86400

	line1 = fmt.Sprintf("1 00000U          %02d%012.8f %s %s %s 0  999",
		epoch.Year()%100, epochDays, ndot, nddot, bstar)
	line2 = fmt.Sprintf("2 00000 %8.4f %8.4f %07d %8.4f %8.4f %11.8f    0",
		record.Inclination, normaliseDegrees(record.RAOfAscNode),
		eccentricityDigits,
		normaliseDegrees(record.ArgOfPericenter), normaliseDegrees(record.MeanAnomaly),
		record.MeanMotion)
	return line1 + tleChecksum(line1), line2 + tleChecksum(line2), nil
}

// tleExponent writes a value in the TLE's implied-decimal form, e.g. -0.00012345 as "-12345-3".
func tleExponent(value float64) (string, error) {
	sign := " "
	if value < 0 {
		sign = "-"
	}
	magnitude := math.Abs(value)
	if magnitude < 1e-10 {
		return " 00000+0", nil
	}
	exponent := int(math.Floor(math.Log10(magnitude))) + 1
	mantissa := int(math.Round(magnitude / math.Pow(10, float64(exponent)) * 1e5))
	if mantissa >= 100000 {
		mantissa /= 10
		exponent++
	}
	if exponent > 9 {
		return "", fmt.Errorf("%g too large for a TLE field", value)
	}
	if exponent < -9 {
		return " 00000+0", nil
	}
	return fmt.Sprintf("%s%05d%+d", sign, mantissa, exponent), nil
}

func normaliseDegrees(value float64) float64 {
	value = math.Mod(value, 360)
	if value < 0 {
		value += 360
	}
	return value
}

func tleChecksum(line string) string {
	sum := 0
	for _, char := range line {
		switch {
		case char >= '0' && char <= '9':
			sum += int(char - '0')
		case char == '-':
			sum++
		}
	}
	return strconv.Itoa(sum % 10)
}

// canReachLatitude is the cheap prefilter. A satellite's ground track never
// exceeds its inclination, so anything that can't get within one horizon
// radius of us is dead weight. At 60°N this discards nothing, since Starlink's
// lowest shell is 43°; it earns its keep further north.
func canReachLatitude(inclinationDeg, observerLatitudeDeg float64) bool {
	maxGroundLatitude := inclinationDeg
	if maxGroundLatitude > 90 {
		maxGroundLatitude = 180 - maxGroundLatitude // retrograde, e.g. 97.6° SSO
	}
	return maxGroundLatitude+HorizonAngularRadiusDeg >= math.Abs(observerLatitudeDeg)
}

// Snapshot propagates every tracked satellite to now and keeps the ones above
// the horizon, highest elevation first.
func (tracker *SkyTracker) Snapshot(now time.Time) SkySnapshot {
	tracker.mutex.RLock()
	elements := tracker.elements
	catalogCount := tracker.catalogCount
	fetchedAt := tracker.elementsFetchedAt
	coneAzimuthDeg := tracker.coneAzimuthDeg
	coneElevationDeg := tracker.coneElevationDeg
	haveBoresight := tracker.haveBoresight
	observer := tracker.observer
	locationSet := tracker.locationSource != ""
	tracker.mutex.RUnlock()

	snapshot := SkySnapshot{
		Timestamp:        now,
		LocationSet:      locationSet,
		Objects:          []SkyObject{},
		TrackedCount:     len(elements),
		CatalogCount:     catalogCount,
		ElementsAgeS:     -1,
		ConeHalfAngleDeg: DefaultConeHalfAngleDeg,
		ConeAzimuthDeg:   coneAzimuthDeg,
		ConeElevationDeg: coneElevationDeg,
		ServiceFloorDeg:  ServiceFloorElevationDeg,
		ConeIsAssumed:    !haveBoresight,
	}
	if !fetchedAt.IsZero() {
		elementAge := now.Sub(fetchedAt)
		snapshot.ElementsAgeS = int64(elementAge.Seconds())
		snapshot.ElementsAreStale = elementAge > TLEStaleAfter
	}
	if len(elements) == 0 || !locationSet {
		return snapshot
	}

	// SGP4 wants UTC calendar components, not a time.Time.
	utc := now.UTC()
	year, month, day := utc.Date()
	hour, minute, second := utc.Clock()
	julianDay := satellite.JDay(year, int(month), day, hour, minute, second)

	for _, tracked := range elements {
		position, _ := satellite.Propagate(tracked.propagator, year, int(month), day, hour, minute, second)
		if math.IsNaN(position.X) || math.IsNaN(position.Y) || math.IsNaN(position.Z) {
			continue // decayed or otherwise unpropagatable element
		}
		azimuthDeg, elevationDeg, rangeKm := lookFrom(position, observer, julianDay)
		if elevationDeg < 0 {
			continue // below the horizon, which is most of them
		}

		// Inside the dish's field of view, and high enough for it to bother.
		inCone := elevationDeg >= ServiceFloorElevationDeg &&
			angularSeparation(azimuthDeg, elevationDeg, coneAzimuthDeg, coneElevationDeg) <= DefaultConeHalfAngleDeg
		if inCone {
			snapshot.InConeCount++
		}
		snapshot.Objects = append(snapshot.Objects, SkyObject{
			Name:         tracked.name,
			AzimuthDeg:   roundTo(azimuthDeg, 1),
			ElevationDeg: roundTo(elevationDeg, 1),
			RangeKm:      roundTo(rangeKm, 0),
			InCone:       inCone,
		})
	}

	sort.Slice(snapshot.Objects, func(i, j int) bool {
		return snapshot.Objects[i].ElevationDeg > snapshot.Objects[j].ElevationDeg
	})
	return snapshot
}

const (
	wgs84EquatorialRadiusKm  = 6378.137
	wgs84EccentricitySquared = 0.00669437999014
)

// lookFrom gives azimuth, elevation and range from an observer on the WGS84 ellipsoid (go-satellite's ECIToLookAngles uses a sphere).
func lookFrom(position satellite.Vector3, observer Observer, julianDay float64) (azimuthDeg, elevationDeg, rangeKm float64) {
	siderealAngle := satellite.ThetaG_JD(julianDay)
	x := position.X*math.Cos(siderealAngle) + position.Y*math.Sin(siderealAngle)
	y := -position.X*math.Sin(siderealAngle) + position.Y*math.Cos(siderealAngle)
	z := position.Z

	latitude := observer.LatitudeDeg * satellite.DEG2RAD
	longitude := observer.LongitudeDeg * satellite.DEG2RAD
	sinLat, cosLat := math.Sin(latitude), math.Cos(latitude)
	sinLon, cosLon := math.Sin(longitude), math.Cos(longitude)
	primeVertical := wgs84EquatorialRadiusKm / math.Sqrt(1-wgs84EccentricitySquared*sinLat*sinLat)

	dx := x - primeVertical*cosLat*cosLon
	dy := y - primeVertical*cosLat*sinLon
	dz := z - primeVertical*(1-wgs84EccentricitySquared)*sinLat
	rangeKm = math.Sqrt(dx*dx + dy*dy + dz*dz)

	east := -sinLon*dx + cosLon*dy
	north := -sinLat*cosLon*dx - sinLat*sinLon*dy + cosLat*dz
	up := cosLat*cosLon*dx + cosLat*sinLon*dy + sinLat*dz
	azimuthDeg = math.Mod(math.Atan2(east, north)*satellite.RAD2DEG+360, 360)
	elevationDeg = math.Asin(up/rangeKm) * satellite.RAD2DEG
	return azimuthDeg, elevationDeg, rangeKm
}

// roundTo trims wire noise: 0.1° is a quarter of a pixel on an all-sky plot,
// and full float64 precision would roughly double the payload.
func roundTo(value float64, decimals int) float64 {
	scale := math.Pow(10, float64(decimals))
	return math.Round(value*scale) / scale
}

func (tracker *SkyTracker) hasSubscribers() bool {
	tracker.mutex.RLock()
	defer tracker.mutex.RUnlock()
	return len(tracker.subscribers) > 0
}

func (tracker *SkyTracker) broadcast(snapshot SkySnapshot) {
	tracker.mutex.RLock()
	currentSubscribers := make([]chan SkySnapshot, 0, len(tracker.subscribers))
	for subscriber := range tracker.subscribers {
		currentSubscribers = append(currentSubscribers, subscriber)
	}
	tracker.mutex.RUnlock()

	for _, subscriber := range currentSubscribers {
		select {
		case subscriber <- snapshot:
		default: // a browser that can't keep up misses this tick, not the next
		}
	}
}

// Subscribe hands back a snapshot stream and the function to detach. There is
// no backfill: the next tick is at most SkyTickInterval away.
func (tracker *SkyTracker) Subscribe() (stream <-chan SkySnapshot, unsubscribe func()) {
	tracker.mutex.Lock()
	defer tracker.mutex.Unlock()

	subscriber := make(chan SkySnapshot, skySubscriberQueueDepth)
	tracker.subscribers[subscriber] = struct{}{}

	return subscriber, func() {
		tracker.mutex.Lock()
		if _, stillSubscribed := tracker.subscribers[subscriber]; stillSubscribed {
			delete(tracker.subscribers, subscriber)
			close(subscriber)
		}
		tracker.mutex.Unlock()
	}
}
