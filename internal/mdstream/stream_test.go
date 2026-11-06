package mdstream

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	glibv2 "github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/yuin/goldmark/ast"
)

type patchRecorder struct {
	mu      sync.Mutex
	patches []RenderPatch
}

func (r *patchRecorder) record(p RenderPatch) {
	r.mu.Lock()
	r.patches = append(r.patches, p)
	r.mu.Unlock()
}

func (r *patchRecorder) snapshot() []RenderPatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RenderPatch, len(r.patches))
	copy(out, r.patches)
	return out
}

func (r *patchRecorder) reset() {
	r.mu.Lock()
	r.patches = nil
	r.mu.Unlock()
}

func TestEngineTableDriven(t *testing.T) {
	tests := []struct {
		name           string
		chunks         []string
		wantCommitted  []string
		wantNodeKinds  []ast.NodeKind
		wantCommitsPre int // minimum blocks committed BEFORE Finalize is called
	}{
		{
			name: "multi-paragraph token-by-token",
			chunks: []string{
				"First ", "paragraph ", "with **bold** ", "text.\n",
				"\n",
				"Second ", "paragraph ", "with `code`.\n\n",
				"Third ", "paragraph ", "at the end.",
			},
			wantCommitted: []string{
				"First paragraph with **bold** text.",
				"Second paragraph with `code`.",
				"Third paragraph at the end.",
			},
			wantNodeKinds: []ast.NodeKind{
				ast.KindParagraph,
				ast.KindParagraph,
				ast.KindParagraph,
			},
			wantCommitsPre: 2,
		},
		{
			name: "headings lists and paragraphs",
			chunks: []string{
				"# Heading 1\n\n",
				"- item one\n",
				"- item two\n",
				"- item three\n\n",
				"Final summary paragraph.",
			},
			wantCommitted: []string{
				"# Heading 1",
				"- item one\n- item two\n- item three",
				"Final summary paragraph.",
			},
			wantNodeKinds: []ast.NodeKind{
				ast.KindHeading,
				ast.KindList,
				ast.KindParagraph,
			},
			wantCommitsPre: 2,
		},
		{
			name: "loose list character-by-character does not split prematurely",
			chunks: splitEveryN(
				"- first item\n\n- second item\n\nAfter list.",
				1,
			),
			wantCommitted: []string{
				"- first item\n\n- second item",
				"After list.",
			},
			wantNodeKinds: []ast.NodeKind{
				ast.KindList,
				ast.KindParagraph,
			},
			wantCommitsPre: 1,
		},
		{
			name: "ordered list character-by-character",
			chunks: splitEveryN(
				"1. step one\n2. step two\n\nDone.",
				1,
			),
			wantCommitted: []string{
				"1. step one\n2. step two",
				"Done.",
			},
			wantNodeKinds: []ast.NodeKind{
				ast.KindList,
				ast.KindParagraph,
			},
			wantCommitsPre: 1,
		},
		{
			name: "fenced code block with internal blank lines",
			chunks: []string{
				"Intro paragraph.\n\n",
				"```go\n",
				"package main\n\n",
				"func main() {\n",
				"\tprintln(\"hi\")\n",
				"}\n",
				"```\n\n",
				"Outro paragraph.",
			},
			wantCommitted: []string{
				"Intro paragraph.",
				"```go\npackage main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n```",
				"Outro paragraph.",
			},
			wantNodeKinds: []ast.NodeKind{
				ast.KindParagraph,
				ast.KindFencedCodeBlock,
				ast.KindParagraph,
			},
			wantCommitsPre: 2,
		},
		{
			name: "setext heading streamed character-by-character",
			chunks: splitEveryN(
				"Section Title\n-------------\n\nBody text.",
				1,
			),
			wantCommitted: []string{
				"Section Title\n-------------",
				"Body text.",
			},
			wantNodeKinds: []ast.NodeKind{
				ast.KindHeading,
				ast.KindParagraph,
			},
			wantCommitsPre: 1,
		},
		{
			name: "blockquote and thematic break",
			chunks: []string{
				"> quoted line 1\n",
				"> quoted line 2\n\n",
				"---\n\n",
				"Closing paragraph.",
			},
			wantCommitted: []string{
				"> quoted line 1\n> quoted line 2",
				"---",
				"Closing paragraph.",
			},
			wantNodeKinds: []ast.NodeKind{
				ast.KindBlockquote,
				ast.KindThematicBreak,
				ast.KindParagraph,
			},
			wantCommitsPre: 2,
		},
		{
			name: "gfm table followed by paragraph",
			chunks: []string{
				"| Name | Value |\n",
				"| ---- | ----- |\n",
				"| foo  | bar   |\n\n",
				"Table caption.",
			},
			wantCommitted: []string{
				"| Name | Value |\n| ---- | ----- |\n| foo  | bar   |",
				"Table caption.",
			},
			wantCommitsPre: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var rec patchRecorder
				s := NewStreamer(35*time.Millisecond, rec.record)
				defer s.Close()

				for _, chunk := range tc.chunks {
					s.Append(chunk)
					time.Sleep(35 * time.Millisecond)
					synctest.Wait()
				}

				patches := rec.snapshot()
				var preFinalizeCommitted []string
				for _, p := range patches {
					if p.Final {
						t.Fatalf("unexpected Final patch before Finalize")
					}
					for _, blk := range p.Committed {
						preFinalizeCommitted = append(preFinalizeCommitted, blk.Raw)
					}
				}
				if len(preFinalizeCommitted) < tc.wantCommitsPre {
					t.Fatalf("committed before Finalize = %d (%v), want at least %d",
						len(preFinalizeCommitted), preFinalizeCommitted, tc.wantCommitsPre)
				}

				s.Finalize("")
				synctest.Wait()

				patches = rec.snapshot()
				var gotCommitted []string
				var gotKinds []ast.NodeKind
				for _, p := range patches {
					for _, blk := range p.Committed {
						gotCommitted = append(gotCommitted, blk.Raw)
						gotKinds = append(gotKinds, blk.Node.Kind())
						if string(blk.Source) != blk.Raw {
							t.Errorf("block Source %q != Raw %q", string(blk.Source), blk.Raw)
						}
					}
				}

				if !reflect.DeepEqual(gotCommitted, tc.wantCommitted) {
					t.Fatalf("committed blocks:\ngot:  %#v\nwant: %#v", gotCommitted, tc.wantCommitted)
				}
				if tc.wantNodeKinds != nil && !reflect.DeepEqual(gotKinds, tc.wantNodeKinds) {
					t.Fatalf("node kinds:\ngot:  %v\nwant: %v", gotKinds, tc.wantNodeKinds)
				}

				last := patches[len(patches)-1]
				if !last.Final {
					t.Errorf("last patch Final = false, want true")
				}
				if last.Tail != nil {
					t.Errorf("last patch Tail = %+v, want nil", last.Tail)
				}
			})
		})
	}
}

