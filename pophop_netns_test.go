package main

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestPopProberNetns(t *testing.T) {
	if os.Getenv("HOUSTON_NETNS_TEST") == "" {
		t.Skip("needs the network from .github/netns.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	prober := NewPopProber()
	if !prober.Discover(ctx) {
		t.Fatal("no gateway found")
	}
	if gateway, ttl := prober.Gateway(), prober.ttl.Load(); gateway != "100.64.0.1" || ttl != 2 {
		t.Fatalf("found %s at ttl %d, want 100.64.0.1 at ttl 2", gateway, ttl)
	}
	for range 5 {
		if sample := prober.PingOnce(ctx); sample.Lost || sample.RTTMs <= 0 {
			t.Fatalf("probe failed: %+v", sample)
		}
	}
}
