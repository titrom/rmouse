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
  // Grid step in world units (virtual-desktop px). Drag-drop snaps the
  // client's monitor-bbox top-left to the nearest multiple. Persisted
  // via localStorage — purely a GUI concern, the router doesn't care
  // about the spacing, only about the final (x, y) we send it.
  const GRID_STEP_KEY = "rmouse.gridStep";
  const GRID_STEP_DEFAULT = 240;
  let gridStep: number = (() => {
    const v = Number(localStorage.getItem(GRID_STEP_KEY));
    return Number.isFinite(v) && v > 0 ? v : GRID_STEP_DEFAULT;
  })();
  $: if (gridStep > 0) localStorage.setItem(GRID_STEP_KEY, String(gridStep));

  // Both client flavors share the same positioning model: offX/offY is
  // the translation applied to the client's own monitor coords, so
  // (monitor.x + offX, monitor.y + offY) is the world position. Live
  // clients additionally persist via SetClientPlacement as absolute world
  // coords (worldX = offX + cMinX, worldY = offY + cMinY).
  type StubClient = { kind: "stub"; name: string; offX: number; offY: number; monitors: main.MonitorDTO[] };
  type LiveClient = {
    kind: "live"; name: string; ids: string[]; remote: string;
    // Persisted placement: absolute world coords of the client's
    // monitor-bbox top-left. Independent of draggable offX/offY so a
    // mid-drag bump doesn't lose the snapshot, and so we can re-derive
    // offX/offY after monitors arrive (placement may load before the
    // client has connected).
    worldX: number; worldY: number;
    offX: number; offY: number; monitors: main.MonitorDTO[];
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
  // clientBbox returns the client's monitor bbox in its own coord system
  // (before offX/offY translation).
  function clientBbox(c: AnyClient) {
    if (c.monitors.length === 0) return { minX: 0, minY: 0, maxX: 0, maxY: 0 };
    return {
      minX: Math.min(...c.monitors.map(m => m.x)),
      minY: Math.min(...c.monitors.map(m => m.y)),
      maxX: Math.max(...c.monitors.map(m => m.x + m.w)),
      maxY: Math.max(...c.monitors.map(m => m.y + m.h)),
    };
  }
  // Seed a live client from a stored placement — world coords of the
  // monitor-bbox top-left. offX/offY is the translation; monitors may
  // still be empty when this runs (placement loaded before the client
  // has connected), in which case we assume cMinX = cMinY = 0.
  function applyLivePlacement(c: LiveClient, worldX: number, worldY: number) {
    const bb = clientBbox(c);
    c.offX = worldX - bb.minX;
    c.offY = worldY - bb.minY;
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

  // Graph-paper grid shown while dragging. Cell size matches the dragged
  // client's monitor bbox (one cell == one screen's worth of space) so the
  // grid visually corresponds to the router's (col, row) placement step,
  // which is also client-sized (see applyPlacementAt). Aligned to world
  // origin so the grid feels anchored to the scene at every zoom level.
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
  // Ring-search outward on the grid from (startX, startY) for the first
  // non-colliding position. startX/startY are world coords of the
  // client's monitor-bbox top-left. Returns null if nothing found within
  // `maxRings` rings — caller should then revert to drag-start.
  function findFreeGridSpot(c: AnyClient, startX: number, startY: number, step: number): { x: number; y: number } | null {
    const bb = clientBbox(c);
    const maxRings = 30;
    for (let r = 1; r <= maxRings; r++) {
      // Walk the perimeter of ring r. For small rings this scans every
      // cell; for larger rings we skip interior to avoid re-testing.
      for (let dx = -r; dx <= r; dx++) {
        for (let dy = -r; dy <= r; dy++) {
          if (Math.abs(dx) !== r && Math.abs(dy) !== r) continue; // perimeter only
          const x = startX + dx * step;
          const y = startY + dy * step;
          if (!clientCollides(c.monitors, x - bb.minX, y - bb.minY, c.name)) {
            return { x, y };
          }
        }
      }
    }
    return null;
  }

  function snapClient(c: AnyClient) {
    if (monitors.length === 0) return;
    const cMinX = Math.min(...c.monitors.map(m => m.x));
    const cMaxX = Math.max(...c.monitors.map(m => m.x + m.w));
    const cMinY = Math.min(...c.monitors.map(m => m.y));
    const cMaxY = Math.max(...c.monitors.map(m => m.y + m.h));

    // Live clients: free placement. The client's monitor-bbox top-left
    // is snapped to the nearest grid intersection (step = gridStep). If
    // that intersection would overlap the server or another client —
    // e.g. grid rounding pulled it into the server despite the drag
    // collision guard — we ring-search outward on the grid for the
    // nearest non-colliding cell, keeping the client on-grid but never
    // on top of the host screen.
    if (c.kind === "live") {
      const worldX = c.offX + cMinX;
      const worldY = c.offY + cMinY;
      const step = Math.max(1, gridStep);
      let snappedX = Math.round(worldX / step) * step;
      let snappedY = Math.round(worldY / step) * step;
      if (clientCollides(c.monitors, snappedX - cMinX, snappedY - cMinY, c.name)) {
        const spot = findFreeGridSpot(c, snappedX, snappedY, step);
        if (spot) { snappedX = spot.x; snappedY = spot.y; }
        else {
          // No free spot within search radius — revert to drag start.
          if (dragging) { snappedX = dragging.startOffX + cMinX; snappedY = dragging.startOffY + cMinY; }
        }
      }
      snapping.add(c.name);
      snapping = snapping;
      c.worldX = snappedX;
      c.worldY = snappedY;
      c.offX = snappedX - cMinX;
      c.offY = snappedY - cMinY;
      if (zoom !== 1) zoom = 1;
      SetClientPlacement(c.name, snappedX, snappedY).catch((err) => {
        log("error", `placement: ${err}`);
      });
      bumpClients();
      setTimeout(() => { snapping.delete(c.name); snapping = snapping; }, 320);
      return;
    }

    // Stub clients (synthetic test screens) keep the flush-to-edge snap —
    // useful for layout experiments where you want to butt screens up
    // against each other without pixel-precision drag.
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
      // Side-flush anchors plus corner anchors. Corners let the user place
      // a client diagonally (col≠0 AND row≠0) — router::applyPlacementAt
      // handles both axes independently, so (1,1) lands flush to the
      // server's bottom-right corner. Diagonal placements are only reached
      // by corner cursor crossings (a pure side-cross won't enter them),
      // but they're useful for multi-client layouts where the user wants
      // to fill in corner cells.
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
  // Grid halo — instead of tiling the whole stage, we render only a
  // limited band of cells around each existing monitor (server + other
  // clients), `haloDepth` cells deep in each cardinal direction. Gives
  // the user a hint of valid snap positions next to existing screens
  // without drowning the view in wallpaper.
  const haloDepth = 3;
  type GridCell = { key: string; x: number; y: number; w: number; h: number };
  $: gridCells = (() => {
    liveClients; stubClients; // reactive deps (same pattern as ghostCells)
    if (!dragging) return [] as GridCell[];
    const step = Math.max(1, gridStep);
    const dragName = dragging.client.name;
    // Collect anchor rects: server monitors + every other client's monitors.
    const anchors: { x: number; y: number; w: number; h: number }[] = [];
    for (const m of monitors) anchors.push({ x: m.x, y: m.y, w: m.w, h: m.h });
    for (const oc of [...liveClients, ...stubClients]) {
      if (oc.name === dragName) continue;
      for (const om of oc.monitors) {
        anchors.push({ x: om.x + oc.offX, y: om.y + oc.offY, w: om.w, h: om.h });
      }
    }
    const cells = new Map<string, GridCell>();
    for (const a of anchors) {
      // Snap anchor edges to the nearest grid multiple so the halo aligns.
      const aLeftCol  = Math.floor(a.x / step);
      const aRightCol = Math.ceil((a.x + a.w) / step);
      const aTopRow   = Math.floor(a.y / step);
      const aBotRow   = Math.ceil((a.y + a.h) / step);
      // Right halo
      for (let col = aRightCol; col < aRightCol + haloDepth; col++)
        for (let row = aTopRow - 1; row < aBotRow + 1; row++) {
          const key = `${col},${row}`;
          if (!cells.has(key)) cells.set(key, { key, x: col * step, y: row * step, w: step, h: step });
        }
      // Left halo
      for (let col = aLeftCol - 1; col >= aLeftCol - haloDepth; col--)
        for (let row = aTopRow - 1; row < aBotRow + 1; row++) {
          const key = `${col},${row}`;
          if (!cells.has(key)) cells.set(key, { key, x: col * step, y: row * step, w: step, h: step });
        }
      // Top halo
      for (let row = aTopRow - 1; row >= aTopRow - haloDepth; row--)
        for (let col = aLeftCol; col < aRightCol; col++) {
          const key = `${col},${row}`;
          if (!cells.has(key)) cells.set(key, { key, x: col * step, y: row * step, w: step, h: step });
        }
      // Bottom halo
      for (let row = aBotRow; row < aBotRow + haloDepth; row++)
        for (let col = aLeftCol; col < aRightCol; col++) {
          const key = `${col},${row}`;
          if (!cells.has(key)) cells.set(key, { key, x: col * step, y: row * step, w: step, h: step });
        }
    }
    // Drop cells that would overlap any anchor — a halo cell sitting
    // inside a monitor would misleadingly suggest that spot is free.
    const out: GridCell[] = [];
    for (const cell of cells.values()) {
      let overlaps = false;
      for (const a of anchors) {
        if (cell.x < a.x + a.w && cell.x + cell.w > a.x &&
            cell.y < a.y + a.h && cell.y + cell.h > a.y) { overlaps = true; break; }
      }
      if (!overlaps) out.push(cell);
    }
    return out;
  })();

  // Cells the dragged client would cover after snap — used to highlight
  // the drop target. Each cell is one gridStep × gridStep rect at world
  // (col*gridStep, row*gridStep). Reads live c.offX/c.offY via findClient
  // so it tracks the ongoing drag: we re-lookup through `liveClients` /
  // `stubClients` to establish a reactive dependency on those arrays
  // (bumpClients reassigns them on every mousemove, which triggers this
  // recompute; depending only on `dragging` wouldn't since its .client
  // reference is stable throughout the drag).
  type GhostCell = { key: string; x: number; y: number; w: number; h: number };
  $: ghostCells = (() => {
    liveClients; stubClients; // reactive deps
    if (!dragging) return [] as GhostCell[];
    const c = findClient(dragging.client.name) ?? dragging.client;
    if (c.monitors.length === 0) return [] as GhostCell[];
    const bb = clientBbox(c);
    const worldX = c.offX + bb.minX;
    const worldY = c.offY + bb.minY;
    const step = Math.max(1, gridStep);
    const snappedX = Math.round(worldX / step) * step;
    const snappedY = Math.round(worldY / step) * step;
    const cW = bb.maxX - bb.minX;
    const cH = bb.maxY - bb.minY;
    const col0 = Math.floor(snappedX / step);
    const row0 = Math.floor(snappedY / step);
    const col1 = Math.ceil((snappedX + cW) / step);
    const row1 = Math.ceil((snappedY + cH) / step);
    const cells: GhostCell[] = [];
    for (let col = col0; col < col1; col++) {
      for (let row = row0; row < row1; row++) {
        cells.push({
          key: `${col},${row}`,
          x: col * step, y: row * step,
          w: step, h: step,
        });
      }
    }
    return cells;
  })();
  // Ghost outline showing the exact rect where the client will land.
  $: ghostRect = (() => {
    liveClients; stubClients; // reactive deps (see ghostCells above)
    if (!dragging) return null;
    const c = findClient(dragging.client.name) ?? dragging.client;
    if (c.monitors.length === 0) return null;
    const bb = clientBbox(c);
    const worldX = c.offX + bb.minX;
    const worldY = c.offY + bb.minY;
    const step = Math.max(1, gridStep);
    const snappedX = Math.round(worldX / step) * step;
    const snappedY = Math.round(worldY / step) * step;
    return { x: snappedX, y: snappedY, w: bb.maxX - bb.minX, h: bb.maxY - bb.minY };
  })();

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
          worldX: p.x, worldY: p.y, offX: p.x, offY: p.y, monitors: [],
        });
      }
      bumpClients();
    } catch (e) { /* placements may not exist yet */ }
  }
  // With absolute world-coord placements, server layout changes don't
  // move clients — their worldX/worldY stays fixed. No recompute needed.
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
    log("info", `placed ${p?.name ?? p?.id} → (${p?.x}, ${p?.y})`);
    if (!p?.name) return;
    let c = liveClients.find(c => c.name === p.name);
    if (!c) {
      // Placement arrived before connection — seed an empty entry.
      // offX/offY fall back to worldX/worldY until monitors arrive (at
      // which point upsertLive re-derives them via applyLivePlacement).
      c = { kind: "live", name: p.name, ids: [], remote: "",
            worldX: p.x, worldY: p.y, offX: p.x, offY: p.y, monitors: [] };
      liveClients = [...liveClients, c];
    } else {
      c.worldX = p.x; c.worldY = p.y;
      applyLivePlacement(c, p.x, p.y);
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
      // Router already auto-assigned a placement and emitted clientPlaced;
      // onPlaced should have seeded worldX/worldY. If we raced ahead, fall
      // back to (0, 0) — the next placement event will correct it.
      c = { kind: "live", name: p.name, ids: [], remote: p.remote || "",
            worldX: 0, worldY: 0, offX: 0, offY: 0, monitors: mons };
      liveClients = [...liveClients, c];
    } else {
      c.monitors = mons;
      c.remote = p.remote || c.remote;
    }
    if (p.id && !c.ids.includes(p.id)) c.ids.push(p.id);
    applyLivePlacement(c, c.worldX, c.worldY);
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
        <label class="grid-step-field" title="Grid cell size (virtual-desktop px). Drag snaps to multiples.">
          grid
          <input
            type="number"
            class="grid-step-input"
            min="1"
            step="10"
            bind:value={gridStep}
          />
          px
        </label>
        <span class="sep"></span>
        <button class="link-btn" on:click={addTestClient} title="Add a stub client for layout testing">+ test client</button>
        <span class="sep"></span>
        <button class="link-btn" on:click={() => zoom = Math.max(minZoom, zoom / 1.2)} title="Zoom out" disabled={zoom <= minZoom}>−</button>
        <button class="link-btn" on:click={resetZoom} title="Reset zoom">{Math.round(zoom * 100)}%</button>
        <button class="link-btn" on:click={() => zoom = Math.min(maxZoom, zoom * 1.2)} title="Zoom in" disabled={zoom >= maxZoom}>+</button>
      </div>
    </header>
    <div
      class="stage"
      class:snapping={snapping.size > 0}
      class:dragging={dragging !== null}
      bind:this={stage}
      on:wheel={onWheel}
    >
      {#if drawables.length === 0}
        <p class="log-empty stage-empty">No monitors detected.</p>
      {:else}
        {#each gridCells as gc (gc.key)}
          <div class="grid-cell"
            style="
              left:   {(gc.x - originX) * scale + offsetX}px;
              top:    {(gc.y - originY) * scale + offsetY}px;
              width:  {gc.w * scale}px;
              height: {gc.h * scale}px;
            "></div>
        {/each}
        {#each ghostCells as gc (gc.key)}
          <div class="ghost-cell"
            style="
              left:   {(gc.x - originX) * scale + offsetX}px;
              top:    {(gc.y - originY) * scale + offsetY}px;
              width:  {gc.w * scale}px;
              height: {gc.h * scale}px;
            "></div>
        {/each}
        {#if ghostRect}
          <div class="ghost-rect"
            style="
              left:   {(ghostRect.x - originX) * scale + offsetX}px;
              top:    {(ghostRect.y - originY) * scale + offsetY}px;
              width:  {ghostRect.w * scale}px;
              height: {ghostRect.h * scale}px;
            "></div>
        {/if}
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
