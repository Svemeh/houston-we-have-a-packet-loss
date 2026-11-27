package main

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestHandoverHistoryKeepsNewestBurstsInRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handovers.jsonl")
	logFile, err := OpenLogFile[HandoverSample](path)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()

	now := time.Now().Truncate(time.Second)
	boundaries := []time.Time{
		now.Add(-2 * time.Hour),
		now.Add(-45 * time.Second),
		now.Add(-30 * time.Second),
		now.Add(-15 * time.Second),
	}
	for _, boundary := range boundaries {
		if err := logFile.Append(HandoverSample{Boundary: boundary, Target: HandoverBurstTarget, StepMs: 20, Lost: 1}); err != nil {
			t.Fatal(err)
		}
	}

	handler := newHistoryHandler(NewLogFileSource[HandoverSample](path), keepNewest[HandoverSample](HandoverSlotSeconds*time.Second), MaxHandoverRange, 2)
	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest("GET", RouteHandovers+"?range=1h", nil))
	if recorder.Code != 200 {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}

	var body historyResponse[HandoverSample]
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.RawCount != 3 {
		t.Errorf("raw_count %d, want 3 (the 2 h old burst is out of range)", body.RawCount)
	}
	if body.KeptCount != 2 || len(body.Samples) != 2 {
		t.Fatalf("kept %d samples, want the newest 2", len(body.Samples))
	}
	if !body.Samples[0].Boundary.Equal(boundaries[2]) || !body.Samples[1].Boundary.Equal(boundaries[3]) {
		t.Errorf("got boundaries %v, %v; want the two newest", body.Samples[0].Boundary, body.Samples[1].Boundary)
	}
	if body.BucketWidthMs != 15000 {
		t.Errorf("bucket_width_ms %d, want 15000", body.BucketWidthMs)
	}
}

func TestServingHistoryServesSlotsInRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serving.jsonl")
	logFile, err := OpenLogFile[ServingSlot](path)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()

	now := time.Now().Truncate(time.Second)
	slots := []ServingSlot{
		{Start: now.Add(-3 * time.Hour), Status: "matched", Best: &ServingCandidate{Satellite: "STARLINK-1"}},
		{Start: now.Add(-45 * time.Second), Status: "matched", Best: &ServingCandidate{Satellite: "STARLINK-2"}},
		{Start: now.Add(-30 * time.Second), Status: "ambiguous"},
		{Start: now.Add(-15 * time.Second), Status: "matched", Best: &ServingCandidate{Satellite: "STARLINK-3"}},
	}
	for _, slot := range slots {
		if err := logFile.Append(slot); err != nil {
			t.Fatal(err)
		}
	}

	handler := newHistoryHandler(NewLogFileSource[ServingSlot](path), keepNewest[ServingSlot](HandoverSlotSeconds*time.Second), MaxHandoverRange, 2)
	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest("GET", RouteServingHistory+"?range=2h", nil))
	if recorder.Code != 200 {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}

	var body historyResponse[ServingSlot]
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.RawCount != 3 {
		t.Errorf("raw_count %d, want 3 (the 3 h old slot is out of range)", body.RawCount)
	}
	if len(body.Samples) != 2 {
		t.Fatalf("kept %d slots, want the newest 2", len(body.Samples))
	}
	if body.Samples[0].Status != "ambiguous" || body.Samples[0].Best != nil {
		t.Errorf("first slot %+v, want the unmatched one without a satellite", body.Samples[0])
	}
	if body.Samples[1].Best == nil || body.Samples[1].Best.Satellite != "STARLINK-3" || !body.Samples[1].Start.Equal(slots[3].Start) {
		t.Errorf("second slot %+v, want STARLINK-3 at %v", body.Samples[1], slots[3].Start)
	}
	if body.BucketWidthMs != 15000 {
		t.Errorf("bucket_width_ms %d, want 15000", body.BucketWidthMs)
	}
}
