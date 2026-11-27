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

func TestComposerInputHistoryWrappedLines(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}
	adw.Init()
	gtksource.Init()

	inputView := gtksource.NewView()
	inputBuf := inputView.Buffer().Cast().(*gtksource.Buffer)
	inputView.SetWrapMode(gtk.WrapWordChar)
	inputView.SetSizeRequest(180, -1)

	win := adw.NewWindow()
	win.SetDefaultSize(200, 200)
	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.Append(inputView)
	win.SetContent(box)
	win.Present()
	defer win.Close()

	pumpMainLoop(150 * time.Millisecond)
	if inputView.Width() == 0 {
		t.Skip("no surface allocation (broadway without client)")
	}

	w := &Window{
		inputView:    inputView,
		inputBuffer:  inputBuf,
		inputHistory: []string{"older prompt", "alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi omicron pi rho sigma tau upsilon phi chi psi omega"},
	}

	draft := "one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty twenty-one twenty-two"
	inputBuf.SetText(draft)
	pumpMainLoop(50 * time.Millisecond)

	var lineStarts []int
	for off := 0; off < inputBuf.CharCount(); off++ {
		if inputView.StartsDisplayLine(inputBuf.IterAtOffset(off)) {
			lineStarts = append(lineStarts, off)
		}
	}
	if len(lineStarts) < 3 {
		t.Fatalf("expected >= 3 wrapped display lines at width=%d, got %v", inputView.Width(), lineStarts)
	}

	// Display line 0: top=true, bottom=false.
	for _, off := range []int{lineStarts[0], (lineStarts[0] + lineStarts[1] - 1) / 2, lineStarts[1] - 1} {
		inputBuf.PlaceCursor(inputBuf.IterAtOffset(off))
		if !w.isInputCursorOnTopLine() || w.isInputCursorOnBottomLine() {
			t.Fatalf("line 0 offset %d: got top=%v bottom=%v, want top=true bottom=false",
				off, w.isInputCursorOnTopLine(), w.isInputCursorOnBottomLine())
		}
	}

	// Middle display line: top=false, bottom=false.
	for _, off := range []int{lineStarts[1], (lineStarts[1] + lineStarts[2] - 1) / 2} {
		inputBuf.PlaceCursor(inputBuf.IterAtOffset(off))
		if w.isInputCursorOnTopLine() || w.isInputCursorOnBottomLine() {
			t.Fatalf("middle line offset %d: got top=%v bottom=%v, want top=false bottom=false",
				off, w.isInputCursorOnTopLine(), w.isInputCursorOnBottomLine())
		}
	}

	// Last display line: top=false, bottom=true.
	lastStart := lineStarts[len(lineStarts)-1]
	for _, off := range []int{lastStart, inputBuf.CharCount()} {
		inputBuf.PlaceCursor(inputBuf.IterAtOffset(off))
		if w.isInputCursorOnTopLine() || !w.isInputCursorOnBottomLine() {
			t.Fatalf("last line offset %d: got top=%v bottom=%v, want top=false bottom=true",
				off, w.isInputCursorOnTopLine(), w.isInputCursorOnBottomLine())
		}
	}

	// Pressing Up while cursor is on wrapped bottom line must NOT override the prompt.
	if w.handleInputHistoryKey(gdk.KEY_Up, 0) {
		t.Fatal("expected Up on wrapped bottom line to move cursor up instead of overriding prompt")
	}
	start, end := inputBuf.Bounds()
	if got := inputBuf.Text(start, end, false); got != draft {
		t.Fatalf("expected draft %q to remain intact, got %q", draft, got)
	}

	// Move cursor to display line 0 and press Up: recalls wrapped history entry.
	inputBuf.PlaceCursor(inputBuf.IterAtOffset(3))
	if !w.handleInputHistoryKey(gdk.KEY_Up, 0) {
		t.Fatal("expected Up on display line 0 to recall history")
	}
	start, end = inputBuf.Bounds()
	wantRecalled := w.inputHistory[1]
	if got := inputBuf.Text(start, end, false); got != wantRecalled {
		t.Fatalf("expected recalled history %q, got %q", wantRecalled, got)
	}

	// Immediately pressing Up again (cursor at end of recalled wrapped entry) must NOT jump again.
	if w.handleInputHistoryKey(gdk.KEY_Up, 0) {
		t.Fatal("expected Up at end of wrapped recalled history entry to move cursor up, not jump history")
	}
	start, end = inputBuf.Bounds()
	if got := inputBuf.Text(start, end, false); got != wantRecalled {
		t.Fatalf("expected recalled history %q to remain intact, got %q", wantRecalled, got)
	}

	// Move cursor to display line 0 of recalled entry: Down must NOT jump history.
	inputBuf.PlaceCursor(inputBuf.IterAtOffset(2))
	if w.handleInputHistoryKey(gdk.KEY_Down, 0) {
		t.Fatal("expected Down on display line 0 of wrapped entry to move cursor down, not jump history")
	}

	// Move cursor to end of recalled entry: Down restores live draft.
	_, end = inputBuf.Bounds()
	inputBuf.PlaceCursor(end)
	if !w.handleInputHistoryKey(gdk.KEY_Down, 0) {
		t.Fatal("expected Down on bottom display line to restore draft")
	}
	start, end = inputBuf.Bounds()
	if got := inputBuf.Text(start, end, false); got != draft {
		t.Fatalf("expected restored draft %q, got %q", draft, got)
	}

	// Explicit newline edge cases: leading and trailing empty lines.
	inputBuf.SetText("\nhello")
	inputBuf.PlaceCursor(inputBuf.IterAtOffset(0))
	if !w.isInputCursorOnTopLine() || w.isInputCursorOnBottomLine() {
		t.Fatalf("leading newline offset 0: got top=%v bottom=%v, want top=true bottom=false",
			w.isInputCursorOnTopLine(), w.isInputCursorOnBottomLine())
	}
	inputBuf.PlaceCursor(inputBuf.IterAtOffset(1))
	if w.isInputCursorOnTopLine() || !w.isInputCursorOnBottomLine() {
		t.Fatalf("leading newline offset 1: got top=%v bottom=%v, want top=false bottom=true",
			w.isInputCursorOnTopLine(), w.isInputCursorOnBottomLine())
	}

	inputBuf.SetText("hello\n")
	inputBuf.PlaceCursor(inputBuf.IterAtOffset(5))
	if !w.isInputCursorOnTopLine() || w.isInputCursorOnBottomLine() {
		t.Fatalf("trailing newline offset 5: got top=%v bottom=%v, want top=true bottom=false",
			w.isInputCursorOnTopLine(), w.isInputCursorOnBottomLine())
	}
	inputBuf.PlaceCursor(inputBuf.IterAtOffset(6))
	if w.isInputCursorOnTopLine() || !w.isInputCursorOnBottomLine() {
		t.Fatalf("trailing newline offset 6: got top=%v bottom=%v, want top=false bottom=true",
			w.isInputCursorOnTopLine(), w.isInputCursorOnBottomLine())
	}
}

