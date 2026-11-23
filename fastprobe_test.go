package main

import (
	"context"
	"testing"
	"time"
)

func TestSummariseFastProbes(t *testing.T) {
	at := time.Unix(1000, 0)
	results := []fastProbeResult{
		{done: true, rtt: 30},
		{done: true, rtt: 10},
		{done: true, lost: true},
		{done: false},
		{done: true, rtt: 20},
	}
	sample := summariseFastProbes("x", at, results)
	if sample.Sent != 5 || sample.Dropped != 2 || sample.Lost || sample.RTTMs != 20 {
		t.Fatalf("got %+v", sample)
	}

	allLost := summariseFastProbes("x", at, []fastProbeResult{{done: true, lost: true}, {done: true, lost: true}})
	if !allLost.Lost || allLost.Sent != 2 || allLost.Dropped != 2 {
		t.Fatalf("all lost: got %+v", allLost)
	}

	none := summariseFastProbes("x", at, nil)
	if !none.Lost || none.Error == "" {
		t.Fatalf("no probes: got %+v", none)
	}
}

func TestFastProbeWindowsAreContiguous(t *testing.T) {
	origin := time.Unix(2000, 0)
	clock := origin
	fast := &FastProbe{
		target: "x",
		probe:  func(context.Context, time.Duration) PingSample { return PingSample{} },
		now:    func() time.Time { return clock },
		sleep: func(_ context.Context, wait time.Duration) bool {
			clock = clock.Add(wait)
			return true
		},
	}
	for sent := origin; sent.Before(origin.Add(20 * time.Second)); sent = sent.Add(FastProbeInterval) {
		fast.results = append(fast.results, &fastProbeResult{sent: sent, done: true, rtt: 5})
	}

	clock = origin.Add(2 * time.Second)
	firstFrom := clock.Add(FastProbeLead - time.Second)
	counted := 0
	for _, pause := range []time.Duration{100, 900, 300, 50, 1100} {
		counted += fast.PingOnce(context.Background()).Sent
		clock = clock.Add(pause * time.Millisecond)
	}
	want := 0
	for sent := origin; sent.Before(fast.windowEnd); sent = sent.Add(FastProbeInterval) {
		if !sent.Before(firstFrom) {
			want++
		}
	}
	if counted != want {
		t.Fatalf("windows counted %d probes, want %d sent between %v and %v", counted, want, firstFrom, fast.windowEnd)
	}
}

func TestDownsampleHopsCountsProbesAndLossSeconds(t *testing.T) {
	start := time.Unix(3000, 0)
	samples := []HopSample{
		{Timestamp: start, Hops: map[string]HopResult{"r": {RTTMs: 2, Sent: 20, Dropped: 1}, "e": {RTTMs: 30}}},
		{Timestamp: start.Add(time.Second), Hops: map[string]HopResult{"r": {RTTMs: 3, Sent: 20}, "e": {Lost: true}}},
		{Timestamp: start.Add(2 * time.Second), Hops: map[string]HopResult{"r": {Lost: true, Sent: 20, Dropped: 20}, "e": {RTTMs: 25}}},
	}
	buckets, _ := DownsampleHops(samples, start, start.Add(3*time.Second), 1)
	if len(buckets) != 1 {
		t.Fatalf("want 1 bucket, got %d", len(buckets))
	}
	router, end := buckets[0].Hops["r"], buckets[0].Hops["e"]
	if router.Sent != 60 || router.Lost != 21 || router.LossSeconds != 2 || router.MaxRTTMs != 3 {
		t.Fatalf("router bucket %+v", router)
	}
	if end.Sent != 3 || end.Lost != 1 || end.LossSeconds != 1 || end.MaxRTTMs != 30 {
		t.Fatalf("end bucket %+v", end)
	}
}
