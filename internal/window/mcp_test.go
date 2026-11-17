package window

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4-sourceview/pkg/gtksource/v5"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"libdb.so/gluppencode/internal/opencode"
)

var (
	testAdwAppOnce sync.Once
	testAdwApp     *adw.Application
	testAdwAppErr  error
)

func newTestApplicationWindow(t *testing.T) *adw.ApplicationWindow {
	t.Helper()
	testAdwAppOnce.Do(func() {
		testAdwApp = adw.NewApplication("", gio.ApplicationFlagsNone)
		testAdwAppErr = testAdwApp.Register(context.Background())
	})
	if testAdwAppErr != nil {
		t.Fatalf("failed to register test adw.Application: %v", testAdwAppErr)
	}
	return adw.NewApplicationWindow(&testAdwApp.Application)
}

func pumpMainLoopUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	ctx := glib.MainContextDefault()
	for time.Now().Before(deadline) {
		ctx.Iteration(false)
		if cond() && !ctx.Pending() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	for ctx.Pending() {
		ctx.Iteration(false)
	}
	if !cond() {
		t.Fatalf("timed out waiting for main loop condition after %s", timeout)
	}
}

func TestMcpStatusChipStatesGreeterAndEvents(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}
	adw.Init()

	dirA := t.TempDir()
	dirB := t.TempDir()
	dirSuspended := t.TempDir()

	var (
		mu          sync.Mutex
		mcpRequests []string
		dirAServers = []opencode.McpServer{
			{Name: "notes", Status: opencode.McpStatus{Status: "connected"}},
			{Name: "search", Status: opencode.McpStatus{Status: "connected"}},
		}
		dirBServers = []opencode.McpServer{
			{Name: "broken", Status: opencode.McpStatus{Status: "failed", Error: "spawn ENOENT <bin>"}},
		}
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/mcp" {
			reqDir := r.URL.Query().Get("location[directory]")
			mu.Lock()
			mcpRequests = append(mcpRequests, reqDir)
			aCopy := append([]opencode.McpServer(nil), dirAServers...)
			bCopy := append([]opencode.McpServer(nil), dirBServers...)
			mu.Unlock()

			resolved := reqDir
			var data []opencode.McpServer
			switch normalizeProjectDir(reqDir) {
			case "":
				resolved = dirA
				data = aCopy
			case normalizeProjectDir(dirA):
				data = aCopy
			case normalizeProjectDir(dirB):
				data = bCopy
			case normalizeProjectDir(dirSuspended):
				data = []opencode.McpServer{{Name: "susp", Status: opencode.McpStatus{Status: "connected"}}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"location": map[string]string{"directory": resolved},
				"data":     data,
			})
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	vs := adw.NewViewStack()
	mcpBox := gtk.NewBox(gtk.OrientationHorizontal, 0)
	mcpLbl := gtk.NewLabel("")
	win := &Window{
		ctx:             t.Context(),
		client:          &opencode.Client{BaseURL: srv.URL},
		viewStack:       vs,
		mcpStatusBox:    mcpBox,
		mcpStatusLabel:  mcpLbl,
		tabs:            make(map[string]*sessionTab),
		childToRoot:     make(map[string]string),
		visibleSessions: make(map[string]bool),
		projectSessions: make(map[string][]opencode.Session),
	}

	uiBuilder := gtk.NewBuilderFromString(windowUI)
	uiBranch := uiBuilder.GetObject("branchStatusBox").Cast().(*gtk.Box)
	uiMcp := uiBuilder.GetObject("mcpStatusBox").Cast().(*gtk.Box)
	if prev := uiMcp.PrevSibling(); prev == nil || gtk.BaseWidget(prev).Object.Native() != uiBranch.Object.Native() {
		t.Fatalf("expected mcpStatusBox immediately after branchStatusBox, got %v", prev)
	}
	if next := uiMcp.NextSibling(); next == nil || !gtk.BaseWidget(next).HExpand() {
		t.Fatalf("expected hexpand spacer immediately after mcpStatusBox, got %v", next)
	}

	// 1. Direct chip formatting states.
	win.mcpByDir = map[string][]opencode.McpServer{
		normalizeProjectDir(dirA): {},
	}
	win.defaultServerDir = normalizeProjectDir(dirA)
	win.refreshMcpStatusChip()
	if mcpBox.Visible() {
		t.Fatalf("expected mcpStatusBox hidden when 0 servers")
	}

	win.mcpByDir[normalizeProjectDir(dirA)] = []opencode.McpServer{
		{Name: "a", Status: opencode.McpStatus{Status: "connected"}},
	}
	win.refreshMcpStatusChip()
	if !mcpBox.Visible() || mcpLbl.Text() != "1 MCP" || !mcpBox.HasCSSClass("glup-status-mcp-ok") {
		t.Fatalf("single-connected chip = visible:%v text:%q ok:%v", mcpBox.Visible(), mcpLbl.Text(), mcpBox.HasCSSClass("glup-status-mcp-ok"))
	}

	win.mcpByDir[normalizeProjectDir(dirA)] = []opencode.McpServer{
		{Name: "a", Status: opencode.McpStatus{Status: "connected"}},
		{Name: "b", Status: opencode.McpStatus{Status: "connected"}},
	}
	win.refreshMcpStatusChip()
	if !mcpBox.Visible() || mcpLbl.Text() != "2 MCPs" || !mcpBox.HasCSSClass("glup-status-mcp-ok") {
		t.Fatalf("all-connected chip = visible:%v text:%q ok:%v", mcpBox.Visible(), mcpLbl.Text(), mcpBox.HasCSSClass("glup-status-mcp-ok"))
	}

	win.mcpByDir[normalizeProjectDir(dirA)] = []opencode.McpServer{
		{Name: "a", Status: opencode.McpStatus{Status: "connected"}},
		{Name: "b", Status: opencode.McpStatus{Status: "failed"}},
	}
	win.refreshMcpStatusChip()
	if mcpLbl.Text() != "1/2 MCPs · 1 err" || !mcpBox.HasCSSClass("glup-status-mcp-err") {
		t.Fatalf("failed chip = text:%q err:%v", mcpLbl.Text(), mcpBox.HasCSSClass("glup-status-mcp-err"))
	}

	win.mcpByDir[normalizeProjectDir(dirA)] = []opencode.McpServer{
		{Name: "a", Status: opencode.McpStatus{Status: "connected"}},
		{Name: "b", Status: opencode.McpStatus{Status: "needs_auth"}},
	}
	win.refreshMcpStatusChip()
	if mcpLbl.Text() != "1/2 MCPs · auth" || !mcpBox.HasCSSClass("glup-status-mcp-warn") {
		t.Fatalf("needs_auth chip = text:%q warn:%v", mcpLbl.Text(), mcpBox.HasCSSClass("glup-status-mcp-warn"))
	}

	win.mcpByDir[normalizeProjectDir(dirA)] = []opencode.McpServer{
		{Name: "a", Status: opencode.McpStatus{Status: "disabled"}},
		{Name: "b", Status: opencode.McpStatus{Status: "disabled"}},
	}
	win.refreshMcpStatusChip()
	if mcpLbl.Text() != "0/2 MCPs" || !mcpBox.HasCSSClass("glup-status-mcp-dim") {
		t.Fatalf("disabled chip = text:%q dim:%v", mcpLbl.Text(), mcpBox.HasCSSClass("glup-status-mcp-dim"))
	}

	win.mcpByDir[normalizeProjectDir(dirA)] = []opencode.McpServer{
		{Name: "a", Status: opencode.McpStatus{Status: "connected"}},
		{Name: "b", Status: opencode.McpStatus{Status: "pending"}},
	}
	win.refreshMcpStatusChip()
	if mcpLbl.Text() != "1/2 MCPs" {
		t.Fatalf("partial chip = %q, want '1/2 MCPs'", mcpLbl.Text())
	}

	// 2. Greeter (tab == nil): updateBottomBar(nil) triggers ListMCP("") -> resolves defaultServerDir.
	win.mcpByDir = make(map[string][]opencode.McpServer)
	win.mcpLoadedDirs = make(map[string]bool)
	win.defaultServerDir = ""
	win.updateBottomBar(nil)
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return win.defaultServerDir == normalizeProjectDir(dirA) && mcpLbl.Text() == "2 MCPs"
	})

	// 3. Greeter SSE matching via defaultServerDir.
	mu.Lock()
	dirAServers = []opencode.McpServer{
		{Name: "notes", Status: opencode.McpStatus{Status: "connected"}},
		{Name: "search", Status: opencode.McpStatus{Status: "failed", Error: "timeout"}},
	}
	mu.Unlock()
	win.handleEvent(opencode.Event{
		Type:     "mcp.status.changed",
		Location: opencode.Location{Directory: dirA},
		Data:     json.RawMessage(`{"server":"search"}`),
	})
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return mcpLbl.Text() == "1/2 MCPs · 1 err"
	})

	// 4. Suspended background directory SSE should NOT issue a fetch, only invalidate cache.
	suspKey := normalizeProjectDir(dirSuspended)
	win.mcpLoadedDirs[suspKey] = true
	mu.Lock()
	reqBefore := len(mcpRequests)
	mu.Unlock()
	win.handleEvent(opencode.Event{
		Type:     "mcp.status.changed",
		Location: opencode.Location{Directory: dirSuspended},
		Data:     json.RawMessage(`{"server":"susp"}`),
	})
	time.Sleep(30 * time.Millisecond)
	glib.MainContextDefault().Iteration(false)
	mu.Lock()
	reqAfter := len(mcpRequests)
	mu.Unlock()
	if reqAfter != reqBefore {
		t.Fatalf("expected SSE for suspended dir not to trigger HTTP request (before=%d, after=%d)", reqBefore, reqAfter)
	}
	if win.mcpLoadedDirs[suspKey] {
		t.Fatalf("expected mcpLoadedDirs[%q] to be invalidated on background SSE", suspKey)
	}

	// 5. Tab selection + /cd (applySessionMoved) updates chip to new directory.
	sess := opencode.Session{
		ID:       "ses_mcp_1",
		Title:    "Test Session",
		Location: opencode.Location{Directory: dirA},
	}
	win.allSessions = []opencode.Session{sess}
	win.projectSessions[normalizeProjectDir(dirA)] = []opencode.Session{sess}
	win.ensureSessionTab(sess)
	vs.SetVisibleChildName(sess.ID)
	win.onVisibleTabChanged()
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return mcpLbl.Text() == "1/2 MCPs · 1 err"
	})

	win.applySessionMoved(sess.ID, dirB)
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return mcpLbl.Text() == "0/1 MCP · 1 err"
	})
}

