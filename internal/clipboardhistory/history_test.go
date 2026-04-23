package clipboardhistory

import (
	"testing"

	"github.com/titrom/rmouse/internal/proto"
)

func TestAddSnapshotNewestFirst(t *testing.T) {
	h := New(4)
	h.Add(proto.ClipboardFormatTextPlain, []byte("one"), "local")
	h.Add(proto.ClipboardFormatTextPlain, []byte("two"), "local")
	h.Add(proto.ClipboardFormatTextPlain, []byte("three"), "peer")
	snap := h.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("len=%d want 3", len(snap))
	}
	if string(snap[0].Data) != "three" || snap[0].Origin != "peer" {
		t.Errorf("newest first: %+v", snap[0])
	}
	if string(snap[2].Data) != "one" {
		t.Errorf("oldest last: %+v", snap[2])
	}
}

func TestEvictionAtCapacity(t *testing.T) {
	h := New(2)
	h.Add(proto.ClipboardFormatTextPlain, []byte("a"), "")
	h.Add(proto.ClipboardFormatTextPlain, []byte("b"), "")
	h.Add(proto.ClipboardFormatTextPlain, []byte("c"), "")
	snap := h.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("cap=2, got len=%d", len(snap))
	}
	if string(snap[0].Data) != "c" || string(snap[1].Data) != "b" {
		t.Errorf("expected [c, b], got %s/%s", snap[0].Data, snap[1].Data)
	}
}

func TestDuplicateCollapses(t *testing.T) {
	h := New(5)
	id1 := h.Add(proto.ClipboardFormatTextPlain, []byte("x"), "a")
	id2 := h.Add(proto.ClipboardFormatTextPlain, []byte("x"), "b")
	if id1 != id2 {
		t.Fatalf("expected duplicate to reuse id, %d vs %d", id1, id2)
	}
	snap := h.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected collapsed, got %d entries", len(snap))
	}
	if snap[0].Origin != "b" {
		t.Errorf("origin should refresh to latest, got %q", snap[0].Origin)
	}
}

func TestOnChangeFires(t *testing.T) {
	h := New(3)
	var count int
	h.SetOnChange(func() { count++ })
	h.Add(proto.ClipboardFormatTextPlain, []byte("a"), "")
	h.Add(proto.ClipboardFormatTextPlain, []byte("b"), "")
	h.Clear()
	if count != 3 {
		t.Errorf("onChange fired %d times, want 3", count)
	}
}

func TestAddEmptyRejected(t *testing.T) {
	h := New(3)
	id := h.Add(proto.ClipboardFormatTextPlain, nil, "")
	if id != 0 {
		t.Fatalf("expected 0 on empty, got %d", id)
	}
	if len(h.Snapshot()) != 0 {
		t.Fatal("empty should not be stored")
	}
}
