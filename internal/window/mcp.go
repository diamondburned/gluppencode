package window

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"github.com/diamondburned/gotkit/gtkutil"
	"libdb.so/gluppencode/internal/opencode"
)

const (
	mcpActionTimeout  = 60 * time.Second
	mcpFetchTimeout   = 15 * time.Second
	mcpMaxLogEntries  = 1000
	mcpLogPollMS      = 500
	mcpBottomFollowPx = 24
)

func mcpFlightKey(dir, serverName string) string {
	return normalizeProjectDir(dir) + "\x00" + serverName
}

func (w *Window) ensureMcpMaps() {
	if w.mcpByDir == nil {
		w.mcpByDir = make(map[string][]opencode.McpServer)
	}
	if w.mcpLoadedDirs == nil {
		w.mcpLoadedDirs = make(map[string]bool)
	}
	if w.mcpFetchingDirs == nil {
		w.mcpFetchingDirs = make(map[string]bool)
	}
	if w.mcpInFlight == nil {
		w.mcpInFlight = make(map[string]bool)
	}
}

func (w *Window) effectiveMcpDirKey() string {
	if d := normalizeProjectDir(w.activeDirectory()); d != "" {
		return d
	}
	return w.defaultServerDir
}

func (w *Window) ensureMcpStatusLoaded(rawDir string) {
	if w.client == nil || w.ctx == nil || w.ctx.Err() != nil {
		return
	}
	w.ensureMcpMaps()

	key := normalizeProjectDir(rawDir)
	if key == "" && w.defaultServerDir != "" {
		key = w.defaultServerDir
	}
	if w.mcpLoadedDirs[key] || w.mcpFetchingDirs[key] {
		return
	}
	if rawDir == "" && w.mcpFetchingDirs[""] {
		return
	}

	w.mcpFetchingDirs[key] = true
	if rawDir == "" {
		w.mcpFetchingDirs[""] = true
	}

	client := w.client
	ctx := w.ctx
	gtkutil.Async(ctx, func() func() {
		reqCtx, cancel := context.WithTimeout(ctx, mcpFetchTimeout)
		defer cancel()
		res, err := client.ListMCP(reqCtx, rawDir)
		return func() {
			w.ensureMcpMaps()
			delete(w.mcpFetchingDirs, key)
			if rawDir == "" {
				delete(w.mcpFetchingDirs, "")
			}
			if err != nil || res == nil {
				return
			}
			resolvedKey := normalizeProjectDir(res.Location.Directory)
			if resolvedKey == "" {
				resolvedKey = key
			}
			if rawDir == "" && resolvedKey != "" {
				w.defaultServerDir = resolvedKey
			}
			w.mcpByDir[resolvedKey] = res.Servers
			w.mcpLoadedDirs[resolvedKey] = true
			if key != "" && key != resolvedKey {
				w.mcpLoadedDirs[key] = true
			}
			w.refreshMcpStatusChip()
			if mgr := w.mcpMgr; mgr != nil && !mgr.isDisposed() && normalizeProjectDir(mgr.dir) == resolvedKey {
				if res.Location.Directory != "" {
					mgr.setResolvedDir(res.Location.Directory)
				}
				mgr.applyServers(res.Servers, "")
			}
		}
	})
}

func (w *Window) refreshMcpForDir(rawDir string, includeMeta bool) {
	if w.client == nil || w.ctx == nil || w.ctx.Err() != nil {
		return
	}
	w.ensureMcpMaps()

	client := w.client
	ctx := w.ctx
	gtkutil.Async(ctx, func() func() {
		reqCtx, cancel := context.WithTimeout(ctx, mcpFetchTimeout)
		defer cancel()

		var (
			mcpRes *opencode.McpListResult
			mcpErr error
			resCat *opencode.McpResourceListResult
			resErr error
			wg     sync.WaitGroup
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			mcpRes, mcpErr = client.ListMCP(reqCtx, rawDir)
		}()
		if includeMeta {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resCat, resErr = client.ListMCPResources(reqCtx, rawDir)
			}()
		}
		wg.Wait()

		return func() {
			if mcpErr != nil || mcpRes == nil {
				return
			}
			w.ensureMcpMaps()
			resolvedKey := normalizeProjectDir(mcpRes.Location.Directory)
			if resolvedKey == "" {
				resolvedKey = normalizeProjectDir(rawDir)
			}
			if resolvedKey == "" {
				resolvedKey = w.defaultServerDir
			}
			if rawDir == "" && resolvedKey != "" {
				w.defaultServerDir = resolvedKey
			}
			w.mcpByDir[resolvedKey] = mcpRes.Servers
			w.mcpLoadedDirs[resolvedKey] = true
			w.refreshMcpStatusChip()

			if mgr := w.mcpMgr; mgr != nil && !mgr.isDisposed() && normalizeProjectDir(mgr.dir) == resolvedKey {
				if mcpRes.Location.Directory != "" {
					mgr.setResolvedDir(mcpRes.Location.Directory)
				}
				if includeMeta && resErr == nil && resCat != nil {
					mgr.resources = resCat.Catalog
				}
				mgr.applyServers(mcpRes.Servers, "")
			}
		}
	})
}

func (w *Window) refreshMcpStatusChip() {
	if w.mcpStatusBox == nil || w.mcpStatusLabel == nil {
		return
	}
	servers := w.mcpByDir[w.effectiveMcpDirKey()]
	if len(servers) == 0 {
		w.mcpStatusBox.SetVisible(false)
		return
	}

	var connected, pending, disabled, failed, needsAuth int
	sorted := slices.Clone(servers)
	slices.SortFunc(sorted, compareMcpServers)
	tips := make([]string, 0, len(sorted))
	for _, s := range sorted {
		switch s.Status.Status {
		case "connected":
			connected++
		case "pending":
			pending++
		case "disabled":
			disabled++
		case "failed":
			failed++
		case "needs_auth":
			needsAuth++
		}
		tips = append(tips, fmt.Sprintf("%s: %s", s.Name, mcpStatusLabel(s.Status.Status)))
	}

	for _, cls := range []string{
		"glup-status-mcp-ok",
		"glup-status-mcp-warn",
		"glup-status-mcp-err",
		"glup-status-mcp-dim",
	} {
		w.mcpStatusBox.RemoveCSSClass(cls)
	}

	total := len(servers)
	unit := "MCP"
	if total > 1 {
		unit = "MCPs"
	}
	var label, cls string
	switch {
	case failed > 0:
		label = fmt.Sprintf("%d/%d %s · %d err", connected, total, unit, failed)
		cls = "glup-status-mcp-err"
	case needsAuth > 0:
		label = fmt.Sprintf("%d/%d %s · auth", connected, total, unit)
		cls = "glup-status-mcp-warn"
	case connected == total:
		label = fmt.Sprintf("%d %s", total, unit)
		cls = "glup-status-mcp-ok"
	case disabled == total:
		label = fmt.Sprintf("0/%d %s", total, unit)
		cls = "glup-status-mcp-dim"
	default:
		label = fmt.Sprintf("%d/%d %s", connected, total, unit)
	}

	if cls != "" {
		w.mcpStatusBox.AddCSSClass(cls)
	}
	w.mcpStatusLabel.SetText(label)
	w.mcpStatusBox.SetTooltipText("MCP Servers (Ctrl+Shift+M)\n" + strings.Join(tips, "\n"))
	w.mcpStatusBox.SetVisible(true)
}

func (w *Window) isBottomSheetOpen() bool {
	return w.bottomSheet != nil && w.bottomSheet.Open()
}

func (w *Window) handleMcpToggleKey(keyval uint, state gdk.ModifierType) bool {
	if (keyval == gdk.KEY_M || keyval == gdk.KEY_m) && state.Has(gdk.ControlMask) && state.Has(gdk.ShiftMask) {
		if w.mcpMgr != nil && w.mcpMgr.authDlg != nil && !w.mcpMgr.authDlg.closed {
			return false
		}
		w.toggleMcpSheet()
		return true
	}
	return false
}

func (w *Window) handleBottomSheetEscape() bool {
	if w.mcpMgr != nil && w.mcpMgr.authDlg != nil && !w.mcpMgr.authDlg.closed {
		return false
	}
	if w.bottomSheet == nil || !w.bottomSheet.Open() {
		return false
	}
	if w.mcpMgr != nil && w.mcpMgr.detailPage != nil && w.sheetNavView != nil {
		w.sheetNavView.Pop()
		return true
	}
	w.bottomSheet.SetOpen(false)
	return true
}

func (w *Window) isFocusInsideSheet() bool {
	if w.ApplicationWindow == nil || w.sheetToastOverlay == nil {
		return false
	}
	cur := w.ApplicationWindow.Focus()
	if cur == nil {
		return false
	}
	curW := gtk.BaseWidget(cur)
	return curW.Object.Native() == w.sheetToastOverlay.Object.Native() || curW.IsAncestor(w.sheetToastOverlay)
}

func (w *Window) focusActiveSheetPage() {
	if !w.isBottomSheetOpen() {
		return
	}
	if w.mcpMgr != nil {
		if w.mcpMgr.detailPage != nil {
			w.mcpMgr.focusDetailPage()
		} else {
			w.mcpMgr.focusOverviewPage()
		}
		return
	}
	if w.sheetOverviewPage != nil {
		w.sheetOverviewPage.ChildFocus(gtk.DirTabForward)
	}
}

