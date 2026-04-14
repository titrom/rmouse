//go:build linux

package linux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/titrom/rmouse/internal/proto"
)

// Injector implements platform.Injector on Linux via /dev/uinput. It creates
// a single virtual device that reports relative motion, absolute motion,
// mouse buttons, wheels, and keyboard keys.
//
// Permissions: /dev/uinput must be writable. Typically this means the user
// is in the "input" group and a udev rule grants 0660 mode on /dev/uinput.
type Injector struct {
	mu sync.Mutex
	f  *os.File // /dev/uinput
}

// Absolute range for the virtual pointer. Callers pass pixel coordinates in
// their own virtual-desktop space; we map that 1:1 into the 0..absRange axis
// here. absRange is picked large enough to cover realistic multi-monitor
// desktops without loss of precision.
const absRange = 32767

// NewInjector opens /dev/uinput, configures a virtual input device, and
// registers it with the kernel. Call Close to tear it down.
func NewInjector() (*Injector, error) {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/uinput (is user in 'input' group?): %w", err)
	}
	if err := configureDevice(f.Fd()); err != nil {
		f.Close()
		return nil, err
	}
	return &Injector{f: f}, nil
}

func configureDevice(fd uintptr) error {
	enables := []struct {
		req  uintptr
		code int
	}{
		{uiSetEVBit, int(evKey)},
		{uiSetEVBit, int(evRel)},
		{uiSetEVBit, int(evAbs)},
		{uiSetEVBit, int(evSyn)},

		{uiSetRelBit, int(relX)},
		{uiSetRelBit, int(relY)},
		{uiSetRelBit, int(relWheel)},
		{uiSetRelBit, int(relHWheel)},

		{uiSetAbsBit, int(absX)},
		{uiSetAbsBit, int(absY)},

		{uiSetKeyBit, int(btnLeft)},
		{uiSetKeyBit, int(btnRight)},
		{uiSetKeyBit, int(btnMiddle)},
		{uiSetKeyBit, int(btnSide)},
		{uiSetKeyBit, int(btnExtra)},

		{uiSetPropBit, inputPropPointer},
	}
	// Register every keyboard key we know how to map.
	for _, code := range knownEvdevKeys() {
		enables = append(enables, struct {
			req  uintptr
			code int
		}{uiSetKeyBit, code})
	}
	for _, e := range enables {
		if err := ioctlSetInt(fd, e.req, e.code); err != nil {
			return fmt.Errorf("uinput SET bit 0x%x code %d: %w", e.req, e.code, err)
		}
	}

	// UI_DEV_SETUP: name + id + abs-axis range for ABS_X / ABS_Y.
	var setup uinputSetup
	setup.ID.BusType = 0x03 // BUS_USB
	setup.ID.Vendor = 0x1d6b
	setup.ID.Product = 0x0104
	setup.ID.Version = 1
	copy(setup.Name[:], []byte("rmouse virtual input"))
	if err := ioctlPtr(fd, uiDevSetup, unsafe.Pointer(&setup)); err != nil {
		return fmt.Errorf("UI_DEV_SETUP: %w", err)
	}

	// Configure ABS_X/ABS_Y ranges via UI_ABS_SETUP.
	for _, axis := range []uint16{absX, absY} {
		var a uinputAbsSetup
		a.Code = axis
		a.AbsInfo.Maximum = absRange
		if err := ioctlPtr(fd, uiAbsSetup, unsafe.Pointer(&a)); err != nil {
			return fmt.Errorf("UI_ABS_SETUP axis %d: %w", axis, err)
		}
	}

	if err := ioctlNone(fd, uiDevCreate); err != nil {
		return fmt.Errorf("UI_DEV_CREATE: %w", err)
	}
	return nil
}

// write sends one or more input_event records followed by a SYN_REPORT.
func (i *Injector) write(events ...inputEvent) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.f == nil {
		return errors.New("uinput: device closed")
	}
	buf := make([]byte, 0, 24*(len(events)+1))
	for _, ev := range events {
		buf = encodeEvent(buf, ev)
	}
	buf = encodeEvent(buf, inputEvent{Type: evSyn, Code: synReport, Value: 0})
	if _, err := i.f.Write(buf); err != nil {
		return fmt.Errorf("uinput write: %w", err)
	}
	return nil
}

// MouseMoveRel dispatches REL_X / REL_Y events.
func (i *Injector) MouseMoveRel(dx, dy int32) error {
	var events []inputEvent
	if dx != 0 {
		events = append(events, inputEvent{Type: evRel, Code: relX, Value: dx})
	}
	if dy != 0 {
		events = append(events, inputEvent{Type: evRel, Code: relY, Value: dy})
	}
	if len(events) == 0 {
		return nil
	}
	return i.write(events...)
}

