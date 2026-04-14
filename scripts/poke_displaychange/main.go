//go:build windows

// Dev-only helper: broadcasts WM_DISPLAYCHANGE to all top-level windows so we
// can verify the client's hotplug pipeline without actually replugging a
// monitor. Use: go run ./scripts/poke_displaychange
package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func main() {
	user32 := windows.NewLazySystemDLL("user32.dll")
	procFindWindowW := user32.NewProc("FindWindowW")
	procPostMessageW := user32.NewProc("PostMessageW")

	const wmDisplayChange = 0x007E
	className, _ := syscall.UTF16PtrFromString("rmouseDisplayWatcher")

	hwnd, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(className)), 0)
	if hwnd == 0 {
		fmt.Fprintln(os.Stderr, "no rmouseDisplayWatcher window found — is the client running?")
		os.Exit(1)
	}
	r, _, errno := procPostMessageW.Call(hwnd, wmDisplayChange, 0, 0)
	if r == 0 {
		fmt.Fprintln(os.Stderr, "PostMessageW:", errno)
		os.Exit(1)
	}
	fmt.Println("WM_DISPLAYCHANGE posted to hwnd", hwnd)
}
