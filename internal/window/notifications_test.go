package window

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"libdb.so/gluppencode/internal/autoscroll"
	"libdb.so/gluppencode/internal/opencode"
)

type sentNotif struct {
	id    string
	notif *gio.Notification
}

func newTestTab(vs *adw.ViewStack, id, title string) (*sessionTab, *switcherIndicator) {
	stack := gtk.NewStack()
	page := vs.AddTitled(stack, id, title)
	ind := &switcherIndicator{
		hBox:         gtk.NewBox(gtk.OrientationHorizontal, 6),
		spinner:      adw.NewSpinner(),
		questionIcon: gtk.NewImageFromIconName("dialog-question-symbolic"),
		shellIcon:    gtk.NewImageFromIconName("terminal-symbolic"),
		dot:          gtk.NewBox(gtk.OrientationHorizontal, 0),
		titleLbl:     gtk.NewLabel(title),
		box:          gtk.NewBox(gtk.OrientationHorizontal, 0),
		label:        gtk.NewLabel(""),
	}
	ind.spinner.SetVisible(false)
	ind.questionIcon.SetVisible(false)
	ind.shellIcon.SetVisible(false)
	ind.dot.SetVisible(false)
	timelineBox := gtk.NewBox(gtk.OrientationVertical, 0)
	scroll := autoscroll.NewWindow()
	scroll.SetChild(timelineBox)
	conv := &convTimeline{
		session:            opencode.Session{ID: id, Title: title, Location: opencode.Location{Directory: "/tmp/proj"}},
		scroll:             scroll,
		timelineBox:        timelineBox,
		thinkingLabel:      gtk.NewLabel(""),
		assistantMap:       make(map[string]*AssistantMessageWidget),
		compactionMap:      make(map[string]*CompactionWidget),
		messageWidgets:     make(map[string]gtk.Widgetter),
		formWidgets:        make(map[string]*QuestionFormWidget),
		permissionWidgets:  make(map[string]*PermissionPromptWidget),
		seenMsgIDs:         make(map[string]bool),
		executingTools:     make(map[string]string),
		finishedTools:      make(map[string]bool),
		blockingTools:      make(map[string]time.Time),
		blockingShellTools: make(map[string]bool),
		blockingShellIDs:   make(map[string]string),
		backgroundShellIDs: make(map[string]bool),
		toolNames:          make(map[string]string),
		ignoredCancelIDs:   make(map[string]bool),
		loaded:             true,
	}
	tab := &sessionTab{
		page:             page,
		stack:            stack,
		main:             conv,
		title:            title,
		subTimelines:     make(map[string]*convTimeline),
		sidebarIndicator: ind,
	}
	return tab, ind
}

func newTestNotifyWindow(t *testing.T, coord *notificationCoordinator) (*Window, *adw.ViewStack) {
	t.Helper()
	vs := adw.NewViewStack()
	win := &Window{
		ctx:                   t.Context(),
		viewStack:             vs,
		stopBtn:               gtk.NewButton(),
		tabs:                  make(map[string]*sessionTab),
		childToRoot:           make(map[string]string),
		activeShells:          make(map[string]opencode.ShellItem),
		notifyCoord:           coord,
		completionSettleDelay: 25 * time.Millisecond,
	}
	if coord != nil {
		coord.registerWindow(win)
	}
	return win, vs
}

func pumpUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		glib.MainContextDefault().Iteration(false)
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func pumpFor(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		glib.MainContextDefault().Iteration(false)
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCompletionSettleAndNotifications(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("GTK cannot be initialized")
	}

	var (
		mu        sync.Mutex
		jobsResp  = `{"data":[]}`
		activeMap = `{}`
		sent      []sentNotif
		withdrawn []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		jr, am := jobsResp, activeMap
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/experimental/job":
			_, _ = w.Write([]byte(jr))
		case r.URL.Path == "/api/session/active":
			_, _ = w.Write([]byte(am))
		case r.URL.Path == "/api/session":
			_, _ = w.Write([]byte(`[]`))
		default:
			_, _ = w.Write([]byte(`{"messages":[]}`))
		}
	}))
	defer srv.Close()

	coord := newNotificationCoordinator(nil)
	coord.notifyFunc = func(id string, notif *gio.Notification) {
		mu.Lock()
		sent = append(sent, sentNotif{id: id, notif: notif})
		mu.Unlock()
	}
	coord.withdrawFunc = func(id string) {
		mu.Lock()
		withdrawn = append(withdrawn, id)
		mu.Unlock()
	}
	win, vs := newTestNotifyWindow(t, coord)
	win.client = &opencode.Client{BaseURL: srv.URL}
	coord.isActiveOverride = func(w *Window) bool { return w == win }

	fgTab, _ := newTestTab(vs, "ses_fg", "Foreground")
	bgTab, bgInd := newTestTab(vs, "ses_bg", "Background Task")
	win.tabs["ses_fg"] = fgTab
	win.tabs["ses_bg"] = bgTab
	win.tabOrder = []string{"ses_fg", "ses_bg"}
	vs.SetVisibleChildName("ses_fg")

	t.Run("mid-turn text does not notify, settles after delay and withdraws on visit", func(t *testing.T) {
		mu.Lock()
		sent, withdrawn = nil, nil
		jobsResp = `{"data":[]}`
		activeMap = `{}`
		mu.Unlock()

		win.handleEvent(opencode.Event{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)})
		win.handleEvent(opencode.Event{Type: "session.text.delta", Data: json.RawMessage(`{"sessionID":"ses_bg","assistantMessageID":"msg_1","ordinal":0,"delta":"All done!"}`)})
		win.handleEvent(opencode.Event{Type: "session.text.ended", Data: json.RawMessage(`{"sessionID":"ses_bg","assistantMessageID":"msg_1","ordinal":0,"text":"All done!"}`)})

		if bgTab.finishedAttention || bgInd.dot.Visible() {
			t.Fatal("mid-turn text must not set finishedAttention or dot")
		}

		win.handleEvent(opencode.Event{Type: "session.execution.succeeded", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)})
		if bgTab.finishedAttention {
			t.Fatal("finishedAttention must wait for completion settle delay")
		}

		pumpUntil(t, 500*time.Millisecond, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(sent) == 1 && bgTab.finishedAttention
		})
		if !bgTab.finishedAttention || !bgInd.dot.Visible() {
			t.Fatalf("expected finishedAttention and dot visible after settle (finished=%v dot=%v)", bgTab.finishedAttention, bgInd.dot.Visible())
		}
		mu.Lock()
		gotSent := append([]sentNotif(nil), sent...)
		mu.Unlock()
		if len(gotSent) != 1 || gotSent[0].id != "finished:ses_bg" {
			t.Fatalf("sent notifications = %+v, want single finished:ses_bg", gotSent)
		}

		// Visiting the tab clears finishedAttention, dot, and withdraws notification.
		vs.SetVisibleChildName("ses_bg")
		win.onVisibleTabChanged()
		if bgTab.finishedAttention || bgInd.dot.Visible() {
			t.Fatalf("expected finishedAttention and dot cleared on visit")
		}
		mu.Lock()
		gotWithdrawn := append([]string(nil), withdrawn...)
		mu.Unlock()
		if len(gotWithdrawn) == 0 || gotWithdrawn[len(gotWithdrawn)-1] != "finished:ses_bg" {
			t.Fatalf("expected withdraw of finished:ses_bg, got %v", gotWithdrawn)
		}
		vs.SetVisibleChildName("ses_fg")
	})

	t.Run("cancelled if work resumes during settle window", func(t *testing.T) {
		mu.Lock()
		sent = nil
		mu.Unlock()
		win.completionSettleDelay = 80 * time.Millisecond
		defer func() { win.completionSettleDelay = 25 * time.Millisecond }()

		win.handleEvent(opencode.Event{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)})
		win.handleEvent(opencode.Event{Type: "session.text.delta", Data: json.RawMessage(`{"sessionID":"ses_bg","assistantMessageID":"msg_2","delta":"First step"}`)})
		win.handleEvent(opencode.Event{Type: "session.execution.succeeded", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)})
		// Resume within 20ms before 80ms settle fires
		pumpFor(20 * time.Millisecond)
		win.handleEvent(opencode.Event{Type: "session.status", Data: json.RawMessage(`{"sessionID":"ses_bg","status":{"type":"busy"}}`)})
		pumpFor(110 * time.Millisecond)

		mu.Lock()
		n := len(sent)
		mu.Unlock()
		if n != 0 || bgTab.finishedAttention {
			t.Fatalf("expected no notification when work resumed during settle (sent=%d finished=%v)", n, bgTab.finishedAttention)
		}
		win.handleEvent(opencode.Event{Type: "session.execution.succeeded", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)})
		pumpFor(110 * time.Millisecond)
		if bgTab.finishedAttention {
			t.Fatal("new turn without reply text should not notify")
		}
	})

	t.Run("suppressed for tool-only, failed, interrupted, error, and child-only text", func(t *testing.T) {
		cases := []struct {
			name string
			evs  []opencode.Event
		}{
			{
				name: "tool-only",
				evs: []opencode.Event{
					{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)},
					{Type: "session.tool.called", Data: json.RawMessage(`{"sessionID":"ses_bg","assistantMessageID":"m_tool","id":"t1","name":"bash"}`)},
					{Type: "session.execution.succeeded", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)},
				},
			},
			{
				name: "failed-even-with-idle",
				evs: []opencode.Event{
					{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)},
					{Type: "session.text.delta", Data: json.RawMessage(`{"sessionID":"ses_bg","assistantMessageID":"m_fail","delta":"partial"}`)},
					{Type: "session.execution.failed", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)},
					{Type: "session.idle", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)},
				},
			},
			{
				name: "interrupted",
				evs: []opencode.Event{
					{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)},
					{Type: "session.text.delta", Data: json.RawMessage(`{"sessionID":"ses_bg","assistantMessageID":"m_int","delta":"partial"}`)},
					{Type: "session.execution.interrupted", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)},
				},
			},
			{
				name: "session-error",
				evs: []opencode.Event{
					{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)},
					{Type: "session.text.delta", Data: json.RawMessage(`{"sessionID":"ses_bg","assistantMessageID":"m_err","delta":"partial"}`)},
					{Type: "session.error", Data: json.RawMessage(`{"sessionID":"ses_bg","error":"rate limit"}`)},
					{Type: "session.status", Data: json.RawMessage(`{"sessionID":"ses_bg","status":{"type":"idle"}}`)},
				},
			},
			{
				name: "child-subagent-text-only",
				evs: []opencode.Event{
					{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)},
					{Type: "session.text.delta", Data: json.RawMessage(`{"sessionID":"ses_child_only","assistantMessageID":"m_c","delta":"child output"}`)},
					{Type: "session.execution.succeeded", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)},
				},
			},
		}
		win.childToRoot["ses_child_only"] = "ses_bg"
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				mu.Lock()
				sent = nil
				mu.Unlock()
				bgTab.setNeedsAttention(false)
				for _, ev := range tc.evs {
					win.handleEvent(ev)
				}
				pumpFor(70 * time.Millisecond)
				mu.Lock()
				n := len(sent)
				mu.Unlock()
				if n != 0 || bgTab.finishedAttention {
					t.Fatalf("%s: expected no finished attention or notification (sent=%d finished=%v)", tc.name, n, bgTab.finishedAttention)
				}
			})
		}
	})

	t.Run("server verification ignores shell/cancelled jobs and blocks on subagent jobs or active sessions", func(t *testing.T) {
		// 1. Running subagent job blocks notification.
		mu.Lock()
		sent = nil
		jobsResp = `{"data":[{"id":"job_1","kind":"subagent","sessionID":"ses_bg","status":"running"}]}`
		activeMap = `{}`
		mu.Unlock()

		win.handleEvent(opencode.Event{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)})
		win.handleEvent(opencode.Event{Type: "session.text.delta", Data: json.RawMessage(`{"sessionID":"ses_bg","assistantMessageID":"m_job1","delta":"Waiting on subagent"}`)})
		win.handleEvent(opencode.Event{Type: "session.execution.succeeded", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)})
		pumpFor(80 * time.Millisecond)
		mu.Lock()
		n := len(sent)
		mu.Unlock()
		if n != 0 || bgTab.finishedAttention {
			t.Fatalf("running subagent job must suppress finished notification")
		}

		// 2. Running shell job + cancelled subagent job do NOT block notification.
		mu.Lock()
		jobsResp = `{"data":[{"id":"job_sh","kind":"shell","sessionID":"ses_bg","status":"running"},{"id":"job_c","kind":"subagent","sessionID":"ses_bg","status":"cancelled"}]}`
		mu.Unlock()
		win.handleEvent(opencode.Event{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)})
		win.handleEvent(opencode.Event{Type: "session.text.delta", Data: json.RawMessage(`{"sessionID":"ses_bg","assistantMessageID":"m_job2","delta":"Shell keeps running"}`)})
		win.handleEvent(opencode.Event{Type: "session.execution.succeeded", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)})
		pumpUntil(t, 500*time.Millisecond, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(sent) == 1
		})
		if !bgTab.finishedAttention {
			t.Fatal("expected finishedAttention when only shell/cancelled jobs remain")
		}
		bgTab.setNeedsAttention(false)

		// 3. ActiveSessions listing root blocks notification.
		mu.Lock()
		sent = nil
		jobsResp = `{"data":[]}`
		activeMap = `{"ses_bg":{"type":"busy"}}`
		mu.Unlock()
		win.handleEvent(opencode.Event{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)})
		win.handleEvent(opencode.Event{Type: "session.text.delta", Data: json.RawMessage(`{"sessionID":"ses_bg","assistantMessageID":"m_act1","delta":"Still busy on server"}`)})
		win.handleEvent(opencode.Event{Type: "session.execution.succeeded", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)})
		pumpFor(80 * time.Millisecond)
		mu.Lock()
		n = len(sent)
		mu.Unlock()
		if n != 0 || bgTab.finishedAttention {
			t.Fatalf("active session in /session/status must suppress finished notification")
		}
	})

	t.Run("local running subagent delays completion until subagent ends", func(t *testing.T) {
		mu.Lock()
		sent = nil
		jobsResp = `{"data":[]}`
		activeMap = `{}`
		mu.Unlock()
		bgTab.setNeedsAttention(false)

		win.upsertSubagent(bgTab, opencode.Session{ID: "sub_wait", ParentID: "ses_bg", Time: opencode.SessionTime{Busy: 100}})
		win.handleEvent(opencode.Event{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)})
		win.handleEvent(opencode.Event{Type: "session.text.delta", Data: json.RawMessage(`{"sessionID":"ses_bg","assistantMessageID":"m_sub1","delta":"Subagent spawned"}`)})
		win.handleEvent(opencode.Event{Type: "session.execution.succeeded", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)})
		pumpFor(80 * time.Millisecond)
		if bgTab.finishedAttention {
			t.Fatal("root must not settle while child subagent is locally running")
		}

		win.handleEvent(opencode.Event{Type: "session.execution.succeeded", Data: json.RawMessage(`{"sessionID":"sub_wait"}`)})
		pumpUntil(t, 500*time.Millisecond, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(sent) == 1 && bgTab.finishedAttention
		})
	})
}

