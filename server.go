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

// dashboardSources is everything the HTTP layer serves: a live hub per stream,
// and the log each history route reads back through.
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
	outageLogPath    string

	settingsPath string
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

	router.HandleFunc(RouteIncidents, newIncidentHandler(
		NewLogFileSource[HopSample](sources.hopsLogPath),
		NewLogFileSource[DishOutage](sources.outageLogPath)))

	router.HandleFunc(RouteSky, func(response http.ResponseWriter, request *http.Request) {
		http.ServeFileFS(response, request, staticRoot, "sky.html")
	})
	router.HandleFunc(RouteSkyEvents, newSkyStreamHandler(sources.tracker))
	router.HandleFunc(RouteLocation, newLocationHandler(sources.tracker, sources.settingsPath))

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

// parseRange is time.ParseDuration plus a day suffix, which it has no unit for
// and the rollup route needs: that log is kept for years, not hours.
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

// newestRollups is the rollup route's downsampler: hourly records are already
// coarse, so there is nothing to collapse. It only caps the response, keeping
// the newest rows, so a multi-year range can't return the whole file at once.
func newestRollups(rollups []HopRollup, _, _ time.Time, maxPoints int) ([]HopRollup, time.Duration) {
	if maxPoints > 0 && len(rollups) > maxPoints {
		rollups = rollups[len(rollups)-maxPoints:]
	}
	return rollups, RollupInterval
}

// incidentResponse is what RouteIncidents returns.
type incidentResponse struct {
	RequestedRange string     `json:"requested_range"`
	WindowStart    time.Time  `json:"window_start"`
	Count          int        `json:"count"`
	Incidents      []Incident `json:"incidents"`
}

// newIncidentHandler serves RouteIncidents: the notable moments in a window,
// newest first. Incidents are derived here rather than stored, so the two logs
// are read on every request and the thresholds are whatever const.go says now.
//
// An optional kind= filters to one of loss, latency or outage. Without it the
// list leans heavily towards latency, which is the most common kind by far.
func newIncidentHandler(hops SampleSource[HopSample], outages SampleSource[DishOutage]) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		requestedRange := strings.TrimSpace(request.URL.Query().Get("range"))
		wantedKind := strings.TrimSpace(request.URL.Query().Get("kind"))

		var oldestWanted time.Time
		if requestedRange != "" && requestedRange != RangeAll {
			requestedSpan, err := parseRange(requestedRange)
			if err != nil || requestedSpan <= 0 {
				http.Error(response, "range must be a duration like 15m or 30d, or "+RangeAll, http.StatusBadRequest)
				return
			}
			if requestedSpan > MaxHistoryRange {
				requestedSpan = MaxHistoryRange
			}
			oldestWanted = time.Now().Add(-requestedSpan)
		}

		query := SampleQuery{Since: oldestWanted}
		hopSamples, err := hops.Samples(request.Context(), query)
		if err != nil {
			http.Error(response, "incidents unavailable: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// A missing outage log is normal — the fake collector never writes one —
		// so its incidents are simply absent rather than an error.
		dishOutages, err := outages.Samples(request.Context(), query)
		if err != nil {
			log.Printf("incidents: could not read dish outages: %v", err)
		}

		incidents := DetectIncidents(hopSamples, dishOutages, 0)
		if wantedKind != "" {
			kept := incidents[:0]
			for _, incident := range incidents {
				if incident.Kind == wantedKind {
					kept = append(kept, incident)
				}
			}
			incidents = kept
		}
		if len(incidents) > MaxIncidents {
			incidents = incidents[:MaxIncidents]
		}

		windowStart := oldestWanted
		if windowStart.IsZero() && len(hopSamples) > 0 {
			windowStart = hopSamples[0].Timestamp
		}

		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(response).Encode(incidentResponse{
			RequestedRange: requestedRange,
			WindowStart:    windowStart,
			Count:          len(incidents),
			Incidents:      incidents,
		})
	}
}

// locationResponse is what RouteLocation returns.
type locationResponse struct {
	LocationSet  bool    `json:"location_set"`
	Source       string  `json:"source,omitempty"` // LocationFromEnv or LocationFromSettings
	LatitudeDeg  float64 `json:"latitude_deg,omitempty"`
	LongitudeDeg float64 `json:"longitude_deg,omitempty"`
}

// maxLocationBodyBytes caps a POST to RouteLocation; the real payload is ~60 bytes.
const maxLocationBodyBytes = 1 << 10

// newLocationHandler serves RouteLocation. GET reports the current position,
// POST {"latitude_deg":..,"longitude_deg":..} saves a new one to the settings
// file and applies it straight away.
//
// POST only takes application/json: a cross-site form can't send that without
// a CORS preflight, which this server never approves, so another web page
// can't move the dish behind the user's back.
func newLocationHandler(tracker *SkyTracker, settingsPath string) http.HandlerFunc {
	respond := func(response http.ResponseWriter) {
		observer, source := tracker.Location()
		payload := locationResponse{LocationSet: source != "", Source: source}
		if source != "" {
			payload.LatitudeDeg, payload.LongitudeDeg = observer.LatitudeDeg, observer.LongitudeDeg
		}
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(response).Encode(payload)
	}

	return func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet, http.MethodHead:
			respond(response)
			return
		case http.MethodPost:
		default:
			response.Header().Set("Allow", "GET, POST")
			http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if !strings.HasPrefix(request.Header.Get("Content-Type"), "application/json") {
			http.Error(response, "send JSON", http.StatusUnsupportedMediaType)
			return
		}
		if _, source := tracker.Location(); source == LocationFromEnv {
			http.Error(response, "location is set by HOUSTON_LAT/HOUSTON_LON; change it there", http.StatusConflict)
			return
		}

		var observer Observer
		decoder := json.NewDecoder(io.LimitReader(request.Body, maxLocationBodyBytes))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&observer); err != nil {
			http.Error(response, "bad location: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Two decimals is ~1 km: far finer than the sky plot can show, and
		// coarse enough that the file doesn't pinpoint a house.
		observer.LatitudeDeg = roundTo(observer.LatitudeDeg, 2)
		observer.LongitudeDeg = roundTo(observer.LongitudeDeg, 2)
		if err := observer.Validate(); err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}

		settings, err := LoadSettings(settingsPath)
		if err != nil {
			log.Printf("sky: replacing unreadable %s: %v", settingsPath, err)
			settings = Settings{}
		}
		settings.Location = &observer
		if err := SaveSettings(settingsPath, settings); err != nil {
			log.Printf("sky: could not save location: %v", err)
			http.Error(response, "could not save location", http.StatusInternalServerError)
			return
		}
		log.Printf("sky: location set to %.2f, %.2f from the dashboard", observer.LatitudeDeg, observer.LongitudeDeg)
		tracker.SetObserver(observer, LocationFromSettings)
		respond(response)
	}
}
