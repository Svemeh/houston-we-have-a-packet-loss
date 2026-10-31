package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeClock is a wall clock that only moves when told to. Sleeping advances it
// instantly, so a test can run hours of schedule in microseconds.
type fakeClock struct {
	mutex sync.Mutex
	at    time.Time
}

func (clock *fakeClock) now() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.at
}

func (clock *fakeClock) advance(by time.Duration) {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	clock.at = clock.at.Add(by)
}

func (clock *fakeClock) sleep(ctx context.Context, wait time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if wait > 0 {
		clock.advance(wait)
	}
	return true
}

// cyclingPhases returns the given phases in turn, forever.
func cyclingPhases(phases ...time.Duration) func() time.Duration {
	next := 0
	return func() time.Duration {
		phase := phases[next%len(phases)]
		next++
		return phase
	}
}

func TestSchedulerRunsOncePerSecondAtItsPhase(t *testing.T) {
	start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{at: start.Add(100 * time.Millisecond)}
	phases := []time.Duration{300 * time.Millisecond, 900 * time.Millisecond, 0, 500 * time.Millisecond}
	scheduler := secondScheduler{now: clock.now, sleep: clock.sleep, phase: cyclingPhases(phases...)}

	ctx, cancel := context.WithCancel(context.Background())
	var calls []time.Time
	scheduler.run(ctx, func() {
		calls = append(calls, clock.now())
		clock.advance(50 * time.Millisecond) // a quick round
		if len(calls) == 40 {
			cancel()
		}
	})

	if len(calls) != 40 {
		t.Fatalf("calls = %d, want 40", len(calls))
	}
	for index, call := range calls {
		wantSecond := start.Add(time.Duration(index) * time.Second)
		if got := call.Truncate(time.Second); !got.Equal(wantSecond) {
			t.Fatalf("call %d in second %v, want %v (one call per second, none skipped)", index, got, wantSecond)
		}
		if got, want := call.Sub(wantSecond), phases[index%len(phases)]; got != want {
			t.Errorf("call %d at phase %v, want %v", index, got, want)
		}
	}
}

func TestSchedulerSkipsSecondWhenRoundOverruns(t *testing.T) {
	start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{at: start}
	scheduler := secondScheduler{now: clock.now, sleep: clock.sleep, phase: cyclingPhases(500 * time.Millisecond)}

	ctx, cancel := context.WithCancel(context.Background())
	var calls []time.Time
	scheduler.run(ctx, func() {
		calls = append(calls, clock.now())
		if len(calls) == 2 {
			// Round in second 1 runs from 1.5 s to 3.0 s: second 2's send
			// time (2.5 s) passes, second 3's (3.5 s) doesn't.
			clock.advance(1500 * time.Millisecond)
		}
		if len(calls) == 4 {
			cancel()
		}
	})

	want := []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond, 3500 * time.Millisecond, 4500 * time.Millisecond}
	if len(calls) != len(want) {
		t.Fatalf("calls = %d, want %d", len(calls), len(want))
	}
	for index, call := range calls {
		if got := call.Sub(start); got != want[index] {
			t.Errorf("call %d at %v, want %v", index, got, want[index])
		}
	}
	if scheduler.skips.skipped != 1 {
		t.Errorf("skipped = %d, want 1", scheduler.skips.skipped)
	}
}

func TestSchedulerResyncsAfterLongSleep(t *testing.T) {
	start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{at: start}
	scheduler := secondScheduler{now: clock.now, phase: cyclingPhases(200 * time.Millisecond)}
	slept := false
	scheduler.sleep = func(ctx context.Context, wait time.Duration) bool {
		if !slept && wait > 0 {
			slept = true
			clock.advance(wait + 2*time.Hour) // laptop lid closed mid-wait
			return ctx.Err() == nil
		}
		return clock.sleep(ctx, wait)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var calls []time.Time
	scheduler.run(ctx, func() {
		calls = append(calls, clock.now())
		cancel()
	})

	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if got, want := calls[0], start.Add(2*time.Hour+200*time.Millisecond); !got.Equal(want) {
		t.Errorf("first call after wake at %v, want %v (current second, no backlog)", got, want)
	}
	if scheduler.skips.skipped != 0 {
		t.Errorf("sleep counted as %d skips, want 0", scheduler.skips.skipped)
	}
}

func TestSkipReportLogsHourlyAndResets(t *testing.T) {
	start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	report := skipReport{name: "test", unit: "seconds"}
	report.add(start, 2)
	report.add(start.Add(30*time.Minute), 1)
	if report.skipped != 3 {
		t.Fatalf("skipped = %d, want 3 before the hour is up", report.skipped)
	}
	report.add(start.Add(time.Hour), 0)
	if report.skipped != 0 || !report.windowStart.Equal(start.Add(time.Hour)) {
		t.Errorf("after an hour: skipped %d, window %v; want reset", report.skipped, report.windowStart)
	}
}
