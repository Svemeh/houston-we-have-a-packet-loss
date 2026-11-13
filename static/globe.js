(() => {
  "use strict";

  const WORLD_URL = "/vendor/world-50m.json";
  const DAY_URL = "/vendor/earth-day.jpg";
  const NIGHT_URL = "/vendor/earth-night.jpg";
  const CONSTELLATION_URL = "/constellation";
  const CONSTELLATION_REFRESH_MS = 60000; // fresh SGP4 states every minute; circular orbits in between are off by well under 1 km
  const SCENE_EARTH_RADIUS_KM = 6371; // the drawn sphere; altitude is scaled against it
  const EARTH_ROTATION_RAD_S = 7.2921158553e-5;
  const IDLE_FRAME_MS = 100; // about 10 fps unless the view is moving
  const POSITION_UPDATE_MS = 100; // satellites move a few pixels a second; no need to recompute every frame
  const PICK_RADIUS_PX = 12; // how close a click has to land to a satellite
  const ORBIT_POINTS = 256;
  const GLOBE_FIT_RADIUS = 1.1; // Earth plus its glow, with a little margin, fits the panel
  const DEG = Math.PI / 180;

  const stage = document.getElementById("globe-stage");
  const canvas = document.getElementById("globe-canvas");
  if (!stage || !canvas) return;

  const ui = {
    tag: document.getElementById("globe-tag"),
    pick: document.getElementById("globe-pick"),
    pickName: document.getElementById("globe-pick-name"),
    pickDetail: document.getElementById("globe-pick-detail"),
    pickClose: document.getElementById("globe-pick-close"),
    pickTag: document.getElementById("globe-pick-tag"),
    centre: document.getElementById("globe-centre"),
    caption: document.getElementById("globe-caption"),
  };
  const setText = (element, text) => {
    if (element && element.textContent !== text) element.textContent = text;
  };

  const shared = (window.houstonSky = window.houstonSky || {});
  const { ecefToGeodetic } = shared.geo;
  let observer = null;

  const applyLocation = (location) => {
    observer = location && location.location_set ? { lat: location.latitude_deg, lon: location.longitude_deg } : null;
    centreOnDish(false);
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

  if (!renderer) return;

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
  const sceneFromEcef = (x, y, z, out) => {
    const g = ecefToGeodetic(x, y, z);
    return sceneFromLatLon(g.lat, g.lon, 1 + g.height / SCENE_EARTH_RADIUS_KM, out);
  };

  // Rough solar position; only drives the day/night shading.
  const sunDirection = (ms) => {
    const date = new Date(ms);
    const day = (ms - Date.UTC(date.getUTCFullYear(), 0, 0)) / 86400000;
    const declination = -23.44 * Math.cos(((2 * Math.PI) / 365) * (day + 10));
    const hours = date.getUTCHours() + date.getUTCMinutes() / 60;
    return sceneFromLatLon(declination, -(hours - 12) * 15, 1).normalize();
  };

  // NASA Blue Marble by day, Black Marble city lights by night, soft terminator.
  const earthMaterial = new THREE.ShaderMaterial({
    uniforms: {
      dayT: { value: null },
      nightT: { value: null },
      sun: { value: new THREE.Vector3(1, 0, 0) },
      ready: { value: 0 },
    },
    vertexShader:
      "varying vec2 vUv; varying vec3 vN; void main(){ vUv=uv; vN=normalize((modelMatrix*vec4(normal,0.0)).xyz); gl_Position=projectionMatrix*modelViewMatrix*vec4(position,1.0); }",
    fragmentShader:
      "uniform sampler2D dayT; uniform sampler2D nightT; uniform vec3 sun; uniform float ready; varying vec2 vUv; varying vec3 vN;" +
      "void main(){ float d=dot(normalize(vN),sun); float k=smoothstep(-0.12,0.16,d);" +
      " vec3 day=texture2D(dayT,vUv).rgb; vec3 night=texture2D(nightT,vUv).rgb;" +
      " vec3 lit=day*(0.35+1.0*max(d,0.0)); vec3 dark=day*0.12+night*vec3(1.0,0.82,0.55)*1.3;" +
      " vec3 c=mix(vec3(0.03,0.08,0.16),mix(dark,lit,k),ready); gl_FragColor=vec4(c,1.0); }",
  });
  scene.add(new THREE.Mesh(new THREE.SphereGeometry(1, 128, 96), earthMaterial));
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
    for (const [filter, opacity] of [[(a, b) => a === b, 0.28], [(a, b) => a !== b, 0.2]]) {
      const points = [];
      for (const line of topojson.mesh(world, world.objects.countries, filter).coordinates) {
        for (let i = 1; i < line.length; i++) {
          points.push(sceneFromLatLon(line[i - 1][1], line[i - 1][0], 1.0006), sceneFromLatLon(line[i][1], line[i][0], 1.0006));
        }
      }
      group.add(new THREE.LineSegments(
        new THREE.BufferGeometry().setFromPoints(points),
        new THREE.LineBasicMaterial({ color: 0xffffff, transparent: true, opacity, depthWrite: false }),
      ));
    }
    return group;
  };

  const loadTexture = (url) => new Promise((resolve, reject) => new THREE.TextureLoader().load(url, resolve, undefined, reject));
  Promise.all([fetch(WORLD_URL).then((response) => response.json()), loadTexture(DAY_URL), loadTexture(NIGHT_URL)])
    .then(([world, day, night]) => {
      for (const texture of [day, night]) texture.anisotropy = renderer.capabilities.getMaxAnisotropy();
      earthMaterial.uniforms.dayT.value = day;
      earthMaterial.uniforms.nightT.value = night;
      earthMaterial.uniforms.ready.value = 1;
      scene.add(coastAndBorders(world));
      markDirty();
    })
    .catch((error) => console.error("could not load the globe map", error));

  // ---- Satellites -------------------------------------------------------------

  const glowTexture = (colour) => {
    const dot = document.createElement("canvas");
    dot.width = dot.height = 64;
    const ctx = dot.getContext("2d");
    const gradient = ctx.createRadialGradient(32, 32, 0, 32, 32, 32);
    gradient.addColorStop(0, "#ffffff");
    gradient.addColorStop(0.25, colour);
    gradient.addColorStop(1, "rgba(0, 0, 0, 0)");
    ctx.fillStyle = gradient;
    ctx.fillRect(0, 0, 64, 64);
    return new THREE.CanvasTexture(dot);
  };
  const makeCloud = (colour, size) => {
    const points = new THREE.Points(new THREE.BufferGeometry(), new THREE.PointsMaterial({
      map: glowTexture(colour), size, sizeAttenuation: false, transparent: true, depthWrite: false, blending: THREE.AdditiveBlending,
    }));
    points.frustumCulled = false;
    scene.add(points);
    return points;
  };
  const allCloud = makeCloud("#19e6ff", 5);

  // Each satellite as a circle in its orbital plane, re-anchored to SGP4 every refresh.
  let constellation = null;
  let selectedName = "";
  let lastPositions = 0;

  const applyConstellation = (payload) => {
    const rows = payload.satellites || [];
    const n = rows.length;
    const next = {
      t0: Date.parse(payload.timestamp),
      theta0: payload.theta_g_rad,
      names: new Array(n),
      ids: new Int32Array(n),
      radius: new Float64Array(n),
      rate: new Float64Array(n),
      u1: new Float64Array(n * 3),
      u2: new Float64Array(n * 3),
      index: new Map(),
      scene: new Float32Array(n * 3),
    };
    rows.forEach(([name, id, x, y, z, vx, vy, vz], i) => {
      const r = Math.hypot(x, y, z);
      const hx = y * vz - z * vy, hy = z * vx - x * vz, hz = x * vy - y * vx, h = Math.hypot(hx, hy, hz);
      next.names[i] = name;
      next.ids[i] = id;
      next.radius[i] = r;
      next.rate[i] = h / (r * r);
      next.u1.set([x / r, y / r, z / r], i * 3);
      const ax = hy * z - hz * y, ay = hz * x - hx * z, az = hx * y - hy * x, a = Math.hypot(ax, ay, az);
      next.u2.set([ax / a, ay / a, az / a], i * 3);
      next.index.set(name, i);
    });
    allCloud.geometry.setAttribute("position", new THREE.BufferAttribute(next.scene, 3));
    constellation = next;
    lastPositions = 0;
    markDirty();
  };

  const loadConstellation = async () => {
    try {
      const response = await fetch(CONSTELLATION_URL, { cache: "no-store" });
      if (response.ok) applyConstellation(await response.json());
    } catch (error) {
      console.warn("constellation fetch failed", error);
    }
  };

  // Earth-fixed position of satellite i at time t, from its circular orbit.
  const satelliteEcef = (i, t, angle = null) => {
    const c = constellation;
    const dt = (t - c.t0) / 1000;
    const a = angle === null ? c.rate[i] * dt : angle;
    const cos = Math.cos(a), sin = Math.sin(a), r = c.radius[i], k = i * 3;
    const X = r * (c.u1[k] * cos + c.u2[k] * sin);
    const Y = r * (c.u1[k + 1] * cos + c.u2[k + 1] * sin);
    const Z = r * (c.u1[k + 2] * cos + c.u2[k + 2] * sin);
    const theta = c.theta0 + EARTH_ROTATION_RAD_S * dt;
    return [X * Math.cos(theta) + Y * Math.sin(theta), -X * Math.sin(theta) + Y * Math.cos(theta), Z];
  };

  const scratch = new THREE.Vector3();
  const updatePositions = (t) => {
    if (!constellation) return;
    const c = constellation;
    for (let i = 0; i < c.names.length; i++) {
      sceneFromEcef(...satelliteEcef(i, t), scratch);
      c.scene[i * 3] = scratch.x;
      c.scene[i * 3 + 1] = scratch.y;
      c.scene[i * 3 + 2] = scratch.z;
    }
    allCloud.geometry.attributes.position.needsUpdate = true;
  };
  const scenePosition = (i, out = new THREE.Vector3()) =>
    out.set(constellation.scene[i * 3], constellation.scene[i * 3 + 1], constellation.scene[i * 3 + 2]);

  const dish = new THREE.Points(
    new THREE.BufferGeometry().setAttribute("position", new THREE.BufferAttribute(new Float32Array(3), 3)),
    new THREE.PointsMaterial({ map: glowTexture("#ffffff"), size: 14, sizeAttenuation: false, transparent: true, depthWrite: false }),
  );
  scene.add(dish);

  const beamCore = new THREE.Mesh(new THREE.CylinderGeometry(0.003, 0.003, 1, 12, 1, true),
    new THREE.MeshBasicMaterial({ color: amber, transparent: true, opacity: 0.95, depthWrite: false }));
  const beamGlow = new THREE.Mesh(new THREE.CylinderGeometry(0.011, 0.011, 1, 16, 1, true),
    new THREE.MeshBasicMaterial({ color: amber, transparent: true, opacity: 0.2, blending: THREE.AdditiveBlending, depthWrite: false }));
  const pulse = new THREE.Mesh(new THREE.SphereGeometry(0.006, 12, 8), new THREE.MeshBasicMaterial({ color: 0xfff1cc }));
  const servingMark = new THREE.Points(
    new THREE.BufferGeometry().setAttribute("position", new THREE.BufferAttribute(new Float32Array(3), 3)),
    new THREE.PointsMaterial({ map: glowTexture("#f5b740"), size: 22, sizeAttenuation: false, transparent: true, depthWrite: false, blending: THREE.AdditiveBlending }),
  );
  const servingParts = [beamCore, beamGlow, pulse, servingMark];
  for (const part of servingParts) scene.add(part);

  const selectedMark = new THREE.Points(
    new THREE.BufferGeometry().setAttribute("position", new THREE.BufferAttribute(new Float32Array(3), 3)),
    new THREE.PointsMaterial({ map: glowTexture("#ffffff"), size: 18, sizeAttenuation: false, transparent: true, depthWrite: false, blending: THREE.AdditiveBlending }),
  );
  const orbit = new THREE.Line(new THREE.BufferGeometry(), new THREE.LineBasicMaterial({ color: 0xe6fdff, transparent: true, opacity: 1 }));
  orbit.frustumCulled = false;
  scene.add(selectedMark, orbit);

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
  const MIN_DISTANCE = 1.25 + 600 / SCENE_EARTH_RADIUS_KM;

  // Distance at which the whole globe fits the narrower side of the panel; zoom 1 is that.
  const fitDistance = () => {
    const half = (camera.fov / 2) * DEG;
    return GLOBE_FIT_RADIUS / Math.sin(Math.min(half, Math.atan(Math.tan(half) * camera.aspect)));
  };
  const cameraDistance = () => Math.max(MIN_DISTANCE, fitDistance() * view.zoom);
  const clampZoom = (zoom) => Math.min(2, Math.max(MIN_DISTANCE / fitDistance(), zoom));

  const placeCamera = () => {
    sceneFromLatLon(view.lat, view.lon, cameraDistance(), camera.position);
    camera.lookAt(0, 0, 0);
  };

  // True when the Earth sits between the camera and the point.
  const behindEarth = (point) => {
    const toPoint = point.clone().sub(camera.position);
    const distance = toPoint.length();
    toPoint.normalize();
    const b = camera.position.dot(toPoint), c = camera.position.lengthSq() - 1, disc = b * b - c;
    return disc > 0 && -b - Math.sqrt(disc) < distance;
  };

  const placeTag = (tag, point, text) => {
    const projected = point.clone().project(camera);
    const rect = stage.getBoundingClientRect();
    tag.hidden = behindEarth(point) || projected.z > 1;
    tag.style.left = `${((projected.x + 1) / 2) * rect.width}px`;
    tag.style.top = `${((1 - projected.y) / 2) * rect.height}px`;
    setText(tag, text);
  };

  const pick = (clientX, clientY) => {
    if (!constellation) return;
    const rect = stage.getBoundingClientRect();
    const point = new THREE.Vector3();
    let best = -1, bestDistance = PICK_RADIUS_PX;
    for (let i = 0; i < constellation.names.length; i++) {
      scenePosition(i, point).project(camera);
      if (point.z > 1) continue;
      const dx = ((point.x + 1) / 2) * rect.width + rect.left - clientX;
      const dy = ((1 - point.y) / 2) * rect.height + rect.top - clientY;
      const distance = Math.hypot(dx, dy);
      if (distance < bestDistance && !behindEarth(scenePosition(i))) {
        best = i;
        bestDistance = distance;
      }
    }
    selectedName = best >= 0 ? constellation.names[best] : "";
    markDirty();
  };

  const pointers = new Map();
  let lastPinch = 0;
  let pressed = null;
  canvas.addEventListener("pointerdown", (event) => {
    canvas.setPointerCapture(event.pointerId);
    pointers.set(event.pointerId, { x: event.clientX, y: event.clientY });
    pressed = pointers.size === 1 ? { x: event.clientX, y: event.clientY } : null;
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
    if (event.type === "pointerup" && pressed && Math.hypot(event.clientX - pressed.x, event.clientY - pressed.y) < 5) {
      pick(event.clientX, event.clientY);
    }
    pressed = null;
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
  if (ui.pickClose) {
    ui.pickClose.addEventListener("click", () => {
      selectedName = "";
      markDirty();
    });
  }

  // ---- Frame ----------------------------------------------------------------

  const direction = new THREE.Vector3();

  const updateServing = () => {
    const object = shared.servingObject();
    const i = object && constellation ? constellation.index.get(object.name) : undefined;
    const show = Boolean(observer && i !== undefined);
    for (const part of servingParts) part.visible = show;
    if (!show) {
      ui.tag.hidden = true;
      return;
    }
    const satellite = scenePosition(i);
    servingMark.position.copy(satellite);
    direction.subVectors(satellite, dish.position);
    const length = direction.length();
    direction.normalize();
    for (const beam of [beamCore, beamGlow]) {
      beam.position.copy(dish.position).addScaledVector(direction, length / 2);
      beam.quaternion.setFromUnitVectors(up, direction);
      beam.scale.set(1, length, 1);
    }
    pulse.position.copy(dish.position).addScaledVector(direction, length * (reduceMotion ? 0.5 : (Date.now() % 1100) / 1100));
    placeTag(ui.tag, satellite, object.name);
  };

  const updateSelection = (t) => {
    const i = constellation && selectedName ? constellation.index.get(selectedName) : undefined;
    const show = i !== undefined;
    selectedMark.visible = orbit.visible = show;
    if (ui.pick) ui.pick.hidden = !show;
    if (!show) {
      if (ui.pickTag) ui.pickTag.hidden = true;
      return;
    }
    const c = constellation;
    const satellite = scenePosition(i);
    selectedMark.position.copy(satellite);
    const ring = [];
    for (let k = 0; k <= ORBIT_POINTS; k++) {
      ring.push(sceneFromEcef(...satelliteEcef(i, t, (2 * Math.PI * k) / ORBIT_POINTS), new THREE.Vector3()));
    }
    orbit.geometry.dispose();
    orbit.geometry = new THREE.BufferGeometry().setFromPoints(ring);

    const k = i * 3;
    const normalZ = c.u1[k] * c.u2[k + 1] - c.u1[k + 1] * c.u2[k];
    const inclination = Math.acos(Math.max(-1, Math.min(1, normalZ))) / DEG;
    const height = ecefToGeodetic(...satelliteEcef(i, t)).height;
    const periodMin = (2 * Math.PI) / c.rate[i] / 60;
    setText(ui.pickName, c.names[i]);
    setText(ui.pickDetail, `NORAD ${c.ids[i]} · ${Math.round(height)} km\n${inclination.toFixed(1)}° incl · ${periodMin.toFixed(1)} min orbit`);
    if (ui.pickTag) placeTag(ui.pickTag, satellite, c.names[i]);
  };

  const draw = (now) => {
    const t = Date.now();
    earthMaterial.uniforms.sun.value.copy(sunDirection(t));
    if (observer) sceneFromLatLon(observer.lat, observer.lon, 1.0015, dish.position);
    dish.visible = Boolean(observer);
    if (target) {
      view.lat += (target.lat - view.lat) * 0.08;
      view.lon += (target.lon - view.lon) * 0.08;
      view.zoom += (target.zoom - view.zoom) * 0.08;
      if (Math.abs(target.zoom - view.zoom) < 0.002 && Math.abs(target.lat - view.lat) < 0.05) target = null;
    }
    placeCamera();
    if (now - lastPositions >= POSITION_UPDATE_MS) {
      lastPositions = now;
      updatePositions(t);
      updateSelection(t);
    } else if (selectedName && ui.pickTag && constellation && constellation.index.has(selectedName)) {
      placeTag(ui.pickTag, scenePosition(constellation.index.get(selectedName)), selectedName);
    }
    updateServing();
    setText(ui.caption, constellation ? `${constellation.names.length.toLocaleString("en-US")} SATELLITES · TRUE ALTITUDE` : "LOADING SATELLITES…");
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

  window.addEventListener("houston:sky", markDirty);
  window.addEventListener("houston:serving", markDirty);
  window.addEventListener("houston:location", (event) => {
    const moved = !observer || !event.detail.location_set
      || observer.lat !== event.detail.latitude_deg || observer.lon !== event.detail.longitude_deg;
    if (moved) applyLocation(event.detail);
  });
  if (shared.location) applyLocation(shared.location);
  loadConstellation();
  setInterval(loadConstellation, CONSTELLATION_REFRESH_MS);
  resize();
  placeCamera();
})();
