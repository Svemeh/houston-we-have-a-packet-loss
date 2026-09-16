package main

import (
	"context"
	"log"
	"runtime"
	"time"

	probing "github.com/prometheus-community/pro-bing"
)

type PingSample struct {
	Timestamp time.Time `json:"timestamp"`
	Target    string    `json:"target"`
	RTTMs     float64   `json:"rtt_ms,omitempty"` // absent when lost
	Lost      bool      `json:"lost"`
	Error string `json:"error,omitempty"`
}

func (sample PingSample) SampleTime() time.Time { return sample.Timestamp }

// Pinger sends one echo per PingInterval to a single target, publishing each
// result to its hub and log.
type Pinger struct {
	target  string
	hub     *Hub[PingSample]
	logFile *LogFile[PingSample]
	privileged bool
}

func NewPinger(target string, hub *Hub[PingSample], logFile *LogFile[PingSample]) *Pinger {
	return &Pinger{
		target:     target,
		hub:        hub,
		logFile:    logFile,
		privileged: runtime.GOOS == "windows",
	}
}

// Run pings until ctx is cancelled. Errors are logged when they start and when
// they clear, not once a second.
func (pinger *Pinger) Run(ctx context.Context) {
	ticker := time.NewTicker(PingInterval)
	defer ticker.Stop()

	lastError := ""

	pingOnce := func() {
		sample := pinger.PingOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if sample.Error != lastError {
			if sample.Error != "" {
				log.Printf("ping %s: %s", pinger.target, sample.Error)
			} else {
				log.Printf("ping %s: sending again", pinger.target)
			}
			lastError = sample.Error
		}
		pinger.hub.PublishSample(sample)
		if err := pinger.logFile.Append(sample); err != nil {
			log.Printf("ping log write failed: %v", err)
		}
	}

	pingOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingOnce()
		}
	}
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
