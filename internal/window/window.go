// Package window implements the main GTK4 / libadwaita UI window and conversation components for Gluppencode.
package window

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"math"
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
	"github.com/diamondburned/gotk4/pkg/graphene"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"github.com/diamondburned/gotkit/app"
	"github.com/diamondburned/gotkit/gtkutil"
	"libdb.so/gluppencode/internal/autoscroll"
	"libdb.so/gluppencode/internal/mdstream"
	"libdb.so/gluppencode/internal/opencode"
)

//go:embed window.ui
var windowUI string

type pendingPrompt struct {
	inboxID   string
	text      string
	files     []opencode.FileAttachment
	opts      opencode.PromptOptions
	delivery  opencode.InboxDelivery
	rewriting bool
	cancelled bool
	attached  bool
	widget    *UserMessageWidget
}

func (p *pendingPrompt) dragKey() string {
	if p == nil {
		return ""
	}
	if p.inboxID != "" {
		return p.inboxID
	}
	return fmt.Sprintf("ptr:%p", p)
}

type activityPhase uint8

const (
	activityWaitingModel activityPhase = iota
	activityThinking
	activityResponding
	activityRetrying
)

type convTimeline struct {
	session            opencode.Session
	scroll             *autoscroll.Window
	clamp              *adw.Clamp
	timelineBox        *gtk.Box
	greeter            *GreeterWidget
	assistantMap       map[string]*AssistantMessageWidget
	compactionMap      map[string]*CompactionWidget
	messageWidgets     map[string]gtk.Widgetter
	slashBubbles       []*UserMessageWidget
	revertBanner       *RevertBannerWidget
	revertFilesRolled  bool
	thinkingLabel      *gtk.Label
	thinkingAttached   bool
	pendingPrompts     []*pendingPrompt
	ignoredCancelIDs   map[string]bool
	pendingForms       []opencode.FormInfo
	formWidgets        map[string]*QuestionFormWidget
	pendingPermissions []opencode.PermissionRequest
	permissionWidgets  map[string]*PermissionPromptWidget
	seenMsgIDs         map[string]bool
	messages           []opencode.Message
	nextCursor         string
	hasOlder           bool
	loadingOlder       bool
	loadingInitial     bool
	loaded             bool
	active             bool
	activityGen        uint64
	activityPhase      activityPhase
	retryAttempt       int
	compacting         bool
	runningUserShell   bool
	runningTools       int
	executingTools     map[string]string
	finishedTools      map[string]bool
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

	if part.State.Status == "running" && !c.finishedTools[part.ID] {
		if c.executingTools == nil {
			c.executingTools = make(map[string]string)
		}
		c.executingTools[part.ID] = name
	} else {
		delete(c.executingTools, part.ID)
		if part.State.Status == "completed" || part.State.Status == "error" || part.State.Status == "failed" {
			if c.finishedTools == nil {
				c.finishedTools = make(map[string]bool)
			}
			c.finishedTools[part.ID] = true
		}
	}

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

func formatExecutingToolsLabel(tools map[string]string) string {
	if len(tools) == 0 {
		return ""
	}
	if len(tools) == 1 {
		for _, name := range tools {
			switch {
			case isShellToolName(name):
				return "Running command..."
			case isSubagentTool(name):
				return "Waiting on subagent..."
			case strings.TrimSpace(name) == "":
				return "Running tool..."
			default:
				return fmt.Sprintf("Running %s...", formatToolTitle(name))
			}
		}
	}
	allShell := true
	allSubagent := true
	for _, name := range tools {
		if !isShellToolName(name) {
			allShell = false
		}
		if !isSubagentTool(name) {
			allSubagent = false
		}
	}
	switch {
	case allShell:
		return "Running commands..."
	case allSubagent:
		return "Waiting on subagents..."
	default:
		return "Running tools..."
	}
}

func (c *convTimeline) effectiveActivityText() string {
	if c == nil {
		return ""
	}
	if c.runningUserShell {
		return "Running command..."
	}
	if len(c.executingTools) > 0 {
		return formatExecutingToolsLabel(c.executingTools)
	}
	if c.compacting {
		return "Compacting context..."
	}
	switch c.activityPhase {
	case activityThinking:
		return "Thinking..."
	case activityResponding:
		return "Responding..."
	case activityRetrying:
		if c.retryAttempt > 0 {
			return fmt.Sprintf("Retrying (attempt %d)...", c.retryAttempt)
		}
		return "Retrying..."
	default:
		return "Waiting for model..."
	}
}

func (c *convTimeline) syncActivityLabel() {
	if c == nil || c.thinkingLabel == nil {
		return
	}
	visible := (c.active || c.compacting || c.runningUserShell) && len(c.pendingForms) == 0 && len(c.pendingPermissions) == 0
	if visible {
		want := c.effectiveActivityText()
		if c.thinkingLabel.Text() != want {
			c.thinkingLabel.SetLabel(want)
		}
	}
	if c.thinkingLabel.Visible() != visible {
		c.thinkingLabel.SetVisible(visible)
	}
}

func (c *convTimeline) setExecutingTool(id, name string) {
	if c == nil || id == "" {
		return
	}
	if c.executingTools == nil {
		c.executingTools = make(map[string]string)
	}
	delete(c.finishedTools, id)
	c.executingTools[id] = name
	c.syncActivityLabel()
}

func (c *convTimeline) clearExecutingTool(id string) {
	if c == nil || id == "" {
		return
	}
	delete(c.executingTools, id)
	if c.finishedTools == nil {
		c.finishedTools = make(map[string]bool)
	}
	c.finishedTools[id] = true
	if len(c.executingTools) == 0 && c.activityPhase != activityRetrying {
		c.activityPhase = activityThinking
	}
	c.syncActivityLabel()
}

func (c *convTimeline) inferActivityStateFromMessages(msgs []opencode.Message) {
	if c == nil {
		return
	}
	c.compacting = false
	c.retryAttempt = 0
	if len(msgs) == 0 {
		c.activityPhase = activityWaitingModel
		return
	}
	last := msgs[len(msgs)-1]
	switch last.Type {
	case "idle":
		c.activityPhase = activityWaitingModel
		return
	case "compaction":
		switch last.Status {
		case "running":
			c.compacting = true
			c.activityPhase = activityWaitingModel
		case "failed":
			c.activityPhase = activityWaitingModel
		default:
			c.activityPhase = activityThinking
		}
		return
	case "assistant":
		if last.Time.Completed != 0 {
			c.activityPhase = activityThinking
			return
		}
		if last.Retry != nil {
			c.activityPhase = activityRetrying
			c.retryAttempt = last.Retry.Attempt
			return
		}
		if len(last.Content) == 0 {
			c.activityPhase = activityThinking
			return
		}
		lastPart := last.Content[len(last.Content)-1]
		switch lastPart.Type {
		case "reasoning":
			c.activityPhase = activityThinking
		case "text":
			c.activityPhase = activityResponding
		case "tool":
			if lastPart.State.Status == "streaming" {
				c.activityPhase = activityResponding
			} else {
				c.activityPhase = activityThinking
			}
		default:
			c.activityPhase = activityThinking
		}
	default:
		// User, synthetic, and notice entries already persisted in history mean
		// inbox delivery has occurred and the runner is dispatching to the model.
		c.activityPhase = activityThinking
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
			var formatted string
			if body := stripSubagentTags(msg.Text); body != "" {
				formatted = FormatShellOutput(body)
			}
			for _, aw := range c.assistantMap {
				for _, tw := range aw.tools {
					if tw.ShellID() == shID {
						tw.SetBackgroundStatus("completed")
						if formatted != "" {
							tw.SetShellLiveOutput(formatted)
						}
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

// steeredInFlight reports whether the user steered p before its inbox ID was known
// (or while a reorder was re-admitting it), so the server still holds it as queued.
func (p *pendingPrompt) steeredInFlight(serverDelivery opencode.InboxDelivery) bool {
	return (p.inboxID == "" || p.rewriting) && p.delivery == opencode.DeliverySteer && serverDelivery == opencode.DeliveryQueue
}

func (c *convTimeline) keepPendingAtBottom() {
	if c == nil || c.timelineBox == nil {
		return
	}
	c.updateGreeterVisibility()
	for _, p := range c.pendingPrompts {
		if p != nil && p.widget != nil && p.attached && p.delivery != opencode.DeliveryQueue {
			c.timelineBox.Remove(p.widget)
			c.timelineBox.Append(p.widget)
		}
	}
	for _, f := range c.pendingForms {
		if qw := c.formWidgets[f.ID]; qw != nil {
			c.timelineBox.Remove(qw)
			c.timelineBox.Append(qw)
		}
	}
	for _, perm := range c.pendingPermissions {
		if pw := c.permissionWidgets[perm.ID]; pw != nil {
			c.timelineBox.Remove(pw)
			c.timelineBox.Append(pw)
		}
	}
	if c.thinkingLabel != nil {
		if c.thinkingAttached {
			c.timelineBox.Remove(c.thinkingLabel)
		}
		c.timelineBox.Append(c.thinkingLabel)
		c.thinkingAttached = true
		c.syncActivityLabel()
	}
	for _, p := range c.pendingPrompts {
		if p != nil && p.widget != nil && p.attached && p.delivery == opencode.DeliveryQueue {
			c.timelineBox.Remove(p.widget)
			c.timelineBox.Append(p.widget)
		}
	}
	if c.revertBanner != nil && c.revertBanner.Visible() {
		c.timelineBox.Remove(c.revertBanner)
		c.timelineBox.Append(c.revertBanner)
	}
}

type switcherIndicator struct {
	row          *gtk.ListBoxRow
	hBox         *gtk.Box
	spinner      *adw.Spinner
	questionIcon *gtk.Image
	shellIcon    *gtk.Image
	dot          *gtk.Box
	titleLbl     *gtk.Label
	box          *gtk.Box
	label        *gtk.Label
	closeBtn     *gtk.Button
	rowPtr       uintptr
}

type sessionTab struct {
	page                *adw.ViewStackPage
	stack               *gtk.Stack
	main                *convTimeline
	subagents           []opencode.Session
	subTimelines        map[string]*convTimeline
	activeSubID         string
	gitBranch           string
	title               string
	needsAttention      bool
	finishedAttention   bool
	actionAttention     bool
	sidebarIndicator    *switcherIndicator
	turnSeq             uint64
	turnSettled         bool
	turnHasReply        bool
	turnFailed          bool
	turnReplyMsgID      string
	turnReplyPreview    string
	turnReplyByMsgID    map[string]string
	completionSettleGen uint64
	completionTimer     *time.Timer
}

func (t *sessionTab) Title() string {
	if t == nil {
		return ""
	}
	if strings.TrimSpace(t.title) != "" {
		return strings.TrimSpace(t.title)
	}
	if t.main != nil && strings.TrimSpace(t.main.session.Title) != "" {
		return strings.TrimSpace(t.main.session.Title)
	}
	if t.page != nil && strings.TrimSpace(t.page.Title()) != "" {
		return strings.TrimSpace(t.page.Title())
	}
	return "Untitled"
}

func (t *sessionTab) setTitle(title string) {
	if t == nil {
		return
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = "Untitled"
	}
	t.title = title
	if t.page != nil && t.page.Title() != title {
		t.page.SetTitle(title)
	}
}

func (t *sessionTab) NeedsAttention() bool {
	if t == nil {
		return false
	}
	return t.needsAttention || t.finishedAttention || t.actionAttention
}

func (t *sessionTab) syncNeedsAttention() {
	if t == nil {
		return
	}
	needs := t.finishedAttention || t.actionAttention
	t.needsAttention = needs
	if t.page != nil && t.page.NeedsAttention() != needs {
		t.page.SetNeedsAttention(needs)
	}
}

func (t *sessionTab) setNeedsAttention(needs bool) {
	if t == nil {
		return
	}
	t.finishedAttention = needs
	if !needs {
		t.actionAttention = false
	}
	t.syncNeedsAttention()
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

	rootContent         *adw.OverlaySplitView
	sidebarBreakpoint   *adw.Breakpoint
	windowStack         *gtk.Stack
	offlineView         *adw.ToolbarView
	offlineStatus       *adw.StatusPage
	offlineErrorLabel   *gtk.Label
	offlineRetryBtn     *gtk.Button
	offline             bool
	offlineErr          string
	sidebarToolbar      *adw.ToolbarView
	sidebarResizeHandle *gtk.Box
	hideSidebarBtn      *gtk.Button
	appMenuSidebarBtn   *gtk.MenuButton
	searchSessionsBtn   *gtk.Button
	sidebarScroll       *gtk.ScrolledWindow
	sidebarList         *gtk.ListBox

	bottomSheet           *adw.BottomSheet
	showSidebarBtn        *gtk.Button
	headerTitleStack      *gtk.Stack
	windowTitleBtn        *gtk.MenuButton
	windowTitle           *adw.WindowTitle
	renameSessionPopover  *gtk.Popover
	renameSessionEntry    *gtk.Entry
	renameSessionSaveBtn  *gtk.Button
	breadcrumbParentBtn   *gtk.Button
	breadcrumbParentLabel *gtk.Label
	breadcrumbChildLabel  *gtk.Label
	newWindowBtn          *gtk.Button
	refreshBtn            *gtk.Button
	takeChargeBtn         *gtk.ToggleButton
	contentMainBox        *gtk.Box
	emptyGreeter          *GreeterWidget
	viewStackOverlay      *gtk.Overlay
	composerInset         int
	composerInsetCSS      *gtk.CSSProvider
	viewStack             *adw.ViewStack
	composerClamp         *adw.Clamp

	inputView         *gtksource.View
	inputBuffer       *gtksource.Buffer
	inputScroll       *gtk.ScrolledWindow
	composerExpandBtn *gtk.Button
	composerExpanded  bool
	mdTagBold         *gtk.TextTag
	mdTagItalic       *gtk.TextTag
	mdTagStrike       *gtk.TextTag
	mdTagMono         *gtk.TextTag
	mdTagCodeBlock    *gtk.TextTag
	sendBtn                *gtk.Button
	stopBtn                *gtk.Button
	attachBtn              *gtk.Button
	attachmentTray         *adw.WrapBox
	pendingFiles           []opencode.FileAttachment

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
	stopConfirmBtn      *gtk.Button
	backgroundStatusBtn *gtk.Button
	mcpStatusBox        *gtk.Box
	mcpStatusLabel      *gtk.Label
	shellStatusBox      *gtk.Box
	shellStatusLabel    *gtk.Label
	subagentStatusBox   *gtk.Box
	subagentStatusLabel *gtk.Label
	tokRateStatusLabel  *gtk.Label
	contextStatusLabel  *gtk.Label

	sheetToastOverlay     *adw.ToastOverlay
	sheetNavView          *adw.NavigationView
	sheetOverviewPage     *adw.NavigationPage
	sheetEmptyLabel       *gtk.Label
	shellsSectionBox      *gtk.Box
	shellListBox          *gtk.ListBox
	subagentsSectionBox   *gtk.Box
	sheetBackToParentBtn  *gtk.Button
	subagentListBox       *gtk.ListBox
	mcpSectionBox         *gtk.Box
	mcpDirLabel           *gtk.Label
	mcpAllLogsBtn         *gtk.Button
	mcpRefreshBtn         *gtk.Button
	mcpErrBanner          *adw.Banner
	mcpSearchEntry        *gtk.SearchEntry
	mcpListStack          *gtk.Stack
	mcpListBox            *gtk.ListBox
	contextSectionBox     *gtk.Box
	contextSessionIDBox   *gtk.Box
	contextSessionIDLabel *gtk.Label
	copySessionIDBtn      *gtk.Button
	contextInfoGrid       *gtk.Grid
	contextInfoCells      []*gtk.Box
	agentListBox          *gtk.ListBox
	modelSearchEntry      *gtk.SearchEntry
	modelListBox          *gtk.ListBox

	activeShells      map[string]opencode.ShellItem
	shellsLoaded      bool // a refreshShells listing has succeeded for every directory
	shellOutputs      map[string]string
	shellOutputLabels map[string]*gtk.Label
	shellPollRunning  bool

	mcpByDir         map[string][]opencode.McpServer
	mcpLoadedDirs    map[string]bool
	mcpFetchingDirs  map[string]bool
	mcpInFlight      map[string]bool
	defaultServerDir string
	mcpLogDir        string
	mcpMgr           *mcpManager

	tabs        map[string]*sessionTab // keyed by root session ID
	tabOrder    []string               // root session IDs in display order
	childToRoot map[string]string      // child session ID -> root session ID

	sessionTouched map[string]time.Time // root session ID -> last activity or visit; see suspend.go
	shownRoot      string               // root session last shown by onVisibleTabChanged
	agents         []opencode.Agent
	agentRows      []agentRowData
	models         []opencode.ModelInfo
	modelNames     map[string]string
	modelRows      []modelRowData
	subagentRows   []subagentRowData
	modelLimits    map[string]int
	branchCache    map[string]string

	modelLastUsed      map[string]int64
	lastSelectedAgent  string
	lastSelectedModel  opencode.ModelRef
	showAllModels      bool
	showAllModelsRow   *gtk.ListBoxRow
	showAllModelsLabel *gtk.Label

	expandedProjects  map[string]bool
	collapsedProjects map[string]bool
	visibleSessions   map[string]bool
	archivedSessions  map[string]bool
	projectOrder      []string
	statePath         string
	draftsDir         string
	drafts            map[string]*composerDraft
	activeDraftKey    string
	draftGenMu        sync.Mutex
	draftWriteGen     map[string]uint64
	draftWriteMu      sync.Mutex
	allSessions       []opencode.Session
	projectSessions   map[string][]opencode.Session

	closeBtnSessID       map[uintptr]string
	hoveredRow           map[uintptr]bool
	rowProjectDir        map[uintptr]string
	sidebarRowSessID     map[uintptr]string
	headerProjectDir     map[uintptr]string
	headerWidgets        map[uintptr]*projectHeaderWidgets
	projectHeaderRows    map[string]*gtk.ListBoxRow
	projectShowMoreRows  map[string]*gtk.ListBoxRow
	projectShowMoreLbls  map[string]*gtk.Label
	projectShowMoreDirs  map[uintptr]string
	sidebarSetupDone     bool
	syncingSidebarSelect bool
	fileOpener           func(string)
	clipboardSetter      func(string)
	uriLauncher          func(string) error

	takeCharge         bool
	takeChargeInFlight map[string]bool

	resolvedSessionTitles map[string]string
	resolvingSessions     map[string][]func(string)

	inputHistory       []string
	historyIndex       int
	historyBrowsing    bool
	historyDraft       string
	settingHistoryText bool

	sidebarWidth          int
	sidebarDragStartWidth int
	resizingSidebar       bool
	resizerHovered        bool

	greeterProfile    *GreeterProfile
	greeterRefreshing bool
	prefs             Preferences

	notifyCoord           *notificationCoordinator
	completionSettleDelay time.Duration

	escStopArmedSess string
	escStopGen       uint64
	escStopTimer     glib.SourceHandle
	escStopTimeoutMS uint

	populating bool
}

var userCSSOnce sync.Once

// NewWindow creates a new main window from window.ui.
func NewWindow(ctx context.Context, client *opencode.Client) *Window {
	gtksource.Init()
	if disp := gdk.DisplayGetDefault(); disp != nil {
		gtk.IconThemeGetForDisplay(disp).AddResourcePath("/so/libdb/gluppencode/icons")
		userCSSOnce.Do(func() {
			prov := gtk.NewCSSProvider()
			prov.LoadFromString(styleCSS)
			gtk.StyleContextAddProviderForDisplay(disp, prov, gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)

			userProv := gtk.NewCSSProvider()
			userProv.LoadFromString("headerbar.glup-sidebar-header > windowhandle > box { padding-right: 7px; }")
			gtk.StyleContextAddProviderForDisplay(disp, userProv, gtk.STYLE_PROVIDER_PRIORITY_USER)
		})
	}
	a := app.FromContext(ctx)
	adwWin := adw.NewApplicationWindow(a.Application)
	adwWin.SetTitle("gluppencode")
	adwWin.SetSizeRequest(360, 480)
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
		sidebarToolbar:        builder.GetObject("sidebarToolbar").Cast().(*adw.ToolbarView),
		sidebarResizeHandle:   builder.GetObject("sidebarResizeHandle").Cast().(*gtk.Box),
		hideSidebarBtn:        builder.GetObject("hideSidebarBtn").Cast().(*gtk.Button),
		appMenuSidebarBtn:     builder.GetObject("appMenuSidebarBtn").Cast().(*gtk.MenuButton),
		searchSessionsBtn:     builder.GetObject("searchSessionsBtn").Cast().(*gtk.Button),
		sidebarScroll:         builder.GetObject("sidebarScroll").Cast().(*gtk.ScrolledWindow),
		sidebarList:           builder.GetObject("sidebarList").Cast().(*gtk.ListBox),
		bottomSheet:           builder.GetObject("bottomSheet").Cast().(*adw.BottomSheet),
		showSidebarBtn:        builder.GetObject("showSidebarBtn").Cast().(*gtk.Button),
		headerTitleStack:      builder.GetObject("headerTitleStack").Cast().(*gtk.Stack),
		windowTitleBtn:        builder.GetObject("windowTitleBtn").Cast().(*gtk.MenuButton),
		windowTitle:           builder.GetObject("windowTitle").Cast().(*adw.WindowTitle),
		renameSessionPopover:  builder.GetObject("renameSessionPopover").Cast().(*gtk.Popover),
		renameSessionEntry:    builder.GetObject("renameSessionEntry").Cast().(*gtk.Entry),
		renameSessionSaveBtn:  builder.GetObject("renameSessionSaveBtn").Cast().(*gtk.Button),
		breadcrumbParentBtn:   builder.GetObject("breadcrumbParentBtn").Cast().(*gtk.Button),
		breadcrumbParentLabel: builder.GetObject("breadcrumbParentLabel").Cast().(*gtk.Label),
		breadcrumbChildLabel:  builder.GetObject("breadcrumbChildLabel").Cast().(*gtk.Label),
		newWindowBtn:          builder.GetObject("newWindowBtn").Cast().(*gtk.Button),
		refreshBtn:            builder.GetObject("refreshBtn").Cast().(*gtk.Button),
		takeChargeBtn:         builder.GetObject("takeChargeBtn").Cast().(*gtk.ToggleButton),
		contentMainBox:        builder.GetObject("contentMainBox").Cast().(*gtk.Box),
		viewStackOverlay:      builder.GetObject("viewStackOverlay").Cast().(*gtk.Overlay),
		viewStack:             builder.GetObject("viewStack").Cast().(*adw.ViewStack),
		composerClamp:     builder.GetObject("composerClamp").Cast().(*adw.Clamp),
		composerExpandBtn: builder.GetObject("composerExpandBtn").Cast().(*gtk.Button),
		inputScroll:       builder.GetObject("inputScroll").Cast().(*gtk.ScrolledWindow),
		inputView:         builder.GetObject("inputView").Cast().(*gtksource.View),
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
		stopConfirmBtn:        builder.GetObject("stopConfirmBtn").Cast().(*gtk.Button),
		backgroundStatusBtn:   builder.GetObject("backgroundStatusBtn").Cast().(*gtk.Button),
		mcpStatusBox:          builder.GetObject("mcpStatusBox").Cast().(*gtk.Box),
		mcpStatusLabel:        builder.GetObject("mcpStatusLabel").Cast().(*gtk.Label),
		shellStatusBox:        builder.GetObject("shellStatusBox").Cast().(*gtk.Box),
		shellStatusLabel:      builder.GetObject("shellStatusLabel").Cast().(*gtk.Label),
		subagentStatusBox:     builder.GetObject("subagentStatusBox").Cast().(*gtk.Box),
		subagentStatusLabel:   builder.GetObject("subagentStatusLabel").Cast().(*gtk.Label),
		tokRateStatusLabel:    builder.GetObject("tokRateStatusLabel").Cast().(*gtk.Label),
		contextStatusLabel:    builder.GetObject("contextStatusLabel").Cast().(*gtk.Label),
		sheetToastOverlay:     builder.GetObject("sheetToastOverlay").Cast().(*adw.ToastOverlay),
		sheetNavView:          builder.GetObject("sheetNavView").Cast().(*adw.NavigationView),
		sheetOverviewPage:     builder.GetObject("sheetOverviewPage").Cast().(*adw.NavigationPage),
		sheetEmptyLabel:       builder.GetObject("sheetEmptyLabel").Cast().(*gtk.Label),
		shellsSectionBox:      builder.GetObject("shellsSectionBox").Cast().(*gtk.Box),
		shellListBox:          builder.GetObject("shellListBox").Cast().(*gtk.ListBox),
		subagentsSectionBox:   builder.GetObject("subagentsSectionBox").Cast().(*gtk.Box),
		sheetBackToParentBtn:  builder.GetObject("sheetBackToParentBtn").Cast().(*gtk.Button),
		subagentListBox:       builder.GetObject("subagentListBox").Cast().(*gtk.ListBox),
		mcpSectionBox:         builder.GetObject("mcpSectionBox").Cast().(*gtk.Box),
		mcpDirLabel:           builder.GetObject("mcpDirLabel").Cast().(*gtk.Label),
		mcpAllLogsBtn:         builder.GetObject("mcpAllLogsBtn").Cast().(*gtk.Button),
		mcpRefreshBtn:         builder.GetObject("mcpRefreshBtn").Cast().(*gtk.Button),
		mcpErrBanner:          builder.GetObject("mcpErrBanner").Cast().(*adw.Banner),
		mcpSearchEntry:        builder.GetObject("mcpSearchEntry").Cast().(*gtk.SearchEntry),
		mcpListStack:          builder.GetObject("mcpListStack").Cast().(*gtk.Stack),
		mcpListBox:            builder.GetObject("mcpListBox").Cast().(*gtk.ListBox),
		contextSectionBox:     builder.GetObject("contextSectionBox").Cast().(*gtk.Box),
		contextSessionIDBox:   builder.GetObject("contextSessionIDBox").Cast().(*gtk.Box),
		contextSessionIDLabel: builder.GetObject("contextSessionIDLabel").Cast().(*gtk.Label),
		copySessionIDBtn:      builder.GetObject("copySessionIDBtn").Cast().(*gtk.Button),
		contextInfoGrid:       builder.GetObject("contextInfoGrid").Cast().(*gtk.Grid),
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
		collapsedProjects:     make(map[string]bool),
		visibleSessions:       make(map[string]bool),
		archivedSessions:      make(map[string]bool),
		statePath:             defaultStateFilePath(),
		draftsDir:             defaultDraftsDirForState(defaultStateFilePath()),
		drafts:                make(map[string]*composerDraft),
		draftWriteGen:         make(map[string]uint64),
		projectSessions:       make(map[string][]opencode.Session),
		activeShells:          make(map[string]opencode.ShellItem),
		shellOutputs:          make(map[string]string),
		shellOutputLabels:     make(map[string]*gtk.Label),
		mcpByDir:              make(map[string][]opencode.McpServer),
		mcpLoadedDirs:         make(map[string]bool),
		mcpFetchingDirs:       make(map[string]bool),
		mcpInFlight:           make(map[string]bool),
		closeBtnSessID:        make(map[uintptr]string),
		hoveredRow:            make(map[uintptr]bool),
		rowProjectDir:         make(map[uintptr]string),
		sidebarRowSessID:      make(map[uintptr]string),
		headerProjectDir:      make(map[uintptr]string),
		headerWidgets:         make(map[uintptr]*projectHeaderWidgets),
		projectHeaderRows:     make(map[string]*gtk.ListBoxRow),
		projectShowMoreRows:   make(map[string]*gtk.ListBoxRow),
		projectShowMoreLbls:   make(map[string]*gtk.Label),
		projectShowMoreDirs:   make(map[uintptr]string),
		takeChargeInFlight:    make(map[string]bool),
		resolvedSessionTitles: make(map[string]string),
		resolvingSessions:     make(map[string][]func(string)),
	}
	w.ctx = mdstream.WithAsyncSessionResolver(w.ctx, w.resolveSessionTitleAsync)
	w.ctx = mdstream.WithLinkHandler(w.ctx, w.handleMarkdownLink)
	w.ctx = WithDiffMaxLines(w.ctx, w.effectiveDiffTruncationLines)
	w.copySessionIDBtn.ConnectClicked(func() {
		if w.contextSessionIDLabel == nil {
			return
		}
		id := strings.TrimSpace(w.contextSessionIDLabel.Text())
		if id != "" {
			if d := gdk.DisplayGetDefault(); d != nil {
				d.Clipboard().SetText(id)
			}
		}
	})
	w.loadState()
	w.updateComposerAgentModel(w.lastSelectedAgent, w.lastSelectedModel)
	w.initNotificationCoordinator(a.Application)
	w.emptyGreeter = NewGreeterWidget(w.effectiveGreeterProfile(), w.populateComposerSuggestion)
	w.contentMainBox.Prepend(w.emptyGreeter)
	w.syncEmptyGreeterVisibility()
	w.setupComposerFade(
		builder.GetObject("contentOverlay").Cast().(*gtk.Overlay),
		builder.GetObject("composerBar").Cast().(*gtk.Box),
		builder.GetObject("timelineBottomFade").Cast().(*gtk.Box),
	)
	w.inputBuffer = w.inputView.Buffer().Cast().(*gtksource.Buffer)
	w.initComposerDrafts()
	w.setupComposerMarkdown()
	w.setupComposerMultiline()

	w.applyAllPreferences()
	adw.StyleManagerGetDefault().NotifyProperty("dark", func() {
		w.applySyntaxTheme()
		if w.mcpMgr != nil {
			w.mcpMgr.applyLogTheme()
		}
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
		w.openBottomSheet(false, "")
	})
	statusBarBox.AddController(statusClick)
	w.initMcpSheet()

	w.stopConfirmBtn.ConnectClicked(w.interruptSession)
	w.backgroundStatusBtn.ConnectClicked(func() {
		w.backgroundCurrentSession()
	})

	// Wire custom GtkListBox sidebar selection/activation to viewStack.
	w.setupSidebarList()

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
	w.setupSidebarResize()

	// Mobile adaptive breakpoint: collapse sidebar into overlay on narrow screens (max(600px, sidebar_width*2)).
	w.sidebarBreakpoint = adw.NewBreakpoint(adw.BreakpointConditionParse(fmt.Sprintf("max-width: %dpx", w.sidebarBreakpointWidth())))
	w.sidebarBreakpoint.AddSetter(w.rootContent, "collapsed", true)
	adwWin.AddBreakpoint(w.sidebarBreakpoint)

	adwWin.NotifyProperty("is-active", func() {
		if adwWin.IsActive() {
			if rootID := w.currentRootSessionID(); rootID != "" {
				if tab := w.tabs[rootID]; tab != nil && tab.finishedAttention {
					tab.finishedAttention = false
					tab.syncNeedsAttention()
					w.updateWindowManagerTitle()
					w.updateSwitcherIndicators()
				}
				w.withdrawFinishedNotification(rootID)
			}
		}
	})

	adwWin.ConnectCloseRequest(func() bool {
		w.flushAllDraftsSync()
		w.disarmEscStop()
		if w.mcpMgr != nil {
			w.mcpMgr.closeAuthDialog()
			w.mcpMgr.stopLogPoller()
		}
		w.cancelAllCompletionSettles()
		w.unregisterFromNotifications()
		cancel()
		for _, tab := range w.tabs {
			tab.main.closeStreams()
			for _, c := range tab.subTimelines {
				c.closeStreams()
			}
		}
		return false
	})

	w.renameSessionPopover.ConnectShow(func() {
		if tab := w.currentTab(); tab != nil {
			w.renameSessionEntry.SetText(tab.main.session.Title)
			w.renameSessionEntry.GrabFocus()
		}
	})
	w.renameSessionEntry.ConnectActivate(w.submitRenameSession)
	w.renameSessionSaveBtn.ConnectClicked(w.submitRenameSession)
	w.searchSessionsBtn.ConnectClicked(w.openSessionSearchDialog)
	w.setupAppMenuButton(w.appMenuSidebarBtn)
	w.newWindowBtn.ConnectClicked(w.openNewWindow)
	w.refreshBtn.ConnectClicked(w.loadAll)
	w.takeChargeBtn.ConnectToggled(func() {
		w.setTakeCharge(w.takeChargeBtn.Active())
	})

	// Window-level shortcuts: Ctrl+, for Preferences, Ctrl+Shift+N for New Window, Ctrl+K for Search Sessions, Ctrl+B to Background.
	winKeyCtrl := gtk.NewEventControllerKey()
	winKeyCtrl.ConnectKeyPressed(func(keyval, _ uint, state gdk.ModifierType) bool {
		if keyval == gdk.KEY_comma && state.Has(gdk.ControlMask) && !state.Has(gdk.ShiftMask) {
			w.openPreferencesDialog()
			return true
		}
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
		if keyval == gdk.KEY_Escape {
			if w.bottomSheet != nil && w.bottomSheet.Open() {
				return w.handleBottomSheetEscape()
			}
			if w.handleEscapeStop(state) {
				return true
			}
		}
		return false
	})
	adwWin.AddController(winKeyCtrl)

	// Composer Enter / Ctrl+Enter / Alt+Enter handling, Up/Down history navigation, Ctrl+B to background, and Ctrl+V clipboard image pasting.
	steerKeyCtrl := gtk.NewEventControllerKey()
	steerKeyCtrl.SetPropagationPhase(gtk.PhaseCapture)
	steerKeyCtrl.ConnectKeyPressed(func(keyval, _ uint, state gdk.ModifierType) bool {
		if delivery, ok := w.composerEnterAction(true, keyval, state); ok {
			w.sendPromptWithDelivery(delivery)
			return true
		}
		if (keyval == gdk.KEY_B || keyval == gdk.KEY_b) && state.Has(gdk.ControlMask) && !state.Has(gdk.ShiftMask) {
			if w.backgroundCurrentSession() {
				return true
			}
		}
		if w.handleInputHistoryKey(keyval, state) {
			return true
		}
		return false
	})
	w.inputView.AddController(steerKeyCtrl)

	keyCtrl := gtk.NewEventControllerKey()
	keyCtrl.ConnectKeyPressed(func(keyval, _ uint, state gdk.ModifierType) bool {
		if delivery, ok := w.composerEnterAction(false, keyval, state); ok {
			w.sendPromptWithDelivery(delivery)
			return true
		}
		if keyval == gdk.KEY_comma && state.Has(gdk.ControlMask) && !state.Has(gdk.ShiftMask) {
			w.openPreferencesDialog()
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

	w.initOfflineScreen()
	adwWin.SetContent(w.windowStack)
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

func (w *Window) initOfflineScreen() {
	if w.offlineView != nil {
		return
	}
	w.offlineView = adw.NewToolbarView()
	w.offlineView.SetTopBarStyle(adw.ToolbarFlat)

	header := adw.NewHeaderBar()
	header.SetTitleWidget(adw.NewWindowTitle("Gluppencode", "Offline"))
	w.offlineView.AddTopBar(header)

	w.offlineStatus = adw.NewStatusPage()
	w.offlineStatus.SetIconName("cloud-off-symbolic")
	w.offlineStatus.SetTitle("OpenCode Instance Offline")
	w.offlineStatus.SetDescription("Could not connect to the OpenCode server. Make sure the OpenCode service is running and try again.")
	w.offlineStatus.SetVExpand(true)
	w.offlineStatus.SetHExpand(true)

	childBox := gtk.NewBox(gtk.OrientationVertical, 16)
	childBox.SetHAlign(gtk.AlignCenter)

	w.offlineErrorLabel = gtk.NewLabel("")
	w.offlineErrorLabel.SetWrap(true)
	w.offlineErrorLabel.SetWrapMode(pango.WrapWordChar)
	w.offlineErrorLabel.SetMaxWidthChars(64)
	w.offlineErrorLabel.SetJustify(gtk.JustifyCenter)
	w.offlineErrorLabel.SetSelectable(true)
	w.offlineErrorLabel.SetVisible(false)
	w.offlineErrorLabel.AddCSSClass("glup-offline-error")
	childBox.Append(w.offlineErrorLabel)

	w.offlineRetryBtn = gtk.NewButtonWithLabel("Retry Connection")
	w.offlineRetryBtn.SetHAlign(gtk.AlignCenter)
	w.offlineRetryBtn.AddCSSClass("suggested-action")
	w.offlineRetryBtn.AddCSSClass("pill")
	w.offlineRetryBtn.ConnectClicked(func() {
		w.retryConnection()
	})
	childBox.Append(w.offlineRetryBtn)

	w.offlineStatus.SetChild(childBox)
	w.offlineView.SetContent(w.offlineStatus)

	if w.windowStack == nil {
		w.windowStack = gtk.NewStack()
		w.windowStack.SetTransitionType(gtk.StackTransitionTypeCrossfade)
		w.windowStack.SetHhomogeneous(false)
		w.windowStack.SetVhomogeneous(false)
		w.windowStack.SetVExpand(true)
		w.windowStack.SetHExpand(true)
	}
	if w.rootContent != nil {
		w.windowStack.AddNamed(w.rootContent, "main")
	}
	w.windowStack.AddNamed(w.offlineView, "offline")
	if w.rootContent != nil {
		w.windowStack.SetVisibleChildName("main")
	}
}

func (w *Window) setOfflineError(err error) {
	if err == nil {
		w.offline = false
		w.offlineErr = ""
		if w.windowStack != nil && w.rootContent != nil {
			w.windowStack.SetVisibleChildName("main")
		}
		return
	}
	w.initOfflineScreen()
	w.offline = true
	w.offlineErr = strings.TrimSpace(err.Error())

	serverURL := "http://127.0.0.1:4096"
	if w.client != nil {
		if u := strings.TrimSpace(w.client.ServerURL()); u != "" {
			serverURL = u
		}
	}
	if w.offlineStatus != nil {
		w.offlineStatus.SetDescription(fmt.Sprintf(
			"Could not connect to the OpenCode server at %s. Make sure the OpenCode service is running and try again.",
			serverURL,
		))
	}
	if w.offlineErrorLabel != nil {
		w.offlineErrorLabel.SetLabel(w.offlineErr)
		w.offlineErrorLabel.SetVisible(w.offlineErr != "")
	}
	if w.windowStack != nil && w.offlineView != nil {
		w.windowStack.SetVisibleChildName("offline")
	}
}

func (w *Window) retryConnection() {
	if w.client != nil {
		w.client.Rediscover(w.ctx)
	}
	w.loadAll()
}

// setupComposerFade positions the fade ramp directly above the floating
// composer and keeps the timelines padded by the composer's live height (the
// inset), so the last message can scroll clear of the fade.
func (w *Window) setupComposerFade(overlay *gtk.Overlay, composerBar, fade *gtk.Box) {
	w.composerInsetCSS = gtk.NewCSSProvider()
	if disp := gdk.DisplayGetDefault(); disp != nil {
		gtk.StyleContextAddProviderForDisplay(disp, w.composerInsetCSS, gtk.STYLE_PROVIDER_PRIORITY_APPLICATION+1)
	}

	fadeNative := gtk.BaseWidget(fade).Object.Native()
	overlay.ConnectGetChildPosition(func(child gtk.Widgetter) (*gdk.Rectangle, bool) {
		if gtk.BaseWidget(child).Object.Native() != fadeNative {
			rect := gdk.NewRectangle(0, 0, 0, 0)
			return &rect, false // default placement
		}
		width, height := overlay.Width(), overlay.Height()
		inset := 0
		if composerBar.Visible() {
			_, inset, _, _ = composerBar.Measure(gtk.OrientationVertical, width)
		}
		_, ramp, _, _ := fade.Measure(gtk.OrientationVertical, width)
		if inset != w.composerInset {
			w.composerInset = inset
			// Restyling during allocation would re-queue a resize mid-layout.
			glib.IdleAdd(w.applyComposerInset)
		}
		rect := gdk.NewRectangle(0, height-inset-ramp, width, ramp)
		return &rect, true
	})
}

func (w *Window) applyComposerInset() {
	// Clearance above the composer for the last message. It sits in the
	// lightest part of the 40px fade ramp, so it's barely dimmed.
	const clearance = 24
	inset := w.composerInset
	w.composerInsetCSS.LoadFromString(fmt.Sprintf(`
.glup-timeline-box { padding-bottom: %dpx; }
scrolledwindow.glup-timeline-scroll > scrollbar.vertical { margin-bottom: %dpx; }
`, inset+clearance, inset))
	if w.emptyGreeter != nil {
		w.emptyGreeter.SetMarginBottom(inset)
	}
}

func (w *Window) syncEmptyGreeterVisibility() {
	hasTab := w.currentTab() != nil
	if w.emptyGreeter != nil {
		w.emptyGreeter.SetVisible(!hasTab)
	}
	if w.viewStackOverlay != nil {
		w.viewStackOverlay.SetVisible(hasTab)
	}
	if w.viewStack != nil {
		w.viewStack.SetVisible(hasTab)
	}
}

func (w *Window) nextVisibleTabID(excludeID, preferDir string, excludeSet map[string]bool) string {
	idx := slices.Index(w.tabOrder, excludeID)
	isAllowed := func(id string) bool {
		return id != "" && id != excludeID && !excludeSet[id] && w.tabs[id] != nil
	}
	if preferDir != "" {
		for i := idx + 1; i < len(w.tabOrder); i++ {
			id := w.tabOrder[i]
			if isAllowed(id) && w.tabs[id].main != nil && normalizeProjectDir(w.tabs[id].main.session.Location.Directory) == preferDir {
				return id
			}
		}
		for i := idx - 1; i >= 0; i-- {
			id := w.tabOrder[i]
			if isAllowed(id) && w.tabs[id].main != nil && normalizeProjectDir(w.tabs[id].main.session.Location.Directory) == preferDir {
				return id
			}
		}
	}
	for i := idx + 1; i < len(w.tabOrder); i++ {
		if id := w.tabOrder[i]; isAllowed(id) {
			return id
		}
	}
	for i := idx - 1; i >= 0; i-- {
		if id := w.tabOrder[i]; isAllowed(id) {
			return id
		}
	}
	return ""
}

func (w *Window) cleanupTabSidebarState(tab *sessionTab) {
	if tab == nil || tab.sidebarIndicator == nil {
		return
	}
	ind := tab.sidebarIndicator
	if ind.closeBtn != nil {
		delete(w.closeBtnSessID, ind.closeBtn.Object.Native())
	}
	if ind.rowPtr != 0 {
		delete(w.hoveredRow, ind.rowPtr)
		delete(w.sidebarRowSessID, ind.rowPtr)
		delete(w.rowProjectDir, ind.rowPtr)
	}
	if ind.row != nil {
		rp := ind.row.Object.Native()
		delete(w.hoveredRow, rp)
		delete(w.sidebarRowSessID, rp)
		delete(w.rowProjectDir, rp)
	}
	tab.sidebarIndicator = nil
}

func (w *Window) moveSessionInProjectCache(sessID, oldRawDir, newRawDir string, updated *opencode.Session) {
	if w.projectSessions == nil || sessID == "" {
		return
	}
	oldDir := normalizeProjectDir(oldRawDir)
	newDir := normalizeProjectDir(newRawDir)
	var found *opencode.Session
	for dir, list := range w.projectSessions {
		if dir == newDir {
			continue
		}
		filtered := list[:0]
		for _, s := range list {
			if s.ID == sessID {
				sCopy := s
				found = &sCopy
				continue
			}
			filtered = append(filtered, s)
		}
		if len(filtered) == 0 {
			delete(w.projectSessions, dir)
		} else {
			w.projectSessions[dir] = filtered
		}
	}
	if oldDir != "" && oldDir != newDir && len(w.projectSessions[oldDir]) == 0 {
		delete(w.projectSessions, oldDir)
	}
	if newDir == "" || w.archivedSessions[sessID] {
		return
	}
	entry := updated
	if entry == nil {
		entry = found
	}
	if entry == nil || entry.ParentID != "" {
		return
	}
	dest := w.projectSessions[newDir]
	placed := false
	for i := range dest {
		if dest[i].ID == sessID {
			dest[i] = *entry
			placed = true
			break
		}
	}
	if !placed {
		dest = append([]opencode.Session{*entry}, dest...)
	}
	slices.SortStableFunc(dest, func(a, b opencode.Session) int {
		return cmp.Compare(sessionUpdatedTime(b), sessionUpdatedTime(a))
	})
	w.projectSessions[newDir] = dest
}

func (w *Window) ensureSessionTab(sess opencode.Session) *sessionTab {
	if sess.Time.Updated == 0 {
		sess.Time.Updated = cmp.Or(sess.Time.Created, time.Now().UnixMilli())
	}
	normDir := normalizeProjectDir(sess.Location.Directory)
	if w.visibleSessions == nil {
		w.visibleSessions = make(map[string]bool)
	}
	stateChanged := false
	if sess.ID != "" && !w.visibleSessions[sess.ID] {
		w.visibleSessions[sess.ID] = true
		stateChanged = true
	}
	if w.ensureProjectOrder(normDir) {
		stateChanged = true
	}
	if stateChanged {
		w.saveState()
	}
	if tab, ok := w.tabs[sess.ID]; ok {
		needSync := false
		if sess.Title != "" && tab.main.session.Title != sess.Title {
			tab.main.session.Title = sess.Title
			tab.setTitle(sess.Title)
			w.updateSwitcherIndicators()
		}
		if sess.Location.Directory != "" {
			oldDir := normalizeProjectDir(tab.main.session.Location.Directory)
			if normDir != "" && normDir != oldDir {
				tab.main.session.Location = sess.Location
				for i := range w.allSessions {
					if w.allSessions[i].ID == sess.ID {
						w.allSessions[i].Location = sess.Location
						break
					}
				}
				w.moveSessionInProjectCache(sess.ID, oldDir, normDir, &tab.main.session)
				needSync = true
			}
		}
		if sess.Time.Updated > tab.main.session.Time.Updated {
			w.touchSessionUpdated(tab, tab.main, sess.Time.Updated)
		}
		if needSync && !w.populating {
			w.syncSidebar()
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
	}
	if prevVisID != "" && w.tabs[prevVisID] != nil {
		w.viewStack.SetVisibleChildName(prevVisID)
	}
	w.populating = prevPopulating
	if !prevPopulating && prevVisID == "" {
		w.onVisibleTabChanged()
	}

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
	if !prevPopulating {
		w.syncSidebar()
	}
	return tab
}

func (w *Window) revealActiveSession(sessionID string) {
	if sessionID == "" || w.archivedSessions[sessionID] {
		return
	}
	if _, ok := w.tabs[sessionID]; ok {
		return
	}
	if _, ok := w.childToRoot[sessionID]; ok {
		return
	}
	for _, s := range w.allSessions {
		if s.ID == sessionID && s.ParentID == "" {
			if w.visibleSessions == nil {
				w.visibleSessions = make(map[string]bool)
			}
			if !w.visibleSessions[s.ID] {
				w.visibleSessions[s.ID] = true
				w.saveState()
			}
			wasEmpty := len(w.tabs) == 0
			w.ensureSessionTab(s)
			if wasEmpty && w.viewStack != nil {
				w.viewStack.SetVisibleChildName(s.ID)
				w.onVisibleTabChanged()
			} else {
				w.syncSidebar()
			}
			return
		}
	}
}

func (w *Window) selectSession(sess opencode.Session) {
	normDir := normalizeProjectDir(sess.Location.Directory)
	stateChanged := false
	if w.visibleSessions == nil {
		w.visibleSessions = make(map[string]bool)
	}
	if !w.visibleSessions[sess.ID] {
		w.visibleSessions[sess.ID] = true
		stateChanged = true
	}
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
	w.viewStack.SetVisibleChildName(sess.ID)
	w.onVisibleTabChanged()
	w.syncSidebar()
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

	if w.archivedSessions == nil {
		w.archivedSessions = make(map[string]bool)
	}
	w.archivedSessions[sessID] = true
	delete(w.visibleSessions, sessID)
	w.saveState()

	if w.projectSessions != nil {
		filtered := w.projectSessions[normDir][:0]
		for _, s := range w.projectSessions[normDir] {
			if s.ID != sessID && !w.archivedSessions[s.ID] {
				filtered = append(filtered, s)
			}
		}
		if len(filtered) == 0 {
			delete(w.projectSessions, normDir)
		} else {
			w.projectSessions[normDir] = filtered
		}

		// If this project is not expanded and now has fewer than maxVisibleSessions open tabs in w.tabs,
		// promote the next unarchived session from w.projectSessions[normDir] so up to maxVisibleSessions remain visible.
		if !w.expandedProjects[normDir] {
			maxVis := w.effectiveMaxVisibleSessions()
			openCount := 0
			for _, id := range w.tabOrder {
				if id != sessID && w.tabs[id] != nil && normalizeProjectDir(w.tabs[id].main.session.Location.Directory) == normDir {
					openCount++
				}
			}
			for _, candidate := range w.projectSessions[normDir] {
				if openCount >= maxVis {
					break
				}
				if _, exists := w.tabs[candidate.ID]; !exists {
					w.ensureSessionTab(candidate)
					openCount++
				}
			}
		}
	}

	if !ok {
		w.deleteSessionDraftsForTab(sessID, nil)
		w.withdrawSessionNotifications(sessID)
		w.syncSidebar()
		return
	}

	wasCurrent := (w.currentRootSessionID() == sessID)
	var nextID string
	if wasCurrent {
		nextID = w.nextVisibleTabID(sessID, normDir, nil)
	}

	prevPopulating := w.populating
	w.populating = true
	w.deleteSessionDraftsForTab(sessID, tab)
	w.cancelCompletionSettle(tab)
	w.withdrawSessionNotifications(sessID)
	tab.main.closeStreams()
	for _, sub := range tab.subTimelines {
		sub.closeStreams()
	}
	w.cleanupTabSidebarState(tab)
	if w.viewStack != nil && tab.stack != nil {
		w.viewStack.Remove(tab.stack)
	}
	delete(w.tabs, sessID)
	if idx := slices.Index(w.tabOrder, sessID); idx >= 0 {
		w.tabOrder = slices.Delete(w.tabOrder, idx, idx+1)
	}
	w.populating = prevPopulating

	if wasCurrent {
		if nextID != "" && w.viewStack != nil {
			w.viewStack.SetVisibleChildName(nextID)
		}
		w.onVisibleTabChanged()
	} else {
		w.updateWindowManagerTitle()
	}
	w.syncSidebar()
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
		delete(w.visibleSessions, id)
		if _, hasTab := w.tabs[id]; !hasTab {
			w.deleteSessionDraftsForTab(id, nil)
		}
	}
	delete(w.collapsedProjects, normDir)
	delete(w.expandedProjects, normDir)
	w.saveState()

	currentID := w.currentRootSessionID()
	wasInProject := toArchive[currentID]
	var nextID string
	if wasInProject {
		nextID = w.nextVisibleTabID(currentID, "", toArchive)
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
			w.deleteSessionDraftsForTab(id, tab)
			w.cancelCompletionSettle(tab)
			tab.main.closeStreams()
			for _, sub := range tab.subTimelines {
				sub.closeStreams()
			}
			w.cleanupTabSidebarState(tab)
			if w.viewStack != nil && tab.stack != nil {
				w.viewStack.Remove(tab.stack)
			}
			delete(w.tabs, id)
		}
		w.withdrawSessionNotifications(id)
	}
	w.tabOrder = nextOrder
	w.populating = prevPopulating

	if wasInProject && w.viewStack != nil {
		if nextID != "" {
			w.viewStack.SetVisibleChildName(nextID)
		}
		w.onVisibleTabChanged()
	} else {
		w.updateWindowManagerTitle()
	}
	w.syncSidebar()
}

func sessionUpdatedTime(sess opencode.Session) int64 {
	return cmp.Or(sess.Time.Updated, sess.Time.Created)
}

type sessionSearchMatch struct {
	sess      opencode.Session
	dir       string
	isProject bool
	row       *gtk.ListBoxRow
	spinner   *adw.Spinner
	active    bool
	score     int
	updatedAt int64
}

func (w *Window) isSessionActiveForSearch(sess opencode.Session, pool []opencode.Session, activeMap map[string]opencode.ActiveStatus) bool {
	if sess.ID == "" {
		return false
	}
	if tab, ok := w.tabs[sess.ID]; ok && tab != nil {
		if tabHasPendingInput(tab) {
			return false
		}
		return w.isTabAgentWorking(tab)
	}
	if sess.Time.Busy > 0 && sess.Time.Busy > sess.Time.Idle {
		return true
	}
	if _, ok := activeMap[sess.ID]; ok {
		return true
	}
	for _, s := range pool {
		if s.ParentID == sess.ID {
			if s.Time.Busy > 0 && s.Time.Busy > sess.Time.Idle {
				return true
			}
			if _, ok := activeMap[s.ID]; ok {
				return true
			}
		}
	}
	return false
}

func (w *Window) knownProjectDirs(pool []opencode.Session) []string {
	var allDirs []string
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
		if tab := w.tabs[id]; tab != nil && tab.main != nil {
			addDir(tab.main.session.Location.Directory)
		}
	}
	for _, s := range pool {
		addDir(s.Location.Directory)
	}
	for _, s := range w.allSessions {
		addDir(s.Location.Directory)
	}
	return allDirs
}

func matchFilesystemDirs(rawQuery, baseDir string) []string {
	q := strings.TrimSpace(rawQuery)
	if q == "" {
		return nil
	}
	looksLikePath := strings.HasPrefix(q, "/") ||
		strings.HasPrefix(q, "~/") ||
		q == "~" ||
		strings.HasPrefix(q, "./") ||
		strings.HasPrefix(q, "../") ||
		strings.Contains(q, "/")
	if !looksLikePath {
		return nil
	}

	expanded := q
	if expanded == "~" || strings.HasPrefix(expanded, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if expanded == "~" {
				expanded = home + "/"
			} else {
				expanded = filepath.Join(home, expanded[2:])
				if strings.HasSuffix(q, "/") {
					expanded += "/"
				}
			}
		}
	} else if !filepath.IsAbs(expanded) {
		base := strings.TrimSpace(baseDir)
		if base == "" {
			base, _ = os.Getwd()
		}
		if base != "" {
			hasTrailingSlash := strings.HasSuffix(expanded, "/")
			expanded = filepath.Join(base, expanded)
			if hasTrailingSlash {
				expanded += "/"
			}
		}
	}

	var dirs []string
	seen := make(map[string]bool)
	addDir := func(d string) {
		norm := normalizeProjectDir(d)
		if norm != "" && !seen[norm] {
			seen[norm] = true
			dirs = append(dirs, norm)
		}
	}

	cleaned := filepath.Clean(expanded)
	if info, err := os.Stat(cleaned); err == nil && info.IsDir() {
		addDir(cleaned)
	}

	scanDir := filepath.Dir(expanded)
	prefix := strings.ToLower(filepath.Base(expanded))
	if strings.HasSuffix(expanded, "/") {
		scanDir = filepath.Clean(expanded)
		prefix = ""
	}

	entries, err := os.ReadDir(scanDir)
	if err != nil {
		return dirs
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".") {
			continue
		}
		if prefix != "" {
			if _, ok := fuzzyMatchScore(prefix, name); !ok {
				continue
			}
		}
		fullPath := filepath.Join(scanDir, name)
		if entry.IsDir() {
			addDir(fullPath)
		} else if entry.Type()&os.ModeSymlink != 0 {
			if info, err := os.Stat(fullPath); err == nil && info.IsDir() {
				addDir(fullPath)
			}
		}
		if len(dirs) >= 16 {
			break
		}
	}
	return dirs
}

func (w *Window) matchAndSortSearchItems(pool []opencode.Session, query string, activeMap map[string]opencode.ActiveStatus) []sessionSearchMatch {
	trimmedQuery := strings.TrimSpace(query)
	sessions := w.matchAndSortSessionsWithActive(pool, query, activeMap)

	latestByDir := make(map[string]int64)
	for _, s := range pool {
		d := normalizeProjectDir(s.Location.Directory)
		if d == "" {
			continue
		}
		if t := sessionUpdatedTime(s); t > latestByDir[d] {
			latestByDir[d] = t
		}
	}

	seenDirs := make(map[string]bool)
	var candidateDirs []string
	for _, d := range w.knownProjectDirs(pool) {
		if !seenDirs[d] {
			seenDirs[d] = true
			candidateDirs = append(candidateDirs, d)
		}
	}
	if trimmedQuery != "" {
		baseDir := w.activeBaseDirectory(w.currentTab(), nil)
		for _, d := range matchFilesystemDirs(trimmedQuery, baseDir) {
			if !seenDirs[d] {
				seenDirs[d] = true
				candidateDirs = append(candidateDirs, d)
			}
		}
	}

	var projects []sessionSearchMatch
	for _, dir := range candidateDirs {
		score := 1
		if trimmedQuery != "" {
			candidate := fmt.Sprintf("%s %s %s", projectName(dir), shortDir(dir), dir)
			var ok bool
			score, ok = fuzzyMatchScore(trimmedQuery, candidate)
			if !ok {
				// If the user typed a filesystem path that resolved directly to dir, keep it with a high score.
				resolved := resolveTargetDirectory(w.activeBaseDirectory(w.currentTab(), nil), trimmedQuery)
				if resolved != "" && strings.HasPrefix(strings.ToLower(dir), strings.ToLower(resolved)) {
					score = 2200
					ok = true
				}
			}
			if !ok {
				continue
			}
		}
		projects = append(projects, sessionSearchMatch{
			dir:       dir,
			isProject: true,
			score:     score,
			updatedAt: latestByDir[dir],
		})
	}

	if trimmedQuery == "" {
		return append(sessions, projects...)
	}

	combined := make([]sessionSearchMatch, 0, len(projects)+len(sessions))
	combined = append(combined, projects...)
	combined = append(combined, sessions...)
	slices.SortStableFunc(combined, func(a, b sessionSearchMatch) int {
		if a.score != b.score {
			return cmp.Compare(b.score, a.score)
		}
		if a.isProject != b.isProject {
			if a.isProject {
				return -1
			}
			return 1
		}
		return cmp.Compare(b.updatedAt, a.updatedAt)
	})
	return combined
}

func (w *Window) matchAndSortSessions(pool []opencode.Session, query string) []sessionSearchMatch {
	return w.matchAndSortSessionsWithActive(pool, query, nil)
}

func (w *Window) matchAndSortSessionsWithActive(pool []opencode.Session, query string, activeMap map[string]opencode.ActiveStatus) []sessionSearchMatch {
	trimmedQuery := strings.TrimSpace(query)
	var matches []sessionSearchMatch
	for _, sess := range pool {
		if sess.ParentID != "" {
			continue
		}
		if tab, ok := w.tabs[sess.ID]; ok && tab != nil && tab.main != nil {
			if tUp := sessionUpdatedTime(tab.main.session); tUp > sessionUpdatedTime(sess) {
				sess.Time.Updated = tUp
			}
			if tab.main.session.Title != "" {
				sess.Title = tab.main.session.Title
			}
		}
		score := 1
		if trimmedQuery != "" {
			title := sess.Title
			dir := sess.Location.Directory
			candidate := fmt.Sprintf("%s %s %s %s", title, projectName(dir), shortDir(dir), dir)
			var ok bool
			score, ok = fuzzyMatchScore(trimmedQuery, candidate)
			if !ok {
				continue
			}
		}
		matches = append(matches, sessionSearchMatch{
			sess:      sess,
			active:    w.isSessionActiveForSearch(sess, pool, activeMap),
			score:     score,
			updatedAt: sessionUpdatedTime(sess),
		})
	}

	slices.SortStableFunc(matches, func(a, b sessionSearchMatch) int {
		if a.score != b.score {
			return cmp.Compare(b.score, a.score)
		}
		return cmp.Compare(sessionUpdatedTime(b.sess), sessionUpdatedTime(a.sess))
	})
	return matches
}

func (w *Window) openSessionSearchDialog() {
	searchWin := adw.NewWindow()
	searchWin.SetTitle("Search Sessions & Projects")
	searchWin.SetDefaultSize(560, 480)
	searchWin.SetTransientFor(&w.ApplicationWindow.Window)
	searchWin.SetModal(true)

	toolbar := adw.NewToolbarView()
	searchWin.SetContent(toolbar)

	header := adw.NewHeaderBar()
	header.SetShowEndTitleButtons(true)
	titleWidget := adw.NewWindowTitle("Search Sessions & Projects", "")
	header.SetTitleWidget(titleWidget)
	toolbar.AddTopBar(header)

	contentBox := gtk.NewBox(gtk.OrientationVertical, 8)
	contentBox.SetMarginTop(8)
	contentBox.SetMarginBottom(8)
	contentBox.SetMarginStart(12)
	contentBox.SetMarginEnd(12)
	toolbar.SetContent(contentBox)

	searchEntry := gtk.NewSearchEntry()
	searchEntry.SetPlaceholderText("Search sessions, projects, or paths (e.g. ~/Projects)…")
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
	emptySearchLabel := gtk.NewLabel("No matching sessions or projects")
	emptySearchLabel.AddCSSClass("dim-label")
	emptySearchLabel.SetMarginTop(24)
	emptySearchLabel.SetMarginBottom(24)
	listBox.SetPlaceholder(emptySearchLabel)
	scrolled.SetChild(listBox)

	folderBtn := gtk.NewButton()
	folderBtn.AddCSSClass("flat")
	folderBtn.AddCSSClass("glup-new-project-btn")
	folderBtnBox := gtk.NewBox(gtk.OrientationHorizontal, 8)
	folderBtnBox.SetHAlign(gtk.AlignCenter)
	folderBtnIcon := gtk.NewImageFromIconName("folder-visiting-symbolic")
	folderBtnLabel := gtk.NewLabel("Open Folder…")
	folderBtnBox.Append(folderBtnIcon)
	folderBtnBox.Append(folderBtnLabel)
	folderBtn.SetChild(folderBtnBox)
	folderBtn.ConnectClicked(func() {
		searchWin.Close()
		w.promptNewProjectFolder()
	})
	contentBox.Append(folderBtn)

	var sessionsLock sync.Mutex
	sessionPool := make([]opencode.Session, len(w.allSessions))
	copy(sessionPool, w.allSessions)
	var activeMap map[string]opencode.ActiveStatus

	var currentMatches []sessionSearchMatch

	activateMatch := func(m sessionSearchMatch) {
		searchWin.Close()
		if m.isProject {
			w.createNewSessionInDir(m.dir)
		} else {
			w.selectSession(m.sess)
		}
	}

	activateSelected := func() {
		sel := listBox.SelectedRow()
		if sel == nil && len(currentMatches) > 0 {
			sel = currentMatches[0].row
		}
		if sel != nil {
			for _, m := range currentMatches {
				if m.row != nil && m.row.Object.Native() == sel.Object.Native() {
					activateMatch(m)
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
				activateMatch(m)
				return
			}
		}
	})

	updateList := func() {
		sessionsLock.Lock()
		pool := make([]opencode.Session, len(sessionPool))
		copy(pool, sessionPool)
		act := activeMap
		sessionsLock.Unlock()

		query := searchEntry.Text()

		for _, prev := range currentMatches {
			if prev.row != nil {
				listBox.Remove(prev.row)
			}
		}
		currentMatches = nil

		matches := w.matchAndSortSearchItems(pool, query, act)

		for i, m := range matches {
			row := gtk.NewListBoxRow()

			rowBox := gtk.NewBox(gtk.OrientationHorizontal, 8)
			rowBox.SetMarginTop(10)
			rowBox.SetMarginBottom(10)
			rowBox.SetMarginStart(12)
			rowBox.SetMarginEnd(12)

			if m.isProject {
				folderIcon := gtk.NewImageFromIconName("folder-symbolic")
				folderIcon.AddCSSClass("glup-sidebar-folder-icon")
				folderIcon.SetVAlign(gtk.AlignCenter)
				rowBox.Append(folderIcon)

				textVBox := gtk.NewBox(gtk.OrientationVertical, 2)
				textVBox.SetHExpand(true)

				titleLabel := gtk.NewLabel(projectName(m.dir))
				titleLabel.SetXAlign(0)
				titleLabel.SetEllipsize(pango.EllipsizeMiddle)
				titleLabel.AddCSSClass("glup-sidebar-project-title")

				subLabel := gtk.NewLabel(shortDir(m.dir))
				subLabel.SetXAlign(0)
				subLabel.SetEllipsize(pango.EllipsizeMiddle)
				subLabel.AddCSSClass("caption")
				subLabel.AddCSSClass("dim-label")

				textVBox.Append(titleLabel)
				textVBox.Append(subLabel)
				rowBox.Append(textVBox)

				newBadge := gtk.NewLabel("New Session")
				newBadge.AddCSSClass("caption")
				newBadge.AddCSSClass("glup-search-archived-badge")
				newBadge.SetVAlign(gtk.AlignCenter)
				rowBox.Append(newBadge)
			} else {
				if m.active {
					spinner := adw.NewSpinner()
					spinner.AddCSSClass("glup-switcher-prefix")
					spinner.SetVAlign(gtk.AlignCenter)
					rowBox.Append(spinner)
					matches[i].spinner = spinner
				}

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

				if tm := formatRelativeTimemark(sessionUpdatedTime(m.sess)); tm != "" {
					timeLabel := gtk.NewLabel(tm)
					timeLabel.AddCSSClass("caption")
					timeLabel.AddCSSClass("dim-label")
					timeLabel.SetVAlign(gtk.AlignCenter)
					rowBox.Append(timeLabel)
				}
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
		var (
			sessions []opencode.Session
			active   map[string]opencode.ActiveStatus
			wg       sync.WaitGroup
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			sessions, _ = w.client.ListSessions(w.ctx, 200)
		}()
		go func() {
			defer wg.Done()
			active, _ = w.client.ActiveSessions(w.ctx)
		}()
		wg.Wait()
		if len(sessions) == 0 && len(active) == 0 {
			return func() {}
		}
		return func() {
			if len(active) > 0 {
				sessionsLock.Lock()
				activeMap = active
				sessionsLock.Unlock()
			}
			if len(sessions) > 0 {
				mergeSessions(sessions)
			}
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

func (w *Window) lookupCachedSessionTitle(sessionID string) string {
	if w == nil || sessionID == "" {
		return ""
	}
	if tab, ok := w.tabs[sessionID]; ok && tab.main != nil {
		if t := strings.TrimSpace(tab.main.session.Title); t != "" {
			return t
		}
	}
	for _, tab := range w.tabs {
		if sub, ok := tab.subTimelines[sessionID]; ok && sub != nil {
			if t := strings.TrimSpace(sub.session.Title); t != "" {
				return t
			}
		}
		for _, sub := range tab.subagents {
			if sub.ID == sessionID {
				if t := strings.TrimSpace(sub.Title); t != "" {
					return t
				}
			}
		}
	}
	for _, s := range w.allSessions {
		if s.ID == sessionID {
			if t := strings.TrimSpace(s.Title); t != "" {
				return t
			}
		}
	}
	for _, list := range w.projectSessions {
		for _, s := range list {
			if s.ID == sessionID {
				if t := strings.TrimSpace(s.Title); t != "" {
					return t
				}
			}
		}
	}
	if w.resolvedSessionTitles != nil {
		if t := strings.TrimSpace(w.resolvedSessionTitles[sessionID]); t != "" {
			return t
		}
	}
	return ""
}

func (w *Window) resolveSessionTitleAsync(sessionID string, onResolved func(title string)) string {
	if w == nil || sessionID == "" {
		return ""
	}
	if title := w.lookupCachedSessionTitle(sessionID); title != "" {
		return title
	}
	if w.client == nil || onResolved == nil {
		return ""
	}
	if w.resolvedSessionTitles != nil {
		if _, tried := w.resolvedSessionTitles[sessionID]; tried {
			return ""
		}
	}
	if w.resolvingSessions == nil {
		w.resolvingSessions = make(map[string][]func(string))
	}
	w.resolvingSessions[sessionID] = append(w.resolvingSessions[sessionID], onResolved)
	if len(w.resolvingSessions[sessionID]) > 1 {
		return ""
	}
	gtkutil.Async(w.ctx, func() func() {
		sess, err := w.client.GetSession(w.ctx, sessionID)
		return func() {
			callbacks := w.resolvingSessions[sessionID]
			delete(w.resolvingSessions, sessionID)
			if w.resolvedSessionTitles == nil {
				w.resolvedSessionTitles = make(map[string]string)
			}
			var title string
			if err == nil && sess != nil {
				title = strings.TrimSpace(sess.Title)
			}
			w.resolvedSessionTitles[sessionID] = title
			if title != "" {
				for _, cb := range callbacks {
					if cb != nil {
						cb(title)
					}
				}
			}
		}
	})
	return ""
}

func (w *Window) handleMarkdownLink(uri string) bool {
	if strings.HasPrefix(uri, mdstream.SessionLinkPrefix) {
		sessionID := strings.TrimPrefix(uri, mdstream.SessionLinkPrefix)
		w.openSessionByID(sessionID)
		return true
	}
	return false
}

func (w *Window) openSessionByID(sessionID string) {
	if w == nil || sessionID == "" {
		return
	}
	if tab, ok := w.tabs[sessionID]; ok && tab.main != nil {
		w.closeSubagent(tab)
		w.selectSession(tab.main.session)
		return
	}
	if rootID, ok := w.childToRoot[sessionID]; ok {
		if tab, ok := w.tabs[rootID]; ok && tab.main != nil {
			w.selectSession(tab.main.session)
			w.openSubagentByID(tab, sessionID)
			return
		}
	}
	for _, tab := range w.tabs {
		for _, sub := range tab.subagents {
			if sub.ID == sessionID {
				w.selectSession(tab.main.session)
				w.openSubagent(tab, sub)
				return
			}
		}
	}
	for _, s := range w.allSessions {
		if s.ID == sessionID {
			if s.ParentID == "" {
				w.selectSession(s)
				return
			}
			if parentTab, ok := w.tabs[s.ParentID]; ok && parentTab.main != nil {
				w.selectSession(parentTab.main.session)
				w.openSubagent(parentTab, s)
				return
			}
		}
	}
	if w.client == nil {
		return
	}
	gtkutil.Async(w.ctx, func() func() {
		sess, err := w.client.GetSession(w.ctx, sessionID)
		var parentSess *opencode.Session
		if err == nil && sess != nil && sess.ParentID != "" {
			parentSess, _ = w.client.GetSession(w.ctx, sess.ParentID)
		}
		return func() {
			if err != nil || sess == nil {
				slog.Error("failed to open session by ID", "sessionID", sessionID, "err", err)
				return
			}
			if w.resolvedSessionTitles != nil && strings.TrimSpace(sess.Title) != "" {
				w.resolvedSessionTitles[sess.ID] = strings.TrimSpace(sess.Title)
			}
			if sess.ParentID != "" {
				if parentSess != nil {
					w.selectSession(*parentSess)
				}
				if parentTab, ok := w.tabs[sess.ParentID]; ok {
					w.openSubagent(parentTab, *sess)
					return
				}
			}
			w.selectSession(*sess)
		}
	})
}

func (w *Window) newConvTimeline(sess opencode.Session) *convTimeline {
	timelineBox := gtk.NewBox(gtk.OrientationVertical, 0)
	timelineBox.AddCSSClass("glup-timeline-box")

	greeter := NewGreeterWidget(w.effectiveGreeterProfile(), func(prompt string) {
		w.populateComposerSuggestion(prompt)
	})
	greeter.SetVisible(false) // shown by updateGreeterVisibility once loaded and empty
	timelineBox.Append(greeter)

	clamp := adw.NewClamp()
	clamp.SetMaximumSize(w.effectiveChatWidth())
	clamp.SetTighteningThreshold(680)
	clamp.SetChild(timelineBox)

	scroll := autoscroll.NewWindow()
	scroll.SetVExpand(true)
	scroll.SetHExpand(true)
	scroll.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	scroll.AddCSSClass("glup-timeline-scroll")
	scroll.SetChild(clamp)

	thinkingLabel := gtk.NewLabel("Waiting for model...")
	thinkingLabel.SetXAlign(0)
	thinkingLabel.SetHAlign(gtk.AlignStart)
	thinkingLabel.AddCSSClass("dim-label")
	thinkingLabel.AddCSSClass("glup-thinking-label")
	thinkingLabel.SetVisible(false)
	timelineBox.Append(thinkingLabel)

	conv := &convTimeline{
		session:            sess,
		scroll:             scroll,
		clamp:              clamp,
		timelineBox:        timelineBox,
		greeter:            greeter,
		thinkingLabel:      thinkingLabel,
		thinkingAttached:   true,
		assistantMap:       make(map[string]*AssistantMessageWidget),
		compactionMap:      make(map[string]*CompactionWidget),
		messageWidgets:     make(map[string]gtk.Widgetter),
		formWidgets:        make(map[string]*QuestionFormWidget),
		permissionWidgets:  make(map[string]*PermissionPromptWidget),
		ignoredCancelIDs:   make(map[string]bool),
		seenMsgIDs:         make(map[string]bool),
		executingTools:     make(map[string]string),
		finishedTools:      make(map[string]bool),
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
		collapsed := w.rootContent.Collapsed()
		if !collapsed && !w.rootContent.ShowSidebar() {
			w.rootContent.SetShowSidebar(true)
		}
		if w.hideSidebarBtn != nil {
			w.hideSidebarBtn.SetVisible(collapsed)
		}
		if w.showSidebarBtn != nil {
			w.showSidebarBtn.SetVisible(collapsed)
		}
		if w.sidebarResizeHandle != nil {
			w.sidebarResizeHandle.SetVisible(!collapsed)
		}
		w.applySidebarWidth()
	}
}

func (w *Window) sidebarBreakpointWidth() int {
	width := cmp.Or(clampSidebarWidth(w.sidebarWidth), defaultSidebarWidth)
	return max(600, width*2)
}

func (w *Window) syncSidebarBreakpoint() {
	if w.sidebarBreakpoint == nil {
		return
	}
	w.sidebarBreakpoint.SetCondition(adw.BreakpointConditionParse(fmt.Sprintf("max-width: %dpx", w.sidebarBreakpointWidth())))
}

func (w *Window) applySidebarWidth() {
	if w.rootContent == nil {
		return
	}
	if w.rootContent.Collapsed() {
		w.rootContent.SetSidebarWidthUnit(adw.LengthUnitSp)
		w.rootContent.SetMinSidebarWidth(220)
		w.rootContent.SetMaxSidebarWidth(300)
		w.rootContent.SetSidebarWidthFraction(0.25)
		return
	}
	width := cmp.Or(clampSidebarWidth(w.sidebarWidth), defaultSidebarWidth)
	if totalW := w.rootContent.Width(); totalW > minSidebarWidth*2 {
		if maxW := (totalW - 1) / 2; width > maxW && maxW >= minSidebarWidth {
			width = maxW
		}
	}
	w.rootContent.SetSidebarWidthUnit(adw.LengthUnitPx)
	w.rootContent.SetMinSidebarWidth(float64(width))
	w.rootContent.SetMaxSidebarWidth(float64(width))
}

func (w *Window) setDesktopSidebarWidth(width int) {
	clamped := clampSidebarWidth(width)
	if clamped <= 0 {
		return
	}
	if w.rootContent != nil {
		if totalW := w.rootContent.Width(); totalW > minSidebarWidth*2 {
			if maxW := (totalW - 1) / 2; clamped > maxW && maxW >= minSidebarWidth {
				clamped = maxW
			}
		}
	}
	if clamped == w.sidebarWidth {
		return
	}
	w.sidebarWidth = clamped
	w.applySidebarWidth()
	if !w.resizingSidebar {
		w.syncSidebarBreakpoint()
	}
}

func (w *Window) setupSidebarResize() {
	if w.sidebarResizeHandle == nil || w.rootContent == nil {
		return
	}
	w.sidebarResizeHandle.SetCursorFromName("ew-resize")

	motion := gtk.NewEventControllerMotion()
	motion.ConnectEnter(func(_, _ float64) {
		w.resizerHovered = true
	})
	motion.ConnectLeave(func() {
		w.resizerHovered = false
	})
	w.sidebarResizeHandle.AddController(motion)

	drag := gtk.NewGestureDrag()
	drag.SetButton(1)
	drag.SetPropagationPhase(gtk.PhaseCapture)
	drag.ConnectDragBegin(func(startX, _ float64) {
		if w.rootContent.Collapsed() || !w.rootContent.ShowSidebar() {
			drag.SetState(gtk.EventSequenceDenied)
			w.resizingSidebar = false
			return
		}
		curW := 0
		if w.sidebarToolbar != nil {
			curW = w.sidebarToolbar.Width()
		}
		if !w.resizerHovered && (curW <= 0 || math.Abs(startX-float64(curW)) > 8) {
			drag.SetState(gtk.EventSequenceDenied)
			w.resizingSidebar = false
			return
		}
		if curW <= 0 {
			curW = cmp.Or(w.sidebarWidth, defaultSidebarWidth)
		}
		w.resizingSidebar = true
		w.sidebarDragStartWidth = curW
		w.sidebarResizeHandle.AddCSSClass("dragging")
		drag.SetState(gtk.EventSequenceClaimed)
	})
	drag.ConnectDragUpdate(func(offsetX, _ float64) {
		if !w.resizingSidebar {
			return
		}
		w.setDesktopSidebarWidth(w.sidebarDragStartWidth + int(math.Round(offsetX)))
	})
	drag.ConnectDragEnd(func(_, _ float64) {
		if !w.resizingSidebar {
			return
		}
		w.resizingSidebar = false
		w.sidebarResizeHandle.RemoveCSSClass("dragging")
		w.syncSidebarBreakpoint()
		w.saveState()
	})
	w.rootContent.AddController(drag)
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
		if conv == tab.main {
			tab.setTitle(newTitle)
		}
		if w.currentTab() == tab {
			w.refreshHeaderAndBottomBar(tab)
		}
		w.updateSwitcherIndicators()
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

type projectHeaderWidgets struct {
	outerBox      *gtk.Box
	sep           *gtk.Separator
	hBox          *gtk.Box
	titleLabel    *gtk.Label
	subtitleLabel *gtk.Label
	countLabel    *gtk.Label
	chevron       *gtk.Image
	newBtn        *gtk.Button
	popover       *gtk.Popover
	newSessBtn    *gtk.Button
	openFilesBtn  *gtk.Button
	copyPathBtn   *gtk.Button
	toggleBtn     *gtk.Button
	toggleLbl     *gtk.Label
	archiveBtn    *gtk.Button
	onNewSession  func()
	onOpenFiles   func()
	onCopyPath    func()
	onToggle      func()
	onArchive     func()
}

func (hw *projectHeaderWidgets) openMenuAt(x, y float64) {
	if hw == nil || hw.popover == nil {
		return
	}
	if x > 0 || y > 0 {
		px, py := float32(x), float32(y)
		if hw.hBox != nil && hw.outerBox != nil {
			if pt, ok := hw.hBox.ComputePoint(hw.outerBox, graphene.NewPointAlloc().Init(px, py)); ok {
				px, py = pt.X(), pt.Y()
			}
		}
		rect := gdk.NewRectangle(int(math.Round(float64(px))), int(math.Round(float64(py))), 1, 1)
		hw.popover.SetPointingTo(&rect)
	} else {
		hw.popover.SetPointingTo(nil)
	}
	if hw.outerBox != nil && hw.outerBox.Root() != nil {
		hw.popover.Popup()
	}
}

func (w *Window) setupAppMenuButton(menuBtn *gtk.MenuButton) {
	if menuBtn == nil {
		return
	}
	menuBox := gtk.NewBox(gtk.OrientationVertical, 2)
	menuBox.AddCSSClass("glup-popover-content")
	menuBox.AddCSSClass("glup-project-menu")

	popover := gtk.NewPopover()
	popover.SetHasArrow(false)
	popover.SetAutohide(true)
	popover.SetChild(menuBox)

	prefsBtn, _ := newProjectHeaderMenuButton("settings-symbolic", "Preferences")
	prefsBtn.ConnectClicked(func() {
		popover.Popdown()
		w.openPreferencesDialog()
	})
	menuBox.Append(prefsBtn)

	mcpBtn, _ := newProjectHeaderMenuButton("bolt-symbolic", "MCP Servers…")
	mcpBtn.ConnectClicked(func() {
		popover.Popdown()
		w.openMcpSheet("")
	})
	menuBox.Append(mcpBtn)

	aboutBtn, _ := newProjectHeaderMenuButton("help-about-symbolic", "About Gluppencode")
	aboutBtn.ConnectClicked(func() {
		popover.Popdown()
		w.openAboutDialog()
	})
	menuBox.Append(aboutBtn)

	menuBtn.SetPopover(popover)
}

func (w *Window) openAboutDialog() {
	if w.ApplicationWindow == nil {
		return
	}
	about := adw.NewAboutDialog()
	about.SetApplicationName("Gluppencode")
	about.SetApplicationIcon("so.libdb.gluppencode")
	about.SetDeveloperName("diamondburned")
	about.SetVersion("1.0")
	about.SetComments("A GTK4 and libadwaita desktop frontend for OpenCode")
	about.SetWebsite("https://github.com/diamondburned/gluppencode")
	about.SetIssueURL("https://github.com/diamondburned/gluppencode/issues")
	about.Present(w.ApplicationWindow)
}

func newProjectHeaderMenuButton(iconName, labelText string) (*gtk.Button, *gtk.Label) {
	btn := gtk.NewButton()
	btn.AddCSSClass("flat")
	btn.AddCSSClass("glup-project-menu-item")

	row := gtk.NewBox(gtk.OrientationHorizontal, 8)
	row.SetVAlign(gtk.AlignCenter)

	icon := gtk.NewImageFromIconName(iconName)
	icon.SetPixelSize(16)
	row.Append(icon)

	lbl := gtk.NewLabel(labelText)
	lbl.SetXAlign(0)
	lbl.SetHExpand(true)
	row.Append(lbl)

	btn.SetChild(row)
	return btn, lbl
}

func (w *Window) openProjectInFiles(rawDir string) {
	dir := normalizeProjectDir(rawDir)
	if dir == "" {
		return
	}
	if w.fileOpener != nil {
		w.fileOpener(dir)
		return
	}
	var parent *gtk.Window
	if w.ApplicationWindow != nil {
		parent = &w.Window
	}
	launcher := gtk.NewFileLauncher(gio.NewFileForPath(dir))
	launcher.Launch(w.ctx, parent, nil)
}

func (w *Window) copyTextToClipboard(text string) {
	if text == "" {
		return
	}
	if w.clipboardSetter != nil {
		w.clipboardSetter(text)
		return
	}
	if d := gdk.DisplayGetDefault(); d != nil {
		d.Clipboard().SetText(text)
	}
}

func (w *Window) launchExternalURI(rawURI string, onError func(error)) {
	cleanURI, err := opencode.ValidateExternalAuthURL(rawURI)
	if err != nil {
		if onError != nil {
			onError(err)
		}
		return
	}
	if w.uriLauncher != nil {
		if err := w.uriLauncher(cleanURI); err != nil && onError != nil {
			onError(err)
		}
		return
	}
	var parent *gtk.Window
	if w.ApplicationWindow != nil {
		parent = &w.Window
	}
	launcher := gtk.NewURILauncher(cleanURI)
	launcher.Launch(w.ctx, parent, func(res gio.AsyncResulter) {
		if err := launcher.LaunchFinish(res); err != nil && onError != nil {
			onError(err)
		}
	})
}

func (w *Window) newProjectHeaderBox(rawDir string, includeSeparator bool) *gtk.Box {
	if w.headerProjectDir == nil {
		w.headerProjectDir = make(map[uintptr]string)
	}
	if w.headerWidgets == nil {
		w.headerWidgets = make(map[uintptr]*projectHeaderWidgets)
	}
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

	sep := gtk.NewSeparator(gtk.OrientationHorizontal)
	sep.SetMarginTop(4)
	sep.SetMarginBottom(6)
	sep.SetMarginStart(6)
	sep.SetMarginEnd(2)
	sep.SetVisible(includeSeparator)
	outerBox.Append(sep)

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

	onNewSession := func() {
		if d := w.headerProjectDir[ptr]; d != "" {
			w.createNewSessionInDir(d)
		}
	}
	newBtn := gtk.NewButtonFromIconName("list-add-symbolic")
	newBtn.AddCSSClass("flat")
	newBtn.AddCSSClass("circular")
	newBtn.AddCSSClass("glup-sidebar-close-btn")
	newBtn.AddCSSClass("glup-sidebar-project-add-btn")
	newBtn.SetTooltipText("New Session in Project")
	newBtn.SetVAlign(gtk.AlignCenter)
	newBtn.ConnectClicked(onNewSession)
	hBox.Append(newBtn)

	menuBox := gtk.NewBox(gtk.OrientationVertical, 2)
	menuBox.AddCSSClass("glup-popover-content")
	menuBox.AddCSSClass("glup-project-menu")

	popover := gtk.NewPopover()
	popover.SetHasArrow(true)
	popover.SetAutohide(true)
	popover.SetPosition(gtk.PosBottom)
	popover.SetChild(menuBox)
	popover.SetParent(outerBox)
	outerBox.ConnectDestroy(func() {
		if popover.Parent() != nil {
			popover.Unparent()
		}
	})

	newSessBtn, _ := newProjectHeaderMenuButton("list-add-symbolic", "New Session")
	newSessBtn.ConnectClicked(func() {
		popover.Popdown()
		onNewSession()
	})
	menuBox.Append(newSessBtn)

	onOpenFiles := func() {
		popover.Popdown()
		if d := w.headerProjectDir[ptr]; d != "" {
			w.openProjectInFiles(d)
		}
	}
	openFilesBtn, _ := newProjectHeaderMenuButton("folder-visiting-symbolic", "Open in Files")
	openFilesBtn.ConnectClicked(onOpenFiles)
	menuBox.Append(openFilesBtn)

	onCopyPath := func() {
		popover.Popdown()
		if d := w.headerProjectDir[ptr]; d != "" {
			w.copyTextToClipboard(d)
		}
	}
	copyPathBtn, _ := newProjectHeaderMenuButton("edit-copy-symbolic", "Copy Path")
	copyPathBtn.ConnectClicked(onCopyPath)
	menuBox.Append(copyPathBtn)

	toggleText := "Collapse Project"
	if w.collapsedProjects[normDir] {
		toggleText = "Expand Project"
	}
	onToggle := func() {
		popover.Popdown()
		if d := w.headerProjectDir[ptr]; d != "" {
			w.toggleProjectCollapsed(d)
		}
	}
	toggleBtn, toggleLbl := newProjectHeaderMenuButton("chevron-right-symbolic", toggleText)
	toggleBtn.ConnectClicked(onToggle)
	menuBox.Append(toggleBtn)

	menuSep := gtk.NewSeparator(gtk.OrientationHorizontal)
	menuSep.SetMarginTop(3)
	menuSep.SetMarginBottom(3)
	menuBox.Append(menuSep)

	onArchive := func() {
		popover.Popdown()
		if d := w.headerProjectDir[ptr]; d != "" {
			w.clearProjectSessions(d)
		}
	}
	archiveBtn, _ := newProjectHeaderMenuButton("window-close-symbolic", "Archive Project")
	archiveBtn.AddCSSClass("glup-project-menu-item-danger")
	archiveBtn.ConnectClicked(onArchive)
	menuBox.Append(archiveBtn)

	hw := &projectHeaderWidgets{
		outerBox:      outerBox,
		sep:           sep,
		hBox:          hBox,
		titleLabel:    titleLabel,
		subtitleLabel: subtitleLabel,
		countLabel:    countLabel,
		chevron:       chevron,
		newBtn:        newBtn,
		popover:       popover,
		newSessBtn:    newSessBtn,
		openFilesBtn:  openFilesBtn,
		copyPathBtn:   copyPathBtn,
		toggleBtn:     toggleBtn,
		toggleLbl:     toggleLbl,
		archiveBtn:    archiveBtn,
		onNewSession:  onNewSession,
		onOpenFiles:   onOpenFiles,
		onCopyPath:    onCopyPath,
		onToggle:      onToggle,
		onArchive:     onArchive,
	}
	w.headerWidgets[ptr] = hw

	outerBox.Append(hBox)

	var suppressNextRelease bool
	click := gtk.NewGestureClick()
	click.SetButton(1)
	click.ConnectPressed(func(_ int, _, _ float64) {
		suppressNextRelease = false
	})
	click.ConnectReleased(func(_ int, x, y float64) {
		if suppressNextRelease {
			suppressNextRelease = false
			return
		}
		if picked := hBox.Pick(x, y, gtk.PickDefault); picked != nil {
			for p := gtk.Widgetter(picked); p != nil; p = gtk.BaseWidget(p).Parent() {
				if gtk.BaseWidget(p).Object.Native() == newBtn.Object.Native() {
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

	rightClick := gtk.NewGestureClick()
	rightClick.SetButton(3)
	rightClick.ConnectPressed(func(_ int, x, y float64) {
		rightClick.SetState(gtk.EventSequenceClaimed)
		hw.openMenuAt(x, y)
	})
	hBox.AddController(rightClick)

	longPress := gtk.NewGestureLongPress()
	longPress.SetTouchOnly(false)
	longPress.ConnectPressed(func(x, y float64) {
		suppressNextRelease = true
		longPress.SetState(gtk.EventSequenceClaimed)
		hw.openMenuAt(x, y)
	})
	hBox.AddController(longPress)

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
	if w.headerProjectDir == nil {
		w.headerProjectDir = make(map[uintptr]string)
	}
	normDir := normalizeProjectDir(rawDir)
	ptr := base.Object.Native()
	w.headerProjectDir[ptr] = normDir
	collapsed := w.collapsedProjects[normDir]
	total := len(w.projectSessions[normDir])

	if hw, ok := w.headerWidgets[ptr]; ok && hw != nil {
		hw.titleLabel.SetText(projectName(rawDir))
		hw.subtitleLabel.SetText(shortDir(rawDir))
		if collapsed {
			hw.chevron.RemoveCSSClass("glup-sidebar-project-chevron-expanded")
			hw.toggleLbl.SetText("Expand Project")
		} else {
			hw.chevron.AddCSSClass("glup-sidebar-project-chevron-expanded")
			hw.toggleLbl.SetText("Collapse Project")
		}
		if collapsed && total > 0 {
			hw.countLabel.SetText(fmt.Sprintf("%d", total))
			hw.countLabel.SetVisible(true)
		} else {
			hw.countLabel.SetVisible(false)
		}
		return
	}

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

func (w *Window) setProjectHeaderSeparator(outerBox *gtk.Box, includeSeparator bool) {
	if outerBox == nil {
		return
	}
	if includeSeparator {
		outerBox.SetMarginTop(2)
	} else {
		outerBox.SetMarginTop(6)
	}
	if hw := w.headerWidgets[outerBox.Object.Native()]; hw != nil && hw.sep != nil {
		hw.sep.SetVisible(includeSeparator)
	}
}

func (w *Window) toggleProjectCollapsed(normDir string) {
	if normDir == "" {
		return
	}
	w.ensureSidebarMaps()
	if w.collapsedProjects[normDir] {
		delete(w.collapsedProjects, normDir)
	} else {
		w.collapsedProjects[normDir] = true
	}
	w.saveState()
	w.syncSidebar()
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
	w.syncSidebar()
}

func (w *Window) compareTabProjects(idA, idB string) int {
	if idA == idB {
		return 0
	}
	tabA := w.tabs[idA]
	tabB := w.tabs[idB]
	okA := tabA != nil && tabA.main != nil
	okB := tabB != nil && tabB.main != nil
	if !okA && !okB {
		return 0
	}
	if !okA {
		return 1
	}
	if !okB {
		return -1
	}
	dirA := normalizeProjectDir(tabA.main.session.Location.Directory)
	dirB := normalizeProjectDir(tabB.main.session.Location.Directory)
	if dirA != dirB {
		return w.compareProjectOrder(dirA, dirB)
	}
	return 0
}

func (w *Window) reorderTabsByProject() bool {
	if len(w.tabOrder) <= 1 || slices.IsSortedFunc(w.tabOrder, w.compareTabProjects) {
		return false
	}
	oldOrder := slices.Clone(w.tabOrder)
	slices.SortStableFunc(w.tabOrder, w.compareTabProjects)
	return !slices.Equal(oldOrder, w.tabOrder)
}

func (w *Window) bumpSessionToTop(tab *sessionTab) {
	if tab == nil || tab.main == nil {
		return
	}
	w.touchSessionUpdated(tab, tab.main, time.Now().UnixMilli())
	w.reorderTabsByProject()
	sessID := tab.main.session.ID
	currIdx := slices.Index(w.tabOrder, sessID)
	if currIdx < 0 {
		return
	}
	targetDir := normalizeProjectDir(tab.main.session.Location.Directory)
	firstInProj := currIdx
	for i, id := range w.tabOrder {
		t := w.tabs[id]
		if t == nil || t.main == nil {
			continue
		}
		if normalizeProjectDir(t.main.session.Location.Directory) == targetDir {
			firstInProj = i
			break
		}
	}
	if currIdx > firstInProj {
		copy(w.tabOrder[firstInProj+1:currIdx+1], w.tabOrder[firstInProj:currIdx])
		w.tabOrder[firstInProj] = sessID
	}
	w.syncSidebar()
}

const defaultCompletionSettleDelay = 1500 * time.Millisecond

func (w *Window) effectiveCompletionSettleDelay() time.Duration {
	if w != nil && w.completionSettleDelay > 0 {
		return w.completionSettleDelay
	}
	return defaultCompletionSettleDelay
}

func (w *Window) markTabNeedsAttention(tab *sessionTab, forceBump bool) {
	if tab == nil || tab.main == nil || tab.main.session.ID == w.currentRootSessionID() {
		return
	}
	wasAttention := tab.NeedsAttention()
	if !tab.finishedAttention {
		tab.finishedAttention = true
		tab.syncNeedsAttention()
		w.updateWindowManagerTitle()
		w.updateSwitcherIndicators()
	}
	if !wasAttention || forceBump {
		w.bumpSessionToTop(tab)
	}
}

func (w *Window) markTabActionAttention(tab *sessionTab, forceBump bool) {
	if tab == nil || tab.main == nil || tab.main.session.ID == w.currentRootSessionID() {
		return
	}
	wasAttention := tab.NeedsAttention()
	if !tab.actionAttention {
		tab.actionAttention = true
		tab.syncNeedsAttention()
		w.updateWindowManagerTitle()
		w.updateSwitcherIndicators()
	}
	if !wasAttention || forceBump {
		w.bumpSessionToTop(tab)
	}
}

func (w *Window) refreshTabActionAttention(tab *sessionTab) {
	if w == nil || tab == nil {
		return
	}
	if !tabHasPendingInput(tab) && tab.actionAttention {
		tab.actionAttention = false
		tab.syncNeedsAttention()
		w.updateWindowManagerTitle()
	}
	w.updateSwitcherIndicators()
}

func (w *Window) resetRootTurn(tab *sessionTab) {
	if tab == nil {
		return
	}
	tab.turnSeq++
	tab.turnSettled = false
	tab.turnHasReply = false
	tab.turnFailed = false
	tab.turnReplyMsgID = ""
	tab.turnReplyPreview = ""
	if tab.turnReplyByMsgID != nil {
		clear(tab.turnReplyByMsgID)
	}
}

func (w *Window) recordRootReplyDelta(tab *sessionTab, msgID, delta string) {
	if tab == nil || delta == "" {
		return
	}
	if strings.TrimSpace(delta) != "" {
		tab.turnHasReply = true
	}
	if msgID != "" {
		if tab.turnReplyByMsgID == nil {
			tab.turnReplyByMsgID = make(map[string]string)
		}
		tab.turnReplyByMsgID[msgID] += delta
		tab.turnReplyMsgID = msgID
		tab.turnReplyPreview = tab.turnReplyByMsgID[msgID]
	} else {
		tab.turnReplyPreview += delta
	}
}

func (w *Window) recordRootReplyEnded(tab *sessionTab, msgID, text string) {
	if tab == nil || strings.TrimSpace(text) == "" {
		return
	}
	tab.turnHasReply = true
	if msgID != "" {
		if tab.turnReplyByMsgID == nil {
			tab.turnReplyByMsgID = make(map[string]string)
		}
		tab.turnReplyByMsgID[msgID] = text
		tab.turnReplyMsgID = msgID
	}
	tab.turnReplyPreview = text
}

func (w *Window) cancelCompletionSettle(tab *sessionTab) {
	if tab == nil {
		return
	}
	tab.completionSettleGen++
	if tab.completionTimer != nil {
		tab.completionTimer.Stop()
		tab.completionTimer = nil
	}
}

func (w *Window) cancelAllCompletionSettles() {
	if w == nil {
		return
	}
	for _, tab := range w.tabs {
		w.cancelCompletionSettle(tab)
	}
}

func (w *Window) hasRunningSubagentLocal(tab *sessionTab) bool {
	if tab == nil {
		return false
	}
	for _, sub := range tab.subagents {
		if sub.IsRunning() {
			return true
		}
	}
	for _, subConv := range tab.subTimelines {
		if subConv != nil && subConv.active {
			return true
		}
	}
	return false
}

func (w *Window) canCompleteRootTurnLocal(tab *sessionTab) bool {
	if tab == nil || tab.main == nil {
		return false
	}
	if tab.main.active {
		return false
	}
	if !tab.turnHasReply || tab.turnFailed || tab.turnSettled {
		return false
	}
	if tabHasPendingInput(tab) {
		return false
	}
	if w.hasRunningSubagentLocal(tab) {
		return false
	}
	return true
}

func (w *Window) maybeScheduleRootTurnCompletion(tab *sessionTab) {
	if w == nil || !w.canCompleteRootTurnLocal(tab) {
		return
	}
	if tab.completionTimer != nil {
		tab.completionTimer.Stop()
		tab.completionTimer = nil
	}
	tab.completionSettleGen++
	gen := tab.completionSettleGen
	turnSeq := tab.turnSeq
	rootID := tab.main.session.ID
	delay := w.effectiveCompletionSettleDelay()

	tab.completionTimer = time.AfterFunc(delay, func() {
		glib.IdleAdd(func() {
			if w.ctx != nil && w.ctx.Err() != nil {
				return
			}
			curTab := w.tabs[rootID]
			if curTab == nil || curTab != tab {
				return
			}
			if curTab.completionSettleGen != gen || curTab.turnSeq != turnSeq {
				return
			}
			curTab.completionTimer = nil
			if !w.canCompleteRootTurnLocal(curTab) {
				return
			}
			w.verifyAndFinalizeRootTurn(curTab, gen, turnSeq)
		})
	})
}

func (w *Window) verifyAndFinalizeRootTurn(tab *sessionTab, gen, turnSeq uint64) {
	if w == nil || tab == nil || tab.main == nil {
		return
	}
	if w.client == nil {
		w.finalizeRootTurnCompletion(tab, gen, turnSeq)
		return
	}
	rootID := tab.main.session.ID
	childIDs := make(map[string]bool)
	for _, sub := range tab.subagents {
		if sub.ID != "" {
			childIDs[sub.ID] = true
		}
	}
	for id := range tab.subTimelines {
		if id != "" {
			childIDs[id] = true
		}
	}
	for cid, rid := range w.childToRoot {
		if rid == rootID && cid != "" {
			childIDs[cid] = true
		}
	}

	ctx := w.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	gtkutil.Async(ctx, func() func() {
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		var (
			jobs      []opencode.JobInfo
			jobsErr   error
			active    map[string]opencode.ActiveStatus
			activeErr error
			wg        sync.WaitGroup
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			jobs, jobsErr = w.client.ListJobs(checkCtx, opencode.ListJobsOptions{
				SessionID:   rootID,
				Descendants: true,
			})
		}()
		go func() {
			defer wg.Done()
			active, activeErr = w.client.ActiveSessions(checkCtx)
		}()
		wg.Wait()

		return func() {
			curTab := w.tabs[rootID]
			if curTab == nil || curTab != tab {
				return
			}
			if curTab.completionSettleGen != gen || curTab.turnSeq != turnSeq {
				return
			}
			if !w.canCompleteRootTurnLocal(curTab) {
				return
			}
			if jobsErr == nil {
				for _, job := range jobs {
					if job.Kind == "shell" || job.Status == "cancelled" {
						continue
					}
					return
				}
			}
			if activeErr == nil && active != nil {
				if _, ok := active[rootID]; ok {
					return
				}
				for cid := range childIDs {
					if _, ok := active[cid]; ok {
						return
					}
				}
			}
			w.finalizeRootTurnCompletion(curTab, gen, turnSeq)
		}
	})
}

func (w *Window) finalizeRootTurnCompletion(tab *sessionTab, gen, turnSeq uint64) {
	if w == nil || tab == nil || tab.main == nil {
		return
	}
	if tab.completionSettleGen != gen || tab.turnSeq != turnSeq {
		return
	}
	if !w.canCompleteRootTurnLocal(tab) {
		return
	}
	tab.turnSettled = true
	rootID := tab.main.session.ID
	if rootID != w.currentRootSessionID() {
		w.markTabNeedsAttention(tab, false)
	}
	w.notifySessionFinished(tab)
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
	w.updateSwitcherIndicators()
}

func (w *Window) expandProjectSessions(dir string) {
	w.ensureSidebarMaps()
	dir = normalizeProjectDir(dir)
	w.expandedProjects[dir] = true
	all := w.projectSessions[dir]
	var pending []opencode.Session
	for _, sess := range all {
		if _, exists := w.tabs[sess.ID]; !exists {
			pending = append(pending, sess)
		}
	}
	if len(pending) == 0 {
		w.syncSidebar()
		return
	}

	const chunkSize = 15
	var step func(idx int)
	step = func(idx int) {
		end := idx + chunkSize
		if end > len(pending) {
			end = len(pending)
		}
		prevPopulating := w.populating
		w.populating = true
		for i := idx; i < end; i++ {
			w.ensureSessionTab(pending[i])
		}
		w.populating = prevPopulating
		w.syncSidebar()
		if end < len(pending) {
			glib.IdleAdd(func() bool {
				step(end)
				return false
			})
		}
	}
	step(0)
}

func (w *Window) ensureSidebarMaps() {
	if w.expandedProjects == nil {
		w.expandedProjects = make(map[string]bool)
	}
	if w.collapsedProjects == nil {
		w.collapsedProjects = make(map[string]bool)
	}
	if w.closeBtnSessID == nil {
		w.closeBtnSessID = make(map[uintptr]string)
	}
	if w.hoveredRow == nil {
		w.hoveredRow = make(map[uintptr]bool)
	}
	if w.rowProjectDir == nil {
		w.rowProjectDir = make(map[uintptr]string)
	}
	if w.sidebarRowSessID == nil {
		w.sidebarRowSessID = make(map[uintptr]string)
	}
	if w.headerProjectDir == nil {
		w.headerProjectDir = make(map[uintptr]string)
	}
	if w.headerWidgets == nil {
		w.headerWidgets = make(map[uintptr]*projectHeaderWidgets)
	}
	if w.projectHeaderRows == nil {
		w.projectHeaderRows = make(map[string]*gtk.ListBoxRow)
	}
	if w.projectShowMoreRows == nil {
		w.projectShowMoreRows = make(map[string]*gtk.ListBoxRow)
	}
	if w.projectShowMoreLbls == nil {
		w.projectShowMoreLbls = make(map[string]*gtk.Label)
	}
	if w.projectShowMoreDirs == nil {
		w.projectShowMoreDirs = make(map[uintptr]string)
	}
}

func (w *Window) setupSidebarList() {
	if w.sidebarList == nil || w.sidebarSetupDone {
		return
	}
	w.sidebarSetupDone = true
	w.ensureSidebarMaps()

	w.sidebarList.ConnectRowSelected(func(row *gtk.ListBoxRow) {
		if w.syncingSidebarSelect || w.populating || row == nil || w.viewStack == nil {
			return
		}
		sessID := w.sidebarRowSessID[row.Object.Native()]
		if sessID == "" {
			return
		}
		if tab := w.tabs[sessID]; tab != nil {
			if w.currentRootSessionID() != sessID {
				w.viewStack.SetVisibleChildName(sessID)
			}
		}
	})

	w.sidebarList.ConnectRowActivated(func(row *gtk.ListBoxRow) {
		if row == nil {
			return
		}
		rp := row.Object.Native()
		if dir := w.projectShowMoreDirs[rp]; dir != "" {
			w.expandProjectSessions(dir)
			return
		}
		if sessID := w.sidebarRowSessID[rp]; sessID != "" {
			if tab := w.tabs[sessID]; tab != nil {
				if tab.activeSubID != "" {
					w.closeSubagent(tab)
				}
				if w.viewStack != nil && w.currentRootSessionID() != sessID {
					w.viewStack.SetVisibleChildName(sessID)
				}
				if w.rootContent != nil && w.rootContent.Collapsed() {
					w.rootContent.SetShowSidebar(false)
				}
			}
		}
	})
}

func (w *Window) attachProjectDropTarget(row *gtk.ListBoxRow) {
	if row == nil {
		return
	}
	rp := row.Object.Native()
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

func (w *Window) buildConversationRow(sessID string, tab *sessionTab) *switcherIndicator {
	w.ensureSidebarMaps()

	row := gtk.NewListBoxRow()
	row.AddCSSClass("glup-sidebar-conversation-row")
	row.SetSelectable(true)
	row.SetActivatable(true)

	hBox := gtk.NewBox(gtk.OrientationHorizontal, 6)
	hBox.AddCSSClass("glup-sidebar-conversation-box")
	hBox.SetVAlign(gtk.AlignCenter)

	spinner := adw.NewSpinner()
	spinner.AddCSSClass("glup-switcher-prefix")
	spinner.SetVAlign(gtk.AlignCenter)
	spinner.SetVisible(false)
	hBox.Append(spinner)

	questionIcon := gtk.NewImageFromIconName("dialog-question-symbolic")
	questionIcon.SetPixelSize(16)
	questionIcon.AddCSSClass("glup-switcher-prefix")
	questionIcon.AddCSSClass("glup-sidebar-question-icon")
	questionIcon.SetTooltipText("Waiting for your input")
	questionIcon.SetVAlign(gtk.AlignCenter)
	questionIcon.SetVisible(false)
	hBox.Append(questionIcon)

	shellIcon := gtk.NewImageFromIconName("terminal-symbolic")
	shellIcon.SetPixelSize(16)
	shellIcon.AddCSSClass("glup-switcher-prefix")
	shellIcon.AddCSSClass("glup-sidebar-shell-icon")
	shellIcon.SetTooltipText("Background shell running")
	shellIcon.SetVAlign(gtk.AlignCenter)
	shellIcon.SetVisible(false)
	hBox.Append(shellIcon)

	titleLbl := gtk.NewLabel(tab.Title())
	titleLbl.AddCSSClass("glup-sidebar-conversation-title")
	titleLbl.SetXAlign(0)
	titleLbl.SetHExpand(true)
	titleLbl.SetSingleLineMode(true)
	titleLbl.SetEllipsize(pango.EllipsizeEnd)
	hBox.Append(titleLbl)

	dot := gtk.NewBox(gtk.OrientationHorizontal, 0)
	dot.AddCSSClass("glup-sidebar-attention-dot")
	dot.SetVAlign(gtk.AlignCenter)
	dot.SetHAlign(gtk.AlignCenter)
	dot.SetVisible(false)
	hBox.Append(dot)

	suffixBox := gtk.NewBox(gtk.OrientationHorizontal, 4)
	suffixBox.AddCSSClass("glup-switcher-suffix")
	suffixBox.SetHAlign(gtk.AlignEnd)
	suffixBox.SetVAlign(gtk.AlignCenter)
	suffixBox.SetHExpand(false)

	label := gtk.NewLabel("")
	label.AddCSSClass("caption")
	label.AddCSSClass("dim-label")
	label.SetVisible(false)
	suffixBox.Append(label)

	closeBtn := gtk.NewButtonFromIconName("window-close-symbolic")
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
	suffixBox.Append(closeBtn)

	hBox.Append(suffixBox)
	row.SetChild(hBox)

	rp := row.Object.Native()
	w.sidebarRowSessID[rp] = sessID

	motion := gtk.NewEventControllerMotion()
	motion.ConnectEnter(func(_, _ float64) {
		w.hoveredRow[rp] = true
		w.updateSwitcherIndicators()
	})
	motion.ConnectLeave(func() {
		delete(w.hoveredRow, rp)
		w.updateSwitcherIndicators()
	})
	row.AddController(motion)
	w.attachProjectDropTarget(row)

	return &switcherIndicator{
		row:          row,
		hBox:         hBox,
		spinner:      spinner,
		questionIcon: questionIcon,
		shellIcon:    shellIcon,
		dot:          dot,
		titleLbl:     titleLbl,
		box:          suffixBox,
		label:        label,
		closeBtn:     closeBtn,
		rowPtr:       rp,
	}
}

func (w *Window) ensureProjectHeaderRow(rawDir string, includeSeparator bool) *gtk.ListBoxRow {
	w.ensureSidebarMaps()
	normDir := normalizeProjectDir(rawDir)
	if row := w.projectHeaderRows[normDir]; row != nil {
		if child := row.Child(); child != nil {
			if box, ok := gtk.BaseWidget(child).Cast().(*gtk.Box); ok {
				w.setProjectHeaderSeparator(box, includeSeparator)
			}
			w.updateProjectHeaderLabels(child, rawDir)
		}
		w.rowProjectDir[row.Object.Native()] = normDir
		return row
	}

	row := gtk.NewListBoxRow()
	row.AddCSSClass("glup-sidebar-header-row")
	row.SetSelectable(false)
	row.SetActivatable(false)
	row.SetFocusable(false)

	hBox := w.newProjectHeaderBox(rawDir, includeSeparator)
	w.updateProjectHeaderLabels(hBox, rawDir)
	tooltip := fmt.Sprintf("%s (%s) — click to collapse/expand, right-click or hold for options, drag to reorder", projectName(rawDir), shortDir(rawDir))
	hBox.SetTooltipText(tooltip)
	row.SetChild(hBox)

	w.rowProjectDir[row.Object.Native()] = normDir
	w.projectHeaderRows[normDir] = row
	return row
}

func (w *Window) ensureProjectShowMoreRow(dir string, hiddenCount int) *gtk.ListBoxRow {
	w.ensureSidebarMaps()
	normDir := normalizeProjectDir(dir)
	text := fmt.Sprintf("Show %d more…", hiddenCount)
	if row := w.projectShowMoreRows[normDir]; row != nil {
		if lbl := w.projectShowMoreLbls[normDir]; lbl != nil && lbl.Text() != text {
			lbl.SetText(text)
		}
		rp := row.Object.Native()
		w.projectShowMoreDirs[rp] = normDir
		w.rowProjectDir[rp] = normDir
		return row
	}

	row := gtk.NewListBoxRow()
	row.AddCSSClass("glup-sidebar-show-more-row")
	row.SetSelectable(false)
	row.SetActivatable(true)

	lbl := gtk.NewLabel(text)
	lbl.AddCSSClass("caption")
	lbl.AddCSSClass("dim-label")
	lbl.SetXAlign(0)
	lbl.SetHAlign(gtk.AlignStart)
	row.SetChild(lbl)

	rp := row.Object.Native()
	w.projectShowMoreRows[normDir] = row
	w.projectShowMoreLbls[normDir] = lbl
	w.projectShowMoreDirs[rp] = normDir
	w.rowProjectDir[rp] = normDir
	w.attachProjectDropTarget(row)
	return row
}

func (w *Window) retireProjectHeader(dir string) {
	row := w.projectHeaderRows[dir]
	if row == nil {
		return
	}
	delete(w.projectHeaderRows, dir)
	rp := row.Object.Native()
	delete(w.rowProjectDir, rp)
	delete(w.hoveredRow, rp)

	var hw *projectHeaderWidgets
	if child := row.Child(); child != nil {
		ptr := gtk.BaseWidget(child).Object.Native()
		hw = w.headerWidgets[ptr]
		delete(w.headerWidgets, ptr)
		delete(w.headerProjectDir, ptr)
	}
	if hw != nil && hw.popover != nil {
		hw.popover.Popdown()
	}
	glib.IdleAdd(func() {
		if hw != nil && hw.popover != nil && hw.popover.Parent() != nil {
			hw.popover.Unparent()
		}
		if row.Parent() == nil {
			row.SetChild(nil)
		}
	})
}

func (w *Window) retireProjectShowMore(dir string) {
	row := w.projectShowMoreRows[dir]
	if row == nil {
		return
	}
	delete(w.projectShowMoreRows, dir)
	delete(w.projectShowMoreLbls, dir)
	rp := row.Object.Native()
	delete(w.projectShowMoreDirs, rp)
	delete(w.rowProjectDir, rp)
	delete(w.hoveredRow, rp)
	if row.Parent() == nil {
		row.SetChild(nil)
	}
}

func (w *Window) syncSidebar() {
	if w == nil {
		return
	}
	w.reorderTabsByProject()
	if w.sidebarList == nil {
		return
	}
	w.ensureSidebarMaps()
	w.setupSidebarList()

	currentID := w.currentRootSessionID()
	maxVis := w.effectiveMaxVisibleSessions()

	type projectGroup struct {
		dir    string
		rawDir string
		tabs   []*sessionTab
	}
	var groups []*projectGroup
	groupMap := make(map[string]*projectGroup)
	seenTabs := make(map[string]bool, len(w.tabOrder))

	for _, id := range w.tabOrder {
		if id == "" || seenTabs[id] {
			continue
		}
		tab := w.tabs[id]
		if tab == nil || tab.main == nil {
			continue
		}
		seenTabs[id] = true
		rawDir := tab.main.session.Location.Directory
		normDir := normalizeProjectDir(rawDir)
		g := groupMap[normDir]
		if g == nil {
			g = &projectGroup{dir: normDir, rawDir: rawDir}
			groupMap[normDir] = g
			groups = append(groups, g)
		}
		g.tabs = append(g.tabs, tab)
	}

	var desiredRows []*gtk.ListBoxRow
	desiredHeaderDirs := make(map[string]bool, len(groups))
	desiredShowMoreDirs := make(map[string]bool)
	desiredSet := make(map[uintptr]bool)
	var activeRow *gtk.ListBoxRow

	for gIdx, g := range groups {
		desiredHeaderDirs[g.dir] = true
		hdrRow := w.ensureProjectHeaderRow(g.rawDir, gIdx > 0)
		desiredRows = append(desiredRows, hdrRow)
		desiredSet[hdrRow.Object.Native()] = true

		if w.collapsedProjects[g.dir] {
			continue
		}

		expanded := w.expandedProjects[g.dir]
		visibleCount := 0
		for i, tab := range g.tabs {
			sessID := tab.main.session.ID
			if !expanded && i >= maxVis && sessID != currentID && !w.isTabAgentWorking(tab) {
				continue
			}
			visibleCount++
			if tab.sidebarIndicator == nil || tab.sidebarIndicator.row == nil {
				tab.sidebarIndicator = w.buildConversationRow(sessID, tab)
			}
			row := tab.sidebarIndicator.row
			rp := row.Object.Native()
			w.sidebarRowSessID[rp] = sessID
			w.rowProjectDir[rp] = g.dir
			desiredRows = append(desiredRows, row)
			desiredSet[rp] = true
			if sessID == currentID {
				activeRow = row
			}
		}

		if !expanded {
			total := len(w.projectSessions[g.dir])
			if len(g.tabs) > total {
				total = len(g.tabs)
			}
			if hidden := total - visibleCount; hidden > 0 {
				desiredShowMoreDirs[g.dir] = true
				moreRow := w.ensureProjectShowMoreRow(g.dir, hidden)
				desiredRows = append(desiredRows, moreRow)
				desiredSet[moreRow.Object.Native()] = true
			}
		}
	}

	unselectIfSelected := func(row *gtk.ListBoxRow) {
		if row != nil && row.IsSelected() {
			w.sidebarList.UnselectRow(row)
		}
	}

	prevSyncing := w.syncingSidebarSelect
	w.syncingSidebarSelect = true

	// Pass 1: remove rows no longer in desiredRows.
	var toRemove []*gtk.ListBoxRow
	for i := 0; ; i++ {
		row := w.sidebarList.RowAtIndex(i)
		if row == nil {
			break
		}
		if !desiredSet[row.Object.Native()] {
			toRemove = append(toRemove, row)
		}
	}
	for _, row := range toRemove {
		delete(w.hoveredRow, row.Object.Native())
		unselectIfSelected(row)
		w.sidebarList.Remove(row)
	}

	// Pass 2: align remaining/new rows to desiredRows order.
	for i, desired := range desiredRows {
		cur := w.sidebarList.RowAtIndex(i)
		if cur != nil && cur.Object.Native() == desired.Object.Native() {
			continue
		}
		if desired.Parent() != nil {
			delete(w.hoveredRow, desired.Object.Native())
			if child := desired.Child(); child != nil {
				if hw := w.headerWidgets[gtk.BaseWidget(child).Object.Native()]; hw != nil && hw.popover != nil {
					hw.popover.Popdown()
				}
			}
			unselectIfSelected(desired)
			w.sidebarList.Remove(desired)
		}
		w.sidebarList.Insert(desired, i)
	}

	if activeRow != nil {
		if !activeRow.IsSelected() {
			w.sidebarList.SelectRow(activeRow)
		}
	} else {
		w.sidebarList.UnselectAll()
	}

	w.syncingSidebarSelect = prevSyncing

	// Retire unused headers and show-more rows.
	for dir := range w.projectHeaderRows {
		if !desiredHeaderDirs[dir] {
			w.retireProjectHeader(dir)
		}
	}
	for dir := range w.projectShowMoreRows {
		if !desiredShowMoreDirs[dir] {
			w.retireProjectShowMore(dir)
		}
	}

	w.updateSwitcherIndicators()
}

func (w *Window) isTabAgentWorking(tab *sessionTab) bool {
	if tab == nil || tabHasPendingInput(tab) {
		return false
	}
	if tab.main != nil && tab.main.active {
		return true
	}
	for _, sub := range tab.subagents {
		if sub.IsRunning() {
			return true
		}
	}
	for _, subConv := range tab.subTimelines {
		if subConv != nil && subConv.active {
			return true
		}
	}
	return false
}

func (w *Window) isTabWorking(tab *sessionTab) bool {
	if tab == nil {
		return false
	}
	return w.isTabAgentWorking(tab) || len(w.visibleBackgroundShellsForTab(tab)) > 0
}

func (w *Window) updateIndicator(ind *switcherIndicator, sessID string, tab *sessionTab) {
	if ind == nil || tab == nil {
		return
	}
	if ind.closeBtn != nil {
		w.ensureSidebarMaps()
		w.closeBtnSessID[ind.closeBtn.Object.Native()] = sessID
	}
	focused := (sessID == w.currentRootSessionID())
	hasPendingInput := tabHasPendingInput(tab)
	agentWorking := !hasPendingInput && w.isTabAgentWorking(tab)
	hasBgShells := !hasPendingInput && !agentWorking && len(w.visibleBackgroundShellsForTab(tab)) > 0
	hovered := ind.rowPtr != 0 && w.hoveredRow[ind.rowPtr]
	showClose := ind.closeBtn != nil && (hovered || focused)
	if ind.closeBtn != nil {
		ind.closeBtn.SetVisible(showClose)
	}
	if ind.spinner != nil {
		ind.spinner.SetVisible(agentWorking)
	}
	if ind.questionIcon != nil {
		ind.questionIcon.SetVisible(hasPendingInput)
	}
	if ind.shellIcon != nil {
		ind.shellIcon.SetVisible(hasBgShells)
	}
	if ind.titleLbl != nil {
		title := tab.Title()
		if ind.titleLbl.Text() != title {
			ind.titleLbl.SetText(title)
		}
	}
	if ind.dot != nil {
		ind.dot.SetVisible(tab.finishedAttention && !focused)
	}
	if ind.label == nil || ind.box == nil {
		return
	}

	switch {
	case showClose:
		ind.label.SetVisible(false)
		ind.box.SetVisible(true)
	case focused:
		ind.label.SetVisible(false)
		ind.box.SetVisible(false)
	default:
		var updatedMs int64
		if tab.main != nil {
			updatedMs = tab.main.session.Time.Updated
		}
		timemark := formatRelativeTimemark(updatedMs)
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
	if len(w.mcpLoadedDirs) > 0 {
		clear(w.mcpLoadedDirs)
	}
	// 1. Fetch sessions, active sessions, and running subagent jobs immediately in parallel
	gtkutil.Async(w.ctx, func() func() {
		var (
			sessions []opencode.Session
			sessErr  error
			active   map[string]opencode.ActiveStatus
			jobs     []opencode.JobInfo
			wg       sync.WaitGroup
		)
		wg.Add(3)
		go func() {
			defer wg.Done()
			sessions, sessErr = w.client.ListSessions(w.ctx, 200)
		}()
		go func() {
			defer wg.Done()
			active, _ = w.client.ActiveSessions(w.ctx)
		}()
		go func() {
			defer wg.Done()
			jobs, _ = w.client.ListJobs(w.ctx, opencode.ListJobsOptions{
				Kind:   "subagent",
				Status: "running",
			})
		}()
		wg.Wait()

		var childSessions []opencode.Session
		if sessErr == nil {
			sessions, childSessions = resolveStartupActiveChildren(w.ctx, w.client, sessions, active, jobs)
		}

		return func() {
			if sessErr != nil {
				if w.ctx.Err() == nil {
					slog.Error("failed to list sessions", "err", sessErr)
					w.setOfflineError(sessErr)
				}
				return
			}
			w.setOfflineError(nil)
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
			tabsSessions := sessions
			if len(childSessions) > 0 {
				tabsSessions = make([]opencode.Session, 0, len(sessions)+len(childSessions))
				tabsSessions = append(tabsSessions, sessions...)
				tabsSessions = append(tabsSessions, childSessions...)
			}
			w.populateSessionTabs(tabsSessions, active)
		}
	})

	// 2. Fetch metadata (agents, models, commands, skills, shells) in parallel
	gtkutil.Async(w.ctx, func() func() {
		var (
			agents   []opencode.Agent
			models   []opencode.ModelInfo
			commands []opencode.CommandInfo
			skills   []opencode.SkillInfo
			wg       sync.WaitGroup
		)
		wg.Add(4)
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
			w.refreshShells()
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
				if cur := w.currentTab(); cur != nil {
					w.updateBottomBar(cur)
				} else {
					w.updateComposerAgentModel(w.lastSelectedAgent, w.lastSelectedModel)
				}
			}
			w.refreshGreeterProfileAsync()
		}
	})
}

func resolveStartupActiveChildren(
	ctx context.Context,
	client *opencode.Client,
	rootSessions []opencode.Session,
	active map[string]opencode.ActiveStatus,
	jobs []opencode.JobInfo,
) ([]opencode.Session, []opencode.Session) {
	knownRoots := make(map[string]bool, len(rootSessions))
	for _, s := range rootSessions {
		if s.ID != "" && s.ParentID == "" {
			knownRoots[s.ID] = true
		}
	}

	var candidateIDs []string
	candidateSeen := make(map[string]bool)
	addCandidate := func(id string) {
		if id == "" || knownRoots[id] || candidateSeen[id] {
			return
		}
		candidateSeen[id] = true
		candidateIDs = append(candidateIDs, id)
	}
	for id := range active {
		addCandidate(id)
	}
	jobByChild := make(map[string]opencode.JobInfo)
	for _, job := range jobs {
		if job.Status != "" && job.Status != "running" {
			continue
		}
		if job.ChildSessionID != "" {
			jobByChild[job.ChildSessionID] = job
			addCandidate(job.ChildSessionID)
		}
	}

	if len(candidateIDs) == 0 {
		return rootSessions, nil
	}

	fetched := make(map[string]opencode.Session)
	if client != nil {
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		for _, id := range candidateIDs {
			wg.Add(1)
			go func(startID string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				curID := startID
				visited := make(map[string]bool)
				for range 8 {
					if curID == "" || knownRoots[curID] || visited[curID] {
						break
					}
					visited[curID] = true

					mu.Lock()
					existing, ok := fetched[curID]
					mu.Unlock()
					if ok {
						curID = existing.ParentID
						continue
					}

					sess, err := client.GetSession(ctx, curID)
					if err != nil || sess == nil || sess.ID == "" {
						break
					}
					mu.Lock()
					fetched[sess.ID] = *sess
					mu.Unlock()
					curID = sess.ParentID
				}
			}(id)
		}
		wg.Wait()
	}

	var childSessions []opencode.Session
	extraRootSeen := make(map[string]bool)
	childSeen := make(map[string]bool)
	for _, s := range fetched {
		if s.ParentID == "" {
			if !knownRoots[s.ID] && !extraRootSeen[s.ID] {
				extraRootSeen[s.ID] = true
				rootSessions = append(rootSessions, s)
			}
			continue
		}
		if !childSeen[s.ID] {
			childSeen[s.ID] = true
			childSessions = append(childSessions, s)
		}
	}
	for childID, job := range jobByChild {
		if knownRoots[childID] || childSeen[childID] || job.SessionID == "" {
			continue
		}
		childSeen[childID] = true
		childSessions = append(childSessions, opencode.Session{
			ID:       childID,
			ParentID: job.SessionID,
			Title:    cmp.Or(job.Title, job.Description),
			Agent:    job.Agent,
		})
	}
	return rootSessions, childSessions
}

func (w *Window) populateSessionTabs(sessions []opencode.Session, active map[string]opencode.ActiveStatus) {
	prevSelected := w.currentRootSessionID()
	w.populating = true
	if w.visibleSessions == nil {
		w.visibleSessions = make(map[string]bool)
	}
	if w.archivedSessions == nil {
		w.archivedSessions = make(map[string]bool)
	}

	parentByID := make(map[string]string, len(sessions))
	for _, s := range sessions {
		if s.ID != "" && s.ParentID != "" {
			parentByID[s.ID] = s.ParentID
		}
	}
	resolveRootID := func(id string) string {
		cur := id
		for range 16 {
			p := parentByID[cur]
			if p == "" {
				return cur
			}
			cur = p
		}
		return cur
	}

	activeRootIDs := make(map[string]bool)
	childrenByRoot := make(map[string][]opencode.Session)
	childSeen := make(map[string]bool)
	for _, s := range sessions {
		if s.ParentID != "" && s.ID != "" && !childSeen[s.ID] {
			childSeen[s.ID] = true
			rootID := resolveRootID(s.ID)
			if _, ok := active[s.ID]; ok {
				if s.Outcome == "" && !s.IsRunning() {
					s.Time.Idle = 0
				}
				activeRootIDs[rootID] = true
			} else if s.IsRunning() {
				activeRootIDs[rootID] = true
			}
			childrenByRoot[rootID] = append(childrenByRoot[rootID], s)
		}
	}

	var topSessions []opencode.Session
	var discoveredDirs, awakeDirs []string
	seenIDs := make(map[string]bool, len(sessions))
	stateChanged := false
	for _, s := range sessions {
		if s.ParentID != "" || s.ID == "" || seenIDs[s.ID] {
			continue
		}
		seenIDs[s.ID] = true
		if s.Location.Directory != "" {
			discoveredDirs = append(discoveredDirs, s.Location.Directory)
		}
		_, isRunning := active[s.ID]
		isRunning = isRunning || (s.Time.Busy > 0 && s.Time.Busy > s.Time.Idle) || activeRootIDs[s.ID]
		// Idle, unvisited and archived sessions are suspended: their
		// locations are not polled for pending forms and permissions.
		if s.Location.Directory != "" && (isRunning ||
			(!w.archivedSessions[s.ID] && (s.ID == prevSelected || w.recentlyTouched(s.ID)))) {
			awakeDirs = append(awakeDirs, s.Location.Directory)
		}
		if isRunning && !w.archivedSessions[s.ID] {
			if !w.visibleSessions[s.ID] {
				w.visibleSessions[s.ID] = true
				stateChanged = true
			}
		}
		if w.archivedSessions[s.ID] && s.ID != prevSelected {
			continue
		}
		if !w.visibleSessions[s.ID] && s.ID != prevSelected {
			continue
		}
		topSessions = append(topSessions, s)
	}
	if w.ensureProjectOrder(discoveredDirs...) {
		stateChanged = true
	}
	if stateChanged {
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
		w.cancelCompletionSettle(tab)
		tab.main.closeStreams()
		for _, sub := range tab.subTimelines {
			sub.closeStreams()
		}
		w.cleanupTabSidebarState(tab)
		w.viewStack.Remove(tab.stack)
	}
	w.tabs = make(map[string]*sessionTab)
	w.childToRoot = make(map[string]string)
	w.tabOrder = make([]string, 0, len(topSessions))

	projCount := make(map[string]int)
	maxVis := w.effectiveMaxVisibleSessions()
	var firstID, firstUncollapsedID string
	for _, sess := range topSessions {
		dir := normalizeProjectDir(sess.Location.Directory)
		count := projCount[dir]
		_, isRunning := active[sess.ID]
		hasActiveChild := activeRootIDs[sess.ID]
		if count < maxVis || sess.ID == prevSelected || isRunning || hasActiveChild || w.expandedProjects[dir] {
			if firstID == "" {
				firstID = sess.ID
			}
			if firstUncollapsedID == "" && !w.collapsedProjects[dir] {
				firstUncollapsedID = sess.ID
			}
			tab := w.addSessionTab(sess)
			w.tabOrder = append(w.tabOrder, sess.ID)
			if isRunning {
				w.setConvActive(sess.ID, true)
			}
			for _, child := range childrenByRoot[sess.ID] {
				w.childToRoot[child.ID] = sess.ID
				tab.subagents = append(tab.subagents, child)
			}
			projCount[dir] = count + 1
		}
	}

	pendingOpenID := w.notifyCoord.consumePendingOpenSessionID()
	targetID := prevSelected
	if pendingOpenID != "" {
		if _, ok := w.tabs[pendingOpenID]; ok {
			targetID = pendingOpenID
			pendingOpenID = ""
		}
	}
	if _, ok := w.tabs[targetID]; !ok {
		if firstUncollapsedID != "" {
			targetID = firstUncollapsedID
		} else {
			targetID = firstID
		}
	}
	w.populating = false
	if targetID != "" {
		w.viewStack.SetVisibleChildName(targetID)
	}
	w.pruneArchivedDrafts()
	w.onVisibleTabChanged()
	w.syncSidebar()
	if pendingOpenID != "" {
		w.openSessionByID(pendingOpenID)
	}

	glib.IdleAdd(func() {
		w.syncSidebar()
		if w.inputView != nil {
			w.inputView.GrabFocus()
		}
	})
	if len(awakeDirs) > 0 {
		w.loadPendingFormsForAllDirs(awakeDirs)
	}
}

func (w *Window) loadPendingFormsForAllDirs(dirs []string) {
	if w.client == nil {
		return
	}
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
			allPerms []opencode.PermissionRequest
			wg       sync.WaitGroup
		)
		for _, dir := range uniqueDirs {
			d := dir
			wg.Add(2)
			go func() {
				defer wg.Done()
				forms, err := w.client.ListForms(w.ctx, d)
				if err == nil && len(forms) > 0 {
					mu.Lock()
					allForms = append(allForms, forms...)
					mu.Unlock()
				}
			}()
			go func() {
				defer wg.Done()
				perms, err := w.client.ListPermissions(w.ctx, d)
				if err == nil && len(perms) > 0 {
					mu.Lock()
					allPerms = append(allPerms, perms...)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		return func() {
			for _, f := range allForms {
				w.upsertPendingForm(f, false)
			}
			for _, p := range allPerms {
				w.upsertPendingPermission(p, false)
			}
		}
	})
}

// TakeChargeDirective is the autonomous decision prompt sent as the answer to
// every pending question when "Take charge" mode is active.
const TakeChargeDirective = "The user is currently away (Take Charge mode is active). Proceed autonomously by choosing the most recommended or sensible option for this question. Record this question, the option you selected, and your reasoning, and report all autonomous choices made at the end of your work."

func buildTakeChargeAnswer(form opencode.FormInfo) map[string]any {
	return buildTakeChargeAnswerWithDirective(form, TakeChargeDirective)
}

func buildTakeChargeAnswerWithDirective(form opencode.FormInfo, directive string) map[string]any {
	if strings.TrimSpace(directive) == "" {
		directive = TakeChargeDirective
	}
	if len(form.Fields) == 0 {
		return map[string]any{"q0": directive}
	}
	ans := make(map[string]any, len(form.Fields))
	for _, f := range form.Fields {
		if f.Key == "" {
			continue
		}
		switch f.Type {
		case "multiselect":
			ans[f.Key] = []string{directive}
		case "boolean":
			ans[f.Key] = true
		default:
			ans[f.Key] = directive
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
		strings.Contains(msg, "PermissionAlreadySettledError") ||
		strings.Contains(msg, "PermissionNotFoundError") ||
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
			w.takeChargeBtn.SetTooltipText("Take Charge: Active — questions and permissions are auto-answered while away")
		} else {
			w.takeChargeBtn.SetTooltipText("Take Charge: Off — click to auto-answer questions and permissions while away")
		}
	}
	if !active {
		return
	}
	var pending []opencode.FormInfo
	var pendingPerms []opencode.PermissionRequest
	for _, tab := range w.tabs {
		if tab.main != nil {
			pending = append(pending, tab.main.pendingForms...)
			pendingPerms = append(pendingPerms, tab.main.pendingPermissions...)
		}
		for _, sub := range tab.subTimelines {
			pending = append(pending, sub.pendingForms...)
			pendingPerms = append(pendingPerms, sub.pendingPermissions...)
		}
	}
	for _, form := range pending {
		w.replyFormTakeCharge(form)
	}
	for _, perm := range pendingPerms {
		w.replyPermissionTakeCharge(perm)
	}
	if dirs := w.awakeTabDirs(); w.client != nil && len(dirs) > 0 {
		w.loadPendingFormsForAllDirs(dirs)
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
	directive := w.effectiveTakeChargeDirective()
	ans := buildTakeChargeAnswerWithDirective(form, directive)

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
			err = w.client.CancelForm(ctx, sessID, formID, directive)
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

func (w *Window) resolveOrEnsureConv(sessionID string, wake bool) (*sessionTab, *convTimeline) {
	var tab *sessionTab
	var conv *convTimeline
	if wake {
		tab, conv = w.resolveEventConv(sessionID)
	} else {
		tab, conv = w.resolveConv(sessionID)
	}
	if tab != nil && conv == nil {
		if rootID, ok := w.childToRoot[sessionID]; ok && rootID != "" {
			conv = w.ensureSubagentConv(tab, sessionID)
		}
	}
	return tab, conv
}

func (w *Window) upsertPendingForm(form opencode.FormInfo, live bool) {
	if form.ID == "" || form.SessionID == "" {
		return
	}
	if w.takeCharge {
		w.replyFormTakeCharge(form)
		if w.client != nil {
			return
		}
	}
	tab, conv := w.resolveOrEnsureConv(form.SessionID, live)
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
	isNew := !found
	if isNew {
		conv.pendingForms = append(conv.pendingForms, form)
	}
	if live {
		w.cancelCompletionSettle(tab)
		w.markTabActionAttention(tab, isNew)
		if isNew {
			w.notifyFormActionNeeded(tab, form)
		}
	}
	w.updateSwitcherIndicators()
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
	tab, conv := w.resolveConv(sessionID)
	if conv == nil {
		// Search across all tabs if sessionID wasn't resolved directly.
		for _, t := range w.tabs {
			if t == nil {
				continue
			}
			if t.main != nil {
				if _, ok := t.main.formWidgets[formID]; ok {
					tab, conv = t, t.main
					break
				}
				for _, f := range t.main.pendingForms {
					if f.ID == formID {
						tab, conv = t, t.main
						break
					}
				}
				if conv != nil {
					break
				}
			}
			for _, sub := range t.subTimelines {
				if sub == nil {
					continue
				}
				if _, ok := sub.formWidgets[formID]; ok {
					tab, conv = t, sub
					break
				}
				for _, f := range sub.pendingForms {
					if f.ID == formID {
						tab, conv = t, sub
						break
					}
				}
				if conv != nil {
					break
				}
			}
			if conv != nil {
				break
			}
		}
	}
	var rootID string
	if tab != nil && tab.main != nil {
		rootID = tab.main.session.ID
	}
	w.withdrawActionNotification(rootID, formNotificationID(formID))
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
	w.refreshTabActionAttention(tab)
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
						if isFormSettledOrMissingError(err) {
							w.removePendingForm(sessID, formID)
							w.reconcileTail(sessID)
							return
						}
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
						if isFormSettledOrMissingError(err) {
							w.removePendingForm(sessID, formID)
							w.reconcileTail(sessID)
							return
						}
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

func (w *Window) replyPermissionTakeCharge(perm opencode.PermissionRequest) {
	if perm.ID == "" || perm.SessionID == "" || w.client == nil {
		return
	}
	if w.takeChargeInFlight == nil {
		w.takeChargeInFlight = make(map[string]bool)
	}
	if w.takeChargeInFlight[perm.ID] {
		return
	}
	w.takeChargeInFlight[perm.ID] = true

	sessID := perm.SessionID
	reqID := perm.ID

	var pw *PermissionPromptWidget
	if _, conv := w.resolveConv(sessID); conv != nil {
		if existing, ok := conv.permissionWidgets[reqID]; ok {
			pw = existing
			pw.SetBusy(true)
		}
	}

	ctx := w.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	gtkutil.Async(ctx, func() func() {
		err := w.client.ReplyPermission(ctx, sessID, reqID, opencode.PermissionOnce, "")
		return func() {
			delete(w.takeChargeInFlight, reqID)
			if err != nil && !isFormSettledOrMissingError(err) {
				slog.Error("failed to auto-reply to permission in take charge mode", "sessionID", sessID, "requestID", reqID, "err", err)
				if pw != nil {
					pw.SetBusy(false)
				}
				return
			}
			w.removePendingPermission(sessID, reqID)
		}
	})
}

func (w *Window) upsertPendingPermission(perm opencode.PermissionRequest, live bool) {
	if perm.ID == "" || perm.SessionID == "" {
		return
	}
	if w.takeCharge {
		w.replyPermissionTakeCharge(perm)
		if w.client != nil {
			return
		}
	}
	tab, conv := w.resolveOrEnsureConv(perm.SessionID, live)
	if tab == nil || conv == nil {
		return
	}
	found := false
	for i, existing := range conv.pendingPermissions {
		if existing.ID == perm.ID {
			conv.pendingPermissions[i] = perm
			found = true
			break
		}
	}
	isNew := !found
	if isNew {
		conv.pendingPermissions = append(conv.pendingPermissions, perm)
	}
	if live {
		w.cancelCompletionSettle(tab)
		w.markTabActionAttention(tab, isNew)
		if isNew {
			w.notifyPermissionActionNeeded(tab, perm)
		}
	}
	w.updateSwitcherIndicators()
	if conv.loaded {
		w.ensurePermissionWidget(tab, conv, perm)
		conv.keepPendingAtBottom()
		if conv.scroll != nil {
			conv.scroll.ScrollToBottomIfBottomed()
		}
	}
}

func (w *Window) removePendingPermission(sessionID, requestID string) {
	if requestID == "" {
		return
	}
	tab, conv := w.resolveConv(sessionID)
	if conv == nil {
		for _, t := range w.tabs {
			if t == nil {
				continue
			}
			if t.main != nil {
				if _, ok := t.main.permissionWidgets[requestID]; ok {
					tab, conv = t, t.main
					break
				}
				for _, p := range t.main.pendingPermissions {
					if p.ID == requestID {
						tab, conv = t, t.main
						break
					}
				}
				if conv != nil {
					break
				}
			}
			for _, sub := range t.subTimelines {
				if sub == nil {
					continue
				}
				if _, ok := sub.permissionWidgets[requestID]; ok {
					tab, conv = t, sub
					break
				}
				for _, p := range sub.pendingPermissions {
					if p.ID == requestID {
						tab, conv = t, sub
						break
					}
				}
				if conv != nil {
					break
				}
			}
			if conv != nil {
				break
			}
		}
	}
	var rootID string
	if tab != nil && tab.main != nil {
		rootID = tab.main.session.ID
	}
	w.withdrawActionNotification(rootID, permNotificationID(requestID))
	if conv == nil {
		return
	}
	next := conv.pendingPermissions[:0]
	for _, p := range conv.pendingPermissions {
		if p.ID != requestID {
			next = append(next, p)
		}
	}
	conv.pendingPermissions = next
	if pw, ok := conv.permissionWidgets[requestID]; ok {
		delete(conv.permissionWidgets, requestID)
		if conv.timelineBox != nil {
			conv.timelineBox.Remove(pw)
		}
	}
	conv.keepPendingAtBottom()
	w.refreshTabActionAttention(tab)
}

func (w *Window) ensurePermissionWidget(tab *sessionTab, conv *convTimeline, perm opencode.PermissionRequest) *PermissionPromptWidget {
	dir := conv.session.Location.Directory
	if dir == "" && tab != nil && tab.main != nil {
		dir = tab.main.session.Location.Directory
	}
	if pw, ok := conv.permissionWidgets[perm.ID]; ok {
		pw.Update(perm, dir)
		return pw
	}
	sessID := conv.session.ID
	reqID := perm.ID
	var pw *PermissionPromptWidget
	pw = NewPermissionPromptWidget(
		perm,
		dir,
		func(decision opencode.PermissionDecision) {
			gtkutil.Async(w.ctx, func() func() {
				err := w.client.ReplyPermission(w.ctx, sessID, reqID, decision, "")
				return func() {
					if err != nil {
						if isFormSettledOrMissingError(err) {
							w.removePendingPermission(sessID, reqID)
							w.reconcileTail(sessID)
							return
						}
						slog.Error("failed to reply to permission", "sessionID", sessID, "requestID", reqID, "err", err)
						pw.SetBusy(false)
						app.Error(w.ctx, err)
						return
					}
					w.removePendingPermission(sessID, reqID)
				}
			})
		},
	)
	conv.permissionWidgets[perm.ID] = pw
	if conv.timelineBox != nil {
		conv.timelineBox.Append(pw)
	}
	return pw
}

func (w *Window) addSessionTab(sess opencode.Session) *sessionTab {
	stack := gtk.NewStack()
	stack.SetTransitionType(gtk.StackTransitionTypeNone)
	stack.SetHhomogeneous(false)
	stack.SetVhomogeneous(false)
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

	tab := &sessionTab{
		page:         page,
		stack:        stack,
		main:         mainConv,
		title:        title,
		subTimelines: make(map[string]*convTimeline),
	}
	w.tabs[sess.ID] = tab
	return tab
}

func (w *Window) onVisibleTabChanged() {
	if w.populating {
		return
	}
	w.noteVisibleRoot()
	w.syncEmptyGreeterVisibility()
	tab := w.currentTab()
	if tab == nil {
		w.switchComposerDraft(greeterDraftKey)
		if w.windowTitle != nil {
			w.windowTitle.SetTitle("Gluppencode")
			w.windowTitle.SetSubtitle("Select or create a session")
		}
		if w.sheetBackToParentBtn != nil {
			w.sheetBackToParentBtn.SetVisible(false)
		}
		w.syncNavigationChrome()
		w.updateBusyState(false)
		w.updateBottomBar(nil)
		w.rebuildSubagentList(nil)
		w.rebuildShellList()
		w.syncSidebar()
		w.updateWindowManagerTitle()
		return
	}
	if tab.main != nil && tab.main.session.ID != "" && !w.archivedSessions[tab.main.session.ID] {
		if w.visibleSessions == nil {
			w.visibleSessions = make(map[string]bool)
		}
		if !w.visibleSessions[tab.main.session.ID] {
			w.visibleSessions[tab.main.session.ID] = true
			w.saveState()
		}
	}
	if tab.main != nil {
		w.withdrawFinishedNotification(tab.main.session.ID)
	}
	tab.setNeedsAttention(false)

	w.switchComposerDraft(tab.activeConv().session.ID)
	w.refreshHeaderAndBottomBar(tab)
	w.rebuildShellList()
	w.ensureShellOutputPoller()
	w.syncSidebar()
	w.refreshShells()
	if w.inputView != nil {
		w.inputView.GrabFocus()
	}

	if !tab.main.loaded && !tab.main.loadingInitial && w.client != nil {
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
	w.updateWindowManagerTitle()
}

func formatWindowManagerTitle(sessionTitle string, attentionCount int) string {
	base := "gluppencode"
	if title := strings.TrimSpace(sessionTitle); title != "" {
		base = title + " – gluppencode"
	}
	if attentionCount > 0 {
		return fmt.Sprintf("(%d) %s", attentionCount, base)
	}
	return base
}

func (w *Window) countNeedsAttention() int {
	count := 0
	for _, tab := range w.tabs {
		if tab != nil && tab.NeedsAttention() {
			count++
		}
	}
	return count
}

func (w *Window) updateWindowManagerTitle() {
	var sessionTitle string
	if tab := w.currentTab(); tab != nil && tab.main != nil {
		sessionTitle = strings.TrimSpace(tab.main.session.Title)
		if sessionTitle == "" {
			sessionTitle = "Untitled Session"
		}
	}
	if w.ApplicationWindow != nil {
		w.SetTitle(formatWindowManagerTitle(sessionTitle, w.countNeedsAttention()))
	}
}

func (w *Window) ensureSubagentConv(tab *sessionTab, childID string) *convTimeline {
	if tab == nil || childID == "" || tab.main == nil {
		return nil
	}
	if w.childToRoot == nil {
		w.childToRoot = make(map[string]string)
	}
	w.childToRoot[childID] = tab.main.session.ID
	if tab.subTimelines == nil {
		tab.subTimelines = make(map[string]*convTimeline)
	}
	if subConv, ok := tab.subTimelines[childID]; ok && subConv != nil {
		return subConv
	}
	subSess := opencode.Session{
		ID:       childID,
		ParentID: tab.main.session.ID,
		Location: tab.main.session.Location,
	}
	for _, s := range tab.subagents {
		if s.ID == childID {
			subSess = s
			break
		}
	}
	subConv := w.newConvTimeline(subSess)
	tab.subTimelines[childID] = subConv
	if tab.stack != nil {
		tab.stack.AddNamed(subConv.scroll, "sub_"+childID)
	}
	return subConv
}

func (w *Window) openSubagent(tab *sessionTab, subSess opencode.Session) {
	if tab == nil {
		return
	}
	subConv := w.ensureSubagentConv(tab, subSess.ID)
	if subConv == nil {
		return
	}
	subConv.session = subSess
	if subSess.IsRunning() && !subConv.active {
		subConv.active = true
		subConv.syncActivityLabel()
	}

	tab.activeSubID = subSess.ID
	tab.stack.SetVisibleChildName("sub_" + subSess.ID)

	if !subConv.loaded && !subConv.loadingInitial {
		subConv.loadingInitial = true
		w.loadConvMessages(tab, subConv)
	}
	if w.currentTab() == tab {
		w.switchComposerDraft(subSess.ID)
	}
	w.refreshHeaderAndBottomBar(tab)
}

func (w *Window) closeSubagent(tab *sessionTab) {
	if tab == nil || tab.activeSubID == "" {
		return
	}
	tab.activeSubID = ""
	tab.stack.SetVisibleChildName("main")
	if w.currentTab() == tab {
		w.switchComposerDraft(tab.activeConv().session.ID)
	}
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
			if s.IsRunning() {
				w.cancelCompletionSettle(tab)
			}
			w.updateSwitcherIndicators()
			return
		}
	}
	if sess.IsRunning() {
		w.cancelCompletionSettle(tab)
	}
	tab.subagents = append([]opencode.Session{sess}, tab.subagents...)
	w.updateSwitcherIndicators()
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
	if active {
		w.cancelCompletionSettle(tab)
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
	w.updateSwitcherIndicators()
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
			w.updateSwitcherIndicators()
		}
	})
}

func (w *Window) loadConvMessages(tab *sessionTab, conv *convTimeline) {
	if w.client == nil || conv == nil {
		return
	}
	sessionID := conv.session.ID
	startGen := conv.activityGen
	gtkutil.Async(w.ctx, func() func() {
		var (
			page       *opencode.MessagePage
			err        error
			inboxItems []opencode.InboxItem
			forms      []opencode.FormInfo
			perms      []opencode.PermissionRequest
			wg         sync.WaitGroup
		)
		wg.Add(4)
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
		go func() {
			defer wg.Done()
			perms, _ = w.client.ListSessionPermissions(w.ctx, sessionID)
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
			for _, w := range conv.messageWidgets {
				if w != nil {
					conv.timelineBox.Remove(w)
				}
			}
			for _, sb := range conv.slashBubbles {
				if sb != nil {
					conv.timelineBox.Remove(sb)
				}
			}
			conv.slashBubbles = nil
			for _, p := range conv.pendingPrompts {
				if p != nil && p.widget != nil && p.attached {
					conv.timelineBox.Remove(p.widget)
					p.attached = false
				}
			}
			for _, qw := range conv.formWidgets {
				if qw != nil {
					conv.timelineBox.Remove(qw)
				}
			}
			for _, pw := range conv.permissionWidgets {
				if pw != nil {
					conv.timelineBox.Remove(pw)
				}
			}
			if conv.thinkingLabel != nil && conv.thinkingAttached {
				conv.timelineBox.Remove(conv.thinkingLabel)
				conv.thinkingAttached = false
			}
			if conv.revertBanner != nil {
				conv.timelineBox.Remove(conv.revertBanner)
			}
			conv.assistantMap = make(map[string]*AssistantMessageWidget)
			conv.compactionMap = make(map[string]*CompactionWidget)
			conv.messageWidgets = make(map[string]gtk.Widgetter)
			conv.formWidgets = make(map[string]*QuestionFormWidget)
			conv.pendingForms = nil
			conv.permissionWidgets = make(map[string]*PermissionPromptWidget)
			conv.pendingPermissions = nil
			conv.revertBanner = nil
			prevBlocking := conv.blockingTools
			conv.seenMsgIDs = make(map[string]bool)
			conv.executingTools = make(map[string]string)
			conv.finishedTools = make(map[string]bool)
			conv.compacting = false
			conv.runningUserShell = false
			conv.blockingTools = make(map[string]time.Time)
			conv.blockingShellTools = make(map[string]bool)
			conv.blockingShellIDs = make(map[string]string)
			conv.backgroundShellIDs = make(map[string]bool)
			conv.toolNames = make(map[string]string)

			if page == nil {
				conv.messages = nil
				conv.nextCursor = ""
				conv.hasOlder = false
				conv.updateGreeterVisibility()
				return
			}

			conv.messages = page.Messages
			conv.nextCursor = page.NextCursor
			conv.hasOlder = page.NextCursor != ""

			staleEpoch := conv.activityGen != startGen
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
							if !staleEpoch {
								conv.syncToolPart(part)
							}
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
			if !staleEpoch {
				conv.inferActivityStateFromMessages(page.Messages)
			}

			// Filter out any pendingPrompts that were already delivered in page.Messages
			remainingPending := conv.pendingPrompts[:0]
			for _, p := range conv.pendingPrompts {
				if p == nil || p.cancelled || (p.inboxID != "" && conv.seenMsgIDs[p.inboxID]) {
					continue
				}
				remainingPending = append(remainingPending, p)
				if p.widget != nil {
					conv.timelineBox.Append(p.widget)
					p.attached = true
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
				w.upsertPendingForm(form, false)
			}
			for _, perm := range perms {
				w.upsertPendingPermission(perm, false)
			}
			w.refreshTabActionAttention(tab)

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
				w.rebuildShellList()
				w.ensureShellOutputPoller()
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
				w.promptRevertToMessage(tab, conv, msgID, msgText)
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
				} else if f := tw.ShellFile(); f != "" {
					if data, err := readTailBytes(f, maxShellOutputBytes); err == nil && len(data) > 0 {
						out := FormatShellOutput(string(data))
						w.shellOutputs[shID] = out
						tw.SetShellLiveOutput(out)
					} else if tw.IsBackgroundRunning() {
						tw.SetShellLiveOutput(FormatShellOutput(""))
					}
				} else if tw.IsBackgroundRunning() {
					tw.SetShellLiveOutput(FormatShellOutput(""))
				}
				if _, active := w.activeShells[shID]; !active && w.shellsLoaded && tw.IsBackgroundRunning() {
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
	startGen := conv.activityGen
	gtkutil.Async(w.ctx, func() func() {
		var (
			page  *opencode.MessagePage
			err   error
			forms []opencode.FormInfo
			perms []opencode.PermissionRequest
			wg    sync.WaitGroup
		)
		wg.Add(3)
		go func() {
			defer wg.Done()
			page, err = w.client.ListMessagesPage(w.ctx, sessionID, 10, "")
		}()
		go func() {
			defer wg.Done()
			forms, _ = w.client.ListSessionForms(w.ctx, sessionID)
		}()
		go func() {
			defer wg.Done()
			perms, _ = w.client.ListSessionPermissions(w.ctx, sessionID)
		}()
		wg.Wait()
		return func() {
			if err != nil || page == nil {
				return
			}
			activeFormIDs := make(map[string]bool, len(forms))
			for _, f := range forms {
				activeFormIDs[f.ID] = true
				w.upsertPendingForm(f, false)
			}
			for i := len(conv.pendingForms) - 1; i >= 0; i-- {
				if id := conv.pendingForms[i].ID; !activeFormIDs[id] {
					w.removePendingForm(sessionID, id)
				}
			}
			activePermIDs := make(map[string]bool, len(perms))
			for _, p := range perms {
				activePermIDs[p.ID] = true
				w.upsertPendingPermission(p, false)
			}
			for i := len(conv.pendingPermissions) - 1; i >= 0; i-- {
				if id := conv.pendingPermissions[i].ID; !activePermIDs[id] {
					w.removePendingPermission(sessionID, id)
				}
			}
			w.refreshTabActionAttention(tab)
			var appended bool
			findPrevWidget := func(idx int) gtk.Widgetter {
				for i := idx - 1; i >= 0; i-- {
					if pw := conv.messageWidgets[conv.messages[i].ID]; pw != nil {
						return pw
					}
				}
				return nil
			}

			staleEpoch := conv.activityGen != startGen
			hadExecuting := len(conv.executingTools) > 0
			for _, msg := range page.Messages {
				if cw, ok := conv.compactionMap[msg.ID]; ok && msg.Type == "compaction" {
					cw.Update(msg)
				}
				if msg.Type == "assistant" {
					for _, part := range msg.Content {
						if part.Type == "tool" && part.ID != "" && !staleEpoch {
							conv.syncToolPart(part)
						}
					}
				} else if msg.Type == "synthetic" && isSubagentOrShellSynthetic(msg) {
					conv.markSyntheticTargetCompleted(msg)
				}
			}
			if !staleEpoch {
				wasCompacting := conv.compacting
				var lastCompactionFailed bool
				if len(page.Messages) > 0 {
					if last := page.Messages[len(page.Messages)-1]; last.Type == "compaction" {
						conv.compacting = last.Status == "running"
						lastCompactionFailed = last.Status == "failed"
					}
				}
				if ((hadExecuting && len(conv.executingTools) == 0) || (wasCompacting && !conv.compacting)) && conv.activityPhase != activityRetrying {
					if lastCompactionFailed {
						conv.activityPhase = activityWaitingModel
					} else {
						conv.activityPhase = activityThinking
					}
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
						matched.attached = false
						if matched.widget != nil {
							matched.widget.SetWaiting(false, "")
							conv.messageWidgets[msgID] = matched.widget
							matched.widget.SetOnUndo(func() {
								w.promptRevertToMessage(tab, conv, msgID, msgText)
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
			w.updateTurnDurations(conv)

			if appended {
				conv.scroll.ScrollToBottomIfBottomed()
			}
			if tab != nil && w.currentTab() == tab {
				w.updateBottomBar(tab)
				w.rebuildShellList()
				w.ensureShellOutputPoller()
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

func (w *Window) updateComposerAgentModel(agent string, model opencode.ModelRef) {
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
	modelKey := model.String()
	modelDisplay := w.modelNames[modelKey]
	if modelDisplay == "" {
		modelDisplay = w.modelNames[model.ID]
	}
	if modelDisplay == "" {
		modelDisplay = model.ID
	}
	if modelDisplay == "" {
		modelDisplay = "Default Model"
	}
	if w.modelStatusLabel != nil {
		w.modelStatusLabel.SetLabel(modelDisplay)
	}
}

func (w *Window) updateBottomBar(tab *sessionTab) {
	w.ensureMcpStatusLoaded(w.activeDirectory())
	w.refreshMcpStatusChip()
	if w.mcpMgr != nil && w.isBottomSheetOpen() {
		w.mcpMgr.syncDirectory(w.activeDirectory())
	}

	if tab == nil {
		if w.backgroundStatusBtn != nil {
			w.backgroundStatusBtn.SetVisible(false)
		}
		if w.tokRateStatusLabel != nil {
			w.tokRateStatusLabel.SetVisible(false)
		}
		w.updateComposerAgentModel(w.lastSelectedAgent, w.lastSelectedModel)
		w.rebuildAgentPills()
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
	w.updateComposerAgentModel(conv.session.Agent, conv.session.Model)
	modelKey := conv.session.Model.String()

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
		if sh.CallID != "" {
			if conv.blockingShellTools[sh.CallID] {
				return true
			}
			for _, aw := range conv.assistantMap {
				if tw, ok := aw.tools[sh.CallID]; ok && tw.IsBackgroundRunning() {
					return false
				}
			}
		}
		for _, aw := range conv.assistantMap {
			for _, tw := range aw.tools {
				if tw.ShellID() == sh.ID && tw.IsBackgroundRunning() {
					return false
				}
			}
		}
		if len(conv.blockingShellTools) > 0 && len(conv.blockingShellIDs) == 0 && sh.CallID == "" {
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
	if tab == nil {
		return nil
	}
	seen := make(map[string]bool)
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
		if sh.EffectiveFile() == "" {
			sh.File = w.findShellOutputFile(sh.ID)
		}
		if sh.EffectiveLocationDir() == "" && tab.main != nil {
			sh.LocationDir = tab.main.session.Location.Directory
		}
		seen[sh.ID] = true
		out = append(out, sh)
	}
	collectFromConv := func(conv *convTimeline) {
		if conv == nil {
			return
		}
		locDir := conv.session.Location.Directory
		if locDir == "" {
			locDir = tab.main.session.Location.Directory
		}
		for _, aw := range conv.assistantMap {
			for _, tw := range aw.tools {
				shID := tw.ShellID()
				if shID == "" || seen[shID] || !tw.IsBackgroundRunning() {
					continue
				}
				seen[shID] = true
				cmd := strings.TrimSpace(tw.subtitleLabel.Label())
				out = append(out, opencode.ShellItem{
					ID:          shID,
					SessionID:   conv.session.ID,
					CallID:      tw.toolID,
					Command:     cmd,
					LocationDir: locDir,
					File:        tw.ShellFile(),
					Status:      "running",
				})
			}
		}
	}
	collectFromConv(tab.main)
	for _, sub := range tab.subTimelines {
		collectFromConv(sub)
	}
	if len(out) == 0 {
		return nil
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

func (w *Window) findShellOutputFile(shellID string) string {
	if shellID == "" {
		return ""
	}
	for _, tab := range w.tabs {
		if tab.main != nil {
			for _, aw := range tab.main.assistantMap {
				for _, tw := range aw.tools {
					if tw.ShellID() == shellID && tw.ShellFile() != "" {
						return tw.ShellFile()
					}
				}
			}
		}
		for _, sub := range tab.subTimelines {
			for _, aw := range sub.assistantMap {
				for _, tw := range aw.tools {
					if tw.ShellID() == shellID && tw.ShellFile() != "" {
						return tw.ShellFile()
					}
				}
			}
		}
	}
	return ""
}

const maxShellOutputBytes = 65536

func (w *Window) fetchSingleShellOutput(sh opencode.ShellItem) (string, bool) {
	if sh.EffectiveFile() == "" {
		sh.File = w.findShellOutputFile(sh.ID)
	}
	if sh.EffectiveLocationDir() == "" {
		if tab := w.currentTab(); tab != nil && tab.main != nil {
			sh.LocationDir = tab.main.session.Location.Directory
		}
	}
	return fetchShellItemOutputWithLines(w.ctx, w.client, sh, w.effectiveShellTailLines())
}

func fetchRawShellItemOutput(ctx context.Context, client *opencode.Client, sh opencode.ShellItem) (string, bool) {
	filePath := sh.EffectiveFile()
	if filePath != "" {
		// Without a client an empty file is the best answer available.
		if data, readErr := readTailBytes(filePath, maxShellOutputBytes); readErr == nil && (len(data) > 0 || client == nil) {
			return FormatShellOutput(string(data)), true
		}
	}
	if client == nil {
		return "", false
	}
	locDir := sh.EffectiveLocationDir()
	out, err := client.GetShellOutput(ctx, sh.ID, locDir, 0, maxShellOutputBytes)
	if err == nil && out != nil {
		if out.Size > len(out.Output) {
			tailOffset := max(0, out.Size-maxShellOutputBytes)
			if tailOut, tailErr := client.GetShellOutput(ctx, sh.ID, locDir, tailOffset, maxShellOutputBytes); tailErr == nil && tailOut != nil && tailOut.Output != "" {
				return FormatShellOutput(tailOut.Output), true
			}
		}
		if out.Output != "" {
			return FormatShellOutput(out.Output), true
		}
	}
	if filePath != "" {
		if data, readErr := readTailBytes(filePath, maxShellOutputBytes); readErr == nil {
			return FormatShellOutput(string(data)), true
		}
	}
	if err == nil && out != nil {
		return FormatShellOutput(out.Output), true
	}
	return "", false
}

func fetchShellItemOutputWithLines(ctx context.Context, client *opencode.Client, sh opencode.ShellItem, maxLines int) (string, bool) {
	maxLines = clampShellTailLines(maxLines)
	raw, ok := fetchRawShellItemOutput(ctx, client, sh)
	if !ok {
		return "", false
	}
	return FormatShellTailLines(raw, maxLines), true
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
	ctx := w.ctx
	client := w.client
	tailLines := w.effectiveShellTailLines()
	gtkutil.Async(ctx, func() func() {
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
				if text, ok := fetchRawShellItemOutput(ctx, client, shCopy); ok {
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
						lbl.SetText(FormatShellTailLines(r.text, tailLines))
					}
					w.updateToolWidgetsForShell(r.id, r.text, "")
				}
			}
		}
	})
}

func (w *Window) ensureShellOutputPoller() {
	if w.shellPollRunning {
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
		if d := tab.main.session.Location.Directory; d != "" && !seenDirs[d] && !w.isTabSuspended(tab) {
			seenDirs[d] = true
			dirs = append(dirs, d)
		}
	}

	hadTabs := len(w.tabs) > 0
	before := maps.Clone(w.activeShells)
	known := w.backgroundRunningShellIDs()

	gtkutil.Async(w.ctx, func() func() {
		var (
			mu     sync.Mutex
			merged = make(map[string]opencode.ShellItem)
			okDirs int
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
				okDirs++
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
			if okDirs == 0 {
				return
			}
			complete := okDirs == len(dirs)
			// Events that arrived while the listing was in flight are newer
			// than it: keep shells created meanwhile (and, after a partial
			// failure, every known shell) and drop shells that exited meanwhile.
			for id, sh := range w.activeShells {
				if _, had := before[id]; !had || !complete {
					if _, ok := merged[id]; !ok {
						merged[id] = sh
					}
				}
			}
			for id := range before {
				if _, still := w.activeShells[id]; !still {
					delete(merged, id)
				}
			}
			w.activeShells = merged
			if complete && hadTabs {
				w.shellsLoaded = true
				// A shell that was already running when the refresh started
				// and is no longer listed died without a shell.exited event
				// (e.g. the server restarted).
				for id := range known {
					if _, active := w.activeShells[id]; !active {
						w.updateToolWidgetsForShell(id, "", "completed")
					}
				}
			}
			w.updateShellStatus()
			w.rebuildShellList()
			w.ensureShellOutputPoller()
		}
	})
}

// backgroundRunningShellIDs returns the shell IDs of all tool widgets that
// still show a running background shell.
func (w *Window) backgroundRunningShellIDs() map[string]bool {
	ids := make(map[string]bool)
	collect := func(conv *convTimeline) {
		if conv == nil {
			return
		}
		for _, aw := range conv.assistantMap {
			for _, tw := range aw.tools {
				if id := tw.ShellID(); id != "" && tw.IsBackgroundRunning() {
					ids[id] = true
				}
			}
		}
	}
	for _, tab := range w.tabs {
		collect(tab.main)
		for _, sub := range tab.subTimelines {
			collect(sub)
		}
	}
	return ids
}

func (w *Window) updateShellStatus() {
	w.updateSwitcherIndicators()
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
		(w.mcpSectionBox != nil && w.mcpSectionBox.Visible()) ||
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
	for _, cell := range w.contextInfoCells {
		w.contextInfoGrid.Remove(cell)
	}
	w.contextInfoCells = w.contextInfoCells[:0]
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
	if w.contextSessionIDLabel != nil {
		w.contextSessionIDLabel.SetText(sess.ID)
		w.contextSessionIDLabel.SetTooltipText(sess.ID)
	}
	if w.contextSessionIDBox != nil {
		w.contextSessionIDBox.SetVisible(sess.ID != "")
	}
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
		kLbl.SetWidthChars(1)
		cell.Append(kLbl)

		vLbl := gtk.NewLabel(item.value)
		vLbl.AddCSSClass("glup-context-cell-value")
		vLbl.SetXAlign(0)
		vLbl.SetEllipsize(pango.EllipsizeMiddle)
		vLbl.SetWidthChars(1)
		vLbl.SetSelectable(true)
		vLbl.SetTooltipText(item.value)
		cell.Append(vLbl)

		w.contextInfoGrid.Attach(cell, col, row, 1, 1)
		w.contextInfoCells = append(w.contextInfoCells, cell)
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

		outText := FormatShellTailLines(w.shellOutputs[shellItem.ID], w.effectiveShellTailLines())
		outLbl := gtk.NewLabel(outText)
		outLbl.AddCSSClass("glup-shell-output")
		outLbl.SetXAlign(0)
		outLbl.SetYAlign(0)
		outLbl.SetHExpand(true)
		outLbl.SetEllipsize(pango.EllipsizeEnd)
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
	} else if w.lastSelectedAgent != "" {
		activeAgent = w.lastSelectedAgent
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
	} else {
		activeModel = w.lastSelectedModel
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
	w.lastSelectedAgent = strings.TrimSpace(agentID)
	w.saveState()
	if w.notifyCoord != nil {
		w.notifyCoord.syncLastSelection(w, w.lastSelectedAgent, w.lastSelectedModel)
	}

	tab := w.currentTab()
	if tab == nil {
		w.updateBottomBar(nil)
		return
	}
	conv := tab.activeConv()
	sessionID := conv.session.ID
	conv.session.Agent = agentID
	w.updateBottomBar(tab)
	if w.client == nil {
		return
	}

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
	w.lastSelectedModel = model
	w.updateModelLastUsed(model, time.Now().UnixMilli())
	w.saveState()
	if w.notifyCoord != nil {
		w.notifyCoord.syncLastSelection(w, w.lastSelectedAgent, w.lastSelectedModel)
	}

	tab := w.currentTab()
	if tab == nil {
		if w.modelListBox != nil {
			w.rebuildModelList()
		}
		w.updateBottomBar(nil)
		return
	}
	conv := tab.activeConv()
	sessionID := conv.session.ID
	conv.session.Model = model
	if w.modelListBox != nil {
		w.rebuildModelList()
	}
	w.updateBottomBar(tab)
	if w.client == nil {
		return
	}

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

func (w *Window) newSessionAgentAndModel() (string, opencode.ModelRef) {
	agent := strings.TrimSpace(w.lastSelectedAgent)
	model := w.lastSelectedModel

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
	w.createNewSessionInDirAndThen(dir, nil)
}

func (w *Window) createNewSessionInDirAndThen(dir string, onReady func()) {
	cwd := strings.TrimSpace(dir)
	if cwd == "" && len(w.projectOrder) > 0 {
		cwd = w.projectOrder[0]
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}
	cwd = filepath.Clean(cwd)

	selectedAgent, selectedModel := w.newSessionAgentAndModel()
	req := opencode.CreateSessionRequest{
		Directory: cwd,
		Agent:     selectedAgent,
	}
	if selectedModel.ID != "" && selectedModel.ProviderID != "" {
		req.Model = &selectedModel
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
			if w.visibleSessions == nil {
				w.visibleSessions = make(map[string]bool)
			}
			if !w.visibleSessions[sess.ID] {
				w.visibleSessions[sess.ID] = true
				stateChanged = true
			}
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
			tab.main.updateGreeterVisibility()
			w.viewStack.SetVisibleChildName(sess.ID)
			w.onVisibleTabChanged()
			if w.inputView != nil {
				w.inputView.GrabFocus()
			}
			glib.IdleAdd(func() {
				w.syncSidebar()
			})
			if onReady != nil {
				onReady()
			}
		}
	})
}

func (w *Window) sendPromptWithDelivery(delivery opencode.InboxDelivery) {
	tab := w.currentTab()
	if tab == nil {
		if w.inputBuffer != nil && w.client != nil {
			start, end := w.inputBuffer.Bounds()
			text := strings.TrimSpace(w.inputBuffer.Text(start, end, false))
			if strings.HasPrefix(text, "/") {
				cmdLine := strings.TrimPrefix(text, "/")
				parts := strings.SplitN(cmdLine, " ", 2)
				if strings.EqualFold(parts[0], "cd") {
					var cmdArgs string
					if len(parts) > 1 {
						cmdArgs = strings.TrimSpace(parts[1])
					}
					w.recordInputHistory(text)
					w.saveState()
					w.inputBuffer.SetText("")
					w.setComposerExpanded(false)
					w.changeSessionDirectory(nil, nil, cmdArgs)
					return
				}
				if strings.EqualFold(parts[0], "mcp") || strings.EqualFold(parts[0], "mcps") {
					var cmdArgs string
					if len(parts) > 1 {
						cmdArgs = strings.TrimSpace(parts[1])
					}
					w.recordInputHistory(text)
					w.saveState()
					w.inputBuffer.SetText("")
					w.setComposerExpanded(false)
					w.openMcpSheet(cmdArgs)
					return
				}
			}
			if text != "" || len(w.pendingFiles) > 0 {
				w.createNewSessionInDirAndThen("", func() {
					// Guard against creating sessions in a loop if no tab became current.
					if cur := w.currentTab(); cur != nil {
						w.adoptDraft(greeterDraftKey, cur.activeConv().session.ID)
						w.sendPromptWithDelivery(delivery)
					}
				})
			}
		}
		return
	}
	conv := tab.activeConv()
	start, end := w.inputBuffer.Bounds()
	text := strings.TrimSpace(w.inputBuffer.Text(start, end, false))
	if text == "" && len(w.pendingFiles) == 0 {
		return
	}
	if text != "" {
		w.recordInputHistory(text)
		w.saveState()
	}

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
		case "undo", "redo", "new", "cd", "compact", "mcp", "mcps":
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
			w.inputBuffer.SetText("")
			w.setComposerExpanded(false)
			w.undoLastMessage(tab, conv)
			return
		case "redo":
			w.inputBuffer.SetText("")
			w.setComposerExpanded(false)
			w.redoRevert(tab, conv)
			return
		case "new":
			w.inputBuffer.SetText("")
			w.setComposerExpanded(false)
			w.createNewSession()
			return
		case "cd":
			w.inputBuffer.SetText("")
			w.setComposerExpanded(false)
			w.changeSessionDirectory(tab, conv, cmdArgs)
			return
		case "compact":
			w.inputBuffer.SetText("")
			w.setComposerExpanded(false)
			w.compactCurrentSession(tab, conv)
			return
		case "mcp", "mcps":
			w.inputBuffer.SetText("")
			w.setComposerExpanded(false)
			w.openMcpSheet(cmdArgs)
			return
		default:
			sessionID := conv.session.ID
			w.inputBuffer.SetText("")
			w.setComposerExpanded(false)
			uw := NewUserMessageWidget(w.ctx, text)
			conv.slashBubbles = append(conv.slashBubbles, uw)
			conv.timelineBox.Append(uw)
			conv.keepPendingAtBottom()
			conv.scroll.ScrollToBottom()
			w.touchSessionUpdated(tab, conv, time.Now().UnixMilli())
			w.bumpSessionToTop(tab)
			w.setConvActive(sessionID, true)

			gtkutil.Async(w.ctx, func() func() {
				err := w.client.RunCommand(w.ctx, sessionID, cmdName, cmdArgs)
				return func() {
					if err != nil {
						slog.Error("failed to run command", "cmd", cmdName, "err", err)
						app.Error(w.ctx, err)
						w.setConvActive(sessionID, false)
						conv.timelineBox.Remove(uw)
						conv.slashBubbles = slices.DeleteFunc(conv.slashBubbles, func(b *UserMessageWidget) bool {
							return b == uw
						})
						conv.updateGreeterVisibility()
						w.restoreDraft(sessionID, text, nil)
					}
				}
			})
			return
		}
	}

	files := slices.Clone(w.pendingFiles)
	sentGen := w.clearDraftInMemoryForSend(conv.session.ID)

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

	opts := parseMentions(text, w.skills, w.agents, w.activeDirectory())
	opts.Files = files
	opts.Delivery = delivery

	p := &pendingPrompt{
		text:     text,
		files:    files,
		opts:     opts,
		delivery: delivery,
		attached: true,
		widget:   uw,
	}
	w.bindPendingPromptCallbacks(conv, p)
	conv.pendingPrompts = append(conv.pendingPrompts, p)
	conv.timelineBox.Append(uw)
	conv.keepPendingAtBottom()
	conv.scroll.ScrollToBottom()
	w.touchSessionUpdated(tab, conv, time.Now().UnixMilli())
	w.bumpSessionToTop(tab)
	w.setConvActive(sessionID, true)

	gtkutil.Async(w.ctx, func() func() {
		item, err := w.client.SendPrompt(w.ctx, sessionID, opts)
		return func() {
			if err != nil {
				slog.Error("failed to send prompt", "err", err)
				app.Error(w.ctx, err)
				w.setConvActive(sessionID, false)
				if p.widget != nil && p.attached {
					conv.timelineBox.Remove(p.widget)
					p.attached = false
				}
				for i, cur := range conv.pendingPrompts {
					if cur == p {
						conv.pendingPrompts = append(conv.pendingPrompts[:i], conv.pendingPrompts[i+1:]...)
						break
					}
				}
				w.restoreDraft(sessionID, text, files)
				return
			}
			w.removeDraftDirIfGen(sessionID, sentGen)
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
				p.attached = false
				if p.widget != nil {
					p.widget.SetWaiting(false, "")
					conv.messageWidgets[msgID] = p.widget
					p.widget.SetOnUndo(func() {
						w.promptRevertToMessage(tab, conv, msgID, msgText)
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
	p.rewriting = false
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
	if conv != nil {
		conv.keepPendingAtBottom()
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
	p.widget.SetDragReorder(
		func() string {
			return p.dragKey()
		},
		func(srcID string) bool {
			return w.reorderPendingPrompt(conv, srcID, p)
		},
	)
}

func (w *Window) reorderPendingPrompt(conv *convTimeline, srcID string, target *pendingPrompt) bool {
	if conv == nil || target == nil || target.cancelled || target.delivery == opencode.DeliverySteer || srcID == "" {
		return false
	}
	srcIdx := -1
	dstIdx := -1
	for i, cur := range conv.pendingPrompts {
		if cur == nil || cur.cancelled {
			continue
		}
		if cur.rewriting {
			// A previous reorder is still re-admitting this suffix; starting another
			// would re-admit the same prompts twice and leave duplicates queued.
			return false
		}
		if cur == target {
			dstIdx = i
		}
		if cur.inboxID == srcID || cur.dragKey() == srcID {
			if cur.delivery == opencode.DeliverySteer {
				return false
			}
			srcIdx = i
		}
	}
	if srcIdx < 0 || dstIdx < 0 || srcIdx == dstIdx {
		return false
	}

	prevOrder := slices.Clone(conv.pendingPrompts)
	moved := conv.pendingPrompts[srcIdx]
	conv.pendingPrompts = append(conv.pendingPrompts[:srcIdx], conv.pendingPrompts[srcIdx+1:]...)
	conv.pendingPrompts = slices.Insert(conv.pendingPrompts, dstIdx, moved)
	conv.keepPendingAtBottom()

	firstChanged := min(srcIdx, dstIdx)
	w.syncQueuedOrderOnServer(conv, firstChanged, prevOrder)
	return true
}

type queuedRewriteEntry struct {
	p     *pendingPrompt
	oldID string
	opts  opencode.PromptOptions
}

// syncQueuedOrderOnServer re-admits the queued suffix from fromIdx in its new order
// and cancels the originals. While p.rewriting is set, p keeps its old inbox ID,
// the replacement's session.inbox.enqueued is swallowed, and Steer/Cancel are
// deferred; the completion callback binds the new IDs and replays them. On failure
// the pre-reorder order (prevOrder) is restored.
func (w *Window) syncQueuedOrderOnServer(conv *convTimeline, fromIdx int, prevOrder []*pendingPrompt) {
	if w == nil || w.client == nil || conv == nil || conv.session.ID == "" {
		return
	}
	if fromIdx < 0 {
		fromIdx = 0
	}
	if conv.ignoredCancelIDs == nil {
		conv.ignoredCancelIDs = make(map[string]bool)
	}

	var entries []queuedRewriteEntry
	for i := fromIdx; i < len(conv.pendingPrompts); i++ {
		p := conv.pendingPrompts[i]
		if p == nil || p.cancelled || p.delivery == opencode.DeliverySteer || p.inboxID == "" {
			continue
		}
		opts := p.opts
		if opts.Text == "" {
			opts = parseMentions(p.text, w.skills, w.agents, conv.session.Location.Directory)
		}
		if len(opts.Files) == 0 && len(p.files) > 0 {
			opts.Files = p.files
		}
		opts.Delivery = opencode.DeliveryQueue
		opts.NoResume = true
		conv.ignoredCancelIDs[p.inboxID] = true
		p.rewriting = true
		entries = append(entries, queuedRewriteEntry{
			p:     p,
			oldID: p.inboxID,
			opts:  opts,
		})
	}
	if len(entries) == 0 {
		return
	}

	sessionID := conv.session.ID
	ctx := w.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	gtkutil.Async(ctx, func() func() {
		newIDs := make([]string, 0, len(entries))
		var rewriteErr error
		for _, entry := range entries {
			item, err := w.client.SendPrompt(ctx, sessionID, entry.opts)
			if err != nil {
				rewriteErr = err
				break
			}
			newID := ""
			if item != nil {
				newID = item.ID
			}
			newIDs = append(newIDs, newID)
		}
		if rewriteErr != nil {
			for _, id := range newIDs {
				if id != "" {
					_ = w.client.CancelInbox(ctx, sessionID, id)
				}
			}
			return func() {
				slog.Error("failed to reorder queued inbox items", "sessionID", sessionID, "err", rewriteErr)
				app.Error(w.ctx, rewriteErr)
				rewritten := make(map[*pendingPrompt]bool, len(entries))
				for _, entry := range entries {
					rewritten[entry.p] = true
				}
				// Put the rewritten prompts back in their pre-reorder relative order,
				// in the slots they occupy now, so the UI matches the server again.
				var slots []int
				for i, cur := range conv.pendingPrompts {
					if rewritten[cur] {
						slots = append(slots, i)
					}
				}
				var restored []*pendingPrompt
				for _, cur := range prevOrder {
					if rewritten[cur] && slices.Contains(conv.pendingPrompts, cur) {
						restored = append(restored, cur)
					}
				}
				for j, i := range slots {
					conv.pendingPrompts[i] = restored[j]
				}
				conv.keepPendingAtBottom()
				for _, entry := range entries {
					delete(conv.ignoredCancelIDs, entry.oldID)
					p := entry.p
					p.rewriting = false
					switch {
					case p.cancelled:
						w.cancelInboxItem(sessionID, entry.oldID)
					case p.delivery == opencode.DeliverySteer:
						w.steerPendingPrompt(conv, p)
					}
				}
			}
		}

		for _, entry := range entries {
			if err := w.client.CancelInbox(ctx, sessionID, entry.oldID); err != nil {
				slog.Warn("failed to cancel replaced queued inbox item", "sessionID", sessionID, "inboxID", entry.oldID, "err", err)
			}
		}
		return func() {
			for i, entry := range entries {
				p := entry.p
				if p.cancelled {
					p.rewriting = false
					if newIDs[i] != "" {
						w.cancelInboxItem(sessionID, newIDs[i])
					}
					continue
				}
				// The replacement was admitted as a plain queue item; bindInboxDelivery
				// replays a Steer clicked while the rewrite was in flight.
				w.bindInboxDelivery(conv, p, newIDs[i], opencode.DeliveryQueue)
			}
		}
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
	conv.keepPendingAtBottom()
	if p.inboxID == "" || p.rewriting {
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
				conv.keepPendingAtBottom()
			}
		}
	})
}

func (w *Window) cancelPendingPrompt(conv *convTimeline, p *pendingPrompt) {
	if conv == nil || p == nil || p.cancelled {
		return
	}
	p.cancelled = true
	if p.widget != nil && p.attached {
		conv.timelineBox.Remove(p.widget)
		p.attached = false
	}
	for i, cur := range conv.pendingPrompts {
		if cur == p {
			conv.pendingPrompts = append(conv.pendingPrompts[:i], conv.pendingPrompts[i+1:]...)
			break
		}
	}
	conv.updateGreeterVisibility()
	w.restoreDraft(conv.session.ID, p.text, p.files)
	if p.inboxID == "" || p.rewriting {
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
		if p == nil {
			continue
		}
		if p.rewriting && p.text == itemText {
			// Replacement admitted by syncQueuedOrderOnServer; its completion binds the new ID.
			return
		}
		if p.cancelled {
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
	opts := parseMentions(itemText, w.skills, w.agents, conv.session.Location.Directory)
	opts.Files = itemFiles
	opts.Delivery = delivery
	p := &pendingPrompt{
		inboxID:  inboxID,
		text:     itemText,
		files:    itemFiles,
		opts:     opts,
		delivery: delivery,
		widget:   uw,
	}
	w.bindPendingPromptCallbacks(conv, p)
	conv.pendingPrompts = append(conv.pendingPrompts, p)
	if conv.loaded {
		conv.timelineBox.Append(uw)
		p.attached = true
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

func (w *Window) recordInputHistory(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if n := len(w.inputHistory); n == 0 || w.inputHistory[n-1] != text {
		w.inputHistory = append(w.inputHistory, text)
		if len(w.inputHistory) > maxInputHistory {
			w.inputHistory = slices.Clone(w.inputHistory[len(w.inputHistory)-maxInputHistory:])
		}
	}
	w.historyBrowsing = false
	w.historyIndex = 0
	w.historyDraft = ""
}

func (w *Window) canUseInputDisplayLines() bool {
	return w.inputView != nil && w.inputBuffer != nil &&
		w.inputView.Width() > 0 &&
		w.inputView.Buffer() != nil &&
		w.inputView.Buffer().Native() == w.inputBuffer.Native()
}

func (w *Window) isInputCursorOnTopLine() bool {
	if w.inputBuffer == nil || w.inputBuffer.HasSelection() {
		return false
	}
	mark := w.inputBuffer.GetInsert()
	if mark == nil {
		return true
	}
	iter := w.inputBuffer.IterAtMark(mark)
	if iter == nil {
		return true
	}
	if iter.Line() != 0 {
		return false
	}
	if w.canUseInputDisplayLines() {
		probe := iter.Copy()
		return !w.inputView.BackwardDisplayLine(probe)
	}
	return true
}

func (w *Window) isInputCursorOnBottomLine() bool {
	if w.inputBuffer == nil || w.inputBuffer.HasSelection() {
		return false
	}
	mark := w.inputBuffer.GetInsert()
	if mark == nil {
		return true
	}
	iter := w.inputBuffer.IterAtMark(mark)
	if iter == nil {
		return true
	}
	lastLine := max(0, w.inputBuffer.LineCount()-1)
	if iter.Line() < lastLine {
		return false
	}
	if w.canUseInputDisplayLines() {
		probe := iter.Copy()
		return !w.inputView.ForwardDisplayLine(probe)
	}
	return true
}

func (w *Window) setHistoryBufferText(text string) {
	if w.inputBuffer == nil {
		return
	}
	w.settingHistoryText = true
	defer func() {
		w.settingHistoryText = false
	}()
	w.inputBuffer.SetText(text)
	_, end := w.inputBuffer.Bounds()
	if end != nil {
		w.inputBuffer.PlaceCursor(end)
	}
}

func (w *Window) navigateInputHistory(delta int) bool {
	if w.inputBuffer == nil || len(w.inputHistory) == 0 {
		return false
	}
	if delta < 0 {
		if !w.historyBrowsing {
			start, end := w.inputBuffer.Bounds()
			if start != nil && end != nil {
				w.historyDraft = w.inputBuffer.Text(start, end, false)
			}
			w.historyBrowsing = true
			w.historyIndex = len(w.inputHistory) - 1
			w.setHistoryBufferText(w.inputHistory[w.historyIndex])
			return true
		}
		if w.historyIndex <= 0 {
			return true
		}
		w.historyIndex--
		w.setHistoryBufferText(w.inputHistory[w.historyIndex])
		return true
	}
	if delta > 0 {
		if !w.historyBrowsing {
			return false
		}
		if w.historyIndex+1 < len(w.inputHistory) {
			w.historyIndex++
			w.setHistoryBufferText(w.inputHistory[w.historyIndex])
			return true
		}
		draft := w.historyDraft
		w.historyBrowsing = false
		w.historyIndex = 0
		w.historyDraft = ""
		w.setHistoryBufferText(draft)
		return true
	}
	return false
}

func (w *Window) handleInputHistoryKey(keyval uint, state gdk.ModifierType) bool {
	if state.Has(gdk.ControlMask) || state.Has(gdk.ShiftMask) || state.Has(gdk.AltMask) {
		return false
	}
	if !w.historyBrowsing {
		if word, atStart := activeTriggerWord(w.inputBuffer); (strings.HasPrefix(word, "/") && atStart) || strings.HasPrefix(word, "@") {
			return false
		}
	}
	switch keyval {
	case gdk.KEY_Up, gdk.KEY_KP_Up:
		if !w.isInputCursorOnTopLine() {
			return false
		}
		return w.navigateInputHistory(-1)
	case gdk.KEY_Down, gdk.KEY_KP_Down:
		if !w.isInputCursorOnBottomLine() {
			return false
		}
		return w.navigateInputHistory(1)
	default:
		return false
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
			mergeAuthoritativeMessage(*currentMessages, msg)
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

// mergeAuthoritativeMessage updates an already-seen message in msgs with authoritative
// timestamps, token usage, cost, status, and content from the server.
func mergeAuthoritativeMessage(msgs []opencode.Message, msg opencode.Message) {
	if msg.ID == "" {
		return
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].ID != msg.ID {
			continue
		}
		if msg.Time.Created != 0 {
			msgs[i].Time.Created = msg.Time.Created
		}
		if msg.Time.Streamed != 0 {
			msgs[i].Time.Streamed = msg.Time.Streamed
		}
		if msg.Time.Completed != 0 {
			msgs[i].Time.Completed = msg.Time.Completed
		}
		if msg.Tokens != (opencode.TokenUsage{}) {
			msgs[i].Tokens = msg.Tokens
		}
		if msg.Cost != 0 {
			msgs[i].Cost = msg.Cost
		}
		if msg.Status != "" {
			msgs[i].Status = msg.Status
		}
		if msg.Summary != "" {
			msgs[i].Summary = msg.Summary
		}
		if len(msg.Content) > 0 {
			msgs[i].Content = msg.Content
		}
		if msg.Agent != "" {
			msgs[i].Agent = msg.Agent
		}
		if msg.Model.ID != "" {
			msgs[i].Model = msg.Model
		}
		return
	}
}

func formatTurnDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	if ms := d.Round(time.Millisecond).Milliseconds(); ms < 1000 {
		if ms <= 0 {
			ms = 1
		}
		return fmt.Sprintf("%dms", ms)
	}
	if tenths := d.Round(100*time.Millisecond).Milliseconds() / 100; tenths < 100 {
		if tenths%10 == 0 {
			return fmt.Sprintf("%ds", tenths/10)
		}
		return fmt.Sprintf("%d.%ds", tenths/10, tenths%10)
	}
	totalSec := int(d.Round(time.Second).Seconds())
	if totalSec < 60 {
		return fmt.Sprintf("%ds", totalSec)
	}
	if totalSec < 3600 {
		m := totalSec / 60
		s := totalSec % 60
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := totalSec / 3600
	m := (totalSec % 3600) / 60
	s := totalSec % 60
	if m == 0 && s == 0 {
		return fmt.Sprintf("%dh", h)
	}
	if s == 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dh %dm %ds", h, m, s)
}

func formatTurnDurationNotice(d time.Duration) string {
	s := formatTurnDuration(d)
	if s == "" {
		return ""
	}
	return "Worked for " + s
}

// computeTurnDurations calculates the total agent execution duration for each completed
// turn in messages, keyed by the ID of the final assistant message in that turn.
// Compaction, configuration switches, system/restart notices, and idle gaps not attributable
// to background subagent/shell execution are excluded. When active is true, the tail turn is
// still in progress and is omitted.
func computeTurnDurations(
	messages []opencode.Message,
	revertMessageID string,
	active bool,
	hasWidget func(id string) bool,
) map[string]time.Duration {
	if revertMessageID != "" {
		for i, msg := range messages {
			if msg.ID == revertMessageID {
				messages = messages[:i]
				break
			}
		}
	}
	if len(messages) == 0 {
		return nil
	}

	var turns [][]opencode.Message
	start := 0
	for i, msg := range messages {
		if msg.Type == "user" {
			if i > start {
				turns = append(turns, messages[start:i])
			}
			start = i + 1
		}
	}
	if start < len(messages) {
		turns = append(turns, messages[start:])
	}
	if active && len(turns) > 0 {
		turns = turns[:len(turns)-1]
	}

	var out map[string]time.Duration
	for _, seg := range turns {
		var (
			targetID   string
			fallbackID string
			totalMs    int64
			prevEndMs  int64

			hasExcludedNotice bool
			hasIdle           bool
			hasBgCompletion   bool
		)
		for _, m := range seg {
			if m.Type != "assistant" {
				if prevEndMs > 0 {
					switch m.Type {
					case "compaction", "agent-switched", "model-switched", "location-switched", "system":
						hasExcludedNotice = true
					case "synthetic":
						if isSubagentOrShellSynthetic(m) {
							hasBgCompletion = true
						} else {
							hasExcludedNotice = true
						}
					case "idle":
						hasIdle = true
					}
				}
				continue
			}

			if m.ID != "" {
				fallbackID = m.ID
				if hasWidget == nil || hasWidget(m.ID) {
					targetID = m.ID
				}
			}

			startMs := m.Time.Created
			endMs := m.Time.Completed
			if endMs == 0 && m.Time.Streamed > startMs {
				endMs = m.Time.Streamed
			}

			if prevEndMs > 0 && startMs > prevEndMs {
				if !hasExcludedNotice && (!hasIdle || hasBgCompletion) {
					totalMs += startMs - prevEndMs
				}
			}
			if startMs > 0 && endMs > startMs {
				totalMs += endMs - startMs
			}

			if endMs > 0 {
				prevEndMs = endMs
				hasExcludedNotice = false
				hasIdle = false
				hasBgCompletion = false
			} else if startMs > prevEndMs {
				prevEndMs = startMs
				hasExcludedNotice = false
				hasIdle = false
				hasBgCompletion = false
			}
		}

		if targetID == "" {
			targetID = fallbackID
		}
		if targetID != "" && totalMs > 0 {
			if out == nil {
				out = make(map[string]time.Duration)
			}
			out[targetID] = time.Duration(totalMs) * time.Millisecond
		}
	}
	return out
}

func (w *Window) updateTurnDurations(conv *convTimeline) {
	if conv == nil || len(conv.assistantMap) == 0 {
		return
	}
	var revertID string
	if conv.session.Revert != nil {
		revertID = conv.session.Revert.MessageID
	}
	durations := computeTurnDurations(conv.messages, revertID, conv.active, func(id string) bool {
		return conv.assistantMap[id] != nil
	})
	for id, aw := range conv.assistantMap {
		if d, ok := durations[id]; ok {
			aw.SetTurnDuration(formatTurnDurationNotice(d))
		} else {
			aw.SetTurnDuration("")
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

// countRevertedUserMessages returns the number of user messages at or after targetMessageID.
// When targetMessageID is non-empty, it returns at least 1.
func countRevertedUserMessages(messages []opencode.Message, targetMessageID string) int {
	if targetMessageID == "" {
		return 0
	}
	var isReverted bool
	var count int
	for _, msg := range messages {
		if msg.ID == targetMessageID {
			isReverted = true
		}
		if isReverted && msg.Type == "user" {
			count++
		}
	}
	if count == 0 {
		return 1
	}
	return count
}

// collectRevertedFileChanges extracts modified files and diffs from assistant tool calls
// at or after targetMessageID as a fallback when the server does not return git snapshot diffs.
func collectRevertedFileChanges(messages []opencode.Message, targetMessageID string) []opencode.FileDiff {
	if targetMessageID == "" {
		return nil
	}
	var isReverted bool
	var order []string
	byFile := make(map[string]*opencode.FileDiff)
	for _, msg := range messages {
		if msg.ID == targetMessageID {
			isReverted = true
		}
		if !isReverted || msg.Type != "assistant" {
			continue
		}
		for _, part := range msg.Content {
			if part.Type != "tool" {
				continue
			}
			filePath, diffOrContent, ok := opencode.ExtractToolModification(part.Name, part.State.Input)
			if !ok || filePath == "" {
				continue
			}
			add, del := opencode.CountDiffChanges(diffOrContent)
			existing, exists := byFile[filePath]
			if !exists {
				status := "modified"
				if strings.EqualFold(part.Name, "write") {
					status = "added"
				}
				fd := &opencode.FileDiff{
					File:      filePath,
					Patch:     diffOrContent,
					Additions: add,
					Deletions: del,
					Status:    status,
				}
				byFile[filePath] = fd
				order = append(order, filePath)
			} else {
				existing.Additions += add
				existing.Deletions += del
				if diffOrContent != "" {
					if existing.Patch != "" {
						existing.Patch += "\n\n" + diffOrContent
					} else {
						existing.Patch = diffOrContent
					}
				}
			}
		}
	}
	if len(order) == 0 {
		return nil
	}
	out := make([]opencode.FileDiff, 0, len(order))
	for _, f := range order {
		out = append(out, *byFile[f])
	}
	return out
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
	w.stageRevertToMessage(tab, conv, targetMsg.ID, targetMsg.Text, true)
}

func (w *Window) promptRevertToMessage(tab *sessionTab, conv *convTimeline, messageID, messageText string) {
	if conv == nil || conv.session.ID == "" || messageID == "" {
		return
	}
	if w.ApplicationWindow == nil {
		w.stageRevertToMessage(tab, conv, messageID, messageText, true)
		return
	}

	userCount := countRevertedUserMessages(conv.messages, messageID)
	body := "Revert this message? You can redo or edit and resend it afterward."
	if userCount > 1 {
		body = fmt.Sprintf("Revert %d messages starting from this prompt? You can redo or edit and resend afterward.", userCount)
	}

	dlg := adw.NewAlertDialog("Revert Message?", body)

	rollbackCheck := gtk.NewCheckButtonWithLabel("Also rollback modified files in the working tree")
	rollbackCheck.SetActive(true)
	rollbackCheck.SetHAlign(gtk.AlignCenter)
	dlg.SetExtraChild(rollbackCheck)

	dlg.AddResponse("cancel", "Cancel")
	dlg.AddResponse("revert", "Revert")
	dlg.SetResponseAppearance("revert", adw.ResponseDestructive)
	dlg.SetDefaultResponse("revert")
	dlg.SetCloseResponse("cancel")

	dlg.ConnectResponse(func(response string) {
		if response == "revert" {
			w.stageRevertToMessage(tab, conv, messageID, messageText, rollbackCheck.Active())
		}
	})
	dlg.Present(w.ApplicationWindow)
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
			conv.revertFilesRolled = false
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

func (w *Window) stageRevertToMessage(tab *sessionTab, conv *convTimeline, messageID, messageText string, revertFiles bool) {
	if conv == nil || conv.session.ID == "" || messageID == "" {
		return
	}
	sessionID := conv.session.ID

	if messageText != "" {
		w.restoreDraft(sessionID, messageText, nil)
	}

	gtkutil.Async(w.ctx, func() func() {
		rev, err := w.client.StageRevert(w.ctx, sessionID, messageID, revertFiles)
		return func() {
			if err != nil {
				slog.Error("failed to stage revert", "sessionID", sessionID, "messageID", messageID, "err", err)
				app.Error(w.ctx, err)
				return
			}
			conv.revertFilesRolled = revertFiles
			if rev != nil && rev.MessageID != "" {
				conv.session.Revert = rev
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
		w.updateTurnDurations(conv)
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
	userCount := countRevertedUserMessages(conv.messages, targetID)
	var fallbackFiles []opencode.FileDiff
	if conv.revertFilesRolled && len(revert.Files) == 0 && revert.Diff == "" {
		fallbackFiles = collectRevertedFileChanges(conv.messages, targetID)
	}
	conv.revertBanner.Update(userCount, revert, fallbackFiles)
	conv.revertBanner.SetVisible(true)
	w.updateTurnDurations(conv)
	conv.keepPendingAtBottom()
	if conv.scroll != nil {
		conv.scroll.ScrollToBottom()
	}
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
	conv.revertFilesRolled = false
	w.updateTurnDurations(conv)
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
	targetKey := w.activeDraftKey
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
		if !w.isLiveDraftTarget(targetKey) {
			return
		}
		count := len(w.pendingFiles)
		if w.drafts != nil {
			if d := w.ensureAndLoadDraft(targetKey); d != nil {
				count = len(d.files)
			}
		}
		w.attachRawToDraft(targetKey, fmt.Sprintf("clipboard-%d.png", count+1), "image/png", pngBytes.Data(), tex)
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

	targetKey := w.activeDraftKey
	dlg.Open(w.ctx, &w.Window, func(res gio.AsyncResulter) {
		file, err := dlg.OpenFinish(res)
		if err != nil || file == nil {
			return
		}
		if !w.isLiveDraftTarget(targetKey) {
			return
		}
		path := file.Path()
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Error("failed to read attached file", "err", err)
			return
		}
		mime := "image/png"
		lower := strings.ToLower(path)
		if strings.HasSuffix(lower, ".jpg") || strings.HasSuffix(lower, ".jpeg") {
			mime = "image/jpeg"
		} else if strings.HasSuffix(lower, ".webp") {
			mime = "image/webp"
		} else if strings.HasSuffix(lower, ".gif") {
			mime = "image/gif"
		}
		w.attachRawToDraft(targetKey, filepath.Base(path), mime, data, nil)
	})
}

func (w *Window) removePendingFile(idx int) {
	if d := w.activeDraft(); d != nil {
		if idx < 0 || idx >= len(d.files) {
			return
		}
		d.files = slices.Delete(d.files, idx, idx+1)
		if idx < len(d.blobNames) {
			d.blobNames = slices.Delete(d.blobNames, idx, idx+1)
		}
		if idx < len(d.blobTextures) {
			d.blobTextures = slices.Delete(d.blobTextures, idx, idx+1)
		}
		w.pendingFiles = d.files
		w.rebuildAttachmentTray()
		if d.key != greeterDraftKey {
			w.saveDraftSync(d)
		}
		return
	}
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
	curDraft := w.activeDraft()
	for i, f := range w.pendingFiles {
		overlay := gtk.NewOverlay()
		overlay.AddCSSClass("glup-attachment-item")

		pic := gtk.NewPicture()
		pic.SetCanShrink(true)
		pic.SetContentFit(gtk.ContentFitCover)
		pic.SetSizeRequest(64, 64)
		pic.AddCSSClass("glup-attachment-thumb")

		var tex *gdk.Texture
		var err error
		if curDraft != nil && i < len(curDraft.blobTextures) && curDraft.blobTextures[i] != nil {
			tex = curDraft.blobTextures[i]
		} else {
			tex, err = decodeBase64Texture(f.Data)
			if err == nil && tex != nil && curDraft != nil && i < len(curDraft.blobTextures) {
				curDraft.blobTextures[i] = tex
			}
		}
		if err == nil && tex != nil {
			pic.SetPaintable(tex)
			pic.SetCursorFromName("pointer")
			pic.SetTooltipText(fmt.Sprintf("%s (%dx%d) — click to zoom", f.Name, tex.Width(), tex.Height()))
			name := f.Name
			click := gtk.NewGestureClick()
			click.SetButton(1)
			click.ConnectReleased(func(_ int, _, _ float64) {
				showImageViewer(w.ctx, name, tex)
			})
			pic.AddController(click)
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

const defaultEscStopConfirmTimeoutMS = 3000

func (w *Window) effectiveEscStopTimeoutMS() uint {
	if w != nil && w.escStopTimeoutMS > 0 {
		return w.escStopTimeoutMS
	}
	return defaultEscStopConfirmTimeoutMS
}

func (w *Window) hasVisibleComposerPopover() bool {
	for _, pop := range []*gtk.Popover{w.agentPopover, w.modelPopover, w.renameSessionPopover} {
		if pop != nil && pop.Visible() {
			return true
		}
	}
	return false
}

func (w *Window) handleEscapeStop(state gdk.ModifierType) bool {
	if state.Has(gdk.ControlMask) || state.Has(gdk.ShiftMask) || state.Has(gdk.AltMask) || state.Has(gdk.SuperMask) {
		return false
	}
	if w.offline || w.hasVisibleComposerPopover() {
		return false
	}
	tab := w.currentTab()
	if tab == nil {
		w.disarmEscStop()
		return false
	}
	conv := tab.activeConv()
	if conv == nil || !conv.active || conv.session.ID == "" {
		w.disarmEscStop()
		return false
	}
	if w.escStopArmedSess == conv.session.ID {
		w.disarmEscStop()
		w.interruptSession()
		return true
	}
	w.armEscStop(conv.session.ID)
	return true
}

func (w *Window) armEscStop(sessionID string) {
	if w.escStopTimer != 0 {
		glib.SourceRemove(w.escStopTimer)
		w.escStopTimer = 0
	}
	w.escStopGen++
	gen := w.escStopGen
	w.escStopArmedSess = sessionID
	w.syncEscStopUI()

	w.escStopTimer = glib.TimeoutAdd(w.effectiveEscStopTimeoutMS(), func() bool {
		w.escStopTimer = 0
		if w.ctx != nil && w.ctx.Err() != nil {
			return false
		}
		if w.escStopGen == gen {
			w.disarmEscStop()
		}
		return false
	})
}

func (w *Window) disarmEscStop() {
	if w.escStopTimer != 0 {
		glib.SourceRemove(w.escStopTimer)
		w.escStopTimer = 0
	}
	w.escStopGen++
	w.escStopArmedSess = ""
	w.syncEscStopUI()
}

func (w *Window) syncEscStopUI() {
	armed := false
	if w.escStopArmedSess != "" {
		if tab := w.currentTab(); tab != nil {
			if conv := tab.activeConv(); conv != nil && conv.active && conv.session.ID == w.escStopArmedSess {
				armed = true
			}
		}
	}
	if w.stopConfirmBtn != nil {
		w.stopConfirmBtn.SetVisible(armed)
	}
	if w.stopBtn != nil {
		if armed {
			w.stopBtn.AddCSSClass("glup-stop-armed")
			w.stopBtn.SetTooltipText("Press Esc again or click to stop session")
		} else {
			w.stopBtn.RemoveCSSClass("glup-stop-armed")
			w.stopBtn.SetTooltipText("Interrupt Session")
		}
	}
}

func (w *Window) interruptSession() {
	w.disarmEscStop()
	if w.client == nil {
		return
	}
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
		for id := range conv.blockingTools {
			delete(conv.executingTools, id)
			if conv.finishedTools == nil {
				conv.finishedTools = make(map[string]bool)
			}
			conv.finishedTools[id] = true
		}
		if len(conv.executingTools) == 0 && conv.activityPhase != activityRetrying {
			conv.activityPhase = activityThinking
		}
		clear(conv.blockingTools)
		clear(conv.blockingShellTools)
		for shID := range conv.blockingShellIDs {
			conv.backgroundShellIDs[shID] = true
		}
		clear(conv.blockingShellIDs)
		conv.syncActivityLabel()
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
	wasActive := conv != nil && conv.active
	if active {
		w.cancelCompletionSettle(tab)
		if conv != nil && conv == tab.main && !wasActive {
			w.resetRootTurn(tab)
		}
	}
	if conv != nil {
		conv.active = active
		if !active {
			conv.runningTools = 0
			conv.activityGen++
			conv.activityPhase = activityWaitingModel
			conv.retryAttempt = 0
			conv.compacting = false
			conv.runningUserShell = false
			clear(conv.executingTools)
			clear(conv.finishedTools)
			clear(conv.blockingTools)
			clear(conv.blockingShellTools)
			clear(conv.blockingShellIDs)
			if n := len(conv.messages); n > 0 && conv.messages[n-1].Type == "assistant" && conv.messages[n-1].Time.Created > 0 && conv.messages[n-1].Time.Completed == 0 {
				conv.messages[n-1].Time.Completed = time.Now().UnixMilli()
			}
		}
		w.updateTurnDurations(conv)
		conv.keepPendingAtBottom()
		if active && conv.scroll != nil {
			conv.scroll.ScrollToBottomIfBottomed()
		}
	}
	if !active && w.escStopArmedSess == sessionID {
		w.disarmEscStop()
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
	if w.stopBtn != nil {
		w.stopBtn.SetVisible(busy)
	}
	if w.escStopArmedSess == "" {
		w.syncEscStopUI()
		return
	}
	if !busy {
		w.disarmEscStop()
		return
	}
	tab := w.currentTab()
	if tab == nil || tab.activeConv() == nil || tab.activeConv().session.ID != w.escStopArmedSess {
		w.disarmEscStop()
		return
	}
	w.syncEscStopUI()
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
	var msg opencode.Message
	msg.ID = msgID
	msg.Type = "assistant"
	msg.Agent = agent
	msg.Time.Created = time.Now().UnixMilli()
	conv.messages = append(conv.messages, msg)
	conv.seenMsgIDs[msgID] = true
	conv.timelineBox.Append(aw)
	w.updateTurnDurations(conv)
	conv.keepPendingAtBottom()
	return aw
}

func (w *Window) resyncOnReconnect() {
	w.ensureMcpMaps()
	clear(w.mcpLoadedDirs)
	clear(w.mcpFetchingDirs)
	w.ensureMcpStatusLoaded(w.activeDirectory())
	if mgr := w.mcpMgr; mgr != nil && w.isBottomSheetOpen() {
		mgr.refreshAll()
	}
	for _, tab := range w.tabs {
		if w.isTabSuspended(tab) {
			continue
		}
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
					if w.visibleSessions == nil {
						w.visibleSessions = make(map[string]bool)
					}
					if !w.visibleSessions[sess.ID] {
						w.visibleSessions[sess.ID] = true
						w.saveState()
					}
					wasEmpty := len(w.tabs) == 0
					w.ensureSessionTab(sess)
					if wasEmpty && w.viewStack != nil {
						w.viewStack.SetVisibleChildName(sess.ID)
						w.onVisibleTabChanged()
					}
					w.syncSidebar()
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
			for i := range w.allSessions {
				if w.allSessions[i].ID == sess.ID {
					if sess.Title != "" {
						w.allSessions[i].Title = sess.Title
					}
					if sess.Agent != "" {
						w.allSessions[i].Agent = sess.Agent
					}
					if sess.Model.ID != "" {
						w.allSessions[i].Model = sess.Model
					}
					if sess.Outcome != "" {
						w.allSessions[i].Outcome = sess.Outcome
					}
					if sess.Time.Created != 0 {
						w.allSessions[i].Time.Created = sess.Time.Created
					}
					if sess.Time.Busy != 0 {
						w.allSessions[i].Time.Busy = sess.Time.Busy
					}
					if sess.Time.Idle != 0 {
						w.allSessions[i].Time.Idle = sess.Time.Idle
					}
					if sess.Time.Updated != 0 {
						w.allSessions[i].Time.Updated = sess.Time.Updated
					}
					if sess.Location.Directory != "" {
						w.allSessions[i].Location = sess.Location
					}
					break
				}
			}
			if tab, ok := w.tabs[sess.ID]; ok {
				if sess.Title != "" && tab.main.session.Title != sess.Title {
					tab.main.session.Title = sess.Title
					tab.setTitle(sess.Title)
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
				dirChanged := false
				if sess.Location.Directory != "" {
					oldDir := normalizeProjectDir(tab.main.session.Location.Directory)
					newDir := normalizeProjectDir(sess.Location.Directory)
					tab.main.session.Location = sess.Location
					if newDir != "" && newDir != oldDir {
						dirChanged = true
						if w.ensureProjectOrder(newDir) {
							w.saveState()
						}
						w.moveSessionInProjectCache(sess.ID, oldDir, newDir, &tab.main.session)
					}
				}
				if sess.Time.Updated != 0 {
					w.touchSessionUpdated(tab, tab.main, sess.Time.Updated)
				}
				if dirChanged {
					w.syncSidebar()
				} else {
					w.updateSwitcherIndicators()
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
				w.deleteDraft(delID)
				if parentTab, ok := w.tabs[parentID]; ok {
					filtered := parentTab.subagents[:0]
					for _, s := range parentTab.subagents {
						if s.ID != delID {
							filtered = append(filtered, s)
						}
					}
					parentTab.subagents = filtered
					if parentTab.activeSubID == delID {
						w.closeSubagent(parentTab)
					}
					if subConv, ok := parentTab.subTimelines[delID]; ok {
						subConv.closeStreams()
						delete(parentTab.subTimelines, delID)
					}
					w.refreshTabActionAttention(parentTab)
					w.maybeScheduleRootTurnCompletion(parentTab)
					if w.currentTab() == parentTab {
						w.updateBottomBar(parentTab)
						w.rebuildSubagentList(parentTab)
					}
				}
			} else {
				var normDir string
				allFiltered := w.allSessions[:0]
				for _, s := range w.allSessions {
					if s.ID == delID {
						if normDir == "" {
							normDir = normalizeProjectDir(s.Location.Directory)
						}
						continue
					}
					allFiltered = append(allFiltered, s)
				}
				w.allSessions = allFiltered

				if tab, ok := w.tabs[delID]; ok && normDir == "" && tab.main != nil {
					normDir = normalizeProjectDir(tab.main.session.Location.Directory)
				}
				if w.projectSessions != nil {
					for dir, list := range w.projectSessions {
						filtered := list[:0]
						removed := false
						for _, s := range list {
							if s.ID == delID {
								removed = true
								if normDir == "" {
									normDir = dir
								}
								continue
							}
							filtered = append(filtered, s)
						}
						if removed {
							if len(filtered) == 0 {
								delete(w.projectSessions, dir)
							} else {
								w.projectSessions[dir] = filtered
							}
						}
					}
				}
				stateDirty := false
				if w.visibleSessions[delID] {
					delete(w.visibleSessions, delID)
					stateDirty = true
				}
				if w.archivedSessions[delID] {
					delete(w.archivedSessions, delID)
					stateDirty = true
				}
				if stateDirty {
					w.saveState()
				}

				if tab, ok := w.tabs[delID]; ok {
					wasCurrent := (w.currentRootSessionID() == delID)
					var nextID string
					if wasCurrent {
						nextID = w.nextVisibleTabID(delID, normDir, nil)
					}

					prevPopulating := w.populating
					w.populating = true
					w.deleteSessionDraftsForTab(delID, tab)
					w.cancelCompletionSettle(tab)
					tab.main.closeStreams()
					for subID, c := range tab.subTimelines {
						c.closeStreams()
						delete(w.childToRoot, subID)
					}
					for _, sub := range tab.subagents {
						delete(w.childToRoot, sub.ID)
					}
					w.cleanupTabSidebarState(tab)
					if w.viewStack != nil && tab.stack != nil {
						w.viewStack.Remove(tab.stack)
					}
					delete(w.tabs, delID)
					if idx := slices.Index(w.tabOrder, delID); idx >= 0 {
						w.tabOrder = slices.Delete(w.tabOrder, idx, idx+1)
					}
					if normDir != "" && w.projectSessions != nil && !w.expandedProjects[normDir] {
						maxVis := w.effectiveMaxVisibleSessions()
						openCount := 0
						for _, id := range w.tabOrder {
							if t := w.tabs[id]; t != nil && t.main != nil && normalizeProjectDir(t.main.session.Location.Directory) == normDir {
								openCount++
							}
						}
						for _, candidate := range w.projectSessions[normDir] {
							if openCount >= maxVis {
								break
							}
							if _, exists := w.tabs[candidate.ID]; !exists {
								w.ensureSessionTab(candidate)
								openCount++
							}
						}
					}
					if wasCurrent && nextID == "" {
						nextID = w.nextVisibleTabID(delID, normDir, nil)
					}
					w.populating = prevPopulating

					if wasCurrent {
						if nextID != "" && w.viewStack != nil {
							w.viewStack.SetVisibleChildName(nextID)
						}
						w.onVisibleTabChanged()
					} else {
						w.updateWindowManagerTitle()
					}
				} else {
					w.deleteSessionDraftsForTab(delID, nil)
				}
				w.withdrawSessionNotifications(delID)
				w.syncSidebar()
			}
		}

	case "session.execution.started":
		var d opencode.SessionIDEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			w.updateSessionActivityState(d.SessionID, true)
			w.revealActiveSession(d.SessionID)
			tab, conv := w.resolveEventConv(d.SessionID)
			w.touchSessionUpdated(tab, conv, time.Now().UnixMilli())
			if conv != nil {
				conv.activityPhase = activityWaitingModel
				conv.retryAttempt = 0
			}
			w.setConvActive(d.SessionID, true)
			w.setSubagentActive(d.SessionID, true, "")
			w.reconcileTail(d.SessionID)
		}

	case "session.execution.succeeded", "session.execution.failed", "session.execution.interrupted", "session.idle":
		var d opencode.SessionIDEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			w.updateSessionActivityState(d.SessionID, false)
			tab, conv := w.resolveEventConv(d.SessionID)
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
			if tab != nil {
				if conv != nil && conv == tab.main {
					if ev.Type == "session.execution.failed" || ev.Type == "session.execution.interrupted" {
						tab.turnFailed = true
						w.cancelCompletionSettle(tab)
					} else {
						w.maybeScheduleRootTurnCompletion(tab)
					}
				} else if _, isChild := w.childToRoot[d.SessionID]; isChild {
					w.maybeScheduleRootTurnCompletion(tab)
				}
			}
		}

	case "session.error":
		var d opencode.SessionErrorEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			if tab, conv := w.resolveEventConv(d.SessionID); tab != nil && conv != nil && conv == tab.main {
				tab.turnFailed = true
				w.cancelCompletionSettle(tab)
			}
		}

	case "session.status":
		var d opencode.SessionStatusEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			switch d.Status.Type {
			case "busy":
				w.updateSessionActivityState(d.SessionID, true)
				w.revealActiveSession(d.SessionID)
				if _, conv := w.resolveEventConv(d.SessionID); conv != nil && conv.activityPhase == activityRetrying {
					conv.activityPhase = activityThinking
					conv.retryAttempt = 0
				}
				w.setConvActive(d.SessionID, true)
				w.setSubagentActive(d.SessionID, true, "")
			case "retry":
				w.updateSessionActivityState(d.SessionID, true)
				w.revealActiveSession(d.SessionID)
				if _, conv := w.resolveEventConv(d.SessionID); conv != nil {
					if d.Status.Attempt > 0 {
						conv.retryAttempt = d.Status.Attempt
					} else if conv.activityPhase != activityRetrying {
						conv.retryAttempt = 0
					}
					conv.activityPhase = activityRetrying
				}
				w.setConvActive(d.SessionID, true)
				w.setSubagentActive(d.SessionID, true, "")
			case "idle":
				w.updateSessionActivityState(d.SessionID, false)
				tab, conv := w.resolveEventConv(d.SessionID)
				w.setConvActive(d.SessionID, false)
				w.setSubagentActive(d.SessionID, false, "succeeded")
				if rootID, ok := w.childToRoot[d.SessionID]; ok {
					w.loadChildSubagents(rootID)
				}
				if tab != nil && ((conv != nil && conv == tab.main) || w.childToRoot[d.SessionID] != "") {
					w.maybeScheduleRootTurnCompletion(tab)
				}
			}
		}

	case "session.retry.scheduled":
		var d opencode.RetryScheduledEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			if _, conv := w.resolveEventConv(d.SessionID); conv != nil {
				conv.activityPhase = activityRetrying
				conv.retryAttempt = d.Attempt
				conv.syncActivityLabel()
			}
		}

	case "session.shell.started", "session.shell.ended":
		var d opencode.SessionIDEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			if _, conv := w.resolveEventConv(d.SessionID); conv != nil {
				conv.runningUserShell = ev.Type == "session.shell.started"
				conv.syncActivityLabel()
			}
		}

	case "session.inbox.enqueued":
		var d opencode.InboxEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			tab, conv := w.resolveEventConv(d.SessionID)
			w.touchSessionUpdated(tab, conv, time.Now().UnixMilli())
			if conv != nil {
				w.onInboxEnqueued(tab, conv, d)
			}
		}

	case "session.inbox.delivery.changed":
		var d opencode.InboxEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			_, conv := w.resolveEventConv(d.SessionID)
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
			_, conv := w.resolveEventConv(d.SessionID)
			if conv != nil && d.InboxID != "" {
				if conv.ignoredCancelIDs[d.InboxID] {
					delete(conv.ignoredCancelIDs, d.InboxID)
					break
				}
				for i, p := range conv.pendingPrompts {
					if p != nil && p.inboxID == d.InboxID {
						p.cancelled = true
						if p.widget != nil && p.attached {
							conv.timelineBox.Remove(p.widget)
							p.attached = false
						}
						conv.pendingPrompts = append(conv.pendingPrompts[:i], conv.pendingPrompts[i+1:]...)
						conv.updateGreeterVisibility()
						break
					}
				}
			}
		}

	case "session.compaction.delta":
		var d opencode.CompactionEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			if _, conv := w.resolveEventConv(d.SessionID); conv != nil && !conv.compacting {
				conv.compacting = true
				conv.syncActivityLabel()
			}
		}

	case "session.inbox.delivered", "session.synthetic", "session.instructions.updated", "session.skill.activated",
		"session.compaction.started", "session.compaction.ended", "session.compaction.failed":
		var d struct {
			SessionID string         `json:"sessionID"`
			Metadata  map[string]any `json:"metadata,omitempty"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			tab, conv := w.resolveEventConv(d.SessionID)
			w.touchSessionUpdated(tab, conv, time.Now().UnixMilli())
			if conv != nil {
				switch ev.Type {
				case "session.inbox.delivered":
					if len(conv.executingTools) == 0 && conv.activityPhase != activityRetrying {
						conv.activityPhase = activityThinking
						conv.retryAttempt = 0
						conv.syncActivityLabel()
					}
				case "session.compaction.started":
					conv.compacting = true
					conv.syncActivityLabel()
				case "session.compaction.ended":
					conv.activityGen++
					conv.compacting = false
					if len(conv.executingTools) == 0 && conv.activityPhase != activityRetrying {
						conv.activityPhase = activityThinking
						conv.retryAttempt = 0
					}
					conv.syncActivityLabel()
				case "session.compaction.failed":
					conv.activityGen++
					conv.compacting = false
					if len(conv.executingTools) == 0 && conv.activityPhase != activityRetrying {
						conv.activityPhase = activityWaitingModel
						conv.retryAttempt = 0
					}
					conv.syncActivityLabel()
				}
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
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" && d.Location.Directory != "" {
			w.applySessionMoved(d.SessionID, d.Location.Directory)
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
			tab, conv := w.resolveEventConv(d.SessionID)
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
			tab, conv := w.resolveEventConv(d.SessionID)
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
			tab, conv := w.resolveEventConv(d.SessionID)
			w.touchSessionUpdated(tab, conv, time.Now().UnixMilli())
			if conv != nil {
				conv.streamStart = time.Now()
				conv.streamEnd = time.Time{}
				conv.streamedChars = 0
				conv.activityPhase = activityThinking
				conv.retryAttempt = 0
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
			_, conv := w.resolveEventConv(d.SessionID)
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
			tab, conv := w.resolveEventConv(d.SessionID)
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
				if len(conv.executingTools) == 0 && conv.activityPhase != activityRetrying {
					conv.activityPhase = activityThinking
					conv.syncActivityLabel()
				}
				w.updateTurnDurations(conv)
				if w.currentTab() == tab {
					w.updateBottomBar(tab)
				}
			}
		}

	case "session.step.failed":
		var d opencode.StepFailedEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			if _, conv := w.resolveEventConv(d.SessionID); conv != nil {
				if len(conv.executingTools) == 0 && conv.activityPhase != activityRetrying {
					conv.activityPhase = activityWaitingModel
					conv.syncActivityLabel()
				}
			}
		}

	case "session.reasoning.started":
		var d opencode.TextStartedEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			if _, conv := w.resolveEventConv(d.SessionID); conv != nil {
				conv.activityPhase = activityThinking
				conv.retryAttempt = 0
				conv.syncActivityLabel()
			}
		}

	case "session.reasoning.delta":
		var d opencode.TextDeltaEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveEventConv(d.SessionID)
			if conv != nil {
				conv.activityPhase = activityThinking
				conv.retryAttempt = 0
				conv.syncActivityLabel()
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
			tab, conv := w.resolveEventConv(d.SessionID)
			if conv != nil && conv.loaded {
				aw := w.ensureAssistantWidget(tab, conv, d.AssistantMessageID, "", "")
				aw.FinalizeReasoning(d.Ordinal, d.Text)
			}
		}

	case "session.text.started":
		var d opencode.TextStartedEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			if _, conv := w.resolveEventConv(d.SessionID); conv != nil {
				conv.activityPhase = activityResponding
				conv.retryAttempt = 0
				conv.syncActivityLabel()
			}
		}

	case "session.text.delta":
		var d opencode.TextDeltaEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveEventConv(d.SessionID)
			if conv != nil {
				conv.activityPhase = activityResponding
				conv.retryAttempt = 0
				conv.syncActivityLabel()
				w.onStreamDelta(tab, conv, len(d.Delta))
				if tab != nil && conv == tab.main {
					w.recordRootReplyDelta(tab, d.AssistantMessageID, d.Delta)
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
			tab, conv := w.resolveEventConv(d.SessionID)
			if tab != nil && conv != nil && conv == tab.main {
				w.recordRootReplyEnded(tab, d.AssistantMessageID, d.Text)
			}
			if conv != nil && conv.loaded {
				aw := w.ensureAssistantWidget(tab, conv, d.AssistantMessageID, "", "")
				aw.FinalizeText(d.Ordinal, d.Text)
			}
		}

	case "session.tool.input.started":
		var d opencode.ToolEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			tab, conv := w.resolveEventConv(d.SessionID)
			if conv != nil {
				conv.runningTools++
				conv.activityPhase = activityResponding
				conv.retryAttempt = 0
				conv.syncActivityLabel()
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
			tab, conv := w.resolveEventConv(d.SessionID)
			if conv != nil {
				conv.activityPhase = activityResponding
				conv.retryAttempt = 0
				conv.syncActivityLabel()
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
			tab, conv := w.resolveEventConv(d.SessionID)
			var name string
			if conv != nil {
				if d.ID != "" && d.Name != "" {
					conv.toolNames[d.ID] = d.Name
				}
				name = d.Name
				if name == "" && d.ID != "" {
					name = conv.toolNames[d.ID]
				}
				conv.setExecutingTool(d.ID, name)
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
			tab, conv := w.resolveEventConv(d.SessionID)
			if conv != nil {
				if d.ID != "" && d.Name != "" {
					conv.toolNames[d.ID] = d.Name
				}
				name := d.Name
				if name == "" && d.ID != "" {
					name = conv.toolNames[d.ID]
				}
				if d.ID != "" && name != "" {
					if _, executing := conv.executingTools[d.ID]; executing {
						conv.executingTools[d.ID] = name
						conv.syncActivityLabel()
					}
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
			tab, conv := w.resolveEventConv(d.SessionID)
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
				conv.clearExecutingTool(d.ID)
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
						out := w.shellOutputs[shID]
						if out == "" {
							out = FormatShellOutput("")
						}
						tw.SetShellLiveOutput(out)
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
			tab, conv := w.resolveEventConv(d.SessionID)
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
				conv.clearExecutingTool(d.ID)
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
			tab, conv := w.resolveEventConv(d.SessionID)
			if conv != nil {
				conv.session.Revert = &d.Revert
				if len(d.Revert.Files) > 0 || d.Revert.Diff != "" {
					conv.revertFilesRolled = true
				}
				w.applyRevertState(tab, conv)
			}
		}

	case "session.revert.cleared":
		var d opencode.RevertClearedEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			tab, conv := w.resolveEventConv(d.SessionID)
			if conv != nil {
				conv.session.Revert = nil
				conv.revertFilesRolled = false
				w.applyRevertState(tab, conv)
			}
		}

	case "session.revert.committed":
		var d opencode.RevertCommittedEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.SessionID != "" {
			tab, conv := w.resolveEventConv(d.SessionID)
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
					if prevSh.EffectiveFile() == "" {
						prevSh.File = w.findShellOutputFile(id)
					}
					if prevSh.EffectiveLocationDir() == "" {
						if tab := w.currentTab(); tab != nil && tab.main != nil {
							prevSh.LocationDir = tab.main.session.Location.Directory
						}
					}
					ctx := w.ctx
					client := w.client
					gtkutil.Async(ctx, func() func() {
						text, ok := fetchRawShellItemOutput(ctx, client, prevSh)
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
			w.upsertPendingForm(d.Form, true)
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

	case "permission.asked":
		var d opencode.PermissionAskedEvent
		if json.Unmarshal(ev.Data, &d) == nil && d.Request.ID != "" {
			w.upsertPendingPermission(d.Request, true)
		}

	case "permission.replied":
		var d opencode.PermissionRepliedEvent
		if json.Unmarshal(ev.Data, &d) == nil {
			if reqID := d.EffectiveRequestID(); reqID != "" {
				w.removePendingPermission(d.SessionID, reqID)
			}
		}

	case "mcp.status.changed":
		evDir := normalizeProjectDir(ev.Location.Directory)
		sheetOpen := w.isBottomSheetOpen()
		mgrMatches := w.mcpMgr != nil && sheetOpen && (evDir == "" || normalizeProjectDir(w.mcpMgr.dir) == evDir)
		if evDir == "" || evDir == w.effectiveMcpDirKey() || mgrMatches {
			includeMeta := mgrMatches
			targetDir := ev.Location.Directory
			if targetDir == "" {
				if w.mcpMgr != nil && sheetOpen && w.mcpMgr.dir != "" {
					targetDir = w.mcpMgr.dir
				} else {
					targetDir = w.activeDirectory()
				}
			}
			w.refreshMcpForDir(targetDir, includeMeta)
		} else if evDir != "" {
			delete(w.mcpLoadedDirs, evDir)
		}

	case "mcp.resources.changed":
		evDir := normalizeProjectDir(ev.Location.Directory)
		if w.mcpMgr != nil && w.isBottomSheetOpen() && (evDir == "" || normalizeProjectDir(w.mcpMgr.dir) == evDir) {
			w.mcpMgr.refreshResources()
		}
	}
}
