(() => {
  "use strict";

  const DISH = "192.168.100.1";
  const CLOUDFLARE = "1.1.1.1";
  const GOOGLE = "8.8.8.8";
  const POP = "pop";
  const KNOWN_TARGETS = new Set([DISH, POP, CLOUDFLARE, GOOGLE]);
  const SPACE_STATES = new Set(["DEGRADED", "OBSTRUCTED", "NO SIGNAL"]);
  const DISH_REPORT_BEFORE_MS = 1000;
  const DISH_REPORT_AFTER_MS = 5000;

  const SEGMENTS = {
    lan: { label: "machine → router", color: "--chart-latency" },
    dish: { label: "router → dish", color: "--chart-packet-loss" },
    space: { label: "dish → PoP", color: "--chart-download" },
    beyond: { label: "PoP → internet", color: "--chart-upload" },
  };

  const normalizeHop = (sample, widthMs) => {
    const hops = {};
    let routerIp = null;
    for (const [ip, value] of Object.entries(sample.hops || {})) {
      const lost = typeof value.lost === "boolean" ? (value.lost ? 1 : 0) : value.lost || 0;
      const sent = value.sent == null ? 1 : value.sent;
      hops[ip] = { rtt: lost >= sent ? null : value.rtt_ms, sent, lost };
      if (!KNOWN_TARGETS.has(ip)) routerIp = ip;
    }
    return { t: Date.parse(sample.timestamp), w: widthMs, hops, routerIp };
  };

  const normalizeTel = (sample) => ({
    t: Date.parse(sample.timestamp),
    latency: sample.link_state === "OFFLINE" ? null : sample.latency_ms,
    drop: sample.drop_rate_fraction || 0,
    linkState: sample.link_state,
  });

  const firstIndexAtOrAfter = (points, t) => {
    let low = 0, high = points.length;
    while (low < high) {
      const mid = (low + high) >> 1;
      if (points[mid].t < t) low = mid + 1;
      else high = mid;
    }
    return low;
  };

  const dishSawDrop = (sample) => sample.drop > 0 || SPACE_STATES.has(sample.linkState);

  const spaceEvidence = (tel, t, widthMs) => {
    const end = t + widthMs + DISH_REPORT_AFTER_MS;
    for (let index = firstIndexAtOrAfter(tel, t - DISH_REPORT_BEFORE_MS); index < tel.length && tel[index].t <= end; index++) {
      if (dishSawDrop(tel[index])) return true;
    }
    return false;
  };

  const lostAt = (point, ip) => Boolean(ip && point.hops[ip] && point.hops[ip].lost > 0);
  const popAnswered = (point) => Boolean(point.hops[POP] && point.hops[POP].lost < point.hops[POP].sent);

  const classify = (point, tel, routerIp) => {
    if (lostAt(point, routerIp)) return "lan";
    if (lostAt(point, DISH)) return "dish";
    if (lostAt(point, POP)) return "space";
    if (lostAt(point, CLOUDFLARE) || lostAt(point, GOOGLE)) return popAnswered(point) || !spaceEvidence(tel, point.t, point.w) ? "beyond" : "space";
    return null;
  };

  const dishOnlyDrops = (tel, hops, start, telWidthMs, routerIp) => {
    const losses = hops.filter((point) => point.t >= start - DISH_REPORT_AFTER_MS && classify(point, tel, routerIp));
    return tel.filter((sample) => {
      if (sample.t < start || !dishSawDrop(sample)) return false;
      const index = firstIndexAtOrAfter(losses, sample.t - DISH_REPORT_AFTER_MS - telWidthMs);
      return !(index < losses.length && losses[index].t <= sample.t + DISH_REPORT_BEFORE_MS);
    });
  };

  window.houstonSegments = {
    DISH, CLOUDFLARE, GOOGLE, POP, SPACE_STATES, SEGMENTS, DISH_REPORT_AFTER_MS,
    normalizeHop, normalizeTel, firstIndexAtOrAfter, dishSawDrop, spaceEvidence, lostAt, popAnswered, classify, dishOnlyDrops,
  };
})();
