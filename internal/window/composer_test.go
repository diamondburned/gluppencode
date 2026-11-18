package window

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4-sourceview/pkg/gtksource/v5"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"libdb.so/gluppencode/internal/opencode"
)

func TestScanComposerMarkdownSpans(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []mdSpan
	}{
		{
			name: "bold asterisks includes delimiters",
			text: "hello **asd** world",
			want: []mdSpan{
				{kind: mdSpanBold, start: 6, end: 13},
			},
		},
		{
			name: "bold underscores includes delimiters",
			text: "__bold__",
			want: []mdSpan{
				{kind: mdSpanBold, start: 0, end: 8},
			},
		},
		{
			name: "italic asterisks and underscores",
			text: "*one* and _two_",
			want: []mdSpan{
				{kind: mdSpanItalic, start: 0, end: 5},
				{kind: mdSpanItalic, start: 10, end: 15},
			},
		},
		{
			name: "intra-word underscores ignored",
			text: "foo_bar_baz and some__identifier__name",
			want: nil,
		},
		{
			name: "nested bold and italic",
			text: "**bold *inner* text**",
			want: []mdSpan{
				{kind: mdSpanItalic, start: 7, end: 14},
				{kind: mdSpanBold, start: 0, end: 21},
			},
		},
		{
			name: "strikethrough",
			text: "old ~~removed~~ text",
			want: []mdSpan{
				{kind: mdSpanStrike, start: 4, end: 15},
			},
		},
		{
			name: "inline code suppresses inner emphasis",
			text: "run `**not bold**` now",
			want: []mdSpan{
				{kind: mdSpanMono, start: 4, end: 18},
			},
		},
		{
			name: "double backtick inline code",
			text: "use ``a ` b`` here",
			want: []mdSpan{
				{kind: mdSpanMono, start: 4, end: 13},
			},
		},
		{
			name: "fenced code block closed",
			text: "before\n```go\n**not bold**\n```\nafter",
			want: []mdSpan{
				{kind: mdSpanCodeBlock, start: 7, end: 29},
			},
		},
		{
			name: "fenced code block unclosed highlights while typing",
			text: "```ts\nconst x = 1",
			want: []mdSpan{
				{kind: mdSpanCodeBlock, start: 0, end: 17},
			},
		},
		{
			name: "tilde fenced code block",
			text: "~~~\nhello\n~~~",
			want: []mdSpan{
				{kind: mdSpanCodeBlock, start: 0, end: 13},
			},
		},
		{
			name: "atx heading",
			text: "## Heading *sub*",
			want: []mdSpan{
				{kind: mdSpanBold, start: 0, end: 16},
				{kind: mdSpanItalic, start: 11, end: 16},
			},
		},
		{
			name: "escaped markers ignored",
			text: "\\*not italic\\* and \\`not code\\`",
			want: nil,
		},
		{
			name: "utf8 rune offsets",
			text: "héllo 🚀 **bold** `code`",
			want: []mdSpan{
				{kind: mdSpanMono, start: 17, end: 23},
				{kind: mdSpanBold, start: 8, end: 16},
			},
		},
		{
			name: "commonmark rule of 3 allows **x*y**",
			text: "**x*y**",
			want: []mdSpan{
				{kind: mdSpanBold, start: 0, end: 7},
			},
		},
		{
			name: "atx heading isolates unclosed inline spans",
			text: "check `unclosed\n## Heading\nuse `bar`",
			want: []mdSpan{
				{kind: mdSpanBold, start: 16, end: 26},
				{kind: mdSpanMono, start: 31, end: 36},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := scanComposerMarkdownSpans(tc.text)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("scanComposerMarkdownSpans(%q) = %+v, want %+v", tc.text, got, tc.want)
			}
		})
	}
}

func TestScanComposerMarkdownSpansLinearTime(t *testing.T) {
	input := strings.Repeat("x* ", 40000)
	start := time.Now()
	spans := scanComposerMarkdownSpans(input)
	elapsed := time.Since(start)
	if len(spans) != 0 {
		t.Fatalf("expected 0 spans for unmatched closers, got %d", len(spans))
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("scanComposerMarkdownSpans took %v on 120KB unmatched closers, want <100ms", elapsed)
	}
}

