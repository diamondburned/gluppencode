package window

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
	"unsafe"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4-sourceview/pkg/gtksource/v5"
	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"libdb.so/gluppencode/internal/opencode"
)

func sliceUTF16(s string, start, end int) string {
	u := utf16.Encode([]rune(s))
	if start < 0 || start > len(u) || end < start || end > len(u) {
		return ""
	}
	return string(utf16.Decode(u[start:end]))
}

func TestShortDir(t *testing.T) {
	fakeHome := "/home/diamond"
	t.Setenv("HOME", fakeHome)

	tests := []struct {
		name string
		dir  string
		want string
	}{
		{
			name: "empty workspace",
			dir:  "",
			want: "workspace",
		},
		{
			name: "exact home directory",
			dir:  fakeHome,
			want: "~",
		},
		{
			name: "child directory in home",
			dir:  filepath.Join(fakeHome, "projects", "gluppencode"),
			want: "~/projects/gluppencode",
		},
		{
			name: "prefix match without separator",
			dir:  fakeHome + "foo",
			want: fakeHome + "foo",
		},
		{
			name: "outside home directory",
			dir:  "/var/log",
			want: "/var/log",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shortDir(tt.dir)
			if got != tt.want {
				t.Errorf("shortDir(%q) = %q, want %q", tt.dir, got, tt.want)
			}
		})
	}
}

func TestFindUndoTargetMessage(t *testing.T) {
	messages := []opencode.Message{
		{ID: "msg-1", Type: "user", Text: "first prompt"},
		{ID: "msg-2", Type: "assistant", Text: "first response"},
		{ID: "msg-3", Type: "user", Text: "second prompt"},
		{ID: "msg-4", Type: "assistant", Text: "second response"},
	}

	t.Run("latest user message when no revert", func(t *testing.T) {
		target := findUndoTargetMessage(messages, "")
		if target == nil || target.ID != "msg-3" {
			t.Fatalf("expected msg-3, got %+v", target)
		}
	})

	t.Run("walk backward past revert target", func(t *testing.T) {
		target := findUndoTargetMessage(messages, "msg-3")
		if target == nil || target.ID != "msg-1" {
			t.Fatalf("expected msg-1, got %+v", target)
		}
	})

	t.Run("revert at earliest message returns nil", func(t *testing.T) {
		target := findUndoTargetMessage(messages, "msg-1")
		if target != nil {
			t.Fatalf("expected nil when reverting past earliest user message, got %+v", target)
		}
	})

	t.Run("unknown revert ID falls back to latest user message", func(t *testing.T) {
		target := findUndoTargetMessage(messages, "non-existent")
		if target == nil || target.ID != "msg-3" {
			t.Fatalf("expected msg-3, got %+v", target)
		}
	})

	t.Run("empty messages list returns nil", func(t *testing.T) {
		target := findUndoTargetMessage(nil, "")
		if target != nil {
			t.Fatalf("expected nil for empty messages, got %+v", target)
		}
	})

	t.Run("messages without user type returns nil", func(t *testing.T) {
		assistantOnly := []opencode.Message{
			{ID: "msg-a", Type: "assistant", Text: "hello"},
		}
		target := findUndoTargetMessage(assistantOnly, "")
		if target != nil {
			t.Fatalf("expected nil for assistant-only messages, got %+v", target)
		}
	})
}

func TestParseMentionsAdvanced(t *testing.T) {
	tempDir := t.TempDir()

	// Create a file with a # in the name
	hashFile := filepath.Join(tempDir, "foo#bar.go")
	if err := os.WriteFile(hashFile, []byte("package main"), 0644); err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}

	skills := []opencode.SkillInfo{
		{ID: "review", Name: "code-review", Description: "Reviews code"},
	}
	agents := []opencode.Agent{
		{Name: "orchestrator", Description: "Coordinates tasks"},
	}

	t.Run("UTF-8 runes and emojis preceding mentions", func(t *testing.T) {
		text := "Hello 🌍 @orchestrator こんにちは @review 🎉"
		opts := parseMentions(text, skills, agents, tempDir)

		if len(opts.Agents) != 1 || opts.Agents[0].Name != "orchestrator" {
			t.Fatalf("expected agent orchestrator, got %+v", opts.Agents)
		}
		if sliceUTF16(text, opts.Agents[0].Mention.Start, opts.Agents[0].Mention.End) != "@orchestrator" {
			t.Errorf("agent slice mismatch: got %q", sliceUTF16(text, opts.Agents[0].Mention.Start, opts.Agents[0].Mention.End))
		}
		if opts.Agents[0].Mention.Start != 9 || opts.Agents[0].Mention.End != 22 {
			t.Errorf("expected agent offsets [9, 22], got [%d, %d]", opts.Agents[0].Mention.Start, opts.Agents[0].Mention.End)
		}

		if len(opts.Skills) != 1 || opts.Skills[0].ID != "review" {
			t.Fatalf("expected skill review, got %+v", opts.Skills)
		}
		if sliceUTF16(text, opts.Skills[0].Mention.Start, opts.Skills[0].Mention.End) != "@review" {
			t.Errorf("skill slice mismatch: got %q", sliceUTF16(text, opts.Skills[0].Mention.Start, opts.Skills[0].Mention.End))
		}
		if opts.Skills[0].Mention.Start != 29 || opts.Skills[0].Mention.End != 36 {
			t.Errorf("expected skill offsets [29, 36], got [%d, %d]", opts.Skills[0].Mention.Start, opts.Skills[0].Mention.End)
		}
	})

	t.Run("BMP non-ASCII runes matching JavaScript UTF-16 slice", func(t *testing.T) {
		text := "日本語 @orchestrator go"
		opts := parseMentions(text, skills, agents, tempDir)
		if len(opts.Agents) != 1 || opts.Agents[0].Name != "orchestrator" {
			t.Fatalf("expected agent orchestrator, got %+v", opts.Agents)
		}
		if opts.Agents[0].Mention.Start != 4 || opts.Agents[0].Mention.End != 17 {
			t.Errorf("expected agent offsets [4, 17], got [%d, %d]", opts.Agents[0].Mention.Start, opts.Agents[0].Mention.End)
		}
		if sliceUTF16(text, opts.Agents[0].Mention.Start, opts.Agents[0].Mention.End) != "@orchestrator" {
			t.Errorf("agent slice mismatch: got %q", sliceUTF16(text, opts.Agents[0].Mention.Start, opts.Agents[0].Mention.End))
		}
	})

	t.Run("at symbol not preceded by space is ignored", func(t *testing.T) {
		text := "email user@orchestrator or 日本語@review"
		opts := parseMentions(text, skills, agents, tempDir)
		if len(opts.Agents) != 0 {
			t.Errorf("expected 0 agents, got %d", len(opts.Agents))
		}
		if len(opts.Skills) != 0 {
			t.Errorf("expected 0 skills, got %d", len(opts.Skills))
		}
	})

	t.Run("file path with hash encodes properly", func(t *testing.T) {
		text := "Check @foo#bar.go please"
		opts := parseMentions(text, skills, agents, tempDir)

		if len(opts.Paths) != 1 {
			t.Fatalf("expected 1 path, got %d", len(opts.Paths))
		}
		pathInput := opts.Paths[0]
		if pathInput.Name != "foo#bar.go" {
			t.Errorf("expected name foo#bar.go, got %q", pathInput.Name)
		}
		if !strings.Contains(pathInput.URI, "foo%23bar.go") {
			t.Errorf("expected URI to URL-escape # to %%23, got %q", pathInput.URI)
		}
		if sliceUTF16(text, pathInput.Mention.Start, pathInput.Mention.End) != "@foo#bar.go" {
			t.Errorf("path slice mismatch: got %q", sliceUTF16(text, pathInput.Mention.Start, pathInput.Mention.End))
		}
	})

	t.Run("strict fallback does not match non-existent file", func(t *testing.T) {
		text := "Inspect @nonexistent_file.go please"
		opts := parseMentions(text, skills, agents, tempDir)
		if len(opts.Paths) != 0 {
			t.Errorf("expected 0 paths for non-existent file in workspace, got %d", len(opts.Paths))
		}
	})
}

