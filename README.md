# Houston, We Have a Packet Loss

### Backend - Golang

- **[main.go](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/main.go)** — Entry point and main loop
- **[const.go](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/const.go)** — All tunables in one place (addresses, intervals, retention, thresholds, routes) plus the `TelemetrySample` struct that defines both the on-disk and wire format.
- **[antenna.go](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/antenna.go)** — The `TelemetryCollector` interface and the real gRPC client for the dish. Converts a `GetStatus` response into a `TelemetrySample` and holds the link-state heuristic. In short: gathers all the data from the antenna
- **[logFile.go](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/logFile.go)** — Persistence. Appends samples to the JSONL log, prunes old ones via temp-file + atomic rename, reads back history with a tail-seek estimate, and downsamples long ranges worst-case-first.
- **[server.go](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/server.go)** — HTTP layer. Serves the embedded static files, the SSE stream at `/events`, the raw log at `/log` and downsampled history at `/history`.
- **[hub.go](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/hub.go)** — Generic `Hub[T]` that fans samples out to connected browsers and keeps a rolling backfill window.
- **[ping.go](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/ping.go)** — Pings `1.1.1.1` from this machine once a second, independent of the dish. Logs to `machineToInternet.jsonl`, serves `/pingevents`, `/pinglog` and `/pinghistory` (buckets of max RTT plus sent/lost counts).
- **[sky.go](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/sky.go)** — Satellite tracking. Loads the observer position from the environment/`.env`, fetches and caches Starlink TLEs from Celestrak, prefilters elements by inclination, propagates them with SGP4 on a tick, and fans snapshots of everything above the horizon out to connected browsers.
- **[hops.go](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/hops.go)** — Pings every hop in `HopTargets` (router, dish, PoP gateway, backbone, Cloudflare, `1.1.1.1`, `8.8.8.8`) in parallel once a second. Logs one line per round to `hops.jsonl`. Disable with `-no-hops`.
- **[outages.go](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/outages.go)** — Polls the dish's `get_history` every 60s, reads its outage list (start, duration, cause) and appends new ones to `dishOutages.jsonl`, deduplicated by start time.

#### Testing 

- **[fakeAntenna.go](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/fakeAntenna.go)** — Stand-in collector for `-fake`. Generates synthetic telemetry that cycles through degraded, obstructed, no-signal and unreachable stretches so every UI state gets exercised without a dish.

### Analysis

- **[analyze_hops.py](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/analyze_hops.py)** — Reads `hops.jsonl`, pins each loss burst to the hop where traffic died (LAN / dish / satellite link / Starlink backbone / Cloudflare), totals loss per hop and matches bursts against `dishOutages.jsonl`. Run from the project root: `python3 analyze_hops.py --since 24h`.

### Frontend - HTML CSS JavaScript 

- **[static/index.html](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/static/index.html)** — Dashboard markup: alarm strip, the four big readouts, range selector, strip charts and status strip.
- **[static/styles.css](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/static/styles.css)** — Theming and layout: colour variables, panel styling, alarm states and responsive breakpoints.
- **[static/dashboard.js](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/static/dashboard.js)** — Client logic. Subscribes to the SSE stream, fetches history when the range changes, updates the readouts and alarm state, and draws the canvas charts with hover inspection.

### Environment example

- **[.env.example](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/.env.example)** — Template for local configuration; copy to `.env` and fill in. `.env` itself is not committed.
