package main

import (
	"context"
	"log"
	"math"
	"sync"
	"time"
)

// HandoverSample is one burst of echoes around one satellite handover.
//
// The JSON tags are the on-disk log format (handovers.jsonl). Offsets are
// milliseconds relative to Boundary, taken from each echo's real send time.
type HandoverSample struct {
	Boundary  time.Time `json:"boundary"` // the :12/:27/:42/:57 instant
	Target    string    `json:"target"`
	StepMs    int       `json:"step_ms"`
	OffsetsMs []int     `json:"offsets_ms"`
	RTTMs     []float64 `json:"rtt_ms"` // -1 when lost
	Lost      int       `json:"lost"`
	// Errors counts echoes that never left this machine (e.g. "no route to
	// host"). They are in Lost too; a burst with errors says more about the
	// local link than about the handover.
	Errors          int  `json:"errors,omitempty"`
	FirstLostMs     *int `json:"first_lost_ms"`
	LastLostMs      *int `json:"last_lost_ms"`
	LossReachesEdge bool `json:"loss_reaches_edge"` // first or last echo lost: the window may be wider than the burst
}

func (sample HandoverSample) SampleTime() time.Time { return sample.Boundary }

// HandoverProber fires one burst of echoes around every Starlink handover.
//
// Handovers are global and fixed to the wall clock (UTC), so the prober is too:
// it plans each burst from the unix time, not from a ticker. A burst it can't
// start on time is dropped whole, because a partial burst would report a
// narrower loss window than the one that happened.
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
		// The next boundary whose burst hasn't started yet. Picked from the
		// current time every round, so waking from sleep never replays a
		// backlog of missed handovers.
		boundary := nextHandoverBoundary(prober.now().Add(HandoverBurstBefore))
		burstStart := boundary.Add(-HandoverBurstBefore)
		if !prober.sleep(ctx, burstStart.Sub(prober.now())) {
			return
		}

		lateBy := prober.now().Sub(burstStart)
		if lateBy < -HandoverBurstStep {
			continue // woke early: the wall clock was stepped back; plan again
		}
		if lateBy > HandoverBurstMaxLateWake {
			prober.skips.add(prober.now(), 1)
			continue
		}

		sample, ok := prober.burst(ctx, boundary)
		if !ok {
			return // cancelled mid-burst: drop it rather than log a half burst
		}
		prober.emit(sample)
		prober.skips.add(prober.now(), 0)
	}
}

// burst sends one echo every HandoverBurstStep from HandoverBurstBefore ahead
// of the boundary to HandoverBurstAfter past it. Each echo runs on its own, so
// a lost one waiting out its timeout doesn't hold back the ones after it.
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

// nextHandoverBoundary returns the first handover strictly after t: the next
// unix second where seconds % HandoverSlotSeconds == HandoverBoundaryOffset.
// Unix seconds are UTC by definition, so time.Local plays no part.
func nextHandoverBoundary(t time.Time) time.Time {
	unixSeconds := t.Unix() // floor for any time after 1970
	sinceBoundary := ((unixSeconds-HandoverBoundaryOffset)%HandoverSlotSeconds + HandoverSlotSeconds) % HandoverSlotSeconds
	lastBoundary := unixSeconds - sinceBoundary // at or before t
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
