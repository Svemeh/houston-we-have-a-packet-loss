package main

import (
	"context"
	"log"
	"math"
	"sort"
	"time"
)

// HopRollup is one hour of hops.jsonl boiled down to a single line: enough to
// see how the link behaved, small enough to keep forever.
//
// Raw samples are pruned at LogRetention, so anything not summarised here is
// gone three days later. The fields are the ones that turned out to matter:
// loss per target, the RTT distribution rather than the mean, and where in the
// 15-second satellite slot the loss landed.
//
// The JSON tags are the on-disk log format (hopsHourly.jsonl).
type HopRollup struct {
	HourStart time.Time               `json:"hour_start"`
	Samples   int                     `json:"samples"` // rounds in the hour; 3600 when nothing was missed
	Hops      map[string]HopHourStats `json:"hops"`

	// HandoverLoss and HandoverSent are indexed by second within the
	// HandoverSlotSeconds slot (timestamp second modulo the slot length), and
	// count echoes to HopEndTarget. Loss concentrated on one index is the
	// satellite handover.
	HandoverLoss []int `json:"handover_loss"`
	HandoverSent []int `json:"handover_sent"`

	// Same buckets, but counting seconds the dish itself reported any drop.
	// The dish reports ~3s late, so these lag HandoverLoss by that much.
	DishDropSlots   []int `json:"dish_drop_slots"`
	DishSlotSamples []int `json:"dish_slot_samples"`

	// From handovers.jsonl: one burst per handover. Bursts with local send
	// errors are counted apart and left out of the rest, since their loss is
	// this machine's link, not the handover. Loss duration per burst is
	// last_lost_ms - first_lost_ms + step_ms, over bursts with loss.
	HandoverBursts          int     `json:"handover_bursts"`
	HandoverBurstsWithLoss  int     `json:"handover_bursts_with_loss"`
	HandoverLostPings       int     `json:"handover_lost_pings"`
	HandoverLossMsP50       float64 `json:"handover_loss_ms_p50"`
	HandoverLossMsP90       float64 `json:"handover_loss_ms_p90"`
	HandoverBurstsSendError int     `json:"handover_bursts_send_error"`

	// Sampling is SamplingRandomPhase when the hour's 1/s samples provably
	// came from the random-phase scheduler, SamplingUnknown otherwise. Lines
	// from before the change don't have it. Only random-phase hours are
	// comparable for overall loss and latency.
	Sampling string `json:"sampling"`
}

const (
	SamplingRandomPhase = "random_phase"
	SamplingUnknown     = "unknown"
)

// processStart is when this process began; any hour starting after it was
// sampled entirely by this binary.
var processStart = time.Now()

func (rollup HopRollup) SampleTime() time.Time { return rollup.HourStart }

// HopHourStats is one target's hour. Percentiles rather than a mean: the mean
// RTT barely moves while the link falls apart, and p99 is what a game feels.
type HopHourStats struct {
	Sent        int     `json:"sent"`
	Lost        int     `json:"lost"`
	P50Ms       float64 `json:"p50_ms"`
	P90Ms       float64 `json:"p90_ms"`
	P99Ms       float64 `json:"p99_ms"`
	MaxMs       float64 `json:"max_ms"`
	JitterP99Ms float64 `json:"jitter_p99_ms"` // 99th percentile of |RTT - previous RTT|
}

