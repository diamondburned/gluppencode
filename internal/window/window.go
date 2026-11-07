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
	"github.com/diamondburned/gotkit/gtkutil"
	"libdb.so/gluppencode/internal/autoscroll"
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
	session            opencode.Session
	scroll             *autoscroll.Window
	timelineBox        *gtk.Box
	assistantMap       map[string]*AssistantMessageWidget
	compactionMap      map[string]*CompactionWidget
	messageWidgets     map[string]gtk.Widgetter
	revertBanner       *RevertBannerWidget
	thinkingLabel      *gtk.Label
	pendingPrompts     []*pendingPrompt
	pendingForms       []opencode.FormInfo
	formWidgets        map[string]*QuestionFormWidget
	seenMsgIDs         map[string]bool
	messages           []opencode.Message
	nextCursor         string
	hasOlder           bool
	loadingOlder       bool
	loadingInitial     bool
	loaded             bool
	active             bool
	runningTools       int
	blockingTools      map[string]time.Time
	blockingShellTools map[string]bool
	blockingShellIDs   map[string]string
	backgroundShellIDs map[string]bool
	toolNames          map[string]string
	streamStart        time.Time
	streamEnd          time.Time
	streamedChars      int
	lastTokRate        string
}

func isShellToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "shell", "bash":
		return true
	default:
		return false
	}
}

func isBackgroundableToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "shell", "bash", "subagent", "task":
		return true
	default:
		return false
	}
}

func isBlockingBackgroundableTool(name, status string, input any) bool {
	if status != "running" || !isBackgroundableToolName(name) {
		return false
	}
	if m := opencode.ParseToolInputMap(input); m != nil {
		if bg, ok := m["background"].(bool); ok && bg {
			return false
		}
	}
	return true
}

const backgroundHintDelay = 5 * time.Second

func (c *convTimeline) isBlockingOnBackgroundable() bool {
	return c != nil && c.active && len(c.blockingTools) > 0
}

func (c *convTimeline) isBlockingForAtLeast(now time.Time, minAge time.Duration) bool {
	if !c.isBlockingOnBackgroundable() {
		return false
	}
	for _, startedAt := range c.blockingTools {
		if !startedAt.IsZero() && now.Sub(startedAt) >= minAge {
			return true
		}
	}
	return false
}

func (c *convTimeline) nextBackgroundHintDelay(now time.Time, minAge time.Duration) (time.Duration, bool) {
	if !c.isBlockingOnBackgroundable() {
		return 0, false
	}
	var shortest time.Duration
	var found bool
	for _, startedAt := range c.blockingTools {
		if startedAt.IsZero() {
			continue
		}
		rem := minAge - now.Sub(startedAt)
		if rem <= 0 {
			return 0, false
		}
		if !found || rem < shortest {
			shortest = rem
			found = true
		}
	}
	return shortest, found
}

func (c *convTimeline) syncToolPart(part opencode.ContentPart) {
	if c == nil || part.ID == "" {
		return
	}
	if part.Name != "" {
		c.toolNames[part.ID] = part.Name
	}
	name := part.Name
	if name == "" {
		name = c.toolNames[part.ID]
	}
	shID, _ := part.State.Metadata["shellID"].(string)
	metaStatus, _ := part.State.Metadata["status"].(string)

	if isBlockingBackgroundableTool(name, part.State.Status, part.State.Input) {
		if c.blockingTools[part.ID].IsZero() {
			c.blockingTools[part.ID] = time.Now()
		}
		if isShellToolName(name) {
			c.blockingShellTools[part.ID] = true
			if shID != "" && !c.backgroundShellIDs[shID] {
				c.blockingShellIDs[shID] = part.ID
			}
		}
	} else {
		delete(c.blockingTools, part.ID)
		delete(c.blockingShellTools, part.ID)
		for k, tid := range c.blockingShellIDs {
			if tid == part.ID {
				delete(c.blockingShellIDs, k)
			}
		}
		if isShellToolName(name) && shID != "" && (metaStatus == "running" || part.State.Status == "completed") {
			if metaStatus == "running" {
				c.backgroundShellIDs[shID] = true
			}
		}
	}
}

func (c *convTimeline) markSyntheticTargetCompleted(msg opencode.Message) {
	if c == nil {
		return
	}
	trimmed := strings.TrimSpace(msg.Text)
	if strings.HasPrefix(trimmed, "<shell") {
		shID := extractShellID(trimmed)
		if shID == "" {
			if id, ok := msg.Metadata["shellID"].(string); ok {
				shID = id
			}
		}
		if shID != "" {
			for _, aw := range c.assistantMap {
				for _, tw := range aw.tools {
					if tw.ShellID() == shID {
						tw.SetBackgroundStatus("completed")
					}
				}
			}
		}
		return
	}
	subID := ""
	if s, ok := msg.Metadata["childID"].(string); ok && s != "" {
		subID = s
	} else if s, ok := msg.Metadata["sessionID"].(string); ok && s != "" {
		subID = s
	} else if matches := subagentSessionRe.FindStringSubmatch(trimmed); len(matches) > 1 {
		subID = matches[1]
	}
	if subID != "" {
		for _, aw := range c.assistantMap {
			for _, tw := range aw.tools {
				if tw.ChildSessionID() == subID {
					tw.SetBackgroundStatus("completed")
				}
			}
		}
	}
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
	for _, f := range c.pendingForms {
		if qw := c.formWidgets[f.ID]; qw != nil && qw.Parent() != nil {
			if last := c.timelineBox.LastChild(); last != nil && gtk.BaseWidget(last) != gtk.BaseWidget(qw) {
				c.timelineBox.ReorderChildAfter(qw, last)
			}
		}
	}
	if c.thinkingLabel != nil {
		if c.thinkingLabel.Parent() == nil {
			c.timelineBox.Append(c.thinkingLabel)
		} else if last := c.timelineBox.LastChild(); last != nil && gtk.BaseWidget(last) != gtk.BaseWidget(c.thinkingLabel) {
			c.timelineBox.ReorderChildAfter(c.thinkingLabel, last)
		}
		c.thinkingLabel.SetVisible(c.active && len(c.pendingForms) == 0)
	}
	if c.revertBanner != nil && c.revertBanner.Visible() && c.revertBanner.Parent() != nil {
		if last := c.timelineBox.LastChild(); last != nil && gtk.BaseWidget(last) != gtk.BaseWidget(c.revertBanner) {
			c.timelineBox.ReorderChildAfter(c.revertBanner, last)
		}
	}
}

type switcherIndicator struct {
	box      *gtk.Box
	spinner  *adw.Spinner
	label    *gtk.Label
	closeBtn *gtk.Button
	rowPtr   uintptr
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
}

func (t *sessionTab) activeConv() *convTimeline {
	if t.activeSubID != "" {
		if sub, ok := t.subTimelines[t.activeSubID]; ok {
			return sub
		}
	}
	return t.main
}

func (t *sessionTab) blockingSessionID() string {
	if t == nil {
		return ""
	}
	if conv := t.activeConv(); conv.isBlockingOnBackgroundable() {
		return conv.session.ID
	}
	if t.main.isBlockingOnBackgroundable() {
		return t.main.session.ID
	}
	return ""
}

func (t *sessionTab) shouldShowBackgroundHint(now time.Time) bool {
	if t == nil {
		return false
	}
	if conv := t.activeConv(); conv.isBlockingForAtLeast(now, backgroundHintDelay) {
		return true
	}
	return t.main.isBlockingForAtLeast(now, backgroundHintDelay)
}

