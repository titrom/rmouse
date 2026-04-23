// Package server implements the rmouse host run-loop: it ensures the TLS
// cert exists, accepts clients (directly or via relay), and echoes Pings.
// Both the CLI binary and the GUI import this package.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/titrom/rmouse/internal/platform"
	"github.com/titrom/rmouse/internal/proto"
	"github.com/titrom/rmouse/internal/transport"
)

// Config is the caller-supplied configuration for Run.
type Config struct {
	Addr      string // host:port; ignored when RelayAddr is set
	Token     string
	RelayAddr string // optional; when set, server dials the relay
	Session   string // relay session id; required iff RelayAddr != ""

	// Placements seeds the router's per-name cell grid. Typically loaded
	// from a GUI-managed config file.
	Placements map[string]Placement

	// OnRouterReady, if non-nil, is called exactly once after the router
	// is constructed. Gives the caller a handle so it can drive runtime
	// edits (e.g. SetPlacement from drag-and-drop).
	OnRouterReady func(*Router)

	// OnPlacementChanged, if non-nil, is invoked whenever a client's
	// placement is created or modified. Mirrored into the router.
	OnPlacementChanged func(name string, p Placement)
	EnableClipboard    bool

	// OnClipboardItem, if non-nil, is invoked whenever a clipboard snapshot
	// passes through this server — either observed locally (origin="server")
	// or received from a client (origin = peer name). Used by the GUI to
	// feed its history panel. Must not block.
	OnClipboardItem func(origin string, format proto.ClipboardFormat, data []byte)
}

// Event is a sum type of things Run reports.
type Event interface{ isEvent() }

type ListeningEvent struct {
	Addr     string
	CertPath string
}

type ServingViaRelayEvent struct {
	Relay    string
	Session  string
	CertPath string
}

// ConnID is a short random per-connection identifier. In relay mode the
// transport RemoteAddr of every client is the same (the relay), so callers
// need this to distinguish simultaneous clients.
type ConnID string

type ServerMonitorsEvent struct {
	Monitors []proto.Monitor
}

type ClientPlacedEvent struct {
	ID   ConnID
	Name string
	X    int32
	Y    int32
}

type ClientConnectedEvent struct {
	ID         ConnID
	RemoteAddr string
	Name       string
	Monitors   []proto.Monitor
}

type MonitorsChangedEvent struct {
	ID         ConnID
	RemoteAddr string
	Name       string
	Monitors   []proto.Monitor
}

type ClientDisconnectedEvent struct {
	ID         ConnID
	RemoteAddr string
	Name       string
	Err        error
}

type RecvErrorEvent struct {
	ID         ConnID
	RemoteAddr string
	Name       string
	Err        error
}

type ByeEvent struct {
	ID         ConnID
	RemoteAddr string
	Name       string
	Reason     string
}

type ClipboardUnavailableEvent struct {
	Err error
}

func (ListeningEvent) isEvent()            {}
func (ServingViaRelayEvent) isEvent()      {}
func (ServerMonitorsEvent) isEvent()       {}
func (ClientConnectedEvent) isEvent()      {}
func (ClientPlacedEvent) isEvent()         {}
func (MonitorsChangedEvent) isEvent()      {}
func (ClientDisconnectedEvent) isEvent()   {}
func (RecvErrorEvent) isEvent()            {}
func (ByeEvent) isEvent()                  {}
func (ClipboardUnavailableEvent) isEvent() {}

// CertPaths returns the on-disk paths used for the server's self-signed cert.
// GUI callers use this to surface a fingerprint without calling Run.
func CertPaths() (certPath, keyPath string, err error) {
	dir, err := transport.ConfigDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"), nil
}

