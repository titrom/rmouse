// mouse-remote-server-gui is the Wails-based desktop UI for the rmouse
// host. It reuses internal/app/server for the actual listen loop.
package main

import (
	"embed"

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