func TestActiveTriggerWord(t *testing.T) {
	tests := []struct {
		input       string
		wantWord    string
		wantAtStart bool
		shouldPopup bool
	}{
		{"", "", false, false},
		{"/", "/", true, true},
		{"/undo", "/undo", true, true},
		{"   /undo", "/undo", true, true},
		{"/undo ", "", false, false},
		{"look in /tmp/x", "/tmp/x", false, false},
		{"look in /tmp/x for logs", "logs", false, false},
		{"@file", "@file", true, true},
		{"look in @file", "@file", false, true},
		{"look in @file.go", "@file.go", false, true},
		{"user@example.com", "user@example.com", true, false},
		{"look in @file.go ", "", false, false},
	}
	for _, tt := range tests {
		word, atStart := activeTriggerWordFromText(tt.input)
		if word != tt.wantWord || atStart != tt.wantAtStart {
			t.Errorf("activeTriggerWordFromText(%q) = (%q, %v), want (%q, %v)", tt.input, word, atStart, tt.wantWord, tt.wantAtStart)
		}
		gotPopup := (strings.HasPrefix(word, "/") && atStart) || strings.HasPrefix(word, "@")
		if gotPopup != tt.shouldPopup {
			t.Errorf("popup condition for %q: got %v, want %v", tt.input, gotPopup, tt.shouldPopup)
		}
	}
}

func TestReconcileTailOrderingAndUndo(t *testing.T) {
	t.Run("normal second turn: [U1, A1] + live A2 with server page [U1, A1, U2]", func(t *testing.T) {
		// conv.messages starts with U1, A1.
		// User submits U2; step.started creates A2 before reconcileTail receives page.
		currentMessages := []opencode.Message{
			{ID: "u1", Type: "user", Text: "first prompt"},
			{ID: "a1", Type: "assistant", Text: "first response"},
			{ID: "a2", Type: "assistant", Text: "in-flight response"},
		}
		inFlight := map[string]bool{"a2": true}
		seenIDs := map[string]bool{"u1": true, "a1": true, "a2": true}

		// Server page has [U1, A1, U2] (authoritative chronological order).
		page := []opencode.Message{
			{ID: "u1", Type: "user", Text: "first prompt"},
			{ID: "a1", Type: "assistant", Text: "first response"},
			{ID: "u2", Type: "user", Text: "second prompt"},
		}

		currentMessages = reconcileConvSlice(currentMessages, seenIDs, page, func(id string) bool {
			return inFlight[id]
		})

		// Must result in [u1, a1, u2, a2] - NOT [u1, u2, a1, a2]!
		wantIDs := []string{"u1", "a1", "u2", "a2"}
		var gotIDs []string
		for _, m := range currentMessages {
			gotIDs = append(gotIDs, m.ID)
		}
		if !slices.Equal(gotIDs, wantIDs) {
			t.Fatalf("expected message order %v, got %v", wantIDs, gotIDs)
		}

		// When undoing U2: U1 and A1 must remain visible, while U2 and A2 are hidden!
		visible, hidden := computeRevertVisibility(currentMessages, "u2")
		wantVisible := []string{"u1", "a1"}
		wantHidden := []string{"u2", "a2"}
		if !slices.Equal(visible, wantVisible) {
			t.Errorf("visible messages = %v, want %v", visible, wantVisible)
		}
		if !slices.Equal(hidden, wantHidden) {
			t.Errorf("hidden messages = %v, want %v", hidden, wantHidden)
		}
	})

	t.Run("pending prompt match and onInsert ordering with in-flight assistant", func(t *testing.T) {
		currentMessages := []opencode.Message{
			{ID: "u1", Type: "user", Text: "first prompt"},
			{ID: "a1", Type: "assistant", Text: "first response"},
			{ID: "a2", Type: "assistant", Text: "in-flight response"},
		}
		inFlight := map[string]bool{"a2": true}
		seenIDs := map[string]bool{"u1": true, "a1": true, "a2": true}
		pendingPrompts := []*pendingPrompt{
			{text: "second prompt"},
		}
		page := []opencode.Message{
			{ID: "u1", Type: "user", Text: "first prompt"},
			{ID: "a1", Type: "assistant", Text: "first response"},
			{ID: "u2", Type: "user", Text: "second prompt"},
		}

		var calledInsert bool
		var insertedMsg opencode.Message
		var gotInsertIdx int
		var gotMatched *pendingPrompt
		var lenCurrentOnInsert int

		reconcileConvMessages(
			&currentMessages,
			seenIDs,
			&pendingPrompts,
			page,
			func(id string) bool { return inFlight[id] },
			func(msg opencode.Message, insertIdx int, matched *pendingPrompt) {
				calledInsert = true
				insertedMsg = msg
				gotInsertIdx = insertIdx
				gotMatched = matched
				lenCurrentOnInsert = len(currentMessages)
			},
		)

		if !calledInsert {
			t.Fatal("onInsert callback was not called")
		}
		if len(pendingPrompts) != 0 {
			t.Errorf("expected pendingPrompts to be empty after match, got %d", len(pendingPrompts))
		}
		if gotMatched == nil || gotMatched.text != "second prompt" {
			t.Fatalf("expected matched pending prompt 'second prompt', got %+v", gotMatched)
		}
		if gotInsertIdx != 2 {
			t.Errorf("expected insertIdx = 2, got %d", gotInsertIdx)
		}
		if insertedMsg.ID != "u2" {
			t.Errorf("expected inserted message ID 'u2', got %q", insertedMsg.ID)
		}
		if lenCurrentOnInsert != 4 {
			t.Errorf("expected len(currentMessages) == 4 during onInsert, got %d", lenCurrentOnInsert)
		}
		if !(gotInsertIdx < lenCurrentOnInsert-1) {
			t.Errorf("expected insertIdx < len(currentMessages)-1 to be true (%d < %d)", gotInsertIdx, lenCurrentOnInsert-1)
		}

		wantIDs := []string{"u1", "a1", "u2", "a2"}
		var gotIDs []string
		for _, m := range currentMessages {
			gotIDs = append(gotIDs, m.ID)
		}
		if !slices.Equal(gotIDs, wantIDs) {
			t.Fatalf("expected message order %v, got %v", wantIDs, gotIDs)
		}
	})

	t.Run("pending prompt matches by inboxID first", func(t *testing.T) {
		currentMessages := []opencode.Message{
			{ID: "u1", Type: "user", Text: "first prompt"},
		}
		seenIDs := map[string]bool{"u1": true}
		p1 := &pendingPrompt{inboxID: "inb_1", text: "same text"}
		p2 := &pendingPrompt{inboxID: "inb_2", text: "same text"}
		pendingPrompts := []*pendingPrompt{p1, p2}

		page := []opencode.Message{
			{ID: "u1", Type: "user", Text: "first prompt"},
			{ID: "inb_2", Type: "user", Text: "same text"},
		}

		var gotMatched *pendingPrompt
		reconcileConvMessages(
			&currentMessages,
			seenIDs,
			&pendingPrompts,
			page,
			nil,
			func(_ opencode.Message, _ int, matched *pendingPrompt) {
				gotMatched = matched
			},
		)

		if gotMatched != p2 {
			t.Fatalf("expected matched pending prompt p2 (inb_2), got %+v", gotMatched)
		}
		if len(pendingPrompts) != 1 || pendingPrompts[0] != p1 {
			t.Fatalf("expected p1 to remain in pendingPrompts, got %+v", pendingPrompts)
		}
	})

	t.Run("missed turn resync: conv has [U1], server page has [U1, A1, U2, A2]", func(t *testing.T) {
		currentMessages := []opencode.Message{
			{ID: "u1", Type: "user", Text: "first prompt"},
		}
		seenIDs := map[string]bool{"u1": true}

		page := []opencode.Message{
			{ID: "u1", Type: "user", Text: "first prompt"},
			{ID: "a1", Type: "assistant", Text: "first response"},
			{ID: "u2", Type: "user", Text: "second prompt"},
			{ID: "a2", Type: "assistant", Text: "second response"},
		}

		currentMessages = reconcileConvSlice(currentMessages, seenIDs, page, nil)

		wantIDs := []string{"u1", "a1", "u2", "a2"}
		var gotIDs []string
		for _, m := range currentMessages {
			gotIDs = append(gotIDs, m.ID)
		}
		if !slices.Equal(gotIDs, wantIDs) {
			t.Fatalf("expected message order %v, got %v", wantIDs, gotIDs)
		}
	})
}

