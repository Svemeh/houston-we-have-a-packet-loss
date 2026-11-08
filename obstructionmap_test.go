package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 5x5 map, 80° max theta: the centre and its 8 neighbours sit above a 25° floor, the outer ring below it.
func smallMap(values map[int]float32) ObstructionMap {
	snr := make([]float32, 25)
	for i := range snr {
		snr[i] = -1
	}
	for cell, value := range values {
		snr[cell] = value
	}
	return ObstructionMap{Rows: 5, Cols: 5, SNR: snr, MaxThetaDeg: 80, Frame: obstructionFrameEarth}
}

var aboveFloor = []int{6, 7, 8, 11, 12, 13, 16, 17, 18}

func testMapper(t *testing.T) *ObstructionMapper {
	return LoadObstructionMapper(filepath.Join(t.TempDir(), "obstructionMap.json"), func() float64 { return 25 })
}

func fillAll(mapper *ObstructionMapper, obstructed []int, at time.Time) {
	values := map[int]float32{0: 1} // below the floor: merged but not counted
	for _, cell := range aboveFloor {
		values[cell] = 1
	}
	for _, cell := range obstructed {
		values[cell] = 0
	}
	mapper.MergeSlot(smallMap(values), at)
}

func TestObstructionMapCoverageAndFraction(t *testing.T) {
	mapper := testMapper(t)
	at := time.Unix(1793488467, 0)

	if status := mapper.Status(at); status.Coverage != 0 || status.Fraction != nil || status.LiveKnown {
		t.Fatalf("empty map: %+v", status)
	}

	mapper.MergeSlot(smallMap(map[int]float32{12: 1, 13: 0, 0: 1}), at)
	status := mapper.Status(at)
	if math.Abs(status.Coverage-2.0/9) > 1e-9 || status.Fraction != nil {
		t.Fatalf("2 of 9 cells: %+v", status)
	}

	fillAll(mapper, []int{13}, at)
	status = mapper.Status(at)
	if status.Coverage != 1 || status.Fraction == nil || math.Abs(*status.Fraction-1.0/9) > 1e-9 {
		t.Fatalf("full map with one blocked cell: %+v", status)
	}
}

func TestObstructionCellFollowsRecentSlots(t *testing.T) {
	mapper := testMapper(t)
	at := time.Unix(1793488467, 0)
	for range ObstructionCellMaxCount * 3 {
		mapper.MergeSlot(smallMap(map[int]float32{12: 1}), at)
	}
	cell := mapper.cells[12]
	if cell.clear+cell.obstructed > ObstructionCellMaxCount || cell.isObstructed() {
		t.Fatalf("after many clear slots: %+v", cell)
	}
	slots := 0
	for !mapper.cells[12].isObstructed() {
		mapper.MergeSlot(smallMap(map[int]float32{12: 0}), at)
		if slots++; slots > ObstructionCellMaxCount {
			t.Fatalf("cell still clear after %d blocked slots: %+v", slots, mapper.cells[12])
		}
	}
	if cell := mapper.cells[12]; cell.lastSNR != 0 || !cell.lastSeen.Equal(at) {
		t.Errorf("last reading not kept: %+v", cell)
	}
}

func TestObstructionCellTieUsesLastReading(t *testing.T) {
	cases := []struct {
		cell mapCell
		want bool
	}{
		{mapCell{}, false},
		{mapCell{clear: 1}, false},
		{mapCell{obstructed: 1, lastSNR: 0}, true},
		{mapCell{clear: 2, obstructed: 2, lastSNR: 0.2}, true},
		{mapCell{clear: 2, obstructed: 2, lastSNR: 1}, false},
		{mapCell{clear: 3, obstructed: 2, lastSNR: 0}, false},
	}
	for _, c := range cases {
		if got := c.cell.isObstructed(); got != c.want {
			t.Errorf("%+v: obstructed %v, want %v", c.cell, got, c.want)
		}
	}
}

