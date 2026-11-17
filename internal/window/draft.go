package window

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/diamondburned/gotk4-sourceview/pkg/gtksource/v5"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"libdb.so/gluppencode/internal/opencode"
)

const (
	greeterDraftKey     = ""
	draftSaveDebounceMS = 75
	draftFileName       = "draft.json"
)

type draftAttachmentMeta struct {
	Name string `json:"name"`
	Mime string `json:"mime"`
	File string `json:"file"`
}

type persistedDraft struct {
	RootID      string                `json:"root_id,omitempty"`
	Text        string                `json:"text,omitempty"`
	Expanded    bool                  `json:"expanded,omitempty"`
	Attachments []draftAttachmentMeta `json:"attachments,omitempty"`
}

type composerDraft struct {
	key             string
	rootID          string
	buffer          *gtksource.Buffer
	files           []opencode.FileAttachment
	blobNames       []string
	blobTextures    []*gdk.Texture
	expanded        bool
	historyBrowsing bool
	historyIndex    int
	historyDraft    string

	loadedFromDisk bool
	suppressSave   bool
	dirty          bool
	saveTimer      glib.SourceHandle
}

type draftSnapshot struct {
	key      string
	rootID   string
	gen      uint64
	dir      string
	text     string
	expanded bool
	blobs    []draftAttachmentMeta
}

func defaultDraftsDirForState(statePath string) string {
	if strings.TrimSpace(statePath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(statePath), "gluppencode", "drafts")
}

func sanitizeDraftKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" || key == "." || key == ".." {
		return ""
	}
	if strings.ContainsAny(key, "/\\\x00") || strings.Contains(key, "..") {
		return ""
	}
	return key
}

func isSafeDraftFileName(name string) bool {
	if name == "" || name == "." || name == ".." || name == draftFileName {
		return false
	}
	if strings.ContainsAny(name, "/\\\x00") {
		return false
	}
	return filepath.Base(name) == name
}

func newDraftBlobName() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "att.bin"
	}
	return hex.EncodeToString(b[:]) + ".bin"
}

func (w *Window) resolvedDraftsDir() string {
	if w.draftsDir != "" {
		return w.draftsDir
	}
	return defaultDraftsDirForState(w.statePath)
}

func (w *Window) sessionDraftDir(key string) string {
	base := w.resolvedDraftsDir()
	safe := sanitizeDraftKey(key)
	if base == "" || safe == "" {
		return ""
	}
	return filepath.Join(base, safe)
}

func (w *Window) bumpDraftGen(key string) uint64 {
	w.draftGenMu.Lock()
	defer w.draftGenMu.Unlock()
	if w.draftWriteGen == nil {
		w.draftWriteGen = make(map[string]uint64)
	}
	w.draftWriteGen[key]++
	return w.draftWriteGen[key]
}

func (w *Window) getDraftGen(key string) uint64 {
	w.draftGenMu.Lock()
	defer w.draftGenMu.Unlock()
	return w.draftWriteGen[key]
}

func (w *Window) initComposerDrafts() {
	if w.drafts == nil {
		w.drafts = make(map[string]*composerDraft)
	}
	if w.draftWriteGen == nil {
		w.draftWriteGen = make(map[string]uint64)
	}
	if w.inputBuffer == nil && w.inputView != nil {
		w.inputBuffer = w.inputView.Buffer().Cast().(*gtksource.Buffer)
	}
	if w.inputBuffer != nil {
		d := w.newComposerDraft(greeterDraftKey, w.inputBuffer)
		d.loadedFromDisk = true
		w.drafts[greeterDraftKey] = d
		w.activeDraftKey = greeterDraftKey
	}
}

func (w *Window) newComposerDraft(key string, buf *gtksource.Buffer) *composerDraft {
	if buf == nil {
		buf = gtksource.NewBuffer(nil)
	}
	d := &composerDraft{
		key:    key,
		buffer: buf,
	}
	w.setupComposerBufferMarkdown(buf)
	if scheme := w.currentSyntaxStyleScheme(); scheme != nil {
		buf.SetStyleScheme(scheme)
	}
	w.connectComposerBufferSignals(d)
	return d
}

