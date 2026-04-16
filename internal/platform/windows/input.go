//go:build windows

package windows

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/titrom/rmouse/internal/proto"
)

// Injector implements platform.Injector on Windows via SendInput.
type Injector struct {
	cursorMu     sync.Mutex
	cursorHidden bool
}

// NewInjector returns a Windows SendInput-based input injector. The Windows
// injector has no resources to release; Close restores the system cursor in
// case it was hidden by a still-active grab.
func NewInjector() (*Injector, error) { return &Injector{}, nil }

var (
	procSendInput              = user32.NewProc("SendInput")
	procSetCursorPos           = user32.NewProc("SetCursorPos")
	procSetSystemCursor        = user32.NewProc("SetSystemCursor")
	procCreateIconIndirect     = user32.NewProc("CreateIconIndirect")
	procDestroyIcon            = user32.NewProc("DestroyIcon")
	procCopyIcon               = user32.NewProc("CopyIcon")
	procSystemParametersInfoW  = user32.NewProc("SystemParametersInfoW")
	procCreateBitmap           = gdi32.NewProc("CreateBitmap")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
)

// Win32 INPUT struct constants.
const (
	inputMouse    uint32 = 0
	inputKeyboard uint32 = 1

	mouseeventfMove       uint32 = 0x0001
	mouseeventfLeftDown   uint32 = 0x0002
	mouseeventfLeftUp     uint32 = 0x0004
	mouseeventfRightDown  uint32 = 0x0008
	mouseeventfRightUp    uint32 = 0x0010
	mouseeventfMiddleDown uint32 = 0x0020
	mouseeventfMiddleUp   uint32 = 0x0040
	mouseeventfXDown      uint32 = 0x0080
	mouseeventfXUp        uint32 = 0x0100
	mouseeventfWheel      uint32 = 0x0800
	mouseeventfHWheel     uint32 = 0x01000

	xbutton1 uint32 = 1
	xbutton2 uint32 = 2

	wheelDelta int32 = 120

	keyeventfKeyUp       uint32 = 0x0002
	keyeventfExtendedKey uint32 = 0x0001
)

// On x64, sizeof(INPUT) == 40. The struct is a tagged union; we keep the
// union as a fixed buffer and cast through unsafe.Pointer when populating.
type inputRecord struct {
	Type uint32
	_    uint32 // padding to 8-byte align the union on x64
	U    [32]byte
}

