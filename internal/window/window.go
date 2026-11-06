// Package window implements the main GTK4 / libadwaita UI window and conversation components for Gluppencode.
package window

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4-sourceview/pkg/gtksource/v5"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"github.com/diamondburned/gotkit/app"
	"github.com/diamondburned/gotkit/components/autoscroll"
	"github.com/diamondburned/gotkit/gtkutil"
	"libdb.so/gluppencode/internal/opencode"
)

//go:embed window.ui
var windowUI string

type pendingPrompt struct {
	inboxID   string
	text      string
	files     []opencode.FileAttachment
	delivery  opencode.InboxDelivery
	cancelled bool
	widget    *UserMessageWidget
}

type convTimeline struct {
	session        opencode.Session
	scroll         *autoscroll.Window
	timelineBox    *gtk.Box
	assistantMap   map[string]*AssistantMessageWidget
	compactionMap  map[string]*CompactionWidget
	messageWidgets map[string]gtk.Widgetter
	revertBanner   *RevertBannerWidget
	pendingPrompts []*pendingPrompt
	seenMsgIDs     map[string]bool
	messages       []opencode.Message
	nextCursor     string
	hasOlder       bool
	loadingOlder   bool
	loadingInitial bool
	loaded         bool
	active         bool
	runningTools   int
	streamStart    time.Time
	streamEnd      time.Time
	streamedChars  int
	lastTokRate    string
}

func (c *convTimeline) closeStreams() {
	for _, aw := range c.assistantMap {
		aw.CloseStreams()
	}
}

// steeredInFlight reports whether the user steered p before its inbox ID was known,
// so the server still holds it under the queued delivery it was sent with.
func (p *pendingPrompt) steeredInFlight(serverDelivery opencode.InboxDelivery) bool {
	return p.inboxID == "" && p.delivery == opencode.DeliverySteer && serverDelivery == opencode.DeliveryQueue
}

func (c *convTimeline) keepPendingAtBottom() {
	if c == nil || c.timelineBox == nil {
		return
	}
	for _, p := range c.pendingPrompts {
		if p != nil && p.widget != nil && p.widget.Parent() != nil {
			if last := c.timelineBox.LastChild(); last != nil && gtk.BaseWidget(last) != gtk.BaseWidget(p.widget) {
				c.timelineBox.ReorderChildAfter(p.widget, last)
			}
		}
	}
	if c.revertBanner != nil && c.revertBanner.Visible() && c.revertBanner.Parent() != nil {
		if last := c.timelineBox.LastChild(); last != nil && gtk.BaseWidget(last) != gtk.BaseWidget(c.revertBanner) {
			c.timelineBox.ReorderChildAfter(c.revertBanner, last)
		}
	}
}

type switcherIndicator struct {
	box     *gtk.Box
	spinner *adw.Spinner
	label   *gtk.Label
}

type sessionTab struct {
	page             *adw.ViewStackPage
	stack            *gtk.Stack
	main             *convTimeline
	subagents        []opencode.Session
	subTimelines     map[string]*convTimeline
	activeSubID      string
	gitBranch        string
	sidebarIndicator *switcherIndicator
	headerIndicator  *switcherIndicator
}

func (t *sessionTab) activeConv() *convTimeline {
	if t.activeSubID != "" {
		if sub, ok := t.subTimelines[t.activeSubID]; ok {
			return sub
		}
	}
	return t.main
}

type agentRowData struct {
	row     *gtk.ListBoxRow
	agentID string
}

type modelRowData struct {
	row   *gtk.ListBoxRow
	model opencode.ModelInfo
	text  string
}

type subagentRowData struct {
	row     *gtk.ListBoxRow
	session opencode.Session
}

// Window is the main Gluppencode window.
type Window struct {
	*adw.ApplicationWindow
	appWin *app.Window
	ctx    context.Context
	cancel context.CancelFunc
	client *opencode.Client

	rootContent          *adw.OverlaySplitView
	hideSidebarBtn       *gtk.Button
	newSessionSidebarBtn *gtk.Button
	searchSessionsBtn    *gtk.Button
	viewSwitcherSidebar  *adw.ViewSwitcherSidebar

	bottomSheet           *adw.BottomSheet
	showSidebarBtn        *gtk.Button
	newSessionTopBtn      *gtk.Button
	headerTitleStack      *gtk.Stack
	windowTitle           *adw.WindowTitle
	breadcrumbParentBtn   *gtk.Button
	breadcrumbParentLabel *gtk.Label
	breadcrumbChildLabel  *gtk.Label
	horizTabScroll        *gtk.ScrolledWindow
	horizTabBox           *gtk.Box
	newWindowBtn          *gtk.Button
	refreshBtn            *gtk.Button
	tabModeBtn            *gtk.ToggleButton
	headerSpinner         *adw.Spinner
	viewStack             *adw.ViewStack

	inputView      *gtksource.View
	inputBuffer    *gtksource.Buffer
	sendBtn        *gtk.Button
	stopBtn        *gtk.Button
	attachBtn      *gtk.Button
	attachmentTray *adw.WrapBox
	pendingFiles   []opencode.FileAttachment

	composerPlaceholder *gtk.Label
	composerAgentBtn    *gtk.MenuButton
	composerModelBtn    *gtk.MenuButton
	agentPopover        *gtk.Popover
	modelPopover        *gtk.Popover

	commands           []opencode.CommandInfo
	skills             []opencode.SkillInfo
	completionProvider *gtksource.CompletionProvider

	agentPillLabel      *gtk.Label
	modelStatusLabel    *gtk.Label
	projectStatusBox    *gtk.Box
	projectStatusLabel  *gtk.Label
	branchStatusBox     *gtk.Box
	branchStatusLabel   *gtk.Label
	shellStatusBox      *gtk.Box
	shellStatusLabel    *gtk.Label
	subagentStatusBox   *gtk.Box
	subagentStatusLabel *gtk.Label
	tokRateStatusLabel  *gtk.Label
	contextStatusLabel  *gtk.Label

	sheetEmptyLabel      *gtk.Label
	shellsSectionBox     *gtk.Box
	shellListBox         *gtk.ListBox
	subagentsSectionBox  *gtk.Box
	sheetBackToParentBtn *gtk.Button
	subagentListBox      *gtk.ListBox
	agentListBox         *gtk.ListBox
	modelSearchEntry     *gtk.SearchEntry
	modelListBox         *gtk.ListBox

	activeShells map[string]opencode.ShellItem

	tabs         map[string]*sessionTab // keyed by root session ID
	tabOrder     []string               // root session IDs in display order
	childToRoot  map[string]string      // child session ID -> root session ID
	agents       []opencode.Agent
	agentRows    []agentRowData
	models       []opencode.ModelInfo
	modelNames   map[string]string
	modelRows    []modelRowData
	subagentRows []subagentRowData
	modelLimits  map[string]int
	branchCache  map[string]string

	modelLastUsed      map[string]int64
	showAllModels      bool
	showAllModelsRow   *gtk.ListBoxRow
	showAllModelsLabel *gtk.Label

	expandedProjects map[string]bool
	allSessions      []opencode.Session
	projectSessions  map[string][]opencode.Session

	horizontalTabs bool
	isMobile       bool
	populating     bool
}

// NewWindow creates a new main window from window.ui.
func NewWindow(ctx context.Context, client *opencode.Client) *Window {
	gtksource.Init()
	if disp := gdk.DisplayGetDefault(); disp != nil {
		gtk.IconThemeGetForDisplay(disp).AddResourcePath("/so/libdb/gluppencode/icons")
	}
	a := app.FromContext(ctx)
	adwWin := adw.NewApplicationWindow(a.Application)
	adwWin.SetTitle("Gluppencode")
	adwWin.SetSizeRequest(720, 560)
	adwWin.SetDefaultSize(1180, 780)

	winCtx, cancel := context.WithCancel(ctx)
	win := app.WrapWindow(a, &adwWin.ApplicationWindow)
	winCtx = app.WithWindow(winCtx, win)

	builder := gtk.NewBuilderFromString(windowUI)

	w := &Window{
		ApplicationWindow:     adwWin,
		appWin:                win,
		ctx:                   winCtx,
		cancel:                cancel,
		client:                client,
		rootContent:           builder.GetObject("rootContent").Cast().(*adw.OverlaySplitView),
		hideSidebarBtn:        builder.GetObject("hideSidebarBtn").Cast().(*gtk.Button),
		newSessionSidebarBtn:  builder.GetObject("newSessionSidebarBtn").Cast().(*gtk.Button),
		searchSessionsBtn:     builder.GetObject("searchSessionsBtn").Cast().(*gtk.Button),
		viewSwitcherSidebar:   builder.GetObject("viewSwitcherSidebar").Cast().(*adw.ViewSwitcherSidebar),
		bottomSheet:           builder.GetObject("bottomSheet").Cast().(*adw.BottomSheet),
		showSidebarBtn:        builder.GetObject("showSidebarBtn").Cast().(*gtk.Button),
		newSessionTopBtn:      builder.GetObject("newSessionTopBtn").Cast().(*gtk.Button),
		headerTitleStack:      builder.GetObject("headerTitleStack").Cast().(*gtk.Stack),
		windowTitle:           builder.GetObject("windowTitle").Cast().(*adw.WindowTitle),
		breadcrumbParentBtn:   builder.GetObject("breadcrumbParentBtn").Cast().(*gtk.Button),
		breadcrumbParentLabel: builder.GetObject("breadcrumbParentLabel").Cast().(*gtk.Label),
		breadcrumbChildLabel:  builder.GetObject("breadcrumbChildLabel").Cast().(*gtk.Label),
		horizTabScroll:        builder.GetObject("horizTabScroll").Cast().(*gtk.ScrolledWindow),
		horizTabBox:           builder.GetObject("horizTabBox").Cast().(*gtk.Box),
		newWindowBtn:          builder.GetObject("newWindowBtn").Cast().(*gtk.Button),
		refreshBtn:            builder.GetObject("refreshBtn").Cast().(*gtk.Button),
		tabModeBtn:            builder.GetObject("tabModeBtn").Cast().(*gtk.ToggleButton),
		headerSpinner:         builder.GetObject("headerSpinner").Cast().(*adw.Spinner),
		viewStack:             builder.GetObject("viewStack").Cast().(*adw.ViewStack),
		inputView:             builder.GetObject("inputView").Cast().(*gtksource.View),
		sendBtn:               builder.GetObject("sendBtn").Cast().(*gtk.Button),
		stopBtn:               builder.GetObject("stopBtn").Cast().(*gtk.Button),
		attachBtn:             builder.GetObject("attachBtn").Cast().(*gtk.Button),
		attachmentTray:        builder.GetObject("attachmentTray").Cast().(*adw.WrapBox),
		composerPlaceholder:   builder.GetObject("composerPlaceholder").Cast().(*gtk.Label),
		composerAgentBtn:      builder.GetObject("composerAgentBtn").Cast().(*gtk.MenuButton),
		composerModelBtn:      builder.GetObject("composerModelBtn").Cast().(*gtk.MenuButton),
		agentPopover:          builder.GetObject("agentPopover").Cast().(*gtk.Popover),
		modelPopover:          builder.GetObject("modelPopover").Cast().(*gtk.Popover),
		agentPillLabel:        builder.GetObject("agentPillLabel").Cast().(*gtk.Label),
		modelStatusLabel:      builder.GetObject("modelStatusLabel").Cast().(*gtk.Label),
		projectStatusBox:      builder.GetObject("projectStatusBox").Cast().(*gtk.Box),
		projectStatusLabel:    builder.GetObject("projectStatusLabel").Cast().(*gtk.Label),
		branchStatusBox:       builder.GetObject("branchStatusBox").Cast().(*gtk.Box),
		branchStatusLabel:     builder.GetObject("branchStatusLabel").Cast().(*gtk.Label),
		shellStatusBox:        builder.GetObject("shellStatusBox").Cast().(*gtk.Box),
		shellStatusLabel:      builder.GetObject("shellStatusLabel").Cast().(*gtk.Label),
		subagentStatusBox:     builder.GetObject("subagentStatusBox").Cast().(*gtk.Box),
		subagentStatusLabel:   builder.GetObject("subagentStatusLabel").Cast().(*gtk.Label),
		tokRateStatusLabel:    builder.GetObject("tokRateStatusLabel").Cast().(*gtk.Label),
		contextStatusLabel:    builder.GetObject("contextStatusLabel").Cast().(*gtk.Label),
		sheetEmptyLabel:       builder.GetObject("sheetEmptyLabel").Cast().(*gtk.Label),
		shellsSectionBox:      builder.GetObject("shellsSectionBox").Cast().(*gtk.Box),
		shellListBox:          builder.GetObject("shellListBox").Cast().(*gtk.ListBox),
		subagentsSectionBox:   builder.GetObject("subagentsSectionBox").Cast().(*gtk.Box),
		sheetBackToParentBtn:  builder.GetObject("sheetBackToParentBtn").Cast().(*gtk.Button),
		subagentListBox:       builder.GetObject("subagentListBox").Cast().(*gtk.ListBox),
		agentListBox:          builder.GetObject("agentListBox").Cast().(*gtk.ListBox),
		modelSearchEntry:      builder.GetObject("modelSearchEntry").Cast().(*gtk.SearchEntry),
		modelListBox:          builder.GetObject("modelListBox").Cast().(*gtk.ListBox),
		tabs:                  make(map[string]*sessionTab),
		childToRoot:           make(map[string]string),
		modelNames:            make(map[string]string),
		modelLimits:           make(map[string]int),
		branchCache:           make(map[string]string),
		modelLastUsed:         make(map[string]int64),
		expandedProjects:      make(map[string]bool),
		projectSessions:       make(map[string][]opencode.Session),
		activeShells:          make(map[string]opencode.ShellItem),
	}
	inputScroll := builder.GetObject("inputScroll").Cast().(*gtk.ScrolledWindow)
	w.inputBuffer = w.inputView.Buffer().Cast().(*gtksource.Buffer)
	w.inputBuffer.SetHighlightSyntax(false)
	w.inputBuffer.SetHighlightMatchingBrackets(false)

	syncInputBufferScheme := func() {
		styleMgr := adw.StyleManagerGetDefault()
		schemeMgr := gtksource.StyleSchemeManagerGetDefault()
		var scheme *gtksource.StyleScheme
		if styleMgr.Dark() {
			scheme = schemeMgr.Scheme("Adwaita-dark")
			if scheme == nil {
				scheme = schemeMgr.Scheme("classic-dark")
			}
		} else {
			scheme = schemeMgr.Scheme("Adwaita")
			if scheme == nil {
				scheme = schemeMgr.Scheme("classic")
			}
		}
		w.inputBuffer.SetStyleScheme(scheme)
	}
	syncInputBufferScheme()
	adw.StyleManagerGetDefault().NotifyProperty("dark", syncInputBufferScheme)

	w.inputBuffer.ConnectChanged(func() {
		w.composerPlaceholder.SetVisible(w.inputBuffer.CharCount() == 0)
		if w.inputBuffer.LineCount() > 4 {
			inputScroll.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
		} else {
			inputScroll.SetPolicy(gtk.PolicyNever, gtk.PolicyExternal)
		}
		w.syncComposerCompletionPopup()
	})
	w.agentPopover.ConnectMap(func() {
		w.rebuildAgentPills()
	})
	w.modelPopover.ConnectMap(func() {
		w.rebuildModelList()
		w.modelSearchEntry.GrabFocus()
	})
	w.modelPopover.ConnectClosed(func() {
		w.showAllModels = false
		w.modelSearchEntry.SetText("")
		w.filterModelList("")
	})
	w.inputView.SetWrapMode(gtk.WrapWordChar)
	w.inputView.SetAcceptsTab(true)
	w.inputView.SetShowLineNumbers(false)
	w.inputView.SetHighlightCurrentLine(false)
	w.inputView.AddCSSClass("glup-composer-text")
	w.setupCompletion()

	statusBarBox := builder.GetObject("statusBarBox").Cast().(*gtk.Box)
	statusClick := gtk.NewGestureClick()
	statusClick.ConnectReleased(func(_ int, _, _ float64) {
		w.bottomSheet.SetOpen(true)
	})
	statusBarBox.AddController(statusClick)

	// Wire AdwViewStack to vertical sidebar switcher and set up horizontal scrollbar.
	w.viewSwitcherSidebar.SetStack(w.viewStack)

	horizScrollCtrl := gtk.NewEventControllerScroll(gtk.EventControllerScrollBothAxes)
	horizScrollCtrl.ConnectScroll(func(dx, dy float64) bool {
		hadj := w.horizTabScroll.HAdjustment()
		if hadj == nil {
			return false
		}
		delta := dx
		if dy != 0 {
			delta = dy * 30
		}
		hadj.SetValue(hadj.Value() + delta)
		return true
	})
	w.horizTabScroll.AddController(horizScrollCtrl)

	// Clicking an item in the sidebar returns to the main conversation if viewing a subagent,
	// and closes the overlay sidebar if collapsed on mobile.
	w.viewSwitcherSidebar.ConnectActivated(func() {
		if tab := w.currentTab(); tab != nil && tab.activeSubID != "" {
			w.closeSubagent(tab)
		}
		if w.rootContent.Collapsed() {
			w.rootContent.SetShowSidebar(false)
		}
	})

	w.breadcrumbParentBtn.ConnectClicked(func() {
		if tab := w.currentTab(); tab != nil {
			w.closeSubagent(tab)
		}
	})
	w.sheetBackToParentBtn.ConnectClicked(func() {
		if tab := w.currentTab(); tab != nil {
			w.closeSubagent(tab)
			w.bottomSheet.SetOpen(false)
		}
	})

	w.viewStack.NotifyProperty("visible-child-name", func() {
		w.onVisibleTabChanged()
	})

	// Sidebar show/hide & horizontal/vertical tab switching.
	w.hideSidebarBtn.ConnectClicked(func() {
		w.rootContent.SetShowSidebar(false)
		w.syncNavigationChrome()
	})
	w.showSidebarBtn.ConnectClicked(func() {
		if w.horizontalTabs && !w.isMobile {
			w.tabModeBtn.SetActive(false)
			return
		}
		w.rootContent.SetShowSidebar(!w.rootContent.ShowSidebar())
		w.syncNavigationChrome()
	})
	w.rootContent.NotifyProperty("show-sidebar", func() {
		w.syncNavigationChrome()
	})
	w.rootContent.NotifyProperty("collapsed", func() {
		w.isMobile = w.rootContent.Collapsed()
		w.syncNavigationChrome()
	})

	w.tabModeBtn.ConnectToggled(func() {
		w.toggleTabMode()
	})

	// Mobile adaptive breakpoint: always use vertical tabs in collapsed overlay sidebar on narrow screens.
	bp := adw.NewBreakpoint(adw.BreakpointConditionParse("max-width: 680sp"))
	bp.AddSetter(w.rootContent, "collapsed", true)
	adwWin.AddBreakpoint(bp)

	adwWin.ConnectCloseRequest(func() bool {
		cancel()
		for _, tab := range w.tabs {
			tab.main.closeStreams()
			for _, c := range tab.subTimelines {
				c.closeStreams()
			}
		}
		return false
	})

	w.newSessionSidebarBtn.ConnectClicked(w.createNewSession)
	w.searchSessionsBtn.ConnectClicked(w.openSessionSearchDialog)
	w.newSessionTopBtn.ConnectClicked(w.createNewSession)
	w.newWindowBtn.ConnectClicked(w.openNewWindow)
	w.refreshBtn.ConnectClicked(w.loadAll)

	// Window-level shortcuts: Ctrl+Shift+N for New Window, Ctrl+K for Search Sessions.
	winKeyCtrl := gtk.NewEventControllerKey()
	winKeyCtrl.ConnectKeyPressed(func(keyval, _ uint, state gdk.ModifierType) bool {
		if (keyval == gdk.KEY_N || keyval == gdk.KEY_n) && state.Has(gdk.ControlMask) && state.Has(gdk.ShiftMask) {
			w.openNewWindow()
			return true
		}
		if (keyval == gdk.KEY_K || keyval == gdk.KEY_k) && state.Has(gdk.ControlMask) && !state.Has(gdk.ShiftMask) {
			w.openSessionSearchDialog()
			return true
		}
		return false
	})
	adwWin.AddController(winKeyCtrl)

	// Composer Enter / Ctrl+Enter / Shift+Enter handling and Ctrl+V clipboard image pasting.
	steerKeyCtrl := gtk.NewEventControllerKey()
	steerKeyCtrl.SetPropagationPhase(gtk.PhaseCapture)
	steerKeyCtrl.ConnectKeyPressed(func(keyval, _ uint, state gdk.ModifierType) bool {
		if (keyval == gdk.KEY_Return || keyval == gdk.KEY_KP_Enter) && state.Has(gdk.ControlMask) && !state.Has(gdk.ShiftMask) {
			w.sendPromptWithDelivery(opencode.DeliverySteer)
			return true
		}
		return false
	})
	w.inputView.AddController(steerKeyCtrl)

	keyCtrl := gtk.NewEventControllerKey()
	keyCtrl.ConnectKeyPressed(func(keyval, _ uint, state gdk.ModifierType) bool {
		if (keyval == gdk.KEY_Return || keyval == gdk.KEY_KP_Enter) && !state.Has(gdk.ShiftMask) {
			if state.Has(gdk.ControlMask) {
				w.sendPromptWithDelivery(opencode.DeliverySteer)
			} else {
				w.sendPromptWithDelivery(opencode.DeliveryQueue)
			}
			return true
		}
		if (keyval == gdk.KEY_v || keyval == gdk.KEY_V) && state.Has(gdk.ControlMask) {
			if w.clipboardHasImage() {
				w.pasteClipboardImage()
				return true
			}
		}
		if (keyval == gdk.KEY_k || keyval == gdk.KEY_K) && state.Has(gdk.ControlMask) {
			w.openSessionSearchDialog()
			return true
		}
		return false
	})
	w.inputView.AddController(keyCtrl)
	w.inputView.ConnectPasteClipboard(w.onPasteClipboard)
	w.attachBtn.ConnectClicked(w.chooseAndAttachFile)
	w.sendBtn.ConnectClicked(func() {
		w.sendPromptWithDelivery(opencode.DeliveryQueue)
	})
	w.stopBtn.ConnectClicked(w.interruptSession)

	// Subagent selection in the bottom sheet.
	w.subagentListBox.ConnectRowActivated(func(row *gtk.ListBoxRow) {
		if row == nil {
			return
		}
		tab := w.currentTab()
		if tab == nil {
			return
		}
		for _, sr := range w.subagentRows {
			if sr.row.Object.Native() == row.Object.Native() {
				w.openSubagent(tab, sr.session)
				w.bottomSheet.SetOpen(false)
				return
			}
		}
	})

	// Agent selection in the agent popover.
	w.agentListBox.ConnectRowActivated(func(row *gtk.ListBoxRow) {
		if row == nil {
			return
		}
		for _, ar := range w.agentRows {
			if ar.row.Object.Native() == row.Object.Native() {
				w.switchAgent(ar.agentID)
				w.agentPopover.Popdown()
				return
			}
		}
	})

	// Model search and selection inside the bottom sheet.
	w.modelSearchEntry.ConnectSearchChanged(func() {
		w.filterModelList(w.modelSearchEntry.Text())
	})
	w.modelSearchEntry.ConnectActivate(func() {
		for _, mr := range w.modelRows {
			if mr.row.Visible() {
				w.switchModel(opencode.ModelRef{
					ProviderID: mr.model.ProviderID,
					ID:         mr.model.ID,
				})
				w.modelPopover.Popdown()
				return
			}
		}
	})
	w.modelListBox.ConnectRowActivated(func(row *gtk.ListBoxRow) {
		if row == nil {
			return
		}
		if w.showAllModelsRow != nil && row.Object.Native() == w.showAllModelsRow.Object.Native() {
			w.showAllModels = true
			w.filterModelList(w.modelSearchEntry.Text())
			return
		}
		for _, mr := range w.modelRows {
			if mr.row.Object.Native() == row.Object.Native() {
				w.switchModel(opencode.ModelRef{
					ProviderID: mr.model.ProviderID,
					ID:         mr.model.ID,
				})
				w.modelPopover.Popdown()
				return
			}
		}
	})

	adwWin.SetContent(w.rootContent)
	w.syncNavigationChrome()

	// 30s ticker to update tab relative timemarks.
	glib.TimeoutSecondsAdd(30, func() bool {
		if w.ctx.Err() != nil {
			return false
		}
		w.updateSwitcherIndicators()
		return true
	})

	w.loadAll()
	go w.runEventLoop()

	return w
}

