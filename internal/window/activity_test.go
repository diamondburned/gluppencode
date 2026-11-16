package window

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"libdb.so/gluppencode/internal/opencode"
)

func TestFormatExecutingToolsLabel(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		tools map[string]string
		want  string
	}{
		"empty": {
			tools: nil,
			want:  "",
		},
		"single shell": {
			tools: map[string]string{"t1": "shell"},
			want:  "Running command...",
		},
		"single bash": {
			tools: map[string]string{"t1": "bash"},
			want:  "Running command...",
		},
		"single subagent": {
			tools: map[string]string{"t1": "subagent"},
			want:  "Waiting on subagent...",
		},
		"single task": {
			tools: map[string]string{"t1": "task"},
			want:  "Waiting on subagent...",
		},
		"single read": {
			tools: map[string]string{"t1": "read"},
			want:  "Running Read...",
		},
		"single custom tool": {
			tools: map[string]string{"t1": "nixos_nix"},
			want:  "Running Nixos_nix...",
		},
		"single unnamed tool": {
			tools: map[string]string{"t1": ""},
			want:  "Running tool...",
		},
		"multiple shells": {
			tools: map[string]string{"t1": "shell", "t2": "bash"},
			want:  "Running commands...",
		},
		"multiple subagents": {
			tools: map[string]string{"t1": "subagent", "t2": "task"},
			want:  "Waiting on subagents...",
		},
		"mixed tools": {
			tools: map[string]string{"t1": "subagent", "t2": "read"},
			want:  "Running tools...",
		},
		"multiple non-shell non-subagent tools": {
			tools: map[string]string{"t1": "read", "t2": "grep"},
			want:  "Running tools...",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := formatExecutingToolsLabel(tc.tools); got != tc.want {
				t.Errorf("formatExecutingToolsLabel(%v) = %q, want %q", tc.tools, got, tc.want)
			}
		})
	}
}

