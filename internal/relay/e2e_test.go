package relay_test

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/titrom/rmouse/internal/proto"
	"github.com/titrom/rmouse/internal/relay"
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
		_ = relay.Run(relay.Config{Addr: addr, PairTTL: 5 * time.Second, PreambleTO: 2 * time.Second})
	}()
	time.Sleep(50 * time.Millisecond)
	return addr
}

// TestEndToEndPingPongViaRelay drives the whole stack: a relay hub, a server
// accepting one session via AcceptOneViaRelay, and a client dialing through
// DialViaRelay. Verifies that the existing Hello/Welcome + Ping/Pong protocol
// works unchanged when routed through a rendezvous proxy.
func TestEndToEndPingPongViaRelay(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "c.pem")
	keyPath := filepath.Join(dir, "k.pem")
	if err := transport.EnsureServerCert(certPath, keyPath); err != nil {
		t.Fatal(err)
	}

	relayAddr := startRelay(t)
	const sessID = "e2e-session-xyz"

	accepted := make(chan *transport.Session, 1)
	go func() {
		_ = transport.AcceptOneViaRelay(context.Background(), transport.ServerConfig{
			CertFile: certPath, KeyFile: keyPath, Token: "tok",
		}, relayAddr, sessID, func(s *transport.Session, _ *proto.Hello) {
			accepted <- s
		})
	}()

	time.Sleep(100 * time.Millisecond)

	cs, welcome, err := transport.DialViaRelay(transport.ClientConfig{
		ServerName: "rmouse",
		ClientName: "laptop",
		Token:      "tok",
		Monitors:   []proto.Monitor{{ID: 0, W: 1920, H: 1080, Primary: true}},
	}, relayAddr, sessID)
	if err != nil {
		t.Fatalf("DialViaRelay: %v", err)
	}
	defer cs.Close()
	if welcome.AssignedName != "laptop" {
		t.Errorf("welcome name: got %q", welcome.AssignedName)
	}

	ss := <-accepted
	defer ss.Close()

	if err := cs.Send(&proto.Ping{Seq: 42}); err != nil {
		t.Fatal(err)
	}
	msg, err := ss.Recv()
	if err != nil {
		t.Fatal(err)
	}
	ping, ok := msg.(*proto.Ping)
	if !ok || ping.Seq != 42 {
		t.Fatalf("unexpected: %T %#v", msg, msg)
	}
	if err := ss.Send(&proto.Pong{Seq: 42}); err != nil {
		t.Fatal(err)
	}
	msg, err = cs.Recv()
	if err != nil {
		t.Fatal(err)
	}
	pong, ok := msg.(*proto.Pong)
	if !ok || pong.Seq != 42 {
		t.Fatalf("unexpected: %T %#v", msg, msg)
	}
}

// TestBadTokenViaRelay confirms pairing token rejection still works end-to-end
// through the relay: the relay splices bytes, but TLS handshake + Hello check
// run between peers unchanged.
func TestBadTokenViaRelay(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "c.pem")
	keyPath := filepath.Join(dir, "k.pem")
	if err := transport.EnsureServerCert(certPath, keyPath); err != nil {
		t.Fatal(err)
	}

	relayAddr := startRelay(t)
	const sessID = "bad-token-test"

	go func() {
		_ = transport.AcceptOneViaRelay(context.Background(), transport.ServerConfig{
			CertFile: certPath, KeyFile: keyPath, Token: "right",
		}, relayAddr, sessID, func(s *transport.Session, _ *proto.Hello) { s.Close() })
	}()

	time.Sleep(100 * time.Millisecond)

	_, _, err := transport.DialViaRelay(transport.ClientConfig{
		ServerName: "rmouse", ClientName: "x",
		Token: "wrong", Monitors: []proto.Monitor{{ID: 0, W: 1, H: 1}},
	}, relayAddr, sessID)
	if err == nil {
		t.Fatal("expected error on bad token")
	}
}
