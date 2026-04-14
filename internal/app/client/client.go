// Package client implements the rmouse client run-loop: it handles monitor
// enumeration + hotplug, dials the server (direct or via relay), and keeps
// the session alive with Ping/Pong until ctx is cancelled. Both the CLI
// binary and the GUI import this package so the behaviour stays identical.
package client

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/titrom/rmouse/internal/platform"
	"github.com/titrom/rmouse/internal/proto"
	"github.com/titrom/rmouse/internal/transport"
)

// Config is the caller-supplied configuration for Run. All fields are
// required unless marked otherwise.
type Config struct {
	Addr         string // server host:port; ignored when RelayAddr is set
	ServerName   string // TLS SNI, defaults to "rmouse" when empty
	Token        string // shared pairing token
	Name         string // reported to server; caller should default to hostname
	PingInterval time.Duration
	RelayAddr    string // optional; when set client dials the relay
	Session      string // relay session id; required iff RelayAddr != ""
}

// State is the coarse lifecycle state surfaced to callers.
type State string

const (
	StateConnecting   State = "connecting"
	StateConnected    State = "connected"
	StateDisconnected State = "disconnected"
)

// Event is a sum type of things Run reports to its caller. Callers type-switch
// to render/log what they care about.
type Event interface{ isEvent() }

type StatusEvent struct {
	State        State
	AssignedName string        // set when State == StateConnected
	Err          error         // set when State == StateDisconnected with a non-nil error
	RetryIn      time.Duration // set when State == StateDisconnected; next retry delay
}

type MonitorsEvent struct {
	Monitors []proto.Monitor
	Live     bool // true for hotplug updates from Subscribe; false for initial Enumerate
}

type PongEvent struct {
	Seq uint32
}

type HotplugUnavailableEvent struct {
	Err error
}

// InjectorUnavailableEvent is emitted once at startup if the platform can't
// open its input injector (e.g. /dev/uinput permission denied). The session
// still connects — it just can't inject received input.
type InjectorUnavailableEvent struct {
	Err error
}

// GrabEvent mirrors a Grab message received from the server: On=true means
// the server believes this client currently owns the cursor.
type GrabEvent struct {
	On bool
}

func (StatusEvent) isEvent()              {}
func (MonitorsEvent) isEvent()            {}
func (PongEvent) isEvent()                {}
func (HotplugUnavailableEvent) isEvent()  {}
func (InjectorUnavailableEvent) isEvent() {}
func (GrabEvent) isEvent()                {}

// Run blocks until ctx is cancelled. It reports lifecycle events through sink.
// sink must not block — callers that need buffering should wrap their own
// goroutine. Returning an error only on fatal-at-startup conditions (e.g.
// monitor enumeration failing on a truly headless machine); otherwise Run
// loops forever with exponential backoff.
func Run(ctx context.Context, cfg Config, sink func(Event)) error {
	if cfg.ServerName == "" {
		cfg.ServerName = "rmouse"
	}
	if sink == nil {
		sink = func(Event) {}
	}

	disp := platform.New()
	initial, err := disp.Enumerate()
	if err != nil {
		return err
	}
	sink(MonitorsEvent{Monitors: initial, Live: false})

	mons := newMonitorStore(initial)
	go watchMonitors(ctx, disp, mons, sink)

	injector, injErr := platform.NewInjector()
	if injErr != nil {
		sink(InjectorUnavailableEvent{Err: injErr})
		injector = nil
	} else {
		defer func() { _ = injector.Close() }()
	}

	backoff := time.Second
	for ctx.Err() == nil {
		sink(StatusEvent{State: StateConnecting})
		sessionErr := runOnce(ctx, cfg, mons, injector, sink)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sink(StatusEvent{State: StateDisconnected, Err: sessionErr, RetryIn: backoff})
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	return ctx.Err()
}

type monitorStore struct {
	mu        sync.Mutex
	current   []proto.Monitor
	listeners []chan struct{}
}

func newMonitorStore(initial []proto.Monitor) *monitorStore {
	return &monitorStore{current: append([]proto.Monitor(nil), initial...)}
}

func (s *monitorStore) snapshot() []proto.Monitor {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]proto.Monitor(nil), s.current...)
}

