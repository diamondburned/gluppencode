package window

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4-sourceview/pkg/gtksource/v5"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"libdb.so/gluppencode/internal/opencode"
)

func newTestDraftWindow(t *testing.T, statePath, srvURL string) *Window {
	t.Helper()
	if !gtk.InitCheck() {
		t.Skip("no display")
	}
	adw.Init()
	gtksource.Init()

	builder := gtk.NewBuilderFromString(windowUI)
	vs := builder.GetObject("viewStack").Cast().(*adw.ViewStack)
	inputView := builder.GetObject("inputView").Cast().(*gtksource.View)
	inputBuf := inputView.Buffer().Cast().(*gtksource.Buffer)

	var client *opencode.Client
	if srvURL != "" {
		client = &opencode.Client{BaseURL: srvURL}
	}

	w := &Window{
		ctx:                 t.Context(),
		client:              client,
		statePath:           statePath,
		draftsDir:           defaultDraftsDirForState(statePath),
		viewStack:           vs,
		inputView:           inputView,
		inputBuffer:         inputBuf,
		inputScroll:         builder.GetObject("inputScroll").Cast().(*gtk.ScrolledWindow),
		composerExpandBtn:   builder.GetObject("composerExpandBtn").Cast().(*gtk.Button),
		composerPlaceholder: builder.GetObject("composerPlaceholder").Cast().(*gtk.Label),
		sendBtn:             builder.GetObject("sendBtn").Cast().(*gtk.Button),
		stopBtn:             builder.GetObject("stopBtn").Cast().(*gtk.Button),
		attachmentTray:      builder.GetObject("attachmentTray").Cast().(*adw.WrapBox),
		tabs:                make(map[string]*sessionTab),
		childToRoot:         make(map[string]string),
		visibleSessions:     make(map[string]bool),
		archivedSessions:    make(map[string]bool),
		projectSessions:     make(map[string][]opencode.Session),
		drafts:              make(map[string]*composerDraft),
		draftWriteGen:       make(map[string]uint64),
	}
	w.setupComposerMultiline()
	w.initComposerDrafts()
	w.viewStack.NotifyProperty("visible-child-name", func() {
		w.onVisibleTabChanged()
	})
	t.Cleanup(func() {
		for _, d := range w.drafts {
			if d != nil && d.saveTimer != 0 {
				glib.SourceRemove(d.saveTimer)
				d.saveTimer = 0
			}
		}
	})
	return w
}

// minimal 1x1 PNG bytes
var testPNGBytes = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}

