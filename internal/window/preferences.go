package window

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/diamondburned/chatkit/md/hl"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4-sourceview/pkg/gtksource/v5"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotkit/app"
	"github.com/diamondburned/gotkit/gtkutil"
	"libdb.so/gluppencode/internal/opencode"
)

func (w *Window) effectiveChatWidth() int {
	return clampChatWidth(w.prefs.ChatWidth)
}

func (w *Window) effectiveMaxVisibleSessions() int {
	return clampMaxVisibleSessions(w.prefs.MaxVisibleSessions)
}

func (w *Window) effectiveShellTailLines() int {
	return clampShellTailLines(w.prefs.ShellTailLines)
}

func (w *Window) effectiveDiffTruncationLines() int {
	return clampDiffTruncationLines(w.prefs.DiffTruncationLines)
}

func (w *Window) effectiveShowTurnDiffs() bool {
	return w != nil && w.prefs.ShowTurnDiffs
}

func (w *Window) effectiveTakeChargeDirective() string {
	if s := strings.TrimSpace(w.prefs.TakeChargeDirective); s != "" {
		return s
	}
	return TakeChargeDirective
}

func (w *Window) effectiveNotifyOnFinished() bool {
	return w.prefs.NotifyOnFinished == nil || *w.prefs.NotifyOnFinished
}

func (w *Window) effectiveNotifyOnActionNeeded() bool {
	return w.prefs.NotifyOnActionNeeded == nil || *w.prefs.NotifyOnActionNeeded
}

func (w *Window) effectiveNotifyOnActiveSession() bool {
	return w.prefs.NotifyOnActiveSession
}

func (w *Window) saveAndSyncPreferences(prev Preferences) {
	w.saveState()
	if w.notifyCoord != nil {
		w.notifyCoord.syncPreferences(w, prev, w.prefs)
	}
}

func (w *Window) applyPreferenceDelta(prev, next Preferences) {
	if w == nil {
		return
	}
	w.prefs = clonePreferences(next)
	if prev.ColorScheme != next.ColorScheme {
		w.applyColorScheme()
	}
	if prev.SyntaxTheme != next.SyntaxTheme {
		w.applySyntaxTheme()
	}
	if prev.ChatWidth != next.ChatWidth {
		w.applyChatWidth()
	}
	if prev.MaxVisibleSessions != next.MaxVisibleSessions {
		w.applyMaxVisibleSessions()
	}
	if prev.ShellTailLines != next.ShellTailLines {
		w.applyShellTailLines()
	}
	if prev.DiffTruncationLines != next.DiffTruncationLines {
		w.applyDiffTruncationLines()
	}
	if prev.ShowTurnDiffs != next.ShowTurnDiffs {
		w.applyShowTurnDiffs()
	}
	if prev.CtrlEnterToSend != next.CtrlEnterToSend {
		w.applySendShortcutUI()
	}
	if prev.GreeterModelOverride != next.GreeterModelOverride ||
		prev.GreeterTitleOverride != next.GreeterTitleOverride ||
		prev.GreeterSubOverride != next.GreeterSubOverride {
		w.applyGreeterProfileToAllConvs()
	}
}

func (w *Window) applyAllPreferences() {
	w.applyColorScheme()
	w.applySyntaxTheme()
	w.applyChatWidth()
	w.applyMaxVisibleSessions()
	w.applyDiffTruncationLines()
	w.applySendShortcutUI()
	w.applyGreeterProfileToAllConvs()
	if w.client != nil && strings.TrimSpace(w.prefs.ServerURL) != "" {
		w.client.SetServerURL(w.ctx, w.prefs.ServerURL)
	}
}

func (w *Window) applyColorScheme() {
	styleMgr := adw.StyleManagerGetDefault()
	if styleMgr == nil {
		return
	}
	switch w.prefs.ColorScheme {
	case "light":
		styleMgr.SetColorScheme(adw.ColorSchemeForceLight)
	case "dark":
		styleMgr.SetColorScheme(adw.ColorSchemeForceDark)
	default:
		styleMgr.SetColorScheme(adw.ColorSchemeDefault)
	}
}