func (w *Window) openBottomSheet(explicitMcp bool, initialServer string) {
	if w.bottomSheet == nil {
		return
	}
	w.rebuildShellList()
	w.ensureShellOutputPoller()
	if w.mcpMgr != nil {
		w.mcpMgr.prepareForOpen(explicitMcp, initialServer)
	}
	if !w.bottomSheet.Open() {
		if w.mcpMgr != nil {
			w.mcpMgr.preparingOpen = true
		}
		w.bottomSheet.SetOpen(true)
		if w.mcpMgr != nil {
			w.mcpMgr.preparingOpen = false
		}
	}
	w.focusActiveSheetPage()
	glib.IdleAdd(func() {
		if w.ctx == nil || w.ctx.Err() != nil || !w.isBottomSheetOpen() {
			return
		}
		if !w.isFocusInsideSheet() {
			w.focusActiveSheetPage()
		}
	})
}

func (w *Window) openMcpSheet(initialServer string) {
	w.openBottomSheet(true, initialServer)
}

func (w *Window) toggleMcpSheet() {
	if w.bottomSheet == nil {
		return
	}
	if w.bottomSheet.Open() {
		if w.mcpMgr != nil && w.mcpSectionBox != nil && !w.mcpSectionBox.Visible() && w.mcpMgr.detailPage == nil {
			w.openMcpSheet("")
			return
		}
		w.bottomSheet.SetOpen(false)
		return
	}
	w.openMcpSheet("")
}

type mcpServerRow struct {
	name         string
	actionRow    *adw.ActionRow
	prefixStack  *gtk.Stack
	dot          *gtk.Box
	spinner      *adw.Spinner
	restartBtn   *gtk.Button
	toggleSwitch *gtk.Switch
	syncingRow   bool
}

type displayedLogEntry struct {
	lineCount int
}

type mcpDetailView struct {
	mgr              *mcpManager
	serverName       string
	page             *adw.NavigationPage
	headerTitle      *adw.WindowTitle
	refreshBtn       *gtk.Button
	statusRow        *adw.ActionRow
	statusPrefix     *gtk.Stack
	statusDot        *gtk.Box
	statusSpinner    *adw.Spinner
	copyErrBtn       *gtk.Button
	loginBtn         *gtk.Button
	startBtn         *gtk.Button
	restartBtn       *gtk.Button
	restartLbl       *gtk.Label
	stopBtn          *gtk.Button
	configExpander   *adw.ExpanderRow
	configRows       []*adw.ActionRow
	resourceExpander *adw.ExpanderRow
	resourceRows     []*adw.ActionRow

	liveBadge           *gtk.Label
	outerScroll         *gtk.ScrolledWindow
	levelDrop           *gtk.DropDown
	logSearch           *gtk.SearchEntry
	followBtn           *gtk.ToggleButton
	clearBtn            *gtk.Button
	copyBtn             *gtk.Button
	bodyStack           *gtk.Stack
	scrolled            *gtk.ScrolledWindow
	vadj                *gtk.Adjustment
	textView            *gtk.TextView
	logBuf              *gtk.TextBuffer
	emptyLabel          *gtk.Label
	tagDim              *gtk.TextTag
	tagDebug            *gtk.TextTag
	tagInfo             *gtk.TextTag
	tagWarn             *gtk.TextTag
	tagError            *gtk.TextTag
	tagMsg              *gtk.TextTag
	tagServer           *gtk.TextTag
	tagExtra            *gtk.TextTag
	rawEntries          []opencode.McpLogEntry
	displayedEntries    []displayedLogEntry
	suppressFollowCheck bool
	programmaticScroll  bool
	isRemoteServer      bool
	lastStatusError     string
}

type mcpManager struct {
	w                 *Window
	toastOverlay      *adw.ToastOverlay
	navView           *adw.NavigationView
	listPage          *adw.NavigationPage
	sectionBox        *gtk.Box
	dirLabel          *gtk.Label
	allLogsBtn        *gtk.Button
	refreshBtn        *gtk.Button
	errBanner         *adw.Banner
	searchEntry       *gtk.SearchEntry
	listStack         *gtk.Stack
	listBox           *gtk.ListBox
	dir               string
	servers           []opencode.McpServer
	configs           map[string]opencode.McpServerConfig
	resources         opencode.McpResourceCatalog
	rows              map[string]*mcpServerRow
	rowOrder          []string
	pendingOpenServer string
	hasLoadedOnce     bool
	explicitOpen      bool
	sheetOpen         bool
	preparingOpen     bool
	resettingNav      bool
	refreshGen        uint64

	detail          *mcpDetailView
	detailPage      *adw.NavigationPage
	authDlg         *mcpOAuthDialog
	tailer          *opencode.McpLogTailer
	logPollGen      uint64
	logPollActive   bool
	logPollInFlight bool
}

func (w *Window) initMcpSheet() {
	if w.bottomSheet == nil || w.sheetNavView == nil || w.sheetOverviewPage == nil {
		return
	}
	w.ensureMcpMaps()

	m := &mcpManager{
		w:            w,
		toastOverlay: w.sheetToastOverlay,
		navView:      w.sheetNavView,
		listPage:     w.sheetOverviewPage,
		sectionBox:   w.mcpSectionBox,
		dirLabel:     w.mcpDirLabel,
		allLogsBtn:   w.mcpAllLogsBtn,
		refreshBtn:   w.mcpRefreshBtn,
		errBanner:    w.mcpErrBanner,
		searchEntry:  w.mcpSearchEntry,
		listStack:    w.mcpListStack,
		listBox:      w.mcpListBox,
		rows:         make(map[string]*mcpServerRow),
	}
	w.mcpMgr = m

	if m.allLogsBtn != nil {
		m.allLogsBtn.ConnectClicked(func() {
			m.openDetailPage("")
		})
	}
	if m.refreshBtn != nil {
		m.refreshBtn.ConnectClicked(func() {
			m.refreshAll()
		})
	}
	if m.errBanner != nil {
		m.errBanner.ConnectButtonClicked(func() {
			m.refreshAll()
		})
	}
	if m.searchEntry != nil {
		m.searchEntry.ConnectSearchChanged(func() {
			m.syncServerList()
		})
		m.searchEntry.ConnectStopSearch(func() {
			if m.searchEntry.Text() != "" {
				m.searchEntry.SetText("")
				return
			}
			w.handleBottomSheetEscape()
		})
	}

	shortcutTarget := gtk.Widgetter(m.toastOverlay)
	if w.ApplicationWindow != nil {
		shortcutTarget = w.ApplicationWindow
	}
	if shortcutTarget != nil {
		sheetKeyCtrl := gtk.NewEventControllerKey()
		sheetKeyCtrl.SetPropagationPhase(gtk.PhaseCapture)
		sheetKeyCtrl.ConnectKeyPressed(func(keyval, _ uint, state gdk.ModifierType) bool {
			return w.handleMcpToggleKey(keyval, state)
		})
		gtk.BaseWidget(shortcutTarget).AddController(sheetKeyCtrl)
	}

	m.navView.ConnectPopped(func(page *adw.NavigationPage) {
		if page != nil && m.detailPage != nil && page.Object.Native() == m.detailPage.Object.Native() {
			m.stopLogPoller()
			m.detail = nil
			m.detailPage = nil
			if !m.resettingNav {
				m.focusOverviewPage()
			}
		}
	})

	w.bottomSheet.NotifyProperty("open", func() {
		if w.bottomSheet.Open() {
			if !m.preparingOpen {
				w.rebuildShellList()
				w.ensureShellOutputPoller()
				m.prepareForOpen(false, "")
				glib.IdleAdd(func() {
					if w.ctx == nil || w.ctx.Err() != nil || !w.isBottomSheetOpen() {
						return
					}
					if !w.isFocusInsideSheet() {
						w.focusActiveSheetPage()
					}
				})
			}
			return
		}
		m.onSheetClosed()
	})
}

func (m *mcpManager) isDisposed() bool {
	return m == nil || m.w == nil || m.w.ctx == nil || m.w.ctx.Err() != nil
}

func (m *mcpManager) isOpen() bool {
	return !m.isDisposed() && m.sheetOpen
}

func (m *mcpManager) prepareForOpen(explicitMcp bool, initialServer string) {
	if m.isDisposed() {
		return
	}
	initialServer = strings.TrimSpace(initialServer)
	m.sheetOpen = true
	if explicitMcp {
		m.explicitOpen = true
	}

	targetDir := m.w.activeDirectory()
	if targetDir == "" && m.w.defaultServerDir != "" {
		targetDir = m.w.defaultServerDir
	}
	targetKey := normalizeProjectDir(targetDir)
	curKey := normalizeProjectDir(m.dir)

	if targetDir != "" && targetKey != curKey {
		m.resetForDirectory(targetDir)
	} else if m.dir == "" && targetDir != "" {
		m.setResolvedDir(targetDir)
	}

	cachedKey := normalizeProjectDir(m.dir)
	if cached, ok := m.w.mcpByDir[cachedKey]; ok {
		m.hasLoadedOnce = true
		m.servers = slices.Clone(cached)
		m.syncViews()
	} else {
		m.syncViews()
	}

	if initialServer != "" {
		if m.hasLoadedOnce {
			if !m.tryNavigateCached(initialServer) {
				m.pendingOpenServer = initialServer
			}
		} else {
			m.pendingOpenServer = initialServer
		}
	} else if m.navView != nil && m.listPage != nil && m.detailPage != nil {
		m.stopLogPoller()
		m.detail = nil
		m.detailPage = nil
		m.resettingNav = true
		m.navView.PopToPage(m.listPage)
		m.resettingNav = false
	}

	m.refreshAll()
}

