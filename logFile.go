package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"
)

const (
	// Scanner sizing: start with a 64 KB line buffer, refuse anything past 1 MB
	// so a corrupt file can't be read into memory as one enormous "line".
	scannerInitialBufferBytes = 64 * 1024
	scannerMaxLineBytes       = 1 << 20

	// readWholeFile tells scanLogTail not to seek — read from byte zero.
	readWholeFile = -1

	// Tail-size estimation. Records run ~250 bytes at DefaultPollInterval;
	// tailEstimateSlack covers fatter ones, and we never bother reading less
	// than minTailBytes.
	approxBytesPerSample = 250
	tailEstimateSlack    = 4
	minTailBytes         = 1 << 20
)

// Timestamped is anything LogFile can store: it needs a time so pruning and
// range reads know which records to keep.
type Timestamped interface {
	SampleTime() time.Time
}

// LogFile is an append-only JSON Lines file of T, one record per line.
type LogFile[T Timestamped] struct {
	path           string
	mutex          sync.Mutex // serializes Append; concurrent HTTP reads are OS-safe
	file           *os.File
	bufferedWriter *bufio.Writer
}

func OpenLogFile[T Timestamped](path string) (*LogFile[T], error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &LogFile[T]{path: path, file: file, bufferedWriter: bufio.NewWriter(file)}, nil
}

// Append writes one sample to the file as a single JSON line and flushes to disk.
func (logFile *LogFile[T]) Append(sample T) error {
	jsonLine, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	logFile.mutex.Lock()
	defer logFile.mutex.Unlock()
	if _, err := logFile.bufferedWriter.Write(jsonLine); err != nil {
		return err
	}
	if err := logFile.bufferedWriter.WriteByte('\n'); err != nil {
		return err
	}
	return logFile.bufferedWriter.Flush()
}

func (logFile *LogFile[T]) Close() error {
	logFile.mutex.Lock()
	defer logFile.mutex.Unlock()
	if err := logFile.bufferedWriter.Flush(); err != nil {
		return err
	}
	return logFile.file.Close()
}

// Prune rewrites the log file, keeping only samples at or after cutoff.
// Uses write-to-temp + atomic rename so a crash mid-prune never loses data.
func (logFile *LogFile[T]) Prune(cutoff time.Time) error {
	logFile.mutex.Lock()
	defer logFile.mutex.Unlock()

	// Flush and close the current writer — we're about to replace the file
	// underneath it.
	if err := logFile.bufferedWriter.Flush(); err != nil {
		return err
	}
	if err := logFile.file.Close(); err != nil {
		return err
	}

	tempPath := logFile.path + ".tmp"
	kept, total, err := rewriteKeepingSamplesSince[T](logFile.path, tempPath, cutoff)
	if err != nil {
		_ = os.Remove(tempPath) // clean up on failure
		// Best-effort: reopen the original so appends can resume even after a failed prune.
		if reopenErr := logFile.reopenAppend(); reopenErr != nil {
			return fmt.Errorf("prune failed: %w; reopen also failed: %v", err, reopenErr)
		}
		return err
	}

	if err := os.Rename(tempPath, logFile.path); err != nil {
		_ = os.Remove(tempPath)
		if reopenErr := logFile.reopenAppend(); reopenErr != nil {
			return fmt.Errorf("rename failed: %w; reopen also failed: %v", err, reopenErr)
		}
		return err
	}

	if dropped := total - kept; dropped > 0 {
		log.Printf("pruned %d samples older than %s (kept %d)", dropped, cutoff.Format(time.RFC3339), kept)
	}
	return logFile.reopenAppend()
}

