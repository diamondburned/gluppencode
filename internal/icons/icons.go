// Package icons embeds and registers GResource assets for gluppencode.
package icons

import (
	_ "embed"
	"log"

	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
)

//go:generate glib-compile-resources gluppencode.gresource.xml

// Resources contains the compiled GResource binary data.
//
//go:embed gluppencode.gresource
var Resources []byte

func init() {
	resources, err := gio.NewResourceFromData(glib.NewBytesWithGo(Resources))
	if err != nil {
		log.Panicf("failed to create resources: %v", err)
	}
	gio.ResourcesRegister(resources)
}
