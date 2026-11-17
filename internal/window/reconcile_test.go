package window

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4-sourceview/pkg/gtksource/v5"
	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
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
	now := time.Now()
	mainConv := &convTimeline{
		session:       opencode.Session{ID: "ses_main"},
		active:        true,
		blockingTools: map[string]time.Time{"tool_1": now.Add(-2 * time.Second)},
	}
	subConv := &convTimeline{
		session:       opencode.Session{ID: "ses_sub"},
		active:        true,
		blockingTools: map[string]time.Time{},
	}
	tab := &sessionTab{
		main:         mainConv,
		subTimelines: map[string]*convTimeline{"ses_sub": subConv},
	}

	if got := tab.blockingSessionID(); got != "ses_main" {
		t.Fatalf("blockingSessionID() = %q, want ses_main", got)
	}
	if tab.shouldShowBackgroundHint(now) {
		t.Fatalf("shouldShowBackgroundHint() = true after 2s, want false (< 5s)")
	}
	if !tab.shouldShowBackgroundHint(now.Add(4 * time.Second)) {
		t.Fatalf("shouldShowBackgroundHint() = false after 6s, want true (>= 5s)")
	}

	// Viewing subagent while main is blocking still resolves ses_main.
	tab.activeSubID = "ses_sub"
	if got := tab.blockingSessionID(); got != "ses_main" {
		t.Fatalf("blockingSessionID() while viewing subagent = %q, want ses_main", got)
	}

	// If subagent itself is blocking on a backgroundable tool, activeConv takes precedence.
	subConv.blockingTools["tool_sub"] = now
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
	// 1. setConvActive(true), reasoning delta, tool call, and mid-turn text delta must NOT set NeedsAttention.
	// 2. Root turn completion after settle delay OR live form.created MUST set NeedsAttention.
	vs := adw.NewViewStack()
	win := &Window{
		viewStack:             vs,
		stopBtn:               gtk.NewButton(),
		tabs:                  make(map[string]*sessionTab),
		childToRoot:           make(map[string]string),
		completionSettleDelay: 20 * time.Millisecond,
	}
	fgPage := vs.AddTitled(gtk.NewBox(gtk.OrientationVertical, 0), "ses_fg", "Foreground")
	bgPage := vs.AddTitled(gtk.NewBox(gtk.OrientationVertical, 0), "ses_bg", "Background")
	win.tabs["ses_fg"] = &sessionTab{
		page: fgPage,
		main: &convTimeline{
			session:            opencode.Session{ID: "ses_fg", Title: "Foreground"},
			formWidgets:        make(map[string]*QuestionFormWidget),
			blockingTools:      make(map[string]time.Time),
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
			blockingTools:      make(map[string]time.Time),
			blockingShellTools: make(map[string]bool),
			blockingShellIDs:   make(map[string]string),
			backgroundShellIDs: make(map[string]bool),
			toolNames:          make(map[string]string),
		},
	}
	win.tabs["ses_bg"] = bgTab
	vs.SetVisibleChildName("ses_fg")

	win.setConvActive("ses_bg", true)
	if bgTab.NeedsAttention() {
		t.Fatal("setConvActive(true) should not set NeedsAttention before text or question")
	}

	win.handleEvent(opencode.Event{
		Type: "session.reasoning.delta",
		Data: json.RawMessage(`{"sessionID":"ses_bg","assistantMessageID":"m1","ordinal":0,"delta":"thinking..."}`),
	})
	if bgTab.NeedsAttention() {
		t.Fatal("session.reasoning.delta should not set NeedsAttention")
	}

	win.handleEvent(opencode.Event{
		Type: "session.tool.called",
		Data: json.RawMessage(`{"sessionID":"ses_bg","assistantMessageID":"m1","id":"t1","name":"read","input":{"path":"a.go"}}`),
	})
	if bgTab.NeedsAttention() {
		t.Fatal("session.tool.called should not set NeedsAttention")
	}

	win.handleEvent(opencode.Event{
		Type: "session.text.delta",
		Data: json.RawMessage(`{"sessionID":"ses_bg","assistantMessageID":"m1","ordinal":1,"delta":"Hello user"}`),
	})
	if bgTab.NeedsAttention() {
		t.Fatal("mid-turn session.text.delta must not set NeedsAttention before turn ends")
	}

	win.handleEvent(opencode.Event{
		Type: "session.execution.succeeded",
		Data: json.RawMessage(`{"sessionID":"ses_bg"}`),
	})
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && !bgTab.NeedsAttention() {
		glib.MainContextDefault().Iteration(false)
		time.Sleep(5 * time.Millisecond)
	}
	if !bgTab.NeedsAttention() || !bgTab.finishedAttention {
		t.Fatalf("expected finishedAttention + NeedsAttention on background tab after turn settled (finished=%v)", bgTab.finishedAttention)
	}

	// Reset and test form.created
	bgTab.setNeedsAttention(false)
	bgTab.main.loaded = true
	win.handleEvent(opencode.Event{
		Type: "form.created",
		Data: json.RawMessage(`{"form":{"id":"frm_99","sessionID":"ses_bg","title":"Questions","fields":[{"key":"q0","title":"Choice","description":"Pick one","type":"string","options":[{"value":"A","label":"A"}]}]}}`),
	})
	if !bgTab.NeedsAttention() || !bgTab.actionAttention {
		t.Fatalf("form.created should set actionAttention + NeedsAttention on background tab (action=%v)", bgTab.actionAttention)
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
	if bgTab.actionAttention || bgTab.NeedsAttention() {
		t.Fatalf("expected actionAttention cleared after form.replied, got action=%v needs=%v", bgTab.actionAttention, bgTab.NeedsAttention())
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
	} else if sw, ok := w.(*SyntheticSubagentWidget); !ok {
		t.Fatalf("expected *SyntheticSubagentWidget for subagentMsg, got %T", w)
	} else if sw.expander != nil {
		t.Fatalf("expected SyntheticSubagentWidget for non-shell subagent finish to omit GtkExpander")
	}

	readTw := &ToolWidget{
		Box:             gtk.NewBox(gtk.OrientationVertical, 0),
		ctx:             t.Context(),
		headerRow:       gtk.NewBox(gtk.OrientationHorizontal, 0),
		summaryRow:      gtk.NewBox(gtk.OrientationHorizontal, 0),
		detailsBox:      gtk.NewBox(gtk.OrientationVertical, 0),
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
		inputView:       gtk.NewTextView(),
		outputScroll:    gtk.NewScrolledWindow(),
		outputView:      gtk.NewTextView(),
		toolID:          "call_read_1",
		toolName:        "read",
	}
	readTw.inputBuf = readTw.inputView.Buffer()
	readTw.outputBuf = readTw.outputView.Buffer()
	readTw.headerRow.Append(readTw.detailsExpander)
	readTw.Append(readTw.headerRow)
	readTw.Append(readTw.detailsBox)
	readTw.applyToolKind()
	if readTw.detailsExpander == nil {
		t.Fatalf("expected read ToolWidget to keep detailsExpander for lazy expansion")
	}
	readTw.SetOutput([]opencode.ToolContentItem{
		{Type: "text", Text: "1: secret file contents"},
		{Type: "file", Mime: "image/png", Name: "dot.png", Data: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg=="},
	}, nil)
	if txt := readTw.outputBuf.Text(readTw.outputBuf.StartIter(), readTw.outputBuf.EndIter(), false); txt != "" {
		t.Fatalf("expected read ToolWidget not to load file output before expansion, got %q", txt)
	}
	if len(readTw.imageWidgets) != 0 || readTw.imageBox.Visible() {
		t.Fatalf("expected read ToolWidget not to decode images before expansion")
	}
	readTw.detailsExpander.SetExpanded(true)
	if txt := readTw.outputBuf.Text(readTw.outputBuf.StartIter(), readTw.outputBuf.EndIter(), false); txt != "1: secret file contents" {
		t.Fatalf("expected read ToolWidget to load file output when expanded, got %q", txt)
	}
	if len(readTw.imageWidgets) != 1 || !readTw.imageBox.Visible() {
		t.Fatalf("expected read ToolWidget to decode and show image when expanded, got len=%d visible=%v", len(readTw.imageWidgets), readTw.imageBox.Visible())
	}
	readTw.detailsExpander.SetExpanded(false)
	if readTw.imageBox.Visible() {
		t.Fatalf("expected read ToolWidget to hide imageBox when collapsed again")
	}

	// 2. Test read ToolWidget with OpenCode URI and "Image read successfully" suppression
	readImgTw := &ToolWidget{
		Box:             gtk.NewBox(gtk.OrientationVertical, 0),
		ctx:             t.Context(),
		headerRow:       gtk.NewBox(gtk.OrientationHorizontal, 0),
		summaryRow:      gtk.NewBox(gtk.OrientationHorizontal, 0),
		detailsBox:      gtk.NewBox(gtk.OrientationVertical, 0),
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
		inputView:       gtk.NewTextView(),
		outputScroll:    gtk.NewScrolledWindow(),
		outputView:      gtk.NewTextView(),
		toolID:          "call_read_img",
		toolName:        "read",
	}
	readImgTw.inputBuf = readImgTw.inputView.Buffer()
	readImgTw.outputBuf = readImgTw.outputView.Buffer()
	readImgTw.headerRow.Append(readImgTw.detailsExpander)
	readImgTw.Append(readImgTw.headerRow)
	readImgTw.Append(readImgTw.detailsBox)
	readImgTw.Append(readImgTw.imageBox)
	readImgTw.applyToolKind()
	readImgTw.SetInput(map[string]any{"path": "opencode/offline-new2.png"})
	readImgTw.SetOutput([]opencode.ToolContentItem{
		{Type: "text", Text: "Image read successfully"},
		{Type: "file", Mime: "image/png", Name: "opencode/offline-new2.png", URI: "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg=="},
	}, nil)

	if readImgTw.imageBox.Visible() {
		t.Fatalf("expected imageBox hidden before expansion")
	}
	readImgTw.detailsExpander.SetExpanded(true)
	if txt := readImgTw.outputBuf.Text(readImgTw.outputBuf.StartIter(), readImgTw.outputBuf.EndIter(), false); txt != "" {
		t.Fatalf("expected 'Image read successfully' placeholder to be suppressed, got %q", txt)
	}
	if readImgTw.outputScroll.Visible() {
		t.Fatalf("expected outputScroll hidden when only image is present")
	}
	if len(readImgTw.imageWidgets) != 1 || !readImgTw.imageBox.Visible() {
		t.Fatalf("expected 1 thumbnail and imageBox visible upon expansion, got len=%d visible=%v", len(readImgTw.imageWidgets), readImgTw.imageBox.Visible())
	}
	readImgTw.detailsExpander.SetExpanded(false)
	if readImgTw.imageBox.Visible() {
		t.Fatalf("expected imageBox hidden when collapsed again")
	}
}

func TestBackgroundShellFilteringAndOutput(t *testing.T) {
	// 1. Test FormatShellTailLines fixed 7-line buffer
	emptyBuf := FormatShellTailLines("", 7)
	if lines := strings.Split(emptyBuf, "\n"); len(lines) != 7 || !strings.Contains(lines[0], "Waiting for output") {
		t.Fatalf("unexpected empty FormatShellTailLines: %q (lines=%d)", emptyBuf, len(lines))
	}

	// A byte-offset tail seek can start mid-rune; GtkTextBuffer rejects invalid UTF-8 outright.
	if got := FormatShellTailLines("\xa0\xe2\x96 done ✓\n\xff\xfebinary\n", 7); !utf8.ValidString(got) || !strings.Contains(got, "done ✓") {
		t.Fatalf("expected valid UTF-8 tail keeping 'done ✓', got %q", got)
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

	// Starting a new foreground shell tool must NOT hide the existing background shell sh_bg!
	tab1.main.assistantMap["m1"].tools["tool_fg2"] = newTestToolWidget("tool_fg2", "shell")
	win.handleEvent(opencode.Event{
		Type: "session.tool.called",
		Data: json.RawMessage(`{"sessionID":"ses_1","assistantMessageID":"m1","id":"tool_fg2","name":"shell","input":{"command":"pwd"}}`),
	})
	if got := win.visibleBackgroundShellsForTab(tab1); len(got) != 1 || got[0].ID != "sh_bg" {
		t.Fatalf("expected sh_bg to remain visible while another foreground shell runs, got %+v", got)
	}

	// Test live tail reading from output file and from GetShellOutput when output exceeds 16 KB.
	outPath := filepath.Join(t.TempDir(), "sh_bg.out")
	if err := os.WriteFile(outPath, []byte("tick 1\ntick 2\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	twBg.SetOutput([]opencode.ToolContentItem{{
		Type: "text",
		Text: "Command moved to the background (shell ID: sh_bg).\nOutput is streaming to: " + outPath,
	}}, nil)
	if twBg.ShellFile() != outPath {
		t.Fatalf("expected ShellFile()=%q, got %q", outPath, twBg.ShellFile())
	}
	if text, ok := win.fetchSingleShellOutput(opencode.ShellItem{ID: "sh_bg"}); !ok || !strings.Contains(text, "tick 2") {
		t.Fatalf("expected fetchSingleShellOutput to read live file tail with 'tick 2', got ok=%v text=%q", ok, text)
	}
	if err := os.WriteFile(outPath, []byte("tick 1\ntick 2\ntick 3\n"), 0o600); err != nil {
		t.Fatalf("WriteFile update: %v", err)
	}
	if text, ok := win.fetchSingleShellOutput(opencode.ShellItem{ID: "sh_bg"}); !ok || !strings.Contains(text, "tick 3") {
		t.Fatalf("expected fetchSingleShellOutput to reflect updated live file tail with 'tick 3', got ok=%v text=%q", ok, text)
	}

	// Also verify fallback to synthetic background shell item when w.activeShells is temporarily empty,
	// and GetShellOutput tail-seek when output size > 16384 bytes.
	delete(win.activeShells, "sh_bg")
	if got := win.visibleBackgroundShellsForTab(tab1); len(got) != 1 || got[0].ID != "sh_bg" || got[0].File != outPath {
		t.Fatalf("expected fallback visible background shell from ToolWidget with File=%q, got %+v", outPath, got)
	}
	_ = os.Remove(outPath)
	bigPrefix := strings.Repeat("old line\n", 2500) // > 20 KB
	fullRemote := bigPrefix + "tail A\ntail B\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := 0
		_, _ = fmt.Sscanf(r.URL.Query().Get("cursor"), "%d", &cursor)
		limit := 16384
		_, _ = fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit)
		if cursor > len(fullRemote) {
			cursor = len(fullRemote)
		}
		end := min(len(fullRemote), cursor+limit)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output": fullRemote[cursor:end],
			"cursor": end,
			"size":   len(fullRemote),
		})
	}))
	defer srv.Close()
	win.client = &opencode.Client{BaseURL: srv.URL}
	if text, ok := win.fetchSingleShellOutput(opencode.ShellItem{ID: "sh_bg"}); !ok || !strings.Contains(text, "tail B") {
		t.Fatalf("expected fetchSingleShellOutput to seek to tail when output > 16KB, got ok=%v text=%q", ok, text)
	}
	win.client = nil

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
	// "Background (Ctrl+B)" button should stay hidden for the first 5 seconds, then become visible once > 5s has elapsed.
	win.handleEvent(opencode.Event{
		Type: "session.tool.input.started",
		Data: json.RawMessage(`{"sessionID":"ses_root","assistantMessageID":"msg_ast_1","id":"call_sub_1","name":"subagent"}`),
	})
	win.handleEvent(opencode.Event{
		Type: "session.tool.called",
		Data: json.RawMessage(`{"sessionID":"ses_root","assistantMessageID":"msg_ast_1","id":"call_sub_1","input":{"agent":"explore","description":"Find dissent package in repo","prompt":"search for dissent"}}`),
	})
	if bgBtn.Visible() {
		t.Fatalf("expected backgroundStatusBtn ('Background (Ctrl+B)') to stay hidden before 5 seconds have elapsed")
	}
	tab.main.blockingTools["call_sub_1"] = time.Now().Add(-6 * time.Second)
	win.updateBottomBar(tab)
	if !bgBtn.Visible() {
		t.Fatalf("expected backgroundStatusBtn ('Background (Ctrl+B)') to be visible once foreground subagent tool has been blocking for > 5s")
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

	// 6. Clicking the window title opens the rename popover and submitting updates the session title,
	// tab page title, and windowTitle widget.
	win.windowTitleBtn = gtk.NewMenuButton()
	win.windowTitle = adw.NewWindowTitle("Dissent v0.0.41 update", "~/.nix")
	win.renameSessionPopover = gtk.NewPopover()
	win.renameSessionEntry = gtk.NewEntry()
	win.renameSessionEntry.SetText("  Updated Dissent Session Title  ")
	win.submitRenameSession()

	if tab.main.session.Title != "Updated Dissent Session Title" {
		t.Errorf("tab.main.session.Title = %q, want Updated Dissent Session Title", tab.main.session.Title)
	}
	if tab.Title() != "Updated Dissent Session Title" {
		t.Errorf("tab.Title() = %q, want Updated Dissent Session Title", tab.Title())
	}
	if win.windowTitle.Title() != "Updated Dissent Session Title" {
		t.Errorf("win.windowTitle.Title() = %q, want Updated Dissent Session Title", win.windowTitle.Title())
	}

	// 7. Conversations do NOT reorder on background tool calls, steps, or routine events;
	// they only reorder to the top when attention is needed (pink dot) or the user sends a message.
	win.collapsedProjects = make(map[string]bool)
	win.expandedProjects = make(map[string]bool)
	win.projectSessions = make(map[string][]opencode.Session)
	nowMs := time.Now().UnixMilli()
	tab.main.session.Time.Updated = nowMs - 9*60*1000
	win.tabOrder = []string{"ses_root"}
	win.ensureSessionTab(opencode.Session{
		ID:       "ses_older_13m",
		Title:    "Testing opencode question tool",
		Location: opencode.Location{Directory: "/home/diamond/.nix"},
		Time:     opencode.SessionTime{Updated: nowMs - 13*60*1000},
	})
	win.ensureSessionTab(opencode.Session{
		ID:       "ses_older_20m",
		Title:    "GNOME Wayland technical discussion",
		Location: opencode.Location{Directory: "/home/diamond/.nix"},
		Time:     opencode.SessionTime{Updated: nowMs - 20*60*1000},
	})
	if !slices.Equal(win.tabOrder, []string{"ses_root", "ses_older_13m", "ses_older_20m"}) {
		t.Fatalf("initial tabOrder = %v, want [ses_root ses_older_13m ses_older_20m]", win.tabOrder)
	}
	vs.SetVisibleChildName("ses_root")

	// Background events (inbox enqueued, step started, tool called, tool success, step ended, session updated)
	// must NOT bump ses_older_20m up the list.
	for _, ev := range []opencode.Event{
		{Type: "session.inbox.enqueued", Data: json.RawMessage(`{"sessionID":"ses_older_20m","inboxID":"in_1","delivery":"queue"}`)},
		{Type: "session.execution.started", Data: json.RawMessage(`{"sessionID":"ses_older_20m"}`)},
		{Type: "session.step.started", Data: json.RawMessage(`{"sessionID":"ses_older_20m","assistantMessageID":"msg_a1"}`)},
		{Type: "session.reasoning.delta", Data: json.RawMessage(`{"sessionID":"ses_older_20m","assistantMessageID":"msg_a1","delta":"thinking..."}`)},
		{Type: "session.tool.called", Data: json.RawMessage(`{"sessionID":"ses_older_20m","assistantMessageID":"msg_a1","id":"call_1","name":"read"}`)},
		{Type: "session.tool.success", Data: json.RawMessage(`{"sessionID":"ses_older_20m","assistantMessageID":"msg_a1","id":"call_1","name":"read"}`)},
		{Type: "session.step.ended", Data: json.RawMessage(`{"sessionID":"ses_older_20m","assistantMessageID":"msg_a1"}`)},
		{Type: "session.updated", Data: json.RawMessage(`{"sessionID":"ses_older_20m","session":{"id":"ses_older_20m","time":{"updated":9999999999999}}}`)},
	} {
		win.handleEvent(ev)
		if !slices.Equal(win.tabOrder, []string{"ses_root", "ses_older_13m", "ses_older_20m"}) {
			t.Fatalf("tabOrder after %s on ses_older_20m = %v, want unchanged [ses_root ses_older_13m ses_older_20m]", ev.Type, win.tabOrder)
		}
	}

	// Mid-turn text delta must NOT bump ses_older_20m; settling after completion DOES bump it to the top of tabOrder.
	win.completionSettleDelay = 20 * time.Millisecond
	win.handleEvent(opencode.Event{
		Type: "session.text.delta",
		Data: json.RawMessage(`{"sessionID":"ses_older_20m","assistantMessageID":"msg_a1","delta":"Here is the answer."}`),
	})
	if !slices.Equal(win.tabOrder, []string{"ses_root", "ses_older_13m", "ses_older_20m"}) {
		t.Fatalf("tabOrder after mid-turn text delta on ses_older_20m = %v, want unchanged [ses_root ses_older_13m ses_older_20m]", win.tabOrder)
	}
	win.handleEvent(opencode.Event{
		Type: "session.execution.succeeded",
		Data: json.RawMessage(`{"sessionID":"ses_older_20m"}`),
	})
	bumpDeadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(bumpDeadline) && !win.tabs["ses_older_20m"].finishedAttention {
		glib.MainContextDefault().Iteration(false)
		time.Sleep(5 * time.Millisecond)
	}
	if !slices.Equal(win.tabOrder, []string{"ses_older_20m", "ses_root", "ses_older_13m"}) {
		t.Fatalf("tabOrder after completion settle on ses_older_20m = %v, want [ses_older_20m ses_root ses_older_13m]", win.tabOrder)
	}

	// User sends a message into ses_older_13m -> bumps ses_older_13m to the top of tabOrder.
	win.bumpSessionToTop(win.tabs["ses_older_13m"])
	if !slices.Equal(win.tabOrder, []string{"ses_older_13m", "ses_older_20m", "ses_root"}) {
		t.Fatalf("tabOrder after user message in ses_older_13m = %v, want [ses_older_13m ses_older_20m ses_root]", win.tabOrder)
	}

	// Permission asked (attention needed) on background tab ses_root -> bumps ses_root to the top of tabOrder.
	vs.SetVisibleChildName("ses_older_13m")
	win.tabs["ses_root"].setNeedsAttention(false)
	win.tabs["ses_root"].main.loaded = false
	win.handleEvent(opencode.Event{
		Type: "permission.asked",
		Data: json.RawMessage(`{"request":{"id":"perm_bump_1","sessionID":"ses_root","permission":"bash","patterns":["rm -rf /tmp/foo"]}}`),
	})
	if !slices.Equal(win.tabOrder, []string{"ses_root", "ses_older_13m", "ses_older_20m"}) {
		t.Fatalf("tabOrder after permission.asked on ses_root = %v, want [ses_root ses_older_13m ses_older_20m]", win.tabOrder)
	}

	// 8. Hiding (clearing) a project removes all of its conversations from the sidebar without deleting them,
	// and creating a new session in that project brings the project back with just that new session alone.
	win.statePath = t.TempDir() + "/gluppencode.json"
	win.archivedSessions = make(map[string]bool)
	win.headerTitleStack = gtk.NewStack()
	win.clearProjectSessions("/home/diamond/.nix")
	if len(win.tabOrder) != 0 || len(win.tabs) != 0 {
		t.Fatalf("expected all project tabs cleared from sidebar, got tabOrder=%v len(tabs)=%d", win.tabOrder, len(win.tabs))
	}
	if !win.archivedSessions["ses_root"] || !win.archivedSessions["ses_older_13m"] || !win.archivedSessions["ses_older_20m"] {
		t.Fatalf("expected all project sessions marked archived, got %+v", win.archivedSessions)
	}
	newSess := opencode.Session{
		ID:       "ses_new_after_clear",
		Title:    "Fresh session after clear",
		Location: opencode.Location{Directory: "/home/diamond/.nix"},
		Time:     opencode.SessionTime{Updated: nowMs},
	}
	win.ensureSessionTab(newSess)
	if !slices.Equal(win.tabOrder, []string{"ses_new_after_clear"}) {
		t.Fatalf("expected only new session in sidebar after clear, got tabOrder=%v", win.tabOrder)
	}
	if !win.archivedSessions["ses_root"] {
		t.Fatalf("expected old sessions to remain hidden until explicitly selected from search")
	}
}

