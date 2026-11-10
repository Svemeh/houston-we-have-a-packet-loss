(() => {
  "use strict";

  const WORLD_URL = "/vendor/world-50m.json";
  const COLOUR_URL = "/vendor/earth-colour.jpg";
  const SERVING_STALE_MS = 45000; // three slots without a result
  const SLOT_SECONDS = 15;
  const SLOT_BOUNDARY_OFFSET = 12; // handovers at :12/:27/:42/:57 UTC
  const SCENE_EARTH_RADIUS_KM = 6371; // the drawn sphere; altitude is scaled against it
  const WGS84_A_KM = 6378.137;
  const WGS84_E2 = 0.00669437999014;
  const LIGHT_KM_PER_MS = 299.792458;
  const TRAIL_SECONDS = 90; // measured track drawn behind the serving satellite
  const EXTRAPOLATE_MAX_S = 2; // snapshots come every second; move dots between them
  const IDLE_FRAME_MS = 100; // about 10 fps unless the view is moving
  const TEXTURE_MAX_PX = 4096;
  const ALTITUDE_SCALE = 4; // satellite heights drawn 4× so they lift off the globe
  const GLOBE_FIT_RADIUS = 1.1; // Earth plus its glow, with a little margin, fits the panel
  const DEG = Math.PI / 180;
  const COMPASS = ["N", "NNE", "NE", "ENE", "E", "ESE", "SE", "SSE", "S", "SSW", "SW", "WSW", "W", "WNW", "NW", "NNW"];
  const STATUS_TEXT = {
    ambiguous: ["NO CLEAR MATCH", "two satellites fit this slot's arc about equally"],
    few_pixels: ["ARC TOO SHORT", "too few map cells this slot to match"],
    no_location: ["NO LOCATION", "set your location in the Sky View card"],
    no_elements: ["NO ELEMENTS", "waiting on satellite elements"],
    unsupported_frame: ["UNSUPPORTED", "this dish's obstruction map can't be matched"],
    error: ["NO ANSWER", "the dish did not answer this slot"],
  };

  const stage = document.getElementById("globe-stage");
  const canvas = document.getElementById("globe-canvas");
  if (!stage || !canvas) return;

  const ui = {
    tag: document.getElementById("globe-tag"),
    centre: document.getElementById("globe-centre"),
    caption: document.getElementById("globe-caption"),
    status: document.getElementById("globe-status"),
    name: document.getElementById("globe-name"),
    sub: document.getElementById("globe-sub"),
    el: document.getElementById("globe-el"),
    az: document.getElementById("globe-az"),
    range: document.getElementById("globe-range"),
    height: document.getElementById("globe-height"),
    fit: document.getElementById("globe-fit"),
    oneWay: document.getElementById("globe-one-way"),
    slot: document.getElementById("globe-slot"),
    next: document.getElementById("globe-next"),
    bar: document.getElementById("globe-bar"),
    lastBlock: document.getElementById("globe-last"),
    lastLabel: document.getElementById("globe-last-label"),
    lastText: document.getElementById("globe-last-text"),
    foot: document.getElementById("globe-foot"),
  };
  const setText = (element, text) => {
    if (element && element.textContent !== text) element.textContent = text;
  };
  const pad = (n) => String(n).padStart(2, "0");

  // ---- Geometry -----------------------------------------------------------
  // The server gives az/el/range from a WGS84 observer; inverting that is exact.

  const geodeticToEcef = (latDeg, lonDeg, heightKm) => {
    const lat = latDeg * DEG, lon = lonDeg * DEG;
    const n = WGS84_A_KM / Math.sqrt(1 - WGS84_E2 * Math.sin(lat) ** 2);
    return [
      (n + heightKm) * Math.cos(lat) * Math.cos(lon),
      (n + heightKm) * Math.cos(lat) * Math.sin(lon),
      (n * (1 - WGS84_E2) + heightKm) * Math.sin(lat),
    ];
  };

  const lookToEcef = (observer, azDeg, elDeg, rangeKm) => {
    const lat = observer.lat * DEG, lon = observer.lon * DEG, az = azDeg * DEG, el = elDeg * DEG;
    const e = rangeKm * Math.cos(el) * Math.sin(az);
    const n = rangeKm * Math.cos(el) * Math.cos(az);
    const u = rangeKm * Math.sin(el);
    const sLat = Math.sin(lat), cLat = Math.cos(lat), sLon = Math.sin(lon), cLon = Math.cos(lon);
    return [
      observer.ecef[0] - sLon * e - sLat * cLon * n + cLat * cLon * u,
      observer.ecef[1] + cLon * e - sLat * sLon * n + cLat * sLon * u,
      observer.ecef[2] + cLat * n + sLat * u,
    ];
  };

  // Bowring's method; well under a metre at LEO heights.
  const ecefToGeodetic = (x, y, z) => {
    const a = WGS84_A_KM, b = a * Math.sqrt(1 - WGS84_E2), ep2 = (a * a - b * b) / (b * b);
    const p = Math.hypot(x, y), theta = Math.atan2(z * a, p * b);
    const lat = Math.atan2(z + ep2 * b * Math.sin(theta) ** 3, p - WGS84_E2 * a * Math.cos(theta) ** 3);
    const n = a / Math.sqrt(1 - WGS84_E2 * Math.sin(lat) ** 2);
    return { lat: lat / DEG, lon: Math.atan2(y, x) / DEG, height: p / Math.cos(lat) - n };
  };

  // ---- Shared state from sky.js ---------------------------------------------

  const shared = (window.houstonSky = window.houstonSky || {});
  let snapshot = null;
  let serving = null;
  let observer = null;
  const tracks = new Map();
  let slotHistory = [];

  const applyLocation = (location) => {
    observer = location && location.location_set
      ? { lat: location.latitude_deg, lon: location.longitude_deg, ecef: geodeticToEcef(location.latitude_deg, location.longitude_deg, 0) }
      : null;
    tracks.clear();
    if (renderer) centreOnDish(false);
    markDirty();
  };

  const applySnapshot = (next) => {
    snapshot = next;
    const now = performance.now();
    const seen = new Set();
    if (observer && next.location_set) {
      for (const object of next.objects || []) {
        const ecef = lookToEcef(observer, object.az, object.el, object.range_km);
        const track = tracks.get(object.name) || { history: [] };
        const dt = (now - (track.at || 0)) / 1000;
        track.velocity = track.ecef && dt > 0.3 && dt < 3
          ? [(ecef[0] - track.ecef[0]) / dt, (ecef[1] - track.ecef[1]) / dt, (ecef[2] - track.ecef[2]) / dt]
          : null;
        track.ecef = ecef;
        track.at = now;
        track.object = object;
        track.history.push([now, ecef[0], ecef[1], ecef[2]]);
        while (track.history.length && now - track.history[0][0] > TRAIL_SECONDS * 1000) track.history.shift();
        tracks.set(object.name, track);
        seen.add(object.name);
      }
    }
    for (const name of tracks.keys()) if (!seen.has(name)) tracks.delete(name);
    if (renderer) rebuildPoints();
    updateReadout();
    markDirty();
  };

  const applyServing = (next) => {
    serving = next;
    const slot = next && next.enabled ? next.slot : null;
    if (slot && (!slotHistory.length || slotHistory[slotHistory.length - 1].slot_start !== slot.slot_start)) {
      slotHistory = [...slotHistory.slice(-1), slot];
    }
    updateReadout();
    markDirty();
  };

  const currentSlot = () => {
    const slot = serving && serving.enabled ? serving.slot : null;
    if (!slot || Date.now() - Date.parse(slot.slot_start) > SERVING_STALE_MS) return null;
    return slot;
  };

  // The beam only ever points at a matched satellite we can also see live.
  const servingTrack = () => {
    const slot = currentSlot();
    if (!slot || slot.status !== "matched" || !slot.best) return null;
    return tracks.get(slot.best.satellite) || null;
  };

  const positionNow = (track, now) => {
    if (!track.velocity) return track.ecef;
    const dt = Math.min((now - track.at) / 1000, EXTRAPOLATE_MAX_S);
    return [track.ecef[0] + track.velocity[0] * dt, track.ecef[1] + track.velocity[1] * dt, track.ecef[2] + track.velocity[2] * dt];
  };

  // ---- Readout --------------------------------------------------------------

  const setStatus = (text, kind) => {
    setText(ui.status, text);
    if (ui.status) ui.status.className = `globe-pill ${kind ? `is-${kind}` : ""}`;
  };

  const clearValues = () => {
    for (const key of ["el", "az", "range", "height", "fit", "oneWay"]) setText(ui[key], "—");
  };

  const slotLabel = (start) => {
    const s = new Date(start).getUTCSeconds();
    return `:${pad(s)} → :${pad((s + SLOT_SECONDS) % 60)}`;
  };

  const slotName = (slot) => (slot.status === "matched" && slot.best ? slot.best.satellite : (STATUS_TEXT[slot.status] || [slot.status])[0].toLowerCase());

  const updateLastHandover = () => {
    const [previous, latest] = slotHistory;
    const consecutive = previous && latest && Date.parse(latest.slot_start) - Date.parse(previous.slot_start) === SLOT_SECONDS * 1000;
    if (ui.lastBlock) ui.lastBlock.hidden = !consecutive || !currentSlot();
    if (!consecutive) return;
    setText(ui.lastLabel, `HANDOVER AT :${pad(new Date(latest.slot_start).getUTCSeconds())}`);
    const same = previous.status === "matched" && latest.status === "matched" && previous.best.satellite === latest.best.satellite;
    setText(ui.lastText, same ? `kept ${latest.best.satellite}` : `${slotName(previous)} → ${slotName(latest)}`);
  };

  const updateClock = () => {
    const into = ((Date.now() / 1000 - SLOT_BOUNDARY_OFFSET) % SLOT_SECONDS + SLOT_SECONDS) % SLOT_SECONDS;
    setText(ui.next, `${Math.ceil(SLOT_SECONDS - into)} s`);
    if (ui.bar) ui.bar.style.width = `${((into / SLOT_SECONDS) * 100).toFixed(1)}%`;
  };

  function updateReadout() {
    const slot = currentSlot();
    const track = servingTrack();
    clearValues();
    setText(ui.slot, slot ? `IDENTIFIED FOR SLOT ${slotLabel(slot.slot_start)}` : "SLOT");

    if (!serving) {
      setStatus("—", "");
      setText(ui.name, "—");
      setText(ui.sub, "waiting for Houston");
    } else if (!serving.enabled) {
      setStatus("TRACKING OFF", "off");
      setText(ui.name, "—");
      setText(ui.sub, "start Houston with -track-satellite to identify it");
    } else if (!slot) {
      setStatus("WAITING", "caution");
      setText(ui.name, "—");
      setText(ui.sub, serving.slot ? "no result for the last three slots" : "waiting for the first slot to finish");
    } else if (slot.status !== "matched" || !slot.best) {
      const [pill, reason] = STATUS_TEXT[slot.status] || [slot.status.toUpperCase(), slot.status];
      setStatus(pill, "caution");
      setText(ui.name, "—");
      setText(ui.sub, reason);
    } else {
      setStatus("● MATCHED", "matched");
      setText(ui.name, slot.best.satellite);
      setText(ui.fit, `${slot.best.fit_deg.toFixed(2)}°`);
      if (track) {
        const object = track.object;
        const height = ecefToGeodetic(...track.ecef).height;
        setText(ui.sub, `NORAD ${slot.best.norad_id}`);
        setText(ui.el, `${Math.round(object.el)}°`);
        setText(ui.az, `${Math.round(object.az) % 360}° ${COMPASS[Math.round(object.az / 22.5) % 16]}`);
        setText(ui.range, `${Math.round(object.range_km)} km`);
        setText(ui.height, `${Math.round(height)} km`);
        setText(ui.oneWay, `${(object.range_km / LIGHT_KM_PER_MS).toFixed(1)} ms`);
      } else {
        setText(ui.sub, `NORAD ${slot.best.norad_id} · not in the live sky right now`);
      }
    }

    updateLastHandover();
    updateClock();
    updateFoot();
  }

  const updateFoot = () => {
    if (!observer) {
      setText(ui.foot, "no location set");
      return;
    }
    const where = `DISH ${Math.abs(observer.lat).toFixed(2)}°${observer.lat >= 0 ? "N" : "S"} ${Math.abs(observer.lon).toFixed(2)}°${observer.lon >= 0 ? "E" : "W"}`;
    if (!snapshot) {
      setText(ui.foot, where);
      return;
    }
    const above = (snapshot.objects || []).length;
    setText(ui.foot, `${where} · ${snapshot.in_cone_count} above ${snapshot.service_floor_deg}° · ${above} above horizon`);
  };

  // ---- Scene ----------------------------------------------------------------

  let renderer = null;
  try {
    if (!window.THREE) throw new Error("three.js did not load");
    renderer = new THREE.WebGLRenderer({ canvas, antialias: true, alpha: true });
  } catch (error) {
    console.warn("globe disabled", error);
    stage.classList.add("is-unavailable");
    setText(ui.caption, "3D VIEW NEEDS WEBGL");
  }

  const dirtyState = { dirty: true };
  function markDirty() {
    dirtyState.dirty = true;
    kick();
  }

  if (!renderer) {
    window.addEventListener("houston:sky", (event) => applySnapshot(event.detail));
    window.addEventListener("houston:serving", (event) => applyServing(event.detail));
    window.addEventListener("houston:location", (event) => applyLocation(event.detail));
    if (shared.location) applyLocation(shared.location);
    if (shared.sky) applySnapshot(shared.sky);
    if (shared.serving) applyServing(shared.serving);
    setInterval(updateClock, 1000);
    return;
  }

  renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 2));
  renderer.setClearColor(0x000000, 0);
  const scene = new THREE.Scene();
  const camera = new THREE.PerspectiveCamera(40, 1, 0.01, 200);
  const amber = new THREE.Color(0xf5b740);
  const up = new THREE.Vector3(0, 1, 0);

  // Same layout as three's SphereGeometry UVs: lon 0 on +x, lon 90 on -z.
  const sceneFromLatLon = (latDeg, lonDeg, radius, out = new THREE.Vector3()) => {
    const lat = latDeg * DEG, lon = lonDeg * DEG;
    return out.set(radius * Math.cos(lat) * Math.cos(lon), radius * Math.sin(lat), -radius * Math.cos(lat) * Math.sin(lon));
  };
  const sceneFromEcef = (ecef, out) => {
    const g = ecefToGeodetic(ecef[0], ecef[1], ecef[2]);
    return sceneFromLatLon(g.lat, g.lon, 1 + (g.height * ALTITUDE_SCALE) / SCENE_EARTH_RADIUS_KM, out);
  };

  // Rough solar position; only drives the day/night shading.
  const sunDirection = (ms) => {
    const date = new Date(ms);
    const day = (ms - Date.UTC(date.getUTCFullYear(), 0, 0)) / 86400000;
    const declination = -23.44 * Math.cos(((2 * Math.PI) / 365) * (day + 10));
    const hours = date.getUTCHours() + date.getUTCMinutes() / 60;
    return sceneFromLatLon(declination, -(hours - 12) * 15, 1).normalize();
  };

  // Equirectangular land/ocean art, drawn once. Land is cut out so the
  // shader fills it with posterised Blue Marble colour.
  const drawMapTexture = (world) => {
    const width = Math.min(TEXTURE_MAX_PX, renderer.capabilities.maxTextureSize);
    const height = width / 2, f = width / 8192;
    const map = document.createElement("canvas");
    map.width = width;
    map.height = height;
    const ctx = map.getContext("2d");
    const x = (lon) => ((lon + 180) / 360) * width;
    const y = (lat) => ((90 - lat) / 180) * height;

    // Longitudes made continuous across ±180; a ring that circles a pole is closed through it.
    const unwrap = (points) => {
      const out = [];
      let shift = 0;
      for (const [lon, lat] of points) {
        if (out.length) {
          const step = lon + shift - out[out.length - 1][0];
          if (step > 180) shift -= 360;
          else if (step < -180) shift += 360;
        }
        out.push([lon + shift, lat]);
      }
      return out;
    };
    const tracePolygons = (multiPolygon) => {
      for (const polygon of multiPolygon.coordinates) {
        for (const ring of polygon) {
          const points = unwrap(ring);
          const winding = points[points.length - 1][0] - points[0][0];
          if (Math.abs(winding) > 180) {
            const pole = points.reduce((sum, p) => sum + p[1], 0) < 0 ? -90 : 90;
            points.push([points[points.length - 1][0], pole], [points[0][0], pole]);
          }
          for (const offset of [-360, 0, 360]) {
            points.forEach(([lon, lat], i) => (i ? ctx.lineTo : ctx.moveTo).call(ctx, x(lon + offset), y(lat)));
            ctx.closePath();
          }
        }
      }
    };
    const traceLines = (multiLine) => {
      for (const line of multiLine.coordinates) {
        const points = unwrap(line);
        for (const offset of [-360, 0, 360]) {
          points.forEach(([lon, lat], i) => (i ? ctx.lineTo : ctx.moveTo).call(ctx, x(lon + offset), y(lat)));
        }
      }
    };

    const countries = world.objects.countries;
    const land = topojson.merge(world, countries.geometries);
    const borders = topojson.mesh(world, countries, (a, b) => a !== b);

    ctx.fillStyle = "#0d58b0";
    ctx.fillRect(0, 0, width, height);
    let seed = 42;
    const random = () => (seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0) / 4294967296;
    ctx.strokeStyle = "rgba(80, 170, 255, 0.45)";
    ctx.lineWidth = 3 * f;
    ctx.lineCap = "round";
    for (let i = 0; i < 2200; i++) {
      const wx = random() * width, wy = height * 0.08 + random() * height * 0.84, s = (14 + random() * 10) * f;
      ctx.beginPath();
      ctx.moveTo(wx - s, wy);
      ctx.quadraticCurveTo(wx - s / 2, wy - s / 2, wx, wy);
      ctx.quadraticCurveTo(wx + s / 2, wy + s / 2, wx + s, wy);
      ctx.stroke();
    }
    ctx.lineJoin = "round";
    for (const [colour, lineWidth] of [["#1572d6", 26], ["#2a8cf0", 12]]) {
      ctx.beginPath();
      tracePolygons(land);
      ctx.strokeStyle = colour;
      ctx.lineWidth = lineWidth * f;
      ctx.stroke();
    }
    ctx.globalCompositeOperation = "destination-out";
    ctx.beginPath();
    tracePolygons(land);
    ctx.fill("evenodd");
    ctx.globalCompositeOperation = "source-over";
    ctx.beginPath();
    traceLines(borders);
    ctx.strokeStyle = "rgba(255, 250, 225, 0.55)";
    ctx.lineWidth = 9 * f;
    ctx.stroke();
    ctx.setLineDash([16 * f, 6 * f]);
    ctx.strokeStyle = "rgba(14, 22, 30, 0.9)";
    ctx.lineWidth = 4 * f;
    ctx.stroke();
    ctx.setLineDash([]);
    ctx.beginPath();
    tracePolygons(land);
    ctx.strokeStyle = "#14202c";
    ctx.lineWidth = 4 * f;
    ctx.stroke();

    const texture = new THREE.CanvasTexture(map);
    texture.anisotropy = renderer.capabilities.getMaxAnisotropy();
    return texture;
  };

  const earthMaterial = new THREE.ShaderMaterial({
    uniforms: {
      mapT: { value: null },
      colourT: { value: null },
      sun: { value: new THREE.Vector3(1, 0, 0) },
      ready: { value: 0 },
    },
    vertexShader:
      "varying vec2 vUv; varying vec3 vN; void main(){ vUv=uv; vN=normalize((modelMatrix*vec4(normal,0.0)).xyz); gl_Position=projectionMatrix*modelViewMatrix*vec4(position,1.0); }",
    fragmentShader:
      "uniform sampler2D mapT; uniform sampler2D colourT; uniform vec3 sun; uniform float ready; varying vec2 vUv; varying vec3 vN;" +
      "void main(){ float d=dot(normalize(vN),sun); float l=0.42+0.33*smoothstep(-0.10,-0.07,d)+0.25*smoothstep(0.22,0.25,d);" +
      " vec4 m=texture2D(mapT,vUv); vec3 p=texture2D(colourT,vUv).rgb; float y=dot(p,vec3(0.299,0.587,0.114)); vec3 h=p/max(y,0.02);" +
      " float q=0.34+floor(y*5.0)/5.0*0.72; vec3 land=clamp(mix(vec3(q),h*q,1.65)*1.15,0.0,1.0); vec3 c=mix(land,m.rgb,m.a);" +
      " c=mix(vec3(0.05,0.35,0.69),c,ready); gl_FragColor=vec4(mix(c*vec3(0.28,0.34,0.62),c,l),1.0); }",
  });
  scene.add(new THREE.Mesh(new THREE.SphereGeometry(1, 128, 96), earthMaterial));
  scene.add(new THREE.Mesh(new THREE.SphereGeometry(1.012, 96, 64), new THREE.MeshBasicMaterial({ color: 0x0b131c, side: THREE.BackSide })));
  scene.add(new THREE.Mesh(new THREE.SphereGeometry(1.035, 96, 64), new THREE.ShaderMaterial({
    side: THREE.BackSide,
    blending: THREE.AdditiveBlending,
    transparent: true,
    depthWrite: false,
    vertexShader: "varying vec3 vN; void main(){ vN=normalize(normalMatrix*normal); gl_Position=projectionMatrix*modelViewMatrix*vec4(position,1.0); }",
    fragmentShader: "varying vec3 vN; void main(){ float i=pow(max(0.0,0.66+dot(vN,vec3(0.0,0.0,1.0))),5.0); gl_FragColor=vec4(0.32,0.58,1.0,1.0)*i*1.4; }",
  })));

  const coastAndBorders = (world) => {
    const group = new THREE.Group();
    for (const [filter, opacity] of [[(a, b) => a === b, 0.9], [(a, b) => a !== b, 0.75]]) {
      const points = [];
      for (const line of topojson.mesh(world, world.objects.countries, filter).coordinates) {
        for (let i = 1; i < line.length; i++) {
          points.push(sceneFromLatLon(line[i - 1][1], line[i - 1][0], 1.0006), sceneFromLatLon(line[i][1], line[i][0], 1.0006));
        }
      }
      group.add(new THREE.LineSegments(
        new THREE.BufferGeometry().setFromPoints(points),
        new THREE.LineBasicMaterial({ color: 0x0e161e, transparent: true, opacity }),
      ));
    }
    return group;
  };

  Promise.all([
    fetch(WORLD_URL).then((response) => response.json()),
    new Promise((resolve, reject) => new THREE.TextureLoader().load(COLOUR_URL, resolve, undefined, reject)),
  ]).then(([world, colour]) => {
    earthMaterial.uniforms.mapT.value = drawMapTexture(world);
    earthMaterial.uniforms.colourT.value = colour;
    earthMaterial.uniforms.ready.value = 1;
    scene.add(coastAndBorders(world));
    markDirty();
  }).catch((error) => console.error("could not load the globe map", error));

  const dotTexture = (fill, stroke) => {
    const dot = document.createElement("canvas");
    dot.width = dot.height = 32;
    const ctx = dot.getContext("2d");
    ctx.beginPath();
    ctx.arc(16, 16, 11, 0, 2 * Math.PI);
    ctx.fillStyle = fill;
    ctx.fill();
    ctx.lineWidth = 5;
    ctx.strokeStyle = stroke;
    ctx.stroke();
    return new THREE.CanvasTexture(dot);
  };
  const makeCloud = (fill, stroke, size) => {
    const geometry = new THREE.BufferGeometry();
    geometry.setAttribute("position", new THREE.BufferAttribute(new Float32Array(0), 3));
    const points = new THREE.Points(geometry, new THREE.PointsMaterial({ map: dotTexture(fill, stroke), size, sizeAttenuation: false, alphaTest: 0.5 }));
    points.frustumCulled = false;
    scene.add(points);
    return { points, names: [] };
  };
  const dimCloud = makeCloud("#4a6076", "#101820", 5);
  const usableCloud = makeCloud("#f5b740", "#14202c", 8);

  function rebuildPoints() {
    dimCloud.names = [];
    usableCloud.names = [];
    for (const [name, track] of tracks) (track.object.in_cone ? usableCloud : dimCloud).names.push(name);
    for (const cloud of [dimCloud, usableCloud]) {
      cloud.points.geometry.setAttribute("position", new THREE.BufferAttribute(new Float32Array(cloud.names.length * 3), 3));
    }
  }

  const dish = new THREE.Points(
    new THREE.BufferGeometry().setAttribute("position", new THREE.BufferAttribute(new Float32Array(3), 3)),
    new THREE.PointsMaterial({ map: dotTexture("#ffffff", "#14202c"), size: 10, sizeAttenuation: false, alphaTest: 0.5 }),
  );
  scene.add(dish);
  const footprint = new THREE.Line(new THREE.BufferGeometry(), new THREE.LineBasicMaterial({ color: amber, transparent: true, opacity: 0.55 }));
  scene.add(footprint);
  let footprintKey = "";

  // Ground circle under which a satellite at heightKm sits above the floor.
  const updateFootprint = () => {
    const objects = snapshot ? (snapshot.objects || []).filter((o) => o.in_cone) : [];
    if (!observer || !objects.length) {
      footprint.visible = false;
      return null;
    }
    const heights = objects.map((o) => ecefToGeodetic(...lookToEcef(observer, o.az, o.el, o.range_km)).height).sort((a, b) => a - b);
    const heightKm = Math.round(heights[Math.floor(heights.length / 2)] / 10) * 10;
    const floor = snapshot.service_floor_deg;
    const key = `${observer.lat},${observer.lon},${heightKm},${floor}`;
    footprint.visible = true;
    if (key !== footprintKey) {
      footprintKey = key;
      const r = SCENE_EARTH_RADIUS_KM;
      const psi = Math.acos((r / (r + heightKm)) * Math.cos(floor * DEG)) - floor * DEG;
      const lat = observer.lat * DEG, lon = observer.lon * DEG, ring = [];
      for (let i = 0; i <= 96; i++) {
        const bearing = (2 * Math.PI * i) / 96;
        const lat2 = Math.asin(Math.sin(lat) * Math.cos(psi) + Math.cos(lat) * Math.sin(psi) * Math.cos(bearing));
        const lon2 = lon + Math.atan2(Math.sin(bearing) * Math.sin(psi) * Math.cos(lat), Math.cos(psi) - Math.sin(lat) * Math.sin(lat2));
        ring.push(sceneFromLatLon(lat2 / DEG, lon2 / DEG, 1.002));
      }
      footprint.geometry.dispose();
      footprint.geometry = new THREE.BufferGeometry().setFromPoints(ring);
    }
    return { heightKm, floor };
  };

  const beamCore = new THREE.Mesh(new THREE.CylinderGeometry(0.0035, 0.0035, 1, 12, 1, true),
    new THREE.MeshBasicMaterial({ color: amber, transparent: true, opacity: 0.95, depthWrite: false }));
  const beamGlow = new THREE.Mesh(new THREE.CylinderGeometry(0.014, 0.014, 1, 16, 1, true),
    new THREE.MeshBasicMaterial({ color: amber, transparent: true, opacity: 0.16, blending: THREE.AdditiveBlending, depthWrite: false }));
  const pulse = new THREE.Mesh(new THREE.SphereGeometry(0.009, 12, 8), new THREE.MeshBasicMaterial({ color: 0xfff1cc }));
  const servingHalo = new THREE.Mesh(new THREE.SphereGeometry(0.03, 16, 12),
    new THREE.MeshBasicMaterial({ color: amber, transparent: true, opacity: 0.18, blending: THREE.AdditiveBlending, depthWrite: false }));
  const servingIcon = (() => {
    const icon = document.createElement("canvas");
    icon.width = 192;
    icon.height = 96;
    const g = icon.getContext("2d");
    g.lineWidth = 6;
    g.strokeStyle = "#14202c";
    g.lineJoin = "round";
    g.beginPath();
    g.moveTo(36, 48);
    g.lineTo(156, 48);
    g.stroke();
    for (const [px, py] of [[8, 30], [124, 30]]) {
      g.fillStyle = "#3d6f9e";
      g.fillRect(px, py, 60, 36);
      g.strokeRect(px, py, 60, 36);
      g.lineWidth = 2.5;
      g.beginPath();
      g.moveTo(px + 20, py);
      g.lineTo(px + 20, py + 36);
      g.moveTo(px + 40, py);
      g.lineTo(px + 40, py + 36);
      g.stroke();
      g.lineWidth = 6;
    }
    g.fillStyle = "#f5b740";
    g.fillRect(76, 28, 40, 40);
    g.strokeRect(76, 28, 40, 40);
    return new THREE.Sprite(new THREE.SpriteMaterial({ map: new THREE.CanvasTexture(icon) }));
  })();
  const trail = new THREE.Line(new THREE.BufferGeometry(), new THREE.LineBasicMaterial({ color: amber, transparent: true, opacity: 0.35 }));
  trail.frustumCulled = false;
  const servingParts = [beamCore, beamGlow, pulse, servingHalo, servingIcon, trail];
  for (const part of servingParts) scene.add(part);

  // ---- Camera and input -----------------------------------------------------

  let view = { lat: 20, lon: 10, zoom: 1 };
  let target = null;
  const reduceMotion = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  // Seen from the equator side, so the beam and satellite heights show against space.
  function centreOnDish(animate) {
    if (!observer) return;
    const next = { lat: Math.max(-80, Math.min(80, observer.lat - 22 * Math.sign(observer.lat || 1))), lon: observer.lon + 2, zoom: 1 };
    if (animate && !reduceMotion) target = next;
    else {
      view = next;
      target = null;
    }
    markDirty();
  }


  // Keep the camera outside the satellite shell, or the satellites end up behind it.
  const minDistance = () => 1.25 + (600 * ALTITUDE_SCALE) / SCENE_EARTH_RADIUS_KM;

  // Distance at which the whole globe fits the narrower side of the panel; zoom 1 is that.
  const fitDistance = () => {
    const half = (camera.fov / 2) * DEG;
    return GLOBE_FIT_RADIUS / Math.sin(Math.min(half, Math.atan(Math.tan(half) * camera.aspect)));
  };
  const cameraDistance = () => Math.max(minDistance(), fitDistance() * view.zoom);
  const clampZoom = (zoom) => Math.min(2, Math.max(minDistance() / fitDistance(), zoom));

  const placeCamera = () => {
    sceneFromLatLon(view.lat, view.lon, cameraDistance(), camera.position);
    camera.lookAt(0, 0, 0);
  };

  const pointers = new Map();
  let lastPinch = 0;
  canvas.addEventListener("pointerdown", (event) => {
    canvas.setPointerCapture(event.pointerId);
    pointers.set(event.pointerId, { x: event.clientX, y: event.clientY });
    target = null;
  });
  canvas.addEventListener("pointermove", (event) => {
    const previous = pointers.get(event.pointerId);
    if (!previous) return;
    pointers.set(event.pointerId, { x: event.clientX, y: event.clientY });
    if (pointers.size === 2) {
      const [a, b] = [...pointers.values()];
      const distance = Math.hypot(a.x - b.x, a.y - b.y);
      if (lastPinch) view.zoom = clampZoom((view.zoom * lastPinch) / distance);
      lastPinch = distance;
    } else {
      const k = (0.25 * (cameraDistance() - 1)) / 2;
      view.lon -= (event.clientX - previous.x) * k;
      view.lat = Math.max(-80, Math.min(80, view.lat + (event.clientY - previous.y) * k));
    }
    markDirty();
  });
  const release = (event) => {
    pointers.delete(event.pointerId);
    lastPinch = 0;
  };
  canvas.addEventListener("pointerup", release);
  canvas.addEventListener("pointercancel", release);
  // Plain scrolling keeps scrolling the page; ctrl/⌘ + scroll (and trackpad pinch) zooms.
  canvas.addEventListener("wheel", (event) => {
    if (!event.ctrlKey && !event.metaKey) return;
    event.preventDefault();
    view.zoom = clampZoom(view.zoom * Math.exp(event.deltaY * 0.003));
    target = null;
    markDirty();
  }, { passive: false });

  if (ui.centre) ui.centre.addEventListener("click", () => centreOnDish(true));

  // ---- Frame ----------------------------------------------------------------

  const scratch = new THREE.Vector3();
  const direction = new THREE.Vector3();

  const updateClouds = (now) => {
    for (const cloud of [dimCloud, usableCloud]) {
      const positions = cloud.points.geometry.attributes.position;
      cloud.names.forEach((name, i) => {
        const track = tracks.get(name);
        if (!track) return;
        sceneFromEcef(positionNow(track, now), scratch);
        positions.setXYZ(i, scratch.x, scratch.y, scratch.z);
      });
      positions.needsUpdate = true;
    }
  };

  const updateServing = (now) => {
    const track = servingTrack();
    for (const part of servingParts) part.visible = Boolean(track && observer);
    if (!track || !observer) {
      ui.tag.hidden = true;
      return;
    }
    const satellite = sceneFromEcef(positionNow(track, now), new THREE.Vector3());
    servingIcon.position.copy(satellite);
    servingHalo.position.copy(satellite);
    const cameraDistance = camera.position.distanceTo(satellite);
    servingIcon.scale.set(0.05 * cameraDistance, 0.025 * cameraDistance, 1);

    direction.subVectors(satellite, dish.position);
    const length = direction.length();
    direction.normalize();
    for (const beam of [beamCore, beamGlow]) {
      beam.position.copy(dish.position).addScaledVector(direction, length / 2);
      beam.quaternion.setFromUnitVectors(up, direction);
      beam.scale.set(1, length, 1);
    }
    pulse.position.copy(dish.position).addScaledVector(direction, length * (reduceMotion ? 0.5 : (Date.now() % 1100) / 1100));

    const trailPoints = track.history.map(([, x, y, z]) => sceneFromEcef([x, y, z], new THREE.Vector3()));
    trailPoints.push(satellite.clone());
    trail.geometry.dispose();
    trail.geometry = new THREE.BufferGeometry().setFromPoints(trailPoints);

    const toSatellite = satellite.clone().sub(camera.position);
    const distance = toSatellite.length();
    toSatellite.normalize();
    const b = camera.position.dot(toSatellite), c = camera.position.lengthSq() - 1, disc = b * b - c;
    const hidden = disc > 0 && -b - Math.sqrt(disc) < distance;
    const projected = satellite.clone().project(camera);
    const rect = stage.getBoundingClientRect();
    ui.tag.hidden = hidden || projected.z > 1;
    ui.tag.style.left = `${((projected.x + 1) / 2) * rect.width}px`;
    ui.tag.style.top = `${((1 - projected.y) / 2) * rect.height}px`;
    setText(ui.tag, track.object.name);
  };

  const draw = (now) => {
    earthMaterial.uniforms.sun.value.copy(sunDirection(Date.now()));
    if (observer) sceneFromLatLon(observer.lat, observer.lon, 1.0015, dish.position);
    dish.visible = Boolean(observer);
    if (target) {
      view.lat += (target.lat - view.lat) * 0.08;
      view.lon += (target.lon - view.lon) * 0.08;
      view.zoom += (target.zoom - view.zoom) * 0.08;
      if (Math.abs(target.zoom - view.zoom) < 0.002 && Math.abs(target.lat - view.lat) < 0.05) target = null;
    }
    placeCamera();
    updateClouds(now);
    updateServing(now);
    const ring = updateFootprint();
    setText(ui.caption, `ALTITUDE ×${ALTITUDE_SCALE} FOR VISIBILITY${ring ? ` · RING = ${ring.floor}° FLOOR AT ${ring.heightKm} KM` : ""}`);
    renderer.render(scene, camera);
  };

  const resize = () => {
    const rect = stage.getBoundingClientRect();
    renderer.setSize(rect.width, rect.height, false);
    camera.aspect = rect.width / Math.max(1, rect.height);
    camera.updateProjectionMatrix();
    markDirty();
  };

  let onScreen = true;
  let frameRequest = 0;
  let lastFrame = 0;
  const loop = (now) => {
    frameRequest = 0;
    if (!onScreen || document.hidden) return;
    if (dirtyState.dirty || target || pointers.size || now - lastFrame >= IDLE_FRAME_MS) {
      dirtyState.dirty = false;
      lastFrame = now;
      draw(now);
    }
    frameRequest = requestAnimationFrame(loop);
  };
  function kick() {
    if (renderer && !frameRequest) frameRequest = requestAnimationFrame(loop);
  }

  new ResizeObserver(resize).observe(stage);
  new IntersectionObserver((entries) => {
    onScreen = entries[entries.length - 1].isIntersecting;
    kick();
  }).observe(stage);
  document.addEventListener("visibilitychange", kick);

  window.addEventListener("houston:sky", (event) => applySnapshot(event.detail));
  window.addEventListener("houston:serving", (event) => applyServing(event.detail));
  window.addEventListener("houston:location", (event) => {
    const moved = !observer || !event.detail.location_set
      || observer.lat !== event.detail.latitude_deg || observer.lon !== event.detail.longitude_deg;
    if (moved) applyLocation(event.detail);
  });
  if (shared.location) applyLocation(shared.location);
  if (shared.sky) applySnapshot(shared.sky);
  if (shared.serving) applyServing(shared.serving);
  setInterval(updateClock, 1000);
  resize();
  placeCamera();
  updateReadout();
})();
