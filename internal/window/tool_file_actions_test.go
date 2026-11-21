package window

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotkit/gtkutil/cssutil"
)

var applyTestCSSOnce sync.Once

func TestIsFilePathTool(t *testing.T) {
	tests := []struct {
		name string
		tool string
		want bool
	}{
		{name: "read", tool: "read", want: true},
		{name: "read uppercase", tool: "READ", want: true},
		{name: "write", tool: "write", want: true},
		{name: "edit", tool: "edit", want: true},
		{name: "multiedit", tool: "multiedit", want: true},
		{name: "multi_edit", tool: "multi_edit", want: true},
		{name: "patch", tool: "patch", want: true},
		{name: "apply_patch", tool: "apply_patch", want: true},
		{name: "shell", tool: "shell", want: false},
		{name: "grep", tool: "grep", want: false},
		{name: "subagent", tool: "subagent", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFilePathTool(tc.tool); got != tc.want {
				t.Fatalf("isFilePathTool(%q) = %v, want %v", tc.tool, got, tc.want)
			}
		})
	}
}

func TestIsMarkdownPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "lowercase md", path: "/tmp/README.md", want: true},
		{name: "uppercase MD", path: "docs/GUIDE.MD", want: true},
		{name: "markdown ext", path: "notes.markdown", want: true},
		{name: "mdown ext", path: "notes.mdown", want: true},
		{name: "mkd ext", path: "notes.mkd", want: true},
		{name: "mdx ext", path: "page.mdx", want: true},
		{name: "go file", path: "main.go", want: false},
		{name: "no ext", path: "README", want: false},
		{name: "empty", path: "", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isMarkdownPath(tc.path); got != tc.want {
				t.Fatalf("isMarkdownPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestResolveToolFilePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	tests := []struct {
		name    string
		baseDir string
		rawPath string
		want    string
	}{
		{
			name:    "empty path",
			baseDir: "/repo",
			rawPath: "   ",
			want:    "",
		},
		{
			name:    "absolute path ignores baseDir",
			baseDir: "/repo",
			rawPath: "/other/dir/../README.md",
			want:    "/other/README.md",
		},
		{
			name:    "relative path joins baseDir",
			baseDir: "/repo/project",
			rawPath: "./docs/DESIGN.md",
			want:    "/repo/project/docs/DESIGN.md",
		},
		{
			name:    "relative path without baseDir",
			baseDir: "",
			rawPath: "docs/../README.md",
			want:    "README.md",
		},
		{
			name:    "tilde path expands HOME",
			baseDir: "/repo",
			rawPath: "~/notes/todo.md",
			want:    filepath.Join(home, "notes", "todo.md"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveToolFilePath(tc.baseDir, tc.rawPath); got != tc.want {
				t.Fatalf("resolveToolFilePath(%q, %q) = %q, want %q", tc.baseDir, tc.rawPath, got, tc.want)
			}
		})
	}
}

func TestToolWidgetFileActionsAndMarkdownPreview(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("GTK display not available in test environment")
	}
	adw.Init()

	tests := []struct {
		name            string
		toolName        string
		baseDir         string
		input           map[string]any
		wantPath        string
		wantContextMenu bool
		wantPreviewBtn  bool
	}{
		{
			name:            "read markdown file shows top preview and menu preview",
			toolName:        "read",
			baseDir:         "/work/repo",
			input:           map[string]any{"path": "docs/README.md"},
			wantPath:        "/work/repo/docs/README.md",
			wantContextMenu: true,
			wantPreviewBtn:  true,
		},
		{
			name:            "read go file hides preview button",
			toolName:        "read",
			baseDir:         "/work/repo",
			input:           map[string]any{"path": "/work/repo/main.go"},
			wantPath:        "/work/repo/main.go",
			wantContextMenu: true,
			wantPreviewBtn:  false,
		},
		{
			name:     "write markdown file shows top preview and menu preview",
			toolName: "write",
			baseDir:  "/work/repo",
			input: map[string]any{
				"filePath": "NOTES.markdown",
				"content":  "# Notes\n\nHello",
			},
			wantPath:        "/work/repo/NOTES.markdown",
			wantContextMenu: true,
			wantPreviewBtn:  true,
		},
		{
			name:     "edit markdown file shows top preview and menu preview",
			toolName: "edit",
			baseDir:  "/work/repo",
			input: map[string]any{
				"filePath":  "/work/repo/DESIGN.md",
				"oldString": "a",
				"newString": "b",
			},
			wantPath:        "/work/repo/DESIGN.md",
			wantContextMenu: true,
			wantPreviewBtn:  true,
		},
		{
			name:     "patch non-markdown file supports copy and open with",
			toolName: "patch",
			baseDir:  "/work/repo",
			input: map[string]any{
				"path":  "pkg/foo.go",
				"patch": "@@ -1 +1 @@\n-a\n+b",
			},
			wantPath:        "/work/repo/pkg/foo.go",
			wantContextMenu: true,
			wantPreviewBtn:  false,
		},
		{
			name:            "shell tool has no file context menu or preview button",
			toolName:        "shell",
			baseDir:         "/work/repo",
			input:           map[string]any{"command": "cat README.md"},
			wantPath:        "",
			wantContextMenu: false,
			wantPreviewBtn:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var copied, openedWith, previewed string
			ctx := WithToolFileActions(context.Background(), ToolFileActions{
				CopyPath:        func(p string) { copied = p },
				OpenWith:        func(p string) { openedWith = p },
				PreviewMarkdown: func(p string) { previewed = p },
			})

			tw := NewToolWidget(ctx, "tool_1", tc.toolName, nil)
			tw.SetBaseDir(tc.baseDir)
			tw.SetInput(tc.input)

			if got := tw.FilePath(); got != tc.wantPath {
				t.Fatalf("tw.FilePath() = %q, want %q", got, tc.wantPath)
			}
			if got := tw.canShowFileContextMenu(); got != tc.wantContextMenu {
				t.Fatalf("canShowFileContextMenu() = %v, want %v", got, tc.wantContextMenu)
			}
			if got := tw.previewBtn.Visible(); got != tc.wantPreviewBtn {
				t.Fatalf("previewBtn.Visible() = %v, want %v", got, tc.wantPreviewBtn)
			}
			if got := tw.contextPreviewBtn.Visible(); got != tc.wantPreviewBtn {
				t.Fatalf("contextPreviewBtn.Visible() = %v, want %v", got, tc.wantPreviewBtn)
			}

			if tc.wantContextMenu {
				tw.contextCopyBtn.Emit("clicked")
				if copied != tc.wantPath {
					t.Fatalf("Copy Path clicked -> %q, want %q", copied, tc.wantPath)
				}
				tw.contextOpenWithBtn.Emit("clicked")
				if openedWith != tc.wantPath {
					t.Fatalf("Open With clicked -> %q, want %q", openedWith, tc.wantPath)
				}
			}

			if tc.wantPreviewBtn {
				tw.previewBtn.Emit("clicked")
				if previewed != tc.wantPath {
					t.Fatalf("top Preview button clicked -> %q, want %q", previewed, tc.wantPath)
				}
				previewed = ""
				tw.contextPreviewBtn.Emit("clicked")
				if previewed != tc.wantPath {
					t.Fatalf("context Preview Markdown clicked -> %q, want %q", previewed, tc.wantPath)
				}
			}
		})
	}
}

