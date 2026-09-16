# Houston, We Have a Packet Loss

### Backend - Golang

- **[main.go](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/main.go)** — Entry point and main loop
- **[const.go](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/const.go)** — All tunables in one place (addresses, intervals, retention, thresholds, routes) plus the `TelemetrySample` struct that defines both the on-disk and wire format.
- **[antenna.go](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/antenna.go)** — The `TelemetryCollector` interface and the real gRPC client for the dish. Converts a `GetStatus` response into a `TelemetrySample` and holds the link-state heuristic. In short: gathers all the data from the antenna
- **[logFile.go](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/logFile.go)** — Persistence. Appends samples to the JSONL log, prunes old ones via temp-file + atomic rename, reads back history with a tail-seek estimate, and downsamples long ranges worst-case-first.
- **[server.go](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/server.go)** — HTTP layer. Serves the embedded static files, the SSE stream at `/events`, the raw log at `/log` and downsampled history at `/history`. Also contains `TelemetryHub`, which fans samples out to connected browsers and keeps a rolling backfill window.
- **[sky.go](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/sky.go)** — Satellite tracking. Loads the observer position from the environment/`.env`, fetches and caches Starlink TLEs from Celestrak, prefilters elements by inclination, propagates them with SGP4 on a tick, and fans snapshots of everything above the horizon out to connected browsers.

#### Testing 

- **[fakeAntenna.go](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/fakeAntenna.go)** — Stand-in collector for `-fake`. Generates synthetic telemetry that cycles through degraded, obstructed, no-signal and unreachable stretches so every UI state gets exercised without a dish.

### Frontend - HTML CSS JavaScript 

- **[static/index.html](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/static/index.html)** — Dashboard markup: alarm strip, the four big readouts, range selector, strip charts and status strip.
- **[static/styles.css](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/static/styles.css)** — Theming and layout: colour variables, panel styling, alarm states and responsive breakpoints.
- **[static/dashboard.js](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/static/dashboard.js)** — Client logic. Subscribes to the SSE stream, fetches history when the range changes, updates the readouts and alarm state, and draws the canvas charts with hover inspection.

### Environment example

- **[.env.example](https://github.com/Svemeh/houston-we-have-a-packet-loss/blob/main/.env.example)** — Template for local configuration; copy to `.env` and fill in. `.env` itself is not committed.
