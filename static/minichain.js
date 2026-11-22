(() => {
  "use strict";

  const POLL_MS = 1000;
  const FETCH_RANGE = "75s";
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

  const beyondState = (states) => {
    for (const state of ["is-lost", "is-affected", "is-recent"]) {
      if (states.cloudflare === state || states.google === state) return state;
    }
    return "is-clean";
  };

  const render = (hops, tel) => {
    let routerIp = null;
    for (const point of hops) if (point.routerIp) routerIp = point.routerIp;
    const chain = segments.chainStatus(hops, tel, routerIp, Date.now());
    const states = { ...chain.states, beyond: beyondState(chain.states) };
    const routerPinged = chain.recent.some((point) => routerIp && point.hops[routerIp]);
    for (const key of ORDER) {
      const missing = !chain.recent.length || (key === "lan" && !routerPinged);
      const count = chain.counts[key];
      const tone = states[key] === "is-clean" ? "" : states[key];
      parts[key].line.setAttribute("class", `chain-seg ${missing ? "is-missing" : states[key]}`);
      parts[key].note.textContent = count && states[key] !== "is-affected" ? `${count} s` : "";
      parts[key].note.setAttribute("class", `chain-note ${tone}`);
    }
    status.textContent = chain.recent.length ? chain.text : "waiting for pings";
    status.className = `mini-chain-status tabular-num ${chain.recent.length ? chain.tone : ""}`;
  };

  let polling = false;
  const poll = async () => {
    if (document.hidden || polling) return;
    polling = true;
    try {
      const [hops, tel] = await Promise.all([fetchJson(`/hopshistory?range=${FETCH_RANGE}`), fetchJson(`/history?range=${FETCH_RANGE}`)]);
      render(
        (hops.samples || []).map((sample) => segments.normalizeHop(sample, Math.max(1000, hops.bucket_width_ms || 0))),
        (tel.samples || []).map(segments.normalizeTel),
      );
    } catch (error) {
      console.warn("mini chain failed", error);
    } finally {
      polling = false;
    }
  };

  document.addEventListener("visibilitychange", poll);
  poll();
  setInterval(poll, POLL_MS);
})();
