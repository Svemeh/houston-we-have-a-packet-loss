(() => {
  "use strict";

  const SKY_EVENTS_URL = "/skyevents";
  const LOCATION_URL = "/location";
  const SERVING_URL = "/serving";
  const SERVING_POLL_MS = 5000;
  const SERVING_STALE_MS = 45000; // three slots without a result
  const SLOT_SECONDS = 15;
  const SLOT_BOUNDARY_OFFSET = 12; // handovers at :12/:27/:42/:57 UTC
  const SERVING_STATUS_TEXT = {
    ambiguous: "no clear match this slot",
    few_pixels: "arc too short this slot",
    no_location: "set your location",
    no_elements: "waiting on satellite elements",
    unsupported_frame: "not supported on this dish",
    error: "dish did not answer",
  };
  const TO_RADIANS = Math.PI / 180;

  // Zenith, the 25° service floor and the horizon mean something on their own;
  // 15° and 30° rings would crowd the floor label.
  const ELEVATION_RINGS = [45, 60];
  const CARDINAL_POINTS = [
    { label: "N", azimuth: 0 },
    { label: "E", azimuth: 90 },
    { label: "S", azimuth: 180 },
    { label: "W", azimuth: 270 },
  ];

  const canvas = document.getElementById("sky-scope");
  if (!canvas) return;
  const drawing = canvas.getContext("2d");

  const elements = {
    strip: document.getElementById("sky-strip"),
    headline: document.getElementById("sky-headline"),
    subtext: document.getElementById("sky-subtext"),
    inCone: document.getElementById("count-in-cone"),
    above: document.getElementById("count-above"),
    coneNote: document.getElementById("cone-note"),
    locationPanel: document.getElementById("location-panel"),
    useMyLocation: document.getElementById("use-my-location"),
    geolocationHint: document.getElementById("geolocation-hint"),
    locationForm: document.getElementById("location-form"),
    latitudeInput: document.getElementById("latitude-input"),
    longitudeInput: document.getElementById("longitude-input"),
    locationStatus: document.getElementById("location-status"),
    changeLocation: document.getElementById("change-location"),
    servingLine: document.getElementById("serving-line"),
    servingName: document.getElementById("serving-name"),
    servingDetail: document.getElementById("serving-detail"),
  };

  const setText = (element, text) => {
    if (element) element.textContent = text;
  };

  let latestSnapshot = null;
  let latestServing = null;

  // The globe panel reads the same data instead of opening its own stream.
  const shared = (window.houstonSky = window.houstonSky || {});
  const publish = (kind, detail) => {
    shared[kind] = detail;
    window.dispatchEvent(new CustomEvent(`houston:${kind}`, { detail }));
  };

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
        `${latestSnapshot.service_floor_deg}° floor${latestSnapshot.service_floor_measured ? " (measured)" : ""}`,
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

  // The sky the dish actually serves from: everything above the service floor,
  // measured from which satellites it used. The cross marks where the panel faces.
  const drawFieldOfView = (snapshot, centreX, centreY, radius) => {
    drawing.save();
    drawing.beginPath();
    drawing.arc(centreX, centreY, radiusForElevation(snapshot.service_floor_deg, radius), 0, Math.PI * 2);
    drawing.fillStyle = "rgba(245, 183, 64, 0.05)";
    drawing.fill();
    drawing.strokeStyle = "rgba(245, 183, 64, 0.45)";
    drawing.lineWidth = 1;
    drawing.stroke();

    if (!snapshot.cone_is_assumed) {
      const aim = project(snapshot.cone_azimuth_deg, snapshot.cone_elevation_deg, centreX, centreY, radius);
      drawing.strokeStyle = "rgba(245, 183, 64, 0.55)";
      drawing.beginPath();
      drawing.moveTo(aim.x - 5, aim.y);
      drawing.lineTo(aim.x + 5, aim.y);
      drawing.moveTo(aim.x, aim.y - 5);
      drawing.lineTo(aim.x, aim.y + 5);
      drawing.stroke();
    }
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

  const servingSlot = () => {
    const slot = latestServing && latestServing.enabled ? latestServing.slot : null;
    if (!slot || Date.now() - Date.parse(slot.slot_start) > SERVING_STALE_MS) return null;
    return slot;
  };

  const drawServing = (objects, centreX, centreY, radius) => {
    const slot = servingSlot();
    if (!slot || slot.status !== "matched") return;
    const object = objects.find((candidate) => candidate.name === slot.best.satellite);
    if (!object) return;
    const point = project(object.az, object.el, centreX, centreY, radius);
    drawing.save();
    drawing.strokeStyle = theme.amber;
    drawing.globalAlpha = 0.8;
    drawing.lineWidth = 1.5;
    drawing.beginPath();
    drawing.moveTo(centreX, centreY);
    drawing.lineTo(point.x, point.y);
    drawing.stroke();
    drawing.globalAlpha = 1;
    drawing.beginPath();
    drawing.arc(point.x, point.y, 7, 0, Math.PI * 2);
    drawing.stroke();
    drawing.restore();
  };

  const render = () => {
    const size = canvas.clientWidth;
    if (!size) return;

    drawing.clearRect(0, 0, size, size);

    const centreX = size / 2;
    const centreY = size / 2;
    const radius = size / 2 - 22; // room for the cardinal labels

    if (latestSnapshot && latestSnapshot.service_floor_deg > 0) {
      drawFieldOfView(latestSnapshot, centreX, centreY, radius);
    }
    drawGraticule(centreX, centreY, radius);
    if (latestSnapshot) {
      drawSatellites(latestSnapshot.objects || [], centreX, centreY, radius);
      drawServing(latestSnapshot.objects || [], centreX, centreY, radius);
    }
  };

  // ---- Readouts --------------------------------------------------------

  const setStripState = (state) => {
    if (!elements.strip) return;
    elements.strip.classList.remove("state-nominal", "state-caution", "state-alarm");
    if (state) elements.strip.classList.add(state);
  };

  const describeCone = (snapshot) => {
    const usable = snapshot.service_floor_measured
      ? `dish serves from above ${snapshot.service_floor_deg}° (measured)`
      : `dish uses the sky above ${snapshot.service_floor_deg}°`;
    if (snapshot.cone_is_assumed) return `${usable} · panel aim not reported yet`;
    const tiltDeg = Math.round(90 - snapshot.cone_elevation_deg);
    const compass = ["N", "NE", "E", "SE", "S", "SW", "W", "NW"][Math.round(snapshot.cone_azimuth_deg / 45) % 8];
    return `${usable} · panel tilted ${tiltDeg}° toward ${compass}`;
  };


  const applySnapshot = (snapshot) => {
    // Set from another tab, or cleared by a restart: ask the server again.
    if (latestSnapshot && latestSnapshot.location_set !== snapshot.location_set) loadLocation();
    latestSnapshot = snapshot;
    publish("sky", snapshot);

    const aboveHorizon = (snapshot.objects || []).length;
    setText(elements.inCone, snapshot.in_cone_count);
    setText(elements.above, aboveHorizon);

    setText(elements.coneNote, describeCone(snapshot));
    if (elements.coneNote) elements.coneNote.classList.toggle("is-assumed", Boolean(snapshot.cone_is_assumed));

    if (!snapshot.location_set) {
      setStripState("state-caution");
      setText(elements.headline, "NO LOCATION SET");
      setText(elements.subtext, "set it below");
    } else if (snapshot.tracked_count === 0) {
      setStripState("state-caution");
      setText(elements.headline, "NO ELEMENTS LOADED");
      setText(elements.subtext, "waiting on celestrak");
    } else if (snapshot.in_cone_count === 0) {
      setStripState("state-caution");
      setText(elements.headline, `NOTHING ABOVE ${snapshot.service_floor_deg}°`);
      setText(elements.subtext, `${aboveHorizon} above horizon`);
    } else {
      setStripState("state-nominal");
      setText(elements.headline,
        `${snapshot.in_cone_count} SATELLITE${snapshot.in_cone_count === 1 ? "" : "S"} IN VIEW`);
      setText(elements.subtext, `${aboveHorizon} above horizon · ${snapshot.tracked_count} tracked`);
    }

    render();
  };

  // ---- Location --------------------------------------------------------
  // Until the server knows where the dish is, the sky card asks. The browser's
  // own location only works on a secure page (http://localhost counts; a LAN
  // address like http://192.168.1.20:2101 doesn't), so typing it in is always
  // offered too.

  let locationSource = "";

  const setLocationStatus = (text, isError = false) => {
    setText(elements.locationStatus, text);
    if (elements.locationStatus) elements.locationStatus.classList.toggle("is-error", isError);
  };

  const showLocationPanel = (visible) => {
    if (!elements.locationPanel) return;
    elements.locationPanel.hidden = !visible;
    // Only a location saved from here can be changed from here.
    if (elements.changeLocation) {
      elements.changeLocation.hidden = locationSource !== "settings";
      elements.changeLocation.textContent = visible ? "Cancel" : "Change location";
    }
  };

  const applyLocation = (location) => {
    locationSource = location.source || "";
    publish("location", location);
    if (location.location_set && elements.latitudeInput && elements.longitudeInput) {
      elements.latitudeInput.value = location.latitude_deg;
      elements.longitudeInput.value = location.longitude_deg;
    }
    showLocationPanel(!location.location_set);
  };

  const loadLocation = async () => {
    if (!elements.locationPanel) return;
    try {
      const response = await fetch(LOCATION_URL, { cache: "no-store" });
      if (response.ok) applyLocation(await response.json());
    } catch (error) {
      console.error("could not read location", error);
    }
  };

  const saveLocation = async (latitude, longitude) => {
    setLocationStatus("Saving…");
    try {
      const response = await fetch(LOCATION_URL, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ latitude_deg: latitude, longitude_deg: longitude }),
      });
      if (!response.ok) {
        setLocationStatus((await response.text()).trim() || `Failed (${response.status})`, true);
        return;
      }
      applyLocation(await response.json());
      setLocationStatus("");
    } catch (error) {
      setLocationStatus("Could not reach Houston", true);
    }
  };

  const roundCoordinate = (value) => Math.round(value * 100) / 100;

  const setUpLocationPanel = () => {
    if (!elements.locationPanel) return;

    if (!navigator.geolocation || !window.isSecureContext) {
      elements.useMyLocation.disabled = true;
      elements.geolocationHint.hidden = false;
      elements.geolocationHint.textContent =
        `Browser location only works on http://localhost:${window.location.port || 80}. Type it in below instead.`;
    } else {
      elements.useMyLocation.addEventListener("click", () => {
        setLocationStatus("Asking the browser…");
        navigator.geolocation.getCurrentPosition(
          (position) => {
            const latitude = roundCoordinate(position.coords.latitude);
            const longitude = roundCoordinate(position.coords.longitude);
            elements.latitudeInput.value = latitude;
            elements.longitudeInput.value = longitude;
            saveLocation(latitude, longitude);
          },
          (error) => {
            const reason = error.code === error.PERMISSION_DENIED ? "Location permission denied" : "Browser could not find a location";
            setLocationStatus(`${reason}. Type it in below instead.`, true);
          },
          { enableHighAccuracy: false, timeout: 15000, maximumAge: 600000 },
        );
      });
    }

    elements.locationForm.addEventListener("submit", (event) => {
      event.preventDefault();
      saveLocation(Number(elements.latitudeInput.value), Number(elements.longitudeInput.value));
    });

    if (elements.changeLocation) {
      elements.changeLocation.addEventListener("click", () => showLocationPanel(elements.locationPanel.hidden));
    }
  };

  // ---- Serving satellite ------------------------------------------------

  const secondsToHandover = () => {
    const into = ((Date.now() / 1000 - SLOT_BOUNDARY_OFFSET) % SLOT_SECONDS + SLOT_SECONDS) % SLOT_SECONDS;
    return Math.ceil(SLOT_SECONDS - into);
  };

  const updateServingReadout = () => {
    if (!elements.servingLine) return;
    const enabled = Boolean(latestServing && latestServing.enabled);
    elements.servingLine.hidden = !enabled;
    if (!enabled) return;

    const slot = servingSlot();
    const handover = `handover in ${secondsToHandover()} s`;
    if (slot && slot.status === "matched") {
      const live = ((latestSnapshot && latestSnapshot.objects) || []).find((object) => object.name === slot.best.satellite);
      const position = live || slot.best;
      setText(elements.servingName, slot.best.satellite);
      setText(elements.servingDetail, `el ${Math.round(position.el)}° · az ${Math.round(position.az)}° · ${handover}`);
    } else {
      setText(elements.servingName, "—");
      const reason = slot ? SERVING_STATUS_TEXT[slot.status] || slot.status : "waiting for the first slot";
      setText(elements.servingDetail, `${reason} · ${handover}`);
    }
  };

  const loadServing = async () => {
    try {
      const response = await fetch(SERVING_URL, { cache: "no-store" });
      if (!response.ok) return;
      latestServing = await response.json();
      publish("serving", latestServing);
      render();
    } catch (error) {
      console.warn("serving poll failed", error);
    }
  };

  // ---- Stream ----------------------------------------------------------

  const connect = () => {
    const stream = new EventSource(SKY_EVENTS_URL);

    stream.onmessage = (event) => {
      try {
        applySnapshot(JSON.parse(event.data));
      } catch (error) {
        console.error("bad sky snapshot", error);
      }
    };
  };

  window.addEventListener("resize", () => {
    resizeCanvas();
    render();
  });

  resizeCanvas();
  render();
  setUpLocationPanel();
  loadLocation();
  connect();
  loadServing();
  setInterval(loadServing, SERVING_POLL_MS);
  setInterval(updateServingReadout, 1000);
})();
