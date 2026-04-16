//go:build linux

package linux

import (
	"fmt"
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"

	"github.com/titrom/rmouse/internal/proto"
)

// Injector implements platform.Injector on Linux via the X11 XTest extension
// (same backend used by xdotool / x11vnc). Requires an X11 session — Wayland
// is not supported. No special privileges are needed beyond access to the
// X display the user is already logged in to.
type Injector struct {
	mu       sync.Mutex
	conn     *xgb.Conn
	root     xproto.Window
	unsynced int
}

// xgb keeps an internal cookieChan of size 1000; once full it forces its own
// synchronous roundtrip to the X server, which shows up as a periodic mouse
// hiccup when streaming positions at high rate. We pre-empt that by syncing
// every syncEvery unchecked requests. Each Sync is a sub-millisecond local
// X roundtrip, so amortised it spreads what would have been a single ~1ms
// stall across many invisible nudges.
const syncEvery = 8

// NewInjector opens an X connection from $DISPLAY, initialises the XTest
// extension, and remembers the default screen's root window for absolute
// motion. Call Close to release the connection.
func NewInjector() (*Injector, error) {
	conn, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("xgb.NewConn (is $DISPLAY set? running under X11?): %w", err)
	}
	if err := xtest.Init(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("XTest init: %w", err)
	}
	screen := xproto.Setup(conn).DefaultScreen(conn)
	return &Injector{conn: conn, root: screen.Root}, nil
}

// fakeInput is the single funnel for every XTest call. The mutex serialises
// access to the connection (xgb is concurrency-safe but we want predictable
// event ordering) and turns a closed connection into a clear error.
func (i *Injector) fakeInput(typ, detail byte, root xproto.Window, x, y int16) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.conn == nil {
		return fmt.Errorf("xtest: connection closed")
	}
	xtest.FakeInput(i.conn, typ, detail, 0, root, x, y, 0)
	i.unsynced++
	if i.unsynced >= syncEvery {
		i.conn.Sync()
		i.unsynced = 0
	}
	return nil
}

// MouseMoveRel uses XTest motion with detail=1; root=None per spec.
func (i *Injector) MouseMoveRel(dx, dy int32) error {
	return i.fakeInput(xproto.MotionNotify, 1, 0, int16(dx), int16(dy))
}

// MouseMoveAbs uses XTest motion with detail=0 against the default root.
// (x, y) are screen pixel coords in the X11 root window's coordinate space.
func (i *Injector) MouseMoveAbs(x, y int32) error {
	return i.fakeInput(xproto.MotionNotify, 0, i.root, int16(x), int16(y))
}

// MouseButton presses or releases a mouse button. X11 button numbering:
// 1=left, 2=middle, 3=right, 8=X1 (back), 9=X2 (forward). Buttons 4-7 are
// reserved for scroll wheels (see MouseWheel).
func (i *Injector) MouseButton(btn proto.MouseButton, down bool) error {
	var b byte
	switch btn {
	case proto.BtnLeft:
		b = 1
	case proto.BtnMiddle:
		b = 2
	case proto.BtnRight:
		b = 3
	case proto.BtnX1:
		b = 8
	case proto.BtnX2:
		b = 9
	default:
		return fmt.Errorf("unknown button: %d", btn)
	}
	typ := byte(xproto.ButtonPress)
	if !down {
		typ = byte(xproto.ButtonRelease)
	}
	return i.fakeInput(typ, b, 0, 0, 0)
}

// MouseWheel emits one press+release pair per notch on the appropriate scroll
// button: 4=up, 5=down, 6=left, 7=right.
func (i *Injector) MouseWheel(dx, dy int16) error {
	emit := func(button byte, count int16) error {
		for n := int16(0); n < count; n++ {
			if err := i.fakeInput(xproto.ButtonPress, button, 0, 0, 0); err != nil {
				return err
			}
			if err := i.fakeInput(xproto.ButtonRelease, button, 0, 0, 0); err != nil {
				return err
			}
		}
		return nil
	}
	switch {
	case dy > 0:
		if err := emit(5, dy); err != nil {
			return err
		}
	case dy < 0:
		if err := emit(4, -dy); err != nil {
			return err
		}
	}
	switch {
	case dx > 0:
		if err := emit(7, dx); err != nil {
			return err
		}
	case dx < 0:
		if err := emit(6, -dx); err != nil {
			return err
		}
	}
	return nil
}

// KeyEvent maps HID to evdev, then to an X11 keycode. With XKB, X11 keycodes
// are uniformly evdev_code + 8.
func (i *Injector) KeyEvent(hidCode uint16, down bool) error {
	code, ok := hidToEvdev(hidCode)
	if !ok {
		return nil
	}
	keycode := byte(code + 8)
	typ := byte(xproto.KeyPress)
	if !down {
		typ = byte(xproto.KeyRelease)
	}
	return i.fakeInput(typ, keycode, 0, 0, 0)
}

