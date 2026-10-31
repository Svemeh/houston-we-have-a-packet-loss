package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestNextHandoverBoundary(t *testing.T) {
	// Not a whole-hour offset, so any use of local time would show.
	savedLocal := time.Local
	time.Local = time.FixedZone("UTC+5:30", 5*3600+1800)
	defer func() { time.Local = savedLocal }()

	hour := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) // unix % 15 == 0
	at := func(offset time.Duration) time.Time { return hour.Add(offset) }
	cases := []struct {
		from time.Time
		want time.Time
	}{
		{at(0), at(12 * time.Second)},
		{at(11*time.Second + 999*time.Millisecond), at(12 * time.Second)},
		{at(12 * time.Second), at(27 * time.Second)}, // exactly on a boundary: the next one
		{at(12*time.Second + time.Millisecond), at(27 * time.Second)},
		{at(56*time.Second + 500*time.Millisecond), at(57 * time.Second)},
		{at(57 * time.Second), at(72 * time.Second)},
		{at(59*time.Minute + 58*time.Second), at(time.Hour + 12*time.Second)},
	}
	for _, c := range cases {
		for _, from := range []time.Time{c.from, c.from.In(time.Local)} {
			got := nextHandoverBoundary(from)
			if !got.Equal(c.want) {
				t.Errorf("nextHandoverBoundary(%v) = %v, want %v", from, got.UTC(), c.want)
			}
			if got.Unix()%HandoverSlotSeconds != HandoverBoundaryOffset {
				t.Errorf("boundary %v: unix %% %d = %d, want %d", got, HandoverSlotSeconds, got.Unix()%HandoverSlotSeconds, HandoverBoundaryOffset)
			}
		}
	}
}

// burstEchoes builds a full burst around boundary, sent on plan plus a little
// jitter, with the echoes at the given offsets (ms) lost.
func burstEchoes(boundary time.Time, lostOffsetsMs ...int) []PingSample {
	lost := make(map[int]bool, len(lostOffsetsMs))
	for _, offset := range lostOffsetsMs {
		lost[offset] = true
	}
	var echoes []PingSample
	for offset := -int(HandoverBurstBefore / time.Millisecond); offset <= int(HandoverBurstAfter/time.Millisecond); offset += int(HandoverBurstStep / time.Millisecond) {
		echo := PingSample{Timestamp: boundary.Add(time.Duration(offset)*time.Millisecond + 300*time.Microsecond), Target: HandoverBurstTarget}
		if lost[offset] {
			echo.Lost = true
		} else {
			echo.RTTMs = 40
		}
		echoes = append(echoes, echo)
	}
	return echoes
}

func TestSummariseBurst(t *testing.T) {
	boundary := time.Date(2026, 10, 31, 12, 0, 12, 0, time.UTC)

	clean := summariseBurst(boundary, HandoverBurstTarget, HandoverBurstStep, burstEchoes(boundary))
	if clean.Lost != 0 || clean.FirstLostMs != nil || clean.LastLostMs != nil || clean.LossReachesEdge {
		t.Errorf("clean burst: lost %d first %v last %v edge %v; want 0 nil nil false", clean.Lost, clean.FirstLostMs, clean.LastLostMs, clean.LossReachesEdge)
	}
	if len(clean.OffsetsMs) != handoverBurstEchoes() || clean.OffsetsMs[0] != -600 || clean.OffsetsMs[len(clean.OffsetsMs)-1] != 900 {
		t.Errorf("offsets %v, want %d from -600 to 900", clean.OffsetsMs, handoverBurstEchoes())
	}
	if clean.StepMs != 20 || clean.RTTMs[0] != 40 {
		t.Errorf("step %d rtt[0] %v, want 20 and 40", clean.StepMs, clean.RTTMs[0])
	}

	mid := summariseBurst(boundary, HandoverBurstTarget, HandoverBurstStep, burstEchoes(boundary, -120, -100, -80))
	if mid.Lost != 3 || *mid.FirstLostMs != -120 || *mid.LastLostMs != -80 || mid.LossReachesEdge {
		t.Errorf("mid burst: lost %d first %d last %d edge %v; want 3 -120 -80 false", mid.Lost, *mid.FirstLostMs, *mid.LastLostMs, mid.LossReachesEdge)
	}
	if mid.RTTMs[24] != -1 { // offset -120
		t.Errorf("lost echo rtt = %v, want -1", mid.RTTMs[24])
	}

	edgeEchoes := burstEchoes(boundary, 880, 900)
	edgeEchoes[len(edgeEchoes)-1].Error = "sendmsg: no route to host"
	edge := summariseBurst(boundary, HandoverBurstTarget, HandoverBurstStep, edgeEchoes)
	if edge.Lost != 2 || edge.Errors != 1 || !edge.LossReachesEdge || *edge.LastLostMs != 900 {
		t.Errorf("edge burst: lost %d errors %d edge %v last %d; want 2 1 true 900", edge.Lost, edge.Errors, edge.LossReachesEdge, *edge.LastLostMs)
	}
}