func TestConvTimelineActivityText(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		conv *convTimeline
		want string
	}{
		"nil timeline": {
			conv: nil,
			want: "",
		},
		"default waiting": {
			conv: &convTimeline{},
			want: "Waiting for model...",
		},
		"thinking": {
			conv: &convTimeline{activityPhase: activityThinking},
			want: "Thinking...",
		},
		"responding": {
			conv: &convTimeline{activityPhase: activityResponding},
			want: "Responding...",
		},
		"retrying without attempt": {
			conv: &convTimeline{activityPhase: activityRetrying},
			want: "Retrying...",
		},
		"retrying with attempt": {
			conv: &convTimeline{activityPhase: activityRetrying, retryAttempt: 3},
			want: "Retrying (attempt 3)...",
		},
		"compacting overrides thinking": {
			conv: &convTimeline{activityPhase: activityThinking, compacting: true},
			want: "Compacting context...",
		},
		"executing tool overrides compacting and phase": {
			conv: &convTimeline{
				activityPhase:  activityResponding,
				compacting:     true,
				executingTools: map[string]string{"t1": "subagent"},
			},
			want: "Waiting on subagent...",
		},
		"user shell overrides all": {
			conv: &convTimeline{
				activityPhase:    activityThinking,
				runningUserShell: true,
				executingTools:   map[string]string{"t1": "read"},
			},
			want: "Running command...",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := tc.conv.effectiveActivityText(); got != tc.want {
				t.Errorf("effectiveActivityText() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInferActivityStateFromMessages(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		msgs             []opencode.Message
		wantPhase        activityPhase
		wantRetryAttempt int
		wantCompacting   bool
	}{
		"empty": {
			msgs:      nil,
			wantPhase: activityWaitingModel,
		},
		"trailing idle message": {
			msgs:      []opencode.Message{{ID: "i1", Type: "idle"}},
			wantPhase: activityWaitingModel,
		},
		"trailing user message": {
			msgs:      []opencode.Message{{ID: "m1", Type: "user"}},
			wantPhase: activityThinking,
		},
		"trailing synthetic message": {
			msgs:      []opencode.Message{{ID: "s1", Type: "synthetic", Text: "continued"}},
			wantPhase: activityThinking,
		},
		"running compaction": {
			msgs:           []opencode.Message{{ID: "c1", Type: "compaction", Status: "running"}},
			wantPhase:      activityWaitingModel,
			wantCompacting: true,
		},
		"completed compaction": {
			msgs:      []opencode.Message{{ID: "c1", Type: "compaction", Status: "completed"}},
			wantPhase: activityThinking,
		},
		"failed compaction": {
			msgs:      []opencode.Message{{ID: "c1", Type: "compaction", Status: "failed"}},
			wantPhase: activityWaitingModel,
		},
		"completed assistant": {
			msgs: []opencode.Message{{
				ID:      "a1",
				Type:    "assistant",
				Content: []opencode.ContentPart{{Type: "text", Text: "done"}},
				Time: struct {
					Created   int64 `json:"created"`
					Streamed  int64 `json:"streamed,omitempty"`
					Completed int64 `json:"completed,omitempty"`
				}{Created: 1, Completed: 2},
			}},
			wantPhase: activityThinking,
		},
		"in-progress assistant retrying": {
			msgs: []opencode.Message{{
				ID:    "a1",
				Type:  "assistant",
				Retry: &opencode.MessageRetry{Attempt: 2, At: 1700000009000},
			}},
			wantPhase:        activityRetrying,
			wantRetryAttempt: 2,
		},
		"in-progress assistant without parts": {
			msgs:      []opencode.Message{{ID: "a1", Type: "assistant"}},
			wantPhase: activityThinking,
		},
		"in-progress assistant reasoning": {
			msgs: []opencode.Message{{
				ID:      "a1",
				Type:    "assistant",
				Content: []opencode.ContentPart{{Type: "reasoning", Text: "hmm"}},
			}},
			wantPhase: activityThinking,
		},
		"in-progress assistant text": {
			msgs: []opencode.Message{{
				ID:      "a1",
				Type:    "assistant",
				Content: []opencode.ContentPart{{Type: "reasoning"}, {Type: "text", Text: "hi"}},
			}},
			wantPhase: activityResponding,
		},
		"in-progress assistant streaming tool input": {
			msgs: []opencode.Message{{
				ID:   "a1",
				Type: "assistant",
				Content: []opencode.ContentPart{{
					Type:  "tool",
					ID:    "t1",
					Name:  "shell",
					State: opencode.ToolState{Status: "streaming"},
				}},
			}},
			wantPhase: activityResponding,
		},
		"in-progress assistant completed tool waiting for next step": {
			msgs: []opencode.Message{{
				ID:   "a1",
				Type: "assistant",
				Content: []opencode.ContentPart{{
					Type:  "tool",
					ID:    "t1",
					Name:  "read",
					State: opencode.ToolState{Status: "completed"},
				}},
			}},
			wantPhase: activityThinking,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			conv := &convTimeline{retryAttempt: 99, compacting: true}
			conv.inferActivityStateFromMessages(tc.msgs)
			if conv.activityPhase != tc.wantPhase || conv.retryAttempt != tc.wantRetryAttempt || conv.compacting != tc.wantCompacting {
				t.Errorf("got phase=%v retryAttempt=%d compacting=%v, want phase=%v retryAttempt=%d compacting=%v",
					conv.activityPhase, conv.retryAttempt, conv.compacting,
					tc.wantPhase, tc.wantRetryAttempt, tc.wantCompacting)
			}
		})
	}
}

func TestSyncToolPartExecutingTracking(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		finished      map[string]bool
		part          opencode.ContentPart
		wantExecuting bool
		wantFinished  bool
	}{
		"streaming args does not mark executing": {
			part:          opencode.ContentPart{ID: "t1", Name: "bash", State: opencode.ToolState{Status: "streaming"}},
			wantExecuting: false,
		},
		"running marks executing": {
			part:          opencode.ContentPart{ID: "t1", Name: "bash", State: opencode.ToolState{Status: "running"}},
			wantExecuting: true,
		},
		"stale running snapshot does not resurrect finished tool": {
			finished:      map[string]bool{"t1": true},
			part:          opencode.ContentPart{ID: "t1", Name: "bash", State: opencode.ToolState{Status: "running"}},
			wantExecuting: false,
			wantFinished:  true,
		},
		"completed marks finished and clears executing": {
			part:          opencode.ContentPart{ID: "t1", Name: "bash", State: opencode.ToolState{Status: "completed"}},
			wantExecuting: false,
			wantFinished:  true,
		},
		"failed marks finished and clears executing": {
			part:          opencode.ContentPart{ID: "t1", Name: "bash", State: opencode.ToolState{Status: "failed"}},
			wantExecuting: false,
			wantFinished:  true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			conv := &convTimeline{
				executingTools:     make(map[string]string),
				finishedTools:      tc.finished,
				blockingTools:      make(map[string]time.Time),
				blockingShellTools: make(map[string]bool),
				blockingShellIDs:   make(map[string]string),
				backgroundShellIDs: make(map[string]bool),
				toolNames:          make(map[string]string),
			}
			conv.syncToolPart(tc.part)
			if _, ok := conv.executingTools[tc.part.ID]; ok != tc.wantExecuting {
				t.Errorf("executingTools[%q] = %v, want %v", tc.part.ID, ok, tc.wantExecuting)
			}
			if conv.finishedTools[tc.part.ID] != tc.wantFinished {
				t.Errorf("finishedTools[%q] = %v, want %v", tc.part.ID, conv.finishedTools[tc.part.ID], tc.wantFinished)
			}
		})
	}
}