func newTestMcpSheetWindow(t *testing.T, srvURL, logDir string) (*Window, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	builder := gtk.NewBuilderFromString(windowUI)
	adwWin := newTestApplicationWindow(t)
	bottomSheet := builder.GetObject("bottomSheet").Cast().(*adw.BottomSheet)
	mcpStatusBox := builder.GetObject("mcpStatusBox").Cast().(*gtk.Box)
	vs := builder.GetObject("viewStack").Cast().(*adw.ViewStack)
	rootContent := builder.GetObject("rootContent").Cast().(*adw.OverlaySplitView)
	adwWin.SetContent(rootContent)

	win := &Window{
		ApplicationWindow:   adwWin,
		ctx:                 ctx,
		client:              &opencode.Client{BaseURL: srvURL},
		statePath:           filepath.Join(t.TempDir(), "state.json"),
		rootContent:         rootContent,
		bottomSheet:         bottomSheet,
		viewStack:           vs,
		inputBuffer:         gtksource.NewBuffer(nil),
		attachmentTray:      adw.NewWrapBox(),
		mcpStatusBox:        mcpStatusBox,
		mcpStatusLabel:      builder.GetObject("mcpStatusLabel").Cast().(*gtk.Label),
		shellStatusBox:      builder.GetObject("shellStatusBox").Cast().(*gtk.Box),
		shellStatusLabel:    builder.GetObject("shellStatusLabel").Cast().(*gtk.Label),
		subagentStatusBox:   builder.GetObject("subagentStatusBox").Cast().(*gtk.Box),
		subagentStatusLabel: builder.GetObject("subagentStatusLabel").Cast().(*gtk.Label),
		sheetToastOverlay:   builder.GetObject("sheetToastOverlay").Cast().(*adw.ToastOverlay),
		sheetNavView:        builder.GetObject("sheetNavView").Cast().(*adw.NavigationView),
		sheetOverviewPage:   builder.GetObject("sheetOverviewPage").Cast().(*adw.NavigationPage),
		sheetEmptyLabel:     builder.GetObject("sheetEmptyLabel").Cast().(*gtk.Label),
		shellsSectionBox:    builder.GetObject("shellsSectionBox").Cast().(*gtk.Box),
		shellListBox:        builder.GetObject("shellListBox").Cast().(*gtk.ListBox),
		subagentsSectionBox: builder.GetObject("subagentsSectionBox").Cast().(*gtk.Box),
		subagentListBox:     builder.GetObject("subagentListBox").Cast().(*gtk.ListBox),
		mcpSectionBox:       builder.GetObject("mcpSectionBox").Cast().(*gtk.Box),
		mcpDirLabel:         builder.GetObject("mcpDirLabel").Cast().(*gtk.Label),
		mcpAllLogsBtn:       builder.GetObject("mcpAllLogsBtn").Cast().(*gtk.Button),
		mcpRefreshBtn:       builder.GetObject("mcpRefreshBtn").Cast().(*gtk.Button),
		mcpErrBanner:        builder.GetObject("mcpErrBanner").Cast().(*adw.Banner),
		mcpSearchEntry:      builder.GetObject("mcpSearchEntry").Cast().(*gtk.SearchEntry),
		mcpListStack:        builder.GetObject("mcpListStack").Cast().(*gtk.Stack),
		mcpListBox:          builder.GetObject("mcpListBox").Cast().(*gtk.ListBox),
		contextSectionBox:   builder.GetObject("contextSectionBox").Cast().(*gtk.Box),
		contextInfoGrid:     builder.GetObject("contextInfoGrid").Cast().(*gtk.Grid),
		tabs:                make(map[string]*sessionTab),
		childToRoot:         make(map[string]string),
		visibleSessions:     make(map[string]bool),
		projectSessions:     make(map[string][]opencode.Session),
		activeShells:        make(map[string]opencode.ShellItem),
		shellOutputs:        make(map[string]string),
		shellOutputLabels:   make(map[string]*gtk.Label),
		mcpLogDir:           logDir,
	}
	win.initMcpSheet()
	return win, cancel
}

