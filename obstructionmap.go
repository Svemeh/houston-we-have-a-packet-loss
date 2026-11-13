package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	ObstructionSourceMap = "map"

	obstructionMapFileVersion = 1
)

type mapCell struct {
	clear      int
	obstructed int
	lastSNR    float32
	lastSeen   time.Time
}

func (cell mapCell) seen() bool { return cell.clear+cell.obstructed > 0 }

func (cell mapCell) isObstructed() bool {
	return cell.obstructed > cell.clear || (cell.obstructed == cell.clear && cell.seen() && cell.lastSNR < ObstructedSNRBelow)
}

// ObstructionStatus: Coverage is the traced share of the sky above the service floor, Fraction the obstructed
// share of traced cells (nil below ObstructionMapMinCoverage), ObstructedNow valid only when LiveKnown.
type ObstructionStatus struct {
	Coverage      float64
	Fraction      *float64
	ObstructedNow bool
	LiveKnown     bool
}

// ObstructionMapper merges the cells the dish traces each slot into one map, so obstruction survives the per-slot clears.
type ObstructionMapper struct {
	path  string
	floor func() float64

	mutex          sync.Mutex
	rows, cols     int
	maxThetaDeg    float64
	cells          []mapCell
	elevations     []float64
	dirty          bool
	liveObstructed bool
	liveAt         time.Time
}

// LoadObstructionMapper reads the saved map; a missing or unreadable file starts an empty one.
func LoadObstructionMapper(path string, floor func() float64) *ObstructionMapper {
	mapper := &ObstructionMapper{path: path, floor: floor}
	if err := mapper.load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("obstruction map: starting empty, could not read %s: %v", path, err)
	}
	return mapper
}

func (mapper *ObstructionMapper) resetGrid(rows, cols int, maxThetaDeg float64) {
	mapper.rows, mapper.cols, mapper.maxThetaDeg = rows, cols, maxThetaDeg
	mapper.cells = make([]mapCell, rows*cols)
	mapper.elevations = make([]float64, rows*cols)
	for cell := range mapper.elevations {
		_, mapper.elevations[cell] = pixelDirection(cell/cols, cell%cols, rows, maxThetaDeg)
	}
}

func (mapper *ObstructionMapper) sameGrid(obstructionMap ObstructionMap) bool {
	return mapper.rows == obstructionMap.Rows && mapper.cols == obstructionMap.Cols && mapper.maxThetaDeg == obstructionMap.MaxThetaDeg
}

// ObserveRead updates "obstructed now" from the cells the dish wrote since the previous read of this slot.
func (mapper *ObstructionMapper) ObserveRead(obstructionMap ObstructionMap, changed []int, at time.Time) {
	if len(changed) == 0 {
		return
	}
	obstructed := false
	for _, cell := range changed {
		if obstructionMap.SNR[cell] < ObstructedSNRBelow {
			obstructed = true
		}
	}
	mapper.mutex.Lock()
	defer mapper.mutex.Unlock()
	mapper.liveObstructed, mapper.liveAt = obstructed, at
}

// MergeSlot adds the last read before a clear, which holds every cell traced in that slot.
func (mapper *ObstructionMapper) MergeSlot(obstructionMap ObstructionMap, at time.Time) {
	mapper.mutex.Lock()
	defer mapper.mutex.Unlock()
	if !mapper.sameGrid(obstructionMap) {
		if mapper.rows != 0 {
			log.Printf("obstruction map: grid changed to %dx%d, %.0f°; starting over", obstructionMap.Rows, obstructionMap.Cols, obstructionMap.MaxThetaDeg)
		}
		mapper.resetGrid(obstructionMap.Rows, obstructionMap.Cols, obstructionMap.MaxThetaDeg)
	}
	for index, snr := range obstructionMap.SNR {
		if snr < 0 {
			continue
		}
		cell := &mapper.cells[index]
		if snr < ObstructedSNRBelow {
			cell.obstructed++
		} else {
			cell.clear++
		}
		if cell.clear+cell.obstructed > ObstructionCellMaxCount {
			cell.clear, cell.obstructed = (cell.clear+1)/2, (cell.obstructed+1)/2
		}
		cell.lastSNR, cell.lastSeen = snr, at
		mapper.dirty = true
	}
}

// Status measures coverage and obstruction over the cells above the service floor.
func (mapper *ObstructionMapper) Status(now time.Time) ObstructionStatus {
	floorDeg := mapper.floor()
	mapper.mutex.Lock()
	defer mapper.mutex.Unlock()

	var status ObstructionStatus
	if now.Sub(mapper.liveAt) <= ObstructionLiveMaxAge {
		status.LiveKnown, status.ObstructedNow = true, mapper.liveObstructed
	}
	total, seen, obstructed := 0, 0, 0
	for index, cell := range mapper.cells {
		if mapper.elevations[index] < floorDeg {
			continue
		}
		total++
		if cell.seen() {
			seen++
			if cell.isObstructed() {
				obstructed++
			}
		}
	}
	if total == 0 {
		return status
	}
	status.Coverage = float64(seen) / float64(total)
	if status.Coverage >= ObstructionMapMinCoverage {
		fraction := float64(obstructed) / float64(seen)
		status.Fraction = &fraction
	}
	return status
}