func (w *Window) connectComposerBufferSignals(d *composerDraft) {
	if d == nil || d.buffer == nil {
		return
	}
	d.buffer.ConnectChanged(func() {
		if w.inputBuffer == d.buffer {
			if w.composerPlaceholder != nil {
				w.composerPlaceholder.SetVisible(d.buffer.CharCount() == 0)
			}
			w.syncInputScrollPolicy()
			w.applyComposerMarkdownHighlightingToBuffer(d.buffer)
			if !w.settingHistoryText {
				w.syncComposerCompletionPopup()
			}
		} else {
			w.applyComposerMarkdownHighlightingToBuffer(d.buffer)
		}
		if !d.suppressSave {
			w.markDraftDirtyAndScheduleSave(d)
		}
	})
}

func (w *Window) activeDraft() *composerDraft {
	if w.drafts == nil {
		return nil
	}
	return w.drafts[w.activeDraftKey]
}

func (w *Window) ensureDraft(key string) *composerDraft {
	if w.drafts == nil {
		w.drafts = make(map[string]*composerDraft)
	}
	if d, ok := w.drafts[key]; ok && d != nil {
		if rootID := w.childToRoot[key]; rootID != "" {
			d.rootID = rootID
		}
		return d
	}
	var buf *gtksource.Buffer
	if key == greeterDraftKey && w.activeDraftKey == greeterDraftKey && w.inputBuffer != nil {
		buf = w.inputBuffer
	}
	d := w.newComposerDraft(key, buf)
	if rootID := w.childToRoot[key]; rootID != "" {
		d.rootID = rootID
	}
	if key == greeterDraftKey {
		d.loadedFromDisk = true
	}
	w.drafts[key] = d
	return d
}

func (w *Window) ensureAndLoadDraft(key string) *composerDraft {
	d := w.ensureDraft(key)
	if key != greeterDraftKey && !d.loadedFromDisk {
		w.loadDraftFromDisk(d)
	}
	return d
}

func (w *Window) syncWindowFromActiveDraft(d *composerDraft) {
	if d == nil {
		return
	}
	w.inputBuffer = d.buffer
	if w.inputView != nil && d.buffer != nil {
		w.inputView.SetBuffer(&d.buffer.TextBuffer)
	}
	w.ensureBufferMarkdownTags(d.buffer)
	w.pendingFiles = d.files
	w.historyBrowsing = d.historyBrowsing
	w.historyIndex = d.historyIndex
	w.historyDraft = d.historyDraft
	w.setComposerExpandedUI(d.expanded)
	if w.composerPlaceholder != nil && d.buffer != nil {
		w.composerPlaceholder.SetVisible(d.buffer.CharCount() == 0)
	}
	if w.attachmentTray != nil {
		w.rebuildAttachmentTray()
	}
}

func (w *Window) syncActiveDraftFromWindow() {
	d := w.activeDraft()
	if d == nil {
		return
	}
	d.files = w.pendingFiles
	d.expanded = w.composerExpanded
	d.historyBrowsing = w.historyBrowsing
	d.historyIndex = w.historyIndex
	d.historyDraft = w.historyDraft
}

func (w *Window) switchComposerDraft(newKey string) {
	if w.drafts == nil || w.inputView == nil {
		return
	}
	if prev := w.drafts[w.activeDraftKey]; prev != nil {
		prev.files = w.pendingFiles
		prev.expanded = w.composerExpanded
		prev.historyBrowsing = w.historyBrowsing
		prev.historyIndex = w.historyIndex
		prev.historyDraft = w.historyDraft
	}
	if w.activeDraftKey == newKey {
		if cur := w.drafts[newKey]; cur != nil && cur.buffer == w.inputBuffer {
			return
		}
	}
	if comp := w.inputView.Completion(); comp != nil {
		comp.Hide()
	}

	next := w.ensureAndLoadDraft(newKey)
	w.activeDraftKey = newKey
	w.syncWindowFromActiveDraft(next)
	if next.dirty && next.key != greeterDraftKey {
		w.markDraftDirtyAndScheduleSave(next)
	}
}