func (w *Window) currentSyntaxStyleScheme() *gtksource.StyleScheme {
	styleMgr := adw.StyleManagerGetDefault()
	schemeMgr := gtksource.StyleSchemeManagerGetDefault()
	if styleMgr == nil || schemeMgr == nil {
		return nil
	}

	var scheme *gtksource.StyleScheme
	customID := strings.TrimSpace(w.prefs.SyntaxTheme)
	if customID != "" {
		scheme = schemeMgr.Scheme(customID)
		_ = hl.ChangeStyle(customID)
	} else {
		_ = hl.ChangeStyle("")
		if styleMgr.Dark() {
			scheme = schemeMgr.Scheme("Adwaita-dark")
			if scheme == nil {
				scheme = schemeMgr.Scheme("classic-dark")
			}
		} else {
			scheme = schemeMgr.Scheme("Adwaita")
			if scheme == nil {
				scheme = schemeMgr.Scheme("classic")
			}
		}
	}
	return scheme
}

func (w *Window) applySyntaxTheme() {
	scheme := w.currentSyntaxStyleScheme()
	if w.inputBuffer != nil {
		w.inputBuffer.SetStyleScheme(scheme)
	}
	for _, d := range w.drafts {
		if d != nil && d.buffer != nil && d.buffer != w.inputBuffer {
			d.buffer.SetStyleScheme(scheme)
		}
	}
}

func (w *Window) applyChatWidth() {
	width := w.effectiveChatWidth()
	if w.composerClamp != nil {
		w.composerClamp.SetMaximumSize(width)
	}
	for _, tab := range w.tabs {
		if tab == nil {
			continue
		}
		if tab.main != nil && tab.main.clamp != nil {
			tab.main.clamp.SetMaximumSize(width)
		}
		for _, sub := range tab.subTimelines {
			if sub != nil && sub.clamp != nil {
				sub.clamp.SetMaximumSize(width)
			}
		}
	}
}

func (w *Window) applyMaxVisibleSessions() {
	maxVis := w.effectiveMaxVisibleSessions()
	prevPopulating := w.populating
	w.populating = true
	for dir, list := range w.projectSessions {
		if w.expandedProjects[dir] {
			continue
		}
		openCount := 0
		for _, id := range w.tabOrder {
			if t := w.tabs[id]; t != nil && normalizeProjectDir(t.main.session.Location.Directory) == dir {
				openCount++
			}
		}
		for _, candidate := range list {
			if openCount >= maxVis {
				break
			}
			if w.archivedSessions[candidate.ID] {
				continue
			}
			if _, exists := w.tabs[candidate.ID]; !exists {
				w.ensureSessionTab(candidate)
				openCount++
			}
		}
	}
	w.populating = prevPopulating
	w.syncSidebar()
}

func (w *Window) applyShellTailLines() {
	lines := w.effectiveShellTailLines()
	for id, text := range w.shellOutputs {
		if lbl, ok := w.shellOutputLabels[id]; ok && lbl != nil {
			lbl.SetText(FormatShellTailLines(text, lines))
		}
	}
	w.pollActiveShellOutputs()
}

func (w *Window) applyDiffTruncationLines() {
	maxLines := w.effectiveDiffTruncationLines()
	applyConv := func(conv *convTimeline) {
		if conv == nil {
			return
		}
		for _, aw := range conv.assistantMap {
			aw.SetDiffMaxLines(maxLines)
			for _, tw := range aw.tools {
				tw.SetDiffMaxLines(maxLines)
			}
		}
	}
	for _, tab := range w.tabs {
		if tab == nil {
			continue
		}
		applyConv(tab.main)
		for _, sub := range tab.subTimelines {
			applyConv(sub)
		}
	}
}