type mouseInput struct {
	Dx          int32
	Dy          int32
	MouseData   uint32
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

type keyboardInput struct {
	WVk         uint16
	WScan       uint16
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

func sendInput(inputs []inputRecord) error {
	if len(inputs) == 0 {
		return nil
	}
	n, _, errno := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(inputs[0]),
	)
	if int(n) != len(inputs) {
		return fmt.Errorf("SendInput injected %d of %d: %w", n, len(inputs), errno)
	}
	return nil
}

func mouseRecord(flags, data uint32, dx, dy int32) inputRecord {
	var rec inputRecord
	rec.Type = inputMouse
	m := (*mouseInput)(unsafe.Pointer(&rec.U[0]))
	m.Dx = dx
	m.Dy = dy
	m.MouseData = data
	m.DwFlags = flags
	return rec
}

func keyRecord(vk uint16, flags uint32) inputRecord {
	var rec inputRecord
	rec.Type = inputKeyboard
	k := (*keyboardInput)(unsafe.Pointer(&rec.U[0]))
	k.WVk = vk
	k.DwFlags = flags
	return rec
}

// MouseMoveRel moves the cursor by (dx, dy) pixels.
func (*Injector) MouseMoveRel(dx, dy int32) error {
	return sendInput([]inputRecord{mouseRecord(mouseeventfMove, 0, dx, dy)})
}

// MouseMoveAbs uses SetCursorPos for a direct pixel-coord placement. We
// prefer this over SendInput's MOUSEEVENTF_ABSOLUTE because SetCursorPos
// handles multi-monitor virtual desktop natively and skips the 0..65535
// normalization rounding that shifts cursor by a pixel on wide desktops.
func (*Injector) MouseMoveAbs(x, y int32) error {
	r, _, errno := procSetCursorPos.Call(uintptr(x), uintptr(y))
	if r == 0 {
		return fmt.Errorf("SetCursorPos: %w", errno)
	}
	return nil
}

// MouseButton presses or releases a mouse button.
func (*Injector) MouseButton(btn proto.MouseButton, down bool) error {
	var flags, data uint32
	switch btn {
	case proto.BtnLeft:
		if down {
			flags = mouseeventfLeftDown
		} else {
			flags = mouseeventfLeftUp
		}
	case proto.BtnRight:
		if down {
			flags = mouseeventfRightDown
		} else {
			flags = mouseeventfRightUp
		}
	case proto.BtnMiddle:
		if down {
			flags = mouseeventfMiddleDown
		} else {
			flags = mouseeventfMiddleUp
		}
	case proto.BtnX1:
		data = xbutton1
		if down {
			flags = mouseeventfXDown
		} else {
			flags = mouseeventfXUp
		}
	case proto.BtnX2:
		data = xbutton2
		if down {
			flags = mouseeventfXDown
		} else {
			flags = mouseeventfXUp
		}
	default:
		return fmt.Errorf("unknown button: %d", btn)
	}
	return sendInput([]inputRecord{mouseRecord(flags, data, 0, 0)})
}

// MouseWheel scrolls vertically and/or horizontally. A dy of 1 corresponds
// to one notch down (WHEEL_DELTA = 120).
func (i *Injector) MouseWheel(dx, dy int16) error {
	var recs []inputRecord
	if dy != 0 {
		recs = append(recs, mouseRecord(mouseeventfWheel, uint32(int32(dy)*wheelDelta), 0, 0))
	}
	if dx != 0 {
		recs = append(recs, mouseRecord(mouseeventfHWheel, uint32(int32(dx)*wheelDelta), 0, 0))
	}
	return sendInput(recs)
}

// KeyEvent maps hidCode to a Windows virtual key and injects. Unknown HID
// codes are dropped silently.
func (*Injector) KeyEvent(hidCode uint16, down bool) error {
	vk, ext, ok := hidToVK(hidCode)
	if !ok {
		return nil // unknown key — swallow, not fatal
	}
	flags := uint32(0)
	if ext {
		flags |= keyeventfExtendedKey
	}
	if !down {
		flags |= keyeventfKeyUp
	}
	return sendInput([]inputRecord{keyRecord(vk, flags)})
}

// Close restores the system cursor in case a grab was active when the server
// shut down, then releases. Safe to call multiple times.
func (i *Injector) Close() error {
	_ = i.SetCursorVisible(true)
	return nil
}

// Win32 cursor IDs used by SetSystemCursor / SystemParametersInfo.
const (
	ocrNormal      uint32 = 32512
	ocrIBeam       uint32 = 32513
	ocrWait        uint32 = 32514
	ocrCross       uint32 = 32515
	ocrUp          uint32 = 32516
	ocrSizeNWSE    uint32 = 32642
	ocrSizeNESW    uint32 = 32643
	ocrSizeWE      uint32 = 32644
	ocrSizeNS      uint32 = 32645
	ocrSizeAll     uint32 = 32646
	ocrNo          uint32 = 32648
	ocrHand        uint32 = 32649
	ocrAppStarting uint32 = 32650

	spiSetCursors uint32 = 0x0057
)

var systemCursorIDs = []uint32{
	ocrNormal, ocrIBeam, ocrWait, ocrCross, ocrUp,
	ocrSizeNWSE, ocrSizeNESW, ocrSizeWE, ocrSizeNS, ocrSizeAll,
	ocrNo, ocrHand, ocrAppStarting,
}

type iconInfo struct {
	FIcon    int32
	XHotspot uint32
	YHotspot uint32
	HbmMask  uintptr
	HbmColor uintptr
}

// SetCursorVisible toggles the global system cursor by replacing every standard
// cursor with a fully-transparent icon while hidden, and restoring the user's
// cursor scheme via SystemParametersInfo on show. Idempotent.
func (i *Injector) SetCursorVisible(visible bool) error {
	i.cursorMu.Lock()
	defer i.cursorMu.Unlock()
	if visible == !i.cursorHidden {
		return nil
	}
	if visible {
		// Restore the user's full cursor scheme.
		ret, _, errno := procSystemParametersInfoW.Call(uintptr(spiSetCursors), 0, 0, 0)
		if ret == 0 {
			return fmt.Errorf("SystemParametersInfo(SPI_SETCURSORS): %w", errno)
		}
		i.cursorHidden = false
		return nil
	}
	blank, err := createBlankCursor()
	if err != nil {
		return err
	}
	defer procDestroyIcon.Call(blank)
	// SetSystemCursor takes ownership of each handle it receives, so duplicate
	// the blank cursor for every slot we replace.
	for _, id := range systemCursorIDs {
		dup, _, _ := procCopyIcon.Call(blank)
		if dup == 0 {
			continue
		}
		ret, _, _ := procSetSystemCursor.Call(dup, uintptr(id))
		if ret == 0 {
			procDestroyIcon.Call(dup)
		}
	}
	i.cursorHidden = true
	return nil
}

// createBlankCursor builds a 1x1 fully-transparent monochrome cursor. AND mask
// bit set + XOR mask bit clear = transparent pixel; the GDI bitmaps are owned
// by the returned icon and freed by DestroyIcon.
func createBlankCursor() (uintptr, error) {
	andBits := [1]byte{0xFF}
	xorBits := [1]byte{0x00}
	hbmMask, _, errno := procCreateBitmap.Call(1, 1, 1, 1, uintptr(unsafe.Pointer(&andBits[0])))
	if hbmMask == 0 {
		return 0, fmt.Errorf("CreateBitmap(mask): %w", errno)
	}
	hbmColor, _, errno := procCreateBitmap.Call(1, 1, 1, 1, uintptr(unsafe.Pointer(&xorBits[0])))
	if hbmColor == 0 {
		procDeleteObject.Call(hbmMask)
		return 0, fmt.Errorf("CreateBitmap(color): %w", errno)
	}
	info := iconInfo{
		FIcon:    0, // cursor
		HbmMask:  hbmMask,
		HbmColor: hbmColor,
	}
	icon, _, errno := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&info)))
	procDeleteObject.Call(hbmMask)
	procDeleteObject.Call(hbmColor)
	if icon == 0 {
		return 0, fmt.Errorf("CreateIconIndirect: %w", errno)
	}
	return icon, nil
}