func (t *sessionTab) nextBackgroundHintDelay(now time.Time) (time.Duration, bool) {
	if t == nil {
		return 0, false
	}
	var shortest time.Duration
	var found bool
	for _, conv := range []*convTimeline{t.activeConv(), t.main} {
		if d, ok := conv.nextBackgroundHintDelay(now, backgroundHintDelay); ok {
			if !found || d < shortest {
				shortest = d
				found = true
			}
		}
	}
	return shortest, found
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

	rootContent                  *adw.OverlaySplitView
	hideSidebarBtn               *gtk.Button
	newSessionSidebarBtn         *gtk.MenuButton
	newSessionSidebarPopover     *gtk.Popover
	newSessionSidebarSearchEntry *gtk.SearchEntry
	newSessionSidebarListBox     *gtk.ListBox
	newSessionSidebarFolderBtn   *gtk.Button
	searchSessionsBtn            *gtk.Button
	viewSwitcherSidebar          *adw.ViewSwitcherSidebar

	bottomSheet              *adw.BottomSheet
	showSidebarBtn           *gtk.Button
	newSessionTopBtn         *gtk.MenuButton
	newSessionTopPopover     *gtk.Popover
	newSessionTopSearchEntry *gtk.SearchEntry
	newSessionTopListBox     *gtk.ListBox
	newSessionTopFolderBtn   *gtk.Button
	headerTitleStack         *gtk.Stack
	windowTitleBtn           *gtk.MenuButton
	windowTitle              *adw.WindowTitle
	renameSessionPopover     *gtk.Popover
	renameSessionEntry       *gtk.Entry
	renameSessionSaveBtn     *gtk.Button
	breadcrumbParentBtn      *gtk.Button
	breadcrumbParentLabel    *gtk.Label
	breadcrumbChildLabel     *gtk.Label
	newWindowBtn             *gtk.Button
	refreshBtn               *gtk.Button
	takeChargeBtn            *gtk.ToggleButton
	viewStack                *adw.ViewStack

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
	backgroundStatusBtn *gtk.Button
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
	contextSectionBox    *gtk.Box
	contextInfoGrid      *gtk.Grid
	agentListBox         *gtk.ListBox
	modelSearchEntry     *gtk.SearchEntry
	modelListBox         *gtk.ListBox

	activeShells      map[string]opencode.ShellItem
	shellOutputs      map[string]string
	shellOutputLabels map[string]*gtk.Label
	shellPollRunning  bool

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

	expandedProjects  map[string]bool
	collapsedProjects map[string]bool
	archivedSessions  map[string]bool
	projectOrder      []string
	statePath         string
	allSessions       []opencode.Session
	projectSessions   map[string][]opencode.Session

	closeBtnSessID   map[uintptr]string
	hoveredRow       map[uintptr]bool
	rowControllers   map[uintptr]bool
	rowProjectDir    map[uintptr]string
	headerProjectDir map[uintptr]string

	takeCharge         bool
	takeChargeInFlight map[string]bool

	populating bool
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
		ApplicationWindow:            adwWin,
		appWin:                       win,
		ctx:                          winCtx,
		cancel:                       cancel,
		client:                       client,
		rootContent:                  builder.GetObject("rootContent").Cast().(*adw.OverlaySplitView),
		hideSidebarBtn:               builder.GetObject("hideSidebarBtn").Cast().(*gtk.Button),
		newSessionSidebarBtn:         builder.GetObject("newSessionSidebarBtn").Cast().(*gtk.MenuButton),
		newSessionSidebarPopover:     builder.GetObject("newSessionSidebarPopover").Cast().(*gtk.Popover),
		newSessionSidebarSearchEntry: builder.GetObject("newSessionSidebarSearchEntry").Cast().(*gtk.SearchEntry),
		newSessionSidebarListBox:     builder.GetObject("newSessionSidebarListBox").Cast().(*gtk.ListBox),
		newSessionSidebarFolderBtn:   builder.GetObject("newSessionSidebarFolderBtn").Cast().(*gtk.Button),
		searchSessionsBtn:            builder.GetObject("searchSessionsBtn").Cast().(*gtk.Button),
		viewSwitcherSidebar:          builder.GetObject("viewSwitcherSidebar").Cast().(*adw.ViewSwitcherSidebar),
		bottomSheet:                  builder.GetObject("bottomSheet").Cast().(*adw.BottomSheet),
		showSidebarBtn:               builder.GetObject("showSidebarBtn").Cast().(*gtk.Button),
		newSessionTopBtn:             builder.GetObject("newSessionTopBtn").Cast().(*gtk.MenuButton),
		newSessionTopPopover:         builder.GetObject("newSessionTopPopover").Cast().(*gtk.Popover),
		newSessionTopSearchEntry:     builder.GetObject("newSessionTopSearchEntry").Cast().(*gtk.SearchEntry),
		newSessionTopListBox:         builder.GetObject("newSessionTopListBox").Cast().(*gtk.ListBox),
		newSessionTopFolderBtn:       builder.GetObject("newSessionTopFolderBtn").Cast().(*gtk.Button),
		headerTitleStack:             builder.GetObject("headerTitleStack").Cast().(*gtk.Stack),
		windowTitleBtn:               builder.GetObject("windowTitleBtn").Cast().(*gtk.MenuButton),
		windowTitle:                  builder.GetObject("windowTitle").Cast().(*adw.WindowTitle),
		renameSessionPopover:         builder.GetObject("renameSessionPopover").Cast().(*gtk.Popover),
		renameSessionEntry:           builder.GetObject("renameSessionEntry").Cast().(*gtk.Entry),
		renameSessionSaveBtn:         builder.GetObject("renameSessionSaveBtn").Cast().(*gtk.Button),
		breadcrumbParentBtn:          builder.GetObject("breadcrumbParentBtn").Cast().(*gtk.Button),
		breadcrumbParentLabel:        builder.GetObject("breadcrumbParentLabel").Cast().(*gtk.Label),
		breadcrumbChildLabel:         builder.GetObject("breadcrumbChildLabel").Cast().(*gtk.Label),
		newWindowBtn:                 builder.GetObject("newWindowBtn").Cast().(*gtk.Button),
		refreshBtn:                   builder.GetObject("refreshBtn").Cast().(*gtk.Button),
		takeChargeBtn:                builder.GetObject("takeChargeBtn").Cast().(*gtk.ToggleButton),
		viewStack:                    builder.GetObject("viewStack").Cast().(*adw.ViewStack),
		inputView:                    builder.GetObject("inputView").Cast().(*gtksource.View),
		sendBtn:                      builder.GetObject("sendBtn").Cast().(*gtk.Button),
		stopBtn:                      builder.GetObject("stopBtn").Cast().(*gtk.Button),
		attachBtn:                    builder.GetObject("attachBtn").Cast().(*gtk.Button),
		attachmentTray:               builder.GetObject("attachmentTray").Cast().(*adw.WrapBox),
		composerPlaceholder:          builder.GetObject("composerPlaceholder").Cast().(*gtk.Label),
		composerAgentBtn:             builder.GetObject("composerAgentBtn").Cast().(*gtk.MenuButton),
		composerModelBtn:             builder.GetObject("composerModelBtn").Cast().(*gtk.MenuButton),
		agentPopover:                 builder.GetObject("agentPopover").Cast().(*gtk.Popover),
		modelPopover:                 builder.GetObject("modelPopover").Cast().(*gtk.Popover),
		agentPillLabel:               builder.GetObject("agentPillLabel").Cast().(*gtk.Label),
		modelStatusLabel:             builder.GetObject("modelStatusLabel").Cast().(*gtk.Label),
		projectStatusBox:             builder.GetObject("projectStatusBox").Cast().(*gtk.Box),
		projectStatusLabel:           builder.GetObject("projectStatusLabel").Cast().(*gtk.Label),
		branchStatusBox:              builder.GetObject("branchStatusBox").Cast().(*gtk.Box),
		branchStatusLabel:            builder.GetObject("branchStatusLabel").Cast().(*gtk.Label),
		backgroundStatusBtn:          builder.GetObject("backgroundStatusBtn").Cast().(*gtk.Button),
		shellStatusBox:               builder.GetObject("shellStatusBox").Cast().(*gtk.Box),
		shellStatusLabel:             builder.GetObject("shellStatusLabel").Cast().(*gtk.Label),
		subagentStatusBox:            builder.GetObject("subagentStatusBox").Cast().(*gtk.Box),
		subagentStatusLabel:          builder.GetObject("subagentStatusLabel").Cast().(*gtk.Label),
		tokRateStatusLabel:           builder.GetObject("tokRateStatusLabel").Cast().(*gtk.Label),
		contextStatusLabel:           builder.GetObject("contextStatusLabel").Cast().(*gtk.Label),
		sheetEmptyLabel:              builder.GetObject("sheetEmptyLabel").Cast().(*gtk.Label),
		shellsSectionBox:             builder.GetObject("shellsSectionBox").Cast().(*gtk.Box),
		shellListBox:                 builder.GetObject("shellListBox").Cast().(*gtk.ListBox),
		subagentsSectionBox:          builder.GetObject("subagentsSectionBox").Cast().(*gtk.Box),
		sheetBackToParentBtn:         builder.GetObject("sheetBackToParentBtn").Cast().(*gtk.Button),
		subagentListBox:              builder.GetObject("subagentListBox").Cast().(*gtk.ListBox),
		contextSectionBox:            builder.GetObject("contextSectionBox").Cast().(*gtk.Box),
		contextInfoGrid:              builder.GetObject("contextInfoGrid").Cast().(*gtk.Grid),
		agentListBox:                 builder.GetObject("agentListBox").Cast().(*gtk.ListBox),
		modelSearchEntry:             builder.GetObject("modelSearchEntry").Cast().(*gtk.SearchEntry),
		modelListBox:                 builder.GetObject("modelListBox").Cast().(*gtk.ListBox),
		tabs:                         make(map[string]*sessionTab),
		childToRoot:                  make(map[string]string),
		modelNames:                   make(map[string]string),
		modelLimits:                  make(map[string]int),
		branchCache:                  make(map[string]string),
		modelLastUsed:                make(map[string]int64),
		expandedProjects:             make(map[string]bool),
		collapsedProjects:            make(map[string]bool),
		archivedSessions:             make(map[string]bool),
		projectSessions:              make(map[string][]opencode.Session),
		activeShells:                 make(map[string]opencode.ShellItem),
		shellOutputs:                 make(map[string]string),
		shellOutputLabels:            make(map[string]*gtk.Label),
		closeBtnSessID:               make(map[uintptr]string),
		hoveredRow:                   make(map[uintptr]bool),
		rowControllers:               make(map[uintptr]bool),
		rowProjectDir:                make(map[uintptr]string),
		headerProjectDir:             make(map[uintptr]string),
		takeChargeInFlight:           make(map[string]bool),
	}
	w.loadState()
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

	w.backgroundStatusBtn.ConnectClicked(func() {
		w.backgroundCurrentSession()
	})

	// Wire AdwViewStack to vertical sidebar switcher.
	w.viewSwitcherSidebar.SetStack(w.viewStack)

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

	// Sidebar show/hide.
	w.hideSidebarBtn.ConnectClicked(func() {
		w.rootContent.SetShowSidebar(false)
		w.syncNavigationChrome()
	})
	w.showSidebarBtn.ConnectClicked(func() {
		w.rootContent.SetShowSidebar(!w.rootContent.ShowSidebar())
		w.syncNavigationChrome()
	})
	w.rootContent.NotifyProperty("show-sidebar", func() {
		w.syncNavigationChrome()
	})
	w.rootContent.NotifyProperty("collapsed", func() {
		w.syncNavigationChrome()
	})

	// Mobile adaptive breakpoint: collapse sidebar into overlay on narrow screens.
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

	w.setupNewSessionPopover(w.newSessionSidebarPopover, w.newSessionSidebarSearchEntry, w.newSessionSidebarListBox, w.newSessionSidebarFolderBtn)
	w.setupNewSessionPopover(w.newSessionTopPopover, w.newSessionTopSearchEntry, w.newSessionTopListBox, w.newSessionTopFolderBtn)
	w.renameSessionPopover.ConnectShow(func() {
		if tab := w.currentTab(); tab != nil {
			w.renameSessionEntry.SetText(tab.main.session.Title)
			w.renameSessionEntry.GrabFocus()
		}
	})
	w.renameSessionEntry.ConnectActivate(w.submitRenameSession)
	w.renameSessionSaveBtn.ConnectClicked(w.submitRenameSession)
	w.searchSessionsBtn.ConnectClicked(w.openSessionSearchDialog)
	w.newWindowBtn.ConnectClicked(w.openNewWindow)
	w.refreshBtn.ConnectClicked(w.loadAll)
	w.takeChargeBtn.ConnectToggled(func() {
		w.setTakeCharge(w.takeChargeBtn.Active())
	})

	// Window-level shortcuts: Ctrl+Shift+N for New Window, Ctrl+K for Search Sessions, Ctrl+B to Background.
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
		if (keyval == gdk.KEY_B || keyval == gdk.KEY_b) && state.Has(gdk.ControlMask) && !state.Has(gdk.ShiftMask) {
			if w.backgroundCurrentSession() {
				return true
			}
		}
		return false
	})
	adwWin.AddController(winKeyCtrl)

	// Composer Enter / Ctrl+Enter / Shift+Enter handling, Ctrl+B to background, and Ctrl+V clipboard image pasting.
	steerKeyCtrl := gtk.NewEventControllerKey()
	steerKeyCtrl.SetPropagationPhase(gtk.PhaseCapture)
	steerKeyCtrl.ConnectKeyPressed(func(keyval, _ uint, state gdk.ModifierType) bool {
		if (keyval == gdk.KEY_Return || keyval == gdk.KEY_KP_Enter) && state.Has(gdk.ControlMask) && !state.Has(gdk.ShiftMask) {
			w.sendPromptWithDelivery(opencode.DeliverySteer)
			return true
		}
		if (keyval == gdk.KEY_B || keyval == gdk.KEY_b) && state.Has(gdk.ControlMask) && !state.Has(gdk.ShiftMask) {
			if w.backgroundCurrentSession() {
				return true
			}
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
	if sess.Time.Updated == 0 {
		sess.Time.Updated = cmp.Or(sess.Time.Created, time.Now().UnixMilli())
	}
	normDir := normalizeProjectDir(sess.Location.Directory)
	if w.ensureProjectOrder(normDir) {
		w.saveState()
	}
	if tab, ok := w.tabs[sess.ID]; ok {
		if sess.Title != "" && tab.main.session.Title != sess.Title {
			tab.main.session.Title = sess.Title
			if tab.page != nil {
				tab.page.SetTitle(sess.Title)
			}
		}
		if sess.Location.Directory != "" && tab.main.session.Location.Directory == "" {
			tab.main.session.Location = sess.Location
			w.syncSectionStarts()
		}
		if sess.Time.Updated > tab.main.session.Time.Updated {
			w.touchSessionUpdated(tab, tab.main, sess.Time.Updated)
		}
		return tab
	}
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
		} else if w.compareProjectOrder(normDir, tDir) < 0 {
			insertIdx = i
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
			if !ok || tailTab.stack == nil {
				continue
			}
			var needsAttention bool
			if tailTab.page != nil {
				needsAttention = tailTab.page.NeedsAttention()
			}
			title := strings.TrimSpace(tailTab.main.session.Title)
			if title == "" {
				title = "Untitled"
			}
			w.viewStack.Remove(tailTab.stack)
			page := w.viewStack.AddTitled(tailTab.stack, tailID, title)
			page.SetBadgeNumber(0)
			page.SetNeedsAttention(needsAttention)
			tailTab.page = page
			tailTab.sidebarIndicator = nil
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
	normDir := normalizeProjectDir(sess.Location.Directory)
	stateChanged := false
	if w.archivedSessions[sess.ID] {
		delete(w.archivedSessions, sess.ID)
		stateChanged = true
	}
	if w.collapsedProjects[normDir] {
		delete(w.collapsedProjects, normDir)
		stateChanged = true
	}
	if stateChanged {
		w.saveState()
	}
	w.ensureSessionTab(sess)
	w.syncProjectSessionVisibility()
	w.viewStack.SetVisibleChildName(sess.ID)
	w.onVisibleTabChanged()
	w.syncSwitcherIndicators()
}

func (w *Window) archiveSession(sessID string) {
	if sessID == "" {
		return
	}
	tab, ok := w.tabs[sessID]
	var rawDir string
	if ok {
		rawDir = tab.main.session.Location.Directory
	} else {
		for _, s := range w.allSessions {
			if s.ID == sessID {
				rawDir = s.Location.Directory
				break
			}
		}
	}
	normDir := normalizeProjectDir(rawDir)

	w.archivedSessions[sessID] = true
	w.saveState()

	if w.projectSessions != nil {
		filtered := w.projectSessions[normDir][:0]
		for _, s := range w.projectSessions[normDir] {
			if s.ID != sessID && !w.archivedSessions[s.ID] {
				filtered = append(filtered, s)
			}
		}
		w.projectSessions[normDir] = filtered

		// If this project is not expanded and now has fewer than 5 open tabs in w.tabs,
		// promote the next unarchived session from w.projectSessions[normDir] so up to 5 remain visible.
		if !w.expandedProjects[normDir] {
			for _, candidate := range w.projectSessions[normDir] {
				if _, exists := w.tabs[candidate.ID]; !exists {
					w.ensureSessionTab(candidate)
					break
				}
			}
		}
	}

	if !ok {
		return
	}

	wasCurrent := (w.currentRootSessionID() == sessID)
	var nextID string
	if wasCurrent {
		idx := slices.Index(w.tabOrder, sessID)
		// Prefer another tab in the same project first, then any neighboring tab.
		for i := idx + 1; i < len(w.tabOrder); i++ {
			id := w.tabOrder[i]
			if t := w.tabs[id]; t != nil && normalizeProjectDir(t.main.session.Location.Directory) == normDir {
				nextID = id
				break
			}
		}
		if nextID == "" {
			for i := idx - 1; i >= 0; i-- {
				id := w.tabOrder[i]
				if t := w.tabs[id]; t != nil && normalizeProjectDir(t.main.session.Location.Directory) == normDir {
					nextID = id
					break
				}
			}
		}
		if nextID == "" {
			for i := idx + 1; i < len(w.tabOrder); i++ {
				if id := w.tabOrder[i]; id != sessID && w.tabs[id] != nil {
					nextID = id
					break
				}
			}
		}
		if nextID == "" {
			for i := idx - 1; i >= 0; i-- {
				if id := w.tabOrder[i]; id != sessID && w.tabs[id] != nil {
					nextID = id
					break
				}
			}
		}
	}

	prevPopulating := w.populating
	w.populating = true
	tab.main.closeStreams()
	for _, sub := range tab.subTimelines {
		sub.closeStreams()
	}
	w.viewStack.Remove(tab.stack)
	delete(w.tabs, sessID)
	if idx := slices.Index(w.tabOrder, sessID); idx >= 0 {
		w.tabOrder = slices.Delete(w.tabOrder, idx, idx+1)
	}
	w.syncSectionStarts()
	w.populating = prevPopulating

	w.syncProjectSessionVisibility()
	if wasCurrent && nextID != "" {
		w.viewStack.SetVisibleChildName(nextID)
		w.onVisibleTabChanged()
	}
	w.syncSwitcherIndicators()
}

func (w *Window) clearProjectSessions(rawDir string) {
	normDir := normalizeProjectDir(rawDir)
	if normDir == "" {
		return
	}
	if w.archivedSessions == nil {
		w.archivedSessions = make(map[string]bool)
	}

	toArchive := make(map[string]bool)
	for _, s := range w.allSessions {
		if s.ParentID == "" && normalizeProjectDir(s.Location.Directory) == normDir && s.ID != "" {
			toArchive[s.ID] = true
		}
	}
	if w.projectSessions != nil {
		for _, s := range w.projectSessions[normDir] {
			if s.ID != "" {
				toArchive[s.ID] = true
			}
		}
		delete(w.projectSessions, normDir)
	}
	for id, tab := range w.tabs {
		if tab != nil && normalizeProjectDir(tab.main.session.Location.Directory) == normDir {
			toArchive[id] = true
		}
	}
	for id := range toArchive {
		w.archivedSessions[id] = true
	}
	delete(w.collapsedProjects, normDir)
	delete(w.expandedProjects, normDir)
	w.saveState()

	currentID := w.currentRootSessionID()
	wasInProject := toArchive[currentID]
	var nextID string
	if wasInProject {
		idx := slices.Index(w.tabOrder, currentID)
		for i := idx + 1; i < len(w.tabOrder); i++ {
			id := w.tabOrder[i]
			if !toArchive[id] && w.tabs[id] != nil {
				nextID = id
				break
			}
		}
		if nextID == "" {
			for i := idx - 1; i >= 0; i-- {
				id := w.tabOrder[i]
				if !toArchive[id] && w.tabs[id] != nil {
					nextID = id
					break
				}
			}
		}
	}

	prevPopulating := w.populating
	w.populating = true
	var nextOrder []string
	for _, id := range w.tabOrder {
		if !toArchive[id] {
			nextOrder = append(nextOrder, id)
			continue
		}
		if tab, ok := w.tabs[id]; ok {
			tab.main.closeStreams()
			for _, sub := range tab.subTimelines {
				sub.closeStreams()
			}
			if w.viewStack != nil && tab.stack != nil {
				w.viewStack.Remove(tab.stack)
			}
			delete(w.tabs, id)
		}
	}
	w.tabOrder = nextOrder
	w.syncSectionStarts()
	w.populating = prevPopulating

	w.syncProjectSessionVisibility()
	if wasInProject && w.viewStack != nil {
		if nextID != "" {
			w.viewStack.SetVisibleChildName(nextID)
		}
		w.onVisibleTabChanged()
	}
	w.syncSwitcherIndicators()
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

			if w.archivedSessions[m.sess.ID] {
				archLabel := gtk.NewLabel("Archived")
				archLabel.AddCSSClass("caption")
				archLabel.AddCSSClass("glup-search-archived-badge")
				archLabel.SetVAlign(gtk.AlignCenter)
				rowBox.Append(archLabel)
			}

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

	mergeSessions := func(incoming []opencode.Session) {
		sessionsLock.Lock()
		defer sessionsLock.Unlock()
		byID := make(map[string]int, len(sessionPool))
		for i, s := range sessionPool {
			byID[s.ID] = i
		}
		for _, s := range incoming {
			if idx, ok := byID[s.ID]; ok {
				sessionPool[idx] = s
			} else {
				byID[s.ID] = len(sessionPool)
				sessionPool = append(sessionPool, s)
			}
		}
		w.allSessions = slices.Clone(sessionPool)
	}

	searchEntry.ConnectSearchChanged(func() {
		updateList()
		q := strings.TrimSpace(searchEntry.Text())
		if q == "" || w.client == nil {
			return
		}
		gtkutil.Async(w.ctx, func() func() {
			found, err := w.client.SearchSessions(w.ctx, q, 50)
			if err != nil || len(found) == 0 {
				return func() {}
			}
			return func() {
				mergeSessions(found)
				updateList()
			}
		})
	})
	updateList()

	gtkutil.Async(w.ctx, func() func() {
		sessions, err := w.client.ListSessions(w.ctx, 200)
		if err != nil || len(sessions) == 0 {
			return func() {}
		}
		return func() {
			mergeSessions(sessions)
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

	thinkingLabel := gtk.NewLabel("Thinking...")
	thinkingLabel.SetXAlign(0)
	thinkingLabel.SetHAlign(gtk.AlignStart)
	thinkingLabel.AddCSSClass("dim-label")
	thinkingLabel.AddCSSClass("glup-thinking-label")
	thinkingLabel.SetVisible(false)
	timelineBox.Append(thinkingLabel)

	conv := &convTimeline{
		session:            sess,
		scroll:             scroll,
		timelineBox:        timelineBox,
		thinkingLabel:      thinkingLabel,
		assistantMap:       make(map[string]*AssistantMessageWidget),
		compactionMap:      make(map[string]*CompactionWidget),
		messageWidgets:     make(map[string]gtk.Widgetter),
		formWidgets:        make(map[string]*QuestionFormWidget),
		seenMsgIDs:         make(map[string]bool),
		blockingTools:      make(map[string]time.Time),
		blockingShellTools: make(map[string]bool),
		blockingShellIDs:   make(map[string]string),
		backgroundShellIDs: make(map[string]bool),
		toolNames:          make(map[string]string),
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

func (w *Window) syncNavigationChrome() {
	tab := w.currentTab()
	viewingSubagent := tab != nil && tab.activeSubID != ""

	if w.windowTitleBtn != nil {
		w.windowTitleBtn.SetSensitive(tab != nil)
	}
	if w.headerTitleStack != nil {
		if viewingSubagent {
			w.headerTitleStack.SetVisibleChildName("breadcrumb")
		} else {
			w.headerTitleStack.SetVisibleChildName("title")
		}
	}
	if w.rootContent != nil {
		sidebarVisible := w.rootContent.ShowSidebar() && !w.rootContent.Collapsed()
		if w.showSidebarBtn != nil {
			w.showSidebarBtn.SetVisible(!sidebarVisible)
		}
		if w.newSessionTopBtn != nil {
			w.newSessionTopBtn.SetVisible(!sidebarVisible)
		}
	}
}

func (w *Window) applySessionRename(sessionID, newTitle string) {
	newTitle = strings.TrimSpace(newTitle)
	if sessionID == "" || newTitle == "" {
		return
	}
	for i := range w.allSessions {
		if w.allSessions[i].ID == sessionID {
			w.allSessions[i].Title = newTitle
		}
	}
	for dir, list := range w.projectSessions {
		for i := range list {
			if list[i].ID == sessionID {
				list[i].Title = newTitle
				w.projectSessions[dir] = list
			}
		}
	}
	tab, conv := w.resolveConv(sessionID)
	if conv != nil {
		conv.session.Title = newTitle
	}
	if rootID, ok := w.childToRoot[sessionID]; ok {
		if parentTab, ok := w.tabs[rootID]; ok {
			w.upsertSubagent(parentTab, opencode.Session{ID: sessionID, ParentID: rootID, Title: newTitle})
			if w.currentTab() == parentTab {
				w.rebuildSubagentList(parentTab)
			}
		}
	}
	if tab != nil {
		if conv == tab.main && tab.page != nil {
			tab.page.SetTitle(newTitle)
		}
		if w.currentTab() == tab {
			w.refreshHeaderAndBottomBar(tab)
		}
		w.syncSwitcherIndicators()
	}
}

func (w *Window) submitRenameSession() {
	if w.renameSessionPopover != nil {
		w.renameSessionPopover.Popdown()
	}
	tab := w.currentTab()
	if tab == nil || w.renameSessionEntry == nil {
		return
	}
	newTitle := strings.TrimSpace(w.renameSessionEntry.Text())
	sessID := tab.main.session.ID
	if sessID == "" || newTitle == "" || newTitle == strings.TrimSpace(tab.main.session.Title) {
		return
	}
	w.applySessionRename(sessID, newTitle)
	if w.client == nil {
		return
	}
	gtkutil.Async(w.ctx, func() func() {
		err := w.client.RenameSession(w.ctx, sessID, newTitle)
		return func() {
			if err != nil {
				slog.Error("failed to rename session", "sessionID", sessID, "err", err)
				app.Error(w.ctx, err)
			}
		}
	})
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

func (w *Window) getOrCreateSwitcherIndicator(hBox *gtk.Box, row *gtk.ListBoxRow, sessID string) *switcherIndicator {
	if hBox == nil {
		return nil
	}
	hBoxPtr := hBox.Object.Native()
	if row != nil {
		if !w.rowControllers[hBoxPtr] {
			w.rowControllers[hBoxPtr] = true
			hp := hBoxPtr
			motion := gtk.NewEventControllerMotion()
			motion.ConnectEnter(func(_, _ float64) {
				w.hoveredRow[hp] = true
				w.updateSwitcherIndicators()
			})
			motion.ConnectLeave(func() {
				delete(w.hoveredRow, hp)
				w.updateSwitcherIndicators()
			})
			hBox.AddController(motion)
		}

		rowPtr := row.Object.Native()
		if !w.rowControllers[rowPtr] {
			w.rowControllers[rowPtr] = true
			rp := rowPtr
			dropTarget := gtk.NewDropTarget(glib.TypeString, gdk.ActionMove)
			dropTarget.ConnectEnter(func(_, _ float64) gdk.DragAction {
				row.AddCSSClass("glup-project-drop-target")
				return gdk.ActionMove
			})
			dropTarget.ConnectLeave(func() {
				row.RemoveCSSClass("glup-project-drop-target")
			})
			dropTarget.ConnectDrop(func(val *glib.Value, _, _ float64) bool {
				row.RemoveCSSClass("glup-project-drop-target")
				srcDir := val.String()
				dstDir := w.rowProjectDir[rp]
				if srcDir == "" || dstDir == "" || srcDir == dstDir {
					return false
				}
				w.moveProjectInOrder(srcDir, dstDir)
				return true
			})
			row.AddController(dropTarget)
		}
	}

	var spinner *adw.Spinner
	var suffixBox *gtk.Box
	var label *gtk.Label
	var closeBtn *gtk.Button

	for child := hBox.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
		childBase := gtk.BaseWidget(child)
		if childBase == nil {
			continue
		}
		if childBase.HasCSSClass("glup-switcher-prefix") {
			if sp, ok := childBase.Cast().(*adw.Spinner); ok {
				spinner = sp
			}
		}
		if childBase.HasCSSClass("glup-switcher-suffix") {
			for inner := childBase.FirstChild(); inner != nil; inner = gtk.BaseWidget(inner).NextSibling() {
				innerBase := gtk.BaseWidget(inner)
				if innerBase == nil {
					continue
				}
				if lbl, ok := innerBase.Cast().(*gtk.Label); ok {
					label = lbl
				}
				if btn, ok := innerBase.Cast().(*gtk.Button); ok {
					closeBtn = btn
				}
			}
			if box, ok := childBase.Cast().(*gtk.Box); ok {
				suffixBox = box
			}
		}
	}
	if spinner != nil && suffixBox != nil && label != nil && closeBtn != nil {
		w.closeBtnSessID[closeBtn.Object.Native()] = sessID
		return &switcherIndicator{box: suffixBox, spinner: spinner, label: label, closeBtn: closeBtn, rowPtr: hBoxPtr}
	}

	if spinner == nil {
		spinner = adw.NewSpinner()
		spinner.AddCSSClass("glup-switcher-prefix")
		spinner.SetVAlign(gtk.AlignCenter)
		spinner.SetVisible(false)
		hBox.Prepend(spinner)
	}

	box := gtk.NewBox(gtk.OrientationHorizontal, 4)
	box.AddCSSClass("glup-switcher-suffix")
	box.SetHAlign(gtk.AlignEnd)
	box.SetVAlign(gtk.AlignCenter)
	box.SetHExpand(false)

	label = gtk.NewLabel("")
	label.AddCSSClass("caption")
	label.AddCSSClass("dim-label")
	label.SetVisible(false)
	box.Append(label)

	closeBtn = gtk.NewButtonFromIconName("window-close-symbolic")
	closeBtn.AddCSSClass("flat")
	closeBtn.AddCSSClass("circular")
	closeBtn.AddCSSClass("glup-sidebar-close-btn")
	closeBtn.SetTooltipText("Archive Session")
	closeBtn.SetVAlign(gtk.AlignCenter)
	closeBtn.SetVisible(false)
	btnPtr := closeBtn.Object.Native()
	w.closeBtnSessID[btnPtr] = sessID
	closeBtn.ConnectClicked(func() {
		if id := w.closeBtnSessID[btnPtr]; id != "" {
			w.archiveSession(id)
		}
	})
	box.Append(closeBtn)

	hBox.SetVAlign(gtk.AlignCenter)
	hBox.Append(box)
	return &switcherIndicator{box: box, spinner: spinner, label: label, closeBtn: closeBtn, rowPtr: hBoxPtr}
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

func (w *Window) newProjectHeaderBox(rawDir string, includeSeparator bool) *gtk.Box {
	normDir := normalizeProjectDir(rawDir)
	outerBox := gtk.NewBox(gtk.OrientationVertical, 0)
	outerBox.AddCSSClass("glup-sidebar-project-header")
	outerBox.SetMarginStart(8)
	outerBox.SetMarginEnd(8)
	if includeSeparator {
		outerBox.SetMarginTop(2)
	} else {
		outerBox.SetMarginTop(6)
	}
	outerBox.SetMarginBottom(2)

	if includeSeparator {
		sep := gtk.NewSeparator(gtk.OrientationHorizontal)
		sep.SetMarginTop(4)
		sep.SetMarginBottom(6)
		sep.SetMarginStart(6)
		sep.SetMarginEnd(2)
		outerBox.Append(sep)
	}

	hBox := gtk.NewBox(gtk.OrientationHorizontal, 6)
	hBox.AddCSSClass("glup-sidebar-project-header-box")

	chevron := gtk.NewImageFromIconName("chevron-right-symbolic")
	chevron.AddCSSClass("glup-sidebar-project-chevron")
	if !w.collapsedProjects[normDir] {
		chevron.AddCSSClass("glup-sidebar-project-chevron-expanded")
	}
	chevron.SetVAlign(gtk.AlignCenter)
	hBox.Append(chevron)

	folderIcon := gtk.NewImageFromIconName("folder-symbolic")
	folderIcon.AddCSSClass("glup-sidebar-folder-icon")
	folderIcon.SetVAlign(gtk.AlignCenter)
	hBox.Append(folderIcon)

	textBox := gtk.NewBox(gtk.OrientationVertical, 1)
	textBox.SetHExpand(true)

	titleLabel := gtk.NewLabel(projectName(rawDir))
	titleLabel.AddCSSClass("glup-sidebar-project-title")
	titleLabel.SetXAlign(0)
	titleLabel.SetEllipsize(pango.EllipsizeMiddle)
	titleLabel.SetHExpand(true)
	textBox.Append(titleLabel)

	subtitleLabel := gtk.NewLabel(shortDir(rawDir))
	subtitleLabel.AddCSSClass("glup-sidebar-project-subtitle")
	subtitleLabel.AddCSSClass("dim-label")
	subtitleLabel.AddCSSClass("caption")
	subtitleLabel.SetXAlign(0)
	subtitleLabel.SetEllipsize(pango.EllipsizeMiddle)
	subtitleLabel.SetHExpand(true)
	textBox.Append(subtitleLabel)

	hBox.Append(textBox)

	countLabel := gtk.NewLabel("")
	countLabel.AddCSSClass("caption")
	countLabel.AddCSSClass("dim-label")
	countLabel.AddCSSClass("glup-sidebar-project-count")
	countLabel.SetVAlign(gtk.AlignCenter)
	countLabel.SetVisible(false)
	hBox.Append(countLabel)

	ptr := outerBox.Object.Native()
	w.headerProjectDir[ptr] = normDir

	clearBtn := gtk.NewButtonFromIconName("window-close-symbolic")
	clearBtn.AddCSSClass("flat")
	clearBtn.AddCSSClass("circular")
	clearBtn.AddCSSClass("glup-sidebar-close-btn")
	clearBtn.AddCSSClass("glup-sidebar-project-close-btn")
	clearBtn.SetTooltipText("Hide Project")
	clearBtn.SetVAlign(gtk.AlignCenter)
	clearBtn.ConnectClicked(func() {
		if d := w.headerProjectDir[ptr]; d != "" {
			w.clearProjectSessions(d)
		}
	})
	hBox.Append(clearBtn)

	outerBox.Append(hBox)

	click := gtk.NewGestureClick()
	click.SetButton(1)
	click.ConnectReleased(func(_ int, x, y float64) {
		if picked := hBox.Pick(x, y, gtk.PickDefault); picked != nil {
			for p := gtk.Widgetter(picked); p != nil; p = gtk.BaseWidget(p).Parent() {
				if gtk.BaseWidget(p).Object.Native() == clearBtn.Object.Native() {
					return
				}
				if gtk.BaseWidget(p).Object.Native() == hBox.Object.Native() {
					break
				}
			}
		}
		click.SetState(gtk.EventSequenceClaimed)
		if d := w.headerProjectDir[ptr]; d != "" {
			w.toggleProjectCollapsed(d)
		}
	})
	hBox.AddController(click)

	dragSource := gtk.NewDragSource()
	dragSource.SetActions(gdk.ActionMove)
	dragSource.ConnectPrepare(func(_, _ float64) *gdk.ContentProvider {
		d := w.headerProjectDir[ptr]
		if d == "" {
			return nil
		}
		return gdk.NewContentProviderForValue(glib.NewValue(d))
	})
	dragSource.ConnectDragBegin(func(_ gdk.Dragger) {
		hBox.AddCSSClass("glup-project-dragging")
	})
	dragSource.ConnectDragEnd(func(_ gdk.Dragger, _ bool) {
		hBox.RemoveCSSClass("glup-project-dragging")
	})
	hBox.AddController(dragSource)

	dropTarget := gtk.NewDropTarget(glib.TypeString, gdk.ActionMove)
	dropTarget.ConnectEnter(func(_, _ float64) gdk.DragAction {
		hBox.AddCSSClass("glup-project-drop-target")
		return gdk.ActionMove
	})
	dropTarget.ConnectLeave(func() {
		hBox.RemoveCSSClass("glup-project-drop-target")
	})
	dropTarget.ConnectDrop(func(val *glib.Value, _, _ float64) bool {
		hBox.RemoveCSSClass("glup-project-drop-target")
		srcDir := val.String()
		dstDir := w.headerProjectDir[ptr]
		if srcDir == "" || dstDir == "" || srcDir == dstDir {
			return false
		}
		w.moveProjectInOrder(srcDir, dstDir)
		return true
	})
	outerBox.AddController(dropTarget)

	return outerBox
}

func (w *Window) updateProjectHeaderLabels(root gtk.Widgetter, rawDir string) {
	if root == nil {
		return
	}
	base := gtk.BaseWidget(root)
	if base == nil {
		return
	}
	normDir := normalizeProjectDir(rawDir)
	w.headerProjectDir[base.Object.Native()] = normDir
	collapsed := w.collapsedProjects[normDir]
	total := len(w.projectSessions[normDir])

	var titleLbl, subLbl, countLbl *gtk.Label
	var chevronImg *gtk.Image
	var visit func(wid gtk.Widgetter)
	visit = func(wid gtk.Widgetter) {
		bw := gtk.BaseWidget(wid)
		if bw == nil {
			return
		}
		if bw.HasCSSClass("glup-sidebar-project-header") {
			w.headerProjectDir[bw.Object.Native()] = normDir
		}
		if bw.HasCSSClass("glup-sidebar-project-title") {
			if l, ok := bw.Cast().(*gtk.Label); ok {
				titleLbl = l
			}
		} else if bw.HasCSSClass("glup-sidebar-project-subtitle") {
			if l, ok := bw.Cast().(*gtk.Label); ok {
				subLbl = l
			}
		} else if bw.HasCSSClass("glup-sidebar-project-count") {
			if l, ok := bw.Cast().(*gtk.Label); ok {
				countLbl = l
			}
		} else if bw.HasCSSClass("glup-sidebar-project-chevron") {
			if img, ok := bw.Cast().(*gtk.Image); ok {
				chevronImg = img
			}
		}
		for c := bw.FirstChild(); c != nil; c = gtk.BaseWidget(c).NextSibling() {
			visit(c)
		}
	}
	visit(base)
	if titleLbl != nil {
		titleLbl.SetText(projectName(rawDir))
	}
	if subLbl != nil {
		subLbl.SetText(shortDir(rawDir))
	}
	if chevronImg != nil {
		if collapsed {
			chevronImg.RemoveCSSClass("glup-sidebar-project-chevron-expanded")
		} else {
			chevronImg.AddCSSClass("glup-sidebar-project-chevron-expanded")
		}
	}
	if countLbl != nil {
		if collapsed && total > 0 {
			countLbl.SetText(fmt.Sprintf("%d", total))
			countLbl.SetVisible(true)
		} else {
			countLbl.SetVisible(false)
		}
	}
}

func (w *Window) updateRowProjectHeader(row *gtk.ListBoxRow, dir string, sectionIdx int) {
	header := row.Header()
	if header == nil {
		return
	}
	headerBase := gtk.BaseWidget(header)
	if headerBase == nil {
		return
	}

	headerBase.AddCSSClass("glup-sidebar-project-header")
	headerBase.SetTooltipText(fmt.Sprintf("%s (%s) — click to collapse/expand, drag to reorder", projectName(dir), shortDir(dir)))

	if stack, ok := headerBase.Cast().(*gtk.Stack); ok {
		stack.SetVhomogeneous(false)
		childName := "glup_header"
		if sectionIdx > 0 {
			childName = "glup_header_sep"
		}
		child := stack.ChildByName(childName)
		if child != nil {
			w.updateProjectHeaderLabels(child, dir)
			stack.SetVisibleChildName(childName)
			return
		}

		hBox := w.newProjectHeaderBox(dir, sectionIdx > 0)
		w.updateProjectHeaderLabels(hBox, dir)
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
			w.updateProjectHeaderLabels(header, dir)
			return
		}
	}

	hBox := w.newProjectHeaderBox(dir, sectionIdx > 0)
	w.updateProjectHeaderLabels(hBox, dir)
	hBox.SetTooltipText(fmt.Sprintf("%s (%s) — click to collapse/expand, drag to reorder", projectName(dir), shortDir(dir)))
	row.SetHeader(hBox)
}

func (w *Window) toggleProjectCollapsed(normDir string) {
	if normDir == "" {
		return
	}
	if w.collapsedProjects[normDir] {
		delete(w.collapsedProjects, normDir)
	} else {
		w.collapsedProjects[normDir] = true
	}
	w.saveState()
	w.syncProjectSessionVisibility()
	w.syncSwitcherIndicators()
}

func (w *Window) moveProjectInOrder(srcDir, dstDir string) {
	normSrc := normalizeProjectDir(srcDir)
	normDst := normalizeProjectDir(dstDir)
	w.ensureProjectOrder(normSrc, normDst)
	next, ok := moveProjectSlice(w.projectOrder, normSrc, normDst)
	if !ok {
		return
	}
	w.projectOrder = next
	w.saveState()

	prevPopulating := w.populating
	w.populating = true
	w.reorderTabsByProject()
	w.syncSectionStarts()
	w.populating = prevPopulating

	w.syncProjectSessionVisibility()
	w.syncSwitcherIndicators()
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
		if w.collapsedProjects[dir] {
			// Keep the first tab page visible so AdwViewSwitcherSidebar's ListBox
			// preserves the section header row, and hide its row body in syncSwitcherIndicators.
			for i, tab := range tabs {
				tab.page.SetVisible(i == 0)
			}
			continue
		}
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

func (w *Window) compareTabIDs(idA, idB string) int {
	if idA == idB {
		return 0
	}
	tabA := w.tabs[idA]
	tabB := w.tabs[idB]
	if tabA == nil || tabB == nil {
		return 0
	}
	dirA := normalizeProjectDir(tabA.main.session.Location.Directory)
	dirB := normalizeProjectDir(tabB.main.session.Location.Directory)
	if dirA != dirB {
		return w.compareProjectOrder(dirA, dirB)
	}
	timeA := cmp.Or(tabA.main.session.Time.Updated, tabA.main.session.Time.Created)
	timeB := cmp.Or(tabB.main.session.Time.Updated, tabB.main.session.Time.Created)
	if c := cmp.Compare(timeB, timeA); c != 0 {
		return c
	}
	return cmp.Compare(idB, idA)
}

func (w *Window) reorderTabsByProject() bool {
	if len(w.tabOrder) <= 1 || slices.IsSortedFunc(w.tabOrder, w.compareTabIDs) {
		return false
	}
	oldOrder := slices.Clone(w.tabOrder)
	slices.SortFunc(w.tabOrder, w.compareTabIDs)
	firstDiff := 0
	for firstDiff < len(w.tabOrder) && w.tabOrder[firstDiff] == oldOrder[firstDiff] {
		firstDiff++
	}
	if firstDiff >= len(w.tabOrder) || w.viewStack == nil {
		return firstDiff < len(w.tabOrder)
	}

	prevPopulating := w.populating
	w.populating = true
	currID := w.viewStack.VisibleChildName()
	for _, id := range w.tabOrder[firstDiff:] {
		tab := w.tabs[id]
		if tab == nil || tab.stack == nil {
			continue
		}
		var needsAttention bool
		if tab.page != nil {
			needsAttention = tab.page.NeedsAttention()
		}
		w.viewStack.Remove(tab.stack)
		title := strings.TrimSpace(tab.main.session.Title)
		if title == "" {
			title = "Untitled"
		}
		page := w.viewStack.AddTitled(tab.stack, id, title)
		page.SetBadgeNumber(0)
		page.SetNeedsAttention(needsAttention)
		tab.page = page
		tab.sidebarIndicator = nil
	}
	if currID != "" && w.tabs[currID] != nil {
		w.viewStack.SetVisibleChildName(currID)
	}
	w.populating = prevPopulating
	return true
}

func (w *Window) touchSessionUpdated(tab *sessionTab, conv *convTimeline, updatedMs int64) {
	if updatedMs <= 0 {
		updatedMs = time.Now().UnixMilli()
	}
	if conv != nil && updatedMs > conv.session.Time.Updated {
		conv.session.Time.Updated = updatedMs
	}
	if tab == nil || tab.main == nil {
		return
	}
	if updatedMs > tab.main.session.Time.Updated {
		tab.main.session.Time.Updated = updatedMs
	}
	sessID := tab.main.session.ID
	for i := range w.allSessions {
		if w.allSessions[i].ID == sessID && updatedMs > w.allSessions[i].Time.Updated {
			w.allSessions[i].Time.Updated = updatedMs
		}
	}
	dir := normalizeProjectDir(tab.main.session.Location.Directory)
	if list := w.projectSessions[dir]; len(list) > 0 {
		for i := range list {
			if list[i].ID == sessID && updatedMs > list[i].Time.Updated {
				list[i].Title = cmp.Or(tab.main.session.Title, list[i].Title)
				list[i].Time.Updated = updatedMs
			}
		}
		slices.SortFunc(list, func(a, b opencode.Session) int {
			return cmp.Compare(cmp.Or(b.Time.Updated, b.Time.Created), cmp.Or(a.Time.Updated, a.Time.Created))
		})
		w.projectSessions[dir] = list
	}
	if w.reorderTabsByProject() {
		w.syncSectionStarts()
		w.syncProjectSessionVisibility()
		w.syncSwitcherIndicators()
	} else {
		w.updateSwitcherIndicators()
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
		if w.expandedProjects[dir] || w.collapsedProjects[dir] {
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
	var lb *gtk.ListBox
	if w.viewSwitcherSidebar != nil {
		lb = findListBox(w.viewSwitcherSidebar)
	}
	if lb != nil {
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
				if tab, ok := w.tabs[sessID]; ok {
					rawDir := tab.main.session.Location.Directory
					dir := normalizeProjectDir(rawDir)
					w.rowProjectDir[row.Object.Native()] = dir
					collapsed := w.collapsedProjects[dir]

					if tab.page.StartsSection() {
						w.updateRowProjectHeader(row, rawDir, sectionIdx)
						sectionIdx++
					} else if hdr := row.Header(); hdr != nil {
						if hb := gtk.BaseWidget(hdr); hb != nil && hb.HasCSSClass("glup-sidebar-project-header") {
							row.SetHeader(nil)
						}
					}

					if collapsed {
						updateRowShowMore(row, 0, nil)
						row.RemoveCSSClass("glup-sidebar-conversation-row")
						row.AddCSSClass("glup-sidebar-row-collapsed")
						row.SetActivatable(false)
						row.SetSelectable(false)
						if rc := row.Child(); rc != nil {
							gtk.BaseWidget(rc).SetVisible(false)
						}
					} else {
						row.RemoveCSSClass("glup-sidebar-row-collapsed")
						row.AddCSSClass("glup-sidebar-conversation-row")
						row.SetActivatable(true)
						row.SetSelectable(true)
						if rc := row.Child(); rc != nil {
							gtk.BaseWidget(rc).SetVisible(true)
						}
						if sessID == currentID {
							activeRow = row
						}
						if hBox := findFirstHorizontalBox(row.Child()); hBox != nil {
							hBox.AddCSSClass("glup-sidebar-conversation-box")
							tab.sidebarIndicator = w.getOrCreateSwitcherIndicator(hBox, row, sessID)
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
			}
			rowIdx++
		}
		if activeRow != nil {
			lb.SelectRow(activeRow)
		} else {
			lb.UnselectAll()
		}
	}

	w.updateSwitcherIndicators()
}

func (w *Window) updateIndicator(ind *switcherIndicator, sessID string, tab *sessionTab) {
	if ind == nil || tab == nil {
		return
	}
	if ind.closeBtn != nil {
		w.closeBtnSessID[ind.closeBtn.Object.Native()] = sessID
	}
	focused := (sessID == w.currentRootSessionID())
	active := tab.main.active
	hovered := ind.rowPtr != 0 && w.hoveredRow[ind.rowPtr]
	showClose := ind.closeBtn != nil && (hovered || focused)
	if ind.closeBtn != nil {
		ind.closeBtn.SetVisible(showClose)
	}
	if ind.spinner != nil {
		ind.spinner.SetVisible(active)
	}

	switch {
	case showClose:
		ind.label.SetVisible(false)
		ind.box.SetVisible(true)
	case focused:
		ind.label.SetVisible(false)
		ind.box.SetVisible(false)
	default:
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
	var discoveredDirs []string
	for _, s := range sessions {
		if s.ParentID != "" {
			continue
		}
		if s.Location.Directory != "" {
			discoveredDirs = append(discoveredDirs, s.Location.Directory)
		}
		if w.archivedSessions[s.ID] && s.ID != prevSelected {
			continue
		}
		topSessions = append(topSessions, s)
	}
	if w.ensureProjectOrder(discoveredDirs...) {
		w.saveState()
	}

	slices.SortStableFunc(topSessions, func(a, b opencode.Session) int {
		if c := w.compareProjectOrder(a.Location.Directory, b.Location.Directory); c != 0 {
			return c
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
	var firstID, firstUncollapsedID string
	for _, sess := range topSessions {
		dir := normalizeProjectDir(sess.Location.Directory)
		count := projCount[dir]
		_, isRunning := active[sess.ID]
		if count < 5 || sess.ID == prevSelected || isRunning || w.expandedProjects[dir] {
			if firstID == "" {
				firstID = sess.ID
			}
			if firstUncollapsedID == "" && !w.collapsedProjects[dir] {
				firstUncollapsedID = sess.ID
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
		if firstUncollapsedID != "" {
			targetID = firstUncollapsedID
		} else {
			targetID = firstID
		}
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
	w.loadPendingFormsForAllDirs(discoveredDirs)
}

func (w *Window) loadPendingFormsForAllDirs(dirs []string) {
	seen := make(map[string]bool)
	var uniqueDirs []string
	for _, raw := range dirs {
		d := normalizeProjectDir(raw)
		if d != "" && !seen[d] {
			seen[d] = true
			uniqueDirs = append(uniqueDirs, d)
		}
	}
	if len(uniqueDirs) == 0 {
		uniqueDirs = []string{""}
	}
	gtkutil.Async(w.ctx, func() func() {
		var (
			mu       sync.Mutex
			allForms []opencode.FormInfo
			wg       sync.WaitGroup
		)
		for _, dir := range uniqueDirs {
			d := dir
			wg.Add(1)
			go func() {
				defer wg.Done()
				forms, err := w.client.ListForms(w.ctx, d)
				if err == nil && len(forms) > 0 {
					mu.Lock()
					allForms = append(allForms, forms...)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		return func() {
			for _, f := range allForms {
				w.upsertPendingForm(f)
			}
		}
	})
}

// TakeChargeDirective is the autonomous decision prompt sent as the answer to
// every pending question when "Take charge" mode is active.
const TakeChargeDirective = "The user is currently away (Take Charge mode is active). Proceed autonomously by choosing the most recommended or sensible option for this question. Record this question, the option you selected, and your reasoning, and report all autonomous choices made at the end of your work."

func buildTakeChargeAnswer(form opencode.FormInfo) map[string]any {
	if len(form.Fields) == 0 {
		return map[string]any{"q0": TakeChargeDirective}
	}
	ans := make(map[string]any, len(form.Fields))
	for _, f := range form.Fields {
		if f.Key == "" {
			continue
		}
		switch f.Type {
		case "multiselect":
			ans[f.Key] = []string{TakeChargeDirective}
		case "boolean":
			ans[f.Key] = true
		default:
			ans[f.Key] = TakeChargeDirective
		}
	}
	return ans
}

func isFormSettledOrMissingError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "FormAlreadySettledError") ||
		strings.Contains(msg, "FormNotFoundError") ||
		strings.Contains(msg, "http 404") ||
		strings.Contains(msg, "http 409")
}

func (w *Window) setTakeCharge(active bool) {
	if w.takeCharge == active {
		return
	}
	w.takeCharge = active
	if w.takeChargeBtn != nil {
		if w.takeChargeBtn.Active() != active {
			w.takeChargeBtn.SetActive(active)
		}
		if active {
			w.takeChargeBtn.SetTooltipText("Take Charge: Active — questions are auto-answered while away")
		} else {
			w.takeChargeBtn.SetTooltipText("Take Charge: Off — click to auto-answer questions while away")
		}
	}
	if !active {
		return
	}
	var pending []opencode.FormInfo
	for _, tab := range w.tabs {
		if tab.main != nil {
			pending = append(pending, tab.main.pendingForms...)
		}
		for _, sub := range tab.subTimelines {
			pending = append(pending, sub.pendingForms...)
		}
	}
	for _, form := range pending {
		w.replyFormTakeCharge(form)
	}
	if w.client != nil {
		w.loadPendingFormsForAllDirs(w.projectOrder)
	}
}

func (w *Window) replyFormTakeCharge(form opencode.FormInfo) {
	if form.ID == "" || form.SessionID == "" || w.client == nil {
		return
	}
	if w.takeChargeInFlight == nil {
		w.takeChargeInFlight = make(map[string]bool)
	}
	if w.takeChargeInFlight[form.ID] {
		return
	}
	w.takeChargeInFlight[form.ID] = true

	sessID := form.SessionID
	formID := form.ID
	ans := buildTakeChargeAnswer(form)

	var qw *QuestionFormWidget
	if _, conv := w.resolveConv(sessID); conv != nil {
		if existing, ok := conv.formWidgets[formID]; ok {
			qw = existing
			qw.SetBusy(true)
		}
	}

	ctx := w.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	gtkutil.Async(ctx, func() func() {
		err := w.client.ReplyForm(ctx, sessID, formID, ans)
		if err != nil && strings.Contains(err.Error(), "FormInvalidAnswerError") {
			err = w.client.CancelForm(ctx, sessID, formID, TakeChargeDirective)
		}
		return func() {
			delete(w.takeChargeInFlight, formID)
			if err != nil && !isFormSettledOrMissingError(err) {
				slog.Error("failed to auto-reply to form in take charge mode", "sessionID", sessID, "formID", formID, "err", err)
				if qw != nil {
					qw.SetBusy(false)
				}
				return
			}
			w.removePendingForm(sessID, formID)
		}
	})
}

func (w *Window) upsertPendingForm(form opencode.FormInfo) {
	if form.ID == "" || form.SessionID == "" {
		return
	}
	if w.takeCharge {
		w.replyFormTakeCharge(form)
		if w.client != nil {
			return
		}
	}
	tab, conv := w.resolveConv(form.SessionID)
	if tab == nil || conv == nil {
		return
	}
	found := false
	for i, existing := range conv.pendingForms {
		if existing.ID == form.ID {
			conv.pendingForms[i] = form
			found = true
			break
		}
	}
	if !found {
		conv.pendingForms = append(conv.pendingForms, form)
	}
	if tab.main.session.ID != w.currentRootSessionID() {
		tab.page.SetNeedsAttention(true)
	}
	if conv.loaded {
		w.ensureFormWidget(tab, conv, form)
		conv.keepPendingAtBottom()
		if conv.scroll != nil {
			conv.scroll.ScrollToBottomIfBottomed()
		}
	}
}

func (w *Window) removePendingForm(sessionID, formID string) {
	if formID == "" {
		return
	}
	_, conv := w.resolveConv(sessionID)
	if conv == nil {
		// Search across all tabs if sessionID wasn't resolved directly.
		for _, t := range w.tabs {
			if _, ok := t.main.formWidgets[formID]; ok {
				conv = t.main
				break
			}
			for _, sub := range t.subTimelines {
				if _, ok := sub.formWidgets[formID]; ok {
					conv = sub
					break
				}
			}
		}
	}
	if conv == nil {
		return
	}
	next := conv.pendingForms[:0]
	for _, f := range conv.pendingForms {
		if f.ID != formID {
			next = append(next, f)
		}
	}
	conv.pendingForms = next
	if qw, ok := conv.formWidgets[formID]; ok {
		delete(conv.formWidgets, formID)
		if conv.timelineBox != nil {
			conv.timelineBox.Remove(qw)
		}
	}
	conv.keepPendingAtBottom()
}

func (w *Window) ensureFormWidget(tab *sessionTab, conv *convTimeline, form opencode.FormInfo) *QuestionFormWidget {
	if qw, ok := conv.formWidgets[form.ID]; ok {
		return qw
	}
	sessID := conv.session.ID
	formID := form.ID
	var qw *QuestionFormWidget
	qw = NewQuestionFormWidget(
		form,
		func(ans map[string]any) {
			gtkutil.Async(w.ctx, func() func() {
				err := w.client.ReplyForm(w.ctx, sessID, formID, ans)
				return func() {
					if err != nil {
						slog.Error("failed to reply to form", "sessionID", sessID, "formID", formID, "err", err)
						qw.SetBusy(false)
						app.Error(w.ctx, err)
						return
					}
					w.removePendingForm(sessID, formID)
				}
			})
		},
		func() {
			gtkutil.Async(w.ctx, func() func() {
				err := w.client.CancelForm(w.ctx, sessID, formID, "")
				return func() {
					if err != nil {
						slog.Error("failed to cancel form", "sessionID", sessID, "formID", formID, "err", err)
						qw.SetBusy(false)
						app.Error(w.ctx, err)
						return
					}
					w.removePendingForm(sessID, formID)
				}
			})
		},
	)
	conv.formWidgets[form.ID] = qw
	if conv.timelineBox != nil {
		conv.timelineBox.Append(qw)
	}
	return qw
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
	w.rebuildShellList()
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
		if w.breadcrumbParentLabel != nil {
			w.breadcrumbParentLabel.SetLabel(mainTitle)
		}
		if w.breadcrumbChildLabel != nil {
			w.breadcrumbChildLabel.SetLabel(subTitle)
		}
		if w.sheetBackToParentBtn != nil {
			w.sheetBackToParentBtn.SetVisible(true)
		}
	} else {
		if w.windowTitle != nil {
			w.windowTitle.SetTitle(mainTitle)
			w.windowTitle.SetSubtitle(shortDir(tab.main.session.Location.Directory))
		}
		if w.sheetBackToParentBtn != nil {
			w.sheetBackToParentBtn.SetVisible(false)
		}
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

func (w *Window) upsertSubagent(tab *sessionTab, sess opencode.Session) {
	if tab == nil || sess.ID == "" {
		return
	}
	if sess.ParentID == "" {
		sess.ParentID = tab.main.session.ID
	}
	w.childToRoot[sess.ID] = tab.main.session.ID
	for i, s := range tab.subagents {
		if s.ID == sess.ID {
			if sess.Title != "" {
				s.Title = sess.Title
			}
			if sess.Agent != "" {
				s.Agent = sess.Agent
			}
			if sess.Model.ID != "" {
				s.Model = sess.Model
			}
			if sess.Location.Directory != "" {
				s.Location = sess.Location
			}
			if sess.Outcome != "" {
				s.Outcome = sess.Outcome
			}
			if sess.Time.Created != 0 {
				s.Time.Created = sess.Time.Created
			}
			if sess.Time.Updated != 0 {
				s.Time.Updated = sess.Time.Updated
			}
			if sess.Time.Busy != 0 {
				s.Time.Busy = sess.Time.Busy
			}
			if sess.Time.Idle != 0 {
				s.Time.Idle = sess.Time.Idle
			}
			tab.subagents[i] = s
			return
		}
	}
	tab.subagents = append([]opencode.Session{sess}, tab.subagents...)
}

func (w *Window) setSubagentActive(childID string, active bool, outcome string) {
	if childID == "" {
		return
	}
	rootID, ok := w.childToRoot[childID]
	if !ok || rootID == "" {
		return
	}
	tab, ok := w.tabs[rootID]
	if !ok || tab == nil {
		return
	}
	now := time.Now().UnixMilli()
	for i, s := range tab.subagents {
		if s.ID == childID {
			if active {
				s.Outcome = ""
				s.Time.Busy = now
				s.Time.Idle = 0
			} else {
				if outcome == "" {
					outcome = "succeeded"
				}
				s.Outcome = outcome
				if s.Time.Busy > now {
					s.Time.Idle = s.Time.Busy
				} else {
					s.Time.Idle = now
				}
			}
			tab.subagents[i] = s
			break
		}
	}
	if !active && tab.main != nil {
		for _, aw := range tab.main.assistantMap {
			for _, tw := range aw.tools {
				if tw.ChildSessionID() == childID {
					tw.SetBackgroundStatus("completed")
				}
			}
		}
	}
	if w.currentTab() == tab {
		w.updateBottomBar(tab)
		w.rebuildSubagentList(tab)
	}
}

func (w *Window) loadChildSubagents(rootSessionID string) {
	if w.client == nil || rootSessionID == "" {
		return
	}
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
			seen := make(map[string]bool, len(children))
			for _, child := range children {
				seen[child.ID] = true
				w.childToRoot[child.ID] = rootSessionID
				if !child.IsRunning() && tab.main != nil {
					for _, aw := range tab.main.assistantMap {
						for _, tw := range aw.tools {
							if tw.ChildSessionID() == child.ID {
								tw.SetBackgroundStatus("completed")
							}
						}
					}
				}
			}
			for _, existing := range tab.subagents {
				if !seen[existing.ID] && existing.IsRunning() {
					children = append([]opencode.Session{existing}, children...)
				}
			}
			tab.subagents = children
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
			forms      []opencode.FormInfo
			wg         sync.WaitGroup
		)
		wg.Add(3)
		go func() {
			defer wg.Done()
			page, err = w.client.ListMessagesPage(w.ctx, sessionID, 50, "")
		}()
		go func() {
			defer wg.Done()
			inboxItems, _ = w.client.ListInbox(w.ctx, sessionID)
		}()
		go func() {
			defer wg.Done()
			forms, _ = w.client.ListSessionForms(w.ctx, sessionID)
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
			conv.formWidgets = make(map[string]*QuestionFormWidget)
			conv.pendingForms = nil
			conv.revertBanner = nil
			prevBlocking := conv.blockingTools
			conv.seenMsgIDs = make(map[string]bool)
			conv.blockingTools = make(map[string]time.Time)
			conv.blockingShellTools = make(map[string]bool)
			conv.blockingShellIDs = make(map[string]string)
			conv.backgroundShellIDs = make(map[string]bool)
			conv.toolNames = make(map[string]string)

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
						if part.Type == "tool" {
							if part.State.Status == "running" || part.State.Status == "streaming" {
								runningTools++
							}
							conv.syncToolPart(part)
						}
					}
				} else if msg.Type == "synthetic" && isSubagentOrShellSynthetic(msg) {
					conv.markSyntheticTargetCompleted(msg)
				}
			}
			for id := range conv.blockingTools {
				if prevStart, ok := prevBlocking[id]; ok && !prevStart.IsZero() {
					conv.blockingTools[id] = prevStart
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

			for _, form := range forms {
				w.upsertPendingForm(form)
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
		for _, tw := range aw.tools {
			if shID := tw.ShellID(); shID != "" {
				if out, ok := w.shellOutputs[shID]; ok && out != "" {
					tw.SetShellLiveOutput(out)
				}
				if _, active := w.activeShells[shID]; !active && tw.IsBackgroundRunning() {
					tw.SetBackgroundStatus("completed")
				}
			}
		}
		conv.assistantMap[msg.ID] = aw
		return aw

	case "compaction":
		cw := NewCompactionWidget(w.ctx, msg)
		conv.compactionMap[msg.ID] = cw
		return cw

	case "synthetic":
		if isSubagentOrShellSynthetic(msg) {
			conv.markSyntheticTargetCompleted(msg)
			return NewSyntheticSubagentWidget(w.ctx, msg, func(subID string) {
				w.openSubagentByID(tab, subID)
			})
		}
		return NewSystemNoticeWidget(w.ctx, msg)

	case "system", "skill":
		return NewSystemNoticeWidget(w.ctx, msg)

	case "agent-switched":
		return NewAgentSwitchedWidget(msg)

	case "model-switched":
		return NewModelSwitchedWidget(msg)

	case "location-switched":
		return NewLocationSwitchedWidget(msg)

	default:
		return nil
	}
}

func (w *Window) reconcileTail(sessionID string) {
	tab, conv := w.resolveConv(sessionID)
	if w.client == nil || conv == nil || !conv.loaded {
		return
	}
	gtkutil.Async(w.ctx, func() func() {
		var (
			page  *opencode.MessagePage
			err   error
			forms []opencode.FormInfo
			wg    sync.WaitGroup
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			page, err = w.client.ListMessagesPage(w.ctx, sessionID, 10, "")
		}()
		go func() {
			defer wg.Done()
			forms, _ = w.client.ListSessionForms(w.ctx, sessionID)
		}()
		wg.Wait()
		return func() {
			if err != nil || page == nil {
				return
			}
			activeFormIDs := make(map[string]bool, len(forms))
			for _, f := range forms {
				activeFormIDs[f.ID] = true
				w.upsertPendingForm(f)
			}
			for i := len(conv.pendingForms) - 1; i >= 0; i-- {
				if id := conv.pendingForms[i].ID; !activeFormIDs[id] {
					w.removePendingForm(sessionID, id)
				}
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
				if msg.Type == "assistant" {
					for _, part := range msg.Content {
						if part.Type == "tool" && part.ID != "" {
							conv.syncToolPart(part)
						}
					}
				} else if msg.Type == "synthetic" && isSubagentOrShellSynthetic(msg) {
					conv.markSyntheticTargetCompleted(msg)
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
				conv.scroll.ScrollToBottomIfBottomed()
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
		if w.backgroundStatusBtn != nil {
			w.backgroundStatusBtn.SetVisible(false)
		}
		if w.tokRateStatusLabel != nil {
			w.tokRateStatusLabel.SetVisible(false)
		}
		w.rebuildContextInfo(nil)
		return
	}
	now := time.Now()
	showBg := tab.shouldShowBackgroundHint(now)
	if w.backgroundStatusBtn != nil {
		w.backgroundStatusBtn.SetVisible(showBg)
	}
	if !showBg {
		if delay, ok := tab.nextBackgroundHintDelay(now); ok && w.ctx != nil {
			ms := uint(delay.Milliseconds()) + 20
			if ms < 50 {
				ms = 50
			}
			tabID := tab.main.session.ID
			glib.TimeoutAdd(ms, func() bool {
				if w.ctx.Err() != nil {
					return false
				}
				if cur := w.currentTab(); cur != nil && cur.main.session.ID == tabID {
					w.updateBottomBar(cur)
				}
				return false
			})
		}
	}
	conv := tab.activeConv()
	agent := conv.session.Agent
	if agent == "" {
		agent = "build"
	}
	agentLower := strings.ToLower(agent)

	// Agent button label in composer row 2
	if w.agentPillLabel != nil {
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
	if w.modelStatusLabel != nil {
		w.modelStatusLabel.SetLabel(modelDisplay)
	}

	// Left: Project badge and separate Git branch badge
	dir := tab.main.session.Location.Directory
	projName := filepath.Base(dir)
	if projName == "" || projName == "." || projName == "/" {
		projName = "workspace"
	}
	if w.projectStatusLabel != nil {
		w.projectStatusLabel.SetLabel(projName)
	}
	if tab.gitBranch == "" {
		tab.gitBranch = w.getGitBranch(dir)
	}
	if w.branchStatusBox != nil && w.branchStatusLabel != nil {
		if tab.gitBranch != "" {
			w.branchStatusLabel.SetLabel(tab.gitBranch)
			w.branchStatusBox.SetVisible(true)
		} else {
			w.branchStatusBox.SetVisible(false)
		}
	}

	// Right: Active shells
	w.updateShellStatus()

	// Right: Active subagents count (only active > 0)
	var activeCount int
	for _, sub := range tab.subagents {
		if sub.IsRunning() {
			activeCount++
		}
	}
	if w.subagentStatusBox != nil && w.subagentStatusLabel != nil {
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
	if w.tokRateStatusLabel != nil {
		if tokRate != "" {
			w.tokRateStatusLabel.SetLabel(tokRate)
			w.tokRateStatusLabel.SetVisible(true)
		} else {
			w.tokRateStatusLabel.SetVisible(false)
		}
	}

	// Right:Context usage
	limit := w.modelLimits[modelKey]
	if limit == 0 {
		limit = w.modelLimits[conv.session.Model.ID]
	}
	ctxText := opencode.FormatContextUsage(conv.messages, conv.session.Tokens, limit)
	if w.contextStatusLabel != nil {
		w.contextStatusLabel.SetLabel(ctxText)
	}

	w.rebuildAgentPills()
	w.rebuildContextInfo(tab)
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
	if w.branchCache == nil {
		w.branchCache = make(map[string]string)
	}
	w.branchCache[dir] = branch
	return branch
}

func (w *Window) isShellInTab(tab *sessionTab, sh opencode.ShellItem) bool {
	if tab == nil {
		return false
	}
	rootID := tab.main.session.ID
	sessID := sh.EffectiveSessionID()
	if sessID != "" {
		if sessID == rootID || w.childToRoot[sessID] == rootID {
			return true
		}
		if tab.subTimelines != nil {
			if _, ok := tab.subTimelines[sessID]; ok {
				return true
			}
		}
		return false
	}
	shDir := normalizeProjectDir(sh.EffectiveLocationDir())
	tabDir := normalizeProjectDir(tab.main.session.Location.Directory)
	return shDir != "" && shDir == tabDir
}

func (w *Window) isForegroundShell(tab *sessionTab, sh opencode.ShellItem) bool {
	if tab == nil {
		return false
	}
	checkConv := func(conv *convTimeline) bool {
		if conv == nil {
			return false
		}
		if conv.backgroundShellIDs[sh.ID] {
			return false
		}
		if _, ok := conv.blockingShellIDs[sh.ID]; ok {
			return true
		}
		if len(conv.blockingShellTools) > 0 {
			sessID := sh.EffectiveSessionID()
			if sessID == "" || sessID == conv.session.ID {
				return true
			}
		}
		return false
	}
	if checkConv(tab.main) {
		return true
	}
	for _, sub := range tab.subTimelines {
		if checkConv(sub) {
			return true
		}
	}
	return false
}

func (w *Window) visibleBackgroundShellsForTab(tab *sessionTab) []opencode.ShellItem {
	if tab == nil || len(w.activeShells) == 0 {
		return nil
	}
	var out []opencode.ShellItem
	for _, sh := range w.activeShells {
		if sh.Status != "" && sh.Status != "running" {
			continue
		}
		if !w.isShellInTab(tab, sh) {
			continue
		}
		if w.isForegroundShell(tab, sh) {
			continue
		}
		out = append(out, sh)
	}
	slices.SortFunc(out, func(a, b opencode.ShellItem) int {
		if a.Time.Created != b.Time.Created {
			return cmp.Compare(a.Time.Created, b.Time.Created)
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}

func readTailBytes(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if size <= 0 {
		return nil, nil
	}
	offset := int64(0)
	if size > maxBytes {
		offset = size - maxBytes
	}
	buf := make([]byte, size-offset)
	n, _ := f.ReadAt(buf, offset)
	return buf[:n], nil
}

func (w *Window) fetchSingleShellOutput(sh opencode.ShellItem) (string, bool) {
	if w.client == nil {
		if f := sh.EffectiveFile(); f != "" {
			if data, readErr := readTailBytes(f, 16384); readErr == nil {
				return FormatShellTailLines(string(data), 7), true
			}
		}
		return "", false
	}
	out, err := w.client.GetShellOutput(w.ctx, sh.ID, sh.EffectiveLocationDir(), 0, 16384)
	if err == nil && out != nil && out.Output != "" {
		return FormatShellTailLines(out.Output, 7), true
	}
	if f := sh.EffectiveFile(); f != "" {
		if data, readErr := readTailBytes(f, 16384); readErr == nil {
			return FormatShellTailLines(string(data), 7), true
		}
	}
	if err == nil && out != nil {
		return FormatShellTailLines(out.Output, 7), true
	}
	return "", false
}

func (w *Window) updateToolWidgetsForShell(shellID, formattedOutput, bgStatus string) {
	if shellID == "" {
		return
	}
	applyConv := func(conv *convTimeline) {
		if conv == nil {
			return
		}
		for _, aw := range conv.assistantMap {
			for _, tw := range aw.tools {
				if tw.ShellID() == shellID {
					if formattedOutput != "" {
						tw.SetShellLiveOutput(formattedOutput)
					}
					if bgStatus != "" {
						tw.SetBackgroundStatus(bgStatus)
					}
				}
			}
		}
	}
	for _, tab := range w.tabs {
		applyConv(tab.main)
		for _, sub := range tab.subTimelines {
			applyConv(sub)
		}
	}
}

func (w *Window) pollActiveShellOutputs() {
	shells := w.visibleBackgroundShellsForTab(w.currentTab())
	if len(shells) == 0 {
		return
	}
	gtkutil.Async(w.ctx, func() func() {
		type res struct {
			id   string
			text string
		}
		var (
			mu      sync.Mutex
			results []res
			wg      sync.WaitGroup
		)
		for _, sh := range shells {
			shCopy := sh
			wg.Add(1)
			go func() {
				defer wg.Done()
				if text, ok := w.fetchSingleShellOutput(shCopy); ok {
					mu.Lock()
					results = append(results, res{id: shCopy.ID, text: text})
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		return func() {
			for _, r := range results {
				if w.shellOutputs[r.id] != r.text {
					w.shellOutputs[r.id] = r.text
					if lbl, ok := w.shellOutputLabels[r.id]; ok && lbl != nil {
						lbl.SetText(r.text)
					}
					w.updateToolWidgetsForShell(r.id, r.text, "")
				}
			}
		}
	})
}

func (w *Window) ensureShellOutputPoller() {
	if w.client == nil || w.shellPollRunning {
		return
	}
	if len(w.visibleBackgroundShellsForTab(w.currentTab())) == 0 {
		return
	}
	w.shellPollRunning = true
	w.pollActiveShellOutputs()
	glib.TimeoutAdd(1000, func() bool {
		if w.ctx.Err() != nil || len(w.visibleBackgroundShellsForTab(w.currentTab())) == 0 {
			w.shellPollRunning = false
			return false
		}
		w.pollActiveShellOutputs()
		return true
	})
}

func (w *Window) refreshShells() {
	if w.client == nil {
		return
	}
	seenDirs := map[string]bool{"": true}
	dirs := []string{""}
	if tab := w.currentTab(); tab != nil && tab.main.session.Location.Directory != "" {
		d := tab.main.session.Location.Directory
		seenDirs[d] = true
		dirs = append(dirs, d)
	}
	for _, tab := range w.tabs {
		if d := tab.main.session.Location.Directory; d != "" && !seenDirs[d] {
			seenDirs[d] = true
			dirs = append(dirs, d)
		}
	}

	gtkutil.Async(w.ctx, func() func() {
		var (
			mu     sync.Mutex
			merged = make(map[string]opencode.ShellItem)
			anyOK  bool
			wg     sync.WaitGroup
		)
		for _, dir := range dirs {
			d := dir
			wg.Add(1)
			go func() {
				defer wg.Done()
				shells, err := w.client.ListShells(w.ctx, d)
				if err != nil {
					return
				}
				mu.Lock()
				anyOK = true
				for _, sh := range shells {
					if sh.Status == "" || sh.Status == "running" {
						if sh.LocationDir == "" && d != "" {
							sh.LocationDir = d
						}
						merged[sh.ID] = sh
					}
				}
				mu.Unlock()
			}()
		}
		wg.Wait()
		return func() {
			if anyOK {
				w.activeShells = merged
				w.updateShellStatus()
				w.rebuildShellList()
				w.ensureShellOutputPoller()
			}
		}
	})
}

func (w *Window) updateShellStatus() {
	if w.shellStatusBox == nil || w.shellStatusLabel == nil || w.shellsSectionBox == nil {
		return
	}
	shells := w.visibleBackgroundShellsForTab(w.currentTab())
	count := len(shells)
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
	hasContent := (w.shellsSectionBox != nil && w.shellsSectionBox.Visible()) ||
		(w.subagentsSectionBox != nil && w.subagentsSectionBox.Visible()) ||
		(w.contextSectionBox != nil && w.contextSectionBox.Visible())
	w.sheetEmptyLabel.SetVisible(!hasContent)
}

func formatIntWithCommas(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func formatSheetTimestamp(ms int64) string {
	if ms <= 0 {
		return "—"
	}
	return time.UnixMilli(ms).Local().Format("Jan 2, 2006, 3:04 PM")
}

func (w *Window) rebuildContextInfo(tab *sessionTab) {
	if w.contextSectionBox == nil || w.contextInfoGrid == nil {
		return
	}
	for c := w.contextInfoGrid.FirstChild(); c != nil; c = w.contextInfoGrid.FirstChild() {
		w.contextInfoGrid.Remove(c)
	}
	if tab == nil {
		w.contextSectionBox.SetVisible(false)
		w.updateSheetEmptyState()
		return
	}
	conv := tab.activeConv()
	if conv == nil {
		w.contextSectionBox.SetVisible(false)
		w.updateSheetEmptyState()
		return
	}

	sess := conv.session
	rawDir := tab.main.session.Location.Directory
	projDisplay := projectName(rawDir)
	if sd := shortDir(rawDir); sd != "" && sd != projDisplay {
		projDisplay = fmt.Sprintf("%s (%s)", projDisplay, sd)
	}
	if tab.gitBranch != "" {
		projDisplay = fmt.Sprintf("%s · %s", projDisplay, tab.gitBranch)
	}

	sessTitle := strings.TrimSpace(sess.Title)
	if sessTitle == "" {
		sessTitle = "Untitled Session"
	}

	providerDisplay := sess.Model.ProviderID
	if providerDisplay == "" {
		providerDisplay = "—"
	}
	modelKey := sess.Model.String()
	modelDisplay := w.modelNames[modelKey]
	if modelDisplay == "" {
		modelDisplay = w.modelNames[sess.Model.ID]
	}
	if modelDisplay == "" {
		modelDisplay = sess.Model.ID
	}
	if modelDisplay == "" {
		modelDisplay = "Default Model"
	}

	limit := w.modelLimits[modelKey]
	if limit == 0 {
		limit = w.modelLimits[sess.Model.ID]
	}
	limitStr := "—"
	if limit > 0 {
		limitStr = formatIntWithCommas(limit)
	}

	var userCount, assistantCount int
	var totalCost float64
	var latestTokens opencode.TokenUsage
	for _, m := range conv.messages {
		switch m.Type {
		case "user":
			userCount++
		case "assistant":
			assistantCount++
		}
		totalCost += m.Cost
		if (m.Type == "" || m.Type == "assistant") && (m.Tokens.Input+m.Tokens.Cache.Read+m.Tokens.Output > 0) {
			latestTokens = m.Tokens
		}
	}
	if sess.Cost > totalCost {
		totalCost = sess.Cost
	}
	tok := latestTokens
	if tok.Input+tok.Cache.Read+tok.Output+tok.Reasoning == 0 {
		tok = sess.Tokens
	}
	contextTokens := tok.Input + tok.Cache.Read
	totalTokens := tok.Input + tok.Output + tok.Reasoning + tok.Cache.Read + tok.Cache.Write

	usageStr := "0%"
	if limit > 0 && contextTokens > 0 {
		pct := int((float64(contextTokens)/float64(limit))*100 + 0.5)
		usageStr = fmt.Sprintf("%d%%", pct)
	} else if contextTokens > 0 {
		usageStr = formatIntWithCommas(contextTokens)
	}

	type kvItem struct {
		label string
		value string
	}
	items := []kvItem{
		{"Project", projDisplay},
		{"Agent", formatAgentDisplayName(sess.Agent)},
		{"Session", sessTitle},
		{"Messages", formatIntWithCommas(len(conv.messages))},
		{"Provider", providerDisplay},
		{"Model", modelDisplay},
		{"Context Limit", limitStr},
		{"Total Tokens", formatIntWithCommas(totalTokens)},
		{"Usage", usageStr},
		{"Input Tokens", formatIntWithCommas(tok.Input)},
		{"Output Tokens", formatIntWithCommas(tok.Output)},
		{"Reasoning Tokens", formatIntWithCommas(tok.Reasoning)},
		{"Cache Tokens (read/write)", fmt.Sprintf("%s / %s", formatIntWithCommas(tok.Cache.Read), formatIntWithCommas(tok.Cache.Write))},
		{"User Messages", formatIntWithCommas(userCount)},
		{"Assistant Messages", formatIntWithCommas(assistantCount)},
		{"Total Cost", fmt.Sprintf("$%.2f", totalCost)},
		{"Session Created", formatSheetTimestamp(sess.Time.Created)},
		{"Last Activity", formatSheetTimestamp(sess.Time.Updated)},
	}

	for i, item := range items {
		col := i % 2
		row := i / 2

		cell := gtk.NewBox(gtk.OrientationVertical, 2)
		cell.AddCSSClass("glup-context-cell")
		cell.SetHExpand(true)

		kLbl := gtk.NewLabel(item.label)
		kLbl.AddCSSClass("glup-context-cell-label")
		kLbl.SetXAlign(0)
		kLbl.SetEllipsize(pango.EllipsizeEnd)
		cell.Append(kLbl)

		vLbl := gtk.NewLabel(item.value)
		vLbl.AddCSSClass("glup-context-cell-value")
		vLbl.SetXAlign(0)
		vLbl.SetEllipsize(pango.EllipsizeMiddle)
		vLbl.SetSelectable(true)
		vLbl.SetTooltipText(item.value)
		cell.Append(vLbl)

		w.contextInfoGrid.Attach(cell, col, row, 1, 1)
	}

	w.contextSectionBox.SetVisible(true)
	w.updateSheetEmptyState()
}

func (w *Window) rebuildShellList() {
	if w.shellListBox == nil {
		return
	}
	w.shellListBox.RemoveAll()
	w.shellOutputLabels = make(map[string]*gtk.Label)

	shells := w.visibleBackgroundShellsForTab(w.currentTab())
	if len(shells) == 0 {
		return
	}

	for _, sh := range shells {
		shellItem := sh
		row := gtk.NewListBoxRow()
		row.SetSelectable(false)
		row.SetActivatable(false)

		outerVBox := gtk.NewBox(gtk.OrientationVertical, 6)
		outerVBox.SetMarginTop(8)
		outerVBox.SetMarginBottom(8)
		outerVBox.SetMarginStart(12)
		outerVBox.SetMarginEnd(12)
		outerVBox.SetHExpand(true)

		hBox := gtk.NewBox(gtk.OrientationHorizontal, 10)
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
		titleLbl.AddCSSClass("glup-shell-command")
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
		subLbl.AddCSSClass("glup-shell-command")
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
			locDir := shellItem.EffectiveLocationDir()
			if locDir == "" {
				if tab := w.currentTab(); tab != nil {
					locDir = tab.main.session.Location.Directory
				}
			}
			gtkutil.Async(w.ctx, func() func() {
				err := w.client.KillShell(w.ctx, shellItem.ID, locDir)
				return func() {
					if err != nil {
						slog.Error("failed to kill shell", "shellID", shellItem.ID, "err", err)
						app.Error(w.ctx, err)
						return
					}
					delete(w.activeShells, shellItem.ID)
					delete(w.shellOutputs, shellItem.ID)
					delete(w.shellOutputLabels, shellItem.ID)
					w.updateToolWidgetsForShell(shellItem.ID, "", "completed")
					w.updateShellStatus()
					w.rebuildShellList()
				}
			})
		})
		hBox.Append(stopBtn)
		outerVBox.Append(hBox)

		outText, ok := w.shellOutputs[shellItem.ID]
		if !ok || outText == "" {
			outText = FormatShellTailLines("", 7)
		}
		outLbl := gtk.NewLabel(outText)
		outLbl.AddCSSClass("glup-shell-output")
		outLbl.SetXAlign(0)
		outLbl.SetYAlign(0)
		outLbl.SetHExpand(true)
		outLbl.SetSelectable(true)
		outLbl.SetOverflow(gtk.OverflowHidden)
		outerVBox.Append(outLbl)
		w.shellOutputLabels[shellItem.ID] = outLbl

		row.SetChild(outerVBox)
		w.shellListBox.Append(row)
	}
}

func (w *Window) rebuildSubagentList(tab *sessionTab) {
	if w.subagentListBox == nil || w.subagentsSectionBox == nil {
		return
	}
	w.subagentListBox.RemoveAll()
	w.subagentRows = nil

	viewingSubagent := tab != nil && tab.activeSubID != ""
	var activeSubs []opencode.Session
	if tab != nil {
		for _, sub := range tab.subagents {
			if sub.IsRunning() {
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
	if w.agentListBox == nil {
		return
	}
	w.agentListBox.RemoveAll()
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
	w.modelListBox.RemoveAll()
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

func (w *Window) setupNewSessionPopover(popover *gtk.Popover, searchEntry *gtk.SearchEntry, listBox *gtk.ListBox, folderBtn *gtk.Button) {
	emptyLabel := gtk.NewLabel("No matching projects")
	emptyLabel.AddCSSClass("dim-label")
	emptyLabel.SetMarginTop(12)
	emptyLabel.SetMarginBottom(12)
	listBox.SetPlaceholder(emptyLabel)

	type projectMatch struct {
		dir   string
		row   *gtk.ListBoxRow
		score int
	}

	var allDirs []string
	var currentMatches []projectMatch

	activateSelected := func() {
		sel := listBox.SelectedRow()
		if sel == nil && len(currentMatches) > 0 {
			sel = currentMatches[0].row
		}
		if sel != nil {
			for _, m := range currentMatches {
				if m.row != nil && m.row.Object.Native() == sel.Object.Native() {
					popover.Popdown()
					w.createNewSessionInDir(m.dir)
					return
				}
			}
		}
	}

	rebuildRows := func(query string) {
		for c := listBox.FirstChild(); c != nil; {
			next := gtk.BaseWidget(c).NextSibling()
			listBox.Remove(c)
			c = next
		}
		currentMatches = nil

		currentDir := ""
		if tab := w.currentTab(); tab != nil {
			currentDir = normalizeProjectDir(tab.main.session.Location.Directory)
		}

		q := strings.TrimSpace(query)
		for _, dir := range allDirs {
			candidate := projectName(dir) + " " + shortDir(dir) + " " + dir
			score, ok := fuzzyMatchScore(q, candidate)
			if !ok {
				continue
			}
			currentMatches = append(currentMatches, projectMatch{
				dir:   dir,
				score: score,
			})
		}
		if q != "" {
			slices.SortStableFunc(currentMatches, func(a, b projectMatch) int {
				if a.score != b.score {
					if a.score > b.score {
						return -1
					}
					return 1
				}
				return 0
			})
		}

		for i := range currentMatches {
			dir := currentMatches[i].dir
			row := gtk.NewListBoxRow()
			hBox := gtk.NewBox(gtk.OrientationHorizontal, 8)
			hBox.SetMarginTop(6)
			hBox.SetMarginBottom(6)
			hBox.SetMarginStart(10)
			hBox.SetMarginEnd(10)

			icon := gtk.NewImageFromIconName("folder-symbolic")
			icon.AddCSSClass("glup-sidebar-folder-icon")
			icon.SetVAlign(gtk.AlignCenter)
			hBox.Append(icon)

			vBox := gtk.NewBox(gtk.OrientationVertical, 1)
			vBox.SetHExpand(true)

			titleLbl := gtk.NewLabel(projectName(dir))
			titleLbl.SetXAlign(0)
			titleLbl.SetEllipsize(pango.EllipsizeMiddle)
			titleLbl.AddCSSClass("glup-sidebar-project-title")
			vBox.Append(titleLbl)

			subLbl := gtk.NewLabel(shortDir(dir))
			subLbl.SetXAlign(0)
			subLbl.SetEllipsize(pango.EllipsizeMiddle)
			subLbl.AddCSSClass("caption")
			subLbl.AddCSSClass("dim-label")
			vBox.Append(subLbl)

			hBox.Append(vBox)

			if dir == currentDir {
				check := gtk.NewImageFromIconName("object-select-symbolic")
				check.SetVAlign(gtk.AlignCenter)
				hBox.Append(check)
			}

			row.SetChild(hBox)
			listBox.Append(row)
			currentMatches[i].row = row
		}
		if len(currentMatches) > 0 && currentMatches[0].row != nil {
			listBox.SelectRow(currentMatches[0].row)
		}
	}

	popover.ConnectShow(func() {
		allDirs = nil
		seen := make(map[string]bool)
		addDir := func(raw string) {
			d := normalizeProjectDir(raw)
			if d != "" && !seen[d] {
				seen[d] = true
				allDirs = append(allDirs, d)
			}
		}
		for _, d := range w.projectOrder {
			addDir(d)
		}
		for _, id := range w.tabOrder {
			if tab := w.tabs[id]; tab != nil {
				addDir(tab.main.session.Location.Directory)
			}
		}
		for _, s := range w.allSessions {
			addDir(s.Location.Directory)
		}
		if cwd, err := os.Getwd(); err == nil && cwd != "" {
			addDir(cwd)
		}

		if searchEntry != nil {
			searchEntry.SetText("")
			rebuildRows("")
			searchEntry.GrabFocus()
		} else {
			rebuildRows("")
		}
	})

	if searchEntry != nil {
		searchEntry.ConnectSearchChanged(func() {
			rebuildRows(searchEntry.Text())
		})
		searchEntry.ConnectActivate(activateSelected)
		searchEntry.ConnectStopSearch(func() {
			popover.Popdown()
		})

		keyCtrl := gtk.NewEventControllerKey()
		keyCtrl.ConnectKeyPressed(func(keyval, _ uint, _ gdk.ModifierType) bool {
			if keyval == gdk.KEY_Down || keyval == gdk.KEY_Up {
				if len(currentMatches) == 0 {
					return false
				}
				selIdx := 0
				if sel := listBox.SelectedRow(); sel != nil {
					for i, m := range currentMatches {
						if m.row != nil && m.row.Object.Native() == sel.Object.Native() {
							selIdx = i
							break
						}
					}
				}
				if keyval == gdk.KEY_Down && selIdx+1 < len(currentMatches) {
					listBox.SelectRow(currentMatches[selIdx+1].row)
					return true
				}
				if keyval == gdk.KEY_Up && selIdx > 0 {
					listBox.SelectRow(currentMatches[selIdx-1].row)
					return true
				}
			}
			return false
		})
		searchEntry.AddController(keyCtrl)
	}

	listBox.ConnectRowActivated(func(row *gtk.ListBoxRow) {
		if row == nil {
			return
		}
		for _, m := range currentMatches {
			if m.row != nil && m.row.Object.Native() == row.Object.Native() {
				popover.Popdown()
				w.createNewSessionInDir(m.dir)
				return
			}
		}
	})

	folderBtn.ConnectClicked(func() {
		popover.Popdown()
		w.promptNewProjectFolder()
	})
}

func (w *Window) promptNewProjectFolder() {
	dialog := gtk.NewFileDialog()
	dialog.SetTitle("Open Project Folder")
	dialog.SetModal(true)
	dialog.SelectFolder(w.ctx, &w.ApplicationWindow.Window, func(res gio.AsyncResulter) {
		file, err := dialog.SelectFolderFinish(res)
		if err != nil || file == nil {
			return
		}
		path := file.Path()
		if path == "" {
			return
		}
		w.createNewSessionInDir(path)
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
	w.createNewSessionInDir(cwd)
}

func (w *Window) inheritedAgentAndModel() (string, opencode.ModelRef) {
	var agent string
	var model opencode.ModelRef

	extractFromConv := func(conv *convTimeline) {
		if conv == nil {
			return
		}
		if agent == "" && conv.session.Agent != "" {
			agent = conv.session.Agent
		}
		if model.ID == "" && conv.session.Model.ID != "" {
			model = conv.session.Model
		}
		for i := len(conv.messages) - 1; i >= 0 && (agent == "" || model.ID == ""); i-- {
			msg := conv.messages[i]
			if agent == "" && msg.Agent != "" {
				agent = msg.Agent
			}
			if model.ID == "" && msg.Model.ID != "" {
				model = msg.Model
			}
		}
	}

	if tab := w.currentTab(); tab != nil {
		extractFromConv(tab.activeConv())
		if tab.main != tab.activeConv() {
			extractFromConv(tab.main)
		}
	}

	if model.ID != "" && model.ProviderID == "" {
		for _, m := range w.models {
			if m.ID == model.ID || m.ModelID == model.ID {
				model.ProviderID = m.ProviderID
				break
			}
		}
	}

	return agent, model
}

func (w *Window) createNewSessionInDir(dir string) {
	cwd := strings.TrimSpace(dir)
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}
	cwd = filepath.Clean(cwd)

	inheritedAgent, inheritedModel := w.inheritedAgentAndModel()
	req := opencode.CreateSessionRequest{
		Directory: cwd,
		Agent:     inheritedAgent,
	}
	if inheritedModel.ID != "" && inheritedModel.ProviderID != "" {
		req.Model = &inheritedModel
	}

	gtkutil.Async(w.ctx, func() func() {
		sess, err := w.client.CreateSessionWithOptions(w.ctx, req)
		return func() {
			if err != nil {
				app.Error(w.ctx, err)
				return
			}
			normDir := normalizeProjectDir(sess.Location.Directory)
			stateChanged := false
			if w.archivedSessions[sess.ID] {
				delete(w.archivedSessions, sess.ID)
				stateChanged = true
			}
			if w.collapsedProjects[normDir] {
				delete(w.collapsedProjects, normDir)
				stateChanged = true
			}
			if w.ensureProjectOrder(normDir) {
				stateChanged = true
			}
			if stateChanged {
				w.saveState()
			}
			w.allSessions = append([]opencode.Session{*sess}, w.allSessions...)
			if w.projectSessions != nil {
				w.projectSessions[normDir] = append([]opencode.Session{*sess}, w.projectSessions[normDir]...)
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
			w.touchSessionUpdated(tab, conv, time.Now().UnixMilli())
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
	w.touchSessionUpdated(tab, conv, time.Now().UnixMilli())
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
		conv.scroll.ScrollToBottomIfBottomed()
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

func (w *Window) backgroundCurrentSession() bool {
	tab := w.currentTab()
	if tab == nil {
		return false
	}
	sessionID := tab.blockingSessionID()
	if sessionID == "" {
		return false
	}
	if _, conv := w.resolveConv(sessionID); conv != nil {
		clear(conv.blockingTools)
		clear(conv.blockingShellTools)
		for shID := range conv.blockingShellIDs {
			conv.backgroundShellIDs[shID] = true
		}
		clear(conv.blockingShellIDs)
	}
	w.updateBottomBar(tab)
	w.rebuildShellList()
	w.ensureShellOutputPoller()
	rootID := tab.main.session.ID
	gtkutil.Async(w.ctx, func() func() {
		err := w.client.BackgroundSession(w.ctx, sessionID)
		return func() {
			if err != nil {
				slog.Error("failed to background session", "sessionID", sessionID, "err", err)
				return
			}
			w.refreshShells()
			if rootID != "" {
				w.loadChildSubagents(rootID)
			}
		}
	})
	return true
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
			clear(conv.blockingTools)
			clear(conv.blockingShellTools)
			clear(conv.blockingShellIDs)
		}
		conv.keepPendingAtBottom()
		if active && conv.scroll != nil {
			conv.scroll.ScrollToBottomIfBottomed()
		}
	}
	if conv == tab.main {
		tab.page.SetBadgeNumber(0)
	}
	if w.currentTab() == tab {
		if tab.activeConv() == conv {
			w.updateBusyState(active)
		}
		w.updateBottomBar(tab)
	}
	w.updateSwitcherIndicators()
}

func (w *Window) updateBusyState(busy bool) {
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
						w.upsertSubagent(tab, sess)
						if w.currentTab() == tab {
							w.updateBottomBar(tab)
							w.rebuildSubagentList(tab)
						}
					}
				} else if sess.ID != "" && !w.archivedSessions[sess.ID] {
					w.ensureSessionTab(sess)
					w.syncProjectSessionVisibility()
					w.syncSwitcherIndicators()
				}
			}
			if w.client != nil && d.Session.ID != "" && d.Session.Location.Directory == "" && d.Session.ParentID == "" && d.Session.Title == "" {
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
					if tab.page != nil {
						tab.page.SetTitle(sess.Title)
					}
				}
				if sess.Agent != "" {
					tab.main.session.Agent = sess.Agent
				}
				if sess.Model.ID != "" {
					tab.main.session.Model = sess.Model
				}
				if sess.Outcome != "" {
					tab.main.session.Outcome = sess.Outcome
				}
				if sess.Time.Created != 0 {
					tab.main.session.Time.Created = sess.Time.Created
				}
				if sess.Time.Busy != 0 {
					tab.main.session.Time.Busy = sess.Time.Busy
				}
				if sess.Time.Idle != 0 {
					tab.main.session.Time.Idle = sess.Time.Idle
				}
				if sess.Tokens.Input+sess.Tokens.Output+sess.Tokens.Cache.Read > 0 {
					tab.main.session.Tokens = sess.Tokens
				}
				tab.main.session.Revert = sess.Revert
				if sess.Location.Directory != "" {
					tab.main.session.Location = sess.Location
				}
				if sess.Time.Updated != 0 {
					w.touchSessionUpdated(tab, tab.main, sess.Time.Updated)
				} else {
					w.syncSwitcherIndicators()
				}
				if w.currentTab() == tab {
					w.refreshHeaderAndBottomBar(tab)
				}
			} else if parentID, ok := w.childToRoot[sess.ID]; ok {
				if parentTab, ok := w.tabs[parentID]; ok {
					w.upsertSubagent(parentTab, sess)
					if w.currentTab() == parentTab {
						w.updateBottomBar(parentTab)
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
						w.updateBottomBar(parentTab)
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
			tab, conv := w.resolveConv(d.SessionID)
			w.touchSessionUpdated(tab, conv, time.Now().UnixMilli())
			w.setConvActive(d.SessionID, true)
			w.setSubagentActive(d.SessionID, true, "")
			w.reconcileTail(d.SessionID)
		}

	case "session.execution.succeeded", "session.execution.failed", "session.execution.interrupted", "session.idle":
		var d opencode.SessionIDEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			tab, conv := w.resolveConv(d.SessionID)
			w.touchSessionUpdated(tab, conv, time.Now().UnixMilli())
			w.setConvActive(d.SessionID, false)
			outcome := "succeeded"
			if ev.Type == "session.execution.failed" {
				outcome = "failed"
			} else if ev.Type == "session.execution.interrupted" {
				outcome = "interrupted"
			}
			w.setSubagentActive(d.SessionID, false, outcome)
			w.reconcileTail(d.SessionID)
			if rootID, ok := w.childToRoot[d.SessionID]; ok {
				w.loadChildSubagents(rootID)
			}
		}

	case "session.status":
		var d struct {
			SessionID string `json:"sessionID"`
			Status    struct {
				Type string `json:"type"`
			} `json:"status"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			switch d.Status.Type {
			case "busy", "retry":
				w.setConvActive(d.SessionID, true)
				w.setSubagentActive(d.SessionID, true, "")
			case "idle":
				w.setConvActive(d.SessionID, false)
				w.setSubagentActive(d.SessionID, false, "succeeded")
				if rootID, ok := w.childToRoot[d.SessionID]; ok {
					w.loadChildSubagents(rootID)
				}
			}
		}

	case "session.inbox.enqueued":
		var d opencode.InboxEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			tab, conv := w.resolveConv(d.SessionID)
			w.touchSessionUpdated(tab, conv, time.Now().UnixMilli())
			if conv != nil {
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

	case "session.inbox.delivered", "session.synthetic", "session.instructions.updated", "session.skill.activated",
		"session.compaction.started", "session.compaction.ended", "session.compaction.failed":
		var d struct {
			SessionID string         `json:"sessionID"`
			Metadata  map[string]any `json:"metadata,omitempty"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			tab, conv := w.resolveConv(d.SessionID)
			w.touchSessionUpdated(tab, conv, time.Now().UnixMilli())
			if conv != nil {
				if conv.loaded {
					w.reconcileTail(d.SessionID)
				}
			}
			if ev.Type == "session.synthetic" {
				if d.Metadata != nil {
					if childID, _ := d.Metadata["childID"].(string); childID != "" {
						state, _ := d.Metadata["state"].(string)
						w.setSubagentActive(childID, false, cmp.Or(state, "succeeded"))
					}
					if shID, _ := d.Metadata["shellID"].(string); shID != "" {
						w.updateToolWidgetsForShell(shID, "", "completed")
						w.refreshShells()
					}
				}
				if tab != nil {
					w.loadChildSubagents(tab.main.session.ID)
				} else if rootID, ok := w.childToRoot[d.SessionID]; ok {
					w.loadChildSubagents(rootID)
				}
			}
		}

	case "session.moved":
		var d struct {
			SessionID string            `json:"sessionID"`
			Location  opencode.Location `json:"location"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			tab, conv := w.resolveConv(d.SessionID)
			if conv != nil {
				if d.Location.Directory != "" {
					conv.session.Location = d.Location
				}
				if conv.loaded {
					w.reconcileTail(d.SessionID)
				}
			}
			if tab != nil {
				if conv == tab.main {
					if w.reorderTabsByProject() {
						w.syncSectionStarts()
						w.syncProjectSessionVisibility()
						w.syncSwitcherIndicators()
					} else {
						w.syncSectionStarts()
					}
				}
				if w.currentTab() == tab {
					w.refreshHeaderAndBottomBar(tab)
				}
			}
		}

	case "session.renamed":
		var d struct {
			SessionID string `json:"sessionID"`
			Title     string `json:"title"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.Title != "" {
			w.applySessionRename(d.SessionID, d.Title)
		}

	case "session.agent.selected":
		var d opencode.AgentSelectedEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveConv(d.SessionID)
			if conv != nil && d.Agent != "" {
				conv.session.Agent = d.Agent
				if conv.loaded {
					w.reconcileTail(d.SessionID)
				}
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
				if conv.loaded {
					w.reconcileTail(d.SessionID)
				}
				if w.currentTab() == tab {
					w.updateBottomBar(tab)
				}
			}
		}

	case "session.step.started":
		var d opencode.StepStartedEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveConv(d.SessionID)
			w.touchSessionUpdated(tab, conv, time.Now().UnixMilli())
			if conv != nil {
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
			w.touchSessionUpdated(tab, conv, time.Now().UnixMilli())
			if conv != nil {
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
				if tab != nil && tab.main.session.ID != w.currentRootSessionID() && strings.TrimSpace(d.Delta) != "" {
					tab.page.SetNeedsAttention(true)
				}
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
			if tab != nil && tab.main.session.ID != w.currentRootSessionID() && strings.TrimSpace(d.Text) != "" {
				tab.page.SetNeedsAttention(true)
			}
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
				if d.ID != "" && d.Name != "" {
					conv.toolNames[d.ID] = d.Name
				}
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
				name := d.Name
				if name == "" && d.ID != "" {
					name = conv.toolNames[d.ID]
				}
				if conv.loaded {
					aw := w.ensureAssistantWidget(tab, conv, d.AssistantMessageID, "", "")
					tw := aw.EnsureTool(d.ID, name)
					tw.AppendInputDelta(d.Delta)
				}
			}
		}

	case "session.tool.called":
		var d opencode.ToolEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveConv(d.SessionID)
			var name string
			if conv != nil {
				if d.ID != "" && d.Name != "" {
					conv.toolNames[d.ID] = d.Name
				}
				name = d.Name
				if name == "" && d.ID != "" {
					name = conv.toolNames[d.ID]
				}
				if d.ID != "" {
					if isBlockingBackgroundableTool(name, "running", d.Input) {
						if conv.blockingTools[d.ID].IsZero() {
							conv.blockingTools[d.ID] = time.Now()
						}
						if isShellToolName(name) {
							conv.blockingShellTools[d.ID] = true
						}
					} else {
						delete(conv.blockingTools, d.ID)
						delete(conv.blockingShellTools, d.ID)
					}
				}
				if conv.loaded {
					aw := w.ensureAssistantWidget(tab, conv, d.AssistantMessageID, "", "")
					tw := aw.EnsureTool(d.ID, name)
					tw.SetStatus("running")
					tw.SetInput(d.Input)
					tw.SetMetadata(d.Metadata)
				}
				if w.currentTab() == tab {
					w.updateBottomBar(tab)
					w.rebuildShellList()
				}
			}
			if name == "subagent" || name == "task" {
				w.loadChildSubagents(d.SessionID)
			}
		}

	case "session.tool.progress", "session.tool.updated":
		var d opencode.ToolEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveConv(d.SessionID)
			if conv != nil {
				if d.ID != "" && d.Name != "" {
					conv.toolNames[d.ID] = d.Name
				}
				name := d.Name
				if name == "" && d.ID != "" {
					name = conv.toolNames[d.ID]
				}
				var inputDesc, inputAgent string
				if d.ID != "" && d.Input != nil {
					if m := opencode.ParseToolInputMap(d.Input); m != nil {
						if bg, ok := m["background"].(bool); ok && bg {
							delete(conv.blockingTools, d.ID)
							delete(conv.blockingShellTools, d.ID)
						}
						inputDesc, _ = m["description"].(string)
						inputAgent, _ = m["agent"].(string)
					}
				}
				var shID, childID string
				if d.Metadata != nil {
					shID, _ = d.Metadata["shellID"].(string)
					if shID != "" {
						if conv.blockingShellTools[d.ID] && !conv.backgroundShellIDs[shID] {
							conv.blockingShellIDs[shID] = d.ID
						} else if st, _ := d.Metadata["status"].(string); st == "running" {
							conv.backgroundShellIDs[shID] = true
							delete(conv.blockingShellIDs, shID)
						}
					}
					childID, _ = d.Metadata["sessionID"].(string)
					if childID != "" && tab != nil {
						agent, _ := d.Metadata["agent"].(string)
						if agent == "" {
							agent = inputAgent
						}
						st, _ := d.Metadata["status"].(string)
						w.upsertSubagent(tab, opencode.Session{
							ID:       childID,
							ParentID: tab.main.session.ID,
							Title:    inputDesc,
							Agent:    agent,
							Location: tab.main.session.Location,
						})
						if st == "" || st == "running" {
							w.setSubagentActive(childID, true, "")
						} else {
							w.setSubagentActive(childID, false, st)
						}
						w.loadChildSubagents(tab.main.session.ID)
					}
				}
				if conv.loaded {
					aw := w.ensureAssistantWidget(tab, conv, d.AssistantMessageID, "", "")
					tw := aw.EnsureTool(d.ID, name)
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
				if w.currentTab() == tab {
					w.updateBottomBar(tab)
					if childID != "" {
						w.rebuildSubagentList(tab)
					}
					if shID != "" {
						w.rebuildShellList()
						w.ensureShellOutputPoller()
					}
				}
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
				if d.ID != "" && d.Name != "" {
					conv.toolNames[d.ID] = d.Name
				}
				name := d.Name
				if name == "" && d.ID != "" {
					name = conv.toolNames[d.ID]
				}
				if d.ID != "" {
					delete(conv.blockingTools, d.ID)
					delete(conv.blockingShellTools, d.ID)
					for k, tid := range conv.blockingShellIDs {
						if tid == d.ID {
							delete(conv.blockingShellIDs, k)
						}
					}
				}
				var shID, childID string
				if d.Metadata != nil {
					shID, _ = d.Metadata["shellID"].(string)
					st, _ := d.Metadata["status"].(string)
					if st == "running" && shID != "" {
						conv.backgroundShellIDs[shID] = true
						delete(conv.blockingShellIDs, shID)
					}
					childID, _ = d.Metadata["sessionID"].(string)
					if childID != "" && tab != nil {
						agent, _ := d.Metadata["agent"].(string)
						w.upsertSubagent(tab, opencode.Session{
							ID:       childID,
							ParentID: tab.main.session.ID,
							Agent:    agent,
							Location: tab.main.session.Location,
						})
						if st == "running" {
							w.setSubagentActive(childID, true, "")
							w.loadChildSubagents(tab.main.session.ID)
						} else {
							w.setSubagentActive(childID, false, cmp.Or(st, "succeeded"))
						}
					}
				}
				if conv.loaded {
					aw := w.ensureAssistantWidget(tab, conv, d.AssistantMessageID, "", "")
					tw := aw.EnsureTool(d.ID, name)
					tw.SetStatus("completed")
					tw.SetOutput(d.Content, nil)
					tw.SetMetadata(d.Metadata)
					if shID == "" {
						shID = tw.ShellID()
					}
					if shID != "" && tw.IsBackgroundRunning() {
						conv.backgroundShellIDs[shID] = true
						delete(conv.blockingShellIDs, shID)
						if out, ok := w.shellOutputs[shID]; ok && out != "" {
							tw.SetShellLiveOutput(out)
						}
					}
				}
				if w.currentTab() == tab {
					w.updateBottomBar(tab)
					w.rebuildSubagentList(tab)
					w.rebuildShellList()
					w.ensureShellOutputPoller()
				}
				if shID != "" && conv.backgroundShellIDs[shID] {
					if _, exists := w.activeShells[shID]; !exists {
						w.refreshShells()
					}
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
				if d.ID != "" && d.Name != "" {
					conv.toolNames[d.ID] = d.Name
				}
				name := d.Name
				if name == "" && d.ID != "" {
					name = conv.toolNames[d.ID]
				}
				if d.ID != "" {
					delete(conv.blockingTools, d.ID)
					delete(conv.blockingShellTools, d.ID)
					for k, tid := range conv.blockingShellIDs {
						if tid == d.ID {
							delete(conv.blockingShellIDs, k)
						}
					}
				}
				if d.Metadata != nil {
					if childID, _ := d.Metadata["sessionID"].(string); childID != "" {
						w.setSubagentActive(childID, false, "failed")
					}
				}
				if conv.loaded {
					aw := w.ensureAssistantWidget(tab, conv, d.AssistantMessageID, "", "")
					tw := aw.EnsureTool(d.ID, name)
					tw.SetStatus("failed")
					tw.SetOutput(d.Content, d.Error)
					tw.SetMetadata(d.Metadata)
				}
				if w.currentTab() == tab {
					w.updateBottomBar(tab)
					w.rebuildSubagentList(tab)
					w.rebuildShellList()
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
			if sh.LocationDir == "" && ev.Location.Directory != "" {
				sh.LocationDir = ev.Location.Directory
			}
			if sh.ID != "" && (sh.Status == "" || sh.Status == "running") {
				w.activeShells[sh.ID] = sh
				w.updateShellStatus()
				w.rebuildShellList()
				w.ensureShellOutputPoller()
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
				prevSh, hadSh := w.activeShells[id]
				delete(w.activeShells, id)
				delete(w.shellOutputLabels, id)
				w.updateToolWidgetsForShell(id, w.shellOutputs[id], "completed")
				if hadSh {
					gtkutil.Async(w.ctx, func() func() {
						text, ok := w.fetchSingleShellOutput(prevSh)
						return func() {
							if ok && text != "" {
								w.shellOutputs[id] = text
								w.updateToolWidgetsForShell(id, text, "completed")
							}
						}
					})
				}
				w.updateShellStatus()
				w.rebuildShellList()
			}
		}

	case "form.created":
		var d opencode.FormCreatedEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.Form.ID != "" {
			w.upsertPendingForm(d.Form)
		}

	case "form.replied":
		var d opencode.FormRepliedEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.ID != "" {
			w.removePendingForm(d.SessionID, d.ID)
		}

	case "form.cancelled":
		var d opencode.FormCancelledEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.ID != "" {
			w.removePendingForm(d.SessionID, d.ID)
		}
	}
}
