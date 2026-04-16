// Package transport wraps TLS connections and framed proto messages into a
// Session used by both server and client.
package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/titrom/rmouse/internal/proto"
)

var ErrBadToken = errors.New("transport: bad pairing token")

// Session is a framed, TLS-protected bidirectional stream of proto messages.
// Writes are serialized internally; reads are expected from a single goroutine.
type Session struct {
	conn net.Conn
	mu   sync.Mutex
}

func newSession(c net.Conn) *Session {
	// Nagle batches small writes into bursts every ~40ms — fine for bulk
	// streams but visible as jitter when our 1-byte-frame mouse positions
	// arrive at the client all at once. Disable it on the underlying TCP
	// socket. tls.Conn (and any other wrapper exposing NetConn()) is unwrapped
	// until we hit a real *net.TCPConn, then SetNoDelay is best-effort.
	type netConner interface{ NetConn() net.Conn }
	cur := c
	for cur != nil {
		if tcp, ok := cur.(*net.TCPConn); ok {
			_ = tcp.SetNoDelay(true)
			break
		}
		w, ok := cur.(netConner)
		if !ok {
			break
		}
		cur = w.NetConn()
	}
	return &Session{conn: c}
}

// Send writes one framed message.
func (s *Session) Send(m proto.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return proto.Write(s.conn, m)
}

// Recv reads one framed message.
func (s *Session) Recv() (proto.Message, error) {
	return proto.Read(s.conn)
}

// SetReadDeadline sets a per-read deadline, used for heartbeat timeouts.
func (s *Session) SetReadDeadline(t time.Time) error {
	return s.conn.SetReadDeadline(t)
}

// RemoteAddr returns the remote peer address.
func (s *Session) RemoteAddr() net.Addr { return s.conn.RemoteAddr() }

func (s *Session) Close() error { return s.conn.Close() }

// ServerConfig holds per-instance server settings.
type ServerConfig struct {
	Addr     string // host:port
	CertFile string
	KeyFile  string
	Token    string // required pairing token
}

// serverTLSConfig builds the TLS config used for both direct-listen and
// relay-accepted connections.
func serverTLSConfig(cfg ServerConfig) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load cert: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		ClientAuth:   tls.NoClientCert,
	}, nil
}

// Listen starts a TLS listener and calls handler for every accepted session
// that passes the pairing handshake. Blocks until ctx is cancelled or the
// listener fails.
func Listen(ctx context.Context, cfg ServerConfig, handler func(*Session, *proto.Hello)) error {
	tlsCfg, err := serverTLSConfig(cfg)
	if err != nil {
		return err
	}
	lis, err := tls.Listen("tcp", cfg.Addr, tlsCfg)
	if err != nil {
		return err
	}
	defer lis.Close()
	go func() { <-ctx.Done(); lis.Close() }()
	for {
		c, err := lis.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		go serveAcceptedConn(c, cfg.Token, handler)
	}
}

// serveAcceptedConn runs the pairing handshake on an already-TLS-wrapped conn
// and dispatches to handler. Used by both Listen and the relay-mode accept path.
func serveAcceptedConn(c net.Conn, token string, handler func(*Session, *proto.Hello)) {
	sess := newSession(c)
	hello, err := acceptHandshake(sess, token)
	if err != nil {
		sess.Close()
		return
	}
	handler(sess, hello)
}

// acceptHandshake reads Hello, validates the pairing token, and returns the Hello.
func acceptHandshake(s *Session, expected string) (*proto.Hello, error) {
	_ = s.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err := s.Recv()
	if err != nil {
		return nil, err
	}
	_ = s.SetReadDeadline(time.Time{})
	hello, ok := msg.(*proto.Hello)
	if !ok {
		return nil, fmt.Errorf("transport: expected Hello, got %T", msg)
	}
	if hello.ProtoVersion != proto.ProtoVersion {
		return nil, fmt.Errorf("transport: proto version mismatch: %d", hello.ProtoVersion)
	}
	if len(hello.Monitors) == 0 {
		return nil, errors.New("transport: Hello without monitors")
	}
	if hello.PairingToken != expected {
		return nil, ErrBadToken
	}
	if err := s.Send(&proto.Welcome{AssignedName: hello.ClientName}); err != nil {
		return nil, err
	}
	return hello, nil
}

// ClientConfig holds per-instance client settings.
type ClientConfig struct {
	Addr       string
	ServerName string
	ClientName string
	Token      string
	// Monitors announced to the server in Hello. At least one entry required.
	Monitors []proto.Monitor
	// TrustedCert, if non-nil, is the only server certificate accepted
	// (pinned). If nil, any server certificate is accepted — intended only
	// for first-pairing bootstrap; callers should pin afterwards.
	TrustedCert *x509.Certificate
}

// clientTLSConfig builds the TLS config used for both direct-dial and relay-
// tunneled client connections.
func clientTLSConfig(cfg ClientConfig) *tls.Config {
	return &tls.Config{
		ServerName:         cfg.ServerName,
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // verification is done via VerifyPeerCertificate
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if cfg.TrustedCert == nil {
				return nil
			}
			if len(rawCerts) == 0 {
				return errors.New("transport: server sent no certificate")
			}
			for _, raw := range rawCerts {
				if bytesEqual(raw, cfg.TrustedCert.Raw) {
					return nil
				}
			}
			return errors.New("transport: server certificate does not match pinned cert")
		},
	}
}

// Dial opens a TLS connection, performs Hello/Welcome, and returns a ready Session.
func Dial(cfg ClientConfig) (*Session, *proto.Welcome, error) {
	c, err := tls.Dial("tcp", cfg.Addr, clientTLSConfig(cfg))
	if err != nil {
		return nil, nil, err
	}
	return clientHandshake(c, cfg)
}

// clientHandshake runs the Hello/Welcome exchange on an already-TLS-wrapped
// conn. Used by both Dial and the relay-mode dial path.
func clientHandshake(c net.Conn, cfg ClientConfig) (*Session, *proto.Welcome, error) {
	sess := newSession(c)
	if err := sess.Send(&proto.Hello{
		ProtoVersion: proto.ProtoVersion,
		ClientName:   cfg.ClientName,
		Monitors:     cfg.Monitors,
		PairingToken: cfg.Token,
	}); err != nil {
		sess.Close()
		return nil, nil, err
	}
	_ = sess.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err := sess.Recv()
	_ = sess.SetReadDeadline(time.Time{})
	if err != nil {
		sess.Close()
		return nil, nil, fmt.Errorf("await welcome: %w", err)
	}
	welcome, ok := msg.(*proto.Welcome)
	if !ok {
		sess.Close()
		return nil, nil, fmt.Errorf("transport: expected Welcome, got %T", msg)
	}
	return sess, welcome, nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