type rawCompletionCell struct {
	gtk.Widget
}

func wrapGTKSourceCompletionCell(obj *coreglib.Object) *gtksource.CompletionCell {
	if obj == nil {
		return nil
	}
	raw := &rawCompletionCell{
		Widget: gtk.Widget{
			InitiallyUnowned: coreglib.InitiallyUnowned{
				Object: obj,
			},
			Object: obj,
			Accessible: gtk.Accessible{
				Object: obj,
			},
			Buildable: gtk.Buildable{
				Object: obj,
			},
			ConstraintTarget: gtk.ConstraintTarget{
				Object: obj,
			},
		},
	}
	return (*gtksource.CompletionCell)(unsafe.Pointer(raw))
}

func TestCompletionProviderTriggerDisplayAndActivate(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("GTK cannot be initialized")
	}

	buf := gtksource.NewBuffer(nil)
	view := gtksource.NewViewWithBuffer(buf)

	win := &Window{
		inputBuffer: buf,
		inputView:   view,
		commands: []opencode.CommandInfo{
			{Name: "/undo", Description: "Undo last message & file changes"},
		},
		skills: []opencode.SkillInfo{
			{ID: "explore", Name: "explore", Description: "Explores codebase"},
			{ID: "claude-coworker", Name: "claude-coworker", Description: "Spawn Claude Code coworker"},
		},
		agents: []opencode.Agent{
			{Name: "explore", Description: "Explores codebase"},
		},
	}

	rawCP := completionProviderType.New()
	rawCP.window = win
	gtksourceCP := wrapGTKSourceCompletionProvider(&rawCP.Object)

	comp := view.Completion()
	if comp != nil {
		comp.SetObjectProperty("select-on-show", true)
		comp.AddProvider(gtksourceCP)
	}

	makeCell := func(col string) *gtksource.CompletionCell {
		b := gtk.NewBuilderFromString(fmt.Sprintf(`<interface><object class="GtkSourceCompletionCell" id="c"><property name="column">%s</property></object></interface>`, col))
		return wrapGTKSourceCompletionCell(b.GetObject("c"))
	}

	t.Run("slash command /und", func(t *testing.T) {
		buf.SetText("/und")
		_, end := buf.Bounds()
		buf.PlaceCursor(end)

		trig, query, startIter := rawCP.extractTriggerAndQuery(nil)
		if trig != '/' {
			t.Fatalf("expected trigger '/', got %q", trig)
		}
		if query != "und" {
			t.Fatalf("expected query 'und', got %q", query)
		}
		if startIter == nil {
			t.Fatal("expected non-nil startIter")
		}

		store, err := rawCP.populate(nil)
		if err != nil {
			t.Fatalf("populate failed: %v", err)
		}
		if store == nil || store.NItems() == 0 {
			t.Fatal("expected proposals in store, got none")
		}

		item := store.Item(0)
		proposaller := wrapGTKSourceCompletionProposal(item)
		if proposaller == nil {
			t.Fatal("expected item to wrap as CompletionProposaller")
		}

		prop := proposalFromProposaller(proposaller)
		if prop == nil {
			t.Fatal("proposalFromProposaller returned nil for store item")
		}
		if prop.TriggerText != "/undo" {
			t.Fatalf("expected proposal /undo, got %q", prop.TriggerText)
		}

		// Verify display populates typed-text and icon cells
		cellText := makeCell("typed-text")
		rawCP.display(nil, proposaller, cellText)

		cellIcon := makeCell("icon")
		rawCP.display(nil, proposaller, cellIcon)

		// Verify activate replaces /und with /undo
		rawCP.activate(nil, proposaller)
		bStart, bEnd := buf.Bounds()
		if got := buf.Text(bStart, bEnd, false); got != "/undo " {
			t.Fatalf("expected buffer text '/undo ', got %q", got)
		}
	})

	t.Run("slash command /file", func(t *testing.T) {
		buf.SetText("/file")
		_, end := buf.Bounds()
		buf.PlaceCursor(end)

		trig, query, startIter := rawCP.extractTriggerAndQuery(nil)
		if trig != '/' {
			t.Fatalf("expected trigger '/', got %q", trig)
		}
		if query != "file" {
			t.Fatalf("expected query 'file', got %q", query)
		}
		if startIter == nil {
			t.Fatal("expected non-nil startIter")
		}

		store, err := rawCP.populate(nil)
		if err != nil {
			t.Fatalf("populate failed: %v", err)
		}
		if store != nil && store.NItems() != 0 {
			t.Fatalf("expected 0 proposals for /file, got %d", store.NItems())
		}
	})

	t.Run("mention see @exp", func(t *testing.T) {
		buf.SetText("see @exp")
		_, end := buf.Bounds()
		buf.PlaceCursor(end)

		trig, query, startIter := rawCP.extractTriggerAndQuery(nil)
		if trig != '@' {
			t.Fatalf("expected trigger '@', got %q", trig)
		}
		if query != "exp" {
			t.Fatalf("expected query 'exp', got %q", query)
		}
		if startIter == nil {
			t.Fatal("expected non-nil startIter")
		}

		store, err := rawCP.populate(nil)
		if err != nil {
			t.Fatalf("populate failed: %v", err)
		}
		if store == nil || store.NItems() == 0 {
			t.Fatal("expected proposals in store, got none")
		}

		var exploreProp gtksource.CompletionProposaller
		for i := uint(0); i < store.NItems(); i++ {
			p := wrapGTKSourceCompletionProposal(store.Item(i))
			if p != nil {
				cp := proposalFromProposaller(p)
				if cp != nil && cp.TriggerText == "@explore" {
					exploreProp = p
					break
				}
			}
		}
		if exploreProp == nil {
			t.Fatal("could not find @explore proposal in store")
		}

		cellText := makeCell("typed-text")
		rawCP.display(nil, exploreProp, cellText)

		rawCP.activate(nil, exploreProp)
		bStart, bEnd := buf.Bounds()
		if got := buf.Text(bStart, bEnd, false); got != "see @explore " {
			t.Fatalf("expected buffer text 'see @explore ', got %q", got)
		}
	})

	t.Run("mention non-ASCII 日本語 @exp", func(t *testing.T) {
		buf.SetText("日本語 @exp")
		_, end := buf.Bounds()
		buf.PlaceCursor(end)

		trig, query, startIter := rawCP.extractTriggerAndQuery(nil)
		if trig != '@' {
			t.Fatalf("expected trigger '@', got %q", trig)
		}
		if query != "exp" {
			t.Fatalf("expected query 'exp', got %q", query)
		}
		if startIter == nil {
			t.Fatal("expected non-nil startIter")
		}

		store, err := rawCP.populate(nil)
		if err != nil {
			t.Fatalf("populate failed: %v", err)
		}
		if store == nil || store.NItems() == 0 {
			t.Fatal("expected proposals in store, got none")
		}

		var exploreProp gtksource.CompletionProposaller
		for i := uint(0); i < store.NItems(); i++ {
			p := wrapGTKSourceCompletionProposal(store.Item(i))
			if p != nil {
				cp := proposalFromProposaller(p)
				if cp != nil && cp.TriggerText == "@explore" {
					exploreProp = p
					break
				}
			}
		}
		if exploreProp == nil {
			t.Fatal("could not find @explore proposal in store")
		}

		rawCP.activate(nil, exploreProp)
		bStart, bEnd := buf.Bounds()
		if got := buf.Text(bStart, bEnd, false); got != "日本語 @explore " {
			t.Fatalf("expected buffer text '日本語 @explore ', got %q", got)
		}
	})

	t.Run("hyphenated mention bug: @claude-co replaces full @claude-co token", func(t *testing.T) {
		buf.SetText("bug: @claude-co")
		_, end := buf.Bounds()
		buf.PlaceCursor(end)

		if !rawCP.isTrigger(end, uint32('-')) {
			t.Fatal("expected isTrigger to return true for '-' inside @claude-co")
		}

		trig, query, startIter := rawCP.extractTriggerAndQuery(nil)
		if trig != '@' {
			t.Fatalf("expected trigger '@', got %q", trig)
		}
		if query != "claude-co" {
			t.Fatalf("expected query 'claude-co', got %q", query)
		}
		if startIter == nil {
			t.Fatal("expected non-nil startIter")
		}

		store, err := rawCP.populate(nil)
		if err != nil {
			t.Fatalf("populate failed: %v", err)
		}
		if store == nil || store.NItems() == 0 {
			t.Fatal("expected proposals in store for @claude-co, got none")
		}

		var coworkerProp gtksource.CompletionProposaller
		for i := uint(0); i < store.NItems(); i++ {
			p := wrapGTKSourceCompletionProposal(store.Item(i))
			if p != nil {
				cp := proposalFromProposaller(p)
				if cp != nil && cp.TriggerText == "@claude-coworker" {
					coworkerProp = p
					break
				}
			}
		}
		if coworkerProp == nil {
			t.Fatal("could not find @claude-coworker proposal in store")
		}

		rawCP.activate(nil, coworkerProp)
		bStart, bEnd := buf.Bounds()
		if got := buf.Text(bStart, bEnd, false); got != "bug: @claude-coworker " {
			t.Fatalf("expected buffer text 'bug: @claude-coworker ', got %q", got)
		}
	})
}