func bufferText(buf *gtksource.Buffer) string {
	if buf == nil {
		return ""
	}
	start, end := buf.Bounds()
	if start == nil || end == nil {
		return ""
	}
	return buf.Text(start, end, false)
}

func setBufferTextWithoutUndo(buf *gtksource.Buffer, text string) {
	if buf == nil {
		return
	}
	buf.BeginIrreversibleAction()
	buf.SetText(text)
	_, end := buf.Bounds()
	if end != nil {
		buf.PlaceCursor(end)
	}
	buf.EndIrreversibleAction()
}

func (w *Window) makeDraftSnapshot(d *composerDraft) draftSnapshot {
	if rootID := w.childToRoot[d.key]; rootID != "" {
		d.rootID = rootID
	}
	snap := draftSnapshot{
		key:      d.key,
		rootID:   d.rootID,
		gen:      w.bumpDraftGen(d.key),
		dir:      w.sessionDraftDir(d.key),
		text:     bufferText(d.buffer),
		expanded: d.expanded,
	}
	for i, f := range d.files {
		blobName := ""
		if i < len(d.blobNames) {
			blobName = d.blobNames[i]
		}
		if blobName == "" {
			continue
		}
		snap.blobs = append(snap.blobs, draftAttachmentMeta{
			Name: f.Name,
			Mime: f.Mime,
			File: blobName,
		})
	}
	return snap
}

func (w *Window) markDraftDirtyAndScheduleSave(d *composerDraft) {
	if d == nil || d.key == greeterDraftKey || w.sessionDraftDir(d.key) == "" {
		return
	}
	d.dirty = true
	if d.saveTimer != 0 {
		glib.SourceRemove(d.saveTimer)
		d.saveTimer = 0
	}
	d.saveTimer = glib.TimeoutAdd(draftSaveDebounceMS, func() bool {
		d.saveTimer = 0
		w.saveDraftAsync(d)
		return false
	})
}

func (w *Window) saveDraftAsync(d *composerDraft) {
	if d == nil || d.key == greeterDraftKey {
		return
	}
	dir := w.sessionDraftDir(d.key)
	if dir == "" {
		return
	}
	snap := w.makeDraftSnapshot(d)
	d.dirty = false
	go func() {
		w.persistDraftSnapshot(snap)
	}()
}

func (w *Window) saveDraftSync(d *composerDraft) {
	if d == nil || d.key == greeterDraftKey {
		return
	}
	if d.saveTimer != 0 {
		glib.SourceRemove(d.saveTimer)
		d.saveTimer = 0
	}
	dir := w.sessionDraftDir(d.key)
	if dir == "" {
		return
	}
	snap := w.makeDraftSnapshot(d)
	d.dirty = false
	w.persistDraftSnapshot(snap)
}

func (w *Window) flushAllDraftsSync() {
	if len(w.drafts) == 0 {
		return
	}
	w.syncActiveDraftFromWindow()
	for _, d := range w.drafts {
		if d == nil || d.key == greeterDraftKey {
			continue
		}
		if d.saveTimer != 0 {
			glib.SourceRemove(d.saveTimer)
			d.saveTimer = 0
			d.dirty = true
		}
		if d.dirty {
			w.saveDraftSync(d)
		}
	}
}

func cleanupOrphanedDraftFilesLocked(dir string, keep map[string]bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == draftFileName || keep[name] {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, name))
	}
}

func (w *Window) persistDraftSnapshot(snap draftSnapshot) {
	if snap.dir == "" || snap.key == greeterDraftKey {
		return
	}
	w.draftWriteMu.Lock()
	defer w.draftWriteMu.Unlock()
	w.persistDraftSnapshotLocked(snap)
}