func (m *mcpManager) onSheetClosed() {
	if m == nil {
		return
	}
	m.sheetOpen = false
	m.explicitOpen = false
	m.pendingOpenServer = ""
	m.stopLogPoller()
	m.detail = nil
	m.detailPage = nil
	if m.navView != nil && m.listPage != nil {
		m.resettingNav = true
		m.navView.PopToPage(m.listPage)
		m.resettingNav = false
	}
	m.syncMcpSectionVisibility()
}

func (m *mcpManager) resetForDirectory(newDir string) {
	m.refreshGen++
	m.stopLogPoller()
	m.detail = nil
	m.detailPage = nil
	if m.navView != nil && m.listPage != nil {
		m.resettingNav = true
		m.navView.PopToPage(m.listPage)
		m.resettingNav = false
	}
	m.clearListBoxRows()
	m.servers = nil
	m.configs = nil
	m.resources = opencode.McpResourceCatalog{}
	m.hasLoadedOnce = false
	m.pendingOpenServer = ""
	if m.errBanner != nil {
		m.errBanner.SetRevealed(false)
	}
	if m.searchEntry != nil {
		m.searchEntry.SetText("")
	}
	m.setResolvedDir(newDir)
}

func (m *mcpManager) syncDirectory(newDir string) {
	if m.isDisposed() {
		return
	}
	if newDir == "" {
		newDir = m.w.defaultServerDir
	}
	if newDir == "" {
		return
	}
	if normalizeProjectDir(newDir) == normalizeProjectDir(m.dir) {
		return
	}
	m.resetForDirectory(newDir)
	if cached, ok := m.w.mcpByDir[normalizeProjectDir(newDir)]; ok {
		m.hasLoadedOnce = true
		m.servers = slices.Clone(cached)
	}
	m.syncViews()
	if m.isOpen() {
		m.refreshAll()
	}
}

func (m *mcpManager) focusOverviewPage() {
	if !m.isOpen() {
		return
	}
	if m.sectionBox != nil && m.sectionBox.Visible() && m.allLogsBtn != nil {
		m.allLogsBtn.GrabFocus()
		return
	}
	if m.listPage != nil {
		m.listPage.ChildFocus(gtk.DirTabForward)
	}
}

func (m *mcpManager) focusDetailPage() {
	if !m.isOpen() {
		return
	}
	if m.detail != nil && m.detail.refreshBtn != nil {
		m.detail.refreshBtn.GrabFocus()
		return
	}
	if m.detailPage != nil {
		m.detailPage.ChildFocus(gtk.DirTabForward)
	}
}

func (m *mcpManager) setResolvedDir(dir string) {
	if dir == "" {
		return
	}
	m.dir = dir
	short := shortDir(dir)
	if m.dirLabel != nil {
		m.dirLabel.SetText(short)
		m.dirLabel.SetTooltipText(dir)
	}
	if m.detail != nil && m.detail.headerTitle != nil {
		m.detail.headerTitle.SetSubtitle(short)
	}
}

func (m *mcpManager) showToast(msg string) {
	if !m.isOpen() || m.toastOverlay == nil || strings.TrimSpace(msg) == "" {
		return
	}
	toast := adw.NewToast(msg)
	toast.SetUseMarkup(false)
	toast.SetTimeout(4)
	m.toastOverlay.AddToast(toast)
}

func (m *mcpManager) tryNavigateCached(query string) bool {
	query = strings.TrimSpace(query)
	if query == "" {
		return true
	}
	if match, ok := findMatchingMcpServer(m.servers, query); ok {
		m.openDetailPage(match)
		return true
	}
	if strings.EqualFold(query, "logs") {
		m.openDetailPage("")
		return true
	}
	return false
}

func (m *mcpManager) navigateToServer(query string) {
	query = strings.TrimSpace(query)
	if query == "" {
		return
	}
	if match, ok := findMatchingMcpServer(m.servers, query); ok {
		m.openDetailPage(match)
		return
	}
	if strings.EqualFold(query, "logs") {
		m.openDetailPage("")
		return
	}
	if m.hasLoadedOnce {
		m.showToast(fmt.Sprintf("MCP server %q not found", query))
	} else {
		m.pendingOpenServer = query
	}
}

func findMatchingMcpServer(servers []opencode.McpServer, query string) (string, bool) {
	for _, s := range servers {
		if s.Name == query {
			return s.Name, true
		}
	}
	for _, s := range servers {
		if strings.EqualFold(s.Name, query) {
			return s.Name, true
		}
	}
	return "", false
}

func (m *mcpManager) refreshAll() {
	if m.isDisposed() || m.w.client == nil {
		return
	}
	client := m.w.client
	ctx := m.w.ctx
	reqDir := m.dir
	m.refreshGen++
	gen := m.refreshGen

	gtkutil.Async(ctx, func() func() {
		reqCtx, cancel := context.WithTimeout(ctx, mcpFetchTimeout)
		defer cancel()

		var (
			mcpRes     *opencode.McpListResult
			mcpErr     error
			cfgEntries []opencode.ConfigEntry
			cfgErr     error
			resCat     *opencode.McpResourceListResult
			resErr     error
			wg         sync.WaitGroup
		)
		wg.Add(3)
		go func() {
			defer wg.Done()
			mcpRes, mcpErr = client.ListMCP(reqCtx, reqDir)
		}()
		go func() {
			defer wg.Done()
			cfgEntries, cfgErr = client.ListConfig(reqCtx, reqDir)
		}()
		go func() {
			defer wg.Done()
			resCat, resErr = client.ListMCPResources(reqCtx, reqDir)
		}()
		wg.Wait()

		return func() {
			if m.isDisposed() || m.refreshGen != gen {
				return
			}
			if mcpErr != nil {
				if m.errBanner != nil {
					m.errBanner.SetTitle(opencode.HumanizeAPIError(mcpErr))
					m.errBanner.SetRevealed(true)
				}
				if !m.hasLoadedOnce {
					m.hasLoadedOnce = true
					m.syncViews()
				} else {
					m.syncMcpSectionVisibility()
				}
				return
			}
			if m.errBanner != nil {
				m.errBanner.SetRevealed(false)
			}

			var servers []opencode.McpServer
			var resolvedDir string
			if mcpRes != nil {
				servers = mcpRes.Servers
				resolvedDir = mcpRes.Location.Directory
			}

			m.w.ensureMcpMaps()
			key := normalizeProjectDir(resolvedDir)
			if key == "" {
				key = normalizeProjectDir(reqDir)
			}
			if key == "" {
				key = normalizeProjectDir(m.dir)
			}
			if m.w.activeDirectory() == "" && key != "" && m.w.defaultServerDir == "" {
				m.w.defaultServerDir = key
			}
			if key != "" {
				m.w.mcpByDir[key] = servers
				m.w.mcpLoadedDirs[key] = true
			}
			m.w.refreshMcpStatusChip()

			if normalizeProjectDir(m.dir) != "" && key != "" && normalizeProjectDir(m.dir) != key {
				return
			}
			if resolvedDir != "" {
				m.setResolvedDir(resolvedDir)
			}
			if cfgErr == nil {
				m.configs = opencode.MergeMcpConfigs(cfgEntries)
			}
			if resErr == nil && resCat != nil {
				m.resources = resCat.Catalog
			}

			pending := m.pendingOpenServer
			m.pendingOpenServer = ""
			m.applyServers(servers, pending)

			if m.isOpen() && m.detail != nil && m.logPollActive {
				m.triggerLogPollOnce()
			}
		}
	})
}

func (m *mcpManager) refreshResources() {
	if !m.isOpen() || m.w.client == nil {
		return
	}
	client := m.w.client
	ctx := m.w.ctx
	reqDir := m.dir

	gtkutil.Async(ctx, func() func() {
		reqCtx, cancel := context.WithTimeout(ctx, mcpFetchTimeout)
		defer cancel()
		resCat, err := client.ListMCPResources(reqCtx, reqDir)
		return func() {
			if m.isDisposed() || err != nil || resCat == nil {
				return
			}
			if normalizeProjectDir(reqDir) != normalizeProjectDir(m.dir) {
				return
			}
			m.resources = resCat.Catalog
			m.syncViews()
		}
	})
}

func (m *mcpManager) applyServers(servers []opencode.McpServer, pendingOpen string) {
	m.hasLoadedOnce = true
	m.servers = slices.Clone(servers)
	m.syncViews()
	if pendingOpen != "" && m.isOpen() {
		m.navigateToServer(pendingOpen)
	}
}

func (m *mcpManager) syncViews() {
	if m.isDisposed() {
		return
	}
	m.syncServerList()
	m.syncMcpSectionVisibility()
	if m.detail != nil {
		m.detail.syncServerAndMeta()
	}
}

func (m *mcpManager) syncMcpSectionVisibility() {
	if m == nil || m.sectionBox == nil {
		return
	}
	errRevealed := m.errBanner != nil && m.errBanner.Revealed()
	show := len(m.servers) > 0 || m.explicitOpen || !m.hasLoadedOnce || errRevealed
	m.sectionBox.SetVisible(show)
	if m.w != nil {
		m.w.updateSheetEmptyState()
	}
}

func (m *mcpManager) findServer(name string) (opencode.McpServer, bool) {
	for _, s := range m.servers {
		if s.Name == name {
			return s, true
		}
	}
	return opencode.McpServer{}, false
}

