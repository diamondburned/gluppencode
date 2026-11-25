package window

import (
	"fmt"
	"strings"
	"sync"

	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotkit/app"
	"github.com/diamondburned/gotkit/gtkutil"
	"libdb.so/gluppencode/internal/opencode"
)

var (
	appCoordinatorsMu sync.Mutex
	appCoordinators   = make(map[uintptr]*notificationCoordinator)
)

// RegisterAppActions registers application-level actions (such as notification click activation).
func RegisterAppActions(a *app.Application) {
	if a == nil {
		return
	}
	coord := ensureAppNotificationCoordinator(a.Application)
	a.AddActionCallbacks(map[string]gtkutil.ActionCallback{
		"app.open-session": {
			ArgType: glib.NewVariantType("s"),
			Func: func(arg *glib.Variant) {
				if arg == nil {
					return
				}
				sessionID := strings.TrimSpace(arg.String())
				if sessionID == "" {
					return
				}
				coord.openSession(sessionID, func() {
					a.Activate()
				})
			},
		},
	})
}

type notificationCoordinator struct {
	gtkApp               *gtk.Application
	windows              []*Window
	sentFinished         map[string]string
	sentActionIDs        map[string]bool
	sentActionIDsByRoot  map[string][]string
	activeFinishedIDs    map[string]bool
	activeActionIDs      map[string]map[string]bool
	pendingOpenSessionID string

	notifyFunc       func(id string, notif *gio.Notification)
	withdrawFunc     func(id string)
	isActiveOverride func(*Window) bool
}

func newNotificationCoordinator(gtkApp *gtk.Application) *notificationCoordinator {
	return &notificationCoordinator{
		gtkApp:              gtkApp,
		sentFinished:        make(map[string]string),
		sentActionIDs:       make(map[string]bool),
		sentActionIDsByRoot: make(map[string][]string),
		activeFinishedIDs:   make(map[string]bool),
		activeActionIDs:     make(map[string]map[string]bool),
	}
}

func ensureAppNotificationCoordinator(gtkApp *gtk.Application) *notificationCoordinator {
	if gtkApp == nil {
		return newNotificationCoordinator(nil)
	}
	key := gtkApp.Object.Native()
	appCoordinatorsMu.Lock()
	defer appCoordinatorsMu.Unlock()
	if c, ok := appCoordinators[key]; ok {
		return c
	}
	c := newNotificationCoordinator(gtkApp)
	appCoordinators[key] = c
	return c
}

func (w *Window) ensureNotificationCoordinator() *notificationCoordinator {
	if w == nil {
		return nil
	}
	if w.notifyCoord == nil {
		w.notifyCoord = newNotificationCoordinator(nil)
		w.notifyCoord.registerWindow(w)
	}
	return w.notifyCoord
}

func (w *Window) initNotificationCoordinator(gtkApp *gtk.Application) {
	if w == nil {
		return
	}
	coord := ensureAppNotificationCoordinator(gtkApp)
	w.notifyCoord = coord
	coord.registerWindow(w)
}

func (w *Window) unregisterFromNotifications() {
	if w == nil || w.notifyCoord == nil {
		return
	}
	w.notifyCoord.unregisterWindow(w)
}

func (c *notificationCoordinator) registerWindow(w *Window) {
	if c == nil || w == nil {
		return
	}
	for _, existing := range c.windows {
		if existing == w {
			return
		}
	}
	c.windows = append(c.windows, w)
}

func (c *notificationCoordinator) unregisterWindow(w *Window) {
	if c == nil || w == nil {
		return
	}
	next := c.windows[:0]
	for _, existing := range c.windows {
		if existing != w {
			next = append(next, existing)
		}
	}
	c.windows = next
}

func (c *notificationCoordinator) isWindowActive(w *Window) bool {
	if c != nil && c.isActiveOverride != nil {
		return c.isActiveOverride(w)
	}
	if w == nil || w.ApplicationWindow == nil {
		return false
	}
	return w.IsActive()
}