func newActivityTestWindow(srvURL string) *Window {
	return &Window{
		ctx:                   nil,
		client:                &opencode.Client{BaseURL: srvURL},
		viewStack:             adw.NewViewStack(),
		tokRateStatusLabel:    gtk.NewLabel(""),
		modelSearchEntry:      gtk.NewSearchEntry(),
		modelListBox:          gtk.NewListBox(),
		tabs:                  make(map[string]*sessionTab),
		childToRoot:           make(map[string]string),
		sessionTouched:        make(map[string]time.Time),
		modelNames:            make(map[string]string),
		modelLimits:           make(map[string]int),
		branchCache:           make(map[string]string),
		modelLastUsed:         make(map[string]int64),
		expandedProjects:      make(map[string]bool),
		collapsedProjects:     make(map[string]bool),
		visibleSessions:       make(map[string]bool),
		archivedSessions:      make(map[string]bool),
		projectSessions:       make(map[string][]opencode.Session),
		activeShells:          make(map[string]opencode.ShellItem),
		shellOutputs:          make(map[string]string),
		shellOutputLabels:     make(map[string]*gtk.Label),
		closeBtnSessID:        make(map[uintptr]string),
		hoveredRow:            make(map[uintptr]bool),
		takeChargeInFlight:    make(map[string]bool),
		resolvedSessionTitles: make(map[string]string),
		resolvingSessions:     make(map[string][]func(string)),
	}
}

