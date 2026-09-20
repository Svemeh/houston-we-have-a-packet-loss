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
	CelestrakStarlinkURL    = "https://celestrak.org/NORAD/elements/gp.php?GROUP=starlink&FORMAT=tle"
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

	RoutePingEvents  = "/pingevents"
	RoutePingLog     = "/pinglog"
	RoutePingHistory = "/pinghistory"

	RouteSky       = "/sky"
	RouteSkyEvents = "/skyevents"

	TLECachePath       = DataDir + "/starlink.tle"
	CelestrakUserAgent = "houston-packet-loss/0.1"

	// Hops: one echo per second to every point along the path (HopTargets), all
	// in parallel, so a loss burst can be pinned to the first hop that went quiet.
	// ~350 bytes per line, 3600 per hour = ~1.2MB/hour, same retention as the dish log.
	DefaultHopsLogPath = DataDir + "/hops.jsonl"

	// Dish outages: the dish's own outage list from get_history, deduplicated.
	// The dish keeps roughly the last 15 minutes, so polling well inside that loses nothing.
	DefaultOutageLogPath = DataDir + "/dishOutages.jsonl"
	OutagePollInterval   = 60 * time.Second
)

// HopTargets are pinged every PingInterval by the hop pinger, in path order
// from this machine outwards. 8.8.8.8 sits off the Cloudflare path on purpose:
// if it drops together with 1.1.1.1 the problem is upstream of both.
var HopTargets = []string{
	"192.168.2.1",   // LAN router
	"192.168.100.1", // dish
	"100.64.0.1",    // Starlink PoP gateway, first hop across the satellite link
	"206.224.70.83", // Starlink backbone
	"162.158.84.78", // Cloudflare edge
	"1.1.1.1",       // Cloudflare anycast, same target as machineToInternet.jsonl
	"8.8.8.8",       // Google, independent control path
}

// TelemetrySample is one poll of the dish: the values it reported, or the
// error that stopped us from reading them.
//
// The JSON tags are the on-disk log format (dishToPop.jsonl) and
// the wire format the dashboard reads.
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
