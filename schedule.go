package main

import (
	"context"
	"log"
	"math/rand/v2"
	"time"
)

// runEverySecondRandomPhase calls fn once per wall-clock second, at a random point in that second.
func runEverySecondRandomPhase(ctx context.Context, name string, fn func()) {
	scheduler := secondScheduler{
		now:   time.Now,
		sleep: sleepContext,
		phase: randomPhase,
		skips: skipReport{name: name, unit: "seconds"},
	}
	scheduler.run(ctx, fn)
}

type secondScheduler struct {
	now   func() time.Time
	sleep func(ctx context.Context, wait time.Duration) bool
	phase func() time.Duration
	skips skipReport
}

func (scheduler *secondScheduler) run(ctx context.Context, fn func()) {
	second, target := scheduler.anchor(scheduler.now())
	for {
		now := scheduler.now()
		if target.Sub(now) > SchedulerResyncAfter {
			second, target = scheduler.anchor(now)
			continue
		}
		if !scheduler.sleep(ctx, target.Sub(now)) {
			return
		}

		now = scheduler.now()
		if now.Sub(target) > SchedulerResyncAfter {
			second, target = scheduler.anchor(now)
			continue
		}
		if now.Before(target) {
			continue
		}

		fn()
		second, target = scheduler.next(second)
	}
}

// anchor picks the first second whose send time is still ahead of now.
func (scheduler *secondScheduler) anchor(now time.Time) (second, target time.Time) {
	second = now.Truncate(time.Second)
	target = second.Add(scheduler.phase())
	if target.Before(now) {
		second = second.Add(time.Second)
		target = second.Add(scheduler.phase())
	}
	return second, target
}

// next returns the next second whose send time hasn't passed, skipping any that have.
func (scheduler *secondScheduler) next(second time.Time) (time.Time, time.Time) {
	now := scheduler.now()
	if now.Sub(second) > SchedulerResyncAfter {
		return scheduler.anchor(now)
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

// sleepContext waits for wait or until ctx is cancelled; false if cancelled
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