func TestObstructionMapGridChangeStartsOver(t *testing.T) {
	mapper := testMapper(t)
	at := time.Unix(1793488467, 0)
	fillAll(mapper, nil, at)
	mapper.MergeSlot(blankMap(obstructionFrameEarth), at)
	if mapper.rows != 123 || mapper.Status(at).Coverage != 0 {
		t.Errorf("old 5x5 cells kept after the grid changed")
	}
}

func TestObstructionMapSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "obstructionMap.json")
	floor := func() float64 { return 25 }
	at := time.Unix(1793488467, 0)

	mapper := LoadObstructionMapper(path, floor)
	if err := mapper.Save(at); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("an untouched map was written")
	}

	fillAll(mapper, []int{13}, at)
	mapper.MergeSlot(smallMap(map[int]float32{12: 0.4}), at.Add(15*time.Second))
	if err := mapper.Save(at); err != nil {
		t.Fatal(err)
	}

	reloaded := LoadObstructionMapper(path, floor)
	if reloaded.View(at).Cells != mapper.View(at).Cells {
		t.Errorf("cells differ:\n%s\n%s", reloaded.View(at).Cells, mapper.View(at).Cells)
	}
	if got, want := reloaded.cells[12], mapper.cells[12]; got.clear != want.clear || got.obstructed != want.obstructed || got.lastSNR != want.lastSNR || !got.lastSeen.Equal(want.lastSeen) {
		t.Errorf("cell 12 = %+v, want %+v", got, want)
	}
	if status := reloaded.Status(at); status.Fraction == nil || math.Abs(*status.Fraction-2.0/9) > 1e-9 { // 12 ties, last reading blocked
		t.Errorf("reloaded status: %+v", status)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if broken := LoadObstructionMapper(path, floor); broken.rows != 0 {
		t.Error("a corrupt file should start an empty map")
	}
}

func TestObstructionMapApply(t *testing.T) {
	mapper := testMapper(t)
	at := time.Unix(1793488467, 0)
	dishFraction := 0.0
	sample := func(obstructed bool) TelemetrySample {
		return TelemetrySample{Timestamp: at, LinkState: deriveLinkState(0, obstructed), Obstructed: obstructed, ObstructionFraction: &dishFraction}
	}

	cleared := sample(false)
	mapper.Apply(&cleared)
	if cleared.ObstructionFraction != nil || cleared.ObstructionSource != ObstructionSourceMap || cleared.Obstructed || cleared.LinkState != LinkStateOnline {
		t.Errorf("empty map should hide the dish's reset 0%%: %+v", cleared)
	}

	mapper.ObserveRead(smallMap(map[int]float32{12: 1, 13: 0}), []int{13}, at.Add(-time.Second))
	blocked := sample(false)
	mapper.Apply(&blocked)
	if !blocked.Obstructed || blocked.DishObstructed || blocked.LinkState != LinkStateObstructed {
		t.Errorf("arc traced into a blocked cell: %+v", blocked)
	}

	stale := sample(false)
	stale.Timestamp = at.Add(ObstructionLiveMaxAge + time.Second)
	mapper.Apply(&stale)
	if stale.Obstructed {
		t.Errorf("an old arc reading should expire: %+v", stale)
	}

	dishSays := sample(true)
	dishSays.Timestamp = stale.Timestamp
	mapper.Apply(&dishSays)
	if !dishSays.Obstructed || !dishSays.DishObstructed || dishSays.LinkState != LinkStateObstructed {
		t.Errorf("the dish's own obstructed flag should stand: %+v", dishSays)
	}

	fillAll(mapper, []int{13}, at)
	filled := sample(false)
	mapper.Apply(&filled)
	if filled.ObstructionFraction == nil || math.Abs(*filled.ObstructionFraction-1.0/9) > 1e-9 || filled.MapCoverage != 1 {
		t.Errorf("full map: %+v", filled)
	}

	offline := TelemetrySample{Timestamp: at, LinkState: LinkStateOffline}
	mapper.Apply(&offline)
	if offline.ObstructionSource != "" || offline.LinkState != LinkStateOffline {
		t.Errorf("offline sample changed: %+v", offline)
	}
}