func TestTakeChargeAutoReply(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("GTK cannot be initialized")
	}

	var (
		mu      sync.Mutex
		replied = make(map[string]map[string]any)
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/form") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reply") {
			var body struct {
				Answer map[string]any `json:"answer"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			formID := parts[len(parts)-2]
			mu.Lock()
			replied[formID] = body.Answer
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	vs := adw.NewViewStack()
	win := &Window{
		ctx:                t.Context(),
		client:             &opencode.Client{BaseURL: srv.URL},
		viewStack:          vs,
		takeChargeBtn:      gtk.NewToggleButton(),
		tabs:               make(map[string]*sessionTab),
		childToRoot:        make(map[string]string),
		takeChargeInFlight: make(map[string]bool),
	}

	fgPage := vs.AddTitled(gtk.NewBox(gtk.OrientationVertical, 0), "ses_fg", "Foreground")
	bgPage := vs.AddTitled(gtk.NewBox(gtk.OrientationVertical, 0), "ses_bg", "Background")
	win.tabs["ses_fg"] = &sessionTab{
		page: fgPage,
		main: &convTimeline{
			session:     opencode.Session{ID: "ses_fg", Title: "Foreground"},
			formWidgets: make(map[string]*QuestionFormWidget),
		},
		subTimelines: make(map[string]*convTimeline),
	}
	bgTab := &sessionTab{
		page: bgPage,
		main: &convTimeline{
			session:     opencode.Session{ID: "ses_bg", Title: "Background"},
			loaded:      true,
			formWidgets: make(map[string]*QuestionFormWidget),
		},
		subTimelines: make(map[string]*convTimeline),
	}
	win.tabs["ses_bg"] = bgTab
	vs.SetVisibleChildName("ses_fg")

	// 1. A pending form arrives while Take Charge is OFF -> waits for user.
	win.handleEvent(opencode.Event{
		Type: "form.created",
		Data: json.RawMessage(`{"form":{"id":"frm_existing","sessionID":"ses_bg","title":"Questions","fields":[{"key":"q0","title":"DB","description":"Which database?","type":"string","custom":true,"options":[{"value":"Postgres (Recommended)","label":"Postgres (Recommended)"}]},{"key":"q1","title":"Features","description":"Which features?","type":"multiselect","custom":true,"options":[{"value":"FTS","label":"FTS"}]}]}}`),
	})
	if len(bgTab.main.pendingForms) != 1 || bgTab.main.formWidgets["frm_existing"] == nil {
		t.Fatalf("expected frm_existing to be pending before Take Charge is enabled, got %+v", bgTab.main.pendingForms)
	}

	// 2. User toggles Take Charge ON -> existing pending form is automatically answered and removed.
	win.takeChargeBtn.SetActive(true)
	win.setTakeCharge(true)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		glib.MainContextDefault().Iteration(false)
		mu.Lock()
		_, ok := replied["frm_existing"]
		mu.Unlock()
		if ok && len(bgTab.main.pendingForms) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	ans1, ok1 := replied["frm_existing"]
	mu.Unlock()
	if !ok1 {
		t.Fatalf("expected frm_existing to be auto-replied when Take Charge was enabled")
	}
	if got, _ := ans1["q0"].(string); got != TakeChargeDirective {
		t.Errorf("frm_existing q0 answer = %q, want %q", got, TakeChargeDirective)
	}
	if gotSlice, ok := ans1["q1"].([]any); !ok || len(gotSlice) != 1 || gotSlice[0] != TakeChargeDirective {
		t.Errorf("frm_existing q1 answer = %#v, want [%q]", ans1["q1"], TakeChargeDirective)
	}
	if len(bgTab.main.pendingForms) != 0 || bgTab.main.formWidgets["frm_existing"] != nil {
		t.Errorf("expected frm_existing removed from pendingForms and formWidgets, got %+v", bgTab.main.pendingForms)
	}

	// 3. While Take Charge is ON, a new form.created arrives -> immediately auto-answered without setting NeedsAttention.
	bgTab.setNeedsAttention(false)
	win.handleEvent(opencode.Event{
		Type: "form.created",
		Data: json.RawMessage(`{"form":{"id":"frm_incoming","sessionID":"ses_bg","title":"Questions","fields":[{"key":"q0","title":"Deploy","description":"Deploy now?","type":"string","custom":true}]}}`),
	})
	if bgTab.NeedsAttention() {
		t.Errorf("expected form.created while Take Charge is active NOT to set NeedsAttention")
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		glib.MainContextDefault().Iteration(false)
		mu.Lock()
		_, ok := replied["frm_incoming"]
		mu.Unlock()
		if ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	ans2, ok2 := replied["frm_incoming"]
	mu.Unlock()
	if !ok2 {
		t.Fatalf("expected frm_incoming to be auto-replied while Take Charge is active")
	}
	if got, _ := ans2["q0"].(string); got != TakeChargeDirective {
		t.Errorf("frm_incoming q0 answer = %q, want %q", got, TakeChargeDirective)
	}
}

func TestReorderPendingPrompts(t *testing.T) {
	if !gtk.InitCheck() {
		return
	}

	var mu sync.Mutex
	var admitted []opencode.PromptRequest
	var cancelled []string
	seq := 10

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/session/ses_q/prompt":
			var req opencode.PromptRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			seq++
			id := fmt.Sprintf("inb_%d", seq)
			admitted = append(admitted, req)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(opencode.InboxItem{
				ID:       id,
				Type:     "user",
				Delivery: req.Delivery,
				Payload:  opencode.InboxPayload{Text: req.Text},
			})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/session/ses_q/inbox/"):
			inboxID := strings.TrimPrefix(r.URL.Path, "/api/session/ses_q/inbox/")
			mu.Lock()
			cancelled = append(cancelled, inboxID)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	win := &Window{
		ctx:    t.Context(),
		client: &opencode.Client{BaseURL: srv.URL},
		tabs:   make(map[string]*sessionTab),
	}
	conv := &convTimeline{
		session:          opencode.Session{ID: "ses_q"},
		thinkingLabel:    gtk.NewLabel(""),
		ignoredCancelIDs: make(map[string]bool),
		loaded:           true,
	}
	conv.thinkingLabel.SetVisible(false)
	win.tabs["ses_q"] = &sessionTab{main: conv}

	// Verify sidebar indicator spinner shows for running subagents, while background shells show
	// a static shellIcon without spinner or Thinking... in the main conversation.
	ind := &switcherIndicator{
		box:          gtk.NewBox(gtk.OrientationHorizontal, 0),
		spinner:      adw.NewSpinner(),
		questionIcon: gtk.NewImageFromIconName("dialog-question-symbolic"),
		shellIcon:    gtk.NewImageFromIconName("terminal-symbolic"),
		label:        gtk.NewLabel(""),
	}
	ind.spinner.SetVisible(false)
	ind.questionIcon.SetVisible(false)
	ind.shellIcon.SetVisible(false)
	win.tabs["ses_q"].sidebarIndicator = ind
	win.activeShells = make(map[string]opencode.ShellItem)
	win.childToRoot = make(map[string]string)

	win.upsertSubagent(win.tabs["ses_q"], opencode.Session{
		ID:       "sub_bg",
		ParentID: "ses_q",
		Time:     opencode.SessionTime{Busy: 100, Idle: 0},
	})
	if !ind.spinner.Visible() {
		t.Errorf("expected sidebar spinner visible while background subagent is running")
	}
	if conv.thinkingLabel.Visible() {
		t.Errorf("expected main thinkingLabel hidden when only background subagent is running")
	}
	win.setSubagentActive("sub_bg", false, "succeeded")
	if ind.spinner.Visible() {
		t.Errorf("expected sidebar spinner hidden after background subagent completes")
	}

	win.activeShells["sh_bg_spin"] = opencode.ShellItem{
		ID:        "sh_bg_spin",
		SessionID: "ses_q",
		Status:    "running",
	}
	win.updateShellStatus()
	if ind.spinner.Visible() {
		t.Errorf("expected sidebar spinner hidden when only background shell is running")
	}
	if !ind.shellIcon.Visible() {
		t.Errorf("expected sidebar shellIcon visible while background shell is running")
	}
	if conv.thinkingLabel.Visible() {
		t.Errorf("expected main thinkingLabel hidden when only background shell is running")
	}
	delete(win.activeShells, "sh_bg_spin")
	win.updateShellStatus()
	if ind.spinner.Visible() || ind.shellIcon.Visible() {
		t.Errorf("expected sidebar spinner and shellIcon hidden after background shell completes (spinner=%v shellIcon=%v)", ind.spinner.Visible(), ind.shellIcon.Visible())
	}

	p1 := &pendingPrompt{inboxID: "inb_1", text: "first", delivery: opencode.DeliveryQueue}
	p2 := &pendingPrompt{inboxID: "inb_2", text: "second", delivery: opencode.DeliveryQueue}
	p3 := &pendingPrompt{inboxID: "inb_3", text: "third", delivery: opencode.DeliveryQueue}
	conv.pendingPrompts = []*pendingPrompt{p1, p2, p3}

	// Dragging p3 onto p2 -> new order [p1, p3, p2]; firstChanged is index 1, so only suffix [p3, p2] is rewritten.
	if !win.reorderPendingPrompt(conv, "inb_3", p2) {
		t.Fatalf("expected reorderPendingPrompt(inb_3 -> p2) to succeed")
	}
	if len(conv.pendingPrompts) != 3 || conv.pendingPrompts[0] != p1 || conv.pendingPrompts[1] != p3 || conv.pendingPrompts[2] != p2 {
		t.Fatalf("unexpected pendingPrompts order: [%s, %s, %s]", conv.pendingPrompts[0].text, conv.pendingPrompts[1].text, conv.pendingPrompts[2].text)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		glib.MainContextDefault().Iteration(false)
		mu.Lock()
		done := len(admitted) == 2 && len(cancelled) == 2
		mu.Unlock()
		if done && !p3.rewriting && !p2.rewriting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	gotAdmitted := slices.Clone(admitted)
	gotCancelled := slices.Clone(cancelled)
	mu.Unlock()

	if len(gotAdmitted) != 2 || gotAdmitted[0].Text != "third" || gotAdmitted[1].Text != "second" {
		t.Fatalf("expected suffix [third, second] to be re-admitted, got %+v", gotAdmitted)
	}
	for i, req := range gotAdmitted {
		if req.Delivery != opencode.DeliveryQueue || req.Resume {
			t.Errorf("admitted[%d]: expected Delivery=queue and Resume=false, got Delivery=%q Resume=%v", i, req.Delivery, req.Resume)
		}
	}
	if !slices.Equal(gotCancelled, []string{"inb_3", "inb_2"}) {
		t.Fatalf("expected replaced IDs [inb_3, inb_2] to be cancelled, got %v", gotCancelled)
	}
	if p1.inboxID != "inb_1" || p3.inboxID != "inb_11" || p2.inboxID != "inb_12" {
		t.Fatalf("expected updated inboxIDs [inb_1, inb_11, inb_12], got [%s, %s, %s]", p1.inboxID, p3.inboxID, p2.inboxID)
	}

	// Simulate the server's SSE session.inbox.cancelled events for the replaced IDs; they must be ignored.
	for _, oldID := range []string{"inb_3", "inb_2"} {
		win.handleEvent(opencode.Event{
			Type: "session.inbox.cancelled",
			Data: json.RawMessage(fmt.Sprintf(`{"sessionID":"ses_q","inboxID":%q}`, oldID)),
		})
	}
	if len(conv.pendingPrompts) != 3 {
		t.Fatalf("expected ignored session.inbox.cancelled events not to remove reordered prompts, got len=%d", len(conv.pendingPrompts))
	}

	// Steered prompts cannot be reordered.
	p2.delivery = opencode.DeliverySteer
	if win.reorderPendingPrompt(conv, p2.inboxID, p1) {
		t.Errorf("expected reorderPendingPrompt to reject steered source prompt")
	}
	if win.reorderPendingPrompt(conv, p1.inboxID, p2) {
		t.Errorf("expected reorderPendingPrompt to reject steered target prompt")
	}
}

func TestComposerInputHistory(t *testing.T) {
	if !gtk.InitCheck() {
		return
	}
	gtksource.Init()

	stateFile := filepath.Join(t.TempDir(), "state.json")
	buf := gtksource.NewBuffer(nil)
	win := &Window{
		statePath:   stateFile,
		inputBuffer: buf,
	}

	// 1. Commit history entries (consecutive duplicates are deduplicated) and persist.
	win.recordInputHistory("first prompt")
	win.recordInputHistory("first prompt")
	win.recordInputHistory("second\nmultiline\nprompt")
	win.recordInputHistory("third prompt")
	win.saveState()

	if !slices.Equal(win.inputHistory, []string{"first prompt", "second\nmultiline\nprompt", "third prompt"}) {
		t.Fatalf("unexpected inputHistory: %#v", win.inputHistory)
	}

	win2 := &Window{statePath: stateFile}
	win2.loadState()
	if !slices.Equal(win2.inputHistory, win.inputHistory) {
		t.Fatalf("expected persisted inputHistory %#v, got %#v", win.inputHistory, win2.inputHistory)
	}

	// 2. Typing a live draft and pressing Up on the top line recalls "third prompt" and saves the draft.
	buf.SetText("unsent draft")
	_, end := buf.Bounds()
	buf.PlaceCursor(end)

	if !win.handleInputHistoryKey(gdk.KEY_Up, 0) {
		t.Fatalf("expected Up on single-line draft to navigate history")
	}
	start, end := buf.Bounds()
	if got := buf.Text(start, end, false); got != "third prompt" {
		t.Fatalf("expected buffer 'third prompt', got %q", got)
	}

	// 3. Pressing Up again recalls the multiline entry and places cursor at the end (line 2).
	if !win.handleInputHistoryKey(gdk.KEY_Up, 0) {
		t.Fatalf("expected second Up to recall multiline prompt")
	}
	start, end = buf.Bounds()
	if got := buf.Text(start, end, false); got != "second\nmultiline\nprompt" {
		t.Fatalf("expected buffer 'second\\nmultiline\\nprompt', got %q", got)
	}

	// Because cursor is at the end (line 2, not top line 0), Up does NOT jump history yet.
	if win.handleInputHistoryKey(gdk.KEY_Up, 0) {
		t.Fatalf("expected Up on bottom line of multiline prompt to move cursor, not jump history")
	}

	// Move cursor to top line (line 0); now Down should NOT jump history, while Up jumps to "first prompt".
	start, _ = buf.Bounds()
	buf.PlaceCursor(start)
	if win.handleInputHistoryKey(gdk.KEY_Down, 0) {
		t.Fatalf("expected Down on top line of multiline prompt to move cursor, not jump history")
	}
	if !win.handleInputHistoryKey(gdk.KEY_Up, 0) {
		t.Fatalf("expected Up on top line of multiline prompt to recall 'first prompt'")
	}
	start, end = buf.Bounds()
	if got := buf.Text(start, end, false); got != "first prompt" {
		t.Fatalf("expected buffer 'first prompt', got %q", got)
	}

	// 4. Pressing Down steps forward through history and restores "unsent draft" at the bottom.
	if !win.handleInputHistoryKey(gdk.KEY_Down, 0) {
		t.Fatalf("expected Down to step forward to 'second\\nmultiline\\nprompt'")
	}
	if !win.handleInputHistoryKey(gdk.KEY_Down, 0) {
		t.Fatalf("expected Down at end of multiline prompt to step forward to 'third prompt'")
	}
	if !win.handleInputHistoryKey(gdk.KEY_Down, 0) {
		t.Fatalf("expected Down from newest history item to restore draft")
	}
	start, end = buf.Bounds()
	if got := buf.Text(start, end, false); got != "unsent draft" {
		t.Fatalf("expected restored draft 'unsent draft', got %q", got)
	}
	if win.handleInputHistoryKey(gdk.KEY_Down, 0) {
		t.Fatalf("expected Down at live draft to return false")
	}

	// 5. Verify Search Sessions sorts strictly by recency when search box is empty (even across large timestamp gaps).
	pool := []opencode.Session{
		{ID: "ses_old", Title: "Old Session", Time: opencode.SessionTime{Updated: 1_700_009_999_000}}, // %10000 = 9999 (used to sort first by mistake!)
		{ID: "ses_mid", Title: "Mid Session", Time: opencode.SessionTime{Updated: 1_700_100_001_000}}, // %10000 = 1
		{ID: "ses_new", Title: "New Session", Time: opencode.SessionTime{Updated: 1_700_200_000_000}}, // %10000 = 0
		{ID: "ses_sub", Title: "Child Session", ParentID: "ses_new", Time: opencode.SessionTime{Updated: 1_700_300_000_000}},
	}
	matches := win.matchAndSortSessions(pool, "")
	if len(matches) != 3 {
		t.Fatalf("expected 3 non-child sessions, got %d", len(matches))
	}
	if matches[0].sess.ID != "ses_new" || matches[1].sess.ID != "ses_mid" || matches[2].sess.ID != "ses_old" {
		t.Fatalf("expected recency order [ses_new, ses_mid, ses_old], got [%s, %s, %s]",
			matches[0].sess.ID, matches[1].sess.ID, matches[2].sess.ID)
	}
}

func TestReorderPendingPromptsInFlight(t *testing.T) {
	if !gtk.InitCheck() {
		return
	}
	gtksource.Init()

	var mu sync.Mutex
	var admitted []string
	var cancelled []string
	var steered []string
	failPost := false
	release := make(chan struct{})
	seq := 10

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/session/ses_f/prompt":
			<-release
			var req opencode.PromptRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			fail := failPost && len(admitted) == 1
			if fail {
				mu.Unlock()
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			seq++
			id := fmt.Sprintf("inb_%d", seq)
			admitted = append(admitted, req.Text)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": opencode.InboxItem{ID: id, Type: "user", Delivery: req.Delivery}})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/session/ses_f/inbox/"):
			mu.Lock()
			cancelled = append(cancelled, strings.TrimPrefix(r.URL.Path, "/api/session/ses_f/inbox/"))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/session/ses_f/inbox/"):
			mu.Lock()
			steered = append(steered, strings.TrimPrefix(r.URL.Path, "/api/session/ses_f/inbox/"))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	newFixture := func() (*Window, *convTimeline, []*pendingPrompt) {
		win := &Window{
			ctx:         t.Context(),
			client:      &opencode.Client{BaseURL: srv.URL},
			tabs:        make(map[string]*sessionTab),
			inputBuffer: gtksource.NewBuffer(nil),
		}
		conv := &convTimeline{
			session:          opencode.Session{ID: "ses_f"},
			ignoredCancelIDs: make(map[string]bool),
			loaded:           true,
		}
		win.tabs["ses_f"] = &sessionTab{main: conv}
		ps := []*pendingPrompt{
			{inboxID: "inb_1", text: "first", delivery: opencode.DeliveryQueue},
			{inboxID: "inb_2", text: "second", delivery: opencode.DeliveryQueue},
			{inboxID: "inb_3", text: "third", delivery: opencode.DeliveryQueue},
		}
		conv.pendingPrompts = slices.Clone(ps)
		return win, conv, ps
	}
	waitFor := func(cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			glib.MainContextDefault().Iteration(false)
			mu.Lock()
			ok := cond()
			mu.Unlock()
			if ok {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for condition")
	}

	t.Run("success", func(t *testing.T) {
		win, conv, ps := newFixture()
		p1, p2, p3 := ps[0], ps[1], ps[2]
		if !win.reorderPendingPrompt(conv, "inb_3", p1) { // -> [p3, p1, p2]
			t.Fatalf("expected reorder to start")
		}
		if win.reorderPendingPrompt(conv, "inb_2", p3) {
			t.Fatalf("expected a second reorder to be rejected while the first is in flight")
		}
		// The replacement's session.inbox.enqueued must not bind or spawn a card.
		win.onInboxEnqueued(nil, conv, opencode.InboxEvent{InboxID: "inb_x", Item: &opencode.InboxItem{Type: "user", Payload: opencode.InboxPayload{Text: "first"}}})
		if len(conv.pendingPrompts) != 3 || p1.inboxID != "inb_1" {
			t.Fatalf("replacement enqueue was not swallowed: len=%d p1.inboxID=%q", len(conv.pendingPrompts), p1.inboxID)
		}
		win.steerPendingPrompt(conv, p1)
		win.cancelPendingPrompt(conv, p2)
		mu.Lock()
		if len(steered) != 0 || len(cancelled) != 0 {
			t.Fatalf("expected Steer/Cancel to be deferred while rewriting, got steered=%v cancelled=%v", steered, cancelled)
		}
		mu.Unlock()

		close(release)
		waitFor(func() bool { return len(admitted) == 3 && len(cancelled) == 4 && len(steered) == 1 })
		mu.Lock()
		defer mu.Unlock()
		if !slices.Equal(admitted, []string{"third", "first", "second"}) {
			t.Fatalf("admitted = %v", admitted)
		}
		// Originals inb_3, inb_1, inb_2 plus the replacement of cancelled p2 (inb_13).
		if !slices.Equal(cancelled, []string{"inb_3", "inb_1", "inb_2", "inb_13"}) {
			t.Fatalf("cancelled = %v", cancelled)
		}
		if p1.inboxID != "inb_12" || !slices.Equal(steered, []string{"inb_12"}) {
			t.Fatalf("expected deferred Steer replayed on replacement inb_12, got p1.inboxID=%q steered=%v", p1.inboxID, steered)
		}
		if p3.inboxID != "inb_11" || p3.rewriting || p1.rewriting {
			t.Fatalf("unexpected p3 state: inboxID=%q rewriting=%v", p3.inboxID, p3.rewriting)
		}
	})

	t.Run("failure restores order", func(t *testing.T) {
		mu.Lock()
		admitted, cancelled, steered = nil, nil, nil
		failPost = true
		mu.Unlock()
		win, conv, ps := newFixture()
		p1, p2, p3 := ps[0], ps[1], ps[2]
		if !win.reorderPendingPrompt(conv, "inb_3", p1) { // -> [p3, p1, p2], second POST fails
			t.Fatalf("expected reorder to start")
		}
		waitFor(func() bool { return len(cancelled) == 1 && !p3.rewriting })
		if !slices.Equal(conv.pendingPrompts, []*pendingPrompt{p1, p2, p3}) {
			t.Fatalf("expected order restored to [first, second, third], got [%s, %s, %s]",
				conv.pendingPrompts[0].text, conv.pendingPrompts[1].text, conv.pendingPrompts[2].text)
		}
		if p1.inboxID != "inb_1" || p2.inboxID != "inb_2" || p3.inboxID != "inb_3" {
			t.Fatalf("expected original inbox IDs kept, got %q %q %q", p1.inboxID, p2.inboxID, p3.inboxID)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(admitted) != 1 || cancelled[0] != fmt.Sprintf("inb_%d", seq) {
			t.Fatalf("expected the one admitted replacement to be withdrawn, admitted=%v cancelled=%v", admitted, cancelled)
		}
	})
}

func TestRefreshShellsCompletesStaleWidgets(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}
	tests := []struct {
		name         string
		fail         bool
		wantLoaded   bool
		wantDeadDone bool
	}{
		{name: "listing succeeds", wantLoaded: true, wantDeadDone: true},
		{name: "listing fails", fail: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			release := make(chan struct{})
			var served atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				<-release
				defer served.Store(true)
				if tt.fail {
					http.Error(w, "boom", http.StatusInternalServerError)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []opencode.ShellItem{{ID: "sh_live", Status: "running"}}})
			}))
			defer srv.Close()

			win := &Window{
				ctx:               t.Context(),
				client:            &opencode.Client{BaseURL: srv.URL},
				viewStack:         adw.NewViewStack(),
				activeShells:      map[string]opencode.ShellItem{"sh_dead": {ID: "sh_dead"}, "sh_live": {ID: "sh_live"}},
				shellOutputs:      make(map[string]string),
				shellOutputLabels: make(map[string]*gtk.Label),
				tabs:              make(map[string]*sessionTab),
				childToRoot:       make(map[string]string),
			}
			tab := win.addSessionTab(opencode.Session{ID: "ses_1"})
			bgTool := func(id, shellID string) *ToolWidget {
				out := gtk.NewTextView()
				tw := &ToolWidget{
					Box: gtk.NewBox(gtk.OrientationVertical, 0), ctx: t.Context(),
					toolIcon: gtk.NewImage(), titleLabel: gtk.NewLabel(""), subtitleLabel: gtk.NewLabel(""),
					badgesBox: gtk.NewBox(gtk.OrientationHorizontal, 0), actionBtn: gtk.NewButton(),
					spinner: adw.NewSpinner(), statusLabel: gtk.NewLabel(""),
					outputScroll: gtk.NewScrolledWindow(), outputView: out, outputBuf: out.Buffer(),
					toolID: id, toolName: "shell",
				}
				tw.isBackground, tw.metaStatus, tw.shellID = true, "running", shellID
				return tw
			}
			tools := map[string]*ToolWidget{"t_dead": bgTool("t_dead", "sh_dead"), "t_live": bgTool("t_live", "sh_live")}
			tab.main.assistantMap["m1"] = &AssistantMessageWidget{tools: tools}

			win.refreshShells()
			// A shell started while the listing is in flight must survive it.
			tools["t_new"] = bgTool("t_new", "sh_new")
			win.handleEvent(opencode.Event{Type: "shell.created", Data: json.RawMessage(`{"info":{"id":"sh_new","status":"running"}}`)})
			close(release)

			deadline := time.Now().Add(3 * time.Second)
			for !served.Load() || glib.MainContextDefault().Pending() {
				if time.Now().After(deadline) {
					t.Fatal("timed out waiting for refreshShells")
				}
				glib.MainContextDefault().Iteration(false)
				time.Sleep(5 * time.Millisecond)
			}

			if win.shellsLoaded != tt.wantLoaded {
				t.Errorf("shellsLoaded = %v, want %v", win.shellsLoaded, tt.wantLoaded)
			}
			if done := !tools["t_dead"].IsBackgroundRunning(); done != tt.wantDeadDone {
				t.Errorf("stale widget completed = %v, want %v", done, tt.wantDeadDone)
			}
			if !tools["t_live"].IsBackgroundRunning() || !tools["t_new"].IsBackgroundRunning() {
				t.Errorf("listed or in-flight shell widgets must stay running")
			}
			if _, ok := win.activeShells["sh_new"]; !ok {
				t.Errorf("shell created during refresh was dropped: %v", win.activeShells)
			}
			if _, ok := win.activeShells["sh_dead"]; ok == tt.wantLoaded {
				t.Errorf("sh_dead in activeShells = %v after listing (fail=%v)", ok, tt.fail)
			}
		})
	}
}

func TestContextSessionIDAndMarkdownSessionResolution(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}

	win := &Window{
		ctx:                   t.Context(),
		viewStack:             adw.NewViewStack(),
		contextSectionBox:     gtk.NewBox(gtk.OrientationVertical, 0),
		contextSessionIDBox:   gtk.NewBox(gtk.OrientationHorizontal, 0),
		contextSessionIDLabel: gtk.NewLabel(""),
		copySessionIDBtn:      gtk.NewButton(),
		contextInfoGrid:       gtk.NewGrid(),
		tabs:                  make(map[string]*sessionTab),
		childToRoot:           make(map[string]string),
		modelNames:            make(map[string]string),
		modelLimits:           make(map[string]int),
		expandedProjects:      make(map[string]bool),
		collapsedProjects:     make(map[string]bool),
		archivedSessions:      make(map[string]bool),
		projectSessions:       make(map[string][]opencode.Session),
		resolvedSessionTitles: make(map[string]string),
		resolvingSessions:     make(map[string][]func(string)),
	}

	tab1 := win.addSessionTab(opencode.Session{
		ID:    "ses_abc1234567890",
		Title: "First Conversation",
	})
	tab2 := win.addSessionTab(opencode.Session{
		ID:    "ses_def0987654321",
		Title: "Second Conversation",
	})
	_ = tab2

	win.rebuildContextInfo(tab1)
	if got := win.contextSessionIDLabel.Text(); got != "ses_abc1234567890" {
		t.Errorf("contextSessionIDLabel.Text() = %q, want ses_abc1234567890", got)
	}
	if !win.contextSessionIDBox.Visible() {
		t.Errorf("expected contextSessionIDBox to be visible")
	}

	if got := win.resolveSessionTitleAsync("ses_def0987654321", nil); got != "Second Conversation" {
		t.Errorf("resolveSessionTitleAsync() = %q, want Second Conversation", got)
	}

	win.viewStack.SetVisibleChildName("ses_abc1234567890")
	if !win.handleMarkdownLink("opencode://session/ses_def0987654321") {
		t.Errorf("expected handleMarkdownLink to handle opencode://session/ link")
	}
	if got := win.viewStack.VisibleChildName(); got != "ses_def0987654321" {
		t.Errorf("VisibleChildName() after clicking session link = %q, want ses_def0987654321", got)
	}
}

func TestStartFromScratchDefaultVisibility(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}

	stateFile := filepath.Join(t.TempDir(), "gluppencode.json")
	sessions := []opencode.Session{
		{
			ID:       "ses_inactive_1",
			Title:    "Old Conversation 1",
			Location: opencode.Location{Directory: "/home/diamond/proj"},
			Time:     opencode.SessionTime{Updated: 1000},
		},
		{
			ID:       "ses_active_1",
			Title:    "Running Conversation",
			Location: opencode.Location{Directory: "/home/diamond/proj"},
			Time:     opencode.SessionTime{Updated: 2000},
		},
		{
			ID:       "ses_inactive_2",
			Title:    "Old Conversation 2",
			Location: opencode.Location{Directory: "/home/diamond/proj"},
			Time:     opencode.SessionTime{Updated: 3000},
		},
	}
	active := map[string]opencode.ActiveStatus{
		"ses_active_1": {Type: "busy"},
	}

	win1 := &Window{
		ctx:               t.Context(),
		statePath:         stateFile,
		viewStack:         adw.NewViewStack(),
		tabs:              make(map[string]*sessionTab),
		childToRoot:       make(map[string]string),
		expandedProjects:  make(map[string]bool),
		collapsedProjects: make(map[string]bool),
		visibleSessions:   make(map[string]bool),
		archivedSessions:  make(map[string]bool),
		projectSessions:   make(map[string][]opencode.Session),
		allSessions:       sessions,
	}
	win1.loadState()
	win1.populateSessionTabs(sessions, active)

	if !slices.Equal(win1.tabOrder, []string{"ses_active_1"}) {
		t.Fatalf("initial tabOrder = %v, want [ses_active_1] (inactive conversations hidden by default)", win1.tabOrder)
	}

	// Explicitly opening an inactive session reveals it and persists it across restarts.
	win1.selectSession(sessions[2])
	if !slices.Equal(win1.tabOrder, []string{"ses_inactive_2", "ses_active_1"}) {
		t.Fatalf("tabOrder after selectSession = %v, want [ses_inactive_2 ses_active_1]", win1.tabOrder)
	}

	// Simulate app restart when no sessions are actively running anymore.
	win2 := &Window{
		ctx:               t.Context(),
		statePath:         stateFile,
		viewStack:         adw.NewViewStack(),
		tabs:              make(map[string]*sessionTab),
		childToRoot:       make(map[string]string),
		expandedProjects:  make(map[string]bool),
		collapsedProjects: make(map[string]bool),
		visibleSessions:   make(map[string]bool),
		archivedSessions:  make(map[string]bool),
		projectSessions:   make(map[string][]opencode.Session),
		allSessions:       sessions,
	}
	win2.loadState()
	win2.populateSessionTabs(sessions, nil)

	if !slices.Equal(win2.tabOrder, []string{"ses_inactive_2", "ses_active_1"}) {
		t.Fatalf("tabOrder after restart = %v, want [ses_inactive_2 ses_active_1] (ses_inactive_1 remains hidden)", win2.tabOrder)
	}
}

func TestThinkingIndicatorAboveQueuedMessages(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}

	// Widgets are hand-built: NewUserMessageWidget trips checkptr under -race.
	box := gtk.NewBox(gtk.OrientationVertical, 0)
	history := gtk.NewLabel("history")
	prompt := func(id string, delivery opencode.InboxDelivery) *pendingPrompt {
		return &pendingPrompt{
			inboxID:  id,
			delivery: delivery,
			attached: true,
			widget:   &UserMessageWidget{Box: gtk.NewBox(gtk.OrientationVertical, 0)},
		}
	}
	queued1 := prompt("inb_q1", opencode.DeliveryQueue)
	steered := prompt("inb_steer", opencode.DeliverySteer)
	queued2 := prompt("inb_q2", opencode.DeliveryQueue)
	conv := &convTimeline{
		timelineBox:      box,
		thinkingLabel:    gtk.NewLabel("Thinking..."),
		thinkingAttached: true,
		revertBanner:     &RevertBannerWidget{Box: gtk.NewBox(gtk.OrientationHorizontal, 0)},
		pendingPrompts:   []*pendingPrompt{queued1, steered, queued2},
		active:           true,
	}
	// Start from a scrambled order to prove keepPendingAtBottom reorders.
	box.Append(conv.revertBanner)
	box.Append(queued2.widget)
	box.Append(conv.thinkingLabel)
	box.Append(history)
	box.Append(queued1.widget)
	box.Append(steered.widget)

	conv.keepPendingAtBottom()

	names := map[uintptr]string{
		history.Object.Native():            "history",
		steered.widget.Object.Native():     "steered",
		conv.thinkingLabel.Object.Native(): "thinking",
		queued1.widget.Object.Native():     "queued1",
		queued2.widget.Object.Native():     "queued2",
		conv.revertBanner.Object.Native():  "revert",
	}
	// ObserveChildren items avoid marshalling *gtk.Box siblings (checkptr-safe).
	children := box.ObserveChildren()
	var order []string
	for i := range children.NItems() {
		order = append(order, names[children.Item(i).Native()])
	}

	want := []string{"history", "steered", "thinking", "queued1", "queued2", "revert"}
	if !slices.Equal(order, want) {
		t.Fatalf("timeline order = %v, want %v", order, want)
	}
	if !conv.thinkingLabel.Visible() {
		t.Error("thinking label hidden while conversation is active")
	}
}

func TestSidebarResizePersistence(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}

	stateFile := filepath.Join(t.TempDir(), "gluppencode.json")
	win1 := &Window{
		ctx:         t.Context(),
		statePath:   stateFile,
		rootContent: adw.NewOverlaySplitView(),
	}
	win1.loadState()
	win1.setDesktopSidebarWidth(380)
	win1.saveState()

	if got := int(win1.rootContent.MaxSidebarWidth()); got != 380 {
		t.Fatalf("MaxSidebarWidth() = %d, want 380", got)
	}

	// Simulate app restart and verify sidebar width is restored on desktop and reset when collapsed.
	win2 := &Window{
		ctx:         t.Context(),
		statePath:   stateFile,
		rootContent: adw.NewOverlaySplitView(),
	}
	win2.loadState()
	if win2.sidebarWidth != 380 {
		t.Fatalf("restored sidebarWidth = %d, want 380", win2.sidebarWidth)
	}
	if got := int(win2.rootContent.MaxSidebarWidth()); got != 380 {
		t.Fatalf("restored MaxSidebarWidth() = %d, want 380", got)
	}

	win2.rootContent.SetCollapsed(true)
	win2.syncNavigationChrome()
	if got := int(win2.rootContent.MaxSidebarWidth()); got != 300 {
		t.Fatalf("collapsed MaxSidebarWidth() = %d, want 300", got)
	}
	win2.rootContent.SetCollapsed(false)
	win2.syncNavigationChrome()
	if got := int(win2.rootContent.MaxSidebarWidth()); got != 380 {
		t.Fatalf("uncollapsed MaxSidebarWidth() = %d, want 380", got)
	}
}

func TestGreeterVisibilityAndPersonalizedGeneration(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}
	gtksource.Init()

	var (
		mu        sync.Mutex
		gotPrompt string
		gotModel  opencode.ModelRef
		genCalls  int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/experimental/generate/text" {
			var body struct {
				Prompt string            `json:"prompt"`
				Model  opencode.ModelRef `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			genCalls++
			gotPrompt = body.Prompt
			gotModel = body.Model
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"text":"{\"userName\":\"diamond\",\"pronouns\":\"it/its (3rd person)\",\"tone\":\"friendly, informal, feminine, direct\",\"title\":\"What is diamond building today?\",\"subtitle\":\"Ask a question, explore the repo, or let's dive into diamond's code together.\"}"}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	stateFile := filepath.Join(t.TempDir(), "gluppencode.json")
	win := &Window{
		ctx:         t.Context(),
		statePath:   stateFile,
		client:      &opencode.Client{BaseURL: srv.URL},
		viewStack:   adw.NewViewStack(),
		inputBuffer: gtksource.NewBuffer(nil),
		tabs:        make(map[string]*sessionTab),
		childToRoot: make(map[string]string),
		agents: []opencode.Agent{
			{
				ID:     "build",
				Name:   "build",
				Mode:   "primary",
				System: "You are providing code help for diamond. Always refer to diamond in the third person using it/its pronouns.",
			},
		},
		models: []opencode.ModelInfo{
			{ProviderID: "cloudflare-workers-ai", ID: "@cf/zai-org/glm-5.3-flash"},
		},
	}
	win.loadState()

	tab := win.addSessionTab(opencode.Session{ID: "ses_empty", Title: "Untitled Session"})
	tab.main.updateGreeterVisibility()
	if tab.main.greeter == nil || tab.main.greeter.Visible() {
		t.Fatalf("expected greeter hidden while conversation history is not loaded")
	}
	tab.main.loaded = true
	tab.main.updateGreeterVisibility()
	if !tab.main.greeter.Visible() {
		t.Fatalf("expected greeter to be visible on empty conversation")
	}
	if got := tab.main.greeter.titleLabel.Label(); got != defaultGreeterTitle {
		t.Fatalf("initial greeter title = %q, want %q", got, defaultGreeterTitle)
	}

	// Trigger lightweight model generation to read the system prompt and persist personalized greeting.
	win.refreshGreeterProfileAsync()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		glib.MainContextDefault().Iteration(false)
		if win.greeterProfile != nil && !win.greeterRefreshing {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	calls := genCalls
	modelUsed := gotModel
	promptUsed := gotPrompt
	mu.Unlock()

	if calls != 1 {
		t.Fatalf("expected 1 generate/text call, got %d", calls)
	}
	if modelUsed.ProviderID != "cloudflare-workers-ai" || modelUsed.ID != "@cf/zai-org/glm-5.3-flash" {
		t.Fatalf("expected lightweight model cloudflare-workers-ai/@cf/zai-org/glm-5.3-flash, got %+v", modelUsed)
	}
	if !strings.Contains(promptUsed, "Always refer to diamond in the third person") {
		t.Fatalf("expected system prompt excerpt in generate/text prompt, got %q", promptUsed)
	}
	if got := tab.main.greeter.titleLabel.Label(); got != "What is diamond building today?" {
		t.Fatalf("personalized greeter title = %q, want 'What is diamond building today?'", got)
	}

	// Calling refreshGreeterProfileAsync again with the same system prompt hash must be a no-op (cached).
	win.refreshGreeterProfileAsync()
	mu.Lock()
	callsAfter := genCalls
	mu.Unlock()
	if callsAfter != 1 {
		t.Fatalf("expected cached greeter profile to skip second LLM call, got %d calls", callsAfter)
	}

	// Verify greeter hides as soon as conversation becomes active or has messages, and reappears if cleared.
	tab.main.active = true
	tab.main.keepPendingAtBottom()
	if tab.main.greeter.Visible() {
		t.Fatalf("expected greeter to hide when conversation is active")
	}
	tab.main.active = false
	tab.main.messages = []opencode.Message{{ID: "m1", Type: "user", Text: "hi"}}
	tab.main.keepPendingAtBottom()
	if tab.main.greeter.Visible() {
		t.Fatalf("expected greeter to stay hidden when conversation has messages")
	}
	tab.main.messages = nil
	tab.main.keepPendingAtBottom()
	if !tab.main.greeter.Visible() {
		t.Fatalf("expected greeter to reappear when conversation is empty again")
	}

	// Verify personalized greeter profile persists across restarts.
	win2 := &Window{
		ctx:       t.Context(),
		statePath: stateFile,
		viewStack: adw.NewViewStack(),
		tabs:      make(map[string]*sessionTab),
	}
	win2.loadState()
	if win2.greeterProfile == nil || win2.greeterProfile.UserName != "diamond" || win2.greeterProfile.Title != "What is diamond building today?" {
		t.Fatalf("expected persisted greeterProfile after reload, got %+v", win2.greeterProfile)
	}
}

func TestEmptyWindowGreeterSessionPersistenceAndOfflineScreen(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}

	stateFile := filepath.Join(t.TempDir(), "gluppencode.json")
	rootContent := adw.NewOverlaySplitView()
	contentMainBox := gtk.NewBox(gtk.OrientationVertical, 0)
	viewStackOverlay := gtk.NewOverlay()
	viewStack := adw.NewViewStack()
	viewStackOverlay.SetChild(viewStack)
	contentMainBox.Append(viewStackOverlay)

	win := &Window{
		ctx:              t.Context(),
		statePath:        stateFile,
		rootContent:      rootContent,
		contentMainBox:   contentMainBox,
		viewStackOverlay: viewStackOverlay,
		viewStack:        viewStack,
		tabs:             make(map[string]*sessionTab),
		childToRoot:      make(map[string]string),
		projectOrder:     []string{"/home/diamond/proj"},
	}
	win.emptyGreeter = NewGreeterWidget(win.greeterProfile, win.populateComposerSuggestion)
	win.contentMainBox.Prepend(win.emptyGreeter)
	win.initOfflineScreen()
	win.syncEmptyGreeterVisibility()

	// 1. When 0 sessions are open, emptyGreeter is visible and viewStackOverlay / viewStack are hidden.
	if !win.emptyGreeter.Visible() {
		t.Fatalf("expected emptyGreeter to be visible when no session is selected")
	}
	if win.viewStackOverlay.Visible() || win.viewStack.Visible() {
		t.Fatalf("expected viewStackOverlay and viewStack to be hidden when no session is selected")
	}

	// 2. Opening a session in an already-known project directory persists visibleSessions to disk
	// and hides emptyGreeter while showing viewStackOverlay / viewStack.
	sess := opencode.Session{
		ID:       "ses_persisted_1",
		Title:    "Persisted Session",
		Location: opencode.Location{Directory: "/home/diamond/proj"},
		Time:     opencode.SessionTime{Updated: 1000},
	}
	win.ensureSessionTab(sess)
	win.viewStack.SetVisibleChildName(sess.ID)
	win.onVisibleTabChanged()

	if win.emptyGreeter.Visible() {
		t.Fatalf("expected emptyGreeter to hide once a session tab is selected")
	}
	if !win.viewStackOverlay.Visible() || !win.viewStack.Visible() {
		t.Fatalf("expected viewStackOverlay and viewStack to be visible once a session tab is selected")
	}

	reloaded := loadPersistedState(stateFile)
	if !reloaded.VisibleSessions["ses_persisted_1"] {
		t.Fatalf("expected ses_persisted_1 to be persisted in visibleSessions even when projectDir was already in projectOrder, got %+v", reloaded.VisibleSessions)
	}

	// 3. Offline error screen transitions windowStack to "offline" and back to "main" on recovery.
	win.client = &opencode.Client{BaseURL: "http://127.0.0.1:4096"}
	if got := win.windowStack.VisibleChildName(); got != "main" {
		t.Fatalf("initial windowStack visible child = %q, want main", got)
	}
	win.setOfflineError(errors.New("dial tcp 127.0.0.1:4096: connect: connection refused"))
	if !win.offline {
		t.Fatalf("expected win.offline = true after setOfflineError")
	}
	if got := win.windowStack.VisibleChildName(); got != "offline" {
		t.Fatalf("windowStack visible child = %q, want offline", got)
	}
	if !strings.Contains(win.offlineStatus.Description(), "http://127.0.0.1:4096") {
		t.Fatalf("expected offlineStatus description to include server URL, got %q", win.offlineStatus.Description())
	}
	if !strings.Contains(win.offlineErrorLabel.Label(), "connection refused") || !win.offlineErrorLabel.Visible() {
		t.Fatalf("expected offlineErrorLabel to be visible with connection error, got %q (visible=%v)", win.offlineErrorLabel.Label(), win.offlineErrorLabel.Visible())
	}

	win.setOfflineError(nil)
	if win.offline {
		t.Fatalf("expected win.offline = false after clearing error")
	}
	if got := win.windowStack.VisibleChildName(); got != "main" {
		t.Fatalf("windowStack visible child after recovery = %q, want main", got)
	}

	// 4. Session search marks agent-active sessions (direct busy, activeMap, running child subagent),
	// while background-shell-only and pending-input tabs do NOT show loading spinners.
	win.activeShells = map[string]opencode.ShellItem{
		"sh_bg_1": {ID: "sh_bg_1", SessionID: "ses_shell_active", Status: "running"},
	}
	win.tabs["ses_pending_input"] = &sessionTab{
		main: &convTimeline{
			session:      opencode.Session{ID: "ses_pending_input"},
			active:       true,
			pendingForms: []opencode.FormInfo{{ID: "frm_srch_1", SessionID: "ses_pending_input"}},
		},
	}
	searchPool := []opencode.Session{
		{ID: "ses_idle", Title: "Idle Session", Time: opencode.SessionTime{Updated: 5000}},
		{ID: "ses_map_active", Title: "Active Map Session", Time: opencode.SessionTime{Updated: 4000}},
		{ID: "ses_parent_active", Title: "Parent With Busy Child", Time: opencode.SessionTime{Updated: 3000}},
		{ID: "ses_child_1", ParentID: "ses_parent_active", Time: opencode.SessionTime{Busy: 500, Idle: 100}},
		{ID: "ses_shell_active", Title: "Shell Active Session", Time: opencode.SessionTime{Updated: 2000}},
		{ID: "ses_pending_input", Title: "Pending Input Session", Time: opencode.SessionTime{Updated: 1000}},
	}
	matches := win.matchAndSortSessionsWithActive(searchPool, "", map[string]opencode.ActiveStatus{
		"ses_map_active":    {Type: "busy"},
		"ses_pending_input": {Type: "busy"},
	})
	if len(matches) != 5 {
		t.Fatalf("len(matches) = %d, want 5 root sessions", len(matches))
	}
	wantActive := map[string]bool{
		"ses_idle":          false,
		"ses_map_active":    true,
		"ses_parent_active": true,
		"ses_shell_active":  false,
		"ses_pending_input": false,
	}
	for _, m := range matches {
		if m.active != wantActive[m.sess.ID] {
			t.Errorf("session %s active = %v, want %v", m.sess.ID, m.active, wantActive[m.sess.ID])
		}
	}

	// 5. Window manager title formats session title and attention count with an en-dash.
	if got := formatWindowManagerTitle("", 0); got != "gluppencode" {
		t.Errorf("formatWindowManagerTitle(\"\", 0) = %q, want %q", got, "gluppencode")
	}
	if got := formatWindowManagerTitle("GTK 4 frontend for Opencode in gotk4", 0); got != "GTK 4 frontend for Opencode in gotk4 – gluppencode" {
		t.Errorf("formatWindowManagerTitle(title, 0) = %q, want %q", got, "GTK 4 frontend for Opencode in gotk4 – gluppencode")
	}
	if got := formatWindowManagerTitle("GTK 4 frontend for Opencode in gotk4", 3); got != "(3) GTK 4 frontend for Opencode in gotk4 – gluppencode" {
		t.Errorf("formatWindowManagerTitle(title, 3) = %q, want %q", got, "(3) GTK 4 frontend for Opencode in gotk4 – gluppencode")
	}
	if got := formatWindowManagerTitle("", 2); got != "(2) gluppencode" {
		t.Errorf("formatWindowManagerTitle(\"\", 2) = %q, want %q", got, "(2) gluppencode")
	}
}