func (w *Window) openNewWindow() {
	newWin := NewWindow(app.FromContext(w.ctx).Context(), w.client)
	newWin.Present()
}

func (w *Window) ensureSessionTab(sess opencode.Session) *sessionTab {
	if tab, ok := w.tabs[sess.ID]; ok {
		if sess.Title != "" && tab.main.session.Title != sess.Title {
			tab.main.session.Title = sess.Title
			tab.page.SetTitle(sess.Title)
		}
		if sess.Location.Directory != "" && tab.main.session.Location.Directory == "" {
			tab.main.session.Location = sess.Location
			w.syncSectionStarts()
		}
		return tab
	}
	normDir := normalizeProjectDir(sess.Location.Directory)
	insertIdx := len(w.tabOrder)
	inProject := false
	for i, id := range w.tabOrder {
		t, ok := w.tabs[id]
		if !ok {
			continue
		}
		tDir := normalizeProjectDir(t.main.session.Location.Directory)
		if tDir == normDir {
			if !inProject {
				inProject = true
				insertIdx = i
			}
			if sess.Time.Updated == 0 || sess.Time.Updated >= t.main.session.Time.Updated {
				insertIdx = i
				break
			}
			insertIdx = i + 1
		} else if inProject {
			break
		}
	}

	prevPopulating := w.populating
	prevVisID := w.currentRootSessionID()
	w.populating = true

	tab := w.addSessionTab(sess)
	if insertIdx >= len(w.tabOrder) {
		w.tabOrder = append(w.tabOrder, sess.ID)
	} else {
		w.tabOrder = slices.Insert(w.tabOrder, insertIdx, sess.ID)
		for _, tailID := range w.tabOrder[insertIdx+1:] {
			tailTab, ok := w.tabs[tailID]
			if !ok {
				continue
			}
			title := strings.TrimSpace(tailTab.main.session.Title)
			if title == "" {
				title = "Untitled"
			}
			w.viewStack.Remove(tailTab.stack)
			page := w.viewStack.AddTitled(tailTab.stack, tailID, title)
			page.SetBadgeNumber(0)
			tailTab.page = page
		}
	}
	w.syncSectionStarts()
	if prevVisID != "" && w.tabs[prevVisID] != nil {
		w.viewStack.SetVisibleChildName(prevVisID)
	}
	w.populating = prevPopulating

	found := false
	for i, s := range w.allSessions {
		if s.ID == sess.ID {
			w.allSessions[i] = sess
			found = true
			break
		}
	}
	if !found {
		w.allSessions = append([]opencode.Session{sess}, w.allSessions...)
	}

	if w.projectSessions != nil {
		foundProj := false
		for i, s := range w.projectSessions[normDir] {
			if s.ID == sess.ID {
				w.projectSessions[normDir][i] = sess
				foundProj = true
				break
			}
		}
		if !foundProj {
			w.projectSessions[normDir] = append([]opencode.Session{sess}, w.projectSessions[normDir]...)
		}
	}
	return tab
}

func (w *Window) syncSectionStarts() {
	var lastDir string
	for i, id := range w.tabOrder {
		tab, ok := w.tabs[id]
		if !ok {
			continue
		}
		rawDir := tab.main.session.Location.Directory
		dir := normalizeProjectDir(rawDir)
		if i == 0 || dir != lastDir {
			tab.page.SetStartsSection(true)
			tab.page.SetSectionTitle(projectName(rawDir))
			lastDir = dir
		} else {
			tab.page.SetStartsSection(false)
		}
	}
}

func (w *Window) selectSession(sess opencode.Session) {
	w.ensureSessionTab(sess)
	w.syncProjectSessionVisibility()
	w.viewStack.SetVisibleChildName(sess.ID)
	w.onVisibleTabChanged()
}

func (w *Window) openSessionSearchDialog() {
	searchWin := adw.NewWindow()
	searchWin.SetTitle("Search Sessions")
	searchWin.SetDefaultSize(540, 440)
	searchWin.SetTransientFor(&w.ApplicationWindow.Window)
	searchWin.SetModal(true)

	toolbar := adw.NewToolbarView()
	searchWin.SetContent(toolbar)

	header := adw.NewHeaderBar()
	header.SetShowEndTitleButtons(true)
	titleWidget := adw.NewWindowTitle("Search Sessions", "")
	header.SetTitleWidget(titleWidget)
	toolbar.AddTopBar(header)

	contentBox := gtk.NewBox(gtk.OrientationVertical, 8)
	contentBox.SetMarginTop(8)
	contentBox.SetMarginBottom(8)
	contentBox.SetMarginStart(12)
	contentBox.SetMarginEnd(12)
	toolbar.SetContent(contentBox)

	searchEntry := gtk.NewSearchEntry()
	searchEntry.SetPlaceholderText("Search sessions by title or project…")
	contentBox.Append(searchEntry)

	scrolled := gtk.NewScrolledWindow()
	scrolled.SetVExpand(true)
	scrolled.SetHExpand(true)
	contentBox.Append(scrolled)

	listBox := gtk.NewListBox()
	listBox.AddCSSClass("boxed-list")
	listBox.AddCSSClass("glup-search-list")
	listBox.SetSelectionMode(gtk.SelectionSingle)
	listBox.SetActivateOnSingleClick(true)
	emptySearchLabel := gtk.NewLabel("No matching sessions")
	emptySearchLabel.AddCSSClass("dim-label")
	emptySearchLabel.SetMarginTop(24)
	emptySearchLabel.SetMarginBottom(24)
	listBox.SetPlaceholder(emptySearchLabel)
	scrolled.SetChild(listBox)

	var sessionsLock sync.Mutex
	sessionPool := make([]opencode.Session, len(w.allSessions))
	copy(sessionPool, w.allSessions)

	type matchRow struct {
		sess  opencode.Session
		row   *gtk.ListBoxRow
		score int
	}

	var currentMatches []matchRow

	activateSelected := func() {
		sel := listBox.SelectedRow()
		if sel == nil && len(currentMatches) > 0 {
			sel = currentMatches[0].row
		}
		if sel != nil {
			for _, m := range currentMatches {
				if m.row != nil && m.row.Object.Native() == sel.Object.Native() {
					searchWin.Close()
					w.selectSession(m.sess)
					return
				}
			}
		}
	}

	escCtrl := gtk.NewEventControllerKey()
	escCtrl.SetPropagationPhase(gtk.PhaseCapture)
	escCtrl.ConnectKeyPressed(func(keyval, _ uint, state gdk.ModifierType) bool {
		if keyval == gdk.KEY_Escape {
			searchWin.Close()
			return true
		}
		if keyval == gdk.KEY_Return || keyval == gdk.KEY_KP_Enter {
			activateSelected()
			return true
		}
		return false
	})
	searchWin.AddController(escCtrl)

	searchEntry.ConnectActivate(activateSelected)
	searchEntry.ConnectStopSearch(func() {
		searchWin.Close()
	})
	listBox.ConnectRowActivated(func(row *gtk.ListBoxRow) {
		if row == nil {
			return
		}
		for _, m := range currentMatches {
			if m.row != nil && m.row.Object.Native() == row.Object.Native() {
				searchWin.Close()
				w.selectSession(m.sess)
				return
			}
		}
	})

	updateList := func() {
		sessionsLock.Lock()
		pool := make([]opencode.Session, len(sessionPool))
		copy(pool, sessionPool)
		sessionsLock.Unlock()

		query := searchEntry.Text()

		for c := listBox.FirstChild(); c != nil; {
			next := gtk.BaseWidget(c).NextSibling()
			listBox.Remove(c)
			c = next
		}
		currentMatches = nil

		var matches []matchRow
		for _, sess := range pool {
			if sess.ParentID != "" {
				continue
			}
			title := sess.Title
			dir := sess.Location.Directory
			candidate := fmt.Sprintf("%s %s %s", title, projectName(dir), shortDir(dir))
			score, ok := fuzzyMatchScore(query, candidate)
			if !ok {
				continue
			}
			finalScore := score*1000 + int((sess.Time.Updated/1000)%10000)
			matches = append(matches, matchRow{sess: sess, score: finalScore})
		}

		slices.SortStableFunc(matches, func(a, b matchRow) int {
			if a.score != b.score {
				return b.score - a.score
			}
			if a.sess.Time.Updated != b.sess.Time.Updated {
				if a.sess.Time.Updated > b.sess.Time.Updated {
					return -1
				}
				return 1
			}
			return 0
		})

		for i, m := range matches {
			row := gtk.NewListBoxRow()

			rowBox := gtk.NewBox(gtk.OrientationHorizontal, 8)
			rowBox.SetMarginTop(10)
			rowBox.SetMarginBottom(10)
			rowBox.SetMarginStart(12)
			rowBox.SetMarginEnd(12)

			textVBox := gtk.NewBox(gtk.OrientationVertical, 2)
			textVBox.SetHExpand(true)

			titleText := strings.TrimSpace(m.sess.Title)
			if titleText == "" {
				titleText = "Untitled Session"
			}
			titleLabel := gtk.NewLabel(titleText)
			titleLabel.SetXAlign(0)
			titleLabel.SetEllipsize(pango.EllipsizeEnd)

			dir := m.sess.Location.Directory
			subText := fmt.Sprintf("%s · %s", projectName(dir), shortDir(dir))
			subLabel := gtk.NewLabel(subText)
			subLabel.SetXAlign(0)
			subLabel.SetEllipsize(pango.EllipsizeMiddle)
			subLabel.AddCSSClass("caption")
			subLabel.AddCSSClass("dim-label")

			textVBox.Append(titleLabel)
			textVBox.Append(subLabel)
			rowBox.Append(textVBox)

			if tm := formatRelativeTimemark(m.sess.Time.Updated); tm != "" {
				timeLabel := gtk.NewLabel(tm)
				timeLabel.AddCSSClass("caption")
				timeLabel.AddCSSClass("dim-label")
				timeLabel.SetVAlign(gtk.AlignCenter)
				rowBox.Append(timeLabel)
			}

			row.SetChild(rowBox)
			listBox.Append(row)
			matches[i].row = row
			if i == 0 {
				listBox.SelectRow(row)
			}
		}
		currentMatches = matches
	}

	searchEntry.ConnectSearchChanged(updateList)
	updateList()

	gtkutil.Async(w.ctx, func() func() {
		sessions, err := w.client.ListSessions(w.ctx, 200)
		if err != nil || len(sessions) == 0 {
			return func() {}
		}
		return func() {
			sessionsLock.Lock()
			sessionPool = sessions
			w.allSessions = sessions
			sessionsLock.Unlock()
			updateList()
		}
	})

	searchWin.Present()
	glib.IdleAdd(func() {
		searchEntry.GrabFocus()
	})
}

func (w *Window) openSubagentByID(tab *sessionTab, childID string) {
	if tab == nil || childID == "" {
		return
	}
	for _, sub := range tab.subagents {
		if sub.ID == childID {
			w.openSubagent(tab, sub)
			return
		}
	}
	gtkutil.Async(w.ctx, func() func() {
		sess, err := w.client.GetSession(w.ctx, childID)
		return func() {
			if err != nil {
				slog.Error("failed to get subagent session", "id", childID, "err", err)
				w.openSubagent(tab, opencode.Session{ID: childID, ParentID: tab.main.session.ID, Title: "Subagent"})
				return
			}
			w.openSubagent(tab, *sess)
		}
	})
}

func (w *Window) newConvTimeline(sess opencode.Session) *convTimeline {
	timelineBox := gtk.NewBox(gtk.OrientationVertical, 0)
	timelineBox.AddCSSClass("glup-timeline-box")

	clamp := adw.NewClamp()
	clamp.SetMaximumSize(920)
	clamp.SetTighteningThreshold(680)
	clamp.SetChild(timelineBox)

	scroll := autoscroll.NewWindow()
	scroll.SetVExpand(true)
	scroll.SetHExpand(true)
	scroll.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	scroll.SetChild(clamp)

	conv := &convTimeline{
		session:        sess,
		scroll:         scroll,
		timelineBox:    timelineBox,
		assistantMap:   make(map[string]*AssistantMessageWidget),
		compactionMap:  make(map[string]*CompactionWidget),
		messageWidgets: make(map[string]gtk.Widgetter),
		seenMsgIDs:     make(map[string]bool),
	}

	vadj := scroll.VAdjustment()
	vadj.ConnectValueChanged(func() {
		if vadj.Value() <= 120 && conv.hasOlder && !conv.loadingOlder && conv.loaded {
			tab, _ := w.resolveConv(conv.session.ID)
			w.loadOlderConvMessages(tab, conv)
		}
	})
	vadj.NotifyProperty("upper", func() {
		if conv.hasOlder && !conv.loadingOlder && conv.loaded && vadj.Upper() <= vadj.PageSize()*1.2 {
			tab, _ := w.resolveConv(conv.session.ID)
			if tab != nil {
				w.loadOlderConvMessages(tab, conv)
			}
		}
	})

	return conv
}

