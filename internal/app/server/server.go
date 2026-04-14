// Package server implements the rmouse host run-loop: it ensures the TLS
// cert exists, accepts clients (directly or via relay), and echoes Pings.
// Both the CLI binary and the GUI import this package.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"path/filepath"
	"time"

	"github.com/titrom/rmouse/internal/proto"
	"github.com/titrom/rmouse/internal/transport"
)

// Config is the caller-supplied configuration for Run.
type Config struct {
	Addr      string // host:port; ignored when RelayAddr is set
	Token     string
	RelayAddr string // optional; when set, server dials the relay
	Session   string // relay session id; required iff RelayAddr != ""
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

func (ListeningEvent) isEvent()          {}
func (ServingViaRelayEvent) isEvent()    {}
func (ClientConnectedEvent) isEvent()    {}
func (MonitorsChangedEvent) isEvent()    {}
func (ClientDisconnectedEvent) isEvent() {}
func (RecvErrorEvent) isEvent()          {}
func (ByeEvent) isEvent()                {}

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

	handler := func(s *transport.Session, hello *proto.Hello) {
		handleClient(ctx, s, hello, sink)
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

func handleClient(ctx context.Context, s *transport.Session, hello *proto.Hello, sink func(Event)) {
	id := newConnID()
	remote := s.RemoteAddr().String()
	name := hello.ClientName
	sink(ClientConnectedEvent{ID: id, RemoteAddr: remote, Name: name, Monitors: hello.Monitors})
	defer s.Close()
	// Unblock s.Recv() immediately when the server shuts down instead of
	// waiting up to 15 s for the read deadline to fire.
	go func() { <-ctx.Done(); _ = s.Close() }()

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
		case *proto.Bye:
			sink(ByeEvent{ID: id, RemoteAddr: remote, Name: name, Reason: m.Reason})
			sink(ClientDisconnectedEvent{ID: id, RemoteAddr: remote, Name: name})
			return
		}
	}
}
