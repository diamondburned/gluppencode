package window

import (
	"slices"
	"testing"
	"time"

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