func TestPendingPromptSteeredInFlight(t *testing.T) {
	tests := []struct {
		name   string
		prompt pendingPrompt
		server opencode.InboxDelivery
		want   bool
	}{
		{
			name:   "steer clicked before the inbox ID arrived",
			prompt: pendingPrompt{delivery: opencode.DeliverySteer},
			server: opencode.DeliveryQueue,
			want:   true,
		},
		{
			name:   "sent with Ctrl+Enter",
			prompt: pendingPrompt{delivery: opencode.DeliverySteer},
			server: opencode.DeliverySteer,
		},
		{
			name:   "still queued",
			prompt: pendingPrompt{delivery: opencode.DeliveryQueue},
			server: opencode.DeliveryQueue,
		},
		{
			name:   "ID already bound, steer already sent",
			prompt: pendingPrompt{inboxID: "msg_1", delivery: opencode.DeliverySteer},
			server: opencode.DeliveryQueue,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.prompt.steeredInFlight(tt.server); got != tt.want {
				t.Errorf("steeredInFlight(%q) = %v, want %v", tt.server, got, tt.want)
			}
		})
	}
}

func TestPersistedStateAndProjectOrdering(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "opencode", "gluppencode.json")

	dirA := filepath.Join(t.TempDir(), "proj-alpha")
	dirB := filepath.Join(t.TempDir(), "proj-beta")
	dirC := filepath.Join(t.TempDir(), "proj-gamma")

	w := &Window{
		statePath:         stateFile,
		archivedSessions:  map[string]bool{"ses_old": true, "ses_false": false},
		collapsedProjects: map[string]bool{normalizeProjectDir(dirB): true},
		projectOrder:      []string{normalizeProjectDir(dirA), normalizeProjectDir(dirB)},
	}
	w.saveState()

	w2 := &Window{statePath: stateFile}
	w2.loadState()

	if !w2.archivedSessions["ses_old"] || w2.archivedSessions["ses_false"] {
		t.Fatalf("unexpected archivedSessions after reload: %+v", w2.archivedSessions)
	}
	if !w2.collapsedProjects[normalizeProjectDir(dirB)] {
		t.Fatalf("expected dirB to remain collapsed after reload: %+v", w2.collapsedProjects)
	}
	wantOrder := []string{normalizeProjectDir(dirA), normalizeProjectDir(dirB)}
	if !slices.Equal(w2.projectOrder, wantOrder) {
		t.Fatalf("projectOrder = %v, want %v", w2.projectOrder, wantOrder)
	}

	// Discovering an existing project should not change order; discovering a new one appends it.
	if w2.ensureProjectOrder(dirB) {
		t.Fatalf("ensureProjectOrder(dirB) unexpectedly reported change")
	}
	if !w2.ensureProjectOrder(dirC) {
		t.Fatalf("ensureProjectOrder(dirC) expected change")
	}
	wantOrder = append(wantOrder, normalizeProjectDir(dirC))
	if !slices.Equal(w2.projectOrder, wantOrder) {
		t.Fatalf("projectOrder after ensure = %v, want %v", w2.projectOrder, wantOrder)
	}

	if w2.compareProjectOrder(dirA, dirC) >= 0 {
		t.Errorf("expected dirA < dirC in projectOrder")
	}
	if w2.compareProjectOrder(dirC, dirB) <= 0 {
		t.Errorf("expected dirC > dirB in projectOrder")
	}

	// Drag-and-drop: move dirC to dirA's position (top)
	moved, ok := moveProjectSlice(w2.projectOrder, dirC, dirA)
	if !ok {
		t.Fatalf("moveProjectSlice(dirC -> dirA) returned false")
	}
	wantMoved := []string{normalizeProjectDir(dirC), normalizeProjectDir(dirA), normalizeProjectDir(dirB)}
	if !slices.Equal(moved, wantMoved) {
		t.Fatalf("moved projectOrder = %v, want %v", moved, wantMoved)
	}
}