func TestMcpSlashCompletionAndDispatch(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}
	adw.Init()

	hasMcp := false
	for _, b := range builtinSlashCommands {
		if b.name == "/mcp" {
			hasMcp = true
			break
		}
	}
	if !hasMcp {
		t.Fatalf("expected builtinSlashCommands to include '/mcp', got %+v", builtinSlashCommands)
	}

	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/mcp":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"location": map[string]string{"directory": dir},
				"data": []opencode.McpServer{
					{Name: "notes", Status: opencode.McpStatus{Status: "connected"}},
				},
			})
		case "/api/mcp/resource":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"location": map[string]string{"directory": dir},
				"data":     map[string]any{"resources": []any{}, "templates": []any{}},
			})
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	pending := []opencode.FileAttachment{{Mime: "text/plain", Name: "keep.txt", Data: "aGVsbG8="}}
	win, _ := newTestMcpSheetWindow(t, srv.URL, t.TempDir())
	win.pendingFiles = append([]opencode.FileAttachment(nil), pending...)

	// 1. tab == nil (greeter): /mcp opens bottomSheet on overview page and preserves pendingFiles.
	win.inputBuffer.SetText("/mcp")
	win.sendPromptWithDelivery(opencode.DeliveryQueue)
	if !win.bottomSheet.Open() || win.mcpMgr == nil || !win.mcpMgr.sheetOpen {
		t.Fatalf("expected /mcp on greeter to open bottomSheet")
	}
	if win.sheetNavView.VisiblePageTag() != "overview" {
		t.Fatalf("expected sheetNavView visible page 'overview', got %q", win.sheetNavView.VisiblePageTag())
	}
	if len(win.pendingFiles) != 1 || win.pendingFiles[0].Name != "keep.txt" {
		t.Fatalf("expected pendingFiles preserved on /mcp greeter dispatch, got %+v", win.pendingFiles)
	}
	start, end := win.inputBuffer.Bounds()
	if got := win.inputBuffer.Text(start, end, false); got != "" {
		t.Fatalf("expected inputBuffer cleared after /mcp, got %q", got)
	}
	win.bottomSheet.SetOpen(false)
	pumpMainLoopUntil(t, time.Second, func() bool {
		return !win.bottomSheet.Open() && !win.mcpMgr.sheetOpen
	})

	// 2. tab != nil: /mcps notes opens bottomSheet and pushes detail page for "notes", preserving pendingFiles.
	sess := opencode.Session{
		ID:       "ses_slash_1",
		Title:    "Slash Session",
		Location: opencode.Location{Directory: dir},
	}
	win.allSessions = []opencode.Session{sess}
	win.projectSessions[normalizeProjectDir(dir)] = []opencode.Session{sess}
	win.ensureSessionTab(sess)
	win.viewStack.SetVisibleChildName(sess.ID)

	win.inputBuffer.SetText("/mcps notes")
	win.sendPromptWithDelivery(opencode.DeliveryQueue)
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return win.bottomSheet.Open() && win.mcpMgr.detail != nil && win.mcpMgr.detail.serverName == "notes" && win.sheetNavView.VisiblePageTag() == "detail"
	})
	if len(win.pendingFiles) != 1 || win.pendingFiles[0].Name != "keep.txt" {
		t.Fatalf("expected pendingFiles preserved on /mcps tab dispatch, got %+v", win.pendingFiles)
	}

	win.bottomSheet.SetOpen(false)
	pumpMainLoopUntil(t, time.Second, func() bool {
		return !win.bottomSheet.Open() && !win.mcpMgr.sheetOpen && win.mcpMgr.detail == nil && win.sheetNavView.VisiblePageTag() == "overview"
	})
}

