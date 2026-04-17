package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/titrom/rmouse/internal/platform"
	"github.com/titrom/rmouse/internal/platform/inputevent"
	"github.com/titrom/rmouse/internal/proto"
	"github.com/titrom/rmouse/internal/transport"
)

// Placement is the absolute virtual-desktop position of a client's
// top-left corner (= minimum monitor X/Y across all its screens). The
// GUI lets the user drop a client anywhere — optionally snapped to a
// visual grid — and sends the resulting world coordinates verbatim;
// the router applies them without any server-anchored arithmetic.
type Placement struct {
	X int32
	Y int32
}

// Router owns the virtual-desktop cursor, decides when to "grab" (forward
// input to a remote client) vs let events pass locally, and streams input
// to the active client over its Session.
//
// Clients live at free world coordinates chosen by the operator (drag-
// and-drop in the GUI, optionally grid-snapped). Placements are keyed by
// client name so they survive reconnect, and are externally editable via
// SetPlacement.
type Router struct {
	mu sync.Mutex

	serverMons []proto.Monitor
	serverMinX int32 // left edge of the server's virtual desktop
	serverMaxX int32 // right edge
	serverMinY int32 // top edge
	serverMaxY int32 // bottom edge

	// Trap point: while grabbed, we repeatedly reset the OS cursor here so
	// hardware motion keeps producing non-zero deltas even though the virtual
	// cursor has "left the screen". Also used to initialise vx/vy.
	trapX, trapY int32

	clients map[ConnID]*routerClient

	// placements maps client name → cell index. Persisted by the GUI so
	// that a reconnecting client lands in the same spot.
	placements map[string]Placement

	// onPlacementChanged is invoked (outside the lock) whenever a
	// placement is created or modified — the GUI uses this to persist.
	onPlacementChanged func(name string, p Placement)

	// Virtual cursor — updates as we consume capture events.
	vx, vy int32

	// Last known physical absolute position (for detecting edge pushing).
	haveLastAbs       bool
	lastAbsX, lastAbsY int32

	// heldBtns is the set of mouse buttons the user currently holds down,
	// tracked from hook events. Used on grab-in to release the server OS's
	// view of those buttons so a drag started on the server doesn't stay
	// stuck when the cursor leaves.
	heldBtns map[proto.MouseButton]bool
	// btnsOnClient is the set of buttons we've forwarded as DOWN to the
	// currently-grabbed client. Used to release them on grab-off so a drag
	// started on the client doesn't stay stuck when the cursor returns to
	// the server, and to skip forwarding UP events for buttons the client
	// never saw DOWN for.
	btnsOnClient map[proto.MouseButton]bool
	// heldKeys / keysOnClient mirror the button-tracking maps above, but for
	// keyboard events. Keyed by HID usage code. Without these, a modifier
	// held during a grab-in (e.g. Ctrl pressed on server before crossing)
	// would stay logically pressed on the client until the user cycles it
	// there manually.
	heldKeys     map[uint16]bool
	keysOnClient map[uint16]bool

	// grabSyncing is set on grab-in until the first hook event whose absX,Y
	// arrives near the trap point — i.e., until the trap-teleport has
	// actually taken effect and the OS event queue has flushed any stale
	// pre-teleport events. Events received during this window are dropped
	// (no delta applied) so their fake "cursor at old screen edge" position
	// doesn't blow vx,vy past the client rect.
	grabSyncing bool

	active *routerClient

	capturer platform.Capturer
	injector platform.Injector

	// ctl is captured from Capturer.Capture in Run so that Unregister can
	// release SetConsume when the active client disconnects. Without this,
	// the OS hook keeps swallowing input and the server appears frozen.
	ctl inputevent.Ctl
}

type routerClient struct {
	id       ConnID
	name     string
	session  *transport.Session
	monitors []proto.Monitor
	// offsetX/offsetY is the virtual-space origin that the client's own
	// top-left corner (minimum monitor X/Y) maps to.
	offsetX, offsetY int32
}