// rollupLoop summarises each complete hour of hops.jsonl into the rollup log.
//
// It reads the hop log rather than counting in memory, so a restart loses
// nothing: the samples are already on disk, and any hour missed while the app
// was down is picked up on the next tick. Hours already summarised are skipped,
// which is why it ticks faster than once an hour — a restart shouldn't wait.
func rollupLoop(ctx context.Context, hopsLogPath, dishLogPath, handoverLogPath string, rollupLogFile *LogFile[HopRollup], rollupLogPath string) {
	ticker := time.NewTicker(RollupInterval)
	defer ticker.Stop()

	summarised := make(map[int64]struct{})
	if prior, err := LoadSamplesSince[HopRollup](rollupLogPath, time.Time{}); err != nil {
		log.Printf("warning: could not load prior rollups: %v", err)
	} else {
		for _, rollup := range prior {
			summarised[rollup.HourStart.Unix()] = struct{}{}
		}
	}

	rollupOnce := func() {
		now := time.Now()
		// Only hours that are both complete and still backed by raw samples.
		oldestHour := now.Add(-LogRetention).Truncate(time.Hour).Add(time.Hour)
		newestHour := now.Truncate(time.Hour)

		var wanted []time.Time
		for hour := oldestHour; hour.Before(newestHour); hour = hour.Add(time.Hour) {
			if _, done := summarised[hour.Unix()]; !done {
				wanted = append(wanted, hour)
			}
		}
		if len(wanted) == 0 {
			return
		}

		samples, err := LoadSamplesSince[HopSample](hopsLogPath, wanted[0])
		if err != nil {
			log.Printf("rollup: could not read %s: %v", hopsLogPath, err)
			return
		}

		// The dish log is optional: without it the dish buckets stay zero.
		dishSamples, err := LoadSamplesSince[TelemetrySample](dishLogPath, wanted[0])
		if err != nil {
			log.Printf("rollup: could not read %s: %v", dishLogPath, err)
			dishSamples = nil
		}

		// Also optional: without it the handover burst fields stay zero.
		handovers, err := LoadSamplesSince[HandoverSample](handoverLogPath, wanted[0])
		if err != nil {
			log.Printf("rollup: could not read %s: %v", handoverLogPath, err)
			handovers = nil
		}

		for _, hour := range wanted {
			rollup := summariseHour(samples, dishSamples, hour)
			addHandoverBursts(&rollup, handovers, hour)
			rollup.Sampling = samplingMode(hour, processStart, handovers)
			// An hour with no samples at all means the app wasn't running.
			// Record nothing rather than a line claiming total loss.
			if rollup.Samples == 0 {
				summarised[hour.Unix()] = struct{}{}
				continue
			}
			if err := rollupLogFile.Append(rollup); err != nil {
				log.Printf("rollup log write failed: %v", err)
				return
			}
			summarised[hour.Unix()] = struct{}{}
		}
	}

	rollupOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rollupOnce()
		}
	}
}

// summariseHour folds every sample inside one hour into a rollup.
func summariseHour(samples []HopSample, dishSamples []TelemetrySample, hourStart time.Time) HopRollup {
	hourEnd := hourStart.Add(time.Hour)
	rollup := HopRollup{
		HourStart:    hourStart,
		Hops:         make(map[string]HopHourStats),
		HandoverLoss: make([]int, HandoverSlotSeconds),
		HandoverSent: make([]int, HandoverSlotSeconds),

		DishDropSlots:   make([]int, HandoverSlotSeconds),
		DishSlotSamples: make([]int, HandoverSlotSeconds),
	}

	roundTrips := make(map[string][]float64)
	jitters := make(map[string][]float64)
	previousRTT := make(map[string]float64)
	sent := make(map[string]int)
	lost := make(map[string]int)

	for _, sample := range samples {
		if sample.Timestamp.Before(hourStart) {
			continue
		}
		if !sample.Timestamp.Before(hourEnd) {
			break // samples are chronological
		}
		rollup.Samples++

		slot := sample.Timestamp.Second() % HandoverSlotSeconds
		if endResult, measured := sample.Hops[HopEndTarget]; measured {
			rollup.HandoverSent[slot]++
			if endResult.Lost {
				rollup.HandoverLoss[slot]++
			}
		}

		for target, result := range sample.Hops {
			sent[target]++
			if result.Lost {
				lost[target]++
				// A lost echo breaks the chain: the next RTT isn't a
				// neighbour of the last one, so it starts a new run.
				delete(previousRTT, target)
				continue
			}
			roundTrips[target] = append(roundTrips[target], result.RTTMs)
			if previous, hadPrevious := previousRTT[target]; hadPrevious {
				jitters[target] = append(jitters[target], math.Abs(result.RTTMs-previous))
			}
			previousRTT[target] = result.RTTMs
		}
	}

	for _, dishSample := range dishSamples {
		if dishSample.Timestamp.Before(hourStart) {
			continue
		}
		if !dishSample.Timestamp.Before(hourEnd) {
			break
		}
		if dishSample.PollError != "" {
			continue
		}
		slot := dishSample.Timestamp.Second() % HandoverSlotSeconds
		rollup.DishSlotSamples[slot]++
		if dishSample.DropRateFraction > 0 {
			rollup.DishDropSlots[slot]++
		}
	}

	for target, count := range sent {
		values := roundTrips[target]
		sort.Float64s(values)
		jitter := jitters[target]
		sort.Float64s(jitter)
		rollup.Hops[target] = HopHourStats{
			Sent:        count,
			Lost:        lost[target],
			P50Ms:       percentile(values, 0.50),
			P90Ms:       percentile(values, 0.90),
			P99Ms:       percentile(values, 0.99),
			MaxMs:       percentile(values, 1),
			JitterP99Ms: percentile(jitter, 0.99),
		}
	}
	return rollup
}

