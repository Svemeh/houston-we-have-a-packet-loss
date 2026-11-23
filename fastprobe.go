package main

import (
	"context"
	"sort"
	"sync"
	"time"
)

const (
	FastProbeInterval = 50 * time.Millisecond
	FastProbeTimeout  = 400 * time.Millisecond
	FastProbeLead     = 400 * time.Millisecond
	FastProbeSettle   = 20 * time.Millisecond
	fastProbeMaxSpan  = 2 * time.Second
	fastProbeKeep     = 5 * time.Second
)

type fastProbeResult struct {
	sent time.Time
	done bool
	lost bool
	rtt  float64
}

type FastProbe struct {
	target    string
	probe     func(ctx context.Context, timeout time.Duration) PingSample
	now       func() time.Time
	sleep     func(ctx context.Context, wait time.Duration) bool
	mutex     sync.Mutex
	results   []*fastProbeResult
	windowEnd time.Time
}

func NewFastProbe(target string, probe func(ctx context.Context, timeout time.Duration) PingSample) *FastProbe {
	return &FastProbe{target: target, probe: probe, now: time.Now, sleep: sleepContext}
}

func (fast *FastProbe) Run(ctx context.Context) {
	ticker := time.NewTicker(FastProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fast.send(ctx)
		}
	}
}

func (fast *FastProbe) send(ctx context.Context) {
	result := &fastProbeResult{sent: fast.now()}
	fast.mutex.Lock()
	fast.results = append(fast.results, result)
	fast.mutex.Unlock()
	go func() {
		sample := fast.probe(ctx, FastProbeTimeout)
		fast.mutex.Lock()
		result.done, result.lost, result.rtt = true, sample.Lost, sample.RTTMs
		fast.mutex.Unlock()
	}()
}

func (fast *FastProbe) PingOnce(ctx context.Context) PingSample {
	start := fast.now()
	end := start.Add(FastProbeLead)
	fast.sleep(ctx, end.Add(FastProbeTimeout+FastProbeSettle).Sub(fast.now()))

	fast.mutex.Lock()
	defer fast.mutex.Unlock()
	from := fast.windowEnd
	if from.IsZero() || end.Sub(from) > fastProbeMaxSpan || !from.Before(end) {
		from = end.Add(-time.Second)
	}
	fast.windowEnd = end
	window := make([]fastProbeResult, 0, 40)
	kept := fast.results[:0]
	for _, result := range fast.results {
		if !result.sent.Before(from) && result.sent.Before(end) {
			window = append(window, *result)
		}
		if result.sent.After(end.Add(-fastProbeKeep)) {
			kept = append(kept, result)
		}
	}
	fast.results = kept
	return summariseFastProbes(fast.target, start, window)
}

func summariseFastProbes(target string, timestamp time.Time, results []fastProbeResult) PingSample {
	sample := PingSample{Timestamp: timestamp, Target: target, Sent: len(results)}
	answered := make([]float64, 0, len(results))
	for _, result := range results {
		if result.done && !result.lost {
			answered = append(answered, result.rtt)
		}
	}
	sample.Dropped = sample.Sent - len(answered)
	if len(answered) == 0 {
		sample.Lost = true
		if sample.Sent == 0 {
			sample.Error = "no probes sent"
		}
		return sample
	}
	sort.Float64s(answered)
	sample.RTTMs = answered[len(answered)/2]
	return sample
}
