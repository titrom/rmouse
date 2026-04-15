<script lang="ts">
  import { onMount, onDestroy } from "svelte";
  import { slide } from "svelte/transition";
  import {
    EnumerateMonitors, IsRunning, LoadConfig, SaveConfig, Start, Stop,
  } from "../wailsjs/go/main/App";
  import { EventsOn, EventsOff } from "../wailsjs/runtime/runtime";
  import { main } from "../wailsjs/go/models";

  // --- config + connection state -----------------------------------------
  let addr = "127.0.0.1:24242";
  let token = "";
  let name = "";
  let pingMs = 2000;
  let relayAddr = "";
  let session = "";

  let running = false;
  let busy = false;
  // High-level state shown as a status dot in the header.
  type ConnState = "idle" | "connecting" | "connected" | "disconnected";
  let state: ConnState = "idle";
  let assignedName = "";
  let lastErr = "";
  let retryMs = 0;
  let pongIntervalMs = 0;
  let lastPongAt = 0;
  let grabbing = false;
  let error = "";
  let advancedOpen = false;

  // --- logs ---------------------------------------------------------------
  type LogEntry = { ts: string; level: "info" | "warn" | "error"; msg: string };
  let logs: LogEntry[] = [];
  let logBox: HTMLDivElement;
  function tsStr() { return new Date().toTimeString().slice(0, 8); }
  function log(level: LogEntry["level"], msg: string) {
    logs = [...logs.slice(-199), { ts: tsStr(), level, msg }];
    queueMicrotask(() => { if (logBox) logBox.scrollTop = logBox.scrollHeight; });
  }
  function clearLogs() { logs = []; }

  // --- monitor stage ------------------------------------------------------
  let monitors: main.MonitorDTO[] = [];
  let monitorsLive = false;
  let stage: HTMLDivElement;
  let stageW = 0, stageH = 0;
  let zoom = 1;

  $: primary = monitors.find(m => m.primary) ?? monitors[0];
  $: originX = primary ? primary.x + primary.w / 2 : 0;
  $: originY = primary ? primary.y + primary.h / 2 : 0;
  $: bbox = monitors.length ? monitors.reduce((b, m) => ({
    minX: Math.min(b.minX, m.x - originX),
    maxX: Math.max(b.maxX, m.x - originX + m.w),
    minY: Math.min(b.minY, m.y - originY),
    maxY: Math.max(b.maxY, m.y - originY + m.h),
  }), { minX: Infinity, maxX: -Infinity, minY: Infinity, maxY: -Infinity })
    : { minX: 0, maxX: 0, minY: 0, maxY: 0 };
  // Symmetric around primary's center so it always sits in the middle.
  $: spanX = Math.max(Math.abs(bbox.minX), Math.abs(bbox.maxX)) * 2;
  $: spanY = Math.max(Math.abs(bbox.minY), Math.abs(bbox.maxY)) * 2;
  $: fitScale = (stageW > 0 && spanX > 0 && spanY > 0)
    ? Math.min((stageW - 32) / spanX, (stageH - 32) / spanY)
    : 0;
  $: smallestW = monitors.length ? Math.min(...monitors.map(m => m.w)) : 0;
  $: smallestH = monitors.length ? Math.min(...monitors.map(m => m.h)) : 0;
  $: minZoom = (fitScale > 0 && smallestW > 0 && smallestH > 0)
    ? Math.max(64 / smallestW, 30 / smallestH) / fitScale
    : 0.2;
  $: scale = fitScale * zoom;
  $: if (zoom < minZoom) zoom = minZoom;
  $: if (zoom > 5) zoom = 5;
  const monGap = 4;

  function onWheel(ev: WheelEvent) {
    ev.preventDefault();
    const f = ev.deltaY < 0 ? 1.1 : 1 / 1.1;
    zoom = Math.max(minZoom, Math.min(5, zoom * f));
  }
  function resetZoom() { zoom = 1; }

  // Strip Windows device-path prefix from monitor names.
  function monName(m: main.MonitorDTO): string {
    const n = (m.name || "").replace(/^\\\\[.?]\\/, "");
    return n || `#${m.id}`;
  }

  // --- actions ------------------------------------------------------------
  async function refresh() {
    const cfg = await LoadConfig();
    addr = cfg.addr || "127.0.0.1:24242";
    token = cfg.token || "";
    name = cfg.name || "";
    pingMs = cfg.pingMs || 2000;
    relayAddr = cfg.relayAddr || "";
    session = cfg.session || "";
    running = await IsRunning();
    state = running ? "connecting" : "idle";
    try { monitors = await EnumerateMonitors(); } catch { monitors = []; }
  }

  async function connect() {
    if (busy || running) return;
    if (!token) { error = "token is required"; return; }
    error = "";
    lastErr = "";
    state = "connecting";
    running = true;
    busy = true;
    try {
      const cfg = new main.ConfigDTO({ addr, token, name, pingMs, relayAddr, session });
      await SaveConfig(cfg);
      await Start(cfg);
      const r = await IsRunning();
      if (!r) { running = false; state = "idle"; }
    } catch (e: any) {
      running = false;
      state = "idle";
      error = String(e?.message ?? e);
      log("error", `start failed: ${error}`);
    } finally { busy = false; }
  }

  async function disconnect() {
    if (busy) return;
    busy = true;
    error = "";
    try { await Stop(); }
    catch (e: any) { error = String(e?.message ?? e); }
    finally {
      running = await IsRunning();
      state = running ? "connecting" : "idle";
      busy = false;
    }
  }

  // --- events -------------------------------------------------------------
  function onStatus(p: any) {
    state = (p?.state as ConnState) || "idle";
    if (p?.assignedName) assignedName = p.assignedName;
    if (p?.err) {
      lastErr = p.err;
      log("error", `disconnected: ${p.err}`);
    } else if (p?.state === "connected") {
      log("info", `connected as ${p.assignedName ?? "?"}`);
    } else if (p?.state === "connecting") {
      log("info", "connecting…");
    } else if (p?.state === "disconnected") {
      log("warn", "disconnected");
    }
    retryMs = p?.retryMs || 0;
  }
  function onMonitors(p: any) {
    monitors = p?.monitors || [];
    monitorsLive = !!p?.live;
    if (p?.live) log("info", `monitors changed (${monitors.length})`);
  }
  function onPong() {
    const now = Date.now();
    pongIntervalMs = lastPongAt ? now - lastPongAt : 0;
    lastPongAt = now;
  }
  function onHotplugUnavailable(p: any) { log("warn", `hotplug unavailable: ${p?.err ?? ""}`); }
  function onInjectorUnavailable(p: any) { log("warn", `injector unavailable: ${p?.err ?? ""}`); }
  function onGrab(p: any) { grabbing = !!p?.on; log("info", `grab ${p?.on ? "on" : "off"}`); }
  function onStopped() {
    running = false;
    state = "idle";
    assignedName = "";
    grabbing = false;
    log("info", "stopped");
  }
  function onFatal(msg: any) {
    const m = typeof msg === "string" ? msg : String(msg);
    error = m;
    log("error", `fatal: ${m}`);
    running = false;
    state = "idle";
  }

  let stageObserver: ResizeObserver | null = null;
  onMount(() => {
    refresh();
    EventsOn("rmouse:status", onStatus);
    EventsOn("rmouse:monitors", onMonitors);
    EventsOn("rmouse:pong", onPong);
    EventsOn("rmouse:hotplugUnavailable", onHotplugUnavailable);
    EventsOn("rmouse:injectorUnavailable", onInjectorUnavailable);
    EventsOn("rmouse:grab", onGrab);
    EventsOn("rmouse:stopped", onStopped);
    EventsOn("rmouse:fatal", onFatal);
    if (stage) {
      stageObserver = new ResizeObserver(() => {
        stageW = stage.clientWidth; stageH = stage.clientHeight;
      });
      stageObserver.observe(stage);
    }
  });
  onDestroy(() => {
    EventsOff("rmouse:status");
    EventsOff("rmouse:monitors");
    EventsOff("rmouse:pong");
    EventsOff("rmouse:hotplugUnavailable");
    EventsOff("rmouse:injectorUnavailable");
    EventsOff("rmouse:grab");
    EventsOff("rmouse:stopped");
    EventsOff("rmouse:fatal");
    stageObserver?.disconnect();
  });
