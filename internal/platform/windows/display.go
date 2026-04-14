//go:build windows

// Package windows implements the platform.Display interface on Windows using
// EnumDisplayMonitors + GetMonitorInfoW.
package windows

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/titrom/rmouse/internal/proto"
)

// Display is a Windows-backed platform.Display implementation. Returned by
// New(); intended to be used via the platform.Display interface.
type Display struct{}

// New returns a Windows display enumerator.
func New() *Display { return &Display{} }

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	kernel32                = windows.NewLazySystemDLL("kernel32.dll")
	procEnumDisplayMonitors = user32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfoW     = user32.NewProc("GetMonitorInfoW")
	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procUnregisterClassW    = user32.NewProc("UnregisterClassW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procPostThreadMessageW  = user32.NewProc("PostThreadMessageW")
	procGetCurrentThreadId  = kernel32.NewProc("GetCurrentThreadId")
)

type rect struct {
	Left, Top, Right, Bottom int32
}

type monitorInfoEx struct {
	Size      uint32
	Monitor   rect
	Work      rect
	Flags     uint32
	SzDevice  [32]uint16
}

const monitorInfoFPrimary = 0x1

// Enumerate returns a snapshot of currently connected monitors. Monitor IDs
// are assigned positionally (0, 1, 2, ...) in the order the OS enumerates them.
func (*Display) Enumerate() ([]proto.Monitor, error) {
	var (
		mu  sync.Mutex
		out []proto.Monitor
		id  uint8
		fn  uintptr
	)
	callback := syscall.NewCallback(func(hMonitor, hdc uintptr, lprcMonitor *rect, lParam uintptr) uintptr {
		var info monitorInfoEx
		info.Size = uint32(unsafe.Sizeof(info))
		r, _, _ := procGetMonitorInfoW.Call(hMonitor, uintptr(unsafe.Pointer(&info)))
		if r == 0 {
			return 1 // keep enumerating even if one query fails
		}
		mu.Lock()
		defer mu.Unlock()
		out = append(out, proto.Monitor{
			ID:      id,
			X:       info.Monitor.Left,
			Y:       info.Monitor.Top,
			W:       uint32(info.Monitor.Right - info.Monitor.Left),
			H:       uint32(info.Monitor.Bottom - info.Monitor.Top),
			Primary: info.Flags&monitorInfoFPrimary != 0,
			Name:    windows.UTF16ToString(info.SzDevice[:]),
		})
		id++
		return 1
	})
	fn = callback
	r, _, errno := procEnumDisplayMonitors.Call(0, 0, fn, 0)
	if r == 0 {
		return nil, fmt.Errorf("EnumDisplayMonitors: %w", errno)
	}
	if len(out) == 0 {
		return nil, errors.New("platform/windows: no monitors found")
	}
	return out, nil
}

// Subscribe runs a hidden message-only window on a dedicated OS thread and
// translates WM_DISPLAYCHANGE notifications into fresh monitor snapshots
// pushed to ch. Notifications are debounced (Windows can fire several in
// rapid succession during a hotplug). Returns when ctx is cancelled or on
// fatal init error.
func (d *Display) Subscribe(ctx context.Context, ch chan<- []proto.Monitor) error {
	notify := make(chan struct{}, 1)
	startedCh := make(chan loopStarted, 1)
	doneCh := make(chan error, 1)

	go runMessageLoop(notify, startedCh, doneCh)

	s := <-startedCh
	if s.err != nil {
		return s.err
	}

	const debounceWindow = 250 * time.Millisecond
	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			procPostThreadMessageW.Call(uintptr(s.threadID), wmQuit, 0, 0)
			<-doneCh
			return ctx.Err()
		case <-notify:
			if debounce == nil {
				debounce = time.After(debounceWindow)
			}
		case <-debounce:
			debounce = nil
			mons, err := d.Enumerate()
			if err != nil {
				continue
			}
			select {
			case ch <- mons:
			case <-ctx.Done():
			}
		case err := <-doneCh:
			return err
		}
	}
}

const (
	wmDisplayChange = 0x007E
	wmQuit          = 0x0012
)

type loopStarted struct {
	threadID uint32
	err      error
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type msgT struct {
	HWND    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

// runMessageLoop pins itself to an OS thread, creates a message-only window
// for WM_DISPLAYCHANGE, and pumps messages until WM_QUIT. Caller signals exit
// via PostThreadMessageW(threadID, WM_QUIT). NOTE: each invocation leaks one
// syscall.NewCallback (Go does not GC syscall callbacks); acceptable because
// Subscribe is expected to be called once per process.
func runMessageLoop(notify chan<- struct{}, startedCh chan<- loopStarted, doneCh chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	tid, _, _ := procGetCurrentThreadId.Call()

	className, _ := syscall.UTF16PtrFromString("rmouseDisplayWatcher")
	wndProc := syscall.NewCallback(func(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
		if msg == wmDisplayChange {
			select {
			case notify <- struct{}{}:
			default:
			}
			return 0
		}
		r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
		return r
	})

	var wcx wndClassEx
	wcx.Size = uint32(unsafe.Sizeof(wcx))
	wcx.WndProc = wndProc
	wcx.ClassName = className
	atom, _, errno := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wcx)))
	if atom == 0 {
		startedCh <- loopStarted{err: fmt.Errorf("RegisterClassExW: %w", errno)}
		return
	}
	defer procUnregisterClassW.Call(uintptr(unsafe.Pointer(className)), 0)

	hwndMessage := ^uintptr(0) - 2 // HWND_MESSAGE = (HWND)-3
	hwnd, _, errno := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(className)), 0, 0,
		0, 0, 0, 0, hwndMessage, 0, 0, 0,
	)
	if hwnd == 0 {
		startedCh <- loopStarted{err: fmt.Errorf("CreateWindowExW: %w", errno)}
		return
	}
	defer procDestroyWindow.Call(hwnd)

	startedCh <- loopStarted{threadID: uint32(tid)}

	var msg msgT
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		// 0 = WM_QUIT, -1 = error
		if r == 0 || int32(r) == -1 {
			doneCh <- nil
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}
