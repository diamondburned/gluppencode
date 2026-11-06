package window

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotkit/app"
)

// decodeBase64Texture decodes a base64-encoded image (with or without data URI prefix)
// into a GdkTexture.
func decodeBase64Texture(data string) (*gdk.Texture, error) {
	if idx := strings.Index(data, ","); idx != -1 && strings.HasPrefix(data, "data:") {
		data = data[idx+1:]
	}
	data = strings.TrimSpace(data)
	rawBytes, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		rawBytes, err = base64.RawStdEncoding.DecodeString(data)
		if err != nil {
			return nil, fmt.Errorf("decode base64 image: %w", err)
		}
	}
	bytes := glib.NewBytesWithGo(rawBytes)
	return gdk.NewTextureFromBytes(bytes)
}

// newImageThumbnail creates a clickable thumbnail widget that opens the lightbox image viewer.
func newImageThumbnail(ctx context.Context, title string, tex *gdk.Texture) gtk.Widgetter {
	pic := gtk.NewPictureForPaintable(tex)
	pic.SetCanShrink(true)
	pic.SetContentFit(gtk.ContentFitCover)
	pic.SetSizeRequest(84, 84)

	hClamp := adw.NewClamp()
	hClamp.SetOrientation(gtk.OrientationHorizontal)
	hClamp.SetMaximumSize(84)
	hClamp.SetTighteningThreshold(84)
	hClamp.SetChild(pic)

	vClamp := adw.NewClamp()
	vClamp.SetOrientation(gtk.OrientationVertical)
	vClamp.SetMaximumSize(84)
	vClamp.SetTighteningThreshold(84)
	vClamp.SetChild(hClamp)

	btn := gtk.NewButton()
	btn.AddCSSClass("flat")
	btn.AddCSSClass("glup-image-thumb")
	btn.SetHAlign(gtk.AlignStart)
	btn.SetVAlign(gtk.AlignStart)
	btn.SetChild(vClamp)
	btn.SetTooltipText(fmt.Sprintf("%s (%dx%d) — click to zoom", title, tex.Width(), tex.Height()))

	btn.ConnectClicked(func() {
		showImageViewer(ctx, title, tex)
	})

	return btn
}

// showImageViewer opens a modal Lightbox Image Viewer window for inspecting full-resolution images.
func showImageViewer(ctx context.Context, title string, tex *gdk.Texture) {
	viewerWin := adw.NewWindow()
	viewerWin.SetTitle(fmt.Sprintf("%s (%dx%d)", title, tex.Width(), tex.Height()))
	viewerWin.SetDefaultSize(960, 720)
	viewerWin.AddCSSClass("glup-image-viewer")

	if parent := app.WindowFromContext(ctx); parent != nil {
		viewerWin.SetTransientFor(&parent.Window)
		viewerWin.SetModal(true)
	}

	toolbar := adw.NewToolbarView()
	header := adw.NewHeaderBar()

	windowTitle := adw.NewWindowTitle(title, fmt.Sprintf("%dx%d px", tex.Width(), tex.Height()))
	header.SetTitleWidget(windowTitle)

	fitBtn := gtk.NewToggleButton()
	fitBtn.SetLabel("1:1")
	fitBtn.SetTooltipText("Toggle 1:1 / Fit to window")

	copyBtn := gtk.NewButtonFromIconName("edit-copy-symbolic")
	copyBtn.SetTooltipText("Copy Image to Clipboard")
	copyBtn.ConnectClicked(func() {
		display := gdk.DisplayGetDefault()
		if display != nil {
			display.Clipboard().SetTexture(tex)
		}
	})

	header.PackEnd(copyBtn)
	header.PackEnd(fitBtn)
	toolbar.AddTopBar(header)

	scroll := gtk.NewScrolledWindow()
	scroll.AddCSSClass("glup-image-viewer-body")

	pic := gtk.NewPictureForPaintable(tex)
	pic.SetCanShrink(true)
	pic.SetContentFit(gtk.ContentFitScaleDown)

	fitBtn.ConnectToggled(func() {
		if fitBtn.Active() {
			pic.SetCanShrink(false)
			pic.SetContentFit(gtk.ContentFitFill)
		} else {
			pic.SetCanShrink(true)
			pic.SetContentFit(gtk.ContentFitScaleDown)
		}
	})

	scroll.SetChild(pic)
	toolbar.SetContent(scroll)
	viewerWin.SetContent(toolbar)

	keyCtrl := gtk.NewEventControllerKey()
	keyCtrl.ConnectKeyPressed(func(keyval, _ uint, _ gdk.ModifierType) bool {
		if keyval == gdk.KEY_Escape {
			viewerWin.Close()
			return true
		}
		return false
	})
	viewerWin.AddController(keyCtrl)

	viewerWin.Present()
}