func (m *mcpManager) countResourcesFor(serverName string) int {
	n := 0
	for _, r := range m.resources.Resources {
		if r.Server == serverName {
			n++
		}
	}
	for _, t := range m.resources.Templates {
		if t.Server == serverName {
			n++
		}
	}
	return n
}

func compareMcpServers(a, b opencode.McpServer) int {
	la := strings.ToLower(a.Name)
	lb := strings.ToLower(b.Name)
	if la < lb {
		return -1
	}
	if la > lb {
		return 1
	}
	return strings.Compare(a.Name, b.Name)
}

func (m *mcpManager) syncServerList() {
	if m.listStack == nil || m.listBox == nil {
		return
	}
	if !m.hasLoadedOnce && len(m.servers) == 0 {
		m.listStack.SetVisibleChildName("loading")
		if m.searchEntry != nil {
			m.searchEntry.SetVisible(false)
		}
		return
	}

	if m.searchEntry != nil {
		m.searchEntry.SetVisible(len(m.servers) > 4)
	}
	if len(m.servers) == 0 {
		m.clearListBoxRows()
		m.listStack.SetVisibleChildName("empty")
		return
	}

	sorted := slices.Clone(m.servers)
	slices.SortFunc(sorted, compareMcpServers)

	var query string
	if m.searchEntry != nil {
		query = strings.ToLower(strings.TrimSpace(m.searchEntry.Text()))
	}
	var visible []opencode.McpServer
	for _, srv := range sorted {
		if query == "" ||
			strings.Contains(strings.ToLower(srv.Name), query) ||
			strings.Contains(strings.ToLower(srv.Title), query) ||
			strings.Contains(strings.ToLower(srv.Status.Status), query) {
			visible = append(visible, srv)
		}
	}

	if len(visible) == 0 {
		m.clearListBoxRows()
		m.listStack.SetVisibleChildName("filtered-empty")
		return
	}

	m.listStack.SetVisibleChildName("list")

	wantOrder := make([]string, len(visible))
	for i, s := range visible {
		wantOrder[i] = s.Name
	}

	hadListFocus := false
	if m.w != nil && m.w.ApplicationWindow != nil {
		if cur := m.w.ApplicationWindow.Focus(); cur != nil {
			curW := gtk.BaseWidget(cur)
			hadListFocus = curW.IsAncestor(m.listBox)
		}
	}

	if !slices.Equal(m.rowOrder, wantOrder) {
		m.clearListBoxRows()
		for _, srv := range visible {
			row := m.rows[srv.Name]
			if row == nil {
				row = m.newServerRow(srv.Name)
				m.rows[srv.Name] = row
			}
			m.listBox.Append(row.actionRow)
		}
		m.rowOrder = wantOrder
		if hadListFocus {
			m.focusOverviewPage()
		}
	}

	for _, srv := range visible {
		if row := m.rows[srv.Name]; row != nil {
			m.syncServerRow(row, srv)
		}
	}
}

func (m *mcpManager) clearListBoxRows() {
	if m.listBox != nil {
		m.listBox.RemoveAll()
	}
	clear(m.rows)
	m.rowOrder = nil
}

func newMcpStatusIndicator() (*gtk.Stack, *gtk.Box, *adw.Spinner) {
	stack := gtk.NewStack()
	stack.SetVAlign(gtk.AlignCenter)
	stack.SetHAlign(gtk.AlignCenter)
	stack.SetSizeRequest(16, 16)

	dotWrap := gtk.NewBox(gtk.OrientationHorizontal, 0)
	dotWrap.SetHAlign(gtk.AlignCenter)
	dotWrap.SetVAlign(gtk.AlignCenter)
	dot := gtk.NewBox(gtk.OrientationHorizontal, 0)
	dot.SetSizeRequest(10, 10)
	dot.SetHAlign(gtk.AlignCenter)
	dot.SetVAlign(gtk.AlignCenter)
	dot.AddCSSClass("glup-mcp-dot")
	dotWrap.Append(dot)
	stack.AddNamed(dotWrap, "dot")

	spinner := adw.NewSpinner()
	spinner.SetSizeRequest(16, 16)
	spinner.SetHAlign(gtk.AlignCenter)
	spinner.SetVAlign(gtk.AlignCenter)
	stack.AddNamed(spinner, "spinner")

	return stack, dot, spinner
}

func applyMcpDotTone(dot *gtk.Box, status string) {
	for _, cls := range []string{
		"glup-mcp-dot-ok",
		"glup-mcp-dot-warn",
		"glup-mcp-dot-err",
		"glup-mcp-dot-dim",
	} {
		dot.RemoveCSSClass(cls)
	}
	switch status {
	case "connected":
		dot.AddCSSClass("glup-mcp-dot-ok")
	case "needs_auth":
		dot.AddCSSClass("glup-mcp-dot-warn")
	case "failed":
		dot.AddCSSClass("glup-mcp-dot-err")
	default:
		dot.AddCSSClass("glup-mcp-dot-dim")
	}
}

func (m *mcpManager) newServerRow(serverName string) *mcpServerRow {
	ar := adw.NewActionRow()
	ar.SetUseMarkup(false)
	ar.SetActivatable(true)
	ar.SetSubtitleLines(2)

	prefixStack, dot, spinner := newMcpStatusIndicator()
	ar.AddPrefix(prefixStack)

	restartBtn := gtk.NewButtonFromIconName("refresh-symbolic")
	restartBtn.AddCSSClass("flat")
	restartBtn.AddCSSClass("circular")
	restartBtn.SetVAlign(gtk.AlignCenter)
	ar.AddSuffix(restartBtn)

	sw := gtk.NewSwitch()
	sw.SetVAlign(gtk.AlignCenter)
	ar.AddSuffix(sw)

	chev := gtk.NewImageFromIconName("chevron-right-symbolic")
	chev.AddCSSClass("dim-label")
	chev.SetVAlign(gtk.AlignCenter)
	ar.AddSuffix(chev)

	row := &mcpServerRow{
		name:         serverName,
		actionRow:    ar,
		prefixStack:  prefixStack,
		dot:          dot,
		spinner:      spinner,
		restartBtn:   restartBtn,
		toggleSwitch: sw,
	}

	ar.ConnectActivated(func() {
		m.openDetailPage(serverName)
	})
	restartBtn.ConnectClicked(func() {
		m.runServerAction(serverName, true)
	})
	sw.ConnectStateSet(func(state bool) bool {
		if row.syncingRow {
			return false
		}
		m.runServerAction(serverName, state)
		return true
	})

	return row
}

func (m *mcpManager) syncServerRow(row *mcpServerRow, srv opencode.McpServer) {
	inFlight := m.w.mcpInFlight[mcpFlightKey(m.dir, srv.Name)]

	title := srv.Name
	if srv.Title != "" && !strings.EqualFold(srv.Title, srv.Name) {
		title = srv.Name + " — " + srv.Title
	}
	row.actionRow.SetTitle(title)
	row.actionRow.SetSubtitle(m.formatRowSubtitle(srv))
	if srv.Status.Error != "" {
		row.actionRow.SetTooltipText(srv.Status.Error)
	} else {
		row.actionRow.SetTooltipText("")
	}

	if inFlight || srv.Status.Status == "pending" {
		row.prefixStack.SetVisibleChildName("spinner")
	} else {
		applyMcpDotTone(row.dot, srv.Status.Status)
		row.prefixStack.SetVisibleChildName("dot")
	}

	hadRowActionFocus := row.restartBtn.IsFocus() || row.toggleSwitch.IsFocus()
	row.restartBtn.SetVisible(srv.Status.Status != "disabled")
	row.restartBtn.SetSensitive(!inFlight)
	if srv.Status.Status == "failed" || srv.Status.Status == "needs_auth" {
		row.restartBtn.SetTooltipText("Retry Connection")
	} else {
		row.restartBtn.SetTooltipText("Restart Server")
	}

	wantActive := srv.Status.Status != "disabled"
	row.syncingRow = true
	if row.toggleSwitch.Active() != wantActive {
		row.toggleSwitch.SetActive(wantActive)
	}
	if row.toggleSwitch.State() != wantActive {
		row.toggleSwitch.SetState(wantActive)
	}
	row.syncingRow = false
	row.toggleSwitch.SetSensitive(!inFlight)
	if hadRowActionFocus && (!row.restartBtn.Visible() || !row.restartBtn.Sensitive() || !row.toggleSwitch.Sensitive()) {
		row.actionRow.GrabFocus()
	}
}

func (m *mcpManager) formatRowSubtitle(srv opencode.McpServer) string {
	var transport string
	if cfg, ok := m.configs[srv.Name]; ok && cfg.Type != "" {
		transport = cfg.Type
	}
	var parts []string
	switch srv.Status.Status {
	case "connected":
		parts = append(parts, "Connected")
		if transport != "" {
			parts = append(parts, transport)
		}
		if n := m.countResourcesFor(srv.Name); n > 0 {
			if n == 1 {
				parts = append(parts, "1 resource")
			} else {
				parts = append(parts, fmt.Sprintf("%d resources", n))
			}
		}
	case "pending":
		parts = append(parts, "Connecting…")
		if transport != "" {
			parts = append(parts, transport)
		}
	case "disabled":
		parts = append(parts, "Disabled")
		if transport != "" {
			parts = append(parts, transport)
		}
	case "needs_auth":
		parts = append(parts, "Authentication required")
		if s := summarizeMcpError(srv.Status.Error); s != "" {
			parts = append(parts, s)
		} else if srv.IntegrationID != "" {
			parts = append(parts, "Open to sign in")
		} else {
			parts = append(parts, "Sign in via OpenCode CLI/web UI")
		}
	case "failed":
		parts = append(parts, "Failed")
		if s := summarizeMcpError(srv.Status.Error); s != "" {
			parts = append(parts, s)
		}
	default:
		parts = append(parts, mcpStatusLabel(srv.Status.Status))
	}
	return strings.Join(parts, " · ")
}

