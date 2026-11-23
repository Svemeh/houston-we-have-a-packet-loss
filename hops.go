package main

import (
	"context"
	"log"
	"slices"
	"sync"
	"time"
)

// HopResult is one echo to one hop. RTTMs is always written, 0 when the echo
// was lost: omitting it would make a genuine 0 ms reply indistinguishable from
// a missing reading, which the first three days of logs managed to produce.
type HopResult struct {
	RTTMs   float64 `json:"rtt_ms"`
	Lost    bool    `json:"lost"`
	Error   string  `json:"error,omitempty"`
	Sent    int     `json:"sent,omitempty"`
	Dropped int     `json:"dropped,omitempty"`
	PoPUp   *bool   `json:"pop_answered,omitempty"`
}

func (result HopResult) Probes() (sent, lost int) {
	if result.Sent > 0 {
		return result.Sent, result.Dropped
	}
	if result.Lost {
		return 1, 1
	}
	return 1, 0
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
	mutex   sync.Mutex
	pingers []hopProbe
	hub     *Hub[HopSample]
	logFile *LogFile[HopSample]
}

func NewHopPinger(targets []string, hub *Hub[HopSample], logFile *LogFile[HopSample], pair func(context.Context) (bool, bool)) *HopPinger {
	pingers := make([]hopProbe, 0, len(targets))
	for _, target := range targets {
		switch {
		case slices.Contains(HopLocalTargets, target):
			pingers = append(pingers, NewFastProbe(target, NewPinger(target).PingWithin))
		case pair != nil:
			pingers = append(pingers, &PairedPinger{pinger: NewPinger(target), pair: pair})
		default:
			pingers = append(pingers, NewPinger(target))
		}
	}
	return &HopPinger{pingers: pingers, hub: hub, logFile: logFile}
}

type PairedPinger struct {
	pinger hopProbe
	pair   func(context.Context) (bool, bool)
}

func (paired *PairedPinger) PingOnce(ctx context.Context) PingSample {
	var answered, available bool
	done := make(chan struct{})
	go func() {
		answered, available = paired.pair(ctx)
		close(done)
	}()
	sample := paired.pinger.PingOnce(ctx)
	<-done
	if available {
		sample.PoPUp = &answered
	}
	return sample
}

type backgroundProbe interface {
	Run(ctx context.Context)
}

type hopProbe interface {
	PingOnce(ctx context.Context) PingSample
}

func (hopPinger *HopPinger) Add(probe hopProbe) {
	hopPinger.mutex.Lock()
	defer hopPinger.mutex.Unlock()
	hopPinger.pingers = append(hopPinger.pingers, probe)
}

// Run pings until ctx is cancelled. Errors are logged per hop when they start
// and when they clear.
func (hopPinger *HopPinger) Run(ctx context.Context) {
	lastErrors := make(map[string]string)
	hopPinger.mutex.Lock()
	for _, pinger := range hopPinger.pingers {
		if background, ok := pinger.(backgroundProbe); ok {
			go background.Run(ctx)
		}
	}
	hopPinger.mutex.Unlock()

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

	runEverySecondRandomPhase(ctx, "hops", pingRound)
}

// PingAll sends one echo to every hop at once and waits for all of them.
func (hopPinger *HopPinger) PingAll(ctx context.Context) HopSample {
	hopPinger.mutex.Lock()
	pingers := append([]hopProbe(nil), hopPinger.pingers...)
	hopPinger.mutex.Unlock()

	sample := HopSample{Timestamp: time.Now(), Hops: make(map[string]HopResult, len(pingers))}
	results := make([]PingSample, len(pingers))

	var waitGroup sync.WaitGroup
	for index, pinger := range pingers {
		waitGroup.Add(1)
		go func(index int, pinger hopProbe) {
			defer waitGroup.Done()
			results[index] = pinger.PingOnce(ctx)
		}(index, pinger)
	}
	waitGroup.Wait()

	for _, result := range results {
		sample.Hops[result.Target] = HopResult{RTTMs: result.RTTMs, Lost: result.Lost, Error: result.Error, Sent: result.Sent, Dropped: result.Dropped, PoPUp: result.PoPUp}
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
	MaxRTTMs    float64 `json:"rtt_ms"` // 0 when every echo in the bucket was lost
	Sent        int     `json:"sent"`
	Lost        int     `json:"lost"`
	LossSeconds int     `json:"loss_seconds"`
	LostPoPUp   int     `json:"lost_pop_up,omitempty"`
	LostPoPDown int     `json:"lost_pop_down,omitempty"`
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
			probesSent, probesLost := result.Probes()
			stats.Sent += probesSent
			stats.Lost += probesLost
			if probesLost > 0 {
				stats.LossSeconds++
				if result.PoPUp != nil && *result.PoPUp {
					stats.LostPoPUp++
				} else if result.PoPUp != nil {
					stats.LostPoPDown++
				}
			}
			if !result.Lost && result.RTTMs > stats.MaxRTTMs {
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