func TestStopButtonVisibilityWithRunningSubagents(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}

	builder := gtk.NewBuilderFromString(windowUI)
	stopBtn := builder.GetObject("stopBtn").Cast().(*gtk.Button)
	stopConfirmBtn := builder.GetObject("stopConfirmBtn").Cast().(*gtk.Button)

	vs := adw.NewViewStack()
	win := &Window{
		ctx:            t.Context(),
		viewStack:      vs,
		tabs:           make(map[string]*sessionTab),
		childToRoot:    make(map[string]string),
		stopBtn:        stopBtn,
		stopConfirmBtn: stopConfirmBtn,
	}

	tab := win.addSessionTab(opencode.Session{ID: "ses_root"})
	vs.SetVisibleChildName("ses_root")

	// Initially idle: stopBtn hidden.
	win.refreshHeaderAndBottomBar(tab)
	if stopBtn.Visible() {
		t.Fatal("expected stopBtn hidden while main and subagents are idle")
	}

	// Main idle + running subagent -> stopBtn visible and Esc stop arming works.
	win.childToRoot["ses_sub1"] = "ses_root"
	win.upsertSubagent(tab, opencode.Session{
		ID:       "ses_sub1",
		ParentID: "ses_root",
		Time:     opencode.SessionTime{Busy: 1000},
	})
	if !stopBtn.Visible() {
		t.Fatal("expected stopBtn visible when a subagent is running even if main is idle")
	}
	if !win.handleEscapeStop(0) || win.escStopArmedSess != "ses_root" || !stopConfirmBtn.Visible() {
		t.Fatalf("expected Esc to arm stop on root tab while subagent is running, armed=%q confirmVisible=%v",
			win.escStopArmedSess, stopConfirmBtn.Visible())
	}

	// Main becomes active then goes idle while subagent is still running -> stopBtn stays visible and Esc stays armed.
	win.setConvActive("ses_root", true)
	win.setConvActive("ses_root", false)
	if !stopBtn.Visible() {
		t.Fatal("expected stopBtn to stay visible when main goes idle while subagent is still running")
	}
	if win.escStopArmedSess != "ses_root" {
		t.Fatalf("expected escStopArmedSess to remain armed while subagent is running, got %q", win.escStopArmedSess)
	}

	// Subagent finishes -> stopBtn hides and Esc stop disarms.
	win.setSubagentActive("ses_sub1", false, "succeeded")
	if stopBtn.Visible() {
		t.Fatal("expected stopBtn hidden after last running subagent finishes")
	}
	if win.escStopArmedSess != "" || stopConfirmBtn.Visible() {
		t.Fatalf("expected Esc stop to disarm after last subagent finishes, armed=%q confirmVisible=%v",
			win.escStopArmedSess, stopConfirmBtn.Visible())
	}

	// Viewing an idle subagent while main is active hides stopBtn; viewing an active subagent shows it.
	win.setConvActive("ses_root", true)
	tab.subTimelines["ses_sub1"] = &convTimeline{session: opencode.Session{ID: "ses_sub1"}, active: false}
	tab.activeSubID = "ses_sub1"
	win.refreshHeaderAndBottomBar(tab)
	if stopBtn.Visible() {
		t.Fatal("expected stopBtn hidden when viewing an idle subagent")
	}
	win.setConvActive("ses_sub1", true)
	if !stopBtn.Visible() {
		t.Fatal("expected stopBtn visible when viewing an active subagent")
	}
	win.setConvActive("ses_sub1", false)
	if stopBtn.Visible() {
		t.Fatal("expected stopBtn hidden once active subagent finishes")
	}
}