func TestActivityStatusLifecycleSSE(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	win := newActivityTestWindow(srv.URL)
	win.ctx = t.Context()

	tab := win.addSessionTab(opencode.Session{
		ID:       "ses_act_1",
		Title:    "Activity Test",
		Location: opencode.Location{Directory: "/tmp/proj"},
	})
	tab.main.loaded = true
	win.viewStack.SetVisibleChildName("ses_act_1")

	steps := []struct {
		name        string
		event       opencode.Event
		wantVisible bool
		wantLabel   string
	}{
		{
			name:        "execution started waits for model",
			event:       opencode.Event{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"ses_act_1"}`)},
			wantVisible: true,
			wantLabel:   "Waiting for model...",
		},
		{
			name:        "inbox delivered switches to thinking",
			event:       opencode.Event{Type: "session.inbox.delivered", Data: json.RawMessage(`{"sessionID":"ses_act_1","inboxID":"msg_u1"}`)},
			wantVisible: true,
			wantLabel:   "Thinking...",
		},
		{
			name:        "step started shows thinking",
			event:       opencode.Event{Type: "session.step.started", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","agent":"build","model":{"providerID":"p","id":"m"}}`)},
			wantVisible: true,
			wantLabel:   "Thinking...",
		},
		{
			name:        "reasoning started shows thinking",
			event:       opencode.Event{Type: "session.reasoning.started", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","ordinal":0}`)},
			wantVisible: true,
			wantLabel:   "Thinking...",
		},
		{
			name:        "reasoning delta keeps thinking",
			event:       opencode.Event{Type: "session.reasoning.delta", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","ordinal":0,"delta":"let me check"}`)},
			wantVisible: true,
			wantLabel:   "Thinking...",
		},
		{
			name:        "text started shows responding",
			event:       opencode.Event{Type: "session.text.started", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","ordinal":1}`)},
			wantVisible: true,
			wantLabel:   "Responding...",
		},
		{
			name:        "step ended with no executing tools switches to thinking",
			event:       opencode.Event{Type: "session.step.ended", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1"}`)},
			wantVisible: true,
			wantLabel:   "Thinking...",
		},
		{
			name:        "next step started keeps thinking",
			event:       opencode.Event{Type: "session.step.started", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","agent":"build","model":{"providerID":"p","id":"m"}}`)},
			wantVisible: true,
			wantLabel:   "Thinking...",
		},
		{
			name:        "tool input streaming shows responding",
			event:       opencode.Event{Type: "session.tool.input.started", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","id":"call_1","name":"bash"}`)},
			wantVisible: true,
			wantLabel:   "Responding...",
		},
		{
			name:        "tool input delta keeps responding",
			event:       opencode.Event{Type: "session.tool.input.delta", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","id":"call_1","delta":"{\"command\":\"ls\"}"}`)},
			wantVisible: true,
			wantLabel:   "Responding...",
		},
		{
			name:        "tool called bash shows running command",
			event:       opencode.Event{Type: "session.tool.called", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","id":"call_1","name":"bash","input":{"command":"ls"}}`)},
			wantVisible: true,
			wantLabel:   "Running command...",
		},
		{
			name:        "step ended does not clear executing tool",
			event:       opencode.Event{Type: "session.step.ended", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1"}`)},
			wantVisible: true,
			wantLabel:   "Running command...",
		},
		{
			name:        "tool success switches to thinking",
			event:       opencode.Event{Type: "session.tool.success", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","id":"call_1","name":"bash"}`)},
			wantVisible: true,
			wantLabel:   "Thinking...",
		},
		{
			name:        "first parallel tool read",
			event:       opencode.Event{Type: "session.tool.called", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","id":"call_2","name":"read"}`)},
			wantVisible: true,
			wantLabel:   "Running Read...",
		},
		{
			name:        "second parallel tool grep switches to plural",
			event:       opencode.Event{Type: "session.tool.called", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","id":"call_3","name":"grep"}`)},
			wantVisible: true,
			wantLabel:   "Running tools...",
		},
		{
			name:        "first parallel tool finishes leaves remaining tool name",
			event:       opencode.Event{Type: "session.tool.success", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","id":"call_2","name":"read"}`)},
			wantVisible: true,
			wantLabel:   "Running Grep...",
		},
		{
			name:        "remaining parallel tool finishes",
			event:       opencode.Event{Type: "session.tool.failed", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","id":"call_3","name":"grep"}`)},
			wantVisible: true,
			wantLabel:   "Thinking...",
		},
		{
			name:        "parallel shells",
			event:       opencode.Event{Type: "session.tool.called", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","id":"sh_1","name":"shell"}`)},
			wantVisible: true,
			wantLabel:   "Running command...",
		},
		{
			name:        "second parallel shell switches to Running commands",
			event:       opencode.Event{Type: "session.tool.called", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","id":"sh_2","name":"bash"}`)},
			wantVisible: true,
			wantLabel:   "Running commands...",
		},
		{
			name:        "cleanup first shell",
			event:       opencode.Event{Type: "session.tool.success", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","id":"sh_1","name":"shell"}`)},
			wantVisible: true,
			wantLabel:   "Running command...",
		},
		{
			name:        "cleanup second shell",
			event:       opencode.Event{Type: "session.tool.success", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","id":"sh_2","name":"bash"}`)},
			wantVisible: true,
			wantLabel:   "Thinking...",
		},
		{
			name:        "parallel subagents",
			event:       opencode.Event{Type: "session.tool.called", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","id":"sub_1","name":"subagent"}`)},
			wantVisible: true,
			wantLabel:   "Waiting on subagent...",
		},
		{
			name:        "second parallel subagent",
			event:       opencode.Event{Type: "session.tool.called", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","id":"sub_2","name":"task"}`)},
			wantVisible: true,
			wantLabel:   "Waiting on subagents...",
		},
		{
			name:        "cleanup first subagent",
			event:       opencode.Event{Type: "session.tool.success", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","id":"sub_1","name":"subagent"}`)},
			wantVisible: true,
			wantLabel:   "Waiting on subagent...",
		},
		{
			name:        "cleanup second subagent",
			event:       opencode.Event{Type: "session.tool.success", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_1","id":"sub_2","name":"task"}`)},
			wantVisible: true,
			wantLabel:   "Thinking...",
		},
		{
			name:        "step failed before retry",
			event:       opencode.Event{Type: "session.step.failed", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_2"}`)},
			wantVisible: true,
			wantLabel:   "Waiting for model...",
		},
		{
			name:        "retry scheduled with attempt",
			event:       opencode.Event{Type: "session.retry.scheduled", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_2","attempt":2}`)},
			wantVisible: true,
			wantLabel:   "Retrying (attempt 2)...",
		},
		{
			name:        "session status retry without attempt preserves attempt 2",
			event:       opencode.Event{Type: "session.status", Data: json.RawMessage(`{"sessionID":"ses_act_1","status":{"type":"retry"}}`)},
			wantVisible: true,
			wantLabel:   "Retrying (attempt 2)...",
		},
		{
			name:        "session status busy clears retry back to thinking",
			event:       opencode.Event{Type: "session.status", Data: json.RawMessage(`{"sessionID":"ses_act_1","status":{"type":"busy"}}`)},
			wantVisible: true,
			wantLabel:   "Thinking...",
		},
		{
			name:        "retry scheduled again cleared by step started",
			event:       opencode.Event{Type: "session.retry.scheduled", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_2","attempt":1}`)},
			wantVisible: true,
			wantLabel:   "Retrying (attempt 1)...",
		},
		{
			name:        "step started clears retry to thinking",
			event:       opencode.Event{Type: "session.step.started", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_2","agent":"build","model":{"providerID":"p","id":"m"}}`)},
			wantVisible: true,
			wantLabel:   "Thinking...",
		},
		{
			name:        "compaction started shows compacting",
			event:       opencode.Event{Type: "session.compaction.started", Data: json.RawMessage(`{"sessionID":"ses_act_1","reason":"auto"}`)},
			wantVisible: true,
			wantLabel:   "Compacting context...",
		},
		{
			name:        "text delta during compaction keeps compacting",
			event:       opencode.Event{Type: "session.text.delta", Data: json.RawMessage(`{"sessionID":"ses_act_1","assistantMessageID":"msg_c","ordinal":0,"delta":"summary"}`)},
			wantVisible: true,
			wantLabel:   "Compacting context...",
		},
		{
			name:        "compaction ended resets to thinking",
			event:       opencode.Event{Type: "session.compaction.ended", Data: json.RawMessage(`{"sessionID":"ses_act_1","reason":"auto"}`)},
			wantVisible: true,
			wantLabel:   "Thinking...",
		},
		{
			name:        "compaction started before failure",
			event:       opencode.Event{Type: "session.compaction.started", Data: json.RawMessage(`{"sessionID":"ses_act_1","reason":"auto"}`)},
			wantVisible: true,
			wantLabel:   "Compacting context...",
		},
		{
			name:        "compaction failed while active returns to waiting for model",
			event:       opencode.Event{Type: "session.compaction.failed", Data: json.RawMessage(`{"sessionID":"ses_act_1","reason":"auto"}`)},
			wantVisible: true,
			wantLabel:   "Waiting for model...",
		},
		{
			name:        "inbox delivered after compaction failure resumes thinking",
			event:       opencode.Event{Type: "session.inbox.delivered", Data: json.RawMessage(`{"sessionID":"ses_act_1","inboxID":"msg_u2"}`)},
			wantVisible: true,
			wantLabel:   "Thinking...",
		},
		{
			name:        "user shell started shows running command",
			event:       opencode.Event{Type: "session.shell.started", Data: json.RawMessage(`{"sessionID":"ses_act_1"}`)},
			wantVisible: true,
			wantLabel:   "Running command...",
		},
		{
			name:        "user shell ended returns to thinking",
			event:       opencode.Event{Type: "session.shell.ended", Data: json.RawMessage(`{"sessionID":"ses_act_1"}`)},
			wantVisible: true,
			wantLabel:   "Thinking...",
		},
		{
			name:        "permission asked hides activity label",
			event:       opencode.Event{Type: "permission.asked", Data: json.RawMessage(`{"id":"per_1","sessionID":"ses_act_1","action":"bash","resources":["ls"]}`)},
			wantVisible: false,
		},
		{
			name:        "permission replied restores thinking label",
			event:       opencode.Event{Type: "permission.replied", Data: json.RawMessage(`{"sessionID":"ses_act_1","requestID":"per_1","reply":"once"}`)},
			wantVisible: true,
			wantLabel:   "Thinking...",
		},
		{
			name:        "session idle hides label and clears state",
			event:       opencode.Event{Type: "session.idle", Data: json.RawMessage(`{"sessionID":"ses_act_1"}`)},
			wantVisible: false,
		},
		{
			name:        "session status retry without attempt shows generic retrying",
			event:       opencode.Event{Type: "session.status", Data: json.RawMessage(`{"sessionID":"ses_act_1","status":{"type":"retry"}}`)},
			wantVisible: true,
			wantLabel:   "Retrying...",
		},
		{
			name:        "execution succeeded hides label",
			event:       opencode.Event{Type: "session.execution.succeeded", Data: json.RawMessage(`{"sessionID":"ses_act_1"}`)},
			wantVisible: false,
		},
		{
			name:        "manual compaction while idle shows label",
			event:       opencode.Event{Type: "session.compaction.started", Data: json.RawMessage(`{"sessionID":"ses_act_1","reason":"manual"}`)},
			wantVisible: true,
			wantLabel:   "Compacting context...",
		},
		{
			name:        "compaction delta keeps compacting",
			event:       opencode.Event{Type: "session.compaction.delta", Data: json.RawMessage(`{"sessionID":"ses_act_1","text":"chunk"}`)},
			wantVisible: true,
			wantLabel:   "Compacting context...",
		},
		{
			name:        "manual compaction failed hides label when idle",
			event:       opencode.Event{Type: "session.compaction.failed", Data: json.RawMessage(`{"sessionID":"ses_act_1","reason":"manual"}`)},
			wantVisible: false,
		},
		{
			name:        "user shell while idle shows label",
			event:       opencode.Event{Type: "session.shell.started", Data: json.RawMessage(`{"sessionID":"ses_act_1"}`)},
			wantVisible: true,
			wantLabel:   "Running command...",
		},
		{
			name:        "user shell ended hides label when idle",
			event:       opencode.Event{Type: "session.shell.ended", Data: json.RawMessage(`{"sessionID":"ses_act_1"}`)},
			wantVisible: false,
		},
	}

	for _, step := range steps {
		win.handleEvent(step.event)
		gotVis := tab.main.thinkingLabel.Visible()
		if gotVis != step.wantVisible {
			t.Fatalf("%s: thinkingLabel.Visible() = %v, want %v (label=%q)",
				step.name, gotVis, step.wantVisible, tab.main.thinkingLabel.Text())
		}
		if step.wantVisible {
			if gotLbl := tab.main.thinkingLabel.Text(); gotLbl != step.wantLabel {
				t.Fatalf("%s: thinkingLabel.Text() = %q, want %q", step.name, gotLbl, step.wantLabel)
			}
		}
	}
}

func TestActivityStatusBackgroundReconcileAndSubagent(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}

	var (
		mu                 sync.Mutex
		messagesBySess     = make(map[string]string)
		bgCalled           bool
		holdNextMsgStart   chan struct{}
		holdNextMsgRelease chan struct{}
		holdNextMsgDone    chan struct{}
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/message"):
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			sessID := parts[len(parts)-2]
			mu.Lock()
			body := messagesBySess[sessID]
			startedCh := holdNextMsgStart
			releaseCh := holdNextMsgRelease
			doneCh := holdNextMsgDone
			holdNextMsgStart = nil
			holdNextMsgRelease = nil
			holdNextMsgDone = nil
			mu.Unlock()
			if startedCh != nil {
				close(startedCh)
			}
			if releaseCh != nil {
				<-releaseCh
			}
			if body == "" {
				body = `{"data":[]}`
			}
			_, _ = w.Write([]byte(body))
			if doneCh != nil {
				close(doneCh)
			}
		case r.Method == http.MethodGet && (strings.HasSuffix(r.URL.Path, "/form") || strings.HasSuffix(r.URL.Path, "/permission") || r.URL.Path == "/api/shell"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/session":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/background"):
			mu.Lock()
			bgCalled = true
			mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	win := newActivityTestWindow(srv.URL)
	win.ctx = t.Context()

	tab := win.addSessionTab(opencode.Session{
		ID:       "ses_root",
		Title:    "Root",
		Location: opencode.Location{Directory: "/tmp/proj"},
	})
	tab.main.loaded = true
	win.viewStack.SetVisibleChildName("ses_root")

	drainMain := func(until func() bool) {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			for glib.MainContextDefault().Pending() {
				glib.MainContextDefault().Iteration(false)
			}
			if until() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	// 1. Start a blocking shell tool, then trigger an in-flight reconcileTail whose snapshot
	// still sees the tool as running, and immediately background the session via Ctrl+B.
	mu.Lock()
	messagesBySess["ses_root"] = `{"data":[{"id":"msg_1","type":"assistant","content":[{"type":"tool","id":"call_bg","name":"bash","state":{"status":"running","input":{"command":"sleep 10"}}}]}]}`
	mu.Unlock()

	win.handleEvent(opencode.Event{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"ses_root"}`)})
	win.handleEvent(opencode.Event{Type: "session.tool.called", Data: json.RawMessage(`{"sessionID":"ses_root","assistantMessageID":"msg_1","id":"call_bg","name":"bash","input":{"command":"sleep 10"}}`)})
	if got := tab.main.thinkingLabel.Text(); got != "Running command..." {
		t.Fatalf("before background: label = %q, want %q", got, "Running command...")
	}

	if !win.backgroundCurrentSession() {
		t.Fatal("expected backgroundCurrentSession() to return true")
	}
	if got := tab.main.thinkingLabel.Text(); got != "Thinking..." {
		t.Fatalf("after background: label = %q, want %q", got, "Thinking...")
	}

	// Drain the async reconcileTail and /background requests; the stale running snapshot must not restore "Running command...".
	drainMain(func() bool {
		mu.Lock()
		done := bgCalled
		mu.Unlock()
		return done && len(tab.main.messages) == 1
	})
	if got := tab.main.thinkingLabel.Text(); got != "Thinking..." {
		t.Fatalf("after stale reconcileTail: label = %q, want %q", got, "Thinking...")
	}

	// 2. Missed SSE tool completion gets reconciled by reconcileTail when the server shows completed.
	tab.main.finishedTools = make(map[string]bool)
	win.handleEvent(opencode.Event{Type: "session.tool.called", Data: json.RawMessage(`{"sessionID":"ses_root","assistantMessageID":"msg_1","id":"call_miss","name":"read"}`)})
	if got := tab.main.thinkingLabel.Text(); got != "Running Read..." {
		t.Fatalf("during call_miss: label = %q, want %q", got, "Running Read...")
	}
	mu.Lock()
	messagesBySess["ses_root"] = `{"data":[{"id":"msg_1","type":"assistant","content":[{"type":"tool","id":"call_miss","name":"read","state":{"status":"completed"}}]}]}`
	mu.Unlock()
	win.reconcileTail("ses_root")
	drainMain(func() bool {
		return len(tab.main.executingTools) == 0
	})
	if got := tab.main.thinkingLabel.Text(); got != "Thinking..." {
		t.Fatalf("after reconcileTail completion: label = %q, want %q", got, "Thinking...")
	}

	// 3. Stale reconcileTail snapshot taken during compaction must not resurrect "Compacting context..." after compaction ends.
	startedCh := make(chan struct{})
	releaseCh := make(chan struct{})
	doneCh := make(chan struct{})
	mu.Lock()
	messagesBySess["ses_root"] = `{"data":[{"id":"cmp_1","type":"compaction","status":"running","reason":"auto"}]}`
	holdNextMsgStart = startedCh
	holdNextMsgRelease = releaseCh
	holdNextMsgDone = doneCh
	mu.Unlock()
	win.handleEvent(opencode.Event{Type: "session.compaction.started", Data: json.RawMessage(`{"sessionID":"ses_root","reason":"auto"}`)})
	if got := tab.main.thinkingLabel.Text(); got != "Compacting context..." {
		t.Fatalf("during compaction: label = %q, want %q", got, "Compacting context...")
	}
	<-startedCh
	mu.Lock()
	messagesBySess["ses_root"] = `{"data":[{"id":"cmp_1","type":"compaction","status":"completed","reason":"auto"}]}`
	mu.Unlock()
	win.handleEvent(opencode.Event{Type: "session.compaction.ended", Data: json.RawMessage(`{"sessionID":"ses_root","reason":"auto"}`)})
	drainMain(func() bool {
		return tab.main.seenMsgIDs["cmp_1"]
	})
	close(releaseCh)
	<-doneCh
	time.Sleep(20 * time.Millisecond)
	var staleDrained bool
	glib.IdleAdd(func() {
		staleDrained = true
	})
	drainMain(func() bool {
		return staleDrained
	})
	if tab.main.compacting || tab.main.thinkingLabel.Text() != "Thinking..." {
		t.Fatalf("after stale compaction reconcileTail: compacting=%v label=%q, want false / %q",
			tab.main.compacting, tab.main.thinkingLabel.Text(), "Thinking...")
	}

	// 4. Missed SSE compaction completion gets reconciled by reconcileTail when the server shows completed.
	mu.Lock()
	messagesBySess["ses_root"] = `{"data":[{"id":"cmp_2","type":"compaction","status":"running","reason":"auto"}]}`
	mu.Unlock()
	win.handleEvent(opencode.Event{Type: "session.compaction.started", Data: json.RawMessage(`{"sessionID":"ses_root","reason":"auto"}`)})
	drainMain(func() bool {
		return tab.main.seenMsgIDs["cmp_2"]
	})
	mu.Lock()
	messagesBySess["ses_root"] = `{"data":[{"id":"cmp_2","type":"compaction","status":"completed","reason":"auto"}]}`
	mu.Unlock()
	win.reconcileTail("ses_root")
	drainMain(func() bool {
		return !tab.main.compacting
	})
	if tab.main.compacting || tab.main.thinkingLabel.Text() != "Thinking..." {
		t.Fatalf("after reconcileTail compaction completion: compacting=%v label=%q, want false / %q",
			tab.main.compacting, tab.main.thinkingLabel.Text(), "Thinking...")
	}

	// 5. Opening a running subagent initializes its timeline label and handles SSE transitions.
	mu.Lock()
	messagesBySess["ses_sub"] = `{"data":[{"id":"sub_msg_1","type":"assistant","content":[{"type":"reasoning","text":"analyzing"}]}]}`
	mu.Unlock()
	win.openSubagent(tab, opencode.Session{
		ID:       "ses_sub",
		ParentID: "ses_root",
		Title:    "Explore",
		Time:     opencode.SessionTime{Created: 10, Busy: 20},
	})
	subConv := tab.subTimelines["ses_sub"]
	if subConv == nil {
		t.Fatal("expected subagent timeline to be created")
	}
	if !subConv.thinkingLabel.Visible() || subConv.thinkingLabel.Text() != "Waiting for model..." {
		t.Fatalf("initial subagent label = (%v, %q), want (true, %q)", subConv.thinkingLabel.Visible(), subConv.thinkingLabel.Text(), "Waiting for model...")
	}
	drainMain(func() bool {
		return subConv.loaded
	})
	if got := subConv.thinkingLabel.Text(); got != "Thinking..." {
		t.Fatalf("loaded subagent label = %q, want %q", got, "Thinking...")
	}

	win.handleEvent(opencode.Event{Type: "session.tool.called", Data: json.RawMessage(`{"sessionID":"ses_sub","assistantMessageID":"sub_msg_1","id":"sub_call_1","name":"read"}`)})
	if got := subConv.thinkingLabel.Text(); got != "Running Read..." {
		t.Fatalf("subagent executing tool label = %q, want %q", got, "Running Read...")
	}
	win.handleEvent(opencode.Event{Type: "session.idle", Data: json.RawMessage(`{"sessionID":"ses_sub"}`)})
	if subConv.thinkingLabel.Visible() {
		t.Fatalf("expected subagent thinkingLabel hidden on idle, still visible with %q", subConv.thinkingLabel.Text())
	}
}