func TestObserveSlotFeedsTheObstructionMap(t *testing.T) {
	start := time.Unix(1793488467, 0)
	sky := NewSkyTracker(Observer{LatitudeDeg: 60.2, LongitudeDeg: 12.1}, LocationFromSettings)

	// The arc grows by one cell per read; the last cell traced is blocked.
	var reads []ObstructionMap
	traced := blankMap(obstructionFrameEarth)
	for read := 0; read < ServingReadsPerSlot; read++ {
		next := ObstructionMap{Rows: 123, Cols: 123, SNR: append([]float32(nil), traced.SNR...), MaxThetaDeg: 80, Frame: obstructionFrameEarth}
		next.SNR[40*123+60+read] = 1
		if read == ServingReadsPerSlot-1 {
			next.SNR[40*123+60+read] = 0
		}
		reads = append(reads, next)
		traced = next
	}
	tracker := testServingTracker(&fakeMapSource{reads: reads}, sky, start)
	tracker.obstruction = LoadObstructionMapper(filepath.Join(t.TempDir(), "obstructionMap.json"), sky.ServiceFloor)

	if _, ok := tracker.observeSlot(context.Background(), start); !ok {
		t.Fatal("slot cut short")
	}
	clearSlots, blockedSlots := 0, 0
	for _, cell := range tracker.obstruction.cells {
		clearSlots += cell.clear
		blockedSlots += cell.obstructed
	}
	if clearSlots != ServingReadsPerSlot-1 || blockedSlots != 1 {
		t.Errorf("merged %d clear, %d blocked; want %d, 1", clearSlots, blockedSlots, ServingReadsPerSlot-1)
	}
	if status := tracker.obstruction.Status(tracker.now()); !status.LiveKnown || !status.ObstructedNow {
		t.Errorf("live state after the blocked cell: %+v", status)
	}

	failing := testServingTracker(&fakeMapSource{clearErr: context.DeadlineExceeded, reads: reads}, sky, start)
	failing.obstruction = LoadObstructionMapper(filepath.Join(t.TempDir(), "obstructionMap.json"), sky.ServiceFloor)
	failing.observeSlot(context.Background(), start)
	if failing.obstruction.rows != 0 {
		t.Error("a slot whose clear failed must not be merged: its cells may belong to earlier slots")
	}
}

func TestObstructionMapHandler(t *testing.T) {
	get := func(mapper *ObstructionMapper) ObstructionMapView {
		recorder := httptest.NewRecorder()
		newObstructionMapHandler(mapper)(recorder, httptest.NewRequest("GET", RouteObstructionMap, nil))
		var view ObstructionMapView
		if err := json.NewDecoder(recorder.Body).Decode(&view); err != nil {
			t.Fatal(err)
		}
		return view
	}
	if view := get(nil); view.Enabled || view.Cells != "" {
		t.Errorf("tracking off: %+v", view)
	}
	mapper := testMapper(t)
	mapper.MergeSlot(smallMap(map[int]float32{12: 1, 13: 0}), time.Now())
	view := get(mapper)
	if !view.Enabled || view.Rows != 5 || view.Cells != "000000000000"+"12"+"00000000000" || view.Fraction != nil || view.ObstructedNow != nil {
		t.Errorf("view: %+v", view)
	}
}

func TestMergeWorstCaseObstructionFraction(t *testing.T) {
	low, high := 0.01, 0.2
	unknown := TelemetrySample{LinkState: LinkStateOnline}
	merged := mergeWorstCase(unknown, TelemetrySample{LinkState: LinkStateOnline, ObstructionFraction: &low})
	if merged.ObstructionFraction == nil || *merged.ObstructionFraction != low {
		t.Fatalf("unknown then 1%%: %+v", merged.ObstructionFraction)
	}
	merged = mergeWorstCase(merged, TelemetrySample{LinkState: LinkStateOnline, ObstructionFraction: &high})
	merged = mergeWorstCase(merged, unknown)
	if *merged.ObstructionFraction != high || low != 0.01 {
		t.Errorf("worst should be 20%% and inputs untouched: %v, low %v", *merged.ObstructionFraction, low)
	}
}
