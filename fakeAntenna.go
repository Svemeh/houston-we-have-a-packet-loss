package main

// this file is purely for testing the program when no starlink is available to test on.
//
// It deliberately misbehaves on a schedule: every minute or two it drops into a
// degraded, obstructed, no-signal or unreachable stretch, so every branch of the
// alarm strip and every colour on the charts actually gets hit. Values run
// through deriveLinkState exactly as real ones do, so the heuristic under test
// is the real one.

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"time"
)

const (
	fakeConditionNominal     = "nominal"
	fakeConditionDegraded    = "degraded"
	fakeConditionObstructed  = "obstructed"
	fakeConditionNoSignal    = "nosignal"
	fakeConditionUnreachable = "unreachable" // Collect returns an error -> OFFLINE
)

const (
	fakeBaseLatencyMs = 38
	fakeBaseDownload  = 180
	fakeBaseUpload    = 15

	fakeAssumedUptime = 61 * time.Hour
)

type FakeCollector struct {
	startedAt      time.Time
	randomSource   *rand.Rand
	condition      string
	conditionUntil time.Time
}

func NewFakeCollector() *FakeCollector {
	now := time.Now()
	log.Print("⚠ -fake: telemetry is synthetic, the dish is not being contacted")
	return &FakeCollector{
		startedAt: now.Add(-fakeAssumedUptime),
		// Not safe for concurrent use, which is fine: only pollLoop calls Collect.
		randomSource:   rand.New(rand.NewSource(now.UnixNano())),
		condition:      fakeConditionNominal,
		conditionUntil: now.Add(30 * time.Second),
	}
}

func (fake *FakeCollector) Close() error { return nil }

func (fake *FakeCollector) Collect(ctx context.Context) (TelemetrySample, error) {
	now := time.Now()
	fake.advanceCondition(now)

	if fake.condition == fakeConditionUnreachable {
		return TelemetrySample{}, fmt.Errorf("fake dish unreachable (simulated)")
	}

	phase := float64(now.UnixNano()) / float64(time.Second)
	latencyMs := fakeBaseLatencyMs + 6*math.Sin(phase/17) + fake.randomSource.NormFloat64()*2
	downloadMbps := fakeBaseDownload + 55*math.Sin(phase/29) + fake.randomSource.NormFloat64()*9
	uploadMbps := fakeBaseUpload + 5*math.Sin(phase/23) + fake.randomSource.NormFloat64()*1.5

	dropRateFraction := 0.0
	isObstructed := false
	obstructionFraction := 0.004 + fake.randomSource.Float64()*0.002

	switch fake.condition {
	case fakeConditionDegraded:
		latencyMs += 40 + fake.randomSource.Float64()*90
		dropRateFraction = 0.03 + fake.randomSource.Float64()*0.15
		downloadMbps *= 0.5
	case fakeConditionObstructed:
		isObstructed = true
		latencyMs += 25
		dropRateFraction = fake.randomSource.Float64() * 0.4
		downloadMbps *= 0.3
		obstructionFraction += 0.05
	case fakeConditionNoSignal:
		dropRateFraction = DropRateNoSignalThreshold
		latencyMs = 0 // the dashboard skips zero latency rather than plotting a floor
		downloadMbps, uploadMbps = 0, 0
	}

	return TelemetrySample{
		Timestamp:           now,
		LinkState:           deriveLinkState(dropRateFraction, isObstructed),
		LatencyMs:           math.Max(0, latencyMs),
		DownloadMbps:        math.Max(0, downloadMbps),
		UploadMbps:          math.Max(0, uploadMbps),
		DropRateFraction:    dropRateFraction,
		Obstructed:          isObstructed,
		ObstructionFraction: obstructionFraction,
		UptimeSeconds:       uint64(now.Sub(fake.startedAt).Seconds()),
		HardwareVersion:     "rev3_proto2 (fake)",
		SoftwareVersion:     "2025.02.14.mr30330 (fake)",
	}, nil
}

func (fake *FakeCollector) advanceCondition(now time.Time) {
	if now.Before(fake.conditionUntil) {
		return
	}
	if fake.condition != fakeConditionNominal {
		fake.condition = fakeConditionNominal
		fake.conditionUntil = now.Add(fake.randomSeconds(40, 160))
		return
	}
	switch roll := fake.randomSource.Float64(); {
	case roll < 0.40:
		fake.condition = fakeConditionDegraded
		fake.conditionUntil = now.Add(fake.randomSeconds(4, 15))
	case roll < 0.70:
		fake.condition = fakeConditionObstructed
		fake.conditionUntil = now.Add(fake.randomSeconds(3, 10))
	case roll < 0.88:
		fake.condition = fakeConditionNoSignal
		fake.conditionUntil = now.Add(fake.randomSeconds(2, 5))
	default:
		fake.condition = fakeConditionUnreachable
		fake.conditionUntil = now.Add(fake.randomSeconds(3, 8))
	}
}

func (fake *FakeCollector) randomSeconds(minSeconds, maxSeconds int) time.Duration {
	return time.Duration(minSeconds+fake.randomSource.Intn(maxSeconds-minSeconds+1)) * time.Second
}
