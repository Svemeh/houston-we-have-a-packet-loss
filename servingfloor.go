package main

import (
	"sort"
	"sync"
	"time"
)

type servingElevation struct {
	at           time.Time
	elevationDeg float64
}

// servingFloor learns the lowest elevation the dish actually serves from, from matched slots.
type servingFloor struct {
	mutex      sync.Mutex
	elevations []servingElevation
}

func (floor *servingFloor) add(slot ServingSlot) {
	if slot.Status != ServingMatched || slot.Best == nil {
		return
	}
	floor.mutex.Lock()
	defer floor.mutex.Unlock()
	floor.elevations = append(floor.elevations, servingElevation{at: slot.Start, elevationDeg: slot.Best.ElevationDeg})
}

// measure returns the measured floor; ok is false until there are ServingFloorMinSlots matched slots within LogRetention.
func (floor *servingFloor) measure(now time.Time) (floorDeg float64, ok bool) {
	floor.mutex.Lock()
	defer floor.mutex.Unlock()

	cutoff := now.Add(-LogRetention)
	kept := floor.elevations[:0]
	for _, elevation := range floor.elevations {
		if elevation.at.After(cutoff) {
			kept = append(kept, elevation)
		}
	}
	floor.elevations = kept
	if len(kept) < ServingFloorMinSlots {
		return 0, false
	}

	sorted := make([]float64, len(kept))
	for i, elevation := range kept {
		sorted[i] = elevation.elevationDeg
	}
	sort.Float64s(sorted)
	return sorted[int(ServingFloorPercentile*float64(len(sorted)))], true
}
