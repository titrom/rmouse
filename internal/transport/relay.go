package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/titrom/rmouse/internal/proto"
)

// RelayPoolSize is how many concurrent server-side waiters ListenViaRelay
// registers on the relay for a single session id. Each free slot lets one
// additional client pair in parallel. When a client disconnects its slot
// redials the relay, so the pool stays replenished.
const RelayPoolSize = 4

// ListenViaRelay dials the relay, announces itself as the server side of the
// given session, and keeps a pool of RelayPoolSize waiters there so that
// multiple clients on that session can pair concurrently. Blocks until ctx
// is cancelled or a fatal TLS-config error occurs.
func ListenViaRelay(ctx context.Context, cfg ServerConfig, relayAddr, sessionID string, handler func(*Session, *proto.Hello)) error {
	tlsCfg, err := serverTLSConfig(cfg)
	if err != nil {
		return err
	}
	for i := 0; i < RelayPoolSize; i++ {
		go relayPoolWorker(ctx, cfg, tlsCfg, relayAddr, sessionID, handler)
	}
	<-ctx.Done()
	return ctx.Err()
}

func relayPoolWorker(ctx context.Context, cfg ServerConfig, tlsCfg *tls.Config, relayAddr, sessionID string, handler func(*Session, *proto.Hello)) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := acceptOneViaRelayWithTLS(ctx, cfg, tlsCfg, relayAddr, sessionID, handler)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Info("relay session ended", "err", err, "retry_in", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		} else {
			backoff = time.Second
		}
	}
}

// AcceptOneViaRelay dials the relay, waits for a client side with the same
// session id, runs the TLS + Hello handshake, and invokes handler for the
// single resulting session. Returns after handler returns or on any error.
func AcceptOneViaRelay(ctx context.Context, cfg ServerConfig, relayAddr, sessionID string, handler func(*Session, *proto.Hello)) error {
	tlsCfg, err := serverTLSConfig(cfg)
	if err != nil {
		return err
	}
	return acceptOneViaRelayWithTLS(ctx, cfg, tlsCfg, relayAddr, sessionID, handler)
}

func acceptOneViaRelayWithTLS(ctx context.Context, cfg ServerConfig, tlsCfg *tls.Config, relayAddr, sessionID string, handler func(*Session, *proto.Hello)) error {
	raw, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", relayAddr)
	if err != nil {
		return fmt.Errorf("dial relay: %w", err)
	}
	_ = raw.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := WritePreamble(raw, SideServer, sessionID); err != nil {
		raw.Close()
		return fmt.Errorf("write preamble: %w", err)
	}
	_ = raw.SetWriteDeadline(time.Time{})

	// tls.Server does not perform the handshake until the first IO; force it
	// now so pairing errors surface here instead of inside the handler.
	tc := tls.Server(raw, tlsCfg)
	_ = tc.SetDeadline(time.Now().Add(30 * time.Second))
	if err := tc.Handshake(); err != nil {
		tc.Close()
		return fmt.Errorf("tls handshake: %w", err)
	}
	_ = tc.SetDeadline(time.Time{})

	serveAcceptedConn(tc, cfg.Token, handler)
	return nil
}

// DialViaRelay dials the relay, announces itself as the client side of the
// given session, upgrades the pipe to TLS, and runs the Hello/Welcome exchange.
func DialViaRelay(cfg ClientConfig, relayAddr, sessionID string) (*Session, *proto.Welcome, error) {
	raw, err := net.DialTimeout("tcp", relayAddr, 10*time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("dial relay: %w", err)
	}
	_ = raw.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := WritePreamble(raw, SideClient, sessionID); err != nil {
		raw.Close()
		return nil, nil, fmt.Errorf("write preamble: %w", err)
	}
	_ = raw.SetWriteDeadline(time.Time{})

	tc := tls.Client(raw, clientTLSConfig(cfg))
	_ = tc.SetDeadline(time.Now().Add(30 * time.Second))
	if err := tc.Handshake(); err != nil {
		tc.Close()
		return nil, nil, fmt.Errorf("tls handshake: %w", err)
	}
	_ = tc.SetDeadline(time.Time{})

	return clientHandshake(tc, cfg)
}
