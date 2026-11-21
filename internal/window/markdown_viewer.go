package window

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotkit/app"

	"libdb.so/gluppencode/internal/mdstream"
)

const (
	maxMarkdownPreviewBytes = 2 * 1024 * 1024
	markdownReloadDebounce  = 60
)

type markdownViewer struct {
	win         *adw.Window
	ctx         context.Context
	path        string
	titleWidget *adw.WindowTitle
	liveBadge   *gtk.Label
	banner      *adw.Banner
	scroll      *gtk.ScrolledWindow
	mdView      *mdstream.View

	copyPath func(string)
	openWith func(string)
	openLink func(string)
	onClose  func()

	monitor       *gio.FileMonitor
	debounceTimer glib.SourceHandle
	lastContent   string
	lastTruncated bool
	hasLoaded     bool
	closed        bool
}

func showMarkdownViewer(ctx context.Context, rawPath string) *markdownViewer {
	cleanPath := filepath.Clean(strings.TrimSpace(rawPath))
	if cleanPath == "" || cleanPath == "." {
		return nil
	}
	v := newMarkdownViewer(ctx, cleanPath, nil, nil, nil, nil, nil)
	v.Present()
	return v
}

func newMarkdownViewer(
	ctx context.Context,
	filePath string,
	copyPath func(string),
	openWith func(string),
	openViewer func(string),
	linkHandler func(string) bool,
	onClose func(),
) *markdownViewer {
	if ctx == nil {
		ctx = context.Background()
	}
	cleanPath := filepath.Clean(strings.TrimSpace(filePath))
	if copyPath == nil {
		copyPath = func(p string) {
			if d := gdk.DisplayGetDefault(); d != nil {
				d.Clipboard().SetText(p)
			}
		}
	}
	if openWith == nil {
		openWith = func(p string) {
			openFileWithDefault(ctx, p)
		}
	}

	win := adw.NewWindow()
	baseName := filepath.Base(cleanPath)
	if baseName == "" || baseName == "." {
		baseName = cleanPath
	}
	win.SetTitle(baseName)
	win.SetDefaultSize(860, 720)
	win.AddCSSClass("glup-md-viewer")

	if parent := app.WindowFromContext(ctx); parent != nil {
		if appInst := parent.Application(); appInst != nil && appInst.Application != nil {
			win.SetApplication(appInst.Application)
		}
	}

	toolbar := adw.NewToolbarView()
	header := adw.NewHeaderBar()

	titleWidget := adw.NewWindowTitle(baseName, shortDir(filepath.Dir(cleanPath)))
	header.SetTitleWidget(titleWidget)

	liveBadge := gtk.NewLabel("Live")
	liveBadge.AddCSSClass("glup-mcp-live-badge")
	liveBadge.SetVAlign(gtk.AlignCenter)
	liveBadge.SetTooltipText("Watching file for live changes")
	liveBadge.SetVisible(false)

	reloadBtn := gtk.NewButtonFromIconName("refresh-symbolic")
	reloadBtn.SetTooltipText("Reload from disk")

	copyBtn := gtk.NewButtonFromIconName("edit-copy-symbolic")
	copyBtn.SetTooltipText("Copy file path")

	openWithBtn := gtk.NewButtonFromIconName("open-in-new-symbolic")
	openWithBtn.SetTooltipText("Open With…")

	header.PackEnd(openWithBtn)
	header.PackEnd(copyBtn)
	header.PackEnd(reloadBtn)
	header.PackEnd(liveBadge)
	toolbar.AddTopBar(header)

	bodyBox := gtk.NewBox(gtk.OrientationVertical, 0)

	banner := adw.NewBanner("")
	banner.SetRevealed(false)
	bodyBox.Append(banner)

	scroll := gtk.NewScrolledWindow()
	scroll.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	scroll.SetVExpand(true)
	scroll.AddCSSClass("glup-md-viewer-body")

	clamp := adw.NewClamp()
	clamp.SetMaximumSize(820)
	clamp.SetTighteningThreshold(600)

	contentBox := gtk.NewBox(gtk.OrientationVertical, 0)
	contentBox.AddCSSClass("glup-md-viewer-content")

	v := &markdownViewer{
		win:         win,
		ctx:         ctx,
		path:        cleanPath,
		titleWidget: titleWidget,
		liveBadge:   liveBadge,
		banner:      banner,
		scroll:      scroll,
		copyPath:    copyPath,
		openWith:    openWith,
		openLink:    openViewer,
		onClose:     onClose,
	}

	viewCtx := mdstream.WithLinkHandler(ctx, func(uri string) bool {
		return v.handleLink(uri, linkHandler)
	})
	mdView := mdstream.NewView(viewCtx)
	v.mdView = mdView

	contentBox.Append(mdView)
	clamp.SetChild(contentBox)
	scroll.SetChild(clamp)
	bodyBox.Append(scroll)
	toolbar.SetContent(bodyBox)
	win.SetContent(toolbar)

	reloadBtn.ConnectClicked(func() {
		v.reloadFromDisk(true)
	})
	copyBtn.ConnectClicked(func() {
		if v.copyPath != nil {
			v.copyPath(v.path)
		}
	})
	openWithBtn.ConnectClicked(func() {
		if v.openWith != nil {
			v.openWith(v.path)
		}
	})

	keyCtrl := gtk.NewEventControllerKey()
	keyCtrl.ConnectKeyPressed(func(keyval, _ uint, _ gdk.ModifierType) bool {
		if keyval == gdk.KEY_Escape {
			win.Close()
			return true
		}
		return false
	})
	win.AddController(keyCtrl)

	win.ConnectCloseRequest(func() bool {
		v.cleanup()
		return false
	})

	v.reloadFromDisk(true)
	v.startMonitor()

	return v
}

