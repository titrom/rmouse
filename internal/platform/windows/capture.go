//go:build windows

package windows

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/titrom/rmouse/internal/platform/inputevent"
	"github.com/titrom/rmouse/internal/proto"
)

// Capturer implements platform.Capturer on Windows via WH_MOUSE_LL and
// WH_KEYBOARD_LL low-level hooks. Each Capture call spins up a dedicated
// OS-thread-locked goroutine that owns the hooks and pumps messages.
type Capturer struct{}

// NewCapturer returns a Windows hook-based input capturer.
func NewCapturer() *Capturer { return &Capturer{} }

var (
	procSetWindowsHookExW   = user32.NewProc("SetWindowsHookExW")
	procUnhookWindowsHookEx = user32.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx      = user32.NewProc("CallNextHookEx")
	procClipCursor          = user32.NewProc("ClipCursor")
	procGetModuleHandleW    = kernel32.NewProc("GetModuleHandleW")
)

// LLMHF_INJECTED flag: set in MSLLHOOKSTRUCT.Flags for events we synthesized
// ourselves (SetCursorPos, SendInput). We ignore those so the router's own
// cursor pokes don't feed back into capture.
const llmhfInjected = 0x00000001

const (
	whMouseLL    = 14
	whKeyboardLL = 13

	wmMouseMove    = 0x0200
	wmLButtonDown  = 0x0201
	wmLButtonUp    = 0x0202
	wmRButtonDown  = 0x0204
	wmRButtonUp    = 0x0205
	wmMButtonDown  = 0x0207
	wmMButtonUp    = 0x0208
	wmMouseWheel   = 0x020A
	wmXButtonDown  = 0x020B
	wmXButtonUp    = 0x020C
	wmMouseHWheel  = 0x020E

	wmKeyDown    = 0x0100
	wmKeyUp      = 0x0101
	wmSysKeyDown = 0x0104
	wmSysKeyUp   = 0x0105
)

type point struct {
	X, Y int32
}