func TestMcpSheetLifecycleMarkupAndLogs(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}
	adw.Init()

	dir := t.TempDir()
	dirOther := t.TempDir()
	logDir := t.TempDir()
	logFile := filepath.Join(logDir, "opencode.log")

	// Seed > mcpMaxLogEntries entries for "notes", including XML/markup characters < > &.
	var logSeed strings.Builder
	for i := 0; i < mcpMaxLogEntries+10; i++ {
		lvl := "INFO"
		if i%2 == 1 {
			lvl = "WARN"
		}
		fmt.Fprintf(&logSeed, "timestamp=2026-11-16T10:00:%02dZ level=%s message=\"tick <#%d> & ok\" server=notes\n", i%60, lvl, i)
	}
	if err := os.WriteFile(logFile, []byte(logSeed.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	var (
		mu             sync.Mutex
		connectCalls   []string
		disconnCalls   []string
		configRequests atomic.Int32
		resRequests    atomic.Int32
		serversState   = []opencode.McpServer{
			{Name: "notes", Title: "Notes <Wiki> & Docs", Status: opencode.McpStatus{Status: "connected"}},
			{Name: "broken", Status: opencode.McpStatus{Status: "failed", Error: "spawn <bin> & failed\ndetail line"}},
			{Name: "authsrv", Status: opencode.McpStatus{Status: "needs_auth", Error: "OAuth <token> expired"}},
			{Name: "sleeping", Status: opencode.McpStatus{Status: "disabled"}},
		}
		blockConnect chan struct{}
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/mcp":
			reqDir := r.URL.Query().Get("location[directory]")
			if normalizeProjectDir(reqDir) == normalizeProjectDir(dirOther) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"location": map[string]string{"directory": dirOther},
					"data":     []opencode.McpServer{},
				})
				return
			}
			mu.Lock()
			copyServers := append([]opencode.McpServer(nil), serversState...)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"location": map[string]string{"directory": dir},
				"data":     copyServers,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/config":
			configRequests.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{
						"type": "document",
						"path": filepath.Join(dir, "opencode.json"),
						"info": map[string]any{
							"mcp": map[string]any{
								"servers": map[string]any{
									"notes": map[string]any{
										"type":        "local",
										"command":     []string{"npx", "-y", "notes-mcp", "<arg&val>"},
										"environment": map[string]string{"API_KEY": "super-secret-value"},
									},
								},
							},
						},
					},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/mcp/resource":
			resRequests.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"location": map[string]string{"directory": dir},
				"data": map[string]any{
					"resources": []map[string]string{
						{"server": "notes", "name": "Doc <1> & Main", "uri": "notes://doc/<1>&a=b", "mimeType": "text/markdown"},
					},
					"templates": []map[string]string{},
				},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/connect"):
			mu.Lock()
			waitCh := blockConnect
			mu.Unlock()
			if waitCh != nil {
				<-waitCh
			}
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			srvName := parts[len(parts)-2]
			mu.Lock()
			connectCalls = append(connectCalls, srvName)
			for i := range serversState {
				if serversState[i].Name == srvName {
					serversState[i].Status = opencode.McpStatus{Status: "connected"}
				}
			}
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/disconnect"):
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			srvName := parts[len(parts)-2]
			mu.Lock()
			disconnCalls = append(disconnCalls, srvName)
			for i := range serversState {
				if serversState[i].Name == srvName {
					serversState[i].Status = opencode.McpStatus{Status: "disabled"}
				}
			}
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	var copied string
	win, _ := newTestMcpSheetWindow(t, srv.URL, logDir)
	win.clipboardSetter = func(s string) {
		copied = s
	}
	win.defaultServerDir = dir

	// 0. Lazy fetch guard: SSE while bottomSheet is closed never fetches config or resources.
	win.handleEvent(opencode.Event{
		Type:     "mcp.status.changed",
		Location: opencode.Location{Directory: dir},
		Data:     json.RawMessage(`{"server":"notes"}`),
	})
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return win.mcpLoadedDirs[normalizeProjectDir(dir)]
	})
	if configRequests.Load() != 0 || resRequests.Load() != 0 {
		t.Fatalf("expected 0 config/resource requests while bottomSheet closed, got config=%d res=%d",
			configRequests.Load(), resRequests.Load())
	}

	// Open bottom sheet casually (as statusBarBox click does) -> shows MCP section since servers > 0.
	win.openBottomSheet(false, "")
	mgr := win.mcpMgr
	if mgr == nil || !win.bottomSheet.Open() {
		t.Fatal("expected bottomSheet to open")
	}
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return mgr.hasLoadedOnce && len(mgr.rows) == 4 && len(mgr.configs) == 1
	})
	if !win.mcpSectionBox.Visible() {
		t.Fatalf("expected mcpSectionBox visible when servers > 0")
	}

	// 1. Verify syncingRow guard: programmatic updates never trigger connect/disconnect.
	mu.Lock()
	serversState[0].Status = opencode.McpStatus{Status: "disabled"}
	mu.Unlock()
	mgr.applyServers(append([]opencode.McpServer(nil), serversState...), "")
	glib.MainContextDefault().Iteration(false)
	mu.Lock()
	if len(connectCalls) != 0 || len(disconnCalls) != 0 {
		mu.Unlock()
		t.Fatalf("expected 0 API calls during programmatic syncServerRow, got connect=%v disconn=%v", connectCalls, disconnCalls)
	}
	serversState[0].Status = opencode.McpStatus{Status: "connected"}
	mu.Unlock()
	mgr.applyServers(append([]opencode.McpServer(nil), serversState...), "")

	// 2. Verify Pango markup disabled + literal < & preserved on rows.
	brokenRow := mgr.rows["broken"]
	if brokenRow == nil || brokenRow.actionRow.UseMarkup() {
		t.Fatalf("expected broken row UseMarkup() == false")
	}
	if !strings.Contains(brokenRow.actionRow.Subtitle(), "spawn <bin> & failed") {
		t.Fatalf("broken row subtitle = %q", brokenRow.actionRow.Subtitle())
	}

	// 3. Push Page 2 for "authsrv" -> verify deferred needs_auth hints & Copy Error button.
	mgr.openDetailPage("authsrv")
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return mgr.detail != nil && mgr.detail.serverName == "authsrv" && mgr.logPollActive && win.sheetNavView.VisiblePageTag() == "detail"
	})
	d := mgr.detail
	if d.statusRow.UseMarkup() {
		t.Fatalf("expected detail statusRow UseMarkup() == false")
	}
	if !strings.Contains(d.statusRow.Subtitle(), "Sign in via OpenCode CLI or web UI") {
		t.Fatalf("expected needs_auth guidance in subtitle, got %q", d.statusRow.Subtitle())
	}
	if d.loginBtn.Visible() || d.restartLbl.Text() != "Retry" || !d.restartBtn.Visible() || !d.stopBtn.Visible() || d.startBtn.Visible() {
		t.Fatalf("unexpected needs_auth buttons: login=%v start=%v restart=%v(%q) stop=%v",
			d.loginBtn.Visible(), d.startBtn.Visible(), d.restartBtn.Visible(), d.restartLbl.Text(), d.stopBtn.Visible())
	}
	d.copyErrBtn.Emit("clicked")
	if copied != "OAuth <token> expired" {
		t.Fatalf("copyErrBtn copied %q, want %q", copied, "OAuth <token> expired")
	}

	// Pop Page 2 -> verify ConnectPopped stops poller and clears detail.
	mgr.navView.Pop()
	pumpMainLoopUntil(t, time.Second, func() bool {
		return mgr.detail == nil && !mgr.logPollActive && win.sheetNavView.VisiblePageTag() == "overview"
	})

	// 4. Push Page 2 for "notes" -> verify config redaction, resources, lifecycle locking, sizing, and log tailing.
	mgr.openDetailPage("notes")
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return mgr.detail != nil && mgr.detail.serverName == "notes" && len(mgr.detail.rawEntries) == mcpMaxLogEntries
	})
	d = mgr.detail
	if d.configExpander.UseMarkup() || d.resourceExpander.UseMarkup() {
		t.Fatalf("expected expanders UseMarkup() == false")
	}
	if len(d.configRows) == 0 || len(d.resourceRows) != 1 {
		t.Fatalf("unexpected configRows=%d resourceRows=%d", len(d.configRows), len(d.resourceRows))
	}
	if d.outerScroll == nil {
		t.Fatalf("expected outerScroll on detail view to prevent bottom sheet overflow")
	}
	d.configExpander.SetExpanded(true)
	d.resourceExpander.SetExpanded(true)
	if minH, natH, _, _ := d.outerScroll.Measure(gtk.OrientationVertical, 680); minH > 510 || natH > 510 {
		t.Fatalf("detail outerScroll height with expanders open = min:%d nat:%d, want <= 510", minH, natH)
	}
	d.configExpander.SetExpanded(false)
	d.resourceExpander.SetExpanded(false)
	if !strings.Contains(d.resourceExpander.Title(), "(1)") {
		t.Fatalf("resourceExpander title = %q", d.resourceExpander.Title())
	}
	for _, r := range d.configRows {
		if strings.Contains(r.Subtitle(), "super-secret-value") {
			t.Fatalf("secret leaked into config row: %q", r.Subtitle())
		}
	}

	// Verify initial buffer capped at mcpMaxLogEntries lines.
	if len(d.displayedEntries) != mcpMaxLogEntries {
		t.Fatalf("displayedEntries = %d, want %d", len(d.displayedEntries), mcpMaxLogEntries)
	}
	s, e := d.logBuf.Bounds()
	initialText := d.logBuf.Text(s, e, false)
	if strings.Contains(initialText, "tick <#0>") || !strings.Contains(initialText, "tick <#10>") {
		t.Fatalf("expected oldest entries trimmed from initial tail (has #0=%v has #10=%v)",
			strings.Contains(initialText, "tick <#0>"), strings.Contains(initialText, "tick <#10>"))
	}

	// Append a new log entry and trigger incremental poll -> buffer trims oldest and appends new line.
	f, err := os.OpenFile(logFile, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("timestamp=2026-11-16T10:05:00Z level=ERROR message=\"live <appended> & boom\" server=notes\n")
	_ = f.Close()

	mgr.triggerLogPollOnce()
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		s, e := d.logBuf.Bounds()
		return strings.Contains(d.logBuf.Text(s, e, false), "live <appended> & boom")
	})
	if len(d.displayedEntries) != mcpMaxLogEntries {
		t.Fatalf("displayedEntries after append = %d, want %d", len(d.displayedEntries), mcpMaxLogEntries)
	}
	s, e = d.logBuf.Bounds()
	if strings.Contains(d.logBuf.Text(s, e, false), "tick <#10>") {
		t.Fatalf("expected tick <#10> trimmed after incremental append")
	}

	// Level filter Error hides INFO/WARN lines.
	d.levelDrop.SetSelected(3) // Error
	pumpMainLoopUntil(t, time.Second, func() bool {
		return len(d.displayedEntries) == 1
	})
	d.levelDrop.SetSelected(0) // All Levels
	d.logSearch.SetText("<appended>")
	pumpMainLoopUntil(t, time.Second, func() bool {
		return len(d.displayedEntries) == 1
	})
	d.logSearch.SetText("")
	d.clearBtn.Emit("clicked")
	if len(d.displayedEntries) != 0 || !d.emptyLabel.Visible() || d.copyBtn.Sensitive() {
		t.Fatalf("expected clearBtn to empty buffer and disable copyBtn")
	}

	// 5. Verify in-flight locking on Start/Stop + disabled button layout.
	d.stopBtn.Emit("clicked")
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		srv, ok := mgr.findServer("notes")
		return ok && srv.Status.Status == "disabled"
	})
	if !d.startBtn.Visible() || d.restartBtn.Visible() || d.stopBtn.Visible() {
		t.Fatalf("expected Start visible & Restart/Stop hidden when disabled")
	}

	// Hold connect blocked to verify in-flight locking.
	var gateOnce sync.Once
	blocked := make(chan struct{})
	mu.Lock()
	blockConnect = blocked
	mu.Unlock()
	defer gateOnce.Do(func() { close(blocked) })
	d.startBtn.Emit("clicked")
	if !win.mcpInFlight[mcpFlightKey(mgr.dir, "notes")] {
		t.Fatalf("expected mcpInFlight set while ConnectMCP running")
	}
	if d.startBtn.Sensitive() {
		t.Fatalf("expected startBtn insensitive while inFlight")
	}
	gateOnce.Do(func() { close(blocked) })
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		srv, ok := mgr.findServer("notes")
		return ok && srv.Status.Status == "connected" && !win.mcpInFlight[mcpFlightKey(mgr.dir, "notes")]
	})
	if d.startBtn.Visible() || !d.restartBtn.Visible() || !d.stopBtn.Visible() {
		t.Fatalf("expected Restart/Stop visible & Start hidden once connected")
	}

	// 6. Remote non-loopback server disables local log tailing, and All MCP Logs uses taller logScroll.
	win.client = &opencode.Client{BaseURL: "https://opencode.remote.example:4096"}
	mgr.openDetailPage("")
	if mgr.bodyStackVisible() != "remote" {
		t.Fatalf("expected remote bodyStack page for non-loopback server, got %q", mgr.bodyStackVisible())
	}
	if mgr.detail.scrolled.MinContentHeight() != 360 {
		t.Fatalf("All MCP Logs logScroll min content height = %d, want 360", mgr.detail.scrolled.MinContentHeight())
	}

	// 7. Closing bottomSheet cleans up poller, clears detail, resets navView back to overview, and resets explicitOpen.
	win.bottomSheet.SetOpen(false)
	pumpMainLoopUntil(t, time.Second, func() bool {
		return !win.bottomSheet.Open() && !mgr.sheetOpen && !mgr.logPollActive && mgr.detail == nil && win.sheetNavView.VisiblePageTag() == "overview" && !mgr.explicitOpen
	})

	// 8. Directory with 0 servers hides mcpSectionBox on casual open, shows on explicit open.
	win.client = &opencode.Client{BaseURL: srv.URL}
	sessOther := opencode.Session{
		ID:       "ses_empty_mcp",
		Title:    "Empty MCP Dir",
		Location: opencode.Location{Directory: dirOther},
	}
	win.allSessions = []opencode.Session{sessOther}
	win.projectSessions[normalizeProjectDir(dirOther)] = []opencode.Session{sessOther}
	win.ensureSessionTab(sessOther)
	win.viewStack.SetVisibleChildName(sessOther.ID)

	win.openBottomSheet(false, "")
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return mgr.hasLoadedOnce && normalizeProjectDir(mgr.dir) == normalizeProjectDir(dirOther)
	})
	if win.mcpSectionBox.Visible() {
		t.Fatalf("expected mcpSectionBox hidden when 0 servers on casual open")
	}
	win.toggleMcpSheet() // explicit open while bottomSheet open and mcpSectionBox hidden -> reveals MCP empty state
	if !win.bottomSheet.Open() || !win.mcpSectionBox.Visible() || win.mcpListStack.VisibleChildName() != "empty" {
		t.Fatalf("expected toggleMcpSheet to reveal empty MCP section (open=%v section=%v stack=%q)",
			win.bottomSheet.Open(), win.mcpSectionBox.Visible(), win.mcpListStack.VisibleChildName())
	}
	win.toggleMcpSheet() // second toggle closes bottomSheet
	pumpMainLoopUntil(t, time.Second, func() bool {
		return !win.bottomSheet.Open() && !mgr.sheetOpen
	})

	// 9. Keyboard shortcut & Escape fallback helpers: Ctrl+Shift+M opens/closes; Escape pops detail then closes sheet.
	if win.handleMcpToggleKey(gdk.KEY_m, gdk.ControlMask) {
		t.Fatalf("expected Ctrl+M without Shift not to trigger MCP toggle")
	}
	if !win.handleMcpToggleKey(gdk.KEY_M, gdk.ControlMask|gdk.ShiftMask) || !win.bottomSheet.Open() {
		t.Fatalf("expected Ctrl+Shift+M to open bottomSheet")
	}
	mgr.openDetailPage("")
	if win.sheetNavView.VisiblePageTag() != "detail" {
		t.Fatalf("expected detail page pushed, got %q", win.sheetNavView.VisiblePageTag())
	}
	if !win.handleBottomSheetEscape() {
		t.Fatalf("expected Escape fallback to pop detail page")
	}
	pumpMainLoopUntil(t, time.Second, func() bool {
		return win.bottomSheet.Open() && mgr.detail == nil && win.sheetNavView.VisiblePageTag() == "overview"
	})
	if !win.handleBottomSheetEscape() {
		t.Fatalf("expected second Escape fallback to close bottomSheet")
	}
	pumpMainLoopUntil(t, time.Second, func() bool {
		return !win.bottomSheet.Open() && !mgr.sheetOpen
	})
	if !win.handleMcpToggleKey(gdk.KEY_m, gdk.ControlMask|gdk.ShiftMask) || !win.bottomSheet.Open() {
		t.Fatalf("expected Ctrl+Shift+m to re-open bottomSheet")
	}
	if !win.handleMcpToggleKey(gdk.KEY_M, gdk.ControlMask|gdk.ShiftMask) {
		t.Fatalf("expected Ctrl+Shift+M to close bottomSheet")
	}
	pumpMainLoopUntil(t, time.Second, func() bool {
		return !win.bottomSheet.Open() && !mgr.sheetOpen
	})
}