// NewRouter captures the server's local monitor layout once, then accepts
// client registrations over its lifetime. It does not start consuming
// capture events until Run is called.
//
// initialPlacements seeds the per-name cell grid (e.g. from the GUI's
// persisted config). onPlacementChanged, if non-nil, is invoked whenever
// a placement is added or changed so the caller can persist it.
func NewRouter(serverMons []proto.Monitor, capturer platform.Capturer, injector platform.Injector, initialPlacements map[string]Placement, onPlacementChanged func(name string, p Placement)) *Router {
	var minX, maxX, minY, maxY int32
	var cx, cy int32
	first := true
	for _, m := range serverMons {
		left := m.X
		right := m.X + int32(m.W)
		top := m.Y
		bottom := m.Y + int32(m.H)
		if first || left < minX {
			minX = left
		}
		if first || right > maxX {
			maxX = right
		}
		if first || top < minY {
			minY = top
		}
		if first || bottom > maxY {
			maxY = bottom
		}
		if m.Primary || first {
			// Clip point = centre of the primary (or first) monitor.
			cx = m.X + int32(m.W)/2
			cy = m.Y + int32(m.H)/2
		}
		first = false
	}
	placements := make(map[string]Placement, len(initialPlacements))
	for k, v := range initialPlacements {
		placements[k] = v
	}
	return &Router{
		serverMons:         append([]proto.Monitor(nil), serverMons...),
		serverMinX:         minX,
		serverMaxX:         maxX,
		serverMinY:         minY,
		serverMaxY:         maxY,
		trapX:              cx,
		trapY:              cy,
		clients:            map[ConnID]*routerClient{},
		placements:         placements,
		onPlacementChanged: onPlacementChanged,
		capturer:           capturer,
		injector:           injector,
		heldBtns:           map[proto.MouseButton]bool{},
		btnsOnClient:       map[proto.MouseButton]bool{},
		heldKeys:           map[uint16]bool{},
		keysOnClient:       map[uint16]bool{},
	}
}

// ServerMonitors returns a copy of the server's monitor list captured at
// construction time.
func (r *Router) ServerMonitors() []proto.Monitor {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]proto.Monitor(nil), r.serverMons...)
}

// UpdateServerMonitors swaps in a new local monitor layout — used when
// the OS reports a hotplug (monitor connected / disconnected / rotated /
// resolution changed). Recomputes bbox and trap point; placements are
// kept as-is (they're absolute world coords, independent of where the
// server sits in the virtual desktop).
func (r *Router) UpdateServerMonitors(mons []proto.Monitor) {
	var minX, maxX, minY, maxY int32
	var cx, cy int32
	first := true
	for _, m := range mons {
		left := m.X
		right := m.X + int32(m.W)
		top := m.Y
		bottom := m.Y + int32(m.H)
		if first || left < minX {
			minX = left
		}
		if first || right > maxX {
			maxX = right
		}
		if first || top < minY {
			minY = top
		}
		if first || bottom > maxY {
			maxY = bottom
		}
		if m.Primary || first {
			cx = m.X + int32(m.W)/2
			cy = m.Y + int32(m.H)/2
		}
		first = false
	}
	r.mu.Lock()
	r.serverMons = append(r.serverMons[:0], mons...)
	r.serverMinX = minX
	r.serverMaxX = maxX
	r.serverMinY = minY
	r.serverMaxY = maxY
	r.trapX = cx
	r.trapY = cy
	r.mu.Unlock()
}

// Placements returns a snapshot of the current name→cell mapping.
func (r *Router) Placements() map[string]Placement {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Placement, len(r.placements))
	for k, v := range r.placements {
		out[k] = v
	}
	return out
}

// Register adds a client to the routing table. Called from handleClient on
// Hello. Returns the Placement that was applied (either the persisted one
// or a freshly auto-assigned cell) so the caller can notify the UI.
func (r *Router) Register(id ConnID, name string, s *transport.Session, monitors []proto.Monitor) Placement {
	r.mu.Lock()
	c := &routerClient{id: id, name: name, session: s}
	r.clients[id] = c
	p, created := r.ensurePlacement(name)
	r.applyPlacementAt(c, monitors, p)
	cb := r.onPlacementChanged
	r.mu.Unlock()
	if created && cb != nil {
		cb(name, p)
	}
	return p
}

// UpdateMonitors replaces a client's monitor layout in response to a
// MonitorsChanged message.
func (r *Router) UpdateMonitors(id ConnID, monitors []proto.Monitor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.clients[id]
	if !ok {
		return
	}
	p := r.placements[c.name]
	r.applyPlacementAt(c, monitors, p)
}

// SetPlacement moves every live client with the given name to absolute
// virtual-desktop coordinates (x, y) — the top-left of its monitor bbox
// — and remembers the placement for future reconnects.
func (r *Router) SetPlacement(name string, x, y int32) {
	p := Placement{X: x, Y: y}
	r.mu.Lock()
	r.placements[name] = p
	for _, c := range r.clients {
		if c.name == name {
			r.applyPlacementAt(c, c.monitors, p)
		}
	}
	cb := r.onPlacementChanged
	r.mu.Unlock()
	if cb != nil {
		cb(name, p)
	}
}

// ensurePlacement returns the stored placement for name, or auto-assigns
// one (flush-right of the rightmost occupied client, same Y as the
// server top) and stores it. Caller must hold r.mu. The bool is true
// when a new placement was allocated.
func (r *Router) ensurePlacement(name string) (Placement, bool) {
	if p, ok := r.placements[name]; ok {
		return p, false
	}
	maxX := r.serverMaxX
	for _, p := range r.placements {
		if p.X > maxX {
			maxX = p.X
		}
	}
	p := Placement{X: maxX, Y: r.serverMinY}
	r.placements[name] = p
	return p, true
}

