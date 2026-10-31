package main

import (
	"context"
	"log"
	"math/rand/v2"
	"time"
)

// runEverySecondRandomPhase calls fn once per wall-clock second, at a random
// point inside that second, until ctx is cancelled.
//
// A time.Ticker locks the send time to one sub-second phase that only drifts
// with the clock, so a 1/s pinger can miss the ~200 ms handover window for
// days and every loss and latency figure ends up biased by where that phase
// happens to sit. A fresh random phase each second turns the samples into an
// unbiased time average, the same today as after next week's restart.
func runEverySecondRandomPhase(ctx context.Context, name string, fn func()) {
	scheduler := secondScheduler{
		now:   time.Now,
		sleep: sleepContext,
		phase: randomPhase,
		skips: skipReport{name: name, unit: "seconds"},
	}
	scheduler.run(ctx, fn)
}

// secondScheduler is runEverySecondRandomPhase with its clock, sleep and
// randomness swappable, so tests can run hours of schedule in microseconds.
type secondScheduler struct {
	now   func() time.Time
	sleep func(ctx context.Context, wait time.Duration) bool // false when ctx was cancelled
	phase func() time.Duration                               // in [0, 1s)
	skips skipReport
}

func (scheduler *secondScheduler) run(ctx context.Context, fn func()) {
	second, target := scheduler.anchor(scheduler.now())
	for {
		now := scheduler.now()
		if target.Sub(now) > SchedulerResyncAfter {
			// The wall clock was stepped back: waiting for the old target
			// would stall pinging for as long as the step.
			second, target = scheduler.anchor(now)
			continue
		}
		if !scheduler.sleep(ctx, target.Sub(now)) {
			return
		}

		now = scheduler.now()
		if now.Sub(target) > SchedulerResyncAfter {
			// The machine slept or stalled. Start again from the current
			// second rather than replaying or counting the gap.
			second, target = scheduler.anchor(now)
			continue
		}
		if now.Before(target) {
			continue // woke early by wall-clock time; wait the rest
		}

		fn()
		second, target = scheduler.next(second)
	}
}

// anchor picks the first second whose send time is still ahead of now.
func (scheduler *secondScheduler) anchor(now time.Time) (second, target time.Time) {
	second = now.Truncate(time.Second) // absolute seconds, independent of time zone
	target = second.Add(scheduler.phase())
	if target.Before(now) {
		second = second.Add(time.Second)
		target = second.Add(scheduler.phase())
	}
	return second, target
}

// next moves on from the second that just ran. A second whose send time went
// by while fn was still running is skipped, never given a new phase: redrawing
// would push late phases into exactly the seconds after a slow round.
func (scheduler *secondScheduler) next(second time.Time) (time.Time, time.Time) {
	now := scheduler.now()
	if now.Sub(second) > SchedulerResyncAfter {
		return scheduler.anchor(now) // fn stalled far longer than any timeout allows
	}
	for skipped := 0; ; skipped++ {
		second = second.Add(time.Second)
		target := second.Add(scheduler.phase())
		if !now.After(target) {
			scheduler.skips.add(now, skipped)
			return second, target
		}
	}
}

func randomPhase() time.Duration { return rand.N(time.Second) }

// sleepContext waits for the given duration or until ctx is cancelled, and
// reports whether it ran the full wait.
func sleepContext(ctx context.Context, wait time.Duration) bool {
	if wait <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// skipReport counts skipped runs and logs the total at most once per
// SkipReportInterval, so a lossy evening can't flood the log.
type skipReport struct {
	name        string
	unit        string
	skipped     int
	windowStart time.Time
}

func (report *skipReport) add(now time.Time, skipped int) {
	if report.windowStart.IsZero() {
		report.windowStart = now
	}
	report.skipped += skipped
	if now.Sub(report.windowStart) < SkipReportInterval {
		return
	}
	if report.skipped > 0 {
		log.Printf("%s: skipped %d %s in the last %s", report.name, report.skipped, report.unit,
			now.Sub(report.windowStart).Round(time.Minute))
	}
	report.skipped, report.windowStart = 0, now
}
