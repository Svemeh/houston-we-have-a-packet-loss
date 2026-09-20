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
	pingTarget := flag.String("ping-target", DefaultPingTarget, "host this machine pings")
	pingLogPath := flag.String("ping-log", DefaultPingLogPath, "ping log file (JSONL)")
	noPing := flag.Bool("no-ping", false, "don't ping from this machine")
	hopsLogPath := flag.String("hops-log", DefaultHopsLogPath, "per-hop ping log file (JSONL)")
	noHops := flag.Bool("no-hops", false, "don't ping the hops along the path")
	outageLogPath := flag.String("outage-log", DefaultOutageLogPath, "dish outage log file (JSONL)")
	flag.Parse()

	collector, err := newCollector(*useFake, *dishAddress)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
		os.Exit(1)
	}

	defer collector.Close()

	// One-shot connectivity test — bypasses hub/logfile machinery.
	// The ping runs even when the dish is unreachable
	if *oneShot {
		checkFailed := false
		if err := runConnectivityCheck(collector, *requestTimeout); err != nil {
			fmt.Fprintf(os.Stderr, "✗ could not reach Starlink dish at %s: %v\n", *dishAddress, err)
			checkFailed = true
		}
		if !*noPing && !runPingCheck(*pingTarget) {
			checkFailed = true
		}
		if checkFailed {
			os.Exit(1)
		}
		return
	}

	logFile, err := OpenLogFile[TelemetrySample](*logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ opening log file %q: %v\n", *logPath, err)
		os.Exit(1)
	}
	defer logFile.Close()

	pingLogFile, err := OpenLogFile[PingSample](*pingLogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ opening ping log file %q: %v\n", *pingLogPath, err)
		os.Exit(1)
	}
	defer pingLogFile.Close()

	hopsLogFile, err := OpenLogFile[HopSample](*hopsLogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ opening hops log file %q: %v\n", *hopsLogPath, err)
		os.Exit(1)
	}
	defer hopsLogFile.Close()

	outageLogFile, err := OpenLogFile[DishOutage](*outageLogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ opening outage log file %q: %v\n", *outageLogPath, err)
		os.Exit(1)
	}
	defer outageLogFile.Close()

	hubCapacity := int(BackfillWindow / DefaultPollInterval)
	hub := NewHub[TelemetrySample](hubCapacity)

	observer, err := LoadObserver()
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
		os.Exit(1)
	}
	tracker := NewSkyTracker(observer)

	priorSamples, err := LoadSamplesSince[TelemetrySample](*logPath, time.Now().Add(-BackfillWindow))
	if err != nil {
		log.Printf("warning: could not load prior telemetry: %v", err)
	} else if len(priorSamples) > 0 {
		hub.SeedHistory(priorSamples)
		log.Printf("loaded %d prior samples from %s", len(priorSamples), *logPath)
	}

	// The ping hub and routes exist even with -no-ping, so the dashboard just
	// shows old or empty data instead of erroring.
	pingHub := NewHub[PingSample](int(BackfillWindow / PingInterval))
	priorPings, err := LoadSamplesSince[PingSample](*pingLogPath, time.Now().Add(-BackfillWindow))
	if err != nil {
		log.Printf("warning: could not load prior pings: %v", err)
	} else if len(priorPings) > 0 {
		pingHub.SeedHistory(priorPings)
		log.Printf("loaded %d prior pings from %s", len(priorPings), *pingLogPath)
	}

	ctx, stopSignalWatch := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopSignalWatch()

	go pollLoop(ctx, collector, hub, logFile, tracker)
	go pruneLoop(ctx, logFile, pingLogFile)
	go pruneLoop(ctx, logFile, pingLogFile, hopsLogFile, outageLogFile)
	go tracker.Run(ctx)
	if !*noPing {
		go NewPinger(*pingTarget, pingHub, pingLogFile).Run(ctx)
	}
	if !*noHops {
		go NewHopPinger(HopTargets, hopsLogFile).Run(ctx)
	}
	if outageSource, ok := collector.(OutageSource); ok {
		go outageLoop(ctx, outageSource, outageLogFile, *outageLogPath)
	} else {
		log.Print("dish outages: not available from this collector, not polling")
	}

	if err := serveDashboard(ctx, *webListenAddress, hub, pingHub, tracker, *logPath, *pingLogPath); err != nil {
		fmt.Fprintf(os.Stderr, "✗ server error: %v\n", err)
		os.Exit(1)
	}
}

// pollLoop polls the dish on a fixed interval until ctx is cancelled, handing
// each sample to the hub and the log file, and each boresight reading to the
// sky tracker so the service cone follows where the dish is actually aimed.
func pollLoop(ctx context.Context, collector TelemetryCollector, hub *Hub[TelemetrySample], logFile *LogFile[TelemetrySample], tracker *SkyTracker) {
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

// prunable is any log pruneLoop can trim.
type prunable interface {
	Prune(cutoff time.Time) error
}

// pruneLoop prunes each log file to defined size in consts.go "LogRetention"
// time between each prune is defined in consts.go "LogPruneInterval"
// This will also run once at startup.
func pruneLoop(ctx context.Context, logFiles ...prunable) {
	ticker := time.NewTicker(LogPruneInterval)
	defer ticker.Stop()

	prune := func() {
		cutoff := time.Now().Add(-LogRetention)
		for _, logFile := range logFiles {
			if err := logFile.Prune(cutoff); err != nil {
				log.Printf("log prune failed: %v", err)
			}
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

// runPingCheck sends a single echo from this machine and prints the result. It reports whether a reply came back.
func runPingCheck(target string) bool {
	sample := NewPinger(target, nil, nil).PingOnce(context.Background())
	switch {
	case sample.Error != "":
		fmt.Fprintf(os.Stderr, "✗ could not ping %s from this machine: %s\n", target, sample.Error)
		return false
	case sample.Lost:
		fmt.Fprintf(os.Stderr, "✗ no reply from %s within %s\n", target, PingTimeout)
		return false
	}
	fmt.Printf("✓ Reached %s from this machine\n", target)
	fmt.Printf("  ping:      %.1f ms\n", sample.RTTMs)
	return true
}
