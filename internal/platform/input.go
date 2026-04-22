package platform

import (
	"context"

	"github.com/titrom/rmouse/internal/platform/inputevent"
	"github.com/titrom/rmouse/internal/proto"
)

// Injector synthesizes mouse/keyboard events on the local OS. Used by the
// client to apply input received over the wire, and by the server when its
// cursor returns from a client (to reposition the local cursor at a boundary).
//
// All methods are safe to call from multiple goroutines unless an
// implementation documents otherwise.
type Injector interface {
	// MouseMoveRel moves the cursor by (dx, dy) pixels relative to its
	// current position.
	MouseMoveRel(dx, dy int32) error

	// MouseMoveAbs moves the cursor to an absolute position in the local
	// virtual-desktop coordinate space (pixels, origin top-left of the
	// primary monitor; negative values address monitors to the left/above).
	MouseMoveAbs(x, y int32) error

	// MouseButton presses (down=true) or releases (down=false) a mouse button.
	MouseButton(btn proto.MouseButton, down bool) error

	// MouseWheel scrolls by (dx, dy) notches. Positive dy = scroll down,
	// positive dx = scroll right.
	MouseWheel(dx, dy int16) error

	// KeyEvent presses or releases a key. hidCode is a USB HID usage code
	// (page 7, keyboard). Unknown codes are dropped silently.
	KeyEvent(hidCode uint16, down bool) error

	// SetCursorVisible hides or restores the OS cursor. The server hides it
	// while a remote client is grabbed so the trap-parked cursor doesn't sit
	// visibly in the centre of the screen. Implementations that don't apply
	// (e.g. clients) may no-op. Safe to call repeatedly with the same value.
	SetCursorVisible(visible bool) error

	// Close releases any OS resources (virtual device, hooks, cursor clips).
	// Safe to call multiple times.
	Close() error
}

// Capturer reports global mouse/keyboard events from the local OS. Used by
// the server to intercept the physical mouse while it "belongs" to a remote
// client.
type Capturer interface {
	// Capture installs OS-level hooks and returns a channel of events plus
	// a controller that toggles "consume" mode. In consume mode, events are
	// still delivered to the channel but are suppressed from the local OS.
	// The capturer stops and closes the event channel when ctx is cancelled.
	Capture(ctx context.Context) (<-chan inputevent.Event, inputevent.Ctl, error)
}

// Clipboard watches and applies local OS clipboard state.
type Clipboard interface {
	// Read returns the current clipboard payload. ok=false means clipboard does
	// not currently contain a supported format.
	Read() (format proto.ClipboardFormat, data []byte, ok bool, err error)
	// Write replaces clipboard contents with one normalized payload.
	Write(format proto.ClipboardFormat, data []byte) error
	// Watch calls sink whenever clipboard payload changes to a supported format.
	Watch(ctx context.Context, sink func(format proto.ClipboardFormat, data []byte)) error
	// Close releases resources. Safe to call multiple times.
	Close() error
}
