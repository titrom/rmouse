package transport

import (
	"errors"
	"fmt"
	"io"
)

// RelayMagic identifies an rmouse relay rendezvous preamble.
var RelayMagic = [4]byte{'R', 'M', 'R', '0'}

// Side is the role a connection claims in the relay rendezvous.
type Side byte

const (
	SideServer Side = 0x01
	SideClient Side = 0x02
)

// MaxSessionIDLen bounds the length-prefixed session id in the preamble.
const MaxSessionIDLen = 64

// WritePreamble sends the rendezvous header that the relay uses to pair two
// connections. Format: 4B magic | 1B role | 1B len | N bytes session id.
func WritePreamble(w io.Writer, role Side, sessionID string) error {
	if role != SideServer && role != SideClient {
		return fmt.Errorf("transport: invalid relay role: %d", role)
	}
	if len(sessionID) == 0 || len(sessionID) > MaxSessionIDLen {
		return fmt.Errorf("transport: session id length out of range: %d", len(sessionID))
	}
	var hdr [6]byte
	copy(hdr[0:4], RelayMagic[:])
	hdr[4] = byte(role)
	hdr[5] = byte(len(sessionID))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := io.WriteString(w, sessionID)
	return err
}

// ReadPreamble parses the rendezvous header. Returns the claimed role and
// session id; does not validate either against any expected value.
func ReadPreamble(r io.Reader) (Side, string, error) {
	var hdr [6]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, "", err
	}
	if !bytesEqual(hdr[0:4], RelayMagic[:]) {
		return 0, "", errors.New("transport: bad relay magic")
	}
	role := Side(hdr[4])
	if role != SideServer && role != SideClient {
		return 0, "", fmt.Errorf("transport: bad relay role: %d", role)
	}
	n := int(hdr[5])
	if n == 0 || n > MaxSessionIDLen {
		return 0, "", fmt.Errorf("transport: bad session id length: %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, "", err
	}
	return role, string(buf), nil
}