func TestDraftPerConversationIsolation(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "opencode", "gluppencode.json")
	w := newTestDraftWindow(t, statePath, "")

	// Greeter draft stays isolated when tabs are added.
	w.inputBuffer.SetText("greeter text")

	sessA := opencode.Session{ID: "ses_a", Title: "Session A", Location: opencode.Location{Directory: "/tmp/a"}}
	sessB := opencode.Session{ID: "ses_b", Title: "Session B", Location: opencode.Location{Directory: "/tmp/b"}}
	subA := opencode.Session{ID: "ses_sub_a", ParentID: "ses_a", Title: "Sub A", Location: opencode.Location{Directory: "/tmp/a"}}

	tabA := w.ensureSessionTab(sessA)
	w.ensureSessionTab(sessB)
	w.upsertSubagent(tabA, subA)

	w.viewStack.SetVisibleChildName("ses_a")
	if got := bufferText(w.inputBuffer); got != "" {
		t.Fatalf("expected ses_a initial draft empty, got %q", got)
	}
	w.inputBuffer.SetText("**alpha**")
	w.setComposerExpanded(true)
	w.attachRawToDraft("ses_a", "a.png", "image/png", testPNGBytes, nil)

	// Verify markdown tag applied in ses_a's buffer.
	if iter := w.inputBuffer.IterAtOffset(0); iter == nil || !iter.HasTag(w.mdTagBold) {
		t.Fatalf("expected bold markdown tag in ses_a buffer")
	}

	// Switch to ses_b: should have empty text, collapsed, no attachments.
	w.viewStack.SetVisibleChildName("ses_b")
	if got := bufferText(w.inputBuffer); got != "" {
		t.Fatalf("expected ses_b empty, got %q", got)
	}
	if w.composerExpanded {
		t.Fatal("expected ses_b composerExpanded false")
	}
	if len(w.pendingFiles) != 0 {
		t.Fatalf("expected ses_b 0 pending files, got %d", len(w.pendingFiles))
	}
	w.inputBuffer.SetText("beta")

	// Open subagent under ses_a: should have its own separate draft.
	w.viewStack.SetVisibleChildName("ses_a")
	if got := bufferText(w.inputBuffer); got != "**alpha**" {
		t.Fatalf("expected ses_a restored **alpha**, got %q", got)
	}
	if !w.composerExpanded {
		t.Fatal("expected ses_a composerExpanded true")
	}
	if len(w.pendingFiles) != 1 || w.pendingFiles[0].Name != "a.png" {
		t.Fatalf("expected ses_a attachment a.png, got %+v", w.pendingFiles)
	}

	w.openSubagent(tabA, subA)
	if w.activeDraftKey != "ses_sub_a" {
		t.Fatalf("expected activeDraftKey ses_sub_a, got %q", w.activeDraftKey)
	}
	if got := bufferText(w.inputBuffer); got != "" || w.composerExpanded || len(w.pendingFiles) != 0 {
		t.Fatalf("expected subagent draft clean, got text=%q expanded=%v files=%d", got, w.composerExpanded, len(w.pendingFiles))
	}
	w.inputBuffer.SetText("subagent draft")

	// Close subagent: returns to ses_a draft.
	w.closeSubagent(tabA)
	if w.activeDraftKey != "ses_a" {
		t.Fatalf("expected activeDraftKey ses_a, got %q", w.activeDraftKey)
	}
	if got := bufferText(w.inputBuffer); got != "**alpha**" {
		t.Fatalf("expected ses_a draft intact after closing subagent, got %q", got)
	}

	// Switch back to ses_sub_a and ses_b to verify both retained their text.
	w.openSubagent(tabA, subA)
	if got := bufferText(w.inputBuffer); got != "subagent draft" {
		t.Fatalf("expected subagent draft preserved, got %q", got)
	}
	w.viewStack.SetVisibleChildName("ses_b")
	if got := bufferText(w.inputBuffer); got != "beta" {
		t.Fatalf("expected ses_b draft preserved, got %q", got)
	}

	// Greeter draft is still intact in memory and never persisted to disk.
	w.switchComposerDraft(greeterDraftKey)
	if got := bufferText(w.inputBuffer); got != "greeter text" {
		t.Fatalf("expected greeter draft preserved in memory, got %q", got)
	}
}