func TestComposerEnterAction(t *testing.T) {
	tests := []struct {
		name            string
		expanded        bool
		ctrlEnterToSend bool
		capture         bool
		keyval          uint
		state           gdk.ModifierType
		wantDelivery    opencode.InboxDelivery
		wantHandled     bool
	}{
		{
			name:         "compact enter queues in bubble phase",
			keyval:       gdk.KEY_Return,
			wantDelivery: opencode.DeliveryQueue,
			wantHandled:  true,
		},
		{
			name:        "compact enter ignored in capture phase",
			capture:     true,
			keyval:      gdk.KEY_Return,
			wantHandled: false,
		},
		{
			name:         "compact ctrl+enter steers when Enter sends",
			capture:      true,
			keyval:       gdk.KEY_Return,
			state:        gdk.ControlMask,
			wantDelivery: opencode.DeliverySteer,
			wantHandled:  true,
		},
		{
			name:            "compact ctrl+enter queues when Ctrl+Enter sends",
			ctrlEnterToSend: true,
			capture:         true,
			keyval:          gdk.KEY_KP_Enter,
			state:           gdk.ControlMask,
			wantDelivery:    opencode.DeliveryQueue,
			wantHandled:     true,
		},
		{
			name:            "compact plain enter inserts newline when Ctrl+Enter sends",
			ctrlEnterToSend: true,
			keyval:          gdk.KEY_Return,
			wantHandled:     false,
		},
		{
			name:        "expanded plain enter inserts newline",
			expanded:    true,
			keyval:      gdk.KEY_Return,
			wantHandled: false,
		},
		{
			name:        "expanded shift+enter inserts newline",
			expanded:    true,
			capture:     true,
			keyval:      gdk.KEY_Return,
			state:       gdk.ShiftMask,
			wantHandled: false,
		},
		{
			name:         "expanded alt+enter queues",
			expanded:     true,
			capture:      true,
			keyval:       gdk.KEY_Return,
			state:        gdk.AltMask,
			wantDelivery: opencode.DeliveryQueue,
			wantHandled:  true,
		},
		{
			name:        "expanded shift+alt+enter ignored",
			expanded:    true,
			capture:     true,
			keyval:      gdk.KEY_Return,
			state:       gdk.ShiftMask | gdk.AltMask,
			wantHandled: false,
		},
		{
			name:        "compact alt+enter ignored",
			expanded:    false,
			capture:     false,
			keyval:      gdk.KEY_Return,
			state:       gdk.AltMask,
			wantHandled: false,
		},
		{
			name:            "expanded ctrl+enter steers even when Ctrl+EnterToSend is true",
			expanded:        true,
			ctrlEnterToSend: true,
			capture:         true,
			keyval:          gdk.KEY_Return,
			state:           gdk.ControlMask,
			wantDelivery:    opencode.DeliverySteer,
			wantHandled:     true,
		},
		{
			name:        "non-return key ignored",
			expanded:    true,
			keyval:      gdk.KEY_a,
			state:       gdk.ControlMask,
			wantHandled: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := &Window{
				composerExpanded: tc.expanded,
				prefs:            Preferences{CtrlEnterToSend: tc.ctrlEnterToSend},
			}
			gotDelivery, gotHandled := w.composerEnterAction(tc.capture, tc.keyval, tc.state)
			if gotHandled != tc.wantHandled || gotDelivery != tc.wantDelivery {
				t.Fatalf("composerEnterAction() = (%q, %v), want (%q, %v)", gotDelivery, gotHandled, tc.wantDelivery, tc.wantHandled)
			}
		})
	}
}

