package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// shutdownGracePeriod is how long in-flight requests get to finish after Ctrl+C.
const shutdownGracePeriod = 3 * time.Second

//go:embed static/*
var embeddedStaticFS embed.FS

type dashboardSources struct {
	telemetryHub *Hub[TelemetrySample]
	pingHub      *Hub[PingSample]
	hopsHub      *Hub[HopSample]
	tracker      *SkyTracker

	// Paths rather than sources, because the raw-log routes stream the file
	// itself rather than parsing it.
	telemetryLogPath string
	pingLogPath      string
	hopsLogPath      string
	rollupLogPath    string
}

func serveDashboard(ctx context.Context, listenAddress string, sources dashboardSources) error {
	staticRoot, err := fs.Sub(embeddedStaticFS, "static")
	if err != nil {
		return err
	}

	router := http.NewServeMux()
	router.Handle(RouteIndex, http.FileServer(http.FS(staticRoot)))

	router.HandleFunc(RouteEvents, newSampleStreamHandler(sources.telemetryHub))
	router.HandleFunc(RouteLog, newRawLogHandler(sources.telemetryLogPath))
	router.HandleFunc(RouteHistory, newHistoryHandler(
		NewLogFileSource[TelemetrySample](sources.telemetryLogPath), DownsampleWorstCase, MaxHistoryRange, MaxHistoryPoints))

	router.HandleFunc(RoutePingEvents, newSampleStreamHandler(sources.pingHub))
	router.HandleFunc(RoutePingLog, newRawLogHandler(sources.pingLogPath))
	router.HandleFunc(RoutePingHistory, newHistoryHandler(
		NewLogFileSource[PingSample](sources.pingLogPath), DownsamplePing, MaxHistoryRange, MaxHistoryPoints))

	router.HandleFunc(RouteHopsEvents, newSampleStreamHandler(sources.hopsHub))
	router.HandleFunc(RouteHopsLog, newRawLogHandler(sources.hopsLogPath))
	router.HandleFunc(RouteHopsHistory, newHistoryHandler(
		NewLogFileSource[HopSample](sources.hopsLogPath), DownsampleHops, MaxHistoryRange, MaxHistoryPoints))

	router.HandleFunc(RouteRollup, newHistoryHandler(
		NewLogFileSource[HopRollup](sources.rollupLogPath), newestRollups, MaxRollupRange, MaxRollupPoints))

	router.HandleFunc(RouteSky, func(response http.ResponseWriter, request *http.Request) {
		http.ServeFileFS(response, request, staticRoot, "sky.html")
	})
	router.HandleFunc(RouteSkyEvents, newSkyStreamHandler(sources.tracker))

	server := &http.Server{Addr: listenAddress, Handler: router}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownGracePeriod)
		defer cancelShutdown()
		_ = server.Shutdown(shutdownCtx)
	}()

	displayAddress := listenAddress
	if strings.HasPrefix(displayAddress, ":") {
		displayAddress = "localhost" + displayAddress
	}
	log.Printf("%s — dashboard live at http://%s (Ctrl+C to stop)", AppName, displayAddress)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// newSampleStreamHandler serves RouteEvents: one Server-Sent Events stream per
// browser, opening with a backfill of the hub's recent history.
func newSampleStreamHandler[T any](hub *Hub[T]) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		flusher, canFlush := response.(http.Flusher)
		if !canFlush {
			http.Error(response, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		response.Header().Set("Content-Type", "text/event-stream")
		response.Header().Set("Cache-Control", "no-cache")
		response.Header().Set("Connection", "keep-alive")

		sampleStream, backfill, unsubscribe := hub.Subscribe()
		defer unsubscribe()

		if len(backfill) > 0 {
			payload, err := json.Marshal(backfill)
			if err == nil {
				fmt.Fprintf(response, "event: backfill\ndata: %s\n\n", payload)
				flusher.Flush()
			}
		}

		for {
			select {
			case <-request.Context().Done():
				return
			case sample, streamOpen := <-sampleStream:
				if !streamOpen {
					return
				}
				payload, err := json.Marshal(sample)
				if err != nil {
					continue
				}
				fmt.Fprintf(response, "data: %s\n\n", payload)
				flusher.Flush()
			}
		}
	}
}

