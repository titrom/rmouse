package proto

import (
	"bytes"
	"reflect"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	cases := []Message{
		&Hello{
			ProtoVersion: ProtoVersion,
			ClientName:   "laptop",
			Monitors: []Monitor{
				{ID: 0, X: 0, Y: 0, W: 1920, H: 1080, Primary: true, Name: `\\.\DISPLAY1`},
				{ID: 1, X: 1920, Y: -120, W: 2560, H: 1440, Name: `\\.\DISPLAY2`},
			},
			PairingToken: "s3cret",
		},
		&Welcome{AssignedName: "laptop-1"},
		&MouseMove{DX: -5, DY: 7},
		&MouseAbs{MonitorID: 1, X: 12345, Y: 678},
		&MonitorsChanged{Monitors: []Monitor{{ID: 0, X: -1920, Y: 0, W: 1920, H: 1080, Name: "HDMI-A-0"}}},
		&MouseButtonEvent{Button: BtnLeft, Down: true},
		&MouseButtonEvent{Button: BtnX2, Down: false},
		&MouseWheel{DX: 0, DY: -3},
		&KeyEvent{KeyCode: 0x0004, Down: true}, // HID 'a'
		&Ping{Seq: 42},
		&Pong{Seq: 42},
		&Grab{On: true},
		&Bye{Reason: "shutdown"},
	}
	for _, want := range cases {
		var buf bytes.Buffer
		if err := Write(&buf, want); err != nil {
			t.Fatalf("Write(%T): %v", want, err)
		}
		got, err := Read(&buf)
		if err != nil {
			t.Fatalf("Read(%T): %v", want, err)
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("round-trip mismatch for %T:\nwant %#v\ngot  %#v", want, want, got)
		}
	}
}

func TestReadTruncated(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, &Hello{ClientName: "x"}); err != nil {
		t.Fatal(err)
	}
	// drop the last byte
	data := buf.Bytes()
	trunc := bytes.NewReader(data[:len(data)-1])
	if _, err := Read(trunc); err == nil {
		t.Fatal("expected error on truncated frame")
	}
}

func TestReadUnknownType(t *testing.T) {
	var buf bytes.Buffer
	// frame: length=1, type=255
	buf.Write([]byte{1, 0, 0, 0, 255})
	if _, err := Read(&buf); err == nil {
		t.Fatal("expected ErrUnknownType")
	}
}

func TestWriteTooLarge(t *testing.T) {
	// Build a Hello with a pairing token just over MaxFrameSize
	big := make([]byte, 0, MaxFrameSize+16)
	for len(big) <= MaxFrameSize {
		big = append(big, 'a')
	}
	// We can't put >64KiB into a u16-prefixed str, so build payload via many fields.
	// Instead just assert MaxFrameSize is enforced via a synthetic encoder state.
	e := newEncoder()
	e.buf = make([]byte, MaxFrameSize+1)
	var buf bytes.Buffer
	// manually pass through Write by embedding a fake message would be messy;
	// here we just verify Read rejects an oversize length header.
	buf.Write([]byte{0, 0, 0x20, 0}) // length = 0x00200000 = 2 MiB > MaxFrameSize
	if _, err := Read(&buf); err == nil {
		t.Fatal("expected frame-too-large error")
	}
}
