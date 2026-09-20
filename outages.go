package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	device "github.com/starlink-community/starlink-grpc-go/pkg/spacex.com/api/device"
	"google.golang.org/protobuf/encoding/protowire"
)

// DishOutage is one entry from the outage list the dish keeps in get_history.
//
// The JSON tags are the on-disk log format (dishOutages.jsonl).
type DishOutage struct {
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	DurationS float64   `json:"duration_s"`
	Cause     string    `json:"cause"` // e.g. NO_SCHEDULE, OBSTRUCTED
	DidSwitch bool      `json:"did_switch"`
	// StartTimestampNs is the dish's own value, kept raw so it can be
	// re-interpreted if the epoch guess in dishTimeToTime is ever wrong.
	StartTimestampNs int64 `json:"start_timestamp_ns"`
}

func (outage DishOutage) SampleTime() time.Time { return outage.Start }

// OutageSource is a collector that can also read the dish's outage history.
// The fake collector isn't one, so -fake simply logs no outages.
type OutageSource interface {
	CollectOutages(ctx context.Context) ([]DishOutage, error)
}

// DishGetHistoryResponse.outages postdates the vendored protos, just like
// boresight, so it arrives in the unknown fields and is read by tag.
const fieldHistoryOutages = 1009

// DishOutage field numbers.
const (
	fieldOutageCause            = 1
	fieldOutageStartTimestampNs = 2
	fieldOutageDurationNs       = 3
	fieldOutageDidSwitch        = 4
)

// outageCauseNames is DishOutage.Cause, taken from dish.protoset.
var outageCauseNames = map[uint64]string{
	0: "UNKNOWN", 1: "BOOTING", 2: "STOWED", 3: "THERMAL_SHUTDOWN", 4: "NO_SCHEDULE",
	5: "NO_SATS", 6: "OBSTRUCTED", 7: "NO_DOWNLINK", 8: "NO_PINGS", 9: "ACTUATOR_ACTIVITY",
	10: "CABLE_TEST", 11: "SLEEPING", 13: "SKY_SEARCH", 14: "INHIBIT_RF",
}

const (
	// The dish clock is GPS time: GPS epoch (1980-01-06) and no leap seconds.
	gpsEpochUnixSeconds = 315964800
	gpsLeapSeconds      = 18

	// A GPS timestamp read as Unix lands ~10 years early; anything older than
	// this is taken to be GPS.
	gpsDetectionAge = 5 * 365 * 24 * time.Hour

	// outageSettleTime: an outage that ended this recently may still be
	// growing, so it's left for the next poll rather than logged short.
	outageSettleTime = 5 * time.Second
)

func (collector *StarlinkCollector) CollectOutages(ctx context.Context) ([]DishOutage, error) {
	response, err := collector.deviceClient.Handle(ctx, &device.Request{
		Request: &device.Request_GetHistory{GetHistory: &device.GetHistoryRequest{}},
	})
	if err != nil {
		return nil, fmt.Errorf("GetHistory: %w", err)
	}
	history := response.GetDishGetHistory()
	if history == nil {
		return nil, fmt.Errorf("response contained no dish history")
	}
	return readOutages(history, time.Now())
}

// readOutages pulls every outage out of a history message's unknown fields.
func readOutages(history *device.DishGetHistoryResponse, now time.Time) ([]DishOutage, error) {
	unknown := history.ProtoReflect().GetUnknown()
	var outages []DishOutage

	for len(unknown) > 0 {
		fieldNumber, wireType, tagLength := protowire.ConsumeTag(unknown)
		if tagLength < 0 {
			return nil, protowire.ParseError(tagLength)
		}
		unknown = unknown[tagLength:]

		if fieldNumber == fieldHistoryOutages && wireType == protowire.BytesType {
			message, messageLength := protowire.ConsumeBytes(unknown)
			if messageLength < 0 {
				return nil, protowire.ParseError(messageLength)
			}
			outage, err := parseOutage(message, now)
			if err != nil {
				return nil, err
			}
			outages = append(outages, outage)
			unknown = unknown[messageLength:]
			continue
		}

		skipLength := protowire.ConsumeFieldValue(fieldNumber, wireType, unknown)
		if skipLength < 0 {
			return nil, protowire.ParseError(skipLength)
		}
		unknown = unknown[skipLength:]
	}
	return outages, nil
}

