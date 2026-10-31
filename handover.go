package main

import (
	"context"
	"log"
	"math"
	"sync"
	"time"
)

type HandoverSample struct {
	Boundary        time.Time `json:"boundary"` // 12:27:42:57
	Target          string    `json:"target"`
	StepMs          int       `json:"step_ms"`
	OffsetsMs       []int     `json:"offsets_ms"`
	RTTMs           []float64 `json:"rtt_ms"`
	Lost            int       `json:"lost"`
	Errors          int       `json:"errors,omitempty"`
	FirstLostMs     *int      `json:"first_lost_ms"`
	LastLostMs      *int      `json:"last_lost_ms"`
	LossReachesEdge bool      `json:"loss_reaches_edge"`
}

func (sample HandoverSample) SampleTime() time.Time { return sample.Boundary }

type HandoverProber struct {
	target string
	ping   func(ctx context.Context) PingSample
	emit   func(HandoverSample)
	now    func() time.Time
	sleep  func(ctx context.Context, wait time.Duration) bool
	skips  skipReport
}

func NewHandoverProber(target string, logFile *LogFile[HandoverSample]) *HandoverProber {
	pinger := NewPinger(target, nil, nil)
	pinger.timeout = HopPingTimeout
	return &HandoverProber{
		target: target,
		ping:   pinger.PingOnce,
		emit: func(sample HandoverSample) {
			if err := logFile.Append(sample); err != nil {
				log.Printf("handover log write failed: %v", err)
			}
		},
		now:   time.Now,
		sleep: sleepContext,
		skips: skipReport{name: "handover burst", unit: "bursts"},
	}
}

// Run fires bursts until ctx is cancelled.
func (prober *HandoverProber) Run(ctx context.Context) {
	for {
		boundary := nextHandoverBoundary(prober.now().Add(HandoverBurstBefore))
		burstStart := boundary.Add(-HandoverBurstBefore)
		if !prober.sleep(ctx, burstStart.Sub(prober.now())) {
			return
		}

		lateBy := prober.now().Sub(burstStart)
		if lateBy < -HandoverBurstStep {
			continue
		}
		if lateBy > HandoverBurstMaxLateWake {
			prober.skips.add(prober.now(), 1)
			continue
		}

		sample, ok := prober.burst(ctx, boundary)
		if !ok {
			return
		}
		prober.emit(sample)
		prober.skips.add(prober.now(), 0)
	}
}

// burst sends one echo every HandoverBurstStep around boundary.
func (prober *HandoverProber) burst(ctx context.Context, boundary time.Time) (HandoverSample, bool) {
	count := handoverBurstEchoes()
	burstStart := boundary.Add(-HandoverBurstBefore)
	echoes := make([]PingSample, count)

	var waitGroup sync.WaitGroup
	for index := range count {
		planned := burstStart.Add(time.Duration(index) * HandoverBurstStep)
		if !prober.sleep(ctx, planned.Sub(prober.now())) {
			break
		}
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			echoes[index] = prober.ping(ctx)
		}()
	}
	waitGroup.Wait()

	if ctx.Err() != nil {
		return HandoverSample{}, false
	}
	return summariseBurst(boundary, prober.target, HandoverBurstStep, echoes), true
}

func handoverBurstEchoes() int {
	return int((HandoverBurstBefore+HandoverBurstAfter)/HandoverBurstStep) + 1
}

// nextHandoverBoundary returns the first handover strictly after t.
func nextHandoverBoundary(t time.Time) time.Time {
	unixSeconds := t.Unix()
	sinceBoundary := ((unixSeconds-HandoverBoundaryOffset)%HandoverSlotSeconds + HandoverSlotSeconds) % HandoverSlotSeconds
	lastBoundary := unixSeconds - sinceBoundary
	return time.Unix(lastBoundary+HandoverSlotSeconds, 0)
}

// summariseBurst turns the echoes of one burst into its log line.
func summariseBurst(boundary time.Time, target string, step time.Duration, echoes []PingSample) HandoverSample {
	sample := HandoverSample{
		Boundary:  boundary,
		Target:    target,
		StepMs:    int(step / time.Millisecond),
		OffsetsMs: make([]int, len(echoes)),
		RTTMs:     make([]float64, len(echoes)),
	}
	for index, echo := range echoes {
		offset := int(math.Round(float64(echo.Timestamp.Sub(boundary)) / float64(time.Millisecond)))
		sample.OffsetsMs[index] = offset
		if !echo.Lost {
			sample.RTTMs[index] = echo.RTTMs
			continue
		}
		sample.RTTMs[index] = -1
		sample.Lost++
		if echo.Error != "" {
			sample.Errors++
		}
		if sample.FirstLostMs == nil || offset < *sample.FirstLostMs {
			sample.FirstLostMs = &offset
		}
		if sample.LastLostMs == nil || offset > *sample.LastLostMs {
			sample.LastLostMs = &offset
		}
	}
	if len(echoes) > 0 {
		sample.LossReachesEdge = echoes[0].Lost || echoes[len(echoes)-1].Lost
	}
	return sample
}
