//go:build windows

package platform

import (
	"github.com/titrom/rmouse/internal/platform/windows"
)

// NewClipboardHistoryHotkey registers the clipboard-history toggle hotkey.
// Currently fixed to Ctrl+Shift+V. If the combination is already held by
// another process, returns an error — callers should fall back to an in-app
// button and log.
func NewClipboardHistoryHotkey() (Hotkey, error) {
	return windows.RegisterGlobalHotkey(
		1, // arbitrary per-thread id; unique within this process is enough
		windows.ModControl|windows.ModShift,
		windows.VK_V,
	)
}