func TestFormatIntWithCommas(t *testing.T) {
	tests := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{452, "452"},
		{2191, "2,191"},
		{124314, "124,314"},
		{200000, "200,000"},
		{1000000, "1,000,000"},
	}
	for _, tt := range tests {
		if got := formatIntWithCommas(tt.in); got != tt.want {
			t.Errorf("formatIntWithCommas(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIsBlockingBackgroundableTool(t *testing.T) {
	tests := []struct {
		name   string
		tool   string
		status string
		input  any
		want   bool
	}{
		{
			name:   "foreground running shell",
			tool:   "shell",
			status: "running",
			input:  map[string]any{"command": "sleep 10"},
			want:   true,
		},
		{
			name:   "foreground running subagent",
			tool:   "subagent",
			status: "running",
			input:  map[string]any{"agent": "explore", "prompt": "check files"},
			want:   true,
		},
		{
			name:   "backgrounded shell is not blocking",
			tool:   "shell",
			status: "running",
			input:  map[string]any{"command": "sleep 10", "background": true},
			want:   false,
		},
		{
			name:   "completed shell is not blocking",
			tool:   "shell",
			status: "completed",
			input:  map[string]any{"command": "sleep 10"},
			want:   false,
		},
		{
			name:   "streaming input before call is not blocking",
			tool:   "shell",
			status: "streaming",
			input:  nil,
			want:   false,
		},
		{
			name:   "non-backgroundable tool is not blocking",
			tool:   "read",
			status: "running",
			input:  map[string]any{"path": "/tmp/a"},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBlockingBackgroundableTool(tt.tool, tt.status, tt.input); got != tt.want {
				t.Errorf("isBlockingBackgroundableTool(%q, %q, %v) = %v, want %v", tt.tool, tt.status, tt.input, got, tt.want)
			}
		})
	}
}

func TestBlockingSessionID(t *testing.T) {
	mainConv := &convTimeline{
		session:       opencode.Session{ID: "ses_main"},
		active:        true,
		blockingTools: map[string]bool{"tool_1": true},
	}
	subConv := &convTimeline{
		session:       opencode.Session{ID: "ses_sub"},
		active:        true,
		blockingTools: map[string]bool{},
	}
	tab := &sessionTab{
		main:         mainConv,
		subTimelines: map[string]*convTimeline{"ses_sub": subConv},
	}

	if got := tab.blockingSessionID(); got != "ses_main" {
		t.Fatalf("blockingSessionID() = %q, want ses_main", got)
	}

	// Viewing subagent while main is blocking still resolves ses_main.
	tab.activeSubID = "ses_sub"
	if got := tab.blockingSessionID(); got != "ses_main" {
		t.Fatalf("blockingSessionID() while viewing subagent = %q, want ses_main", got)
	}

	// If subagent itself is blocking on a backgroundable tool, activeConv takes precedence.
	subConv.blockingTools["tool_sub"] = true
	if got := tab.blockingSessionID(); got != "ses_sub" {
		t.Fatalf("blockingSessionID() with blocking subagent = %q, want ses_sub", got)
	}

	// When inactive, not blocking.
	mainConv.active = false
	subConv.active = false
	if got := tab.blockingSessionID(); got != "" {
		t.Fatalf("blockingSessionID() when inactive = %q, want empty", got)
	}
}

func TestFormatQuestionToolInput(t *testing.T) {
	input := map[string]any{
		"questions": []any{
			map[string]any{
				"header":   "Database",
				"question": "Which database should we use?",
				"options": []any{
					map[string]any{"label": "PostgreSQL (Recommended)", "description": "Relational DB"},
					map[string]any{"label": "SQLite", "description": "Embedded DB"},
				},
			},
		},
	}

	sub, details := formatQuestionToolInput(input)
	if sub != "Which database should we use?" {
		t.Errorf("subtitle = %q, want 'Which database should we use?'", sub)
	}
	if !strings.Contains(details, "1. [Database] Which database should we use?") ||
		!strings.Contains(details, "• PostgreSQL (Recommended) — Relational DB") {
		t.Errorf("unexpected details: %q", details)
	}

	multiInput := map[string]any{
		"questions": []any{
			map[string]any{"header": "DB", "question": "Q1"},
			map[string]any{"header": "Cache", "question": "Q2"},
		},
	}
	subMulti, _ := formatQuestionToolInput(multiInput)
	if subMulti != "DB, Cache" {
		t.Errorf("multi subtitle = %q, want 'DB, Cache'", subMulti)
	}
}

func TestQuestionFormWidgetAndNeedsAttention(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("GTK cannot be initialized")
	}

	form := opencode.FormInfo{
		ID:        "frm_test_1",
		SessionID: "ses_bg",
		Title:     "Questions",
		Fields: []opencode.FormField{
			{
				Key:         "q0",
				Title:       "Database",
				Description: "Which database should we use?",
				Type:        "string",
				Options: []opencode.FormOption{
					{Value: "PostgreSQL", Label: "PostgreSQL", Description: "Relational"},
					{Value: "SQLite", Label: "SQLite", Description: "Embedded"},
				},
				Custom: true,
			},
			{
				Key:         "q1",
				Title:       "Features",
				Description: "Select features",
				Type:        "multiselect",
				Options: []opencode.FormOption{
					{Value: "Auth", Label: "Auth"},
					{Value: "Metrics", Label: "Metrics"},
				},
				Custom: true,
			},
		},
	}

	var submitted map[string]any
	var dismissed bool
	qw := NewQuestionFormWidget(
		form,
		func(ans map[string]any) { submitted = ans },
		func() { dismissed = true },
	)

	// Initially q0 has first option selected, but q1 (multiselect, required) has none selected -> invalid.
	if _, ok := qw.CollectAnswers(); ok {
		t.Fatal("expected CollectAnswers to be invalid before selecting a multiselect option")
	}
	if qw.StepCount() != 2 || qw.CurrentStep() != 0 {
		t.Fatalf("expected StepCount=2 CurrentStep=0, got %d %d", qw.StepCount(), qw.CurrentStep())
	}
	// On step 0, q0 is valid so Next button is sensitive and Submit is hidden.
	if !qw.nextBtn.Sensitive() || qw.submitBtn.Visible() {
		t.Fatalf("expected nextBtn sensitive and submitBtn hidden on step 0")
	}
	if !qw.NextStep() || qw.CurrentStep() != 1 {
		t.Fatalf("expected NextStep() to advance to step 1")
	}
	if qw.submitBtn.Sensitive() {
		t.Fatal("expected submitBtn to be insensitive on step 1 when required multiselect is empty")
	}
	if !qw.PrevStep() || qw.CurrentStep() != 0 {
		t.Fatalf("expected PrevStep() to return to step 0")
	}

	// Select an option in q1 and type a custom answer in q0.
	qw.fields[0].customEntry.SetText("CockroachDB")
	qw.SetStep(1)
	qw.fields[1].options[0].btn.SetActive(true)
	qw.fields[1].customEntry.SetText("Tracing")

	ans, ok := qw.CollectAnswers()
	if !ok {
		t.Fatal("expected CollectAnswers to succeed after filling fields")
	}
	if ans["q0"] != "CockroachDB" {
		t.Errorf("ans[q0] = %v, want CockroachDB", ans["q0"])
	}
	q1Vals, _ := ans["q1"].([]string)
	if !slices.Equal(q1Vals, []string{"Auth", "Tracing"}) {
		t.Errorf("ans[q1] = %v, want [Auth, Tracing]", q1Vals)
	}

	if !qw.Submit() {
		t.Fatal("expected qw.Submit() to succeed")
	}
	if submitted == nil || submitted["q0"] != "CockroachDB" {
		t.Errorf("expected onSubmit to be called with answers, got %+v", submitted)
	}

	qw.SetBusy(false)
	qw.Dismiss()
	if !dismissed {
		t.Errorf("expected onDismiss to be called")
	}

	// Verify NeedsAttention behavior on background tab:
	// 1. setConvActive(true), reasoning delta, and tool call must NOT set NeedsAttention.
	// 2. text delta or form.created MUST set NeedsAttention.
	vs := adw.NewViewStack()
	win := &Window{
		viewStack:   vs,
		stopBtn:     gtk.NewButton(),
		tabs:        make(map[string]*sessionTab),
		childToRoot: make(map[string]string),
	}
	fgPage := vs.AddTitled(gtk.NewBox(gtk.OrientationVertical, 0), "ses_fg", "Foreground")
	bgPage := vs.AddTitled(gtk.NewBox(gtk.OrientationVertical, 0), "ses_bg", "Background")
	win.tabs["ses_fg"] = &sessionTab{
		page: fgPage,
		main: &convTimeline{
			session:            opencode.Session{ID: "ses_fg", Title: "Foreground"},
			formWidgets:        make(map[string]*QuestionFormWidget),
			blockingTools:      make(map[string]bool),
			blockingShellTools: make(map[string]bool),
			blockingShellIDs:   make(map[string]string),
			backgroundShellIDs: make(map[string]bool),
			toolNames:          make(map[string]string),
		},
	}
	bgTab := &sessionTab{
		page: bgPage,
		main: &convTimeline{
			session:            opencode.Session{ID: "ses_bg", Title: "Background"},
			formWidgets:        make(map[string]*QuestionFormWidget),
			blockingTools:      make(map[string]bool),
			blockingShellTools: make(map[string]bool),
			blockingShellIDs:   make(map[string]string),
			backgroundShellIDs: make(map[string]bool),
			toolNames:          make(map[string]string),
		},
	}
	win.tabs["ses_bg"] = bgTab
	vs.SetVisibleChildName("ses_fg")

	win.setConvActive("ses_bg", true)
	if bgTab.page.NeedsAttention() {
		t.Fatal("setConvActive(true) should not set NeedsAttention before text or question")
	}

	win.handleEvent(opencode.Event{
		Type: "session.reasoning.delta",
		Data: json.RawMessage(`{"sessionID":"ses_bg","assistantMessageID":"m1","ordinal":0,"delta":"thinking..."}`),
	})
	if bgTab.page.NeedsAttention() {
		t.Fatal("session.reasoning.delta should not set NeedsAttention")
	}

	win.handleEvent(opencode.Event{
		Type: "session.tool.called",
		Data: json.RawMessage(`{"sessionID":"ses_bg","assistantMessageID":"m1","id":"t1","name":"read","input":{"path":"a.go"}}`),
	})
	if bgTab.page.NeedsAttention() {
		t.Fatal("session.tool.called should not set NeedsAttention")
	}

	win.handleEvent(opencode.Event{
		Type: "session.text.delta",
		Data: json.RawMessage(`{"sessionID":"ses_bg","assistantMessageID":"m1","ordinal":1,"delta":"Hello user"}`),
	})
	if !bgTab.page.NeedsAttention() {
		t.Fatal("session.text.delta with non-empty text should set NeedsAttention on background tab")
	}

	// Reset and test form.created
	bgTab.page.SetNeedsAttention(false)
	bgTab.main.loaded = true
	win.handleEvent(opencode.Event{
		Type: "form.created",
		Data: json.RawMessage(`{"form":{"id":"frm_99","sessionID":"ses_bg","title":"Questions","fields":[{"key":"q0","title":"Choice","description":"Pick one","type":"string","options":[{"value":"A","label":"A"}]}]}}`),
	})
	if !bgTab.page.NeedsAttention() {
		t.Fatal("form.created should set NeedsAttention on background tab")
	}
	if len(bgTab.main.pendingForms) != 1 || bgTab.main.formWidgets["frm_99"] == nil {
		t.Fatalf("expected pending form frm_99 to be added to bgTab, got %+v", bgTab.main.pendingForms)
	}

	win.handleEvent(opencode.Event{
		Type: "form.replied",
		Data: json.RawMessage(`{"id":"frm_99","sessionID":"ses_bg","answer":{"q0":"A"}}`),
	})
	if len(bgTab.main.pendingForms) != 0 || bgTab.main.formWidgets["frm_99"] != nil {
		t.Fatalf("expected pending form frm_99 to be removed after form.replied, got %+v", bgTab.main.pendingForms)
	}
}

