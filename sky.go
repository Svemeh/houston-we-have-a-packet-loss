package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	satellite "github.com/joshuaferrara/go-satellite"
)

// Where the dish sits. Read from the environment so a real position never
// lands in the repo. Altitude is deliberately absent: at 550 km slant range a
// couple of hundred metres moves a look angle by ~0.02°, well under the 0.1°
// we round to on the wire.
type Observer struct {
	LatitudeDeg  float64
	LongitudeDeg float64
}

const (
	envLatitude  = "HOUSTON_LAT"
	envLongitude = "HOUSTON_LON"
	envFilePath  = ".env"
)

// LoadObserver prefers real environment variables and falls back to .env.
// A missing .env is not an error; missing coordinates are.
//
// The dish knows its own position, but refuses to hand it over unless
// location_request_mode is set to LOCAL — a config write that needs SpaceX's
// signing credentials. So these two values stay manual.
func LoadObserver() (Observer, error) {
	loadEnvFile(envFilePath)

	latitude, err := requiredFloat(envLatitude)
	if err != nil {
		return Observer{}, err
	}
	longitude, err := requiredFloat(envLongitude)
	if err != nil {
		return Observer{}, err
	}
	if latitude < -90 || latitude > 90 || longitude < -180 || longitude > 180 {
		return Observer{}, fmt.Errorf("observer position out of range: %.4f, %.4f", latitude, longitude)
	}

	return Observer{LatitudeDeg: latitude, LongitudeDeg: longitude}, nil
}

func requiredFloat(key string) (float64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, fmt.Errorf("%s is not set (put it in %s)", key, envFilePath)
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a number", key, raw)
	}
	return value, nil
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
	Timestamp        time.Time   `json:"timestamp"`
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
	propagator satellite.Satellite
}

// SkyTracker holds the current element set, propagates it on a tick, and fans
// each snapshot out to connected browsers.
type SkyTracker struct {
	observer   Observer
	httpClient *http.Client

	mutex             sync.RWMutex
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

func NewSkyTracker(observer Observer) *SkyTracker {
	return &SkyTracker{
		observer:    observer,
		httpClient:  &http.Client{Timeout: TLEFetchTimeout},
		subscribers: make(map[chan SkySnapshot]struct{}),
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
	file, err := os.Open(TLECachePath)
	if err != nil {
		return err
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return err
	}
	tracked, catalogCount, err := parseElementStream(file, tracker.observer.LatitudeDeg)
	if err != nil {
		return err
	}
	if len(tracked) == 0 {
		return fmt.Errorf("cache holds no usable elements")
	}

	tracker.storeElements(tracked, catalogCount, fileInfo.ModTime())
	log.Printf("sky: seeded %d satellites from %s (%s old)", len(tracked), TLECachePath,
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
	tracked, catalogCount, err := parseElementStream(bytes.NewReader(rawElements), tracker.observer.LatitudeDeg)
	if err != nil {
		return err
	}
	if len(tracked) == 0 {
		return fmt.Errorf("no usable elements in response (%d parsed)", catalogCount)
	}

	// Only cache what parsed, so a truncated download can't poison the restart.
	if err := os.WriteFile(TLECachePath, rawElements, 0o644); err != nil {
		log.Printf("sky: could not write %s: %v", TLECachePath, err)
	}
	tracker.storeElements(tracked, catalogCount, time.Now())

	log.Printf("sky: %d of %d satellites can reach %.2f° latitude", len(tracked), catalogCount,
		math.Abs(tracker.observer.LatitudeDeg))
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

// parseElementStream reads Celestrak's 3-line TLE format, dropping satellites
// whose inclination puts them permanently below our horizon. Returns the kept
// set and how many records were seen in total.
func parseElementStream(reader io.Reader, observerLatitudeDeg float64) ([]trackedSatellite, int, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, scannerInitialBufferBytes), scannerMaxLineBytes)

	var tracked []trackedSatellite
	catalogCount := 0
	currentName, firstLine := "", ""

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), " \r\t")
		switch {
		case strings.HasPrefix(line, "1 "):
			firstLine = line
		case strings.HasPrefix(line, "2 "):
			catalogCount++
			if firstLine == "" {
				continue
			}
			if canReachLatitude(line, observerLatitudeDeg) {
				propagator := satellite.TLEToSat(firstLine, line, satellite.GravityWGS84)
				if propagator.Error == 0 {
					tracked = append(tracked, trackedSatellite{name: currentName, propagator: propagator})
				}
			}
			currentName, firstLine = "", ""
		default:
			currentName = strings.TrimSpace(line)
		}
	}
	return tracked, catalogCount, scanner.Err()
}

// canReachLatitude is the cheap prefilter. A satellite's ground track never
// exceeds its inclination, so anything that can't get within one horizon
// radius of us is dead weight. At 60°N this discards nothing, since Starlink's
// lowest shell is 43°; it earns its keep further north.
func canReachLatitude(tleLine2 string, observerLatitudeDeg float64) bool {
	if len(tleLine2) < 16 {
		return false
	}
	inclinationDeg, err := strconv.ParseFloat(strings.TrimSpace(tleLine2[8:16]), 64)
	if err != nil {
		return false
	}
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
	tracker.mutex.RUnlock()

	snapshot := SkySnapshot{
		Timestamp:        now,
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
	if len(elements) == 0 {
		return snapshot
	}

	// SGP4 wants UTC calendar components, not a time.Time.
	utc := now.UTC()
	year, month, day := utc.Date()
	hour, minute, second := utc.Clock()
	julianDay := satellite.JDay(year, int(month), day, hour, minute, second)

	observerCoords := satellite.LatLong{
		Latitude:  tracker.observer.LatitudeDeg * satellite.DEG2RAD,
		Longitude: tracker.observer.LongitudeDeg * satellite.DEG2RAD,
	}

	for _, tracked := range elements {
		position, _ := satellite.Propagate(tracked.propagator, year, int(month), day, hour, minute, second)
		if math.IsNaN(position.X) || math.IsNaN(position.Y) || math.IsNaN(position.Z) {
			continue // decayed or otherwise unpropagatable element
		}
		lookAngles := satellite.ECIToLookAngles(position, observerCoords, 0, julianDay)

		elevationDeg := lookAngles.El * satellite.RAD2DEG
		if elevationDeg < 0 {
			continue // below the horizon, which is most of them
		}
		azimuthDeg := math.Mod(lookAngles.Az*satellite.RAD2DEG+360, 360)

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
			RangeKm:      roundTo(lookAngles.Rg, 0),
			InCone:       inCone,
		})
	}

	sort.Slice(snapshot.Objects, func(i, j int) bool {
		return snapshot.Objects[i].ElevationDeg > snapshot.Objects[j].ElevationDeg
	})
	return snapshot
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