func TestStreamerCoalescesRapidBursts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = 35 * time.Millisecond

		var rec patchRecorder
		s := NewStreamer(interval, rec.record)
		defer s.Close()

		// Rapid burst of 20 tokens within a single 35ms batch window.
		tokens := []string{
			"# ", "Title", "\n\n",
			"Token ", "1 ", "2 ", "3 ", "4 ", "5 ", "6 ",
			"7 ", "8 ", "9 ", "10 ", "11 ", "12 ", "13 ",
			"14 ", "15 ", "end.",
		}
		for _, tok := range tokens {
			s.Append(tok)
		}

		// Before the batch window elapses, no patch should be emitted.
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		if got := len(rec.snapshot()); got != 0 {
			t.Fatalf("got %d patches at 10ms, want 0", got)
		}

		// Advance past the 35ms window: all 20 tokens must coalesce into exactly 1 patch.
		time.Sleep(25 * time.Millisecond)
		synctest.Wait()
		patches := rec.snapshot()
		if len(patches) != 1 {
			t.Fatalf("got %d patches at 35ms, want 1", len(patches))
		}

		p := patches[0]
		if len(p.Committed) != 1 || p.Committed[0].Raw != "# Title" {
			t.Fatalf("patch[0].Committed = %+v, want [# Title]", p.Committed)
		}
		wantTail := "Token 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 end."
		if p.Tail == nil || p.Tail.Raw != wantTail {
			t.Fatalf("patch[0].Tail = %+v, want %q", p.Tail, wantTail)
		}
	})
}

func TestPrefixBlocksCommittedExactlyOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = 35 * time.Millisecond

		var rec patchRecorder
		s := NewStreamer(interval, rec.record)
		defer s.Close()

		// Stream paragraph 1, then start paragraph 2.
		s.Append("Paragraph one is complete.\n\nStart of two")
		time.Sleep(interval)
		synctest.Wait()

		patches := rec.snapshot()
		if len(patches) != 1 {
			t.Fatalf("got %d patches, want 1", len(patches))
		}
		if len(patches[0].Committed) != 1 || patches[0].Committed[0].Raw != "Paragraph one is complete." {
			t.Fatalf("expected paragraph one committed in patch 0, got %+v", patches[0].Committed)
		}

		// Stream 30 more tokens into paragraph 2 and code block across 30 separate batch windows.
		for i := 0; i < 30; i++ {
			s.Append(" word")
			time.Sleep(interval)
			synctest.Wait()
		}

		patches = rec.snapshot()
		// Verify Paragraph one was NEVER re-emitted in any subsequent patch's Committed slice.
		for idx := 1; idx < len(patches); idx++ {
			if len(patches[idx].Committed) != 0 {
				t.Fatalf("patch[%d] unexpectedly emitted Committed = %+v", idx, patches[idx].Committed)
			}
			if patches[idx].Tail == nil {
				t.Fatalf("patch[%d] Tail is nil", idx)
			}
		}

		s.Finalize("")
		synctest.Wait()

		patches = rec.snapshot()
		// Across the entire stream lifecycle, count how many times each block was committed.
		commitCounts := make(map[string]int)
		var order []string
		for _, p := range patches {
			for _, c := range p.Committed {
				commitCounts[c.Raw]++
				order = append(order, c.Raw)
			}
		}

		if len(order) != 2 {
			t.Fatalf("total committed blocks = %d (%v), want 2", len(order), order)
		}
		for raw, count := range commitCounts {
			if count != 1 {
				t.Errorf("block %q committed %d times, want 1", raw, count)
			}
		}
	})
}

func TestUnclosedFencedCodeBlockRemainsInTail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = 35 * time.Millisecond

		var rec patchRecorder
		s := NewStreamer(interval, rec.record)
		defer s.Close()

		// 1. Open a fenced code block and stream lines with blank lines inside.
		codeSteps := []string{
			"```go\n",
			"package main\n\n",
			"// heading-like comment\n# Not a markdown heading\n\n",
			"func add(a, b int) int {\n",
			"\treturn a + b\n",
			"}\n",
		}
		for _, tok := range codeSteps {
			s.Append(tok)
			time.Sleep(interval)
			synctest.Wait()
		}

		for i, p := range rec.snapshot() {
			if len(p.Committed) != 0 {
				t.Fatalf("step %d: unclosed fence should not commit, got %+v", i, p.Committed)
			}
			if p.Tail == nil {
				t.Fatalf("step %d: expected non-nil Tail for unclosed fence", i)
			}
		}

		// 2. Close the fence, but do NOT start a subsequent block yet.
		rec.reset()
		s.Append("```\n")
		time.Sleep(interval)
		synctest.Wait()

		for i, p := range rec.snapshot() {
			if len(p.Committed) != 0 {
				t.Fatalf("after closing fence without following block: patch %d committed %+v, want none yet", i, p.Committed)
			}
		}

		// 3. Start a subsequent paragraph block -> now the fenced code block MUST commit!
		rec.reset()
		s.Append("\nSubsequent paragraph.")
		time.Sleep(interval)
		synctest.Wait()

		patches := rec.snapshot()
		if len(patches) != 1 {
			t.Fatalf("expected 1 patch after subsequent block started, got %d", len(patches))
		}
		if len(patches[0].Committed) != 1 {
			t.Fatalf("expected fenced code block to commit once subsequent block started, got %+v", patches[0].Committed)
		}
		if patches[0].Committed[0].Node.Kind() != ast.KindFencedCodeBlock {
			t.Errorf("committed node kind = %v, want FencedCodeBlock", patches[0].Committed[0].Node.Kind())
		}
		if patches[0].Tail == nil || patches[0].Tail.Raw != "Subsequent paragraph." {
			t.Errorf("Tail = %+v, want 'Subsequent paragraph.'", patches[0].Tail)
		}

		// 4. Finalize commits the subsequent paragraph.
		s.Finalize("")
		synctest.Wait()

		patches = rec.snapshot()
		last := patches[len(patches)-1]
		if !last.Final || len(last.Committed) != 1 || last.Committed[0].Raw != "Subsequent paragraph." {
			t.Fatalf("final patch = %+v, want Committed=['Subsequent paragraph.'] Final=true", last)
		}
	})
}