func TestSystemNoticeAndSwitchMessages(t *testing.T) {
	restartMsg := opencode.Message{
		ID:          "msg_restart",
		Type:        "synthetic",
		Description: "Continuing after restart",
		Text:        "The server restarted while you were working. Continue from where you left off.",
		Metadata:    map[string]any{"notice": "restart"},
	}
	if isSubagentOrShellSynthetic(restartMsg) {
		t.Fatal("restart synthetic message should not be classified as subagent/shell completion")
	}
	icon, title, sub := formatSystemNoticeSummary(restartMsg)
	if icon != "refresh-symbolic" || title != "Continuing after restart" || sub != "" {
		t.Errorf("restart summary = (%q, %q, %q), want (refresh-symbolic, Continuing after restart, '')", icon, title, sub)
	}

	instrMsg := opencode.Message{
		ID:          "msg_instr",
		Type:        "system",
		Description: "Instructions updated: core/codemode, core/mcp-guidance, core/skill-guidance",
		Text:        "Updated instructions body",
		Metadata: map[string]any{
			"notice":             "instructions",
			"instructionSources": []any{"core/codemode", "core/mcp-guidance", "core/skill-guidance"},
		},
	}
	icon, title, sub = formatSystemNoticeSummary(instrMsg)
	if icon != "channel-broadcast-symbolic" || title != "Instructions updated:" || sub != "core/codemode, core/mcp-guidance, core/skill-guidance" {
		t.Errorf("instructions summary = (%q, %q, %q)", icon, title, sub)
	}

	subagentMsg := opencode.Message{
		ID:          "msg_sub",
		Type:        "synthetic",
		Description: "Explore codebase",
		Text:        "<subagent sessionID=\"ses_child\" state=\"completed\">\ndone\n</subagent>",
		Metadata:    map[string]any{"source": "subagent", "childID": "ses_child"},
	}
	if !isSubagentOrShellSynthetic(subagentMsg) {
		t.Fatal("subagent synthetic message should be classified as subagent/shell completion")
	}

	skillMsg := opencode.Message{
		ID:   "msg_skill",
		Type: "skill",
		Name: "oklch-skill",
		Text: "Skill instructions...",
	}
	icon, title, sub = formatSystemNoticeSummary(skillMsg)
	if icon != "arrow-forward-symbolic" || title != "Skill" || sub != "oklch-skill" {
		t.Errorf("skill summary = (%q, %q, %q)", icon, title, sub)
	}

	if !gtk.InitCheck() {
		return
	}
	vs := adw.NewViewStack()
	win := &Window{
		viewStack:   vs,
		tabs:        make(map[string]*sessionTab),
		childToRoot: make(map[string]string),
	}
	tab := win.addSessionTab(opencode.Session{ID: "ses_1", Title: "Test"})

	if w := win.buildMessageWidget(tab, tab.main, restartMsg); w == nil {
		t.Fatal("expected SystemNoticeWidget for restartMsg")
	} else if _, ok := w.(*SystemNoticeWidget); !ok {
		t.Fatalf("expected *SystemNoticeWidget for restartMsg, got %T", w)
	}
	if w := win.buildMessageWidget(tab, tab.main, instrMsg); w == nil {
		t.Fatal("expected SystemNoticeWidget for instrMsg")
	}
	if w := win.buildMessageWidget(tab, tab.main, subagentMsg); w == nil {
		t.Fatal("expected SyntheticSubagentWidget for subagentMsg")
	} else if _, ok := w.(*SyntheticSubagentWidget); !ok {
		t.Fatalf("expected *SyntheticSubagentWidget for subagentMsg, got %T", w)
	}
}