// applyPlacementAt positions the client's top-left (min monitor X/Y)
// at the placement's absolute world coordinates. Because the operator
// drags freely — optionally against a visual grid — there is no cell
// formula here; we just translate the client's own monitor offsets so
// its top-left lands exactly on (p.X, p.Y). Must be called with r.mu held.
func (r *Router) applyPlacementAt(c *routerClient, monitors []proto.Monitor, p Placement) {
	c.monitors = append(c.monitors[:0], monitors...)
	var cMinX, cMinY int32
	first := true
	for _, m := range monitors {
		if first || m.X < cMinX {
			cMinX = m.X
		}
		if first || m.Y < cMinY {
			cMinY = m.Y
		}
		first = false
	}
	c.offsetX = p.X - cMinX
	c.offsetY = p.Y - cMinY
}

// Unregister removes a client from routing. If it was the grabbed client,
// grab is released and capture is reset to pass-through so the server's
// local input doesn't stay swallowed by the OS hook.
func (r *Router) Unregister(id ConnID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.clients[id]
	if !ok {
		return
	}
	if r.active == c {
		// Session is going away — can't deliver UPs over the wire. Drop
		// state; the client is responsible for releasing any still-held
		// inputs locally when its session dies.
		for btn := range r.btnsOnClient {
			delete(r.btnsOnClient, btn)
		}
		for hid := range r.keysOnClient {
			delete(r.keysOnClient, hid)
		}
		r.active = nil
		r.grabSyncing = false
		if r.ctl != nil {
			r.ctl.SetConsume(false)
		}
		// Park the physical cursor back inside the server's desktop so the
		// user isn't left with a frozen cursor at the trap point.
		r.vx, r.vy = clampToServer(r.vx, r.vy, r.serverMons)
		if r.injector != nil {
			_ = r.injector.MouseMoveAbs(r.vx, r.vy)
			_ = r.injector.SetCursorVisible(true)
		}
		r.lastAbsX, r.lastAbsY = r.vx, r.vy
	}
	delete(r.clients, id)
}

// Run starts capture and consumes events until ctx is cancelled. If
// capture is unavailable on this platform, Run logs and returns — the
// server still accepts clients, just without routing.
func (r *Router) Run(ctx context.Context) error {
	events, ctl, err := r.capturer.Capture(ctx)
	if err != nil {
		slog.Warn("input capture unavailable; server cannot route mouse/keys", "err", err)
		return err
	}
	r.mu.Lock()
	r.ctl = ctl
	// Initialise virtual cursor at the trap point (centre of primary).
	r.vx, r.vy = r.trapX, r.trapY
	r.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			ctl.SetConsume(false)
			_ = ctl.ReleaseClip()
			if r.injector != nil {
				_ = r.injector.SetCursorVisible(true)
			}
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return errors.New("capture channel closed")
			}
			r.handleEvent(ev, ctl)
		}
	}
}

func (r *Router) handleEvent(ev inputevent.Event, ctl inputevent.Ctl) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch ev.Kind {
	case inputevent.MouseMove:
		r.onMouseMove(ev.AbsX, ev.AbsY, ctl)
	case inputevent.MouseButton:
		if ev.Down {
			r.heldBtns[ev.Button] = true
		} else {
			delete(r.heldBtns, ev.Button)
		}
		if r.active != nil {
			if ev.Down {
				_ = r.active.session.Send(&proto.MouseButtonEvent{Button: ev.Button, Down: true})
				r.btnsOnClient[ev.Button] = true
			} else if r.btnsOnClient[ev.Button] {
				// Only forward UP if we forwarded the matching DOWN — otherwise
				// the client gets a spurious up event (e.g. for a button the
				// user pressed on the server before grab-in and we released on
				// grab-in by injecting UP locally).
				_ = r.active.session.Send(&proto.MouseButtonEvent{Button: ev.Button, Down: false})
				delete(r.btnsOnClient, ev.Button)
			}
		}
	case inputevent.MouseWheel:
		if r.active != nil {
			_ = r.active.session.Send(&proto.MouseWheel{DX: ev.WheelDX, DY: ev.WheelDY})
		}
	case inputevent.KeyEvent:
		if ev.Down {
			r.heldKeys[ev.KeyCode] = true
		} else {
			delete(r.heldKeys, ev.KeyCode)
		}
		if r.active != nil {
			if ev.Down {
				_ = r.active.session.Send(&proto.KeyEvent{KeyCode: ev.KeyCode, Down: true})
				r.keysOnClient[ev.KeyCode] = true
			} else if r.keysOnClient[ev.KeyCode] {
				// Same rule as buttons: suppress spurious UP for a key the
				// client never saw DOWN for (e.g. Ctrl pressed on server
				// before grab-in and released on server by injecting UP).
				_ = r.active.session.Send(&proto.KeyEvent{KeyCode: ev.KeyCode, Down: false})
				delete(r.keysOnClient, ev.KeyCode)
			}
		}
	}
}

