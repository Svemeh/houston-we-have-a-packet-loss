package main

import (
	"context"
	"fmt"
	"math"
	"time"

	device "github.com/starlink-community/starlink-grpc-go/pkg/spacex.com/api/device"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protowire"
)

// TelemetryCollector is anything that can produce one sample on demand: the
// live dish, or a fake standing in for it.
type TelemetryCollector interface {
	Collect(ctx context.Context) (TelemetrySample, error)
	Close() error
}

// newCollector builds the telemetry source, either: ( live dish / fake dish )
func newCollector(useFake bool, dishAddress string) (TelemetryCollector, error) {
	if useFake {
		return NewFakeCollector(), nil
	}
	return NewStarlinkCollector(dishAddress)
}

type StarlinkCollector struct {
	connection   *grpc.ClientConn
	deviceClient device.DeviceClient
}

func NewStarlinkCollector(dishAddress string) (*StarlinkCollector, error) {
	connection, err := grpc.NewClient(dishAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("creating client: %w", err)
	}
	return &StarlinkCollector{connection: connection, deviceClient: device.NewDeviceClient(connection)}, nil
}

func (collector *StarlinkCollector) Collect(ctx context.Context) (TelemetrySample, error) {
	response, err := collector.deviceClient.Handle(ctx, &device.Request{
		Request: &device.Request_GetStatus{GetStatus: &device.GetStatusRequest{}},
	})
	if err != nil {
		return TelemetrySample{}, fmt.Errorf("GetStatus: %w", err)
	}
	dishStatus := response.GetDishGetStatus()
	if dishStatus == nil {
		return TelemetrySample{}, fmt.Errorf("response contained no dish status")
	}
	obstructionStats := dishStatus.GetObstructionStats()
	isObstructed := obstructionStats.GetCurrentlyObstructed()
	dropRateFraction := float64(dishStatus.GetPopPingDropRate())
	boresightAzimuth, boresightElevation, boresightOK := readBoresight(dishStatus)

	return TelemetrySample{
		Timestamp:             time.Now(),
		LinkState:             deriveLinkState(dropRateFraction, isObstructed),
		LatencyMs:             float64(dishStatus.GetPopPingLatencyMs()),
		DownloadMbps:          float64(dishStatus.GetDownlinkThroughputBps()) / 1e6, // bps -> Mbps
		UploadMbps:            float64(dishStatus.GetUplinkThroughputBps()) / 1e6,   // bps -> Mbps
		DropRateFraction:      dropRateFraction,
		Obstructed:            isObstructed,
		ObstructionFraction:   float64(obstructionStats.GetFractionObstructed()),
		BoresightAzimuthDeg:   boresightAzimuth,
		BoresightElevationDeg: boresightElevation,
		BoresightValid:        boresightOK,
		UptimeSeconds:         dishStatus.GetDeviceState().GetUptimeS(),
		HardwareVersion:       dishStatus.GetDeviceInfo().GetHardwareVersion(),
		SoftwareVersion:       dishStatus.GetDeviceInfo().GetSoftwareVersion(),
	}, nil
}

func (collector *StarlinkCollector) Close() error { return collector.connection.Close() }

// Boresight field numbers on DishGetStatus. These postdate the vendored protos
// by several years, so the dish sends them but the generated struct has no
// place to put them — they land in the message's unknown fields instead.
// Reading them by tag is far cheaper than regenerating the entire API surface
// (which now carries Wifi, transceiver and diagnostics messages) for two floats.
const (
	fieldBoresightAzimuthDeg   = 1011
	fieldBoresightElevationDeg = 1012
)

// readBoresight pulls the two boresight angles out of a status message's
// unknown fields. ok is false when the firmware didn't send them, which is the
// expected case on older dishes.
func readBoresight(dishStatus *device.DishGetStatusResponse) (azimuthDeg, elevationDeg float64, ok bool) {
	unknown := dishStatus.ProtoReflect().GetUnknown()
	foundAzimuth, foundElevation := false, false

	for len(unknown) > 0 {
		fieldNumber, wireType, tagLength := protowire.ConsumeTag(unknown)
		if tagLength < 0 {
			return 0, 0, false // malformed; don't trust anything we read
		}
		unknown = unknown[tagLength:]

		// Both fields are proto floats, so 32-bit fixed on the wire.
		if wireType == protowire.Fixed32Type {
			bits, valueLength := protowire.ConsumeFixed32(unknown)
			if valueLength < 0 {
				return 0, 0, false
			}
			switch fieldNumber {
			case fieldBoresightAzimuthDeg:
				azimuthDeg, foundAzimuth = float64(math.Float32frombits(bits)), true
			case fieldBoresightElevationDeg:
				elevationDeg, foundElevation = float64(math.Float32frombits(bits)), true
			}
			unknown = unknown[valueLength:]
			continue
		}

		skipLength := protowire.ConsumeFieldValue(fieldNumber, wireType, unknown)
		if skipLength < 0 {
			return 0, 0, false
		}
		unknown = unknown[skipLength:]
	}
	return azimuthDeg, elevationDeg, foundAzimuth && foundElevation
}

func deriveLinkState(dropRateFraction float64, isObstructed bool) string {
	switch {
	case isObstructed:
		return LinkStateObstructed
	case dropRateFraction >= DropRateNoSignalThreshold:
		return LinkStateNoSignal
	case dropRateFraction > DropRateDegradedThreshold:
		return LinkStateDegraded
	default:
		return LinkStateOnline
	}
}