func TestBackgroundShellFilteringAndOutput(t *testing.T) {
	// 1. Test FormatShellTailLines fixed 7-line buffer
	emptyBuf := FormatShellTailLines("", 7)
	if lines := strings.Split(emptyBuf, "\n"); len(lines) != 7 || !strings.Contains(lines[0], "Waiting for output") {
		t.Fatalf("unexpected empty FormatShellTailLines: %q (lines=%d)", emptyBuf, len(lines))
	}

	rawAns := "\x1b[32mline1\x1b[0m\nline2\rline2_updated\nline3\nline4\nline5\nline6\nline7\nline8\n"
	tailBuf := FormatShellTailLines(rawAns, 7)
	tailLines := strings.Split(tailBuf, "\n")
	if len(tailLines) != 7 {
		t.Fatalf("expected 7 lines from FormatShellTailLines, got %d: %q", len(tailLines), tailBuf)
	}
	if tailLines[0] != "line2_updated" || tailLines[6] != "line8" {
		t.Errorf("unexpected tailLines: first=%q last=%q all=%v", tailLines[0], tailLines[6], tailLines)
	}

	if !gtk.InitCheck() {
		return
	}

	// 2. Test foreground shell suppression vs background shell visibility & session.tool.progress
	vs := adw.NewViewStack()
	win := &Window{
		ctx:               t.Context(),
		viewStack:         vs,
		stopBtn:           gtk.NewButton(),
		shellStatusBox:    gtk.NewBox(gtk.OrientationHorizontal, 0),
		shellStatusLabel:  gtk.NewLabel(""),
		shellsSectionBox:  gtk.NewBox(gtk.OrientationVertical, 0),
		shellListBox:      gtk.NewListBox(),
		sheetEmptyLabel:   gtk.NewLabel(""),
		activeShells:      make(map[string]opencode.ShellItem),
		shellOutputs:      make(map[string]string),
		shellOutputLabels: make(map[string]*gtk.Label),
		tabs:              make(map[string]*sessionTab),
		childToRoot:       make(map[string]string),
	}
	tab1 := win.addSessionTab(opencode.Session{ID: "ses_1", Title: "Tab 1"})
	tab1.main.loaded = true
	_ = win.addSessionTab(opencode.Session{ID: "ses_2", Title: "Tab 2"})
	vs.SetVisibleChildName("ses_1")

	newTestToolWidget := func(id, name string) *ToolWidget {
		inView := gtk.NewTextView()
		outView := gtk.NewTextView()
		return &ToolWidget{
			Box:             gtk.NewBox(gtk.OrientationVertical, 0),
			ctx:             t.Context(),
			toolIcon:        gtk.NewImage(),
			titleLabel:      gtk.NewLabel(""),
			subtitleLabel:   gtk.NewLabel(""),
			badgesBox:       gtk.NewBox(gtk.OrientationHorizontal, 0),
			actionBtn:       gtk.NewButton(),
			spinner:         adw.NewSpinner(),
			statusLabel:     gtk.NewLabel(""),
			imageBox:        gtk.NewBox(gtk.OrientationHorizontal, 0),
			diffBox:         gtk.NewBox(gtk.OrientationVertical, 0),
			detailsExpander: gtk.NewExpander(""),
			inputView:       inView,
			outputScroll:    gtk.NewScrolledWindow(),
			outputView:      outView,
			inputBuf:        inView.Buffer(),
			outputBuf:       outView.Buffer(),
			toolID:          id,
			toolName:        name,
		}
	}
	tab1.main.assistantMap["m1"] = &AssistantMessageWidget{
		Box:      gtk.NewBox(gtk.OrientationVertical, 0),
		ctx:      t.Context(),
		partsBox: gtk.NewBox(gtk.OrientationVertical, 0),
		tools: map[string]*ToolWidget{
			"tool_fg": newTestToolWidget("tool_fg", "shell"),
			"tool_bg": newTestToolWidget("tool_bg", "shell"),
		},
	}

	// Start a foreground shell tool in ses_1: input.started -> called -> progress (with shellID) -> shell.created
	win.handleEvent(opencode.Event{
		Type: "session.tool.input.started",
		Data: json.RawMessage(`{"sessionID":"ses_1","assistantMessageID":"m1","id":"tool_fg","name":"shell"}`),
	})
	win.handleEvent(opencode.Event{
		Type: "session.tool.called",
		Data: json.RawMessage(`{"sessionID":"ses_1","assistantMessageID":"m1","id":"tool_fg","input":{"command":"ls -la"}}`),
	})
	win.handleEvent(opencode.Event{
		Type: "session.tool.progress",
		Data: json.RawMessage(`{"sessionID":"ses_1","assistantMessageID":"m1","id":"tool_fg","metadata":{"shellID":"sh_fg"}}`),
	})
	win.handleEvent(opencode.Event{
		Type: "shell.created",
		Data: json.RawMessage(`{"info":{"id":"sh_fg","pid":101,"command":"ls -la","status":"running","source":{"type":"session","sessionID":"ses_1"}}}`),
	})

	// Foreground blocking shell must NOT appear in visibleBackgroundShellsForTab!
	if got := win.visibleBackgroundShellsForTab(tab1); len(got) != 0 {
		t.Fatalf("expected 0 visible background shells while foreground shell is blocking, got %+v", got)
	}

	// Shell belonging to another session (ses_2) must NOT appear in tab1!
	win.handleEvent(opencode.Event{
		Type: "shell.created",
		Data: json.RawMessage(`{"info":{"id":"sh_other","pid":202,"command":"sleep 30","status":"running","source":{"type":"session","sessionID":"ses_2"}}}`),
	})
	if got := win.visibleBackgroundShellsForTab(tab1); len(got) != 0 {
		t.Fatalf("expected 0 visible background shells for tab1 when shell belongs to ses_2, got %+v", got)
	}

	// Now start a background shell in ses_1 (background: true):
	win.handleEvent(opencode.Event{
		Type: "session.tool.input.started",
		Data: json.RawMessage(`{"sessionID":"ses_1","assistantMessageID":"m1","id":"tool_bg","name":"shell"}`),
	})
	win.handleEvent(opencode.Event{
		Type: "session.tool.called",
		Data: json.RawMessage(`{"sessionID":"ses_1","assistantMessageID":"m1","id":"tool_bg","input":{"command":"sleep 10","background":true}}`),
	})
	win.handleEvent(opencode.Event{
		Type: "shell.created",
		Data: json.RawMessage(`{"info":{"id":"sh_bg","pid":303,"command":"sleep 10","status":"running","source":{"type":"session","sessionID":"ses_1"}}}`),
	})
	win.handleEvent(opencode.Event{
		Type: "session.tool.success",
		Data: json.RawMessage(`{"sessionID":"ses_1","assistantMessageID":"m1","id":"tool_bg","content":[{"type":"text","text":"Started background shell"}],"metadata":{"shellID":"sh_bg","status":"running"}}`),
	})

	got := win.visibleBackgroundShellsForTab(tab1)
	if len(got) != 1 || got[0].ID != "sh_bg" {
		t.Fatalf("expected [sh_bg] visible in tab1, got %+v", got)
	}

	// ToolWidget for tool_bg should remain in background-running state even after session.tool.success
	aw := tab1.main.assistantMap["m1"]
	if aw == nil || aw.tools["tool_bg"] == nil {
		t.Fatalf("expected AssistantMessageWidget m1 and tool_bg widget")
	}
	twBg := aw.tools["tool_bg"]
	if !twBg.IsBackgroundRunning() || twBg.ShellID() != "sh_bg" {
		t.Errorf("expected twBg.IsBackgroundRunning()=true and ShellID=sh_bg, got bgRunning=%v shellID=%q", twBg.IsBackgroundRunning(), twBg.ShellID())
	}

	// 3. Test inheritedAgentAndModel inherits agent and model from current conversation
	tab1.main.session.Agent = "plan"
	tab1.main.session.Model = opencode.ModelRef{ProviderID: "google", ID: "antigravity-gemini-4-argon-high"}
	inhAgent, inhModel := win.inheritedAgentAndModel()
	if inhAgent != "plan" || inhModel.ProviderID != "google" || inhModel.ID != "antigravity-gemini-4-argon-high" {
		t.Errorf("inheritedAgentAndModel() = (%q, %+v), want (plan, google/antigravity-gemini-4-argon-high)", inhAgent, inhModel)
	}
}

