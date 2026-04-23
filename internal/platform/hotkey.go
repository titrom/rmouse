package platform

// Hotkey is a cross-platform handle to a registered global hotkey.
// Fired() returns a channel that receives one signal per key press.
// Close() unregisters the hotkey and must be called to release the
// backing OS resources and goroutine.
type Hotkey interface {
	Fired() <-chan struct{}
	Close()
}