type msllHookStruct struct {
	Pt          point
	MouseData   uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type kbdllHookStruct struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type winMsg struct {
	Hwnd     uintptr
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       point
	LPrivate uint32
}

// captureSession holds per-Capture state shared between the hook callbacks
// and the controller. Consume is atomic because the callback reads it on a
// non-Go-scheduled thread; events is buffered so the hook thread never blocks.
type captureSession struct {
	consume atomic.Bool
	events  chan inputevent.Event
}

func (s *captureSession) SetConsume(on bool) { s.consume.Store(on) }

type winRect struct {
	Left, Top, Right, Bottom int32
}

// ClipToPoint locks the OS cursor to a single pixel so Windows stops
// clamping motion against a screen edge and we keep receiving deltas.
func (*captureSession) ClipToPoint(x, y int32) error {
	r := winRect{Left: x, Top: y, Right: x + 1, Bottom: y + 1}
	ret, _, errno := procClipCursor.Call(uintptr(unsafe.Pointer(&r)))
	if ret == 0 {
		return fmt.Errorf("ClipCursor: %w", errno)
	}
	return nil
}

// ReleaseClip restores free cursor movement.
func (*captureSession) ReleaseClip() error {
	ret, _, errno := procClipCursor.Call(0)
	if ret == 0 {
		return fmt.Errorf("ClipCursor release: %w", errno)
	}
	return nil
}

// Capture installs the global hooks and returns a channel + controller. The
// hooks are released when ctx is cancelled.
func (*Capturer) Capture(ctx context.Context) (<-chan inputevent.Event, inputevent.Ctl, error) {
	sess := &captureSession{events: make(chan inputevent.Event, 256)}

	type startResult struct {
		threadID uint32
		err      error
	}
	started := make(chan startResult, 1)

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		hmod, _, _ := procGetModuleHandleW.Call(0)

		mouseProc := syscall.NewCallback(func(code int32, wParam uintptr, lParam uintptr) uintptr {
			return sess.onMouseHook(code, wParam, lParam)
		})
		keyProc := syscall.NewCallback(func(code int32, wParam uintptr, lParam uintptr) uintptr {
			return sess.onKeyHook(code, wParam, lParam)
		})

		mouseHook, _, errno := procSetWindowsHookExW.Call(uintptr(whMouseLL), mouseProc, hmod, 0)
		if mouseHook == 0 {
			started <- startResult{err: fmt.Errorf("SetWindowsHookEx(mouse): %w", errno)}
			close(sess.events)
			return
		}
		keyHook, _, errno := procSetWindowsHookExW.Call(uintptr(whKeyboardLL), keyProc, hmod, 0)
		if keyHook == 0 {
			procUnhookWindowsHookEx.Call(mouseHook)
			started <- startResult{err: fmt.Errorf("SetWindowsHookEx(keyboard): %w", errno)}
			close(sess.events)
			return
		}

		tid, _, _ := procGetCurrentThreadId.Call()
		started <- startResult{threadID: uint32(tid)}

		// Message pump — required for hook callbacks to fire.
		var msg winMsg
		for {
			ret, _, _ := procGetMessageW.Call(
				uintptr(unsafe.Pointer(&msg)),
				0, 0, 0,
			)
			if ret == 0 || int32(ret) == -1 {
				break // WM_QUIT or error — either way, stop
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
		}

		procUnhookWindowsHookEx.Call(keyHook)
		procUnhookWindowsHookEx.Call(mouseHook)
		close(sess.events)
	}()

	res := <-started
	if res.err != nil {
		return nil, nil, res.err
	}

	// Stopper goroutine — wakes the message pump on ctx cancel.
	go func() {
		<-ctx.Done()
		procPostThreadMessageW.Call(uintptr(res.threadID), wmQuit, 0, 0)
	}()

	return sess.events, sess, nil
}

// onMouseHook is invoked by the OS on the hook thread for every global mouse
// event. We translate to a inputevent.Event, post it, and optionally
// suppress the event by returning 1 instead of calling the next hook.
func (s *captureSession) onMouseHook(code int32, wParam uintptr, lParam uintptr) uintptr {
	if code < 0 {
		r, _, _ := procCallNextHookEx.Call(0, uintptr(code), wParam, lParam)
		return r
	}
	data := (*msllHookStruct)(unsafe.Pointer(lParam))
	if data.Flags&llmhfInjected != 0 {
		// Event originated from SendInput / SetCursorPos — don't feed our
		// own signals back into the router.
		r, _, _ := procCallNextHookEx.Call(0, uintptr(code), wParam, lParam)
		return r
	}

	switch uint32(wParam) {
	case wmMouseMove:
		s.emit(inputevent.Event{
			Kind: inputevent.MouseMove,
			AbsX: data.Pt.X,
			AbsY: data.Pt.Y,
		})
	case wmLButtonDown:
		s.emit(inputevent.Event{Kind: inputevent.MouseButton, Button: proto.BtnLeft, Down: true})
	case wmLButtonUp:
		s.emit(inputevent.Event{Kind: inputevent.MouseButton, Button: proto.BtnLeft, Down: false})
	case wmRButtonDown:
		s.emit(inputevent.Event{Kind: inputevent.MouseButton, Button: proto.BtnRight, Down: true})
	case wmRButtonUp:
		s.emit(inputevent.Event{Kind: inputevent.MouseButton, Button: proto.BtnRight, Down: false})
	case wmMButtonDown:
		s.emit(inputevent.Event{Kind: inputevent.MouseButton, Button: proto.BtnMiddle, Down: true})
	case wmMButtonUp:
		s.emit(inputevent.Event{Kind: inputevent.MouseButton, Button: proto.BtnMiddle, Down: false})
	case wmXButtonDown, wmXButtonUp:
		btn := proto.BtnX1
		if hiWord(data.MouseData) == 2 {
			btn = proto.BtnX2
		}
		s.emit(inputevent.Event{
			Kind:   inputevent.MouseButton,
			Button: btn,
			Down:   uint32(wParam) == wmXButtonDown,
		})
	case wmMouseWheel:
		delta := int16(hiWord(data.MouseData))
		s.emit(inputevent.Event{Kind: inputevent.MouseWheel, WheelDY: delta / 120})
	case wmMouseHWheel:
		delta := int16(hiWord(data.MouseData))
		s.emit(inputevent.Event{Kind: inputevent.MouseWheel, WheelDX: delta / 120})
	}

	if s.consume.Load() {
		return 1 // swallow; Windows stops propagating to other hooks/apps
	}
	r, _, _ := procCallNextHookEx.Call(0, uintptr(code), wParam, lParam)
	return r
}

func (s *captureSession) onKeyHook(code int32, wParam uintptr, lParam uintptr) uintptr {
	if code < 0 {
		r, _, _ := procCallNextHookEx.Call(0, uintptr(code), wParam, lParam)
		return r
	}
	data := (*kbdllHookStruct)(unsafe.Pointer(lParam))
	down := uint32(wParam) == wmKeyDown || uint32(wParam) == wmSysKeyDown

	if hid, ok := vkToHID(uint16(data.VkCode)); ok {
		s.emit(inputevent.Event{Kind: inputevent.KeyEvent, KeyCode: hid, Down: down})
	}

	if s.consume.Load() {
		return 1
	}
	r, _, _ := procCallNextHookEx.Call(0, uintptr(code), wParam, lParam)
	return r
}

// emit is non-blocking: if the consumer is slow and the buffer is full we
// drop the event rather than stall the OS hook thread (which would hang the
// user's physical mouse).
func (s *captureSession) emit(ev inputevent.Event) {
	select {
	case s.events <- ev:
	default:
	}
}

func hiWord(v uint32) uint32 { return (v >> 16) & 0xffff }

// vkToHID inverts hidToVK for the subset of keys the injector handles.
func vkToHID(vk uint16) (uint16, bool) {
	if h, ok := vkReverseMap[vk]; ok {
		return h, true
	}
	return 0, false
}

var vkReverseMap = func() map[uint16]uint16 {
	// Walk the HID range we care about and call the forward map.
	m := make(map[uint16]uint16, 128)
	for hid := uint16(0); hid < 0x100; hid++ {
		if vk, _, ok := hidToVK(hid); ok {
			// If two HIDs map to the same VK (e.g. modifiers), prefer the
			// lower HID — rarely matters in practice.
			if _, dup := m[vk]; !dup {
				m[vk] = hid
			}
		}
	}
	return m
}()
