// Package proto defines the rmouse wire protocol: message types and a
// length-prefixed binary codec used over the TLS transport.
package proto

import "fmt"

const ProtoVersion uint16 = 2

type MsgType uint8

const (
	TypeHello           MsgType = 1
	TypeWelcome         MsgType = 2
	TypeMouseMove       MsgType = 3
	TypeMouseAbs        MsgType = 4
	TypeMouseButton     MsgType = 5
	TypeMouseWheel      MsgType = 6
	TypeKeyEvent        MsgType = 7
	TypePing            MsgType = 8
	TypePong            MsgType = 9
	TypeGrab            MsgType = 10
	TypeBye             MsgType = 11
	TypeMonitorsChanged MsgType = 12
)

// MaxMonitors bounds the number of monitors a peer may announce in Hello or
// MonitorsChanged. Keeps wire framing deterministic and rejects garbage.
const MaxMonitors = 16

// Monitor describes one physical display of a peer, positioned in that peer's
// virtual desktop. Coordinates may be negative (e.g. a monitor to the left of
// the primary one).
type Monitor struct {
	ID      uint8  // stable within a single announcement
	X, Y    int32  // top-left in virtual desktop coordinates
	W, H    uint32 // pixel dimensions
	Primary bool
	Name    string // platform-specific, informational only (e.g. "\\.\DISPLAY1")
}

func (m *Monitor) encode(e *encoder) {
	e.u8(m.ID)
	e.i32(m.X)
	e.i32(m.Y)
	e.u32(m.W)
	e.u32(m.H)
	e.bool(m.Primary)
	e.str(m.Name)
}

func (m *Monitor) decode(d *decoder) error {
	m.ID = d.u8()
	m.X = d.i32()
	m.Y = d.i32()
	m.W = d.u32()
	m.H = d.u32()
	m.Primary = d.bool()
	m.Name = d.str()
	return d.err
}

func encodeMonitors(e *encoder, ms []Monitor) {
	if len(ms) > MaxMonitors {
		ms = ms[:MaxMonitors]
	}
	e.u8(uint8(len(ms)))
	for i := range ms {
		ms[i].encode(e)
	}
}

func decodeMonitors(d *decoder) []Monitor {
	n := int(d.u8())
	if n > MaxMonitors {
		d.err = fmt.Errorf("proto: monitor count out of range: %d", n)
		return nil
	}
	out := make([]Monitor, n)
	for i := 0; i < n; i++ {
		if err := out[i].decode(d); err != nil {
			return nil
		}
	}
	return out
}

type MouseButton uint8

const (
	BtnLeft    MouseButton = 1
	BtnRight   MouseButton = 2
	BtnMiddle  MouseButton = 3
	BtnX1      MouseButton = 4
	BtnX2      MouseButton = 5
)

type Message interface {
	Type() MsgType
	encode(e *encoder)
	decode(d *decoder) error
}

// Hello — sent by client immediately after TLS handshake. Monitors describes
// the peer's virtual desktop; at least one entry is required.
type Hello struct {
	ProtoVersion uint16
	ClientName   string
	Monitors     []Monitor
	PairingToken string
}

func (*Hello) Type() MsgType { return TypeHello }

func (m *Hello) encode(e *encoder) {
	e.u16(m.ProtoVersion)
	e.str(m.ClientName)
	encodeMonitors(e, m.Monitors)
	e.str(m.PairingToken)
}

func (m *Hello) decode(d *decoder) error {
	m.ProtoVersion = d.u16()
	m.ClientName = d.str()
	m.Monitors = decodeMonitors(d)
	m.PairingToken = d.str()
	return d.err
}

// Welcome — server's response to Hello with the final assigned name.
type Welcome struct {
	AssignedName string
}

func (*Welcome) Type() MsgType { return TypeWelcome }

func (m *Welcome) encode(e *encoder) { e.str(m.AssignedName) }
func (m *Welcome) decode(d *decoder) error {
	m.AssignedName = d.str()
	return d.err
}

// MouseMove — relative pointer motion.
type MouseMove struct {
	DX int16
	DY int16
}

func (*MouseMove) Type() MsgType { return TypeMouseMove }

func (m *MouseMove) encode(e *encoder) {
	e.i16(m.DX)
	e.i16(m.DY)
}
func (m *MouseMove) decode(d *decoder) error {
	m.DX = d.i16()
	m.DY = d.i16()
	return d.err
}

// MouseAbs — absolute positioning, used on first jump to a remote screen.
// X,Y are in the target monitor's local coordinate space (0..W-1, 0..H-1).
// MonitorID must match an ID previously announced via Hello or MonitorsChanged.
type MouseAbs struct {
	MonitorID uint8
	X         uint16
	Y         uint16
}

