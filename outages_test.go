package main

import (
	"testing"
	"time"

	device "github.com/starlink-community/starlink-grpc-go/pkg/spacex.com/api/device"
	"google.golang.org/protobuf/encoding/protowire"
)

// encodeOutage builds a DishOutage message the way the dish sends it.
func encodeOutage(cause uint64, startNs int64, durationNs uint64, didSwitch bool) []byte {
	var message []byte
	message = protowire.AppendTag(message, fieldOutageCause, protowire.VarintType)
	message = protowire.AppendVarint(message, cause)
	message = protowire.AppendTag(message, fieldOutageStartTimestampNs, protowire.VarintType)
	message = protowire.AppendVarint(message, uint64(startNs))
	message = protowire.AppendTag(message, fieldOutageDurationNs, protowire.VarintType)
	message = protowire.AppendVarint(message, durationNs)
	message = protowire.AppendTag(message, fieldOutageDidSwitch, protowire.VarintType)
	message = protowire.AppendVarint(message, protowire.EncodeBool(didSwitch))
	return message
}

func TestReadOutagesFromUnknownFields(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	want := now.Add(-time.Hour)
	// The dish clock is GPS time: same instant, different epoch, no leap seconds.
	gpsNs := want.Add(-(gpsEpochUnixSeconds - gpsLeapSeconds) * time.Second).UnixNano()

	var unknown []byte
	unknown = protowire.AppendTag(unknown, 7, protowire.VarintType) // unrelated field, must be skipped
	unknown = protowire.AppendVarint(unknown, 42)
	unknown = protowire.AppendTag(unknown, fieldHistoryOutages, protowire.BytesType)
	unknown = protowire.AppendBytes(unknown, encodeOutage(4, gpsNs, uint64(1500*time.Millisecond), true))
	unknown = protowire.AppendTag(unknown, fieldHistoryOutages, protowire.BytesType)
	unknown = protowire.AppendBytes(unknown, encodeOutage(99, gpsNs, 0, false))

	history := &device.DishGetHistoryResponse{}
	history.ProtoReflect().SetUnknown(unknown)

	outages, err := readOutages(history, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(outages) != 2 {
		t.Fatalf("got %d outages, want 2", len(outages))
	}
	first := outages[0]
	if !first.Start.Equal(want) {
		t.Errorf("Start = %v, want %v", first.Start, want)
	}
	if first.Cause != "NO_SCHEDULE" || first.DurationS != 1.5 || !first.DidSwitch {
		t.Errorf("got %+v, want NO_SCHEDULE, 1.5 s, switched", first)
	}
	if !first.End.Equal(want.Add(1500 * time.Millisecond)) {
		t.Errorf("End = %v", first.End)
	}
	if outages[1].Cause != "CAUSE_99" {
		t.Errorf("unknown cause = %q, want CAUSE_99", outages[1].Cause)
	}
}

func TestReadOutagesRejectsTruncatedInput(t *testing.T) {
	var unknown []byte
	unknown = protowire.AppendTag(unknown, fieldHistoryOutages, protowire.BytesType)
	unknown = protowire.AppendVarint(unknown, 50) // claims 50 bytes, has none

	history := &device.DishGetHistoryResponse{}
	history.ProtoReflect().SetUnknown(unknown)

	if _, err := readOutages(history, time.Now()); err == nil {
		t.Error("expected a parse error for a truncated message")
	}
}

func TestDishTimeToTimeKeepsUnixTimestamps(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Minute)
	if got := dishTimeToTime(recent.UnixNano(), now); !got.Equal(recent) {
		t.Errorf("a recent Unix timestamp was shifted: got %v, want %v", got, recent)
	}
}