func (v *markdownViewer) Present() {
	if v == nil || v.win == nil || v.closed {
		return
	}
	v.win.Present()
}

func (v *markdownViewer) Close() {
	if v == nil || v.closed {
		return
	}
	if v.win != nil {
		v.win.Close()
		return
	}
	v.cleanup()
}

func (v *markdownViewer) cleanup() {
	if v == nil || v.closed {
		return
	}
	v.closed = true
	if v.debounceTimer != 0 {
		glib.SourceRemove(v.debounceTimer)
		v.debounceTimer = 0
	}
	if v.monitor != nil {
		v.monitor.Cancel()
		v.monitor = nil
	}
	if v.mdView != nil {
		v.mdView.Close()
	}
	if v.onClose != nil {
		cb := v.onClose
		v.onClose = nil
		cb()
	}
}

func (v *markdownViewer) startMonitor() {
	if v == nil || v.closed || v.path == "" {
		return
	}
	gfile := gio.NewFileForPath(v.path)
	mon, err := gfile.MonitorFile(v.ctx, gio.FileMonitorNone)
	if err != nil {
		return
	}
	fm := gio.BaseFileMonitor(mon)
	if fm == nil {
		return
	}
	v.monitor = fm
	v.liveBadge.SetVisible(true)
	fm.ConnectChanged(func(_, _ gio.Filer, ev gio.FileMonitorEvent) {
		switch ev {
		case gio.FileMonitorEventChanged,
			gio.FileMonitorEventChangesDoneHint,
			gio.FileMonitorEventCreated,
			gio.FileMonitorEventDeleted,
			gio.FileMonitorEventMovedIn,
			gio.FileMonitorEventRenamed:
			v.scheduleReload()
		}
	})
}

func (v *markdownViewer) scheduleReload() {
	if v == nil || v.closed {
		return
	}
	if v.debounceTimer != 0 {
		glib.SourceRemove(v.debounceTimer)
		v.debounceTimer = 0
	}
	v.debounceTimer = glib.TimeoutAdd(markdownReloadDebounce, func() bool {
		v.debounceTimer = 0
		if !v.closed {
			v.reloadFromDisk(false)
		}
		return false
	})
}

