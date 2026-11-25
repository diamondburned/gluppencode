package window

import (
	"slices"
	"testing"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"libdb.so/gluppencode/internal/opencode"
)

func newSuspendTab(id, dir string) *sessionTab {
	return &sessionTab{main: &convTimeline{session: opencode.Session{
		ID:       id,
		Location: opencode.Location{Directory: dir},
	}}}
}

func TestIsTabSuspended(t *testing.T) {
	tests := []struct {
		name      string
		touchedAt time.Duration // age of the last touch; 0 means never touched
		setup     func(*sessionTab)
		want      bool
	}{
		{name: "never touched", want: true},
		{name: "touched just now", touchedAt: time.Second, want: false},
		{name: "touched long ago", touchedAt: 2 * tabSuspendAfter, want: true},
		{
			name:  "running session",
			setup: func(tab *sessionTab) { tab.main.active = true },
			want:  false,
		},
		{
			name:  "pending form",
			setup: func(tab *sessionTab) { tab.main.pendingForms = []opencode.FormInfo{{}} },
			want:  false,
		},
		{
			name: "pending permission on a subagent",
			setup: func(tab *sessionTab) {
				tab.subTimelines = map[string]*convTimeline{"sub": {pendingPermissions: []opencode.PermissionRequest{{}}}}
			},
			want: false,
		},
		{
			name:  "running subagent",
			setup: func(tab *sessionTab) { tab.subTimelines = map[string]*convTimeline{"sub": {active: true}} },
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &Window{}
			tab := newSuspendTab("ses_1", "/a")
			if tt.setup != nil {
				tt.setup(tab)
			}
			if tt.touchedAt > 0 {
				w.sessionTouched = map[string]time.Time{"ses_1": time.Now().Add(-tt.touchedAt)}
			}
			if got := w.isTabSuspended(tab); got != tt.want {
				t.Fatalf("isTabSuspended = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEventWakesSuspendedTab(t *testing.T) {
	w := &Window{
		tabs:        map[string]*sessionTab{"ses_1": newSuspendTab("ses_1", "/a")},
		childToRoot: map[string]string{},
	}
	tab := w.tabs["ses_1"]
	if !w.isTabSuspended(tab) {
		t.Fatal("tab should start suspended")
	}
	if got := w.awakeTabDirs(); len(got) != 0 {
		t.Fatalf("awakeTabDirs = %v, want none", got)
	}

	w.resolveEventConv("ses_1")

	if w.isTabSuspended(tab) {
		t.Fatal("event did not wake the tab")
	}
	if got := w.awakeTabDirs(); !slices.Equal(got, []string{"/a"}) {
		t.Fatalf("awakeTabDirs = %v, want [/a]", got)
	}
}

func TestPlainResolveConvDoesNotWake(t *testing.T) {
	w := &Window{
		tabs:        map[string]*sessionTab{"ses_1": newSuspendTab("ses_1", "/a")},
		childToRoot: map[string]string{},
	}
	w.resolveConv("ses_1")
	if !w.isTabSuspended(w.tabs["ses_1"]) {
		t.Fatal("resolveConv must not count as activity")
	}
}

func TestCanDeepUnloadTab(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}
	w := &Window{
		ctx:         t.Context(),
		viewStack:   adw.NewViewStack(),
		tabs:        make(map[string]*sessionTab),
		childToRoot: make(map[string]string),
	}
	tabCur := w.addSessionTab(opencode.Session{ID: "ses_cur", Location: opencode.Location{Directory: "/a"}})
	tabCur.main.loaded = true
	w.viewStack.SetVisibleChildName("ses_cur")

	tabIdle := w.addSessionTab(opencode.Session{ID: "ses_idle", Location: opencode.Location{Directory: "/a"}})
	tabIdle.main.loaded = true
	tabIdle.main.messages = []opencode.Message{{ID: "m1", Type: "user", Text: "hi"}}

	w.sessionTouched = map[string]time.Time{
		"ses_cur":  time.Now().Add(-2 * tabDeepUnloadAfter),
		"ses_idle": time.Now().Add(-2 * tabDeepUnloadAfter),
	}

	if w.canDeepUnloadTab(tabCur) {
		t.Fatal("must not deep-unload currently visible tab")
	}
	if !w.canDeepUnloadTab(tabIdle) {
		t.Fatal("expected long-idle offscreen tab to be eligible for deep unload")
	}

	w.sessionTouched["ses_idle"] = time.Now().Add(-tabSuspendAfter - time.Minute)
	if w.canDeepUnloadTab(tabIdle) {
		t.Fatal("must not deep-unload tab before tabDeepUnloadAfter")
	}

	w.sessionTouched["ses_idle"] = time.Now().Add(-2 * tabDeepUnloadAfter)
	tabIdle.main.pendingForms = []opencode.FormInfo{{ID: "f1"}}
	if w.canDeepUnloadTab(tabIdle) {
		t.Fatal("must not deep-unload tab with pending forms")
	}
	tabIdle.main.pendingForms = nil
	tabIdle.main.active = true
	if w.canDeepUnloadTab(tabIdle) {
		t.Fatal("must not deep-unload working tab")
	}
}

func TestUnloadLongSuspendedTabs(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}
	w := &Window{
		ctx:            t.Context(),
		viewStack:      adw.NewViewStack(),
		tabs:           make(map[string]*sessionTab),
		childToRoot:    make(map[string]string),
		sessionTouched: make(map[string]time.Time),
	}
	tabCur := w.addSessionTab(opencode.Session{ID: "ses_cur", Location: opencode.Location{Directory: "/a"}})
	tabCur.main.loaded = true
	w.viewStack.SetVisibleChildName("ses_cur")

	tabOld := w.addSessionTab(opencode.Session{ID: "ses_old", Title: "Old Tab", Location: opencode.Location{Directory: "/a"}})
	tabOld.main.loaded = true
	tabOld.main.hasOlder = true
	tabOld.main.nextCursor = "cur_1"
	msg := opencode.Message{ID: "m1", Type: "user", Text: "hello"}
	tabOld.main.messages = []opencode.Message{msg}
	tabOld.main.seenMsgIDs["m1"] = true
	uw := NewUserMessageWidget(w.ctx, "hello")
	tabOld.main.messageWidgets["m1"] = uw
	tabOld.main.timelineBox.Append(uw)

	subSess := opencode.Session{ID: "sub_1", ParentID: "ses_old", Title: "Sub", Outcome: "succeeded"}
	subSess.Time.Idle = 100
	tabOld.subagents = []opencode.Session{subSess}
	w.childToRoot["sub_1"] = "ses_old"
	subConv := w.ensureSubagentConv(tabOld, "sub_1")
	subConv.loaded = true
	subConv.messages = []opencode.Message{{ID: "sm1", Type: "user", Text: "sub"}}

	w.sessionTouched["ses_old"] = time.Now().Add(-2 * tabDeepUnloadAfter)

	w.unloadLongSuspendedTabs()

	if tabOld.main.loaded {
		t.Fatal("expected tabOld.main.loaded == false after deep unload")
	}
	if len(tabOld.main.messages) != 0 || len(tabOld.main.messageWidgets) != 0 || len(tabOld.main.seenMsgIDs) != 0 {
		t.Fatal("expected tabOld.main timeline state cleared after deep unload")
	}
	if tabOld.main.hasOlder || tabOld.main.nextCursor != "" {
		t.Fatal("expected pagination cursor cleared after deep unload")
	}
	if len(tabOld.subTimelines) != 0 {
		t.Fatalf("expected non-active subTimelines pruned on deep unload, got %d", len(tabOld.subTimelines))
	}
	if w.tabs["ses_old"] != tabOld || tabOld.main.session.Title != "Old Tab" {
		t.Fatal("expected tab shell and metadata preserved in w.tabs")
	}
	if w.childToRoot["sub_1"] != "ses_old" || len(tabOld.subagents) != 1 {
		t.Fatal("expected subagent metadata and childToRoot preserved")
	}
	if !tabCur.main.loaded {
		t.Fatal("expected visible tab to remain loaded")
	}
}
