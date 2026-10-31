package main

import "testing"

func TestHubBackfillIsCapped(t *testing.T) {
	hub := NewHub[int](3)
	hub.SeedHistory([]int{1, 2})
	for value := 3; value <= 5; value++ {
		hub.PublishSample(value)
	}

	_, backfill, unsubscribe := hub.Subscribe()
	defer unsubscribe()
	if len(backfill) != 3 || backfill[0] != 3 || backfill[2] != 5 {
		t.Errorf("backfill = %v, want [3 4 5]", backfill)
	}
}

func TestHubSlowSubscriberDoesNotBlock(t *testing.T) {
	hub := NewHub[int](1)
	stream, _, unsubscribe := hub.Subscribe()

	// Nobody reads the stream: publishing past its queue must drop, not hang.
	for value := 0; value < subscriberQueueDepth*4; value++ {
		hub.PublishSample(value)
	}
	if len(stream) != subscriberQueueDepth {
		t.Errorf("queued %d, want %d", len(stream), subscriberQueueDepth)
	}

	unsubscribe()
	unsubscribe() // must be safe to call twice
	for range stream {
	}
}

func TestDeriveLinkState(t *testing.T) {
	cases := []struct {
		drop       float64
		obstructed bool
		want       string
	}{
		{0, false, LinkStateOnline},
		{DropRateDegradedThreshold, false, LinkStateOnline},
		{0.05, false, LinkStateDegraded},
		{1, false, LinkStateNoSignal},
		{1, true, LinkStateObstructed},
	}
	for _, c := range cases {
		if got := deriveLinkState(c.drop, c.obstructed); got != c.want {
			t.Errorf("deriveLinkState(%v, %v) = %s, want %s", c.drop, c.obstructed, got, c.want)
		}
	}
}

func TestCanReachLatitude(t *testing.T) {
	if !canReachLatitude(53.0544, 60) {
		t.Error("53° shell should be visible from 60°N")
	}
	if canReachLatitude(43, 70) {
		t.Error("43° shell should not be visible from 70°N")
	}
	if !canReachLatitude(97.6, 70) {
		t.Error("retrograde 97.6° orbit reaches 82.4°, should be visible from 70°N")
	}
}
