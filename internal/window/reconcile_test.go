package window

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
	"unsafe"

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
		pendingPrompts := []pendingPrompt{
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
}
