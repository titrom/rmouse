//go:build windows

package platform

import "github.com/titrom/rmouse/internal/platform/windows"

// New returns the OS-specific Display implementation for the current platform.
func New() Display { return windows.New() }

// NewInjector returns the OS-specific input injector for the current platform.
func NewInjector() (Injector, error) { return windows.NewInjector() }