// MouseMoveAbs dispatches ABS_X / ABS_Y events. (x, y) are pixel coordinates
// in the local virtual desktop; they are clamped to [0, absRange].
func (i *Injector) MouseMoveAbs(x, y int32) error {
	return i.write(
		inputEvent{Type: evAbs, Code: absX, Value: clampAbs(x)},
		inputEvent{Type: evAbs, Code: absY, Value: clampAbs(y)},
	)
}

// MouseButton presses or releases a mouse button.
func (i *Injector) MouseButton(btn proto.MouseButton, down bool) error {
	var code uint16
	switch btn {
	case proto.BtnLeft:
		code = btnLeft
	case proto.BtnRight:
		code = btnRight
	case proto.BtnMiddle:
		code = btnMiddle
	case proto.BtnX1:
		code = btnSide
	case proto.BtnX2:
		code = btnExtra
	default:
		return fmt.Errorf("unknown button: %d", btn)
	}
	val := int32(0)
	if down {
		val = 1
	}
	return i.write(inputEvent{Type: evKey, Code: code, Value: val})
}

// MouseWheel scrolls by (dx, dy) notches.
func (i *Injector) MouseWheel(dx, dy int16) error {
	var events []inputEvent
	if dy != 0 {
		events = append(events, inputEvent{Type: evRel, Code: relWheel, Value: int32(dy)})
	}
	if dx != 0 {
		events = append(events, inputEvent{Type: evRel, Code: relHWheel, Value: int32(dx)})
	}
	if len(events) == 0 {
		return nil
	}
	return i.write(events...)
}

// KeyEvent maps HID to evdev and dispatches an EV_KEY event. Unknown codes
// are dropped silently.
func (i *Injector) KeyEvent(hidCode uint16, down bool) error {
	code, ok := hidToEvdev(hidCode)
	if !ok {
		return nil
	}
	val := int32(0)
	if down {
		val = 1
	}
	return i.write(inputEvent{Type: evKey, Code: code, Value: val})
}

// Close destroys the virtual device and releases the uinput fd. Safe to
// call multiple times.
func (i *Injector) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.f == nil {
		return nil
	}
	_ = ioctlNone(i.f.Fd(), uiDevDestroy)
	err := i.f.Close()
	i.f = nil
	return err
}

// --- Linux kernel types and ioctl plumbing ------------------------------

// ioctl encoding — see Documentation/userspace-api/ioctl/ioctl-decoding.rst.
const (
	iocNrbits    = 8
	iocTypebits  = 8
	iocNrshift   = 0
	iocTypeshift = iocNrshift + iocNrbits
	iocSizeshift = iocTypeshift + iocTypebits
	iocDirshift  = iocSizeshift + 14

	iocNone  uintptr = 0
	iocWrite uintptr = 1
)

func ioc(dir, typ, nr, size uintptr) uintptr {
	return dir<<iocDirshift | typ<<iocTypeshift | nr<<iocNrshift | size<<iocSizeshift
}
func iow(typ, nr, size uintptr) uintptr { return ioc(iocWrite, typ, nr, size) }
func io_(typ, nr uintptr) uintptr       { return ioc(iocNone, typ, nr, 0) }

const uinputIoctlBase uintptr = 'U'

var (
	uiDevCreate  = io_(uinputIoctlBase, 1)
	uiDevDestroy = io_(uinputIoctlBase, 2)
	uiDevSetup   = iow(uinputIoctlBase, 3, unsafe.Sizeof(uinputSetup{}))
	uiAbsSetup   = iow(uinputIoctlBase, 4, unsafe.Sizeof(uinputAbsSetup{}))
	uiSetEVBit   = iow(uinputIoctlBase, 100, 4)
	uiSetKeyBit  = iow(uinputIoctlBase, 101, 4)
	uiSetRelBit  = iow(uinputIoctlBase, 102, 4)
	uiSetAbsBit  = iow(uinputIoctlBase, 103, 4)
	uiSetPropBit = iow(uinputIoctlBase, 110, 4)
)

const (
	evSyn uint16 = 0x00
	evKey uint16 = 0x01
	evRel uint16 = 0x02
	evAbs uint16 = 0x03

	synReport uint16 = 0

	relX      uint16 = 0x00
	relY      uint16 = 0x01
	relHWheel uint16 = 0x06
	relWheel  uint16 = 0x08

	absX uint16 = 0x00
	absY uint16 = 0x01

	btnLeft   uint16 = 0x110
	btnRight  uint16 = 0x111
	btnMiddle uint16 = 0x112
	btnSide   uint16 = 0x113
	btnExtra  uint16 = 0x114

	inputPropPointer = 0x00
)

