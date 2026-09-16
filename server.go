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
	"strings"
	"sync"
	"time"
)

// shutdownGracePeriod is how long in-flight requests get to finish after Ctrl+C.
const shutdownGracePeriod = 3 * time.Second

//go:embed static/*
var embeddedStaticFS embed.FS

func serveDashboard(ctx context.Context, listenAddress string, hub *TelemetryHub, tracker *SkyTracker, logPath string) error {
	staticRoot, err := fs.Sub(embeddedStaticFS, "static")
	if err != nil {
		return err
	}

	router := http.NewServeMux()
	router.Handle(RouteIndex, http.FileServer(http.FS(staticRoot)))
	router.HandleFunc(RouteEvents, newSampleStreamHandler(hub))
	router.HandleFunc(RouteLog, newRawLogHandler(logPath))
	router.HandleFunc(RouteHistory, newHistoryHandler(logPath))
	router.HandleFunc(RouteSky, func(response http.ResponseWriter, request *http.Request) {
		http.ServeFileFS(response, request, staticRoot, "sky.html")
	})
	router.HandleFunc(RouteSkyEvents, newSkyStreamHandler(tracker))

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
func newSampleStreamHandler(hub *TelemetryHub) http.HandlerFunc {
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

// subscriberQueueDepth is how many samples a single browser can fall behind
// before we start dropping its updates rather than stalling every other client.
const subscriberQueueDepth = 16

// TelemetryHub fans each new sample out to every connected browser and keeps a
// short rolling history, so a tab that connects late has something to draw.
type TelemetryHub struct {
	mutex              sync.Mutex
	recentSamples      []TelemetrySample                 // ring buffer, chronological order
	maxRetainedSamples int                               // how many samples recentSamples holds
	subscribers        map[chan TelemetrySample]struct{} // one channel per connected browser
}

func NewTelemetryHub(maxRetainedSamples int) *TelemetryHub {
	return &TelemetryHub{
		recentSamples:      make([]TelemetrySample, 0, maxRetainedSamples),
		maxRetainedSamples: maxRetainedSamples,
		subscribers:        make(map[chan TelemetrySample]struct{}),
	}
}

// SeedHistory pre-fills the rolling history, normally from the log file at startup.
func (hub *TelemetryHub) SeedHistory(samples []TelemetrySample) {
	hub.mutex.Lock()
	defer hub.mutex.Unlock()
	if len(samples) > hub.maxRetainedSamples {
		samples = samples[len(samples)-hub.maxRetainedSamples:]
	}
	hub.recentSamples = append(hub.recentSamples[:0], samples...)
}

func (hub *TelemetryHub) PublishSample(sample TelemetrySample) {
	hub.mutex.Lock()
	hub.recentSamples = append(hub.recentSamples, sample)
	if len(hub.recentSamples) > hub.maxRetainedSamples {
		hub.recentSamples = hub.recentSamples[len(hub.recentSamples)-hub.maxRetainedSamples:]
	}

	currentSubscribers := make([]chan TelemetrySample, 0, len(hub.subscribers))
	for subscriber := range hub.subscribers {
		currentSubscribers = append(currentSubscribers, subscriber)
	}
	hub.mutex.Unlock()

	for _, subscriber := range currentSubscribers {
		select {
		case subscriber <- sample:
		default:
		}
	}
}

// Subscribe hands back a live stream, a copy of the history so far, and
// the function the caller must invoke to detach.
func (hub *TelemetryHub) Subscribe() (stream <-chan TelemetrySample, backfill []TelemetrySample, unsubscribe func()) {
	hub.mutex.Lock()
	defer hub.mutex.Unlock()

	subscriber := make(chan TelemetrySample, subscriberQueueDepth)
	hub.subscribers[subscriber] = struct{}{}

	snapshot := make([]TelemetrySample, len(hub.recentSamples))
	copy(snapshot, hub.recentSamples)

	return subscriber, snapshot, func() {
		hub.mutex.Lock()
		if _, stillSubscribed := hub.subscribers[subscriber]; stillSubscribed {
			delete(hub.subscribers, subscriber)
			close(subscriber)
		}
		hub.mutex.Unlock()
	}
}

// historyResponse is what RouteHistory returns. BucketWidthMs tells the client how
// far apart adjacent points are so it can size its gap-detection threshold —
// without it a downsampled series renders as disconnected fragments.
type historyResponse struct {
	RequestedRange string            `json:"requested_range"`
	WindowStart    time.Time         `json:"window_start"`
	BucketWidthMs  int64             `json:"bucket_width_ms"`
	RawCount       int               `json:"raw_count"`  // samples read before downsampling
	KeptCount      int               `json:"kept_count"` // samples actually returned
	Samples        []TelemetrySample `json:"samples"`
}

func newHistoryHandler(logPath string) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		requestedRange := strings.TrimSpace(request.URL.Query().Get("range"))

		// A zero oldestWanted means "read everything".
		var oldestWanted time.Time
		if requestedRange != "" && requestedRange != RangeAll {
			requestedSpan, err := time.ParseDuration(requestedRange)
			if err != nil || requestedSpan <= 0 {
				http.Error(response, "range must be a duration like 15m, or "+RangeAll, http.StatusBadRequest)
				return
			}
			if requestedSpan > MaxHistoryRange {
				requestedSpan = MaxHistoryRange
			}
			oldestWanted = time.Now().Add(-requestedSpan)
		}

		samples, err := LoadSamplesSince(logPath, oldestWanted)
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
				windowStart = samples[0].Timestamp
			}
		}

		points, bucketWidth := DownsampleWorstCase(samples, windowStart, now, MaxHistoryPoints)

		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(response).Encode(historyResponse{
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
