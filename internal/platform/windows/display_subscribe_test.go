//go:build windows

package windows

import (
	"context"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/titrom/rmouse/internal/proto"
)

// TestSubscribe_WMDisplayChange verifies the wiring: the message-only window
// is created, a synthetic WM_DISPLAYCHANGE triggers a snapshot, and ctx
// cancellation shuts the loop down cleanly.
func TestSubscribe_WMDisplayChange(t *testing.T) {
	d := New()
	if _, err := d.Enumerate(); err != nil {
		t.Skipf("no monitors in this environment: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan []proto.Monitor, 4)
	done := make(chan error, 1)
	go func() { done <- d.Subscribe(ctx, ch) }()

	// Poll for the hidden window to be created and then post WM_DISPLAYCHANGE.
	user32 := windows.NewLazySystemDLL("user32.dll")
	procFindWindowW := user32.NewProc("FindWindowW")
	procPostMessageW := user32.NewProc("PostMessageW")

	className, _ := syscall.UTF16PtrFromString("rmouseDisplayWatcher")

	var hwnd uintptr
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(className)), 0)
		if h != 0 {
			hwnd = h
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if hwnd == 0 {
		cancel()
		<-done
		t.Fatal("message-only window was never created")
	}

	r, _, errno := procPostMessageW.Call(hwnd, wmDisplayChange, 0, 0)
	if r == 0 {
		cancel()
		<-done
		t.Fatalf("PostMessageW: %v", errno)
	}

	select {
	case mons := <-ch:
		if len(mons) == 0 {
			t.Fatal("snapshot has no monitors")
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("no snapshot received within debounce+timeout")
	}

	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Subscribe returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe did not return after ctx cancel")
	}
}
