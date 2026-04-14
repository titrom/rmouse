// mouse-remote-server is the rmouse host binary. For M1 it only accepts
// TLS clients and echoes Pings with Pongs.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"

	"github.com/titrom/rmouse/internal/app/server"
)

func main() {
	addr := flag.String("listen", "0.0.0.0:24242", "host:port to listen on (ignored when --relay is set)")
	token := flag.String("token", "", "shared pairing token (required)")
	relayAddr := flag.String("relay", "", "relay host:port; when set, server dials the relay instead of listening locally")
	session := flag.String("session", "", "relay session id (required with --relay)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if *token == "" {
		log.Error("missing --token")
		os.Exit(2)
	}
	if (*relayAddr == "") != (*session == "") {
		log.Error("--relay and --session must be set together")
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	cfg := server.Config{
		Addr:      *addr,
		Token:     *token,
		RelayAddr: *relayAddr,
		Session:   *session,
	}

	if err := server.Run(ctx, cfg, logEvent); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}

func logEvent(ev server.Event) {
	switch e := ev.(type) {
	case server.ListeningEvent:
		slog.Info("listening", "addr", e.Addr, "cert", e.CertPath)
	case server.ServingViaRelayEvent:
		slog.Info("serving via relay", "relay", e.Relay, "cert", e.CertPath)
	case server.ClientConnectedEvent:
		l := slog.With("remote", e.RemoteAddr, "client", e.Name)
		l.Info("client connected", "monitors", len(e.Monitors))
		for _, m := range e.Monitors {
			l.Info("  monitor", "id", m.ID, "name", m.Name, "pos", [2]int32{m.X, m.Y}, "size", [2]uint32{m.W, m.H}, "primary", m.Primary)
		}
	case server.MonitorsChangedEvent:
		l := slog.With("remote", e.RemoteAddr, "client", e.Name)
		l.Info("monitors changed", "count", len(e.Monitors))
		for _, m := range e.Monitors {
			l.Info("  monitor", "id", m.ID, "name", m.Name, "pos", [2]int32{m.X, m.Y}, "size", [2]uint32{m.W, m.H}, "primary", m.Primary)
		}
	case server.RecvErrorEvent:
		slog.With("remote", e.RemoteAddr, "client", e.Name).Info("recv ended", "err", e.Err)
	case server.ByeEvent:
		slog.With("remote", e.RemoteAddr, "client", e.Name).Info("bye", "reason", e.Reason)
	case server.ClientDisconnectedEvent:
		slog.With("remote", e.RemoteAddr, "client", e.Name).Info("client disconnected")
	}
}