// onMouseMove processes one absolute-position mouse event from capture.
//
// When not grabbed, the virtual cursor tracks the physical cursor
// directly. If the physical cursor is sitting at any edge of the server's
// desktop AND the OS reports the same position as the previous event, we
// infer the user is pushing the mouse further in that direction — Windows
// can't move the cursor past the edge, but the hook still fires for
// hardware motion. That's our cue to enter grab mode.
//
// Once grabbed, every event position is compared against the trap point.
// The delta is the raw hardware motion since the previous event; we add
// that to the virtual cursor, then reset the OS cursor to the trap so
// the next event once again reflects a fresh hardware delta.
func (r *Router) onMouseMove(absX, absY int32, ctl inputevent.Ctl) {
	prevVX, prevVY := r.vx, r.vy
	if r.active == nil {
		// Not grabbed — virtual cursor follows the real cursor.
		// Edge-pushing detection: the OS clamps the cursor at the desktop
		// boundary, so any continued hardware motion against the wall lands
		// at the same absolute coord. With an irregular multi-monitor server
		// (e.g. a 1920×1080 secondary sitting beside a taller primary) the
		// "wall" is the boundary of the *union* of monitors, not just the
		// outer bbox — pushing down off the secondary is a real edge even
		// though absY < serverMaxY. So we ask: is (absX±1, absY±1) inside
		// any server monitor? If not, that direction is clamped. Cross only
		// after two consecutive at-edge samples so a single edge-touch on
		// normal pointing doesn't trigger a stray grab.
		atRight := !anyServerMonContains(r.serverMons, absX+1, absY)
		atLeft := !anyServerMonContains(r.serverMons, absX-1, absY)
		atBot := !anyServerMonContains(r.serverMons, absX, absY+1)
		atTop := !anyServerMonContains(r.serverMons, absX, absY-1)
		var lastAtRight, lastAtLeft, lastAtBot, lastAtTop bool
		if r.haveLastAbs {
			lastAtRight = !anyServerMonContains(r.serverMons, r.lastAbsX+1, r.lastAbsY)
			lastAtLeft = !anyServerMonContains(r.serverMons, r.lastAbsX-1, r.lastAbsY)
			lastAtBot = !anyServerMonContains(r.serverMons, r.lastAbsX, r.lastAbsY+1)
			lastAtTop = !anyServerMonContains(r.serverMons, r.lastAbsX, r.lastAbsY-1)
		}
		pushRight := atRight && lastAtRight
		pushLeft := atLeft && lastAtLeft
		pushBot := atBot && lastAtBot
		pushTop := atTop && lastAtTop
		// On grab-in we project the virtual cursor grabHysteresis pixels
		// *past* the boundary, not exactly at it. A wireless mouse jitters
		// ±1–2px between hook samples; landing exactly on the boundary
		// meant the next noisy sample would shove vx back into server,
		// triggering an immediate release, then re-cross, then release —
		// the visible "twitch in the corner" the user reported. Putting
		// vx well inside the client's rect absorbs that jitter.
		// Raycast to the nearest client monitor in the push direction
		// whose perpendicular range contains the traversal coordinate, so
		// the projection lands grabHysteresis pixels INSIDE that client —
		// even when a gap separates it from the server edge. With free
		// world-coord placement (see applyPlacementAt) clients can be
		// dropped anywhere on the grid, so clients are often not flush
		// against the server; without raycasting, the old "project exactly
		// past the server edge" would miss and grab would never open.
		// When no client is in range we still set vx,vy past the edge so
		// the miss-log has meaningful coords.
		var crossed bool
		var crossDir string
		switch {
		case pushRight:
			targetX := r.serverMaxX
			if c, left := r.raycastClient("right", absY); c != nil {
				targetX = left
			}
			r.vx, r.vy = targetX+grabHysteresis, absY
			crossed = true
			crossDir = "right"
		case pushLeft:
			targetX := r.serverMinX
			if c, right := r.raycastClient("left", absY); c != nil {
				targetX = right
			}
			r.vx, r.vy = targetX-1-grabHysteresis, absY
			crossed = true
			crossDir = "left"
		case pushBot:
			targetY := r.serverMaxY
			if c, top := r.raycastClient("bot", absX); c != nil {
				targetY = top
			}
			r.vx, r.vy = absX, targetY+grabHysteresis
			crossed = true
			crossDir = "bot"
		case pushTop:
			targetY := r.serverMinY
			if c, bot := r.raycastClient("top", absX); c != nil {
				targetY = bot
			}
			r.vx, r.vy = absX, targetY-1-grabHysteresis
			crossed = true
			crossDir = "top"
		}
		if !crossed {
			r.vx, r.vy = absX, absY
			r.lastAbsX, r.lastAbsY = absX, absY
			r.haveLastAbs = true
			slog.Info("router/move free",
				"abs", fmt.Sprintf("(%d,%d)", absX, absY),
				"v", fmt.Sprintf("(%d,%d)", r.vx, r.vy),
				"pushR", pushRight, "pushL", pushLeft, "pushB", pushBot, "pushT", pushTop)
			return
		}
		slog.Info("router/move cross attempt",
			"abs", fmt.Sprintf("(%d,%d)", absX, absY),
			"vBefore", fmt.Sprintf("(%d,%d)", prevVX, prevVY),
			"vAfter", fmt.Sprintf("(%d,%d)", r.vx, r.vy),
			"dir", crossDir)
		r.resolveRegion(ctl)
		if r.active != nil {
			// Grab succeeded — trap the physical cursor at centre so we
			// keep getting non-zero deltas (Windows clamps cursor at screen
			// edges, so without trapping a user pushing further in the
			// grabbed direction would just produce repeated absX,absY at
			// the screen edge with zero delta).
			//
			// The trap teleport doesn't take effect synchronously — Windows
			// queues the SetCursorPos behind any hook events already in
			// flight, so the next 1–3 hook events report absX,absY from
			// the *pre*-teleport position. We mark grabSyncing=true and
			// drop those stale events; once a hook event arrives near the
			// trap, we sync lastAbs and start applying deltas normally.
			_ = r.injector.MouseMoveAbs(r.trapX, r.trapY)
			r.lastAbsX, r.lastAbsY = r.trapX, r.trapY
			r.grabSyncing = true
			slog.Info("router/move grab on",
				"client", r.active.name,
				"v", fmt.Sprintf("(%d,%d)", r.vx, r.vy),
				"trap", fmt.Sprintf("(%d,%d)", r.trapX, r.trapY))
		} else {
			// Grab not opened (no client placed in that direction). Update
			// last-position bookkeeping so subsequent events still track.
			r.lastAbsX, r.lastAbsY = absX, absY
			r.haveLastAbs = true
			slog.Info("router/move cross missed (no client at target)",
				"v", fmt.Sprintf("(%d,%d)", r.vx, r.vy))
		}
		return
	}

	// After grab-in the trap teleport is in flight: Windows queues hook
	// events from before the teleport landed. Their absX,Y reflect the
	// pre-teleport (cross-edge) position, so trap-relative deltas would
	// look like hundreds of pixels in the pushed direction, blowing the
	// virtual cursor past the client. Drop everything until a hook event
	// arrives near the trap point — that's our signal the queue has
	// flushed.
	if r.grabSyncing {
		if absInt32(absX-r.trapX) <= grabSyncSettlePx && absInt32(absY-r.trapY) <= grabSyncSettlePx {
			r.grabSyncing = false
			slog.Info("router/move grab synced",
				"client", r.active.name,
				"abs", fmt.Sprintf("(%d,%d)", absX, absY))
		} else {
			slog.Info("router/move grab sync drop",
				"client", r.active.name,
				"abs", fmt.Sprintf("(%d,%d)", absX, absY))
		}
		return
	}
	// Grabbed: each event's delta is hardware motion since the last trap
	// teleport. We re-trap after every event so the OS cursor stays in the
	// middle of the primary monitor and never bumps the screen edge (which
	// would clamp future deltas to zero).
	dx := absX - r.trapX
	dy := absY - r.trapY
	if dx == 0 && dy == 0 {
		return
	}
	r.vx += dx
	r.vy += dy
	// Clamp to the active client's monitor union, except when the new
	// position is inside the server (legitimate release path). Without this
	// a client that's shorter or narrower than the server lets the cursor
	// fly past a client edge that doesn't lead anywhere — clientAt returns
	// nil and resolveRegion would release grab.
	r.vx, r.vy = clampToActiveClient(r.vx, r.vy, r.active, r.serverMons)
	_ = r.injector.MouseMoveAbs(r.trapX, r.trapY)
	r.lastAbsX, r.lastAbsY = r.trapX, r.trapY
	slog.Info("router/move grabbed",
		"client", r.active.name,
		"abs", fmt.Sprintf("(%d,%d)", absX, absY),
		"d", fmt.Sprintf("(%+d,%+d)", dx, dy),
		"v", fmt.Sprintf("(%d,%d)", r.vx, r.vy))
	r.resolveRegion(ctl)
}

