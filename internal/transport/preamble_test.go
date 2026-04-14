package transport

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestPreambleRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		role Side
		sid  string
	}{
		{"server short", SideServer, "abc"},
		{"client long", SideClient, strings.Repeat("A", MaxSessionIDLen)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WritePreamble(&buf, tc.role, tc.sid); err != nil {
				t.Fatal(err)
			}
			role, sid, err := ReadPreamble(&buf)
			if err != nil {
				t.Fatal(err)
			}
			if role != tc.role {
				t.Errorf("role: got %d want %d", role, tc.role)
			}
			if sid != tc.sid {
				t.Errorf("sid: got %q want %q", sid, tc.sid)
			}
		})
	}
}

func TestPreambleBadMagic(t *testing.T) {
	buf := bytes.NewReader([]byte{0, 0, 0, 0, 0x01, 1, 'x'})
	if _, _, err := ReadPreamble(buf); err == nil {
		t.Fatal("expected magic error")
	}
}

func TestPreambleBadRole(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(RelayMagic[:])
	buf.WriteByte(0x99) // invalid role
	buf.WriteByte(1)
	buf.WriteByte('x')
	if _, _, err := ReadPreamble(&buf); err == nil {
		t.Fatal("expected role error")
	}
}

func TestWritePreambleRejectsEmpty(t *testing.T) {
	if err := WritePreamble(io.Discard, SideServer, ""); err == nil {
		t.Fatal("expected error for empty session id")
	}
	if err := WritePreamble(io.Discard, SideServer, strings.Repeat("A", MaxSessionIDLen+1)); err == nil {
		t.Fatal("expected error for oversize session id")
	}
}
