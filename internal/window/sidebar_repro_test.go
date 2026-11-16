package window

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"libdb.so/gluppencode/internal/opencode"
)

func pumpMainLoop(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		for glib.MainContextDefault().Pending() {
			glib.MainContextDefault().Iteration(false)
		}
		time.Sleep(5 * time.Millisecond)
	}
	for glib.MainContextDefault().Pending() {
		glib.MainContextDefault().Iteration(false)
	}
}

func assertSidebarIntegrity(t *testing.T, label string, w *Window) {
	t.Helper()
	lb := w.sidebarList
	if lb == nil {
		t.Fatalf("[%s] sidebarList == nil", label)
	}

	seenHeaders := make(map[string]int)
	seenSessions := make(map[string]int)
	seenShowMore := make(map[string]int)
	var headerOrder []string
	var curDir string
	var hasCurDir bool

	prevBottom := float32(-1)
	selectedCount := 0
	var selectedSessID string

	for idx := 0; ; idx++ {
		row := lb.RowAtIndex(idx)
		if row == nil {
			break
		}
		if !row.Visible() || !row.ChildVisible() {
			t.Fatalf("[%s] row %d visible=%v childVisible=%v, want true/true", label, idx, row.Visible(), row.ChildVisible())
		}
		if rect, ok := row.ComputeBounds(lb); ok {
			if rect.Height() <= 0 {
				t.Fatalf("[%s] row %d has non-positive height %.1f", label, idx, rect.Height())
			}
			if prevBottom >= 0 && rect.Y() < prevBottom-0.5 {
				t.Fatalf("[%s] row %d overlaps previous row: y=%.1f < prevBottom=%.1f", label, idx, rect.Y(), prevBottom)
			}
			prevBottom = rect.Y() + rect.Height()
		} else {
			t.Fatalf("[%s] row %d ComputeBounds failed", label, idx)
		}

		rp := row.Object.Native()
		switch {
		case row.HasCSSClass("glup-sidebar-header-row"):
			if row.Selectable() || row.Activatable() {
				t.Fatalf("[%s] header row %d should not be selectable/activatable", label, idx)
			}
			child := row.Child()
			if child == nil {
				t.Fatalf("[%s] header row %d has nil child", label, idx)
			}
			dir := w.headerProjectDir[gtk.BaseWidget(child).Object.Native()]
			seenHeaders[dir]++
			if seenHeaders[dir] > 1 {
				t.Fatalf("[%s] duplicate header row for dir %q", label, dir)
			}
			headerOrder = append(headerOrder, dir)
			curDir = dir
			hasCurDir = true

		case row.HasCSSClass("glup-sidebar-conversation-row"):
			if !hasCurDir {
				t.Fatalf("[%s] conversation row %d appeared before any project header", label, idx)
			}
			sessID := w.sidebarRowSessID[rp]
			if sessID == "" {
				t.Fatalf("[%s] conversation row %d missing sidebarRowSessID", label, idx)
			}
			seenSessions[sessID]++
			if seenSessions[sessID] > 1 {
				t.Fatalf("[%s] duplicate conversation row for session %q", label, sessID)
			}
			tab := w.tabs[sessID]
			if tab == nil || tab.main == nil {
				t.Fatalf("[%s] conversation row %d references missing tab %q", label, idx, sessID)
			}
			tabDir := normalizeProjectDir(tab.main.session.Location.Directory)
			if tabDir != curDir {
				t.Fatalf("[%s] conversation row %d (%q) is under header %q, want %q", label, idx, sessID, curDir, tabDir)
			}
			if w.collapsedProjects[curDir] {
				t.Fatalf("[%s] collapsed project %q still shows conversation row %q", label, curDir, sessID)
			}
			if row.IsSelected() {
				selectedCount++
				selectedSessID = sessID
			}

		case row.HasCSSClass("glup-sidebar-show-more-row"):
			if !hasCurDir {
				t.Fatalf("[%s] show-more row %d appeared before any project header", label, idx)
			}
			if row.Selectable() || !row.Activatable() {
				t.Fatalf("[%s] show-more row %d should be non-selectable and activatable", label, idx)
			}
			dir := w.projectShowMoreDirs[rp]
			if dir != curDir {
				t.Fatalf("[%s] show-more row %d dir=%q under header %q", label, idx, dir, curDir)
			}
			seenShowMore[dir]++
			if seenShowMore[dir] > 1 {
				t.Fatalf("[%s] duplicate show-more row for dir %q", label, dir)
			}

		default:
			t.Fatalf("[%s] row %d has unexpected CSS classes %v", label, idx, row.CSSClasses())
		}
	}

	if selectedCount > 1 {
		t.Fatalf("[%s] expected at most 1 selected row, got %d", label, selectedCount)
	}

	curID := w.currentRootSessionID()
	curTab := w.tabs[curID]
	expectSelected := curID != "" && curTab != nil && curTab.main != nil && !w.collapsedProjects[normalizeProjectDir(curTab.main.session.Location.Directory)]
	if expectSelected && selectedSessID != curID {
		t.Fatalf("[%s] selected row = %q, want %q", label, selectedSessID, curID)
	}
	if !expectSelected && selectedCount != 0 {
		t.Fatalf("[%s] expected no selected row when current tab=%q is collapsed/missing, got %q", label, curID, selectedSessID)
	}

	// Verify header order follows projectOrder for projects present in tabOrder.
	var wantHeaders []string
	for _, id := range w.tabOrder {
		if tab := w.tabs[id]; tab != nil && tab.main != nil {
			d := normalizeProjectDir(tab.main.session.Location.Directory)
			if !slices.Contains(wantHeaders, d) {
				wantHeaders = append(wantHeaders, d)
			}
		}
	}
	if !slices.Equal(headerOrder, wantHeaders) {
		t.Fatalf("[%s] headerOrder = %v, want %v", label, headerOrder, wantHeaders)
	}
}