func (w *Window) applyShowTurnDiffs() {
	for _, tab := range w.tabs {
		if tab == nil {
			continue
		}
		if tab.main != nil && tab.main.loaded {
			w.updateTurnDiffs(tab.main)
		}
		for _, sub := range tab.subTimelines {
			if sub != nil && sub.loaded {
				w.updateTurnDiffs(sub)
			}
		}
	}
}

func (w *Window) applySendShortcutUI() {
	if w.sendBtn == nil {
		return
	}
	if w.composerExpanded {
		w.sendBtn.SetTooltipText("Queue Prompt (Alt+Enter) · Steer Immediately (Ctrl+Enter)")
		return
	}
	if w.prefs.CtrlEnterToSend {
		w.sendBtn.SetTooltipText("Send Message (Ctrl+Enter)")
	} else {
		w.sendBtn.SetTooltipText("Send Message (Enter)")
	}
}

func (w *Window) openPreferencesDialog() {
	if w.ApplicationWindow == nil {
		return
	}

	dlg := adw.NewPreferencesDialog()
	dlg.SetTitle("Preferences")
	dlg.SetSearchEnabled(true)

	// Page 1: Appearance & Layout
	appearancePage := adw.NewPreferencesPage()
	appearancePage.SetTitle("Appearance")
	appearancePage.SetIconName("settings-symbolic")

	// Group: Theme & Syntax
	themeGroup := adw.NewPreferencesGroup()
	themeGroup.SetTitle("Theme & Syntax")

	colorRow := adw.NewComboRow()
	colorRow.SetTitle("Color Scheme")
	colorRow.SetSubtitle("Override system light or dark appearance")
	colorModel := gtk.NewStringList([]string{"Follow System", "Light", "Dark"})
	colorRow.SetModel(colorModel)
	switch w.prefs.ColorScheme {
	case "light":
		colorRow.SetSelected(1)
	case "dark":
		colorRow.SetSelected(2)
	default:
		colorRow.SetSelected(0)
	}
	colorRow.NotifyProperty("selected", func() {
		prev := clonePreferences(w.prefs)
		switch colorRow.Selected() {
		case 1:
			w.prefs.ColorScheme = "light"
		case 2:
			w.prefs.ColorScheme = "dark"
		default:
			w.prefs.ColorScheme = ""
		}
		w.saveAndSyncPreferences(prev)
		w.applyColorScheme()
	})
	themeGroup.Add(colorRow)

	syntaxRow := adw.NewComboRow()
	syntaxRow.SetTitle("Syntax Highlighting Theme")
	syntaxRow.SetSubtitle("Color scheme for composer and Markdown code blocks")
	schemeIDs := []string{""}
	schemeLabels := []string{"Automatic (Adwaita / Monokai)"}
	if mgr := gtksource.StyleSchemeManagerGetDefault(); mgr != nil {
		ids := slices.Clone(mgr.SchemeIDs())
		slices.Sort(ids)
		for _, id := range ids {
			schemeIDs = append(schemeIDs, id)
			label := id
			if sc := mgr.Scheme(id); sc != nil && sc.Name() != "" {
				label = fmt.Sprintf("%s (%s)", sc.Name(), id)
			}
			schemeLabels = append(schemeLabels, label)
		}
	}
	syntaxRow.SetModel(gtk.NewStringList(schemeLabels))
	selectedSyntax := uint(0)
	for i, id := range schemeIDs {
		if id == w.prefs.SyntaxTheme {
			selectedSyntax = uint(i)
			break
		}
	}
	syntaxRow.SetSelected(selectedSyntax)
	syntaxRow.NotifyProperty("selected", func() {
		idx := int(syntaxRow.Selected())
		if idx >= 0 && idx < len(schemeIDs) {
			prev := clonePreferences(w.prefs)
			w.prefs.SyntaxTheme = schemeIDs[idx]
			w.saveAndSyncPreferences(prev)
			w.applySyntaxTheme()
		}
	})
	themeGroup.Add(syntaxRow)
	appearancePage.Add(themeGroup)

	// Group: Layout & Density
	layoutGroup := adw.NewPreferencesGroup()
	layoutGroup.SetTitle("Layout & Density")

	chatWidthRow := adw.NewSpinRowWithRange(float64(minChatWidth), float64(maxChatWidth), 20)
	chatWidthRow.SetTitle("Chat Width")
	chatWidthRow.SetSubtitle("Maximum width of the message timeline and composer in pixels")
	chatWidthRow.SetValue(float64(w.effectiveChatWidth()))
	chatWidthRow.NotifyProperty("value", func() {
		val := clampChatWidth(int(math.Round(chatWidthRow.Value())))
		if w.effectiveChatWidth() == val && w.prefs.ChatWidth != 0 {
			return
		}
		prev := clonePreferences(w.prefs)
		w.prefs.ChatWidth = val
		w.saveAndSyncPreferences(prev)
		w.applyChatWidth()
	})
	layoutGroup.Add(chatWidthRow)

	maxSessRow := adw.NewSpinRowWithRange(float64(minMaxVisibleSessions), float64(maxMaxVisibleSessions), 1)
	maxSessRow.SetTitle("Max Visible Sessions per Project")
	maxSessRow.SetSubtitle("Sessions shown under each project before collapsing into \"Show N more…\"")
	maxSessRow.SetValue(float64(w.effectiveMaxVisibleSessions()))
	maxSessRow.NotifyProperty("value", func() {
		val := clampMaxVisibleSessions(int(math.Round(maxSessRow.Value())))
		if w.effectiveMaxVisibleSessions() == val && w.prefs.MaxVisibleSessions != 0 {
			return
		}
		prev := clonePreferences(w.prefs)
		w.prefs.MaxVisibleSessions = val
		w.saveAndSyncPreferences(prev)
		w.applyMaxVisibleSessions()
	})
	layoutGroup.Add(maxSessRow)

	shellLinesRow := adw.NewSpinRowWithRange(float64(minShellTailLines), float64(maxShellTailLines), 1)
	shellLinesRow.SetTitle("Background Shell Tail Lines")
	shellLinesRow.SetSubtitle("Number of live output lines shown for active background shells")
	shellLinesRow.SetValue(float64(w.effectiveShellTailLines()))
	shellLinesRow.NotifyProperty("value", func() {
		val := clampShellTailLines(int(math.Round(shellLinesRow.Value())))
		if w.effectiveShellTailLines() == val && w.prefs.ShellTailLines != 0 {
			return
		}
		prev := clonePreferences(w.prefs)
		w.prefs.ShellTailLines = val
		w.saveAndSyncPreferences(prev)
		w.applyShellTailLines()
	})
	layoutGroup.Add(shellLinesRow)

	diffLinesRow := adw.NewSpinRowWithRange(float64(minDiffTruncationLines), float64(maxDiffTruncationLines), 1)
	diffLinesRow.SetTitle("Inline Diff Truncation Limit")
	diffLinesRow.SetSubtitle("Maximum lines shown in inline tool diffs before truncating")
	diffLinesRow.SetValue(float64(w.effectiveDiffTruncationLines()))
	diffLinesRow.NotifyProperty("value", func() {
		val := clampDiffTruncationLines(int(math.Round(diffLinesRow.Value())))
		if w.effectiveDiffTruncationLines() == val && w.prefs.DiffTruncationLines != 0 {
			return
		}
		prev := clonePreferences(w.prefs)
		w.prefs.DiffTruncationLines = val
		w.saveAndSyncPreferences(prev)
		w.applyDiffTruncationLines()
	})
	layoutGroup.Add(diffLinesRow)

	showTurnDiffsRow := adw.NewSwitchRow()
	showTurnDiffsRow.SetTitle("Per-Turn Workspace Diffs")
	showTurnDiffsRow.SetSubtitle("Show a summary of workspace file changes at the end of each turn")
	showTurnDiffsRow.SetActive(w.effectiveShowTurnDiffs())
	showTurnDiffsRow.NotifyProperty("active", func() {
		active := showTurnDiffsRow.Active()
		if w.prefs.ShowTurnDiffs == active {
			return
		}
		prev := clonePreferences(w.prefs)
		w.prefs.ShowTurnDiffs = active
		w.saveAndSyncPreferences(prev)
		w.applyShowTurnDiffs()
	})
	layoutGroup.Add(showTurnDiffsRow)
	appearancePage.Add(layoutGroup)

	// Group: Greeter
	greeterGroup := adw.NewPreferencesGroup()
	greeterGroup.SetTitle("Empty-State Greeter")
	greeterGroup.SetDescription("Personalized welcome headline and subtitle shown in new sessions")

	previewRow := adw.NewActionRow()
	updatePreviewRow := func() {
		prof := w.effectiveGreeterProfile()
		title := strings.TrimSpace(prof.Title)
		if title == "" {
			title = defaultGreeterTitle
		}
		sub := strings.TrimSpace(prof.Subtitle)
		if sub == "" {
			sub = defaultGreeterSubtitle
		}
		if prof.Model != "" {
			sub = fmt.Sprintf("%s · (%s)", sub, prof.Model)
		}
		previewRow.SetTitle(title)
		previewRow.SetSubtitle(sub)
	}
	updatePreviewRow()

	regenBtn := gtk.NewButtonFromIconName("refresh-symbolic")
	regenBtn.SetVAlign(gtk.AlignCenter)
	regenBtn.AddCSSClass("flat")
	regenBtn.SetTooltipText("Regenerate Greeting Prompt")
	regenBtn.ConnectClicked(func() {
		regenBtn.SetSensitive(false)
		w.refreshGreeterProfileWithCallback(true, func(_ error) {
			regenBtn.SetSensitive(true)
			updatePreviewRow()
		})
	})
	previewRow.AddSuffix(regenBtn)
	greeterGroup.Add(previewRow)

	greeterModelRow := adw.NewComboRow()
	greeterModelRow.SetTitle("Greeter Generation Model")
	greeterModelRow.SetSubtitle("Model used to synthesize the greeting from your system prompt")
	modelIDs := []string{""}
	modelLabels := []string{"Automatic (Lightweight Flash Model)"}
	for _, m := range w.models {
		if m.ProviderID == "" || m.ID == "" {
			continue
		}
		ref := m.ProviderID + "/" + m.ID
		modelIDs = append(modelIDs, ref)
		label := ref
		if m.Name != "" && m.Name != m.ID {
			label = fmt.Sprintf("%s (%s)", m.Name, ref)
		}
		modelLabels = append(modelLabels, label)
	}
	greeterModelRow.SetModel(gtk.NewStringList(modelLabels))
	selectedModelIdx := uint(0)
	for i, id := range modelIDs {
		if id == w.prefs.GreeterModelOverride {
			selectedModelIdx = uint(i)
			break
		}
	}
	greeterModelRow.SetSelected(selectedModelIdx)
	greeterModelRow.NotifyProperty("selected", func() {
		idx := int(greeterModelRow.Selected())
		if idx >= 0 && idx < len(modelIDs) {
			prev := clonePreferences(w.prefs)
			w.prefs.GreeterModelOverride = modelIDs[idx]
			w.saveAndSyncPreferences(prev)
		}
	})
	greeterGroup.Add(greeterModelRow)

	titleOverrideRow := adw.NewEntryRow()
	titleOverrideRow.SetTitle("Custom Greeting Title (Optional)")
	titleOverrideRow.SetText(w.prefs.GreeterTitleOverride)
	titleOverrideRow.ConnectChanged(func() {
		prev := clonePreferences(w.prefs)
		w.prefs.GreeterTitleOverride = strings.TrimSpace(titleOverrideRow.Text())
		w.saveAndSyncPreferences(prev)
		w.applyGreeterProfileToAllConvs()
		updatePreviewRow()
	})
	greeterGroup.Add(titleOverrideRow)

	subOverrideRow := adw.NewEntryRow()
	subOverrideRow.SetTitle("Custom Greeting Subtitle (Optional)")
	subOverrideRow.SetText(w.prefs.GreeterSubOverride)
	subOverrideRow.ConnectChanged(func() {
		prev := clonePreferences(w.prefs)
		w.prefs.GreeterSubOverride = strings.TrimSpace(subOverrideRow.Text())
		w.saveAndSyncPreferences(prev)
		w.applyGreeterProfileToAllConvs()
		updatePreviewRow()
	})
	greeterGroup.Add(subOverrideRow)
	appearancePage.Add(greeterGroup)
	dlg.Add(appearancePage)

	// Page 2: Behavior & Server
	behaviorPage := adw.NewPreferencesPage()
	behaviorPage.SetTitle("Behavior")
	behaviorPage.SetIconName("utilities-terminal-symbolic")

	composerGroup := adw.NewPreferencesGroup()
	composerGroup.SetTitle("Composer & Automation")

	sendKeyRow := adw.NewComboRow()
	sendKeyRow.SetTitle("Send Message Shortcut")
	sendKeyRow.SetSubtitle("Choose whether Enter or Ctrl+Enter sends a prompt")
	sendKeyRow.SetModel(gtk.NewStringList([]string{
		"Enter to Send (Shift+Enter for Newline, Ctrl+Enter to Steer)",
		"Ctrl+Enter to Send (Enter for Newline)",
	}))
	if w.prefs.CtrlEnterToSend {
		sendKeyRow.SetSelected(1)
	} else {
		sendKeyRow.SetSelected(0)
	}
	sendKeyRow.NotifyProperty("selected", func() {
		prev := clonePreferences(w.prefs)
		w.prefs.CtrlEnterToSend = sendKeyRow.Selected() == 1
		w.saveAndSyncPreferences(prev)
		w.applySendShortcutUI()
	})
	composerGroup.Add(sendKeyRow)

	takeChargeRow := adw.NewEntryRow()
	takeChargeRow.SetTitle("Take Charge Auto-Reply Text")
	takeChargeRow.SetText(w.prefs.TakeChargeDirective)
	takeChargeRow.ConnectChanged(func() {
		prev := clonePreferences(w.prefs)
		w.prefs.TakeChargeDirective = strings.TrimSpace(takeChargeRow.Text())
		w.saveAndSyncPreferences(prev)
	})
	resetDirectiveBtn := gtk.NewButtonFromIconName("edit-undo-symbolic")
	resetDirectiveBtn.SetVAlign(gtk.AlignCenter)
	resetDirectiveBtn.AddCSSClass("flat")
	resetDirectiveBtn.SetTooltipText("Reset to default directive")
	resetDirectiveBtn.ConnectClicked(func() {
		takeChargeRow.SetText("")
	})
	takeChargeRow.AddSuffix(resetDirectiveBtn)
	composerGroup.Add(takeChargeRow)
	behaviorPage.Add(composerGroup)

	notifyGroup := adw.NewPreferencesGroup()
	notifyGroup.SetTitle("Notifications")
	notifyGroup.SetDescription("Desktop notifications when sessions finish or wait for your input")

	notifyFinishedRow := adw.NewSwitchRow()
	notifyFinishedRow.SetTitle("Session Finished")
	notifyFinishedRow.SetSubtitle("Notify when an assistant turn finishes with a reply")
	notifyFinishedRow.SetActive(w.effectiveNotifyOnFinished())
	notifyFinishedRow.NotifyProperty("active", func() {
		val := notifyFinishedRow.Active()
		if w.effectiveNotifyOnFinished() == val && w.prefs.NotifyOnFinished != nil {
			return
		}
		prev := clonePreferences(w.prefs)
		w.prefs.NotifyOnFinished = boolPtr(val)
		w.saveAndSyncPreferences(prev)
	})
	notifyGroup.Add(notifyFinishedRow)

	notifyActionRow := adw.NewSwitchRow()
	notifyActionRow.SetTitle("Action Required")
	notifyActionRow.SetSubtitle("Notify when a session asks a question or requests permission")
	notifyActionRow.SetActive(w.effectiveNotifyOnActionNeeded())
	notifyActionRow.NotifyProperty("active", func() {
		val := notifyActionRow.Active()
		if w.effectiveNotifyOnActionNeeded() == val && w.prefs.NotifyOnActionNeeded != nil {
			return
		}
		prev := clonePreferences(w.prefs)
		w.prefs.NotifyOnActionNeeded = boolPtr(val)
		w.saveAndSyncPreferences(prev)
	})
	notifyGroup.Add(notifyActionRow)

	notifyActiveRow := adw.NewSwitchRow()
	notifyActiveRow.SetTitle("Notify for Active Tab")
	notifyActiveRow.SetSubtitle("Also send desktop notifications for the currently focused session")
	notifyActiveRow.SetActive(w.effectiveNotifyOnActiveSession())
	notifyActiveRow.NotifyProperty("active", func() {
		val := notifyActiveRow.Active()
		if w.effectiveNotifyOnActiveSession() == val {
			return
		}
		prev := clonePreferences(w.prefs)
		w.prefs.NotifyOnActiveSession = val
		w.saveAndSyncPreferences(prev)
	})
	notifyGroup.Add(notifyActiveRow)
	behaviorPage.Add(notifyGroup)

	serverGroup := adw.NewPreferencesGroup()
	serverGroup.SetTitle("OpenCode Server")
	serverGroup.SetDescription("Leave blank to automatically discover the local OpenCode service")

	serverURLRow := adw.NewEntryRow()
	serverURLRow.SetTitle("Server URL Override")
	serverURLRow.SetText(w.prefs.ServerURL)
	serverURLRow.SetShowApplyButton(true)
	applyServerURL := func() {
		prev := clonePreferences(w.prefs)
		w.prefs.ServerURL = strings.TrimSpace(serverURLRow.Text())
		w.saveAndSyncPreferences(prev)
		if w.client != nil {
			w.client.SetServerURL(w.ctx, w.prefs.ServerURL)
			w.retryConnection()
		}
	}
	serverURLRow.ConnectApply(applyServerURL)

	reconnectBtn := gtk.NewButtonFromIconName("refresh-symbolic")
	reconnectBtn.SetVAlign(gtk.AlignCenter)
	reconnectBtn.AddCSSClass("flat")
	reconnectBtn.SetTooltipText("Reconnect to OpenCode Server")
	reconnectBtn.ConnectClicked(applyServerURL)
	serverURLRow.AddSuffix(reconnectBtn)
	serverGroup.Add(serverURLRow)
	behaviorPage.Add(serverGroup)

	dlg.Add(behaviorPage)
	dlg.Present(w.ApplicationWindow)
}

