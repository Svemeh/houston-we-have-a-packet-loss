package main

import (
	"math"
	"testing"

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
