package main

import (
	"context"
	"log"
	"sync"
	"time"
)

// HopResult is one echo to one hop. RTTMs is always written, 0 when the echo
// was lost: omitting it would make a genuine 0 ms reply indistinguishable from
// a missing reading, which the first three days of logs managed to produce.
type HopResult struct {
	RTTMs float64 `json:"rtt_ms"`
	Lost  bool    `json:"lost"`
	Error string  `json:"error,omitempty"`
}

// HopSample is one round of echoes to every hop, sent at the same moment.
//
// The JSON tags are the on-disk log format (hops.jsonl).
type HopSample struct {
	Timestamp time.Time            `json:"timestamp"`
	Hops      map[string]HopResult `json:"hops"`
}

func (sample HopSample) SampleTime() time.Time { return sample.Timestamp }

// HopPinger pings every target in parallel once per PingInterval and logs the
// round as a single line, so every hop in a line shares the same second.
type HopPinger struct {
	pingers []*Pinger
	hub     *Hub[HopSample]
	logFile *LogFile[HopSample]
}

func NewHopPinger(targets []string, hub *Hub[HopSample], logFile *LogFile[HopSample]) *HopPinger {
	pingers := make([]*Pinger, 0, len(targets))
	for _, target := range targets {
		pinger := NewPinger(target, nil, nil)
		pinger.timeout = HopPingTimeout
		pingers = append(pingers, pinger)
	}
	return &HopPinger{pingers: pingers, hub: hub, logFile: logFile}
}

// Run pings until ctx is cancelled. Like Pinger.Run, errors are logged per hop
// when they start and when they clear.
func (hopPinger *HopPinger) Run(ctx context.Context) {
	ticker := time.NewTicker(PingInterval)
	defer ticker.Stop()

	lastErrors := make(map[string]string, len(hopPinger.pingers))

	pingRound := func() {
		sample := hopPinger.PingAll(ctx)
		if ctx.Err() != nil {
			return
		}
		for target, result := range sample.Hops {
			if result.Error == lastErrors[target] {
				continue
			}
			if result.Error != "" {
				log.Printf("hop %s: %s", target, result.Error)
			} else {
				log.Printf("hop %s: sending again", target)
			}
			lastErrors[target] = result.Error
		}
		hopPinger.hub.PublishSample(sample)
		if err := hopPinger.logFile.Append(sample); err != nil {
			log.Printf("hops log write failed: %v", err)
		}
	}

	pingRound()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingRound()
		}
	}
}

// PingAll sends one echo to every hop at once and waits for all of them.
func (hopPinger *HopPinger) PingAll(ctx context.Context) HopSample {
	sample := HopSample{Timestamp: time.Now(), Hops: make(map[string]HopResult, len(hopPinger.pingers))}
	results := make([]PingSample, len(hopPinger.pingers))

	var waitGroup sync.WaitGroup
	for index, pinger := range hopPinger.pingers {
		waitGroup.Add(1)
		go func(index int, pinger *Pinger) {
			defer waitGroup.Done()
			results[index] = pinger.PingOnce(ctx)
		}(index, pinger)
	}
	waitGroup.Wait()

	for _, result := range results {
		sample.Hops[result.Target] = HopResult{RTTMs: result.RTTMs, Lost: result.Lost, Error: result.Error}
	}
	return sample
}

// HopBucket is one point of hop history: per target, the worst RTT in the
// bucket and how many echoes were sent and lost. Loss stays as counts rather
// than a percentage so it merges correctly at any zoom, same as PingBucket.
type HopBucket struct {
	Timestamp time.Time                 `json:"timestamp"`
	Hops      map[string]HopBucketStats `json:"hops"`
}

type HopBucketStats struct {
	MaxRTTMs float64 `json:"rtt_ms"` // 0 when every echo in the bucket was lost
	Sent     int     `json:"sent"`
	Lost     int     `json:"lost"`
}

// DownsampleHops collapses samples into at most maxPoints time buckets. Like
// the other downsamplers, buckets are cut by time so a stretch with no samples
// stays a gap, and RTT keeps the peak rather than the mean.
func DownsampleHops(samples []HopSample, windowStart, windowEnd time.Time, maxPoints int) ([]HopBucket, time.Duration) {
	bucketWidth := time.Duration(0) // zero: one bucket per sample
	if windowSpan := windowEnd.Sub(windowStart); maxPoints > 0 && len(samples) > maxPoints && windowSpan > 0 {
		bucketWidth = windowSpan / time.Duration(maxPoints)
	}

	buckets := make([]HopBucket, 0, min(len(samples), maxPoints+1))
	currentBucketIndex := int64(-1)

	for sampleIndex, sample := range samples {
		bucketIndex := int64(sampleIndex)
		if bucketWidth > 0 {
			bucketIndex = int64(sample.Timestamp.Sub(windowStart) / bucketWidth)
		}
		if bucketIndex != currentBucketIndex {
			buckets = append(buckets, HopBucket{
				Timestamp: sample.Timestamp,
				Hops:      make(map[string]HopBucketStats, len(sample.Hops)),
			})
			currentBucketIndex = bucketIndex
		}

		bucket := &buckets[len(buckets)-1]
		for target, result := range sample.Hops {
			stats := bucket.Hops[target]
			stats.Sent++
			if result.Lost {
				stats.Lost++
			} else if result.RTTMs > stats.MaxRTTMs {
				stats.MaxRTTMs = result.RTTMs
			}
			bucket.Hops[target] = stats
		}
	}

	if bucketWidth == 0 {
		bucketWidth = PingInterval
	}
	return buckets, bucketWidth
}