func (w *Window) toggleTabMode() {
	w.horizontalTabs = w.tabModeBtn.Active()
	if w.horizontalTabs && !w.isMobile {
		w.rootContent.SetShowSidebar(false)
	} else if !w.horizontalTabs && !w.isMobile {
		w.rootContent.SetShowSidebar(true)
	}
	w.syncProjectSessionVisibility()
	w.syncNavigationChrome()
	w.syncSwitcherIndicators()
}

func (w *Window) syncNavigationChrome() {
	tab := w.currentTab()
	viewingSubagent := tab != nil && tab.activeSubID != ""
	useHorizontal := w.horizontalTabs && !w.isMobile

	switch {
	case viewingSubagent:
		w.headerTitleStack.SetVisibleChildName("breadcrumb")
		sidebarVisible := w.rootContent.ShowSidebar() && !w.rootContent.Collapsed()
		w.showSidebarBtn.SetVisible(!sidebarVisible)
		w.newSessionTopBtn.SetVisible(!sidebarVisible)
	case useHorizontal:
		w.headerTitleStack.SetVisibleChildName("switcher")
		w.showSidebarBtn.SetVisible(false)
		w.newSessionTopBtn.SetVisible(true)
	default:
		w.headerTitleStack.SetVisibleChildName("title")
		sidebarVisible := w.rootContent.ShowSidebar() && !w.rootContent.Collapsed()
		w.showSidebarBtn.SetVisible(!sidebarVisible)
		w.newSessionTopBtn.SetVisible(!sidebarVisible)
	}
	w.tabModeBtn.SetVisible(!w.isMobile)
}

func (w *Window) currentRootSessionID() string {
	if w == nil || w.viewStack == nil {
		return ""
	}
	return w.viewStack.VisibleChildName()
}

func (w *Window) currentTab() *sessionTab {
	id := w.currentRootSessionID()
	if id == "" {
		return nil
	}
	return w.tabs[id]
}

func shortDir(dir string) string {
	if dir == "" {
		return "workspace"
	}
	if home, err := os.UserHomeDir(); err == nil {
		if dir == home {
			return "~"
		}
		sep := string(filepath.Separator)
		if strings.HasPrefix(dir, home+sep) {
			return "~" + dir[len(home):]
		}
	}
	return dir
}

var projectDirCache sync.Map

func normalizeProjectDir(dir string) string {
	if dir == "" {
		return ""
	}
	if cached, ok := projectDirCache.Load(dir); ok {
		return cached.(string)
	}
	cleaned := filepath.Clean(dir)
	resolved := cleaned
	if r, err := filepath.EvalSymlinks(cleaned); err == nil && r != "" {
		resolved = filepath.Clean(r)
	}
	projectDirCache.Store(dir, resolved)
	if dir != cleaned {
		projectDirCache.Store(cleaned, resolved)
	}
	return resolved
}

func projectName(dir string) string {
	cleaned := normalizeProjectDir(dir)
	base := filepath.Base(cleaned)
	if base == "" || base == "/" || base == "." {
		return shortDir(dir)
	}
	return base
}

func formatRelativeTimemark(updatedMs int64) string {
	if updatedMs <= 0 {
		return ""
	}
	t := time.UnixMilli(updatedMs)
	diff := time.Since(t)
	if diff < 0 {
		return "now"
	}
	switch {
	case diff < 60*time.Second:
		return "now"
	case diff < 60*time.Minute:
		return fmt.Sprintf("%dm", int(diff.Minutes()))
	case diff < 24*time.Hour:
		return fmt.Sprintf("%dh", int(diff.Hours()))
	case diff < 14*24*time.Hour:
		return fmt.Sprintf("%dd", int(diff.Hours()/24))
	default:
		return fmt.Sprintf("%dw", int(diff.Hours()/(24*7)))
	}
}

func findFirstHorizontalBox(w gtk.Widgetter) *gtk.Box {
	if w == nil {
		return nil
	}
	base := gtk.BaseWidget(w)
	if base == nil {
		return nil
	}
	if box, ok := base.Cast().(*gtk.Box); ok && box.Orientation() == gtk.OrientationHorizontal {
		return box
	}
	for child := base.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
		if found := findFirstHorizontalBox(child); found != nil {
			return found
		}
	}
	return nil
}

func findListBox(w gtk.Widgetter) *gtk.ListBox {
	if w == nil {
		return nil
	}
	base := gtk.BaseWidget(w)
	if base == nil {
		return nil
	}
	if lb, ok := base.Cast().(*gtk.ListBox); ok {
		return lb
	}
	for child := base.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
		if found := findListBox(child); found != nil {
			return found
		}
	}
	return nil
}

func getOrCreateSwitcherIndicator(hBox *gtk.Box) *switcherIndicator {
	if hBox == nil {
		return nil
	}
	for child := hBox.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
		childBase := gtk.BaseWidget(child)
		if childBase != nil && childBase.HasCSSClass("glup-switcher-suffix") {
			var spinner *adw.Spinner
			var label *gtk.Label
			for inner := childBase.FirstChild(); inner != nil; inner = gtk.BaseWidget(inner).NextSibling() {
				innerBase := gtk.BaseWidget(inner)
				if innerBase == nil {
					continue
				}
				if sp, ok := innerBase.Cast().(*adw.Spinner); ok {
					spinner = sp
				}
				if lbl, ok := innerBase.Cast().(*gtk.Label); ok {
					label = lbl
				}
			}
			if box, ok := childBase.Cast().(*gtk.Box); ok && spinner != nil && label != nil {
				return &switcherIndicator{box: box, spinner: spinner, label: label}
			}
		}
	}

	box := gtk.NewBox(gtk.OrientationHorizontal, 4)
	box.AddCSSClass("glup-switcher-suffix")
	box.SetHAlign(gtk.AlignEnd)
	box.SetVAlign(gtk.AlignCenter)
	box.SetHExpand(false)

	spinner := adw.NewSpinner()
	spinner.SetVisible(false)
	box.Append(spinner)

	label := gtk.NewLabel("")
	label.AddCSSClass("caption")
	label.AddCSSClass("dim-label")
	label.SetVisible(false)
	box.Append(label)

	hBox.SetVAlign(gtk.AlignCenter)
	hBox.Append(box)
	return &switcherIndicator{box: box, spinner: spinner, label: label}
}

func updateRowShowMore(row *gtk.ListBoxRow, hiddenCount int, onExpand func()) {
	origChild := row.Child()
	if origChild == nil {
		return
	}

	var vBox *gtk.Box
	if b, ok := origChild.(*gtk.Box); ok && b.Orientation() == gtk.OrientationVertical && b.HasCSSClass("glup-sidebar-row-vbox") {
		vBox = b
	} else if base := gtk.BaseWidget(origChild); base != nil {
		if b, ok := base.Cast().(*gtk.Box); ok && b.Orientation() == gtk.OrientationVertical && b.HasCSSClass("glup-sidebar-row-vbox") {
			vBox = b
		}
	}

	if hiddenCount <= 0 || onExpand == nil {
		row.RemoveCSSClass("glup-has-show-more")
		if vBox == nil {
			return
		}
		// Remove any existing footer button and unwrap the original horizontal box
		for c := vBox.FirstChild(); c != nil; {
			next := gtk.BaseWidget(c).NextSibling()
			if gtk.BaseWidget(c).HasCSSClass("glup-sidebar-show-more") {
				vBox.Remove(c)
			}
			c = next
		}
		if inner := vBox.FirstChild(); inner != nil {
			vBox.Remove(inner)
			row.SetChild(inner)
		}
		return
	}

	row.AddCSSClass("glup-has-show-more")
	if vBox == nil {
		vBox = gtk.NewBox(gtk.OrientationVertical, 0)
		vBox.AddCSSClass("glup-sidebar-row-vbox")
		row.SetChild(vBox)
		vBox.Append(origChild)
	}

	// Remove any existing footer button
	for c := vBox.FirstChild(); c != nil; c = gtk.BaseWidget(c).NextSibling() {
		if gtk.BaseWidget(c).HasCSSClass("glup-sidebar-show-more") {
			vBox.Remove(c)
			break
		}
	}

	btn := gtk.NewButtonWithLabel(fmt.Sprintf("Show %d more…", hiddenCount))
	btn.AddCSSClass("flat")
	btn.AddCSSClass("caption")
	btn.AddCSSClass("dim-label")
	btn.AddCSSClass("glup-sidebar-show-more")
	btn.SetHAlign(gtk.AlignStart)
	btn.ConnectClicked(onExpand)
	vBox.Append(btn)
}

func newProjectHeaderBox(dir string, includeSeparator bool) *gtk.Box {
	outerBox := gtk.NewBox(gtk.OrientationVertical, 0)
	outerBox.AddCSSClass("glup-sidebar-project-header")
	outerBox.SetMarginStart(14)
	outerBox.SetMarginEnd(10)
	outerBox.SetMarginTop(6)
	outerBox.SetMarginBottom(2)

	if includeSeparator {
		sep := gtk.NewSeparator(gtk.OrientationHorizontal)
		sep.SetMarginTop(4)
		sep.SetMarginBottom(8)
		outerBox.Append(sep)
	}

	hBox := gtk.NewBox(gtk.OrientationHorizontal, 8)
	hBox.AddCSSClass("glup-sidebar-project-header-box")

	folderIcon := gtk.NewImageFromIconName("folder-symbolic")
	folderIcon.AddCSSClass("glup-sidebar-folder-icon")
	folderIcon.SetVAlign(gtk.AlignCenter)
	hBox.Append(folderIcon)

	textBox := gtk.NewBox(gtk.OrientationVertical, 1)
	textBox.SetHExpand(true)

	titleLabel := gtk.NewLabel(projectName(dir))
	titleLabel.AddCSSClass("glup-sidebar-project-title")
	titleLabel.SetXAlign(0)
	titleLabel.SetEllipsize(pango.EllipsizeMiddle)
	titleLabel.SetHExpand(true)
	textBox.Append(titleLabel)

	subtitleLabel := gtk.NewLabel(shortDir(dir))
	subtitleLabel.AddCSSClass("glup-sidebar-project-subtitle")
	subtitleLabel.AddCSSClass("dim-label")
	subtitleLabel.AddCSSClass("caption")
	subtitleLabel.SetXAlign(0)
	subtitleLabel.SetEllipsize(pango.EllipsizeMiddle)
	subtitleLabel.SetHExpand(true)
	textBox.Append(subtitleLabel)

	hBox.Append(textBox)
	outerBox.Append(hBox)
	return outerBox
}

func updateProjectHeaderLabels(root gtk.Widgetter, dir string) {
	if root == nil {
		return
	}
	base := gtk.BaseWidget(root)
	if base == nil {
		return
	}
	var titleLbl, subLbl *gtk.Label
	var visit func(w gtk.Widgetter)
	visit = func(w gtk.Widgetter) {
		bw := gtk.BaseWidget(w)
		if bw == nil {
			return
		}
		if bw.HasCSSClass("glup-sidebar-project-title") {
			if l, ok := bw.Cast().(*gtk.Label); ok {
				titleLbl = l
			}
		} else if bw.HasCSSClass("glup-sidebar-project-subtitle") {
			if l, ok := bw.Cast().(*gtk.Label); ok {
				subLbl = l
			}
		}
		for c := bw.FirstChild(); c != nil; c = gtk.BaseWidget(c).NextSibling() {
			visit(c)
		}
	}
	visit(base)
	if titleLbl != nil {
		titleLbl.SetText(projectName(dir))
	}
	if subLbl != nil {
		subLbl.SetText(shortDir(dir))
	}
}

func updateRowProjectHeader(row *gtk.ListBoxRow, dir string, sectionIdx int) {
	header := row.Header()
	if header == nil {
		return
	}
	headerBase := gtk.BaseWidget(header)
	if headerBase == nil {
		return
	}

	headerBase.AddCSSClass("glup-sidebar-project-header")
	headerBase.SetTooltipText(fmt.Sprintf("%s (%s)", projectName(dir), shortDir(dir)))

	if stack, ok := headerBase.Cast().(*gtk.Stack); ok {
		stack.SetVhomogeneous(false)
		childName := "glup_header"
		if sectionIdx > 0 {
			childName = "glup_header_sep"
		}
		child := stack.ChildByName(childName)
		if child != nil {
			updateProjectHeaderLabels(child, dir)
			stack.SetVisibleChildName(childName)
			return
		}

		hBox := newProjectHeaderBox(dir, sectionIdx > 0)
		stack.AddNamed(hBox, childName)
		stack.SetVisibleChildName(childName)
		return
	}

	if headerBase.HasCSSClass("glup-sidebar-project-header") {
		hasSep := false
		if first := gtk.BaseWidget(headerBase).FirstChild(); first != nil {
			if _, ok := first.(*gtk.Separator); ok {
				hasSep = true
			}
		}
		if hasSep == (sectionIdx > 0) {
			updateProjectHeaderLabels(header, dir)
			return
		}
	}

	hBox := newProjectHeaderBox(dir, sectionIdx > 0)
	hBox.SetTooltipText(fmt.Sprintf("%s (%s)", projectName(dir), shortDir(dir)))
	row.SetHeader(hBox)
}

func (w *Window) syncProjectSessionVisibility() {
	currentID := w.currentRootSessionID()

	projectTabs := make(map[string][]*sessionTab)
	for _, id := range w.tabOrder {
		if tab, ok := w.tabs[id]; ok {
			dir := normalizeProjectDir(tab.main.session.Location.Directory)
			projectTabs[dir] = append(projectTabs[dir], tab)
		}
	}

	for dir, tabs := range projectTabs {
		expanded := w.expandedProjects[dir]
		for i, tab := range tabs {
			if expanded || i < 5 || tab.main.session.ID == currentID {
				tab.page.SetVisible(true)
			} else {
				tab.page.SetVisible(false)
			}
		}
	}
}

func (w *Window) reorderTabsByProject() {
	if len(w.tabOrder) <= 1 {
		return
	}
	slices.SortFunc(w.tabOrder, func(idA, idB string) int {
		tabA := w.tabs[idA]
		tabB := w.tabs[idB]
		if tabA == nil || tabB == nil {
			return 0
		}
		dirA := normalizeProjectDir(tabA.main.session.Location.Directory)
		dirB := normalizeProjectDir(tabB.main.session.Location.Directory)
		if dirA != dirB {
			return cmp.Compare(dirA, dirB)
		}
		timeA := tabA.main.session.Time.Updated
		if timeA == 0 {
			timeA = tabA.main.session.Time.Created
		}
		timeB := tabB.main.session.Time.Updated
		if timeB == 0 {
			timeB = tabB.main.session.Time.Created
		}
		return cmp.Compare(timeB, timeA)
	})

	currID := w.viewStack.VisibleChildName()
	for _, id := range w.tabOrder {
		tab := w.tabs[id]
		if tab == nil {
			continue
		}
		w.viewStack.Remove(tab.stack)
		title := tab.main.session.Title
		if strings.TrimSpace(title) == "" {
			title = "Untitled"
		}
		tab.page = w.viewStack.AddTitled(tab.stack, id, title)
	}
	if currID != "" {
		w.viewStack.SetVisibleChildName(currID)
	}
}

func (w *Window) expandProjectSessions(dir string) {
	w.expandedProjects[dir] = true
	all := w.projectSessions[dir]
	var pending []opencode.Session
	for _, sess := range all {
		if _, exists := w.tabs[sess.ID]; !exists {
			pending = append(pending, sess)
		}
	}
	if len(pending) == 0 {
		w.syncProjectSessionVisibility()
		w.syncSwitcherIndicators()
		return
	}

	const chunkSize = 15
	var step func(idx int)
	step = func(idx int) {
		end := idx + chunkSize
		if end > len(pending) {
			end = len(pending)
		}
		for i := idx; i < end; i++ {
			w.ensureSessionTab(pending[i])
		}
		w.reorderTabsByProject()
		w.syncSectionStarts()
		w.syncProjectSessionVisibility()
		w.syncSwitcherIndicators()
		if end < len(pending) {
			glib.IdleAdd(func() bool {
				step(end)
				return false
			})
		}
	}
	step(0)
}

func (w *Window) syncHorizTabStrip() {
	if !w.horizontalTabs || w.isMobile {
		return
	}
	currentID := w.currentRootSessionID()

	for c := w.horizTabBox.FirstChild(); c != nil; {
		next := gtk.BaseWidget(c).NextSibling()
		w.horizTabBox.Remove(c)
		c = next
	}

	var activeBtn *gtk.Button
	for _, sessID := range w.tabOrder {
		tab, ok := w.tabs[sessID]
		if !ok || !tab.page.Visible() {
			continue
		}

		id := sessID
		btn := gtk.NewButton()
		btn.AddCSSClass("flat")
		btn.AddCSSClass("glup-horiz-tab-btn")

		box := gtk.NewBox(gtk.OrientationHorizontal, 6)
		box.SetHExpand(true)
		box.SetVAlign(gtk.AlignCenter)

		rawDir := tab.main.session.Location.Directory
		pName := projectName(rawDir)
		if pName != "" {
			projLabel := gtk.NewLabel(pName)
			projLabel.AddCSSClass("dim-label")
			projLabel.AddCSSClass("caption")
			projLabel.AddCSSClass("glup-horiz-tab-proj")
			projLabel.SetXAlign(0)
			box.Append(projLabel)
		}

		title := strings.TrimSpace(tab.main.session.Title)
		if title == "" {
			title = "Untitled"
		}
		titleLabel := gtk.NewLabel(title)
		titleLabel.AddCSSClass("glup-horiz-tab-title")
		titleLabel.SetEllipsize(pango.EllipsizeEnd)
		titleLabel.SetHExpand(true)
		titleLabel.SetXAlign(0)
		box.Append(titleLabel)

		if tab.main.active {
			spinner := adw.NewSpinner()
			spinner.SetVisible(true)
			box.Append(spinner)
		} else if id != currentID {
			timemark := formatRelativeTimemark(tab.main.session.Time.Updated)
			if timemark != "" {
				ageLabel := gtk.NewLabel(timemark)
				ageLabel.AddCSSClass("dim-label")
				ageLabel.AddCSSClass("caption")
				box.Append(ageLabel)
			}
		}

		btn.SetChild(box)
		btn.SetTooltipText(fmt.Sprintf("%s (%s)", title, shortDir(rawDir)))

		if id == currentID {
			btn.AddCSSClass("glup-horiz-tab-active")
			activeBtn = btn
		}

		btn.ConnectClicked(func() {
			w.viewStack.SetVisibleChildName(id)
			w.onVisibleTabChanged()
		})

		w.horizTabBox.Append(btn)
	}

	if activeBtn != nil {
		glib.IdleAdd(func() bool {
			if rect, ok := activeBtn.ComputeBounds(w.horizTabBox); ok {
				hadj := w.horizTabScroll.HAdjustment()
				if hadj != nil {
					hadj.ClampPage(float64(rect.Origin().X()), float64(rect.Origin().X()+rect.Size().Width()))
				}
			}
			return false
		})
	}
}