func TestDraftFastDebounceAndReload(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "opencode", "gluppencode.json")
	w := newTestDraftWindow(t, statePath, "")

	// Greeter typing must NOT write anything under drafts/.
	w.inputBuffer.SetText("in memory only")
	pumpFor(120 * time.Millisecond)
	if entries, err := os.ReadDir(w.resolvedDraftsDir()); err == nil && len(entries) != 0 {
		t.Fatalf("expected no persisted draft dirs for greeter, got %d", len(entries))
	}

	sess := opencode.Session{ID: "ses_persist", Title: "Persist", Location: opencode.Location{Directory: "/tmp/p"}}
	w.ensureSessionTab(sess)
	w.viewStack.SetVisibleChildName("ses_persist")

	w.inputBuffer.SetText("hello crash recovery")
	w.setComposerExpanded(true)
	w.attachRawToDraft("ses_persist", "clip.png", "image/png", testPNGBytes, nil)

	// Wait for fast debounce to flush to disk.
	draftFile := filepath.Join(w.resolvedDraftsDir(), "ses_persist", draftFileName)
	pumpUntil(t, 500*time.Millisecond, func() bool {
		raw, err := os.ReadFile(draftFile)
		return err == nil && strings.Contains(string(raw), "hello crash recovery")
	})

	// Type more and immediately flush via close-request path without waiting for debounce.
	w.inputBuffer.SetText("hello crash recovery final")
	w.flushAllDraftsSync()

	raw, err := os.ReadFile(draftFile)
	if err != nil {
		t.Fatalf("read draft.json: %v", err)
	}
	var pd persistedDraft
	if err := json.Unmarshal(raw, &pd); err != nil {
		t.Fatalf("unmarshal draft.json: %v", err)
	}
	if pd.Text != "hello crash recovery final" || !pd.Expanded || len(pd.Attachments) != 1 {
		t.Fatalf("unexpected persisted draft: %+v", pd)
	}
	if !strings.HasSuffix(pd.Attachments[0].File, ".bin") {
		t.Fatalf("expected .bin attachment filename, got %q", pd.Attachments[0].File)
	}

	// Simulate fresh window after restart/crash and verify lazy restore on tab activation.
	w2 := newTestDraftWindow(t, statePath, "")
	w2.ensureSessionTab(sess)
	w2.viewStack.SetVisibleChildName("ses_persist")

	if got := bufferText(w2.inputBuffer); got != "hello crash recovery final" {
		t.Fatalf("restored text = %q, want %q", got, "hello crash recovery final")
	}
	if !w2.composerExpanded {
		t.Fatal("expected restored composerExpanded true")
	}
	if len(w2.pendingFiles) != 1 || w2.pendingFiles[0].Name != "clip.png" {
		t.Fatalf("expected restored attachment clip.png, got %+v", w2.pendingFiles)
	}
	if w2.pendingFiles[0].Data != base64.StdEncoding.EncodeToString(testPNGBytes) {
		t.Fatalf("restored attachment bytes mismatch")
	}
	// Restored text should not be undoable back to empty.
	if w2.inputBuffer.CanUndo() {
		t.Fatal("expected CanUndo false right after restoring draft from disk")
	}
}

