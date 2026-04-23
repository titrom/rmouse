//go:build !windows

package platform

import "errors"

// NewClipboardHistoryHotkey is only implemented on Windows so far. Other
// platforms return an error; the GUI falls back to its in-app button.
func NewClipboardHistoryHotkey() (Hotkey, error) {
	return nil, errors.New("global hotkey is not supported on this platform")
}