func mcpStatusLabel(status string) string {
	switch status {
	case "connected":
		return "Connected"
	case "pending":
		return "Connecting…"
	case "disabled":
		return "Disabled"
	case "needs_auth":
		return "Needs Authentication"
	case "failed":
		return "Failed"
	default:
		if status == "" {
			return "Unknown"
		}
		return status
	}
}

func summarizeMcpError(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if nl := strings.IndexAny(raw, "\r\n"); nl >= 0 {
		raw = strings.TrimSpace(raw[:nl])
	}
	return raw
}

func (m *mcpManager) runServerAction(serverName string, connect bool) {
	if m.isDisposed() || m.w.client == nil {
		return
	}
	m.w.ensureMcpMaps()
	targetDir := m.dir
	flightKey := mcpFlightKey(targetDir, serverName)
	if m.w.mcpInFlight[flightKey] {
		return
	}

	m.w.mcpInFlight[flightKey] = true
	m.syncViews()

	client := m.w.client
	ctx := m.w.ctx
	gtkutil.Async(ctx, func() func() {
		reqCtx, cancel := context.WithTimeout(ctx, mcpActionTimeout)
		defer cancel()

		var actionErr error
		if connect {
			actionErr = client.ConnectMCP(reqCtx, serverName, targetDir)
		} else {
			actionErr = client.DisconnectMCP(reqCtx, serverName, targetDir)
		}

		listCtx, listCancel := context.WithTimeout(ctx, mcpFetchTimeout)
		defer listCancel()
		mcpRes, listErr := client.ListMCP(listCtx, targetDir)

		var resCat *opencode.McpResourceListResult
		var resErr error
		if connect && listErr == nil {
			resCat, resErr = client.ListMCPResources(listCtx, targetDir)
		}

		return func() {
			m.w.ensureMcpMaps()
			delete(m.w.mcpInFlight, flightKey)

			if listErr == nil && mcpRes != nil {
				resolvedKey := normalizeProjectDir(mcpRes.Location.Directory)
				if resolvedKey == "" {
					resolvedKey = normalizeProjectDir(targetDir)
				}
				if resolvedKey == "" {
					resolvedKey = m.w.defaultServerDir
				}
				if resolvedKey != "" {
					m.w.mcpByDir[resolvedKey] = mcpRes.Servers
					m.w.mcpLoadedDirs[resolvedKey] = true
				}
				m.w.refreshMcpStatusChip()

				if !m.isDisposed() && m.w.mcpMgr == m && normalizeProjectDir(m.dir) == resolvedKey {
					if mcpRes.Location.Directory != "" {
						m.setResolvedDir(mcpRes.Location.Directory)
					}
					if resErr == nil && resCat != nil {
						m.resources = resCat.Catalog
					}
					m.applyServers(mcpRes.Servers, "")
				}
			} else if !m.isDisposed() && m.w.mcpMgr == m {
				m.syncViews()
			}

			if !m.isOpen() || m.w.mcpMgr != m {
				return
			}

			if actionErr != nil {
				m.showToast(opencode.HumanizeAPIError(actionErr))
				return
			}
			if !connect {
				m.showToast(serverName + " stopped")
				return
			}
			if listErr == nil && mcpRes != nil {
				if srv, ok := findServerInSlice(mcpRes.Servers, serverName); ok {
					switch srv.Status.Status {
					case "connected":
						m.showToast(serverName + " connected")
					case "failed":
						msg := "Failed to start " + serverName
						if s := summarizeMcpError(srv.Status.Error); s != "" {
							msg += ": " + s
						}
						m.showToast(msg)
					case "needs_auth":
						if srv.IntegrationID != "" {
							m.showToast(serverName + " requires authentication — click Log In to sign in")
						} else {
							m.showToast(serverName + " requires authentication (sign in via OpenCode CLI/web UI)")
						}
					}
				}
			}
		}
	})
}

func findServerInSlice(servers []opencode.McpServer, name string) (opencode.McpServer, bool) {
	for _, s := range servers {
		if s.Name == name {
			return s, true
		}
	}
	return opencode.McpServer{}, false
}

func (m *mcpManager) openDetailPage(serverName string) {
	if !m.isOpen() || m.navView == nil {
		return
	}
	if m.detail != nil && m.detailPage != nil {
		if vp := m.navView.VisiblePage(); vp != nil && vp.Object.Native() == m.detailPage.Object.Native() {
			if m.detail.serverName == serverName {
				m.focusDetailPage()
				return
			}
			m.stopLogPoller()
			m.resettingNav = true
			m.navView.Pop()
			m.resettingNav = false
		} else {
			m.stopLogPoller()
		}
	}

	d := m.newDetailView(serverName)
	m.detail = d
	m.detailPage = d.page
	m.navView.Push(d.page)
	d.syncServerAndMeta()
	m.startLogTailing()
	m.focusDetailPage()
}