func (s *monitorStore) set(mons []proto.Monitor) {
	s.mu.Lock()
	s.current = append([]proto.Monitor(nil), mons...)
	ls := s.listeners
	s.mu.Unlock()
	for _, ch := range ls {
		if ch == nil {
			continue
		}
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *monitorStore) subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	idx := -1
	for i, slot := range s.listeners {
		if slot == nil {
			s.listeners[i] = ch
			idx = i
			break
		}
	}
	if idx == -1 {
		s.listeners = append(s.listeners, ch)
		idx = len(s.listeners) - 1
	}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		s.listeners[idx] = nil
		s.mu.Unlock()
	}
}

func watchMonitors(ctx context.Context, disp platform.Display, store *monitorStore, sink func(Event)) {
	ch := make(chan []proto.Monitor, 4)
	done := make(chan error, 1)
	go func() { done <- disp.Subscribe(ctx, ch) }()
	for {
		select {
		case <-ctx.Done():
			<-done
			return
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				sink(HotplugUnavailableEvent{Err: err})
			}
			return
		case snap := <-ch:
			store.set(snap)
			sink(MonitorsEvent{Monitors: snap, Live: true})
		}
	}
}

func runOnce(ctx context.Context, cfg Config, mons *monitorStore, injector platform.Injector, sink func(Event)) error {
	snapshot := mons.snapshot()
	tcfg := transport.ClientConfig{
		Addr:       cfg.Addr,
		ServerName: cfg.ServerName,
		ClientName: cfg.Name,
		Token:      cfg.Token,
		Monitors:   snapshot,
	}
	var (
		sess    *transport.Session
		welcome *proto.Welcome
		err     error
	)
	if cfg.RelayAddr != "" {
		sess, welcome, err = transport.DialViaRelay(tcfg, cfg.RelayAddr, cfg.Session)
	} else {
		sess, welcome, err = transport.Dial(tcfg)
	}
	if err != nil {
		return err
	}
	defer sess.Close()
	sink(StatusEvent{State: StateConnected, AssignedName: welcome.AssignedName})

	updates, unsubscribe := mons.subscribe()
	defer unsubscribe()

	ticker := time.NewTicker(cfg.PingInterval)
	defer ticker.Stop()

	errs := make(chan error, 2)
	seq := uint32(0)

	go func() {
		for {
			_ = sess.SetReadDeadline(time.Now().Add(3 * cfg.PingInterval))
			msg, err := sess.Recv()
			if err != nil {
				errs <- err
				return
			}
			dispatchIncoming(msg, injector, mons, sink)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			seq++
			if err := sess.Send(&proto.Ping{Seq: seq}); err != nil {
				return err
			}
		case <-updates:
			snap := mons.snapshot()
			if err := sess.Send(&proto.MonitorsChanged{Monitors: snap}); err != nil {
				return err
			}
		case err := <-errs:
			return err
		}
	}
}

// dispatchIncoming routes a received message. Pong is surfaced as a status
// event; input messages are handed to the injector (if available). Unknown
// messages are ignored so future protocol additions don't break old clients.
func dispatchIncoming(msg proto.Message, injector platform.Injector, mons *monitorStore, sink func(Event)) {
	switch m := msg.(type) {
	case *proto.Pong:
		sink(PongEvent{Seq: m.Seq})
	case *proto.Grab:
		sink(GrabEvent{On: m.On})
	case *proto.MouseMove:
		if injector != nil {
			_ = injector.MouseMoveRel(int32(m.DX), int32(m.DY))
		}
	case *proto.MouseAbs:
		if injector != nil {
			x, y, ok := resolveAbs(mons.snapshot(), m)
			if ok {
				_ = injector.MouseMoveAbs(x, y)
			}
		}
	case *proto.MouseButtonEvent:
		if injector != nil {
			_ = injector.MouseButton(m.Button, m.Down)
		}
	case *proto.MouseWheel:
		if injector != nil {
			_ = injector.MouseWheel(m.DX, m.DY)
		}
	case *proto.KeyEvent:
		if injector != nil {
			_ = injector.KeyEvent(m.KeyCode, m.Down)
		}
	}
}

// resolveAbs translates a MouseAbs message (monitor-local coords) to the
// local virtual-desktop pixel position by adding the referenced monitor's
// origin. Returns ok=false if MonitorID doesn't match any known monitor.
func resolveAbs(monitors []proto.Monitor, m *proto.MouseAbs) (int32, int32, bool) {
	for _, mon := range monitors {
		if mon.ID == m.MonitorID {
			return mon.X + int32(m.X), mon.Y + int32(m.Y), true
		}
	}
	return 0, 0, false
}