// Apply replaces the dish's obstruction readings, which the clears reset, with the merged map's.
func (mapper *ObstructionMapper) Apply(sample *TelemetrySample) {
	if sample.LinkState == LinkStateOffline {
		return
	}
	status := mapper.Status(sample.Timestamp)
	sample.DishObstructed = sample.Obstructed
	sample.Obstructed = sample.Obstructed || (status.LiveKnown && status.ObstructedNow)
	sample.ObstructionFraction = status.Fraction
	sample.ObstructionSource = ObstructionSourceMap
	sample.MapCoverage = roundTo(status.Coverage, 3)
	sample.LinkState = deriveLinkState(sample.DropRateFraction, sample.Obstructed)
}

// ObstructionMapView is RouteObstructionMap's payload. Cells holds one character per cell:
// '0' never traced, '1' clear, '2' obstructed.
type ObstructionMapView struct {
	Enabled       bool     `json:"enabled"`
	Rows          int      `json:"rows,omitempty"`
	Cols          int      `json:"cols,omitempty"`
	MaxThetaDeg   float64  `json:"max_theta_deg,omitempty"`
	FloorDeg      float64  `json:"floor_deg,omitempty"`
	Coverage      float64  `json:"coverage"`
	MinCoverage   float64  `json:"min_coverage"`
	Fraction      *float64 `json:"obstruction_fraction"`
	ObstructedNow *bool    `json:"obstructed_now"`
	Cells         string   `json:"cells,omitempty"`
}

func (mapper *ObstructionMapper) View(now time.Time) ObstructionMapView {
	status := mapper.Status(now)
	view := ObstructionMapView{
		Enabled:     true,
		FloorDeg:    roundTo(mapper.floor(), 1),
		Coverage:    roundTo(status.Coverage, 3),
		MinCoverage: ObstructionMapMinCoverage,
		Fraction:    status.Fraction,
	}
	if status.LiveKnown {
		view.ObstructedNow = &status.ObstructedNow
	}
	mapper.mutex.Lock()
	defer mapper.mutex.Unlock()
	view.Rows, view.Cols, view.MaxThetaDeg = mapper.rows, mapper.cols, mapper.maxThetaDeg
	var cells strings.Builder
	cells.Grow(len(mapper.cells))
	for _, cell := range mapper.cells {
		switch {
		case !cell.seen():
			cells.WriteByte('0')
		case cell.isObstructed():
			cells.WriteByte('2')
		default:
			cells.WriteByte('1')
		}
	}
	view.Cells = cells.String()
	return view
}

type savedCell struct {
	Index      int     `json:"i"`
	Clear      int     `json:"c"`
	Obstructed int     `json:"o"`
	LastSNR    float32 `json:"s"`
	LastSeen   int64   `json:"t"`
}

type savedObstructionMap struct {
	Version     int         `json:"version"`
	Rows        int         `json:"rows"`
	Cols        int         `json:"cols"`
	MaxThetaDeg float64     `json:"max_theta_deg"`
	Saved       time.Time   `json:"saved"`
	Cells       []savedCell `json:"cells"`
}

// Save writes the traced cells only, through a temp file and a rename.
func (mapper *ObstructionMapper) Save(now time.Time) error {
	mapper.mutex.Lock()
	if !mapper.dirty {
		mapper.mutex.Unlock()
		return nil
	}
	saved := savedObstructionMap{Version: obstructionMapFileVersion, Rows: mapper.rows, Cols: mapper.cols, MaxThetaDeg: mapper.maxThetaDeg, Saved: now, Cells: []savedCell{}}
	for index, cell := range mapper.cells {
		if cell.seen() {
			saved.Cells = append(saved.Cells, savedCell{index, cell.clear, cell.obstructed, cell.lastSNR, cell.lastSeen.Unix()})
		}
	}
	mapper.dirty = false
	mapper.mutex.Unlock()

	raw, err := json.Marshal(saved)
	if err == nil {
		err = writeFileAtomic(mapper.path, append(raw, '\n'))
	}
	if err != nil {
		mapper.mutex.Lock()
		mapper.dirty = true
		mapper.mutex.Unlock()
	}
	return err
}

func (mapper *ObstructionMapper) load() error {
	raw, err := os.ReadFile(mapper.path)
	if err != nil {
		return err
	}
	var saved savedObstructionMap
	if err := json.Unmarshal(raw, &saved); err != nil {
		return err
	}
	if saved.Version != obstructionMapFileVersion || saved.Rows <= 1 || saved.Cols <= 0 || saved.MaxThetaDeg <= 0 {
		return fmt.Errorf("unsupported map: version %d, %dx%d", saved.Version, saved.Rows, saved.Cols)
	}
	mapper.mutex.Lock()
	defer mapper.mutex.Unlock()
	mapper.resetGrid(saved.Rows, saved.Cols, saved.MaxThetaDeg)
	for _, cell := range saved.Cells {
		if cell.Index < 0 || cell.Index >= len(mapper.cells) {
			continue
		}
		mapper.cells[cell.Index] = mapCell{clear: cell.Clear, obstructed: cell.Obstructed, lastSNR: cell.LastSNR, lastSeen: time.Unix(cell.LastSeen, 0)}
	}
	return nil
}

// Run saves the map every ObstructionMapSaveInterval until ctx ends; main saves once more on exit.
func (mapper *ObstructionMapper) Run(ctx context.Context) {
	ticker := time.NewTicker(ObstructionMapSaveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := mapper.Save(time.Now()); err != nil {
				log.Printf("obstruction map save failed: %v", err)
			}
		}
	}
}