func (v *markdownViewer) reloadFromDisk(force bool) {
	if v == nil || v.closed {
		return
	}
	content, truncated, err := readMarkdownPreviewFile(v.path, maxMarkdownPreviewBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if v.hasLoaded {
				v.banner.SetTitle("File was removed from disk — watching for changes")
			} else {
				v.banner.SetTitle("File does not exist on disk yet — watching for creation")
			}
		} else {
			v.banner.SetTitle(fmt.Sprintf("Could not read file: %v", err))
		}
		v.banner.SetRevealed(true)
		return
	}

	if truncated {
		v.banner.SetTitle("Preview truncated at 2 MiB")
		v.banner.SetRevealed(true)
	} else {
		v.banner.SetRevealed(false)
	}

	if !force && v.hasLoaded && content == v.lastContent && truncated == v.lastTruncated {
		return
	}

	var prevVal, prevUpper, prevPage float64
	var hasScroll bool
	if v.scroll != nil {
		if vadj := v.scroll.VAdjustment(); vadj != nil {
			prevVal = vadj.Value()
			prevUpper = vadj.Upper()
			prevPage = vadj.PageSize()
			hasScroll = true
		}
	}
	wasAtBottom := hasScroll && prevVal > 0 && prevUpper > prevPage && prevVal >= prevUpper-prevPage-24

	v.lastContent = content
	v.lastTruncated = truncated
	v.hasLoaded = true
	v.mdView.SetStatic(content)

	if hasScroll {
		glib.IdleAdd(func() {
			if v.closed || v.scroll == nil {
				return
			}
			vadj := v.scroll.VAdjustment()
			if vadj == nil {
				return
			}
			maxVal := max(0, vadj.Upper()-vadj.PageSize())
			if wasAtBottom {
				vadj.SetValue(maxVal)
			} else {
				vadj.SetValue(min(prevVal, maxVal))
			}
		})
	}
}

func (v *markdownViewer) handleLink(uri string, fallback func(string) bool) bool {
	if strings.HasPrefix(uri, mdstream.SessionLinkPrefix) {
		if fallback != nil {
			return fallback(uri)
		}
		return true
	}
	if hasURIScheme(uri) {
		if fallback != nil && fallback(uri) {
			return true
		}
		return false
	}

	rawTarget := uri
	if idx := strings.IndexAny(rawTarget, "#?"); idx != -1 {
		rawTarget = rawTarget[:idx]
	}
	rawTarget = strings.TrimSpace(rawTarget)
	if rawTarget == "" {
		return true
	}
	target := rawTarget
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(v.path), target)
	}
	target = filepath.Clean(target)

	info, err := os.Stat(target)
	if err != nil || info.IsDir() {
		return false
	}
	if isMarkdownPath(target) {
		if v.openLink != nil {
			v.openLink(target)
		} else {
			showMarkdownViewer(v.ctx, target)
		}
		return true
	}
	app.OpenURI(v.ctx, gio.NewFileForPath(target).URI())
	return true
}

func hasURIScheme(uri string) bool {
	return strings.Contains(uri, "://") ||
		strings.HasPrefix(uri, "mailto:") ||
		strings.HasPrefix(uri, "tel:")
}

func readMarkdownPreviewFile(path string, maxBytes int64) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()

	limited := io.LimitReader(f, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return "", false, err
	}
	truncated := int64(len(data)) > maxBytes
	if truncated {
		data = data[:maxBytes]
	}
	return strings.ToValidUTF8(string(data), "\uFFFD"), truncated, nil
}

func openFileWithDefault(ctx context.Context, rawPath string) {
	path := strings.TrimSpace(rawPath)
	if path == "" {
		return
	}
	var parent *gtk.Window
	if p := app.WindowFromContext(ctx); p != nil {
		parent = &p.Window
	}
	launcher := gtk.NewFileLauncher(gio.NewFileForPath(path))
	launcher.SetAlwaysAsk(true)
	launcher.Launch(ctx, parent, func(res gio.AsyncResulter) {
		_ = launcher.LaunchFinish(res)
	})
}
