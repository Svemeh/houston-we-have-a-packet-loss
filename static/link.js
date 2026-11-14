(() => {
  "use strict";

  const DISH_HEIGHT = 0.46; // panel centre above the ground, in scene units
  const SATELLITE_DISTANCE = 3.1; // dish to satellite in scene units; the real ~700 km is not to scale
  const GROUND_RADIUS = 2.75;
  const FIT_RADIUS = 3.0; // dish, beam and satellite in most directions fit the panel at zoom 1
  const GLIDE_SECONDS = 0.35; // time constant for the satellite moving to a new position
  const IDLE_FRAME_MS = 50; // 20 fps while the beam pulse runs
  const LIGHT_KM_PER_MS = 299.792458;
  const COMPASS_8 = ["N", "NE", "E", "SE", "S", "SW", "W", "NW"];
  const COMPASS_16 = ["N", "NNE", "NE", "ENE", "E", "ESE", "SE", "SSE", "S", "SSW", "SW", "WSW", "W", "WNW", "NW", "NNW"];
  const LABEL_FONT = "ui-monospace, SF Mono, Menlo, Consolas, monospace";
  const LABEL_COLOURS = { text: "#dfe7ef", amber: "#f5b740", dim: "#8a97a6" };
  const COLOURS = { amber: 0xf5b740, pulse: 0xfff1cc, label: 0x8a97a6, metal: 0x7d8792, panel: 0xc9d1d9, panelFace: 0xeef2f6, panelBack: 0x8e98a3, wing: 0x2f5f8f };
  const DEG = Math.PI / 180;

  const stage = document.getElementById("link-stage");
  const canvas = document.getElementById("link-canvas");
  if (!stage || !canvas) return;
  const shared = (window.houstonSky = window.houstonSky || {});
  const ui = {
    caption: document.getElementById("link-caption"),
    direction: document.getElementById("link-direction"),
  };
  const setText = (element, text) => {
    if (element && element.textContent !== text) element.textContent = text;
  };

  let renderer = null;
  try {
    if (!window.THREE) throw new Error("three.js did not load");
    renderer = new THREE.WebGLRenderer({ canvas, antialias: true, alpha: true });
  } catch (error) {
    console.warn("serving link view disabled", error);
    stage.classList.add("is-unavailable");
    setText(ui.caption, "3D VIEW NEEDS WEBGL");
    return;
  }
  renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 2));
  renderer.setClearColor(0x000000, 0);
  const scene = new THREE.Scene();
  const camera = new THREE.PerspectiveCamera(35, 1, 0.1, 100);
  const up = new THREE.Vector3(0, 1, 0);
  const dishCentre = new THREE.Vector3(0, DISH_HEIGHT, 0);

  // North is -z, east is +x, up is +y.
  const direction = (azDeg, elDeg, distance = 1, out = new THREE.Vector3()) =>
    out.set(distance * Math.cos(elDeg * DEG) * Math.sin(azDeg * DEG), distance * Math.sin(elDeg * DEG), -distance * Math.cos(elDeg * DEG) * Math.cos(azDeg * DEG));

  scene.add(new THREE.AmbientLight(0x8fa3b8, 0.55));
  const sun = new THREE.DirectionalLight(0xffffff, 0.9);
  sun.position.set(3, 6, 4);
  scene.add(sun);

  // Ground disc with compass rings and ticks; the rim breaks at N/E/S/W, where the letters sit on it.
  const groundTexture = () => {
    const size = 1024, c = size / 2, rim = 470;
    const texture = document.createElement("canvas");
    texture.width = texture.height = size;
    const ctx = texture.getContext("2d");
    const fade = ctx.createRadialGradient(c, c, 0, c, c, c);
    fade.addColorStop(0, "rgba(42, 55, 71, 0.55)");
    fade.addColorStop(1, "rgba(42, 55, 71, 0)");
    ctx.fillStyle = fade;
    ctx.fillRect(0, 0, size, size);
    ctx.lineWidth = 3;
    ctx.strokeStyle = "rgba(138, 151, 166, 0.45)";
    for (let quarter = 0; quarter < 4; quarter++) {
      const gap = 7 * DEG, start = quarter * Math.PI / 2;
      ctx.beginPath();
      ctx.arc(c, c, rim, start + gap, start + Math.PI / 2 - gap);
      ctx.stroke();
    }
    ctx.lineWidth = 2;
    ctx.strokeStyle = "rgba(138, 151, 166, 0.15)";
    for (const radius of [160, 320]) {
      ctx.beginPath();
      ctx.arc(c, c, radius, 0, 2 * Math.PI);
      ctx.stroke();
    }
    for (let az = 15; az < 360; az += 15) {
      if (az % 90 === 0) continue;
      const major = az % 45 === 0, length = major ? 34 : 18, a = az * DEG;
      ctx.strokeStyle = major ? "rgba(223, 231, 239, 0.6)" : "rgba(138, 151, 166, 0.4)";
      ctx.beginPath();
      ctx.moveTo(c + Math.sin(a) * (rim - length), c - Math.cos(a) * (rim - length));
      ctx.lineTo(c + Math.sin(a) * rim, c - Math.cos(a) * rim);
      ctx.stroke();
    }
    return new THREE.CanvasTexture(texture);
  };
  const ground = new THREE.Mesh(
    new THREE.PlaneGeometry(GROUND_RADIUS * 2 * (512 / 470), GROUND_RADIUS * 2 * (512 / 470)),
    new THREE.MeshBasicMaterial({ map: groundTexture(), transparent: true, depthWrite: false }),
  );
  ground.rotation.x = -Math.PI / 2;
  scene.add(ground);

  // Gen 3 style dish: rectangular panel on a mast with a cross foot.
  const metal = new THREE.MeshStandardMaterial({ color: COLOURS.metal, roughness: 0.6 });
  const mast = new THREE.Mesh(new THREE.CylinderGeometry(0.035, 0.045, DISH_HEIGHT - 0.04, 16), metal);
  mast.position.y = (DISH_HEIGHT - 0.04) / 2;
  const foot = new THREE.Mesh(new THREE.BoxGeometry(0.5, 0.03, 0.12), metal);
  foot.position.y = 0.015;
  const footCross = foot.clone();
  footCross.rotation.y = Math.PI / 2;
  const sideMaterial = new THREE.MeshStandardMaterial({ color: COLOURS.panel });
  const panel = new THREE.Mesh(new THREE.BoxGeometry(0.62, 0.045, 0.94), [
    sideMaterial, sideMaterial,
    new THREE.MeshStandardMaterial({ color: COLOURS.panelFace, roughness: 0.35 }),
    new THREE.MeshStandardMaterial({ color: COLOURS.panelBack }),
    sideMaterial, sideMaterial,
  ]);
  const tilt = new THREE.Group();
  tilt.add(panel);
  const aim = new THREE.Group();
  aim.position.y = DISH_HEIGHT;
  aim.add(tilt);
  scene.add(mast, foot, footCross, aim);

  // Satellite: amber bus with two solar wings, facing the dish.
  const satellite = new THREE.Group();
  satellite.add(new THREE.Mesh(new THREE.BoxGeometry(0.16, 0.07, 0.28), new THREE.MeshStandardMaterial({ color: COLOURS.amber, emissive: 0x5a3c00 })));
  const wingMaterial = new THREE.MeshStandardMaterial({ color: COLOURS.wing, emissive: 0x0a1a2a, roughness: 0.4, metalness: 0.3 });
  for (const side of [-1, 1]) {
    const wing = new THREE.Mesh(new THREE.BoxGeometry(0.62, 0.012, 0.22), wingMaterial);
    wing.position.x = side * 0.42;
    const arm = new THREE.Mesh(new THREE.BoxGeometry(0.1, 0.015, 0.015), metal);
    arm.position.x = side * 0.12;
    satellite.add(wing, arm);
  }
  const glowCanvas = document.createElement("canvas");
  glowCanvas.width = glowCanvas.height = 64;
  const glowContext = glowCanvas.getContext("2d");
  const glowGradient = glowContext.createRadialGradient(32, 32, 0, 32, 32, 32);
  glowGradient.addColorStop(0, "rgba(245, 183, 64, 0.6)");
  glowGradient.addColorStop(1, "rgba(245, 183, 64, 0)");
  glowContext.fillStyle = glowGradient;
  glowContext.fillRect(0, 0, 64, 64);
  const glow = new THREE.Sprite(new THREE.SpriteMaterial({ map: new THREE.CanvasTexture(glowCanvas), depthWrite: false, blending: THREE.AdditiveBlending }));
  glow.scale.set(1.1, 1.1, 1);

  const beamCore = new THREE.Mesh(new THREE.CylinderGeometry(0.012, 0.012, 1, 16, 1, true),
    new THREE.MeshBasicMaterial({ color: COLOURS.amber, depthWrite: false }));
  const beamGlow = new THREE.Mesh(new THREE.CylinderGeometry(0.045, 0.045, 1, 16, 1, true),
    new THREE.MeshBasicMaterial({ color: COLOURS.amber, transparent: true, opacity: 0.18, blending: THREE.AdditiveBlending, depthWrite: false }));
  const pulse = new THREE.Mesh(new THREE.SphereGeometry(0.03, 12, 8), new THREE.MeshBasicMaterial({ color: COLOURS.pulse }));

  const line = (material) => {
    const object = new THREE.Line(new THREE.BufferGeometry(), material);
    object.frustumCulled = false;
    return object;
  };
  const dashed = (opacity) => new THREE.LineDashedMaterial({ color: COLOURS.label, dashSize: 0.06, gapSize: 0.05, transparent: true, opacity });
  const groundRay = line(dashed(0.7));
  const drop = line(dashed(0.35));
  const elevationArc = line(new THREE.LineBasicMaterial({ color: COLOURS.label }));
  const azimuthArc = line(new THREE.LineBasicMaterial({ color: COLOURS.amber, transparent: true, opacity: 0.6 }));
  const linkParts = [satellite, glow, beamCore, beamGlow, pulse, groundRay, drop, elevationArc, azimuthArc];
  scene.add(...linkParts);

  // Text drawn into the scene as sprites, so it moves with the model in the same frame.
  const makeLabel = ({ colour = LABEL_COLOURS.text, weight = 400, size = 12, anchor = [0.5, 0.5] } = {}) => {
    const ratio = 2;
    const canvas = document.createElement("canvas");
    const context = canvas.getContext("2d");
    const texture = new THREE.CanvasTexture(canvas);
    texture.minFilter = THREE.LinearFilter;
    const sprite = new THREE.Sprite(new THREE.SpriteMaterial({ map: texture, transparent: true, depthTest: false, depthWrite: false }));
    sprite.center.set(anchor[0], anchor[1]);
    sprite.userData.anchor = anchor;
    sprite.renderOrder = 10;
    let current = null;
    sprite.userData.setText = (text) => {
      if (text === current) return;
      current = text;
      const font = `${weight} ${size * ratio}px ${LABEL_FONT}`;
      context.font = font;
      const padding = 6 * ratio;
      canvas.width = Math.ceil(context.measureText(text).width) + padding * 2;
      canvas.height = Math.ceil(size * ratio * 1.6);
      context.font = font;
      context.textBaseline = "middle";
      context.shadowColor = "rgba(0, 0, 0, 0.9)";
      context.shadowBlur = 6 * ratio;
      context.fillStyle = colour;
      context.fillText(text, padding, canvas.height / 2);
      texture.needsUpdate = true;
      sprite.userData.cssWidth = canvas.width / ratio;
      sprite.userData.cssHeight = canvas.height / ratio;
    };
    scene.add(sprite);
    return sprite;
  };
  // Keeps a label the same size on screen wherever it is, and slides it sideways to stay inside the panel.
  const sizeLabel = (sprite) => {
    const { cssWidth, cssHeight, anchor } = sprite.userData;
    const perPixel = (2 * Math.tan((camera.fov / 2) * DEG) * camera.position.distanceTo(sprite.position)) / Math.max(1, stage.clientHeight);
    sprite.scale.set(cssWidth * perPixel, cssHeight * perPixel, 1);
    const x = ((sprite.position.clone().project(camera).x + 1) / 2) * stage.clientWidth;
    const margin = 4;
    const lowest = (x + margin - stage.clientWidth) / cssWidth + 1, highest = (x - margin) / cssWidth;
    sprite.center.x = Math.min(Math.max(anchor[0], lowest), Math.max(lowest, highest));
  };
  const labels = {
    satellite: makeLabel({ colour: LABEL_COLOURS.amber, anchor: [0.5, -0.5] }),
    range: makeLabel({ anchor: [-0.05, 0.5] }),
    elevation: makeLabel({ colour: LABEL_COLOURS.dim, anchor: [1.1, 0.5] }),
    dish: makeLabel({ anchor: [0.5, 1.7] }),
    N: makeLabel({ colour: LABEL_COLOURS.dim, weight: 600, size: 13 }),
    E: makeLabel({ colour: LABEL_COLOURS.dim, weight: 600, size: 13 }),
    S: makeLabel({ colour: LABEL_COLOURS.dim, weight: 600, size: 13 }),
    W: makeLabel({ colour: LABEL_COLOURS.dim, weight: 600, size: 13 }),
  };
  const linkLabels = [labels.satellite, labels.range, labels.elevation];

  const setLine = (object, points) => {
    object.geometry.dispose();
    object.geometry = new THREE.BufferGeometry().setFromPoints(points);
    if (object.material.isLineDashedMaterial) object.computeLineDistances();
  };

  // ---- State ----------------------------------------------------------------

  const shown = new THREE.Vector3(); // the satellite direction on screen, gliding toward the live one
  let showing = false;

  // The dish's own boresight, or a level panel until it reports one.
  const updateDish = () => {
    const sky = shared.sky;
    const known = sky && !sky.cone_is_assumed;
    const tiltDeg = known ? 90 - sky.cone_elevation_deg : 0;
    const towardDeg = known ? sky.cone_azimuth_deg : 0;
    tilt.rotation.x = -tiltDeg * DEG;
    aim.rotation.y = -towardDeg * DEG;
    return known ? `DISH · tilted ${Math.round(tiltDeg)}° toward ${COMPASS_8[Math.round(towardDeg / 45) % 8]}` : "DISH · aim not reported yet";
  };

  const linkStatus = () => {
    const serving = shared.serving;
    if (!serving) return "";
    if (!serving.enabled) return "TRACKING OFF";
    const slot = shared.currentSlot();
    if (!slot) return "WAITING FOR A SLOT";
    if (slot.status !== "matched") return "NO MATCH THIS SLOT";
    return "NOT IN THE LIVE SKY";
  };

  // ---- Camera and input -----------------------------------------------------

  const view = { az: 60, el: 24, zoom: 1 };
  const target = new THREE.Vector3(0, 1.3, 0);
  const fitDistance = () => {
    const half = (camera.fov / 2) * DEG;
    return FIT_RADIUS / Math.sin(Math.min(half, Math.atan(Math.tan(half) * camera.aspect)));
  };
  const placeCamera = () => {
    direction(view.az, view.el, fitDistance() * view.zoom, camera.position).add(target);
    camera.lookAt(target);
  };

  const pointers = new Map();
  canvas.addEventListener("pointerdown", (event) => {
    canvas.setPointerCapture(event.pointerId);
    pointers.set(event.pointerId, { x: event.clientX, y: event.clientY });
  });
  canvas.addEventListener("pointermove", (event) => {
    const previous = pointers.get(event.pointerId);
    if (!previous) return;
    pointers.set(event.pointerId, { x: event.clientX, y: event.clientY });
    view.az -= (event.clientX - previous.x) * 0.4;
    view.el = Math.max(5, Math.min(80, view.el + (event.clientY - previous.y) * 0.3));
    markDirty();
  });
  const release = (event) => pointers.delete(event.pointerId);
  canvas.addEventListener("pointerup", release);
  canvas.addEventListener("pointercancel", release);
  // Plain scrolling keeps scrolling the page; ctrl/⌘ + scroll (and trackpad pinch) zooms.
  canvas.addEventListener("wheel", (event) => {
    if (!event.ctrlKey && !event.metaKey) return;
    event.preventDefault();
    view.zoom = Math.min(1.6, Math.max(0.55, view.zoom * Math.exp(event.deltaY * 0.003)));
    markDirty();
  }, { passive: false });

  // ---- Frame ----------------------------------------------------------------

  const placeLabel = (sprite, point, text) => {
    sprite.position.copy(point);
    sprite.userData.setText(text);
    sprite.visible = true;
  };

  let lastDraw = 0;
  // draw renders one frame; it returns true while the satellite is still gliding.
  const draw = (now) => {
    const dt = lastDraw ? Math.min((now - lastDraw) / 1000, 0.25) : 0;
    lastDraw = now;
    placeCamera();
    const dishText = updateDish();

    const object = shared.servingObject();
    let gliding = false;
    if (object) {
      const goal = direction(object.az, object.el);
      if (!showing) shown.copy(goal);
      else shown.lerp(goal, 1 - Math.exp(-dt / GLIDE_SECONDS)).normalize();
      gliding = shown.angleTo(goal) > 0.001;
      showing = true;
    } else {
      showing = false;
    }
    for (const part of linkParts) part.visible = showing;

    if (showing) {
      const elDeg = Math.asin(shown.y) / DEG;
      const azDeg = ((Math.atan2(shown.x, -shown.z) / DEG) + 360) % 360;
      const position = shown.clone().multiplyScalar(SATELLITE_DISTANCE).add(dishCentre);
      satellite.position.copy(position);
      satellite.lookAt(dishCentre);
      satellite.rotateX(Math.PI / 2);
      glow.position.copy(position);

      const beam = position.clone().sub(dishCentre);
      const length = beam.length();
      beam.normalize();
      for (const mesh of [beamCore, beamGlow]) {
        mesh.position.copy(dishCentre).addScaledVector(beam, length / 2);
        mesh.quaternion.setFromUnitVectors(up, beam);
        mesh.scale.set(1, length, 1);
      }
      pulse.position.copy(dishCentre).addScaledVector(beam, length * ((now % 1400) / 1400));

      setLine(groundRay, [new THREE.Vector3(0, 0.005, 0), direction(azDeg, 0, 2.4).setY(0.005)]);
      setLine(drop, [position, position.clone().setY(0.005)]);
      const arc = [];
      for (let e = 0; e <= elDeg; e += 2) arc.push(direction(azDeg, e, 0.9).add(dishCentre));
      arc.push(direction(azDeg, elDeg, 0.9).add(dishCentre));
      setLine(elevationArc, arc);
      const swing = ((azDeg + 180) % 360) - 180;
      const azPoints = [];
      for (let k = 0; k <= 48; k++) azPoints.push(direction((swing * k) / 48, 0, 1.3).setY(0.006));
      setLine(azimuthArc, azPoints);

      const height = shared.location ? Math.round(shared.geo.heightOf(shared.location, object)) : null;
      placeLabel(labels.satellite, position.clone().add(new THREE.Vector3(0, 0.25, 0)), `${object.name}${height === null ? "" : ` · ${height} km up`}`);
      placeLabel(labels.elevation, direction(azDeg, elDeg / 2, 0.95).add(dishCentre), `${Math.round(object.el)}°`);
      placeLabel(labels.range, dishCentre.clone().addScaledVector(beam, length * 0.62),
        `${Math.round(object.range_km)} km · ${(object.range_km / LIGHT_KM_PER_MS).toFixed(1)} ms`);
      setText(ui.direction, `${Math.round(object.az) % 360}° ${COMPASS_16[Math.round(object.az / 22.5) % 16]} · ${Math.round(object.el)}° UP`);
    } else {
      for (const sprite of linkLabels) sprite.visible = false;
      setText(ui.direction, linkStatus());
    }

    placeLabel(labels.dish, new THREE.Vector3(0, 0, 0), dishText);
    for (const [letter, azDeg] of [["N", 0], ["E", 90], ["S", 180], ["W", 270]]) {
      placeLabel(labels[letter], direction(azDeg, 0, GROUND_RADIUS), letter);
    }
    for (const sprite of Object.values(labels)) if (sprite.visible) sizeLabel(sprite);
    renderer.render(scene, camera);
    return gliding;
  };

  const resize = () => {
    renderer.setSize(stage.clientWidth, stage.clientHeight, false);
    camera.aspect = stage.clientWidth / Math.max(1, stage.clientHeight);
    camera.updateProjectionMatrix();
    markDirty();
  };

  let dirty = true;
  let animating = false;
  let onScreen = true;
  let frameRequest = 0;
  let lastFrame = 0;
  const loop = (now) => {
    frameRequest = 0;
    if (!onScreen || document.hidden) return;
    if (dirty || animating || pointers.size || (showing && now - lastFrame >= IDLE_FRAME_MS)) {
      dirty = false;
      lastFrame = now;
      animating = draw(now);
    }
    if (dirty || animating || showing || pointers.size) frameRequest = requestAnimationFrame(loop);
  };
  function markDirty() {
    dirty = true;
    if (!frameRequest) frameRequest = requestAnimationFrame(loop);
  }

  new ResizeObserver(resize).observe(stage);
  new IntersectionObserver((entries) => {
    onScreen = entries[entries.length - 1].isIntersecting;
    markDirty();
  }).observe(stage);
  document.addEventListener("visibilitychange", markDirty);
  for (const kind of ["sky", "serving", "location"]) window.addEventListener(`houston:${kind}`, markDirty);
  setText(ui.caption, "ANGLES TRUE · DISTANCE NOT TO SCALE");
  resize();
})();
