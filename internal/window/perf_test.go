package window

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"libdb.so/gluppencode/internal/opencode"
)

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	return b
}

func TestCoalesceSSEEvents(t *testing.T) {
	events := []opencode.Event{
		{
			Type: "session.text.delta",
			Data: mustJSON(t, opencode.TextDeltaEvent{SessionID: "s1", AssistantMessageID: "m1", Ordinal: 0, Delta: "Hel"}),
		},
		{
			Type: "session.text.delta",
			Data: mustJSON(t, opencode.TextDeltaEvent{SessionID: "s1", AssistantMessageID: "m1", Ordinal: 0, Delta: "lo "}),
		},
		{
			Type: "session.text.delta",
			Data: mustJSON(t, opencode.TextDeltaEvent{SessionID: "s1", AssistantMessageID: "m1", Ordinal: 0, Delta: "world"}),
		},
		{
			Type: "session.text.delta",
			Data: mustJSON(t, opencode.TextDeltaEvent{SessionID: "s1", AssistantMessageID: "m1", Ordinal: 1, Delta: "next"}),
		},
		{
			Type: "session.tool.input.delta",
			Data: mustJSON(t, opencode.ToolEvent{SessionID: "s1", AssistantMessageID: "m1", ID: "t1", Name: "bash", Delta: `{"cmd":`}),
		},
		{
			Type: "session.tool.input.delta",
			Data: mustJSON(t, opencode.ToolEvent{SessionID: "s1", AssistantMessageID: "m1", ID: "t1", Delta: `"ls"}`}),
		},
		{
			Type: "session.tool.called",
			Data: mustJSON(t, opencode.ToolEvent{SessionID: "s1", AssistantMessageID: "m1", ID: "t1", Name: "bash"}),
		},
		{
			Type: "session.text.delta",
			Data: mustJSON(t, opencode.TextDeltaEvent{SessionID: "s1", AssistantMessageID: "m1", Ordinal: 2, Delta: "after"}),
		},
	}

	got := coalesceSSEEvents(events)
	if len(got) != 5 {
		t.Fatalf("coalesceSSEEvents len = %d, want 5", len(got))
	}

	var first opencode.TextDeltaEvent
	if err := json.Unmarshal(got[0].Data, &first); err != nil {
		t.Fatal(err)
	}
	if first.Delta != "Hello world" || first.Ordinal != 0 {
		t.Fatalf("first coalesced text = %+v, want Delta='Hello world' Ordinal=0", first)
	}

	var second opencode.TextDeltaEvent
	if err := json.Unmarshal(got[1].Data, &second); err != nil {
		t.Fatal(err)
	}
	if second.Delta != "next" || second.Ordinal != 1 {
		t.Fatalf("second text = %+v, want Delta='next' Ordinal=1", second)
	}

	var tool opencode.ToolEvent
	if err := json.Unmarshal(got[2].Data, &tool); err != nil {
		t.Fatal(err)
	}
	if tool.Delta != `{"cmd":"ls"}` || tool.Name != "bash" {
		t.Fatalf("coalesced tool = %+v, want Delta='{\"cmd\":\"ls\"}' Name='bash'", tool)
	}

	if got[3].Type != "session.tool.called" {
		t.Fatalf("got[3].Type = %q, want session.tool.called", got[3].Type)
	}
	if got[4].Type != "session.text.delta" {
		t.Fatalf("got[4].Type = %q, want session.text.delta", got[4].Type)
	}
}

func TestLazyRebuildModelList(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}
	modelPop := gtk.NewPopover()
	modelList := gtk.NewListBox()
	w := &Window{
		modelPopover:     modelPop,
		modelListBox:     modelList,
		modelSearchEntry: gtk.NewSearchEntry(),
		models: []opencode.ModelInfo{
			{
				ID:         "claude-test",
				Name:       "Claude Test",
				ProviderID: "anthropic",
			},
		},
	}

	w.rebuildModelList()
	if !w.modelListDirty {
		t.Fatal("expected rebuildModelList to defer work when popover is not mapped")
	}
	if modelList.RowAtIndex(0) != nil {
		t.Fatal("expected modelList to remain empty while deferred")
	}

	w.rebuildModelListNow()
	if w.modelListDirty {
		t.Fatal("expected modelListDirty cleared after rebuildModelListNow")
	}
	if modelList.RowAtIndex(0) == nil {
		t.Fatal("expected modelList populated after rebuildModelListNow")
	}
}

