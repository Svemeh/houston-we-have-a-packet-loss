(() => {
  "use strict";

  // How each link state is presented in the alarm strip.
  const LINK_STATE_DISPLAY = {
    ONLINE: { className: "state-nominal", message: "ALL SYSTEMS NOMINAL", subtext: "" },
    DEGRADED: { className: "state-caution", message: "PACKET LOSS DETECTED", subtext: "caution" },
    OBSTRUCTED: { className: "state-alarm", message: "LINE OF SIGHT OBSTRUCTED", subtext: "off-nominal" },
    "NO SIGNAL": { className: "state-alarm", message: "SIGNAL LOST — REACQUIRING", subtext: "off-nominal" },
    OFFLINE: { className: "state-alarm", message: "TELEMETRY LINK DOWN", subtext: "no contact" },
  };

  const byId = elementId => document.getElementById(elementId);
  const alarmStrip = byId("alarm-strip");
  const alarmMessage = byId("alarm-message");
  const alarmSubtext = byId("alarm-subtext");
  const connectionIndicator = byId("connection-status");
  const connectionText = byId("connection-text");

  const traceFillOpacity = 0.03; // Opacity of fill under line in the live charts

  const formatNumber = (value, decimals = 1) =>
    value == null || isNaN(value) ? "—" : Number(value).toFixed(decimals);

  // Renders dish uptime as the mission-clock readout, DDD:HH:MM:SS.
  function formatUptimeClock(totalSeconds) {
    let remaining = Math.max(0, Math.floor(totalSeconds || 0));
    const days = Math.floor(remaining / 86400);
    remaining -= days * 86400;
    const hours = Math.floor(remaining / 3600);
    remaining -= hours * 3600;
    const minutes = Math.floor(remaining / 60);
    const seconds = remaining - minutes * 60;
    const pad = number => String(number).padStart(2, "0");
    return `${String(days).padStart(3, "0")}:${pad(hours)}:${pad(minutes)}:${pad(seconds)}`;
  }

  function setLinkState(linkState) {
    const display = LINK_STATE_DISPLAY[linkState] || LINK_STATE_DISPLAY["OFFLINE"];
    alarmStrip.className = "alarm-strip " + display.className;
    alarmMessage.textContent = display.message;
    alarmSubtext.textContent = display.subtext;
  }

  // ---- Time ranges -------------------------------------------------------
  // `spanMs: null` means everything in the log. For that case the window start
  // is pinned to the oldest sample the server returns and the span grows as
  // time passes, rather than sliding.
  //
  // The id doubles as the query value, so it has to parse as a Go duration
  // (or be the literal "all").
  const RANGES = [
    { id: "2m", label: "2m", spanMs: 2 * 60 * 1000 },
    { id: "5m", label: "5m", spanMs: 5 * 60 * 1000 },
    { id: "15m", label: "15m", spanMs: 15 * 60 * 1000 },
    { id: "1h", label: "1h", spanMs: 60 * 60 * 1000 },
    { id: "6h", label: "6h", spanMs: 6 * 60 * 60 * 1000 },
    { id: "24h", label: "24h", spanMs: 24 * 60 * 60 * 1000 },
    { id: "all", label: "All", spanMs: null },
  ];
  const DEFAULT_RANGE_ID = "15m";

  // Adjacent samples further apart than the gap threshold break the trace.
  const MIN_GAP_THRESHOLD_MS = 3000;
  const GAP_THRESHOLD_MULTIPLIER = 2.5; // slack over the server's bucket width
  const ASSUMED_BUCKET_MS = 1000; // used when the server doesn't report one
  const EMPTY_ALL_RANGE_SPAN_MS = 60 * 1000; // "All" with nothing logged yet

  const viewWindow = {
    selectedRange: RANGES.find(range => range.id === DEFAULT_RANGE_ID),
    pinnedStartMs: null, // only set for the "all" range
  };

  function viewWindowStartMs(nowMs) {
    return viewWindow.pinnedStartMs !== null ? viewWindow.pinnedStartMs : nowMs - viewWindow.selectedRange.spanMs;
  }

  // Hard ceiling on retained points. Pruning is normally by time (see
  // dropPointsBefore), this only catches the "all" range left running for hours.
  const MAX_POINTS_PER_SCOPE = 20000;

  const cssVarCache = {};
  function getCssVar(variableName) {
    if (!cssVarCache[variableName]) {
      cssVarCache[variableName] = getComputedStyle(document.documentElement).getPropertyValue(variableName);
    }
    return cssVarCache[variableName];
  }

  // Canvas needs rgba() for the translucent crosshair, and the trace colours
  // arrive as hex from CSS.
  function withAlpha(hexColor, alpha) {
    let hex = hexColor.trim().replace("#", "");
    if (hex.length === 3) {
      hex = hex.split("").map(digit => digit + digit).join("");
    }
    const rgbInt = parseInt(hex, 16);
    return `rgba(${(rgbInt >> 16) & 255},${(rgbInt >> 8) & 255},${rgbInt & 255},${alpha})`;
  }

  // Pick a gridline step that respects the scope's ideal step (minimum) but
  // scales up through nice human-readable values so we never crowd the axis
  // with more than ~6 divisions.
  const GRID_STEP_CASCADE = [1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000];
  const MAX_GRID_DIVISIONS = 6;
  function pickGridStep(axisTop, minimumStep) {
    for (const candidateStep of GRID_STEP_CASCADE) {
      if (candidateStep >= minimumStep && axisTop / candidateStep <= MAX_GRID_DIVISIONS) {
        return candidateStep;
      }
    }
    return GRID_STEP_CASCADE[GRID_STEP_CASCADE.length - 1];
  }

  // ---- Scope (one strip chart) -------------------------------------------
  const PLOT_BOTTOM_PADDING_PX = 14; // room under the plot for axis labels
  const AXIS_HEADROOM = 1.25; // top of scale sits 25% above the peak
  const HOVER_SNAP_PX = 8; // how close the cursor must be to snap to a point
  const LEADING_DOT_RADIUS_PX = 2.6;
  const HOVER_DOT_RADIUS_PX = 3.2;
  const HOVER_RING_RADIUS_PX = 5.5;

  const amber = getCssVar("--amber").trim();
  const CROSSHAIR_LINE_COLOR = withAlpha(amber, 0.35);
  const CROSSHAIR_RING_COLOR = withAlpha(amber, 0.45);

  function createScope(config) {
    const canvas = byId(config.canvasId);
    const canvasCtx = canvas.getContext("2d");
    const rangeLabelEl = byId(config.rangeLabelId);
    // Each point is {time: Date, value: number}. Chronological order is
    // preserved by construction (the hub is single-writer server-side, history
    // arrives sorted, live samples arrive newest-last).
    let points = [];
    let hoveredIndex = -1;
    let widthPx = 0;
    let heightPx = 0;
    // Each scope tracks its own threshold: dish and ping history arrive from
    // separate requests and may be bucketed differently.
    let gapThresholdMs = MIN_GAP_THRESHOLD_MS;

    const formatValue = config.formatValue || (value => `${Math.round(value)} ${config.unit}`);

    // The badge lives inside the scope's own container so it's clipped correctly.
    const hoverBadge = document.createElement("div");
    hoverBadge.className = "hover-badge";
    canvas.parentElement.appendChild(hoverBadge);
    hoverBadge.style.setProperty("--badge-fg", getCssVar(config.colorVar).trim());

    function resizeCanvas() {
      const pixelRatio = window.devicePixelRatio || 1;
      const rect = canvas.getBoundingClientRect();
      widthPx = rect.width;
      heightPx = rect.height;
      canvas.width = Math.round(widthPx * pixelRatio);
      canvas.height = Math.round(heightPx * pixelRatio);
      canvasCtx.setTransform(pixelRatio, 0, 0, pixelRatio, 0, 0);
      draw();
    }

    // Drop points that have scrolled off the left edge. Slicing once beats
    // shift()-ing in a loop, and pruning by time rather than by count means
    // a long range keeps its full history no matter how many live samples
    // arrive on top of it.
    function dropPointsBefore(windowStartMs) {
      if (points.length > 1 && points[0].time < windowStartMs) {
        let firstVisibleIndex = 0;
        while (firstVisibleIndex < points.length && points[firstVisibleIndex].time < windowStartMs) {
          firstVisibleIndex++;
        }
        // Keep one point off-screen so the trace enters from the edge.
        if (firstVisibleIndex > 0) firstVisibleIndex--;
        if (firstVisibleIndex > 0) {
          points = points.slice(firstVisibleIndex);
          hoveredIndex -= firstVisibleIndex;
        }
      }
      if (points.length > MAX_POINTS_PER_SCOPE) {
        const excess = points.length - MAX_POINTS_PER_SCOPE;
        points = points.slice(excess);
        hoveredIndex -= excess;
      }
      if (hoveredIndex < 0) hoveredIndex = -1;
    }

    function draw() {
      canvasCtx.clearRect(0, 0, widthPx, heightPx);
      if (!widthPx) return;

      const nowMs = Date.now();
      const windowStartMs = viewWindowStartMs(nowMs);
      const windowSpanMs = Math.max(1, nowMs - windowStartMs);
      const xForTime = timeMs => (widthPx * (timeMs - windowStartMs)) / windowSpanMs;

      dropPointsBefore(windowStartMs);

      // Peak over visible samples only — a spike from the far end of the
      // window shouldn't dictate the scale after it scrolls off.
      let peakValue = 0;
      let visiblePointCount = 0;
      for (const point of points) {
        if (point.time < windowStartMs) continue;
        visiblePointCount++;
        if (point.value > peakValue) peakValue = point.value;
      }
      const axisTop = Math.min(
        config.maxAxisTop || Infinity,
        Math.max(
          config.minAxisTop,
          Math.ceil((Math.max(peakValue, 0) * AXIS_HEADROOM) / config.axisRounding) * config.axisRounding,
        ),
      );
      rangeLabelEl.textContent = visiblePointCount
        ? `0–${axisTop} ${config.unit} · ${viewWindow.selectedRange.label}`
        : `${viewWindow.selectedRange.label} span`;
      const plotHeight = heightPx - PLOT_BOTTOM_PADDING_PX;
      const traceColor = getCssVar(config.colorVar).trim();
      const yForValue = value => plotHeight * (1 - Math.min(value, axisTop) / axisTop);

      // Grid
      canvasCtx.strokeStyle = getCssVar("--grid");
      canvasCtx.lineWidth = 1;
      canvasCtx.font = "10px " + getCssVar("--mono").trim();
      canvasCtx.fillStyle = getCssVar("--label-dim");
      const gridStep = config.minGridStep ? pickGridStep(axisTop, config.minGridStep) : axisTop / 4;
      const gridDivisions = Math.max(1, Math.round(axisTop / gridStep));
      for (let division = 0; division <= gridDivisions; division++) {
        const gridLineY = Math.round((plotHeight * division) / gridDivisions) + 0.5;
        canvasCtx.beginPath();
        canvasCtx.moveTo(0, gridLineY);
        canvasCtx.lineTo(widthPx, gridLineY);
        canvasCtx.stroke();
        const gridValue = Math.round(axisTop - division * gridStep);
        canvasCtx.fillText(String(gridValue), 2, gridLineY - 3 < 8 ? gridLineY + 11 : gridLineY - 3);
      }
      if (visiblePointCount < 1) return;

      // Split visible samples into contiguous segments, breaking anywhere
      // consecutive readings are more than gapThresholdMs apart. Each
      // segment gets its own fill + stroke so offline periods render as blank
      // space. The threshold tracks the server's bucket width — at 24h the
      // points are ~40s apart by design, and a fixed 3s gap would shred the trace.
      const segments = [];
      let currentSegment = [];
      for (const point of points) {
        if (point.time < windowStartMs) continue;
        const previousPoint = currentSegment[currentSegment.length - 1];
        if (previousPoint && point.time - previousPoint.time > gapThresholdMs) {
          segments.push(currentSegment);
          currentSegment = [];
        }
        currentSegment.push(point);
      }
      if (currentSegment.length > 0) segments.push(currentSegment);

      for (const segment of segments) {
        // Fill under
        canvasCtx.beginPath();
        canvasCtx.moveTo(xForTime(segment[0].time), plotHeight);
        for (const point of segment) {
          canvasCtx.lineTo(xForTime(point.time), yForValue(point.value));
        }
        canvasCtx.lineTo(xForTime(segment[segment.length - 1].time), plotHeight);
        canvasCtx.closePath();
        canvasCtx.fillStyle = withAlpha(traceColor, traceFillOpacity);
        canvasCtx.fill();
        // Trace
        canvasCtx.beginPath();
        for (let pointIndex = 0; pointIndex < segment.length; pointIndex++) {
          const pointX = xForTime(segment[pointIndex].time);
          const pointY = yForValue(segment[pointIndex].value);
          pointIndex ? canvasCtx.lineTo(pointX, pointY) : canvasCtx.moveTo(pointX, pointY);
        }
        canvasCtx.strokeStyle = traceColor;
        canvasCtx.lineWidth = 1;
        canvasCtx.lineJoin = "round";
        canvasCtx.stroke();
      }

      // Leading dot on the most recent visible sample
      const newestPoint = points[points.length - 1];
      if (newestPoint && newestPoint.time >= windowStartMs) {
        const dotX = xForTime(newestPoint.time);
        const dotY = yForValue(newestPoint.value);
        canvasCtx.beginPath();
        canvasCtx.arc(dotX, dotY, LEADING_DOT_RADIUS_PX, 0, Math.PI * 2);
        canvasCtx.fillStyle = traceColor;
        canvasCtx.fill();
      }

      // Hover crosshair
      if (hoveredIndex >= 0 && hoveredIndex < points.length) {
        const hoveredPoint = points[hoveredIndex];
        if (hoveredPoint.time >= windowStartMs) {
          const crosshairX = xForTime(hoveredPoint.time);
          const crosshairY = yForValue(hoveredPoint.value);
          canvasCtx.strokeStyle = CROSSHAIR_LINE_COLOR;
          canvasCtx.lineWidth = 1;
          canvasCtx.beginPath();
          canvasCtx.moveTo(Math.round(crosshairX) + 0.5, 0);
          canvasCtx.lineTo(Math.round(crosshairX) + 0.5, plotHeight);
          canvasCtx.stroke();
          canvasCtx.beginPath();
          canvasCtx.arc(crosshairX, crosshairY, HOVER_DOT_RADIUS_PX, 0, Math.PI * 2);
          canvasCtx.fillStyle = traceColor;
          canvasCtx.fill();
          canvasCtx.beginPath();
          canvasCtx.arc(crosshairX, crosshairY, HOVER_RING_RADIUS_PX, 0, Math.PI * 2);
          canvasCtx.strokeStyle = CROSSHAIR_RING_COLOR;
          canvasCtx.lineWidth = 1;
          canvasCtx.stroke();
        }
      }
    }

    // addPointSilently appends without redrawing. Used when loading a range so
    // we can insert a couple thousand points without repainting once per point.
    // Caller must invoke draw() when the batch is done.
    function addPointSilently(time, value) {
      points.push({ time, value });
    }
    function addPoint(time, value) {
      addPointSilently(time, value);
      if (hoveredIndex >= 0 && lastMouseX !== null) {
        hoveredIndex = nearestPointIndexAtX(lastMouseX);
      }
      draw();
      if (hoveredIndex >= 0) updateHoverBadge();
    }
    // Trace-breaking threshold follows the server's bucket width, with slack
    // for jitter and the occasional dropped sample.
    function setBucketWidth(bucketWidthMs) {
      gapThresholdMs = Math.max(MIN_GAP_THRESHOLD_MS, (bucketWidthMs || ASSUMED_BUCKET_MS) * GAP_THRESHOLD_MULTIPLIER);
    }
    function clearPoints() {
      points = [];
      hoveredIndex = -1;
      hoverBadge.classList.remove("is-visible");
    }

    // Binary search for the sample closest in TIME to the pixel x. Then
    // gate on pixel distance so hovering in the middle of a big gap doesn't
    // snap onto a distant sample from the far side of the gap.
    function nearestPointIndexAtX(pixelX) {
      if (points.length < 1) return -1;
      const nowMs = Date.now();
      const windowStartMs = viewWindowStartMs(nowMs);
      const windowSpanMs = Math.max(1, nowMs - windowStartMs);
      const targetTimeMs = windowStartMs + (pixelX / widthPx) * windowSpanMs;

      let low = 0;
      let high = points.length - 1;
      while (low < high) {
        const middle = (low + high) >> 1;
        if (points[middle].time < targetTimeMs) low = middle + 1;
        else high = middle;
      }
      let nearestIndex = low;
      if (low > 0 && Math.abs(points[low - 1].time - targetTimeMs) < Math.abs(points[low].time - targetTimeMs)) {
        nearestIndex = low - 1;
      }
      if (points[nearestIndex].time < windowStartMs) return -1;
      // Snap only if the cursor is within HOVER_SNAP_PX of the nearest sample.
      const pointX = (widthPx * (points[nearestIndex].time - windowStartMs)) / windowSpanMs;
      if (Math.abs(pointX - pixelX) > HOVER_SNAP_PX) return -1;
      return nearestIndex;
    }

    function updateHoverBadge() {
      if (hoveredIndex < 0 || hoveredIndex >= points.length) {
        hoverBadge.classList.remove("is-visible");
        return;
      }
      const hoveredPoint = points[hoveredIndex];
      const nowMs = Date.now();
      const windowStartMs = viewWindowStartMs(nowMs);
      const windowSpanMs = Math.max(1, nowMs - windowStartMs);
      const pointX = (widthPx * (hoveredPoint.time - windowStartMs)) / windowSpanMs;

      hoverBadge.style.left = canvas.offsetLeft + pointX + "px";
      hoverBadge.style.top = canvas.offsetTop + "px";

      const pointTime = hoveredPoint.time;

      const day = pointTime.getDate();
      const month = pointTime.toLocaleString("en-US", { month: "short" });
      const dateLabel = `${day}. ${month}`;

      const hours = String(pointTime.getHours()).padStart(2, "0");
      const minutes = String(pointTime.getMinutes()).padStart(2, "0");
      const seconds = String(pointTime.getSeconds()).padStart(2, "0");
      const timeLabel = `${hours}:${minutes}:${seconds}`;

      hoverBadge.innerHTML = `${formatValue(hoveredPoint.value)}` + `<span class="hover-badge-time">${dateLabel} - ${timeLabel}</span>`;
      hoverBadge.classList.add("is-visible");
    }

    let lastMouseX = null;
    canvas.addEventListener("mousemove", event => {
      const rect = canvas.getBoundingClientRect();
      lastMouseX = event.clientX - rect.left;
      const newHoveredIndex = nearestPointIndexAtX(lastMouseX);
      if (newHoveredIndex !== hoveredIndex) {
        hoveredIndex = newHoveredIndex;
        draw();
      }
      updateHoverBadge();
    });
    canvas.addEventListener("mouseleave", () => {
      lastMouseX = null;
      hoveredIndex = -1;
      hoverBadge.classList.remove("is-visible");
      draw();
    });

    return { resizeCanvas, draw, addPoint, addPointSilently, clearPoints, setBucketWidth };
  }

  const formatMs = value => `${Math.round(value)} ms`;
  const formatPercent = value => `${value.toFixed(2)} %`;
  const formatMbps = value => `${value.toFixed(1)} Mbps`;

  const latencyScope = createScope({
    canvasId: "chart-latency",
    rangeLabelId: "chart-range-latency",
    unit: "ms",
    minAxisTop: 20,
    axisRounding: 10,
    colorVar: "--chart-latency",
    formatValue: formatMs,
  });
  const packetLossScope = createScope({
    canvasId: "chart-packet-loss",
    rangeLabelId: "chart-range-packet-loss",
    unit: "%",
    minAxisTop: 5,
    axisRounding: 5,
    minGridStep: 1,
    colorVar: "--chart-packet-loss",
    formatValue: formatPercent,
  });
  const downloadScope = createScope({
    canvasId: "chart-download",
    rangeLabelId: "chart-range-download",
    unit: "Mbps",
    minAxisTop: 100,
    axisRounding: 100,
    colorVar: "--chart-download",
    formatValue: formatMbps,
  });
  const uploadScope = createScope({
    canvasId: "chart-upload",
    rangeLabelId: "chart-range-upload",
    unit: "Mbps",
    minAxisTop: 5,
    axisRounding: 10,
    colorVar: "--chart-upload",
    formatValue: formatMbps,
  });
  const machineToInternetPingScope = createScope({
    canvasId: "chart-machine-to-internet-ping",
    rangeLabelId: "chart-range-machine-to-internet-ping",
    unit: "ms",
    minAxisTop: 20,
    axisRounding: 10,
    colorVar: "--chart-machine-to-internet-ping",
    formatValue: formatMs,
  });
  const machineToInternetLossScope = createScope({
    canvasId: "chart-machine-to-internet-loss",
    rangeLabelId: "chart-range-machine-to-internet-loss",
    unit: "%",
    minAxisTop: 5,
    maxAxisTop: 100, // a lost ping is a 100% bucket; headroom above that is meaningless
    axisRounding: 5,
    minGridStep: 1,
    colorVar: "--chart-machine-to-internet-loss",
    formatValue: formatPercent,
  });
  const dishScopes = [latencyScope, packetLossScope, downloadScope, uploadScope];
  const machineToInternetScopes = [machineToInternetPingScope, machineToInternetLossScope];
  const scopes = [...dishScopes, ...machineToInternetScopes];

  // ---- Applying samples --------------------------------------------------
  // updateReadouts sets the current-state UI (alarm strip, tiles, uptime clock).
  // chartSample only pushes to the strip charts. Splitting the two lets a
  // range load push thousands of points silently without doing thousands of
  // pointless UI updates — and keeps downsampled worst-case buckets out of the
  // readouts, which must always show the genuine latest reading.
  const LATENCY_HOT_MS = 100;
  const LATENCY_WARM_MS = 70;
  const DROP_HOT_PERCENT = 5;
  const DROP_WARM_PERCENT = 0.5;

  function updateReadouts(sample) {
    setLinkState(sample.link_state);
    byId("value-latency").textContent = formatNumber(sample.latency_ms, 0);
    byId("value-download").textContent = formatNumber(sample.download_mbps, 1);
    byId("value-upload").textContent = formatNumber(sample.upload_mbps, 1);
    const dropPercent = (sample.drop_rate_fraction || 0) * 100;
    byId("value-packet-loss").textContent = formatNumber(dropPercent, 1);

    byId("cell-latency").className =
      "cell" +
      (sample.latency_ms > LATENCY_HOT_MS
        ? " is-critical"
        : sample.latency_ms > LATENCY_WARM_MS
          ? " is-caution"
          : "");
    byId("cell-packet-loss").className =
      "cell" +
      (dropPercent > DROP_HOT_PERCENT
        ? " is-critical"
        : dropPercent > DROP_WARM_PERCENT
          ? " is-caution"
          : "");

    const isOffline = sample.link_state === "OFFLINE";
    byId("line-of-sight-state").textContent = isOffline
      ? "—"
      : sample.obstructed
        ? "OBSTRUCTED"
        : "CLEAR";
    byId("line-of-sight").className =
      "status-value is-row " + (isOffline ? "" : sample.obstructed ? "is-alarm" : "is-nominal");
    byId("line-of-sight-percent").textContent = isOffline
      ? ""
      : formatNumber((sample.obstruction_fraction || 0) * 100, 2) + "% obstruction";

    if (sample.hardware_version) byId("hardware-version").textContent = sample.hardware_version;
    if (sample.software_version) byId("software-version").textContent = sample.software_version;
    byId("uptime-clock").textContent = formatUptimeClock(sample.uptime_seconds);
    byId("last-contact").textContent = sample.timestamp
      ? new Date(sample.timestamp).toLocaleTimeString()
      : new Date().toLocaleTimeString();
  }

  function chartSample(sample, silent) {
    if (sample.link_state === "OFFLINE") return;
    const timestamp = sample.timestamp ? new Date(sample.timestamp) : new Date();
    const dropPercent = (sample.drop_rate_fraction || 0) * 100;
    const addMethod = silent ? "addPointSilently" : "addPoint";
    packetLossScope[addMethod](timestamp, dropPercent);
    downloadScope[addMethod](timestamp, sample.download_mbps || 0);
    uploadScope[addMethod](timestamp, sample.upload_mbps || 0);
    if (sample.latency_ms > 0) latencyScope[addMethod](timestamp, sample.latency_ms);
  }

  function applyLiveSample(sample) {
    updateReadouts(sample);
    chartSample(sample, false);
  }

  // ---- Applying pings ----------------------------------------------------
  // Live pings arrive as one echo ({rtt_ms, lost: bool}); history arrives as
  // buckets ({rtt_ms, sent, lost: count}). Charting works on buckets, so a live
  // ping is just a bucket of one.
  const MACHINE_TO_INTERNET_LOSS_WINDOW_MS = 60 * 1000; // readout loss % covers this much
  let recentPings = []; // {timeMs, lost} inside MACHINE_TO_INTERNET_LOSS_WINDOW_MS

  function toPingBucket(ping) {
    if (typeof ping.sent === "number") return ping;
    return { timestamp: ping.timestamp, rtt_ms: ping.rtt_ms || 0, sent: 1, lost: ping.lost ? 1 : 0 };
  }

  function severityClass(value, hot, warm) {
    return value > hot ? " is-critical" : value > warm ? " is-caution" : "";
  }

  function updatePingReadouts(ping) {
    const timeMs = new Date(ping.timestamp).getTime();
    recentPings.push({ timeMs, lost: Boolean(ping.lost) });
    recentPings = recentPings.filter(recent => recent.timeMs > timeMs - MACHINE_TO_INTERNET_LOSS_WINDOW_MS);

    const lostCount = recentPings.filter(recent => recent.lost).length;
    const lossPercent = (lostCount / recentPings.length) * 100;
    byId("value-machine-to-internet-loss").textContent = formatNumber(lossPercent, 1);
    byId("cell-machine-to-internet-loss").className = "cell" + severityClass(lossPercent, DROP_HOT_PERCENT, DROP_WARM_PERCENT);

    byId("value-machine-to-internet-ping").textContent = ping.lost ? "—" : formatNumber(ping.rtt_ms, 0);
    byId("cell-machine-to-internet-ping").className =
      "cell" + (ping.lost ? " is-critical" : severityClass(ping.rtt_ms, LATENCY_HOT_MS, LATENCY_WARM_MS));
    if (ping.target) byId("machine-to-internet-ping-target").textContent = ping.target;
  }

  function clearPingReadouts() {
    recentPings = [];
    for (const id of ["value-machine-to-internet-ping", "value-machine-to-internet-loss"]) byId(id).textContent = "—";
    for (const id of ["cell-machine-to-internet-ping", "cell-machine-to-internet-loss"]) byId(id).className = "cell";
  }

  function chartPing(ping, silent) {
    const bucket = toPingBucket(ping);
    if (!bucket.sent) return;
    const timestamp = new Date(bucket.timestamp);
    const addMethod = silent ? "addPointSilently" : "addPoint";
    // A fully lost bucket has no RTT; leave the ping trace alone and let the
    // loss chart carry it.
    if (bucket.lost < bucket.sent && bucket.rtt_ms > 0) machineToInternetPingScope[addMethod](timestamp, bucket.rtt_ms);
    machineToInternetLossScope[addMethod](timestamp, (bucket.lost / bucket.sent) * 100);
  }

  // ---- Range selector ----------------------------------------------------
  const rangeBarEl = byId("range-bar");

  function buildRangeBar() {
    for (const rangeOption of RANGES) {
      const button = document.createElement("button");
      button.type = "button";
      button.className = "range-button";
      button.dataset.id = rangeOption.id;
      button.textContent = rangeOption.label;
      button.addEventListener("click", () => selectRange(rangeOption.id));
      rangeBarEl.appendChild(button);
    }
  }

  function markActiveRange(activeRangeId) {
    for (const button of rangeBarEl.children) {
      const isActive = button.dataset.id === activeRangeId;
      button.classList.toggle("is-selected", isActive);
      button.setAttribute("aria-pressed", String(isActive));
    }
  }

  // Guards against out-of-order responses: mash 24h then 2m and the slow 24h
  // reply must not overwrite the fast one.
  let latestRangeLoadToken = 0;

  async function fetchHistory(route, rangeId) {
    const response = await fetch(`${route}?range=${encodeURIComponent(rangeId)}`, { cache: "no-store" });
    if (!response.ok) throw new Error(`${response.status} ${response.statusText}`);
    const payload = await response.json();
    if (!Array.isArray(payload.samples)) payload.samples = [];
    return payload;
  }

  async function selectRange(rangeId) {
    const selectedRange = RANGES.find(range => range.id === rangeId);
    if (!selectedRange) return;
    const loadToken = ++latestRangeLoadToken;

    viewWindow.selectedRange = selectedRange;
    viewWindow.pinnedStartMs = null;
    markActiveRange(selectedRange.id);
    rangeBarEl.classList.add("is-loading");

    try {
      // Dish and ping history load independently: one failing shouldn't
      // blank the other's charts.
      const [dishResult, pingResult] = await Promise.allSettled([
        fetchHistory("/history", selectedRange.id),
        fetchHistory("/pinghistory", selectedRange.id),
      ]);
      if (loadToken !== latestRangeLoadToken) return; // superseded by a later click

      for (const [source, result] of [["dish", dishResult], ["ping", pingResult]]) {
        if (result.status === "rejected") console.error("could not load", source, "range", selectedRange.id, result.reason);
      }
      const dishPayload = dishResult.status === "fulfilled" ? dishResult.value : null;
      const pingPayload = pingResult.status === "fulfilled" ? pingResult.value : null;

      if (selectedRange.spanMs === null) {
        const oldestTimes = [dishPayload, pingPayload]
          .filter(payload => payload && payload.samples.length)
          .map(payload => new Date(payload.samples[0].timestamp).getTime());
        viewWindow.pinnedStartMs = oldestTimes.length ? Math.min(...oldestTimes) : Date.now() - EMPTY_ALL_RANGE_SPAN_MS;
      }

      if (dishPayload) {
        dishScopes.forEach(scope => {
          scope.clearPoints();
          scope.setBucketWidth(dishPayload.bucket_width_ms);
        });
        for (const sample of dishPayload.samples) chartSample(sample, true);
      }
      if (pingPayload) {
        machineToInternetScopes.forEach(scope => {
          scope.clearPoints();
          scope.setBucketWidth(pingPayload.bucket_width_ms);
        });
        for (const bucket of pingPayload.samples) chartPing(bucket, true);
      }
      scopes.forEach(scope => scope.draw());
    } finally {
      if (loadToken === latestRangeLoadToken) rangeBarEl.classList.remove("is-loading");
    }
  }

  // ---- Live stream -------------------------------------------------------
  function connectToTelemetryStream() {
    const eventSource = new EventSource("/events");
    eventSource.onopen = () => {
      connectionIndicator.className = "connection-status is-live";
      connectionText.textContent = "Telemetry live";
    };
    // The hub's backfill is no longer the charts' data source — /history owns
    // that — so we only take the newest sample from it to prime the readouts.
    eventSource.addEventListener("backfill", event => {
      try {
        const backfillSamples = JSON.parse(event.data);
        if (Array.isArray(backfillSamples) && backfillSamples.length) {
          updateReadouts(backfillSamples[backfillSamples.length - 1]);
        }
      } catch {}
    });
    eventSource.onmessage = event => {
      try {
        applyLiveSample(JSON.parse(event.data));
      } catch {}
    };
    eventSource.onerror = () => {
      connectionIndicator.className = "connection-status is-lost";
      connectionText.textContent = "Reacquiring…";
      setLinkState("OFFLINE");
    };
  }

  function connectToPingStream() {
    const eventSource = new EventSource("/pingevents");
    // Like the dish stream: history owns the charts, backfill only primes the
    // readouts — here with the last minute, so loss % is right immediately.
    eventSource.addEventListener("backfill", event => {
      try {
        const backfillPings = JSON.parse(event.data);
        if (!Array.isArray(backfillPings) || !backfillPings.length) return;
        const newestMs = new Date(backfillPings[backfillPings.length - 1].timestamp).getTime();
        clearPingReadouts();
        for (const ping of backfillPings) {
          if (new Date(ping.timestamp).getTime() > newestMs - MACHINE_TO_INTERNET_LOSS_WINDOW_MS) updatePingReadouts(ping);
        }
      } catch {}
    });
    eventSource.onmessage = event => {
      try {
        const ping = JSON.parse(event.data);
        updatePingReadouts(ping);
        chartPing(ping, false);
      } catch {}
    };
    // Losing the dashboard server says nothing about the internet, so show no
    // reading rather than a stale one.
    eventSource.onerror = () => clearPingReadouts();
  }

  // Repaint once per second even without new data, so the strip keeps
  // advancing to keep "now" at the right edge — otherwise a disconnected
  // client would show a frozen chart with the last sample stuck on the right.
  const REPAINT_INTERVAL_MS = 1000;
  setInterval(() => scopes.forEach(scope => scope.draw()), REPAINT_INTERVAL_MS);

  // On a long range, live 1 Hz samples pile onto a downsampled series and the
  // right-hand end slowly gets denser than the rest. Re-pulling occasionally
  // re-buckets it. Might cause reload flicker
  const REBUCKET_INTERVAL_MS = 5 * 60 * 1000;
  const REBUCKET_RANGES_LONGER_THAN_MS = 15 * 60 * 1000;
  setInterval(() => {
    const spanMs = viewWindow.selectedRange.spanMs;
    if (spanMs === null || spanMs > REBUCKET_RANGES_LONGER_THAN_MS) {
      selectRange(viewWindow.selectedRange.id);
    }
  }, REBUCKET_INTERVAL_MS);

  window.addEventListener("resize", () => scopes.forEach(scope => scope.resizeCanvas()));
  buildRangeBar();
  scopes.forEach(scope => scope.resizeCanvas());
  connectToTelemetryStream();
  connectToPingStream();
  selectRange(DEFAULT_RANGE_ID);
})();