func TestArchiveSessionAndProjectDoesNotStopRunningSession(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}

	var (
		mu             sync.Mutex
		interruptCalls int
		repliedForms   = make(map[string]map[string]any)
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/interrupt"):
			mu.Lock()
			interruptCalls++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
		case strings.Contains(r.URL.Path, "/form/") && strings.HasSuffix(r.URL.Path, "/reply"):
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			formID := parts[len(parts)-2]
			var body struct {
				Answer map[string]any `json:"answer"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			repliedForms[formID] = body.Answer
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	stateFile := filepath.Join(t.TempDir(), "gluppencode.json")
	vs := adw.NewViewStack()
	win := &Window{
		ctx:              t.Context(),
		statePath:        stateFile,
		client:           &opencode.Client{BaseURL: srv.URL},
		viewStack:        vs,
		tabs:             make(map[string]*sessionTab),
		childToRoot:      make(map[string]string),
		visibleSessions:  make(map[string]bool),
		archivedSessions: make(map[string]bool),
		projectSessions:  make(map[string][]opencode.Session),
		projectOrder:     []string{"/home/diamond/proj1", "/home/diamond/proj2"},
		takeCharge:       true,
	}

	s1 := opencode.Session{
		ID:       "ses_active_1",
		Title:    "Running Task 1",
		Location: opencode.Location{Directory: "/home/diamond/proj1"},
		Time:     opencode.SessionTime{Updated: 1000, Busy: 1000, Idle: 500},
	}
	s2 := opencode.Session{
		ID:       "ses_active_2",
		Title:    "Running Task 2",
		Location: opencode.Location{Directory: "/home/diamond/proj2"},
		Time:     opencode.SessionTime{Updated: 2000, Busy: 2000, Idle: 500},
	}
	win.allSessions = []opencode.Session{s1, s2}
	win.projectSessions["/home/diamond/proj1"] = []opencode.Session{s1}
	win.projectSessions["/home/diamond/proj2"] = []opencode.Session{s2}
	win.ensureSessionTab(s1)
	win.ensureSessionTab(s2)
	win.setConvActive("ses_active_1", true)
	win.setConvActive("ses_active_2", true)

	// 1. Archive individual running session ("x" on chat row).
	win.archiveSession("ses_active_1")
	if !win.archivedSessions["ses_active_1"] {
		t.Fatalf("expected ses_active_1 to be marked archived")
	}
	if _, exists := win.tabs["ses_active_1"]; exists {
		t.Fatalf("expected ses_active_1 tab to be removed from sidebar")
	}

	// 2. Archive entire project with running session ("x" on project header).
	win.clearProjectSessions("/home/diamond/proj2")
	if !win.archivedSessions["ses_active_2"] {
		t.Fatalf("expected ses_active_2 to be marked archived when project is cleared")
	}
	if _, exists := win.tabs["ses_active_2"]; exists {
		t.Fatalf("expected ses_active_2 tab to be removed from sidebar")
	}

	mu.Lock()
	gotInterrupts := interruptCalls
	mu.Unlock()
	if gotInterrupts != 0 {
		t.Fatalf("archiving session or project must NOT interrupt running sessions, got %d interrupt calls", gotInterrupts)
	}

	// 3. Background SSE events for archived sessions must NOT re-open tabs,
	// must keep allSessions activity state updated for Search Sessions,
	// and must still auto-reply to forms when Take Charge is enabled.
	win.handleEvent(opencode.Event{
		Type: "session.status",
		Data: json.RawMessage(`{"sessionID":"ses_active_1","status":{"type":"busy"}}`),
	})
	if _, exists := win.tabs["ses_active_1"]; exists {
		t.Fatalf("expected archived ses_active_1 to stay hidden on session.status busy")
	}
	matches := win.matchAndSortSessionsWithActive(win.allSessions, "Running Task 1", nil)
	if len(matches) != 1 || !matches[0].active {
		t.Fatalf("expected archived ses_active_1 to still appear as active in Search Sessions, got %+v", matches)
	}

	win.handleEvent(opencode.Event{
		Type: "form.created",
		Data: json.RawMessage(`{"form":{"id":"frm_archived","sessionID":"ses_active_1","title":"Confirm","fields":[{"key":"q0","title":"Proceed?","type":"string"}]}}`),
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		glib.MainContextDefault().Iteration(false)
		mu.Lock()
		_, ok := repliedForms["frm_archived"]
		mu.Unlock()
		if ok && !glib.MainContextDefault().Pending() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	ans, ok := repliedForms["frm_archived"]
	mu.Unlock()
	if !ok || ans["q0"] != TakeChargeDirective {
		t.Fatalf("expected Take Charge to auto-reply to form in archived session ses_active_1, got %v", ans)
	}

	// 4. When the archived session finishes in the background, its status in allSessions transitions to idle.
	win.handleEvent(opencode.Event{
		Type: "session.idle",
		Data: json.RawMessage(`{"sessionID":"ses_active_1"}`),
	})
	matchesAfter := win.matchAndSortSessionsWithActive(win.allSessions, "Running Task 1", nil)
	if len(matchesAfter) != 1 || matchesAfter[0].active {
		t.Fatalf("expected archived ses_active_1 to become idle in Search Sessions after session.idle, got %+v", matchesAfter)
	}
}

func TestCdCommandAndSessionMoved(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}

	// 1. Test builtinSlashCommands includes "/cd".
	hasCd := false
	for _, b := range builtinSlashCommands {
		if b.name == "/cd" {
			hasCd = true
			break
		}
	}
	if !hasCd {
		t.Fatalf("expected builtinSlashCommands to include '/cd', got %+v", builtinSlashCommands)
	}

	// 2. Test resolveTargetDirectory with relative paths, ~, and absolute paths.
	home, _ := os.UserHomeDir()
	if home != "" {
		if got := resolveTargetDirectory("/tmp", "~/subproject"); got != normalizeProjectDir(filepath.Join(home, "subproject")) {
			t.Fatalf("resolveTargetDirectory('~/subproject') = %q, want %q", got, normalizeProjectDir(filepath.Join(home, "subproject")))
		}
	}
	baseDir := t.TempDir()
	subDir := filepath.Join(baseDir, "nested")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveTargetDirectory(baseDir, "nested"); got != normalizeProjectDir(subDir) {
		t.Fatalf("resolveTargetDirectory(baseDir, 'nested') = %q, want %q", got, normalizeProjectDir(subDir))
	}

	// 3. Test /cd moving an active session via API and updating project grouping on session.moved.
	var (
		mu       sync.Mutex
		movedID  string
		movedDir string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/move") {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			sessID := parts[len(parts)-2]
			var body struct {
				Directory string `json:"directory"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			movedID = sessID
			movedDir = body.Directory
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	dirA := normalizeProjectDir(baseDir)
	dirB := normalizeProjectDir(subDir)
	vs := adw.NewViewStack()
	win := &Window{
		ctx:             t.Context(),
		client:          &opencode.Client{BaseURL: srv.URL},
		viewStack:       vs,
		tabs:            make(map[string]*sessionTab),
		childToRoot:     make(map[string]string),
		visibleSessions: make(map[string]bool),
		projectSessions: make(map[string][]opencode.Session),
		projectOrder:    []string{dirA},
	}

	sess := opencode.Session{
		ID:       "ses_move_1",
		Title:    "Movable Session",
		Location: opencode.Location{Directory: dirA},
		Time:     opencode.SessionTime{Updated: 1000},
	}
	win.allSessions = []opencode.Session{sess}
	win.projectSessions[dirA] = []opencode.Session{sess}
	tab := win.ensureSessionTab(sess)
	vs.SetVisibleChildName(sess.ID)

	win.changeSessionDirectory(tab, tab.main, "nested")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		glib.MainContextDefault().Iteration(false)
		mu.Lock()
		done := movedID != ""
		mu.Unlock()
		if done && tab.main.session.Location.Directory == dirB && !glib.MainContextDefault().Pending() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	gotID, gotDir := movedID, movedDir
	mu.Unlock()
	if gotID != "ses_move_1" || gotDir != dirB {
		t.Fatalf("expected MoveSession('ses_move_1', %q), got (%q, %q)", dirB, gotID, gotDir)
	}
	if tab.main.session.Location.Directory != dirB {
		t.Fatalf("tab session directory = %q, want %q", tab.main.session.Location.Directory, dirB)
	}
	if len(win.projectSessions[dirA]) != 0 {
		t.Fatalf("expected dirA projectSessions to be empty after move, got %+v", win.projectSessions[dirA])
	}
	if len(win.projectSessions[dirB]) != 1 || win.projectSessions[dirB][0].ID != "ses_move_1" {
		t.Fatalf("expected dirB projectSessions to contain ses_move_1, got %+v", win.projectSessions[dirB])
	}
}

func TestPreferencesPersistenceAndLiveApply(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}

	stateFile := filepath.Join(t.TempDir(), "gluppencode.json")
	vs := adw.NewViewStack()
	sidebarList := gtk.NewListBox()
	composerClamp := adw.NewClamp()
	sendBtn := gtk.NewButton()

	win := &Window{
		ctx:               t.Context(),
		statePath:         stateFile,
		sidebarList:       sidebarList,
		viewStack:         vs,
		composerClamp:     composerClamp,
		sendBtn:           sendBtn,
		tabs:              make(map[string]*sessionTab),
		childToRoot:       make(map[string]string),
		visibleSessions:   make(map[string]bool),
		collapsedProjects: make(map[string]bool),
		expandedProjects:  make(map[string]bool),
		projectSessions:   make(map[string][]opencode.Session),
		shellOutputs:      make(map[string]string),
		greeterProfile: &GreeterProfile{
			Title:    "Generated Title",
			Subtitle: "Generated Subtitle",
			Model:    "glm-5.3-flash",
		},
		prefs: Preferences{
			ColorScheme:          "dark",
			ChatWidth:            1240,
			MaxVisibleSessions:   3,
			ShellTailLines:       5,
			DiffTruncationLines:  25,
			CtrlEnterToSend:      true,
			TakeChargeDirective:  "Custom take charge reply",
			GreeterTitleOverride: "Custom Greeting Headline",
			GreeterSubOverride:   "Custom Greeting Subtext",
			GreeterModelOverride: "google/antigravity-gemini-3.5-flash-lite",
			ServerURL:            "http://127.0.0.1:4199/",
		},
	}
	win.ctx = WithDiffMaxLines(win.ctx, win.effectiveDiffTruncationLines)
	win.emptyGreeter = NewGreeterWidget(win.effectiveGreeterProfile(), nil)
	win.saveState()

	// 1. Verify persistence round-trip.
	reloaded := &Window{
		ctx:       t.Context(),
		statePath: stateFile,
		viewStack: adw.NewViewStack(),
		tabs:      make(map[string]*sessionTab),
	}
	reloaded.loadState()
	if reloaded.prefs != win.prefs {
		t.Fatalf("reloaded prefs = %+v, want %+v", reloaded.prefs, win.prefs)
	}

	// 2. Verify live-apply on chat width, send shortcut, and greeter overrides.
	dir := "/home/diamond/proj"
	win.projectOrder = []string{dir}
	for i := 1; i <= 5; i++ {
		s := opencode.Session{
			ID:       fmt.Sprintf("ses_%d", i),
			Title:    fmt.Sprintf("Session %d", i),
			Location: opencode.Location{Directory: dir},
			Time:     opencode.SessionTime{Updated: int64(6000 - i*100)},
		}
		win.projectSessions[dir] = append(win.projectSessions[dir], s)
		if i <= 3 {
			win.ensureSessionTab(s)
		}
	}
	win.applyAllPreferences()

	if got := composerClamp.MaximumSize(); got != 1240 {
		t.Fatalf("composerClamp.MaximumSize() = %d, want 1240", got)
	}
	if got := win.tabs["ses_1"].main.clamp.MaximumSize(); got != 1240 {
		t.Fatalf("conv.clamp.MaximumSize() = %d, want 1240", got)
	}
	if got := sendBtn.TooltipText(); got != "Send Message (Ctrl+Enter)" {
		t.Fatalf("sendBtn.TooltipText() = %q, want 'Send Message (Ctrl+Enter)'", got)
	}
	if got := win.emptyGreeter.titleLabel.Label(); got != "Custom Greeting Headline" {
		t.Fatalf("emptyGreeter title = %q, want 'Custom Greeting Headline'", got)
	}

	// 3. Increasing MaxVisibleSessions from 3 to 4 dynamically promotes the 4th session into a tab.
	win.prefs.MaxVisibleSessions = 4
	win.applyMaxVisibleSessions()
	if _, exists := win.tabs["ses_4"]; !exists {
		t.Fatalf("expected ses_4 to be promoted when MaxVisibleSessions increased to 4")
	}

	// 4. Decreasing MaxVisibleSessions from 4 to 2 hides tabs beyond the first 2 in the sidebar
	// while keeping their viewStack pages visible so active sessions stay selectable.
	win.prefs.MaxVisibleSessions = 2
	win.applyMaxVisibleSessions()
	for _, id := range []string{"ses_1", "ses_2", "ses_3", "ses_4"} {
		if !win.tabs[id].page.Visible() {
			t.Fatalf("expected %s viewStack page to remain visible", id)
		}
	}
	for _, id := range []string{"ses_1", "ses_2"} {
		ind := win.tabs[id].sidebarIndicator
		if ind == nil || ind.row == nil || ind.row.Parent() == nil {
			t.Fatalf("expected %s sidebar row to remain attached in sidebarList", id)
		}
	}
	for _, id := range []string{"ses_3", "ses_4"} {
		ind := win.tabs[id].sidebarIndicator
		if ind != nil && ind.row != nil && ind.row.Parent() != nil {
			t.Fatalf("expected %s sidebar row to be detached from sidebarList when MaxVisibleSessions=2", id)
		}
	}
	showMoreRow := win.projectShowMoreRows[dir]
	showMoreLbl := win.projectShowMoreLbls[dir]
	if showMoreRow == nil || showMoreRow.Parent() == nil || showMoreLbl == nil || showMoreLbl.Text() != "Show 3 more…" {
		t.Fatalf("expected attached 'Show 3 more…' row for %s, got row=%v lbl=%v", dir, showMoreRow, showMoreLbl)
	}

	// 5. Verify Take Charge custom directive and ToolWidget diff truncation limit.
	form := opencode.FormInfo{
		ID:        "f1",
		SessionID: "ses_1",
		Fields:    []opencode.FormField{{Key: "q0", Type: "string"}},
	}
	ans := buildTakeChargeAnswerWithDirective(form, win.effectiveTakeChargeDirective())
	if ans["q0"] != "Custom take charge reply" {
		t.Fatalf("buildTakeChargeAnswerWithDirective = %v, want 'Custom take charge reply'", ans)
	}

	var diffLines []string
	for i := 1; i <= 40; i++ {
		diffLines = append(diffLines, fmt.Sprintf("+line %d", i))
	}
	tw := &ToolWidget{
		Box:          gtk.NewBox(gtk.OrientationVertical, 0),
		ctx:          win.ctx,
		diffBox:      gtk.NewBox(gtk.OrientationVertical, 0),
		diffMaxLines: diffMaxLinesFromContext(win.ctx),
	}
	tw.renderInlineDiff(strings.Join(diffLines, "\n"))
	// 25 lines + 1 "... (15 more lines)" label = 26 diffWidgets.
	if len(tw.diffWidgets) != 26 {
		t.Fatalf("len(tw.diffWidgets) with limit 25 = %d, want 26", len(tw.diffWidgets))
	}
	tw.SetDiffMaxLines(30)
	if len(tw.diffWidgets) != 31 {
		t.Fatalf("len(tw.diffWidgets) after SetDiffMaxLines(30) = %d, want 31", len(tw.diffWidgets))
	}
}

