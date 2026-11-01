package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"time"

	satellite "github.com/joshuaferrara/go-satellite"
	device "github.com/starlink-community/starlink-grpc-go/pkg/spacex.com/api/device"
	"google.golang.org/protobuf/encoding/protowire"
)

const (
	fieldRequestGetObstructionMap   = 2008
	fieldRequestClearObstructionMap = 2017
	fieldResponseGetObstructionMap  = 2008
	obstructionFrameEarth           = 1

	ServingMatched          = "matched"
	ServingAmbiguous        = "ambiguous"
	ServingFewPixels        = "few_pixels"
	ServingNoLocation       = "no_location"
	ServingNoElements       = "no_elements"
	ServingUnsupportedFrame = "unsupported_frame"
	ServingError            = "error"
)

type ObstructionMap struct {
	Rows        int
	Cols        int
	SNR         []float32
	MaxThetaDeg float64
	Frame       int
}

type ObstructionMapSource interface {
	ObstructionMap(ctx context.Context) (ObstructionMap, error)
	ClearObstructionMap(ctx context.Context) error
}

type ServingCandidate struct {
	Satellite    string  `json:"satellite"`
	NoradID      int     `json:"norad_id"`
	FitDeg       float64 `json:"fit_deg"`
	TimeOffsetS  int     `json:"time_offset_s"`
	AzimuthDeg   float64 `json:"az"`
	ElevationDeg float64 `json:"el"`
	RangeKm      float64 `json:"range_km"`
}

type ServingSlot struct {
	Start    time.Time         `json:"slot_start"`
	Status   string            `json:"status"`
	Pixels   int               `json:"pixels"`
	Best     *ServingCandidate `json:"best,omitempty"`
	RunnerUp *ServingCandidate `json:"runner_up,omitempty"`
	Error    string            `json:"error,omitempty"`
}

func (slot ServingSlot) SampleTime() time.Time { return slot.Start }

type arcPixel struct {
	at           time.Time
	azimuthDeg   float64
	elevationDeg float64
}

func (collector *StarlinkCollector) ObstructionMap(ctx context.Context) (ObstructionMap, error) {
	response, err := collector.handleUnknown(ctx, fieldRequestGetObstructionMap)
	if err != nil {
		return ObstructionMap{}, fmt.Errorf("dish_get_obstruction_map: %w", err)
	}
	body, ok := findBytesField(response.ProtoReflect().GetUnknown(), fieldResponseGetObstructionMap)
	if !ok {
		return ObstructionMap{}, fmt.Errorf("response contained no obstruction map")
	}
	return parseObstructionMap(body)
}

func (collector *StarlinkCollector) ClearObstructionMap(ctx context.Context) error {
	if _, err := collector.handleUnknown(ctx, fieldRequestClearObstructionMap); err != nil {
		return fmt.Errorf("dish_clear_obstruction_map: %w", err)
	}
	return nil
}

// handleUnknown sends a request the vendored protos don't know, as an empty message on its raw field number.
func (collector *StarlinkCollector) handleUnknown(ctx context.Context, field protowire.Number) (*device.Response, error) {
	request := &device.Request{}
	raw := protowire.AppendTag(nil, field, protowire.BytesType)
	raw = protowire.AppendBytes(raw, nil)
	request.ProtoReflect().SetUnknown(raw)
	return collector.deviceClient.Handle(ctx, request)
}

// findBytesField returns the payload of the first length-delimited field with this number.
func findBytesField(raw []byte, field protowire.Number) ([]byte, bool) {
	for len(raw) > 0 {
		number, wireType, tagLength := protowire.ConsumeTag(raw)
		if tagLength < 0 {
			return nil, false
		}
		raw = raw[tagLength:]
		if number == field && wireType == protowire.BytesType {
			value, valueLength := protowire.ConsumeBytes(raw)
			return value, valueLength >= 0
		}
		skip := protowire.ConsumeFieldValue(number, wireType, raw)
		if skip < 0 {
			return nil, false
		}
		raw = raw[skip:]
	}
	return nil, false
}

