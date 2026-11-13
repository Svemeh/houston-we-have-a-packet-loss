(() => {
  "use strict";

  const SKY_EVENTS_URL = "/skyevents";
  const SERVING_URL = "/serving";
  const LOCATION_URL = "/location";
  const SERVING_POLL_MS = 5000;
  const SERVING_STALE_MS = 45000; // three slots without a result
  const WGS84_A_KM = 6378.137;
  const WGS84_E2 = 0.00669437999014;
  const DEG = Math.PI / 180;

  const shared = (window.houstonSky = window.houstonSky || {});

  // publish keeps the latest value and tells every panel on the page.
  shared.publish = (kind, detail) => {
    shared[kind] = detail;
    window.dispatchEvent(new CustomEvent(`houston:${kind}`, { detail }));
  };

  // currentSlot is the latest identified slot, or null when tracking is off or it has gone stale.
  shared.currentSlot = () => {
    const slot = shared.serving && shared.serving.enabled ? shared.serving.slot : null;
    if (!slot || Date.now() - Date.parse(slot.slot_start) > SERVING_STALE_MS) return null;
    return slot;
  };

  // servingObject is the matched satellite as the dish sees it right now, or null. Beams are drawn only to this.
  shared.servingObject = () => {
    const slot = shared.currentSlot();
    if (!slot || slot.status !== "matched" || !slot.best || !shared.sky || !shared.sky.location_set) return null;
    return (shared.sky.objects || []).find((object) => object.name === slot.best.satellite) || null;
  };

  // geo inverts the server's WGS84 look angles exactly.
  shared.geo = {
    geodeticToEcef(latDeg, lonDeg, heightKm) {
      const lat = latDeg * DEG, lon = lonDeg * DEG;
      const n = WGS84_A_KM / Math.sqrt(1 - WGS84_E2 * Math.sin(lat) ** 2);
      return [
        (n + heightKm) * Math.cos(lat) * Math.cos(lon),
        (n + heightKm) * Math.cos(lat) * Math.sin(lon),
        (n * (1 - WGS84_E2) + heightKm) * Math.sin(lat),
      ];
    },
    lookToEcef(location, azDeg, elDeg, rangeKm) {
      const origin = shared.geo.geodeticToEcef(location.latitude_deg, location.longitude_deg, 0);
      const lat = location.latitude_deg * DEG, lon = location.longitude_deg * DEG, az = azDeg * DEG, el = elDeg * DEG;
      const e = rangeKm * Math.cos(el) * Math.sin(az);
      const n = rangeKm * Math.cos(el) * Math.cos(az);
      const u = rangeKm * Math.sin(el);
      const sLat = Math.sin(lat), cLat = Math.cos(lat), sLon = Math.sin(lon), cLon = Math.cos(lon);
      return [
        origin[0] - sLon * e - sLat * cLon * n + cLat * cLon * u,
        origin[1] + cLon * e - sLat * sLon * n + cLat * sLon * u,
        origin[2] + cLat * n + sLat * u,
      ];
    },
    // Bowring's method; well under a metre at LEO heights.
    ecefToGeodetic(x, y, z) {
      const a = WGS84_A_KM, b = a * Math.sqrt(1 - WGS84_E2), ep2 = (a * a - b * b) / (b * b);
      const p = Math.hypot(x, y), theta = Math.atan2(z * a, p * b);
      const lat = Math.atan2(z + ep2 * b * Math.sin(theta) ** 3, p - WGS84_E2 * a * Math.cos(theta) ** 3);
      const n = a / Math.sqrt(1 - WGS84_E2 * Math.sin(lat) ** 2);
      return { lat: lat / DEG, lon: Math.atan2(y, x) / DEG, height: p / Math.cos(lat) - n };
    },
    // Height above the ellipsoid of a satellite seen at az/el/range.
    heightOf(location, object) {
      return shared.geo.ecefToGeodetic(...shared.geo.lookToEcef(location, object.az, object.el, object.range_km)).height;
    },
  };

  const loadLocation = async () => {
    try {
      const response = await fetch(LOCATION_URL, { cache: "no-store" });
      if (response.ok) shared.publish("location", await response.json());
    } catch (error) {
      console.error("could not read location", error);
    }
  };

  const loadServing = async () => {
    try {
      const response = await fetch(SERVING_URL, { cache: "no-store" });
      if (response.ok) shared.publish("serving", await response.json());
    } catch (error) {
      console.warn("serving poll failed", error);
    }
  };

  const stream = new EventSource(SKY_EVENTS_URL);
  stream.onmessage = (event) => {
    let snapshot;
    try {
      snapshot = JSON.parse(event.data);
    } catch (error) {
      console.error("bad sky snapshot", error);
      return;
    }
    // Set from another tab, or cleared by a restart: ask the server again.
    if (shared.sky && shared.sky.location_set !== snapshot.location_set) loadLocation();
    shared.publish("sky", snapshot);
  };

  loadLocation();
  loadServing();
  setInterval(loadServing, SERVING_POLL_MS);
})();