func TestAssistantMessageWidgetPropagatesBaseDirToToolWidgets(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("GTK display not available in test environment")
	}
	adw.Init()

	aw := NewAssistantMessageWidget(context.Background(), "build", "test", nil)
	tw := aw.EnsureTool("t_1", "read")
	tw.SetInput(map[string]any{"path": "README.md"})
	if got := tw.FilePath(); got != "README.md" {
		t.Fatalf("initial FilePath() = %q, want %q", got, "README.md")
	}

	aw.SetBaseDir("/srv/workspace")
	if got := tw.FilePath(); got != "/srv/workspace/README.md" {
		t.Fatalf("FilePath() after SetBaseDir = %q, want %q", got, "/srv/workspace/README.md")
	}
	if !tw.previewBtn.Visible() {
		t.Fatalf("expected Preview button to be visible for README.md")
	}
}

func TestToolWidgetPreviewLeftOfBadgesAndConsistentHeight(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("GTK display not available in test environment")
	}
	adw.Init()
	applyTestCSSOnce.Do(cssutil.ApplyGlobalCSS)

	tw := NewToolWidget(context.Background(), "t_edit_md", "edit", nil)
	tw.SetInput(map[string]any{
		"filePath":  "/work/repo/DESIGN.md",
		"oldString": "a",
		"newString": "b",
	})

	if !tw.previewBtn.Visible() {
		t.Fatalf("expected previewBtn to be visible for markdown edit")
	}
	if len(tw.badgeWidgets) != 2 {
		t.Fatalf("expected 2 delta badges (+1/-1), got %d", len(tw.badgeWidgets))
	}

	// Verify previewBtn precedes badgesBox inside headerRow.
	previewIdx, badgesIdx := -1, -1
	idx := 0
	for child := tw.headerRow.FirstChild(); child != nil; {
		bw := gtk.BaseWidget(child)
		if bw.Object.Native() == tw.previewBtn.Object.Native() {
			previewIdx = idx
		} else if bw.Object.Native() == tw.badgesBox.Object.Native() {
			badgesIdx = idx
		}
		idx++
		child = bw.NextSibling()
	}
	if previewIdx == -1 || badgesIdx == -1 || previewIdx >= badgesIdx {
		t.Fatalf("expected previewBtn (%d) to appear to the left of badgesBox (%d)", previewIdx, badgesIdx)
	}

	// Measure preferred heights of detailsExpander, previewBtn, and badgesBox to ensure
	// neither pills nor the Preview button bloat the compact tool row height.
	_, expNat, _, _ := tw.detailsExpander.Measure(gtk.OrientationVertical, -1)
	_, previewNat, _, _ := tw.previewBtn.Measure(gtk.OrientationVertical, -1)
	_, badgesNat, _, _ := tw.badgesBox.Measure(gtk.OrientationVertical, -1)
	t.Logf("measured heights: expander=%d previewBtn=%d badgesBox=%d", expNat, previewNat, badgesNat)
	if diff := previewNat - badgesNat; diff < -1 || diff > 1 {
		t.Fatalf("expected previewBtn height (%d) and badgesBox height (%d) to match within 1px (expander=%d)", previewNat, badgesNat, expNat)
	}
	if previewNat > expNat+2 || badgesNat > expNat+2 {
		t.Fatalf("expected previewBtn (%d) and badgesBox (%d) not to exceed expander height (%d) by more than 2px", previewNat, badgesNat, expNat)
	}
}