// reopenAppend restores logFile.file and logFile.bufferedWriter after a prune.
// Assumes the caller already holds the mutex.
func (logFile *LogFile[T]) reopenAppend() error {
	file, err := os.OpenFile(logFile.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	logFile.file = file
	logFile.bufferedWriter = bufio.NewWriter(file)
	return nil
}

// rewriteKeepingSamplesSince reads sourcePath line-by-line and writes the
// samples at or after cutoff to destPath. Returns (kept, total, err).
// Corrupted lines are skipped (matching the read-side behavior elsewhere).
func rewriteKeepingSamplesSince[T Timestamped](sourcePath, destPath string, cutoff time.Time) (kept, total int, err error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil // nothing to prune
		}
		return 0, 0, err
	}
	defer source.Close()

	dest, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, 0, err
	}
	writer := bufio.NewWriter(dest)

	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, scannerInitialBufferBytes), scannerMaxLineBytes)

	for scanner.Scan() {
		total++
		var sample T
		if err := json.Unmarshal(scanner.Bytes(), &sample); err != nil {
			continue // skip corrupt lines
		}
		if sample.SampleTime().Before(cutoff) {
			continue
		}
		if _, err := writer.Write(scanner.Bytes()); err != nil {
			dest.Close()
			return kept, total, err
		}
		if err := writer.WriteByte('\n'); err != nil {
			dest.Close()
			return kept, total, err
		}
		kept++
	}
	if err := scanner.Err(); err != nil {
		dest.Close()
		return kept, total, err
	}
	if err := writer.Flush(); err != nil {
		dest.Close()
		return kept, total, err
	}
	if err := dest.Close(); err != nil {
		return kept, total, err
	}
	return kept, total, nil
}

// LoadSamplesSince returns every sample at or after oldestWanted, oldest first.
// A zero oldestWanted reads the whole file.
func LoadSamplesSince[T Timestamped](logPath string, oldestWanted time.Time) ([]T, error) {
	samples, didSeek, err := scanLogTail[T](logPath, oldestWanted, estimateTailBytes(oldestWanted))
	if err != nil {
		return nil, err
	}
	// We guessed how far back to seek. If we started mid-file and the oldest
	// record we found is still newer than the cutoff, records we wanted sit
	// before our starting offset — the guess was too small, so re-read fully.
	if didSeek && len(samples) > 0 && samples[0].SampleTime().After(oldestWanted) {
		samples, _, err = scanLogTail[T](logPath, oldestWanted, readWholeFile)
		if err != nil {
			return nil, err
		}
	}
	return samples, nil
}

// estimateTailBytes guesses how many trailing bytes could hold the requested
// span, so a 2-minute request doesn't parse a month of history. LoadSamplesSince
// re-reads from the top if the estimate came up short.
func estimateTailBytes(oldestWanted time.Time) int64 {
	if oldestWanted.IsZero() {
		return readWholeFile
	}
	requestedSpan := time.Since(oldestWanted)
	if requestedSpan <= 0 {
		requestedSpan = time.Minute
	}
	estimatedBytes := int64(requestedSpan/DefaultPollInterval) * approxBytesPerSample * tailEstimateSlack
	if estimatedBytes < minTailBytes {
		estimatedBytes = minTailBytes
	}
	return estimatedBytes
}

// scanLogTail reads the last maxTailBytes of the log (or all of it when
// maxTailBytes is readWholeFile), returning in-range samples and whether it had
// to seek to get there.
func scanLogTail[T Timestamped](logPath string, oldestWanted time.Time, maxTailBytes int64) (samples []T, didSeek bool, err error) {
	file, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return nil, false, err
	}

	startOffset := int64(0)
	if maxTailBytes > 0 && fileInfo.Size() > maxTailBytes {
		startOffset = fileInfo.Size() - maxTailBytes
		didSeek = true
	}
	if _, err := file.Seek(startOffset, io.SeekStart); err != nil {
		return nil, false, err
	}

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, scannerInitialBufferBytes), scannerMaxLineBytes)

	skipPartialFirstLine := didSeek // first line after a seek is probably a fragment
	for scanner.Scan() {
		if skipPartialFirstLine {
			skipPartialFirstLine = false
			continue
		}
		var sample T
		// Corrupted lines (a partial write from a crash, or the tail of a
		// record being appended right now) are skipped silently.
		if err := json.Unmarshal(scanner.Bytes(), &sample); err != nil {
			continue
		}
		if oldestWanted.IsZero() || !sample.SampleTime().Before(oldestWanted) {
			samples = append(samples, sample)
		}
	}
	return samples, didSeek, scanner.Err()
}