</script>

<main class="app">
  <div class="row-top">
    <section class="panel panel-connection">
      <header class="panel-head">
        <h2>Connection</h2>
        <span class="status {state}">
          <span class="dot"></span>
          {state}
        </span>
      </header>

      {#if assignedName && state === "connected"}
        <p class="detail">as <strong>{assignedName}</strong></p>
      {:else if retryMs > 0 && state !== "connected"}
        <p class="detail">retry in {(retryMs / 1000).toFixed(1)}s</p>
      {/if}

      <div class="field">
        <label for="addr">Server</label>
        <input id="addr" type="text" bind:value={addr} placeholder="host:port" disabled={running} />
      </div>

      <div class="field">
        <label for="token">Token</label>
        <input id="token" type="password" bind:value={token} placeholder="shared secret" disabled={running} />
      </div>

      <div class="field">
        <label for="name">Name</label>
        <input id="name" type="text" bind:value={name} placeholder="(hostname)" disabled={running} />
      </div>

      <button class="adv-toggle" on:click={() => advancedOpen = !advancedOpen}
              aria-expanded={advancedOpen}>
        {advancedOpen ? "▾" : "▸"} Advanced
      </button>

      {#if error || lastErr}
        <p class="error">{error || lastErr}</p>
      {/if}

      <div class="actions">
        {#if running}
          <button class="btn danger" on:click={disconnect} disabled={busy}>Disconnect</button>
        {:else}
          <button class="btn primary" on:click={connect} disabled={busy || !token}>Connect</button>
        {/if}
      </div>
    </section>

    {#if advancedOpen}
      <section class="panel panel-advanced"
               transition:slide={{ axis: "x", duration: 200 }}>
        <header class="panel-head">
          <h2>Advanced</h2>
          <button class="link-btn" on:click={() => advancedOpen = false}>Close</button>
        </header>
        <div class="field">
          <label for="ping">Ping interval (ms)</label>
          <input id="ping" type="number" bind:value={pingMs} min="100" disabled={running} />
        </div>
        <div class="field">
          <label for="relay">Relay address</label>
          <input id="relay" type="text" bind:value={relayAddr} placeholder="(optional)" disabled={running} />
        </div>
        <div class="field">
          <label for="session">Session</label>
          <input id="session" type="text" bind:value={session} placeholder="(required with relay)" disabled={running} />
        </div>
      </section>
    {/if}

    <section class="panel panel-logs">
      <header class="panel-head">
        <h2>Logs</h2>
        <button class="link-btn" on:click={clearLogs} disabled={logs.length === 0}>Clear</button>
      </header>

      <div class="info">
        {#if pongIntervalMs > 0}
          <div class="k">Pong</div><div class="v">{pongIntervalMs} ms</div>
        {/if}
      </div>

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

  <section class="panel panel-monitors">
    <header class="panel-head">
      <h2>Monitors{monitorsLive ? " · live" : ""}</h2>
      <div class="zoom-ctrl" style="display:inline-flex; gap:2px; align-items:center;">
        <button class="link-btn" on:click={() => zoom = Math.max(minZoom, zoom / 1.2)} disabled={zoom <= minZoom} title="Zoom out">−</button>
        <button class="link-btn" on:click={resetZoom} title="Reset zoom" style="min-width:42px;">{Math.round(zoom * 100)}%</button>
        <button class="link-btn" on:click={() => zoom = Math.min(5, zoom * 1.2)} disabled={zoom >= 5} title="Zoom in">+</button>
      </div>
    </header>
    <div class="stage" class:grabbing bind:this={stage} on:wheel={onWheel}>
      {#if monitors.length === 0}
        <p class="log-empty stage-empty">No monitors detected.</p>
      {:else}
        {#each monitors as m (m.id)}
          <div
            class="mon"
            class:primary={m.primary}
            style="
              left:   {(m.x - originX) * scale + stageW / 2 + monGap / 2}px;
              top:    {(m.y - originY) * scale + stageH / 2 + monGap / 2}px;
              width:  {Math.max(0, m.w * scale - monGap)}px;
              height: {Math.max(0, m.h * scale - monGap)}px;
            "
          >
            <span class="mon-label">
              <strong>{monName(m)}</strong>
              <span class="mon-res">{m.w}×{m.h}</span>
            </span>
          </div>
        {/each}
      {/if}
    </div>
  </section>
</main>