func TestFinalizeFlushesPendingAndUnclosedFenceImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = 35 * time.Millisecond

		var rec patchRecorder
		s := NewStreamer(interval, rec.record)
		defer s.Close()

		// Append tokens and immediately call Finalize WITHOUT waiting for the 35ms timer.
		s.Append("Paragraph before code.\n\n")
		s.Append("```python\n")
		s.Append("def hello():\n    return 42")
		s.Finalize("")
		synctest.Wait()

		patches := rec.snapshot()
		if len(patches) != 1 {
			t.Fatalf("expected 1 immediate final patch, got %d", len(patches))
		}
		p := patches[0]
		if !p.Final {
			t.Errorf("patch.Final = false, want true")
		}
		if p.Tail != nil {
			t.Errorf("patch.Tail = %+v, want nil", p.Tail)
		}
		if len(p.Committed) != 2 {
			t.Fatalf("len(Committed) = %d (%+v), want 2", len(p.Committed), p.Committed)
		}
		if p.Committed[0].Raw != "Paragraph before code." {
			t.Errorf("Committed[0].Raw = %q", p.Committed[0].Raw)
		}
		if p.Committed[1].Raw != "```python\ndef hello():\n    return 42" {
			t.Errorf("Committed[1].Raw = %q", p.Committed[1].Raw)
		}
	})
}

func TestFinalizeWithFullTextOverride(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = 35 * time.Millisecond

		var rec patchRecorder
		s := NewStreamer(interval, rec.record)
		defer s.Close()

		full := "First block.\n\nSecond block.\n\nThird block."
		// Stream first two blocks and let first block commit.
		s.Append("First block.\n\nSecond ")
		time.Sleep(interval)
		synctest.Wait()

		// Now Finalize with the full text containing the rest of Second block and Third block.
		s.Finalize(full)
		synctest.Wait()

		var committed []string
		for _, p := range rec.snapshot() {
			for _, c := range p.Committed {
				committed = append(committed, c.Raw)
			}
		}

		want := []string{"First block.", "Second block.", "Third block."}
		if !reflect.DeepEqual(committed, want) {
			t.Fatalf("committed = %#v, want %#v", committed, want)
		}
		if strings.Join(committed, "\n\n") != full {
			t.Fatalf("joined committed != full")
		}
	})
}