type inputID struct {
	BusType uint16
	Vendor  uint16
	Product uint16
	Version uint16
}

type uinputSetup struct {
	ID           inputID
	Name         [80]byte
	FFEffectsMax uint32
}

type absInfo struct {
	Value      int32
	Minimum    int32
	Maximum    int32
	Fuzz       int32
	Flat       int32
	Resolution int32
}

type uinputAbsSetup struct {
	Code    uint16
	_       [2]byte
	AbsInfo absInfo
}

type inputEvent struct {
	Sec, Usec int64 // timeval; zero means "kernel fills in"
	Type      uint16
	Code      uint16
	Value     int32
}

// encodeEvent writes a 24-byte input_event record (x86_64 layout).
func encodeEvent(buf []byte, ev inputEvent) []byte {
	var tmp [24]byte
	binary.LittleEndian.PutUint64(tmp[0:], uint64(ev.Sec))
	binary.LittleEndian.PutUint64(tmp[8:], uint64(ev.Usec))
	binary.LittleEndian.PutUint16(tmp[16:], ev.Type)
	binary.LittleEndian.PutUint16(tmp[18:], ev.Code)
	binary.LittleEndian.PutUint32(tmp[20:], uint32(ev.Value))
	return append(buf, tmp[:]...)
}

func ioctlSetInt(fd, req uintptr, val int) error {
	v := int32(val)
	_, _, e := syscall.Syscall(unix.SYS_IOCTL, fd, req, uintptr(unsafe.Pointer(&v)))
	if e != 0 {
		return e
	}
	return nil
}
func ioctlPtr(fd, req uintptr, p unsafe.Pointer) error {
	_, _, e := syscall.Syscall(unix.SYS_IOCTL, fd, req, uintptr(p))
	if e != 0 {
		return e
	}
	return nil
}
func ioctlNone(fd, req uintptr) error {
	_, _, e := syscall.Syscall(unix.SYS_IOCTL, fd, req, 0)
	if e != 0 {
		return e
	}
	return nil
}

func clampAbs(v int32) int32 {
	if v < 0 {
		return 0
	}
	if v > absRange {
		return absRange
	}
	return v
}

// --- HID → evdev keymap ------------------------------------------------

// hidToEvdev maps USB HID keyboard usage codes (page 7) to Linux evdev
// KEY_* codes. Unknown codes return ok=false.
func hidToEvdev(hid uint16) (code uint16, ok bool) {
	if v, found := hidKeymap[hid]; found {
		return v, true
	}
	return 0, false
}

// knownEvdevKeys returns every evdev code in hidKeymap, for UI_SET_KEYBIT.
func knownEvdevKeys() []int {
	out := make([]int, 0, len(hidKeymap))
	for _, v := range hidKeymap {
		out = append(out, int(v))
	}
	return out
}

// evdev codes (linux/input-event-codes.h). Kept local so we don't pull in
// a bigger dependency just for these constants.
const (
	keyEsc       = 1
	keyMinus     = 12
	keyEqual     = 13
	keyBackspace = 14
	keyTab       = 15
	keyLeftbrace = 26
	keyRightbrace = 27
	keyEnter     = 28
	keyLeftctrl  = 29
	keySemicolon = 39
	keyApostrophe = 40
	keyGrave     = 41
	keyLeftshift = 42
	keyBackslash = 43
	keyComma     = 51
	keyDot       = 52
	keySlash     = 53
	keyRightshift = 54
	keyLeftalt   = 56
	keySpace     = 57
	keyCapslock  = 58
	keyF1        = 59
	keyF11       = 87
	keyF12       = 88
	keyRightctrl = 97
	keyRightalt  = 100
	keyHome      = 102
	keyUp        = 103
	keyPageup    = 104
	keyLeft      = 105
	keyRight     = 106
	keyEnd       = 107
	keyDown      = 108
	keyPagedown  = 109
	keyInsert    = 110
	keyDelete    = 111
	keyLeftmeta  = 125
	keyRightmeta = 126
)

var hidKeymap = func() map[uint16]uint16 {
	m := make(map[uint16]uint16, 128)
	// a..z → KEY_A (30) + offsets derived from QWERTY layout
	// HID a..z is sequential but evdev row order is different.
	qwerty := map[uint16]uint16{
		// HID → evdev
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
