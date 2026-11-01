package main

import "time"

const (
	AppName = "Houston, We Have a Packet Loss"

	DefaultDishAddress    = "192.168.100.1:9200"
	DefaultRequestTimeout = 5 * time.Second

	// Polling: how often we ask the dish for a fresh status snapshot.
	DefaultPollInterval = 1 * time.Second

	// History: how much recent data the web UI keeps so a freshly opened browser
	// tab can backfill its chart instead of starting empty.
	BackfillWindow = 45 * time.Minute

	// Web UI: the dashboard
	DefaultWebAddress = ":2101"
	RouteIndex        = "/"
	RouteEvents       = "/events"
	RouteLog          = "/log"
	RouteHistory      = "/history"

	RangeAll         = "all"
	MaxHistoryRange  = 24 * time.Hour
	MaxHistoryPoints = 2000

	DataDir = "data"

	// Persistence: relative path to raw telemetry log
	// single JSON-Lines file (one sample per line).
	// ~250 bytes per sample, 3600 samples per hour = 900KB/hour
	// Kept as one file so it's grep-, tail-, and cat-friendly.
	DefaultLogFilePath = DataDir + "/dishToPop.jsonl"

	// LogRetention: samples older than this are pruned from the log.
	// The log files grows at roughly ~900KB/hour.
	// so a rough estimate of max file size is: 900KB * LogRetention + LogPruneInterval
	LogRetention = 72 * time.Hour

	// LogPruneInterval: how often the pruner runs.
	LogPruneInterval = 1 * time.Hour

	// Link-health heuristic: thresholds for degraded/lost signal.
	// Used to approximate connection health from packet-drop rate
	//
	// Values are decimals representing percentage loss
	// 1 = 100%, 0.25 = 25%
	DropRateNoSignalThreshold = 1.0  // fully dropping -> no usable signal
	DropRateDegradedThreshold = 0.02 // more than 2% drop -> degraded but still up

	// Simplified link states produced by the heuristic above.
	LinkStateOnline     = "ONLINE"
	LinkStateDegraded   = "DEGRADED"
	LinkStateObstructed = "OBSTRUCTED"
	LinkStateNoSignal   = "NO SIGNAL"
	LinkStateOffline    = "OFFLINE"

	// Sky: satellite tracking.
	CelestrakStarlinkURL    = "https://celestrak.org/NORAD/elements/gp.php?GROUP=starlink&FORMAT=json"
	TLERefreshInterval      = 8 * time.Hour
	TLERefreshRetryInterval = 5 * time.Minute
	TLEFetchTimeout         = 30 * time.Second
	TLEStaleAfter           = 3 * 24 * time.Hour
	SkyTickInterval         = 1 * time.Second
	skySubscriberQueueDepth = 4

	// Geometry at the Starlink operational altitude (~550 km).
	HorizonAngularRadiusDeg  = 23.0 // arccos(Re/(Re+h)) — how far a sub-satellite point can be and still be visible
	ServiceFloorElevationDeg = 10.0 // below this the dish won't use it
	DefaultConeHalfAngleDeg  = 55.0 // Standard/Mini ~110° full FOV; Flat HP ~140°

	// Ping: ICMP echo from this machine, independent of the dish.
	// 1.1.1.1 is anycast, so it lands on the nearest Cloudflare edge.
	// ~90 bytes per sample, 3600 per hour = ~320KB/hour, same retention as the dish log.
	DefaultPingTarget  = "1.1.1.1"
	DefaultPingLogPath = DataDir + "/machineToInternet.jsonl"
	PingInterval       = 1 * time.Second
	PingTimeout        = 1 * time.Second // no reply within this counts as lost

	// HopPingTimeout is shorter than PingInterval so a lost echo can't stretch a hop round past the next tick
	HopPingTimeout = 800 * time.Millisecond

	RoutePingEvents  = "/pingevents"
	RoutePingLog     = "/pinglog"
	RoutePingHistory = "/pinghistory"

	RouteSkyEvents = "/skyevents"
	RouteLocation  = "/location"

	// Settings made from the dashboard, such as the location.
	DefaultSettingsPath = DataDir + "/settings.json"

	ElementCachePath   = DataDir + "/starlink.json"
	CelestrakUserAgent = "houston-packet-loss/0.1"

	RouteHopsEvents  = "/hopsevents"
	RouteHopsLog     = "/hopslog"
	RouteHopsHistory = "/hopshistory"
	RouteRollup      = "/rollup"

	MaxRollupRange  = 365 * 24 * time.Hour
	MaxRollupPoints = 2000

	RouteIncidents = "/incidents"

	IncidentBurstGapSeconds = 2   // answered seconds tolerated inside one burst
	IncidentSpikeFactor     = 3.0 // multiple of the window's own median RTT
	IncidentSpikeFloorMs    = 100 // below this, a spike isn't worth reporting
	MaxIncidents            = 500

	DefaultHopsLogPath = DataDir + "/hops.jsonl"

	// Hourly rollup: one line per hour summarising hops.jsonl
	DefaultRollupLogPath = DataDir + "/hopsHourly.jsonl"
	RollupInterval       = 10 * time.Minute

	// HandoverSlotSeconds: Starlink switches satellites on a 15s boundary. The rollup
	// buckets loss by position in the slot to separate handover loss from the rest.
	HandoverSlotSeconds = 15

	// Dish outages: the dish's own outage list from get_history.
	DefaultOutageLogPath = DataDir + "/dishOutages.jsonl"
	OutagePollInterval   = 60 * time.Second

	// Handover burst around every handover: unix_seconds % 15 == 12 (:12/:27/:42/:57 UTC).
	HandoverBoundaryOffset   = 12
	HandoverBurstTarget      = HopEndTarget
	HandoverBurstStep        = 20 * time.Millisecond // a handover loses only 1–2 echoes at ~12 ms spacing
	HandoverBurstBefore      = 600 * time.Millisecond
	HandoverBurstAfter       = 900 * time.Millisecond
	HandoverBurstMaxLateWake = 50 * time.Millisecond
	DefaultHandoverLogPath   = DataDir + "/handovers.jsonl"

	// Serving satellite (-track-satellite): clear the obstruction map each slot, match the traced arc to a track.
	ServingClearDelay               = 300 * time.Millisecond // after the slot boundary
	ServingReadInterval             = 1 * time.Second
	ServingReadsPerSlot             = 14
	ServingMinPixels                = 5
	ServingMaxFitDeg                = 2.5 // median arc-to-track angle, about 2 map pixels
	ServingMinMarginDeg             = 2.0 // the runner-up must fit at least this much worse
	ServingMaxTimeOffsetS           = 4
	ServingCandidateMinElevationDeg = 5.0
	DefaultServingLogPath           = DataDir + "/serving.jsonl"

	// SchedulerResyncAfter: a wake this far off plan restarts the 1/s schedule.
	SchedulerResyncAfter = 3 * time.Second
	SkipReportInterval   = 1 * time.Hour
)

