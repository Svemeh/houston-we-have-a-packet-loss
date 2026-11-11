package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestPingSamplesFromPicksTheTarget(t *testing.T) {
	start := time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC)
	rounds := []HopSample{
		{Timestamp: start, Hops: map[string]HopResult{HopEndTarget: {RTTMs: 38}, HopControlTarget: {RTTMs: 41}}},
		{Timestamp: start.Add(time.Second), Hops: map[string]HopResult{HopEndTarget: {Lost: true, Error: "timeout"}}},
		{Timestamp: start.Add(2 * time.Second), Hops: map[string]HopResult{HopControlTarget: {RTTMs: 40}}},
	}

	samples := pingSamplesFrom(rounds, HopEndTarget)
	if len(samples) != 2 {
		t.Fatalf("got %d samples, want 2 (round without the target skipped)", len(samples))
	}
	if got := samples[0]; got.Target != HopEndTarget || got.RTTMs != 38 || got.Lost || !got.Timestamp.Equal(start) {
		t.Errorf("first sample = %+v", got)
	}
	if got := samples[1]; !got.Lost || got.Error != "timeout" {
		t.Errorf("second sample = %+v, want lost with its error", got)
	}
}

func TestHopTargetSourceReadsTheHopLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hops.jsonl")
	logFile, err := OpenLogFile[HopSample](path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	for offset := range 3 {
		round := HopSample{Timestamp: now.Add(time.Duration(offset-3) * time.Second), Hops: map[string]HopResult{HopEndTarget: {RTTMs: float64(30 + offset)}}}
		if err := logFile.Append(round); err != nil {
			t.Fatal(err)
		}
	}
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}

	source := hopTargetSource{hops: NewLogFileSource[HopSample](path), target: HopEndTarget}
	samples, err := source.Samples(context.Background(), SampleQuery{Since: now.Add(-2 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 || samples[0].RTTMs != 31 || samples[1].RTTMs != 32 {
		t.Fatalf("samples = %+v, want RTTs 31 and 32", samples)
	}
}

func TestRelayHopTargetRepublishes(t *testing.T) {
	hopsHub := NewHub[HopSample](10)
	pingHub := NewHub[PingSample](10)
	pings, _, unsubscribe := pingHub.Subscribe()
	defer unsubscribe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relayDone := make(chan struct{})
	go func() {
		relayHopTarget(ctx, hopsHub, pingHub, HopEndTarget)
		close(relayDone)
	}()

	// The relay subscribes asynchronously; publish until it picks one up.
	deadline := time.After(2 * time.Second)
	for {
		hopsHub.PublishSample(HopSample{Timestamp: time.Now(), Hops: map[string]HopResult{HopEndTarget: {RTTMs: 40}}})
		select {
		case sample := <-pings:
			if sample.Target != HopEndTarget || sample.RTTMs != 40 {
				t.Fatalf("relayed %+v", sample)
			}
			cancel()
			<-relayDone
			return
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("nothing relayed")
		}
	}
}
