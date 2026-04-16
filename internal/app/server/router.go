package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/titrom/rmouse/internal/platform"
	"github.com/titrom/rmouse/internal/platform/inputevent"
	"github.com/titrom/rmouse/internal/proto"
	"github.com/titrom/rmouse/internal/transport"
)

// Placement is a discrete cell index of a client relative to the server's
// bounding box in the virtual desktop. (0,0) sits on top of the server and
// is not allowed; (1,0) is flush-right, (-1,0) is flush-left, etc. Larger
// magnitudes place the client further away in cell-sized steps.
type Placement struct {
	Col int32
	Row int32
}

// Router owns the virtual-desktop cursor, decides when to "grab" (forward
// input to a remote client) vs let events pass locally, and streams input
// to the active client over its Session.
//
// Clients live in a cell grid around the server: each cell is the size of
// the server's bounding box. Placements are keyed by client name so that
// they survive reconnect, and are externally editable (drag-and-drop in
// the GUI) via SetPlacement.
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
	}
}

// ServerMonitors returns a copy of the server's monitor list captured at
// construction time.
func (r *Router) ServerMonitors() []proto.Monitor {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]proto.Monitor(nil), r.serverMons...)
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

// SetPlacement moves every live client with the given name to (col,row)
// and remembers the placement for future reconnects.
func (r *Router) SetPlacement(name string, col, row int32) {
	p := Placement{Col: col, Row: row}
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
// one (next free column to the right of the rightmost occupied cell, row 0)
// and stores it. Caller must hold r.mu. The bool is true when a new
// placement was allocated.
func (r *Router) ensurePlacement(name string) (Placement, bool) {
	if p, ok := r.placements[name]; ok {
		return p, false
	}
	maxCol := int32(0)
	for _, p := range r.placements {
		if p.Row == 0 && p.Col > maxCol {
			maxCol = p.Col
		}
	}
	p := Placement{Col: maxCol + 1, Row: 0}
	r.placements[name] = p
	return p, true
}

// applyPlacementAt positions the client so its bounding box sits in the
// (col,row) cell of the server-sized grid. Must be called with r.mu held.
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
	sw := r.serverMaxX - r.serverMinX
	sh := r.serverMaxY - r.serverMinY
	c.offsetX = r.serverMinX + p.Col*sw - cMinX
	c.offsetY = r.serverMinY + p.Row*sh - cMinY
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
		r.active = nil
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
		if r.active != nil {
			_ = r.active.session.Send(&proto.MouseButtonEvent{Button: ev.Button, Down: ev.Down})
		}
	case inputevent.MouseWheel:
		if r.active != nil {
			_ = r.active.session.Send(&proto.MouseWheel{DX: ev.WheelDX, DY: ev.WheelDY})
		}
	case inputevent.KeyEvent:
		if r.active != nil {
			_ = r.active.session.Send(&proto.KeyEvent{KeyCode: ev.KeyCode, Down: ev.Down})
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
	if r.active == nil {
		// Not grabbed — virtual cursor follows the real cursor.
		// Edge-pushing detection: the OS clamps the cursor at the desktop
		// boundary, so any continued hardware motion against the wall lands
		// at the same absolute coord. We previously required the *whole*
		// position to be unchanged (absX & absY both equal to last), which
		// failed on a low-DPI / slow-handed mouse where the perpendicular
		// axis often inches by ±1px between hook events. Now: an axis is
		// "stuck against its edge" if both this event AND the previous one
		// sit at that bound, regardless of the other axis. Cross only after
		// two consecutive at-edge samples so a single edge-touch on normal
		// pointing doesn't trigger a stray grab.
		pushRight := absX >= r.serverMaxX-1 && r.haveLastAbs && r.lastAbsX >= r.serverMaxX-1
		pushLeft := absX <= r.serverMinX && r.haveLastAbs && r.lastAbsX <= r.serverMinX
		pushBot := absY >= r.serverMaxY-1 && r.haveLastAbs && r.lastAbsY >= r.serverMaxY-1
		pushTop := absY <= r.serverMinY && r.haveLastAbs && r.lastAbsY <= r.serverMinY
		var crossed bool
		switch {
		case pushRight:
			r.vx, r.vy = r.serverMaxX, absY
			crossed = true
		case pushLeft:
			r.vx, r.vy = r.serverMinX-1, absY
			crossed = true
		case pushBot:
			r.vx, r.vy = absX, r.serverMaxY
			crossed = true
		case pushTop:
			r.vx, r.vy = absX, r.serverMinY-1
			crossed = true
		}
		if !crossed {
			r.vx, r.vy = absX, absY
			r.lastAbsX, r.lastAbsY = absX, absY
			r.haveLastAbs = true
			return
		}
		r.resolveRegion(ctl)
		if r.active != nil {
			// Grab succeeded — trap the physical cursor at centre so we
			// keep getting non-zero deltas.
			_ = r.injector.MouseMoveAbs(r.trapX, r.trapY)
			r.lastAbsX, r.lastAbsY = r.trapX, r.trapY
		} else {
			// Grab not opened (no client placed in that direction). Update
			// last-position bookkeeping so subsequent events still track.
			r.lastAbsX, r.lastAbsY = absX, absY
			r.haveLastAbs = true
		}
		return
	}

	// Grabbed mode: compute hardware delta relative to the trap point.
	dx := absX - r.trapX
	dy := absY - r.trapY
	if dx == 0 && dy == 0 {
		return
	}
	r.vx += dx
	r.vy += dy
	_ = r.injector.MouseMoveAbs(r.trapX, r.trapY)
	r.lastAbsX, r.lastAbsY = r.trapX, r.trapY
	r.resolveRegion(ctl)
}

// resolveRegion decides whether the virtual cursor is on the server or
// inside a client and updates grab state accordingly. Caller must hold mu.
func (r *Router) resolveRegion(ctl inputevent.Ctl) {
	target := r.clientAt(r.vx, r.vy)
	switch {
	case target != nil && r.active == nil:
		// Crossing from server into a client.
		r.active = target
		_ = target.session.Send(&proto.Grab{On: true})
		r.sendAbs(target)
		ctl.SetConsume(true)
		// Hide the host cursor: while grabbed it sits parked at the trap point
		// and would otherwise hover visibly in the centre of the screen.
		_ = r.injector.SetCursorVisible(false)

	case target == nil && r.active != nil:
		// Returning to the server — restore local cursor at the boundary.
		_ = r.active.session.Send(&proto.Grab{On: false})
		r.active = nil
		ctl.SetConsume(false)
		// Clamp virtual cursor to server bounds so next grab re-enters cleanly.
		r.vx, r.vy = clampToServer(r.vx, r.vy, r.serverMons)
		_ = r.injector.MouseMoveAbs(r.vx, r.vy)
		r.lastAbsX, r.lastAbsY = r.vx, r.vy
		_ = r.injector.SetCursorVisible(true)

	case target != nil && r.active != nil && target != r.active:
		// Crossed between two clients.
		_ = r.active.session.Send(&proto.Grab{On: false})
		r.active = target
		_ = target.session.Send(&proto.Grab{On: true})
		r.sendAbs(target)

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
