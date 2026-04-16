// mouse-remote-server-gui is the Wails-based desktop UI for the rmouse
// host. It reuses internal/app/server for the actual listen loop.
package main

import (
	"embed"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// Wails on Windows is a GUI subsystem app — no console attached, so
	// stderr is /dev/null. Tee slog to a file in TempDir so the router's
	// per-event coords are inspectable; tail with `Get-Content -Wait` or
	// `tail -f` on the printed path.
	logPath := filepath.Join(os.TempDir(), "rmouse-server.log")
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})))
		slog.Info("rmouse-server-gui boot", "logPath", logPath)
	}

	app := NewApp()

	err := wails.Run(&options.App{
		Title:            "rmouse server",
		Width:            900,
		Height:           900,
		MinWidth:         900,
		MinHeight:        900,
		MaxWidth:         900,
		MaxHeight:        900,
		DisableResize:    true,
		BackgroundColour: &options.RGBA{R: 0, G: 0, B: 0, A: 0},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		AssetServer:      &assetserver.Options{Assets: assets},
		Bind:             []interface{}{app},
		Windows: &windows.Options{
			BackdropType:         windows.Mica,
			WebviewIsTransparent: true,
			WindowIsTranslucent:  true,
			Theme:                windows.SystemDefault,
		},
		Mac: &mac.Options{
			TitleBar:             mac.TitleBarHiddenInset(),
			WebviewIsTransparent: true,
			WindowIsTranslucent:  true,
		},
		Linux: &linux.Options{
			ProgramName: "rmouse-server",
		},
	})
	if err != nil {
		println("Error:", err.Error())
	}
}