// parseObstructionMap decodes DishGetObstructionMapResponse; snr may arrive packed or unpacked.
func parseObstructionMap(raw []byte) (ObstructionMap, error) {
	var result ObstructionMap
	for len(raw) > 0 {
		number, wireType, tagLength := protowire.ConsumeTag(raw)
		if tagLength < 0 {
			return ObstructionMap{}, fmt.Errorf("malformed obstruction map")
		}
		raw = raw[tagLength:]
		consumed := -1
		switch {
		case (number == 1 || number == 2 || number == 6) && wireType == protowire.VarintType:
			var value uint64
			value, consumed = protowire.ConsumeVarint(raw)
			switch number {
			case 1:
				result.Rows = int(value)
			case 2:
				result.Cols = int(value)
			default:
				result.Frame = int(value)
			}
		case number == 3 && wireType == protowire.BytesType:
			var packed []byte
			packed, consumed = protowire.ConsumeBytes(raw)
			for len(packed) >= 4 {
				bits, n := protowire.ConsumeFixed32(packed)
				result.SNR = append(result.SNR, math.Float32frombits(bits))
				packed = packed[n:]
			}
		case number == 3 && wireType == protowire.Fixed32Type:
			var bits uint32
			bits, consumed = protowire.ConsumeFixed32(raw)
			result.SNR = append(result.SNR, math.Float32frombits(bits))
		case number == 5 && wireType == protowire.Fixed32Type:
			var bits uint32
			bits, consumed = protowire.ConsumeFixed32(raw)
			result.MaxThetaDeg = float64(math.Float32frombits(bits))
		default:
			consumed = protowire.ConsumeFieldValue(number, wireType, raw)
		}
		if consumed < 0 {
			return ObstructionMap{}, fmt.Errorf("malformed obstruction map field %d", number)
		}
		raw = raw[consumed:]
	}
	if result.Rows <= 1 || result.Rows*result.Cols != len(result.SNR) || result.MaxThetaDeg <= 0 {
		return ObstructionMap{}, fmt.Errorf("obstruction map is %dx%d with %d cells", result.Rows, result.Cols, len(result.SNR))
	}
	return result, nil
}

// pixelDirection converts a FRAME_EARTH cell to azimuth and elevation: a polar plot centred on zenith, north up.
func pixelDirection(row, col, rows int, maxThetaDeg float64) (azimuthDeg, elevationDeg float64) {
	centre := float64(rows-1) / 2
	north := (centre - float64(row)) / centre
	east := (float64(col) - centre) / centre
	zenithDeg := math.Hypot(north, east) * maxThetaDeg
	azimuthDeg = math.Mod(math.Atan2(east, north)*satellite.RAD2DEG+360, 360)
	return azimuthDeg, 90 - zenithDeg
}

// lookAngles propagates one satellite to the nearest whole second and returns where it sits in the observer's sky.
func lookAngles(propagator satellite.Satellite, observer Observer, at time.Time) (azimuthDeg, elevationDeg, rangeKm float64, ok bool) {
	utc := at.Round(time.Second).UTC()
	year, month, day := utc.Date()
	hour, minute, second := utc.Clock()
	position, _ := satellite.Propagate(propagator, year, int(month), day, hour, minute, second)
	if math.IsNaN(position.X) || math.IsNaN(position.Y) || math.IsNaN(position.Z) {
		return 0, 0, 0, false
	}
	azimuthDeg, elevationDeg, rangeKm = lookFrom(position, observer, satellite.JDay(year, int(month), day, hour, minute, second))
	return azimuthDeg, elevationDeg, rangeKm, true
}

// servingCandidates returns the tracked satellites above minElevationDeg at a moment; ok is false without a location.
func (tracker *SkyTracker) servingCandidates(at time.Time, minElevationDeg float64) (candidates []trackedSatellite, observer Observer, ok bool) {
	tracker.mutex.RLock()
	elements := tracker.elements
	observer = tracker.observer
	locationSet := tracker.locationSource != ""
	tracker.mutex.RUnlock()
	if !locationSet {
		return nil, observer, false
	}
	for _, tracked := range elements {
		if _, elevationDeg, _, visible := lookAngles(tracked.propagator, observer, at); visible && elevationDeg >= minElevationDeg {
			candidates = append(candidates, tracked)
		}
	}
	return candidates, observer, true
}

// matchArc scores each candidate by the median angle between the arc and its track, over a small clock offset search.
func matchArc(pixels []arcPixel, candidates []trackedSatellite, observer Observer) (best, runnerUp *ServingCandidate) {
	var scored []ServingCandidate
	errors := make([]float64, len(pixels))
	for _, tracked := range candidates {
		fit, fitOffset := math.Inf(1), 0
		for offset := -ServingMaxTimeOffsetS; offset <= ServingMaxTimeOffsetS; offset++ {
			usable := true
			for i, pixel := range pixels {
				azimuthDeg, elevationDeg, _, ok := lookAngles(tracked.propagator, observer, pixel.at.Add(time.Duration(offset)*time.Second))
				if !ok {
					usable = false
					break
				}
				errors[i] = angularSeparation(azimuthDeg, elevationDeg, pixel.azimuthDeg, pixel.elevationDeg)
			}
			if !usable {
				break
			}
			if median := medianOf(errors); median < fit {
				fit, fitOffset = median, offset
			}
		}
		if math.IsInf(fit, 1) {
			continue
		}
		middle := pixels[len(pixels)/2].at.Add(time.Duration(fitOffset) * time.Second)
		azimuthDeg, elevationDeg, rangeKm, _ := lookAngles(tracked.propagator, observer, middle)
		scored = append(scored, ServingCandidate{
			Satellite:    tracked.name,
			NoradID:      tracked.noradID,
			FitDeg:       roundTo(fit, 2),
			TimeOffsetS:  fitOffset,
			AzimuthDeg:   roundTo(azimuthDeg, 1),
			ElevationDeg: roundTo(elevationDeg, 1),
			RangeKm:      roundTo(rangeKm, 0),
		})
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].FitDeg < scored[j].FitDeg })
	if len(scored) > 0 {
		best = &scored[0]
	}
	if len(scored) > 1 {
		runnerUp = &scored[1]
	}
	return best, runnerUp
}