func TestActionNeededHydrationAndIndicators(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("GTK cannot be initialized")
	}

	var (
		mu        sync.Mutex
		sent      []sentNotif
		withdrawn []string
	)
	coord := newNotificationCoordinator(nil)
	coord.notifyFunc = func(id string, notif *gio.Notification) {
		mu.Lock()
		sent = append(sent, sentNotif{id: id, notif: notif})
		mu.Unlock()
	}
	coord.withdrawFunc = func(id string) {
		mu.Lock()
		withdrawn = append(withdrawn, id)
		mu.Unlock()
	}

	win, vs := newTestNotifyWindow(t, coord)
	coord.isActiveOverride = func(w *Window) bool { return w == win }

	fgTab, _ := newTestTab(vs, "ses_fg", "Foreground")
	bgTab, bgInd := newTestTab(vs, "ses_bg", "Background")
	win.tabs["ses_fg"] = fgTab
	win.tabs["ses_bg"] = bgTab
	vs.SetVisibleChildName("ses_fg")

	// REST hydration (live=false) renders questionIcon without actionAttention or notifications.
	win.upsertPendingForm(opencode.FormInfo{ID: "frm_rest", SessionID: "ses_bg", Title: "Hydrated Question"}, false)
	if bgTab.actionAttention || bgTab.NeedsAttention() {
		t.Fatal("REST hydration must not set actionAttention or NeedsAttention")
	}
	if !bgInd.questionIcon.Visible() || bgInd.spinner.Visible() {
		t.Fatalf("expected questionIcon visible and spinner hidden after REST hydration (q=%v spin=%v)", bgInd.questionIcon.Visible(), bgInd.spinner.Visible())
	}
	mu.Lock()
	if len(sent) != 0 {
		t.Fatalf("REST hydration must not send notifications, got %d", len(sent))
	}
	mu.Unlock()
	win.removePendingForm("ses_bg", "frm_rest")

	// Live form.created + active agent suppresses spinner, shows questionIcon, notifies, and withdraws on reply.
	win.setConvActive("ses_bg", true)
	if !bgInd.spinner.Visible() {
		t.Fatal("expected spinner visible when agent active without pending input")
	}
	win.handleEvent(opencode.Event{
		Type: "form.created",
		Data: json.RawMessage(`{"form":{"id":"frm_live","sessionID":"ses_bg","title":"Pick one","fields":[{"key":"q","title":"Choice","description":"Choose A or B"}]}}`),
	})
	if !bgTab.actionAttention || !bgInd.questionIcon.Visible() || bgInd.spinner.Visible() {
		t.Fatalf("expected actionAttention + questionIcon + hidden spinner (act=%v q=%v spin=%v)", bgTab.actionAttention, bgInd.questionIcon.Visible(), bgInd.spinner.Visible())
	}
	mu.Lock()
	if len(sent) != 1 || sent[0].id != "form:frm_live" {
		t.Fatalf("expected form:frm_live notification, got %+v", sent)
	}
	mu.Unlock()

	// Permission on child subagent triggers on parent tab and withdraws on permission.replied.
	win.childToRoot["sub_perm"] = "ses_bg"
	win.handleEvent(opencode.Event{
		Type: "permission.asked",
		Data: json.RawMessage(`{"request":{"id":"perm_live","sessionID":"sub_perm","action":"bash","resources":["ls"]}}`),
	})
	mu.Lock()
	if len(sent) != 2 || sent[1].id != "perm:perm_live" {
		t.Fatalf("expected perm:perm_live notification, got %+v", sent)
	}
	mu.Unlock()

	win.handleEvent(opencode.Event{Type: "form.replied", Data: json.RawMessage(`{"id":"frm_live","sessionID":"ses_bg"}`)})
	// perm_live still pending -> actionAttention and questionIcon remain true
	if !bgTab.actionAttention || !bgInd.questionIcon.Visible() {
		t.Fatal("expected actionAttention and questionIcon to remain while perm_live is still pending")
	}
	win.handleEvent(opencode.Event{Type: "permission.replied", Data: json.RawMessage(`{"id":"perm_live","sessionID":"sub_perm"}`)})
	if bgTab.actionAttention || bgInd.questionIcon.Visible() || bgTab.finishedAttention {
		t.Fatalf("expected actionAttention, questionIcon, and finishedAttention false after all pending cleared (act=%v q=%v fin=%v)", bgTab.actionAttention, bgInd.questionIcon.Visible(), bgTab.finishedAttention)
	}
	mu.Lock()
	gotWithdrawn := append([]string(nil), withdrawn...)
	mu.Unlock()
	if len(gotWithdrawn) < 2 {
		t.Fatalf("expected both form:frm_live and perm:perm_live withdrawn, got %v", gotWithdrawn)
	}
}

