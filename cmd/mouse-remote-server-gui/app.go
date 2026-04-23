package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"github.com/zalando/go-keyring"

	"github.com/titrom/rmouse/internal/app/server"
	"github.com/titrom/rmouse/internal/clipboardhistory"
	"github.com/titrom/rmouse/internal/platform"
	"github.com/titrom/rmouse/internal/proto"
	"github.com/titrom/rmouse/internal/transport"
)

const (
	keyringService = "rmouse-server"
	keyringUser    = "default"
)

type App struct {
	ctx context.Context

	mu         sync.Mutex
	cancel     context.CancelFunc
	done       chan struct{}
	router     *server.Router
	placements map[string]server.Placement

	history     *clipboardhistory.History
	restoreClip platform.Clipboard
	hotkey      platform.Hotkey
	hotkeyStop  chan struct{}
}

func NewApp() *App {
	return &App{history: clipboardhistory.New(30)}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.history.SetOnChange(func() {
		runtime.EventsEmit(a.ctx, "rmouse:clipboardHistory", nil)
	})
	if cb, err := platform.NewClipboard(); err == nil {
		a.restoreClip = cb
	}
	if hk, err := platform.NewClipboardHistoryHotkey(); err == nil {
		a.hotkey = hk
		a.hotkeyStop = make(chan struct{})
		go a.drainHotkey()
	} else {
		runtime.EventsEmit(a.ctx, "rmouse:hotkeyUnavailable", map[string]any{"err": err.Error()})
	}
}

func (a *App) shutdown(_ context.Context) {
	_ = a.Stop()
	if a.hotkey != nil {
		close(a.hotkeyStop)
		a.hotkey.Close()
	}
	if a.restoreClip != nil {
		_ = a.restoreClip.Close()
	}
}

func (a *App) drainHotkey() {
	for {
		select {
		case <-a.hotkeyStop:
			return
		case <-a.hotkey.Fired():
			runtime.WindowShow(a.ctx)
			runtime.WindowUnminimise(a.ctx)
			runtime.EventsEmit(a.ctx, "rmouse:clipboardHistoryOpen", nil)
		}
	}
}

// --- DTOs ----------------------------------------------------------------

type ConfigDTO struct {
	Addr      string `json:"addr"`
	Token     string `json:"token"`
	RelayAddr string `json:"relayAddr"`
	Session   string `json:"session"`
	Clipboard bool   `json:"clipboard"`
}

type MonitorDTO struct {
	ID      uint8  `json:"id"`
	X       int32  `json:"x"`
	Y       int32  `json:"y"`
	W       uint32 `json:"w"`
	H       uint32 `json:"h"`
	Primary bool   `json:"primary"`
	Name    string `json:"name"`
}

func toMonitorDTOs(mons []proto.Monitor) []MonitorDTO {
	out := make([]MonitorDTO, len(mons))
	for i, m := range mons {
		out[i] = MonitorDTO{ID: m.ID, X: m.X, Y: m.Y, W: m.W, H: m.H, Primary: m.Primary, Name: m.Name}
	}
	return out
}

// --- Config persistence --------------------------------------------------

type placedClient struct {
	X int32 `json:"x"`
	Y int32 `json:"y"`
}

type persistedConfig struct {
	Addr       string                  `json:"addr"`
	RelayAddr  string                  `json:"relayAddr"`
	Session    string                  `json:"session"`
	Clipboard  bool                    `json:"clipboard"`
	Placements map[string]placedClient `json:"placements,omitempty"`
}

// PlacementDTO mirrors server.Placement for the frontend — absolute
// world coordinates (top-left of the client's monitor bbox).
type PlacementDTO struct {
	Name string `json:"name"`
	X    int32  `json:"x"`
	Y    int32  `json:"y"`
}

func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "rmouse", "server-gui.json"), nil
}