// DishHopTarget is the dish's LAN address, the same on every Starlink setup.
const DishHopTarget = "192.168.100.1"

// hopTargets lists what the hop pinger pings every PingInterval. router is the
// LAN router found at startup, or "" when there isn't one we can trust; it is
// left out rather than guessed.
func hopTargets(router string) []string {
	targets := []string{
		DishHopTarget,
		"1.1.1.1", // Cloudflare anycast, same target as machineToInternet.jsonl
		"8.8.8.8", // Google, independent control path
	}
	if router != "" {
		targets = append([]string{router}, targets...)
	}
	return targets
}

// HopLocalTargets are the hops on this side of the dish. main adds the router
// once it has been found.
var HopLocalTargets = []string{DishHopTarget}

const HopControlTarget = "8.8.8.8"

const HopEndTarget = DefaultPingTarget

// TelemetrySample is one poll of the dish: the values it reported, or the
// error that stopped us from reading them.
type TelemetrySample struct {
	Timestamp             time.Time `json:"timestamp"`
	LinkState             string    `json:"link_state"`
	LatencyMs             float64   `json:"latency_ms"`
	DownloadMbps          float64   `json:"download_mbps"`
	UploadMbps            float64   `json:"upload_mbps"`
	DropRateFraction      float64   `json:"drop_rate_fraction"` // fraction in [0,1]
	Obstructed            bool      `json:"obstructed"`
	ObstructionFraction   float64   `json:"obstruction_fraction"`
	BoresightAzimuthDeg   float64   `json:"boresight_az_deg"`
	BoresightElevationDeg float64   `json:"boresight_el_deg"`
	BoresightValid        bool      `json:"boresight_valid"`
	UptimeSeconds         uint64    `json:"uptime_seconds"`
	HardwareVersion       string    `json:"hardware_version"`
	SoftwareVersion       string    `json:"software_version"`
	// PollError is set when a poll failed, and empty otherwise.
	PollError string `json:"poll_error,omitempty"`
}

func (sample TelemetrySample) SampleTime() time.Time { return sample.Timestamp }