func (w *Window) persistDraftSnapshotLocked(snap draftSnapshot) {
	if w.getDraftGen(snap.key) != snap.gen {
		return
	}

	if snap.text == "" && len(snap.blobs) == 0 {
		_ = os.RemoveAll(snap.dir)
		return
	}

	if err := os.MkdirAll(snap.dir, 0o700); err != nil {
		slog.Warn("failed to create draft directory", "dir", snap.dir, "err", err)
		return
	}

	payload := persistedDraft{
		RootID:      snap.rootID,
		Text:        snap.text,
		Expanded:    snap.expanded,
		Attachments: snap.blobs,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("failed to marshal draft", "sessionID", snap.key, "err", err)
		return
	}

	target := filepath.Join(snap.dir, draftFileName)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Warn("failed to write draft file", "path", tmp, "err", err)
		return
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		slog.Warn("failed to rename draft file", "path", target, "err", err)
		return
	}

	keep := make(map[string]bool, len(snap.blobs))
	for _, b := range snap.blobs {
		if b.File != "" {
			keep[b.File] = true
		}
	}
	cleanupOrphanedDraftFilesLocked(snap.dir, keep)
}

func (w *Window) loadDraftFromDisk(d *composerDraft) {
	if d == nil || d.loadedFromDisk {
		return
	}
	d.loadedFromDisk = true
	dir := w.sessionDraftDir(d.key)
	if dir == "" {
		return
	}

	w.draftWriteMu.Lock()
	defer w.draftWriteMu.Unlock()

	rawJSON, err := os.ReadFile(filepath.Join(dir, draftFileName))
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("failed to read draft file", "sessionID", d.key, "err", err)
		}
		_ = os.RemoveAll(dir)
		return
	}

	var loaded persistedDraft
	if err := json.Unmarshal(rawJSON, &loaded); err != nil {
		slog.Warn("failed to parse draft file", "sessionID", d.key, "err", err)
		_ = os.RemoveAll(dir)
		return
	}

	keep := make(map[string]bool, len(loaded.Attachments))
	var files []opencode.FileAttachment
	var blobNames []string
	var blobTextures []*gdk.Texture
	repaired := false

	for _, att := range loaded.Attachments {
		if !isSafeDraftFileName(att.File) {
			repaired = true
			continue
		}
		blobPath := filepath.Join(dir, att.File)
		rawBytes, err := os.ReadFile(blobPath)
		if err != nil {
			slog.Warn("missing draft attachment blob", "sessionID", d.key, "file", att.File, "err", err)
			repaired = true
			continue
		}
		keep[att.File] = true
		files = append(files, opencode.FileAttachment{
			Name: att.Name,
			Mime: att.Mime,
			Data: base64.StdEncoding.EncodeToString(rawBytes),
		})
		blobNames = append(blobNames, att.File)
		blobTextures = append(blobTextures, nil)
	}

	cleanupOrphanedDraftFilesLocked(dir, keep)

	if loaded.Text == "" && len(files) == 0 {
		_ = os.RemoveAll(dir)
		return
	}

	if d.rootID == "" && loaded.RootID != "" {
		d.rootID = loaded.RootID
	}
	d.files = files
	d.blobNames = blobNames
	d.blobTextures = blobTextures
	d.expanded = loaded.Expanded
	d.dirty = repaired

	if d.buffer != nil && loaded.Text != "" {
		d.suppressSave = true
		setBufferTextWithoutUndo(d.buffer, loaded.Text)
		d.suppressSave = false
		w.applyComposerMarkdownHighlightingToBuffer(d.buffer)
	}
}

func (w *Window) removeDraftDir(key string) {
	dir := w.sessionDraftDir(key)
	if dir == "" {
		return
	}
	gen := w.bumpDraftGen(key)
	w.draftWriteMu.Lock()
	defer w.draftWriteMu.Unlock()
	if w.getDraftGen(key) != gen {
		return
	}
	_ = os.RemoveAll(dir)
}

func (w *Window) removeDraftDirIfGen(key string, gen uint64) {
	dir := w.sessionDraftDir(key)
	if dir == "" {
		return
	}
	w.draftWriteMu.Lock()
	defer w.draftWriteMu.Unlock()
	if w.getDraftGen(key) != gen {
		return
	}
	_ = os.RemoveAll(dir)
}