func (a *App) LoadConfig() (ConfigDTO, error) {
	dto := ConfigDTO{Addr: "0.0.0.0:24242"}
	path, err := configPath()
	if err != nil {
		return dto, nil
	}
	placements := map[string]server.Placement{}
	if raw, err := os.ReadFile(path); err == nil {
		var p persistedConfig
		if err := json.Unmarshal(raw, &p); err == nil {
			if p.Addr != "" {
				dto.Addr = p.Addr
			}
			dto.RelayAddr = p.RelayAddr
			dto.Session = p.Session
			dto.Clipboard = p.Clipboard
			for name, pl := range p.Placements {
				placements[name] = server.Placement{X: pl.X, Y: pl.Y}
			}
		}
	}
	a.mu.Lock()
	a.placements = placements
	a.mu.Unlock()
	if tok, err := keyring.Get(keyringService, keyringUser); err == nil {
		dto.Token = tok
	}
	return dto, nil
}

func (a *App) SaveConfig(cfg ConfigDTO) error {
	if err := a.writePersisted(&cfg); err != nil {
		return err
	}
	if cfg.Token == "" {
		_ = keyring.Delete(keyringService, keyringUser)
	} else {
		if err := keyring.Set(keyringService, keyringUser, cfg.Token); err != nil {
			return fmt.Errorf("keyring: %w", err)
		}
	}
	return nil
}

// writePersisted flushes the non-secret config + current placements to disk.
// When cfg is nil we keep the existing addr/relay/session values by
// re-reading the file first — used by the placement callback so a drag
// event doesn't clobber what the user typed in the form.
func (a *App) writePersisted(cfg *ConfigDTO) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var existing persistedConfig
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &existing)
	}
	p := existing
	if cfg != nil {
		p.Addr = cfg.Addr
		p.RelayAddr = cfg.RelayAddr
		p.Session = cfg.Session
		p.Clipboard = cfg.Clipboard
	}
	a.mu.Lock()
	p.Placements = make(map[string]placedClient, len(a.placements))
	for name, pl := range a.placements {
		p.Placements[name] = placedClient{X: pl.X, Y: pl.Y}
	}
	a.mu.Unlock()
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// CertFingerprint returns SHA-256 hex of the DER-encoded self-signed cert.
// The fingerprint is how a client operator verifies the server's identity
// out-of-band (paste it into the client, compare face-to-face, etc.).
func (a *App) CertFingerprint() (string, error) {
	certPath, keyPath, err := server.CertPaths()
	if err != nil {
		return "", err
	}
	// Generate the cert on first run so the fingerprint is always available.
	// server.Run also calls EnsureServerCert; calling it here is idempotent.
	if err := transport.EnsureServerCert(certPath, keyPath); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(certPath)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return "", errors.New("cert file not PEM-encoded")
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}

// --- Session control -----------------------------------------------------

func (a *App) Start(cfg ConfigDTO) error {
	a.mu.Lock()
	if a.cancel != nil {
		a.mu.Unlock()
		return errors.New("already running")
	}
	if cfg.Token == "" {
		a.mu.Unlock()
		return errors.New("token is required")
	}
	ctx, cancel := context.WithCancel(a.ctx)
	done := make(chan struct{})
	a.cancel = cancel
	a.done = done
	a.mu.Unlock()

	a.mu.Lock()
	placementsSnapshot := make(map[string]server.Placement, len(a.placements))
	for k, v := range a.placements {
		placementsSnapshot[k] = v
	}
	a.mu.Unlock()

	go func() {
		defer close(done)
		sc := server.Config{
			Addr:            cfg.Addr,
			Token:           cfg.Token,
			RelayAddr:       cfg.RelayAddr,
			Session:         cfg.Session,
			EnableClipboard: cfg.Clipboard,
			OnClipboardItem: func(origin string, format proto.ClipboardFormat, data []byte) {
				a.history.Add(format, data, origin)
			},
			Placements: placementsSnapshot,
			OnRouterReady: func(r *server.Router) {
				a.mu.Lock()
				a.router = r
				a.mu.Unlock()
			},
			OnPlacementChanged: func(name string, p server.Placement) {
				a.mu.Lock()
				if a.placements == nil {
					a.placements = map[string]server.Placement{}
				}
				a.placements[name] = p
				a.mu.Unlock()
				if err := a.writePersisted(nil); err != nil {
					runtime.EventsEmit(a.ctx, "rmouse:fatal", "persist placements: "+err.Error())
				}
			},
		}
		err := server.Run(ctx, sc, func(ev server.Event) {
			a.emitEvent(ev)
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			runtime.EventsEmit(a.ctx, "rmouse:fatal", err.Error())
		}
		a.mu.Lock()
		a.cancel = nil
		a.done = nil
		a.router = nil
		a.mu.Unlock()
		runtime.EventsEmit(a.ctx, "rmouse:stopped", nil)
	}()
	// Give the listener a moment to bind before returning so the frontend
	// can distinguish "starting" from "bound".
	time.Sleep(50 * time.Millisecond)
	return nil
}

func (a *App) Stop() error {
	a.mu.Lock()
	cancel, done := a.cancel, a.done
	a.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	<-done
	return nil
}

func (a *App) IsRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cancel != nil
}

