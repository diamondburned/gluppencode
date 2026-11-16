package window

import "time"

// tabSuspendAfter is how long a root session must go without activity and
// without being visited before its tab is considered suspended.
const tabSuspendAfter = 5 * time.Minute

// A suspended tab is like an archived session as far as background polling is
// concerned: reconnect resyncs and directory sweeps skip it, so the server does
// not have to (re)boot its location just to refresh a session nobody is
// watching. Nothing is torn down. Events still reach the tab, and a tab wakes
// as soon as it is visited, receives an event, or starts working again.

// touchSession records activity or a visit for a root session.
func (w *Window) touchSession(rootID string) {
	if rootID == "" {
		return
	}
	if w.sessionTouched == nil {
		w.sessionTouched = make(map[string]time.Time)
	}
	w.sessionTouched[rootID] = time.Now()
}

// recentlyTouched reports whether a root session had activity or a visit within
// tabSuspendAfter.
func (w *Window) recentlyTouched(rootID string) bool {
	at, ok := w.sessionTouched[rootID]
	return ok && time.Since(at) < tabSuspendAfter
}

// noteVisibleRoot stamps the tab being shown and the one just left, so a tab
// that was viewed for a long time does not suspend the moment it is left.
func (w *Window) noteVisibleRoot() {
	cur := w.currentRootSessionID()
	w.touchSession(w.shownRoot)
	w.touchSession(cur)
	w.shownRoot = cur
}

// resolveEventConv is resolveConv for server events: an event routed to a tab
// counts as activity and wakes it.
func (w *Window) resolveEventConv(sessionID string) (*sessionTab, *convTimeline) {
	tab, conv := w.resolveConv(sessionID)
	if tab != nil && tab.main != nil {
		w.touchSession(tab.main.session.ID)
	}
	return tab, conv
}

// tabHasPending reports whether any timeline in the tab awaits an answer.
func tabHasPending(tab *sessionTab) bool {
	pending := func(c *convTimeline) bool {
		return c != nil && (len(c.pendingForms) > 0 || len(c.pendingPermissions) > 0 || len(c.pendingPrompts) > 0)
	}
	if pending(tab.main) {
		return true
	}
	for _, sub := range tab.subTimelines {
		if pending(sub) {
			return true
		}
	}
	return false
}

// tabHasPendingInput reports whether any timeline in the tab awaits user input
// for a pending form (question) or permission request.
func tabHasPendingInput(tab *sessionTab) bool {
	if tab == nil {
		return false
	}
	pending := func(c *convTimeline) bool {
		return c != nil && (len(c.pendingForms) > 0 || len(c.pendingPermissions) > 0)
	}
	if pending(tab.main) {
		return true
	}
	for _, sub := range tab.subTimelines {
		if pending(sub) {
			return true
		}
	}
	return false
}

// isTabSuspended reports whether a tab is idle and unvisited long enough to be
// skipped by background polling.
func (w *Window) isTabSuspended(tab *sessionTab) bool {
	if tab == nil || tab.main == nil {
		return false
	}
	id := tab.main.session.ID
	if id == w.currentRootSessionID() || w.recentlyTouched(id) {
		return false
	}
	return !w.isTabWorking(tab) && !tabHasPending(tab)
}

// awakeTabDirs returns the distinct directories of tabs that are not suspended.
func (w *Window) awakeTabDirs() []string {
	var dirs []string
	seen := make(map[string]bool)
	for _, tab := range w.tabs {
		d := tab.main.session.Location.Directory
		if d == "" || seen[d] || w.isTabSuspended(tab) {
			continue
		}
		seen[d] = true
		dirs = append(dirs, d)
	}
	return dirs
}