func TestMultiWindowNotificationsPrefsAndRouting(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("GTK cannot be initialized")
	}

	var (
		mu   sync.Mutex
		sent []sentNotif
	)
	coord := newNotificationCoordinator(nil)
	coord.notifyFunc = func(id string, notif *gio.Notification) {
		mu.Lock()
		sent = append(sent, sentNotif{id: id, notif: notif})
		mu.Unlock()
	}

	win1, vs1 := newTestNotifyWindow(t, coord)
	win2, vs2 := newTestNotifyWindow(t, coord)

	tab1A, _ := newTestTab(vs1, "ses_a", "Session A")
	tab1B, _ := newTestTab(vs1, "ses_b", "Session B")
	win1.tabs["ses_a"] = tab1A
	win1.tabs["ses_b"] = tab1B
	vs1.SetVisibleChildName("ses_a")

	tab2A, _ := newTestTab(vs2, "ses_a", "Session A")
	tab2B, _ := newTestTab(vs2, "ses_b", "Session B")
	win2.tabs["ses_a"] = tab2A
	win2.tabs["ses_b"] = tab2B
	vs2.SetVisibleChildName("ses_b")

	// Window 2 is focused on ses_b; Window 1 is UNFOCUSED with visible tab ses_a.
	// Completing ses_a MUST still notify (because Window 1 is not active), and deduplicate across both windows!
	coord.isActiveOverride = func(w *Window) bool { return w == win2 }

	for _, w := range []*Window{win1, win2} {
		w.handleEvent(opencode.Event{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"ses_a"}`)})
		w.handleEvent(opencode.Event{Type: "session.text.delta", Data: json.RawMessage(`{"sessionID":"ses_a","assistantMessageID":"msg_shared","delta":"Hello from A"}`)})
		w.handleEvent(opencode.Event{Type: "session.execution.succeeded", Data: json.RawMessage(`{"sessionID":"ses_a"}`)})
	}
	pumpUntil(t, 500*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(sent) == 1
	})
	pumpFor(40 * time.Millisecond)
	mu.Lock()
	if len(sent) != 1 || sent[0].id != "finished:ses_a" {
		t.Fatalf("expected exactly 1 deduplicated notification for ses_a, got %+v", sent)
	}
	sent = nil
	mu.Unlock()

	// Completing ses_b while Window 2 is focused on ses_b and NotifyOnActiveSession=false -> suppressed on win2,
	// and deduplication does not mark ses_b sent; enabling NotifyOnActiveSession syncs to win2 and notifies!
	win2.handleEvent(opencode.Event{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"ses_b"}`)})
	win2.handleEvent(opencode.Event{Type: "session.text.delta", Data: json.RawMessage(`{"sessionID":"ses_b","assistantMessageID":"msg_b1","delta":"Active tab reply"}`)})
	win2.handleEvent(opencode.Event{Type: "session.execution.succeeded", Data: json.RawMessage(`{"sessionID":"ses_b"}`)})
	pumpFor(60 * time.Millisecond)
	mu.Lock()
	if len(sent) != 0 {
		t.Fatalf("expected active tab notification suppressed when NotifyOnActiveSession=false, got %+v", sent)
	}
	mu.Unlock()

	// Cross-window preference sync from win1 -> win2.
	win1.statePath = t.TempDir() + "/state.json"
	prev := clonePreferences(win1.prefs)
	win1.prefs.NotifyOnActiveSession = true
	win1.prefs.NotifyOnFinished = boolPtr(true)
	win1.prefs.NotifyOnActionNeeded = boolPtr(false)
	win1.saveAndSyncPreferences(prev)

	if !win2.effectiveNotifyOnActiveSession() || win2.effectiveNotifyOnActionNeeded() {
		t.Fatalf("expected win2 prefs synced from win1 (active=%v action=%v)", win2.effectiveNotifyOnActiveSession(), win2.effectiveNotifyOnActionNeeded())
	}

	// Now active tab ses_b notifies when NotifyOnActiveSession=true, while form.created is suppressed by NotifyOnActionNeeded=false.
	win2.handleEvent(opencode.Event{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"ses_b"}`)})
	win2.handleEvent(opencode.Event{Type: "session.text.delta", Data: json.RawMessage(`{"sessionID":"ses_b","assistantMessageID":"msg_b2","delta":"Active tab notify on"}`)})
	win2.handleEvent(opencode.Event{Type: "session.execution.succeeded", Data: json.RawMessage(`{"sessionID":"ses_b"}`)})
	pumpUntil(t, 500*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(sent) == 1
	})
	win2.handleEvent(opencode.Event{Type: "form.created", Data: json.RawMessage(`{"form":{"id":"frm_off","sessionID":"ses_b","title":"Should not notify"}}`)})
	mu.Lock()
	if len(sent) != 1 {
		t.Fatalf("expected form notification suppressed when NotifyOnActionNeeded=false, got %+v", sent)
	}
	mu.Unlock()

	// openSession routes to win1 (which owns ses_only_1) and switches visible tab.
	tabOnly1, _ := newTestTab(vs1, "ses_only_1", "Only 1")
	win1.tabs["ses_only_1"] = tabOnly1
	coord.openSession("ses_only_1", nil)
	if got := win1.currentRootSessionID(); got != "ses_only_1" {
		t.Fatalf("expected win1 currentRootSessionID = ses_only_1, got %q", got)
	}
}

func TestSessionViewAPIOutbound(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("GTK cannot be initialized")
	}

	type viewCall struct {
		sessionID string
		idle      int64
	}
	var (
		mu        sync.Mutex
		viewCalls []viewCall
		failNext  bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && len(r.URL.Path) > len("/api/session/") && r.URL.Path[len(r.URL.Path)-len("/view"):] == "/view" {
			sessID := r.URL.Path[len("/api/session/") : len(r.URL.Path)-len("/view")]
			var body struct {
				Idle int64 `json:"idle"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			shouldFail := failNext
			if shouldFail {
				failNext = false
			} else {
				viewCalls = append(viewCalls, viewCall{sessionID: sessID, idle: body.Idle})
			}
			mu.Unlock()
			if shouldFail {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	activeWin := false
	coord := newNotificationCoordinator(nil)
	coord.isActiveOverride = func(*Window) bool { return activeWin }

	win, vs := newTestNotifyWindow(t, coord)
	win.client = &opencode.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	tabA, _ := newTestTab(vs, "ses_a", "Session A")
	tabA.main.session.Time.Idle = 1795550000000
	tabB, _ := newTestTab(vs, "ses_b", "Session B")
	tabB.main.session.Time.Idle = 1795550000500
	tabB.main.session.Time.Viewed = 1795550000500
	tabC, _ := newTestTab(vs, "ses_c", "Session C")
	tabD, _ := newTestTab(vs, "ses_d", "Session D")
	win.tabs["ses_a"] = tabA
	win.tabs["ses_b"] = tabB
	win.tabs["ses_c"] = tabC
	win.tabs["ses_d"] = tabD

	// 1. Selecting an unviewed idle tab while window is unfocused does not call ViewSession.
	vs.SetVisibleChildName("ses_a")
	win.onVisibleTabChanged()
	pumpFor(30 * time.Millisecond)
	mu.Lock()
	if len(viewCalls) != 0 {
		t.Fatalf("expected 0 view calls while unfocused, got %+v", viewCalls)
	}
	mu.Unlock()

	// 2. Gaining window focus marks the displayed unviewed idle root tab viewed once.
	activeWin = true
	win.onWindowActiveChanged()
	pumpUntil(t, 500*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(viewCalls) == 1
	})
	mu.Lock()
	if viewCalls[0] != (viewCall{sessionID: "ses_a", idle: 1795550000000}) {
		t.Fatalf("unexpected first view call: %+v", viewCalls[0])
	}
	mu.Unlock()

	// 3. Repeated focus changes and tab flips back to ses_a (or already-viewed ses_b) do not spam.
	win.onWindowActiveChanged()
	vs.SetVisibleChildName("ses_b")
	win.onVisibleTabChanged()
	vs.SetVisibleChildName("ses_a")
	win.onVisibleTabChanged()
	pumpFor(40 * time.Millisecond)
	mu.Lock()
	if len(viewCalls) != 1 {
		t.Fatalf("expected deduplication to keep 1 view call, got %+v", viewCalls)
	}
	mu.Unlock()

	// 4. Background tab ses_c receives session.updated with Time.Idle while ses_a is active -> not viewed yet.
	win.handleEvent(opencode.Event{
		Type: "session.updated",
		Data: json.RawMessage(`{"sessionID":"ses_c","session":{"id":"ses_c","time":{"idle":1795550001000}}}`),
	})
	pumpFor(30 * time.Millisecond)
	mu.Lock()
	if len(viewCalls) != 1 {
		t.Fatalf("expected background update not to trigger view call, got %+v", viewCalls)
	}
	mu.Unlock()

	// Switching to ses_c while window is active marks it viewed.
	vs.SetVisibleChildName("ses_c")
	win.onVisibleTabChanged()
	pumpUntil(t, 500*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(viewCalls) == 2
	})
	mu.Lock()
	if viewCalls[1] != (viewCall{sessionID: "ses_c", idle: 1795550001000}) {
		t.Fatalf("unexpected second view call: %+v", viewCalls[1])
	}
	mu.Unlock()

	// 5. Focused tab transitions to idle via session.updated + session.idle -> single call using server Time.Idle.
	win.handleEvent(opencode.Event{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"ses_c"}`)})
	win.handleEvent(opencode.Event{
		Type: "session.updated",
		Data: json.RawMessage(`{"sessionID":"ses_c","session":{"id":"ses_c","time":{"idle":1795550002000}}}`),
	})
	win.handleEvent(opencode.Event{Type: "session.idle", Data: json.RawMessage(`{"sessionID":"ses_c"}`)})
	pumpUntil(t, 500*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(viewCalls) == 3
	})
	pumpFor(30 * time.Millisecond)
	mu.Lock()
	if len(viewCalls) != 3 || viewCalls[2] != (viewCall{sessionID: "ses_c", idle: 1795550002000}) {
		t.Fatalf("expected 3rd view call for ses_c @ 1795550002000, got %+v", viewCalls)
	}
	mu.Unlock()

	// 6. Focused tab with Time.Idle == 0 does not call ViewSession until session.idle arrives (falling back to current ms).
	vs.SetVisibleChildName("ses_d")
	win.onVisibleTabChanged()
	pumpFor(30 * time.Millisecond)
	mu.Lock()
	if len(viewCalls) != 3 {
		t.Fatalf("expected no view call when Time.Idle == 0, got %+v", viewCalls)
	}
	mu.Unlock()

	beforeMs := time.Now().UnixMilli()
	win.handleEvent(opencode.Event{Type: "session.idle", Data: json.RawMessage(`{"sessionID":"ses_d"}`)})
	pumpUntil(t, 500*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(viewCalls) == 4
	})
	mu.Lock()
	gotD := viewCalls[3]
	mu.Unlock()
	if gotD.sessionID != "ses_d" || gotD.idle < beforeMs || tabD.main.session.Time.Idle != gotD.idle {
		t.Fatalf("unexpected fallback view call %+v (tab idle=%d, beforeMs=%d)", gotD, tabD.main.session.Time.Idle, beforeMs)
	}

	// 7. Failed ViewSession clears dedup entry so subsequent focus retries.
	mu.Lock()
	failNext = true
	mu.Unlock()
	nextIdle := gotD.idle + 5000
	win.handleEvent(opencode.Event{
		Type: "session.updated",
		Data: json.RawMessage(`{"sessionID":"ses_d","session":{"id":"ses_d","time":{"idle":` + jsonInt64(nextIdle) + `}}}`),
	})
	pumpFor(40 * time.Millisecond)
	win.onWindowActiveChanged()
	pumpUntil(t, 500*time.Millisecond, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(viewCalls) == 5
	})
	mu.Lock()
	if viewCalls[4] != (viewCall{sessionID: "ses_d", idle: nextIdle}) {
		t.Fatalf("expected retry call for ses_d @ %d, got %+v", nextIdle, viewCalls)
	}
	mu.Unlock()
}

func jsonInt64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestSessionViewedSSEHandling(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("GTK cannot be initialized")
	}

	var (
		mu        sync.Mutex
		sent      []sentNotif
		withdrawn []string
	)
	coord := newNotificationCoordinator(nil)
	coord.isActiveOverride = func(*Window) bool { return true }
	coord.notifyFunc = func(id string, notif *gio.Notification) {
		mu.Lock()
		sent = append(sent, sentNotif{id: id, notif: notif})
		mu.Unlock()
	}
	coord.withdrawFunc = func(id string) {
		mu.Lock()
		withdrawn = append(withdrawn, id)
		mu.Unlock()
	}

	win, vs := newTestNotifyWindow(t, coord)
	tabCur, _ := newTestTab(vs, "ses_cur", "Current")
	tabBg, indBg := newTestTab(vs, "ses_bg", "Background")
	tabBg.main.session.Time.Idle = 1795550000000
	win.tabs["ses_cur"] = tabCur
	win.tabs["ses_bg"] = tabBg
	vs.SetVisibleChildName("ses_cur")
	win.onVisibleTabChanged()

	// Complete ses_bg in background to set finishedAttention and send notification.
	win.handleEvent(opencode.Event{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)})
	win.handleEvent(opencode.Event{Type: "session.text.delta", Data: json.RawMessage(`{"sessionID":"ses_bg","assistantMessageID":"msg_1","delta":"Done"}`)})
	win.handleEvent(opencode.Event{Type: "session.execution.succeeded", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)})
	pumpUntil(t, 500*time.Millisecond, func() bool {
		return tabBg.NeedsAttention() && indBg.dot.Visible()
	})
	if !tabBg.NeedsAttention() || !indBg.dot.Visible() {
		t.Fatalf("expected ses_bg attention and dot visible")
	}

	// Older session.viewed (idle < Time.Idle) updates Time.Viewed without clearing badge/notification.
	win.handleEvent(opencode.Event{
		Type: "session.viewed",
		Data: json.RawMessage(`{"sessionID":"ses_bg","idle":1795549999999}`),
	})
	if tabBg.main.session.Time.Viewed != 1795549999999 {
		t.Fatalf("expected Time.Viewed=1795549999999, got %d", tabBg.main.session.Time.Viewed)
	}
	if !tabBg.NeedsAttention() || !indBg.dot.Visible() {
		t.Fatalf("expected older session.viewed not to clear attention")
	}

	// Matching session.viewed (idle >= Time.Idle) clears badge, indicator, and withdraws notification.
	win.handleEvent(opencode.Event{
		Type: "session.viewed",
		Data: json.RawMessage(`{"sessionID":"ses_bg","idle":1795550000000}`),
	})
	if tabBg.main.session.Time.Viewed != 1795550000000 {
		t.Fatalf("expected Time.Viewed=1795550000000, got %d", tabBg.main.session.Time.Viewed)
	}
	if tabBg.NeedsAttention() || tabBg.page.NeedsAttention() || indBg.dot.Visible() {
		t.Fatalf("expected session.viewed to clear NeedsAttention and dot")
	}
	mu.Lock()
	if len(withdrawn) != 1 || withdrawn[0] != "finished:ses_bg" {
		t.Fatalf("expected withdrawn [finished:ses_bg], got %+v", withdrawn)
	}
	mu.Unlock()

	// Pending form keeps actionAttention across session.viewed until replied.
	win.handleEvent(opencode.Event{
		Type: "form.created",
		Data: json.RawMessage(`{"form":{"id":"frm_1","sessionID":"ses_bg","title":"Pick"}}`),
	})
	win.handleEvent(opencode.Event{
		Type: "session.viewed",
		Data: json.RawMessage(`{"sessionID":"ses_bg","idle":1795550001000}`),
	})
	if !tabBg.actionAttention || !tabBg.NeedsAttention() || !indBg.questionIcon.Visible() {
		t.Fatalf("expected pending form actionAttention preserved on session.viewed")
	}
	win.handleEvent(opencode.Event{
		Type: "form.replied",
		Data: json.RawMessage(`{"id":"frm_1","sessionID":"ses_bg"}`),
	})
	if tabBg.NeedsAttention() {
		t.Fatalf("expected form.replied to clear actionAttention")
	}

	// session.viewed arriving during completion settle window prevents stale finished badge/notification.
	mu.Lock()
	sentBefore := len(sent)
	mu.Unlock()
	win.handleEvent(opencode.Event{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)})
	win.handleEvent(opencode.Event{Type: "session.text.delta", Data: json.RawMessage(`{"sessionID":"ses_bg","assistantMessageID":"msg_2","delta":"Second turn"}`)})
	win.handleEvent(opencode.Event{
		Type: "session.updated",
		Data: json.RawMessage(`{"sessionID":"ses_bg","session":{"id":"ses_bg","time":{"idle":1795550005000}}}`),
	})
	win.handleEvent(opencode.Event{Type: "session.execution.succeeded", Data: json.RawMessage(`{"sessionID":"ses_bg"}`)})
	win.handleEvent(opencode.Event{
		Type: "session.viewed",
		Data: json.RawMessage(`{"sessionID":"ses_bg","idle":1795550005000}`),
	})
	pumpFor(60 * time.Millisecond)
	if tabBg.NeedsAttention() || indBg.dot.Visible() {
		t.Fatalf("expected settle-window session.viewed to suppress finished attention")
	}
	mu.Lock()
	if len(sent) != sentBefore {
		t.Fatalf("expected no new notification after settle-window session.viewed, got %+v", sent[sentBefore:])
	}
	mu.Unlock()
}