func resolveTargetDirectory(baseDir, arg string) string {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return ""
	}
	if arg == "~" || strings.HasPrefix(arg, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if arg == "~" {
				arg = home
			} else {
				arg = filepath.Join(home, arg[2:])
			}
		}
	}
	if !filepath.IsAbs(arg) {
		base := strings.TrimSpace(baseDir)
		if base == "" {
			base, _ = os.Getwd()
		}
		if base != "" {
			arg = filepath.Join(base, arg)
		}
	}
	return normalizeProjectDir(arg)
}

func (w *Window) activeBaseDirectory(tab *sessionTab, conv *convTimeline) string {
	if conv != nil && conv.session.Location.Directory != "" {
		return conv.session.Location.Directory
	}
	if tab != nil && tab.main != nil && tab.main.session.Location.Directory != "" {
		return tab.main.session.Location.Directory
	}
	if len(w.projectOrder) > 0 && w.projectOrder[0] != "" {
		return w.projectOrder[0]
	}
	cwd, _ := os.Getwd()
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}
	return cwd
}

func (w *Window) changeSessionDirectory(tab *sessionTab, conv *convTimeline, arg string) {
	if tab == nil || conv == nil {
		if strings.TrimSpace(arg) == "" {
			w.promptNewProjectFolder()
			return
		}
		target := resolveTargetDirectory(w.activeBaseDirectory(nil, nil), arg)
		w.createNewSessionInDir(target)
		return
	}
	if strings.TrimSpace(arg) == "" {
		w.promptMoveSessionFolder(tab, conv)
		return
	}
	target := resolveTargetDirectory(w.activeBaseDirectory(tab, conv), arg)
	w.moveSessionToDir(tab, conv, target)
}

