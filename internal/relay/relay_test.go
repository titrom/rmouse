package relay

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/titrom/rmouse/internal/transport"
)

func startRelay(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	lis.Close()
	go func() {
		_ = Run(Config{Addr: addr, PairTTL: 2 * time.Second, PreambleTO: time.Second})
	}()
	// brief wait so the listener is bound
	time.Sleep(50 * time.Millisecond)
	return addr
}

func TestRelayPairsTwoSides(t *testing.T) {
	addr := startRelay(t)

	serverConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer serverConn.Close()
	if err := transport.WritePreamble(serverConn, transport.SideServer, "sess-1"); err != nil {
		t.Fatal(err)
	}

	clientConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()
	if err := transport.WritePreamble(clientConn, transport.SideClient, "sess-1"); err != nil {
		t.Fatal(err)
	}

	// Once paired the relay copies bytes in both directions.
	if _, err := serverConn.Write([]byte("hello-from-server")); err != nil {
		t.Fatal(err)
	}
	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, len("hello-from-server"))
	if _, err := io.ReadFull(clientConn, buf); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(buf) != "hello-from-server" {
		t.Fatalf("got %q", string(buf))
	}

	if _, err := clientConn.Write([]byte("hello-from-client")); err != nil {
		t.Fatal(err)
	}
	_ = serverConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf = make([]byte, len("hello-from-client"))
	if _, err := io.ReadFull(serverConn, buf); err != nil {
		t.Fatalf("server read: %v", err)
	}
	if string(buf) != "hello-from-client" {
		t.Fatalf("got %q", string(buf))
	}
}

func TestRelayPairingTimeout(t *testing.T) {
	addr := startRelay(t)

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := transport.WritePreamble(c, transport.SideServer, "lonely"); err != nil {
		t.Fatal(err)
	}

	_ = c.SetReadDeadline(time.Now().Add(4 * time.Second))
	buf := make([]byte, 1)
	_, err = c.Read(buf)
	if err == nil {
		t.Fatal("expected relay to close lonely connection after pair TTL")
	}
}
