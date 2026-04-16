<script lang="ts">
  import { onMount, onDestroy } from "svelte";
  import {
    LoadConfig, SaveConfig, Start, Stop, IsRunning, CertFingerprint,
    GetServerMonitors, GetPlacements, SetClientPlacement,
  } from "../wailsjs/go/main/App";
  import { EventsOn, EventsOff } from "../wailsjs/runtime/runtime";
  import { main } from "../wailsjs/go/models";

  let addr = "0.0.0.0:24242";
  let token = "";
  let relayAddr = "";
  let session = "";
  let fingerprint = "";
  let running = false;
  let busy = false;
  let status = "stopped";
  let statusDetail = "";
  let error = "";

  type LogEntry = { ts: string; level: "info" | "warn" | "error"; msg: string };
  let logs: LogEntry[] = [];
  let logBox: HTMLDivElement;

  // --- screen / monitors --------------------------------------------------
  let monitors: main.MonitorDTO[] = [];
  let stage: HTMLDivElement;
  let stageW = 0, stageH = 0;
  // User zoom multiplier on top of the auto fit-to-stage scale.
  let zoom = 1;

  // Two flavors of clients on the stage:
  // - stub: synthetic, free-form drag/snap, never persists.
  // - live: real connected client; drag/snap calls SetClientPlacement and
  //   the local offset mirrors the router's cell-grid formula so the
  //   visual matches the actual mouse-transition geometry.
  type StubClient = { kind: "stub"; name: string; offX: number; offY: number; monitors: main.MonitorDTO[] };
  type LiveClient = {
    kind: "live"; name: string; ids: string[]; remote: string;
    col: number; row: number; offX: number; offY: number; monitors: main.MonitorDTO[];
  };
  type AnyClient = StubClient | LiveClient;
  let stubClients: StubClient[] = [];
  let liveClients: LiveClient[] = [];

  function findClient(name: string): AnyClient | undefined {
    return liveClients.find(c => c.name === name) ?? stubClients.find(c => c.name === name);
  }
  function bumpClients() {
    liveClients = liveClients;
    stubClients = stubClients;
  }
  // Mirror internal/app/server/router.go::applyPlacementAt so the visual
  // offset matches where the router will actually send the cursor.
  function liveOffsetFromCell(c: LiveClient) {
    if (monitors.length === 0 || c.monitors.length === 0) return;
    const sw = serverBbox.maxX - serverBbox.minX || 1;
    const sh = serverBbox.maxY - serverBbox.minY || 1;
    const cMinX = Math.min(...c.monitors.map(m => m.x));
    const cMinY = Math.min(...c.monitors.map(m => m.y));
    c.offX = serverBbox.minX + c.col * sw - cMinX;
    c.offY = serverBbox.minY + c.row * sh - cMinY;
  }
  let nextStubId = 1;
  function addTestClient() {
    const i = nextStubId++;
    const mons: main.MonitorDTO[] = [
      { id: 1, x: 0,    y: 0, w: 1920, h: 1080, primary: true,  name: "MAIN" } as main.MonitorDTO,
      { id: 2, x: 1920, y: 0, w: 1280, h: 1024, primary: false, name: "AUX"  } as main.MonitorDTO,
    ];
    // Drop new clients to the right of the server, walking further right /
    // down per index. If the chosen spot collides with a server screen,
    // bump it right until clear.
    const sw = serverBbox.maxX - serverBbox.minX || 1920;
    let offX = serverBbox.maxX + 200 + ((i - 1) % 3) * (sw + 200);
    let offY = serverBbox.minY + Math.floor((i - 1) / 3) * 1200;
    const newName = `test-${i}`;
    while (clientCollides(mons, offX, offY, newName)) offX += 200;
    stubClients = [...stubClients, { kind: "stub", name: newName, offX, offY, monitors: mons }];
  }
  function removeTestClient(name: string) {
    stubClients = stubClients.filter(c => c.name !== name);
  }

  // Collision: any moved client monitor overlapping any server monitor?
  // Used both for initial placement and for clamping the drag.
  function rectsOverlap(ax: number, ay: number, aw: number, ah: number,
                        bx: number, by: number, bw: number, bh: number): boolean {
    return ax < bx + bw && ax + aw > bx && ay < by + bh && ay + ah > by;
  }
  // Returns true if the moved client's monitors would overlap any server
  // monitor or any *other* client's monitors (the client being moved is
  // skipped so it doesn't collide with itself).
  function clientCollides(mons: main.MonitorDTO[], offX: number, offY: number, excludeName?: string): boolean {
    for (const m of mons) {
      const ax = m.x + offX, ay = m.y + offY;
      for (const s of monitors) {
        if (rectsOverlap(ax, ay, m.w, m.h, s.x, s.y, s.w, s.h)) return true;
      }
      for (const oc of [...liveClients, ...stubClients]) {
        if (oc.name === excludeName) continue;
        for (const om of oc.monitors) {
          if (rectsOverlap(ax, ay, m.w, m.h, om.x + oc.offX, om.y + oc.offY, om.w, om.h)) return true;
        }
      }
    }
    return false;
  }

  // --- drag ---------------------------------------------------------------
  let dragging: {
    client: AnyClient; startMouseX: number; startMouseY: number;
    startOffX: number; startOffY: number;
  } | null = null;
  function onMonMouseDown(ev: MouseEvent, d: Drawable) {
    if (d.kind !== "client" || !d.clientName) return;
    const c = findClient(d.clientName);
    if (!c) return;
    ev.preventDefault();
    dragging = {
      client: c, startMouseX: ev.clientX, startMouseY: ev.clientY,
      startOffX: c.offX, startOffY: c.offY,
    };
    window.addEventListener("mousemove", onDragMove);
    window.addEventListener("mouseup", onDragEnd);
  }
  function onDragMove(ev: MouseEvent) {
    if (!dragging || scale <= 0) return;
    const c = dragging.client;
    const wantX = dragging.startOffX + (ev.clientX - dragging.startMouseX) / scale;
    const wantY = dragging.startOffY + (ev.clientY - dragging.startMouseY) / scale;
    // Axis-separated: try X first, then Y, so the client slides along
    // server / other-client edges instead of locking up on contact.
    let nextX = wantX;
    if (clientCollides(c.monitors, nextX, c.offY, c.name)) nextX = c.offX;
    let nextY = wantY;
    if (clientCollides(c.monitors, nextX, nextY, c.name)) nextY = c.offY;
    c.offX = nextX;
    c.offY = nextY;
    bumpClients();
  }
  function onDragEnd() {
    const c = dragging?.client;
    dragging = null;
    window.removeEventListener("mousemove", onDragMove);
    window.removeEventListener("mouseup", onDragEnd);
    if (c) snapClient(c);
  }

  // Names of clients currently animating into their snapped position.
  // Used by the template to enable a CSS transition only for the brief
  // settle window — we don't want zoom changes or drag updates to animate.
  let snapping = new Set<string>();
  // Snap a dropped client flush against an edge or corner of one of the
  // server's *individual* monitors — different-sized screens contribute
  // their own edges, so a small secondary monitor offers its own snap
  // targets distinct from the larger primary's. We build all candidates,
  // pick the nearest non-colliding, and re-fit zoom afterwards so the
  // newly-extended layout stays visible.
  function snapClient(c: AnyClient) {
    if (monitors.length === 0) return;
    const cMinX = Math.min(...c.monitors.map(m => m.x));
    const cMaxX = Math.max(...c.monitors.map(m => m.x + m.w));
    const cMinY = Math.min(...c.monitors.map(m => m.y));
    const cMaxY = Math.max(...c.monitors.map(m => m.y + m.h));

    type Anchor = { x: number; y: number };
    const anchors: Anchor[] = [];
    // Targets to snap against: every server monitor + every monitor of
    // every *other* client. World coordinates of each target rect.
    const targets: { x: number; y: number; w: number; h: number }[] = [];
    for (const s of monitors) targets.push({ x: s.x, y: s.y, w: s.w, h: s.h });
    for (const oc of [...liveClients, ...stubClients]) {
      if (oc.name === c.name) continue;
      for (const om of oc.monitors) {
        targets.push({ x: om.x + oc.offX, y: om.y + oc.offY, w: om.w, h: om.h });
      }
    }
    for (const t of targets) {
      const tLeft = t.x, tRight = t.x + t.w, tTop = t.y, tBot = t.y + t.h;
      const right  = tRight - cMinX;
      const left   = tLeft  - cMaxX;
      const bottom = tBot   - cMinY;
      const top    = tTop   - cMaxY;
      const freeY = Math.min(tBot   - cMinY - 1, Math.max(tTop  - cMaxY + 1, c.offY));
      const freeX = Math.min(tRight - cMinX - 1, Math.max(tLeft - cMaxX + 1, c.offX));
      anchors.push(
        { x: right, y: freeY }, { x: left,  y: freeY },
        { x: freeX, y: top   }, { x: freeX, y: bottom },
        { x: right, y: top   }, { x: right, y: bottom },
        { x: left,  y: top   }, { x: left,  y: bottom },
      );
    }

    const scored = anchors
      .map(p => ({ p, d: (p.x - c.offX) ** 2 + (p.y - c.offY) ** 2 }))
      .sort((a, b) => a.d - b.d);

    for (const { p } of scored) {
      if (clientCollides(c.monitors, p.x, p.y, c.name)) continue;
      snapping.add(c.name);
      snapping = snapping;
      c.offX = p.x;
      c.offY = p.y;
      if (zoom !== 1) zoom = 1;
      // Live clients persist via the router's (col, row) cell grid. We
      // round the snapped offset back into a cell, push it to Go, and
      // realign offX/offY to the router's exact formula so what the
      // user sees is what the cursor will follow.
      if (c.kind === "live") {
        const sw = serverBbox.maxX - serverBbox.minX || 1;
        const sh = serverBbox.maxY - serverBbox.minY || 1;
        let col = Math.round((c.offX + cMinX - serverBbox.minX) / sw);
        let row = Math.round((c.offY + cMinY - serverBbox.minY) / sh);
        if (col === 0 && row === 0) col = 1; // (0,0) is reserved for the server.
        c.col = col;
        c.row = row;
        liveOffsetFromCell(c);
        SetClientPlacement(c.name, col, row).catch((err) => {
          log("error", `placement: ${err}`);
        });
      }
      bumpClients();
      setTimeout(() => { snapping.delete(c.name); snapping = snapping; }, 320);
      return;
    }
  }

  // Server bbox — used both as the origin reference and as the cell size
  // for client placement (one cell == one server bbox).
  $: primary = monitors.find(m => m.primary) ?? monitors[0];
  $: originX = primary ? primary.x + primary.w / 2 : 0;
  $: originY = primary ? primary.y + primary.h / 2 : 0;
  $: serverBbox = monitors.length ? monitors.reduce((b, m) => ({
    minX: Math.min(b.minX, m.x), maxX: Math.max(b.maxX, m.x + m.w),
    minY: Math.min(b.minY, m.y), maxY: Math.max(b.maxY, m.y + m.h),
  }), { minX: Infinity, maxX: -Infinity, minY: Infinity, maxY: -Infinity })
    : { minX: 0, maxX: 0, minY: 0, maxY: 0 };
  $: serverW = Math.max(1, serverBbox.maxX - serverBbox.minX);
  $: serverH = Math.max(1, serverBbox.maxY - serverBbox.minY);

  // Flatten everything into one render list with world coords (relative
  // to origin = server primary center). Live clients are rendered like
  // stubs but are visually distinct (handled via class:live in template).
  type Drawable = {
    key: string; kind: "server" | "client"; subkind?: "stub" | "live";
    clientName?: string;
    x: number; y: number; w: number; h: number; primary: boolean; label: string;
  };
  $: drawables = [
    ...monitors.map<Drawable>(m => ({
      key: `s-${m.id}`, kind: "server",
      x: m.x - originX, y: m.y - originY, w: m.w, h: m.h,
      primary: m.primary, label: monName(m),
    })),
    ...liveClients.flatMap<Drawable>(c => c.monitors.map(m => ({
      key: `live-${c.name}-${m.id}`, kind: "client", subkind: "live",
      clientName: c.name,
      x: m.x + c.offX - originX,
      y: m.y + c.offY - originY,
      w: m.w, h: m.h, primary: false,
      label: `${c.name}/${monName(m)}`,
    }))),
    ...stubClients.flatMap<Drawable>(c => c.monitors.map(m => ({
      key: `stub-${c.name}-${m.id}`, kind: "client", subkind: "stub",
      clientName: c.name,
      x: m.x + c.offX - originX,
      y: m.y + c.offY - originY,
      w: m.w, h: m.h, primary: false,
      label: `${c.name}/${monName(m)}`,
    }))),
  ];

  // True whenever there's at least one client (live or stub) on the stage.
  $: anyClient = liveClients.length + stubClients.length > 0;

  // bbox over everything (already in origin-relative coords).
  $: bbox = drawables.length ? drawables.reduce((b, d) => ({
    minX: Math.min(b.minX, d.x), maxX: Math.max(b.maxX, d.x + d.w),
    minY: Math.min(b.minY, d.y), maxY: Math.max(b.maxY, d.y + d.h),
  }), { minX: Infinity, maxX: -Infinity, minY: Infinity, maxY: -Infinity })
    : { minX: 0, maxX: 0, minY: 0, maxY: 0 };
  // Span: with no clients we lock primary to dead-center by symmetrizing
  // around origin. With clients, span is just the natural bbox so the whole
  // layout sits centered (and primary may drift off-center).
  $: spanX = !anyClient
    ? Math.max(Math.abs(bbox.minX), Math.abs(bbox.maxX)) * 2
    : (bbox.maxX - bbox.minX);
  $: spanY = !anyClient
    ? Math.max(Math.abs(bbox.minY), Math.abs(bbox.maxY)) * 2
    : (bbox.maxY - bbox.minY);
  $: fitScale = (stageW > 0 && spanX > 0 && spanY > 0)
    ? Math.min((stageW - 32) / spanX, (stageH - 32) / spanY)
    : 0;
  // Smallest monitor in world units — used to decide how far we can zoom
  // out before its rendered rectangle would clip the label.
  $: smallestW = drawables.length ? Math.min(...drawables.map(d => d.w)) : 0;
  $: smallestH = drawables.length ? Math.min(...drawables.map(d => d.h)) : 0;
  // Dynamic minimum zoom: the scale at which the smallest monitor still
  // fits its label, expressed as a multiplier on top of fitScale.
  $: minZoom = (fitScale > 0 && smallestW > 0 && smallestH > 0)
    ? Math.max(minLabelW / smallestW, minLabelH / smallestH) / fitScale
    : 0.2;
  $: scale = fitScale * zoom;
  // Keep the user's zoom within the dynamic bounds — if a newly added
  // client shrinks fitScale enough that the current zoom is below the
  // new floor, snap it up.
  $: if (zoom < minZoom) zoom = minZoom;
  $: if (zoom > maxZoom) zoom = maxZoom;
  // Pixel offset that maps world (origin-relative) coords into the stage.
  // No clients: origin → stage center. With clients: bbox center → stage center.
  $: offsetX = !anyClient
    ? stageW / 2
    : stageW / 2 - ((bbox.minX + bbox.maxX) / 2) * scale;
  $: offsetY = !anyClient
    ? stageH / 2
    : stageH / 2 - ((bbox.minY + bbox.maxY) / 2) * scale;
  // Visual gap between adjacent monitors — each rectangle is inset by
  // gap/2 on every side so two touching screens show a thin breathing
  // space without distorting their relative positions.
  const monGap = 4;

  async function loadMonitors() {
    try { monitors = await GetServerMonitors(); } catch { monitors = []; }
  }
  async function loadPlacements() {
    try {
      const ps = await GetPlacements();
      // Seed entries with no monitors yet — they'll be filled in by the
      // 'connected' event when the client appears.
      const seen = new Set(liveClients.map(c => c.name));
      for (const p of ps) {
        if (seen.has(p.name)) continue;
        liveClients.push({
          kind: "live", name: p.name, ids: [], remote: "",
          col: p.col, row: p.row, offX: 0, offY: 0, monitors: [],
        });
      }
      bumpClients();
    } catch (e) { /* placements may not exist yet */ }
  }
  // When the server's own monitor layout changes, every live client's
  // offset (which is anchored to serverMin/serverW/serverH) becomes stale.
  // Recompute from the stored (col, row). NOTE: depend ONLY on `monitors`,
  // not `liveClients` — otherwise a drag-induced bump would re-trigger
  // this and snap the offset back to the cell on every mousemove.
  let lastMonsKey = "";
  $: {
    const key = monitors.map(m => `${m.id}:${m.x},${m.y},${m.w},${m.h}`).join("|");
    if (key !== lastMonsKey) {
      lastMonsKey = key;
      if (monitors.length > 0) {
        for (const c of liveClients) liveOffsetFromCell(c);
        bumpClients();
      }
    }
  }
  const maxZoom = 5;
  // Minimum on-screen size a monitor rectangle must keep so its label
  // (name + WxH on two lines) doesn't get clipped. Tuned to the .mon-label
  // font sizes; tweak together with the CSS.
  const minLabelW = 64;
  const minLabelH = 30;
  function onWheel(ev: WheelEvent) {
    ev.preventDefault();
    const f = ev.deltaY < 0 ? 1.1 : 1 / 1.1;
    zoom = Math.max(minZoom, Math.min(maxZoom, zoom * f));
  }
  function resetZoom() { zoom = 1; }

  function onMonitorsEvent(p: any) {
    if (p?.monitors) monitors = p.monitors;
  }
  // Windows reports device names like "\\.\DISPLAY2" — strip the device-path
  // prefix so the label reads as just "DISPLAY2".
  function monName(m: main.MonitorDTO): string {
    const n = (m.name || "").replace(/^\\\\[.?]\\/, "");
    return n || `#${m.id}`;
  }

  function ts() {
    const d = new Date();
    return d.toTimeString().slice(0, 8);
  }
  function log(level: LogEntry["level"], msg: string) {
    logs = [...logs.slice(-199), { ts: ts(), level, msg }];
    queueMicrotask(() => { if (logBox) logBox.scrollTop = logBox.scrollHeight; });
  }
  function clearLogs() { logs = []; }
  function spamTestLogs() {
    const levels: LogEntry["level"][] = ["info", "warn", "error"];
    for (let i = 0; i < 50; i++) {
      log(levels[i % 3], `test entry #${logs.length + 1} — lorem ipsum dolor sit amet`);
    }
  }

  async function refresh() {
    const cfg = await LoadConfig();
    addr = cfg.addr || "0.0.0.0:24242";
    token = cfg.token || "";
    relayAddr = cfg.relayAddr || "";
    session = cfg.session || "";
    running = await IsRunning();
    status = running ? "running" : "stopped";
    try { fingerprint = await CertFingerprint(); } catch (e) { /* cert lazy */ }
  }

  async function start() {
    if (busy || running) return;
    if (!token) { error = "token is required"; return; }
    error = "";
    statusDetail = "";
    // Set the optimistic state BEFORE the await — otherwise the Go side
    // may emit `rmouse:listening` (status → "running") within the 50ms
    // sleep that Start does before returning, and we'd then clobber that
    // back to "starting" here, leaving the UI stuck.
    status = "starting";
    running = true;
    busy = true;
    try {
      await SaveConfig(new main.ConfigDTO({ addr, token, relayAddr, session }));
      await Start(new main.ConfigDTO({ addr, token, relayAddr, session }));
      // Reconcile against ground truth in case Go failed silently between
      // the optimistic flip above and now.
      const r = await IsRunning();
      if (!r) { running = false; status = "stopped"; }
    } catch (e: any) {
      running = false;
      status = "stopped";
      statusDetail = "";
      error = String(e?.message ?? e);
    } finally { busy = false; }
  }

  async function stop() {
    if (busy) return;
    busy = true;
    error = "";
    try {
      await Stop();
    } catch (e: any) {
      error = String(e?.message ?? e);
    } finally {
      // Trust the Go side: IsRunning is the source of truth after Stop.
      running = await IsRunning();
      status = running ? "running" : "stopped";
      if (!running) statusDetail = "";
      busy = false;
    }
  }

  function copyFp() { if (fingerprint) navigator.clipboard.writeText(fingerprint); }

  function onListening(p: any) {
    status = "running";
    if (p?.addr) { statusDetail = `bound ${p.addr}`; log("info", `listening on ${p.addr}`); }
    else if (p?.relay) { statusDetail = `relay ${p.relay} · ${p.session}`; log("info", `serving via relay ${p.relay} (${p.session})`); }
  }
  function onStopped() { running = false; status = "stopped"; statusDetail = ""; log("info", "server stopped"); }
  function onFatal(msg: string) { error = msg; running = false; status = "stopped"; log("error", msg); }
  function onClient(p: any) {
    const who = p?.name ? `${p.name} (${p.remote ?? p.id})` : (p?.remote ?? p?.id ?? "?");
    switch (p?.state) {
      case "connected":       log("info", `+ ${who} connected`); upsertLive(p); break;
      case "disconnected":    log(p.err ? "warn" : "info", `- ${who} disconnected${p.err ? ": " + p.err : ""}`); removeLiveId(p); break;
      case "bye":             log("info", `${who} bye${p.reason ? ": " + p.reason : ""}`); break;
      case "monitorsChanged": log("info", `${who} monitors changed`); upsertLive(p); break;
    }
  }
  function onPlaced(p: any) {
    log("info", `placed ${p?.name ?? p?.id} → (${p?.col}, ${p?.row})`);
    if (!p?.name) return;
    let c = liveClients.find(c => c.name === p.name);
    if (!c) {
      // Placement arrived before connection — seed an empty entry; offset
      // is meaningless until monitors arrive via 'connected'.
      c = { kind: "live", name: p.name, ids: [], remote: "",
            col: p.col, row: p.row, offX: 0, offY: 0, monitors: [] };
      liveClients = [...liveClients, c];
    } else {
      c.col = p.col; c.row = p.row;
      liveOffsetFromCell(c);
    }
    bumpClients();
  }

  // Add or update a live client from a 'connected' / 'monitorsChanged'
  // event. Multiple connections from the same name share the entry — we
  // track every conn id but render a single rectangle per name.
  function upsertLive(p: any) {
    if (!p?.name) return;
    const mons = Array.isArray(p?.monitors) ? p.monitors as main.MonitorDTO[] : [];
    let c = liveClients.find(c => c.name === p.name);
    if (!c) {
      c = { kind: "live", name: p.name, ids: [], remote: p.remote || "",
            col: 0, row: 0, offX: 0, offY: 0, monitors: mons };
      liveClients = [...liveClients, c];
    } else {
      c.monitors = mons;
      c.remote = p.remote || c.remote;
    }
    if (p.id && !c.ids.includes(p.id)) c.ids.push(p.id);
    liveOffsetFromCell(c);
    bumpClients();
  }
  function removeLiveId(p: any) {
    if (!p?.name || !p?.id) return;
    const c = liveClients.find(c => c.name === p.name);
    if (!c) return;
    c.ids = c.ids.filter(x => x !== p.id);
    if (c.ids.length === 0) liveClients = liveClients.filter(x => x !== c);
    bumpClients();
  }
  function onRecvErr(p: any) { log("warn", `recv ${p?.name ?? p?.id}: ${p?.err}`); }

  let stageObserver: ResizeObserver | null = null;
  onMount(() => {
    refresh();
    loadMonitors();
    loadPlacements();
    EventsOn("rmouse:listening", onListening);
    EventsOn("rmouse:stopped", onStopped);
    EventsOn("rmouse:fatal", onFatal);
    EventsOn("rmouse:client", onClient);
    EventsOn("rmouse:clientPlaced", onPlaced);
    EventsOn("rmouse:recvError", onRecvErr);
    EventsOn("rmouse:serverMonitors", onMonitorsEvent);
    if (stage) {
      stageObserver = new ResizeObserver(() => {
        stageW = stage.clientWidth;
        stageH = stage.clientHeight;
      });
      stageObserver.observe(stage);
    }
  });
  onDestroy(() => {
    EventsOff("rmouse:listening");
    EventsOff("rmouse:stopped");
    EventsOff("rmouse:fatal");
    EventsOff("rmouse:client");
    EventsOff("rmouse:clientPlaced");
    EventsOff("rmouse:recvError");
    EventsOff("rmouse:serverMonitors");
    stageObserver?.disconnect();
  });