func TestSubagentSpawnBottomBarAndSheet(t *testing.T) {
	if !gtk.InitCheck() {
		return
	}

	vs := adw.NewViewStack()
	bgBtn := gtk.NewButton()
	bgBtn.SetVisible(false)
	subBox := gtk.NewBox(gtk.OrientationHorizontal, 0)
	subBox.SetVisible(false)
	subLbl := gtk.NewLabel("")
	subSection := gtk.NewBox(gtk.OrientationVertical, 0)
	subSection.SetVisible(false)
	subList := gtk.NewListBox()

	win := &Window{
		ctx:                 t.Context(),
		viewStack:           vs,
		stopBtn:             gtk.NewButton(),
		backgroundStatusBtn: bgBtn,
		subagentStatusBox:   subBox,
		subagentStatusLabel: subLbl,
		subagentsSectionBox: subSection,
		subagentListBox:     subList,
		sheetEmptyLabel:     gtk.NewLabel(""),
		tabs:                make(map[string]*sessionTab),
		childToRoot:         make(map[string]string),
	}

	tab := win.addSessionTab(opencode.Session{
		ID:       "ses_root",
		Title:    "Dissent v0.0.41 update",
		Agent:    "build",
		Location: opencode.Location{Directory: "/home/diamond/.nix"},
	})
	tab.main.loaded = true
	tab.main.active = true
	vs.SetVisibleChildName("ses_root")

	inView := gtk.NewTextView()
	outView := gtk.NewTextView()
	twSub := &ToolWidget{
		Box:             gtk.NewBox(gtk.OrientationVertical, 0),
		ctx:             t.Context(),
		toolIcon:        gtk.NewImage(),
		titleLabel:      gtk.NewLabel(""),
		subtitleLabel:   gtk.NewLabel(""),
		badgesBox:       gtk.NewBox(gtk.OrientationHorizontal, 0),
		actionBtn:       gtk.NewButton(),
		spinner:         adw.NewSpinner(),
		statusLabel:     gtk.NewLabel(""),
		imageBox:        gtk.NewBox(gtk.OrientationHorizontal, 0),
		diffBox:         gtk.NewBox(gtk.OrientationVertical, 0),
		detailsExpander: gtk.NewExpander(""),
		inputView:       inView,
		outputScroll:    gtk.NewScrolledWindow(),
		outputView:      outView,
		inputBuf:        inView.Buffer(),
		outputBuf:       outView.Buffer(),
		toolID:          "call_sub_1",
		toolName:        "subagent",
	}
	tab.main.assistantMap["msg_ast_1"] = &AssistantMessageWidget{
		Box:      gtk.NewBox(gtk.OrientationVertical, 0),
		ctx:      t.Context(),
		partsBox: gtk.NewBox(gtk.OrientationVertical, 0),
		tools: map[string]*ToolWidget{
			"call_sub_1": twSub,
		},
	}

	// 1. Subagent tool input started and called (blocking foreground subagent):
	// "Background (Ctrl+B)" button should immediately become visible!
	win.handleEvent(opencode.Event{
		Type: "session.tool.input.started",
		Data: json.RawMessage(`{"sessionID":"ses_root","assistantMessageID":"msg_ast_1","id":"call_sub_1","name":"subagent"}`),
	})
	win.handleEvent(opencode.Event{
		Type: "session.tool.called",
		Data: json.RawMessage(`{"sessionID":"ses_root","assistantMessageID":"msg_ast_1","id":"call_sub_1","input":{"agent":"explore","description":"Find dissent package in repo","prompt":"search for dissent"}}`),
	})
	if !bgBtn.Visible() {
		t.Fatalf("expected backgroundStatusBtn ('Background (Ctrl+B)') to be visible while foreground subagent tool is blocking")
	}

	// 2. OpenCode V2 emits flat session.created for the child session (with "sessionID" and "parentID"):
	// Bottom bar should immediately show "1 subagent" and subagent sheet should list the running subagent.
	win.handleEvent(opencode.Event{
		Type: "session.created",
		Data: json.RawMessage(`{"sessionID":"ses_child","parentID":"ses_root","title":"Find dissent package in repo","agent":"explore","location":{"directory":"/home/diamond/.nix"}}`),
	})
	if !subBox.Visible() || subLbl.Label() != "1 subagent" {
		t.Fatalf("expected subagentStatusBox visible with label '1 subagent' after flat session.created, got visible=%v label=%q", subBox.Visible(), subLbl.Label())
	}
	if !subSection.Visible() || len(win.subagentRows) != 1 || win.subagentRows[0].session.ID != "ses_child" {
		t.Fatalf("expected subagentsSectionBox visible with 1 row for ses_child, got visible=%v rows=%+v", subSection.Visible(), win.subagentRows)
	}

	// 3. Even if session.created was missed, session.tool.progress with metadata.sessionID registers the child subagent
	// and wires the ToolWidget's "View Session ->" button!
	win.handleEvent(opencode.Event{
		Type: "session.tool.progress",
		Data: json.RawMessage(`{"sessionID":"ses_root","assistantMessageID":"msg_ast_1","id":"call_sub_1","text":"Find dissent package in repo","metadata":{"agent":"explore","sessionID":"ses_child"}}`),
	})
	if twSub.ChildSessionID() != "ses_child" || !twSub.actionBtn.Visible() {
		t.Fatalf("expected ToolWidget ChildSessionID=ses_child and actionBtn visible after session.tool.progress, got childID=%q visible=%v", twSub.ChildSessionID(), twSub.actionBtn.Visible())
	}

	// 4. Suppose the user backgrounds the subagent: session.tool.success arrives with metadata.status="running".
	// The blocking "Background (Ctrl+B)" button hides, while "1 subagent" stays visible in the bottom bar
	// and the ToolWidget stays in background-running state!
	win.handleEvent(opencode.Event{
		Type: "session.tool.success",
		Data: json.RawMessage(`{"sessionID":"ses_root","assistantMessageID":"msg_ast_1","id":"call_sub_1","content":[{"type":"text","text":"Subagent moved to background"}],"metadata":{"agent":"explore","sessionID":"ses_child","status":"running"}}`),
	})
	if bgBtn.Visible() {
		t.Fatalf("expected backgroundStatusBtn hidden once subagent tool moves to background")
	}
	if !subBox.Visible() || subLbl.Label() != "1 subagent" {
		t.Fatalf("expected '1 subagent' to remain visible while background subagent is still running, got visible=%v label=%q", subBox.Visible(), subLbl.Label())
	}
	if !twSub.IsBackgroundRunning() {
		t.Fatalf("expected ToolWidget IsBackgroundRunning()=true while subagent is running in background")
	}

	// 5. When the background subagent finishes and delivers a synthetic completion message to ses_root,
	// "1 subagent" hides from the bottom bar and the ToolWidget transitions out of background-running state.
	win.handleEvent(opencode.Event{
		Type: "session.synthetic",
		Data: json.RawMessage(`{"sessionID":"ses_root","metadata":{"source":"subagent","childID":"ses_child","state":"completed"}}`),
	})
	if subBox.Visible() {
		t.Fatalf("expected subagentStatusBox hidden after subagent completes, got visible=true label=%q", subLbl.Label())
	}
	if twSub.IsBackgroundRunning() {
		t.Fatalf("expected ToolWidget IsBackgroundRunning()=false after subagent completes")
	}
}