func (c *notificationCoordinator) isActiveTabInFocusedWindow(rootID string) bool {
	if c == nil || rootID == "" {
		return false
	}
	for _, win := range c.windows {
		if win != nil && win.currentRootSessionID() == rootID && c.isWindowActive(win) {
			return true
		}
	}
	return false
}

func (c *notificationCoordinator) windowOwnsSession(w *Window, sessionID string) bool {
	if w == nil || sessionID == "" {
		return false
	}
	if _, ok := w.tabs[sessionID]; ok {
		return true
	}
	if _, ok := w.childToRoot[sessionID]; ok {
		return true
	}
	for _, s := range w.allSessions {
		if s.ID == sessionID {
			return true
		}
	}
	return false
}

func (c *notificationCoordinator) pickTargetWindow(sessionID string) *Window {
	if c == nil || len(c.windows) == 0 {
		return nil
	}
	var ownerFallback *Window
	for _, win := range c.windows {
		if win != nil && c.windowOwnsSession(win, sessionID) {
			if c.isWindowActive(win) {
				return win
			}
			if ownerFallback == nil {
				ownerFallback = win
			}
		}
	}
	if ownerFallback != nil {
		return ownerFallback
	}
	for _, win := range c.windows {
		if win != nil && c.isWindowActive(win) {
			return win
		}
	}
	return c.windows[0]
}

func (c *notificationCoordinator) openSession(sessionID string, activate func()) {
	if c == nil || sessionID == "" {
		return
	}
	target := c.pickTargetWindow(sessionID)
	if target == nil {
		c.pendingOpenSessionID = sessionID
		if activate != nil {
			activate()
		}
		return
	}
	if target.ApplicationWindow != nil {
		target.Present()
	}
	target.openSessionByID(sessionID)
}

func (c *notificationCoordinator) consumePendingOpenSessionID() string {
	if c == nil {
		return ""
	}
	id := c.pendingOpenSessionID
	c.pendingOpenSessionID = ""
	return id
}

func (c *notificationCoordinator) sendNotification(id string, notif *gio.Notification) {
	if c == nil || id == "" || notif == nil {
		return
	}
	if c.notifyFunc != nil {
		c.notifyFunc(id, notif)
		return
	}
	if c.gtkApp != nil && c.gtkApp.ApplicationID() != "" && c.gtkApp.IsRegistered() {
		c.gtkApp.SendNotification(id, notif)
	}
}

func (c *notificationCoordinator) withdrawNotification(id string) {
	if c == nil || id == "" {
		return
	}
	if c.withdrawFunc != nil {
		c.withdrawFunc(id)
		return
	}
	if c.gtkApp != nil && c.gtkApp.ApplicationID() != "" && c.gtkApp.IsRegistered() {
		c.gtkApp.WithdrawNotification(id)
	}
}

func finishedNotificationID(rootID string) string {
	return "finished:" + rootID
}

func formNotificationID(formID string) string {
	return "form:" + formID
}

func permNotificationID(permID string) string {
	return "perm:" + permID
}

func notificationBodyPreview(raw, fallback string, maxRunes int) string {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) == 0 {
		return fallback
	}
	collapsed := strings.Join(fields, " ")
	runes := []rune(collapsed)
	if maxRunes > 1 && len(runes) > maxRunes {
		return string(runes[:maxRunes-1]) + "…"
	}
	return collapsed
}

func sessionNotificationTitle(tab *sessionTab, fallback string) string {
	if tab != nil {
		if title := strings.TrimSpace(tab.Title()); title != "" && title != "Untitled" {
			return title
		}
		if tab.main != nil {
			if title := strings.TrimSpace(tab.main.session.Title); title != "" {
				return title
			}
		}
	}
	return fallback
}

