package main

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	satellite "github.com/joshuaferrara/go-satellite"
	"google.golang.org/protobuf/encoding/protowire"
)

func encodeObstructionMap(rows, cols int, snr []float32, frame int, packed bool) []byte {
	var raw []byte
	raw = protowire.AppendTag(raw, 1, protowire.VarintType)
	raw = protowire.AppendVarint(raw, uint64(rows))
	raw = protowire.AppendTag(raw, 2, protowire.VarintType)
	raw = protowire.AppendVarint(raw, uint64(cols))
	if packed {
		var cells []byte
		for _, value := range snr {
			cells = protowire.AppendFixed32(cells, math.Float32bits(value))
		}
		raw = protowire.AppendTag(raw, 3, protowire.BytesType)
		raw = protowire.AppendBytes(raw, cells)
	} else {
		for _, value := range snr {
			raw = protowire.AppendTag(raw, 3, protowire.Fixed32Type)
			raw = protowire.AppendFixed32(raw, math.Float32bits(value))
		}
	}
	raw = protowire.AppendTag(raw, 4, protowire.Fixed32Type)
	raw = protowire.AppendFixed32(raw, math.Float32bits(10))
	raw = protowire.AppendTag(raw, 5, protowire.Fixed32Type)
	raw = protowire.AppendFixed32(raw, math.Float32bits(80))
	raw = protowire.AppendTag(raw, 6, protowire.VarintType)
	return protowire.AppendVarint(raw, uint64(frame))
}

func TestParseObstructionMap(t *testing.T) {
	snr := []float32{-1, -1, 1, 0.5, -1, 0, 1, 1, -1}
	for _, packed := range []bool{true, false} {
		parsed, err := parseObstructionMap(encodeObstructionMap(3, 3, snr, obstructionFrameEarth, packed))
		if err != nil {
			t.Fatalf("packed=%v: %v", packed, err)
		}
		if parsed.Rows != 3 || parsed.Cols != 3 || parsed.MaxThetaDeg != 80 || parsed.Frame != obstructionFrameEarth || len(parsed.SNR) != 9 || parsed.SNR[3] != 0.5 {
			t.Errorf("packed=%v: got %+v", packed, parsed)
		}
	}
	if _, err := parseObstructionMap(encodeObstructionMap(3, 3, snr[:8], obstructionFrameEarth, true)); err == nil {
		t.Error("a map with a missing cell should be rejected")
	}
}

func TestFindBytesField(t *testing.T) {
	raw := protowire.AppendTag(nil, 1, protowire.VarintType)
	raw = protowire.AppendVarint(raw, 7)
	raw = protowire.AppendTag(raw, fieldResponseGetObstructionMap, protowire.BytesType)
	raw = protowire.AppendBytes(raw, []byte{1, 2, 3})
	body, ok := findBytesField(raw, fieldResponseGetObstructionMap)
	if !ok || len(body) != 3 {
		t.Fatalf("got %v, %v", body, ok)
	}
	if _, ok := findBytesField(raw, 2015); ok {
		t.Error("found a field that isn't there")
	}
}

func TestPixelDirection(t *testing.T) {
	cases := []struct {
		row, col      int
		azimuth, elev float64
	}{
		{61, 61, 0, 90},
		{0, 61, 0, 10},
		{61, 122, 90, 10},
		{122, 61, 180, 10},
		{61, 0, 270, 10},
	}
	for _, c := range cases {
		azimuth, elevation := pixelDirection(c.row, c.col, 123, 80)
		if math.Abs(elevation-c.elev) > 1e-9 || (c.elev != 90 && math.Abs(azimuth-c.azimuth) > 1e-9) {
			t.Errorf("(%d,%d) = %.2f/%.2f, want %.2f/%.2f", c.row, c.col, azimuth, elevation, c.azimuth, c.elev)
		}
	}
}

func testCandidates(t *testing.T) []trackedSatellite {
	tracked, _, err := parseElementStream(strings.NewReader(referenceOMM), 0)
	if err != nil || len(tracked) != 2 {
		t.Fatalf("parse: %v, %d tracked", err, len(tracked))
	}
	return tracked
}

// observerUnder returns a site a few degrees off the satellite's ground track at moment, so it passes high but not overhead.
func observerUnder(propagator satellite.Satellite, moment time.Time) Observer {
	utc := moment.UTC()
	year, month, day := utc.Date()
	hour, minute, second := utc.Clock()
	position, _ := satellite.Propagate(propagator, year, int(month), day, hour, minute, second)
	_, _, groundPoint := satellite.ECIToLLA(position, satellite.GSTimeFromDate(year, int(month), day, hour, minute, second))
	degrees := satellite.LatLongDeg(groundPoint)
	return Observer{LatitudeDeg: degrees.Latitude + 3, LongitudeDeg: degrees.Longitude}
}

func syntheticArc(propagator satellite.Satellite, observer Observer, start time.Time, offset time.Duration) []arcPixel {
	var pixels []arcPixel
	for second := 0; second < 13; second++ {
		at := start.Add(time.Duration(second) * time.Second)
		azimuthDeg, elevationDeg, _, _ := lookAngles(propagator, observer, at.Add(offset))
		pixels = append(pixels, arcPixel{at: at, azimuthDeg: azimuthDeg, elevationDeg: elevationDeg})
	}
	return pixels
}

