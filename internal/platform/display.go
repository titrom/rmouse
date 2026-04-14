// Package platform bridges between the OS-specific display and input stacks
// and the rest of rmouse. Concrete implementations live in platform/windows
// and platform/linux; they are selected automatically via build tags.
package platform

import (
	"context"

	"github.com/titrom/rmouse/internal/proto"
)

// Display enumerates physical monitors of the local machine and optionally
// notifies on layout changes (hotplug, resolution edit, primary reassignment).
type Display interface {
	// Enumerate returns a snapshot of the current monitor layout. IDs are
	// stable within a single process run: Enumerate followed by Subscribe
	// updates must keep IDs consistent for monitors that did not change.
	Enumerate() ([]proto.Monitor, error)

	// Subscribe blocks, pushing layout snapshots to ch whenever the OS
	// reports a change. Returns when ctx is cancelled or on fatal error.
	// Implementations that do not support hotplug should return a sentinel
	// error immediately so the caller can fall back to periodic polling.
	Subscribe(ctx context.Context, ch chan<- []proto.Monitor) error
}
