package window

import (
	"cmp"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	maxInputHistory            = 200
	defaultSidebarWidth        = 280
	minSidebarWidth            = 180
	maxSidebarWidth            = 600
	defaultChatWidth           = 920
	minChatWidth               = 600
	maxChatWidth               = 1800
	defaultMaxVisibleSessions  = 5
	minMaxVisibleSessions      = 1
	maxMaxVisibleSessions      = 50
	defaultShellTailLines      = 7
	minShellTailLines          = 3
	maxShellTailLines          = 30
	defaultDiffTruncationLines = 80
	minDiffTruncationLines     = 20
	maxDiffTruncationLines     = 500
)

func clampSidebarWidth(width int) int {
	if width <= 0 {
		return 0
	}
	if width < minSidebarWidth {
		return minSidebarWidth
	}
	if width > maxSidebarWidth {
		return maxSidebarWidth
	}
	return width
}

func clampChatWidth(width int) int {
	if width <= 0 {
		return defaultChatWidth
	}
	if width < minChatWidth {
		return minChatWidth
	}
	if width > maxChatWidth {
		return maxChatWidth
	}
	return width
}

func clampMaxVisibleSessions(n int) int {
	if n <= 0 {
		return defaultMaxVisibleSessions
	}
	if n < minMaxVisibleSessions {
		return minMaxVisibleSessions
	}
	if n > maxMaxVisibleSessions {
		return maxMaxVisibleSessions
	}
	return n
}

func clampShellTailLines(n int) int {
	if n <= 0 {
		return defaultShellTailLines
	}
	if n < minShellTailLines {
		return minShellTailLines
	}
	if n > maxShellTailLines {
		return maxShellTailLines
	}
	return n
}

func clampDiffTruncationLines(n int) int {
	if n <= 0 {
		return defaultDiffTruncationLines
	}
	if n < minDiffTruncationLines {
		return minDiffTruncationLines
	}
	if n > maxDiffTruncationLines {
		return maxDiffTruncationLines
	}
	return n
}

// Preferences stores user-configurable appearance, layout, greeter, composer, notification, and server settings.
type Preferences struct {
	ChatWidth             int    `json:"chatWidth,omitempty"`
	MaxVisibleSessions    int    `json:"maxVisibleSessions,omitempty"`
	ColorScheme           string `json:"colorScheme,omitempty"` // "" ("system"), "light", "dark"
	SyntaxTheme           string `json:"syntaxTheme,omitempty"` // "" (auto), or GtkSourceView scheme ID
	ShellTailLines        int    `json:"shellTailLines,omitempty"`
	DiffTruncationLines   int    `json:"diffTruncationLines,omitempty"`
	CtrlEnterToSend       bool   `json:"ctrlEnterToSend,omitempty"`
	TakeChargeDirective   string `json:"takeChargeDirective,omitempty"`
	GreeterModelOverride  string `json:"greeterModelOverride,omitempty"` // "" (auto), or "providerID/modelID"
	GreeterTitleOverride  string `json:"greeterTitleOverride,omitempty"`
	GreeterSubOverride    string `json:"greeterSubOverride,omitempty"`
	NotifyOnFinished      *bool  `json:"notifyOnFinished,omitempty"`     // nil => true
	NotifyOnActionNeeded  *bool  `json:"notifyOnActionNeeded,omitempty"` // nil => true
	NotifyOnActiveSession bool   `json:"notifyOnActiveSession,omitempty"`
	ServerURL             string `json:"serverURL,omitempty"`
}

func boolPtr(v bool) *bool {
	return &v
}

func clonePreferences(p Preferences) Preferences {
	if p.NotifyOnFinished != nil {
		v := *p.NotifyOnFinished
		p.NotifyOnFinished = &v
	}
	if p.NotifyOnActionNeeded != nil {
		v := *p.NotifyOnActionNeeded
		p.NotifyOnActionNeeded = &v
	}
	return p
}