func TestMatchArcPicksTheTracingSatellite(t *testing.T) {
	candidates := testCandidates(t)
	moment := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	observer := observerUnder(candidates[0].propagator, moment)
	pixels := syntheticArc(candidates[0].propagator, observer, moment, 2*time.Second)

	best, runnerUp := matchArc(pixels, candidates, observer)
	if best == nil || best.Satellite != candidates[0].name || best.TimeOffsetS != 2 || best.FitDeg > 0.3 {
		t.Fatalf("best = %+v", best)
	}
	if runnerUp == nil || runnerUp.FitDeg < 10 {
		t.Errorf("runner-up = %+v", runnerUp)
	}
	if best.ElevationDeg < 50 {
		t.Errorf("test geometry: elevation %.1f, want a high pass", best.ElevationDeg)
	}
	if status := classifyMatch(best, runnerUp); status != ServingMatched {
		t.Errorf("status = %s", status)
	}
}

func TestClassifyMatch(t *testing.T) {
	cases := []struct {
		best, runnerUp *ServingCandidate
		want           string
	}{
		{nil, nil, ServingNoElements},
		{&ServingCandidate{FitDeg: 0.8}, nil, ServingMatched},
		{&ServingCandidate{FitDeg: 0.8}, &ServingCandidate{FitDeg: 9}, ServingMatched},
		{&ServingCandidate{FitDeg: 0.8}, &ServingCandidate{FitDeg: 1.5}, ServingAmbiguous},
		{&ServingCandidate{FitDeg: 4}, &ServingCandidate{FitDeg: 15}, ServingAmbiguous},
	}
	for _, c := range cases {
		if got := classifyMatch(c.best, c.runnerUp); got != c.want {
			t.Errorf("classifyMatch(%+v, %+v) = %s, want %s", c.best, c.runnerUp, got, c.want)
		}
	}
}

type fakeMapSource struct {
	clearErr error
	reads    []ObstructionMap
	cleared  int
	read     int
}

func (source *fakeMapSource) ClearObstructionMap(context.Context) error {
	source.cleared++
	return source.clearErr
}

func (source *fakeMapSource) ObstructionMap(context.Context) (ObstructionMap, error) {
	reading := source.reads[min(source.read, len(source.reads)-1)]
	source.read++
	return reading, nil
}

func blankMap(frame int) ObstructionMap {
	snr := make([]float32, 123*123)
	for i := range snr {
		snr[i] = -1
	}
	return ObstructionMap{Rows: 123, Cols: 123, SNR: snr, MaxThetaDeg: 80, Frame: frame}
}

func testServingTracker(source ObstructionMapSource, sky *SkyTracker, start time.Time) *ServingTracker {
	clock := start
	return &ServingTracker{
		source:  source,
		sky:     sky,
		emit:    func(ServingSlot) {},
		now:     func() time.Time { return clock },
		sleep:   func(_ context.Context, wait time.Duration) bool { clock = clock.Add(max(wait, 0)); return true },
		timeout: time.Second,
	}
}

func TestObserveSlotStatuses(t *testing.T) {
	start := time.Unix(1793488467, 0)
	sky := NewSkyTracker(Observer{LatitudeDeg: 60.2, LongitudeDeg: 12.1}, LocationFromSettings)

	empty := &fakeMapSource{reads: []ObstructionMap{blankMap(obstructionFrameEarth)}}
	slot, ok := testServingTracker(empty, sky, start).observeSlot(context.Background(), start)
	if !ok || slot.Status != ServingFewPixels || empty.cleared != 1 || empty.read != ServingReadsPerSlot {
		t.Errorf("empty map: %+v, cleared %d, read %d", slot, empty.cleared, empty.read)
	}

	traced := blankMap(2)
	for col := 70; col < 80; col++ {
		traced.SNR[40*123+col] = 1
	}
	userFrame := &fakeMapSource{reads: []ObstructionMap{traced}}
	if slot, _ := testServingTracker(userFrame, sky, start).observeSlot(context.Background(), start); slot.Status != ServingUnsupportedFrame {
		t.Errorf("FRAME_UT map: %+v", slot)
	}

	traced.Frame = obstructionFrameEarth
	noElements := &fakeMapSource{reads: []ObstructionMap{traced}}
	if slot, _ := testServingTracker(noElements, sky, start).observeSlot(context.Background(), start); slot.Status != ServingNoElements || slot.Pixels != 10 {
		t.Errorf("no elements loaded: %+v", slot)
	}

	failing := &fakeMapSource{clearErr: errors.New("refused"), reads: []ObstructionMap{traced}}
	if slot, _ := testServingTracker(failing, sky, start).observeSlot(context.Background(), start); slot.Status != ServingError || slot.Error != "refused" || failing.read != 0 {
		t.Errorf("clear refused: %+v, read %d", slot, failing.read)
	}
}
