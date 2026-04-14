// mouse-remote-relay is a public rendezvous server that pairs two rmouse
// peers (server + client) by shared session id and copies bytes between them.
// TLS stays end-to-end between the peers; the relay only splices raw TCP.
package main

import (
	"flag"
	"log/slog"
	"os"
	"time"

	"github.com/titrom/rmouse/internal/relay"
)

func main() {
	addr := flag.String("listen", "0.0.0.0:24243", "host:port to listen on")
	pairTTL := flag.Duration("pair-ttl", 60*time.Second, "how long one side waits for the other")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	err := relay.Run(relay.Config{
		Addr:    *addr,
		PairTTL: *pairTTL,
	})
	if err != nil {
		log.Error("relay", "err", err)
		os.Exit(1)
	}
}