func (m *mcpManager) newDetailView(serverName string) *mcpDetailView {
	titleText := serverName
	if titleText == "" {
		titleText = "All MCP Logs"
	}

	toolbar := adw.NewToolbarView()
	toolbar.SetTopBarStyle(adw.ToolbarFlat)
	header := adw.NewHeaderBar()
	header.SetShowStartTitleButtons(false)
	header.SetShowEndTitleButtons(false)
	header.AddCSSClass("glup-sheet-detail-header")
	headerTitle := adw.NewWindowTitle(titleText, shortDir(m.dir))
	header.SetTitleWidget(headerTitle)

	refreshBtn := gtk.NewButtonFromIconName("refresh-symbolic")
	refreshBtn.SetTooltipText("Refresh")
	refreshBtn.ConnectClicked(func() {
		m.refreshAll()
	})
	header.PackEnd(refreshBtn)
	toolbar.AddTopBar(header)

	outerScroll := gtk.NewScrolledWindow()
	outerScroll.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	outerScroll.SetPropagateNaturalHeight(true)
	outerScroll.SetMaxContentHeight(510)
	outerScroll.SetVExpand(true)

	clamp := adw.NewClamp()
	clamp.SetMaximumSize(680)

	content := gtk.NewBox(gtk.OrientationVertical, 12)
	content.SetMarginTop(8)
	content.SetMarginBottom(16)
	content.SetMarginStart(16)
	content.SetMarginEnd(16)
	content.SetVExpand(true)

	d := &mcpDetailView{
		mgr:         m,
		serverName:  serverName,
		headerTitle: headerTitle,
		refreshBtn:  refreshBtn,
	}

	if serverName != "" {
		statusGroup := adw.NewPreferencesGroup()

		statusRow := adw.NewActionRow()
		statusRow.SetUseMarkup(false)
		statusRow.SetSubtitleSelectable(true)
		statusRow.SetSubtitleLines(0)
		statusPrefix, statusDot, statusSpinner := newMcpStatusIndicator()
		statusRow.AddPrefix(statusPrefix)

		copyErrBtn := gtk.NewButtonFromIconName("edit-copy-symbolic")
		copyErrBtn.AddCSSClass("flat")
		copyErrBtn.AddCSSClass("circular")
		copyErrBtn.SetVAlign(gtk.AlignCenter)
		copyErrBtn.SetTooltipText("Copy Error")
		copyErrBtn.SetVisible(false)
		copyErrBtn.ConnectClicked(func() {
			if d.lastStatusError != "" {
				m.w.copyTextToClipboard(d.lastStatusError)
				m.showToast("Copied error message")
			}
		})
		statusRow.AddSuffix(copyErrBtn)
		statusGroup.Add(statusRow)

		controlsRow := gtk.NewListBoxRow()
		controlsRow.SetActivatable(false)
		controlsRow.SetSelectable(false)
		ctrlBox := gtk.NewBox(gtk.OrientationHorizontal, 8)
		ctrlBox.SetMarginTop(8)
		ctrlBox.SetMarginBottom(8)
		ctrlBox.SetMarginStart(10)
		ctrlBox.SetMarginEnd(10)

		loginBtn, _ := newIconTextButton("shield-symbolic", "Log In…")
		loginBtn.AddCSSClass("suggested-action")
		loginBtn.SetVisible(false)
		loginBtn.ConnectClicked(func() {
			m.openAuthDialog(serverName)
		})

		startBtn, _ := newIconTextButton("arrow-forward-symbolic", "Start")
		startBtn.AddCSSClass("suggested-action")
		startBtn.ConnectClicked(func() {
			m.runServerAction(serverName, true)
		})

		restartBtn, restartLbl := newIconTextButton("refresh-symbolic", "Restart")
		restartBtn.ConnectClicked(func() {
			m.runServerAction(serverName, true)
		})

		stopBtn, _ := newIconTextButton("stop-circle-symbolic", "Stop")
		stopBtn.AddCSSClass("destructive-action")
		stopBtn.ConnectClicked(func() {
			m.runServerAction(serverName, false)
		})

		ctrlBox.Append(loginBtn)
		ctrlBox.Append(startBtn)
		ctrlBox.Append(restartBtn)
		ctrlBox.Append(stopBtn)
		controlsRow.SetChild(ctrlBox)
		statusGroup.Add(controlsRow)
		content.Append(statusGroup)

		metaGroup := adw.NewPreferencesGroup()
		configExpander := adw.NewExpanderRow()
		configExpander.SetUseMarkup(false)
		configExpander.SetTitle("Configuration")
		configExpander.SetExpanded(false)
		metaGroup.Add(configExpander)

		resourceExpander := adw.NewExpanderRow()
		resourceExpander.SetUseMarkup(false)
		resourceExpander.SetTitle("Resources & Templates (0)")
		resourceExpander.SetExpanded(false)
		metaGroup.Add(resourceExpander)
		content.Append(metaGroup)

		d.statusRow = statusRow
		d.statusPrefix = statusPrefix
		d.statusDot = statusDot
		d.statusSpinner = statusSpinner
		d.copyErrBtn = copyErrBtn
		d.loginBtn = loginBtn
		d.startBtn = startBtn
		d.restartBtn = restartBtn
		d.restartLbl = restartLbl
		d.stopBtn = stopBtn
		d.configExpander = configExpander
		d.resourceExpander = resourceExpander
	}

	// Logs section (shared by single-server and all-logs modes)
	logsBox := gtk.NewBox(gtk.OrientationVertical, 8)
	logsBox.SetVExpand(true)

	toolbarRow := gtk.NewBox(gtk.OrientationHorizontal, 6)
	logsTitle := gtk.NewLabel("Logs")
	logsTitle.AddCSSClass("heading")
	toolbarRow.Append(logsTitle)

	liveBadge := gtk.NewLabel("Live")
	liveBadge.AddCSSClass("glup-mcp-live-badge")
	liveBadge.SetVisible(false)
	toolbarRow.Append(liveBadge)

	spacer := gtk.NewBox(gtk.OrientationHorizontal, 0)
	spacer.SetHExpand(true)
	toolbarRow.Append(spacer)

	levelDrop := gtk.NewDropDownFromStrings([]string{"All Levels", "Info+", "Warn+", "Error"})
	levelDrop.SetSelected(0)
	levelDrop.SetTooltipText("Minimum log level")
	levelDrop.NotifyProperty("selected", func() {
		d.rebuildLogBuffer()
	})
	toolbarRow.Append(levelDrop)

	logSearch := gtk.NewSearchEntry()
	logSearch.SetPlaceholderText("Filter logs…")
	logSearch.SetWidthChars(13)
	logSearch.ConnectSearchChanged(func() {
		d.rebuildLogBuffer()
	})
	logSearch.ConnectStopSearch(func() {
		if logSearch.Text() != "" {
			logSearch.SetText("")
			return
		}
		m.w.handleBottomSheetEscape()
	})
	toolbarRow.Append(logSearch)

	followBtn := gtk.NewToggleButton()
	followBtn.SetActive(true)
	followBtn.SetIconName("arrow-downward-symbolic")
	followBtn.SetTooltipText("Follow new log lines")
	toolbarRow.Append(followBtn)

	clearBtn := gtk.NewButtonFromIconName("edit-clear-all-symbolic")
	clearBtn.AddCSSClass("flat")
	clearBtn.SetTooltipText("Clear log display")
	clearBtn.ConnectClicked(func() {
		d.clearLogDisplay()
	})
	toolbarRow.Append(clearBtn)

	copyBtn := gtk.NewButtonFromIconName("edit-copy-symbolic")
	copyBtn.AddCSSClass("flat")
	copyBtn.SetTooltipText("Copy visible logs")
	copyBtn.SetSensitive(false)
	copyBtn.ConnectClicked(func() {
		s, e := d.logBuf.Bounds()
		txt := d.logBuf.Text(s, e, false)
		if strings.TrimSpace(txt) != "" {
			m.w.copyTextToClipboard(txt)
			m.showToast("Copied logs")
		}
	})
	toolbarRow.Append(copyBtn)
	logsBox.Append(toolbarRow)

	bodyStack := gtk.NewStack()
	bodyStack.SetVhomogeneous(false)
	bodyStack.SetHhomogeneous(false)
	bodyStack.SetVExpand(true)

	remotePage := adw.NewStatusPage()
	remotePage.AddCSSClass("compact")
	remotePage.SetIconName("cloud-off-symbolic")
	remotePage.SetTitle("Remote Server Logs Unavailable")
	remotePage.SetDescription("OpenCode logs are stored locally on the server host.")
	bodyStack.AddNamed(remotePage, "remote")

	missingPage := adw.NewStatusPage()
	missingPage.AddCSSClass("compact")
	missingPage.SetIconName("summarize-symbolic")
	missingPage.SetTitle("No Log File Found")
	missingPage.SetDescription("Expected OpenCode log file at ~/.local/share/opencode/log/opencode.log")
	bodyStack.AddNamed(missingPage, "missing")

	logFrame := gtk.NewBox(gtk.OrientationVertical, 0)
	logFrame.AddCSSClass("glup-mcp-log-frame")
	logFrame.SetVExpand(true)

	logOverlay := gtk.NewOverlay()
	logOverlay.SetVExpand(true)

	logScroll := gtk.NewScrolledWindow()
	logScroll.SetVExpand(true)
	if serverName == "" {
		logScroll.SetMinContentHeight(360)
	} else {
		logScroll.SetMinContentHeight(200)
	}
	logScroll.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)

	logBuf := gtk.NewTextBuffer(nil)
	textView := gtk.NewTextViewWithBuffer(logBuf)
	textView.SetEditable(false)
	textView.SetCursorVisible(false)
	textView.SetWrapMode(gtk.WrapWordChar)
	textView.SetTopMargin(8)
	textView.SetBottomMargin(8)
	textView.SetLeftMargin(10)
	textView.SetRightMargin(10)
	textView.AddCSSClass("glup-mcp-log-view")
	logScroll.SetChild(textView)

	emptyMsg := "No MCP log entries yet"
	if serverName != "" {
		emptyMsg = fmt.Sprintf("No log entries for %s", serverName)
	}
	emptyLabel := gtk.NewLabel(emptyMsg)
	emptyLabel.AddCSSClass("dim-label")
	emptyLabel.SetCanTarget(false)
	emptyLabel.SetHAlign(gtk.AlignCenter)
	emptyLabel.SetVAlign(gtk.AlignCenter)

	logOverlay.SetChild(logScroll)
	logOverlay.AddOverlay(emptyLabel)
	logFrame.Append(logOverlay)
	bodyStack.AddNamed(logFrame, "logs")
	logsBox.Append(bodyStack)

	hintLbl := gtk.NewLabel("MCP server stderr lines only appear when the OpenCode daemon runs with OPENCODE_LOG_LEVEL=DEBUG.")
	hintLbl.AddCSSClass("caption")
	hintLbl.AddCSSClass("dim-label")
	hintLbl.SetXAlign(0)
	hintLbl.SetWrap(true)
	logsBox.Append(hintLbl)

	content.Append(logsBox)
	clamp.SetChild(content)
	outerScroll.SetChild(clamp)
	toolbar.SetContent(outerScroll)

	d.outerScroll = outerScroll
	d.liveBadge = liveBadge
	d.levelDrop = levelDrop
	d.logSearch = logSearch
	d.followBtn = followBtn
	d.clearBtn = clearBtn
	d.copyBtn = copyBtn
	d.bodyStack = bodyStack
	d.scrolled = logScroll
	d.vadj = logScroll.VAdjustment()
	d.textView = textView
	d.logBuf = logBuf
	d.emptyLabel = emptyLabel
	d.page = adw.NewNavigationPageWithTag(toolbar, titleText, "detail")

	d.initTags()
	d.applyLogTheme()

	d.vadj.NotifyProperty("value", func() {
		if d.programmaticScroll || d.suppressFollowCheck {
			return
		}
		upper := d.vadj.Upper()
		page := d.vadj.PageSize()
		if upper <= page {
			return
		}
		atBottom := d.vadj.Value()+page >= upper-mcpBottomFollowPx
		if !atBottom && d.followBtn.Active() {
			d.suppressFollowCheck = true
			d.followBtn.SetActive(false)
			d.suppressFollowCheck = false
		} else if atBottom && !d.followBtn.Active() {
			d.suppressFollowCheck = true
			d.followBtn.SetActive(true)
			d.suppressFollowCheck = false
		}
	})
	d.followBtn.ConnectToggled(func() {
		if !d.suppressFollowCheck && d.followBtn.Active() {
			d.scrollToBottom()
		}
	})

	return d
}

func newIconTextButton(iconName, label string) (*gtk.Button, *gtk.Label) {
	btn := gtk.NewButton()
	box := gtk.NewBox(gtk.OrientationHorizontal, 6)
	box.SetVAlign(gtk.AlignCenter)
	img := gtk.NewImageFromIconName(iconName)
	img.SetPixelSize(16)
	lbl := gtk.NewLabel(label)
	box.Append(img)
	box.Append(lbl)
	btn.SetChild(box)
	return btn, lbl
}

func (d *mcpDetailView) initTags() {
	table := d.logBuf.TagTable()
	makeTag := func(name string, cfg func(t *gtk.TextTag)) *gtk.TextTag {
		t := gtk.NewTextTag(name)
		if cfg != nil {
			cfg(t)
		}
		table.Add(t)
		return t
	}
	d.tagDim = makeTag("dim", nil)
	d.tagDebug = makeTag("debug", nil)
	d.tagInfo = makeTag("info", nil)
	d.tagWarn = makeTag("warn", nil)
	d.tagError = makeTag("error", nil)
	d.tagMsg = makeTag("msg", func(t *gtk.TextTag) {
		t.SetObjectProperty("weight", int(pango.WeightSemibold))
	})
	d.tagServer = makeTag("server", func(t *gtk.TextTag) {
		t.SetObjectProperty("weight", int(pango.WeightBold))
	})
	d.tagExtra = makeTag("extra", nil)
}

