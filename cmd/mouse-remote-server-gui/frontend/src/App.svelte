<script lang="ts">
  import { onMount } from 'svelte'
  import {
    CertFingerprint,
    IsRunning,
    LoadConfig,
    SaveConfig,
    Start,
    Stop,
  } from '../wailsjs/go/main/App'
  import { EventsOn, ClipboardSetText, Environment } from '../wailsjs/runtime/runtime'
  import type { main } from '../wailsjs/go/models'

  type Cfg = main.ConfigDTO
  type Mon = main.MonitorDTO

  let cfg: Cfg = {
    addr: '0.0.0.0:24242',
    token: '',
    relayAddr: '',
    session: '',
  }

  let running = false
  let listening = false
  let listenAddr = ''
  let certPath = ''
  let certFp = ''

  // Multiple simultaneous clients, keyed by relay-side remote address.
  type ClientState = {
    id: string
    remote: string
    name: string
    connectedAt: number
    monitors: Mon[]
  }
  let clients: ClientState[] = []

  let logLines: { t: string; cls: string; msg: string }[] = []
  const LOG_MAX = 100

  function log(msg: string, cls = '') {
    const t = new Date().toLocaleTimeString(undefined, { hour12: false })
    logLines = [{ t, cls, msg }, ...logLines].slice(0, LOG_MAX)
  }

  function upsertClient(id: string, patch: Partial<ClientState>) {
    const idx = clients.findIndex((c) => c.id === id)
    if (idx >= 0) {
      clients[idx] = { ...clients[idx], ...patch }
      clients = clients
    } else if (patch.name !== undefined && patch.monitors !== undefined) {
      clients = [
        ...clients,
        {
          id,
          remote: patch.remote ?? '',
          name: patch.name,
          monitors: patch.monitors,
          connectedAt: patch.connectedAt ?? Date.now(),
        },
      ]
    }
  }

  onMount(async () => {
    try {
      const env = await Environment()
      if (env?.platform) {
        document.body.classList.add(`platform-${env.platform}`)
      }
    } catch {}
    cfg = await LoadConfig()
    running = await IsRunning()
    try {
      certFp = await CertFingerprint()
    } catch (e: any) {
      log(`cert fingerprint: ${e}`, 'err')
    }

    EventsOn('rmouse:listening', (p: any) => {
      listening = true
      if (p.addr) listenAddr = p.addr
      else if (p.relay) listenAddr = `relay ${p.relay} · session ${p.session}`
      if (p.certPath) certPath = p.certPath
      log(`listening on ${listenAddr}`, 'ok')
    })
    EventsOn('rmouse:client', (p: any) => {
      if (p.state === 'connected') {
        upsertClient(p.id, {
          remote: p.remote,
          name: p.name,
          monitors: p.monitors || [],
          connectedAt: Date.now(),
        })
        log(`client connected: ${p.name} @ ${p.remote} (${(p.monitors || []).length} monitors)`, 'ok')
      } else if (p.state === 'monitorsChanged') {
        upsertClient(p.id, { monitors: p.monitors || [] })
        log(`[${p.name}] monitors changed (${(p.monitors || []).length})`, 'ok')
      } else if (p.state === 'bye') {
        log(`[${p.name}] bye: ${p.reason || ''}`)
      } else if (p.state === 'disconnected') {
        log(`[${p.name}] disconnected${p.err ? ': ' + p.err : ''}`, p.err ? 'err' : '')
        clients = clients.filter((c) => c.id !== p.id)
      }
    })
    EventsOn('rmouse:recvError', (p: any) => {
      log(`[${p.name}] recv: ${p.err}`, 'warn')
    })
    EventsOn('rmouse:stopped', () => {
      running = false
      listening = false
      clients = []
      log('stopped')
    })
    EventsOn('rmouse:fatal', (err: any) => {
      log(`fatal: ${err}`, 'err')
      running = false
      listening = false
    })
  })

  async function start() {
    try {
      await SaveConfig(cfg)
      await Start(cfg)
      running = true
    } catch (e: any) {
      log(`start failed: ${e}`, 'err')
    }
  }

  async function stop() {
    await Stop()
    running = false
  }

  async function copyFp() {
    if (!certFp) return
    await ClipboardSetText(certFp)
    log('fingerprint copied', 'ok')
  }

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

  function fmtElapsed(from: number): string {
    const s = Math.floor((Date.now() - from) / 1000)
    if (s < 60) return `${s}s`
    if (s < 3600) return `${Math.floor(s / 60)}m${s % 60}s`
    return `${Math.floor(s / 3600)}h${Math.floor((s % 3600) / 60)}m`
  }

  // Ticker so elapsed-time renders live.
  let now = Date.now()
  onMount(() => {
    const id = setInterval(() => (now = Date.now()), 1000)
    return () => clearInterval(id)
  })
