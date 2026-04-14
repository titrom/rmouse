// Package inputevent carries the value types used to pass captured input
// events between a platform-specific Capturer and the rest of the app. It
// is a leaf package so that platform/windows and platform/linux can depend
// on it without creating an import cycle with their parent.
package inputevent

import "github.com/titrom/rmouse/internal/proto"

// Event is a flat sum type covering every kind of captured input.
type Event struct {
	Kind       Kind
	AbsX, AbsY int32             // Kind=MouseMove — post-clamp screen position
	DX, DY     int32             // Kind=MouseMove (relative, if available)
	Button     proto.MouseButton // Kind=MouseButton
	Down       bool              // Kind=MouseButton or KeyEvent
	WheelDX    int16             // Kind=MouseWheel
	WheelDY    int16             // Kind=MouseWheel
	KeyCode    uint16            // Kind=KeyEvent (USB HID usage code)
}

// Kind tags the shape of an Event.
type Kind uint8

const (
	MouseMove   Kind = 1
	MouseButton Kind = 2
	MouseWheel  Kind = 3
	KeyEvent    Kind = 4
)

// Ctl toggles capture suppression. SetConsume(true) makes the capturer
// swallow events locally while still delivering them to its channel.
//
// ClipToPoint and ReleaseClip lock / release the OS cursor to a single
// pixel so the capturer keeps seeing relative deltas while the cursor is
// "virtually" beyond the local screen edge. Implementations that can't
// clip (e.g. Linux placeholder) may no-op.
type Ctl interface {
	SetConsume(on bool)
	ClipToPoint(x, y int32) error
	ReleaseClip() error
}
