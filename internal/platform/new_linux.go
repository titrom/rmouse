//go:build linux

package platform

import "github.com/titrom/rmouse/internal/platform/linux"

// New returns the OS-specific Display implementation for the current platform.
func New() Display { return linux.New() }

// NewInjector returns the OS-specific input injector for the current platform.
func NewInjector() (Injector, error) { return linux.NewInjector() }

// NewCapturer returns the OS-specific input capturer for the current platform.
func NewCapturer() Capturer { return linux.NewCapturer() }

// NewClipboard returns Linux clipboard integration when implemented.
func NewClipboard() (Clipboard, error) { return linux.NewClipboard() }
