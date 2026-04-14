// mouse-remote-client-gui is the Wails-based desktop UI for the rmouse
// client. It reuses internal/app/client for the actual run-loop and only
// adds window chrome + frontend bindings.
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
		Title:     "rmouse client",
		Width:     900,
		Height:    640,
		MinWidth:  640,
		MinHeight: 480,
		// Transparent background lets the native acrylic/mica/vibrancy come
		// through behind the web content.
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
			ProgramName: "rmouse-client",
		},
	})
	if err != nil {
		println("Error:", err.Error())
	}
}
