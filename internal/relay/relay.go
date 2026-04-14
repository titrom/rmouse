// Package relay implements a public rendezvous server that pairs two TCP
// connections with the same session id and copies bytes between them. The
// relay does not participate in the TLS handshake of the peers it connects;
// end-to-end encryption stays between server and client.
//
// Pairing is FIFO per (session-id, side): both sides may queue multiple
// waiters, and the next arrival of the opposite side is paired with the
// oldest queued peer. This lets a single server-side pre-register a pool
// of connections on one session-id so that N clients can pair concurrently.
package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/titrom/rmouse/internal/transport"
)

// Config are tunables for Run.
type Config struct {
	Addr       string        // listen host:port
	PairTTL    time.Duration // how long a queued waiter holds before being dropped
	PreambleTO time.Duration // max time to read the rendezvous preamble
}

// Run starts a relay listener on cfg.Addr. Blocks until the listener closes.
func Run(cfg Config) error {
	if cfg.PairTTL <= 0 {
		cfg.PairTTL = 60 * time.Second
	}
	if cfg.PreambleTO <= 0 {
		cfg.PreambleTO = 5 * time.Second
	}
	lis, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	defer lis.Close()
	slog.Info("relay listening", "addr", lis.Addr().String())

	h := &hub{queues: make(map[queueKey][]*pending)}
	for {
		c, err := lis.Accept()
		if err != nil {
			return err
		}
		go h.handle(c, cfg)
	}
}

type pending struct {
	conn    net.Conn
	claimed chan struct{}
}

type queueKey struct {
	sid  string
	side transport.Side
}

type hub struct {
	mu     sync.Mutex
	queues map[queueKey][]*pending
}

// tryPair looks for a waiter of the opposite side to claim. Returns nil if
// none queued.
func (h *hub) tryPair(sid string, side transport.Side) *pending {
	oppKey := queueKey{sid: sid, side: oppositeSide(side)}
	h.mu.Lock()
	defer h.mu.Unlock()
	q := h.queues[oppKey]
	if len(q) == 0 {
		return nil
	}
	p := q[0]
	h.queues[oppKey] = q[1:]
	if len(h.queues[oppKey]) == 0 {
		delete(h.queues, oppKey)
	}
	close(p.claimed)
	return p
}

// enqueue appends self to its own-side queue and returns the pending entry
// plus a remover to call on TTL expiry.
func (h *hub) enqueue(sid string, side transport.Side, p *pending) func() {
	key := queueKey{sid: sid, side: side}
	h.mu.Lock()
	h.queues[key] = append(h.queues[key], p)
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		q := h.queues[key]
		for i, x := range q {
			if x == p {
				h.queues[key] = append(q[:i], q[i+1:]...)
				if len(h.queues[key]) == 0 {
					delete(h.queues, key)
				}
				return
			}
		}
	}
}

func (h *hub) handle(c net.Conn, cfg Config) {
	_ = c.SetReadDeadline(time.Now().Add(cfg.PreambleTO))
	side, sid, err := transport.ReadPreamble(c)
	if err != nil {
		slog.Debug("relay: bad preamble", "remote", c.RemoteAddr(), "err", err)
		c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})

	sidTag := hashTag(sid)
	log := slog.With("remote", c.RemoteAddr().String(), "session", sidTag, "side", sideName(side))

	// Fast path: opposite side already queued.
	if peer := h.tryPair(sid, side); peer != nil {
		log.Info("relay: paired")
		splice(c, peer.conn, log)
		return
	}
	// Slow path: queue and wait.
	self := &pending{conn: c, claimed: make(chan struct{})}
	remove := h.enqueue(sid, side, self)
	log.Info("relay: waiting for peer")

	select {
	case <-self.claimed:
		// The matching tryPair call removed us from the queue, popped us,
		// and will splice this conn with the caller.
		// But since we are not the caller, we need to splice ourselves with
		// the other side. Actually tryPair handed our `pending` to the
		// newcomer; the newcomer splices. We just return — splice() on the
		// newcomer's goroutine will close our conn when it finishes.
		return
	case <-time.After(cfg.PairTTL):
		remove()
		log.Info("relay: pairing timeout")
		c.Close()
	}
}

// splice copies bytes between a and b until either side closes. Returns only
// after both copiers exit.
func splice(a, b net.Conn, log *slog.Logger) {
	defer a.Close()
	defer b.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	copyOne := func(dst, src net.Conn) {
		defer wg.Done()
		_, err := io.Copy(dst, src)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			log.Debug("relay: copy ended", "err", err)
		}
		if tcp, ok := dst.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}
	go copyOne(a, b)
	go copyOne(b, a)
	wg.Wait()
}

func oppositeSide(s transport.Side) transport.Side {
	if s == transport.SideServer {
		return transport.SideClient
	}
	return transport.SideServer
}

func sideName(s transport.Side) string {
	switch s {
	case transport.SideServer:
		return "server"
	case transport.SideClient:
		return "client"
	}
	return "unknown"
}

// hashTag returns a short, non-reversible tag for logs so session secrets
// aren't printed verbatim.
func hashTag(sid string) string {
	h := sha256.Sum256([]byte(sid))
	return hex.EncodeToString(h[:4])
}