func (w *Window) syncSwitcherIndicators() {
	if len(w.tabOrder) == 0 {
		return
	}

	currentID := w.currentRootSessionID()

	// Compute hidden sessions and last visible tab for each project
	projectTabs := make(map[string][]*sessionTab)
	for _, id := range w.tabOrder {
		if tab, ok := w.tabs[id]; ok {
			dir := normalizeProjectDir(tab.main.session.Location.Directory)
			projectTabs[dir] = append(projectTabs[dir], tab)
		}
	}

	lastVisibleID := make(map[string]string)
	hiddenCounts := make(map[string]int)
	for dir, tabs := range projectTabs {
		if w.expandedProjects[dir] {
			continue
		}
		var visibleCount int
		var lastID string
		for _, tab := range tabs {
			if tab.page.Visible() {
				visibleCount++
				lastID = tab.main.session.ID
			}
		}
		totalSessions := len(w.projectSessions[dir])
		if totalSessions == 0 {
			totalSessions = len(tabs)
		}
		hidden := totalSessions - visibleCount
		if hidden > 0 && lastID != "" {
			lastVisibleID[dir] = lastID
			hiddenCounts[dir] = hidden
		}
	}

	var visibleTabOrder []string
	for _, id := range w.tabOrder {
		if tab, ok := w.tabs[id]; ok && tab.page.Visible() {
			visibleTabOrder = append(visibleTabOrder, id)
		}
	}

	// 1. In AdwViewSwitcherSidebar: walk listbox rows.
	if lb := findListBox(w.viewSwitcherSidebar); lb != nil {
		var rowIdx int
		var activeRow *gtk.ListBoxRow
		var sectionIdx int
		for child := lb.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
			childBase := gtk.BaseWidget(child)
			if childBase == nil {
				continue
			}
			row, ok := childBase.Cast().(*gtk.ListBoxRow)
			if !ok {
				continue
			}
			if !row.Visible() || !row.ChildVisible() {
				updateRowShowMore(row, 0, nil)
				continue
			}
			if rowIdx < len(visibleTabOrder) {
				sessID := visibleTabOrder[rowIdx]
				if sessID == currentID {
					activeRow = row
				}
				row.AddCSSClass("glup-sidebar-conversation-row")
				if tab, ok := w.tabs[sessID]; ok {
					rawDir := tab.main.session.Location.Directory
					dir := normalizeProjectDir(rawDir)
					if tab.page.StartsSection() {
						updateRowProjectHeader(row, rawDir, sectionIdx)
						sectionIdx++
					} else if hdr := row.Header(); hdr != nil {
						if hb := gtk.BaseWidget(hdr); hb != nil && hb.HasCSSClass("glup-sidebar-project-header") {
							row.SetHeader(nil)
						}
					}
					if hBox := findFirstHorizontalBox(row.Child()); hBox != nil {
						hBox.AddCSSClass("glup-sidebar-conversation-box")
						tab.sidebarIndicator = getOrCreateSwitcherIndicator(hBox)
					}

					if lastVisibleID[dir] == sessID {
						dirKey := dir
						updateRowShowMore(row, hiddenCounts[dir], func() {
							w.expandProjectSessions(dirKey)
						})
					} else {
						updateRowShowMore(row, 0, nil)
					}
				}
			}
			rowIdx++
		}
		if activeRow != nil {
			lb.SelectRow(activeRow)
		}
	}

	w.syncHorizTabStrip()
	w.updateSwitcherIndicators()
}

func (w *Window) updateIndicator(ind *switcherIndicator, sessID string, tab *sessionTab) {
	if ind == nil || tab == nil {
		return
	}
	focused := (sessID == w.currentRootSessionID())
	active := tab.main.active

	switch {
	case active:
		ind.spinner.SetVisible(true)
		ind.label.SetVisible(false)
		ind.box.SetVisible(true)
	case focused:
		ind.spinner.SetVisible(false)
		ind.label.SetVisible(false)
		ind.box.SetVisible(false)
	default:
		ind.spinner.SetVisible(false)
		timemark := formatRelativeTimemark(tab.main.session.Time.Updated)
		if timemark != "" {
			ind.label.SetLabel(timemark)
			ind.label.SetVisible(true)
			ind.box.SetVisible(true)
		} else {
			ind.label.SetVisible(false)
			ind.box.SetVisible(false)
		}
	}
}

func (w *Window) updateSwitcherIndicators() {
	for sessID, tab := range w.tabs {
		w.updateIndicator(tab.sidebarIndicator, sessID, tab)
		w.updateIndicator(tab.headerIndicator, sessID, tab)
	}
	if w.horizontalTabs && !w.isMobile {
		w.syncHorizTabStrip()
	}
}

func (w *Window) loadAll() {
	// 1. Fetch sessions & active sessions immediately in parallel
	gtkutil.Async(w.ctx, func() func() {
		var (
			sessions []opencode.Session
			sessErr  error
			active   map[string]opencode.ActiveStatus
			wg       sync.WaitGroup
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			sessions, sessErr = w.client.ListSessions(w.ctx, 200)
		}()
		go func() {
			defer wg.Done()
			active, _ = w.client.ActiveSessions(w.ctx)
		}()
		wg.Wait()

		return func() {
			if sessErr != nil {
				slog.Error("failed to list sessions", "err", sessErr)
				return
			}
			w.allSessions = sessions
			for _, s := range sessions {
				if s.Model.ID != "" {
					ts := s.Time.Updated
					if ts == 0 {
						ts = s.Time.Created
					}
					w.updateModelLastUsed(s.Model, ts)
				}
			}
			if len(w.models) > 0 {
				w.rebuildModelList()
			}
			w.populateSessionTabs(sessions, active)
		}
	})

	// 2. Fetch metadata (agents, models, commands, skills, shells) in parallel
	gtkutil.Async(w.ctx, func() func() {
		var (
			agents   []opencode.Agent
			models   []opencode.ModelInfo
			commands []opencode.CommandInfo
			skills   []opencode.SkillInfo
			shells   []opencode.ShellItem
			wg       sync.WaitGroup
		)
		wg.Add(5)
		go func() {
			defer wg.Done()
			agents, _ = w.client.ListAgents(w.ctx, "")
		}()
		go func() {
			defer wg.Done()
			models, _ = w.client.ListModels(w.ctx)
		}()
		go func() {
			defer wg.Done()
			commands, _ = w.client.ListCommands(w.ctx, "")
		}()
		go func() {
			defer wg.Done()
			skills, _ = w.client.ListSkills(w.ctx, "")
		}()
		go func() {
			defer wg.Done()
			shells, _ = w.client.ListShells(w.ctx, "")
		}()
		wg.Wait()

		return func() {
			if len(agents) > 0 {
				w.agents = agents
				w.rebuildAgentButtons()
			}
			if len(commands) > 0 {
				w.commands = commands
			}
			if len(skills) > 0 {
				w.skills = skills
			}
			w.activeShells = make(map[string]opencode.ShellItem)
			for _, sh := range shells {
				if sh.Status == "" || sh.Status == "running" {
					w.activeShells[sh.ID] = sh
				}
			}
			w.updateShellStatus()
			w.rebuildShellList()
			if len(models) > 0 {
				w.models = models
				for _, m := range models {
					key := m.ProviderID + "/" + m.ID
					if m.Limit.Context > 0 {
						w.modelLimits[key] = m.Limit.Context
						w.modelLimits[m.ID] = m.Limit.Context
					}
					if m.Name != "" {
						w.modelNames[key] = m.Name
						w.modelNames[m.ID] = m.Name
					}
				}
				w.rebuildModelList()
			}
		}
	})
}

func (w *Window) populateSessionTabs(sessions []opencode.Session, active map[string]opencode.ActiveStatus) {
	prevSelected := w.currentRootSessionID()
	w.populating = true

	var topSessions []opencode.Session
	for _, s := range sessions {
		if s.ParentID != "" {
			continue
		}
		topSessions = append(topSessions, s)
	}

	projectOrder := make(map[string]int)
	var projects []string
	for _, s := range topSessions {
		dir := normalizeProjectDir(s.Location.Directory)
		if _, exists := projectOrder[dir]; !exists {
			projectOrder[dir] = len(projects)
			projects = append(projects, dir)
		}
	}
	slices.SortStableFunc(topSessions, func(a, b opencode.Session) int {
		pa := projectOrder[normalizeProjectDir(a.Location.Directory)]
		pb := projectOrder[normalizeProjectDir(b.Location.Directory)]
		if pa != pb {
			return pa - pb
		}
		if a.Time.Updated != b.Time.Updated {
			if a.Time.Updated > b.Time.Updated {
				return -1
			}
			return 1
		}
		return 0
	})

	w.projectSessions = make(map[string][]opencode.Session)
	for _, s := range topSessions {
		dir := normalizeProjectDir(s.Location.Directory)
		w.projectSessions[dir] = append(w.projectSessions[dir], s)
	}

	for _, tab := range w.tabs {
		tab.main.closeStreams()
		for _, sub := range tab.subTimelines {
			sub.closeStreams()
		}
		w.viewStack.Remove(tab.stack)
	}
	w.tabs = make(map[string]*sessionTab)
	w.childToRoot = make(map[string]string)
	w.tabOrder = make([]string, 0, len(topSessions))

	projCount := make(map[string]int)
	var firstID string
	for _, sess := range topSessions {
		dir := normalizeProjectDir(sess.Location.Directory)
		count := projCount[dir]
		_, isRunning := active[sess.ID]
		if count < 5 || sess.ID == prevSelected || isRunning || w.expandedProjects[dir] {
			if firstID == "" {
				firstID = sess.ID
			}
			w.addSessionTab(sess)
			w.tabOrder = append(w.tabOrder, sess.ID)
			if isRunning {
				w.setConvActive(sess.ID, true)
			}
			projCount[dir] = count + 1
		}
	}
	w.syncSectionStarts()

	targetID := prevSelected
	if _, ok := w.tabs[targetID]; !ok {
		targetID = firstID
	}
	w.populating = false
	if targetID != "" {
		w.syncProjectSessionVisibility()
		w.viewStack.SetVisibleChildName(targetID)
		w.onVisibleTabChanged()
	}

	glib.IdleAdd(func() {
		w.syncProjectSessionVisibility()
		w.syncSwitcherIndicators()
		w.inputView.GrabFocus()
	})
}

func (w *Window) addSessionTab(sess opencode.Session) *sessionTab {
	stack := gtk.NewStack()
	stack.SetTransitionType(gtk.StackTransitionTypeNone)
	stack.SetVExpand(true)
	stack.SetHExpand(true)

	mainConv := w.newConvTimeline(sess)
	stack.AddNamed(mainConv.scroll, "main")
	stack.SetVisibleChildName("main")

	title := strings.TrimSpace(sess.Title)
	if title == "" {
		title = "Untitled"
	}

	page := w.viewStack.AddTitled(stack, sess.ID, title)
	page.SetBadgeNumber(0)

	tab := &sessionTab{
		page:         page,
		stack:        stack,
		main:         mainConv,
		subTimelines: make(map[string]*convTimeline),
	}
	w.tabs[sess.ID] = tab
	return tab
}

func (w *Window) onVisibleTabChanged() {
	if w.populating {
		return
	}
	tab := w.currentTab()
	if tab == nil {
		return
	}
	tab.page.SetNeedsAttention(false)

	w.refreshHeaderAndBottomBar(tab)
	w.syncProjectSessionVisibility()
	w.syncSwitcherIndicators()
	w.refreshShells()
	w.inputView.GrabFocus()

	if !tab.main.loaded && !tab.main.loadingInitial {
		tab.main.loadingInitial = true
		w.loadConvMessages(tab, tab.main)
		w.loadChildSubagents(tab.main.session.ID)
	}
}

func (w *Window) refreshHeaderAndBottomBar(tab *sessionTab) {
	if tab == nil {
		return
	}
	conv := tab.activeConv()
	mainTitle := strings.TrimSpace(tab.main.session.Title)
	if mainTitle == "" {
		mainTitle = "Untitled Session"
	}

	if tab.activeSubID != "" {
		subTitle := strings.TrimSpace(conv.session.Title)
		if subTitle == "" {
			subTitle = conv.session.ID
		}
		w.breadcrumbParentLabel.SetLabel(mainTitle)
		w.breadcrumbChildLabel.SetLabel(subTitle)
		w.sheetBackToParentBtn.SetVisible(true)
	} else {
		w.windowTitle.SetTitle(mainTitle)
		w.windowTitle.SetSubtitle(shortDir(tab.main.session.Location.Directory))
		w.sheetBackToParentBtn.SetVisible(false)
	}

	w.syncNavigationChrome()
	w.updateBusyState(conv.active)
	w.updateBottomBar(tab)
	w.rebuildSubagentList(tab)
}

func (w *Window) openSubagent(tab *sessionTab, subSess opencode.Session) {
	if tab == nil {
		return
	}
	w.childToRoot[subSess.ID] = tab.main.session.ID
	subConv, ok := tab.subTimelines[subSess.ID]
	if !ok {
		subConv = w.newConvTimeline(subSess)
		tab.subTimelines[subSess.ID] = subConv
		tab.stack.AddNamed(subConv.scroll, "sub_"+subSess.ID)
	} else {
		subConv.session = subSess
	}

	tab.activeSubID = subSess.ID
	tab.stack.SetVisibleChildName("sub_" + subSess.ID)

	if !subConv.loaded && !subConv.loadingInitial {
		subConv.loadingInitial = true
		w.loadConvMessages(tab, subConv)
	}
	w.refreshHeaderAndBottomBar(tab)
}

func (w *Window) closeSubagent(tab *sessionTab) {
	if tab == nil || tab.activeSubID == "" {
		return
	}
	tab.activeSubID = ""
	tab.stack.SetVisibleChildName("main")
	w.refreshHeaderAndBottomBar(tab)
}

func (w *Window) loadChildSubagents(rootSessionID string) {
	gtkutil.Async(w.ctx, func() func() {
		children, err := w.client.ListChildSessions(w.ctx, rootSessionID)
		return func() {
			if err != nil {
				slog.Error("failed to list child subagents", "rootID", rootSessionID, "err", err)
				return
			}
			tab, ok := w.tabs[rootSessionID]
			if !ok {
				return
			}
			tab.subagents = children
			for _, child := range children {
				w.childToRoot[child.ID] = rootSessionID
			}
			if w.currentRootSessionID() == rootSessionID {
				w.updateBottomBar(tab)
				w.rebuildSubagentList(tab)
			}
		}
	})
}

func (w *Window) loadConvMessages(tab *sessionTab, conv *convTimeline) {
	sessionID := conv.session.ID
	gtkutil.Async(w.ctx, func() func() {
		var (
			page       *opencode.MessagePage
			err        error
			inboxItems []opencode.InboxItem
			wg         sync.WaitGroup
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			page, err = w.client.ListMessagesPage(w.ctx, sessionID, 50, "")
		}()
		go func() {
			defer wg.Done()
			inboxItems, _ = w.client.ListInbox(w.ctx, sessionID)
		}()
		wg.Wait()

		return func() {
			conv.loadingInitial = false
			if err != nil {
				slog.Error("failed to load messages", "sessionID", sessionID, "err", err)
				return
			}
			conv.loaded = true
			conv.closeStreams()
			for child := conv.timelineBox.FirstChild(); child != nil; child = conv.timelineBox.FirstChild() {
				conv.timelineBox.Remove(child)
			}
			conv.assistantMap = make(map[string]*AssistantMessageWidget)
			conv.compactionMap = make(map[string]*CompactionWidget)
			conv.messageWidgets = make(map[string]gtk.Widgetter)
			conv.revertBanner = nil
			conv.seenMsgIDs = make(map[string]bool)

			if page == nil {
				conv.messages = nil
				conv.nextCursor = ""
				conv.hasOlder = false
				return
			}

			conv.messages = page.Messages
			conv.nextCursor = page.NextCursor
			conv.hasOlder = page.NextCursor != ""

			var runningTools int
			for _, msg := range page.Messages {
				conv.seenMsgIDs[msg.ID] = true
				widget := w.buildMessageWidget(tab, conv, msg)
				if widget != nil {
					conv.messageWidgets[msg.ID] = widget
					conv.timelineBox.Append(widget)
				}
				if msg.Type == "assistant" {
					for _, part := range msg.Content {
						if part.Type == "tool" && (part.State.Status == "running" || part.State.Status == "streaming") {
							runningTools++
						}
					}
				}
			}
			conv.runningTools = runningTools

			// Filter out any pendingPrompts that were already delivered in page.Messages
			remainingPending := conv.pendingPrompts[:0]
			for _, p := range conv.pendingPrompts {
				if p == nil || p.cancelled || (p.inboxID != "" && conv.seenMsgIDs[p.inboxID]) {
					continue
				}
				remainingPending = append(remainingPending, p)
				if p.widget != nil {
					conv.timelineBox.Append(p.widget)
				}
			}
			conv.pendingPrompts = remainingPending

			for _, item := range inboxItems {
				if item.ID == "" || conv.seenMsgIDs[item.ID] || (item.Type != "" && item.Type != "user") {
					continue
				}
				w.onInboxEnqueued(tab, conv, opencode.InboxEvent{
					SessionID: sessionID,
					InboxID:   item.ID,
					Delivery:  item.Delivery,
					Item:      &item,
				})
			}

			tab.page.SetBadgeNumber(0)
			w.applyRevertState(tab, conv)
			conv.keepPendingAtBottom()
			conv.scroll.ScrollToBottom()
			glib.TimeoutAdd(50, func() bool {
				conv.scroll.ScrollToBottom()
				return false
			})

			vadj := conv.scroll.VAdjustment()
			glib.IdleAdd(func() {
				if conv.hasOlder && !conv.loadingOlder && conv.loaded && vadj.Upper() <= vadj.PageSize()*1.2 {
					tab, _ := w.resolveConv(conv.session.ID)
					if tab != nil {
						w.loadOlderConvMessages(tab, conv)
					}
				}
			})

			if w.currentTab() == tab {
				w.updateBottomBar(tab)
			}
			w.updateSwitcherIndicators()
		}
	})
}

