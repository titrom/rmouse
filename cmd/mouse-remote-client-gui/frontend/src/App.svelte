<script lang="ts">
  import { onMount } from 'svelte'
  import {
    EnumerateMonitors,
    IsRunning,
    LoadConfig,
    SaveConfig,
    Start,
    Stop,
  } from '../wailsjs/go/main/App'
  import { EventsOn, Environment } from '../wailsjs/runtime/runtime'
  import type { main } from '../wailsjs/go/models'

  type Cfg = main.ConfigDTO
  type Mon = main.MonitorDTO

  let cfg: Cfg = {
    addr: '127.0.0.1:24242',
    token: '',
    name: '',
    pingMs: 2000,
    relayAddr: '',
    session: '',
  }

  let monitors: Mon[] = []
  let monitorsLive = false
  let state: 'idle' | 'connecting' | 'connected' | 'disconnected' = 'idle'
  let assignedName = ''
  let lastErr = ''
  let retryMs = 0
  let lastPongAt = 0
  let pingMs = 0
  let running = false
  let saving = false
  let logLines: { t: string; cls: string; msg: string }[] = []

  const LOG_MAX = 100

  function log(msg: string, cls = '') {
    const t = new Date().toLocaleTimeString(undefined, { hour12: false })
    logLines = [{ t, cls, msg }, ...logLines].slice(0, LOG_MAX)
  }

  onMount(async () => {
    try {
      const env = await Environment()
      if (env?.platform) {
        document.body.classList.add(`platform-${env.platform}`)
      }
    } catch {}
    cfg = await LoadConfig()
    try {
      monitors = await EnumerateMonitors()
    } catch (e: any) {
      log(`enumerate monitors: ${e}`, 'err')
    }
    running = await IsRunning()

    EventsOn('rmouse:status', (p: any) => {
      state = p.state
      if (p.assignedName) assignedName = p.assignedName
      if (p.err) {
        lastErr = p.err
        log(`disconnected: ${p.err}`, 'err')
      } else if (p.state === 'connected') {
        log(`connected as ${p.assignedName}`, 'ok')
      } else if (p.state === 'connecting') {
        log('connecting…')
      }
      retryMs = p.retryMs || 0
    })
    EventsOn('rmouse:monitors', (p: any) => {
      monitors = p.monitors
      monitorsLive = !!p.live
      if (p.live) log(`monitors changed (${p.monitors.length})`, 'ok')
    })
    EventsOn('rmouse:pong', (_p: any) => {
      const now = Date.now()
      pingMs = lastPongAt ? now - lastPongAt : 0
      lastPongAt = now
    })
    EventsOn('rmouse:hotplugUnavailable', (p: any) => {
      log(`hotplug unavailable: ${p.err}`, 'warn')
    })
    EventsOn('rmouse:stopped', () => {
      running = false
      state = 'idle'
      assignedName = ''
      log('stopped')
    })
    EventsOn('rmouse:fatal', (err: any) => {
      log(`fatal: ${err}`, 'err')
      running = false
    })
  })

  async function connect() {
    lastErr = ''
    saving = true
    try {
      await SaveConfig(cfg)
      await Start(cfg)
      running = true
    } catch (e: any) {
      log(`start failed: ${e}`, 'err')
    } finally {
      saving = false
    }
  }

  async function disconnect() {
    await Stop()
    running = false
  }

  $: layout = computeLayout(monitors)

  function computeLayout(mons: Mon[]) {
    if (!mons.length) return { viewBox: '0 0 100 100', rects: [] as Mon[] }
    let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity
    for (const m of mons) {
      if (m.x < minX) minX = m.x
      if (m.y < minY) minY = m.y
      if (m.x + m.w > maxX) maxX = m.x + m.w
      if (m.y + m.h > maxY) maxY = m.y + m.h
    }
    const pad = 40
    minX -= pad; minY -= pad; maxX += pad; maxY += pad
    return {
      viewBox: `${minX} ${minY} ${maxX - minX} ${maxY - minY}`,
      rects: mons,
    }
  }
</script>

<div class="app-header">
  <div class="app-title">rmouse · client</div>
  {#if state === 'connected'}
    <span class="badge ok"><span class="dot"></span>connected</span>
  {:else if state === 'connecting'}
    <span class="badge warn"><span class="dot"></span>connecting</span>
  {:else if state === 'disconnected'}
    <span class="badge err"><span class="dot"></span>disconnected</span>
  {:else}
    <span class="badge"><span class="dot"></span>idle</span>
  {/if}
</div>

<div class="panel">
  <div class="panel-title">Connection</div>
  <div class="row">
    <label>Server</label>
    <input bind:value={cfg.addr} placeholder="host:port" disabled={running} />
  </div>
  <div class="row">
    <label>Token</label>
    <input type="password" bind:value={cfg.token} disabled={running} />
  </div>
  <div class="row">
    <label>Name</label>
    <input bind:value={cfg.name} placeholder="(hostname)" disabled={running} />
  </div>
  <div class="row">
    <label>Ping (ms)</label>
    <input type="number" bind:value={cfg.pingMs} min="100" disabled={running} />
  </div>
  <div class="row">
    <label>Relay</label>
    <input bind:value={cfg.relayAddr} placeholder="(optional)" disabled={running} />
  </div>
  <div class="row">
    <label>Session</label>
    <input bind:value={cfg.session} placeholder="(required with relay)" disabled={running} />
  </div>
  <div class="row" style="justify-content: flex-end; margin-top: 4px;">
    {#if running}
      <button on:click={disconnect}>Disconnect</button>
    {:else}
      <button class="primary" on:click={connect} disabled={saving || !cfg.token}>Connect</button>
    {/if}
  </div>
</div>

<div class="panel">
  <div class="panel-title">Status</div>
  <div class="kv">
    <div class="k">State</div><div class="v">{state}</div>
    {#if assignedName}
      <div class="k">Assigned name</div><div class="v">{assignedName}</div>
    {/if}
    {#if lastErr}
      <div class="k">Last error</div><div class="v">{lastErr}</div>
    {/if}
    {#if retryMs}
      <div class="k">Retry in</div><div class="v">{(retryMs / 1000).toFixed(1)}s</div>
    {/if}
    {#if pingMs > 0}
      <div class="k">Pong interval</div><div class="v">{pingMs} ms</div>
    {/if}
  </div>
</div>

<div class="panel">
  <div class="panel-title">
    Monitors ({monitors.length}{monitorsLive ? ' · live' : ''})
  </div>
  <svg class="monitors" viewBox={layout.viewBox} preserveAspectRatio="xMidYMid meet">
    {#each layout.rects as r}
      <rect x={r.x} y={r.y} width={r.w} height={r.h}
        fill={r.primary ? 'rgba(122,167,255,0.15)' : 'rgba(255,255,255,0.06)'}
        stroke={r.primary ? '#7aa7ff' : 'rgba(255,255,255,0.35)'}
        stroke-width="3" rx="8" />
      <text class="monitor-label"
        x={r.x + 20} y={r.y + 40}>{r.name || '#' + r.id}</text>
      <text class="monitor-label"
        x={r.x + 20} y={r.y + r.h - 20}>{r.w}×{r.h}{r.primary ? ' · primary' : ''}</text>
    {/each}
  </svg>
</div>

<div class="panel">
  <div class="panel-title">Log</div>
  <div class="log">
    {#each logLines as line}
      <p class="line {line.cls}">[{line.t}] {line.msg}</p>
    {/each}
  </div>
</div>