func (m *mcpManager) applyLogTheme() {
	if m.detail != nil {
		m.detail.applyLogTheme()
	}
}

func (d *mcpDetailView) applyLogTheme() {
	dark := true
	if sm := adw.StyleManagerGetDefault(); sm != nil {
		dark = sm.Dark()
	}
	if dark {
		d.tagDim.SetObjectProperty("foreground", "#9a9996")
		d.tagDebug.SetObjectProperty("foreground", "#8e8e93")
		d.tagInfo.SetObjectProperty("foreground", "#78aeed")
		d.tagWarn.SetObjectProperty("foreground", "#f6d32d")
		d.tagError.SetObjectProperty("foreground", "#ff7b63")
		d.tagServer.SetObjectProperty("foreground", "#99c1f1")
		d.tagExtra.SetObjectProperty("foreground", "#a8a7a3")
	} else {
		d.tagDim.SetObjectProperty("foreground", "#6e6d6a")
		d.tagDebug.SetObjectProperty("foreground", "#6e6d70")
		d.tagInfo.SetObjectProperty("foreground", "#1c71d8")
		d.tagWarn.SetObjectProperty("foreground", "#ae6a00")
		d.tagError.SetObjectProperty("foreground", "#c01c28")
		d.tagServer.SetObjectProperty("foreground", "#1a5fb4")
		d.tagExtra.SetObjectProperty("foreground", "#5e5c64")
	}
}

func (d *mcpDetailView) syncServerAndMeta() {
	if d.serverName == "" {
		return
	}
	srv, ok := d.mgr.findServer(d.serverName)
	if !ok {
		srv = opencode.McpServer{
			Name:   d.serverName,
			Status: opencode.McpStatus{Status: "disabled"},
		}
	}
	inFlight := d.mgr.w.mcpInFlight[mcpFlightKey(d.mgr.dir, d.serverName)]

	canOAuth := srv.Status.Status == "needs_auth" && strings.TrimSpace(srv.IntegrationID) != ""
	d.statusRow.SetTitle(mcpStatusLabel(srv.Status.Status))
	subtitle := srv.Status.Error
	if srv.Status.Status == "needs_auth" {
		hint := "Sign in via OpenCode CLI or web UI, then click Retry."
		if canOAuth {
			hint = "Click Log In to authenticate with this server."
		}
		if subtitle == "" {
			subtitle = hint
		} else {
			subtitle += "\n" + hint
		}
	}
	d.statusRow.SetSubtitle(subtitle)
	d.lastStatusError = srv.Status.Error
	d.copyErrBtn.SetVisible(srv.Status.Error != "")

	if inFlight || srv.Status.Status == "pending" {
		d.statusPrefix.SetVisibleChildName("spinner")
	} else {
		applyMcpDotTone(d.statusDot, srv.Status.Status)
		d.statusPrefix.SetVisibleChildName("dot")
	}

	hadActionFocus := d.loginBtn.IsFocus() || d.startBtn.IsFocus() || d.restartBtn.IsFocus() || d.stopBtn.IsFocus()
	isDisabled := srv.Status.Status == "disabled"
	d.loginBtn.SetVisible(canOAuth)
	d.loginBtn.SetSensitive(!inFlight)
	d.startBtn.SetVisible(isDisabled)
	d.restartBtn.SetVisible(!isDisabled)
	d.stopBtn.SetVisible(!isDisabled)
	if srv.Status.Status == "failed" || srv.Status.Status == "needs_auth" {
		d.restartLbl.SetText("Retry")
	} else {
		d.restartLbl.SetText("Restart")
	}
	d.startBtn.SetSensitive(!inFlight)
	d.restartBtn.SetSensitive(!inFlight)
	d.stopBtn.SetSensitive(!inFlight)
	if hadActionFocus {
		switch {
		case inFlight:
			if d.refreshBtn != nil {
				d.refreshBtn.GrabFocus()
			}
		case canOAuth:
			d.loginBtn.GrabFocus()
		case isDisabled:
			d.startBtn.GrabFocus()
		default:
			d.restartBtn.GrabFocus()
		}
	}

	d.syncConfigExpander()
	d.syncResourceExpander()
}

func (d *mcpDetailView) addConfigRow(title, subtitle string, monospace bool) {
	ar := adw.NewActionRow()
	ar.SetUseMarkup(false)
	ar.AddCSSClass("property")
	if monospace {
		ar.AddCSSClass("monospace")
	}
	ar.SetTitle(title)
	ar.SetSubtitle(subtitle)
	ar.SetSubtitleSelectable(true)
	ar.SetSubtitleLines(0)
	d.configExpander.AddRow(ar)
	d.configRows = append(d.configRows, ar)
}

func (d *mcpDetailView) syncConfigExpander() {
	for _, r := range d.configRows {
		d.configExpander.Remove(r)
	}
	d.configRows = nil

	cfg, ok := d.mgr.configs[d.serverName]
	if !ok {
		d.addConfigRow("Source", "Dynamic / Plugin (no config entry)", false)
		return
	}

	switch cfg.Type {
	case "local":
		d.addConfigRow("Transport", "Local (stdio)", false)
		if len(cfg.Command) > 0 {
			d.addConfigRow("Command", strings.Join(cfg.Command, " "), true)
		}
		if cfg.Cwd != "" {
			d.addConfigRow("Working Directory", shortDir(cfg.Cwd), true)
		}
	case "remote":
		d.addConfigRow("Transport", "Remote (HTTP)", false)
		if cfg.URL != "" {
			d.addConfigRow("URL", cfg.URL, true)
		}
		if cfg.Protocol != "" {
			d.addConfigRow("Protocol", cfg.Protocol, false)
		}
	default:
		if cfg.Type != "" {
			d.addConfigRow("Transport", cfg.Type, false)
		}
	}

	if cfg.Pool != nil {
		if *cfg.Pool {
			d.addConfigRow("Connection Pool", "Enabled", false)
		} else {
			d.addConfigRow("Connection Pool", "Disabled", false)
		}
	}

	if cfg.Timeout != nil && (cfg.Timeout.Startup > 0 || cfg.Timeout.Catalog > 0 || cfg.Timeout.Execution > 0) {
		var parts []string
		if cfg.Timeout.Startup > 0 {
			parts = append(parts, fmt.Sprintf("startup %dms", cfg.Timeout.Startup))
		}
		if cfg.Timeout.Catalog > 0 {
			parts = append(parts, fmt.Sprintf("catalog %dms", cfg.Timeout.Catalog))
		}
		if cfg.Timeout.Execution > 0 {
			parts = append(parts, fmt.Sprintf("exec %dms", cfg.Timeout.Execution))
		}
		d.addConfigRow("Timeouts", strings.Join(parts, " · "), false)
	}

	if len(cfg.EnvKeys) > 0 {
		d.addConfigRow("Environment Keys", strings.Join(cfg.EnvKeys, ", ")+" (values redacted)", false)
	}
	if len(cfg.HeaderKeys) > 0 {
		d.addConfigRow("Header Keys", strings.Join(cfg.HeaderKeys, ", ")+" (values redacted)", false)
	}
	if cfg.SourcePath != "" {
		d.addConfigRow("Config File", shortDir(cfg.SourcePath), true)
	}
}

func (d *mcpDetailView) syncResourceExpander() {
	for _, r := range d.resourceRows {
		d.resourceExpander.Remove(r)
	}
	d.resourceRows = nil

	var count int
	addResRow := func(title, subtitle string) {
		ar := adw.NewActionRow()
		ar.SetUseMarkup(false)
		ar.AddCSSClass("property")
		ar.SetTitle(title)
		ar.SetSubtitle(subtitle)
		ar.SetSubtitleSelectable(true)
		ar.SetSubtitleLines(0)
		d.resourceExpander.AddRow(ar)
		d.resourceRows = append(d.resourceRows, ar)
	}

	for _, r := range d.mgr.resources.Resources {
		if r.Server != d.serverName {
			continue
		}
		count++
		title := r.Name
		if title == "" {
			title = r.URI
		}
		var sub []string
		if r.URI != "" {
			sub = append(sub, r.URI)
		}
		if r.MimeType != "" {
			sub = append(sub, r.MimeType)
		}
		if r.Description != "" {
			sub = append(sub, r.Description)
		}
		addResRow(title, strings.Join(sub, " · "))
	}
	for _, t := range d.mgr.resources.Templates {
		if t.Server != d.serverName {
			continue
		}
		count++
		title := t.Name
		if title == "" {
			title = t.URITemplate
		}
		var sub []string
		if t.URITemplate != "" {
			sub = append(sub, t.URITemplate)
		}
		if t.MimeType != "" {
			sub = append(sub, t.MimeType)
		}
		if t.Description != "" {
			sub = append(sub, t.Description)
		}
		addResRow(title+" (template)", strings.Join(sub, " · "))
	}

	d.resourceExpander.SetTitle(fmt.Sprintf("Resources & Templates (%d)", count))
	if count == 0 {
		ar := adw.NewActionRow()
		ar.SetUseMarkup(false)
		ar.SetTitle("No resources or templates exposed")
		ar.SetSensitive(false)
		d.resourceExpander.AddRow(ar)
		d.resourceRows = append(d.resourceRows, ar)
	}
}