func TestComposerMultilineAndMarkdownWidgets(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}
	adw.Init()
	gtksource.Init()

	builder := gtk.NewBuilderFromString(windowUI)
	inputView := builder.GetObject("inputView").Cast().(*gtksource.View)
	inputBuf := inputView.Buffer().Cast().(*gtksource.Buffer)

	w := &Window{
		composerExpandBtn: builder.GetObject("composerExpandBtn").Cast().(*gtk.Button),
		inputScroll:       builder.GetObject("inputScroll").Cast().(*gtk.ScrolledWindow),
		inputView:         inputView,
		inputBuffer:       inputBuf,
		sendBtn:           builder.GetObject("sendBtn").Cast().(*gtk.Button),
	}

	w.setupComposerMultiline()
	w.setupComposerMarkdown()
	inputBuf.ConnectChanged(func() {
		w.syncInputScrollPolicy()
		w.applyComposerMarkdownHighlighting()
	})

	if w.composerExpandBtn.IconName() != "unfold-more-symbolic" {
		t.Fatalf("IconName = %q, want unfold-more-symbolic", w.composerExpandBtn.IconName())
	}

	w.composerExpandBtn.Emit("clicked")
	for glib.MainContextDefault().Iteration(false) {
	}
	if !w.composerExpanded {
		t.Fatal("expected composerExpanded to be true after clicking composerExpandBtn")
	}
	if w.composerExpandBtn.IconName() != "unfold-less-symbolic" {
		t.Fatalf("IconName = %q, want unfold-less-symbolic", w.composerExpandBtn.IconName())
	}
	if w.inputScroll.MinContentHeight() != composerExpandedMinHeight {
		t.Fatalf("MinContentHeight = %d, want %d", w.inputScroll.MinContentHeight(), composerExpandedMinHeight)
	}
	if got := w.sendBtn.TooltipText(); !strings.Contains(got, "Alt+Enter") {
		t.Fatalf("expanded sendBtn tooltip = %q, want Alt+Enter hint", got)
	}

	w.composerExpandBtn.Emit("clicked")
	for glib.MainContextDefault().Iteration(false) {
	}
	if w.composerExpanded {
		t.Fatal("expected composerExpanded to be false after second click")
	}
	if w.composerExpandBtn.IconName() != "unfold-more-symbolic" {
		t.Fatalf("IconName = %q, want unfold-more-symbolic", w.composerExpandBtn.IconName())
	}
	if w.inputScroll.MinContentHeight() != -1 {
		t.Fatalf("MinContentHeight = %d, want -1", w.inputScroll.MinContentHeight())
	}
	if w.inputScroll.MaxContentHeight() != composerCollapsedMaxHeight {
		t.Fatalf("MaxContentHeight = %d, want %d", w.inputScroll.MaxContentHeight(), composerCollapsedMaxHeight)
	}
	if got := w.sendBtn.TooltipText(); got != "Send Message (Enter)" {
		t.Fatalf("collapsed sendBtn tooltip = %q, want %q", got, "Send Message (Enter)")
	}

	inputBuf.SetText("**bold** and `mono` foo_bar_baz")
	for glib.MainContextDefault().Iteration(false) {
	}

	sBold := inputBuf.IterAtOffset(0)
	if !sBold.HasTag(w.mdTagBold) {
		t.Fatal("expected glup-md-bold tag at start of **bold**")
	}
	sMono := inputBuf.IterAtOffset(14)
	if !sMono.HasTag(w.mdTagMono) {
		t.Fatal("expected glup-md-mono tag inside `mono`")
	}
	sSnake := inputBuf.IterAtOffset(24) // inside _bar_ of foo_bar_baz
	if tags := sSnake.Tags(); len(tags) != 0 {
		t.Fatalf("expected no tags on intra-word underscore in foo_bar_baz, got %d", len(tags))
	}
}

func TestComposerAutoCollapseOnSend(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/mcp":
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	win, _ := newTestMcpSheetWindow(t, srv.URL, t.TempDir())
	win.setComposerExpanded(true)

	// Empty/whitespace send must not collapse.
	win.inputBuffer.SetText("   \n  ")
	win.sendPromptWithDelivery(opencode.DeliveryQueue)
	if !win.composerExpanded {
		t.Fatal("expected whitespace-only send to keep composerExpanded true")
	}

	// Valid send collapses back to compact mode.
	win.inputBuffer.SetText("/mcp")
	win.sendPromptWithDelivery(opencode.DeliveryQueue)
	if win.composerExpanded {
		t.Fatal("expected sendPromptWithDelivery to collapse composerExpanded to false")
	}
}

