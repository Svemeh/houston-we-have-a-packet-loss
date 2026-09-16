(() => {
  "use strict";

  const SKY_EVENTS_URL = "/skyevents";
  const TO_RADIANS = Math.PI / 180;

  // Only 60° is decorative. Zenith, the service floor and the horizon all mean
  // something on their own, so a 30° ring would just crowd the floor.
  const ELEVATION_RINGS = [15, 30, 45, 60];
  const CARDINAL_POINTS = [
    { label: "N", azimuth: 0 },
    { label: "E", azimuth: 90 },
    { label: "S", azimuth: 180 },
    { label: "W", azimuth: 270 },
  ];

  const canvas = document.getElementById("sky-scope");
  const drawing = canvas.getContext("2d");

  const elements = {
    clock: document.getElementById("sky-clock"),
    strip: document.getElementById("sky-strip"),
    headline: document.getElementById("sky-headline"),
    subtext: document.getElementById("sky-subtext"),
    inCone: document.getElementById("count-in-cone"),
    above: document.getElementById("count-above"),
    coneNote: document.getElementById("cone-note"),
    elementAge: document.getElementById("element-age"),
    connection: document.getElementById("connection-status"),
    connectionText: document.getElementById("connection-text"),
  };

  let latestSnapshot = null;

  const readVariable = (name, fallback) => {
    const value = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
    return value || fallback;
  };

  const theme = {
    edge: readVariable("--edge", "#232e3b"),
    grid: readVariable("--grid", "#1b2531"),
    amber: readVariable("--amber", "#f5b740"),
    labelDim: readVariable("--label-dim", "#586675"),
    label: readVariable("--label", "#8a97a6"),
    aboveHorizon: "#4a6076",
    mono: readVariable("--mono", "monospace"),
  };

  // ---- Projection ------------------------------------------------------
  // Zenith at the centre, horizon at the rim, north up, azimuth clockwise.
  // Radius is linear in zenith angle: equidistant, so a satellite crossing
  // overhead moves at a believable rate rather than piling up at the edge.
  const project = (azimuthDeg, elevationDeg, centreX, centreY, radius) => {
    const distanceFromZenith = ((90 - elevationDeg) / 90) * radius;
    const azimuthRadians = azimuthDeg * TO_RADIANS;
    return {
      x: centreX + distanceFromZenith * Math.sin(azimuthRadians),
      y: centreY - distanceFromZenith * Math.cos(azimuthRadians),
    };
  };

  const radiusForElevation = (elevationDeg, radius) => ((90 - elevationDeg) / 90) * radius;

  // ---- Drawing ---------------------------------------------------------

  const resizeCanvas = () => {
    const pixelRatio = window.devicePixelRatio || 1;
    const cssSize = canvas.clientWidth;
    canvas.width = Math.round(cssSize * pixelRatio);
    canvas.height = Math.round(cssSize * pixelRatio);
    drawing.setTransform(pixelRatio, 0, 0, pixelRatio, 0, 0);
  };

  const drawGraticule = (centreX, centreY, radius) => {
    drawing.lineWidth = 1;

    drawing.strokeStyle = theme.grid;
    for (const elevation of ELEVATION_RINGS) {
      drawing.beginPath();
      drawing.arc(centreX, centreY, radiusForElevation(elevation, radius), 0, Math.PI * 2);
      drawing.stroke();
    }

    for (let azimuth = 0; azimuth < 360; azimuth += 45) {
      const outer = project(azimuth, 0, centreX, centreY, radius);
      drawing.beginPath();
      drawing.moveTo(centreX, centreY);
      drawing.lineTo(outer.x, outer.y);
      drawing.stroke();
    }

    // Service floor: the dish ignores anything below this, cone or not, so it
    // deserves a stronger line than the decorative rings.
    if (latestSnapshot && latestSnapshot.service_floor_deg > 0) {
      drawing.strokeStyle = theme.labelDim;
      drawing.beginPath();
      drawing.arc(centreX, centreY, radiusForElevation(latestSnapshot.service_floor_deg, radius), 0, Math.PI * 2);
      drawing.stroke();
    }

    drawing.strokeStyle = theme.edge;
    drawing.beginPath();
    drawing.arc(centreX, centreY, radius, 0, Math.PI * 2);
    drawing.stroke();

    // Zenith crosshair.
    drawing.strokeStyle = theme.labelDim;
    drawing.beginPath();
    drawing.moveTo(centreX - 4, centreY);
    drawing.lineTo(centreX + 4, centreY);
    drawing.moveTo(centreX, centreY - 4);
    drawing.lineTo(centreX, centreY + 4);
    drawing.stroke();

    drawing.fillStyle = theme.labelDim;
    drawing.font = `10px ${theme.mono}`;

    drawing.textAlign = "left";
    drawing.textBaseline = "middle";
    for (const elevation of ELEVATION_RINGS) {
      drawing.fillText(`${elevation}°`, centreX + 4, centreY - radiusForElevation(elevation, radius));
    }

    // Centred on the north spoke and sitting above its own ring, so the text
    // never straddles the line it describes.
    if (latestSnapshot && latestSnapshot.service_floor_deg > 0) {
      drawing.textAlign = "center";
      drawing.textBaseline = "bottom";
      drawing.fillText(
        `${latestSnapshot.service_floor_deg}° floor`,
        centreX,
        centreY - radiusForElevation(latestSnapshot.service_floor_deg, radius) - 4
      );
      drawing.textBaseline = "middle";
    }

    drawing.fillStyle = theme.label;
    drawing.font = `600 11px ${theme.mono}`;
    drawing.textAlign = "center";
    for (const cardinal of CARDINAL_POINTS) {
      const point = project(cardinal.azimuth, 0, centreX, centreY, radius + 13);
      drawing.fillText(cardinal.label, point.x, point.y);
    }
  };

  // A cone around a tilted axis isn't a circle in this projection, so walk its
  // rim in spherical coordinates and project each point individually.
  const coneRimPoints = (axisAzimuthDeg, axisElevationDeg, halfAngleDeg, samples = 180) => {
    const axisElevation = axisElevationDeg * TO_RADIANS;
    const halfAngle = halfAngleDeg * TO_RADIANS;
    const points = [];

    for (let step = 0; step <= samples; step++) {
      const bearing = (step / samples) * 2 * Math.PI;
      const elevation = Math.asin(
        Math.sin(axisElevation) * Math.cos(halfAngle) +
        Math.cos(axisElevation) * Math.sin(halfAngle) * Math.cos(bearing)
      );
      const azimuth = axisAzimuthDeg * TO_RADIANS + Math.atan2(
        Math.sin(bearing) * Math.sin(halfAngle) * Math.cos(axisElevation),
        Math.cos(halfAngle) - Math.sin(axisElevation) * Math.sin(elevation)
      );
      points.push({
        az: (((azimuth / TO_RADIANS) % 360) + 360) % 360,
        el: elevation / TO_RADIANS,
      });
    }
    return points;
  };

  const drawCone = (snapshot, centreX, centreY, radius) => {
    const rim = coneRimPoints(
      snapshot.cone_azimuth_deg,
      snapshot.cone_elevation_deg,
      snapshot.cone_half_angle_deg
    );

    drawing.save();
    // Clip to the service floor rather than the horizon. Below it the dish
    // won't use a satellite even when the cone covers it, so shading that
    // band would claim more sky than is actually in play.
    const floorDeg = snapshot.service_floor_deg > 0 ? snapshot.service_floor_deg : 0;
    drawing.beginPath();
    drawing.arc(centreX, centreY, radiusForElevation(floorDeg, radius), 0, Math.PI * 2);
    drawing.clip();

    drawing.beginPath();
    rim.forEach((point, index) => {
      const projected = project(point.az, point.el, centreX, centreY, radius);
      if (index === 0) drawing.moveTo(projected.x, projected.y);
      else drawing.lineTo(projected.x, projected.y);
    });
    drawing.closePath();

    drawing.fillStyle = "rgba(245, 183, 64, 0.05)";
    drawing.fill();

    // Dashed, because the aim is an assumption rather than a measurement.
    drawing.setLineDash([4, 4]);
    drawing.strokeStyle = "rgba(245, 183, 64, 0.45)";
    drawing.lineWidth = 1;
    drawing.stroke();

    // Where we think the dish is actually pointed.
    const axis = project(snapshot.cone_azimuth_deg, snapshot.cone_elevation_deg, centreX, centreY, radius);
    drawing.setLineDash([]);
    drawing.strokeStyle = "rgba(245, 183, 64, 0.55)";
    drawing.beginPath();
    drawing.moveTo(axis.x - 5, axis.y);
    drawing.lineTo(axis.x + 5, axis.y);
    drawing.moveTo(axis.x, axis.y - 5);
    drawing.lineTo(axis.x, axis.y + 5);
    drawing.stroke();

    drawing.restore();
  };

  const drawSatellites = (objects, centreX, centreY, radius) => {
    for (const object of objects) {
      if (object.in_cone) continue;
      const point = project(object.az, object.el, centreX, centreY, radius);
      drawing.beginPath();
      drawing.arc(point.x, point.y, 1.8, 0, Math.PI * 2);
      drawing.fillStyle = theme.aboveHorizon;
      drawing.fill();
    }

    drawing.save();
    drawing.shadowColor = "rgba(245, 183, 64, 0.8)";
    drawing.shadowBlur = 8;
    for (const object of objects) {
      if (!object.in_cone) continue;
      const point = project(object.az, object.el, centreX, centreY, radius);
      drawing.beginPath();
      drawing.arc(point.x, point.y, 3.2, 0, Math.PI * 2);
      drawing.fillStyle = theme.amber;
      drawing.fill();
    }
    drawing.restore();
  };

  const render = () => {
    const size = canvas.clientWidth;
    if (!size) return;

    drawing.clearRect(0, 0, size, size);

    const centreX = size / 2;
    const centreY = size / 2;
    const radius = size / 2 - 22; // room for the cardinal labels

    if (latestSnapshot && latestSnapshot.cone_half_angle_deg > 0) {
      drawCone(latestSnapshot, centreX, centreY, radius);
    }
    drawGraticule(centreX, centreY, radius);
    if (latestSnapshot) {
      drawSatellites(latestSnapshot.objects || [], centreX, centreY, radius);
    }
  };

  // ---- Readouts --------------------------------------------------------

  const formatAge = (ageSeconds) => {
    if (ageSeconds < 0) return "none yet";
    const hours = Math.floor(ageSeconds / 3600);
    if (hours < 1) return `${Math.floor(ageSeconds / 60)}m old`;
    return `${hours}h old`;
  };

  const setStripState = (state) => {
    elements.strip.classList.remove("state-nominal", "state-caution", "state-alarm");
    if (state) elements.strip.classList.add(state);
  };

  const describeCone = (snapshot) => {
    const tiltDeg = Math.round(90 - snapshot.cone_elevation_deg);
    if (snapshot.cone_is_assumed) return `${snapshot.cone_half_angle_deg}° cone, zenith (no boresight yet)`;
    const compass = ["N", "NE", "E", "SE", "S", "SW", "W", "NW"][Math.round(snapshot.cone_azimuth_deg / 45) % 8];
    return `${snapshot.cone_half_angle_deg}° cone, ${tiltDeg}° toward ${compass}`;
  };

  const applySnapshot = (snapshot) => {
    latestSnapshot = snapshot;

    const aboveHorizon = (snapshot.objects || []).length;
    elements.inCone.textContent = snapshot.in_cone_count;
    elements.above.textContent = aboveHorizon;
    elements.clock.textContent = new Date(snapshot.timestamp).toLocaleTimeString();

    elements.coneNote.textContent = describeCone(snapshot);
    elements.coneNote.classList.toggle("is-assumed", Boolean(snapshot.cone_is_assumed));

    elements.elementAge.textContent = formatAge(snapshot.elements_age_s);
    elements.elementAge.classList.toggle("is-alarm", Boolean(snapshot.elements_are_stale));

    if (snapshot.tracked_count === 0) {
      setStripState("state-caution");
      elements.headline.textContent = "NO ELEMENTS LOADED";
      elements.subtext.textContent = "waiting on celestrak";
    } else if (snapshot.in_cone_count === 0) {
      setStripState("state-caution");
      elements.headline.textContent = "NOTHING IN THE CONE";
      elements.subtext.textContent = `${aboveHorizon} above horizon`;
    } else {
      setStripState("state-nominal");
      elements.headline.textContent =
        `${snapshot.in_cone_count} SATELLITE${snapshot.in_cone_count === 1 ? "" : "S"} IN VIEW`;
      elements.subtext.textContent = `${aboveHorizon} above horizon · ${snapshot.tracked_count} tracked`;
    }

    render();
  };

  const setConnection = (state, text) => {
    elements.connection.classList.remove("is-live", "is-lost");
    if (state) elements.connection.classList.add(state);
    elements.connectionText.textContent = text;
  };

  // ---- Stream ----------------------------------------------------------

  const connect = () => {
    const stream = new EventSource(SKY_EVENTS_URL);

    stream.onopen = () => setConnection("is-live", "Live");

    stream.onmessage = (event) => {
      try {
        applySnapshot(JSON.parse(event.data));
      } catch (error) {
        console.error("bad sky snapshot", error);
      }
    };

    // EventSource reconnects on its own; just reflect the state.
    stream.onerror = () => setConnection("is-lost", "Reconnecting…");
  };

  window.addEventListener("resize", () => {
    resizeCanvas();
    render();
  });

  resizeCanvas();
  render();
  connect();
})();
