package main

import (
	"context"
	"math"
	"testing"
	"time"

	device "github.com/starlink-community/starlink-grpc-go/pkg/spacex.com/api/device"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestReadFloatsPackedAndUnpacked(t *testing.T) {
	var packed []byte
	for _, watts := range []float32{40.5, 41, 42.25} {
		packed = protowire.AppendFixed32(packed, math.Float32bits(watts))
	}

	var unknown []byte
	unknown = protowire.AppendTag(unknown, 1009, protowire.BytesType) // another field, skipped
	unknown = protowire.AppendBytes(unknown, []byte{1, 2, 3})
	unknown = protowire.AppendTag(unknown, fieldHistoryPowerIn, protowire.BytesType)
	unknown = protowire.AppendBytes(unknown, packed)
	unknown = protowire.AppendTag(unknown, fieldHistoryPowerIn, protowire.Fixed32Type)
	unknown = protowire.AppendFixed32(unknown, math.Float32bits(43))

	got := readFloats(unknown, fieldHistoryPowerIn)
	want := []float32{40.5, 41, 42.25, 43}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestReadFloatsMissingField(t *testing.T) {
	if got := readFloats(nil, fieldHistoryPowerIn); len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}

func TestCollectOutagesReusesRecentHistory(t *testing.T) {
	history := &device.DishGetHistoryResponse{}
	history.ProtoReflect().SetUnknown(protowire.AppendBytes(
		protowire.AppendTag(nil, fieldHistoryOutages, protowire.BytesType),
		encodeOutage(4, time.Now().Add(-time.Minute).UnixNano(), uint64(2*time.Second), false)))

	// No deviceClient: a GetHistory call here would panic.
	collector := &StarlinkCollector{latestHistory: history, latestHistoryAt: time.Now()}
	outages, err := collector.CollectOutages(context.Background())
	if err != nil || len(outages) != 1 {
		t.Fatalf("got %v, %v; want one outage from the cached history", outages, err)
	}
}