func (w *Window) loadOlderConvMessages(tab *sessionTab, conv *convTimeline) {
	if conv == nil || conv.loadingOlder || !conv.hasOlder {
		return
	}
	conv.loadingOlder = true
	sessionID := conv.session.ID
	cursor := conv.nextCursor

	gtkutil.Async(w.ctx, func() func() {
		page, err := w.client.ListMessagesPage(w.ctx, sessionID, 50, cursor)
		return func() {
			conv.loadingOlder = false
			if err != nil {
				slog.Error("failed to load older messages", "sessionID", sessionID, "err", err)
				return
			}
			if page == nil {
				conv.hasOlder = false
				return
			}
			conv.nextCursor = page.NextCursor
			conv.hasOlder = page.NextCursor != ""

			if len(page.Messages) == 0 {
				return
			}

			unlock := conv.scroll.LockScroll()
			defer glib.IdleAdd(unlock)

			conv.messages = append(page.Messages, conv.messages...)

			for i := len(page.Messages) - 1; i >= 0; i-- {
				msg := page.Messages[i]
				if conv.seenMsgIDs[msg.ID] {
					continue
				}
				conv.seenMsgIDs[msg.ID] = true
				widget := w.buildMessageWidget(tab, conv, msg)
				if widget != nil {
					conv.messageWidgets[msg.ID] = widget
					conv.timelineBox.Prepend(widget)
				}
			}

			w.applyRevertState(tab, conv)

			vadj := conv.scroll.VAdjustment()
			glib.IdleAdd(func() {
				if conv.hasOlder && !conv.loadingOlder && conv.loaded && vadj.Upper() <= vadj.PageSize()*1.2 {
					tab, _ := w.resolveConv(conv.session.ID)
					if tab != nil {
						w.loadOlderConvMessages(tab, conv)
					}
				}
			})
		}
	})
}

func (w *Window) buildMessageWidget(tab *sessionTab, conv *convTimeline, msg opencode.Message) gtk.Widgetter {
	switch msg.Type {
	case "user":
		if strings.TrimSpace(msg.Text) == "" && len(msg.Files) == 0 {
			return nil
		}
		uw := NewUserMessageWidget(w.ctx, msg.Text, msg.Files...)
		if msg.ID != "" {
			msgID := msg.ID
			msgText := msg.Text
			uw.SetOnUndo(func() {
				w.stageRevertToMessage(tab, conv, msgID, msgText)
			})
		}
		return uw

	case "assistant":
		if len(msg.Content) == 0 {
			return nil
		}
		if msg.Agent != "" {
			conv.session.Agent = msg.Agent
		}
		if msg.Model.ID != "" {
			conv.session.Model = msg.Model
		}
		aw := NewAssistantMessageWidget(w.ctx, msg.Agent, msg.Model.String(), func(subID string) {
			w.openSubagentByID(tab, subID)
		})
		aw.LoadContent(msg.Content)
		conv.assistantMap[msg.ID] = aw
		return aw

	case "compaction":
		cw := NewCompactionWidget(w.ctx, msg)
		conv.compactionMap[msg.ID] = cw
		return cw

	case "synthetic":
		return NewSyntheticSubagentWidget(w.ctx, msg, func(subID string) {
			w.openSubagentByID(tab, subID)
		})

	case "agent-switched":
		return NewAgentSwitchedWidget(msg)

	default:
		return nil
	}
}

func (w *Window) reconcileTail(sessionID string) {
	tab, conv := w.resolveConv(sessionID)
	if conv == nil || !conv.loaded {
		return
	}
	gtkutil.Async(w.ctx, func() func() {
		page, err := w.client.ListMessagesPage(w.ctx, sessionID, 10, "")
		return func() {
			if err != nil || page == nil {
				return
			}
			var appended bool
			findPrevWidget := func(idx int) gtk.Widgetter {
				for i := idx - 1; i >= 0; i-- {
					if pw := conv.messageWidgets[conv.messages[i].ID]; pw != nil {
						return pw
					}
				}
				return nil
			}

			for _, msg := range page.Messages {
				if cw, ok := conv.compactionMap[msg.ID]; ok && msg.Type == "compaction" {
					cw.Update(msg)
				}
			}

			reconcileConvMessages(
				&conv.messages,
				conv.seenMsgIDs,
				&conv.pendingPrompts,
				page.Messages,
				func(id string) bool {
					return conv.assistantMap[id] != nil
				},
				func(msg opencode.Message, insertIdx int, matched *pendingPrompt) {
					if matched != nil {
						msgID := msg.ID
						msgText := msg.Text
						if matched.widget != nil {
							matched.widget.SetWaiting(false, "")
							conv.messageWidgets[msgID] = matched.widget
							matched.widget.SetOnUndo(func() {
								w.stageRevertToMessage(tab, conv, msgID, msgText)
							})
							conv.timelineBox.ReorderChildAfter(matched.widget, findPrevWidget(insertIdx))
						}
						return
					}
					widget := w.buildMessageWidget(tab, conv, msg)
					if widget != nil {
						conv.messageWidgets[msg.ID] = widget
						if insertIdx < len(conv.messages)-1 {
							conv.timelineBox.InsertChildAfter(widget, findPrevWidget(insertIdx))
						} else {
							conv.timelineBox.Append(widget)
						}
						appended = true
					}
				},
			)
			conv.keepPendingAtBottom()

			if appended {
				conv.scroll.ScrollToBottom()
			}
			if tab != nil && w.currentTab() == tab {
				w.updateBottomBar(tab)
			}
			w.updateSwitcherIndicators()
		}
	})
}

func formatAgentDisplayName(agent string) string {
	if agent == "" {
		return "Build"
	}
	r, size := utf8.DecodeRuneInString(agent)
	return string(unicode.ToUpper(r)) + agent[size:]
}

func (w *Window) onStreamDelta(tab *sessionTab, conv *convTimeline, deltaLen int) {
	if deltaLen <= 0 {
		return
	}
	if conv.streamStart.IsZero() {
		conv.streamStart = time.Now()
	}
	conv.streamedChars += deltaLen
	dur := time.Since(conv.streamStart).Seconds()
	if dur >= 0.15 && conv.streamedChars > 0 {
		rate := (float64(conv.streamedChars) / 4.0) / dur
		conv.lastTokRate = fmt.Sprintf("%.1f tok/s", rate)
		if w.currentTab() == tab && tab != nil && tab.activeConv() == conv {
			w.tokRateStatusLabel.SetLabel(conv.lastTokRate)
			w.tokRateStatusLabel.SetVisible(true)
		}
	}
}

func (w *Window) updateBottomBar(tab *sessionTab) {
	if tab == nil {
		w.tokRateStatusLabel.SetVisible(false)
		return
	}
	conv := tab.activeConv()
	agent := conv.session.Agent
	if agent == "" {
		agent = "build"
	}
	agentLower := strings.ToLower(agent)

	// Agent button label in composer row 2
	w.agentPillLabel.SetLabel(formatAgentDisplayName(agent))
	for _, cls := range []string{
		"glup-agent-build", "glup-agent-plan", "glup-agent-bare", "glup-agent-orchestrator",
		"glup-agent-text-build", "glup-agent-text-plan", "glup-agent-text-bare", "glup-agent-text-orchestrator",
	} {
		w.agentPillLabel.RemoveCSSClass(cls)
	}
	switch agentLower {
	case "build", "plan", "bare", "orchestrator":
		w.agentPillLabel.AddCSSClass("glup-agent-text-" + agentLower)
	}

	// Model button label in composer row 2
	modelKey := conv.session.Model.String()
	modelDisplay := w.modelNames[modelKey]
	if modelDisplay == "" {
		modelDisplay = w.modelNames[conv.session.Model.ID]
	}
	if modelDisplay == "" {
		modelDisplay = conv.session.Model.ID
	}
	if modelDisplay == "" {
		modelDisplay = "Default Model"
	}
	w.modelStatusLabel.SetLabel(modelDisplay)

	// Left: Project badge and separate Git branch badge
	dir := tab.main.session.Location.Directory
	projName := filepath.Base(dir)
	if projName == "" || projName == "." || projName == "/" {
		projName = "workspace"
	}
	w.projectStatusLabel.SetLabel(projName)
	if tab.gitBranch == "" {
		tab.gitBranch = w.getGitBranch(dir)
	}
	if tab.gitBranch != "" {
		w.branchStatusLabel.SetLabel(tab.gitBranch)
		w.branchStatusBox.SetVisible(true)
	} else {
		w.branchStatusBox.SetVisible(false)
	}

	// Right: Active shells
	w.updateShellStatus()

	// Right: Active subagents count (only active > 0)
	var activeCount int
	for _, sub := range tab.subagents {
		if sub.Outcome == "" {
			activeCount++
		}
	}
	if activeCount > 0 {
		noun := "subagents"
		if activeCount == 1 {
			noun = "subagent"
		}
		w.subagentStatusLabel.SetLabel(fmt.Sprintf("%d %s", activeCount, noun))
		w.subagentStatusBox.SetVisible(true)
	} else {
		w.subagentStatusBox.SetVisible(false)
	}

	// Right: tok/s rate
	var tokRate string
	if !conv.active || conv.streamStart.IsZero() {
		tokRate = opencode.FormatMessageTokPerSec(conv.messages)
		if tokRate == "" {
			tokRate = conv.lastTokRate
		}
	} else {
		tokRate = conv.lastTokRate
		if tokRate == "" {
			tokRate = opencode.FormatMessageTokPerSec(conv.messages)
		}
	}
	if tokRate != "" {
		w.tokRateStatusLabel.SetLabel(tokRate)
		w.tokRateStatusLabel.SetVisible(true)
	} else {
		w.tokRateStatusLabel.SetVisible(false)
	}

	// Right:Context usage
	limit := w.modelLimits[modelKey]
	if limit == 0 {
		limit = w.modelLimits[conv.session.Model.ID]
	}
	ctxText := opencode.FormatContextUsage(conv.messages, conv.session.Tokens, limit)
	w.contextStatusLabel.SetLabel(ctxText)

	w.rebuildAgentPills()
}

func (w *Window) getGitBranch(dir string) string {
	if dir == "" {
		return ""
	}
	if b, ok := w.branchCache[dir]; ok {
		return b
	}
	headPath := filepath.Join(dir, ".git", "HEAD")
	data, err := os.ReadFile(headPath)
	if err != nil {
		if gitFile, err := os.ReadFile(filepath.Join(dir, ".git")); err == nil {
			line := strings.TrimSpace(string(gitFile))
			if after, ok := strings.CutPrefix(line, "gitdir:"); ok {
				gitDir := strings.TrimSpace(after)
				if !filepath.IsAbs(gitDir) {
					gitDir = filepath.Join(dir, gitDir)
				}
				data, _ = os.ReadFile(filepath.Join(gitDir, "HEAD"))
			}
		}
	}
	branch := ""
	if len(data) > 0 {
		line := strings.TrimSpace(string(data))
		if after, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			branch = strings.TrimSpace(after)
		} else if len(line) >= 7 {
			branch = line[:7]
		}
	}
	w.branchCache[dir] = branch
	return branch
}

func (w *Window) refreshShells() {
	gtkutil.Async(w.ctx, func() func() {
		shells, err := w.client.ListShells(w.ctx, "")
		return func() {
			if err == nil {
				w.activeShells = make(map[string]opencode.ShellItem)
				for _, sh := range shells {
					if sh.Status == "" || sh.Status == "running" {
						w.activeShells[sh.ID] = sh
					}
				}
				w.updateShellStatus()
				w.rebuildShellList()
			}
		}
	})
}

func (w *Window) updateShellStatus() {
	count := len(w.activeShells)
	if count > 0 {
		noun := "shells"
		if count == 1 {
			noun = "shell"
		}
		w.shellStatusLabel.SetLabel(fmt.Sprintf("%d %s", count, noun))
		w.shellStatusBox.SetVisible(true)
		w.shellsSectionBox.SetVisible(true)
	} else {
		w.shellStatusBox.SetVisible(false)
		w.shellsSectionBox.SetVisible(false)
	}
	w.updateSheetEmptyState()
}

func (w *Window) updateSheetEmptyState() {
	if w.sheetEmptyLabel == nil {
		return
	}
	hasContent := w.shellsSectionBox.Visible() || w.subagentsSectionBox.Visible()
	w.sheetEmptyLabel.SetVisible(!hasContent)
}

func (w *Window) rebuildShellList() {
	if w.shellListBox == nil {
		return
	}
	for c := w.shellListBox.FirstChild(); c != nil; c = w.shellListBox.FirstChild() {
		w.shellListBox.Remove(c)
	}
	if len(w.activeShells) == 0 {
		return
	}

	for _, sh := range w.activeShells {
		shellItem := sh
		row := gtk.NewListBoxRow()
		row.SetSelectable(false)
		row.SetActivatable(false)

		hBox := gtk.NewBox(gtk.OrientationHorizontal, 10)
		hBox.SetMarginTop(8)
		hBox.SetMarginBottom(8)
		hBox.SetMarginStart(12)
		hBox.SetMarginEnd(12)
		hBox.SetHExpand(true)
		hBox.SetVAlign(gtk.AlignCenter)

		icon := gtk.NewImageFromIconName("terminal-symbolic")
		icon.AddCSSClass("glup-tool-icon")
		icon.SetVAlign(gtk.AlignCenter)
		hBox.Append(icon)

		vBox := gtk.NewBox(gtk.OrientationVertical, 2)
		vBox.SetHExpand(true)

		titleText := shellItem.Description
		if titleText == "" {
			titleText = shellItem.Command
		}
		titleLbl := gtk.NewLabel(titleText)
		titleLbl.AddCSSClass("heading")
		titleLbl.SetXAlign(0)
		titleLbl.SetEllipsize(pango.EllipsizeEnd)
		vBox.Append(titleLbl)

		subtitleText := fmt.Sprintf("PID %d", shellItem.PID)
		if shellItem.Command != "" && shellItem.Description != "" {
			subtitleText = fmt.Sprintf("PID %d · %s", shellItem.PID, shellItem.Command)
		}
		subLbl := gtk.NewLabel(subtitleText)
		subLbl.AddCSSClass("dim-label")
		subLbl.AddCSSClass("caption")
		subLbl.SetXAlign(0)
		subLbl.SetEllipsize(pango.EllipsizeEnd)
		vBox.Append(subLbl)

		hBox.Append(vBox)

		stopBtn := gtk.NewButtonFromIconName("stop-circle-symbolic")
		stopBtn.AddCSSClass("destructive-action")
		stopBtn.AddCSSClass("circular")
		stopBtn.SetVAlign(gtk.AlignCenter)
		stopBtn.SetTooltipText("Stop Shell")
		stopBtn.ConnectClicked(func() {
			gtkutil.Async(w.ctx, func() func() {
				err := w.client.KillShell(w.ctx, shellItem.ID)
				return func() {
					if err != nil {
						slog.Error("failed to kill shell", "shellID", shellItem.ID, "err", err)
						app.Error(w.ctx, err)
						return
					}
					delete(w.activeShells, shellItem.ID)
					w.updateShellStatus()
					w.rebuildShellList()
				}
			})
		})
		hBox.Append(stopBtn)

		row.SetChild(hBox)
		w.shellListBox.Append(row)
	}
}

func (w *Window) rebuildSubagentList(tab *sessionTab) {
	for child := w.subagentListBox.FirstChild(); child != nil; child = w.subagentListBox.FirstChild() {
		w.subagentListBox.Remove(child)
	}
	w.subagentRows = nil

	viewingSubagent := tab != nil && tab.activeSubID != ""
	var activeSubs []opencode.Session
	if tab != nil {
		for _, sub := range tab.subagents {
			if sub.Outcome == "" {
				activeSubs = append(activeSubs, sub)
			}
		}
	}

	if len(activeSubs) == 0 && !viewingSubagent {
		w.subagentsSectionBox.SetVisible(false)
		w.updateSheetEmptyState()
		return
	}
	w.subagentsSectionBox.SetVisible(true)
	w.updateSheetEmptyState()

	for _, sub := range activeSubs {
		row := gtk.NewListBoxRow()
		row.SetActivatable(true)

		box := gtk.NewBox(gtk.OrientationHorizontal, 8)
		box.SetMarginTop(8)
		box.SetMarginBottom(8)
		box.SetMarginStart(10)
		box.SetMarginEnd(10)

		vbox := gtk.NewBox(gtk.OrientationVertical, 2)
		vbox.SetHExpand(true)

		title := strings.TrimSpace(sub.Title)
		if title == "" {
			title = sub.ID
		}
		titleLbl := gtk.NewLabel(title)
		titleLbl.SetXAlign(0)
		titleLbl.SetEllipsize(pango.EllipsizeEnd)
		if tab != nil && tab.activeSubID == sub.ID {
			titleLbl.AddCSSClass("heading")
		}
		vbox.Append(titleLbl)

		meta := sub.Agent
		if sub.Model.ID != "" {
			meta += " · " + sub.Model.String()
		}
		subLbl := gtk.NewLabel(meta)
		subLbl.SetXAlign(0)
		subLbl.SetEllipsize(pango.EllipsizeEnd)
		subLbl.AddCSSClass("caption")
		subLbl.AddCSSClass("dim-label")
		vbox.Append(subLbl)

		box.Append(vbox)

		statusLbl := gtk.NewLabel("running")
		statusLbl.AddCSSClass("caption")
		statusLbl.AddCSSClass("glup-tool-status-running")
		box.Append(statusLbl)

		row.SetChild(box)
		w.subagentListBox.Append(row)
		w.subagentRows = append(w.subagentRows, subagentRowData{
			row:     row,
			session: sub,
		})
	}
}

func (w *Window) rebuildAgentButtons() {
	w.rebuildAgentPills()
}

func (w *Window) rebuildAgentPills() {
	for child := w.agentListBox.FirstChild(); child != nil; child = w.agentListBox.FirstChild() {
		w.agentListBox.Remove(child)
	}
	w.agentRows = nil
	primary := opencode.PrimaryAgents(w.agents)
	if len(primary) == 0 {
		primary = []opencode.Agent{
			{ID: "build", Name: "build"},
			{ID: "plan", Name: "plan"},
			{ID: "bare", Name: "bare"},
			{ID: "orchestrator", Name: "orchestrator"},
		}
	}
	activeAgent := "build"
	if tab := w.currentTab(); tab != nil {
		if conv := tab.activeConv(); conv != nil && conv.session.Agent != "" {
			activeAgent = conv.session.Agent
		}
	}
	for _, ag := range primary {
		agentID := ag.ID
		isActive := strings.EqualFold(agentID, activeAgent)

		row := gtk.NewListBoxRow()
		box := gtk.NewBox(gtk.OrientationHorizontal, 8)
		box.SetMarginTop(6)
		box.SetMarginBottom(6)
		box.SetMarginStart(10)
		box.SetMarginEnd(10)

		vbox := gtk.NewBox(gtk.OrientationVertical, 2)
		vbox.SetHExpand(true)

		titleLbl := gtk.NewLabel(formatAgentDisplayName(agentID))
		titleLbl.SetXAlign(0)
		titleLbl.SetEllipsize(pango.EllipsizeEnd)
		titleLbl.AddCSSClass("glup-agent-pill-label")
		switch strings.ToLower(agentID) {
		case "build", "plan", "bare", "orchestrator":
			titleLbl.AddCSSClass("glup-agent-text-" + strings.ToLower(agentID))
		}
		vbox.Append(titleLbl)

		if desc := strings.TrimSpace(ag.Description); desc != "" {
			subLbl := gtk.NewLabel(desc)
			subLbl.SetXAlign(0)
			subLbl.SetEllipsize(pango.EllipsizeEnd)
			subLbl.AddCSSClass("caption")
			subLbl.AddCSSClass("dim-label")
			vbox.Append(subLbl)
			row.SetTooltipText(desc)
		}

		box.Append(vbox)

		if isActive {
			checkImg := gtk.NewImageFromIconName("object-select-symbolic")
			checkImg.SetPixelSize(16)
			checkImg.SetVAlign(gtk.AlignCenter)
			checkImg.AddCSSClass("glup-model-selected-icon")
			box.Append(checkImg)
		}

		row.SetChild(box)
		w.agentListBox.Append(row)
		w.agentRows = append(w.agentRows, agentRowData{
			row:     row,
			agentID: agentID,
		})
	}
}

