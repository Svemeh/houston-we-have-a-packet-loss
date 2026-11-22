(() => {
  "use strict";

  const WINDOW_MS = 60 * 1000;
  const POLL_MS = 5000;
  const NODES = [
    ["mac", "machine", 22],
    ["router", "router", 112],
    ["dish", "dish", 202],
    ["pop", "PoP", 292],
    ["internet", "internet", 382],
  ];
  const ORDER = ["lan", "dish", "space", "beyond"];

  const segments = window.houstonSegments;
  const svgNs = "http://www.w3.org/2000/svg";
  const chain = document.getElementById("mini-chain");
  const status = document.getElementById("mini-chain-status");
  if (!chain || !segments) return;

  const parts = {};
  const element = (tag, attributes, text) => {
    const node = document.createElementNS(svgNs, tag);
    for (const [key, value] of Object.entries(attributes)) node.setAttribute(key, value);
    if (text != null) node.textContent = text;
    chain.appendChild(node);
    return node;
  };

  ORDER.forEach((key, index) => {
    const from = NODES[index][2], to = NODES[index + 1][2];
    parts[key] = {
      line: element("line", { x1: from, y1: 20, x2: to, y2: 20, class: "chain-seg is-missing" }),
      note: element("text", { x: (from + to) / 2, y: 11, "text-anchor": "middle", class: "chain-note" }, ""),
    };
  });
  NODES.forEach(([, label, x], index) => {
    const anchor = index === 0 ? "start" : index === NODES.length - 1 ? "end" : "middle";
    element("circle", { cx: x, cy: 20, r: 6, class: "chain-node" });
    element("text", { x: anchor === "start" ? x - 8 : anchor === "end" ? x + 8 : x, y: 44, "text-anchor": anchor, class: "chain-label" }, label);
  });

  const fetchJson = async (url) => {
    const response = await fetch(url, { cache: "no-store" });
    if (!response.ok) throw new Error(`${response.status} ${response.statusText}`);
    return response.json();
  };

  const render = (hops, tel) => {
    const since = Date.now() - WINDOW_MS;
    let routerIp = null;
    for (const point of hops) if (point.routerIp) routerIp = point.routerIp;
    const recent = hops.filter((point) => point.t >= since);
    const counts = { lan: 0, dish: 0, space: 0, beyond: 0 };
    for (const point of recent) {
      const segment = segments.classify(point, tel, routerIp);
      if (segment) counts[segment]++;
    }
    counts.space += segments.dishOnlyDrops(tel, hops, since, 1000, routerIp).length;

    const routerPinged = recent.some((point) => routerIp && point.hops[routerIp]);
    const firstFailing = ORDER.findIndex((key) => counts[key] > 0);
    ORDER.forEach((key, index) => {
      const missing = !recent.length || (key === "lan" && !routerPinged);
      let state = "is-clean";
      if (missing) state = "is-missing";
      else if (counts[key]) state = "is-lost";
      else if (firstFailing >= 0 && index > firstFailing) state = "is-affected";
      parts[key].line.setAttribute("class", `chain-seg ${state}`);
      parts[key].note.textContent = counts[key] ? `${counts[key]} s` : "";
      parts[key].note.setAttribute("class", `chain-note ${counts[key] ? "is-lost" : ""}`);
    });

    const failing = ORDER.filter((key) => counts[key] > 0);
    status.textContent = !recent.length
      ? "waiting for pings"
      : failing.length
        ? `last 60 s: loss in ${failing.map((key) => `${segments.SEGMENTS[key].label} (${counts[key]} s)`).join(", ")}`
        : "last 60 s: clean";
    status.classList.toggle("is-lost", failing.length > 0);
  };

  const poll = async () => {
    if (document.hidden) return;
    try {
      const [hops, tel] = await Promise.all([fetchJson("/hopshistory?range=2m"), fetchJson("/history?range=2m")]);
      render(
        (hops.samples || []).map((sample) => segments.normalizeHop(sample, Math.max(1000, hops.bucket_width_ms || 0))),
        (tel.samples || []).map(segments.normalizeTel),
      );
    } catch (error) {
      console.warn("mini chain failed", error);
    }
  };

  document.addEventListener("visibilitychange", poll);
  poll();
  setInterval(poll, POLL_MS);
})();
