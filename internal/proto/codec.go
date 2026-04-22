package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxFrameSize caps a single decoded frame to guard against malformed input.
// Clipboard sync may carry PNG payloads, so this limit is intentionally larger
// than input-only frames.
const MaxFrameSize = 16 << 20 // 16 MiB

var ErrUnknownType = errors.New("proto: unknown message type")

// Write frames and sends a message.
// Frame layout: [4B LE length][1B type][payload].
func Write(w io.Writer, m Message) error {
	e := newEncoder()
	e.u8(uint8(m.Type()))
	m.encode(e)
	payload := e.buf
	if len(payload) > MaxFrameSize {
		return fmt.Errorf("proto: frame too large: %d", len(payload))
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// Read decodes a single framed message from r.
func Read(r io.Reader) (Message, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	length := binary.LittleEndian.Uint32(hdr[:])
	if length == 0 {
		return nil, errors.New("proto: empty frame")
	}
	if length > MaxFrameSize {
		return nil, fmt.Errorf("proto: frame too large: %d", length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	t := MsgType(buf[0])
	msg := newByType(t)
	if msg == nil {
		return nil, fmt.Errorf("%w: %d", ErrUnknownType, t)
	}
	d := &decoder{buf: buf[1:]}
	if err := msg.decode(d); err != nil {
		return nil, err
	}
	if d.pos != len(d.buf) {
		return nil, fmt.Errorf("proto: %d trailing bytes for type %d", len(d.buf)-d.pos, t)
	}
	return msg, nil
}

// encoder / decoder are tiny helpers for building message payloads.

type encoder struct {
	buf []byte
}

func newEncoder() *encoder { return &encoder{buf: make([]byte, 0, 64)} }

func (e *encoder) u8(v uint8) { e.buf = append(e.buf, v) }
func (e *encoder) bool(v bool) {
	if v {
		e.buf = append(e.buf, 1)
	} else {
		e.buf = append(e.buf, 0)
	}
}

func (e *encoder) u16(v uint16) {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	e.buf = append(e.buf, b[:]...)
}
func (e *encoder) i16(v int16) { e.u16(uint16(v)) }

func (e *encoder) u32(v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	e.buf = append(e.buf, b[:]...)
}
func (e *encoder) i32(v int32) { e.u32(uint32(v)) }
func (e *encoder) u64(v uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	e.buf = append(e.buf, b[:]...)
}
func (e *encoder) bytes(v []byte) {
	e.u32(uint32(len(v)))
	e.buf = append(e.buf, v...)
}

func (e *encoder) str(s string) {
	if len(s) > 0xFFFF {
		// strings are bounded by u16 length; callers must enforce upstream.
		s = s[:0xFFFF]
	}
	e.u16(uint16(len(s)))
	e.buf = append(e.buf, s...)
}

type decoder struct {
	buf []byte
	pos int
	err error
}

func (d *decoder) need(n int) bool {
	if d.err != nil {
		return false
	}
	if d.pos+n > len(d.buf) {
		d.err = io.ErrUnexpectedEOF
		return false
	}
	return true
}

func (d *decoder) u8() uint8 {
	if !d.need(1) {
		return 0
	}
	v := d.buf[d.pos]
	d.pos++
	return v
}

func (d *decoder) bool() bool { return d.u8() != 0 }

func (d *decoder) u16() uint16 {
	if !d.need(2) {
		return 0
	}
	v := binary.LittleEndian.Uint16(d.buf[d.pos:])
	d.pos += 2
	return v
}
func (d *decoder) i16() int16 { return int16(d.u16()) }

func (d *decoder) u32() uint32 {
	if !d.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(d.buf[d.pos:])
	d.pos += 4
	return v
}
func (d *decoder) i32() int32 { return int32(d.u32()) }
func (d *decoder) u64() uint64 {
	if !d.need(8) {
		return 0
	}
	v := binary.LittleEndian.Uint64(d.buf[d.pos:])
	d.pos += 8
	return v
}
func (d *decoder) bytes() []byte {
	n := int(d.u32())
	if n > MaxFrameSize {
		d.err = fmt.Errorf("proto: byte slice too large: %d", n)
		return nil
	}
	if !d.need(n) {
		return nil
	}
	out := append([]byte(nil), d.buf[d.pos:d.pos+n]...)
	d.pos += n
	return out
}

func (d *decoder) str() string {
	n := int(d.u16())
	if !d.need(n) {
		return ""
	}
	s := string(d.buf[d.pos : d.pos+n])
	d.pos += n
	return s
}