// SetCursorVisible is a no-op on Linux. The host (Windows) hides its own
// cursor while grabbed; on the client there is nothing to hide.
func (*Injector) SetCursorVisible(bool) error { return nil }

// Close releases the X connection. Safe to call multiple times.
func (i *Injector) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.conn != nil {
		i.conn.Close()
		i.conn = nil
	}
	return nil
}

// --- HID → evdev keymap ------------------------------------------------

// hidToEvdev maps USB HID keyboard usage codes (page 7) to Linux evdev
// KEY_* codes. The X11 keycode is then evdev + 8 (XKB convention).
// Unknown HID codes return ok=false.
func hidToEvdev(hid uint16) (code uint16, ok bool) {
	if v, found := hidKeymap[hid]; found {
		return v, true
	}
	return 0, false
}

// evdev codes (linux/input-event-codes.h). Kept local so we don't pull in
// a bigger dependency just for these constants.
const (
	keyEsc        = 1
	keyMinus      = 12
	keyEqual      = 13
	keyBackspace  = 14
	keyTab        = 15
	keyLeftbrace  = 26
	keyRightbrace = 27
	keyEnter      = 28
	keyLeftctrl   = 29
	keySemicolon  = 39
	keyApostrophe = 40
	keyGrave      = 41
	keyLeftshift  = 42
	keyBackslash  = 43
	keyComma      = 51
	keyDot        = 52
	keySlash      = 53
	keyRightshift = 54
	keyLeftalt    = 56
	keySpace      = 57
	keyCapslock   = 58
	keyF1         = 59
	keyF11        = 87
	keyF12        = 88
	keyRightctrl  = 97
	keyRightalt   = 100
	keyHome       = 102
	keyUp         = 103
	keyPageup     = 104
	keyLeft       = 105
	keyRight      = 106
	keyEnd        = 107
	keyDown       = 108
	keyPagedown   = 109
	keyInsert     = 110
	keyDelete     = 111
	keyLeftmeta   = 125
	keyRightmeta  = 126
)

var hidKeymap = func() map[uint16]uint16 {
	m := make(map[uint16]uint16, 128)
	// HID a..z → evdev KEY_* (QWERTY row order, not alphabetical).
	qwerty := map[uint16]uint16{
		0x04: 30, 0x05: 48, 0x06: 46, 0x07: 32, 0x08: 18, 0x09: 33, // a b c d e f
		0x0a: 34, 0x0b: 35, 0x0c: 23, 0x0d: 36, 0x0e: 37, 0x0f: 38, // g h i j k l
		0x10: 50, 0x11: 49, 0x12: 24, 0x13: 25, 0x14: 16, 0x15: 19, // m n o p q r
		0x16: 31, 0x17: 20, 0x18: 22, 0x19: 47, 0x1a: 17, 0x1b: 45, // s t u v w x
		0x1c: 21, 0x1d: 44, // y z
	}
	for k, v := range qwerty {
		m[k] = v
	}
	// 1..9
	for i := uint16(0); i < 9; i++ {
		m[0x1e+i] = 2 + i // KEY_1 = 2
	}
	m[0x27] = 11 // 0 → KEY_0
	// F1..F10
	for i := uint16(0); i < 10; i++ {
		m[0x3a+i] = uint16(keyF1) + i
	}
	m[0x44] = keyF11
	m[0x45] = keyF12
	// Named keys
	m[0x28] = keyEnter
	m[0x29] = keyEsc
	m[0x2a] = keyBackspace
	m[0x2b] = keyTab
	m[0x2c] = keySpace
	m[0x2d] = keyMinus
	m[0x2e] = keyEqual
	m[0x2f] = keyLeftbrace
	m[0x30] = keyRightbrace
	m[0x31] = keyBackslash
	m[0x33] = keySemicolon
	m[0x34] = keyApostrophe
	m[0x35] = keyGrave
	m[0x36] = keyComma
	m[0x37] = keyDot
	m[0x38] = keySlash
	m[0x39] = keyCapslock
	m[0x49] = keyInsert
	m[0x4a] = keyHome
	m[0x4b] = keyPageup
	m[0x4c] = keyDelete
	m[0x4d] = keyEnd
	m[0x4e] = keyPagedown
	m[0x4f] = keyRight
	m[0x50] = keyLeft
	m[0x51] = keyDown
	m[0x52] = keyUp
	// Modifiers
	m[0xe0] = keyLeftctrl
	m[0xe1] = keyLeftshift
	m[0xe2] = keyLeftalt
	m[0xe3] = keyLeftmeta
	m[0xe4] = keyRightctrl
	m[0xe5] = keyRightshift
	m[0xe6] = keyRightalt
	m[0xe7] = keyRightmeta
	return m
}()