// grabSyncSettlePx is how close to the trap point a hook event's absX,Y
// has to be before we consider the post-grab teleport "settled" and start
// applying deltas. Generous because the user may have a few ms of motion
// already accumulated against the trap by the time we see it.
const grabSyncSettlePx int32 = 100

func absInt32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

// resolveRegion decides whether the virtual cursor is on the server or
// inside a client and updates grab state accordingly. Caller must hold mu.
func (r *Router) resolveRegion(ctl inputevent.Ctl) {
	target := r.clientAt(r.vx, r.vy)
	switch {
	case target != nil && r.active == nil:
		// Crossing from server into a client.
		// Release any buttons / keys the user is physically holding: the
		// server's OS saw the DOWN events before the grab and is mid-drag
		// or has a modifier latched, but the cursor is about to leave.
		// Without releasing, that state stays stuck on the server.
		if r.injector != nil {
			for btn := range r.heldBtns {
				_ = r.injector.MouseButton(btn, false)
			}
			for hid := range r.heldKeys {
				_ = r.injector.KeyEvent(hid, false)
			}
		}
		// btnsOnClient / keysOnClient should already be empty (no active
		// client), but clear them so we don't carry ghost state into the
		// new grab.
		for btn := range r.btnsOnClient {
			delete(r.btnsOnClient, btn)
		}
		for hid := range r.keysOnClient {
			delete(r.keysOnClient, hid)
		}
		r.active = target
		_ = target.session.Send(&proto.Grab{On: true})
		r.sendAbs(target)
		ctl.SetConsume(true)
		// Hide the host cursor: while grabbed it sits parked at the trap point
		// and would otherwise hover visibly in the centre of the screen.
		_ = r.injector.SetCursorVisible(false)
		slog.Info("router/region grab acquired", "client", target.name,
			"v", fmt.Sprintf("(%d,%d)", r.vx, r.vy))

	case target == nil && r.active != nil:
		// Returning to the server — restore local cursor at the boundary.
		releasedFrom := r.active.name
		preClampVX, preClampVY := r.vx, r.vy
		// Release any buttons / keys we forwarded as DOWN to the client.
		// Without this, a drag or held modifier started on the client stays
		// stuck when the user crosses back to the server.
		for btn := range r.btnsOnClient {
			_ = r.active.session.Send(&proto.MouseButtonEvent{Button: btn, Down: false})
			delete(r.btnsOnClient, btn)
		}
		for hid := range r.keysOnClient {
			_ = r.active.session.Send(&proto.KeyEvent{KeyCode: hid, Down: false})
			delete(r.keysOnClient, hid)
		}
		_ = r.active.session.Send(&proto.Grab{On: false})
		r.active = nil
		r.grabSyncing = false
		ctl.SetConsume(false)
		// Clamp virtual cursor to server bounds so next grab re-enters cleanly.
		r.vx, r.vy = clampToServer(r.vx, r.vy, r.serverMons)
		// Hysteresis on release: park the cursor grabHysteresis pixels away
		// from any boundary it might have just exited through. Without this
		// vx lands flush against e.g. serverMaxX-1, lastAbs is flush, and the
		// very next at-edge hook event re-triggers pushRight — wireless-mouse
		// jitter at the corner would oscillate forever.
		r.vx, r.vy = padInsideServer(r.vx, r.vy, r.serverMinX, r.serverMaxX, r.serverMinY, r.serverMaxY)
		_ = r.injector.MouseMoveAbs(r.vx, r.vy)
		r.lastAbsX, r.lastAbsY = r.vx, r.vy
		_ = r.injector.SetCursorVisible(true)
		slog.Info("router/region grab released", "client", releasedFrom,
			"vExit", fmt.Sprintf("(%d,%d)", preClampVX, preClampVY),
			"vParked", fmt.Sprintf("(%d,%d)", r.vx, r.vy))

	case target != nil && r.active != nil && target != r.active:
		// Crossed between two clients. Release buttons and keys on the old
		// client so a drag or held modifier there doesn't stay stuck after
		// we switch.
		from := r.active.name
		for btn := range r.btnsOnClient {
			_ = r.active.session.Send(&proto.MouseButtonEvent{Button: btn, Down: false})
			delete(r.btnsOnClient, btn)
		}
		for hid := range r.keysOnClient {
			_ = r.active.session.Send(&proto.KeyEvent{KeyCode: hid, Down: false})
			delete(r.keysOnClient, hid)
		}
		_ = r.active.session.Send(&proto.Grab{On: false})
		r.active = target
		_ = target.session.Send(&proto.Grab{On: true})
		r.sendAbs(target)
		slog.Info("router/region client switch", "from", from, "to", target.name,
			"v", fmt.Sprintf("(%d,%d)", r.vx, r.vy))

	case target != nil && r.active != nil:
		// Still in the same client — just update position.
		r.sendAbs(target)
	}
}