// fakeProber is a HandoverProber on a fake clock whose echoes all answer.
func fakeProber(clock *fakeClock, emit func(HandoverSample)) *HandoverProber {
	return &HandoverProber{
		target: HandoverBurstTarget,
		ping: func(ctx context.Context) PingSample {
			return PingSample{Timestamp: clock.now(), Target: HandoverBurstTarget, RTTMs: 40}
		},
		emit:  emit,
		now:   clock.now,
		sleep: clock.sleep,
	}
}

func TestHandoverProberFiresEveryBoundary(t *testing.T) {
	clock := &fakeClock{at: time.Date(2026, 10, 31, 12, 0, 0, 300_000_000, time.UTC)}
	ctx, cancel := context.WithCancel(context.Background())
	var bursts []HandoverSample
	prober := fakeProber(clock, func(sample HandoverSample) {
		bursts = append(bursts, sample)
		if len(bursts) == 5 {
			cancel()
		}
	})
	prober.Run(ctx)

	if len(bursts) != 5 {
		t.Fatalf("bursts = %d, want 5", len(bursts))
	}
	for index, burst := range bursts {
		want := time.Date(2026, 10, 31, 12, 0, 12+15*index, 0, time.UTC)
		if !burst.Boundary.Equal(want) {
			t.Errorf("burst %d boundary %v, want %v", index, burst.Boundary.UTC(), want)
		}
		if len(burst.OffsetsMs) != handoverBurstEchoes() || burst.Lost != 0 {
			t.Errorf("burst %d: %d echoes, %d lost; want %d, 0", index, len(burst.OffsetsMs), burst.Lost, handoverBurstEchoes())
		}
	}
}

func TestHandoverProberSkipsLateAndSleptBursts(t *testing.T) {
	cases := []struct {
		name      string
		oversleep time.Duration
		want      time.Time
	}{
		{"late wake drops that burst", 200 * time.Millisecond, time.Date(2026, 10, 31, 12, 0, 27, 0, time.UTC)},
		{"no backlog after sleep", time.Hour + 5*time.Second, time.Date(2026, 10, 31, 13, 0, 27, 0, time.UTC)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clock := &fakeClock{at: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
			ctx, cancel := context.WithCancel(context.Background())
			var bursts []HandoverSample
			prober := fakeProber(clock, func(sample HandoverSample) {
				bursts = append(bursts, sample)
				cancel()
			})
			overslept := false
			prober.sleep = func(ctx context.Context, wait time.Duration) bool {
				if !overslept && wait > 0 {
					overslept = true
					clock.advance(wait + c.oversleep)
					return ctx.Err() == nil
				}
				return clock.sleep(ctx, wait)
			}
			prober.Run(ctx)

			if len(bursts) != 1 || !bursts[0].Boundary.Equal(c.want) {
				t.Fatalf("bursts %v, want one at %v", bursts, c.want)
			}
			if prober.skips.skipped != 1 {
				t.Errorf("skipped = %d, want 1", prober.skips.skipped)
			}
		})
	}
}