// Run blocks until ctx is cancelled or the listener fails. It ensures the
// self-signed cert exists on first call. Events are pushed to sink; sink must
// not block.
func Run(ctx context.Context, cfg Config, sink func(Event)) error {
	if sink == nil {
		sink = func(Event) {}
	}

	certPath, keyPath, err := CertPaths()
	if err != nil {
		return err
	}
	if err := transport.EnsureServerCert(certPath, keyPath); err != nil {
		return err
	}

	srvCfg := transport.ServerConfig{
		Addr:     cfg.Addr,
		CertFile: certPath,
		KeyFile:  keyPath,
		Token:    cfg.Token,
	}

	// Build the router so incoming clients can be registered as they connect.
	// A failure to enumerate monitors or open the injector is not fatal:
	// the server keeps running, just without local routing.
	disp := platform.New()
	serverMons, monErr := disp.Enumerate()
	if monErr != nil {
		slog.Warn("cannot enumerate local monitors; routing disabled", "err", monErr)
	}
	injector, injErr := platform.NewInjector()
	if injErr != nil {
		slog.Warn("cannot open input injector; routing disabled", "err", injErr)
	}
	capturer := platform.NewCapturer()

	var router *Router
	if monErr == nil && injErr == nil {
		router = NewRouter(serverMons, capturer, injector, cfg.Placements, cfg.OnPlacementChanged)
		if cfg.OnRouterReady != nil {
			cfg.OnRouterReady(router)
		}
		go func() {
			_ = router.Run(ctx)
			if injector != nil {
				_ = injector.Close()
			}
		}()
	}
	if monErr == nil {
		sink(ServerMonitorsEvent{Monitors: serverMons})
	}

	// Watch for hotplug / resolution changes on the host. Each snapshot
	// updates the router's server bbox (so grab/release math stays in
	// sync with the new layout) and is re-emitted to the GUI. Runs on a
	// separate goroutine so the listener start isn't blocked; exits when
	// ctx is cancelled. If the platform can't push events the goroutine
	// returns early and we just keep the initial snapshot — a periodic
	// poll fallback would belong here but isn't wired yet.
	if monErr == nil {
		ch := make(chan []proto.Monitor, 4)
		go func() {
			_ = disp.Subscribe(ctx, ch)
			close(ch)
		}()
		go func() {
			for mons := range ch {
				if router != nil {
					router.UpdateServerMonitors(mons)
				}
				sink(ServerMonitorsEvent{Monitors: mons})
			}
		}()
	}

	var (
		clipboard platform.Clipboard
		hub       *clipboardHub
	)
	if cfg.EnableClipboard {
		cb, cbErr := platform.NewClipboard()
		if cbErr != nil {
			sink(ClipboardUnavailableEvent{Err: cbErr})
		} else {
			clipboard = cb
			defer func() { _ = clipboard.Close() }()
		}
	}
	hub = newClipboardHub(clipboard)
	if hub != nil {
		hub.onItem = cfg.OnClipboardItem
		go hub.watchLocal(ctx)
	}
	handler := func(s *transport.Session, hello *proto.Hello) {
		handleClient(ctx, s, hello, router, hub, sink)
	}

	if cfg.RelayAddr != "" {
		sink(ServingViaRelayEvent{Relay: cfg.RelayAddr, Session: cfg.Session, CertPath: certPath})
		return transport.ListenViaRelay(ctx, srvCfg, cfg.RelayAddr, cfg.Session, handler)
	}
	sink(ListeningEvent{Addr: cfg.Addr, CertPath: certPath})
	return transport.Listen(ctx, srvCfg, handler)
}

func newConnID() ConnID {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return ConnID(hex.EncodeToString(b[:]))
}

func handleClient(ctx context.Context, s *transport.Session, hello *proto.Hello, router *Router, hub *clipboardHub, sink func(Event)) {
	id := newConnID()
	remote := s.RemoteAddr().String()
	name := hello.ClientName
	sink(ClientConnectedEvent{ID: id, RemoteAddr: remote, Name: name, Monitors: hello.Monitors})
	defer s.Close()
	// Unblock s.Recv() immediately when the server shuts down instead of
	// waiting up to 15 s for the read deadline to fire.
	go func() { <-ctx.Done(); _ = s.Close() }()

	if router != nil {
		p := router.Register(id, name, s, hello.Monitors)
		sink(ClientPlacedEvent{ID: id, Name: name, X: p.X, Y: p.Y})
		defer router.Unregister(id)
	}
	if hub != nil {
		hub.add(id, s)
		defer hub.remove(id)
	}

	for {
		if ctx.Err() != nil {
			sink(ClientDisconnectedEvent{ID: id, RemoteAddr: remote, Name: name, Err: ctx.Err()})
			return
		}
		_ = s.SetReadDeadline(time.Now().Add(15 * time.Second))
		msg, err := s.Recv()
		if err != nil {
			if ctx.Err() != nil {
				sink(ClientDisconnectedEvent{ID: id, RemoteAddr: remote, Name: name, Err: ctx.Err()})
				return
			}
			sink(RecvErrorEvent{ID: id, RemoteAddr: remote, Name: name, Err: err})
			sink(ClientDisconnectedEvent{ID: id, RemoteAddr: remote, Name: name})
			return
		}
		switch m := msg.(type) {
		case *proto.Ping:
			if err := s.Send(&proto.Pong{Seq: m.Seq}); err != nil {
				sink(RecvErrorEvent{ID: id, RemoteAddr: remote, Name: name, Err: err})
				sink(ClientDisconnectedEvent{ID: id, RemoteAddr: remote, Name: name, Err: err})
				return
			}
		case *proto.MonitorsChanged:
			sink(MonitorsChangedEvent{ID: id, RemoteAddr: remote, Name: name, Monitors: m.Monitors})
			if router != nil {
				router.UpdateMonitors(id, m.Monitors)
			}
		case *proto.Bye:
			sink(ByeEvent{ID: id, RemoteAddr: remote, Name: name, Reason: m.Reason})
			sink(ClientDisconnectedEvent{ID: id, RemoteAddr: remote, Name: name})
			return
		case *proto.ClipboardUpdate:
			if hub != nil && validClipboardPayload(m.Format, m.Data) {
				hub.applyRemote(name, m.Format, m.Data)
				hub.broadcast(id, m)
			}
		}
	}
}