func (w *Window) updateModelLastUsed(ref opencode.ModelRef, ts int64) {
	if ref.ID == "" {
		return
	}
	keys := []string{ref.ProviderID + "/" + ref.ID, ref.String(), ref.ID}
	for _, k := range keys {
		if ts > w.modelLastUsed[k] {
			w.modelLastUsed[k] = ts
		}
	}
}

func (w *Window) rebuildModelList() {
	for child := w.modelListBox.FirstChild(); child != nil; child = w.modelListBox.FirstChild() {
		w.modelListBox.Remove(child)
	}
	w.modelRows = nil

	sortedModels := make([]opencode.ModelInfo, len(w.models))
	copy(sortedModels, w.models)
	slices.SortFunc(sortedModels, func(a, b opencode.ModelInfo) int {
		keyA := a.ProviderID + "/" + a.ID
		keyB := b.ProviderID + "/" + b.ID
		usedA := w.modelLastUsed[keyA]
		if usedA == 0 {
			usedA = w.modelLastUsed[a.ID]
		}
		usedB := w.modelLastUsed[keyB]
		if usedB == 0 {
			usedB = w.modelLastUsed[b.ID]
		}
		if usedA != usedB {
			if usedA > usedB {
				return -1
			}
			return 1
		}
		if a.Time.Released != b.Time.Released {
			if a.Time.Released > b.Time.Released {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})

	var activeModel opencode.ModelRef
	if tab := w.currentTab(); tab != nil {
		if conv := tab.activeConv(); conv != nil {
			activeModel = conv.session.Model
		}
	}

	for _, m := range sortedModels {
		row := gtk.NewListBoxRow()
		box := gtk.NewBox(gtk.OrientationHorizontal, 8)
		box.SetMarginTop(6)
		box.SetMarginBottom(6)
		box.SetMarginStart(10)
		box.SetMarginEnd(10)

		vbox := gtk.NewBox(gtk.OrientationVertical, 2)
		vbox.SetHExpand(true)

		title := m.Name
		if title == "" {
			title = m.ID
		}
		titleLbl := gtk.NewLabel(title)
		titleLbl.SetXAlign(0)
		titleLbl.SetEllipsize(pango.EllipsizeEnd)
		vbox.Append(titleLbl)

		subLbl := gtk.NewLabel(m.ProviderID + "/" + m.ID)
		subLbl.SetXAlign(0)
		subLbl.SetEllipsize(pango.EllipsizeEnd)
		subLbl.AddCSSClass("caption")
		subLbl.AddCSSClass("dim-label")
		vbox.Append(subLbl)

		box.Append(vbox)
		if m.Limit.Context > 0 {
			ctxLbl := gtk.NewLabel(fmt.Sprintf("%dK ctx", m.Limit.Context/1000))
			ctxLbl.AddCSSClass("caption")
			ctxLbl.AddCSSClass("dim-label")
			ctxLbl.SetVAlign(gtk.AlignCenter)
			box.Append(ctxLbl)
		}

		isSelectedModel := (activeModel.ID != "" && activeModel.ID == m.ID && (activeModel.ProviderID == "" || activeModel.ProviderID == m.ProviderID))
		if isSelectedModel {
			checkImg := gtk.NewImageFromIconName("object-select-symbolic")
			checkImg.SetPixelSize(16)
			checkImg.SetVAlign(gtk.AlignCenter)
			checkImg.AddCSSClass("glup-model-selected-icon")
			box.Append(checkImg)
		}

		row.SetChild(box)
		w.modelListBox.Append(row)

		w.modelRows = append(w.modelRows, modelRowData{
			row:   row,
			model: m,
			text:  strings.ToLower(title + " " + m.ProviderID + "/" + m.ID),
		})
	}

	showAllRow := gtk.NewListBoxRow()
	showAllBox := gtk.NewBox(gtk.OrientationHorizontal, 8)
	showAllBox.SetMarginTop(8)
	showAllBox.SetMarginBottom(8)
	showAllBox.SetMarginStart(10)
	showAllBox.SetMarginEnd(10)
	showAllBox.SetHAlign(gtk.AlignCenter)

	showAllLbl := gtk.NewLabel("Show all models…")
	showAllLbl.AddCSSClass("dim-label")
	showAllLbl.AddCSSClass("heading")
	showAllBox.Append(showAllLbl)
	showAllRow.SetChild(showAllBox)

	w.modelListBox.Append(showAllRow)
	w.showAllModelsRow = showAllRow
	w.showAllModelsLabel = showAllLbl

	w.filterModelList(w.modelSearchEntry.Text())
}

func (w *Window) filterModelList(query string) {
	q := strings.ToLower(strings.TrimSpace(query))
	matched := 0
	shown := 0

	for _, mr := range w.modelRows {
		isMatch := q == "" || strings.Contains(mr.text, q)
		if !isMatch {
			mr.row.SetVisible(false)
			continue
		}
		matched++
		if w.showAllModels || shown < 6 {
			mr.row.SetVisible(true)
			shown++
		} else {
			mr.row.SetVisible(false)
		}
	}

	if w.showAllModelsRow != nil {
		if !w.showAllModels && matched > 6 {
			w.showAllModelsLabel.SetLabel(fmt.Sprintf("Show all (%d more)…", matched-6))
			w.showAllModelsRow.SetVisible(true)
		} else {
			w.showAllModelsRow.SetVisible(false)
		}
	}
}

func (w *Window) switchAgent(agentID string) {
	tab := w.currentTab()
	if tab == nil {
		return
	}
	conv := tab.activeConv()
	sessionID := conv.session.ID
	conv.session.Agent = agentID
	w.updateBottomBar(tab)

	gtkutil.Async(w.ctx, func() func() {
		err := w.client.SwitchAgent(w.ctx, sessionID, agentID)
		return func() {
			if err != nil {
				slog.Error("failed to switch agent", "err", err)
				app.Error(w.ctx, err)
			}
		}
	})
}

func (w *Window) switchModel(model opencode.ModelRef) {
	tab := w.currentTab()
	if tab == nil {
		return
	}
	conv := tab.activeConv()
	sessionID := conv.session.ID
	conv.session.Model = model
	w.updateModelLastUsed(model, time.Now().UnixMilli())
	w.rebuildModelList()
	w.updateBottomBar(tab)

	gtkutil.Async(w.ctx, func() func() {
		err := w.client.SwitchModel(w.ctx, sessionID, model)
		return func() {
			if err != nil {
				slog.Error("failed to switch model", "err", err)
				app.Error(w.ctx, err)
			}
		}
	})
}

func (w *Window) createNewSession() {
	cwd, _ := os.Getwd()
	if tab := w.currentTab(); tab != nil && tab.main.session.Location.Directory != "" {
		cwd = tab.main.session.Location.Directory
	}
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}
	cwd = filepath.Clean(cwd)

	gtkutil.Async(w.ctx, func() func() {
		sess, err := w.client.CreateSession(w.ctx, cwd)
		return func() {
			if err != nil {
				app.Error(w.ctx, err)
				return
			}
			tab := w.ensureSessionTab(*sess)
			tab.main.loaded = true
			w.syncProjectSessionVisibility()
			w.viewStack.SetVisibleChildName(sess.ID)
			w.onVisibleTabChanged()
			w.inputView.GrabFocus()
			glib.IdleAdd(func() {
				w.syncSwitcherIndicators()
			})
		}
	})
}

func (w *Window) sendPromptWithDelivery(delivery opencode.InboxDelivery) {
	tab := w.currentTab()
	if tab == nil {
		return
	}
	conv := tab.activeConv()
	start, end := w.inputBuffer.Bounds()
	text := strings.TrimSpace(w.inputBuffer.Text(start, end, false))
	if text == "" && len(w.pendingFiles) == 0 {
		return
	}
	w.inputBuffer.SetText("")

	files := slices.Clone(w.pendingFiles)
	w.pendingFiles = nil
	w.rebuildAttachmentTray()

	var isSlashCmd bool
	var cmdName string
	var cmdArgs string
	if strings.HasPrefix(text, "/") {
		cmdLine := strings.TrimPrefix(text, "/")
		parts := strings.SplitN(cmdLine, " ", 2)
		cmdName = strings.ToLower(parts[0])
		if len(parts) > 1 {
			cmdArgs = strings.TrimSpace(parts[1])
		}
		switch cmdName {
		case "undo", "redo", "new", "compact":
			isSlashCmd = true
		default:
			for _, c := range w.commands {
				if strings.EqualFold(c.Name, cmdName) {
					isSlashCmd = true
					break
				}
			}
		}
	}

	if isSlashCmd {
		switch cmdName {
		case "undo":
			if len(files) > 0 {
				w.pendingFiles = files
				w.rebuildAttachmentTray()
			}
			w.undoLastMessage(tab, conv)
			return
		case "redo":
			if len(files) > 0 {
				w.pendingFiles = files
				w.rebuildAttachmentTray()
			}
			w.redoRevert(tab, conv)
			return
		case "new":
			if len(files) > 0 {
				w.pendingFiles = files
				w.rebuildAttachmentTray()
			}
			w.createNewSession()
			return
		case "compact":
			if len(files) > 0 {
				w.pendingFiles = files
				w.rebuildAttachmentTray()
			}
			w.compactCurrentSession(tab, conv)
			return
		default:
			sessionID := conv.session.ID
			uw := NewUserMessageWidget(w.ctx, text)
			conv.timelineBox.Append(uw)
			conv.keepPendingAtBottom()
			conv.scroll.ScrollToBottom()
			w.setConvActive(sessionID, true)

			gtkutil.Async(w.ctx, func() func() {
				err := w.client.RunCommand(w.ctx, sessionID, cmdName, cmdArgs)
				return func() {
					if err != nil {
						slog.Error("failed to run command", "cmd", cmdName, "err", err)
						app.Error(w.ctx, err)
						w.setConvActive(sessionID, false)
						conv.timelineBox.Remove(uw)
						start, end := w.inputBuffer.Bounds()
						if strings.TrimSpace(w.inputBuffer.Text(start, end, false)) == "" {
							w.inputBuffer.SetText(text)
						}
						if len(files) > 0 && len(w.pendingFiles) == 0 {
							w.pendingFiles = files
							w.rebuildAttachmentTray()
						}
					}
				}
			})
			return
		}
	}

	if text == "" && len(files) > 0 {
		var parts []string
		for i, f := range files {
			name := f.Name
			if name == "" {
				name = fmt.Sprintf("Image %d", i+1)
			}
			parts = append(parts, fmt.Sprintf("[%s]", name))
		}
		text = strings.Join(parts, " ")
	}

	if delivery == "" {
		delivery = opencode.DeliveryQueue
	}

	sessionID := conv.session.ID
	uw := NewUserMessageWidget(w.ctx, text, files...)
	uw.SetWaiting(true, delivery)

	p := &pendingPrompt{
		text:     text,
		files:    files,
		delivery: delivery,
		widget:   uw,
	}
	w.bindPendingPromptCallbacks(conv, p)
	conv.pendingPrompts = append(conv.pendingPrompts, p)
	conv.timelineBox.Append(uw)
	conv.keepPendingAtBottom()
	conv.scroll.ScrollToBottom()
	w.setConvActive(sessionID, true)

	opts := parseMentions(text, w.skills, w.agents, w.activeDirectory())
	opts.Files = files
	opts.Delivery = delivery

	gtkutil.Async(w.ctx, func() func() {
		item, err := w.client.SendPrompt(w.ctx, sessionID, opts)
		return func() {
			if err != nil {
				slog.Error("failed to send prompt", "err", err)
				app.Error(w.ctx, err)
				w.setConvActive(sessionID, false)
				if p.widget != nil && p.widget.Parent() != nil {
					conv.timelineBox.Remove(p.widget)
				}
				for i, cur := range conv.pendingPrompts {
					if cur == p {
						conv.pendingPrompts = append(conv.pendingPrompts[:i], conv.pendingPrompts[i+1:]...)
						break
					}
				}
				start, end := w.inputBuffer.Bounds()
				if strings.TrimSpace(w.inputBuffer.Text(start, end, false)) == "" {
					w.inputBuffer.SetText(text)
				}
				if len(files) > 0 && len(w.pendingFiles) == 0 {
					w.pendingFiles = files
					w.rebuildAttachmentTray()
				}
				return
			}
			if item == nil || item.ID == "" {
				return
			}
			if p.cancelled {
				// cancelPendingPrompt already sent the DELETE if session.inbox.enqueued
				// bound the ID before this response did.
				if p.inboxID == "" {
					w.cancelInboxItem(sessionID, item.ID)
				}
				return
			}
			if conv.seenMsgIDs[item.ID] {
				msgID := item.ID
				msgText := p.text
				if p.widget != nil {
					p.widget.SetWaiting(false, "")
					conv.messageWidgets[msgID] = p.widget
					p.widget.SetOnUndo(func() {
						w.stageRevertToMessage(tab, conv, msgID, msgText)
					})
				}
				for i, cur := range conv.pendingPrompts {
					if cur == p {
						conv.pendingPrompts = append(conv.pendingPrompts[:i], conv.pendingPrompts[i+1:]...)
						break
					}
				}
				return
			}
			if p.inboxID == "" {
				w.bindInboxDelivery(conv, p, item.ID, item.Delivery)
			}
		}
	})
}

// bindInboxDelivery applies the server's view of a pending prompt. session.inbox.enqueued
// and the SendPrompt response race, so the first one to deliver the inbox ID also replays
// a Steer the user clicked before that ID was known.
func (w *Window) bindInboxDelivery(conv *convTimeline, p *pendingPrompt, inboxID string, delivery opencode.InboxDelivery) {
	steered := p.steeredInFlight(delivery)
	if inboxID != "" {
		p.inboxID = inboxID
	}
	if steered {
		w.steerPendingPrompt(conv, p)
		return
	}
	if delivery != "" {
		p.delivery = delivery
	}
	if p.widget != nil {
		p.widget.SetWaiting(true, p.delivery)
	}
}

func (w *Window) bindPendingPromptCallbacks(conv *convTimeline, p *pendingPrompt) {
	if p == nil || p.widget == nil {
		return
	}
	p.widget.SetOnSteer(func() {
		w.steerPendingPrompt(conv, p)
	})
	p.widget.SetOnCancel(func() {
		w.cancelPendingPrompt(conv, p)
	})
}

func (w *Window) steerPendingPrompt(conv *convTimeline, p *pendingPrompt) {
	if conv == nil || p == nil || p.cancelled {
		return
	}
	p.delivery = opencode.DeliverySteer
	if p.widget != nil {
		p.widget.SetWaiting(true, opencode.DeliverySteer)
	}
	if p.inboxID == "" {
		return
	}
	sessionID := conv.session.ID
	inboxID := p.inboxID
	gtkutil.Async(w.ctx, func() func() {
		err := w.client.UpdateInboxDelivery(w.ctx, sessionID, inboxID, opencode.DeliverySteer)
		return func() {
			if err == nil {
				return
			}
			if strings.HasPrefix(err.Error(), "http 409:") {
				w.reconcileTail(sessionID)
				return
			}
			slog.Error("failed to steer inbox item", "sessionID", sessionID, "inboxID", inboxID, "err", err)
			app.Error(w.ctx, err)
			if !p.cancelled && slices.Contains(conv.pendingPrompts, p) {
				p.delivery = opencode.DeliveryQueue
				if p.widget != nil {
					p.widget.SetWaiting(true, opencode.DeliveryQueue)
				}
			}
		}
	})
}

func (w *Window) cancelPendingPrompt(conv *convTimeline, p *pendingPrompt) {
	if conv == nil || p == nil || p.cancelled {
		return
	}
	p.cancelled = true
	if p.widget != nil && p.widget.Parent() != nil {
		conv.timelineBox.Remove(p.widget)
	}
	for i, cur := range conv.pendingPrompts {
		if cur == p {
			conv.pendingPrompts = append(conv.pendingPrompts[:i], conv.pendingPrompts[i+1:]...)
			break
		}
	}
	start, end := w.inputBuffer.Bounds()
	if strings.TrimSpace(w.inputBuffer.Text(start, end, false)) == "" && p.text != "" {
		w.inputBuffer.SetText(p.text)
	}
	if len(p.files) > 0 && len(w.pendingFiles) == 0 {
		w.pendingFiles = slices.Clone(p.files)
		w.rebuildAttachmentTray()
	}
	if p.inboxID == "" {
		return
	}
	w.cancelInboxItem(conv.session.ID, p.inboxID)
}

func (w *Window) cancelInboxItem(sessionID, inboxID string) {
	gtkutil.Async(w.ctx, func() func() {
		err := w.client.CancelInbox(w.ctx, sessionID, inboxID)
		return func() {
			if err != nil {
				slog.Error("failed to cancel inbox item", "sessionID", sessionID, "inboxID", inboxID, "err", err)
				app.Error(w.ctx, err)
			}
		}
	})
}

func (w *Window) onInboxEnqueued(tab *sessionTab, conv *convTimeline, ev opencode.InboxEvent) {
	if conv == nil {
		return
	}
	inboxID := ev.InboxID
	delivery := ev.Delivery
	if delivery == "" && ev.Item != nil {
		delivery = ev.Item.Delivery
	}
	if delivery == "" {
		delivery = opencode.DeliveryQueue
	}
	var itemText string
	var itemFiles []opencode.FileAttachment
	if ev.Item != nil {
		if ev.Item.Type != "" && ev.Item.Type != "user" {
			return
		}
		itemText = strings.TrimSpace(ev.Item.Payload.Text)
		itemFiles = ev.Item.Payload.Files
	}

	if inboxID != "" && conv.seenMsgIDs[inboxID] {
		return
	}

	for _, p := range conv.pendingPrompts {
		if p == nil || p.cancelled {
			continue
		}
		if (inboxID != "" && p.inboxID == inboxID) || (p.inboxID == "" && itemText != "" && p.text == itemText) {
			w.bindInboxDelivery(conv, p, inboxID, delivery)
			conv.keepPendingAtBottom()
			return
		}
	}

	if itemText == "" && len(itemFiles) == 0 {
		return
	}
	uw := NewUserMessageWidget(w.ctx, itemText, itemFiles...)
	uw.SetWaiting(true, delivery)
	p := &pendingPrompt{
		inboxID:  inboxID,
		text:     itemText,
		files:    itemFiles,
		delivery: delivery,
		widget:   uw,
	}
	w.bindPendingPromptCallbacks(conv, p)
	conv.pendingPrompts = append(conv.pendingPrompts, p)
	if conv.loaded {
		conv.timelineBox.Append(uw)
		conv.keepPendingAtBottom()
		conv.scroll.ScrollToBottom()
	}
}

func (w *Window) setupCompletion() {
	cp := newCompletionProvider(w)
	w.completionProvider = cp
	completion := w.inputView.Completion()
	if completion != nil {
		completion.SetObjectProperty("select-on-show", true)
		completion.AddProvider(cp)
	}
}

func activeTriggerWordFromText(prefix string) (word string, atBufferStart bool) {
	if len(prefix) == 0 {
		return "", false
	}
	r, _ := utf8.DecodeLastRuneInString(prefix)
	if unicode.IsSpace(r) {
		return "", false
	}
	lastSpace := strings.LastIndexFunc(prefix, unicode.IsSpace)
	if lastSpace == -1 {
		return prefix, true
	}
	word = prefix[lastSpace+1:]
	atBufferStart = strings.TrimSpace(prefix[:lastSpace]) == ""
	return word, atBufferStart
}

func activeTriggerWord(buf *gtksource.Buffer) (word string, atBufferStart bool) {
	if buf == nil {
		return "", false
	}
	start, end := buf.Bounds()
	if start == nil || end == nil {
		return "", false
	}
	cursor := end
	if mark := buf.GetInsert(); mark != nil {
		if iter := buf.IterAtMark(mark); iter != nil {
			cursor = iter
		}
	}
	return activeTriggerWordFromText(buf.Text(start, cursor, false))
}

func (w *Window) syncComposerCompletionPopup() {
	completion := w.inputView.Completion()
	if completion == nil {
		return
	}
	word, atStart := activeTriggerWord(w.inputBuffer)
	if (strings.HasPrefix(word, "/") && atStart) || strings.HasPrefix(word, "@") {
		completion.Show()
	}
}

func (w *Window) activeDirectory() string {
	tab := w.currentTab()
	if tab != nil {
		if conv := tab.activeConv(); conv != nil && conv.session.Location.Directory != "" {
			return conv.session.Location.Directory
		}
		if tab.main != nil && tab.main.session.Location.Directory != "" {
			return tab.main.session.Location.Directory
		}
	}
	return ""
}

func findUndoTargetMessage(messages []opencode.Message, currentRevertID string) *opencode.Message {
	start := len(messages) - 1
	if currentRevertID != "" {
		for idx, m := range messages {
			if m.ID == currentRevertID {
				start = idx - 1
				break
			}
		}
	}
	for i := start; i >= 0; i-- {
		if messages[i].Type == "user" && messages[i].ID != "" {
			return &messages[i]
		}
	}
	return nil
}

// computeMessageInsertIndex determines the insertion index in currentMessages for a message
// at index k in authoritative pageMessages.
func computeMessageInsertIndex(
	currentMessages []opencode.Message,
	pagePos map[string]int,
	k int,
	isInFlightAssistant func(id string) bool,
) int {
	idx := len(currentMessages)
	for idx > 0 {
		prev := currentMessages[idx-1]
		p, ok := pagePos[prev.ID]
		if ok {
			if p > k {
				idx-- // server says prev is newer than this message
				continue
			}
			break // server says prev is older than this message; stop!
		}
		// prev is not in page.Messages: only step past a live in-flight assistant message
		// at the very tail that was created by step.started and hasn't landed in the page yet.
		if idx == len(currentMessages) && prev.Type == "assistant" && isInFlightAssistant != nil && isInFlightAssistant(prev.ID) {
			idx--
			continue
		}
		break
	}
	return idx
}

// reconcileConvMessages reconciles a page of authoritative messages into the current message slice,
// updating seenIDs and inserting unseen messages at their computed position.
// If onInsert is non-nil, it is called for each inserted message with its index and any matched pending prompt.
func reconcileConvMessages(
	currentMessages *[]opencode.Message,
	seenIDs map[string]bool,
	pendingPrompts *[]*pendingPrompt,
	pageMessages []opencode.Message,
	isInFlight func(id string) bool,
	onInsert func(msg opencode.Message, insertIdx int, matchedPending *pendingPrompt),
) {
	if currentMessages == nil {
		return
	}
	pagePos := make(map[string]int, len(pageMessages))
	for i, m := range pageMessages {
		if m.ID != "" {
			pagePos[m.ID] = i
		}
	}
	for k, msg := range pageMessages {
		if seenIDs != nil && seenIDs[msg.ID] {
			continue
		}
		var matched *pendingPrompt
		if msg.Type == "user" && pendingPrompts != nil {
			matchIdx := -1
			for i, p := range *pendingPrompts {
				if p != nil && msg.ID != "" && p.inboxID == msg.ID {
					matchIdx = i
					break
				}
			}
			if matchIdx == -1 {
				for i, p := range *pendingPrompts {
					if p != nil && (p.inboxID == "" || msg.ID == "") && p.text == msg.Text {
						matchIdx = i
						break
					}
				}
			}
			if matchIdx >= 0 {
				matched = (*pendingPrompts)[matchIdx]
				*pendingPrompts = append((*pendingPrompts)[:matchIdx], (*pendingPrompts)[matchIdx+1:]...)
			}
		}
		if seenIDs != nil {
			seenIDs[msg.ID] = true
		}
		insertIdx := computeMessageInsertIndex(*currentMessages, pagePos, k, isInFlight)
		if insertIdx < len(*currentMessages) {
			*currentMessages = slices.Insert(*currentMessages, insertIdx, msg)
		} else {
			*currentMessages = append(*currentMessages, msg)
		}
		if onInsert != nil {
			onInsert(msg, insertIdx, matched)
		}
	}
}

// reconcileConvSlice reconciles a page of authoritative messages into a message slice
// and returns the updated slice.
func reconcileConvSlice(
	currentMessages []opencode.Message,
	seenIDs map[string]bool,
	pageMessages []opencode.Message,
	isInFlight func(id string) bool,
) []opencode.Message {
	msgs := slices.Clone(currentMessages)
	reconcileConvMessages(&msgs, seenIDs, nil, pageMessages, isInFlight, nil)
	return msgs
}

// computeRevertVisibility calculates which messages are visible and which are hidden
// when reverting to targetMessageID. If targetMessageID is empty, all messages are visible.
func computeRevertVisibility(messages []opencode.Message, targetMessageID string) (visibleIDs, hiddenIDs []string) {
	if targetMessageID == "" {
		for _, msg := range messages {
			if msg.ID != "" {
				visibleIDs = append(visibleIDs, msg.ID)
			}
		}
		return visibleIDs, nil
	}
	var isReverted bool
	for _, msg := range messages {
		if msg.ID == targetMessageID {
			isReverted = true
		}
		if msg.ID != "" {
			if isReverted {
				hiddenIDs = append(hiddenIDs, msg.ID)
			} else {
				visibleIDs = append(visibleIDs, msg.ID)
			}
		}
	}
	return visibleIDs, hiddenIDs
}

func (w *Window) undoLastMessage(tab *sessionTab, conv *convTimeline) {
	if conv == nil || conv.session.ID == "" {
		return
	}
	revertMsgID := ""
	if conv.session.Revert != nil {
		revertMsgID = conv.session.Revert.MessageID
	}
	targetMsg := findUndoTargetMessage(conv.messages, revertMsgID)
	if targetMsg == nil {
		return
	}
	w.stageRevertToMessage(tab, conv, targetMsg.ID, targetMsg.Text)
}

func (w *Window) redoRevert(tab *sessionTab, conv *convTimeline) {
	if conv == nil || conv.session.ID == "" {
		return
	}
	sessionID := conv.session.ID
	gtkutil.Async(w.ctx, func() func() {
		sess, err := w.client.ClearRevert(w.ctx, sessionID)
		return func() {
			if err != nil {
				slog.Error("failed to clear revert", "sessionID", sessionID, "err", err)
				app.Error(w.ctx, err)
				return
			}
			conv.session.Revert = nil
			if sess != nil {
				conv.session = *sess
			}
			w.applyRevertState(tab, conv)
		}
	})
}

func (w *Window) compactCurrentSession(tab *sessionTab, conv *convTimeline) {
	if conv == nil || conv.session.ID == "" {
		return
	}
	sessionID := conv.session.ID
	gtkutil.Async(w.ctx, func() func() {
		err := w.client.CompactSession(w.ctx, sessionID)
		return func() {
			if err != nil {
				slog.Error("failed to compact session", "sessionID", sessionID, "err", err)
				app.Error(w.ctx, err)
			}
		}
	})
}

func (w *Window) stageRevertToMessage(tab *sessionTab, conv *convTimeline, messageID, messageText string) {
	if conv == nil || conv.session.ID == "" || messageID == "" {
		return
	}
	sessionID := conv.session.ID

	start, end := w.inputBuffer.Bounds()
	curText := strings.TrimSpace(w.inputBuffer.Text(start, end, false))
	if curText == "" && messageText != "" {
		w.inputBuffer.SetText(messageText)
	}

	gtkutil.Async(w.ctx, func() func() {
		sess, err := w.client.StageRevert(w.ctx, sessionID, messageID, true)
		return func() {
			if err != nil {
				slog.Error("failed to stage revert", "sessionID", sessionID, "messageID", messageID, "err", err)
				app.Error(w.ctx, err)
				return
			}
			if sess != nil && sess.Revert != nil {
				conv.session.Revert = sess.Revert
			} else {
				conv.session.Revert = &opencode.SessionRevert{
					MessageID: messageID,
				}
			}
			w.applyRevertState(tab, conv)
		}
	})
}

func (w *Window) applyRevertState(tab *sessionTab, conv *convTimeline) {
	if conv == nil {
		return
	}
	revert := conv.session.Revert
	if revert == nil || revert.MessageID == "" {
		for _, widget := range conv.messageWidgets {
			gtk.BaseWidget(widget).SetVisible(true)
		}
		if conv.revertBanner != nil {
			conv.revertBanner.SetVisible(false)
		}
		return
	}

	targetID := revert.MessageID
	visibleIDs, hiddenIDs := computeRevertVisibility(conv.messages, targetID)
	for _, id := range visibleIDs {
		if widget, ok := conv.messageWidgets[id]; ok {
			gtk.BaseWidget(widget).SetVisible(true)
		}
	}
	for _, id := range hiddenIDs {
		if widget, ok := conv.messageWidgets[id]; ok {
			gtk.BaseWidget(widget).SetVisible(false)
		}
	}

	if conv.revertBanner == nil {
		conv.revertBanner = NewRevertBannerWidget(w.ctx, func() {
			w.redoRevert(tab, conv)
		})
		conv.timelineBox.Append(conv.revertBanner)
	}
	conv.revertBanner.Update(len(hiddenIDs), revert.Diff)
	conv.revertBanner.SetVisible(true)
	conv.keepPendingAtBottom()
	conv.scroll.ScrollToBottom()
}

func (w *Window) commitRevertState(tab *sessionTab, conv *convTimeline) {
	if conv == nil {
		return
	}
	targetID := ""
	if conv.session.Revert != nil {
		targetID = conv.session.Revert.MessageID
	}
	if targetID != "" {
		targetIdx := -1
		for i, msg := range conv.messages {
			if msg.ID == targetID {
				targetIdx = i
				break
			}
		}
		if targetIdx >= 0 {
			for i := targetIdx; i < len(conv.messages); i++ {
				id := conv.messages[i].ID
				if widget, ok := conv.messageWidgets[id]; ok {
					conv.timelineBox.Remove(widget)
					delete(conv.messageWidgets, id)
				}
				delete(conv.assistantMap, id)
				delete(conv.compactionMap, id)
				delete(conv.seenMsgIDs, id)
			}
			conv.messages = conv.messages[:targetIdx]
		}
	}
	if conv.revertBanner != nil {
		conv.timelineBox.Remove(conv.revertBanner)
		conv.revertBanner = nil
	}
	conv.session.Revert = nil
}

func (w *Window) clipboardHasImage() bool {
	clipboard := w.inputView.Clipboard()
	if clipboard == nil {
		clipboard = gdk.DisplayGetDefault().Clipboard()
	}
	if clipboard == nil {
		return false
	}
	formats := clipboard.Formats()
	if formats == nil {
		return false
	}
	for _, m := range formats.MIMETypes() {
		if strings.HasPrefix(m, "image/") {
			return true
		}
	}
	return false
}

func (w *Window) onPasteClipboard() {
	if !w.clipboardHasImage() {
		return
	}
	w.pasteClipboardImage()
}

func (w *Window) pasteClipboardImage() {
	clipboard := w.inputView.Clipboard()
	if clipboard == nil {
		clipboard = gdk.DisplayGetDefault().Clipboard()
	}
	if clipboard == nil {
		return
	}
	clipboard.ReadTextureAsync(w.ctx, func(res gio.AsyncResulter) {
		texer, err := clipboard.ReadTextureFinish(res)
		if err != nil || texer == nil {
			return
		}
		tex := gdk.BaseTexture(texer)
		if tex == nil {
			return
		}
		pngBytes := tex.SaveToPNGBytes()
		if pngBytes == nil {
			return
		}
		b64 := base64.StdEncoding.EncodeToString(pngBytes.Data())
		w.addPendingImage(fmt.Sprintf("clipboard-%d.png", len(w.pendingFiles)+1), "image/png", b64, tex)
	})
}

func (w *Window) chooseAndAttachFile() {
	dlg := gtk.NewFileDialog()
	dlg.SetTitle("Attach Image")

	filter := gtk.NewFileFilter()
	filter.SetName("Images")
	filter.AddMIMEType("image/png")
	filter.AddMIMEType("image/jpeg")
	filter.AddMIMEType("image/webp")
	filter.AddMIMEType("image/gif")

	filters := gio.NewListStore(gtk.GTypeFileFilter)
	filters.Append(filter.Object)
	dlg.SetFilters(filters)
	dlg.SetDefaultFilter(filter)

	dlg.Open(w.ctx, &w.Window, func(res gio.AsyncResulter) {
		file, err := dlg.OpenFinish(res)
		if err != nil || file == nil {
			return
		}
		path := file.Path()
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Error("failed to read attached file", "err", err)
			return
		}
		b64 := base64.StdEncoding.EncodeToString(data)
		mime := "image/png"
		lower := strings.ToLower(path)
		if strings.HasSuffix(lower, ".jpg") || strings.HasSuffix(lower, ".jpeg") {
			mime = "image/jpeg"
		} else if strings.HasSuffix(lower, ".webp") {
			mime = "image/webp"
		} else if strings.HasSuffix(lower, ".gif") {
			mime = "image/gif"
		}
		tex, _ := decodeBase64Texture(b64)
		w.addPendingImage(filepath.Base(path), mime, b64, tex)
	})
}