func (w *Window) promptMoveSessionFolder(tab *sessionTab, conv *convTimeline) {
	dialog := gtk.NewFileDialog()
	dialog.SetTitle("Move Session to Folder")
	dialog.SetModal(true)
	dialog.SelectFolder(w.ctx, &w.ApplicationWindow.Window, func(res gio.AsyncResulter) {
		file, err := dialog.SelectFolderFinish(res)
		if err != nil || file == nil {
			return
		}
		path := file.Path()
		if path == "" {
			return
		}
		w.moveSessionToDir(tab, conv, path)
	})
}

func (w *Window) moveSessionToDir(tab *sessionTab, conv *convTimeline, dir string) {
	normDir := normalizeProjectDir(dir)
	if normDir == "" || tab == nil || conv == nil || w.client == nil {
		return
	}
	sessionID := conv.session.ID
	if sessionID == "" {
		return
	}
	gtkutil.Async(w.ctx, func() func() {
		err := w.client.MoveSession(w.ctx, sessionID, normDir)
		return func() {
			if err != nil {
				slog.Error("failed to move session", "sessionID", sessionID, "dir", normDir, "err", err)
				app.Error(w.ctx, err)
				return
			}
			w.applySessionMoved(sessionID, normDir)
		}
	})
}

func (w *Window) updateSessionActivityState(sessionID string, busy bool) {
	if sessionID == "" {
		return
	}
	nowMs := time.Now().UnixMilli()
	for i := range w.allSessions {
		if w.allSessions[i].ID == sessionID {
			if busy {
				w.allSessions[i].Time.Busy = nowMs
				if nowMs <= w.allSessions[i].Time.Idle {
					w.allSessions[i].Time.Busy = w.allSessions[i].Time.Idle + 1
				}
			} else if w.allSessions[i].Time.Idle == 0 || w.allSessions[i].Time.Idle < w.allSessions[i].Time.Busy {
				w.allSessions[i].Time.Idle = nowMs
				if nowMs < w.allSessions[i].Time.Busy {
					w.allSessions[i].Time.Idle = w.allSessions[i].Time.Busy
				}
			}
			if nowMs > w.allSessions[i].Time.Updated {
				w.allSessions[i].Time.Updated = nowMs
			}
			break
		}
	}
}