func TestSubagentToolPreservesViewSessionActionButton(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("GTK display not available in test environment")
	}
	adw.Init()

	var openedSubagent string
	tw := NewToolWidget(context.Background(), "t_sub", "subagent", func(id string) {
		openedSubagent = id
	})
	tw.SetMetadata(map[string]any{"sessionID": "ses_child_1"})
	if !tw.actionBtn.Visible() {
		t.Fatalf("expected subagent actionBtn to be visible")
	}
	if tw.canShowFileContextMenu() {
		t.Fatalf("expected subagent tool not to expose file context menu")
	}
	tw.actionBtn.Emit("clicked")
	if openedSubagent != "ses_child_1" {
		t.Fatalf("openedSubagent = %q, want ses_child_1", openedSubagent)
	}
}

func TestMarkdownViewerLiveReloadAndMissingFile(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("GTK display not available in test environment")
	}
	adw.Init()

	dir := t.TempDir()
	mdPath := filepath.Join(dir, "live.md")

	// Start with a missing file: viewer shows banner and watches for creation.
	var closed bool
	v := newMarkdownViewer(context.Background(), mdPath, nil, nil, nil, nil, func() {
		closed = true
	})
	defer v.cleanup()

	if !v.banner.Revealed() {
		t.Fatalf("expected banner to be revealed when markdown file does not exist yet")
	}
	if v.hasLoaded {
		t.Fatalf("expected hasLoaded=false when file does not exist yet")
	}

	// Create the file and wait for gio.FileMonitor live reload (or fallback if monitor unsupported).
	initialMD := "# Initial Heading\n\nFirst paragraph.\n"
	if err := os.WriteFile(mdPath, []byte(initialMD), 0o644); err != nil {
		t.Fatalf("WriteFile initial: %v", err)
	}

	waitForContent := func(wantSubstr string) bool {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			for glib.MainContextDefault().Iteration(false) {
			}
			if v.hasLoaded && strings.Contains(v.lastContent, wantSubstr) {
				return true
			}
			time.Sleep(15 * time.Millisecond)
		}
		return false
	}

	if !waitForContent("Initial Heading") {
		// Some sandboxed/virtual filesystems do not deliver inotify events; verify reloadFromDisk directly.
		v.reloadFromDisk(false)
		if !strings.Contains(v.lastContent, "Initial Heading") {
			t.Fatalf("expected viewer to load initial content, got %q", v.lastContent)
		}
	}
	if v.banner.Revealed() {
		t.Fatalf("expected banner to hide once file exists and is loaded")
	}
	if len(v.mdView.CommittedWidgets()) == 0 {
		t.Fatalf("expected mdView to render widgets after file load")
	}

	// Update file on disk and verify live update.
	updatedMD := "# Updated Heading\n\nSecond version content.\n"
	if err := os.WriteFile(mdPath, []byte(updatedMD), 0o644); err != nil {
		t.Fatalf("WriteFile update: %v", err)
	}
	if !waitForContent("Updated Heading") {
		v.reloadFromDisk(false)
		if !strings.Contains(v.lastContent, "Updated Heading") {
			t.Fatalf("expected viewer to update to %q, got %q", updatedMD, v.lastContent)
		}
	}

	v.cleanup()
	if !closed {
		t.Fatalf("expected onClose callback to fire on cleanup")
	}
}