func TestDraftOrphansRemoveSendRestoreAndArchiveCleanup(t *testing.T) {
	var failSend atomic.Bool
	failSend.Store(true)
	var blockSend atomic.Bool
	sendGate := make(chan struct{})
	sendStarted := make(chan struct{}, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session/ses_send/prompt":
			if blockSend.Load() {
				sendStarted <- struct{}{}
				<-sendGate
			}
			if failSend.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_1","delivery":"queue"}`))
			return
		case "/api/session/ses_send/command":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	statePath := filepath.Join(t.TempDir(), "opencode", "gluppencode.json")
	w := newTestDraftWindow(t, statePath, srv.URL)
	w.commands = []opencode.CommandInfo{{Name: "review"}}

	// Place orphaned files in ses_send dir before loading it.
	sesDir := filepath.Join(w.resolvedDraftsDir(), "ses_send")
	if err := os.MkdirAll(sesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	orphanPath := filepath.Join(sesDir, "orphan.bin")
	if err := os.WriteFile(orphanPath, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	sessSend := opencode.Session{ID: "ses_send", Title: "Send", Location: opencode.Location{Directory: "/tmp/s"}}
	sessOther := opencode.Session{ID: "ses_other", Title: "Other", Location: opencode.Location{Directory: "/tmp/s"}}
	subSend := opencode.Session{ID: "ses_send_sub", ParentID: "ses_send", Title: "Sub", Location: opencode.Location{Directory: "/tmp/s"}}

	tabSend := w.ensureSessionTab(sessSend)
	w.ensureSessionTab(sessOther)
	w.upsertSubagent(tabSend, subSend)

	// Switching to ses_send loads from disk, finds no draft.json, and removes orphan dir.
	w.viewStack.SetVisibleChildName("ses_send")
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("expected orphan file cleaned on load, stat err=%v", err)
	}

	// Put another orphan file in ses_send before beginning a brand new draft via attachRawToDraft.
	if err := os.MkdirAll(sesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphanPath, []byte("junk2"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.attachRawToDraft("ses_send", "pic.png", "image/png", testPNGBytes, nil)
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("expected orphan cleaned when beginning new draft, stat err=%v", err)
	}

	// Removing attachment deletes its .bin and cleans up empty draft dir.
	w.removePendingFile(0)
	if _, err := os.Stat(sesDir); !os.IsNotExist(err) {
		t.Fatalf("expected empty draft dir removed after removing last attachment, stat err=%v", err)
	}

	// Re-attach + type, verify permissions and stale in-flight saveDraftAsync safety.
	w.inputBuffer.SetText("unsent prompt")
	staleSnap := w.makeDraftSnapshot(w.drafts["ses_send"])
	w.attachRawToDraft("ses_send", "pic2.png", "image/png", testPNGBytes, nil)
	// Simulate an in-flight async snapshot finishing after attachRawToDraft wrote its .bin.
	w.persistDraftSnapshot(staleSnap)

	if info, err := os.Stat(sesDir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("expected sesDir mode 0700, err=%v mode=%v", err, info.Mode().Perm())
	}
	if info, err := os.Stat(filepath.Join(sesDir, draftFileName)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("expected draft.json mode 0600, err=%v mode=%v", err, info.Mode().Perm())
	}
	// Verify the blob survived the stale snapshot write.
	reloadedRaw, err := os.ReadFile(filepath.Join(sesDir, draftFileName))
	if err != nil {
		t.Fatal(err)
	}
	var reloadedPD persistedDraft
	if err := json.Unmarshal(reloadedRaw, &reloadedPD); err != nil || len(reloadedPD.Attachments) != 1 {
		t.Fatalf("expected 1 persisted attachment after stale snapshot, err=%v pd=%+v", err, reloadedPD)
	}
	blobInfo, err := os.Stat(filepath.Join(sesDir, reloadedPD.Attachments[0].File))
	if err != nil || blobInfo.Mode().Perm() != 0o600 {
		t.Fatalf("expected .bin blob preserved with mode 0600, err=%v", err)
	}

	// Server slash command (/review) must preserve pending attachments.
	w.inputBuffer.SetText("/review")
	w.sendPromptWithDelivery(opencode.DeliveryQueue)
	if len(w.pendingFiles) != 1 || w.pendingFiles[0].Name != "pic2.png" {
		t.Fatalf("expected /review slash command to preserve pendingFiles, got %+v", w.pendingFiles)
	}
	if _, err := os.Stat(filepath.Join(sesDir, reloadedPD.Attachments[0].File)); err != nil {
		t.Fatalf("expected .bin attachment preserved across RunCommand: %v", err)
	}

	// Restore prompt text and send when server returns 500, switching to ses_other before callback arrives.
	w.inputBuffer.SetText("unsent prompt")
	blockSend.Store(true)
	w.sendPromptWithDelivery(opencode.DeliveryQueue)
	<-sendStarted

	// In-memory composer clears immediately on dispatch, while on-disk draft stays crash-safe mid-flight.
	if got := bufferText(w.inputBuffer); got != "" || len(w.pendingFiles) != 0 {
		t.Fatalf("expected in-memory composer cleared on dispatch, got text=%q files=%d", got, len(w.pendingFiles))
	}
	if _, err := os.Stat(filepath.Join(sesDir, draftFileName)); err != nil {
		t.Fatalf("expected draft.json preserved on disk while SendPrompt is in flight: %v", err)
	}
	sendGate <- struct{}{}

	// Switch away while SendPrompt 500 is processed.
	w.viewStack.SetVisibleChildName("ses_other")
	pumpUntil(t, 2*time.Second, func() bool {
		d := w.drafts["ses_send"]
		return d != nil && bufferText(d.buffer) == "unsent prompt" && len(d.files) == 1
	})
	// Active view (ses_other) must remain untouched while ses_send gets its draft back.
	if got := bufferText(w.inputBuffer); got != "" {
		t.Fatalf("expected ses_other buffer untouched, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(sesDir, draftFileName)); err != nil {
		t.Fatalf("expected restored ses_send draft.json on disk: %v", err)
	}

	// No-op restoreDraft must not clobber active historyBrowsing state.
	w.viewStack.SetVisibleChildName("ses_send")
	w.historyBrowsing = true
	w.historyDraft = "earlier typed draft"
	w.restoreDraft("ses_send", "should not overwrite", nil)
	if !w.historyBrowsing || w.historyDraft != "earlier typed draft" {
		t.Fatalf("expected restoreDraft no-op to preserve history state, got browsing=%v draft=%q", w.historyBrowsing, w.historyDraft)
	}
	w.historyBrowsing = false
	w.historyDraft = ""

	// Now succeed send on ses_send -> cleans up ses_send draft dir only after SendPrompt completes.
	blockSend.Store(false)
	failSend.Store(false)
	w.sendPromptWithDelivery(opencode.DeliveryQueue)
	pumpUntil(t, 2*time.Second, func() bool {
		_, err := os.Stat(sesDir)
		return os.IsNotExist(err)
	})

	// Typing a follow-up draft while SendPrompt is in-flight must not be deleted when SendPrompt succeeds.
	tabSend.main.pendingPrompts = nil
	w.inputBuffer.SetText("first prompt")
	blockSend.Store(true)
	w.sendPromptWithDelivery(opencode.DeliveryQueue)
	<-sendStarted
	w.inputBuffer.SetText("follow-up draft")
	w.flushAllDraftsSync()
	sendGate <- struct{}{}
	pumpUntil(t, 2*time.Second, func() bool {
		return len(tabSend.main.pendingPrompts) > 0 && tabSend.main.pendingPrompts[0].inboxID == "msg_1"
	})
	if _, err := os.Stat(filepath.Join(sesDir, draftFileName)); err != nil {
		t.Fatalf("expected follow-up draft preserved after in-flight SendPrompt succeeded: %v", err)
	}
	blockSend.Store(false)
	w.clearDraft("ses_send")

	// Create drafts on both ses_send and ses_send_sub, then delete the open subagent via SSE ->
	// must close subagent back to ses_send and remove subagent draft dir.
	w.inputBuffer.SetText("root draft")
	w.flushAllDraftsSync()
	w.openSubagent(tabSend, subSend)
	w.inputBuffer.SetText("sub draft")
	w.flushAllDraftsSync()

	subDir := filepath.Join(w.resolvedDraftsDir(), "ses_send_sub")
	if _, err := os.Stat(filepath.Join(sesDir, draftFileName)); err != nil {
		t.Fatalf("expected root draft.json before sub delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(subDir, draftFileName)); err != nil {
		t.Fatalf("expected sub draft.json before sub delete: %v", err)
	}
	w.handleEvent(opencode.Event{
		Type: "session.deleted",
		Data: json.RawMessage(`{"info":{"id":"ses_send_sub"}}`),
	})
	if tabSend.activeSubID != "" || w.activeDraftKey != "ses_send" {
		t.Fatalf("expected open subagent closed to parent, activeSubID=%q activeDraftKey=%q", tabSend.activeSubID, w.activeDraftKey)
	}
	if _, err := os.Stat(subDir); !os.IsNotExist(err) {
		t.Fatalf("expected deleted subagent draft dir removed, stat err=%v", err)
	}
	if got := bufferText(w.inputBuffer); got != "root draft" {
		t.Fatalf("expected root draft restored after open subagent deleted, got %q", got)
	}

	// Simulate post-restart unloaded subagent draft on disk (root_id=ses_send), then archive ses_send -> deletes both!
	if err := os.MkdirAll(subDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, draftFileName), []byte(`{"root_id":"ses_send","text":"reloaded sub"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	w.archiveSession("ses_send")
	if _, err := os.Stat(sesDir); !os.IsNotExist(err) {
		t.Fatalf("expected archived root draft dir deleted, stat err=%v", err)
	}
	if _, err := os.Stat(subDir); !os.IsNotExist(err) {
		t.Fatalf("expected archived unloaded subagent draft dir deleted via root_id, stat err=%v", err)
	}

	// Test session.deleted SSE event deletes draft for an unloaded session,
	// clearProjectSessions deletes drafts for a whole project, and pruneArchivedDrafts sweeps stale archived dirs.
	sesUnloadedDir := filepath.Join(w.resolvedDraftsDir(), "ses_unloaded")
	if err := os.MkdirAll(sesUnloadedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(sesUnloadedDir, draftFileName), []byte(`{"text":"bye"}`), 0o644)
	w.handleEvent(opencode.Event{
		Type: "session.deleted",
		Data: json.RawMessage(`{"info":{"id":"ses_unloaded"}}`),
	})
	if _, err := os.Stat(sesUnloadedDir); !os.IsNotExist(err) {
		t.Fatalf("expected session.deleted to remove unloaded draft dir, stat err=%v", err)
	}

	w.viewStack.SetVisibleChildName("ses_other")
	w.inputBuffer.SetText("clear project draft")
	w.flushAllDraftsSync()
	otherDir := filepath.Join(w.resolvedDraftsDir(), "ses_other")
	if _, err := os.Stat(filepath.Join(otherDir, draftFileName)); err != nil {
		t.Fatalf("expected ses_other draft.json before clearProjectSessions: %v", err)
	}
	w.clearProjectSessions("/tmp/s")
	if _, err := os.Stat(otherDir); !os.IsNotExist(err) {
		t.Fatalf("expected clearProjectSessions to remove draft dir, stat err=%v", err)
	}

	staleArchDir := filepath.Join(w.resolvedDraftsDir(), "ses_stale_arch")
	if err := os.MkdirAll(staleArchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(staleArchDir, draftFileName), []byte(`{"text":"stale"}`), 0o644)
	w.archivedSessions["ses_stale_arch"] = true
	w.pruneArchivedDrafts()
	if _, err := os.Stat(staleArchDir); !os.IsNotExist(err) {
		t.Fatalf("expected pruneArchivedDrafts to remove archived draft dir, stat err=%v", err)
	}
}

func TestDraftGreeterAdoptOnSend(t *testing.T) {
	var (
		mu       sync.Mutex
		sentBody string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/session":
			_, _ = w.Write([]byte(`{"id":"ses_new_1","title":"New","location":{"directory":"/tmp/g"}}`))
		case "/api/session/ses_new_1/prompt":
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			sentBody = string(raw)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"id":"msg_1","delivery":"queue"}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	statePath := filepath.Join(t.TempDir(), "opencode", "gluppencode.json")
	w := newTestDraftWindow(t, statePath, srv.URL)

	// Type + attach on empty greeter and send.
	w.inputBuffer.SetText("from greeter")
	w.attachRawToDraft(greeterDraftKey, "g.png", "image/png", testPNGBytes, nil)
	w.sendPromptWithDelivery(opencode.DeliveryQueue)

	var body string
	pumpUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		body = sentBody
		mu.Unlock()
		return body != ""
	})
	if !strings.Contains(body, "from greeter") || !strings.Contains(body, "g.png") {
		t.Fatalf("expected adopted greeter text & file in sent prompt, got %s", body)
	}
	if greeter := w.drafts[greeterDraftKey]; greeter == nil || bufferText(greeter.buffer) != "" || len(greeter.files) != 0 {
		t.Fatalf("expected greeter draft cleared after adoption, got text=%q files=%d", bufferText(greeter.buffer), len(greeter.files))
	}
}
