package main

import (
	"context"
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
}

func NewApp() *App { return &App{} }

func (a *App) startup(ctx context.Context) { a.ctx = ctx }

func (a *App) shutdown(_ context.Context) { _ = a.Stop() }

// --- DTOs ----------------------------------------------------------------

type ConfigDTO struct {
	Addr      string `json:"addr"`
	Token     string `json:"token"`
	Name      string `json:"name"`
	PingMs    int    `json:"pingMs"`
	RelayAddr string `json:"relayAddr"`
	Session   string `json:"session"`
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
			Addr:         cfg.Addr,
			Token:        cfg.Token,
			Name:         cfg.Name,
			PingInterval: time.Duration(cfg.PingMs) * time.Millisecond,
			RelayAddr:    cfg.RelayAddr,
			Session:      cfg.Session,
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
	}
}