func (r *Router) sendAbs(c *routerClient) {
	for _, m := range c.monitors {
		left := c.offsetX + m.X
		top := c.offsetY + m.Y
		if r.vx >= left && r.vx < left+int32(m.W) &&
			r.vy >= top && r.vy < top+int32(m.H) {
			_ = c.session.Send(&proto.MouseAbs{
				MonitorID: m.ID,
				X:         uint16(r.vx - left),
				Y:         uint16(r.vy - top),
			})
			return
		}
	}
}

// clampToActiveClient keeps the cursor inside the active client's monitor
// union once grabbed, except when it has crossed back into a server monitor
// (the legitimate release path). When neither inside the client nor inside
// the server, the cursor is snapped to the nearest point on the client's
// rect — turns "fall through a non-server-adjacent client edge" from a grab
// loss into a wall hit.
func clampToActiveClient(vx, vy int32, c *routerClient, serverMons []proto.Monitor) (int32, int32) {
	if c == nil {
		return vx, vy
	}
	for _, m := range c.monitors {
		l := c.offsetX + m.X
		t := c.offsetY + m.Y
		if vx >= l && vx < l+int32(m.W) && vy >= t && vy < t+int32(m.H) {
			return vx, vy
		}
	}
	if anyServerMonContains(serverMons, vx, vy) {
		return vx, vy
	}
	// Cursor is in free space (neither in the client nor in any server
	// monitor). With gap-placed clients this is the normal "user is
	// trying to exit the client back toward the server" situation —
	// previously we just snapped to the nearest client edge (wall hit),
	// which trapped the user on the client forever. Instead, if the
	// cursor exited via the client's server-facing edge and the server
	// bbox lies on that side, teleport across the gap to just inside
	// the server so resolveRegion can take the release path. Done per-
	// axis so a diagonal escape still finds the server.
	var cL, cR, cT, cB int32
	{
		first := true
		for _, m := range c.monitors {
			l := c.offsetX + m.X
			rgt := l + int32(m.W)
			t := c.offsetY + m.Y
			bot := t + int32(m.H)
			if first || l < cL {
				cL = l
			}
			if first || rgt > cR {
				cR = rgt
			}
			if first || t < cT {
				cT = t
			}
			if first || bot > cB {
				cB = bot
			}
			first = false
		}
	}
	var sL, sR, sT, sB int32
	{
		first := true
		for _, m := range serverMons {
			l := m.X
			rgt := l + int32(m.W)
			t := m.Y
			bot := t + int32(m.H)
			if first || l < sL {
				sL = l
			}
			if first || rgt > sR {
				sR = rgt
			}
			if first || t < sT {
				sT = t
			}
			if first || bot > sB {
				sB = bot
			}
			first = false
		}
	}
	jumpX, jumpY := vx, vy
	crossed := false
	switch {
	case vx < cL && sR <= cL:
		jumpX = sR - 1 - grabHysteresis
		crossed = true
	case vx >= cR && sL >= cR:
		jumpX = sL + grabHysteresis
		crossed = true
	}
	switch {
	case vy < cT && sB <= cT:
		jumpY = sB - 1 - grabHysteresis
		crossed = true
	case vy >= cB && sT >= cB:
		jumpY = sT + grabHysteresis
		crossed = true
	}
	if crossed {
		// Ensure the landing point sits inside the server bbox so
		// resolveRegion sees "not in any client" and releases.
		if jumpX < sL+grabHysteresis {
			jumpX = sL + grabHysteresis
		}
		if jumpX > sR-1-grabHysteresis {
			jumpX = sR - 1 - grabHysteresis
		}
		if jumpY < sT+grabHysteresis {
			jumpY = sT + grabHysteresis
		}
		if jumpY > sB-1-grabHysteresis {
			jumpY = sB - 1 - grabHysteresis
		}
		return jumpX, jumpY
	}
	// No server in the exit direction — fall back to the "wall hit"
	// behaviour: snap to the nearest point on the client's union.
	var bestX, bestY int32
	var bestD int64 = -1
	for _, m := range c.monitors {
		l := c.offsetX + m.X
		t := c.offsetY + m.Y
		rgt := l + int32(m.W) - 1
		bot := t + int32(m.H) - 1
		cx := vx
		switch {
		case cx < l:
			cx = l
		case cx > rgt:
			cx = rgt
		}
		cy := vy
		switch {
		case cy < t:
			cy = t
		case cy > bot:
			cy = bot
		}
		dxp := int64(vx - cx)
		dyp := int64(vy - cy)
		d := dxp*dxp + dyp*dyp
		if bestD < 0 || d < bestD {
			bestD = d
			bestX, bestY = cx, cy
		}
	}
	return bestX, bestY
}