func (m *mcpManager) startLogTailing() {
	d := m.detail
	if d == nil || !m.isOpen() {
		return
	}
	serverURL := ""
	if m.w.client != nil {
		serverURL = m.w.client.ServerURL()
	}
	if !opencode.IsLoopbackServerURL(serverURL) {
		d.isRemoteServer = true
		d.liveBadge.SetVisible(false)
		d.bodyStack.SetVisibleChildName("remote")
		return
	}

	d.isRemoteServer = false
	m.tailer = opencode.NewMcpLogTailer(m.w.mcpLogDir)
	m.logPollGen++
	gen := m.logPollGen
	m.logPollActive = true
	m.logPollInFlight = true

	serverName := d.serverName
	tailer := m.tailer
	ctx := m.w.ctx

	gtkutil.Async(ctx, func() func() {
		entries, err := tailer.ReadInitialTail(serverName)
		return func() {
			if !m.isOpen() || m.detail != d || m.logPollGen != gen {
				return
			}
			m.logPollInFlight = false
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					d.liveBadge.SetVisible(false)
					d.bodyStack.SetVisibleChildName("missing")
				} else {
					d.liveBadge.SetVisible(false)
					d.bodyStack.SetVisibleChildName("logs")
					d.rawEntries = []opencode.McpLogEntry{{
						Timestamp: time.Now(),
						Level:     "ERROR",
						Message:   "Failed to read OpenCode log",
						Error:     err.Error(),
					}}
					d.rebuildLogBuffer()
				}
			} else {
				d.liveBadge.SetVisible(true)
				d.bodyStack.SetVisibleChildName("logs")
				d.setInitialEntries(entries)
			}

			glib.TimeoutAdd(mcpLogPollMS, func() bool {
				if !m.isOpen() || m.detail != d || m.logPollGen != gen {
					return false
				}
				m.triggerLogPollOnce()
				return true
			})
		}
	})
}

func (m *mcpManager) triggerLogPollOnce() {
	d := m.detail
	if !m.isOpen() || d == nil || !m.logPollActive || m.logPollInFlight || m.tailer == nil {
		return
	}
	m.logPollInFlight = true
	gen := m.logPollGen
	serverName := d.serverName
	tailer := m.tailer

	gtkutil.Async(m.w.ctx, func() func() {
		entries, reset, err := tailer.PollNewLines(serverName)
		return func() {
			if !m.isOpen() || m.detail != d || m.logPollGen != gen {
				return
			}
			m.logPollInFlight = false
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					d.liveBadge.SetVisible(false)
					d.bodyStack.SetVisibleChildName("missing")
				}
				return
			}
			d.liveBadge.SetVisible(true)
			d.bodyStack.SetVisibleChildName("logs")
			if reset {
				d.setInitialEntries(entries)
				return
			}
			if len(entries) > 0 {
				d.appendPolledEntries(entries)
			}
		}
	})
}

func (m *mcpManager) stopLogPoller() {
	m.logPollGen++
	m.logPollActive = false
	m.logPollInFlight = false
}

func (d *mcpDetailView) clearLogDisplay() {
	d.rawEntries = nil
	d.displayedEntries = nil
	d.logBuf.SetText("")
	d.emptyLabel.SetVisible(true)
	d.copyBtn.SetSensitive(false)
}

func (d *mcpDetailView) setInitialEntries(entries []opencode.McpLogEntry) {
	if len(entries) > mcpMaxLogEntries {
		entries = slices.Clone(entries[len(entries)-mcpMaxLogEntries:])
	} else {
		entries = slices.Clone(entries)
	}
	d.rawEntries = entries
	d.rebuildLogBuffer()
}

func (d *mcpDetailView) appendPolledEntries(entries []opencode.McpLogEntry) {
	d.rawEntries = append(d.rawEntries, entries...)
	if len(d.rawEntries) > mcpMaxLogEntries {
		d.rawEntries = slices.Clone(d.rawEntries[len(d.rawEntries)-mcpMaxLogEntries:])
	}

	addedAny := false
	for _, entry := range entries {
		if d.matchesFilters(entry) {
			d.appendEntryToBuffer(entry)
			addedAny = true
		}
	}
	d.emptyLabel.SetVisible(len(d.displayedEntries) == 0)
	d.copyBtn.SetSensitive(len(d.displayedEntries) > 0)
	if addedAny && d.followBtn.Active() {
		d.scrollToBottom()
	}
}

func (d *mcpDetailView) rebuildLogBuffer() {
	d.displayedEntries = nil
	d.logBuf.SetText("")
	for _, entry := range d.rawEntries {
		if d.matchesFilters(entry) {
			d.appendEntryToBuffer(entry)
		}
	}
	d.emptyLabel.SetVisible(len(d.displayedEntries) == 0)
	d.copyBtn.SetSensitive(len(d.displayedEntries) > 0)
	if d.followBtn.Active() {
		d.scrollToBottom()
	}
}

func (d *mcpDetailView) matchesFilters(entry opencode.McpLogEntry) bool {
	if !entry.Compacted {
		minRank := int(d.levelDrop.Selected())
		if mcpLogLevelRank(entry.Level) < minRank {
			return false
		}
	}
	q := strings.ToLower(strings.TrimSpace(d.logSearch.Text()))
	if q == "" {
		return true
	}
	if strings.Contains(strings.ToLower(entry.Message), q) ||
		strings.Contains(strings.ToLower(entry.Server), q) ||
		strings.Contains(strings.ToLower(entry.Error), q) ||
		strings.Contains(strings.ToLower(entry.Output), q) ||
		strings.Contains(strings.ToLower(entry.Directory), q) {
		return true
	}
	for _, f := range entry.Extras {
		if strings.Contains(strings.ToLower(f.Key), q) || strings.Contains(strings.ToLower(f.Value), q) {
			return true
		}
	}
	return false
}

func mcpLogLevelRank(level string) int {
	switch strings.ToUpper(level) {
	case "DEBUG":
		return 0
	case "INFO":
		return 1
	case "WARN", "WARNING":
		return 2
	case "ERROR", "FATAL":
		return 3
	default:
		return 1
	}
}

func (d *mcpDetailView) appendEntryToBuffer(entry opencode.McpLogEntry) {
	beforeLines := d.logBuf.LineCount()
	insertTagged := func(text string, tag *gtk.TextTag) {
		if text == "" {
			return
		}
		startOff := d.logBuf.EndIter().Offset()
		endIter := d.logBuf.EndIter()
		d.logBuf.Insert(endIter, text)
		if tag != nil {
			sIter := d.logBuf.IterAtOffset(startOff)
			eIter := d.logBuf.EndIter()
			d.logBuf.ApplyTag(tag, sIter, eIter)
		}
	}

	if entry.Compacted {
		insertTagged("--- "+entry.Message+" ---\n", d.tagDim)
	} else {
		ts := entry.RawTime
		if !entry.Timestamp.IsZero() {
			ts = entry.Timestamp.Local().Format("15:04:05.000")
		}
		if ts != "" {
			insertTagged(ts+" ", d.tagDim)
		}

		lvl := fmt.Sprintf("%-5s", strings.ToUpper(entry.Level))
		var lvlTag *gtk.TextTag
		switch strings.ToUpper(entry.Level) {
		case "DEBUG":
			lvlTag = d.tagDebug
		case "WARN", "WARNING":
			lvlTag = d.tagWarn
		case "ERROR", "FATAL":
			lvlTag = d.tagError
		default:
			lvlTag = d.tagInfo
		}
		insertTagged(lvl+" ", lvlTag)

		if d.serverName == "" && entry.Server != "" {
			insertTagged("["+entry.Server+"] ", d.tagServer)
		}
		if entry.Directory != "" && normalizeProjectDir(entry.Directory) != normalizeProjectDir(d.mgr.dir) {
			insertTagged("dir="+shortDir(entry.Directory)+" ", d.tagDim)
		}

		if entry.Message != "" {
			insertTagged(entry.Message, d.tagMsg)
		}
		if errText := strings.TrimRight(entry.Error, "\r\n"); errText != "" {
			insertTagged(" error="+errText, d.tagError)
		}
		if outText := strings.TrimRight(entry.Output, "\r\n"); outText != "" {
			insertTagged(" | "+outText, d.tagExtra)
		}
		for _, f := range entry.Extras {
			insertTagged(fmt.Sprintf(" %s=%s", f.Key, f.Value), d.tagExtra)
		}
		insertTagged("\n", nil)
	}

	addedLines := d.logBuf.LineCount() - beforeLines
	if addedLines < 1 {
		addedLines = 1
	}
	d.displayedEntries = append(d.displayedEntries, displayedLogEntry{lineCount: addedLines})

	for len(d.displayedEntries) > mcpMaxLogEntries {
		oldest := d.displayedEntries[0]
		d.displayedEntries = d.displayedEntries[1:]
		start := d.logBuf.StartIter()
		end, _ := d.logBuf.IterAtLine(oldest.lineCount)
		d.logBuf.Delete(start, end)
	}
}

func (d *mcpDetailView) scrollToBottom() {
	glib.IdleAdd(func() {
		if !d.mgr.isOpen() || d.mgr.detail != d || d.vadj == nil {
			return
		}
		target := d.vadj.Upper() - d.vadj.PageSize()
		if target < 0 {
			target = 0
		}
		d.programmaticScroll = true
		d.vadj.SetValue(target)
		d.programmaticScroll = false
	})
}
