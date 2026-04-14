package relay

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/titrom/rmouse/internal/transport"
)

// TestRelayPoolPairsConcurrently verifies the multi-client pairing: the
// server side pre-registers N connections on one session-id, then N clients
// arrive and every one of them is paired to a distinct server-side waiter.
func TestRelayPoolPairsConcurrently(t *testing.T) {
	const N = 3
	addr := startRelay(t)

	// Register N server-side waiters on the same session id.
	serverConns := make([]net.Conn, N)
	for i := 0; i < N; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		if err := transport.WritePreamble(c, transport.SideServer, "pool-1"); err != nil {
			t.Fatal(err)
		}
		serverConns[i] = c
	}
	defer func() {
		for _, c := range serverConns {
			_ = c.Close()
		}
	}()

	// Brief pause so all server-side preambles are read and queued before
	// the clients arrive. Otherwise a fast client could beat the last
	// server into the queue.
	time.Sleep(100 * time.Millisecond)

	// Each client writes a unique byte so we can verify which server received it.
	var wg sync.WaitGroup
	got := make(chan byte, N)
	for _, sc := range serverConns {
		wg.Add(1)
		sc := sc
		go func() {
			defer wg.Done()
			_ = sc.SetReadDeadline(time.Now().Add(3 * time.Second))
			b := make([]byte, 1)
			if _, err := io.ReadFull(sc, b); err != nil {
				t.Errorf("server read: %v", err)
				return
			}
			got <- b[0]
		}()
	}

	for i := 0; i < N; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err := transport.WritePreamble(c, transport.SideClient, "pool-1"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write([]byte{byte('A' + i)}); err != nil {
			t.Fatal(err)
		}
	}

	wg.Wait()
	close(got)
	seen := map[byte]bool{}
	for b := range got {
		seen[b] = true
	}
	if len(seen) != N {
		t.Fatalf("expected %d distinct bytes received, got %d (%v)", N, len(seen), seen)
	}
}

// TestRelayExcessClientTimesOut: one server waiter, two clients. First client
// pairs, the second (no remaining server) waits and then times out.
func TestRelayExcessClientTimesOut(t *testing.T) {
	addr := startRelay(t)

	s, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := transport.WritePreamble(s, transport.SideServer, "excess"); err != nil {
		t.Fatal(err)
	}

	c1, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	if err := transport.WritePreamble(c1, transport.SideClient, "excess"); err != nil {
		t.Fatal(err)
	}

	// First pair proves the path works.
	if _, err := s.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	_ = c1.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2)
	if _, err := io.ReadFull(c1, buf); err != nil {
		t.Fatalf("c1 read: %v", err)
	}

	// Second client has no server to pair with and must time out.
	c2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if err := transport.WritePreamble(c2, transport.SideClient, "excess"); err != nil {
		t.Fatal(err)
	}
	_ = c2.SetReadDeadline(time.Now().Add(4 * time.Second))
	one := make([]byte, 1)
	if _, err := c2.Read(one); err == nil {
		t.Fatal("expected second client to be closed after TTL")
	}
}