func TestSidebarLayoutRepro(t *testing.T) {
	if os.Getenv("GDK_BACKEND") != "broadway" {
		t.Skip("run under GDK_BACKEND=broadway to test full Present/layout bounds")
	}
	adw.Init()
	if !gtk.InitCheck() {
		t.Skip("no broadway display")
	}
	if disp := gdk.DisplayGetDefault(); disp != nil {
		prov := gtk.NewCSSProvider()
		prov.LoadFromString(styleCSS)
		gtk.StyleContextAddProviderForDisplay(disp, prov, gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
	}

	builder := gtk.NewBuilderFromString(windowUI)
	rootContent := builder.GetObject("rootContent").Cast().(*adw.OverlaySplitView)
	sidebarScroll := builder.GetObject("sidebarScroll").Cast().(*gtk.ScrolledWindow)
	sidebarList := builder.GetObject("sidebarList").Cast().(*gtk.ListBox)
	viewStack := builder.GetObject("viewStack").Cast().(*adw.ViewStack)

	win := adw.NewWindow()
	win.SetDefaultSize(1180, 780)
	win.SetContent(rootContent)

	w := &Window{
		ctx:                 t.Context(),
		rootContent:         rootContent,
		sidebarScroll:       sidebarScroll,
		sidebarList:         sidebarList,
		viewStack:           viewStack,
		windowTitle:         builder.GetObject("windowTitle").Cast().(*adw.WindowTitle),
		windowTitleBtn:      builder.GetObject("windowTitleBtn").Cast().(*gtk.MenuButton),
		tabs:                make(map[string]*sessionTab),
		childToRoot:         make(map[string]string),
		expandedProjects:    make(map[string]bool),
		collapsedProjects:   make(map[string]bool),
		visibleSessions:     make(map[string]bool),
		archivedSessions:    make(map[string]bool),
		projectSessions:     make(map[string][]opencode.Session),
		closeBtnSessID:      make(map[uintptr]string),
		hoveredRow:          make(map[uintptr]bool),
		rowProjectDir:       make(map[uintptr]string),
		sidebarRowSessID:    make(map[uintptr]string),
		headerProjectDir:    make(map[uintptr]string),
		headerWidgets:       make(map[uintptr]*projectHeaderWidgets),
		projectHeaderRows:   make(map[string]*gtk.ListBoxRow),
		projectShowMoreRows: make(map[string]*gtk.ListBoxRow),
		projectShowMoreLbls: make(map[string]*gtk.Label),
		projectShowMoreDirs: make(map[uintptr]string),
		modelNames:          make(map[string]string),
		modelLimits:         make(map[string]int),
		branchCache:         make(map[string]string),
	}
	w.setupSidebarList()
	viewStack.NotifyProperty("visible-child-name", func() {
		w.onVisibleTabChanged()
	})

	p0 := "/home/diamond/Scripts/diamondburned/gluppencode"
	p1 := "/home/diamond/Scripts/diamondburned/agentic-ha"
	p2 := "/home/diamond/Scripts/dma/sticker-tools"
	p3 := "/home/diamond/Scripts/diamondburned/eink-server-gio"
	p4 := "/home/diamond/Scripts/diamondburned/libdb"

	w.projectOrder = []string{p0, p1, p2, p3, p4}
	w.collapsedProjects[p1] = true
	w.prefs.MaxVisibleSessions = 4

	now := time.Now().UnixMilli()
	sessions := []opencode.Session{
		{ID: "g0", Title: "Glup 0", Location: opencode.Location{Directory: p0}, Time: opencode.SessionTime{Updated: now - 1000}},
		{ID: "g1", Title: "Glup 1", Location: opencode.Location{Directory: p0}, Time: opencode.SessionTime{Updated: now - 2000}},
		{ID: "g2", Title: "Glup 2", Location: opencode.Location{Directory: p0}, Time: opencode.SessionTime{Updated: now - 3000}},
		{ID: "g3", Title: "Glup 3", Location: opencode.Location{Directory: p0}, Time: opencode.SessionTime{Updated: now - 4000}},
		{ID: "g4", Title: "Glup 4", Location: opencode.Location{Directory: p0}, Time: opencode.SessionTime{Updated: now - 4500}},
		{ID: "a0", Title: "Agentic 0", Location: opencode.Location{Directory: p1}, Time: opencode.SessionTime{Updated: now - 5000}},
		{ID: "a1", Title: "Agentic 1", Location: opencode.Location{Directory: p1}, Time: opencode.SessionTime{Updated: now - 6000}},
		{ID: "a2", Title: "Agentic 2", Location: opencode.Location{Directory: p1}, Time: opencode.SessionTime{Updated: now - 7000}},
		{ID: "a3", Title: "Agentic 3", Location: opencode.Location{Directory: p1}, Time: opencode.SessionTime{Updated: now - 8000}},
		{ID: "s0", Title: "Cricut Explore Air 3 cutting", Location: opencode.Location{Directory: p2}, Time: opencode.SessionTime{Updated: now - 9000}},
		{ID: "s1", Title: "PNG sticker outline script", Location: opencode.Location{Directory: p2}, Time: opencode.SessionTime{Updated: now - 10000}},
		{ID: "e0", Title: "4-color BMP conversion for", Location: opencode.Location{Directory: p3}, Time: opencode.SessionTime{Updated: now - 11000}},
		{ID: "e1", Title: "eink-server CLI and HTTP se", Location: opencode.Location{Directory: p3}, Time: opencode.SessionTime{Updated: now - 12000}},
		{ID: "l0", Title: "libdb 0", Location: opencode.Location{Directory: p4}, Time: opencode.SessionTime{Updated: now - 13000}},
	}
	for _, s := range sessions {
		w.visibleSessions[s.ID] = true
	}
	w.allSessions = sessions

	win.Present()
	pumpMainLoop(150 * time.Millisecond)
	if win.Width() == 0 || win.Height() == 0 {
		t.Skip("broadway surface not configured (no browser client connected)")
	}

	w.populateSessionTabs(sessions, nil)
	pumpMainLoop(150 * time.Millisecond)
	assertSidebarIntegrity(t, "after populate", w)
	if got := w.projectShowMoreLbls[p0].Text(); got != "Show 1 more…" {
		t.Fatalf("p0 show-more label = %q, want 'Show 1 more…'", got)
	}

	// Activate "Show 1 more…" row to expand p0 without losing g0 selection.
	moreRow := w.projectShowMoreRows[p0]
	if moreRow == nil {
		t.Fatal("expected p0 show-more row")
	}
	moreRow.Activate()
	pumpMainLoop(100 * time.Millisecond)
	assertSidebarIntegrity(t, "after activate show-more p0", w)
	if w.projectShowMoreRows[p0] != nil {
		t.Fatal("expected p0 show-more row retired after expansion")
	}

	// Test ensureSessionTab (new session in p2 sticker-tools)
	w.ensureSessionTab(opencode.Session{
		ID: "s_new", Title: "New Sticker Session",
		Location: opencode.Location{Directory: p2},
		Time:     opencode.SessionTime{Updated: now},
	})
	pumpMainLoop(100 * time.Millisecond)
	assertSidebarIntegrity(t, "after ensureSessionTab s_new", w)

	// Test bumpSessionToTop on s1 (in sticker-tools) and g2 (in gluppencode)
	w.bumpSessionToTop(w.tabs["s1"])
	pumpMainLoop(100 * time.Millisecond)
	assertSidebarIntegrity(t, "after bump s1", w)

	w.bumpSessionToTop(w.tabs["g2"])
	pumpMainLoop(100 * time.Millisecond)
	assertSidebarIntegrity(t, "after bump g2", w)

	// Test attention dot + rename
	w.markTabNeedsAttention(w.tabs["s1"], false)
	if !w.tabs["s1"].sidebarIndicator.dot.Visible() {
		t.Fatal("expected s1 attention dot visible")
	}
	w.applySessionRename("s1", "Renamed Sticker Outline")
	if got := w.tabs["s1"].sidebarIndicator.titleLbl.Text(); got != "Renamed Sticker Outline" {
		t.Fatalf("s1 row title = %q, want 'Renamed Sticker Outline'", got)
	}

	// Test toggle collapse on p1 (expand then collapse)
	w.toggleProjectCollapsed(p1)
	pumpMainLoop(100 * time.Millisecond)
	assertSidebarIntegrity(t, "after expand p1", w)

	w.toggleProjectCollapsed(p1)
	pumpMainLoop(100 * time.Millisecond)
	assertSidebarIntegrity(t, "after re-collapse p1", w)

	// Test moveProjectInOrder (drag p2 before p1)
	w.moveProjectInOrder(p2, p1)
	pumpMainLoop(100 * time.Millisecond)
	assertSidebarIntegrity(t, "after move p2 before p1", w)

	// Test 2nd populateSessionTabs (reconnect / refresh)
	w.populateSessionTabs(sessions, nil)
	pumpMainLoop(150 * time.Millisecond)
	assertSidebarIntegrity(t, "after 2nd populate", w)

	// Test archiveSession on first session of p2 (s0)
	w.archiveSession("s0")
	pumpMainLoop(100 * time.Millisecond)
	assertSidebarIntegrity(t, "after archive s0", w)

	// Test selectSession on hidden session in collapsed p1 (a2), then re-collapse p1 (must UnselectRow before Remove), then select g0!
	w.selectSession(sessions[7]) // a2
	pumpMainLoop(100 * time.Millisecond)
	assertSidebarIntegrity(t, "after select a2", w)

	w.toggleProjectCollapsed(p1)
	pumpMainLoop(100 * time.Millisecond)
	assertSidebarIntegrity(t, "after collapse p1 with active a2", w)

	w.viewStack.SetVisibleChildName("g0")
	pumpMainLoop(100 * time.Millisecond)
	assertSidebarIntegrity(t, "after switch to g0", w)

	// Re-expand p1 and ensure previously selected a2 does not remain falsely selected in GTK ListBox.
	w.toggleProjectCollapsed(p1)
	pumpMainLoop(100 * time.Millisecond)
	assertSidebarIntegrity(t, "after re-expand p1 with g0 active", w)

	// Test ensureSessionTab with empty dir then location updated
	w.ensureSessionTab(opencode.Session{ID: "x0", Title: "Empty Dir"})
	pumpMainLoop(100 * time.Millisecond)
	assertSidebarIntegrity(t, "after empty dir x0", w)

	w.ensureSessionTab(opencode.Session{ID: "x0", Title: "Moved to p2", Location: opencode.Location{Directory: p2}, Time: opencode.SessionTime{Updated: now + 5000}})
	pumpMainLoop(100 * time.Millisecond)
	assertSidebarIntegrity(t, "after empty dir -> p2", w)

	// Test show-sidebar toggle and collapsed mobile mode toggle
	w.rootContent.SetShowSidebar(false)
	pumpMainLoop(100 * time.Millisecond)
	w.rootContent.SetShowSidebar(true)
	pumpMainLoop(100 * time.Millisecond)
	assertSidebarIntegrity(t, "after show-sidebar toggle", w)

	w.rootContent.SetCollapsed(true)
	pumpMainLoop(100 * time.Millisecond)
	w.rootContent.SetCollapsed(false)
	pumpMainLoop(100 * time.Millisecond)
	assertSidebarIntegrity(t, "after mobile collapsed toggle", w)

	if os.Getenv("BROADWAY_WAIT") != "" {
		pumpMainLoop(2 * time.Second)
	}
}