func (w *Window) addPendingImage(name, mime, b64 string, tex *gdk.Texture) {
	if mime == "" {
		mime = "image/png"
	}
	if name == "" {
		name = fmt.Sprintf("image-%d.png", len(w.pendingFiles)+1)
	}
	w.pendingFiles = append(w.pendingFiles, opencode.FileAttachment{
		Name: name,
		Mime: mime,
		Data: b64,
	})
	w.rebuildAttachmentTray()
}

func (w *Window) removePendingFile(idx int) {
	if idx < 0 || idx >= len(w.pendingFiles) {
		return
	}
	w.pendingFiles = slices.Delete(w.pendingFiles, idx, idx+1)
	w.rebuildAttachmentTray()
}

func (w *Window) rebuildAttachmentTray() {
	w.attachmentTray.RemoveAll()
	if len(w.pendingFiles) == 0 {
		w.attachmentTray.SetVisible(false)
		return
	}
	w.attachmentTray.SetVisible(true)
	for i, f := range w.pendingFiles {
		overlay := gtk.NewOverlay()
		overlay.AddCSSClass("glup-attachment-item")

		pic := gtk.NewPicture()
		pic.SetCanShrink(true)
		pic.SetContentFit(gtk.ContentFitCover)
		pic.SetSizeRequest(64, 64)
		pic.AddCSSClass("glup-attachment-thumb")

		tex, err := decodeBase64Texture(f.Data)
		if err == nil && tex != nil {
			pic.SetPaintable(tex)
		}
		hClamp := adw.NewClamp()
		hClamp.SetOrientation(gtk.OrientationHorizontal)
		hClamp.SetMaximumSize(64)
		hClamp.SetTighteningThreshold(64)
		hClamp.SetChild(pic)

		vClamp := adw.NewClamp()
		vClamp.SetOrientation(gtk.OrientationVertical)
		vClamp.SetMaximumSize(64)
		vClamp.SetTighteningThreshold(64)
		vClamp.SetChild(hClamp)

		overlay.SetChild(vClamp)

		removeBtn := gtk.NewButtonFromIconName("window-close-symbolic")
		removeBtn.AddCSSClass("flat")
		removeBtn.AddCSSClass("circular")
		removeBtn.AddCSSClass("glup-attachment-remove-btn")
		removeBtn.SetHAlign(gtk.AlignEnd)
		removeBtn.SetVAlign(gtk.AlignStart)
		removeBtn.SetTooltipText("Remove attachment")

		idx := i
		removeBtn.ConnectClicked(func() {
			w.removePendingFile(idx)
		})

		overlay.AddOverlay(removeBtn)
		w.attachmentTray.Append(overlay)
	}
}

func (w *Window) interruptSession() {
	tab := w.currentTab()
	if tab == nil {
		return
	}
	sessionID := tab.activeConv().session.ID
	if sessionID == "" {
		return
	}
	gtkutil.Async(w.ctx, func() func() {
		err := w.client.Interrupt(w.ctx, sessionID)
		return func() {
			if err != nil {
				slog.Error("failed to interrupt session", "err", err)
			}
		}
	})
}

func (w *Window) resolveConv(sessionID string) (*sessionTab, *convTimeline) {
	if tab, ok := w.tabs[sessionID]; ok {
		return tab, tab.main
	}
	if rootID, ok := w.childToRoot[sessionID]; ok {
		if tab, ok := w.tabs[rootID]; ok {
			return tab, tab.subTimelines[sessionID]
		}
	}
	return nil, nil
}

