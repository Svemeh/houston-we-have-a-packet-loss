package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"
)

func main() {
	dishAddress := flag.String("addr", DefaultDishAddress, "Starlink dish gRPC address")
	webListenAddress := flag.String("web", DefaultWebAddress, "dashboard listen address")
	logPath := flag.String("log", DefaultLogFilePath, "telemetry log file (JSONL)")
	oneShot := flag.Bool("oneshot", false, "run a one-shot connectivity check and exit")
	useFake := flag.Bool("fake", false, "generate fake data instead of polling from the antenna")
	requestTimeout := flag.Duration("timeout", DefaultRequestTimeout, "per-request timeout")
	flag.Parse()

	collector, err := newCollector(*useFake, *dishAddress)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
		os.Exit(1)
	}

	defer collector.Close()

	// One-shot connectivity test — bypasses hub/logfile machinery.
	if *oneShot {
		if err := runConnectivityCheck(collector, *requestTimeout); err != nil {
			fmt.Fprintf(os.Stderr, "✗ could not reach Starlink dish at %s: %v\n", *dishAddress, err)
			os.Exit(1)
		}
		return
	}

	logFile, err := OpenLogFile(*logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ opening log file %q: %v\n", *logPath, err)
		os.Exit(1)
	}
	defer logFile.Close()

	hubCapacity := int(BackfillWindow / DefaultPollInterval)
	hub := NewTelemetryHub(hubCapacity)

	observer, err := LoadObserver()
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
		os.Exit(1)
	}
	tracker := NewSkyTracker(observer)

	priorSamples, err := LoadSamplesSince(*logPath, time.Now().Add(-BackfillWindow))
	if err != nil {
		log.Printf("warning: could not load prior telemetry: %v", err)
	} else if len(priorSamples) > 0 {
		hub.SeedHistory(priorSamples)
		log.Printf("loaded %d prior samples from %s", len(priorSamples), *logPath)
	}

	ctx, stopSignalWatch := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopSignalWatch()

	go pollLoop(ctx, collector, hub, logFile, tracker)
	go pruneLoop(ctx, logFile)
	go tracker.Run(ctx)

	if err := serveDashboard(ctx, *webListenAddress, hub, tracker, *logPath); err != nil {
		fmt.Fprintf(os.Stderr, "✗ server error: %v\n", err)
		os.Exit(1)
	}
}

// pollLoop polls the dish on a fixed interval until ctx is cancelled, handing
// each sample to the hub and the log file, and each boresight reading to the
// sky tracker so the service cone follows where the dish is actually aimed.
func pollLoop(ctx context.Context, collector TelemetryCollector, hub *TelemetryHub, logFile *LogFile, tracker *SkyTracker) {
	ticker := time.NewTicker(DefaultPollInterval)
	defer ticker.Stop()

	loggedBoresight := false

	pollOnce := func() {
		pollCtx, cancelPoll := context.WithTimeout(ctx, DefaultRequestTimeout)
		defer cancelPoll()
		sample, err := collector.Collect(pollCtx)
		if err != nil {
			sample = TelemetrySample{
				Timestamp: time.Now(),
				LinkState: LinkStateOffline,
				PollError: err.Error(),
			}
		}
		if sample.BoresightValid {
			tracker.SetBoresight(sample.BoresightAzimuthDeg, sample.BoresightElevationDeg)
			if !loggedBoresight {
				log.Printf("sky: dish reports boresight az %.2f° el %.2f° — cone follows it",
					sample.BoresightAzimuthDeg, sample.BoresightElevationDeg)
				loggedBoresight = true
			}
		}
		hub.PublishSample(sample)
		if err := logFile.Append(sample); err != nil {
			log.Printf("log write failed: %v", err)
		}
	}

	pollOnce() // one immediate poll so /events isn't blank on first connect
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pollOnce()
		}
	}
}

// pruneLoop prunes the log file to defined size in consts.go "LogRetention"
// time between each prune is defined in consts.go "LogPruneInterval"
// This will also run once at startup.
func pruneLoop(ctx context.Context, logFile *LogFile) {
	ticker := time.NewTicker(LogPruneInterval)
	defer ticker.Stop()

	prune := func() {
		cutoff := time.Now().Add(-LogRetention)
		if err := logFile.Prune(cutoff); err != nil {
			log.Printf("log prune failed: %v", err)
		}
	}
	prune()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}

// runConnectivityCheck performs a single poll and prints a human-readable summary.
func runConnectivityCheck(collector TelemetryCollector, timeout time.Duration) error {
	ctx, cancelTimeout := context.WithTimeout(context.Background(), timeout)
	defer cancelTimeout()

	sample, err := collector.Collect(ctx)
	if err != nil {
		return err
	}

	fmt.Println("✓ Connected to Starlink dish (received live telemetry)")
	fmt.Printf("  link:      %s\n", sample.LinkState)
	fmt.Printf("  latency:   %.1f ms\n", sample.LatencyMs)
	fmt.Printf("  drop rate: %.1f%%\n", sample.DropRateFraction*100)
	fmt.Printf("  obstruction: %.2f%% of sky\n", sample.ObstructionFraction*100)
	fmt.Printf("  hardware:  %s\n", sample.HardwareVersion)
	fmt.Printf("  software:  %s\n", sample.SoftwareVersion)
	if sample.BoresightValid {
		fmt.Printf("  boresight: az %.2f° el %.2f°\n", sample.BoresightAzimuthDeg, sample.BoresightElevationDeg)
	} else {
		fmt.Println("  boresight: not reported by this firmware")
	}
	fmt.Printf("  uptime:    %s\n", time.Duration(sample.UptimeSeconds)*time.Second)
	return nil
}
