package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"github.com/zalando/go-keyring"

	"github.com/titrom/rmouse/internal/app/client"
	"github.com/titrom/rmouse/internal/clipboardhistory"
	"github.com/titrom/rmouse/internal/platform"
	"github.com/titrom/rmouse/internal/proto"
)

const (
	keyringService = "rmouse-client"
	keyringUser    = "default"
)

// App is the Wails-bound object. All exported methods are callable from JS.
type App struct {
	ctx context.Context

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{} // closed when the current Run returns

	history *clipboardhistory.History
	// restoreClip is a long-lived Clipboard handle used only by
	// RestoreClipboardItem — independent from the session's own Watcher so
	// the user can pick from history even when not connected.
	restoreClip platform.Clipboard
	hotkey      platform.Hotkey
	hotkeyStop  chan struct{}
}

func NewApp() *App {
	return &App{
		history: clipboardhistory.New(30),
	}
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
	Name      string `json:"name"`
	PingMs    int    `json:"pingMs"`
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

type persistedConfig struct {
	Addr      string `json:"addr"`
	Name      string `json:"name"`
	PingMs    int    `json:"pingMs"`
	RelayAddr string `json:"relayAddr"`
	Session   string `json:"session"`
	Clipboard bool   `json:"clipboard"`
}

func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "rmouse", "client-gui.json"), nil
}

// LoadConfig reads the saved config and merges the token from the OS keyring.
// Missing file returns an empty DTO with sensible defaults — never an error.
func (a *App) LoadConfig() (ConfigDTO, error) {
	dto := ConfigDTO{Addr: "127.0.0.1:24242", PingMs: 2000}
	path, err := configPath()
	if err != nil {
		return dto, nil
	}
	if raw, err := os.ReadFile(path); err == nil {
		var p persistedConfig
		if err := json.Unmarshal(raw, &p); err == nil {
			if p.Addr != "" {
				dto.Addr = p.Addr
			}
			dto.Name = p.Name
			if p.PingMs > 0 {
				dto.PingMs = p.PingMs
			}
			dto.RelayAddr = p.RelayAddr
			dto.Session = p.Session
			dto.Clipboard = p.Clipboard
		}
	}
	if tok, err := keyring.Get(keyringService, keyringUser); err == nil {
		dto.Token = tok
	}
	return dto, nil
}

// SaveConfig writes the non-secret fields to a JSON file and the token to
// the OS credential store (Windows Credential Manager / macOS Keychain /
// Linux Secret Service).
func (a *App) SaveConfig(cfg ConfigDTO) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	p := persistedConfig{
		Addr:      cfg.Addr,
		Name:      cfg.Name,
		PingMs:    cfg.PingMs,
		RelayAddr: cfg.RelayAddr,
		Session:   cfg.Session,
		Clipboard: cfg.Clipboard,
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
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

// EnumerateMonitors returns the current layout without starting a session.
// The GUI uses this to render the monitor map before the user connects.
func (a *App) EnumerateMonitors() ([]MonitorDTO, error) {
	mons, err := platform.New().Enumerate()
	if err != nil {
		return nil, err
	}
	return toMonitorDTOs(mons), nil
}

// --- Session control -----------------------------------------------------

// Start kicks off client.Run in a goroutine and streams events to the
// frontend via the "rmouse:event" channel. Subsequent calls while a session
// is active are a no-op — call Stop first to restart with new config.
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
	if cfg.PingMs <= 0 {
		cfg.PingMs = 2000
	}
	if cfg.Name == "" {
		if h, _ := os.Hostname(); h != "" {
			cfg.Name = h
		} else {
			cfg.Name = "client"
		}
	}
	ctx, cancel := context.WithCancel(a.ctx)
	done := make(chan struct{})
	a.cancel = cancel
	a.done = done
	a.mu.Unlock()

	go func() {
		defer close(done)
		cc := client.Config{
			Addr:            cfg.Addr,
			Token:           cfg.Token,
			Name:            cfg.Name,
			PingInterval:    time.Duration(cfg.PingMs) * time.Millisecond,
			RelayAddr:       cfg.RelayAddr,
			Session:         cfg.Session,
			EnableClipboard: cfg.Clipboard,
			OnClipboardItem: func(origin string, format proto.ClipboardFormat, data []byte) {
				a.history.Add(format, data, origin)
			},
		}
		err := client.Run(ctx, cc, func(ev client.Event) {
			a.emitEvent(ev)
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			runtime.EventsEmit(a.ctx, "rmouse:fatal", err.Error())
		}
		a.mu.Lock()
		a.cancel = nil
		a.done = nil
		a.mu.Unlock()
		runtime.EventsEmit(a.ctx, "rmouse:stopped", nil)
	}()
	return nil
}

// Stop cancels the current session and blocks until Run returns. Safe to
// call when nothing is running.
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

// IsRunning lets the frontend recover state after a reload.
func (a *App) IsRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cancel != nil
}

