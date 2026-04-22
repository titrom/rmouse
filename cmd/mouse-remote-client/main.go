// mouse-remote-client is the rmouse target binary. For M1 it dials the
// server, completes the TLS + pairing handshake, and runs a Ping/Pong loop.
// A monitor-hotplug watcher runs alongside: layout changes are pushed to the
// server as MonitorsChanged while a session is live.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/titrom/rmouse/internal/app/client"
)

func main() {
	addr := flag.String("connect", "127.0.0.1:24242", "server host:port (ignored when --relay is set)")
	token := flag.String("token", "", "shared pairing token (required)")
	name := flag.String("name", "", "client name (default: hostname)")
	interval := flag.Duration("ping", 2*time.Second, "ping interval")
	relayAddr := flag.String("relay", "", "relay host:port; when set, client dials the relay instead of the server directly")
	session := flag.String("session", "", "relay session id (required with --relay)")
	clipboard := flag.Bool("clipboard", false, "enable shared clipboard sync (Windows only)")
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
	if *name == "" {
		h, _ := os.Hostname()
		if h == "" {
			h = "client"
		}
		*name = h
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	cfg := client.Config{
		Addr:            *addr,
		Token:           *token,
		Name:            *name,
		PingInterval:    *interval,
		RelayAddr:       *relayAddr,
		Session:         *session,
		EnableClipboard: *clipboard,
	}

	if err := client.Run(ctx, cfg, logEvent); err != nil && err != context.Canceled {
		log.Error("client stopped", "err", err)
		os.Exit(1)
	}
}

// logEvent renders client.Event values in the original CLI log format so
// that live-tests and existing scrapers keep working after the refactor.
func logEvent(ev client.Event) {
	switch e := ev.(type) {
	case client.MonitorsEvent:
		if e.Live {
			slog.Info("monitors changed", "count", len(e.Monitors))
		}
		for _, m := range e.Monitors {
			if e.Live {
				slog.Info("  monitor", "id", m.ID, "name", m.Name, "pos", [2]int32{m.X, m.Y}, "size", [2]uint32{m.W, m.H}, "primary", m.Primary)
			} else {
				slog.Info("local monitor", "id", m.ID, "name", m.Name, "pos", [2]int32{m.X, m.Y}, "size", [2]uint32{m.W, m.H}, "primary", m.Primary)
			}
		}
	case client.StatusEvent:
		switch e.State {
		case client.StateConnected:
			slog.Info("connected", "assigned_name", e.AssignedName)
		case client.StateDisconnected:
			slog.Info("session ended", "err", e.Err, "retry_in", e.RetryIn)
		}
	case client.PongEvent:
		slog.Debug("pong", "seq", e.Seq)
	case client.HotplugUnavailableEvent:
		slog.Warn("monitor hotplug unavailable; running without live updates", "err", e.Err)
	case client.InjectorUnavailableEvent:
		slog.Warn("input injection unavailable; received input will be dropped", "err", e.Err)
	case client.GrabEvent:
		slog.Info("grab", "on", e.On)
	case client.ClipboardUnavailableEvent:
		slog.Warn("clipboard sync unavailable", "err", e.Err)
	}
}