func normalizePreferences(p Preferences) Preferences {
	p = clonePreferences(p)
	if p.ChatWidth != 0 {
		p.ChatWidth = clampChatWidth(p.ChatWidth)
	}
	if p.MaxVisibleSessions != 0 {
		p.MaxVisibleSessions = clampMaxVisibleSessions(p.MaxVisibleSessions)
	}
	switch strings.ToLower(strings.TrimSpace(p.ColorScheme)) {
	case "light":
		p.ColorScheme = "light"
	case "dark":
		p.ColorScheme = "dark"
	default:
		p.ColorScheme = ""
	}
	p.SyntaxTheme = strings.TrimSpace(p.SyntaxTheme)
	if p.ShellTailLines != 0 {
		p.ShellTailLines = clampShellTailLines(p.ShellTailLines)
	}
	if p.DiffTruncationLines != 0 {
		p.DiffTruncationLines = clampDiffTruncationLines(p.DiffTruncationLines)
	}
	p.TakeChargeDirective = strings.TrimSpace(p.TakeChargeDirective)
	p.GreeterModelOverride = strings.TrimSpace(p.GreeterModelOverride)
	p.GreeterTitleOverride = strings.TrimSpace(p.GreeterTitleOverride)
	p.GreeterSubOverride = strings.TrimSpace(p.GreeterSubOverride)
	p.ServerURL = strings.TrimSpace(p.ServerURL)
	return p
}

// GreeterProfile stores personalized greeter copy and extracted user communication preferences.
type GreeterProfile struct {
	SystemHash string `json:"systemHash,omitempty"`
	UserName   string `json:"userName,omitempty"`
	Pronouns   string `json:"pronouns,omitempty"`
	Tone       string `json:"tone,omitempty"`
	Title      string `json:"title,omitempty"`
	Subtitle   string `json:"subtitle,omitempty"`
	Model      string `json:"model,omitempty"`
}

// persistedState represents the local UI state stored in ~/.local/share/opencode/gluppencode.json.
type persistedState struct {
	VisibleSessions   map[string]bool `json:"visibleSessions,omitempty"`
	ArchivedSessions  map[string]bool `json:"archivedSessions,omitempty"`
	CollapsedProjects map[string]bool `json:"collapsedProjects,omitempty"`
	ProjectOrder      []string        `json:"projectOrder,omitempty"`
	InputHistory      []string        `json:"inputHistory,omitempty"`
	SidebarWidth      int             `json:"sidebarWidth,omitempty"`
	Greeter           *GreeterProfile `json:"greeter,omitempty"`
	Preferences       Preferences     `json:"preferences,omitempty"`
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
		VisibleSessions:   make(map[string]bool),
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
	cleanVisible := make(map[string]bool, len(st.VisibleSessions))
	for id, v := range st.VisibleSessions {
		if id != "" && v && !st.ArchivedSessions[id] {
			cleanVisible[id] = true
		}
	}
	st.VisibleSessions = cleanVisible
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
	if len(st.InputHistory) > maxInputHistory {
		st.InputHistory = slices.Clone(st.InputHistory[len(st.InputHistory)-maxInputHistory:])
	}
	st.SidebarWidth = clampSidebarWidth(st.SidebarWidth)
	st.Preferences = normalizePreferences(st.Preferences)
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
		return
	}
	st := loadPersistedState(w.statePath)
	w.visibleSessions = st.VisibleSessions
	w.archivedSessions = st.ArchivedSessions
	w.collapsedProjects = st.CollapsedProjects
	w.projectOrder = st.ProjectOrder
	w.inputHistory = st.InputHistory
	w.sidebarWidth = st.SidebarWidth
	w.greeterProfile = st.Greeter
	w.prefs = st.Preferences
	w.applySidebarWidth()
}

func (w *Window) saveState() {
	if w.statePath == "" {
		return
	}
	visible := make(map[string]bool)
	for id, v := range w.visibleSessions {
		if v && !w.archivedSessions[id] {
			visible[id] = true
		}
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
		VisibleSessions:   visible,
		ArchivedSessions:  archived,
		CollapsedProjects: collapsed,
		ProjectOrder:      w.projectOrder,
		InputHistory:      w.inputHistory,
		SidebarWidth:      clampSidebarWidth(w.sidebarWidth),
		Greeter:           w.greeterProfile,
		Preferences:       normalizePreferences(w.prefs),
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
