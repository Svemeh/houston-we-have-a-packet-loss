package main

import (
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	cases := []struct {
		fraction float64
		want     float64
	}{
		{0.50, 6},
		{0.90, 10},
		{0.99, 10},
		{1, 10},
		{0, 1},
	}
	for _, c := range cases {
		if got := percentile(values, c.fraction); got != c.want {
			t.Errorf("percentile(%v) = %v, want %v", c.fraction, got, c.want)
		}
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("percentile(empty) = %v, want 0", got)
	}
	if got := percentile([]float64{1.23456}, 0.5); got != 1.23 {
		t.Errorf("percentile rounds to 2 decimals: got %v", got)
	}
}

var testHopTargets = hopTargets("192.168.2.1")

// hopRound builds one round where every target answered in rttMs, except the
// ones listed in lost.
func hopRound(at time.Time, rttMs float64, lost ...string) HopSample {
	sample := HopSample{Timestamp: at, Hops: map[string]HopResult{}}
	for _, target := range testHopTargets {
		sample.Hops[target] = HopResult{RTTMs: rttMs}
	}
	for _, target := range lost {
		sample.Hops[target] = HopResult{Lost: true}
	}
	return sample
}

func TestSummariseHourBucketsLossBySlot(t *testing.T) {
	hourStart := time.Date(2026, 9, 22, 14, 0, 0, 0, time.UTC)

	var samples []HopSample
	samples = append(samples, hopRound(hourStart.Add(-time.Second), 40, HopEndTarget)) // previous hour: ignored
	for second := 0; second < 3600; second++ {
		at := hourStart.Add(time.Duration(second) * time.Second)
		if second%HandoverSlotSeconds == 11 {
			samples = append(samples, hopRound(at, 40, HopEndTarget))
		} else {
			samples = append(samples, hopRound(at, 40))
		}
	}
	samples = append(samples, hopRound(hourStart.Add(time.Hour), 40, HopEndTarget)) // next hour: ignored

	rollup := summariseHour(samples, nil, hourStart)

	if rollup.Samples != 3600 {
		t.Fatalf("Samples = %d, want 3600", rollup.Samples)
	}
	perSlot := 3600 / HandoverSlotSeconds
	for slot := 0; slot < HandoverSlotSeconds; slot++ {
		if rollup.HandoverSent[slot] != perSlot {
			t.Errorf("HandoverSent[%d] = %d, want %d", slot, rollup.HandoverSent[slot], perSlot)
		}
		wantLoss := 0
		if slot == 11 {
			wantLoss = perSlot
		}
		if rollup.HandoverLoss[slot] != wantLoss {
			t.Errorf("HandoverLoss[%d] = %d, want %d", slot, rollup.HandoverLoss[slot], wantLoss)
		}
	}

	end := rollup.Hops[HopEndTarget]
	if end.Sent != 3600 || end.Lost != perSlot {
		t.Errorf("end target sent/lost = %d/%d, want 3600/%d", end.Sent, end.Lost, perSlot)
	}
	if router := rollup.Hops[testHopTargets[0]]; router.Lost != 0 {
		t.Errorf("router lost %d, want 0", router.Lost)
	}
}

func TestSummariseHourJitterSkipsLostEchoes(t *testing.T) {
	hourStart := time.Date(2026, 9, 22, 14, 0, 0, 0, time.UTC)
	target := testHopTargets[0]
	at := func(second int) time.Time { return hourStart.Add(time.Duration(second) * time.Second) }

	samples := []HopSample{
		{Timestamp: at(0), Hops: map[string]HopResult{target: {RTTMs: 10}}},
		{Timestamp: at(1), Hops: map[string]HopResult{target: {RTTMs: 20}}}, // jitter 10
		{Timestamp: at(2), Hops: map[string]HopResult{target: {Lost: true}}},
		{Timestamp: at(3), Hops: map[string]HopResult{target: {RTTMs: 500}}}, // new run: no jitter vs 20
		{Timestamp: at(4), Hops: map[string]HopResult{target: {RTTMs: 505}}}, // jitter 5
	}

	stats := summariseHour(samples, nil, hourStart).Hops[target]
	if stats.Sent != 5 || stats.Lost != 1 {
		t.Fatalf("sent/lost = %d/%d, want 5/1", stats.Sent, stats.Lost)
	}
	if stats.JitterP99Ms != 10 {
		t.Errorf("JitterP99Ms = %v, want 10 (a loss must not create a 480 ms jump)", stats.JitterP99Ms)
	}
	if stats.MaxMs != 505 {
		t.Errorf("MaxMs = %v, want 505", stats.MaxMs)
	}
}

func TestSummariseHourCountsDishDropsBySlot(t *testing.T) {
	hourStart := time.Date(2026, 9, 22, 14, 0, 0, 0, time.UTC)
	dish := []TelemetrySample{
		{Timestamp: hourStart.Add(14 * time.Second), DropRateFraction: 0.5},
		{Timestamp: hourStart.Add(29 * time.Second), DropRateFraction: 0},
		{Timestamp: hourStart.Add(44 * time.Second), PollError: "unreachable"}, // not a reading
	}

	rollup := summariseHour(nil, dish, hourStart)
	if rollup.DishSlotSamples[14] != 2 || rollup.DishDropSlots[14] != 1 {
		t.Errorf("slot 14 drops/samples = %d/%d, want 1/2", rollup.DishDropSlots[14], rollup.DishSlotSamples[14])
	}
}
