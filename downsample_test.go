package main

import (
	"testing"
	"time"
)

func TestDownsampleWorstCaseKeepsPeaksAndGaps(t *testing.T) {
	start := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	var samples []TelemetrySample
	for second := 0; second < 100; second++ {
		if second >= 40 && second < 60 {
			continue // outage: no samples at all
		}
		sample := TelemetrySample{Timestamp: start.Add(time.Duration(second) * time.Second), LatencyMs: 40, LinkState: LinkStateOnline}
		if second == 15 {
			sample.LatencyMs = 571
			sample.LinkState = LinkStateDegraded
		}
		samples = append(samples, sample)
	}

	points, width := DownsampleWorstCase(samples, start, start.Add(100*time.Second), 10)
	if width != 10*time.Second {
		t.Fatalf("bucket width = %v, want 10s", width)
	}
	if len(points) != 8 {
		t.Fatalf("got %d points, want 8 (two empty buckets stay gaps)", len(points))
	}
	if points[1].LatencyMs != 571 || points[1].LinkState != LinkStateDegraded {
		t.Errorf("bucket 1 = %v ms %s, want the 571 ms spike and DEGRADED", points[1].LatencyMs, points[1].LinkState)
	}
}

func TestDownsampleWorstCaseOnlineBeatsOffline(t *testing.T) {
	start := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	samples := []TelemetrySample{
		{Timestamp: start, LinkState: LinkStateOffline, PollError: "timeout"},
		{Timestamp: start.Add(time.Second), LinkState: LinkStateOnline},
		{Timestamp: start.Add(2 * time.Second), LinkState: LinkStateOnline},
	}
	points, _ := DownsampleWorstCase(samples, start, start.Add(10*time.Second), 1)
	if len(points) != 1 || points[0].LinkState != LinkStateOnline || points[0].PollError != "" {
		t.Errorf("got %+v, want one ONLINE point with no error", points)
	}
}

func TestDownsamplePingCountsLoss(t *testing.T) {
	start := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	var samples []PingSample
	for second := 0; second < 20; second++ {
		samples = append(samples, PingSample{
			Timestamp: start.Add(time.Duration(second) * time.Second),
			RTTMs:     float64(30 + second),
			Lost:      second == 3 || second == 4,
		})
	}

	buckets, width := DownsamplePing(samples, start, start.Add(20*time.Second), 2)
	if width != 10*time.Second || len(buckets) != 2 {
		t.Fatalf("got %d buckets of %v, want 2 of 10s", len(buckets), width)
	}
	if buckets[0].Sent != 10 || buckets[0].Lost != 2 {
		t.Errorf("bucket 0 sent/lost = %d/%d, want 10/2", buckets[0].Sent, buckets[0].Lost)
	}
	if buckets[1].MaxRTTMs != 49 {
		t.Errorf("bucket 1 max RTT = %v, want 49", buckets[1].MaxRTTMs)
	}
}

func TestDownsamplePingPassesThroughSmallInput(t *testing.T) {
	start := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	samples := []PingSample{{Timestamp: start, RTTMs: 30}, {Timestamp: start.Add(time.Second), Lost: true}}
	buckets, width := DownsamplePing(samples, start, start.Add(time.Hour), 2000)
	if len(buckets) != 2 || width != PingInterval {
		t.Errorf("got %d buckets of %v, want 2 of %v", len(buckets), width, PingInterval)
	}
}