func (w *Window) applySessionMoved(sessionID, newRawDir string) {
	newDir := normalizeProjectDir(newRawDir)
	if sessionID == "" || newDir == "" {
		return
	}
	loc := opencode.Location{Directory: newDir}

	if w.ensureProjectOrder(newDir) {
		w.saveState()
	}

	var oldRawDir string
	var movedSess *opencode.Session
	for i := range w.allSessions {
		if w.allSessions[i].ID == sessionID {
			oldRawDir = w.allSessions[i].Location.Directory
			w.allSessions[i].Location = loc
			s := w.allSessions[i]
			movedSess = &s
			break
		}
	}

	tab, conv := w.resolveConv(sessionID)
	if conv != nil {
		if oldRawDir == "" {
			oldRawDir = conv.session.Location.Directory
		}
		conv.session.Location = loc
		if movedSess == nil && conv == tab.main {
			s := conv.session
			movedSess = &s
		}
		if conv.loaded {
			w.reconcileTail(sessionID)
		}
	}

	w.moveSessionInProjectCache(sessionID, oldRawDir, newDir, movedSess)

	if tab != nil {
		if conv == tab.main {
			w.bumpSessionToTop(tab)
		}
		if w.currentTab() == tab {
			w.refreshHeaderAndBottomBar(tab)
		}
	} else {
		w.syncSidebar()
	}
}