// GetServerMonitors returns the host's monitor layout. When the server is
// running, the cached list from the router is returned; otherwise the
// platform is enumerated fresh so the GUI can draw the grid at any time.
func (a *App) GetServerMonitors() ([]MonitorDTO, error) {
	a.mu.Lock()
	r := a.router
	a.mu.Unlock()
	if r != nil {
		return toMonitorDTOs(r.ServerMonitors()), nil
	}
	mons, err := platform.New().Enumerate()
	if err != nil {
		return nil, err
	}
	return toMonitorDTOs(mons), nil
}

// GetPlacements returns the current client-name → cell mapping. Used by
// the GUI on startup to restore the grid before any clients reconnect.
func (a *App) GetPlacements() []PlacementDTO {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]PlacementDTO, 0, len(a.placements))
	for name, p := range a.placements {
		out = append(out, PlacementDTO{Name: name, X: p.X, Y: p.Y})
	}
	return out
}

// SetClientPlacement places every live client with the given name so its
// monitor-bbox top-left sits at absolute virtual-desktop coordinates (x,
// y) and persists the choice. The GUI is responsible for collision
// avoidance and (optional) grid snapping before calling this.
func (a *App) SetClientPlacement(name string, x, y int32) error {
	if name == "" {
		return errors.New("name is required")
	}
	a.mu.Lock()
	r := a.router
	a.mu.Unlock()
	if r != nil {
		r.SetPlacement(name, x, y)
		return nil
	}
	// Server not running — still persist so the placement is applied on
	// next Start.
	a.mu.Lock()
	if a.placements == nil {
		a.placements = map[string]server.Placement{}
	}
	a.placements[name] = server.Placement{X: x, Y: y}
	a.mu.Unlock()
	return a.writePersisted(nil)
}

// --- Clipboard history ---------------------------------------------------

type ClipboardHistoryItemDTO struct {
	ID          uint64 `json:"id"`
	Kind        string `json:"kind"` // "text" | "image" | "files"
	Origin      string `json:"origin"`
	Timestamp   int64  `json:"timestamp"`
	Preview     string `json:"preview"`
	ImageBase64 string `json:"imageBase64,omitempty"`
	SizeBytes   int    `json:"sizeBytes"`
}

func itemToDTO(it clipboardhistory.Item) ClipboardHistoryItemDTO {
	dto := ClipboardHistoryItemDTO{
		ID:        it.ID,
		Origin:    it.Origin,
		Timestamp: it.Timestamp.UnixMilli(),
		SizeBytes: len(it.Data),
	}
	switch it.Format {
	case proto.ClipboardFormatTextPlain:
		dto.Kind = "text"
		s := string(it.Data)
		if len(s) > 200 {
			s = s[:200] + "…"
		}
		dto.Preview = s
	case proto.ClipboardFormatImagePNG:
		dto.Kind = "image"
		dto.ImageBase64 = base64.StdEncoding.EncodeToString(it.Data)
	case proto.ClipboardFormatFilesList:
		dto.Kind = "files"
		var paths []string
		if err := json.Unmarshal(it.Data, &paths); err == nil {
			if len(paths) > 5 {
				dto.Preview = fmt.Sprintf("%d files: %s, …", len(paths), paths[0])
			} else {
				dto.Preview = fmt.Sprintf("%d files: %v", len(paths), paths)
			}
		} else {
			dto.Preview = string(it.Data)
		}
	}
	return dto
}

