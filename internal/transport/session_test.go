package transport

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/titrom/rmouse/internal/proto"
)

// setupCerts creates a throwaway cert/key pair in a tempdir.
func setupCerts(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := EnsureServerCert(certPath, keyPath); err != nil {
		t.Fatal(err)
	}
	return
}

// runLoopback spawns a Listen in a goroutine on an OS-assigned port and
// returns the actual address plus a teardown func.
func runLoopback(t *testing.T, cfg ServerConfig, handler func(*Session, *proto.Hello)) (addr string, cleanup func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Addr = lis.Addr().String()
	lis.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Listen(ctx, cfg, handler)
	}()
	// brief wait so the listener is up — avoids races in fast machines.
	time.Sleep(50 * time.Millisecond)
	return cfg.Addr, func() {
		cancel()
		<-done
	}
}

func TestPairingAndPingPong(t *testing.T) {
	certPath, keyPath := setupCerts(t)
	sessions := make(chan *Session, 1)
	addr, cleanup := runLoopback(t, ServerConfig{
		CertFile: certPath,
		KeyFile:  keyPath,
		Token:    "correct-horse",
	}, func(s *Session, _ *proto.Hello) {
		sessions <- s
	})
	defer cleanup()

	cs, welcome, err := Dial(ClientConfig{
		Addr:       addr,
		ServerName: "rmouse",
		ClientName: "laptop",
		Token:      "correct-horse",
		Monitors:   []proto.Monitor{{ID: 0, W: 1920, H: 1080, Primary: true}},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer cs.Close()
	if welcome.AssignedName != "laptop" {
		t.Errorf("welcome name: got %q", welcome.AssignedName)
	}

	ss := <-sessions
	defer ss.Close()

	if err := cs.Send(&proto.Ping{Seq: 7}); err != nil {
		t.Fatal(err)
	}
	msg, err := ss.Recv()
	if err != nil {
		t.Fatal(err)
	}
	ping, ok := msg.(*proto.Ping)
	if !ok || ping.Seq != 7 {
		t.Fatalf("unexpected msg: %T %#v", msg, msg)
	}

	if err := ss.Send(&proto.Pong{Seq: 7}); err != nil {
		t.Fatal(err)
	}
	msg, err = cs.Recv()
	if err != nil {
		t.Fatal(err)
	}
	pong, ok := msg.(*proto.Pong)
	if !ok || pong.Seq != 7 {
		t.Fatalf("unexpected msg: %T %#v", msg, msg)
	}
}

func TestBadToken(t *testing.T) {
	certPath, keyPath := setupCerts(t)
	addr, cleanup := runLoopback(t, ServerConfig{
		CertFile: certPath, KeyFile: keyPath, Token: "right",
	}, func(s *Session, _ *proto.Hello) { s.Close() })
	defer cleanup()

	_, _, err := Dial(ClientConfig{
		Addr: addr, ServerName: "rmouse",
		ClientName: "x", Token: "wrong", Monitors: []proto.Monitor{{ID: 0, W: 1, H: 1}},
	})
	if err == nil {
		t.Fatal("expected error on bad token")
	}
	// server closes on ErrBadToken; client sees EOF or read error.
	if errors.Is(err, ErrBadToken) {
		// direct propagation would be unusual, but harmless
	}
}
