package main

import (
	"sort"
	"time"
)

// Incident is one thing worth a line in the dashboard's list: a stretch of
// loss, a stretch of high latency, or an outage the dish reported itself.
//
// Incidents are derived from the logs on request rather than stored, so a
// change to the thresholds in const.go reshapes all of history instead of
// leaving older rows judged by older rules.
type Incident struct {
	Start     time.Time `json:"start"`
	DurationS float64   `json:"duration_s"`
	Kind      string    `json:"kind"`

	// SlotSecond is where the incident began within the satellite slot. It is
	// a timing fact and nothing more: no single incident can be pinned on a
	// handover, only the distribution of many of them can.
	SlotSecond int `json:"slot_second"`

	Scope     string  `json:"scope,omitempty"` // loss only
	LostCount int     `json:"lost_count,omitempty"`
	PeakRTTMs float64 `json:"peak_rtt_ms,omitempty"`
	Cause     string  `json:"cause,omitempty"` // dish outages only
}

func (incident Incident) SampleTime() time.Time { return incident.Start }

// Incident kinds.
const (
	IncidentLoss    = "loss"
	IncidentLatency = "latency"
	IncidentOutage  = "outage"
)

// Scopes for a loss incident, decided by which targets went quiet together
// rather than by walking the path. Routers rate-limit echoes to themselves, so
// an intermediate hop dropping says little; two unrelated destinations
// dropping in the same second says a lot.
const (
	ScopeLocal      = "local"      // the router or the dish lost it too
	ScopeUpstream   = "upstream"   // the control target lost it too
	ScopeCloudflare = "cloudflare" // only the end target's path lost it
)

// DetectIncidents finds everything notable in one window of hop samples and
// dish outages, newest first, capped at limit. Samples must be chronological.
func DetectIncidents(samples []HopSample, outages []DishOutage, limit int) []Incident {
	incidents := append(lossBursts(samples), latencySpikes(samples)...)
	for _, outage := range outages {
		incidents = append(incidents, Incident{
			Start:      outage.Start,
			DurationS:  outage.DurationS,
			Kind:       IncidentOutage,
			SlotSecond: outage.Start.Second() % HandoverSlotSeconds,
			Cause:      outage.Cause,
		})
	}

	sort.Slice(incidents, func(i, j int) bool { return incidents[i].Start.After(incidents[j].Start) })
	if limit > 0 && len(incidents) > limit {
		incidents = incidents[:limit]
	}
	return incidents
}

// lossBursts groups lost seconds into runs. A burst survives up to
// IncidentBurstGapSeconds answered seconds, because one reply in the middle of
// a dropout is a gap in the measurement rather than a recovery.
func lossBursts(samples []HopSample) []Incident {
	var incidents []Incident
	var current []HopSample
	lastLostIndex := 0

	flush := func() {
		if len(current) > 0 {
			incidents = append(incidents, newLossIncident(current))
			current = nil
		}
	}

	for index, sample := range samples {
		result, measured := sample.Hops[HopEndTarget]
		if !measured || !result.Lost {
			if len(current) > 0 && index-lastLostIndex > IncidentBurstGapSeconds {
				flush()
			}
			continue
		}
		current = append(current, sample)
		lastLostIndex = index
	}
	flush()
	return incidents
}

func newLossIncident(burst []HopSample) Incident {
	scopes := make(map[string]int, 3)
	for _, sample := range burst {
		scopes[classifyLoss(sample.Hops)]++
	}

	// Majority scope. Ties go to the more local explanation: blaming the
	// network for something that might be ours is the worse mistake.
	best, bestCount := ScopeCloudflare, -1
	for _, scope := range []string{ScopeLocal, ScopeUpstream, ScopeCloudflare} {
		if scopes[scope] > bestCount {
			best, bestCount = scope, scopes[scope]
		}
	}

	start := burst[0].Timestamp
	return Incident{
		Start:      start,
		DurationS:  burst[len(burst)-1].Timestamp.Add(PingInterval).Sub(start).Seconds(),
		Kind:       IncidentLoss,
		SlotSecond: start.Second() % HandoverSlotSeconds,
		Scope:      best,
		LostCount:  len(burst),
	}
}

// classifyLoss reads one lost second: who else went quiet at the same moment.
func classifyLoss(hops map[string]HopResult) string {
	for _, target := range HopLocalTargets {
		if result, measured := hops[target]; measured && result.Lost {
			return ScopeLocal
		}
	}
	if result, measured := hops[HopControlTarget]; measured && result.Lost {
		return ScopeUpstream
	}
	return ScopeCloudflare
}

// latencySpikes groups consecutive slow seconds into runs, so a minute of bad
// latency is one line rather than sixty.
//
// The threshold comes from the window being scanned rather than a fixed
// number, so it keeps meaning the same thing on a link whose baseline differs
// from this one — with a floor, so a 12 ms reply on a 4 ms LAN isn't an event.
func latencySpikes(samples []HopSample) []Incident {
	threshold := spikeThreshold(samples)

	var incidents []Incident
	var start time.Time
	var last time.Time
	var peak float64

	flush := func() {
		if peak == 0 {
			return
		}
		incidents = append(incidents, Incident{
			Start:      start,
			DurationS:  last.Add(PingInterval).Sub(start).Seconds(),
			Kind:       IncidentLatency,
			SlotSecond: start.Second() % HandoverSlotSeconds,
			PeakRTTMs:  peak,
		})
		peak = 0
	}

	for _, sample := range samples {
		result, measured := sample.Hops[HopEndTarget]
		if !measured || result.Lost || result.RTTMs <= threshold {
			flush()
			continue
		}
		if peak == 0 {
			start = sample.Timestamp
		}
		last = sample.Timestamp
		if result.RTTMs > peak {
			peak = result.RTTMs
		}
	}
	flush()
	return incidents
}

// spikeThreshold is the window's median RTT times IncidentSpikeFactor, never
// below IncidentSpikeFloorMs.
func spikeThreshold(samples []HopSample) float64 {
	roundTrips := make([]float64, 0, len(samples))
	for _, sample := range samples {
		if result, measured := sample.Hops[HopEndTarget]; measured && !result.Lost {
			roundTrips = append(roundTrips, result.RTTMs)
		}
	}
	if len(roundTrips) == 0 {
		return IncidentSpikeFloorMs
	}
	sort.Float64s(roundTrips)

	threshold := roundTrips[len(roundTrips)/2] * IncidentSpikeFactor
	if threshold < IncidentSpikeFloorMs {
		return IncidentSpikeFloorMs
	}
	return threshold
}
