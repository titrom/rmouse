//go:build !windows && !linux

package platform

import (
	"context"
	"errors"

	"github.com/titrom/rmouse/internal/platform/inputevent"
	"github.com/titrom/rmouse/internal/proto"
)

type unsupported struct{}

func (unsupported) Enumerate() ([]proto.Monitor, error) {
	return nil, errors.New("platform: display enumeration not implemented on this OS")
}
func (unsupported) Subscribe(ctx context.Context, ch chan<- []proto.Monitor) error {
	return errors.New("platform: hotplug subscription not implemented on this OS")
}

// New returns a stub Display for unsupported platforms.
func New() Display { return unsupported{} }

// NewInjector returns a stub error for unsupported platforms.
func NewInjector() (Injector, error) {
	return nil, errors.New("platform: input injection not implemented on this OS")
}

type unsupportedCapturer struct{}

func (unsupportedCapturer) Capture(context.Context) (<-chan inputevent.Event, inputevent.Ctl, error) {
	return nil, nil, errors.New("platform: input capture not implemented on this OS")
}

// NewCapturer returns a stub capturer for unsupported platforms.
func NewCapturer() Capturer { return unsupportedCapturer{} }

// NewClipboard returns a stub error for unsupported platforms.
func NewClipboard() (Clipboard, error) {
	return nil, errors.New("platform: clipboard not implemented on this OS")
}