</script>

<div class="app-header">
  <div class="app-title">rmouse · server</div>
  {#if clients.length > 0}
    <span class="badge ok"><span class="dot"></span>{clients.length} connected</span>
  {:else if listening}
    <span class="badge warn"><span class="dot"></span>waiting</span>
  {:else if running}
    <span class="badge warn"><span class="dot"></span>starting</span>
  {:else}
    <span class="badge"><span class="dot"></span>stopped</span>
  {/if}
</div>

<div class="panel">
  <div class="panel-title">Listener</div>
  <div class="row">
    <label>Listen</label>
    <input bind:value={cfg.addr} placeholder="host:port" disabled={running} />
  </div>
  <div class="row">
    <label>Token</label>
    <input type="password" bind:value={cfg.token} disabled={running} />
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
      <button on:click={stop}>Stop</button>
    {:else}
      <button class="primary" on:click={start} disabled={!cfg.token}>Start</button>
    {/if}
  </div>
</div>

<div class="panel">
  <div class="panel-title">Certificate</div>
  <div class="kv">
    {#if certPath}
      <div class="k">Path</div><div class="v">{certPath}</div>
    {/if}
    <div class="k">Fingerprint</div>
    <div class="v" style="word-break: break-all; font-family: 'SF Mono', Consolas, monospace; font-size: 12px;">
      {certFp || '(not generated)'}
    </div>
  </div>
  <div class="row" style="justify-content: flex-end; margin-top: 8px;">
    <button on:click={copyFp} disabled={!certFp}>Copy fingerprint</button>
  </div>
</div>

<div class="panel">
  <div class="panel-title">Clients ({clients.length})</div>
  {#if clients.length === 0}
    <div style="color: var(--fg-dim); font-size: 13px;">No clients connected.</div>
  {:else}
    {#each clients as c (c.id)}
      {@const layout = computeLayout(c.monitors)}
      <div style="padding: 10px 0; border-top: 1px solid var(--border); margin-top: 10px;">
        <div class="kv" style="margin-bottom: 8px;">
          <div class="k">Name</div><div class="v">{c.name}</div>
          <div class="k">Remote</div><div class="v">{c.remote}</div>
          <div class="k">Uptime</div><div class="v">{fmtElapsed(c.connectedAt)}{now > 0 ? '' : ''}</div>
          <div class="k">Monitors</div><div class="v">{c.monitors.length}</div>
        </div>
        <svg class="monitors" viewBox={layout.viewBox} preserveAspectRatio="xMidYMid meet" style="height: 140px;">
          {#each layout.rects as r}
            <rect x={r.x} y={r.y} width={r.w} height={r.h}
              fill={r.primary ? 'rgba(122,167,255,0.15)' : 'rgba(255,255,255,0.06)'}
              stroke={r.primary ? '#7aa7ff' : 'rgba(255,255,255,0.35)'}
              stroke-width="3" rx="8" />
            <text class="monitor-label" x={r.x + 20} y={r.y + 40}>{r.name || '#' + r.id}</text>
            <text class="monitor-label" x={r.x + 20} y={r.y + r.h - 20}>{r.w}×{r.h}{r.primary ? ' · primary' : ''}</text>
          {/each}
        </svg>
      </div>
    {/each}
  {/if}
</div>

<div class="panel">
  <div class="panel-title">Log</div>
  <div class="log">
    {#each logLines as line}
      <p class="line {line.cls}">[{line.t}] {line.msg}</p>
    {/each}
  </div>
</div>