// hidToVK maps a USB HID keyboard usage (page 7) to a Windows virtual-key
// code. The second return signals whether the key needs the Extended flag
// set in KEYEVENTF_EXTENDEDKEY (arrow keys, right-side modifiers, etc.).
// Covers ASCII + common navigation, modifiers, and F1-F12. Unknown codes
// return ok=false and are dropped by the caller.
func hidToVK(hid uint16) (vk uint16, ext bool, ok bool) {
	switch {
	case hid >= 0x04 && hid <= 0x1D: // a..z
		return uint16('A') + (hid - 0x04), false, true
	case hid >= 0x1E && hid <= 0x26: // 1..9
		return uint16('1') + (hid - 0x1E), false, true
	case hid == 0x27: // 0
		return uint16('0'), false, true
	case hid >= 0x3A && hid <= 0x45: // F1..F12
		return 0x70 + (hid - 0x3A), false, true
	}
	switch hid {
	case 0x28:
		return 0x0D, false, true // Enter
	case 0x29:
		return 0x1B, false, true // Esc
	case 0x2A:
		return 0x08, false, true // Backspace
	case 0x2B:
		return 0x09, false, true // Tab
	case 0x2C:
		return 0x20, false, true // Space
	case 0x2D:
		return 0xBD, false, true // -
	case 0x2E:
		return 0xBB, false, true // =
	case 0x2F:
		return 0xDB, false, true // [
	case 0x30:
		return 0xDD, false, true // ]
	case 0x31:
		return 0xDC, false, true // \
	case 0x33:
		return 0xBA, false, true // ;
	case 0x34:
		return 0xDE, false, true // '
	case 0x35:
		return 0xC0, false, true // `
	case 0x36:
		return 0xBC, false, true // ,
	case 0x37:
		return 0xBE, false, true // .
	case 0x38:
		return 0xBF, false, true // /
	case 0x39:
		return 0x14, false, true // CapsLock
	case 0x49:
		return 0x2D, true, true // Insert
	case 0x4A:
		return 0x24, true, true // Home
	case 0x4B:
		return 0x21, true, true // PageUp
	case 0x4C:
		return 0x2E, true, true // Delete
	case 0x4D:
		return 0x23, true, true // End
	case 0x4E:
		return 0x22, true, true // PageDown
	case 0x4F:
		return 0x27, true, true // Right
	case 0x50:
		return 0x25, true, true // Left
	case 0x51:
		return 0x28, true, true // Down
	case 0x52:
		return 0x26, true, true // Up
	case 0xE0:
		return 0xA2, false, true // LCtrl
	case 0xE1:
		return 0xA0, false, true // LShift
	case 0xE2:
		return 0xA4, false, true // LAlt
	case 0xE3:
		return 0x5B, true, true // LGui (LWIN)
	case 0xE4:
		return 0xA3, true, true // RCtrl
	case 0xE5:
		return 0xA1, false, true // RShift
	case 0xE6:
		return 0xA5, true, true // RAlt
	case 0xE7:
		return 0x5C, true, true // RGui (RWIN)
	}
	return 0, false, false
}