func (w *Window) writeDraftBlobImmediate(key string, isNewDraft bool, raw []byte) string {
	dir := w.sessionDraftDir(key)
	if dir == "" {
		return ""
	}
	blobName := newDraftBlobName()
	gen := w.bumpDraftGen(key)
	w.draftWriteMu.Lock()
	defer w.draftWriteMu.Unlock()
	if w.getDraftGen(key) != gen {
		return ""
	}
	if isNewDraft {
		_ = os.RemoveAll(dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("failed to create draft dir for attachment", "dir", dir, "err", err)
		return ""
	}
	if err := os.WriteFile(filepath.Join(dir, blobName), raw, 0o600); err != nil {
		slog.Warn("failed to write draft attachment blob", "sessionID", key, "err", err)
		return ""
	}
	return blobName
}

func (w *Window) attachRawToDraft(key, name, mime string, raw []byte, tex *gdk.Texture) {
	if len(raw) == 0 {
		return
	}
	if mime == "" {
		mime = "image/png"
	}
	if w.drafts == nil {
		if name == "" {
			name = fmt.Sprintf("image-%d.png", len(w.pendingFiles)+1)
		}
		b64 := base64.StdEncoding.EncodeToString(raw)
		w.pendingFiles = append(w.pendingFiles, opencode.FileAttachment{
			Name: name,
			Mime: mime,
			Data: b64,
		})
		if w.attachmentTray != nil {
			w.rebuildAttachmentTray()
		}
		return
	}

	d := w.ensureAndLoadDraft(key)
	if name == "" {
		name = fmt.Sprintf("image-%d.png", len(d.files)+1)
	}

	// Write both the raw attachment blob and draft.json atomically to disk before
	// decoding thumbnails or base64-encoding the payload in memory.
	isNewDraft := len(d.blobNames) == 0 && bufferText(d.buffer) == "" && !d.expanded
	idx := len(d.files)
	d.files = append(d.files, opencode.FileAttachment{
		Name: name,
		Mime: mime,
	})
	d.blobTextures = append(d.blobTextures, nil)

	if key != greeterDraftKey {
		blobName := newDraftBlobName()
		d.blobNames = append(d.blobNames, blobName)
		if d.saveTimer != 0 {
			glib.SourceRemove(d.saveTimer)
			d.saveTimer = 0
		}
		snap := w.makeDraftSnapshot(d)
		d.dirty = false
		w.draftWriteMu.Lock()
		if isNewDraft {
			_ = os.RemoveAll(snap.dir)
		}
		if err := os.MkdirAll(snap.dir, 0o700); err != nil {
			slog.Warn("failed to create draft dir for attachment", "dir", snap.dir, "err", err)
			d.blobNames[idx] = ""
		} else if err := os.WriteFile(filepath.Join(snap.dir, blobName), raw, 0o600); err != nil {
			slog.Warn("failed to write draft attachment blob", "sessionID", key, "err", err)
			d.blobNames[idx] = ""
		} else {
			w.persistDraftSnapshotLocked(snap)
		}
		w.draftWriteMu.Unlock()
	} else {
		d.blobNames = append(d.blobNames, "")
	}

	if tex == nil {
		tex, _ = decodeTextureBytes(raw)
	}
	d.files[idx].Data = base64.StdEncoding.EncodeToString(raw)
	d.blobTextures[idx] = tex

	if key == w.activeDraftKey {
		w.pendingFiles = d.files
		if w.attachmentTray != nil {
			w.rebuildAttachmentTray()
		}
	}
}

func (w *Window) resetDraftInMemory(d *composerDraft) {
	if d == nil {
		return
	}
	d.dirty = false
	d.files = nil
	d.blobNames = nil
	d.blobTextures = nil
	d.expanded = false
	d.historyBrowsing = false
	d.historyIndex = 0
	d.historyDraft = ""
	if d.buffer != nil {
		d.suppressSave = true
		d.buffer.SetText("")
		d.suppressSave = false
	}

	if d.key == w.activeDraftKey {
		w.pendingFiles = nil
		w.historyBrowsing = false
		w.historyIndex = 0
		w.historyDraft = ""
		w.setComposerExpandedUI(false)
		if w.composerPlaceholder != nil {
			w.composerPlaceholder.SetVisible(true)
		}
		if w.attachmentTray != nil {
			w.rebuildAttachmentTray()
		}
	}
}

func (w *Window) clearDraftInMemoryForSend(key string) uint64 {
	if w.drafts == nil {
		w.clearDraft(key)
		return 0
	}
	d := w.ensureDraft(key)
	d.loadedFromDisk = true
	if key == w.activeDraftKey {
		w.syncActiveDraftFromWindow()
	}
	if d.saveTimer != 0 || d.dirty {
		w.saveDraftSync(d)
	}
	gen := w.getDraftGen(key)
	w.resetDraftInMemory(d)
	return gen
}

func (w *Window) clearDraft(key string) {
	if w.drafts == nil {
		if w.inputBuffer != nil {
			w.inputBuffer.SetText("")
		}
		w.setComposerExpanded(false)
		w.pendingFiles = nil
		if w.attachmentTray != nil {
			w.rebuildAttachmentTray()
		}
		return
	}
	d := w.ensureDraft(key)
	d.loadedFromDisk = true
	if d.saveTimer != 0 {
		glib.SourceRemove(d.saveTimer)
		d.saveTimer = 0
	}
	w.resetDraftInMemory(d)
	if key != greeterDraftKey {
		w.removeDraftDir(key)
	}
}

func (w *Window) deleteDraft(key string) {
	if key == "" {
		return
	}
	if w.drafts != nil {
		if d := w.drafts[key]; d != nil {
			if d.saveTimer != 0 {
				glib.SourceRemove(d.saveTimer)
				d.saveTimer = 0
			}
			d.dirty = false
			delete(w.drafts, key)
		}
	}
	w.removeDraftDir(key)
}

func (w *Window) readPersistedDraftRootID(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, draftFileName))
	if err != nil {
		return ""
	}
	var pd persistedDraft
	if json.Unmarshal(raw, &pd) != nil {
		return ""
	}
	return pd.RootID
}