func (r *Router) clientAt(x, y int32) *routerClient {
	for _, c := range r.clients {
		for _, m := range c.monitors {
			left := c.offsetX + m.X
			top := c.offsetY + m.Y
			if x >= left && x < left+int32(m.W) && y >= top && y < top+int32(m.H) {
				return c
			}
		}
	}
	return nil
}

// raycastClient returns the nearest client monitor in the given cardinal
// direction whose perpendicular range contains the traversal coordinate,
// along with the traversal-axis edge the ray hits first. Used on grab-in
// to bridge gaps between the server and a client placed at arbitrary
// world coordinates: with free placement (see applyPlacementAt) clients
// no longer have to be flush against the server edge, so the virtual
// cursor must jump across any gap to the client.
//
// dir is one of "right", "left", "bot", "top". For "right"/"left" we
// scan along X with Y as the perpendicular traversal coord; for "bot"
// /"top" X is perpendicular. Returns (nil, 0) when no client lies in
// the given half-plane with overlapping perpendicular range.
func (r *Router) raycastClient(dir string, perp int32) (*routerClient, int32) {
	var best *routerClient
	var bestEdge int32
	haveBest := false
	for _, c := range r.clients {
		for _, m := range c.monitors {
			left := c.offsetX + m.X
			top := c.offsetY + m.Y
			right := left + int32(m.W)
			bot := top + int32(m.H)
			switch dir {
			case "right":
				if left < r.serverMaxX {
					continue
				}
				if perp < top || perp >= bot {
					continue
				}
				if !haveBest || left < bestEdge {
					best = c
					bestEdge = left
					haveBest = true
				}
			case "left":
				if right > r.serverMinX {
					continue
				}
				if perp < top || perp >= bot {
					continue
				}
				if !haveBest || right > bestEdge {
					best = c
					bestEdge = right
					haveBest = true
				}
			case "bot":
				if top < r.serverMaxY {
					continue
				}
				if perp < left || perp >= right {
					continue
				}
				if !haveBest || top < bestEdge {
					best = c
					bestEdge = top
					haveBest = true
				}
			case "top":
				if bot > r.serverMinY {
					continue
				}
				if perp < left || perp >= right {
					continue
				}
				if !haveBest || bot > bestEdge {
					best = c
					bestEdge = bot
					haveBest = true
				}
			}
		}
	}
	return best, bestEdge
}