func TestPopulateSessionTabsPreservesLoadedTimelines(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}
	w := &Window{
		ctx:             t.Context(),
		viewStack:       adw.NewViewStack(),
		sidebarList:     gtk.NewListBox(),
		tabs:            make(map[string]*sessionTab),
		childToRoot:     make(map[string]string),
		visibleSessions: map[string]bool{"s1": true, "s2": true, "s3": true},
		allSessions: []opencode.Session{
			{ID: "s1", Title: "One", Location: opencode.Location{Directory: "/p1"}, Time: opencode.SessionTime{Updated: 200}},
			{ID: "s2", Title: "Two", Location: opencode.Location{Directory: "/p1"}, Time: opencode.SessionTime{Updated: 100}},
		},
	}

	w.populateSessionTabs(w.allSessions, nil)
	tab1 := w.tabs["s1"]
	tab2 := w.tabs["s2"]
	if tab1 == nil || tab2 == nil {
		t.Fatal("expected s1 and s2 tabs created")
	}
	tab1.main.loaded = true
	tab1.main.messages = []opencode.Message{{ID: "m1", Type: "user", Text: "keep me"}}
	uw := NewUserMessageWidget(w.ctx, "keep me")
	tab1.main.messageWidgets["m1"] = uw
	tab1.main.timelineBox.Append(uw)
	ind1 := tab1.sidebarIndicator

	w.viewStack.SetVisibleChildName("s1")
	if got := w.currentRootSessionID(); got != "s1" {
		t.Fatalf("currentRootSessionID = %q, want s1", got)
	}

	// Reconnect refresh: s1 title updates, s2 is removed, s3 is added.
	refreshed := []opencode.Session{
		{ID: "s1", Title: "One Renamed", Location: opencode.Location{Directory: "/p1"}, Time: opencode.SessionTime{Updated: 300}},
		{ID: "s3", Title: "Three", Location: opencode.Location{Directory: "/p1"}, Time: opencode.SessionTime{Updated: 250}},
	}
	w.allSessions = refreshed
	w.populateSessionTabs(refreshed, nil)

	if w.tabs["s1"] != tab1 {
		t.Fatal("expected existing *sessionTab for s1 to be reused across reconnect")
	}
	if !tab1.main.loaded || len(tab1.main.messages) != 1 || tab1.main.messageWidgets["m1"] != uw {
		t.Fatal("expected loaded timeline and widgets for s1 to be preserved")
	}
	if tab1.main.session.Title != "One Renamed" {
		t.Fatalf("s1 title = %q, want One Renamed", tab1.main.session.Title)
	}
	if tab1.sidebarIndicator != ind1 {
		t.Fatal("expected sidebarIndicator reused for preserved tab")
	}
	if w.tabs["s2"] != nil {
		t.Fatal("expected obsolete tab s2 removed")
	}
	if w.tabs["s3"] == nil {
		t.Fatal("expected new tab s3 created")
	}
	if got := w.currentRootSessionID(); got != "s1" {
		t.Fatalf("currentRootSessionID after refresh = %q, want s1", got)
	}
}

func TestConvTimelineToolIndexesAndCleanup(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}
	w := &Window{
		ctx:          t.Context(),
		viewStack:    adw.NewViewStack(),
		tabs:         make(map[string]*sessionTab),
		childToRoot:  make(map[string]string),
		activeShells: make(map[string]opencode.ShellItem),
		shellOutputs: make(map[string]string),
	}
	tab := w.addSessionTab(opencode.Session{ID: "ses_idx", Location: opencode.Location{Directory: "/p1"}})
	conv := tab.main
	conv.loaded = true

	aw := NewAssistantMessageWidget(w.ctx, "build", "test", nil)
	twBg := NewToolWidget(w.ctx, "call_bg", "shell", nil)
	twBg.SetStatus("completed")
	twBg.SetMetadata(map[string]any{
		"shellID": "sh_1",
		"status":  "running",
	})
	twBg.shellFile = "/tmp/opencode/sh_1.log"
	aw.tools["call_bg"] = twBg
	conv.assistantMap["a1"] = aw
	conv.indexAssistantTools(aw)

	if !w.hasVisibleBackgroundShellsForTab(tab) {
		t.Fatal("expected hasVisibleBackgroundShellsForTab to return true via activeBgShellTools")
	}
	if got := w.findShellOutputFile("sh_1"); got != "/tmp/opencode/sh_1.log" {
		t.Fatalf("findShellOutputFile = %q, want /tmp/opencode/sh_1.log", got)
	}

	conv.markSyntheticTargetCompleted(opencode.Message{
		ID:   "syn_1",
		Type: "synthetic",
		Text: `<shell id="sh_1" state="completed">done</shell>`,
	})
	if twBg.IsBackgroundRunning() {
		t.Fatal("expected markSyntheticTargetCompleted to stop background shell via index")
	}
	if w.hasVisibleBackgroundShellsForTab(tab) {
		t.Fatal("expected hasVisibleBackgroundShellsForTab false after completion")
	}

	conv.removeAssistantToolIndexes(aw)
	if len(conv.shellToolsByID) != 0 || len(conv.activeBgShellTools) != 0 {
		t.Fatalf("expected tool indexes empty after removeAssistantToolIndexes, got shells=%d activeBg=%d",
			len(conv.shellToolsByID), len(conv.activeBgShellTools))
	}
}