// clipboardSuppressWindow is the time window after applyRemote during which
// the local watcher ignores changes. Covers the re-encoding round-trip on
// formats where hash(input) != hash(read-back) — notably PNG on Windows
// (DIB→PNG is non-deterministic) and DIB alpha-channel corrections.
const clipboardSuppressWindow = 1500 * time.Millisecond

type clipboardHub struct {
	clipboard platform.Clipboard

	mu           sync.Mutex
	sessions     map[ConnID]*transport.Session
	lastHash     [32]byte
	haveHash     bool
	suppressTill time.Time

	// onItem is invoked (without hub.mu held) whenever a clipboard snapshot
	// is processed — either from a peer (applyRemote) or locally
	// (watchLocal). Set once before Run spins up the watcher goroutine.
	onItem func(origin string, format proto.ClipboardFormat, data []byte)
}

func newClipboardHub(clipboard platform.Clipboard) *clipboardHub {
	if clipboard == nil {
		return nil
	}
	return &clipboardHub{
		clipboard: clipboard,
		sessions:  map[ConnID]*transport.Session{},
	}
}

func (h *clipboardHub) add(id ConnID, s *transport.Session) {
	h.mu.Lock()
	h.sessions[id] = s
	h.mu.Unlock()
}

func (h *clipboardHub) remove(id ConnID) {
	h.mu.Lock()
	delete(h.sessions, id)
	h.mu.Unlock()
}

// clipboardBroadcastTimeout bounds how long we'll wait on a single peer's
// Send. If exceeded, we close that session so one stalled client can't block
// the watcher goroutine and queue up clipboard updates for everyone else.
const clipboardBroadcastTimeout = 2 * time.Second

func (h *clipboardHub) broadcast(skip ConnID, msg *proto.ClipboardUpdate) {
	h.mu.Lock()
	peers := make([]*transport.Session, 0, len(h.sessions))
	for id, s := range h.sessions {
		if id == skip {
			continue
		}
		peers = append(peers, s)
	}
	h.mu.Unlock()
	for _, peer := range peers {
		if err := peer.SendWithTimeout(msg, clipboardBroadcastTimeout); err != nil {
			_ = peer.Close()
		}
	}
}

func (h *clipboardHub) applyRemote(origin string, format proto.ClipboardFormat, data []byte) {
	if !validClipboardPayload(format, data) {
		return
	}
	if err := h.clipboard.Write(format, data); err != nil {
		return
	}
	hash := clipboardHash(format, data)
	h.mu.Lock()
	h.lastHash = hash
	h.haveHash = true
	h.suppressTill = time.Now().Add(clipboardSuppressWindow)
	cb := h.onItem
	h.mu.Unlock()
	if cb != nil {
		cb(origin, format, data)
	}
}

func (h *clipboardHub) watchLocal(ctx context.Context) {
	_ = h.clipboard.Watch(ctx, func(format proto.ClipboardFormat, data []byte) {
		if !validClipboardPayload(format, data) {
			return
		}
		hash := clipboardHash(format, data)
		if h.shouldSuppress(hash) {
			return
		}
		msg := &proto.ClipboardUpdate{
			OriginID: "server",
			Seq:      uint64(time.Now().UnixNano()),
			Format:   format,
			Data:     append([]byte(nil), data...),
		}
		h.broadcast("", msg)
		h.setLastHash(hash)
		h.mu.Lock()
		cb := h.onItem
		h.mu.Unlock()
		if cb != nil {
			cb("server", format, data)
		}
	})
}

// shouldSuppress reports whether the watcher callback should swallow this
// update. Suppressed when it's an exact duplicate of the last emitted/applied
// content, or when we're still inside the post-applyRemote settling window
// (covers non-deterministic re-encoding paths).
func (h *clipboardHub) shouldSuppress(hash [32]byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.haveHash && h.lastHash == hash {
		return true
	}
	return time.Now().Before(h.suppressTill)
}

func (h *clipboardHub) setLastHash(hash [32]byte) {
	h.mu.Lock()
	h.lastHash = hash
	h.haveHash = true
	h.mu.Unlock()
}

func clipboardHash(format proto.ClipboardFormat, data []byte) [32]byte {
	sum := sha256.Sum256(append([]byte{byte(format)}, data...))
	return sum
}

func validClipboardPayload(format proto.ClipboardFormat, data []byte) bool {
	if len(data) == 0 || len(data) > proto.MaxClipboardData {
		return false
	}
	switch format {
	case proto.ClipboardFormatTextPlain, proto.ClipboardFormatImagePNG, proto.ClipboardFormatFilesList:
		return true
	default:
		return false
	}
}
