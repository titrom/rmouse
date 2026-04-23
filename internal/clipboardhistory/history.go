// Package clipboardhistory is a tiny in-memory ring buffer of clipboard
// snapshots (text / PNG image / file list) used by the GUIs to render a
// history panel and restore previous items into the local clipboard.
//
// The history is per-process: each GUI maintains its own, fed from whatever
// clipboard events flow through that process (local copies + items received
// from peers). It is not synchronised across peers — that would require a
// protocol change and deliberate memory limits on the server.
package clipboardhistory

import (
	"sync"
	"time"

	"github.com/titrom/rmouse/internal/proto"
)

// Item is one snapshot of the OS clipboard.
type Item struct {
	ID        uint64
	Format    proto.ClipboardFormat
	Data      []byte // full payload — used to restore into the OS clipboard.
	Origin    string // "local", peer name, or "server".
	Timestamp time.Time
}

// History is a bounded, thread-safe ring buffer of Item. Oldest entries are
// evicted when capacity is reached. Consecutive duplicates are collapsed
// (we refresh the timestamp on the existing entry rather than inserting a
// second copy).
type History struct {
	mu       sync.Mutex
	items    []Item
	capacity int
	nextID   uint64
	onChange func()
}

// New returns an empty history with the given capacity. capacity <= 0 falls
// back to a sensible default (30).
func New(capacity int) *History {
	if capacity <= 0 {
		capacity = 30
	}
	return &History{capacity: capacity}
}

// SetOnChange registers a callback invoked (without the mutex held) every
// time the history mutates — intended so a GUI can push a "changed" event
// to its frontend. Passing nil disables notifications.
func (h *History) SetOnChange(fn func()) {
	h.mu.Lock()
	h.onChange = fn
	h.mu.Unlock()
}

// Add records a clipboard snapshot. Returns the assigned ID, or 0 if the
// payload was rejected (empty data, or an exact duplicate of the most recent
// entry — in the latter case we just bump its timestamp).
func (h *History) Add(format proto.ClipboardFormat, data []byte, origin string) uint64 {
	if len(data) == 0 {
		return 0
	}
	h.mu.Lock()
	if n := len(h.items); n > 0 {
		last := &h.items[n-1]
		if last.Format == format && bytesEqual(last.Data, data) {
			last.Timestamp = time.Now()
			last.Origin = origin
			id := last.ID
			cb := h.onChange
			h.mu.Unlock()
			if cb != nil {
				cb()
			}
			return id
		}
	}
	h.nextID++
	it := Item{
		ID:        h.nextID,
		Format:    format,
		Data:      append([]byte(nil), data...),
		Origin:    origin,
		Timestamp: time.Now(),
	}
	h.items = append(h.items, it)
	if len(h.items) > h.capacity {
		// Drop oldest entries. Typical case: one entry over — one shift.
		drop := len(h.items) - h.capacity
		h.items = append(h.items[:0:0], h.items[drop:]...)
	}
	cb := h.onChange
	h.mu.Unlock()
	if cb != nil {
		cb()
	}
	return it.ID
}

// Snapshot returns a newest-first copy of the current items. Data slices
// inside the result reference the same underlying bytes as the internal
// storage — callers must not mutate them.
func (h *History) Snapshot() []Item {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Item, len(h.items))
	for i := range h.items {
		out[len(h.items)-1-i] = h.items[i]
	}
	return out
}

// Get returns the item with the given ID, or (_, false) if not found.
func (h *History) Get(id uint64) (Item, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.items {
		if h.items[i].ID == id {
			return h.items[i], true
		}
	}
	return Item{}, false
}

// Clear drops every entry.
func (h *History) Clear() {
	h.mu.Lock()
	h.items = nil
	cb := h.onChange
	h.mu.Unlock()
	if cb != nil {
		cb()
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