// DownsampleWorstCase collapses samples into at most maxPoints time buckets,
// keeping the worst reading in each, and reports the resulting bucket width.
//
// Buckets are cut by time rather than by index so that an outage stays an
// outage: a stretch with no samples produces no points, and the client's gap
// detection still renders it as blank space.
//
// Worst-case rather than mean is deliberate. Averaging a three-second dropout
// into a minute-wide bucket hides exactly the event this dashboard exists to
// show. The cost is that at long ranges you are reading peaks, not readings —
// a 24h chart showing 400 ms is saying "something touched 400 ms in that
// bucket", not "latency was 400 ms".
func DownsampleWorstCase(samples []TelemetrySample, windowStart, windowEnd time.Time, maxPoints int) ([]TelemetrySample, time.Duration) {
	windowSpan := windowEnd.Sub(windowStart)
	if maxPoints <= 0 || len(samples) <= maxPoints || windowSpan <= 0 {
		return samples, DefaultPollInterval
	}
	bucketWidth := windowSpan / time.Duration(maxPoints)
	if bucketWidth <= 0 {
		return samples, DefaultPollInterval
	}

	downsampled := make([]TelemetrySample, 0, maxPoints+1)
	worstInBucket := TelemetrySample{}
	currentBucketIndex := int64(-1)

	for _, sample := range samples {
		bucketIndex := int64(sample.Timestamp.Sub(windowStart) / bucketWidth)
		if bucketIndex != currentBucketIndex {
			if currentBucketIndex >= 0 {
				downsampled = append(downsampled, worstInBucket)
			}
			worstInBucket, currentBucketIndex = sample, bucketIndex
			continue
		}
		worstInBucket = mergeWorstCase(worstInBucket, sample)
	}
	if currentBucketIndex >= 0 {
		downsampled = append(downsampled, worstInBucket)
	}
	return downsampled, bucketWidth
}

// mergeWorstCase folds candidate into worst, keeping the least flattering value
// of each field. The timestamp stays at the bucket's first sample so output is
// monotonic.
func mergeWorstCase(worst, candidate TelemetrySample) TelemetrySample {
	if candidate.LatencyMs > worst.LatencyMs {
		worst.LatencyMs = candidate.LatencyMs
	}
	if candidate.DownloadMbps > worst.DownloadMbps {
		worst.DownloadMbps = candidate.DownloadMbps
	}
	if candidate.UploadMbps > worst.UploadMbps {
		worst.UploadMbps = candidate.UploadMbps
	}
	if candidate.DropRateFraction > worst.DropRateFraction {
		worst.DropRateFraction = candidate.DropRateFraction
	}
	if candidate.ObstructionFraction > worst.ObstructionFraction {
		worst.ObstructionFraction = candidate.ObstructionFraction
	}
	worst.Obstructed = worst.Obstructed || candidate.Obstructed

	if linkStateSeverity(candidate.LinkState) > linkStateSeverity(worst.LinkState) {
		worst.LinkState = candidate.LinkState
	}
	// A bucket only reads OFFLINE if every poll in it failed — one bad poll
	// among good ones shouldn't blank the bucket, since the client drops
	// OFFLINE samples from the charts entirely.
	if worst.LinkState != LinkStateOffline {
		worst.PollError = ""
	}

	worst.UptimeSeconds = candidate.UptimeSeconds
	if candidate.HardwareVersion != "" {
		worst.HardwareVersion = candidate.HardwareVersion
	}
	if candidate.SoftwareVersion != "" {
		worst.SoftwareVersion = candidate.SoftwareVersion
	}
	return worst
}

// linkStateSeverity ranks link states for merging. OFFLINE sorts below every
// real reading so that any successful poll in a bucket wins.
func linkStateSeverity(linkState string) int {
	switch linkState {
	case LinkStateOnline:
		return 1
	case LinkStateDegraded:
		return 2
	case LinkStateObstructed:
		return 3
	case LinkStateNoSignal:
		return 4
	default:
		return 0
	}
}