func TestWindowOpenMarkdownViewerDeduplicates(t *testing.T) {
	if !gtk.InitCheck() {
		t.Skip("GTK display not available in test environment")
	}
	adw.Init()

	dir := t.TempDir()
	mdPath := filepath.Join(dir, "doc.md")
	if err := os.WriteFile(mdPath, []byte("# Doc\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	w := &Window{
		ctx:       context.Background(),
		mdViewers: make(map[string]*markdownViewer),
	}
	w.openMarkdownViewer(mdPath)
	if len(w.mdViewers) != 1 {
		t.Fatalf("expected 1 open viewer, got %d", len(w.mdViewers))
	}
	first := w.mdViewers[mdPath]
	if first == nil {
		t.Fatalf("expected viewer entry for %q", mdPath)
	}

	// Opening the same path again reuses the existing viewer instead of creating a second one.
	w.openMarkdownViewer(filepath.Join(dir, ".", "doc.md"))
	if len(w.mdViewers) != 1 || w.mdViewers[mdPath] != first {
		t.Fatalf("expected duplicate openMarkdownViewer to reuse existing viewer")
	}

	first.cleanup()
	if len(w.mdViewers) != 0 {
		t.Fatalf("expected viewer entry to be removed after cleanup, got %d", len(w.mdViewers))
	}
}