func TestRevertUserMessageCountAndFileRollbackBanner(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}

	messages := []opencode.Message{
		{ID: "u1", Type: "user", Text: "first prompt"},
		{ID: "a1", Type: "assistant", Text: "first reply"},
		{ID: "u2", Type: "user", Text: "second prompt"},
		{
			ID:   "a2_1",
			Type: "assistant",
			Content: []opencode.ContentPart{
				{
					Type:  "tool",
					Name:  "edit",
					State: opencode.ToolState{Input: json.RawMessage(`{"filePath":"internal/window/window.go","oldString":"old1\n","newString":"new1\nnew2\n"}`)},
				},
			},
		},
		{ID: "a2_2", Type: "assistant", Text: "step 2"},
		{
			ID:   "a2_3",
			Type: "assistant",
			Content: []opencode.ContentPart{
				{
					Type:  "tool",
					Name:  "write",
					State: opencode.ToolState{Input: json.RawMessage(`{"filePath":"internal/window/preferences.go","content":"package window\n"}`)},
				},
			},
		},
		{ID: "a2_4", Type: "assistant", Text: "done"},
	}

	// 1. Reverting the latest prompt (u2) hides 5 messages [u2, a2_1, a2_2, a2_3, a2_4],
	// but countRevertedUserMessages must only count user messages (1).
	_, hidden := computeRevertVisibility(messages, "u2")
	if len(hidden) != 5 {
		t.Fatalf("len(hidden) = %d, want 5", len(hidden))
	}
	if got := countRevertedUserMessages(messages, "u2"); got != 1 {
		t.Fatalf("countRevertedUserMessages(u2) = %d, want 1", got)
	}
	if got := countRevertedUserMessages(messages, "u1"); got != 2 {
		t.Fatalf("countRevertedUserMessages(u1) = %d, want 2", got)
	}

	// 2. collectRevertedFileChanges extracts modified files from assistant tool calls at/after u2.
	fallbackFiles := collectRevertedFileChanges(messages, "u2")
	if len(fallbackFiles) != 2 {
		t.Fatalf("len(fallbackFiles) = %d, want 2: %+v", len(fallbackFiles), fallbackFiles)
	}
	if fallbackFiles[0].File != "internal/window/window.go" || fallbackFiles[0].Status != "modified" {
		t.Fatalf("fallbackFiles[0] = %+v, want internal/window/window.go (modified)", fallbackFiles[0])
	}
	if fallbackFiles[1].File != "internal/window/preferences.go" || fallbackFiles[1].Status != "added" {
		t.Fatalf("fallbackFiles[1] = %+v, want internal/window/preferences.go (added)", fallbackFiles[1])
	}

	// 3. StageRevert with files=true vs files=false updates RevertBannerWidget with user message count and rolled-back files.
	var (
		mu          sync.Mutex
		gotFilesArg bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/revert/stage") {
			var body struct {
				MessageID string `json:"messageID"`
				Files     bool   `json:"files"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			gotFilesArg = body.Files
			mu.Unlock()
			if body.Files {
				_, _ = w.Write([]byte(`{"data":{"messageID":"u2","snapshot":"snap_2","files":[{"file":"internal/window/window.go","patch":"@@ -1 +1,2 @@\n-old1\n+new1\n+new2","additions":2,"deletions":1,"status":"modified"},{"file":"internal/window/preferences.go","patch":"@@ -0,0 +1 @@\n+package window","additions":1,"deletions":0,"status":"added"}]}}`))
			} else {
				_, _ = w.Write([]byte(`{"data":{"messageID":"u2"}}`))
			}
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	win := &Window{
		ctx:         t.Context(),
		client:      &opencode.Client{BaseURL: srv.URL},
		viewStack:   adw.NewViewStack(),
		tabs:        make(map[string]*sessionTab),
		childToRoot: make(map[string]string),
	}
	tab := win.addSessionTab(opencode.Session{ID: "ses_rev"})
	conv := tab.main
	conv.messages = messages

	// Revert with files rollback = true.
	win.stageRevertToMessage(tab, conv, "u2", "second prompt", true)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		glib.MainContextDefault().Iteration(false)
		if conv.session.Revert != nil && !glib.MainContextDefault().Pending() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	filesTrue := gotFilesArg
	mu.Unlock()
	if !filesTrue {
		t.Fatalf("expected files=true sent to /revert/stage")
	}
	if conv.revertBanner == nil || !conv.revertBanner.Visible() {
		t.Fatalf("expected revertBanner to be visible")
	}
	if got := conv.revertBanner.titleLabel.Text(); got != "1 reverted message" {
		t.Fatalf("revertBanner title = %q, want '1 reverted message'", got)
	}
	if !conv.revertBanner.filesBox.Visible() || len(conv.revertBanner.fileWidgets) != 2 {
		t.Fatalf("expected 2 rolled-back file rows visible in revertBanner, got visible=%v len=%d", conv.revertBanner.filesBox.Visible(), len(conv.revertBanner.fileWidgets))
	}
	if got := conv.revertBanner.diffBadge.Text(); got != "+3 -1" || !conv.revertBanner.diffBadge.Visible() {
		t.Fatalf("revertBanner diffBadge = %q (visible=%v), want '+3 -1'", got, conv.revertBanner.diffBadge.Visible())
	}

	// Revert with files rollback = false -> no files listed in banner.
	conv.session.Revert = nil
	win.stageRevertToMessage(tab, conv, "u2", "second prompt", false)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		glib.MainContextDefault().Iteration(false)
		if conv.session.Revert != nil && !glib.MainContextDefault().Pending() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	filesFalse := gotFilesArg
	mu.Unlock()
	if filesFalse {
		t.Fatalf("expected files=false sent to /revert/stage")
	}
	if conv.revertBanner.filesBox.Visible() || len(conv.revertBanner.fileWidgets) != 0 {
		t.Fatalf("expected filesBox hidden when files=false, got visible=%v len=%d", conv.revertBanner.filesBox.Visible(), len(conv.revertBanner.fileWidgets))
	}
}

func TestPermissionPromptWidgetAndTakeCharge(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}

	var (
		mu              sync.Mutex
		repliedDecision = make(map[string]string)
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/permission/") && strings.HasSuffix(r.URL.Path, "/reply") {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			reqID := parts[len(parts)-2]
			var body struct {
				Decision string `json:"decision"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			repliedDecision[reqID] = body.Decision
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	vs := adw.NewViewStack()
	win := &Window{
		ctx:              t.Context(),
		client:           &opencode.Client{BaseURL: srv.URL},
		viewStack:        vs,
		tabs:             make(map[string]*sessionTab),
		childToRoot:      make(map[string]string),
		visibleSessions:  make(map[string]bool),
		archivedSessions: make(map[string]bool),
		projectSessions:  make(map[string][]opencode.Session),
	}

	tab1 := win.addSessionTab(opencode.Session{
		ID:       "ses_perm_1",
		Title:    "Active Session",
		Location: opencode.Location{Directory: "/home/diamond/proj"},
	})
	tab1.main.loaded = true
	tab1.main.active = true
	tab2 := win.addSessionTab(opencode.Session{
		ID:       "ses_perm_2",
		Title:    "Background Session",
		Location: opencode.Location{Directory: "/home/diamond/proj"},
	})
	tab2.main.loaded = true
	tab2.main.active = true
	vs.SetVisibleChildName("ses_perm_1")

	// 1. Emit permission.asked on background session ses_perm_2 -> marks NeedsAttention, hides thinkingLabel, creates PermissionPromptWidget.
	win.handleEvent(opencode.Event{
		Type: "permission.asked",
		Data: json.RawMessage(`{
			"id": "per_bg_1",
			"sessionID": "ses_perm_2",
			"action": "bash",
			"resources": ["run0 systemctl restart opencode.service"],
			"save": ["run0 systemctl restart *"],
			"message": "Allow Shell tool to execute via run0?"
		}`),
	})

	if !tab2.NeedsAttention() {
		t.Fatalf("expected tab2.NeedsAttention() = true when permission is asked on background tab")
	}
	if len(tab2.main.pendingPermissions) != 1 {
		t.Fatalf("expected 1 pending permission in tab2, got %d", len(tab2.main.pendingPermissions))
	}
	if tab2.main.thinkingLabel.Visible() {
		t.Fatalf("expected thinkingLabel to be hidden while permission prompt is pending")
	}
	pw := tab2.main.permissionWidgets["per_bg_1"]
	if pw == nil {
		t.Fatalf("expected PermissionPromptWidget to be created for per_bg_1")
	}
	if got := pw.pillLabel.Text(); got != "Permission Required · Shell Command" {
		t.Errorf("pw.pillLabel.Text() = %q, want %q", got, "Permission Required · Shell Command")
	}
	if got := pw.titleLabel.Text(); got != "Allow Shell tool to execute via run0?" {
		t.Errorf("pw.titleLabel.Text() = %q, want %q", got, "Allow Shell tool to execute via run0?")
	}
	if got := pw.cmdLabel.Text(); got != "$ run0 systemctl restart opencode.service" {
		t.Errorf("pw.cmdLabel.Text() = %q, want %q", got, "$ run0 systemctl restart opencode.service")
	}
	if got := pw.footerLbl.Text(); got != "Always Allow saves: run0 systemctl restart *" {
		t.Errorf("pw.footerLbl.Text() = %q, want %q", got, "Always Allow saves: run0 systemctl restart *")
	}

	// 2. Clicking "Always Allow" sends decision="always" and removes the card.
	pw.TriggerDecision(opencode.PermissionAlways)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		glib.MainContextDefault().Iteration(false)
		if len(tab2.main.pendingPermissions) == 0 && !glib.MainContextDefault().Pending() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	gotDec := repliedDecision["per_bg_1"]
	mu.Unlock()
	if gotDec != "always" {
		t.Fatalf("repliedDecision[per_bg_1] = %q, want 'always'", gotDec)
	}
	if _, exists := tab2.main.permissionWidgets["per_bg_1"]; exists {
		t.Fatalf("expected permission widget per_bg_1 to be removed after reply")
	}
	if !tab2.main.thinkingLabel.Visible() {
		t.Fatalf("expected thinkingLabel to become visible again after permission is resolved")
	}

	// 3. Emit permission.asked and resolve via permission.replied SSE event.
	win.handleEvent(opencode.Event{
		Type: "permission.asked",
		Data: json.RawMessage(`{
			"id": "per_sse_2",
			"sessionID": "ses_perm_1",
			"action": "external_directory",
			"resources": ["/tmp/opencode/scratch"]
		}`),
	})
	if len(tab1.main.pendingPermissions) != 1 || tab1.main.permissionWidgets["per_sse_2"] == nil {
		t.Fatalf("expected per_sse_2 widget to be present on tab1")
	}
	win.handleEvent(opencode.Event{
		Type: "permission.replied",
		Data: json.RawMessage(`{"sessionID":"ses_perm_1","requestID":"per_sse_2","reply":"reject"}`),
	})
	if len(tab1.main.pendingPermissions) != 0 || tab1.main.permissionWidgets["per_sse_2"] != nil {
		t.Fatalf("expected per_sse_2 to be removed on permission.replied SSE event")
	}

	// 4. Enabling Take Charge auto-allows pending permissions with decision="once".
	win.handleEvent(opencode.Event{
		Type: "permission.asked",
		Data: json.RawMessage(`{
			"id": "per_tc_3",
			"sessionID": "ses_perm_1",
			"action": "edit",
			"resources": ["internal/window/window.go"]
		}`),
	})
	if len(tab1.main.pendingPermissions) != 1 {
		t.Fatalf("expected per_tc_3 to be pending before Take Charge")
	}
	win.setTakeCharge(true)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		glib.MainContextDefault().Iteration(false)
		if len(tab1.main.pendingPermissions) == 0 && !glib.MainContextDefault().Pending() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	tcDec := repliedDecision["per_tc_3"]
	mu.Unlock()
	if tcDec != "once" {
		t.Fatalf("expected Take Charge to auto-reply with decision='once', got %q", tcDec)
	}
}

func TestProjectHeaderPlusButtonAndContextMenu(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}

	var (
		mu             sync.Mutex
		createdDirs    []string
		sessionCounter int
		openedDir      string
		copiedPath     string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/session" {
			var body opencode.CreateSessionRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			reqDir := body.Directory
			if reqDir == "" && body.Location != nil {
				reqDir = body.Location.Directory
			}
			mu.Lock()
			sessionCounter++
			id := fmt.Sprintf("ses_new_%d", sessionCounter)
			createdDirs = append(createdDirs, reqDir)
			mu.Unlock()
			resp := opencode.Session{
				ID:       id,
				Title:    "New Session",
				Location: opencode.Location{Directory: reqDir},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	dir := "/home/diamond/proj_header"
	vs := adw.NewViewStack()
	win := &Window{
		ctx:               t.Context(),
		client:            &opencode.Client{BaseURL: srv.URL},
		viewStack:         vs,
		tabs:              make(map[string]*sessionTab),
		childToRoot:       make(map[string]string),
		visibleSessions:   make(map[string]bool),
		archivedSessions:  make(map[string]bool),
		collapsedProjects: make(map[string]bool),
		projectSessions:   make(map[string][]opencode.Session),
		projectOrder:      []string{dir},
		fileOpener: func(d string) {
			openedDir = d
		},
		clipboardSetter: func(s string) {
			copiedPath = s
		},
	}

	s1 := opencode.Session{
		ID:       "ses_existing_1",
		Title:    "Existing Session",
		Location: opencode.Location{Directory: dir},
	}
	win.allSessions = []opencode.Session{s1}
	win.projectSessions[dir] = []opencode.Session{s1}
	win.ensureSessionTab(s1)

	hdrBox := win.newProjectHeaderBox(dir, false)
	hw := win.headerWidgets[hdrBox.Object.Native()]
	if hw == nil {
		t.Fatalf("expected projectHeaderWidgets to be registered for hdrBox")
	}
	if got := hw.newBtn.IconName(); got != "list-add-symbolic" {
		t.Fatalf("hw.newBtn.IconName() = %q, want 'list-add-symbolic'", got)
	}
	if got := hw.newBtn.TooltipText(); got != "New Session in Project" {
		t.Fatalf("hw.newBtn.TooltipText() = %q, want 'New Session in Project'", got)
	}

	// 1. Clicking the "+" button on the project header creates a new session in that project's directory.
	hw.onNewSession()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		glib.MainContextDefault().Iteration(false)
		mu.Lock()
		n := len(createdDirs)
		mu.Unlock()
		if n == 1 && win.tabs["ses_new_1"] != nil && !glib.MainContextDefault().Pending() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	gotDirs := slices.Clone(createdDirs)
	mu.Unlock()
	if len(gotDirs) != 1 || gotDirs[0] != dir {
		t.Fatalf("expected newBtn click to create session in %q, got %v", dir, gotDirs)
	}
	if win.tabs["ses_new_1"] == nil {
		t.Fatalf("expected ses_new_1 tab to be created and selected")
	}

	// 2. Popover menu actions: Open in Files, Copy Path, Collapse/Expand, and Archive Project.
	hw.openMenuAt(12, 16)
	hw.onOpenFiles()
	if openedDir != dir {
		t.Fatalf("openedDir = %q, want %q", openedDir, dir)
	}

	hw.onCopyPath()
	if copiedPath != dir {
		t.Fatalf("copiedPath = %q, want %q", copiedPath, dir)
	}

	if got := hw.toggleLbl.Text(); got != "Collapse Project" {
		t.Fatalf("hw.toggleLbl.Text() = %q, want 'Collapse Project'", got)
	}
	hw.onToggle()
	if !win.collapsedProjects[dir] {
		t.Fatalf("expected project %q to be collapsed after clicking Collapse Project", dir)
	}
	win.updateProjectHeaderLabels(hdrBox, dir)
	if got := hw.toggleLbl.Text(); got != "Expand Project" {
		t.Fatalf("hw.toggleLbl.Text() after collapse = %q, want 'Expand Project'", got)
	}

	hw.onArchive()
	if !win.archivedSessions["ses_existing_1"] || !win.archivedSessions["ses_new_1"] {
		t.Fatalf("expected Archive Project in popover menu to archive all project sessions, got %+v", win.archivedSessions)
	}

	// 3. App menu button setup attaches a popover menu, window.ui keeps toggle buttons hidden initially
	// and removes duplicate top-bar search/menu buttons, and syncNavigationChrome only shows sidebar
	// toggle buttons when collapsed.
	b := gtk.NewBuilderFromString(windowUI)
	uiHideBtn := b.GetObject("hideSidebarBtn").Cast().(*gtk.Button)
	uiSearchBtn := b.GetObject("searchSessionsBtn").Cast().(*gtk.Button)
	uiMenuBtn := b.GetObject("appMenuSidebarBtn").Cast().(*gtk.MenuButton)
	uiShowBtn := b.GetObject("showSidebarBtn").Cast().(*gtk.Button)
	if uiSearchBtn == nil || uiMenuBtn == nil {
		t.Fatalf("expected searchSessionsBtn and appMenuSidebarBtn in sidebarHeader")
	}
	if uiHideBtn.Visible() || uiShowBtn.Visible() {
		t.Fatalf("expected hideSidebarBtn and showSidebarBtn to start hidden in window.ui")
	}
	if b.GetObject("appMenuTopBtn") != nil || b.GetObject("searchSessionsTopBtn") != nil {
		t.Fatalf("expected duplicate appMenuTopBtn and searchSessionsTopBtn to be removed from contentHeader")
	}

	menuBtn := gtk.NewMenuButton()
	win.setupAppMenuButton(menuBtn)
	if menuBtn.Popover() == nil {
		t.Fatalf("expected setupAppMenuButton to attach a popover to menuBtn")
	}
	win.rootContent = adw.NewOverlaySplitView()
	win.hideSidebarBtn = gtk.NewButton()
	win.showSidebarBtn = gtk.NewButton()
	win.rootContent.SetCollapsed(false)
	win.syncNavigationChrome()
	if win.hideSidebarBtn.Visible() || win.showSidebarBtn.Visible() {
		t.Fatalf("expected hideSidebarBtn and showSidebarBtn to be hidden when uncollapsed")
	}
	if !win.rootContent.ShowSidebar() {
		t.Fatalf("expected sidebar to stay shown when uncollapsed")
	}
	win.rootContent.SetCollapsed(true)
	win.syncNavigationChrome()
	if !win.hideSidebarBtn.Visible() || !win.showSidebarBtn.Visible() {
		t.Fatalf("expected hideSidebarBtn and showSidebarBtn to be visible when collapsed")
	}
	win.rootContent.SetCollapsed(false)
	win.syncNavigationChrome()
	if win.hideSidebarBtn.Visible() || win.showSidebarBtn.Visible() {
		t.Fatalf("expected hideSidebarBtn and showSidebarBtn to be hidden after uncollapsing")
	}

	// 4. Unified search (matchAndSortSearchItems) matches sessions, known projects, and filesystem paths.
	tmpRoot := t.TempDir()
	unvisitedDir := filepath.Join(tmpRoot, "brand_new_repo")
	if err := os.MkdirAll(unvisitedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Searching by project name matches the known project and its sessions.
	projItems := win.matchAndSortSearchItems(win.allSessions, "proj_header", nil)
	hasProjMatch := false
	for _, it := range projItems {
		if it.isProject && it.dir == dir {
			hasProjMatch = true
			break
		}
	}
	if !hasProjMatch {
		t.Fatalf("expected matchAndSortSearchItems('proj_header') to include project match for %q, got %+v", dir, projItems)
	}

	// Searching by filesystem path prefix discovers unvisited directories on disk.
	fsQuery := filepath.Join(tmpRoot, "brand_new")
	fsItems := win.matchAndSortSearchItems(win.allSessions, fsQuery, nil)
	hasFSMatch := false
	for _, it := range fsItems {
		if it.isProject && it.dir == normalizeProjectDir(unvisitedDir) {
			hasFSMatch = true
			break
		}
	}
	if !hasFSMatch {
		t.Fatalf("expected matchAndSortSearchItems(%q) to discover unvisited directory %q, got %+v", fsQuery, unvisitedDir, fsItems)
	}

	// 5. applySidebarWidth pins both min and max sidebar width to the exact pixel width when uncollapsed,
	// and rebuildShellList ellipsizes background shell output labels so long lines cannot inflate bottomSheet.
	win.sidebarWidth = 0
	win.sidebarBreakpoint = adw.NewBreakpoint(adw.BreakpointConditionParse("max-width: 600px"))
	win.applySidebarWidth()
	win.syncSidebarBreakpoint()
	if gotMin, gotMax := win.rootContent.MinSidebarWidth(), win.rootContent.MaxSidebarWidth(); gotMin != defaultSidebarWidth || gotMax != defaultSidebarWidth {
		t.Fatalf("applySidebarWidth() with 0 width: min=%v max=%v, want %v", gotMin, gotMax, defaultSidebarWidth)
	}
	if gotBP := win.sidebarBreakpointWidth(); gotBP != 600 {
		t.Fatalf("sidebarBreakpointWidth() with default width = %d, want 600", gotBP)
	}
	if gotCond := win.sidebarBreakpoint.Condition().String(); gotCond != "max-width: 600px" {
		t.Fatalf("sidebarBreakpoint.Condition() = %q, want %q", gotCond, "max-width: 600px")
	}
	win.setDesktopSidebarWidth(340)
	if gotMin, gotMax := win.rootContent.MinSidebarWidth(), win.rootContent.MaxSidebarWidth(); gotMin != 340 || gotMax != 340 {
		t.Fatalf("setDesktopSidebarWidth(340): min=%v max=%v, want 340", gotMin, gotMax)
	}
	if gotBP := win.sidebarBreakpointWidth(); gotBP != 680 {
		t.Fatalf("sidebarBreakpointWidth() with 340 width = %d, want 680", gotBP)
	}
	if gotCond := win.sidebarBreakpoint.Condition().String(); gotCond != "max-width: 680px" {
		t.Fatalf("sidebarBreakpoint.Condition() = %q, want %q", gotCond, "max-width: 680px")
	}
	delete(win.archivedSessions, "ses_existing_1")
	win.ensureSessionTab(s1)
	win.viewStack.SetVisibleChildName("ses_existing_1")
	win.shellListBox = gtk.NewListBox()
	win.activeShells = map[string]opencode.ShellItem{
		"sh_test_1": {ID: "sh_test_1", SessionID: "ses_existing_1", Command: "nix build", Status: "running"},
	}
	win.shellOutputs = map[string]string{
		"sh_test_1": strings.Repeat("x", 140),
	}
	win.rebuildShellList()
	outLbl := win.shellOutputLabels["sh_test_1"]
	if outLbl == nil || outLbl.Ellipsize() != pango.EllipsizeEnd {
		t.Fatalf("expected shell output label to ellipsize at end, got %v", outLbl)
	}

	// 6. Inline diffs and long subagent titles must keep a compact horizontal minimum width
	// so content and windowStack never overflow narrow windows (e.g. <953px).
	tw := &ToolWidget{
		diffBox: gtk.NewBox(gtk.OrientationVertical, 0),
	}
	tw.renderInlineDiff("+" + strings.Repeat("extremely_long_unbroken_identifier_token_", 10) + "\n-" + strings.Repeat("another_very_long_diff_line_", 10))
	minW, _, _, _ := tw.diffBox.Measure(gtk.OrientationHorizontal, -1)
	if minW > 120 {
		t.Fatalf("diffBox horizontal minimum width = %d px, want <= 120 px", minW)
	}
	subW := NewSyntheticSubagentWidget(t.Context(), opencode.Message{
		ID:    "msg_sub_1",
		Agent: strings.Repeat("very-long-subagent-agent-name-", 6),
		Metadata: map[string]any{
			"description": strings.Repeat("very-long-subagent-description-segment-", 6),
			"childID":     "ses_child_1",
		},
	}, nil)
	subMinW, _, _, _ := subW.Measure(gtk.OrientationHorizontal, -1)
	if subMinW > 200 {
		t.Fatalf("SyntheticSubagentWidget horizontal minimum width = %d px, want <= 200 px", subMinW)
	}

	win.initOfflineScreen()
	if win.windowStack.Hhomogeneous() || win.windowStack.Vhomogeneous() {
		t.Fatalf("expected windowStack hhomogeneous=false and vhomogeneous=false")
	}
}

func TestComputeTurnDurations(t *testing.T) {
	t.Parallel()

	makeAssistant := func(id string, created, completed int64) opencode.Message {
		var m opencode.Message
		m.ID = id
		m.Type = "assistant"
		m.Time.Created = created
		m.Time.Completed = completed
		m.Content = []opencode.ContentPart{{Type: "text", Text: "ok"}}
		return m
	}
	makeNotice := func(id, typ, text string, created int64) opencode.Message {
		var m opencode.Message
		m.ID = id
		m.Type = typ
		m.Text = text
		m.Time.Created = created
		return m
	}

	formatTests := []struct {
		dur  time.Duration
		want string
	}{
		{0, ""},
		{-5 * time.Second, ""},
		{420 * time.Millisecond, "Worked for 420ms"},
		{1000 * time.Millisecond, "Worked for 1s"},
		{2440 * time.Millisecond, "Worked for 2.4s"},
		{9960 * time.Millisecond, "Worked for 10s"},
		{18300 * time.Millisecond, "Worked for 18s"},
		{59600 * time.Millisecond, "Worked for 1m"},
		{83000 * time.Millisecond, "Worked for 1m 23s"},
		{3600 * time.Second, "Worked for 1h"},
		{3660 * time.Second, "Worked for 1h 1m"},
		{3725 * time.Second, "Worked for 1h 2m 5s"},
	}
	for _, tc := range formatTests {
		if got := formatTurnDurationNotice(tc.dur); got != tc.want {
			t.Errorf("formatTurnDurationNotice(%v) = %q, want %q", tc.dur, got, tc.want)
		}
	}

	tests := []struct {
		name     string
		messages []opencode.Message
		revertID string
		active   bool
		want     map[string]time.Duration
	}{
		{
			name: "contiguous multi-step turn with trailing compaction and model switch excluded",
			messages: []opencode.Message{
				makeNotice("u1", "user", "build feature", 1000),
				makeAssistant("a1", 1010, 4010),
				makeAssistant("a2", 4030, 8030),
				makeAssistant("a3", 8050, 11010),
				makeNotice("idle1", "idle", "", 11020),
				makeNotice("c1", "compaction", "", 12000),
				makeNotice("ms1", "model-switched", "", 25000),
			},
			want: map[string]time.Duration{
				"a3": 10 * time.Second,
			},
		},
		{
			name: "mid-turn auto-compaction gap is excluded",
			messages: []opencode.Message{
				makeNotice("u1", "user", "refactor", 1000),
				makeAssistant("a1", 1000, 6000), // 5s
				makeNotice("c1", "compaction", "", 6010),
				makeAssistant("a2", 21000, 25000), // 4s (15s compaction gap excluded)
			},
			want: map[string]time.Duration{
				"a2": 9 * time.Second,
			},
		},
		{
			name: "background subagent or shell wait between steps is included",
			messages: []opencode.Message{
				makeNotice("u1", "user", "run background job", 1000),
				makeAssistant("a1", 1000, 3000), // 2s
				makeNotice("i1", "idle", "", 3010),
				makeNotice("syn1", "synthetic", `<shell id="sh_1" state="completed">done</shell>`, 15000),
				makeAssistant("a2", 15020, 18000), // 12.02s wait + 2.98s step = 15s -> 17s total
			},
			want: map[string]time.Duration{
				"a2": 17 * time.Second,
			},
		},
		{
			name: "server restart synthetic notice gap is excluded",
			messages: []opencode.Message{
				makeNotice("u1", "user", "long task", 1000),
				makeAssistant("a1", 1000, 4000), // 3s
				makeNotice("syn_restart", "synthetic", "Continuing after restart", 604000),
				makeAssistant("a2", 604020, 606020), // 2s
			},
			want: map[string]time.Duration{
				"a2": 5 * time.Second,
			},
		},
		{
			name: "active second turn suppresses tail duration while preserving first turn duration",
			messages: []opencode.Message{
				makeNotice("u1", "user", "turn 1", 1000),
				makeAssistant("a1", 1000, 5000), // 4s
				makeNotice("u2", "user", "turn 2", 10000),
				makeAssistant("a2", 10000, 13000),
			},
			active: true,
			want: map[string]time.Duration{
				"a1": 4 * time.Second,
			},
		},
		{
			name: "reverted second turn is excluded while first turn remains visible",
			messages: []opencode.Message{
				makeNotice("u1", "user", "turn 1", 1000),
				makeAssistant("a1", 1000, 5000), // 4s
				makeNotice("u2", "user", "turn 2", 10000),
				makeAssistant("a2", 10000, 16000),
			},
			revertID: "u2",
			want: map[string]time.Duration{
				"a1": 4 * time.Second,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := computeTurnDurations(tc.messages, tc.revertID, tc.active, nil)
			if len(got) != len(tc.want) {
				t.Fatalf("computeTurnDurations() = %v, want %v", got, tc.want)
			}
			for k, wantDur := range tc.want {
				if got[k] != wantDur {
					t.Errorf("computeTurnDurations()[%q] = %v, want %v", k, got[k], wantDur)
				}
			}
		})
	}
}

func TestTurnDurationAndScrollableInlineShellOutput(t *testing.T) {
	// 1. Verify FormatShellOutput preserves all lines and full line lengths for scrollable inline tool output,
	// whereas FormatShellTailLines truncates to the configured N-line tail for the bottom sheet.
	var rawLines []string
	for i := 1; i <= 25; i++ {
		rawLines = append(rawLines, fmt.Sprintf("\x1b[32mline %02d\x1b[0m: %s", i, strings.Repeat("a", 160)))
	}
	raw := strings.Join(rawLines, "\n") + "\n"

	fullOut := FormatShellOutput(raw)
	fullSplit := strings.Split(fullOut, "\n")
	if len(fullSplit) != 25 {
		t.Fatalf("FormatShellOutput line count = %d, want 25", len(fullSplit))
	}
	if !strings.HasPrefix(fullSplit[0], "line 01: ") || len(fullSplit[0]) < 165 {
		t.Fatalf("expected FormatShellOutput to preserve line 01 without 140-char truncation, got len=%d", len(fullSplit[0]))
	}

	tailOut := FormatShellTailLines(raw, 7)
	tailSplit := strings.Split(tailOut, "\n")
	if len(tailSplit) != 7 || !strings.HasPrefix(tailSplit[0], "line 19: ") {
		t.Fatalf("expected FormatShellTailLines(..., 7) to return lines 19..25, got %d lines starting with %q", len(tailSplit), tailSplit[0])
	}

	if !gtk.InitCheck() {
		return
	}

	// 2. Verify live turn duration lifecycle and authoritative timestamp merging on reconcileConvMessages.
	vs := adw.NewViewStack()
	win := &Window{
		ctx:               t.Context(),
		viewStack:         vs,
		stopBtn:           gtk.NewButton(),
		activeShells:      make(map[string]opencode.ShellItem),
		shellOutputs:      make(map[string]string),
		shellOutputLabels: make(map[string]*gtk.Label),
		shellListBox:      gtk.NewListBox(),
		shellsSectionBox:  gtk.NewBox(gtk.OrientationVertical, 0),
		sheetEmptyLabel:   gtk.NewLabel(""),
		tabs:              make(map[string]*sessionTab),
		childToRoot:       make(map[string]string),
	}
	tab := win.addSessionTab(opencode.Session{ID: "ses_turn", Title: "Turn Test"})
	conv := tab.main
	conv.loaded = true

	var u1 opencode.Message
	u1.ID = "u1"
	u1.Type = "user"
	u1.Text = "hello"
	u1.Time.Created = 1000
	conv.messages = append(conv.messages, u1)
	conv.seenMsgIDs["u1"] = true

	aw1 := &AssistantMessageWidget{
		Box:               gtk.NewBox(gtk.OrientationVertical, 0),
		ctx:               t.Context(),
		partsBox:          gtk.NewBox(gtk.OrientationVertical, 0),
		turnDurationLabel: gtk.NewLabel(""),
		tools:             make(map[string]*ToolWidget),
	}
	conv.assistantMap["a1"] = aw1
	conv.messageWidgets["a1"] = aw1

	var a1Placeholder opencode.Message
	a1Placeholder.ID = "a1"
	a1Placeholder.Type = "assistant"
	a1Placeholder.Time.Created = 1000
	conv.messages = append(conv.messages, a1Placeholder)
	conv.seenMsgIDs["a1"] = true

	win.setConvActive("ses_turn", true)
	if aw1.TurnDuration() != "" {
		t.Fatalf("expected empty TurnDuration while turn is active, got %q", aw1.TurnDuration())
	}

	// Reconcile authoritative message from server with exact Created and Completed timestamps.
	var a1Server opencode.Message
	a1Server.ID = "a1"
	a1Server.Type = "assistant"
	a1Server.Time.Created = 10000
	a1Server.Time.Streamed = 13500
	a1Server.Time.Completed = 14200
	reconcileConvMessages(&conv.messages, conv.seenMsgIDs, &conv.pendingPrompts, []opencode.Message{u1, a1Server}, nil, nil)

	win.setConvActive("ses_turn", false)
	if got := aw1.TurnDuration(); got != "Worked for 4.2s" {
		t.Fatalf("aw1.TurnDuration() = %q, want %q", got, "Worked for 4.2s")
	}
	if !aw1.turnDurationLabel.Visible() {
		t.Fatalf("expected aw1.turnDurationLabel to be visible after turn completion")
	}

	// 3. Verify expanded shell ToolWidget receives full scrollable output while bottom sheet label gets 7-line tail.
	outView := gtk.NewTextView()
	twShell := &ToolWidget{
		Box:          gtk.NewBox(gtk.OrientationVertical, 0),
		ctx:          t.Context(),
		outputScroll: gtk.NewScrolledWindow(),
		outputView:   outView,
		outputBuf:    outView.Buffer(),
		toolID:       "call_sh_1",
		toolName:     "shell",
		shellID:      "sh_scroll_1",
	}
	aw1.tools["call_sh_1"] = twShell

	win.activeShells["sh_scroll_1"] = opencode.ShellItem{
		ID:        "sh_scroll_1",
		SessionID: "ses_turn",
		Command:   "seq 1 25",
		Status:    "running",
	}
	win.shellOutputs["sh_scroll_1"] = fullOut
	win.rebuildShellList()
	win.updateToolWidgetsForShell("sh_scroll_1", fullOut, "")

	startIter, endIter := twShell.outputBuf.Bounds()
	gotToolText := twShell.outputBuf.Text(startIter, endIter, false)
	if gotLines := strings.Split(gotToolText, "\n"); len(gotLines) != 25 {
		t.Fatalf("expected inline ToolWidget outputBuf to contain all 25 scrollable lines, got %d", len(gotLines))
	}
	bottomLbl := win.shellOutputLabels["sh_scroll_1"]
	if bottomLbl == nil {
		t.Fatalf("expected bottom sheet shell output label for sh_scroll_1")
	}
	if gotBottomLines := strings.Split(bottomLbl.Text(), "\n"); len(gotBottomLines) != 7 {
		t.Fatalf("expected bottom sheet label to stay a 7-line tail, got %d lines", len(gotBottomLines))
	}

	// Verify markSyntheticTargetCompleted also populates full output on completed <shell> synthetic message.
	twShell2Out := gtk.NewTextView()
	twShell2 := &ToolWidget{
		Box:          gtk.NewBox(gtk.OrientationVertical, 0),
		ctx:          t.Context(),
		statusLabel:  gtk.NewLabel(""),
		spinner:      adw.NewSpinner(),
		outputScroll: gtk.NewScrolledWindow(),
		outputView:   twShell2Out,
		outputBuf:    twShell2Out.Buffer(),
		toolID:       "call_sh_2",
		toolName:     "shell",
		shellID:      "sh_scroll2",
	}
	aw1.tools["call_sh_2"] = twShell2
	conv.markSyntheticTargetCompleted(opencode.Message{
		ID:   "syn_sh_2",
		Type: "synthetic",
		Text: "<shell id=\"sh_scroll2\" state=\"completed\">\n" + raw + "</shell>",
	})
	s2, e2 := twShell2.outputBuf.Bounds()
	if gotLines := strings.Split(twShell2.outputBuf.Text(s2, e2, false), "\n"); len(gotLines) != 25 {
		t.Fatalf("expected markSyntheticTargetCompleted to populate all 25 lines in ToolWidget, got %d", len(gotLines))
	}
}

func TestStartupUnvisitedSubagentSpinnerAndResolution(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("no display")
	}

	// 1. resolveStartupActiveChildren fetches only unknown active/job child IDs
	// and walks nested parents up to their root sessions.
	var listChildCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session":
			if r.URL.Query().Get("parentID") != "null" {
				listChildCalls.Add(1)
			}
			_ = json.NewEncoder(w).Encode([]opencode.Session{})
		case "/api/session/ses_child_1":
			_ = json.NewEncoder(w).Encode(opencode.Session{
				ID:       "ses_child_1",
				ParentID: "ses_root_bg",
				Title:    "Background Researcher",
				Agent:    "explore",
				Time:     opencode.SessionTime{Created: 100, Busy: 200},
			})
		case "/api/session/ses_grandchild_1":
			_ = json.NewEncoder(w).Encode(opencode.Session{
				ID:       "ses_grandchild_1",
				ParentID: "ses_child_mid",
				Title:    "Nested Worker",
				Agent:    "general",
				Time:     opencode.SessionTime{Created: 150, Busy: 250},
			})
		case "/api/session/ses_child_mid":
			_ = json.NewEncoder(w).Encode(opencode.Session{
				ID:       "ses_child_mid",
				ParentID: "ses_root_bg",
				Title:    "Mid Coordinator",
				Agent:    "general",
				Time:     opencode.SessionTime{Created: 120, Busy: 180, Idle: 190},
				Outcome:  "succeeded",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := &opencode.Client{BaseURL: srv.URL, HTTP: srv.Client()}
	rootSessions := []opencode.Session{
		{
			ID:       "ses_root_vis",
			Title:    "Visible Idle Session",
			Location: opencode.Location{Directory: "/tmp/proj"},
			Time:     opencode.SessionTime{Created: 10, Updated: 5000, Busy: 100, Idle: 200},
		},
		{
			ID:       "ses_root_bg",
			Title:    "Unvisited With Running Subagents",
			Location: opencode.Location{Directory: "/tmp/proj"},
			Time:     opencode.SessionTime{Created: 20, Updated: 3000, Busy: 100, Idle: 200},
		},
		{
			ID:       "ses_root_job",
			Title:    "Unvisited With Job Fallback",
			Location: opencode.Location{Directory: "/tmp/proj"},
			Time:     opencode.SessionTime{Created: 30, Updated: 2000, Busy: 100, Idle: 200},
		},
	}
	active := map[string]opencode.ActiveStatus{
		"ses_child_1":      {Type: "busy"},
		"ses_grandchild_1": {Type: "busy"},
	}
	jobs := []opencode.JobInfo{
		{
			ID:             "job_1",
			Kind:           "subagent",
			SessionID:      "ses_root_job",
			ChildSessionID: "ses_job_child_1",
			Status:         "running",
			Title:          "Fallback Subagent Job",
			Agent:          "explore",
		},
	}

	resolvedRoots, resolvedChildren := resolveStartupActiveChildren(t.Context(), client, rootSessions, active, jobs)
	if len(resolvedRoots) != 3 {
		t.Fatalf("len(resolvedRoots) = %d, want 3", len(resolvedRoots))
	}
	if listChildCalls.Load() != 0 {
		t.Fatalf("unexpected ListChildSessions calls: %d", listChildCalls.Load())
	}
	gotChildIDs := make(map[string]string, len(resolvedChildren))
	for _, ch := range resolvedChildren {
		gotChildIDs[ch.ID] = ch.ParentID
	}
	for _, want := range []struct {
		id     string
		parent string
	}{
		{"ses_child_1", "ses_root_bg"},
		{"ses_child_mid", "ses_root_bg"},
		{"ses_grandchild_1", "ses_child_mid"},
		{"ses_job_child_1", "ses_root_job"},
	} {
		if gotChildIDs[want.id] != want.parent {
			t.Fatalf("resolved child %s parent = %q, want %q", want.id, gotChildIDs[want.id], want.parent)
		}
	}

	// 2. populateSessionTabs creates unvisited tabs with running subagent spinners
	// visible immediately without clicking, while keeping main.active = false,
	// and maps childToRoot so subsequent SSE idle events turn the spinner off.
	win := &Window{
		ctx:               t.Context(),
		statePath:         filepath.Join(t.TempDir(), "state.json"),
		viewStack:         adw.NewViewStack(),
		sidebarList:       gtk.NewListBox(),
		tabs:              make(map[string]*sessionTab),
		childToRoot:       make(map[string]string),
		expandedProjects:  make(map[string]bool),
		collapsedProjects: make(map[string]bool),
		visibleSessions:   map[string]bool{"ses_root_vis": true},
		archivedSessions:  make(map[string]bool),
		projectSessions:   make(map[string][]opencode.Session),
	}
	// Make maxVisibleSessions=1 so we also verify active-subagent roots bypass maxVis truncation.
	win.prefs.MaxVisibleSessions = 1

	allForTabs := append(slices.Clone(resolvedRoots), resolvedChildren...)
	win.populateSessionTabs(allForTabs, active)

	if win.currentRootSessionID() != "ses_root_vis" {
		t.Fatalf("currentRootSessionID() = %q, want ses_root_vis", win.currentRootSessionID())
	}
	bgTab := win.tabs["ses_root_bg"]
	if bgTab == nil {
		t.Fatalf("expected unvisited ses_root_bg tab to be created despite MaxVisibleSessions=1")
	}
	if bgTab.main.active {
		t.Fatalf("expected ses_root_bg main.active = false when only child subagents are running")
	}
	if bgTab.sidebarIndicator == nil || bgTab.sidebarIndicator.spinner == nil {
		t.Fatalf("expected sidebarIndicator and spinner to be built for ses_root_bg")
	}
	if !bgTab.sidebarIndicator.spinner.Visible() {
		t.Fatalf("expected sidebar spinner to be visible immediately on startup for unvisited ses_root_bg with running subagents")
	}
	if win.childToRoot["ses_child_1"] != "ses_root_bg" || win.childToRoot["ses_grandchild_1"] != "ses_root_bg" {
		t.Fatalf("childToRoot mappings missing for ses_root_bg children: %+v", win.childToRoot)
	}

	jobTab := win.tabs["ses_root_job"]
	if jobTab == nil || jobTab.sidebarIndicator == nil || !jobTab.sidebarIndicator.spinner.Visible() {
		t.Fatalf("expected unvisited ses_root_job with running subagent job to show sidebar spinner on startup")
	}

	// Simulate SSE idle for both running children of ses_root_bg before ever clicking the tab.
	win.handleEvent(opencode.Event{
		Type: "session.status",
		Data: json.RawMessage(`{"sessionID":"ses_child_1","status":{"type":"idle"}}`),
	})
	win.handleEvent(opencode.Event{
		Type: "session.status",
		Data: json.RawMessage(`{"sessionID":"ses_grandchild_1","status":{"type":"idle"}}`),
	})
	if bgTab.sidebarIndicator.spinner.Visible() {
		t.Fatalf("expected sidebar spinner for ses_root_bg to hide once all running subagents go idle")
	}
}