func formNotificationPreview(form opencode.FormInfo) string {
	for _, f := range form.Fields {
		if text := strings.TrimSpace(f.Description); text != "" {
			return notificationBodyPreview(text, "Waiting for your answer", 160)
		}
		if title := strings.TrimSpace(f.Title); title != "" {
			return notificationBodyPreview(title, "Waiting for your answer", 160)
		}
	}
	if title := strings.TrimSpace(form.Title); title != "" {
		return notificationBodyPreview(title, "Waiting for your answer", 160)
	}
	return "Waiting for your answer"
}

func permNotificationPreview(perm opencode.PermissionRequest) string {
	if msg := strings.TrimSpace(perm.Message); msg != "" {
		return notificationBodyPreview(msg, "Waiting for your approval", 160)
	}
	if len(perm.Resources) > 0 {
		if perm.Action != "" {
			return notificationBodyPreview(perm.Action+": "+perm.Resources[0], "Waiting for your approval", 160)
		}
		return notificationBodyPreview(perm.Resources[0], "Waiting for your approval", 160)
	}
	if perm.Action != "" {
		return notificationBodyPreview(perm.Action, "Waiting for your approval", 160)
	}
	return "Waiting for your approval"
}

func (c *notificationCoordinator) notifyFinished(w *Window, tab *sessionTab) {
	if c == nil || w == nil || tab == nil || tab.main == nil {
		return
	}
	rootID := tab.main.session.ID
	if rootID == "" || !w.effectiveNotifyOnFinished() {
		return
	}
	if c.isActiveTabInFocusedWindow(rootID) && !w.effectiveNotifyOnActiveSession() {
		return
	}
	dedupKey := tab.turnReplyMsgID
	if dedupKey == "" {
		dedupKey = fmt.Sprintf("anon:%d", tab.turnSeq)
	}
	if c.sentFinished[rootID] == dedupKey {
		return
	}
	c.sentFinished[rootID] = dedupKey

	title := sessionNotificationTitle(tab, "Session Finished")
	body := notificationBodyPreview(tab.turnReplyPreview, "Response finished", 160)

	notif := gio.NewNotification(title)
	notif.SetBody(body)
	notif.SetPriority(gio.NotificationPriorityNormal)
	notif.SetDefaultActionAndTarget("app.open-session", glib.NewVariantString(rootID))

	c.activeFinishedIDs[rootID] = true
	c.sendNotification(finishedNotificationID(rootID), notif)
}

func (c *notificationCoordinator) notifyActionNeeded(w *Window, rootID, targetID, actionID, title, body string) {
	if c == nil || w == nil || rootID == "" || actionID == "" {
		return
	}
	if !w.effectiveNotifyOnActionNeeded() {
		return
	}
	if c.isActiveTabInFocusedWindow(rootID) && !w.effectiveNotifyOnActiveSession() {
		return
	}
	if c.sentActionIDs[actionID] {
		return
	}
	c.sentActionIDs[actionID] = true
	if c.sentActionIDsByRoot == nil {
		c.sentActionIDsByRoot = make(map[string][]string)
	}
	c.sentActionIDsByRoot[rootID] = append(c.sentActionIDsByRoot[rootID], actionID)
	if c.activeActionIDs[rootID] == nil {
		c.activeActionIDs[rootID] = make(map[string]bool)
	}
	c.activeActionIDs[rootID][actionID] = true

	if targetID == "" {
		targetID = rootID
	}
	notif := gio.NewNotification(title)
	notif.SetBody(body)
	notif.SetPriority(gio.NotificationPriorityHigh)
	notif.SetDefaultActionAndTarget("app.open-session", glib.NewVariantString(targetID))
	c.sendNotification(actionID, notif)
}

func (c *notificationCoordinator) withdrawFinished(rootID string) {
	if c == nil || rootID == "" || !c.activeFinishedIDs[rootID] {
		return
	}
	delete(c.activeFinishedIDs, rootID)
	c.withdrawNotification(finishedNotificationID(rootID))
}

