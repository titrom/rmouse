//go:build windows

package windows

import (
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

// Hotkey modifier flags matching the RegisterHotKey Win32 constants.
const (
	ModAlt     uint32 = 0x0001
	ModControl uint32 = 0x0002
	ModShift   uint32 = 0x0004
	ModWin     uint32 = 0x0008
	// modNoRepeat suppresses auto-repeat while the key is held.
	modNoRepeat uint32 = 0x4000
)

// Virtual-key codes we're likely to bind. Full list in winuser.h.
const (
	VK_V uint32 = 0x56
)

const (
	wmHotkey = 0x0312
	pmRemove = 0x0001
)

var (
	procRegisterHotKey   = user32.NewProc("RegisterHotKey")
	procUnregisterHotKey = user32.NewProc("UnregisterHotKey")
	procPeekMessageW     = user32.NewProc("PeekMessageW")
)

// msg mirrors the Win32 MSG structure. Only WParam is needed for hotkey
// routing, but we must declare the whole layout so PeekMessageW writes into
// the right memory.
type msg struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
	Private uint32
}

// Hotkey is a single globally-registered hotkey. Fired() returns a channel
// that receives one signal per key press (auto-repeat suppressed). Call
// Close() to unregister and stop the background thread.
type Hotkey struct {
	id      int32
	fired   chan struct{}
	stopped chan struct{}
	stop    chan struct{}
	once    sync.Once
}

// RegisterGlobalHotkey registers a global hotkey with the given modifier
// mask and virtual-key code. Returns an error if the combination is already
// owned by another process. Callers must Close() the returned Hotkey to
// release the registration and tear down the message-pump goroutine.
func RegisterGlobalHotkey(id int32, mods, vk uint32) (*Hotkey, error) {
	hk := &Hotkey{
		id:      id,
		fired:   make(chan struct{}, 1),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	ready := make(chan error, 1)
	go hk.pump(mods, vk, ready)
	if err := <-ready; err != nil {
		close(hk.stopped)
		return nil, err
	}
	return hk, nil
}

// Fired is a signalling channel; receive drains one keypress.
func (h *Hotkey) Fired() <-chan struct{} { return h.fired }

// Close unregisters the hotkey and blocks until the message-pump goroutine
// exits. Safe to call multiple times.
func (h *Hotkey) Close() {
	h.once.Do(func() { close(h.stop) })
	<-h.stopped
}

// pump owns an OS thread for the lifetime of the hotkey registration.
// RegisterHotKey with hwnd=0 routes WM_HOTKEY to the registering thread's
// message queue, which is why we must own the thread here.
func (h *Hotkey) pump(mods, vk uint32, ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(h.stopped)

	r, _, errno := procRegisterHotKey.Call(0, uintptr(h.id), uintptr(mods|modNoRepeat), uintptr(vk))
	if r == 0 {
		ready <- fmt.Errorf("RegisterHotKey(mods=0x%x vk=0x%x): %w", mods, vk, errno)
		return
	}
	defer procUnregisterHotKey.Call(0, uintptr(h.id))
	ready <- nil

	var m msg
	for {
		select {
		case <-h.stop:
			return
		default:
		}
		// PeekMessage is non-blocking; use a short sleep to avoid busy-looping
		// a whole core. 40ms is well below human-perceptible hotkey latency.
		r, _, _ := procPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, pmRemove)
		if r == 0 {
			time.Sleep(40 * time.Millisecond)
			continue
		}
		if m.Message == wmHotkey && int32(m.WParam) == h.id {
			select {
			case h.fired <- struct{}{}:
			default: // coalesce rapid presses — one pending signal is enough.
			}
		}
	}
}