func (w *Window) setConvActive(sessionID string, active bool) {
	tab, conv := w.resolveConv(sessionID)
	if tab == nil {
		return
	}
	if conv != nil {
		conv.active = active
		if !active {
			conv.runningTools = 0
		}
	}
	if conv == tab.main {
		tab.page.SetBadgeNumber(0)
	}
	if tab.main.session.ID != w.currentRootSessionID() && active {
		tab.page.SetNeedsAttention(true)
	}
	if w.currentTab() == tab && tab.activeConv() == conv {
		w.updateBusyState(active)
	}
	w.updateSwitcherIndicators()
}

func (w *Window) updateBusyState(busy bool) {
	w.headerSpinner.SetVisible(busy)
	w.stopBtn.SetVisible(busy)
}

func (w *Window) ensureAssistantWidget(tab *sessionTab, conv *convTimeline, msgID, agent, model string) *AssistantMessageWidget {
	if aw, ok := conv.assistantMap[msgID]; ok {
		if agent != "" || model != "" {
			aw.SetMeta(agent, model)
		}
		return aw
	}
	if agent == "" {
		agent = conv.session.Agent
	}
	if model == "" {
		model = conv.session.Model.String()
	}
	if tab == nil {
		tab = w.currentTab()
	}
	aw := NewAssistantMessageWidget(w.ctx, agent, model, func(subID string) {
		w.openSubagentByID(tab, subID)
	})
	conv.assistantMap[msgID] = aw
	conv.messageWidgets[msgID] = aw
	conv.messages = append(conv.messages, opencode.Message{
		ID:    msgID,
		Type:  "assistant",
		Agent: agent,
	})
	conv.seenMsgIDs[msgID] = true
	conv.timelineBox.Append(aw)
	conv.keepPendingAtBottom()
	return aw
}

func (w *Window) resyncOnReconnect() {
	for _, tab := range w.tabs {
		if tab.main.loaded {
			w.reconcileTail(tab.main.session.ID)
		}
		for _, conv := range tab.subTimelines {
			if conv.loaded {
				w.reconcileTail(conv.session.ID)
			}
		}
	}
	w.loadAll()
}

func (w *Window) runEventLoop() {
	_ = w.client.SubscribeEventsLoop(w.ctx, 2*time.Second, func(ev opencode.Event) {
		glib.IdleAdd(func() {
			w.handleEvent(ev)
		})
	}, func() {
		glib.IdleAdd(func() {
			w.resyncOnReconnect()
		})
	})
}

func (w *Window) handleEvent(ev opencode.Event) {
	switch ev.Type {
	case "session.created":
		var d opencode.SessionCreatedEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			applyCreated := func(sess opencode.Session) {
				if sess.ParentID != "" {
					if tab, ok := w.tabs[sess.ParentID]; ok {
						w.childToRoot[sess.ID] = sess.ParentID
						found := false
						for i, s := range tab.subagents {
							if s.ID == sess.ID {
								tab.subagents[i] = sess
								found = true
								break
							}
						}
						if !found {
							tab.subagents = append([]opencode.Session{sess}, tab.subagents...)
						}
						if w.currentTab() == tab {
							w.updateBottomBar(tab)
							w.rebuildSubagentList(tab)
						}
					}
				} else if sess.ID != "" {
					w.ensureSessionTab(sess)
					w.syncProjectSessionVisibility()
					w.syncSwitcherIndicators()
				}
			}
			if d.Session.ID != "" && d.Session.Location.Directory == "" && d.Session.ParentID == "" && d.Session.Title == "" {
				sessID := d.Session.ID
				gtkutil.Async(w.ctx, func() func() {
					sess, err := w.client.GetSession(w.ctx, sessID)
					return func() {
						if err == nil && sess != nil {
							applyCreated(*sess)
						} else {
							applyCreated(d.Session)
						}
					}
				})
			} else {
				applyCreated(d.Session)
			}
		}

	case "session.updated":
		var d opencode.SessionCreatedEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			sess := d.Session
			if sess.ID == "" {
				sess.ID = d.SessionID
			}
			if tab, ok := w.tabs[sess.ID]; ok {
				if sess.Title != "" && tab.main.session.Title != sess.Title {
					tab.main.session.Title = sess.Title
					tab.page.SetTitle(sess.Title)
					w.syncSwitcherIndicators()
				}
				if sess.Time.Updated != 0 {
					tab.main.session.Time.Updated = sess.Time.Updated
				}
				if sess.Location.Directory != "" && tab.main.session.Location.Directory == "" {
					tab.main.session.Location = sess.Location
				}
				tab.main.session = sess
				if w.currentTab() == tab {
					w.refreshHeaderAndBottomBar(tab)
				}
			} else if parentID, ok := w.childToRoot[sess.ID]; ok {
				if parentTab, ok := w.tabs[parentID]; ok {
					for i, s := range parentTab.subagents {
						if s.ID == sess.ID {
							parentTab.subagents[i] = sess
							break
						}
					}
					if w.currentTab() == parentTab {
						w.rebuildSubagentList(parentTab)
					}
				}
			}
		}

	case "session.deleted":
		var d opencode.SessionCreatedEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			delID := d.SessionID
			if parentID, ok := w.childToRoot[delID]; ok {
				delete(w.childToRoot, delID)
				if parentTab, ok := w.tabs[parentID]; ok {
					filtered := parentTab.subagents[:0]
					for _, s := range parentTab.subagents {
						if s.ID != delID {
							filtered = append(filtered, s)
						}
					}
					parentTab.subagents = filtered
					if parentTab.activeConv() != nil && parentTab.activeConv().session.ID == delID {
						w.closeSubagent(parentTab)
					}
					if w.currentTab() == parentTab {
						w.rebuildSubagentList(parentTab)
					}
				}
			} else if tab, ok := w.tabs[delID]; ok {
				tab.main.closeStreams()
				for _, c := range tab.subTimelines {
					c.closeStreams()
				}
				w.viewStack.Remove(tab.stack)
				delete(w.tabs, delID)
				for i, id := range w.tabOrder {
					if id == delID {
						w.tabOrder = append(w.tabOrder[:i], w.tabOrder[i+1:]...)
						break
					}
				}
				w.syncSectionStarts()
				w.syncProjectSessionVisibility()
				w.syncSwitcherIndicators()
			}
		}

	case "session.execution.started":
		var d opencode.SessionIDEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			w.setConvActive(d.SessionID, true)
			w.reconcileTail(d.SessionID)
		}

	case "session.execution.succeeded", "session.execution.failed", "session.execution.interrupted", "session.idle":
		var d opencode.SessionIDEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			tab, conv := w.resolveConv(d.SessionID)
			now := time.Now().UnixMilli()
			if tab != nil {
				tab.main.session.Time.Updated = now
			}
			if conv != nil {
				conv.session.Time.Updated = now
			}
			w.setConvActive(d.SessionID, false)
			w.reconcileTail(d.SessionID)
			if rootID, ok := w.childToRoot[d.SessionID]; ok {
				w.loadChildSubagents(rootID)
			}
		}

	case "session.inbox.enqueued":
		var d opencode.InboxEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			tab, conv := w.resolveConv(d.SessionID)
			if tab != nil {
				tab.main.session.Time.Updated = time.Now().UnixMilli()
			}
			if conv != nil {
				conv.session.Time.Updated = time.Now().UnixMilli()
				w.onInboxEnqueued(tab, conv, d)
			}
		}

	case "session.inbox.delivery.changed":
		var d opencode.InboxEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			_, conv := w.resolveConv(d.SessionID)
			if conv != nil && d.InboxID != "" {
				for _, p := range conv.pendingPrompts {
					if p != nil && p.inboxID == d.InboxID {
						p.delivery = d.Delivery
						if p.widget != nil {
							p.widget.SetWaiting(true, d.Delivery)
						}
						break
					}
				}
			}
		}

	case "session.inbox.cancelled":
		var d opencode.InboxEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			_, conv := w.resolveConv(d.SessionID)
			if conv != nil && d.InboxID != "" {
				for i, p := range conv.pendingPrompts {
					if p != nil && p.inboxID == d.InboxID {
						p.cancelled = true
						if p.widget != nil && p.widget.Parent() != nil {
							conv.timelineBox.Remove(p.widget)
						}
						conv.pendingPrompts = append(conv.pendingPrompts[:i], conv.pendingPrompts[i+1:]...)
						break
					}
				}
			}
		}

	case "session.inbox.delivered", "session.synthetic",
		"session.compaction.started", "session.compaction.ended", "session.compaction.failed":
		var d opencode.SessionIDEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			tab, conv := w.resolveConv(d.SessionID)
			if tab != nil {
				tab.main.session.Time.Updated = time.Now().UnixMilli()
			}
			if conv != nil {
				conv.session.Time.Updated = time.Now().UnixMilli()
				if conv.loaded {
					w.reconcileTail(d.SessionID)
				}
			}
			if ev.Type == "session.synthetic" {
				if rootID, ok := w.childToRoot[d.SessionID]; ok {
					w.loadChildSubagents(rootID)
				}
			}
		}

	case "session.renamed":
		var d struct {
			SessionID string `json:"sessionID"`
			Title     string `json:"title"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.Title != "" {
			tab, conv := w.resolveConv(d.SessionID)
			if conv != nil {
				conv.session.Title = d.Title
			}
			if tab != nil {
				if conv == tab.main {
					tab.page.SetTitle(d.Title)
				}
				if w.currentTab() == tab {
					w.refreshHeaderAndBottomBar(tab)
				}
			}
		}

	case "session.agent.selected":
		var d opencode.AgentSelectedEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveConv(d.SessionID)
			if conv != nil && d.Agent != "" {
				conv.session.Agent = d.Agent
				if w.currentTab() == tab {
					w.updateBottomBar(tab)
				}
			}
		}

	case "session.model.selected":
		var d opencode.ModelSelectedEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveConv(d.SessionID)
			if conv != nil && d.Model.ID != "" {
				conv.session.Model = d.Model
				w.updateModelLastUsed(d.Model, time.Now().UnixMilli())
				w.rebuildModelList()
				if w.currentTab() == tab {
					w.updateBottomBar(tab)
				}
			}
		}

	case "session.step.started":
		var d opencode.StepStartedEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveConv(d.SessionID)
			if tab != nil {
				tab.main.session.Time.Updated = time.Now().UnixMilli()
			}
			if conv != nil {
				conv.session.Time.Updated = time.Now().UnixMilli()
				conv.streamStart = time.Now()
				conv.streamEnd = time.Time{}
				conv.streamedChars = 0
				w.setConvActive(d.SessionID, true)
				if d.Agent != "" {
					conv.session.Agent = d.Agent
				}
				if d.Model.ID != "" {
					conv.session.Model = d.Model
					w.updateModelLastUsed(d.Model, time.Now().UnixMilli())
					w.rebuildModelList()
				}
				if conv.loaded {
					w.ensureAssistantWidget(tab, conv, d.AssistantMessageID, d.Agent, d.Model.String())
					w.reconcileTail(d.SessionID)
				}
				if w.currentTab() == tab {
					w.updateBottomBar(tab)
				}
			}
		}

	case "session.step.streamed":
		var d opencode.StepStreamedEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			_, conv := w.resolveConv(d.SessionID)
			if conv != nil {
				if conv.streamStart.IsZero() {
					conv.streamStart = time.Now()
				}
				conv.streamEnd = time.Now()
			}
		}

	case "session.step.ended":
		var d opencode.StepEndedEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveConv(d.SessionID)
			if tab != nil {
				tab.main.session.Time.Updated = time.Now().UnixMilli()
			}
			if conv != nil {
				conv.session.Time.Updated = time.Now().UnixMilli()
				totalTok := d.Tokens.Output + d.Tokens.Reasoning
				if totalTok > 0 && !conv.streamStart.IsZero() {
					var dur float64
					if !conv.streamEnd.IsZero() {
						dur = conv.streamEnd.Sub(conv.streamStart).Seconds()
					} else {
						dur = time.Since(conv.streamStart).Seconds()
					}
					if dur > 0.05 {
						conv.lastTokRate = fmt.Sprintf("%.1f tok/s", float64(totalTok)/dur)
					}
				}
				conv.streamStart = time.Time{}
				conv.streamEnd = time.Time{}
				conv.streamedChars = 0

				if d.Tokens.Input+d.Tokens.Cache.Read > 0 {
					conv.session.Tokens = d.Tokens
				}
				found := false
				for i := len(conv.messages) - 1; i >= 0; i-- {
					if conv.messages[i].ID == d.AssistantMessageID {
						conv.messages[i].Tokens = d.Tokens
						conv.messages[i].Cost = d.Cost
						if conv.messages[i].Time.Completed == 0 {
							conv.messages[i].Time.Completed = time.Now().UnixMilli()
						}
						found = true
						break
					}
				}
				if !found && d.AssistantMessageID != "" {
					var msg opencode.Message
					msg.ID = d.AssistantMessageID
					msg.Type = "assistant"
					msg.Tokens = d.Tokens
					msg.Cost = d.Cost
					msg.Time.Completed = time.Now().UnixMilli()
					conv.messages = append(conv.messages, msg)
				}
				if authoritative := opencode.FormatMessageTokPerSec(conv.messages); authoritative != "" {
					conv.lastTokRate = authoritative
				}
				if w.currentTab() == tab {
					w.updateBottomBar(tab)
				}
			}
		}

	case "session.reasoning.delta":
		var d opencode.TextDeltaEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveConv(d.SessionID)
			if conv != nil {
				w.onStreamDelta(tab, conv, len(d.Delta))
				if conv.loaded {
					aw := w.ensureAssistantWidget(tab, conv, d.AssistantMessageID, "", "")
					aw.AppendReasoningDelta(d.Ordinal, d.Delta)
				}
			}
		}

	case "session.reasoning.ended":
		var d opencode.TextEndedEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveConv(d.SessionID)
			if conv != nil && conv.loaded {
				aw := w.ensureAssistantWidget(tab, conv, d.AssistantMessageID, "", "")
				aw.FinalizeReasoning(d.Ordinal, d.Text)
			}
		}

	case "session.text.delta":
		var d opencode.TextDeltaEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveConv(d.SessionID)
			if conv != nil {
				w.onStreamDelta(tab, conv, len(d.Delta))
				if conv.loaded {
					aw := w.ensureAssistantWidget(tab, conv, d.AssistantMessageID, "", "")
					aw.AppendTextDelta(d.Ordinal, d.Delta)
				}
			}
		}

	case "session.text.ended":
		var d opencode.TextEndedEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveConv(d.SessionID)
			if conv != nil && conv.loaded {
				aw := w.ensureAssistantWidget(tab, conv, d.AssistantMessageID, "", "")
				aw.FinalizeText(d.Ordinal, d.Text)
			}
		}

	case "session.tool.input.started":
		var d opencode.ToolEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveConv(d.SessionID)
			if conv != nil {
				conv.runningTools++
				if conv.loaded {
					aw := w.ensureAssistantWidget(tab, conv, d.AssistantMessageID, "", "")
					tw := aw.EnsureTool(d.ID, d.Name)
					tw.SetStatus("streaming")
				}
			}
		}

	case "session.tool.input.delta":
		var d opencode.ToolEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveConv(d.SessionID)
			if conv != nil {
				w.onStreamDelta(tab, conv, len(d.Delta))
				if conv.loaded {
					aw := w.ensureAssistantWidget(tab, conv, d.AssistantMessageID, "", "")
					tw := aw.EnsureTool(d.ID, d.Name)
					tw.AppendInputDelta(d.Delta)
				}
			}
		}

	case "session.tool.called":
		var d opencode.ToolEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveConv(d.SessionID)
			if conv != nil && conv.loaded {
				aw := w.ensureAssistantWidget(tab, conv, d.AssistantMessageID, "", "")
				tw := aw.EnsureTool(d.ID, d.Name)
				tw.SetStatus("running")
				tw.SetInput(d.Input)
				tw.SetMetadata(d.Metadata)
			}
			if d.Name == "subagent" {
				w.loadChildSubagents(d.SessionID)
			}
		}

	case "session.tool.updated":
		var d opencode.ToolEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveConv(d.SessionID)
			if conv != nil && conv.loaded {
				aw := w.ensureAssistantWidget(tab, conv, d.AssistantMessageID, "", "")
				tw := aw.EnsureTool(d.ID, d.Name)
				if d.Text != "" {
					tw.SetStatus(d.Text)
				}
				if d.Input != nil {
					tw.SetInput(d.Input)
				}
				if len(d.Content) > 0 {
					tw.SetOutput(d.Content, d.Error)
				}
				tw.SetMetadata(d.Metadata)
			}
		}

	case "session.tool.success":
		var d opencode.ToolEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveConv(d.SessionID)
			if conv != nil {
				if conv.runningTools > 0 {
					conv.runningTools--
				}
				if conv.loaded {
					aw := w.ensureAssistantWidget(tab, conv, d.AssistantMessageID, "", "")
					tw := aw.EnsureTool(d.ID, d.Name)
					tw.SetStatus("completed")
					tw.SetOutput(d.Content, nil)
					tw.SetMetadata(d.Metadata)
				}
			}
		}

	case "session.tool.failed":
		var d opencode.ToolEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveConv(d.SessionID)
			if conv != nil {
				if conv.runningTools > 0 {
					conv.runningTools--
				}
				if conv.loaded {
					aw := w.ensureAssistantWidget(tab, conv, d.AssistantMessageID, "", "")
					tw := aw.EnsureTool(d.ID, d.Name)
					tw.SetStatus("failed")
					tw.SetOutput(d.Content, d.Error)
					tw.SetMetadata(d.Metadata)
				}
			}
		}

	case "session.revert.staged":
		var d opencode.RevertStagedEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			tab, conv := w.resolveConv(d.SessionID)
			if conv != nil {
				conv.session.Revert = &d.Revert
				w.applyRevertState(tab, conv)
			}
		}

	case "session.revert.cleared":
		var d opencode.RevertClearedEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			tab, conv := w.resolveConv(d.SessionID)
			if conv != nil {
				conv.session.Revert = nil
				w.applyRevertState(tab, conv)
			}
		}

	case "session.revert.committed":
		var d opencode.RevertCommittedEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			tab, conv := w.resolveConv(d.SessionID)
			if conv != nil {
				w.commitRevertState(tab, conv)
			}
		}

	case "shell.created":
		var d opencode.ShellEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			sh := d.Info
			if sh.ID == "" {
				sh = d.Shell
			}
			if sh.ID != "" && (sh.Status == "" || sh.Status == "running") {
				w.activeShells[sh.ID] = sh
				w.updateShellStatus()
				w.rebuildShellList()
			}
		}

	case "shell.exited", "shell.deleted":
		var d opencode.ShellEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			id := d.ID
			if id == "" {
				id = d.ShellID
			}
			if id == "" {
				id = d.Info.ID
			}
			if id == "" {
				id = d.Shell.ID
			}
			if id != "" {
				delete(w.activeShells, id)
				w.updateShellStatus()
				w.rebuildShellList()
			}
		}
	}
}