func TestConvTimelineHeadEvictionAndReload(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}

	// Fake server serving 150 chronological messages (m001..m150).
	allMsgs := make([]opencode.Message, 150)
	for i := range allMsgs {
		allMsgs[i] = opencode.Message{
			ID:   fmt.Sprintf("m%03d", i+1),
			Type: "user",
			Text: fmt.Sprintf("message %d", i+1),
		}
		allMsgs[i].Time.Created = int64(1000 + i)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/session/ses_evict/message" {
			http.NotFound(rw, r)
			return
		}
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 {
			limit = 50
		}
		cursor := q.Get("cursor")

		end := len(allMsgs)
		if cursor != "" {
			idx, err := strconv.Atoi(cursor)
			if err != nil || idx < 0 || idx > len(allMsgs) {
				http.Error(rw, "bad cursor", http.StatusBadRequest)
				return
			}
			end = idx
		}
		start := max(0, end-limit)
		// Server returns newest-first in Data, with Cursor.Next pointing to start.
		desc := make([]opencode.Message, 0, end-start)
		for i := end - 1; i >= start; i-- {
			desc = append(desc, allMsgs[i])
		}
		var next string
		if start > 0 {
			next = strconv.Itoa(start)
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"data": desc,
			"cursor": map[string]string{
				"next": next,
			},
		})
	}))
	defer srv.Close()

	w := &Window{
		ctx:         t.Context(),
		client:      &opencode.Client{BaseURL: srv.URL},
		viewStack:   adw.NewViewStack(),
		tabs:        make(map[string]*sessionTab),
		childToRoot: make(map[string]string),
	}
	tab := w.addSessionTab(opencode.Session{ID: "ses_evict", Location: opencode.Location{Directory: "/p1"}})
	conv := tab.main
	conv.loaded = true

	// Populate newest 130 messages (m021..m150).
	for _, msg := range allMsgs[20:] {
		conv.messages = append(conv.messages, msg)
		conv.seenMsgIDs[msg.ID] = true
		uw := NewUserMessageWidget(w.ctx, msg.Text)
		conv.messageWidgets[msg.ID] = uw
		conv.timelineBox.Append(uw)
	}
	conv.nextCursor = "20"
	conv.hasOlder = true
	conv.scroll.ScrollToBottom()

	trimmed := conv.maybeTrimTimelineHead()
	if trimmed != 70 {
		t.Fatalf("trimmed = %d, want 70 (130 -> 60)", trimmed)
	}
	if len(conv.messages) != keepTimelineMessages {
		t.Fatalf("len(conv.messages) = %d, want %d", len(conv.messages), keepTimelineMessages)
	}
	if conv.messages[0].ID != "m091" || conv.messages[len(conv.messages)-1].ID != "m150" {
		t.Fatalf("kept range = [%s..%s], want [m091..m150]", conv.messages[0].ID, conv.messages[len(conv.messages)-1].ID)
	}
	if !conv.trimmedHead || !conv.hasOlder {
		t.Fatalf("expected trimmedHead=true hasOlder=true, got %v/%v", conv.trimmedHead, conv.hasOlder)
	}
	if conv.seenMsgIDs["m021"] || conv.messageWidgets["m021"] != nil {
		t.Fatal("expected evicted message m021 cleared from seenMsgIDs and messageWidgets")
	}

	// Guard: revert staged must prevent eviction.
	conv.session.Revert = &opencode.SessionRevert{MessageID: "m100"}
	if got := conv.trimTimelineHead(10, 5); got != 0 {
		t.Fatalf("expected trimTimelineHead=0 while revert staged, got %d", got)
	}
	conv.session.Revert = nil

	// Guard: scrolled away from bottom must prevent maybeTrimTimelineHead.
	conv.scroll.Unbottom()
	if got := conv.trimTimelineHead(10, 5); got == 0 {
		t.Fatal("direct trimTimelineHead should still work in unit setup")
	}
	// Restore 60-message tail [m091..m150] with trimmedHead=true and test loadOlderConvMessages recovery.
	conv.messages = append([]opencode.Message(nil), allMsgs[90:]...)
	conv.seenMsgIDs = make(map[string]bool)
	for _, m := range conv.messages {
		conv.seenMsgIDs[m.ID] = true
	}
	conv.trimmedHead = true
	conv.hasOlder = true
	conv.nextCursor = ""

	w.loadOlderConvMessages(tab, conv)
	// Stop as soon as the single loadOlderConvMessages call finishes (before idle
	// auto-fill fires due to zero-height unrealized ScrolledWindow in headless tests).
	deadline := time.Now().Add(2 * time.Second)
	for conv.loadingOlder && time.Now().Before(deadline) {
		for glib.MainContextDefault().Pending() {
			glib.MainContextDefault().Iteration(false)
			if !conv.loadingOlder {
				break
			}
		}
		if conv.loadingOlder {
			time.Sleep(5 * time.Millisecond)
		}
	}

	if conv.trimmedHead {
		t.Fatal("expected trimmedHead cleared after loadOlderConvMessages")
	}
	if conv.messages[0].ID != "m041" {
		t.Fatalf("oldest loaded after scroll-up = %s, want m041", conv.messages[0].ID)
	}
	if len(conv.messages) != 110 {
		t.Fatalf("len(conv.messages) = %d, want 110 (50 older + 60 kept)", len(conv.messages))
	}
	if !conv.hasOlder || conv.nextCursor != "40" {
		t.Fatalf("pagination after reload = hasOlder:%v nextCursor:%q, want true/'40'", conv.hasOlder, conv.nextCursor)
	}
}