func TestViewCodeTagAndTableRendering(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("GTK cannot be initialized")
	}

	ctx := context.Background()
	view := NewView(ctx)

	if got := view.committedBox.Spacing(); got != 8 {
		t.Errorf("committedBox spacing = %d, want 8", got)
	}

	t.Run("inline code span rendering", func(t *testing.T) {
		blocks := ParseStatic("Paragraph with `code` span.")
		if len(blocks) != 1 {
			t.Fatalf("ParseStatic returned %d blocks, want 1", len(blocks))
		}
		markup := renderInline(blocks[0].Node, blocks[0].Source)
		wantSpan := `<tt><span background="#787c88" bgalpha="20%" size="92%">&#160;code&#160;</span></tt>`
		if !strings.Contains(markup, wantSpan) {
			t.Errorf("renderInline() = %q, want it to contain %q", markup, wantSpan)
		}

		view.SetStatic("Paragraph with `code` span.")
		widgets := view.CommittedWidgets()
		if len(widgets) == 0 {
			t.Fatal("committedBox has no children after SetStatic")
		}
		lbl, ok := widgets[0].(*gtk.Label)
		if !ok {
			t.Fatalf("expected *gtk.Label, got %T", widgets[0])
		}
		if !lbl.HasCSSClass("glup-md-paragraph") {
			t.Errorf("label missing class glup-md-paragraph")
		}
	})

	t.Run("code block rendering", func(t *testing.T) {
		view.SetStatic("```go\nfunc add(a, b int) int { return a + b }\n```")
		widgets := view.CommittedWidgets()
		if len(widgets) == 0 {
			t.Fatal("committedBox has no children after SetStatic")
		}
		codeBlock, ok := widgets[0].(*CodeBlock)
		if !ok {
			t.Fatalf("expected *CodeBlock for codeblock, got %T", widgets[0])
		}
		if !codeBlock.HasCSSClass("glup-md-codeblock") {
			t.Errorf("codeBlock missing class glup-md-codeblock")
		}

		if codeBlock.LangLabel == nil || codeBlock.LangLabel.Text() != "go" {
			t.Errorf("langLabel = %v, want 'go'", codeBlock.LangLabel)
		}

		if codeBlock.TextView == nil {
			t.Fatal("expected TextView inside codeBlock")
		}
		buf := codeBlock.TextView.Buffer()
		start := buf.StartIter()
		end := buf.EndIter()
		text := buf.Slice(start, end, true)
		if !strings.Contains(text, "func add(a, b int)") {
			t.Errorf("code text = %q, want to contain func add", text)
		}
	})

	t.Run("list rendering", func(t *testing.T) {
		view.SetStatic("- item one\n- item two")
		widgets := view.CommittedWidgets()
		if len(widgets) == 0 {
			t.Fatal("committedBox has no children after SetStatic")
		}
		listBlock, ok := widgets[0].(*ListBlock)
		if !ok {
			t.Fatalf("expected *ListBlock for list, got %T", widgets[0])
		}
		if !listBlock.HasCSSClass("glup-md-list") {
			t.Errorf("listBlock missing class glup-md-list")
		}

		if len(listBlock.Items) == 0 {
			t.Fatal("listBlock has no children")
		}
		item1 := listBlock.Items[0]
		if !item1.HasCSSClass("glup-md-listitem") {
			t.Errorf("itemBox missing class glup-md-listitem")
		}
		if item1.Bullet == nil || item1.Bullet.Text() != "•" {
			t.Errorf("bullet = %v, want •", item1.Bullet)
		}
	})

	t.Run("table rendering", func(t *testing.T) {
		tableMD := "| Header A | Header B |\n| :--- | :---: |\n| Cell 1 | Cell 2 |"
		view.SetStatic(tableMD)

		widgets := view.CommittedWidgets()
		if len(widgets) == 0 {
			t.Fatal("committedBox has no children after SetStatic")
		}

		grid, ok := widgets[0].(*TableGrid)
		if !ok {
			t.Fatalf("child is not *TableGrid: %T", widgets[0])
		}
		if !grid.HasCSSClass("glup-md-table") {
			t.Errorf("grid missing class glup-md-table")
		}
		if !grid.HExpand() {
			t.Errorf("grid.HExpand() = false, want true")
		}

		// Verify header and cell labels
		labels := grid.Labels
		if len(labels) != 4 {
			t.Fatalf("expected 4 table labels (2 headers + 2 cells), got %d", len(labels))
		}

		var texts []string
		for _, lbl := range labels {
			texts = append(texts, lbl.Text())
			if !lbl.HExpand() {
				t.Errorf("table label %q HExpand = false, want true", lbl.Text())
			}
			if !lbl.Wrap() {
				t.Errorf("table label %q Wrap = false, want true", lbl.Text())
			}
		}

		for _, want := range []string{"Header A", "Header B", "Cell 1", "Cell 2"} {
			found := false
			for _, got := range texts {
				if got == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("table labels missing text %q in %v", want, texts)
			}
		}

		for _, lbl := range labels {
			if strings.HasPrefix(lbl.Text(), "Header") {
				if !lbl.HasCSSClass("glup-md-table-header") {
					t.Errorf("header label %q missing glup-md-table-header class", lbl.Text())
				}
			} else {
				if !lbl.HasCSSClass("glup-md-table-cell") {
					t.Errorf("cell label %q missing glup-md-table-cell class", lbl.Text())
				}
			}
		}
	})

	t.Run("image rendering", func(t *testing.T) {
		md := "Image with alt: ![Example Diagram](https://example.com/diagram.png) and without alt: ![](https://example.com/icon.svg)"
		blocks := ParseStatic(md)
		if len(blocks) != 1 {
			t.Fatalf("expected 1 block, got %d", len(blocks))
		}
		markup := renderInline(blocks[0].Node, blocks[0].Source)
		wantAlt := `<a href="https://example.com/diagram.png">Example Diagram</a>`
		if !strings.Contains(markup, wantAlt) {
			t.Errorf("renderInline() = %q, want it to contain %q", markup, wantAlt)
		}
		wantNoAlt := `<a href="https://example.com/icon.svg">https://example.com/icon.svg</a>`
		if !strings.Contains(markup, wantNoAlt) {
			t.Errorf("renderInline() = %q, want it to contain %q", markup, wantNoAlt)
		}
	})
}