func (a *App) GetClipboardHistory() []ClipboardHistoryItemDTO {
	snap := a.history.Snapshot()
	out := make([]ClipboardHistoryItemDTO, len(snap))
	for i, it := range snap {
		out[i] = itemToDTO(it)
	}
	return out
}

func (a *App) RestoreClipboardItem(id uint64) error {
	if a.restoreClip == nil {
		return errors.New("clipboard is not available on this platform")
	}
	it, ok := a.history.Get(id)
	if !ok {
		return fmt.Errorf("history item %d not found", id)
	}
	return a.restoreClip.Write(it.Format, it.Data)
}

func (a *App) ClearClipboardHistory() { a.history.Clear() }

func (a *App) ShowClipboardHistory() {
	runtime.WindowShow(a.ctx)
	runtime.EventsEmit(a.ctx, "rmouse:clipboardHistoryOpen", nil)
}

func (a *App) emitEvent(ev server.Event) {
	switch e := ev.(type) {
	case server.ListeningEvent:
		runtime.EventsEmit(a.ctx, "rmouse:listening", map[string]any{
			"addr": e.Addr, "certPath": e.CertPath,
		})
	case server.ServingViaRelayEvent:
		runtime.EventsEmit(a.ctx, "rmouse:listening", map[string]any{
			"relay": e.Relay, "session": e.Session, "certPath": e.CertPath,
		})
	case server.ServerMonitorsEvent:
		runtime.EventsEmit(a.ctx, "rmouse:serverMonitors", map[string]any{
			"monitors": toMonitorDTOs(e.Monitors),
		})
	case server.ClientPlacedEvent:
		runtime.EventsEmit(a.ctx, "rmouse:clientPlaced", map[string]any{
			"id":   string(e.ID),
			"name": e.Name,
			"x":    e.X,
			"y":    e.Y,
		})
	case server.ClientConnectedEvent:
		runtime.EventsEmit(a.ctx, "rmouse:client", map[string]any{
			"state":    "connected",
			"id":       string(e.ID),
			"name":     e.Name,
			"remote":   e.RemoteAddr,
			"monitors": toMonitorDTOs(e.Monitors),
		})
	case server.MonitorsChangedEvent:
		runtime.EventsEmit(a.ctx, "rmouse:client", map[string]any{
			"state":    "monitorsChanged",
			"id":       string(e.ID),
			"name":     e.Name,
			"remote":   e.RemoteAddr,
			"monitors": toMonitorDTOs(e.Monitors),
		})
	case server.ByeEvent:
		runtime.EventsEmit(a.ctx, "rmouse:client", map[string]any{
			"state": "bye", "id": string(e.ID), "name": e.Name, "remote": e.RemoteAddr, "reason": e.Reason,
		})
	case server.ClientDisconnectedEvent:
		payload := map[string]any{"state": "disconnected", "id": string(e.ID), "name": e.Name, "remote": e.RemoteAddr}
		if e.Err != nil {
			payload["err"] = e.Err.Error()
		}
		runtime.EventsEmit(a.ctx, "rmouse:client", payload)
	case server.RecvErrorEvent:
		runtime.EventsEmit(a.ctx, "rmouse:recvError", map[string]any{
			"id": string(e.ID), "name": e.Name, "remote": e.RemoteAddr, "err": e.Err.Error(),
		})
	case server.ClipboardUnavailableEvent:
		msg := ""
		if e.Err != nil {
			msg = e.Err.Error()
		}
		runtime.EventsEmit(a.ctx, "rmouse:clipboardUnavailable", map[string]any{"err": msg})
	}
}