func TestHandoverProberDropsCancelledBurst(t *testing.T) {
	clock := &fakeClock{at: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	ctx, cancel := context.WithCancel(context.Background())
	emitted := 0
	prober := fakeProber(clock, func(HandoverSample) { emitted++ })
	pings := 0
	prober.ping = func(ctx context.Context) PingSample {
		// Goroutines may run in any order, so count under the clock's lock.
		clock.mutex.Lock()
		pings++
		if pings == 10 {
			cancel()
		}
		clock.mutex.Unlock()
		return PingSample{Timestamp: clock.now(), Lost: true, Error: "context canceled"}
	}
	prober.Run(ctx)
	if emitted != 0 {
		t.Errorf("emitted %d bursts after cancel, want 0", emitted)
	}
}

// burst returns a summarised burst whose echoes at the given offsets were lost.
func burst(boundary time.Time, lostOffsetsMs ...int) HandoverSample {
	return summariseBurst(boundary, HandoverBurstTarget, HandoverBurstStep, burstEchoes(boundary, lostOffsetsMs...))
}

func TestAddHandoverBursts(t *testing.T) {
	hourStart := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	at := func(second int) time.Time { return hourStart.Add(time.Duration(second) * time.Second) }

	sendError := burst(at(57), 0)
	sendError.Errors = 1
	handovers := []HandoverSample{
		burst(at(-3), -100),            // previous hour: ignored
		burst(at(12), -120, -100, -80), // 60 ms
		burst(at(27)),                  // clean
		burst(at(42), -100, 880),       // 20 ms: a stray loss at the edge isn't part of the run
		sendError,                      // this machine's link: counted apart
		burst(at(3612), -100),          // next hour: ignored
	}

	var rollup HopRollup
	addHandoverBursts(&rollup, handovers, hourStart)
	if rollup.HandoverBursts != 3 || rollup.HandoverBurstsWithLoss != 2 || rollup.HandoverLostPings != 5 {
		t.Errorf("bursts/with loss/lost = %d/%d/%d, want 3/2/5", rollup.HandoverBursts, rollup.HandoverBurstsWithLoss, rollup.HandoverLostPings)
	}
	if rollup.HandoverLossMsP50 != 60 || rollup.HandoverLossMsP90 != 60 {
		t.Errorf("loss ms p50/p90 = %v/%v, want 60/60", rollup.HandoverLossMsP50, rollup.HandoverLossMsP90)
	}
	if rollup.HandoverBurstsSendError != 1 {
		t.Errorf("send-error bursts = %d, want 1", rollup.HandoverBurstsSendError)
	}
}

func TestAddHandoverBurstsWithoutLog(t *testing.T) {
	hourStart := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	handovers, err := LoadSamplesSince[HandoverSample](filepath.Join(t.TempDir(), "handovers.jsonl"), hourStart)
	if err != nil {
		t.Fatalf("missing log should not be an error: %v", err)
	}
	var rollup HopRollup
	addHandoverBursts(&rollup, handovers, hourStart)
	if rollup.HandoverBursts != 0 || rollup.HandoverBurstsWithLoss != 0 || rollup.HandoverLostPings != 0 ||
		rollup.HandoverLossMsP50 != 0 || rollup.HandoverLossMsP90 != 0 || rollup.HandoverBurstsSendError != 0 {
		t.Errorf("no handovers.jsonl: got %+v, want zeros", rollup)
	}
}

func TestSamplingMode(t *testing.T) {
	hourStart := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	firstBurst := []HandoverSample{burst(hourStart.Add(12 * time.Second))}
	laterBurst := []HandoverSample{burst(hourStart.Add(27 * time.Second))}

	cases := []struct {
		name      string
		started   time.Time
		handovers []HandoverSample
		want      string
	}{
		{"hour after process start", hourStart.Add(-time.Minute), nil, SamplingRandomPhase},
		{"hour starts as process starts", hourStart, nil, SamplingRandomPhase},
		{"old hour, burst at first handover", hourStart.Add(2 * time.Hour), firstBurst, SamplingRandomPhase},
		{"old hour, upgraded mid-hour", hourStart.Add(2 * time.Hour), laterBurst, SamplingUnknown},
		{"old hour, no bursts", hourStart.Add(2 * time.Hour), nil, SamplingUnknown},
	}
	for _, c := range cases {
		if got := samplingMode(hourStart, c.started, c.handovers); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
