package main

import (
	"testing"
	"time"
)

func TestServingFloor(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	matched := func(at time.Time, elevation float64) ServingSlot {
		return ServingSlot{Start: at, Status: ServingMatched, Best: &ServingCandidate{ElevationDeg: elevation}}
	}
	var floor servingFloor
	for i := 0; i < ServingFloorMinSlots-1; i++ {
		floor.add(matched(now.Add(-time.Hour), 26+float64(i%60)))
	}
	if _, ok := floor.measure(now); ok {
		t.Fatal("measured before ServingFloorMinSlots")
	}
	floor.add(ServingSlot{Start: now, Status: ServingAmbiguous, Best: &ServingCandidate{ElevationDeg: 5}})
	floor.add(matched(now.Add(-time.Hour), 3)) // one bad match
	floorDeg, ok := floor.measure(now)
	if !ok || floorDeg != 26 {
		t.Errorf("floor = %.1f, %v; want 26", floorDeg, ok)
	}
	if _, ok := floor.measure(now.Add(LogRetention)); ok {
		t.Error("slots older than LogRetention should be dropped")
	}
}

func TestSnapshotUsesMeasuredFloor(t *testing.T) {
	sky := NewSkyTracker(Observer{LatitudeDeg: 60.19, LongitudeDeg: 12}, LocationFromSettings)
	if snapshot := sky.Snapshot(time.Now()); snapshot.ServiceFloorDeg != ServiceFloorElevationDeg || snapshot.ServiceFloorMeasured {
		t.Errorf("before measuring: %.1f, %v", snapshot.ServiceFloorDeg, snapshot.ServiceFloorMeasured)
	}
	sky.SetServiceFloor(26.4)
	if snapshot := sky.Snapshot(time.Now()); snapshot.ServiceFloorDeg != 26.4 || !snapshot.ServiceFloorMeasured {
		t.Errorf("after measuring: %.1f, %v", snapshot.ServiceFloorDeg, snapshot.ServiceFloorMeasured)
	}
}