func medianOf(values []float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return sorted[len(sorted)/2]
}

// classifyMatch names the serving satellite only when the fit is tight and clearly better than the next one.
func classifyMatch(best, runnerUp *ServingCandidate) string {
	if best == nil {
		return ServingNoElements
	}
	if best.FitDeg > ServingMaxFitDeg || (runnerUp != nil && runnerUp.FitDeg-best.FitDeg < ServingMinMarginDeg) {
		return ServingAmbiguous
	}
	return ServingMatched
}

type ServingTracker struct {
	source  ObstructionMapSource
	sky     *SkyTracker
	emit    func(ServingSlot)
	now     func() time.Time
	sleep   func(ctx context.Context, wait time.Duration) bool
	timeout time.Duration

	mutex  sync.RWMutex
	latest *ServingSlot
}

func NewServingTracker(source ObstructionMapSource, sky *SkyTracker, logFile *LogFile[ServingSlot], timeout time.Duration) *ServingTracker {
	return &ServingTracker{
		source: source,
		sky:    sky,
		emit: func(slot ServingSlot) {
			if err := logFile.Append(slot); err != nil {
				log.Printf("serving log write failed: %v", err)
			}
		},
		now:     time.Now,
		sleep:   sleepContext,
		timeout: timeout,
	}
}

// Run clears the obstruction map at the start of every slot and identifies the satellite that traces it, until ctx ends.
func (tracker *ServingTracker) Run(ctx context.Context) {
	for {
		start := nextHandoverBoundary(tracker.now())
		if !tracker.sleep(ctx, start.Add(ServingClearDelay).Sub(tracker.now())) {
			return
		}
		slot, ok := tracker.observeSlot(ctx, start)
		if !ok {
			return
		}
		tracker.mutex.Lock()
		tracker.latest = &slot
		tracker.mutex.Unlock()
		tracker.emit(slot)
	}
}

// Latest returns the most recent finished slot; ok is false before the first one.
func (tracker *ServingTracker) Latest() (slot ServingSlot, ok bool) {
	tracker.mutex.RLock()
	defer tracker.mutex.RUnlock()
	if tracker.latest == nil {
		return ServingSlot{}, false
	}
	return *tracker.latest, true
}

// observeSlot clears the map, reads it once a second and matches the arc. ok is false when ctx ended mid-slot.
func (tracker *ServingTracker) observeSlot(ctx context.Context, start time.Time) (ServingSlot, bool) {
	slot := ServingSlot{Start: start}
	callCtx, cancel := context.WithTimeout(ctx, tracker.timeout)
	err := tracker.source.ClearObstructionMap(callCtx)
	cancel()
	if err != nil {
		slot.Status, slot.Error = ServingError, err.Error()
		return slot, ctx.Err() == nil
	}

	firstSeen := make(map[int]time.Time)
	var latest ObstructionMap
	var lastErr error
	for read := 1; read <= ServingReadsPerSlot; read++ {
		readAt := start.Add(ServingClearDelay + time.Duration(read)*ServingReadInterval)
		if !tracker.sleep(ctx, readAt.Sub(tracker.now())) {
			return slot, false
		}
		callCtx, cancel := context.WithTimeout(ctx, tracker.timeout)
		obstructionMap, err := tracker.source.ObstructionMap(callCtx)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		for cell, snr := range obstructionMap.SNR {
			if _, seen := firstSeen[cell]; snr >= 0 && !seen {
				firstSeen[cell] = readAt.Add(-ServingReadInterval / 2)
			}
		}
		latest = obstructionMap
	}
	if latest.Rows == 0 {
		slot.Status = ServingError
		if lastErr != nil {
			slot.Error = lastErr.Error()
		}
		return slot, true
	}
	if latest.Frame != obstructionFrameEarth {
		slot.Status = ServingUnsupportedFrame
		return slot, true
	}

	pixels := make([]arcPixel, 0, len(firstSeen))
	for cell, at := range firstSeen {
		azimuthDeg, elevationDeg := pixelDirection(cell/latest.Cols, cell%latest.Cols, latest.Rows, latest.MaxThetaDeg)
		pixels = append(pixels, arcPixel{at: at, azimuthDeg: azimuthDeg, elevationDeg: elevationDeg})
	}
	sort.Slice(pixels, func(i, j int) bool { return pixels[i].at.Before(pixels[j].at) })
	slot.Pixels = len(pixels)
	if len(pixels) < ServingMinPixels {
		slot.Status = ServingFewPixels
		return slot, true
	}

	candidates, observer, locationSet := tracker.sky.servingCandidates(start.Add(HandoverSlotSeconds*time.Second/2), ServingCandidateMinElevationDeg)
	if !locationSet {
		slot.Status = ServingNoLocation
		return slot, true
	}
	slot.Best, slot.RunnerUp = matchArc(pixels, candidates, observer)
	slot.Status = classifyMatch(slot.Best, slot.RunnerUp)
	return slot, true
}