func (w *Window) deleteSessionDraftsForTab(sessID string, tab *sessionTab) {
	w.deleteDraft(sessID)
	if tab != nil {
		for _, sub := range tab.subagents {
			if sub.ID != "" && sub.ID != sessID {
				w.deleteDraft(sub.ID)
			}
		}
		for subID := range tab.subTimelines {
			if subID != "" && subID != sessID {
				w.deleteDraft(subID)
			}
		}
	}
	for id, d := range w.drafts {
		if d != nil && d.rootID == sessID && id != sessID {
			w.deleteDraft(id)
		}
	}
	if base := w.resolvedDraftsDir(); base != "" {
		if entries, err := os.ReadDir(base); err == nil {
			for _, entry := range entries {
				name := entry.Name()
				if entry.IsDir() && name != sessID && sanitizeDraftKey(name) != "" {
					if w.readPersistedDraftRootID(filepath.Join(base, name)) == sessID {
						w.deleteDraft(name)
					}
				}
			}
		}
	}
}

func (w *Window) pruneArchivedDrafts() {
	if len(w.archivedSessions) == 0 {
		return
	}
	base := w.resolvedDraftsDir()
	if base == "" {
		return
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		if sanitizeDraftKey(id) == "" {
			continue
		}
		if w.archivedSessions[id] {
			w.deleteDraft(id)
			continue
		}
		if rootID := w.readPersistedDraftRootID(filepath.Join(base, id)); rootID != "" && w.archivedSessions[rootID] {
			w.deleteDraft(id)
		}
	}
}

func (w *Window) isLiveDraftTarget(key string) bool {
	if key == greeterDraftKey {
		return true
	}
	if w.archivedSessions[key] {
		return false
	}
	if _, ok := w.tabs[key]; ok {
		return true
	}
	if rootID, ok := w.childToRoot[key]; ok && rootID != "" {
		if w.archivedSessions[rootID] {
			return false
		}
		if _, ok := w.tabs[rootID]; ok {
			return true
		}
	}
	for _, s := range w.allSessions {
		if s.ID == key {
			return true
		}
	}
	return false
}