// grabHysteresis is the pixel buffer applied on grab transitions. On cross-in
// the virtual cursor is pushed this far past the boundary so subsequent
// motion doesn't immediately exit; on release it is parked this far inside
// server bounds for the symmetric reason. Kept small so the visible jump at
// the seam is barely perceptible — the edge-push detector (two consecutive
// at-wall hook samples) already absorbs normal mouse jitter, so a large
// spatial buffer is redundant for jitter and just hurts smoothness.
const grabHysteresis int32 = 30

// padInsideServer pulls (x, y) at least grabHysteresis pixels away from any
// server bbox edge it sits at. Used after releasing grab so the next at-edge
// hook event has to actually be pushed there, not just inherit from release.
func padInsideServer(x, y, minX, maxX, minY, maxY int32) (int32, int32) {
	if x >= maxX-1-grabHysteresis {
		x = maxX - 1 - grabHysteresis
	}
	if x <= minX+grabHysteresis {
		x = minX + grabHysteresis
	}
	if y >= maxY-1-grabHysteresis {
		y = maxY - 1 - grabHysteresis
	}
	if y <= minY+grabHysteresis {
		y = minY + grabHysteresis
	}
	return x, y
}

// anyServerMonContains reports whether (x, y) lies inside any server monitor.
// Used by the edge-push detector to decide if stepping one pixel further in
// some direction would still land on a real screen.
func anyServerMonContains(mons []proto.Monitor, x, y int32) bool {
	for _, m := range mons {
		if x >= m.X && x < m.X+int32(m.W) && y >= m.Y && y < m.Y+int32(m.H) {
			return true
		}
	}
	return false
}

func clampToServer(x, y int32, mons []proto.Monitor) (int32, int32) {
	// If already inside a server monitor, return as-is.
	for _, m := range mons {
		if x >= m.X && x < m.X+int32(m.W) && y >= m.Y && y < m.Y+int32(m.H) {
			return x, y
		}
	}
	// Otherwise snap to the nearest edge point of the primary / first monitor.
	if len(mons) == 0 {
		return 0, 0
	}
	m := mons[0]
	for _, c := range mons {
		if c.Primary {
			m = c
			break
		}
	}
	if x < m.X {
		x = m.X
	} else if x >= m.X+int32(m.W) {
		x = m.X + int32(m.W) - 1
	}
	if y < m.Y {
		y = m.Y
	} else if y >= m.Y+int32(m.H) {
		y = m.Y + int32(m.H) - 1
	}
	return x, y
}
