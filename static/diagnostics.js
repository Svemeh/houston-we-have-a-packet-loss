(() => {
  "use strict";

  const RANGES = [
    { id: "15m", spanMs: 15 * 60 * 1000 },
    { id: "1h", spanMs: 60 * 60 * 1000 },
    { id: "6h", spanMs: 6 * 60 * 60 * 1000 },
    { id: "24h", spanMs: 24 * 60 * 60 * 1000 },
  ];
  const DEFAULT_RANGE_ID = "15m";
  const REFETCH_MS = 5 * 60 * 1000;
  const REFETCH_RANGES_LONGER_THAN_MS = 15 * 60 * 1000;
  const MIN_GAP_MS = 3000;
  const GAP_MULTIPLIER = 2.5;

  const segments = window.houstonSegments;
  const { DISH, CLOUDFLARE, GOOGLE, POP, SPACE_STATES, SEGMENTS, DISH_REPORT_AFTER_MS, CHAIN_WINDOW_MS } = segments;
  const { normalizeTel, firstIndexAtOrAfter, dishSawDrop } = segments;

  const byId = (id) => document.getElementById(id);
  const css = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  const median = (values) => {
    if (!values.length) return null;
    const sorted = [...values].sort((a, b) => a - b);
    return sorted[Math.floor(sorted.length / 2)];
  };
  const quantile = (values, q) => {
    if (!values.length) return null;
    const sorted = [...values].sort((a, b) => a - b);
    return sorted[Math.min(sorted.length - 1, Math.floor(q * sorted.length))];
  };
  const clock = (ms) => new Date(ms).toLocaleTimeString([], { hour12: false });
  const fmtMs = (value) => (value == null ? "—" : `${Math.round(value)} ms`);
  const fmtPct = (value) => (value == null ? "—" : `${value < 1 ? value.toFixed(2) : value.toFixed(1)}%`);

  const state = {
    range: RANGES.find((range) => range.id === DEFAULT_RANGE_ID),
    hops: [],
    tel: [],
    hopBucketMs: 1000,
    telBucketMs: 1000,
    liveHops: [],
    liveTel: [],
    routerIp: null,
    hoverMs: null,
    pinnedMs: null,
  };

  const normalizeHop = (sample, widthMs) => {
    const point = segments.normalizeHop(sample, widthMs);
    if (point.routerIp) state.routerIp = point.routerIp;
    return point;
  };
  const classify = (point, tel = state.tel) => segments.classify(point, tel, state.routerIp);
  const dishOnlyDrops = (tel, hops, start, telWidthMs) => segments.dishOnlyDrops(tel, hops, start, telWidthMs, state.routerIp);

  const lostBySegment = (point) => {
    const lost = (ip) => (ip && point.hops[ip] ? point.hops[ip].lossSeconds : 0);
    const lan = lost(state.routerIp);
    const dish = Math.max(0, lost(DISH) - lan);
    const upstream = Math.max(0, Math.max(lost(CLOUDFLARE), lost(GOOGLE)) - lan - dish);
    const paired = (key) => Math.max(...[CLOUDFLARE, GOOGLE].map((ip) => (point.hops[ip] ? point.hops[ip][key] : 0)));
    const space = Math.max(0, Math.max(lost(POP), paired("popDown")) - lan - dish);
    const beyond = Math.min(upstream, paired("popUp"));
    return { lan, dish, space, beyond, unknown: Math.max(0, upstream - space - beyond) };
  };

  const windowEnd = () => Date.now();
  const windowStart = () => windowEnd() - state.range.spanMs;

  const fetchJson = async (url) => {
    const response = await fetch(url, { cache: "no-store" });
    if (!response.ok) throw new Error(`${response.status} ${response.statusText}`);
    return response.json();
  };

  const announceRange = () => {
    document.body.dataset.rangeMs = String(state.range.spanMs);
    window.dispatchEvent(new CustomEvent("houston:diag-range", { detail: { spanMs: state.range.spanMs } }));
  };

  const loadRange = async () => {
    const rangeBar = byId("diag-range");
    rangeBar.classList.add("is-loading");
    try {
      const [hops, tel] = await Promise.all([
        fetchJson(`/hopshistory?range=${state.range.id}`),
        fetchJson(`/history?range=${state.range.id}`),
      ]);
      state.hopBucketMs = Math.max(1000, hops.bucket_width_ms || 0);
      state.telBucketMs = Math.max(1000, tel.bucket_width_ms || 0);
      state.hops = (hops.samples || []).map((sample) => normalizeHop(sample, state.hopBucketMs));
      state.tel = (tel.samples || []).map(normalizeTel);
    } catch (error) {
      console.warn("diagnostics history failed", error);
    } finally {
      rangeBar.classList.remove("is-loading");
      redraw();
    }
  };

  const pruneOld = () => {
    const cutoff = windowStart() - DISH_REPORT_AFTER_MS;
    const keepHops = firstIndexAtOrAfter(state.hops, cutoff);
    if (keepHops > 0) state.hops = state.hops.slice(keepHops);
    const keepTel = firstIndexAtOrAfter(state.tel, cutoff);
    if (keepTel > 0) state.tel = state.tel.slice(keepTel);
  };

  const appendNewer = (list, item) => {
    if (!list.length || item.t > list[list.length - 1].t) list.push(item);
  };

  const trimLive = () => {
    const cutoff = Date.now() - CHAIN_WINDOW_MS - DISH_REPORT_AFTER_MS;
    state.liveHops = state.liveHops.slice(firstIndexAtOrAfter(state.liveHops, cutoff));
    state.liveTel = state.liveTel.slice(firstIndexAtOrAfter(state.liveTel, cutoff));
  };

  const connectLive = () => {
    const shared = window.houstonSky;
    shared.liveStream("/hopsevents", (hopStream) => {
      hopStream.addEventListener("backfill", (event) => {
        try {
          state.liveHops = JSON.parse(event.data).map((sample) => normalizeHop(sample, 1000));
          trimLive();
        } catch {}
      });
      hopStream.onmessage = (event) => {
        try {
          const point = normalizeHop(JSON.parse(event.data), 1000);
          appendNewer(state.liveHops, point);
          appendNewer(state.hops, point);
        } catch {}
      };
    }, loadRange);
    shared.liveStream("/events", (telStream) => {
      telStream.addEventListener("backfill", (event) => {
        try {
          state.liveTel = JSON.parse(event.data).map(normalizeTel);
          trimLive();
        } catch {}
      });
      telStream.onmessage = (event) => {
        try {
          const sample = normalizeTel(JSON.parse(event.data));
          appendNewer(state.liveTel, sample);
          appendNewer(state.tel, sample);
        } catch {}
      };
    });
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
    return { context, width, height };
  };

  const xOf = (t, width) => ((t - windowStart()) / state.range.spanMs) * width;

  const drawMarker = (context, width, height) => {
    const marker = state.hoverMs ?? state.pinnedMs;
    if (marker == null) return;
    const x = xOf(marker, width);
    context.strokeStyle = css("--text");
    context.globalAlpha = state.hoverMs != null ? 0.5 : 0.8;
    context.beginPath();
    context.moveTo(x + 0.5, 0);
    context.lineTo(x + 0.5, height);
    context.stroke();
    context.globalAlpha = 1;
  };

  const drawStrip = () => {
    const { context, width, height } = prepareCanvas(byId("strip"));
    context.fillStyle = css("--edge");
    context.fillRect(0, height / 2 - 0.5, width, 1);
    const counts = { lan: 0, dish: 0, space: 0, beyond: 0, unknown: 0 };
    const start = windowStart();
    for (const point of state.hops) {
      if (point.t < start) continue;
      const segment = classify(point);
      if (!segment) continue;
      const split = lostBySegment(point);
      for (const key of Object.keys(counts)) counts[key] += split[key];
      const x = xOf(point.t, width);
      context.fillStyle = css(SEGMENTS[segment].color);
      context.fillRect(x, 2, Math.max(2, xOf(point.t + point.w, width) - x), height - 4);
    }
    const dishOnly = dishOnlyDrops(state.tel, state.hops, start, state.telBucketMs);
    context.fillStyle = css(SEGMENTS.space.color);
    for (const sample of dishOnly) {
      const x = xOf(sample.t, width);
      context.fillRect(x, 2, Math.max(2, xOf(sample.t + state.telBucketMs, width) - x), height - 4);
    }
    counts.space += dishOnly.length;
    const atLeast = { space: dishOnly.length > 0 && state.telBucketMs > 1000 ? "≥" : "" };
    drawMarker(context, width, height);
    byId("strip-range").textContent = `last ${state.range.id}`;
    byId("strip-legend").innerHTML = Object.entries(SEGMENTS)
      .map(([key, segment]) => `<span><i style="background:var(${segment.color})"></i>${segment.label} <b class="tabular-num">${atLeast[key] || ""}${counts[key]} s</b></span>`)
      .join("");
  };

  const lanes = () => [
    { key: "router", segment: "lan", label: state.routerIp ? `router ${state.routerIp}` : "router (not pinged)", ip: state.routerIp },
    { key: "dish", segment: "dish", label: `dish ${DISH}`, ip: DISH },
    { key: "pop", segment: "space", label: "PoP gateway · our probe", ip: POP },
    { key: "space", segment: "space", label: "dish → PoP · the dish's own ping", tel: true },
    { key: "cloudflare", segment: "beyond", label: `Cloudflare ${CLOUDFLARE}`, ip: CLOUDFLARE },
    { key: "google", segment: "beyond", label: `Google ${GOOGLE}`, ip: GOOGLE },
  ];

  const buildLanes = () => {
    byId("lane-list").innerHTML = lanes()
      .map((lane) => `<div class="diag-lane"><div class="diag-lane-head"><span class="label" id="lane-label-${lane.key}">${lane.label}</span><span class="scope-range-label tabular-num" id="lane-note-${lane.key}"></span></div><canvas id="lane-${lane.key}"></canvas></div>`)
      .join("");
    for (const lane of lanes()) attachHover(byId(`lane-${lane.key}`));
  };

  const lanePoints = (lane) => {
    const start = windowStart();
    if (lane.tel) {
      return state.tel.filter((sample) => sample.t >= start).map((sample) => ({
        t: sample.t,
        w: state.telBucketMs,
        rtt: sample.latency,
        lostFraction: SPACE_STATES.has(sample.linkState) ? Math.max(sample.drop, 0.05) : sample.drop,
      }));
    }
    return state.hops.filter((point) => point.t >= start && point.hops[lane.ip]).map((point) => {
      const hop = point.hops[lane.ip];
      return { t: point.t, w: point.w, rtt: hop.rtt, lostFraction: hop.sent ? hop.lost / hop.sent : 0, lost: hop.lost, sent: hop.sent };
    });
  };

  const drawLane = (lane) => {
    const canvas = byId(`lane-${lane.key}`);
    if (!canvas) return;
    byId(`lane-label-${lane.key}`).textContent = lane.label;
    const { context, width, height } = prepareCanvas(canvas);
    const points = lane.ip || lane.tel ? lanePoints(lane) : [];
    const rtts = points.map((point) => point.rtt).filter((rtt) => rtt != null);
    const top = Math.max(5, (quantile(rtts, 0.99) || 0) * 1.25);
    const yOf = (rtt) => height - 2 - (Math.min(rtt, top) / top) * (height - 14);

    context.fillStyle = css("--grid");
    context.fillRect(0, height - 2, width, 1);
    context.fillStyle = css("--label-dim");
    context.font = `10px ${css("--mono")}`;
    context.fillText(`${Math.round(top)} ms`, 4, 10);

    const color = css(SEGMENTS[lane.segment].color);
    context.fillStyle = color;
    for (const point of points) {
      if (!point.lostFraction) continue;
      const x = xOf(point.t, width);
      context.globalAlpha = Math.min(0.6, 0.3 + point.lostFraction);
      context.fillRect(x, 0, Math.max(1.5, xOf(point.t + point.w, width) - x), height);
    }
    context.globalAlpha = 1;

    const gapMs = Math.max(MIN_GAP_MS, GAP_MULTIPLIER * (lane.tel ? state.telBucketMs : state.hopBucketMs));
    context.strokeStyle = color;
    context.lineWidth = 1.2;
    context.beginPath();
    let previous = null;
    for (const point of points) {
      if (point.rtt == null) {
        previous = null;
        continue;
      }
      const x = xOf(point.t, width), y = yOf(point.rtt);
      if (previous && point.t - previous.t <= gapMs) context.lineTo(x, y);
      else context.moveTo(x, y);
      previous = point;
    }
    context.stroke();
    drawMarker(context, width, height);

    const latest = [...points].reverse().find((point) => point.rtt != null);
    const lostTotal = lane.tel
      ? points.filter((point) => point.lostFraction > 0).length
      : points.reduce((sum, point) => sum + (point.lost || 0), 0);
    const sentTotal = points.reduce((sum, point) => sum + (point.sent || 0), 0);
    const fast = !lane.tel && points.some((point) => point.sent > 2 * Math.max(1, point.w / 1000));
    let lossText = `${lostTotal} s lost`;
    if (lane.tel) lossText = `dish drop in ${lostTotal} ${state.telBucketMs > 1000 ? "buckets" : "s"}`;
    else if (fast) lossText = `drop ${fmtPct(sentTotal ? (100 * lostTotal) / sentTotal : null)}`;
    const note = lane.ip === POP && !points.length ? "not probed" : `${fmtMs(latest && latest.rtt)} now · ${lossText}`;
    byId(`lane-note-${lane.key}`).textContent = lane.ip || lane.tel ? note : "";
  };

  const NODE = {
    mac: [30, 150],
    router: [150, 150],
    dish: [270, 150],
    sat: [380, 46],
    pop: [490, 150],
    cloudflare: [690, 120],
    google: [690, 180],
  };
  const SVG_NS = "http://www.w3.org/2000/svg";
  const chainParts = {};

  const svgEl = (parent, tag, attributes, text) => {
    const element = document.createElementNS(SVG_NS, tag);
    for (const [key, value] of Object.entries(attributes)) element.setAttribute(key, value);
    if (text != null) element.textContent = text;
    parent.appendChild(element);
    return element;
  };

  const buildChain = () => {
    const svg = byId("chain");
    const [macX, macY] = NODE.mac, [routerX] = NODE.router, [dishX] = NODE.dish, [satX, satY] = NODE.sat, [popX, popY] = NODE.pop;
    const paths = {
      lan: `M${macX} ${macY}L${routerX} ${macY}`,
      dish: `M${routerX} ${macY}L${dishX} ${macY}`,
      space: `M${dishX} ${macY}Q${dishX + 30} ${satY + 30} ${satX} ${satY}Q${popX - 30} ${satY + 30} ${popX} ${popY}`,
      cloudflare: `M${popX} ${popY}L${popX + 50} ${popY}C${popX + 95} ${popY} ${popX + 95} ${NODE.cloudflare[1]} ${popX + 140} ${NODE.cloudflare[1]}L${NODE.cloudflare[0]} ${NODE.cloudflare[1]}`,
      google: `M${popX} ${popY}L${popX + 50} ${popY}C${popX + 95} ${popY} ${popX + 95} ${NODE.google[1]} ${popX + 140} ${NODE.google[1]}L${NODE.google[0]} ${NODE.google[1]}`,
    };
    for (const [key, d] of Object.entries(paths)) chainParts[key] = { path: svgEl(svg, "path", { d, class: "chain-seg" }) };

    const labelAt = (key, x, y) => {
      chainParts[key].rtt = svgEl(svg, "text", { x, y, class: "chain-value", "text-anchor": "middle" }, "—");
      chainParts[key].loss = svgEl(svg, "text", { x, y: y + 15, class: "chain-note", "text-anchor": "middle" }, "");
    };
    labelAt("lan", (macX + routerX) / 2, macY - 30);
    labelAt("dish", (routerX + dishX) / 2, macY - 30);
    labelAt("space", satX, popY - 22);
    labelAt("cloudflare", popX + 100, NODE.cloudflare[1] - 20);
    labelAt("google", popX + 100, NODE.google[1] + 22);

    const nodes = [["mac", "this machine", "below"], ["router", "router", "below"], ["dish", "dish", "below"], ["pop", "PoP", "below"], ["cloudflare", CLOUDFLARE, "right"], ["google", GOOGLE, "right"]];
    for (const [key, label, place] of nodes) {
      const [x, y] = NODE[key];
      svgEl(svg, "circle", { cx: x, cy: y, r: 6, class: "chain-node" });
      const text = place === "right" ? { x: x + 13, y: y + 4, "text-anchor": "start" } : { x, y: y + 28, "text-anchor": "middle" };
      chainParts[`node-${key}`] = svgEl(svg, "text", { ...text, class: "chain-label" }, label);
    }
    svgEl(svg, "rect", { x: satX - 8, y: satY - 8, width: 16, height: 16, rx: 3, class: "chain-sat" });
    const satLine = svgEl(svg, "text", { x: satX + 16, y: satY + 4, "text-anchor": "start" });
    chainParts.satName = svgEl(satLine, "tspan", { class: "chain-value" }, "satellite ?");
    chainParts.satNote = svgEl(satLine, "tspan", { class: "chain-note", dx: 8 }, "");
  };

  const setSegment = (key, rttText, lossText, state, missing) => {
    const part = chainParts[key];
    const tone = state === "is-clean" ? "" : state;
    part.path.setAttribute("class", `chain-seg ${missing ? "is-missing" : state}`);
    part.rtt.textContent = rttText;
    part.loss.textContent = state === "is-affected" ? "affected" : lossText;
    part.rtt.setAttribute("class", `chain-value ${tone}`);
    part.loss.setAttribute("class", `chain-note ${tone}`);
  };

  const drawChain = () => {
    trimLive();
    const since = windowEnd() - CHAIN_WINDOW_MS;
    const chain = segments.chainStatus(state.liveHops, state.liveTel, state.routerIp, windowEnd());
    const { recent, counts, states } = chain;
    const recentTel = state.liveTel.slice(firstIndexAtOrAfter(state.liveTel, since)).filter((sample) => sample.latency != null);
    const rttOf = (ip) => median(recent.map((point) => point.hops[ip] && point.hops[ip].rtt).filter((rtt) => rtt != null));
    const lossText = (key) => {
      const all = counts[key], whole = chain.full[key];
      if (!all) return "no loss";
      return [whole ? `lost ${whole} s` : "", all > whole ? `drop in ${all - whole} s` : ""].filter(Boolean).join(" · ");
    };
    const recentPop = recent.map((point) => point.hops[POP]).filter(Boolean);
    const popSent = recentPop.reduce((sum, hop) => sum + hop.sent, 0);
    const probePct = popSent ? (100 * recentPop.reduce((sum, hop) => sum + hop.lost, 0)) / popSent : null;
    const probedPopMs = rttOf(POP);
    const popMs = probedPopMs != null ? probedPopMs : median(recentTel.map((sample) => sample.latency));
    const dropPct = recentTel.length ? (100 * recentTel.reduce((sum, sample) => sum + sample.drop, 0)) / recentTel.length : null;

    const noData = !recent.length;
    const routerPinged = recent.some((point) => state.routerIp && point.hops[state.routerIp]);
    setSegment("lan", routerPinged ? fmtMs(rttOf(state.routerIp)) : "not pinged", routerPinged ? lossText("lan") : "", states.lan, noData || !routerPinged);
    setSegment("dish", fmtMs(rttOf(DISH)), lossText("dish"), states.dish, noData);
    const popValue = probePct == null ? `PoP ${fmtMs(popMs)}` : `PoP ${fmtMs(popMs)} · drop ${fmtPct(probePct)}`;
    setSegment("space", popValue, `${lossText("space")} · dish ${fmtPct(dropPct)}`, states.space, probedPopMs == null && !recentTel.length);
    for (const [key, ip] of [["cloudflare", CLOUDFLARE], ["google", GOOGLE]]) {
      const rtt = rttOf(ip);
      const beyond = rtt != null && popMs != null ? `+${Math.max(0, Math.round(rtt - popMs))} ms${probedPopMs == null ? " est." : ""}` : "—";
      setSegment(key, beyond, lossText(key), states[key], noData);
    }

    const shared = window.houstonSky || {};
    const slot = shared.currentSlot ? shared.currentSlot() : null;
    if (slot && slot.status === "matched" && slot.best) {
      chainParts.satName.textContent = slot.best.satellite;
      chainParts.satNote.textContent = "";
    } else {
      chainParts.satName.textContent = "satellite ?";
      chainParts.satNote.textContent = slot ? slot.status.replace(/_/g, " ") : "";
    }

    const status = byId("chain-status");
    status.textContent = noData ? "waiting for pings" : chain.text;
    status.className = `scope-range-label ${noData ? "" : chain.tone}`;
  };

  const describeMoment = (t) => {
    const hopIndex = firstIndexAtOrAfter(state.hops, t - state.hopBucketMs);
    const point = state.hops[hopIndex];
    const telIndex = firstIndexAtOrAfter(state.tel, t - 1000);
    const tel = state.tel[telIndex];
    if (!point || Math.abs(point.t - t) > Math.max(3000, 2 * state.hopBucketMs)) return `${clock(t)} · no data`;
    const dishDropped = tel && Math.abs(tel.t - t) <= Math.max(1000, state.telBucketMs) && dishSawDrop(tel);
    const segment = classify(point) || (dishDropped ? "space" : null);
    const hop = (ip) => {
      const result = point.hops[ip];
      if (!result) return "—";
      if (!result.lost) return fmtMs(result.rtt);
      return result.sent > 1 ? `${fmtMs(result.rtt)} ${result.lost}/${result.sent} lost` : "lost";
    };
    const parts = [
      clock(point.t),
      segment ? `loss: ${SEGMENTS[segment].label}` : "clean",
      state.routerIp ? `router ${hop(state.routerIp)}` : null,
      `dish ${hop(DISH)}`,
      point.hops[POP] ? `PoP ${hop(POP)}` : null,
      tel ? `dish → PoP ${fmtMs(tel.latency)} drop ${(100 * tel.drop).toFixed(0)}%` : null,
      `${CLOUDFLARE} ${hop(CLOUDFLARE)}`,
      `${GOOGLE} ${hop(GOOGLE)}`,
    ];
    return parts.filter(Boolean).join(" · ");
  };

  const updateHoverReadout = () => {
    const marker = state.hoverMs ?? state.pinnedMs;
    byId("hover-readout").textContent = marker == null ? "hover the strip or a lane · click the strip to pin a moment" : `${state.pinnedMs != null && state.hoverMs == null ? "pinned · " : ""}${describeMoment(marker)}`;
  };

  const attachHover = (canvas) => {
    if (!canvas) return;
    canvas.addEventListener("mousemove", (event) => {
      const rect = canvas.getBoundingClientRect();
      state.hoverMs = windowStart() + ((event.clientX - rect.left) / rect.width) * state.range.spanMs;
      redraw();
    });
    canvas.addEventListener("mouseleave", () => {
      state.hoverMs = null;
      redraw();
    });
  };

  const redraw = () => {
    pruneOld();
    drawChain();
    drawStrip();
    for (const lane of lanes()) drawLane(lane);
    updateHoverReadout();
  };

  const buildRangeBar = () => {
    const bar = byId("diag-range");
    for (const range of RANGES) {
      const button = document.createElement("button");
      button.type = "button";
      button.className = `range-button ${range === state.range ? "is-selected" : ""}`;
      button.textContent = range.id;
      button.addEventListener("click", () => {
        state.range = range;
        state.pinnedMs = null;
        for (const other of bar.children) other.classList.toggle("is-selected", other === button);
        announceRange();
        loadRange();
      });
      bar.appendChild(button);
    }
  };

  const strip = byId("strip");
  attachHover(strip);
  strip.addEventListener("click", () => {
    if (state.hoverMs == null) return;
    state.pinnedMs = state.hoverMs;
    byId("lanes").scrollIntoView({ behavior: "smooth", block: "start" });
  });
  document.addEventListener("keydown", (event) => {
    if (event.key !== "Escape") return;
    state.pinnedMs = null;
    redraw();
  });

  buildRangeBar();
  announceRange();
  buildChain();
  buildLanes();
  window.addEventListener("resize", redraw);
  window.addEventListener("houston:serving", drawChain);
  window.addEventListener("houston:sky", drawChain);
  connectLive();
  loadRange();
  setInterval(redraw, 1000);
  setInterval(() => {
    if (state.range.spanMs > REFETCH_RANGES_LONGER_THAN_MS) loadRange();
  }, REFETCH_MS);
})();