</script>

<main class="app">
  <div class="row-top">
  <section class="panel panel-listener">
    <header class="panel-head">
      <h2>Listener</h2>
      <span class="status" class:on={running} class:starting={status === "starting"}>
        <span class="dot"></span>
        {status}
      </span>
    </header>

    {#if statusDetail}
      <p class="detail">{statusDetail}</p>
    {/if}

    <div class="field">
      <label for="addr">Bind address</label>
      <input id="addr" type="text" bind:value={addr} placeholder="0.0.0.0:24242" disabled={running} />
    </div>

    <div class="field">
      <label for="token">Token</label>
      <input id="token" type="password" bind:value={token} placeholder="shared secret" disabled={running} />
    </div>

    <details class="advanced">
      <summary>Relay (optional)</summary>
      <div class="field">
        <label for="relay">Relay address</label>
        <input id="relay" type="text" bind:value={relayAddr} placeholder="relay.example:24243" disabled={running} />
      </div>
      <div class="field">
        <label for="session">Session</label>
        <input id="session" type="text" bind:value={session} placeholder="session id" disabled={running} />
      </div>
    </details>

    <div class="field fp">
      <label>Cert fingerprint</label>
      <button class="fp-btn" on:click={copyFp} title="Click to copy">
        <code>{fingerprint || "—"}</code>
      </button>
    </div>

    {#if error}
      <p class="error">{error}</p>
    {/if}

    <div class="actions">
      {#if running}
        <button class="btn danger" on:click={stop} disabled={busy}>Stop</button>
      {:else}
        <button class="btn primary" on:click={start} disabled={busy || !token}>Start</button>
      {/if}
    </div>
  </section>

  <section class="panel panel-logs">
    <header class="panel-head">
      <h2>Logs</h2>
      <div class="zoom-ctrl">
        <button class="link-btn" on:click={spamTestLogs} title="Append 50 dummy entries to test scrolling">+50 test</button>
        <span class="sep"></span>
        <button class="link-btn" on:click={clearLogs} disabled={logs.length === 0}>Clear</button>
      </div>
    </header>
    <div class="log-box" bind:this={logBox}>
      {#if logs.length === 0}
        <p class="log-empty">No events yet.</p>
      {:else}
        {#each logs as l}
          <div class="log-row {l.level}">
            <span class="log-ts">{l.ts}</span>
            <span class="log-msg">{l.msg}</span>
          </div>
        {/each}
      {/if}
    </div>
  </section>
  </div>

  <section class="panel panel-screen">
    <header class="panel-head">
      <h2>Screen</h2>
      <div class="zoom-ctrl">
        <button class="link-btn" on:click={addTestClient} title="Add a stub client for layout testing">+ test client</button>
        <span class="sep"></span>
        <button class="link-btn" on:click={() => zoom = Math.max(minZoom, zoom / 1.2)} title="Zoom out" disabled={zoom <= minZoom}>−</button>
        <button class="link-btn" on:click={resetZoom} title="Reset zoom">{Math.round(zoom * 100)}%</button>
        <button class="link-btn" on:click={() => zoom = Math.min(maxZoom, zoom * 1.2)} title="Zoom in" disabled={zoom >= maxZoom}>+</button>
      </div>
    </header>
    <div class="stage" class:snapping={snapping.size > 0} bind:this={stage} on:wheel={onWheel}>
      {#if drawables.length === 0}
        <p class="log-empty stage-empty">No monitors detected.</p>
      {:else}
        {#each drawables as d (d.key)}
          <div
            class="mon mon-{d.kind}"
            class:primary={d.primary}
            class:live={d.subkind === "live"}
            class:stub={d.subkind === "stub"}
            class:dragging={dragging?.client.name === d.clientName}
            class:snapping={d.clientName ? snapping.has(d.clientName) : false}
            on:mousedown={(e) => onMonMouseDown(e, d)}
            style="
              left:   {d.x * scale + offsetX + monGap / 2}px;
              top:    {d.y * scale + offsetY + monGap / 2}px;
              width:  {Math.max(0, d.w * scale - monGap)}px;
              height: {Math.max(0, d.h * scale - monGap)}px;
            "
          >
            <span class="mon-label">
              <strong>{d.label}</strong>
              <span class="mon-res">{d.w}×{d.h}</span>
            </span>
            {#if d.subkind === "stub"}
              <button
                class="mon-close"
                on:mousedown|stopPropagation
                on:click|stopPropagation={() => d.clientName && removeTestClient(d.clientName)}
                title="Remove"
              >×</button>
            {/if}
          </div>
        {/each}
      {/if}
    </div>
  </section>
</main>