func (*MouseAbs) Type() MsgType { return TypeMouseAbs }

func (m *MouseAbs) encode(e *encoder) {
	e.u8(m.MonitorID)
	e.u16(m.X)
	e.u16(m.Y)
}
func (m *MouseAbs) decode(d *decoder) error {
	m.MonitorID = d.u8()
	m.X = d.u16()
	m.Y = d.u16()
	return d.err
}

// MouseButtonEvent — mouse button press or release.
type MouseButtonEvent struct {
	Button MouseButton
	Down   bool
}

func (*MouseButtonEvent) Type() MsgType { return TypeMouseButton }

func (m *MouseButtonEvent) encode(e *encoder) {
	e.u8(uint8(m.Button))
	e.bool(m.Down)
}
func (m *MouseButtonEvent) decode(d *decoder) error {
	m.Button = MouseButton(d.u8())
	m.Down = d.bool()
	return d.err
}

// MouseWheel — scroll in pixels (positive = right/down).
type MouseWheel struct {
	DX int16
	DY int16
}

func (*MouseWheel) Type() MsgType { return TypeMouseWheel }

func (m *MouseWheel) encode(e *encoder) {
	e.i16(m.DX)
	e.i16(m.DY)
}
func (m *MouseWheel) decode(d *decoder) error {
	m.DX = d.i16()
	m.DY = d.i16()
	return d.err
}

// KeyEvent — platform-neutral key press or release (keycode = HID usage).
type KeyEvent struct {
	KeyCode uint16
	Down    bool
}

func (*KeyEvent) Type() MsgType { return TypeKeyEvent }

func (m *KeyEvent) encode(e *encoder) {
	e.u16(m.KeyCode)
	e.bool(m.Down)
}
func (m *KeyEvent) decode(d *decoder) error {
	m.KeyCode = d.u16()
	m.Down = d.bool()
	return d.err
}

// Ping / Pong — heartbeat.
type Ping struct{ Seq uint32 }

func (*Ping) Type() MsgType          { return TypePing }
func (m *Ping) encode(e *encoder)    { e.u32(m.Seq) }
func (m *Ping) decode(d *decoder) error {
	m.Seq = d.u32()
	return d.err
}

type Pong struct{ Seq uint32 }

func (*Pong) Type() MsgType          { return TypePong }
func (m *Pong) encode(e *encoder)    { e.u32(m.Seq) }
func (m *Pong) decode(d *decoder) error {
	m.Seq = d.u32()
	return d.err
}

// Grab — server tells a client that it currently owns the cursor.
type Grab struct{ On bool }

func (*Grab) Type() MsgType          { return TypeGrab }
func (m *Grab) encode(e *encoder)    { e.bool(m.On) }
func (m *Grab) decode(d *decoder) error {
	m.On = d.bool()
	return d.err
}

// Bye — graceful shutdown notice with human-readable reason.
type Bye struct{ Reason string }

func (*Bye) Type() MsgType          { return TypeBye }
func (m *Bye) encode(e *encoder)    { e.str(m.Reason) }
func (m *Bye) decode(d *decoder) error {
	m.Reason = d.str()
	return d.err
}

// MonitorsChanged — peer's display layout changed (hotplug, resolution edit,
// monitor reassignment). Receiver replaces its cached Monitors for this peer.
type MonitorsChanged struct {
	Monitors []Monitor
}

func (*MonitorsChanged) Type() MsgType           { return TypeMonitorsChanged }
func (m *MonitorsChanged) encode(e *encoder)     { encodeMonitors(e, m.Monitors) }
func (m *MonitorsChanged) decode(d *decoder) error {
	m.Monitors = decodeMonitors(d)
	return d.err
}

// newByType returns a zero-value message for a given type tag.
func newByType(t MsgType) Message {
	switch t {
	case TypeHello:
		return &Hello{}
	case TypeWelcome:
		return &Welcome{}
	case TypeMouseMove:
		return &MouseMove{}
	case TypeMouseAbs:
		return &MouseAbs{}
	case TypeMouseButton:
		return &MouseButtonEvent{}
	case TypeMouseWheel:
		return &MouseWheel{}
	case TypeKeyEvent:
		return &KeyEvent{}
	case TypePing:
		return &Ping{}
	case TypePong:
		return &Pong{}
	case TypeGrab:
		return &Grab{}
	case TypeBye:
		return &Bye{}
	case TypeMonitorsChanged:
		return &MonitorsChanged{}
	}
	return nil
}