func (m *mcpManager) bodyStackVisible() string {
	if m.detail == nil || m.detail.bodyStack == nil {
		return ""
	}
	return m.detail.bodyStack.VisibleChildName()
}

func TestMcpOAuthDialogFlows(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}
	adw.Init()

	dir := t.TempDir()

	type scenarioCfg struct {
		mode           string
		paste          bool
		authURL        string
		instructions   string
		lifetimeMS     int64
		pollStatuses   []opencode.OAuthAttemptStatus
		completeErr    string
		postAuthStatus string
	}

	var (
		mu             sync.Mutex
		scenario       scenarioCfg
		serversState   []opencode.McpServer
		launchedURIs   []string
		launchErr      error
		copiedText     string
		startCount     int
		statusPollCnt  int
		statusPolls    map[string]int
		completeCodes  []string
		cancelAttempts []string
		connectCalls   []string
		blockStart     chan struct{}
	)

	resetState := func(srv []opencode.McpServer, sc scenarioCfg) {
		mu.Lock()
		defer mu.Unlock()
		serversState = srv
		scenario = sc
		launchedURIs = nil
		launchErr = nil
		copiedText = ""
		startCount = 0
		statusPollCnt = 0
		statusPolls = make(map[string]int)
		completeCodes = nil
		cancelAttempts = nil
		connectCalls = nil
		blockStart = nil
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/mcp":
			mu.Lock()
			copyServers := append([]opencode.McpServer(nil), serversState...)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"location": map[string]string{"directory": dir},
				"data":     copyServers,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/mcp/resource":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"location": map[string]string{"directory": dir},
				"data":     map[string]any{"resources": []any{}, "templates": []any{}},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/connect") && strings.HasPrefix(r.URL.Path, "/api/experimental/mcp/"):
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			srvName := parts[len(parts)-2]
			mu.Lock()
			connectCalls = append(connectCalls, srvName)
			for i := range serversState {
				if serversState[i].Name == srvName {
					serversState[i].Status = opencode.McpStatus{Status: "connected"}
				}
			}
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/api/integration/mcp_123":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"location": map[string]string{"directory": dir},
				"data": opencode.IntegrationInfo{
					ID:   "mcp_123",
					Name: "oauthsrv",
					Methods: []opencode.IntegrationMethod{
						{ID: "meth_oauth", Type: "oauth", Label: "oauthsrv"},
					},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/integration/mcp_123/connect/oauth":
			mu.Lock()
			waitCh := blockStart
			startCount++
			n := startCount
			sc := scenario
			mu.Unlock()
			if waitCh != nil {
				<-waitCh
			}
			exp := int64(60_000)
			if sc.lifetimeMS > 0 {
				exp = sc.lifetimeMS
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"location": map[string]string{"directory": dir},
				"data": opencode.OAuthAttempt{
					AttemptID:    fmt.Sprintf("att_%d", n),
					URL:          sc.authURL,
					Instructions: sc.instructions,
					Mode:         sc.mode,
					Paste:        sc.paste,
					Time:         opencode.OAuthAttemptTime{Created: 1000, Expires: 1000 + exp},
				},
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/integration/mcp_123/connect/oauth/att_"):
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			attID := parts[len(parts)-1]
			mu.Lock()
			statusPollCnt++
			if statusPolls == nil {
				statusPolls = make(map[string]int)
			}
			statusPolls[attID]++
			idx := statusPollCnt - 1
			sc := scenario
			var st opencode.OAuthAttemptStatus
			if len(sc.pollStatuses) > 0 {
				if idx >= len(sc.pollStatuses) {
					st = sc.pollStatuses[len(sc.pollStatuses)-1]
				} else {
					st = sc.pollStatuses[idx]
				}
			} else {
				st = opencode.OAuthAttemptStatus{Status: "pending"}
			}
			if st.Status == "complete" && sc.postAuthStatus != "" {
				for i := range serversState {
					if serversState[i].Name == "oauthsrv" {
						serversState[i].Status = opencode.McpStatus{Status: sc.postAuthStatus}
					}
				}
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"location": map[string]string{"directory": dir},
				"data":     st,
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/complete"):
			var body struct {
				Code string `json:"code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			completeCodes = append(completeCodes, body.Code)
			errMsg := scenario.completeErr
			if errMsg == "" {
				scenario.pollStatuses = []opencode.OAuthAttemptStatus{{Status: "complete"}}
			}
			mu.Unlock()
			if errMsg != "" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": errMsg})
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/integration/mcp_123/connect/oauth/att_"):
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			attID := parts[len(parts)-1]
			mu.Lock()
			cancelAttempts = append(cancelAttempts, attID)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	win, _ := newTestMcpSheetWindow(t, srv.URL, t.TempDir())
	win.defaultServerDir = dir
	win.clipboardSetter = func(s string) {
		mu.Lock()
		copiedText = s
		mu.Unlock()
	}
	win.uriLauncher = func(u string) error {
		mu.Lock()
		launchedURIs = append(launchedURIs, u)
		err := launchErr
		mu.Unlock()
		return err
	}

	// 1. Detail view Log In button visibility + auto/paste flow completing via poll + ConnectMCP fallback.
	resetState([]opencode.McpServer{
		{Name: "oauthsrv", IntegrationID: "mcp_123", Status: opencode.McpStatus{Status: "needs_auth"}},
		{Name: "nooauth", Status: opencode.McpStatus{Status: "needs_auth"}},
	}, scenarioCfg{
		mode:         "auto",
		paste:        true,
		authURL:      "https://auth.example.com/oauth?state=s1&code_challenge=xyz",
		instructions: "Authorize <notes> & workspace",
		pollStatuses: []opencode.OAuthAttemptStatus{
			{Status: "pending"},
			{Status: "complete"},
		},
		postAuthStatus: "disabled",
	})

	win.openBottomSheet(true, "oauthsrv")
	mgr := win.mcpMgr
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return mgr.detail != nil && mgr.detail.serverName == "oauthsrv"
	})
	d := mgr.detail
	if !d.loginBtn.Visible() {
		t.Fatal("expected loginBtn visible on needs_auth server with IntegrationID")
	}
	if !strings.Contains(d.statusRow.Subtitle(), "Click Log In") {
		t.Fatalf("expected Log In hint in detail status subtitle, got %q", d.statusRow.Subtitle())
	}

	// Connected or missing-IntegrationID servers must not show loginBtn.
	mgr.openDetailPage("nooauth")
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return mgr.detail != nil && mgr.detail.serverName == "nooauth"
	})
	if mgr.detail.loginBtn.Visible() {
		t.Fatal("expected loginBtn hidden when IntegrationID is empty")
	}

	mgr.openDetailPage("oauthsrv")
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return mgr.detail != nil && mgr.detail.serverName == "oauthsrv"
	})
	d = mgr.detail

	d.loginBtn.Emit("clicked")
	if mgr.authDlg == nil {
		t.Fatal("expected authDlg opened after clicking Log In")
	}
	// Modal dialog guards Ctrl+Shift+M / sheet escape while open.
	if win.handleMcpToggleKey(gdk.KEY_M, gdk.ControlMask|gdk.ShiftMask) {
		t.Fatal("expected handleMcpToggleKey ignored while authDlg is open")
	}
	if win.handleBottomSheetEscape() {
		t.Fatal("expected handleBottomSheetEscape ignored while authDlg is open")
	}

	// Copy Link button works once waiting view appears, and poll completion closes dialog + connects server.
	pumpMainLoopUntil(t, 3*time.Second, func() bool {
		return mgr.authDlg == nil
	})
	mu.Lock()
	gotURIs := append([]string(nil), launchedURIs...)
	gotConnects := append([]string(nil), connectCalls...)
	mu.Unlock()
	if len(gotURIs) != 1 || gotURIs[0] != "https://auth.example.com/oauth?state=s1&code_challenge=xyz" {
		t.Fatalf("launchedURIs = %v", gotURIs)
	}
	if len(gotConnects) != 1 || gotConnects[0] != "oauthsrv" {
		t.Fatalf("expected ConnectMCP fallback for disabled server after auth, got %v", gotConnects)
	}
	if d.loginBtn.Visible() {
		t.Fatal("expected loginBtn hidden once server connected")
	}

	// 2. Browser launch failure, empty validation, 400 rejection (polling continues), copy link, and pasteAccepted skip-DELETE on close.
	resetState([]opencode.McpServer{
		{Name: "oauthsrv", IntegrationID: "mcp_123", Status: opencode.McpStatus{Status: "needs_auth"}},
	}, scenarioCfg{
		mode:        "auto",
		paste:       true,
		authURL:     "https://auth.example.com/oauth?state=s2",
		completeErr: "State <mismatch> & invalid",
	})
	mu.Lock()
	launchErr = errors.New("xdg-open <missing>")
	mu.Unlock()

	mgr.refreshAll()
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return d.loginBtn.Visible()
	})
	d.loginBtn.Emit("clicked")
	dlg := mgr.authDlg
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return dlg.viewStack.VisibleChildName() == "waiting"
	})
	if !dlg.browserWarnBanner.Revealed() || dlg.browserWarnBanner.UseMarkup() {
		t.Fatalf("expected browserWarnBanner revealed with UseMarkup(false), revealed=%v markup=%v",
			dlg.browserWarnBanner.Revealed(), dlg.browserWarnBanner.UseMarkup())
	}
	if dlg.waitingRow.UseMarkup() || dlg.entryErrLabel.UseMarkup() || dlg.errDetailLabel.UseMarkup() {
		t.Fatal("expected UseMarkup(false) across dialog widgets")
	}

	dlg.copyLinkBtn.Emit("clicked")
	mu.Lock()
	gotCopied := copiedText
	mu.Unlock()
	if gotCopied != "https://auth.example.com/oauth?state=s2" {
		t.Fatalf("copiedText = %q", gotCopied)
	}

	// Empty submit triggers inline validation error without POST.
	dlg.submitBtn.Emit("clicked")
	if !dlg.entryErrLabel.Visible() {
		t.Fatal("expected inline entryErrLabel for empty submit")
	}
	mu.Lock()
	if len(completeCodes) != 0 {
		mu.Unlock()
		t.Fatalf("expected 0 complete POSTs on empty submit, got %v", completeCodes)
	}
	mu.Unlock()

	// Bad redirect URL -> 400 error shown, form unlocked, polling still active.
	dlg.codeEntryRow.SetText("http://127.0.0.1:9/bad")
	dlg.submitBtn.Emit("clicked")
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return dlg.entryErrLabel.Visible() && strings.Contains(dlg.entryErrLabel.Text(), "State <mismatch> & invalid")
	})
	if !dlg.codeEntryRow.Sensitive() {
		t.Fatal("expected codeEntryRow re-enabled after 400 rejection")
	}
	mu.Lock()
	pollsAfterErr := statusPollCnt
	mu.Unlock()
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return statusPollCnt > pollsAfterErr
	})

	// Clear error, accept paste -> keep status pending and close dialog while pasteAccepted == true -> no DELETE sent.
	mu.Lock()
	scenario.completeErr = ""
	scenario.pollStatuses = []opencode.OAuthAttemptStatus{{Status: "pending"}}
	mu.Unlock()
	mu.Lock()
	pollsBeforePaste := statusPolls[dlg.attemptID]
	mu.Unlock()
	dlg.codeEntryRow.SetText("http://127.0.0.1:9/ok?code=123")
	dlg.submitBtn.Emit("clicked")
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return dlg.pasteAccepted && !dlg.codeEntryRow.Sensitive() && statusPolls[dlg.attemptID] > pollsBeforePaste
	})
	dlg.close()
	pumpMainLoopUntil(t, time.Second, func() bool {
		return mgr.authDlg == nil
	})
	mu.Lock()
	if len(cancelAttempts) != 0 {
		mu.Unlock()
		t.Fatalf("expected 0 DELETE cancels when closing after pasteAccepted, got %v", cancelAttempts)
	}
	mu.Unlock()

	// 3. Code mode (no polling while waiting) -> submit succeeds -> syncs & closes.
	resetState([]opencode.McpServer{
		{Name: "oauthsrv", IntegrationID: "mcp_123", Status: opencode.McpStatus{Status: "needs_auth"}},
	}, scenarioCfg{
		mode:           "code",
		paste:          false,
		authURL:        "https://auth.example.com/device",
		postAuthStatus: "connected",
	})
	mgr.refreshAll()
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return d.loginBtn.Visible()
	})
	d.loginBtn.Emit("clicked")
	dlg = mgr.authDlg
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return dlg.viewStack.VisibleChildName() == "waiting"
	})
	if !dlg.formGroup.Visible() {
		t.Fatal("expected formGroup visible in code mode")
	}
	mu.Lock()
	codePolls := statusPolls[dlg.attemptID]
	mu.Unlock()
	if codePolls != 0 {
		t.Fatalf("expected 0 background status polls while waiting in code mode, got %d", codePolls)
	}
	dlg.codeEntryRow.SetText("AUTH-CODE-999")
	dlg.submitBtn.Emit("clicked")
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return mgr.authDlg == nil
	})
	mu.Lock()
	if len(completeCodes) != 1 || completeCodes[0] != "AUTH-CODE-999" {
		mu.Unlock()
		t.Fatalf("completeCodes = %v, want [AUTH-CODE-999]", completeCodes)
	}
	mu.Unlock()

	// 4. Unsafe URL rejected without launching browser, and server attempt cancelled immediately.
	resetState([]opencode.McpServer{
		{Name: "oauthsrv", IntegrationID: "mcp_123", Status: opencode.McpStatus{Status: "needs_auth"}},
	}, scenarioCfg{
		mode:    "auto",
		paste:   true,
		authURL: "javascript:alert(1)",
	})
	mgr.refreshAll()
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return d.loginBtn.Visible()
	})
	d.loginBtn.Emit("clicked")
	dlg = mgr.authDlg
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return dlg.viewStack.VisibleChildName() == "error"
	})
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(cancelAttempts) == 1
	})
	mu.Lock()
	if len(launchedURIs) != 0 {
		mu.Unlock()
		t.Fatalf("unsafe URL must not invoke uriLauncher, got %v", launchedURIs)
	}
	mu.Unlock()
	dlg.close()

	// 5. Attempt expiration (80ms lifetime) -> DELETE sent, error page shown, Try Again starts fresh attempt.
	resetState([]opencode.McpServer{
		{Name: "oauthsrv", IntegrationID: "mcp_123", Status: opencode.McpStatus{Status: "needs_auth"}},
	}, scenarioCfg{
		mode:       "auto",
		paste:      false,
		authURL:    "https://auth.example.com/exp",
		lifetimeMS: 80,
	})
	d.loginBtn.Emit("clicked")
	dlg = mgr.authDlg
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return dlg.viewStack.VisibleChildName() == "error"
	})
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(cancelAttempts) == 1
	})
	mu.Lock()
	scenario.lifetimeMS = 60_000
	scenario.pollStatuses = []opencode.OAuthAttemptStatus{{Status: "complete"}}
	scenario.postAuthStatus = "connected"
	mu.Unlock()
	dlg.retryBtn.Emit("clicked")
	pumpMainLoopUntil(t, 3*time.Second, func() bool {
		return mgr.authDlg == nil
	})
	mu.Lock()
	if startCount != 2 {
		mu.Unlock()
		t.Fatalf("expected startCount == 2 after Try Again, got %d", startCount)
	}
	mu.Unlock()

	// 6. Pending close sends DELETE, and close while StartIntegrationOAuth in flight still cancels once POST returns.
	resetState([]opencode.McpServer{
		{Name: "oauthsrv", IntegrationID: "mcp_123", Status: opencode.McpStatus{Status: "needs_auth"}},
	}, scenarioCfg{
		mode:    "auto",
		paste:   false,
		authURL: "https://auth.example.com/pending",
	})
	mgr.refreshAll()
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return d.loginBtn.Visible()
	})
	d.loginBtn.Emit("clicked")
	dlg = mgr.authDlg
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		return dlg.viewStack.VisibleChildName() == "waiting"
	})
	dlg.close()
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(cancelAttempts) == 1 && cancelAttempts[0] == "att_1"
	})

	// Race-on-close while StartIntegrationOAuth is blocked.
	var unblockOnce sync.Once
	startGate := make(chan struct{})
	mu.Lock()
	blockStart = startGate
	mu.Unlock()
	defer unblockOnce.Do(func() { close(startGate) })

	d.loginBtn.Emit("clicked")
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return startCount == 2
	})
	mgr.closeAuthDialog()
	unblockOnce.Do(func() { close(startGate) })
	pumpMainLoopUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(cancelAttempts) == 2 && cancelAttempts[1] == "att_2"
	})
}