func (w *Window) adoptDraft(fromKey, toKey string) {
	if fromKey == toKey || w.drafts == nil {
		return
	}
	src := w.drafts[fromKey]
	if src == nil {
		return
	}
	if fromKey == w.activeDraftKey {
		w.syncActiveDraftFromWindow()
	}
	text := bufferText(src.buffer)
	files := slices.Clone(src.files)
	textures := slices.Clone(src.blobTextures)
	expanded := src.expanded
	if text == "" && len(files) == 0 && !expanded {
		return
	}

	dst := w.ensureAndLoadDraft(toKey)
	dst.loadedFromDisk = true
	if dst.buffer != nil && text != "" {
		dst.suppressSave = true
		setBufferTextWithoutUndo(dst.buffer, text)
		dst.suppressSave = false
		w.applyComposerMarkdownHighlightingToBuffer(dst.buffer)
	}
	dst.expanded = expanded

	if len(files) > 0 {
		var blobNames []string
		for _, f := range files {
			var blobName string
			if toKey != greeterDraftKey {
				if raw, err := decodeBase64Bytes(f.Data); err == nil && len(raw) > 0 {
					blobName = w.writeDraftBlobImmediate(toKey, len(blobNames) == 0 && text == "" && !expanded, raw)
				}
			}
			blobNames = append(blobNames, blobName)
		}
		dst.files = files
		dst.blobNames = blobNames
		dst.blobTextures = textures
	}

	w.clearDraft(fromKey)
	if toKey == w.activeDraftKey {
		w.syncWindowFromActiveDraft(dst)
	}
	if toKey != greeterDraftKey {
		w.saveDraftSync(dst)
	}
}

func (w *Window) restoreDraft(key, text string, files []opencode.FileAttachment) {
	if w.drafts == nil {
		if w.inputBuffer != nil {
			start, end := w.inputBuffer.Bounds()
			if strings.TrimSpace(w.inputBuffer.Text(start, end, false)) == "" && text != "" {
				w.inputBuffer.SetText(text)
			}
		}
		if len(files) > 0 && len(w.pendingFiles) == 0 {
			w.pendingFiles = slices.Clone(files)
			if w.attachmentTray != nil {
				w.rebuildAttachmentTray()
			}
		}
		return
	}
	if key != greeterDraftKey && !w.isLiveDraftTarget(key) {
		return
	}

	d := w.ensureAndLoadDraft(key)
	if key == w.activeDraftKey {
		w.syncActiveDraftFromWindow()
	}
	changed := false

	if d.buffer != nil && strings.TrimSpace(bufferText(d.buffer)) == "" && text != "" {
		d.suppressSave = true
		d.buffer.SetText(text)
		_, end := d.buffer.Bounds()
		if end != nil {
			d.buffer.PlaceCursor(end)
		}
		d.suppressSave = false
		w.applyComposerMarkdownHighlightingToBuffer(d.buffer)
		changed = true
	}

	if len(files) > 0 && len(d.files) == 0 {
		cloned := slices.Clone(files)
		blobNames := make([]string, len(cloned))
		blobTextures := make([]*gdk.Texture, len(cloned))
		for i, f := range cloned {
			if key != greeterDraftKey {
				if raw, err := decodeBase64Bytes(f.Data); err == nil && len(raw) > 0 {
					blobNames[i] = w.writeDraftBlobImmediate(key, i == 0 && bufferText(d.buffer) == "" && !d.expanded, raw)
				}
			}
		}
		d.files = cloned
		d.blobNames = blobNames
		d.blobTextures = blobTextures
		changed = true
	}

	if changed && key == w.activeDraftKey {
		w.syncWindowFromActiveDraft(d)
	}
	if changed && key != greeterDraftKey {
		w.saveDraftSync(d)
	}
}
