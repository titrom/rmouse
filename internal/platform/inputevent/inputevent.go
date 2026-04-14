// Package inputevent carries the value types used to pass captured input
// events between a platform-specific Capturer and the rest of the app. It
// is a leaf package so that platform/windows and platform/linux can depend
// on it without creating an import cycle with their parent.
package inputevent

import "github.com/titrom/rmouse/internal/proto"

// Event is a flat sum type covering every kind of captured input.
type Event struct {
	Kind    Kind
	DX, DY  int32             // Kind=MouseMove (relative)
	Button  proto.MouseButton // Kind=MouseButton
	Down    bool              // Kind=MouseButton or KeyEvent
	WheelDX int16             // Kind=MouseWheel
	WheelDY int16             // Kind=MouseWheel
	KeyCode uint16            // Kind=KeyEvent (USB HID usage code)
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
type Ctl interface {
	SetConsume(on bool)
}
