(() => {
  "use strict";

  const SLOT_SECONDS = 15;
  const BOUNDARY_SECOND = 12;
  const DISH_REPORT_LAG_S = 3;
  const MAX_BURST_RANGE_MS = 2 * 60 * 60 * 1000;
  const LIVE_POLL_MS = 5000;
  const ROLLUP_REFETCH_MS = 5 * 60 * 1000;
  const HOUR_MS = 60 * 60 * 1000;

  const byId = (id) => document.getElementById(id);
  const css = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  const clock = (ms) => new Date(ms).toLocaleTimeString([], { hour12: false });
  const hourClock = (ms) => new Date(ms).toLocaleTimeString([], { hour12: false, hour: "2-digit", minute: "2-digit" });
  const pct = (value) => (value == null ? "—" : `${value < 10 ? value.toFixed(1) : Math.round(value)}%`);
  const quantile = (sorted, q) => {
    if (!sorted.length) return null;
    return sorted[Math.min(sorted.length - 1, Math.floor(q * sorted.length))];
  };

  const state = {
    spanMs: 15 * 60 * 1000,
    rollups: [],
    bursts: [],
    serving: new Map(),
    selectedT: null,
  };

  const fetchJson = async (url) => {
    const response = await fetch(url, { cache: "no-store" });
    if (!response.ok) throw new Error(`${response.status} ${response.statusText}`);
    return response.json();
  };

  const rangeParam = (ms) => `${Math.round(ms / 60000)}m`;

  const normalizeBurst = (sample) => {
    const rtts = sample.rtt_ms || [];
    const lost = rtts.map((rtt) => rtt < 0);
    let run = 0, longest = 0;
    for (const isLost of lost) {
      run = isLost ? run + 1 : 0;
      longest = Math.max(longest, run);
    }
    const answered = rtts.filter((rtt) => rtt >= 0);
    return {
      t: Date.parse(sample.boundary),
      offsets: sample.offsets_ms || [],
      rtts,
      lost,
      lostCount: sample.lost || 0,
      gapMs: longest * (sample.step_ms || 20),
      peak: answered.length ? Math.max(...answered) : null,
      failed: (sample.errors || 0) > 0,
    };
  };

  const slotKey = (ms) => Math.round(ms / 1000);

  const addServing = (samples) => {
    let added = 0;
    for (const slot of samples || []) {
      const key = slotKey(Date.parse(slot.slot_start));
      if (!state.serving.has(key)) added++;
      state.serving.set(key, {
        matched: slot.status === "matched" && !!slot.best?.satellite,
        satellite: slot.best?.satellite,
      });
    }
    const cutoff = slotKey(Date.now() - Math.min(state.spanMs, MAX_BURST_RANGE_MS)) - SLOT_SECONDS;
    for (const key of state.serving.keys()) if (key < cutoff) state.serving.delete(key);
    return added;
  };

  const usableBursts = () => state.bursts.filter((burst) => !burst.failed && burst.rtts.length);

  const selectedIndex = (bursts) => {
    if (!bursts.length) return null;
    const index = state.selectedT == null ? -1 : bursts.findIndex((burst) => burst.t === state.selectedT);
    return index < 0 ? bursts.length - 1 : index;
  };

  const slotOffset = (index) => ((index - BOUNDARY_SECOND + SLOT_SECONDS + 7) % SLOT_SECONDS) - 7;
  const wrapOffset = (offset) => ((offset + 7 + SLOT_SECONDS) % SLOT_SECONDS) - 7;

  const slotTotals = () => {
    const sum = (key) => {
      const totals = new Array(SLOT_SECONDS).fill(0);
      for (const rollup of state.rollups) {
        (rollup[key] || []).forEach((value, index) => {
          if (index < SLOT_SECONDS) totals[index] += value || 0;
        });
      }
      return totals;
    };
    const lost = sum("handover_loss"), sent = sum("handover_sent");
    const dishDrop = sum("dish_drop_slots"), dishSamples = sum("dish_slot_samples");
    const ours = new Array(SLOT_SECONDS).fill(null);
    const dish = new Array(SLOT_SECONDS).fill(null);
    for (let index = 0; index < SLOT_SECONDS; index++) {
      if (sent[index]) ours[slotOffset(index) + 7] = (100 * lost[index]) / sent[index];
      if (dishSamples[index]) dish[wrapOffset(slotOffset(index) - DISH_REPORT_LAG_S) + 7] = (100 * dishDrop[index]) / dishSamples[index];
    }
    return { ours, dish };
  };

  const prepareCanvas = (canvas) => {
    const ratio = window.devicePixelRatio || 1;
    const width = canvas.clientWidth, height = canvas.clientHeight;
    if (canvas.width !== Math.round(width * ratio) || canvas.height !== Math.round(height * ratio)) {
      canvas.width = Math.round(width * ratio);
      canvas.height = Math.round(height * ratio);
    }
    const context = canvas.getContext("2d");
    context.setTransform(ratio, 0, 0, ratio, 0, 0);
    context.clearRect(0, 0, width, height);
    context.font = `10px ${css("--mono")}`;
    return { context, width, height };
  };

  const drawSlots = () => {
    const { context, width, height } = prepareCanvas(byId("slot-chart"));
    const { ours, dish } = slotTotals();
    const values = [...ours, ...dish].filter((value) => value != null);
    const top = Math.max(1, Math.max(0, ...values) * 1.2);
    const plotTop = 14, plotBottom = height - 16;
    const yOf = (value) => plotBottom - (Math.min(value, top) / top) * (plotBottom - plotTop);
    const step = width / SLOT_SECONDS;

    context.fillStyle = css("--grid");
    context.fillRect(0, plotBottom, width, 1);
    context.fillStyle = css("--label-dim");
    context.fillText(pct(top), 4, 10);

    context.fillStyle = css("--chart-machine-to-internet-loss");
    ours.forEach((value, column) => {
      if (value == null) return;
      const y = yOf(value);
      context.globalAlpha = column === 7 ? 0.95 : 0.6;
      context.fillRect(column * step + step * 0.18, y, step * 0.64, plotBottom - y);
    });
    context.globalAlpha = 1;

    context.strokeStyle = css("--chart-download");
    context.fillStyle = css("--chart-download");
    context.lineWidth = 1.5;
    context.beginPath();
    let started = false;
    dish.forEach((value, column) => {
      if (value == null) return;
      const x = column * step + step / 2, y = yOf(value);
      if (started) context.lineTo(x, y);
      else context.moveTo(x, y);
      started = true;
    });
    context.stroke();
    dish.forEach((value, column) => {
      if (value == null) return;
      context.beginPath();
      context.arc(column * step + step / 2, yOf(value), 2.5, 0, 2 * Math.PI);
      context.fill();
    });

    context.fillStyle = css("--label-dim");
    context.textAlign = "center";
    for (let column = 0; column < SLOT_SECONDS; column++) {
      const offset = column - 7;
      if (offset % 2 !== 0 && offset !== 0) continue;
      context.fillStyle = offset === 0 ? css("--text") : css("--label-dim");
      context.fillText(offset === 0 ? "handover" : `${offset > 0 ? "+" : ""}${offset} s`, column * step + step / 2, height - 3);
    }
    context.textAlign = "left";

    const hours = state.rollups.length;
    const last = state.rollups[hours - 1];
    byId("slot-range").textContent = hours ? `${hours} h to ${hourClock(Date.parse(last.hour_start) + HOUR_MS)}` : "no full hour yet";
    byId("slot-legend").innerHTML = [
      ["--chart-machine-to-internet-loss", "our pings to 1.1.1.1 lost"],
      ["--chart-download", `dish-reported drop, moved ${DISH_REPORT_LAG_S} s earlier`],
    ].map(([color, label]) => `<span><i style="background:var(${color})"></i>${label}</span>`).join("");
  };

  const positionStats = (bursts) => {
    const length = Math.max(0, ...bursts.map((burst) => burst.rtts.length));
    const stats = [];
    for (let index = 0; index < length; index++) {
      const answered = [], offsets = [];
      let lost = 0, total = 0;
      for (const burst of bursts) {
        if (index >= burst.rtts.length) continue;
        total++;
        offsets.push(burst.offsets[index]);
        if (burst.lost[index]) lost++;
        else answered.push(burst.rtts[index]);
      }
      answered.sort((a, b) => a - b);
      offsets.sort((a, b) => a - b);
      stats.push({
        offset: quantile(offsets, 0.5),
        p50: quantile(answered, 0.5),
        p90: quantile(answered, 0.9),
        lossPct: total ? (100 * lost) / total : 0,
      });
    }
    return stats;
  };

  const drawBurst = () => {
    const bursts = usableBursts();
    const stats = positionStats(bursts);
    const selected = selectedIndex(bursts);
    const burst = selected == null ? null : bursts[selected];
    const { context, width, height } = prepareCanvas(byId("burst-chart"));
    const offsets = stats.map((stat) => stat.offset).filter((offset) => offset != null);
    const minOffset = offsets.length ? offsets[0] : -600;
    const maxOffset = offsets.length ? offsets[offsets.length - 1] : 900;
    const xOf = (offset) => ((offset - minOffset) / Math.max(1, maxOffset - minOffset)) * (width - 8) + 4;
    const plotTop = 14, plotBottom = height - 16;
    const highs = stats.map((stat) => stat.p90).filter((value) => value != null);
    if (burst && burst.peak != null) highs.push(burst.peak);
    const top = Math.max(20, Math.max(0, ...highs) * 1.15);
    const yOf = (rtt) => plotBottom - (Math.min(rtt, top) / top) * (plotBottom - plotTop);

    context.fillStyle = css("--grid");
    context.fillRect(0, plotBottom, width, 1);
    context.fillStyle = css("--label-dim");
    context.fillText(`${Math.round(top)} ms`, 4, 10);

    const zeroX = xOf(0);
    context.strokeStyle = css("--label-dim");
    context.setLineDash([3, 4]);
    context.beginPath();
    context.moveTo(zeroX + 0.5, plotTop);
    context.lineTo(zeroX + 0.5, plotBottom);
    context.stroke();
    context.setLineDash([]);

    const band = stats.filter((stat) => stat.p50 != null && stat.offset != null);
    if (band.length > 1) {
      context.fillStyle = css("--chart-download");
      context.globalAlpha = 0.18;
      context.beginPath();
      band.forEach((stat, index) => (index ? context.lineTo(xOf(stat.offset), yOf(stat.p90)) : context.moveTo(xOf(stat.offset), yOf(stat.p90))));
      [...band].reverse().forEach((stat) => context.lineTo(xOf(stat.offset), yOf(stat.p50)));
      context.closePath();
      context.fill();
      context.globalAlpha = 0.6;
      context.strokeStyle = css("--chart-download");
      context.lineWidth = 1;
      context.beginPath();
      band.forEach((stat, index) => (index ? context.lineTo(xOf(stat.offset), yOf(stat.p50)) : context.moveTo(xOf(stat.offset), yOf(stat.p50))));
      context.stroke();
      context.globalAlpha = 1;
    }

    if (burst) {
      context.strokeStyle = css("--text");
      context.lineWidth = 1.4;
      context.beginPath();
      let drawing = false;
      burst.rtts.forEach((rtt, index) => {
        if (burst.lost[index]) {
          drawing = false;
          return;
        }
        const x = xOf(burst.offsets[index]), y = yOf(rtt);
        if (drawing) context.lineTo(x, y);
        else context.moveTo(x, y);
        drawing = true;
      });
      context.stroke();
      context.fillStyle = css("--alarm");
      burst.lost.forEach((isLost, index) => {
        if (!isLost) return;
        const x = xOf(burst.offsets[index]);
        context.fillRect(x - 1.5, plotTop, 3, plotBottom - plotTop);
      });
    }

    context.fillStyle = css("--label-dim");
    context.textAlign = "center";
    for (const offset of [-600, -400, -200, 0, 200, 400, 600, 800]) {
      if (offset < minOffset - 1 || offset > maxOffset + 1) continue;
      context.fillStyle = offset === 0 ? css("--text") : css("--label-dim");
      context.fillText(offset === 0 ? "handover" : `${offset > 0 ? "+" : ""}${offset} ms`, xOf(offset), height - 3);
    }
    context.textAlign = "left";

    const lossCanvas = prepareCanvas(byId("burst-loss"));
    const lossTop = Math.max(1, ...stats.map((stat) => stat.lossPct)) * 1.1;
    lossCanvas.context.fillStyle = css("--grid");
    lossCanvas.context.fillRect(0, lossCanvas.height - 1, lossCanvas.width, 1);
    lossCanvas.context.fillStyle = css("--chart-machine-to-internet-loss");
    const barWidth = Math.max(1.5, (width - 8) / Math.max(1, stats.length) - 1);
    for (const stat of stats) {
      if (!stat.lossPct || stat.offset == null) continue;
      const barHeight = (stat.lossPct / lossTop) * (lossCanvas.height - 12);
      lossCanvas.context.fillRect(xOf(stat.offset) - barWidth / 2, lossCanvas.height - 1 - barHeight, barWidth, barHeight);
    }
    lossCanvas.context.fillStyle = css("--label-dim");
    lossCanvas.context.fillText(`${pct(lossTop)} of handovers lost this ping`, 4, 10);

    const label = byId("burst-label");
    if (!burst) {
      label.textContent = "no handovers yet";
    } else {
      const position = selected === bursts.length - 1 ? "latest" : `${selected + 1}/${bursts.length}`;
      const outcome = burst.lostCount ? `lost ${burst.lostCount}` : "clean";
      label.textContent = `${clock(burst.t)} · ${outcome} · peak ${burst.peak == null ? "—" : Math.round(burst.peak)} ms · ${position}`;
      label.classList.toggle("is-lost", burst.lostCount > 0);
    }
    byId("burst-prev").disabled = !burst || selected === 0;
    byId("burst-next").disabled = !burst || selected === bursts.length - 1;

    const gaps = bursts.filter((item) => item.lostCount > 0).map((item) => item.gapMs).sort((a, b) => a - b);
    const since = bursts.length ? `since ${hourClock(bursts[0].t)}` : "";
    const changed = burst ? satelliteChanged(burst) : null;
    const satellite = changed == null ? "" : changed ? " · switched satellite" : " · stayed on same satellite";
    byId("burst-legend").innerHTML = [
      `<span><i style="background:var(--chart-download);opacity:.4"></i>p50–p90 RTT of ${bursts.length} bursts ${since}</span>`,
      `<span><i style="background:var(--text)"></i>${!burst || selected === bursts.length - 1 ? "latest handover" : `handover at ${clock(burst.t)}`}${satellite}</span>`,
      `<span><i style="background:var(--alarm)"></i>lost ping</span>`,
      gaps.length ? `<span>loss gap p50 <b>${quantile(gaps, 0.5)} ms</b> · p90 <b>${quantile(gaps, 0.9)} ms</b></span>` : "",
    ].join("");
  };

  const satelliteChanged = (burst) => {
    const before = state.serving.get(slotKey(burst.t) - SLOT_SECONDS);
    const after = state.serving.get(slotKey(burst.t));
    if (!before?.matched || !after?.matched) return null;
    return before.satellite !== after.satellite;
  };

  const drawSplit = () => {
    const bursts = usableBursts();
    const groups = { changed: { n: 0, lost: 0 }, same: { n: 0, lost: 0 } };
    for (const burst of bursts) {
      const changed = satelliteChanged(burst);
      if (changed == null) continue;
      const group = changed ? groups.changed : groups.same;
      group.n++;
      if (burst.lostCount > 0) group.lost++;
    }
    const split = byId("handover-split");
    split.hidden = !groups.changed.n && !groups.same.n;
    if (split.hidden) return;
    const part = (group) => (group.n ? `<b>${pct((100 * group.lost) / group.n)}</b> of ${group.n.toLocaleString()}` : "—");
    split.innerHTML = `<span>satellite changed: ${part(groups.changed)} lost a ping · same satellite: ${part(groups.same)} · since ${hourClock(bursts[0].t)}</span>`;
  };

  const drawStats = () => {
    let bursts = 0, withLoss = 0, lostPings = 0;
    for (const rollup of state.rollups) {
      bursts += rollup.handover_bursts || 0;
      withLoss += rollup.handover_bursts_with_loss || 0;
      lostPings += rollup.handover_lost_pings || 0;
    }
    const stats = byId("handover-stats");
    stats.textContent = bursts
      ? `${bursts.toLocaleString()} handovers · ${pct((100 * withLoss) / bursts)} lost a ping · ${lostPings.toLocaleString()} pings lost`
      : "—";
  };

  const redraw = () => {
    drawSlots();
    drawBurst();
    drawStats();
    drawSplit();
  };

  const loadRollups = async () => {
    try {
      const payload = await fetchJson(`/rollup?range=${rangeParam(state.spanMs + 2 * HOUR_MS)}`);
      const rollups = payload.samples || [];
      const inRange = rollups.filter((rollup) => Date.parse(rollup.hour_start) + HOUR_MS > Date.now() - state.spanMs);
      state.rollups = inRange.length ? inRange : rollups.slice(-1);
    } catch (error) {
      console.warn("handover rollups failed", error);
    }
    redraw();
  };

  const loadBursts = async () => {
    const spanMs = Math.min(state.spanMs, MAX_BURST_RANGE_MS);
    const [bursts, serving] = await Promise.allSettled([
      fetchJson(`/handovers?range=${rangeParam(spanMs)}`),
      fetchJson(`/servinghistory?range=${rangeParam(spanMs + 60 * 1000)}`),
    ]);
    if (bursts.status === "fulfilled") {
      state.bursts = (bursts.value.samples || []).map(normalizeBurst);
      state.selectedT = null;
    } else {
      console.warn("handover bursts failed", bursts.reason);
    }
    state.serving = new Map();
    if (serving.status === "fulfilled") addServing(serving.value.samples);
    else console.warn("serving history failed", serving.reason);
    redraw();
  };

  const pollBursts = async () => {
    if (document.hidden) return;
    try {
      const [payload, serving] = await Promise.all([fetchJson("/handovers?range=1m"), fetchJson("/servinghistory?range=1m").catch(() => null)]);
      const addedSlots = serving ? addServing(serving.samples) : 0;
      const newest = state.bursts.length ? state.bursts[state.bursts.length - 1].t : 0;
      const fresh = (payload.samples || []).map(normalizeBurst).filter((burst) => burst.t > newest);
      if (fresh.length) {
        const cutoff = Date.now() - Math.min(state.spanMs, MAX_BURST_RANGE_MS);
        state.bursts = state.bursts.concat(fresh).filter((burst) => burst.t >= cutoff);
        redraw();
      } else if (addedSlots) {
        drawBurst();
        drawSplit();
      }
    } catch {}
  };

  const setRange = (spanMs) => {
    if (!spanMs || spanMs === state.spanMs) return;
    state.spanMs = spanMs;
    loadRollups();
    loadBursts();
  };

  const step = (delta) => {
    const bursts = usableBursts();
    const current = selectedIndex(bursts);
    if (current == null) return;
    const next = Math.min(bursts.length - 1, Math.max(0, current + delta));
    state.selectedT = next === bursts.length - 1 ? null : bursts[next].t;
    drawBurst();
  };

  byId("burst-prev").addEventListener("click", () => step(-1));
  byId("burst-next").addEventListener("click", () => step(1));
  window.addEventListener("houston:diag-range", (event) => setRange(event.detail.spanMs));
  window.addEventListener("resize", redraw);
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) pollBursts();
  });

  state.spanMs = Number(document.body.dataset.rangeMs) || state.spanMs;
  loadRollups();
  loadBursts();
  setInterval(pollBursts, LIVE_POLL_MS);
  setInterval(loadRollups, ROLLUP_REFETCH_MS);
})();
