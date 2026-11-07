package window

import (
	"cmp"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
)

// persistedState represents the local UI state stored in ~/.local/share/opencode/gluppencode.json.
type persistedState struct {
	ArchivedSessions  map[string]bool `json:"archivedSessions,omitempty"`
	CollapsedProjects map[string]bool `json:"collapsedProjects,omitempty"`
	ProjectOrder      []string        `json:"projectOrder,omitempty"`
}

func defaultStateFilePath() string {
	if p := os.Getenv("GLUPPENCODE_STATE_FILE"); p != "" {
		return p
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "opencode", "gluppencode.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "gluppencode.json")
	}
	return filepath.Join(home, ".local", "share", "opencode", "gluppencode.json")
}

func loadPersistedState(path string) persistedState {
	st := persistedState{
		ArchivedSessions:  make(map[string]bool),
		CollapsedProjects: make(map[string]bool),
	}
	if path == "" {
		return st
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	if err := json.Unmarshal(data, &st); err != nil {
		slog.Warn("failed to parse gluppencode state file", "path", path, "err", err)
	}
	if st.ArchivedSessions == nil {
		st.ArchivedSessions = make(map[string]bool)
	}
	cleanCollapsed := make(map[string]bool, len(st.CollapsedProjects))
	for dir, v := range st.CollapsedProjects {
		if norm := normalizeProjectDir(dir); norm != "" && v {
			cleanCollapsed[norm] = true
		}
	}
	st.CollapsedProjects = cleanCollapsed
	var cleanOrder []string
	seen := make(map[string]bool, len(st.ProjectOrder))
	for _, dir := range st.ProjectOrder {
		norm := normalizeProjectDir(dir)
		if norm != "" && !seen[norm] {
			seen[norm] = true
			cleanOrder = append(cleanOrder, norm)
		}
	}
	st.ProjectOrder = cleanOrder
	return st
}

func savePersistedState(path string, st persistedState) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func (w *Window) loadState() {
	if w.statePath == "" {
		w.statePath = defaultStateFilePath()
	}
	st := loadPersistedState(w.statePath)
	w.archivedSessions = st.ArchivedSessions
	w.collapsedProjects = st.CollapsedProjects
	w.projectOrder = st.ProjectOrder
}

func (w *Window) saveState() {
	if w.statePath == "" {
		w.statePath = defaultStateFilePath()
	}
	archived := make(map[string]bool)
	for id, v := range w.archivedSessions {
		if v {
			archived[id] = true
		}
	}
	collapsed := make(map[string]bool)
	for dir, v := range w.collapsedProjects {
		if v {
			collapsed[dir] = true
		}
	}
	st := persistedState{
		ArchivedSessions:  archived,
		CollapsedProjects: collapsed,
		ProjectOrder:      w.projectOrder,
	}
	if err := savePersistedState(w.statePath, st); err != nil {
		slog.Warn("failed to save gluppencode state", "path", w.statePath, "err", err)
	}
}

// ensureProjectOrder appends any newly discovered project directories to w.projectOrder
// without reordering existing projects. Returns true if w.projectOrder was modified.
func (w *Window) ensureProjectOrder(dirs ...string) bool {
	changed := false
	for _, rawDir := range dirs {
		dir := normalizeProjectDir(rawDir)
		if dir == "" {
			continue
		}
		if !slices.Contains(w.projectOrder, dir) {
			w.projectOrder = append(w.projectOrder, dir)
			changed = true
		}
	}
	return changed
}

// compareProjectOrder compares two project directories according to w.projectOrder.
func (w *Window) compareProjectOrder(rawA, rawB string) int {
	dirA := normalizeProjectDir(rawA)
	dirB := normalizeProjectDir(rawB)
	if dirA == dirB {
		return 0
	}
	idxA := slices.Index(w.projectOrder, dirA)
	idxB := slices.Index(w.projectOrder, dirB)
	switch {
	case idxA >= 0 && idxB >= 0:
		return cmp.Compare(idxA, idxB)
	case idxA >= 0:
		return -1
	case idxB >= 0:
		return 1
	default:
		return cmp.Compare(dirA, dirB)
	}
}

// moveProjectInOrder moves srcDir to the position of dstDir in projectOrder.
func moveProjectSlice(order []string, rawSrc, rawDst string) ([]string, bool) {
	srcDir := normalizeProjectDir(rawSrc)
	dstDir := normalizeProjectDir(rawDst)
	if srcDir == "" || dstDir == "" || srcDir == dstDir {
		return order, false
	}
	srcIdx := slices.Index(order, srcDir)
	dstIdx := slices.Index(order, dstDir)
	if srcIdx < 0 || dstIdx < 0 || srcIdx == dstIdx {
		return order, false
	}
	next := slices.Clone(order)
	next = slices.Delete(next, srcIdx, srcIdx+1)
	next = slices.Insert(next, dstIdx, srcDir)
	return next, true
}
