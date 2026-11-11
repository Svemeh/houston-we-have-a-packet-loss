package main

import (
	"context"
	"runtime"
	"time"

	probing "github.com/prometheus-community/pro-bing"
)

type PingSample struct {
	Timestamp time.Time `json:"timestamp"`
	Target    string    `json:"target"`
	RTTMs     float64   `json:"rtt_ms,omitempty"` // absent when lost
	Lost      bool      `json:"lost"`
	Error     string    `json:"error,omitempty"`
}

func (sample PingSample) SampleTime() time.Time { return sample.Timestamp }

type Pinger struct {
	target     string
	privileged bool
}

func NewPinger(target string) *Pinger {
	return &Pinger{target: target, privileged: runtime.GOOS == "windows"}
}

// PingOnce sends a single echo and waits up to PingTimeout for the reply.
func (pinger *Pinger) PingOnce(ctx context.Context) PingSample {
	sample := PingSample{Timestamp: time.Now(), Target: pinger.target}

	echo := probing.New(pinger.target)
	echo.Count = 1
	echo.Timeout = PingTimeout
	echo.SetPrivileged(pinger.privileged)
	echo.SetLogger(probing.NoopLogger{})

	if err := echo.RunWithContext(ctx); err != nil {
		sample.Lost, sample.Error = true, err.Error()
		return sample
	}
	stats := echo.Statistics()
	if stats.PacketsRecv == 0 {
		sample.Lost = true
		return sample
	}
	sample.RTTMs = float64(stats.MaxRtt.Microseconds()) / 1000
	return sample
}

// pingSampleFrom picks target's echo out of a hop round; false when the round didn't ping it.
func pingSampleFrom(round HopSample, target string) (PingSample, bool) {
	result, measured := round.Hops[target]
	if !measured {
		return PingSample{}, false
	}
	return PingSample{Timestamp: round.Timestamp, Target: target, RTTMs: result.RTTMs, Lost: result.Lost, Error: result.Error}, true
}

// pingSamplesFrom picks target's echoes out of hop rounds.
func pingSamplesFrom(rounds []HopSample, target string) []PingSample {
	samples := make([]PingSample, 0, len(rounds))
	for _, round := range rounds {
		if sample, measured := pingSampleFrom(round, target); measured {
			samples = append(samples, sample)
		}
	}
	return samples
}

type hopTargetSource struct {
	hops   SampleSource[HopSample]
	target string
}

// Samples reads one target's echoes back out of the hop log.
func (source hopTargetSource) Samples(ctx context.Context, query SampleQuery) ([]PingSample, error) {
	rounds, err := source.hops.Samples(ctx, query)
	if err != nil {
		return nil, err
	}
	return pingSamplesFrom(rounds, source.target), nil
}

// relayHopTarget republishes target's echo from every hop round to pingHub.
func relayHopTarget(ctx context.Context, hopsHub *Hub[HopSample], pingHub *Hub[PingSample], target string) {
	rounds, _, unsubscribe := hopsHub.Subscribe()
	defer unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case round, open := <-rounds:
			if !open {
				return
			}
			if sample, measured := pingSampleFrom(round, target); measured {
				pingHub.PublishSample(sample)
			}
		}
	}
}

// PingBucket is one point of ping history: the worst RTT in a time bucket and
// how many echoes in it were sent and lost. Loss % is Lost/Sent, which stays
// correct at any zoom — unlike a stored percentage, which can't be merged.
type PingBucket struct {
	Timestamp time.Time `json:"timestamp"`
	MaxRTTMs  float64   `json:"rtt_ms"` // 0 when every echo in the bucket was lost
	Sent      int       `json:"sent"`
	Lost      int       `json:"lost"`
}

// DownsamplePing collapses samples into at most maxPoints time buckets. Like
// DownsampleWorstCase, buckets are cut by time so an outage with no records
// stays a gap, and RTT keeps the peak rather than the mean.
func DownsamplePing(samples []PingSample, windowStart, windowEnd time.Time, maxPoints int) ([]PingBucket, time.Duration) {
	bucketWidth := time.Duration(0) // zero: one bucket per sample
	if windowSpan := windowEnd.Sub(windowStart); maxPoints > 0 && len(samples) > maxPoints && windowSpan > 0 {
		bucketWidth = windowSpan / time.Duration(maxPoints)
	}

	buckets := make([]PingBucket, 0, min(len(samples), maxPoints+1))
	currentBucketIndex := int64(-1)

	for sampleIndex, sample := range samples {
		bucketIndex := int64(sampleIndex)
		if bucketWidth > 0 {
			bucketIndex = int64(sample.Timestamp.Sub(windowStart) / bucketWidth)
		}
		if bucketIndex != currentBucketIndex {
			buckets = append(buckets, PingBucket{Timestamp: sample.Timestamp})
			currentBucketIndex = bucketIndex
		}
		bucket := &buckets[len(buckets)-1]
		bucket.Sent++
		if sample.Lost {
			bucket.Lost++
		} else if sample.RTTMs > bucket.MaxRTTMs {
			bucket.MaxRTTMs = sample.RTTMs
		}
	}

	if bucketWidth == 0 {
		bucketWidth = PingInterval
	}
	return buckets, bucketWidth
}
