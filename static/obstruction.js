(() => {
  "use strict";

  const OBSTRUCTION_MAP_URL = "/obstructionmap";
  const OBSTRUCTION_MAP_POLL_MS = 60000;
  const RING_STEP_DEG = 2.2; // dome dot spacing; each dot covers every map cell (~1.3°) within half a step
  const DEFAULT_TILT_DEG = 35; // how far above the horizon the dome is seen from
  const MIN_TILT_DEG = 10;
  const MAX_TILT_DEG = 85;
  const NEAR_SIDE_ALPHA = 0.4; // the half of the dome facing the viewer fades to this
  const TO_RADIANS = Math.PI / 180;
  const COMPASS = [
    { label: "N", azimuth: 0 },
    { label: "E", azimuth: 90 },
    { label: "S", azimuth: 180 },
    { label: "W", azimuth: 270 },
  ];

  const canvas = document.getElementById("obstruction-dome");
  if (!canvas) return;
  const drawing = canvas.getContext("2d");
  const card = document.getElementById("obstruction-card");
  const summary = document.getElementById("obstruction-summary");

  const readVariable = (name, fallback) =>
    getComputedStyle(document.documentElement).getPropertyValue(name).trim() || fallback;
  const theme = {
    label: readVariable("--label", "#8a97a6"),
    labelDim: readVariable("--label-dim", "#586675"),
    alarm: readVariable("--alarm", "#e5533d"),
    mono: readVariable("--mono", "monospace"),
  };
  const DOT_COLOURS = {
    outside: theme.labelDim, // below the service floor: the dish never uses it
    unmapped: "#434b55",
    clear: "#eef2f6",
    obstructed: theme.alarm,
  };

  let map = null;
  let boresight = null;
  let yawDeg = 180;
  let tiltDeg = DEFAULT_TILT_DEG;
  let userTurned = false;
  let redrawQueued = false;

  // Dots on rings of equal elevation, like the Starlink app's dome.
  const dots = [];
  for (let elevation = 0; elevation <= 90; elevation += RING_STEP_DEG) {
    const count = Math.max(1, Math.round((360 * Math.cos(elevation * TO_RADIANS)) / RING_STEP_DEG));
    const stagger = (dots.length % 2) * 0.5;
    for (let k = 0; k < count; k++) {
      const azimuth = ((k + stagger) * 360) / count;
      const az = azimuth * TO_RADIANS;
      const el = elevation * TO_RADIANS;
      dots.push({
        azimuth,
        elevation,
        x: Math.cos(el) * Math.sin(az),
        y: Math.cos(el) * Math.cos(az),
        z: Math.sin(el),
        state: "unmapped",
      });
    }
  }

  // A dot covers the map cells within half a dot spacing: obstructed if any is (so none is skipped),
  // clear only if most are mapped, so a part-filled map isn't drawn as clear sky.
  const classifyDots = () => {
    const usable = map && map.enabled && map.cells && map.cells.length === map.rows * map.cols;
    for (const dot of dots) {
      dot.state = "unmapped";
      if (!usable) continue;
      const zenith = 90 - dot.elevation;
      if (zenith > map.max_theta_deg) {
        dot.state = "outside";
        continue;
      }
      const centre = (map.rows - 1) / 2;
      const cellDeg = map.max_theta_deg / centre;
      const radius = zenith / cellDeg;
      const col = centre + Math.sin(dot.azimuth * TO_RADIANS) * radius;
      const row = centre - Math.cos(dot.azimuth * TO_RADIANS) * radius;
      const stretch = zenith > 0 ? (zenith * TO_RADIANS) / Math.sin(zenith * TO_RADIANS) : 1; // equidistant map, along azimuth
      const reach = ((RING_STEP_DEG / 2) * stretch) / cellDeg + 0.5;
      let cells = 0;
      let mapped = 0;
      let seenObstructed = false;
      for (let r = Math.round(row - reach); r <= Math.round(row + reach); r++) {
        for (let c = Math.round(col - reach); c <= Math.round(col + reach); c++) {
          if (r < 0 || c < 0 || r >= map.rows || c >= map.cols) continue;
          const cell = map.cells[r * map.cols + c];
          cells++;
          if (cell !== "0") mapped++;
          if (cell === "2") seenObstructed = true;
        }
      }
      if (seenObstructed) dot.state = "obstructed";
      else if (mapped * 2 >= cells && cells > 0) dot.state = "clear";
      else if (dot.elevation < map.floor_deg) dot.state = "outside";
    }
  };

  // Orthographic camera at yawDeg azimuth, tiltDeg above the horizon. depth > 0 faces the viewer.
  const camera = () => {
    const yaw = yawDeg * TO_RADIANS;
    const tilt = tiltDeg * TO_RADIANS;
    return {
      right: [-Math.cos(yaw), Math.sin(yaw), 0],
      up: [-Math.sin(tilt) * Math.sin(yaw), -Math.sin(tilt) * Math.cos(yaw), Math.cos(tilt)],
      toward: [Math.cos(tilt) * Math.sin(yaw), Math.cos(tilt) * Math.cos(yaw), Math.sin(tilt)],
    };
  };
  const dot3 = (a, b) => a[0] * b[0] + a[1] * b[1] + a[2] * b[2];

  const resizeCanvas = () => {
    const pixelRatio = window.devicePixelRatio || 1;
    canvas.width = Math.round(canvas.clientWidth * pixelRatio);
    canvas.height = Math.round(canvas.clientHeight * pixelRatio);
    drawing.setTransform(pixelRatio, 0, 0, pixelRatio, 0, 0);
  };

  const render = () => {
    redrawQueued = false;
    const width = canvas.clientWidth;
    const height = canvas.clientHeight;
    if (!width || !height) return;
    drawing.clearRect(0, 0, width, height);

    const view = camera();
    const tilt = tiltDeg * TO_RADIANS;
    const top = Math.max(Math.cos(tilt), 1.32 * Math.sin(tilt));
    const bottom = 1.32 * Math.sin(tilt);
    const radius = Math.min(width / 2.75, (height - 16) / (top + bottom + 0.12));
    const centreX = width / 2;
    const centreY = (height - (top + bottom) * radius) / 2 + top * radius;
    const toScreen = (point) => ({
      x: centreX + dot3(view.right, point) * radius,
      y: centreY - dot3(view.up, point) * radius,
      depth: dot3(view.toward, point),
    });

    // Ground under the dome.
    drawing.save();
    drawing.translate(centreX, centreY);
    drawing.scale(1, Math.max(Math.sin(tilt), 0.05));
    const ground = drawing.createRadialGradient(0, 0, 0, 0, 0, radius * 1.1);
    ground.addColorStop(0, "rgba(255, 255, 255, 0.07)");
    ground.addColorStop(1, "rgba(255, 255, 255, 0)");
    drawing.fillStyle = ground;
    drawing.beginPath();
    drawing.arc(0, 0, radius * 1.1, 0, Math.PI * 2);
    drawing.fill();
    drawing.restore();

    // Compass ticks on the ground, a long tick every 45°.
    drawing.strokeStyle = theme.label;
    drawing.lineWidth = 1.2;
    drawing.beginPath();
    for (let azimuth = 0; azimuth < 360; azimuth += 5) {
      if (azimuth % 90 === 0) continue;
      const az = azimuth * TO_RADIANS;
      const length = azimuth % 45 === 0 ? 0.1 : 0.05;
      const inner = toScreen([1.12 * Math.sin(az), 1.12 * Math.cos(az), 0]);
      const outer = toScreen([(1.12 + length) * Math.sin(az), (1.12 + length) * Math.cos(az), 0]);
      drawing.moveTo(inner.x, inner.y);
      drawing.lineTo(outer.x, outer.y);
    }
    drawing.stroke();
    drawing.fillStyle = theme.label;
    drawing.font = `600 13px ${theme.mono}`;
    drawing.textAlign = "center";
    drawing.textBaseline = "middle";
    for (const point of COMPASS) {
      const az = point.azimuth * TO_RADIANS;
      const at = toScreen([1.2 * Math.sin(az), 1.2 * Math.cos(az), 0]);
      drawing.fillText(point.label, at.x, at.y);
    }

    const dotRadius = Math.min(2.4, Math.max(1, radius / 95));
    const drawDots = (nearSide) => {
      const paths = new Map();
      for (const dot of dots) {
        const at = toScreen([dot.x, dot.y, dot.z]);
        if ((at.depth >= 0) !== nearSide) continue;
        let alpha = nearSide ? 1 - (1 - NEAR_SIDE_ALPHA) * Math.min(1, at.depth * 1.6) : 1;
        if (dot.state === "obstructed") alpha = Math.max(alpha, 0.6);
        if (dot.state === "outside") alpha *= 0.35;
        const key = `${dot.state}|${Math.round(alpha * 10)}`;
        if (!paths.has(key)) paths.set(key, new Path2D());
        const size = dot.state === "obstructed" ? dotRadius * 1.25 : dotRadius;
        paths.get(key).moveTo(at.x + size, at.y);
        paths.get(key).arc(at.x, at.y, size, 0, Math.PI * 2);
      }
      for (const [key, path] of paths) {
        const [state, alpha] = key.split("|");
        drawing.globalAlpha = Number(alpha) / 10;
        drawing.fillStyle = DOT_COLOURS[state];
        drawing.fill(path);
      }
      drawing.globalAlpha = 1;
    };

    drawDots(false);
    drawDots(true);
    drawPanel(toScreen, view); // on top, like the app, so near-side dots don't speckle it
  };

  // The dish panel at the centre, tilted to its reported boresight (flat until the dish reports one).
  const drawPanel = (toScreen, view) => {
    const az = (boresight ? boresight.azimuth : 0) * TO_RADIANS;
    const el = (boresight ? boresight.elevation : 90) * TO_RADIANS;
    const normal = [Math.cos(el) * Math.sin(az), Math.cos(el) * Math.cos(az), Math.sin(el)];
    const across = [Math.cos(az), -Math.sin(az), 0];
    const along = [
      normal[1] * across[2] - normal[2] * across[1],
      normal[2] * across[0] - normal[0] * across[2],
      normal[0] * across[1] - normal[1] * across[0],
    ];
    const corner = (a, b, lift, height) =>
      [0, 1, 2].map((i) => a * 0.13 * across[i] + b * 0.2 * along[i] + lift * normal[i] + (i === 2 ? height : 0));
    const outline = (lift, height, flatten) =>
      [[-1, -1], [1, -1], [1, 1], [-1, 1]].map(([a, b]) => {
        const point = corner(a, b, lift, height);
        if (flatten) point[2] = 0;
        return toScreen(point);
      });
    const fillPolygon = (points, style) => {
      drawing.beginPath();
      points.forEach((point, i) => (i ? drawing.lineTo(point.x, point.y) : drawing.moveTo(point.x, point.y)));
      drawing.closePath();
      drawing.fillStyle = style;
      drawing.fill();
    };

    drawing.save();
    drawing.filter = "blur(4px)";
    fillPolygon(outline(0, 0.1, true), "rgba(0, 0, 0, 0.55)");
    drawing.restore();
    const facing = dot3(normal, view.toward) > 0;
    fillPolygon(outline(facing ? -0.012 : 0.012, 0.1, false), "#7d8792");
    fillPolygon(outline(0, 0.1, false), facing ? "#f4f6f8" : "#aab2bb");
  };

  const requestRender = () => {
    if (redrawQueued) return;
    redrawQueued = true;
    requestAnimationFrame(render);
  };

  const updateSummary = () => {
    const enabled = Boolean(map && map.enabled);
    if (card) card.hidden = !enabled;
    if (!enabled || !summary) return;
    const filled = `map ${Math.floor(map.coverage * 100)}% filled`;
    summary.textContent = map.obstruction_fraction == null
      ? `${filled} · % shown from ${Math.round(map.min_coverage * 100)}%`
      : `${(map.obstruction_fraction * 100).toFixed(2)}% obstructed · ${filled}`;
  };

  const loadMap = async () => {
    try {
      const response = await fetch(OBSTRUCTION_MAP_URL, { cache: "no-store" });
      if (!response.ok) return;
      const wasHidden = !card || card.hidden;
      map = await response.json();
      updateSummary();
      classifyDots();
      if (wasHidden && map.enabled) resizeCanvas();
      requestRender();
    } catch (error) {
      console.warn("obstruction map poll failed", error);
    }
  };

  // Seen from in front of the panel by default, like the Starlink app.
  window.addEventListener("houston:sky", (event) => {
    const snapshot = event.detail;
    if (!snapshot || snapshot.cone_is_assumed) return;
    const next = { azimuth: snapshot.cone_azimuth_deg, elevation: snapshot.cone_elevation_deg };
    if (boresight && boresight.azimuth === next.azimuth && boresight.elevation === next.elevation) return;
    boresight = next;
    if (!userTurned) yawDeg = boresight.azimuth;
    requestRender();
  });

  let dragFrom = null;
  canvas.addEventListener("pointerdown", (event) => {
    dragFrom = { x: event.clientX, y: event.clientY };
    canvas.setPointerCapture(event.pointerId);
  });
  canvas.addEventListener("pointermove", (event) => {
    if (!dragFrom) return;
    yawDeg = (yawDeg - (event.clientX - dragFrom.x) * 0.5 + 360) % 360;
    tiltDeg = Math.min(MAX_TILT_DEG, Math.max(MIN_TILT_DEG, tiltDeg + (event.clientY - dragFrom.y) * 0.3));
    dragFrom = { x: event.clientX, y: event.clientY };
    userTurned = true;
    requestRender();
  });
  const endDrag = () => (dragFrom = null);
  canvas.addEventListener("pointerup", endDrag);
  canvas.addEventListener("pointercancel", endDrag);

  window.addEventListener("resize", () => {
    resizeCanvas();
    requestRender();
  });

  loadMap();
  setInterval(loadMap, OBSTRUCTION_MAP_POLL_MS);
})();
