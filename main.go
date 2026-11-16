// Gluppencode is a GTK4 / libadwaita desktop client for OpenCode.
package main

import (
	"context"
	"os"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4-sourceview/pkg/gtksource/v5"
	"github.com/diamondburned/gotkit/app"
	"libdb.so/gluppencode/internal/opencode"
	"libdb.so/gluppencode/internal/window"

	_ "github.com/diamondburned/gotkit/gtkutil/aggressivegc"
	_ "libdb.so/gluppencode/internal/icons"
)

var appID = "so.libdb.gluppencode"

func main() {
	adw.Init()
	gtksource.Init()

	a := app.New(context.Background(), appID, "Gluppencode")
	a.SetResourceBasePath("/so/libdb/gluppencode")
	window.RegisterAppActions(a)
	a.ConnectActivate(func() {
		ctx := a.Context()
		client, err := opencode.Discover(ctx)
		if err != nil {
			app.Error(ctx, err)
			return
		}
		win := window.NewWindow(ctx, client)
		win.Present()
	})

	os.Exit(a.Run(os.Args))
}