// addHandoverBursts folds the hour's handover bursts into the rollup.
func addHandoverBursts(rollup *HopRollup, handovers []HandoverSample, hourStart time.Time) {
	hourEnd := hourStart.Add(time.Hour)
	var lossDurations []float64
	for _, burst := range handovers {
		if burst.Boundary.Before(hourStart) {
			continue
		}
		if !burst.Boundary.Before(hourEnd) {
			break // bursts are chronological
		}
		if burst.Errors > 0 {
			rollup.HandoverBurstsSendError++
			continue
		}
		rollup.HandoverBursts++
		rollup.HandoverLostPings += burst.Lost
		if burst.Lost == 0 || burst.FirstLostMs == nil || burst.LastLostMs == nil {
			continue
		}
		rollup.HandoverBurstsWithLoss++
		lossDurations = append(lossDurations, float64(*burst.LastLostMs-*burst.FirstLostMs+burst.StepMs))
	}
	sort.Float64s(lossDurations)
	rollup.HandoverLossMsP50 = percentile(lossDurations, 0.50)
	rollup.HandoverLossMsP90 = percentile(lossDurations, 0.90)
}

// samplingMode reports SamplingRandomPhase only when it can be proven: the
// hour began after this process did, or a burst fired at the hour's first
// handover, so the random-phase code was running from the top of the hour.
// The first rollup after an upgrade still summarises older fixed-phase hours
// from hops.jsonl, and those must not pass as comparable.
func samplingMode(hourStart, started time.Time, handovers []HandoverSample) string {
	if !hourStart.Before(started) {
		return SamplingRandomPhase
	}
	firstSlotEnd := hourStart.Add(HandoverSlotSeconds * time.Second)
	for _, burst := range handovers {
		if !burst.Boundary.Before(hourStart) && burst.Boundary.Before(firstSlotEnd) {
			return SamplingRandomPhase
		}
	}
	return SamplingUnknown
}

// percentile returns the nearest-rank percentile of an already sorted slice,
// rounded to two decimals since these are milliseconds, not measurements of
// anything finer. An empty slice gives 0: every echo in the hour was lost.
func percentile(sortedValues []float64, fraction float64) float64 {
	if len(sortedValues) == 0 {
		return 0
	}
	index := int(fraction * float64(len(sortedValues)))
	if index >= len(sortedValues) {
		index = len(sortedValues) - 1
	}
	return math.Round(sortedValues[index]*100) / 100
}
