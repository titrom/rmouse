//go:build linux

package linux

import (
	"context"
	"errors"

	"github.com/titrom/rmouse/internal/platform/inputevent"
)

// Capturer is a placeholder for Linux input capture. Implementing it well
// requires either /dev/input/event* + EVIOCGRAB (root / input group) or a
// Wayland-compositor-specific protocol; deferred to a future phase.
type Capturer struct{}

// NewCapturer returns the stub Linux capturer.
func NewCapturer() *Capturer { return &Capturer{} }

// Capture always fails on Linux in the current milestone. The server can
// still run on Linux, but it won't intercept local mouse/keyboard input.
func (*Capturer) Capture(context.Context) (<-chan inputevent.Event, inputevent.Ctl, error) {
	return nil, nil, errors.New("platform/linux: input capture not yet implemented")
}