// parseOutage decodes one DishOutage message. Every field is a varint.
func parseOutage(message []byte, now time.Time) (DishOutage, error) {
	var cause, durationNs uint64
	outage := DishOutage{}

	for len(message) > 0 {
		fieldNumber, wireType, tagLength := protowire.ConsumeTag(message)
		if tagLength < 0 {
			return DishOutage{}, protowire.ParseError(tagLength)
		}
		message = message[tagLength:]

		if wireType == protowire.VarintType {
			value, valueLength := protowire.ConsumeVarint(message)
			if valueLength < 0 {
				return DishOutage{}, protowire.ParseError(valueLength)
			}
			switch fieldNumber {
			case fieldOutageCause:
				cause = value
			case fieldOutageStartTimestampNs:
				outage.StartTimestampNs = int64(value)
			case fieldOutageDurationNs:
				durationNs = value
			case fieldOutageDidSwitch:
				outage.DidSwitch = protowire.DecodeBool(value)
			}
			message = message[valueLength:]
			continue
		}

		skipLength := protowire.ConsumeFieldValue(fieldNumber, wireType, message)
		if skipLength < 0 {
			return DishOutage{}, protowire.ParseError(skipLength)
		}
		message = message[skipLength:]
	}

	outage.Cause = outageCauseNames[cause]
	if outage.Cause == "" {
		outage.Cause = fmt.Sprintf("CAUSE_%d", cause)
	}
	outage.Start = dishTimeToTime(outage.StartTimestampNs, now)
	outage.End = outage.Start.Add(time.Duration(durationNs))
	outage.DurationS = time.Duration(durationNs).Seconds()
	return outage, nil
}

// dishTimeToTime converts a dish timestamp to wall time. Firmware reports GPS
// time; if a value already reads as recent Unix time it's used as-is.
func dishTimeToTime(timestampNs int64, now time.Time) time.Time {
	asUnix := time.Unix(0, timestampNs)
	if now.Sub(asUnix) < gpsDetectionAge {
		return asUnix
	}
	return asUnix.Add((gpsEpochUnixSeconds - gpsLeapSeconds) * time.Second)
}

// outageLoop polls the dish's outage list every OutagePollInterval and appends
// outages it hasn't logged before. The dish repeats its whole list on every
// call, so dedupe is by start timestamp, seeded from the log at startup.
func outageLoop(ctx context.Context, source OutageSource, logFile *LogFile[DishOutage], logPath string) {
	ticker := time.NewTicker(OutagePollInterval)
	defer ticker.Stop()

	logged := make(map[int64]time.Time)
	if prior, err := LoadSamplesSince[DishOutage](logPath, time.Time{}); err != nil {
		log.Printf("warning: could not load prior outages: %v", err)
	} else {
		for _, outage := range prior {
			logged[outage.StartTimestampNs] = outage.Start
		}
	}

	lastError := ""

	pollOnce := func() {
		pollCtx, cancelPoll := context.WithTimeout(ctx, DefaultRequestTimeout)
		defer cancelPoll()
		outages, err := source.CollectOutages(pollCtx)
		if ctx.Err() != nil {
			return
		}
		errorText := ""
		if err != nil {
			errorText = err.Error()
		}
		if errorText != lastError {
			if errorText != "" {
				log.Printf("dish outages: %s", errorText)
			} else {
				log.Print("dish outages: reading again")
			}
			lastError = errorText
		}
		if err != nil {
			return
		}

		now := time.Now()
		sort.Slice(outages, func(i, j int) bool { return outages[i].StartTimestampNs < outages[j].StartTimestampNs })
		for _, outage := range outages {
			if _, seen := logged[outage.StartTimestampNs]; seen {
				continue
			}
			if outage.End.After(now.Add(-outageSettleTime)) {
				continue
			}
			if err := logFile.Append(outage); err != nil {
				log.Printf("outage log write failed: %v", err)
				return
			}
			logged[outage.StartTimestampNs] = outage.Start
		}

		// Forget anything the pruner has already dropped from the file.
		cutoff := now.Add(-LogRetention)
		for startNs, start := range logged {
			if start.Before(cutoff) {
				delete(logged, startNs)
			}
		}
	}

	pollOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pollOnce()
		}
	}
}
