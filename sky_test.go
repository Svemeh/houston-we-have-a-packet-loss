package main

import (
	"math"
	"strings"
	"testing"

	satellite "github.com/joshuaferrara/go-satellite"
)

const (
	referenceTLE1 = "1 44714U 19074B   26297.24041667  .00012345  12345-4  45678-3 0  9993"
	referenceTLE2 = "2 44714  53.0544 123.4567 0001234  89.1234 270.9876 15.06412345123456"

	referenceOMM = `[
{"OBJECT_NAME":"STARLINK-1008","NORAD_CAT_ID":44714,"EPOCH":"2026-10-24T05:46:12.000288",
 "MEAN_MOTION":15.06412345,"ECCENTRICITY":0.0001234,"INCLINATION":53.0544,"RA_OF_ASC_NODE":123.4567,
 "ARG_OF_PERICENTER":89.1234,"MEAN_ANOMALY":270.9876,"BSTAR":0.00045678,"MEAN_MOTION_DOT":0.00012345,
 "MEAN_MOTION_DDOT":0.000012345},
{"OBJECT_NAME":"STARLINK-40001","NORAD_CAT_ID":100123,"EPOCH":"2026-10-30T23:59:59.999999",
 "MEAN_MOTION":15.5,"ECCENTRICITY":0.0002,"INCLINATION":43,"RA_OF_ASC_NODE":-10,
 "ARG_OF_PERICENTER":0,"MEAN_ANOMALY":359.9,"BSTAR":-0.0001,"MEAN_MOTION_DOT":-0.00002,
 "MEAN_MOTION_DDOT":0},
{"OBJECT_NAME":"BROKEN","NORAD_CAT_ID":100124,"EPOCH":"2026-10-30T00:00:00",
 "MEAN_MOTION":15.5,"ECCENTRICITY":1.5,"INCLINATION":53,"RA_OF_ASC_NODE":0,
 "ARG_OF_PERICENTER":0,"MEAN_ANOMALY":0,"BSTAR":0,"MEAN_MOTION_DOT":0,"MEAN_MOTION_DDOT":0}
]`
)

func TestOMMPropagatesLikeTLE(t *testing.T) {
	tracked, catalogCount, err := parseElementStream(strings.NewReader(referenceOMM), 0)
	if err != nil {
		t.Fatal(err)
	}
	if catalogCount != 3 || len(tracked) != 2 {
		t.Fatalf("catalog %d, tracked %d; want 3 and 2", catalogCount, len(tracked))
	}
	if tracked[1].noradID != 100123 || tracked[1].name != "STARLINK-40001" {
		t.Errorf("six-digit satellite = %+v", tracked[1])
	}

	reference := satellite.TLEToSat(referenceTLE1, referenceTLE2, satellite.GravityWGS84)
	for _, hoursAfterEpoch := range []int{0, 6, 48} {
		hour := 5 + hoursAfterEpoch
		want, _ := satellite.Propagate(reference, 2026, 10, 24+hour/24, hour%24, 46, 12)
		got, _ := satellite.Propagate(tracked[0].propagator, 2026, 10, 24+hour/24, hour%24, 46, 12)
		if math.Abs(got.X-want.X) > 1e-6 || math.Abs(got.Y-want.Y) > 1e-6 || math.Abs(got.Z-want.Z) > 1e-6 {
			t.Errorf("+%dh: got %+v, want %+v", hoursAfterEpoch, got, want)
		}
	}
}

func TestOMMToTLELayout(t *testing.T) {
	tracked, _, err := parseElementStream(strings.NewReader(referenceOMM), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, sat := range tracked {
		if len(sat.propagator.Line1) != 69 || len(sat.propagator.Line2) != 69 {
			t.Errorf("%s: line lengths %d/%d", sat.name, len(sat.propagator.Line1), len(sat.propagator.Line2))
		}
	}
	line1, line2 := tracked[0].propagator.Line1, tracked[0].propagator.Line2
	if line1[18:61] != referenceTLE1[18:61] || line2[7:63] != referenceTLE2[7:63] {
		t.Errorf("got\n%s\n%s\nwant (cols 19-61 / 8-63)\n%s\n%s", line1, line2, referenceTLE1, referenceTLE2)
	}
}

func TestTLEExponent(t *testing.T) {
	cases := map[float64]string{
		0:           " 00000+0",
		0.00045678:  " 45678-3",
		-0.0001:     "-10000-3",
		0.999999:    " 10000+1",
		0.000012345: " 12345-4",
	}
	for value, want := range cases {
		got, err := tleExponent(value)
		if err != nil || got != want {
			t.Errorf("tleExponent(%g) = %q, %v; want %q", value, got, err, want)
		}
	}
	if _, err := tleExponent(1e12); err == nil {
		t.Error("1e12 should not fit")
	}
}

func TestLookFromUsesTheEllipsoid(t *testing.T) {
	observer := Observer{LatitudeDeg: 60.19, LongitudeDeg: 12.00}
	julianDay := satellite.JDay(2026, 11, 1, 2, 0, 0)
	siderealAngle := satellite.ThetaG_JD(julianDay)
	latitude, longitude := observer.LatitudeDeg*satellite.DEG2RAD, observer.LongitudeDeg*satellite.DEG2RAD
	primeVertical := wgs84EquatorialRadiusKm / math.Sqrt(1-wgs84EccentricitySquared*math.Sin(latitude)*math.Sin(latitude))
	site := [3]float64{
		primeVertical * math.Cos(latitude) * math.Cos(longitude),
		primeVertical * math.Cos(latitude) * math.Sin(longitude),
		primeVertical * (1 - wgs84EccentricitySquared) * math.Sin(latitude),
	}
	up := [3]float64{math.Cos(latitude) * math.Cos(longitude), math.Cos(latitude) * math.Sin(longitude), math.Sin(latitude)}
	north := [3]float64{-math.Sin(latitude) * math.Cos(longitude), -math.Sin(latitude) * math.Sin(longitude), math.Cos(latitude)}
	toTEME := func(offset [3]float64) satellite.Vector3 {
		x, y, z := site[0]+offset[0], site[1]+offset[1], site[2]+offset[2]
		return satellite.Vector3{
			X: x*math.Cos(siderealAngle) - y*math.Sin(siderealAngle),
			Y: x*math.Sin(siderealAngle) + y*math.Cos(siderealAngle),
			Z: z,
		}
	}

	_, elevation, rangeKm := lookFrom(toTEME([3]float64{550 * up[0], 550 * up[1], 550 * up[2]}), observer, julianDay)
	if math.Abs(elevation-90) > 1e-6 || math.Abs(rangeKm-550) > 1e-6 {
		t.Errorf("straight up: el %.6f, range %.6f", elevation, rangeKm)
	}
	azimuth, elevation, _ := lookFrom(toTEME([3]float64{
		500*north[0] + 500*up[0], 500*north[1] + 500*up[1], 500*north[2] + 500*up[2],
	}), observer, julianDay)
	if math.Abs(azimuth) > 1e-6 && math.Abs(azimuth-360) > 1e-6 || math.Abs(elevation-45) > 1e-6 {
		t.Errorf("north at 45°: az %.6f, el %.6f", azimuth, elevation)
	}
}