// --- Clipboard history ---------------------------------------------------

// ClipboardHistoryItemDTO is one entry surfaced to the GUI. Small text and
// file-list payloads are inlined; PNG images are sent as base64 so Svelte
// can render them via `src="data:image/png;base64,..."`. The full payload
// stays in the Go-side history and is restored by RestoreClipboardItem.
type ClipboardHistoryItemDTO struct {
	ID         uint64 `json:"id"`
	Kind       string `json:"kind"` // "text" | "image" | "files"
	Origin     string `json:"origin"`
	Timestamp  int64  `json:"timestamp"` // unix millis
	Preview    string `json:"preview"`   // ≤200 chars for text/files
	ImageBase64 string `json:"imageBase64,omitempty"`
	SizeBytes  int    `json:"sizeBytes"`
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
		// Thumbnail would be nicer; inlining full PNG is fine for the
		// 16 MiB cap and ≤30 history items.
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

// GetClipboardHistory returns the history snapshot, newest first.
func (a *App) GetClipboardHistory() []ClipboardHistoryItemDTO {
	snap := a.history.Snapshot()
	out := make([]ClipboardHistoryItemDTO, len(snap))
	for i, it := range snap {
		out[i] = itemToDTO(it)
	}
	return out
}

// RestoreClipboardItem writes the history item with the given id back to
// the OS clipboard. Propagation to peers happens automatically via the
// session's clipboard Watcher if sync is enabled.
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

// ClearClipboardHistory drops every history entry.
func (a *App) ClearClipboardHistory() { a.history.Clear() }

// ShowClipboardHistory is an in-app button fallback when the global hotkey
// is unavailable. Emits the same event the hotkey does.
func (a *App) ShowClipboardHistory() {
	runtime.WindowShow(a.ctx)
	runtime.EventsEmit(a.ctx, "rmouse:clipboardHistoryOpen", nil)
}

// HasInputPermission used to report uinput writability on the old Linux
// backend; the XTest backend needs no privilege grant, so this is now
// always true. Kept on the App because the frontend bindings still call it.
func (a *App) HasInputPermission() bool { return true }

// RequestInputPermission is a no-op for the same reason as
// HasInputPermission. Kept for binding compatibility.
func (a *App) RequestInputPermission() error { return nil }

func (a *App) emitEvent(ev client.Event) {
	switch e := ev.(type) {
	case client.StatusEvent:
		payload := map[string]any{"state": string(e.State)}
		if e.AssignedName != "" {
			payload["assignedName"] = e.AssignedName
		}
		if e.Err != nil {
			payload["err"] = e.Err.Error()
		}
		if e.RetryIn > 0 {
			payload["retryMs"] = e.RetryIn.Milliseconds()
		}
		runtime.EventsEmit(a.ctx, "rmouse:status", payload)
	case client.MonitorsEvent:
		runtime.EventsEmit(a.ctx, "rmouse:monitors", map[string]any{
			"monitors": toMonitorDTOs(e.Monitors),
			"live":     e.Live,
		})
	case client.PongEvent:
		runtime.EventsEmit(a.ctx, "rmouse:pong", map[string]any{"seq": e.Seq})
	case client.HotplugUnavailableEvent:
		msg := ""
		if e.Err != nil {
			msg = e.Err.Error()
		}
		runtime.EventsEmit(a.ctx, "rmouse:hotplugUnavailable", map[string]any{"err": msg})
	case client.InjectorUnavailableEvent:
		msg := ""
		if e.Err != nil {
			msg = e.Err.Error()
		}
		runtime.EventsEmit(a.ctx, "rmouse:injectorUnavailable", map[string]any{"err": msg})
	case client.GrabEvent:
		runtime.EventsEmit(a.ctx, "rmouse:grab", map[string]any{"on": e.On})
	case client.ClipboardUnavailableEvent:
		msg := ""
		if e.Err != nil {
			msg = e.Err.Error()
		}
		runtime.EventsEmit(a.ctx, "rmouse:clipboardUnavailable", map[string]any{"err": msg})
	}
}