// newRawLogHandler serves RouteLog: the JSONL file, streamed back verbatim.
func newRawLogHandler(logPath string) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		response.Header().Set("Cache-Control", "no-cache")

		logFile, err := os.Open(logPath)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Fprintln(response, "# no telemetry recorded yet")
				return
			}
			http.Error(response, "log unavailable: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer logFile.Close()
		_, _ = io.Copy(response, logFile)
	}
}

// historyResponse is what the history routes return. BucketWidthMs tells the client how
// far apart adjacent points are so it can size its gap-detection threshold —
// without it a downsampled series renders as disconnected fragments.
type historyResponse[P any] struct {
	RequestedRange string    `json:"requested_range"`
	WindowStart    time.Time `json:"window_start"`
	BucketWidthMs  int64     `json:"bucket_width_ms"`
	RawCount       int       `json:"raw_count"`  // samples read before downsampling
	KeptCount      int       `json:"kept_count"` // samples actually returned
	Samples        []P       `json:"samples"`
}

// downsampler turns a range of logged samples into at most maxPoints chart
// points, reporting the bucket width it used.
type downsampler[S Timestamped, P any] func(samples []S, windowStart, windowEnd time.Time, maxPoints int) ([]P, time.Duration)

// newHistoryHandler serves a log's history over a requested range, shaped by
// that log's own downsampler.
func newHistoryHandler[S Timestamped, P any](source SampleSource[S], downsample downsampler[S, P], maxRange time.Duration, maxPoints int) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		requestedRange := strings.TrimSpace(request.URL.Query().Get("range"))

		// A zero oldestWanted means "read everything".
		var oldestWanted time.Time
		if requestedRange != "" && requestedRange != RangeAll {
			requestedSpan, err := parseRange(requestedRange)
			if err != nil || requestedSpan <= 0 {
				http.Error(response, "range must be a duration like 15m or 30d, or "+RangeAll, http.StatusBadRequest)
				return
			}
			if requestedSpan > maxRange {
				requestedSpan = maxRange
			}
			oldestWanted = time.Now().Add(-requestedSpan)
		}

		samples, err := source.Samples(request.Context(), SampleQuery{Since: oldestWanted})
		if err != nil {
			http.Error(response, "history unavailable: "+err.Error(), http.StatusInternalServerError)
			return
		}

		now := time.Now()
		windowStart := oldestWanted
		if windowStart.IsZero() {
			// "All" — the window begins at the oldest record we have.
			windowStart = now
			if len(samples) > 0 {
				windowStart = samples[0].SampleTime()
			}
		}

		points, bucketWidth := downsample(samples, windowStart, now, maxPoints)

		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(response).Encode(historyResponse[P]{
			RequestedRange: requestedRange,
			WindowStart:    windowStart,
			BucketWidthMs:  bucketWidth.Milliseconds(),
			RawCount:       len(samples),
			KeptCount:      len(points),
			Samples:        points,
		})
	}
}

func newSkyStreamHandler(tracker *SkyTracker) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		flusher, canFlush := response.(http.Flusher)
		if !canFlush {
			http.Error(response, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		response.Header().Set("Content-Type", "text/event-stream")
		response.Header().Set("Cache-Control", "no-cache")
		response.Header().Set("Connection", "keep-alive")

		snapshotStream, unsubscribe := tracker.Subscribe()
		defer unsubscribe()

		for {
			select {
			case <-request.Context().Done():
				return
			case snapshot, streamOpen := <-snapshotStream:
				if !streamOpen {
					return
				}
				payload, err := json.Marshal(snapshot)
				if err != nil {
					continue
				}
				fmt.Fprintf(response, "data: %s\n\n", payload)
				flusher.Flush()
			}
		}
	}
}

func parseRange(requestedRange string) (time.Duration, error) {
	if days, isDays := strings.CutSuffix(requestedRange, "d"); isDays {
		dayCount, err := strconv.ParseFloat(days, 64)
		if err != nil {
			return 0, err
		}
		return time.Duration(dayCount * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(requestedRange)
}

func newestRollups(rollups []HopRollup, _, _ time.Time, maxPoints int) ([]HopRollup, time.Duration) {
	if maxPoints > 0 && len(rollups) > maxPoints {
		rollups = rollups[len(rollups)-maxPoints:]
	}
	return rollups, RollupInterval
}