func splitEveryN(s string, n int) []string {
	var out []string
	for len(s) > 0 {
		if len(s) < n {
			n = len(s)
		}
		out = append(out, s[:n])
		s = s[n:]
	}
	return out
}

func TestViewDedup(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("GTK cannot be initialized")
	}

	flushGTK := func() {
		for glibv2.MainContextDefault().Iteration(false) {
		}
	}

	t.Run("SetStatic followed by Finalize produces exactly 2 widgets", func(t *testing.T) {
		view := NewView(context.Background())
		view.SetStatic("para1\n\npara2")
		view.Finalize("para1\n\npara2")
		flushGTK()
		if got := len(view.CommittedWidgets()); got != 2 {
			t.Errorf("len(CommittedWidgets()) = %d, want 2", got)
		}
	})

	t.Run("SetStatic then AppendDelta then Finalize produces exactly 3 widgets", func(t *testing.T) {
		view := NewView(context.Background())
		view.SetStatic("para1\n\n")
		view.AppendDelta("para2\n\npara3")
		view.Finalize("para1\n\npara2\n\npara3")
		flushGTK()
		if got := len(view.CommittedWidgets()); got != 3 {
			t.Errorf("len(CommittedWidgets()) = %d, want 3", got)
		}
	})

	t.Run("AppendDelta then Finalize with different text produces exactly 3 widgets", func(t *testing.T) {
		view := NewView(context.Background())
		view.AppendDelta("old1\n\nold2")
		time.Sleep(50 * time.Millisecond)
		flushGTK()
		view.Finalize("new1\n\nnew2\n\nnew3")
		flushGTK()
		if got := len(view.CommittedWidgets()); got != 3 {
			t.Errorf("len(CommittedWidgets()) = %d, want 3", got)
		}
	})

	t.Run("SetStatic followed by AppendDelta continues paragraph without splitting", func(t *testing.T) {
		view := NewView(context.Background())
		view.SetStatic("Hello")
		view.AppendDelta(" world\n\nNext para\n\nThird")
		view.Finalize("Hello world\n\nNext para\n\nThird")
		flushGTK()
		widgets := view.CommittedWidgets()
		if len(widgets) != 3 {
			t.Fatalf("len(CommittedWidgets()) = %d, want 3", len(widgets))
		}
		lbl0, ok := widgets[0].(*gtk.Label)
		if !ok || lbl0.Text() != "Hello world" {
			t.Errorf("block 0 text = %q, want 'Hello world'", lbl0.Text())
		}
		lbl1, ok := widgets[1].(*gtk.Label)
		if !ok || lbl1.Text() != "Next para" {
			t.Errorf("block 1 text = %q, want 'Next para'", lbl1.Text())
		}
		lbl2, ok := widgets[2].(*gtk.Label)
		if !ok || lbl2.Text() != "Third" {
			t.Errorf("block 2 text = %q, want 'Third'", lbl2.Text())
		}
	})
}