func TestEscapeDoublePressStopSession(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}
	adw.Init()

	var interruptCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/interrupt") {
			interruptCalls.Add(1)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	builder := gtk.NewBuilderFromString(windowUI)
	stopBtn := builder.GetObject("stopBtn").Cast().(*gtk.Button)
	stopConfirmBtn := builder.GetObject("stopConfirmBtn").Cast().(*gtk.Button)

	vs := adw.NewViewStack()
	win := &Window{
		ctx:              t.Context(),
		client:           &opencode.Client{BaseURL: srv.URL},
		viewStack:        vs,
		tabs:             make(map[string]*sessionTab),
		stopBtn:          stopBtn,
		stopConfirmBtn:   stopConfirmBtn,
		escStopTimeoutMS: 60,
	}
	stopConfirmBtn.ConnectClicked(win.interruptSession)

	sess := opencode.Session{ID: "ses_busy"}
	conv := &convTimeline{session: sess, active: false}
	pageBox := gtk.NewBox(gtk.OrientationVertical, 0)
	vs.AddNamed(pageBox, sess.ID)
	vs.SetVisibleChildName(sess.ID)
	win.tabs[sess.ID] = &sessionTab{main: conv}

	// Esc while idle must not arm.
	if win.handleEscapeStop(0) {
		t.Fatal("expected handleEscapeStop to return false while session is idle")
	}
	if stopConfirmBtn.Visible() || stopBtn.HasCSSClass("glup-stop-armed") {
		t.Fatal("expected stop confirmation UI hidden when idle")
	}

	// Mark session active.
	conv.active = true
	win.updateBusyState(true)

	// Modified Esc (e.g. Shift+Esc) must not arm.
	if win.handleEscapeStop(gdk.ShiftMask) {
		t.Fatal("expected Shift+Esc to be ignored")
	}

	// First Esc arms stop confirmation and pulses stopBtn.
	if !win.handleEscapeStop(0) {
		t.Fatal("expected first Esc to arm stop confirmation")
	}
	if win.escStopArmedSess != sess.ID {
		t.Fatalf("escStopArmedSess = %q, want %q", win.escStopArmedSess, sess.ID)
	}
	if !stopConfirmBtn.Visible() {
		t.Fatal("expected stopConfirmBtn visible after first Esc")
	}
	if !stopBtn.HasCSSClass("glup-stop-armed") {
		t.Fatal("expected stopBtn to have glup-stop-armed class after first Esc")
	}
	if interruptCalls.Load() != 0 {
		t.Fatalf("interruptCalls = %d after first Esc, want 0", interruptCalls.Load())
	}

	// Timeout automatically disarms.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && win.escStopArmedSess != "" {
		glib.MainContextDefault().Iteration(false)
		time.Sleep(5 * time.Millisecond)
	}
	if win.escStopArmedSess != "" || stopConfirmBtn.Visible() || stopBtn.HasCSSClass("glup-stop-armed") {
		t.Fatal("expected stop confirmation to disarm after timeout")
	}

	// Arm again and press Esc a second time -> interrupts and disarms.
	if !win.handleEscapeStop(0) {
		t.Fatal("expected re-arming Esc to return true")
	}
	if !win.handleEscapeStop(0) {
		t.Fatal("expected second Esc to interrupt and return true")
	}
	if win.escStopArmedSess != "" || stopConfirmBtn.Visible() || stopBtn.HasCSSClass("glup-stop-armed") {
		t.Fatal("expected stop confirmation to disarm immediately after second Esc")
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && interruptCalls.Load() == 0 {
		glib.MainContextDefault().Iteration(false)
		time.Sleep(5 * time.Millisecond)
	}
	if got := interruptCalls.Load(); got != 1 {
		t.Fatalf("interruptCalls = %d after double Esc, want 1", got)
	}

	// Clicking stopConfirmBtn while armed also interrupts and disarms.
	if !win.handleEscapeStop(0) {
		t.Fatal("expected Esc to arm before clicking stopConfirmBtn")
	}
	stopConfirmBtn.Emit("clicked")
	for glib.MainContextDefault().Iteration(false) {
	}
	if win.escStopArmedSess != "" || stopConfirmBtn.Visible() {
		t.Fatal("expected clicking stopConfirmBtn to disarm")
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && interruptCalls.Load() < 2 {
		glib.MainContextDefault().Iteration(false)
		time.Sleep(5 * time.Millisecond)
	}
	if got := interruptCalls.Load(); got != 2 {
		t.Fatalf("interruptCalls = %d after clicking stopConfirmBtn, want 2", got)
	}

	// Becoming idle while armed disarms.
	if !win.handleEscapeStop(0) {
		t.Fatal("expected Esc to arm before becoming idle")
	}
	conv.active = false
	win.updateBusyState(false)
	if win.escStopArmedSess != "" || stopConfirmBtn.Visible() || stopBtn.HasCSSClass("glup-stop-armed") {
		t.Fatal("expected updateBusyState(false) to disarm stop confirmation")
	}
}