func (c *notificationCoordinator) withdrawAction(rootID, actionID string) {
	if c == nil || actionID == "" {
		return
	}
	if rootID != "" {
		if set := c.activeActionIDs[rootID]; set != nil && set[actionID] {
			delete(set, actionID)
			if len(set) == 0 {
				delete(c.activeActionIDs, rootID)
			}
			c.withdrawNotification(actionID)
		}
		return
	}
	for rid, set := range c.activeActionIDs {
		if set[actionID] {
			delete(set, actionID)
			if len(set) == 0 {
				delete(c.activeActionIDs, rid)
			}
			c.withdrawNotification(actionID)
			return
		}
	}
}

func (c *notificationCoordinator) withdrawSessionNotifications(rootID string) {
	if c == nil || rootID == "" {
		return
	}
	c.withdrawFinished(rootID)
	delete(c.sentFinished, rootID)
	if set := c.activeActionIDs[rootID]; set != nil {
		for actionID := range set {
			c.withdrawNotification(actionID)
		}
		delete(c.activeActionIDs, rootID)
	}
	if ids := c.sentActionIDsByRoot[rootID]; len(ids) > 0 {
		for _, actionID := range ids {
			delete(c.sentActionIDs, actionID)
		}
		delete(c.sentActionIDsByRoot, rootID)
	}
}

func (c *notificationCoordinator) syncPreferences(sender *Window, prev, next Preferences) {
	if c == nil {
		return
	}
	prevNorm := normalizePreferences(prev)
	nextNorm := normalizePreferences(next)
	for _, win := range c.windows {
		if win == nil || win == sender {
			continue
		}
		win.applyPreferenceDelta(prevNorm, nextNorm)
	}
}

func (c *notificationCoordinator) syncLastSelection(sender *Window, agent string, model opencode.ModelRef) {
	if c == nil {
		return
	}
	for _, win := range c.windows {
		if win == nil || win == sender {
			continue
		}
		win.lastSelectedAgent = agent
		win.lastSelectedModel = model
		if win.currentTab() == nil {
			win.updateComposerAgentModel(agent, model)
			win.rebuildAgentPills()
			if len(win.models) > 0 {
				win.rebuildModelList()
			}
		}
	}
}

func (w *Window) notifySessionFinished(tab *sessionTab) {
	w.ensureNotificationCoordinator().notifyFinished(w, tab)
}

func (w *Window) notifyFormActionNeeded(tab *sessionTab, form opencode.FormInfo) {
	if tab == nil || tab.main == nil || form.ID == "" {
		return
	}
	rootID := tab.main.session.ID
	sessTitle := sessionNotificationTitle(tab, "")
	title := "Question Required"
	if sessTitle != "" {
		title = "Question · " + sessTitle
	}
	w.ensureNotificationCoordinator().notifyActionNeeded(
		w,
		rootID,
		form.SessionID,
		formNotificationID(form.ID),
		title,
		formNotificationPreview(form),
	)
}

func (w *Window) notifyPermissionActionNeeded(tab *sessionTab, perm opencode.PermissionRequest) {
	if tab == nil || tab.main == nil || perm.ID == "" {
		return
	}
	rootID := tab.main.session.ID
	sessTitle := sessionNotificationTitle(tab, "")
	title := "Permission Required"
	if sessTitle != "" {
		title = "Permission · " + sessTitle
	}
	w.ensureNotificationCoordinator().notifyActionNeeded(
		w,
		rootID,
		perm.SessionID,
		permNotificationID(perm.ID),
		title,
		permNotificationPreview(perm),
	)
}

func (w *Window) withdrawFinishedNotification(rootID string) {
	if w == nil || w.notifyCoord == nil {
		return
	}
	w.notifyCoord.withdrawFinished(rootID)
}

func (w *Window) withdrawActionNotification(rootID, actionID string) {
	if w == nil || w.notifyCoord == nil {
		return
	}
	w.notifyCoord.withdrawAction(rootID, actionID)
}

func (w *Window) withdrawSessionNotifications(rootID string) {
	if w == nil || w.notifyCoord == nil {
		return
	}
	w.notifyCoord.withdrawSessionNotifications(rootID)
}
