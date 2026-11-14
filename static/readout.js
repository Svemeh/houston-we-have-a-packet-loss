(() => {
  "use strict";

  const SLOT_SECONDS = 15;
  const SLOT_BOUNDARY_OFFSET = 12; // handovers at :12/:27/:42/:57 UTC
  const LIGHT_KM_PER_MS = 299.792458;
  const COMPASS = ["N", "NNE", "NE", "ENE", "E", "ESE", "SE", "SSE", "S", "SSW", "SW", "WSW", "W", "WNW", "NW", "NNW"];
  const STATUS_TEXT = {
    ambiguous: ["NO CLEAR MATCH", "two satellites fit this slot's arc about equally"],
    few_pixels: ["ARC TOO SHORT", "too few map cells this slot to match"],
    no_location: ["NO LOCATION", "set your location in the Sky View card"],
    no_elements: ["NO ELEMENTS", "waiting on satellite elements"],
    unsupported_frame: ["UNSUPPORTED", "this dish's obstruction map can't be matched"],
    error: ["NO ANSWER", "the dish did not answer this slot"],
  };

  const root = document.getElementById("readout");
  if (!root) return;
  const shared = (window.houstonSky = window.houstonSky || {});
  const ui = Object.fromEntries(
    ["status", "name", "sub", "el", "az", "range", "height", "fit", "one-way", "slot", "next", "bar", "last", "last-label", "last-text", "foot"]
      .map((id) => [id, document.getElementById(`readout-${id}`)]),
  );
  const setText = (element, text) => {
    if (element && element.textContent !== text) element.textContent = text;
  };
  const pad = (n) => String(n).padStart(2, "0");
  let slotHistory = [];

  const setStatus = (text, kind) => {
    setText(ui.status, text);
    if (ui.status) ui.status.className = `readout-pill ${kind ? `is-${kind}` : ""}`;
  };

  const slotLabel = (start) => {
    const s = new Date(start).getUTCSeconds();
    return `:${pad(s)} → :${pad((s + SLOT_SECONDS) % 60)}`;
  };

  const slotName = (slot) => (slot.status === "matched" && slot.best ? slot.best.satellite : (STATUS_TEXT[slot.status] || [slot.status])[0].toLowerCase());

  const updateLastHandover = () => {
    const [previous, latest] = slotHistory;
    const consecutive = previous && latest && Date.parse(latest.slot_start) - Date.parse(previous.slot_start) === SLOT_SECONDS * 1000;
    if (ui.last) ui.last.hidden = !consecutive || !shared.currentSlot();
    if (!consecutive) return;
    setText(ui["last-label"], `HANDOVER AT :${pad(new Date(latest.slot_start).getUTCSeconds())}`);
    const same = previous.status === "matched" && latest.status === "matched" && previous.best.satellite === latest.best.satellite;
    setText(ui["last-text"], same ? `kept ${latest.best.satellite}` : `${slotName(previous)} → ${slotName(latest)}`);
  };

  // The bar runs as one looping animation; each tick only nudges it back onto the wall clock.
  let slotBar = null;
  const updateClock = () => {
    const into = ((Date.now() / 1000 - SLOT_BOUNDARY_OFFSET) % SLOT_SECONDS + SLOT_SECONDS) % SLOT_SECONDS;
    setText(ui.next, `${Math.ceil(SLOT_SECONDS - into)} s`);
    if (!ui.bar || !ui.bar.animate) return;
    if (!slotBar) {
      slotBar = ui.bar.animate([{ transform: "scaleX(0)" }, { transform: "scaleX(1)" }], { duration: SLOT_SECONDS * 1000, iterations: Infinity });
    }
    slotBar.currentTime = into * 1000;
  };

  // Ticks just after each wall-clock second, when the countdown text changes (handovers fall on whole seconds).
  const startClock = () => {
    updateClock();
    setTimeout(startClock, 1005 - (Date.now() % 1000));
  };

  const updateFoot = () => {
    const location = shared.location;
    if (!location || !location.location_set) {
      setText(ui.foot, "no location set");
      return;
    }
    const lat = location.latitude_deg, lon = location.longitude_deg;
    const where = `DISH ${Math.abs(lat).toFixed(2)}°${lat >= 0 ? "N" : "S"} ${Math.abs(lon).toFixed(2)}°${lon >= 0 ? "E" : "W"}`;
    const sky = shared.sky;
    if (!sky) {
      setText(ui.foot, where);
      return;
    }
    setText(ui.foot, `${where} · ${sky.in_cone_count} above ${sky.service_floor_deg}° · ${(sky.objects || []).length} above horizon`);
  };

  // update redraws the whole panel from the shared state.
  const update = () => {
    const serving = shared.serving;
    const slot = shared.currentSlot();
    const object = shared.servingObject();
    for (const key of ["el", "az", "range", "height", "fit", "one-way"]) setText(ui[key], "—");
    setText(ui.slot, slot ? `IDENTIFIED FOR SLOT ${slotLabel(slot.slot_start)}` : "SLOT");

    if (!serving) {
      setStatus("—", "");
      setText(ui.name, "—");
      setText(ui.sub, "waiting for Houston");
    } else if (!serving.enabled) {
      setStatus("TRACKING OFF", "off");
      setText(ui.name, "—");
      setText(ui.sub, "satellite tracking is off (-track-satellite=false)");
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
      if (object) {
        setText(ui.sub, `NORAD ${slot.best.norad_id}`);
        setText(ui.el, `${Math.round(object.el)}°`);
        setText(ui.az, `${Math.round(object.az) % 360}° ${COMPASS[Math.round(object.az / 22.5) % 16]}`);
        setText(ui.range, `${Math.round(object.range_km)} km`);
        setText(ui.height, `${Math.round(shared.geo.heightOf(shared.location, object))} km`);
        setText(ui["one-way"], `${(object.range_km / LIGHT_KM_PER_MS).toFixed(1)} ms`);
      } else {
        setText(ui.sub, `NORAD ${slot.best.norad_id} · not in the live sky right now`);
      }
    }

    updateLastHandover();
    updateClock();
    updateFoot();
  };

  window.addEventListener("houston:serving", (event) => {
    const slot = event.detail && event.detail.enabled ? event.detail.slot : null;
    if (slot && (!slotHistory.length || slotHistory[slotHistory.length - 1].slot_start !== slot.slot_start)) {
      slotHistory = [...slotHistory.slice(-1), slot];
    }
    update();
  });
  window.addEventListener("houston:sky", update);
  window.addEventListener("houston:location", update);
  update();
  startClock();
})();
