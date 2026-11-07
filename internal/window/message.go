package window

import (
	"context"
	_ "embed"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"github.com/diamondburned/gotkit/gtkutil/cssutil"
	"libdb.so/gluppencode/internal/mdstream"
	"libdb.so/gluppencode/internal/opencode"
)

var (
	//go:embed message.ui
	messageUI string
	//go:embed tool.ui
	toolUI string
	//go:embed style.css
	styleCSS string
)

var _ = cssutil.WriteCSS(styleCSS)

// UserMessageWidget displays a user post inside a card box.
type UserMessageWidget struct {
	*gtk.Box
	mdView    *mdstream.View
	undoBtn   *gtk.Button
	steerBtn  *gtk.Button
	cancelBtn *gtk.Button
	onUndo    func()
	onSteer   func()
	onCancel  func()
	waiting   bool
	delivery  opencode.InboxDelivery
}

// SetOnUndo sets the callback invoked when the user clicks the Undo button on this card.
func (u *UserMessageWidget) SetOnUndo(fn func()) {
	u.onUndo = fn
}

// SetOnSteer sets the callback invoked when the user clicks the Steer button on a queued card.
func (u *UserMessageWidget) SetOnSteer(fn func()) {
	u.onSteer = fn
}

// SetOnCancel sets the callback invoked when the user clicks the Cancel/withhold button on a waiting card.
func (u *UserMessageWidget) SetOnCancel(fn func()) {
	u.onCancel = fn
}

// Waiting reports whether this user message card is currently waiting in the inbox to be delivered.
func (u *UserMessageWidget) Waiting() bool {
	return u.waiting
}

// Delivery returns the current inbox delivery mode when the card is waiting.
func (u *UserMessageWidget) Delivery() opencode.InboxDelivery {
	return u.delivery
}

// SetWaiting configures whether the user prompt card is waiting to be sent (showing Steer and Cancel/withhold
// instead of Revert) or already delivered (showing the Revert button).
func (u *UserMessageWidget) SetWaiting(waiting bool, delivery opencode.InboxDelivery) {
	u.waiting = waiting
	u.delivery = delivery
	if waiting {
		u.Box.AddCSSClass("glup-user-card-queued")
		u.undoBtn.SetVisible(false)
		u.cancelBtn.SetVisible(true)
		showSteer := delivery != opencode.DeliverySteer
		u.steerBtn.SetVisible(showSteer)
		if u.mdView != nil {
			if showSteer {
				u.mdView.SetMarginEnd(104)
			} else {
				u.mdView.SetMarginEnd(32)
			}
		}
	} else {
		u.Box.RemoveCSSClass("glup-user-card-queued")
		u.steerBtn.SetVisible(false)
		u.cancelBtn.SetVisible(false)
		u.undoBtn.SetVisible(true)
		if u.mdView != nil {
			u.mdView.SetMarginEnd(26)
		}
	}
}

// NewUserMessageWidget creates a new UserMessageWidget displaying user text and file attachments.
func NewUserMessageWidget(ctx context.Context, textContent string, files ...opencode.FileAttachment) *UserMessageWidget {
	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.AddCSSClass("glup-user-card")

	overlay := gtk.NewOverlay()
	overlay.SetHExpand(true)

	contentBox := gtk.NewBox(gtk.OrientationVertical, 0)
	contentBox.SetHExpand(true)

	trimmed := strings.TrimSpace(textContent)
	var userMD *mdstream.View
	if trimmed != "" {
		userMD = mdstream.NewView(ctx)
		userMD.SetStatic(trimmed)
		userMD.SetVAlign(gtk.AlignCenter)
		userMD.SetMarginEnd(26) // reserve space so bottom-right action icons never overlap text
		contentBox.SetVAlign(gtk.AlignCenter)
		contentBox.Append(userMD)
	}

	if len(files) > 0 {
		var imgBox *adw.WrapBox
		for _, f := range files {
			if strings.HasPrefix(f.Mime, "image/") && f.Data != "" {
				tex, err := decodeBase64Texture(f.Data)
				if err == nil {
					if imgBox == nil {
						imgBox = adw.NewWrapBox()
						imgBox.SetChildSpacing(8)
						imgBox.SetLineSpacing(8)
						imgBox.SetHAlign(gtk.AlignStart)
						imgBox.SetMarginTop(6)
						imgBox.AddCSSClass("glup-user-images")
					}
					name := f.Name
					if name == "" {
						name = "Attached Image"
					}
					imgBox.Append(newImageThumbnail(ctx, name, tex))
				}
			}
		}
		if imgBox != nil {
			contentBox.Append(imgBox)
		}
	}

	overlay.SetChild(contentBox)

	actionsBox := gtk.NewBox(gtk.OrientationHorizontal, 4)
	actionsBox.SetHAlign(gtk.AlignEnd)
	actionsBox.SetVAlign(gtk.AlignEnd)

	steerBtn := gtk.NewButton()
	steerBtn.AddCSSClass("flat")
	steerBtn.AddCSSClass("glup-user-steer-btn")
	steerBtn.SetTooltipText("Steer (send immediately)")
	steerBtn.SetVAlign(gtk.AlignCenter)
	steerBtn.SetVisible(false)
	steerInner := gtk.NewBox(gtk.OrientationHorizontal, 4)
	steerInner.SetVAlign(gtk.AlignCenter)
	steerIcon := gtk.NewImageFromIconName("paper-plane-symbolic")
	steerIcon.SetPixelSize(13)
	steerLbl := gtk.NewLabel("Steer")
	steerInner.Append(steerIcon)
	steerInner.Append(steerLbl)
	steerBtn.SetChild(steerInner)

	cancelBtn := gtk.NewButtonFromIconName("window-close-symbolic")
	cancelBtn.AddCSSClass("flat")
	cancelBtn.AddCSSClass("glup-user-cancel-btn")
	cancelBtn.SetTooltipText("Cancel / withhold prompt")
	cancelBtn.SetVAlign(gtk.AlignCenter)
	cancelBtn.SetVisible(false)

	undoBtn := gtk.NewButtonFromIconName("edit-undo-symbolic")
	undoBtn.AddCSSClass("flat")
	undoBtn.AddCSSClass("glup-user-undo-btn")
	undoBtn.SetTooltipText("Revert to here")
	undoBtn.SetVAlign(gtk.AlignCenter)

	actionsBox.Append(steerBtn)
	actionsBox.Append(cancelBtn)
	actionsBox.Append(undoBtn)

	overlay.AddOverlay(actionsBox)
	box.Append(overlay)

	w := &UserMessageWidget{
		Box:       box,
		mdView:    userMD,
		undoBtn:   undoBtn,
		steerBtn:  steerBtn,
		cancelBtn: cancelBtn,
	}

	undoBtn.ConnectClicked(func() {
		if w.onUndo != nil {
			w.onUndo()
		}
	})
	steerBtn.ConnectClicked(func() {
		if w.onSteer != nil {
			w.onSteer()
		}
	})
	cancelBtn.ConnectClicked(func() {
		if w.onCancel != nil {
			w.onCancel()
		}
	})

	return w
}

// ToolWidget displays an inline tool invocation, showing file diffs expanded by default.
type ToolWidget struct {
	*gtk.Box
	ctx context.Context

	headerRow       *gtk.Box
	summaryRow      *gtk.Box
	detailsBox      *gtk.Box
	toolIcon        *gtk.Image
	titleLabel      *gtk.Label
	subtitleLabel   *gtk.Label
	badgesBox       *gtk.Box
	actionBtn       *gtk.Button
	spinner         *adw.Spinner
	statusLabel     *gtk.Label
	imageBox        *gtk.Box
	diffBox         *gtk.Box
	detailsExpander *gtk.Expander
	inputView       *gtk.TextView
	outputScroll    *gtk.ScrolledWindow
	outputView      *gtk.TextView

	inputBuf  *gtk.TextBuffer
	outputBuf *gtk.TextBuffer

	badgeWidgets []*gtk.Label
	imageWidgets []gtk.Widgetter
	diffWidgets  []*gtk.Label

	toolID             string
	toolName           string
	partStatus         string
	metaStatus         string
	isBackground       bool
	hasBgBadge         bool
	subagentClickWired bool
	shellID            string
	liveShellOutput    string
	childSessionID     string
	onOpenSubagent     func(string)
}

func isNonExpandableTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "read", "subagent", "task":
		return true
	default:
		return false
	}
}

func isSubagentTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "subagent", "task":
		return true
	default:
		return false
	}
}

// NewToolWidget creates a new ToolWidget for an inline tool invocation.
func NewToolWidget(ctx context.Context, id, name string, onOpenSubagent func(string)) *ToolWidget {
	builder := gtk.NewBuilderFromString(toolUI)
	root := builder.GetObject("rootContent").Cast().(*gtk.Box)
	headerRow := builder.GetObject("headerRow").Cast().(*gtk.Box)
	summaryRow := builder.GetObject("summaryRow").Cast().(*gtk.Box)
	toolIcon := builder.GetObject("toolIcon").Cast().(*gtk.Image)
	titleLabel := builder.GetObject("titleLabel").Cast().(*gtk.Label)
	subtitleLabel := builder.GetObject("subtitleLabel").Cast().(*gtk.Label)
	badgesBox := builder.GetObject("badgesBox").Cast().(*gtk.Box)
	actionBtn := builder.GetObject("actionBtn").Cast().(*gtk.Button)
	spinner := builder.GetObject("spinner").Cast().(*adw.Spinner)
	statusLabel := builder.GetObject("statusLabel").Cast().(*gtk.Label)
	imageBox := builder.GetObject("imageBox").Cast().(*gtk.Box)
	diffBox := builder.GetObject("diffBox").Cast().(*gtk.Box)
	detailsExpander := builder.GetObject("detailsExpander").Cast().(*gtk.Expander)
	detailsBox := builder.GetObject("detailsBox").Cast().(*gtk.Box)
	inputView := builder.GetObject("inputView").Cast().(*gtk.TextView)
	outputScroll := builder.GetObject("outputScroll").Cast().(*gtk.ScrolledWindow)
	outputView := builder.GetObject("outputView").Cast().(*gtk.TextView)

	titleLabel.SetLabel(formatToolTitle(name))
	setInitialToolIcon(toolIcon, name)

	detailsExpander.NotifyProperty("expanded", func() {
		detailsBox.SetVisible(detailsExpander.Expanded())
	})

	tw := &ToolWidget{
		Box:             root,
		ctx:             ctx,
		headerRow:       headerRow,
		summaryRow:      summaryRow,
		detailsBox:      detailsBox,
		toolIcon:        toolIcon,
		titleLabel:      titleLabel,
		subtitleLabel:   subtitleLabel,
		badgesBox:       badgesBox,
		actionBtn:       actionBtn,
		spinner:         spinner,
		statusLabel:     statusLabel,
		imageBox:        imageBox,
		diffBox:         diffBox,
		detailsExpander: detailsExpander,
		inputView:       inputView,
		outputScroll:    outputScroll,
		outputView:      outputView,
		inputBuf:        inputView.Buffer(),
		outputBuf:       outputView.Buffer(),
		toolID:          id,
		toolName:        name,
		onOpenSubagent:  onOpenSubagent,
	}

	actionBtn.ConnectClicked(func() {
		if tw.childSessionID != "" && tw.onOpenSubagent != nil {
			tw.onOpenSubagent(tw.childSessionID)
		}
	})

	tw.applyToolKind()
	return tw
}

func (t *ToolWidget) applyToolKind() {
	if isNonExpandableTool(t.toolName) && t.detailsExpander != nil && t.headerRow != nil && t.summaryRow != nil {
		t.detailsExpander.SetExpanded(false)
		t.detailsExpander.SetLabelWidget(nil)
		t.headerRow.Remove(t.detailsExpander)
		t.headerRow.Prepend(t.summaryRow)
		t.summaryRow.AddCSSClass("glup-tool-expander")
		if t.detailsBox != nil {
			t.Remove(t.detailsBox)
			t.detailsBox = nil
		}
		t.detailsExpander = nil
	}
	if isSubagentTool(t.toolName) && !t.subagentClickWired && t.headerRow != nil {
		t.subagentClickWired = true
		click := gtk.NewGestureClick()
		click.SetButton(1)
		click.ConnectReleased(func(_ int, _, _ float64) {
			if t.childSessionID != "" && t.onOpenSubagent != nil {
				t.onOpenSubagent(t.childSessionID)
			}
		})
		t.headerRow.AddController(click)
		if t.childSessionID != "" {
			t.headerRow.AddCSSClass("glup-tool-clickable")
			t.headerRow.SetCursorFromName("pointer")
		}
	}
}

func formatToolTitle(name string) string {
	switch strings.ToLower(name) {
	case "edit":
		return "Edit"
	case "write":
		return "Write"
	case "patch", "apply_patch":
		return "Patch"
	case "multiedit", "multi_edit":
		return "Multi-Edit"
	case "shell", "bash":
		return "Shell"
	case "read":
		return "Read"
	case "grep":
		return "Grep"
	case "glob":
		return "Glob"
	case "execute":
		return "Execute"
	case "subagent", "task":
		return "Subagent"
	case "question":
		return "Question"
	default:
		if len(name) > 0 {
			return strings.ToUpper(name[:1]) + name[1:]
		}
		return "Tool"
	}
}

func setInitialToolIcon(icon *gtk.Image, name string) {
	switch strings.ToLower(name) {
	case "edit", "write", "patch", "apply_patch", "multiedit", "multi_edit":
		icon.SetFromIconName("document-edit-symbolic")
	case "shell", "bash":
		icon.SetFromIconName("terminal-symbolic")
	case "read":
		icon.SetFromIconName("document-open-symbolic")
	case "grep", "glob":
		icon.SetFromIconName("system-search-symbolic")
	case "execute":
		icon.SetFromIconName("system-run-symbolic")
	case "subagent", "task":
		icon.SetFromIconName("system-users-symbolic")
	case "question":
		icon.SetFromIconName("forum-symbolic")
	default:
		icon.SetFromIconName("utilities-terminal-symbolic")
	}
}

func (t *ToolWidget) clearBadges() {
	for _, lbl := range t.badgeWidgets {
		t.badgesBox.Remove(lbl)
	}
	t.badgeWidgets = nil
	t.hasBgBadge = false
}

func (t *ToolWidget) addBadge(text, cssClass string) {
	lbl := gtk.NewLabel(text)
	lbl.AddCSSClass("glup-tool-badge")
	if cssClass != "" {
		lbl.AddCSSClass(cssClass)
	}
	t.badgesBox.Append(lbl)
	t.badgeWidgets = append(t.badgeWidgets, lbl)
}

func (t *ToolWidget) ensureBackgroundBadge() {
	if t.hasBgBadge {
		return
	}
	t.hasBgBadge = true
	t.addBadge("Background", "glup-tool-badge-bg")
}

func setViewSessionButtonContent(btn *gtk.Button) {
	box := gtk.NewBox(gtk.OrientationHorizontal, 4)
	box.SetVAlign(gtk.AlignCenter)
	lbl := gtk.NewLabel("View Session")
	icon := gtk.NewImageFromIconName("arrow-forward-symbolic")
	icon.SetPixelSize(14)
	box.Append(lbl)
	box.Append(icon)
	btn.SetChild(box)
}

func (t *ToolWidget) setChildSessionID(id string) {
	if id == "" {
		return
	}
	t.childSessionID = id
	setViewSessionButtonContent(t.actionBtn)
	t.actionBtn.SetVisible(true)
	if t.headerRow != nil {
		t.headerRow.AddCSSClass("glup-tool-clickable")
		t.headerRow.SetCursorFromName("pointer")
	}
}

// ShellID returns the background shell ID associated with this tool widget, if any.
func (t *ToolWidget) ShellID() string {
	return t.shellID
}

// ChildSessionID returns the child subagent session ID associated with this tool widget, if any.
func (t *ToolWidget) ChildSessionID() string {
	return t.childSessionID
}

// IsBackgroundRunning reports whether the tool call was moved to the background and its job is still running.
func (t *ToolWidget) IsBackgroundRunning() bool {
	return t.isBackground && t.metaStatus == "running"
}

// SetBackgroundStatus updates the background execution status (e.g. "completed" when a background job finishes).
func (t *ToolWidget) SetBackgroundStatus(status string) {
	t.metaStatus = status
	t.syncStatus()
}

// SetShellLiveOutput sets the live 7-line monospace tail output buffer for a background shell tool.
func (t *ToolWidget) SetShellLiveOutput(text string) {
	t.liveShellOutput = text
	t.outputBuf.SetText(text)
	t.outputScroll.SetVisible(true)
}

// SetMetadata updates tool metadata such as child subagent session IDs or shell IDs.
func (t *ToolWidget) SetMetadata(meta map[string]any) {
	if meta == nil {
		return
	}
	if id, ok := meta["sessionID"].(string); ok && id != "" {
		t.setChildSessionID(id)
	} else if id, ok := meta["childID"].(string); ok && id != "" {
		t.setChildSessionID(id)
	}
	if shID, ok := meta["shellID"].(string); ok && shID != "" {
		t.shellID = shID
	}
	if status, ok := meta["status"].(string); ok && status != "" {
		t.metaStatus = status
		if status == "running" && t.partStatus == "completed" {
			t.isBackground = true
			t.ensureBackgroundBadge()
		}
		t.syncStatus()
	}
}

// SetStatus updates the visual execution status of the tool.
func (t *ToolWidget) SetStatus(status string) {
	t.partStatus = status
	if status == "completed" && t.metaStatus == "running" {
		t.isBackground = true
		t.ensureBackgroundBadge()
	}
	t.syncStatus()
}

func (t *ToolWidget) syncStatus() {
	effective := t.partStatus
	if effective == "completed" && t.metaStatus == "running" {
		effective = "running"
	}
	t.statusLabel.SetLabel(effective)
	t.statusLabel.RemoveCSSClass("glup-tool-status-running")
	t.statusLabel.RemoveCSSClass("glup-tool-status-completed")
	t.statusLabel.RemoveCSSClass("glup-tool-status-error")

	switch effective {
	case "streaming", "running":
		t.spinner.SetVisible(true)
		t.statusLabel.SetVisible(true)
		t.statusLabel.AddCSSClass("glup-tool-status-running")
	case "completed":
		t.spinner.SetVisible(false)
		t.statusLabel.SetVisible(false)
	case "error", "failed":
		t.spinner.SetVisible(false)
		t.statusLabel.SetVisible(true)
		t.statusLabel.AddCSSClass("glup-tool-status-error")
	default:
		t.spinner.SetVisible(false)
		t.statusLabel.SetVisible(false)
	}
}

// SetInput sets and formats the tool's input parameters or diff view.
func (t *ToolWidget) SetInput(raw any) {
	if raw == nil {
		return
	}

	if filePath, diffText, ok := opencode.ExtractToolModification(t.toolName, raw); ok {
		t.toolIcon.SetFromIconName("document-edit-symbolic")
		t.titleLabel.SetLabel(formatToolTitle(t.toolName))
		if filePath != "" {
			t.subtitleLabel.SetLabel(shortPath(filePath))
		}
		t.clearBadges()
		added, deleted := opencode.CountDiffChanges(diffText)
		if added > 0 {
			t.addBadge(fmt.Sprintf("+%d", added), "glup-diff-badge-add")
		}
		if deleted > 0 {
			t.addBadge(fmt.Sprintf("-%d", deleted), "glup-diff-badge-del")
		}
		t.renderInlineDiff(diffText)
		t.inputView.SetVisible(false)
		return
	}

	m := opencode.ParseToolInputMap(raw)
	if bg, ok := m["background"].(bool); ok && bg {
		t.isBackground = true
	}
	t.clearBadges()
	if t.isBackground {
		t.ensureBackgroundBadge()
	}

	switch strings.ToLower(t.toolName) {
	case "subagent", "task":
		t.toolIcon.SetFromIconName("system-users-symbolic")
		agent, _ := m["agent"].(string)
		desc, _ := m["description"].(string)
		sessID, _ := m["sessionID"].(string)
		model, _ := m["model"].(string)

		if sessID != "" {
			t.titleLabel.SetLabel("Continue subagent")
			sub := desc
			if model != "" {
				if sub != "" {
					sub += " · " + model
				} else {
					sub = model
				}
			}
			t.subtitleLabel.SetLabel(sub)
			t.setChildSessionID(sessID)
		} else {
			agentTitle := "Subagent"
			if agent != "" {
				agentTitle = formatAgentDisplayName(agent) + " Subagent"
			}
			t.titleLabel.SetLabel(agentTitle)
			t.subtitleLabel.SetLabel(desc)
		}
		if t.inputView != nil {
			t.inputView.SetVisible(false)
		}

	case "shell", "bash":
		t.toolIcon.SetFromIconName("terminal-symbolic")
		t.titleLabel.SetLabel("Shell")
		cmd, _ := m["command"].(string)
		desc, _ := m["description"].(string)
		if desc != "" {
			t.subtitleLabel.SetLabel(desc)
		} else if cmd != "" {
			firstLine := strings.TrimSpace(strings.SplitN(cmd, "\n", 2)[0])
			t.subtitleLabel.SetLabel(firstLine)
		}
		if cmd != "" {
			t.inputBuf.SetText("$ " + cmd)
			t.inputView.SetVisible(true)
		} else {
			t.inputView.SetVisible(false)
		}

	case "read":
		t.toolIcon.SetFromIconName("document-open-symbolic")
		t.titleLabel.SetLabel("Read")
		path, _ := m["path"].(string)
		t.subtitleLabel.SetLabel(shortPath(path))
		offset := toInt(m["offset"])
		limit := toInt(m["limit"])
		if offset > 0 && limit > 0 {
			t.addBadge(fmt.Sprintf("lines %d–%d", offset, offset+limit), "")
		} else if limit > 0 {
			t.addBadge(fmt.Sprintf("limit=%d", limit), "")
		} else if offset > 0 {
			t.addBadge(fmt.Sprintf("offset=%d", offset), "")
		}
		t.inputView.SetVisible(false)

	case "grep", "glob":
		t.toolIcon.SetFromIconName("system-search-symbolic")
		t.titleLabel.SetLabel(formatToolTitle(t.toolName))
		pattern, _ := m["pattern"].(string)
		path, _ := m["path"].(string)
		include, _ := m["include"].(string)
		sub := pattern
		if path != "" {
			sub += " in " + shortPath(path)
		}
		if include != "" {
			sub += " include=" + include
		}
		t.subtitleLabel.SetLabel(sub)
		t.inputView.SetVisible(false)

	case "execute":
		t.toolIcon.SetFromIconName("system-run-symbolic")
		t.titleLabel.SetLabel("Execute")
		code, _ := m["code"].(string)
		firstLine := strings.TrimSpace(strings.SplitN(code, "\n", 2)[0])
		t.subtitleLabel.SetLabel(firstLine)
		if code != "" {
			t.inputBuf.SetText(code)
			t.inputView.SetVisible(true)
		} else {
			t.inputView.SetVisible(false)
		}

	case "question":
		t.toolIcon.SetFromIconName("forum-symbolic")
		t.titleLabel.SetLabel("Question")
		sub, details := formatQuestionToolInput(m)
		t.subtitleLabel.SetLabel(sub)
		if details != "" {
			t.inputBuf.SetText(details)
			t.inputView.SetVisible(true)
		} else {
			t.inputView.SetVisible(false)
		}

	default:
		t.titleLabel.SetLabel(formatToolTitle(t.toolName))
		if summary := summarizeToolInput(m); summary != "" {
			t.subtitleLabel.SetLabel(summary)
		}
		if len(m) > 0 {
			var sb strings.Builder
			for k, v := range m {
				sb.WriteString(fmt.Sprintf("%s: %v\n", k, v))
			}
			t.inputBuf.SetText(strings.TrimRight(sb.String(), "\n"))
			t.inputView.SetVisible(true)
		} else {
			t.inputView.SetVisible(false)
		}
	}
}

func shortPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(p), "/")
	if len(parts) > 3 {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	return p
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

func extractSessionID(text string) string {
	return extractPrefixedID(text, "ses_")
}

func extractShellID(text string) string {
	return extractPrefixedID(text, "sh_")
}

func extractPrefixedID(text, prefix string) string {
	idx := strings.Index(text, prefix)
	if idx == -1 {
		return ""
	}
	end := idx + len(prefix)
	for end < len(text) && ((text[end] >= 'a' && text[end] <= 'z') || (text[end] >= 'A' && text[end] <= 'Z') || (text[end] >= '0' && text[end] <= '9')) {
		end++
	}
	if end == idx+len(prefix) {
		return ""
	}
	return text[idx:end]
}

var ansiEscapeRe = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])`)

// FormatShellTailLines strips ANSI escapes, normalizes carriage returns, and returns a fixed
// maxLines-line string containing the last maxLines lines of output (padded with empty lines).
func FormatShellTailLines(raw string, maxLines int) string {
	if maxLines <= 0 {
		maxLines = 7
	}
	cleaned := ansiEscapeRe.ReplaceAllString(raw, "")
	cleaned = strings.ReplaceAll(cleaned, "\r\n", "\n")

	rawLines := strings.Split(cleaned, "\n")
	var lines []string
	for _, line := range rawLines {
		if strings.Contains(line, "\r") {
			parts := strings.Split(line, "\r")
			last := ""
			for i := len(parts) - 1; i >= 0; i-- {
				if parts[i] != "" {
					last = parts[i]
					break
				}
			}
			line = last
		}
		line = strings.ReplaceAll(line, "\t", "    ")
		if r := []rune(line); len(r) > 140 {
			line = string(r[:139]) + "…"
		}
		lines = append(lines, line)
	}

	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		lines = []string{"Waiting for output…"}
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	for len(lines) < maxLines {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func (t *ToolWidget) renderInlineDiff(diffText string) {
	for _, lbl := range t.diffWidgets {
		t.diffBox.Remove(lbl)
	}
	t.diffWidgets = nil
	diffText = strings.TrimSpace(diffText)
	if diffText == "" {
		t.diffBox.SetVisible(false)
		return
	}

	lines := strings.Split(diffText, "\n")
	const maxLines = 80
	truncated := 0
	if len(lines) > maxLines {
		truncated = len(lines) - maxLines
		lines = lines[:maxLines]
	}

	for _, line := range lines {
		lbl := gtk.NewLabel(line)
		lbl.SetXAlign(0)
		lbl.SetSelectable(true)
		lbl.SetWrap(true)
		lbl.SetWrapMode(pango.WrapWordChar)
		switch {
		case strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "@@"):
			lbl.AddCSSClass("glup-diff-line-hdr")
		case strings.HasPrefix(line, "+"):
			lbl.AddCSSClass("glup-diff-line-add")
		case strings.HasPrefix(line, "-"):
			lbl.AddCSSClass("glup-diff-line-del")
		}
		t.diffBox.Append(lbl)
		t.diffWidgets = append(t.diffWidgets, lbl)
	}
	if truncated > 0 {
		more := gtk.NewLabel(fmt.Sprintf("… (%d more lines)", truncated))
		more.SetXAlign(0)
		more.AddCSSClass("glup-diff-line-hdr")
		t.diffBox.Append(more)
		t.diffWidgets = append(t.diffWidgets, more)
	}
	t.diffBox.SetVisible(true)
}

func summarizeToolInput(m map[string]any) string {
	if m == nil {
		return ""
	}
	for _, key := range []string{"description", "query", "url", "path", "pattern", "name", "id", "action", "command"} {
		if val, ok := m[key].(string); ok && val != "" {
			val = strings.ReplaceAll(val, "\n", " ")
			runes := []rune(val)
			if len(runes) > 80 {
				val = string(runes[:80]) + "…"
			}
			return val
		}
	}
	return ""
}

func formatQuestionToolInput(m map[string]any) (subtitle, details string) {
	if m == nil {
		return "", ""
	}
	rawQs, ok := m["questions"].([]any)
	if !ok || len(rawQs) == 0 {
		return "", ""
	}

	var headers []string
	var firstQuestion string
	var sb strings.Builder

	for i, item := range rawQs {
		qm, ok := item.(map[string]any)
		if !ok {
			continue
		}
		hdr, _ := qm["header"].(string)
		qText, _ := qm["question"].(string)
		hdr = strings.TrimSpace(hdr)
		qText = strings.TrimSpace(qText)
		if hdr != "" {
			headers = append(headers, hdr)
		}
		if firstQuestion == "" && qText != "" {
			firstQuestion = qText
		}

		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		if hdr != "" && qText != "" {
			fmt.Fprintf(&sb, "%d. [%s] %s", i+1, hdr, qText)
		} else if qText != "" {
			fmt.Fprintf(&sb, "%d. %s", i+1, qText)
		} else if hdr != "" {
			fmt.Fprintf(&sb, "%d. %s", i+1, hdr)
		}

		if opts, ok := qm["options"].([]any); ok {
			for _, rawOpt := range opts {
				om, ok := rawOpt.(map[string]any)
				if !ok {
					continue
				}
				lbl, _ := om["label"].(string)
				desc, _ := om["description"].(string)
				lbl = strings.TrimSpace(lbl)
				desc = strings.TrimSpace(desc)
				if lbl == "" {
					continue
				}
				if desc != "" {
					fmt.Fprintf(&sb, "\n   • %s — %s", lbl, desc)
				} else {
					fmt.Fprintf(&sb, "\n   • %s", lbl)
				}
			}
		}
	}

	if len(rawQs) == 1 && firstQuestion != "" {
		subtitle = strings.ReplaceAll(firstQuestion, "\n", " ")
		if runes := []rune(subtitle); len(runes) > 80 {
			subtitle = string(runes[:80]) + "…"
		}
	} else if len(headers) > 0 {
		subtitle = strings.Join(headers, ", ")
	} else if len(rawQs) > 1 {
		subtitle = fmt.Sprintf("%d questions", len(rawQs))
	}

	return subtitle, sb.String()
}

// AppendInputDelta appends a streaming delta to the tool's input view.
func (t *ToolWidget) AppendInputDelta(delta string) {
	if isNonExpandableTool(t.toolName) || t.inputBuf == nil {
		return
	}
	end := t.inputBuf.EndIter()
	t.inputBuf.Insert(end, delta)
}

// SetOutput renders the tool's output content items, images, and errors.
func (t *ToolWidget) SetOutput(items []opencode.ToolContentItem, errVal any) {
	for _, img := range t.imageWidgets {
		t.imageBox.Remove(img)
	}
	t.imageWidgets = nil

	var hasImage bool
	for _, item := range items {
		if item.Type == "file" && strings.HasPrefix(item.Mime, "image/") && item.Data != "" {
			tex, err := decodeBase64Texture(item.Data)
			if err == nil {
				name := filepath.Base(item.Name)
				if name == "" || name == "." {
					name = "Image Output"
				}
				thumb := newImageThumbnail(t.ctx, name, tex)
				t.imageBox.Append(thumb)
				t.imageWidgets = append(t.imageWidgets, thumb)
				hasImage = true
			}
		}
	}
	t.imageBox.SetVisible(hasImage)

	if isSubagentTool(t.toolName) {
		if t.childSessionID == "" {
			for _, item := range items {
				if id := extractSessionID(item.Text); id != "" {
					t.setChildSessionID(id)
					break
				}
			}
		}
		return
	}
	if strings.EqualFold(t.toolName, "read") {
		return
	}

	var sb strings.Builder
	for _, item := range items {
		txt := strings.TrimSpace(item.Text)
		if txt == "" {
			continue
		}
		// Skip internal LLM instructions appended to backgrounded shell/subagent tool results.
		if strings.HasPrefix(txt, "You will be notified automatically when the command finishes.") {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(item.Text)
	}
	if errVal != nil {
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(fmt.Sprintf("Error: %v", errVal))
	}
	out := strings.TrimSpace(sb.String())
	if t.liveShellOutput != "" && errVal == nil {
		t.outputBuf.SetText(t.liveShellOutput)
		t.outputScroll.SetVisible(true)
	} else if out != "" {
		t.outputBuf.SetText(out)
		t.outputScroll.SetVisible(true)
		if errVal != nil && t.detailsExpander != nil {
			t.detailsExpander.SetVisible(true)
			t.detailsExpander.SetExpanded(true)
		}
	}

	if t.shellID == "" && (t.toolName == "shell" || t.toolName == "bash") {
		if shID := extractShellID(out); shID != "" {
			t.shellID = shID
		}
	}
}

// AssistantMessageWidget displays an assistant post inline with no author/model header.
type AssistantMessageWidget struct {
	*gtk.Box
	ctx context.Context

	partsBox *gtk.Box

	reasoningViews map[int]*mdstream.View
	textViews      map[int]*mdstream.View
	tools          map[string]*ToolWidget

	agent          string
	model          string
	onOpenSubagent func(string)
}

// NewAssistantMessageWidget creates a new AssistantMessageWidget for assistant responses.
func NewAssistantMessageWidget(ctx context.Context, agent, model string, onOpenSubagent func(string)) *AssistantMessageWidget {
	builder := gtk.NewBuilderFromString(messageUI)
	root := builder.GetObject("rootContent").Cast().(*gtk.Box)
	partsBox := builder.GetObject("partsBox").Cast().(*gtk.Box)

	w := &AssistantMessageWidget{
		Box:            root,
		ctx:            ctx,
		partsBox:       partsBox,
		reasoningViews: make(map[int]*mdstream.View),
		textViews:      make(map[int]*mdstream.View),
		tools:          make(map[string]*ToolWidget),
		agent:          agent,
		model:          model,
		onOpenSubagent: onOpenSubagent,
	}
	return w
}

// SetMeta updates the agent and model metadata on the assistant message.
func (w *AssistantMessageWidget) SetMeta(agent, model string) {
	if agent != "" {
		w.agent = agent
	}
	if model != "" {
		w.model = model
	}
}

func (w *AssistantMessageWidget) newReasoningView() *mdstream.View {
	wrap := gtk.NewBox(gtk.OrientationVertical, 0)
	wrap.AddCSSClass("glup-reasoning-inline")

	mv := mdstream.NewView(w.ctx)
	mv.AddCSSClass("glup-reasoning-text")
	wrap.Append(mv)

	w.partsBox.Append(wrap)
	return mv
}

// LoadContent initializes the message widget from an existing list of content parts.
func (w *AssistantMessageWidget) LoadContent(parts []opencode.ContentPart) {
	for i, part := range parts {
		switch part.Type {
		case "reasoning":
			if strings.TrimSpace(part.Text) == "" {
				continue
			}
			mv := w.newReasoningView()
			mv.SetStatic(part.Text)
			w.reasoningViews[i] = mv

		case "tool":
			tw := w.EnsureTool(part.ID, part.Name)
			tw.SetStatus(part.State.Status)
			tw.SetInput(part.State.Input)
			tw.SetMetadata(part.State.Metadata)
			tw.SetOutput(part.State.Content, part.State.Error)
			w.tools[part.ID] = tw

		case "text":
			if strings.TrimSpace(part.Text) == "" {
				continue
			}
			mv := mdstream.NewView(w.ctx)
			mv.SetStatic(part.Text)
			w.textViews[i] = mv
			w.partsBox.Append(mv)
		}
	}
}

// AppendReasoningDelta appends streaming delta text to a reasoning view.
func (w *AssistantMessageWidget) AppendReasoningDelta(ordinal int, delta string) {
	mv, ok := w.reasoningViews[ordinal]
	if !ok {
		mv = w.newReasoningView()
		w.reasoningViews[ordinal] = mv
	}
	mv.AppendDelta(delta)
}

// FinalizeReasoning finalizes streaming for a reasoning view with full text.
func (w *AssistantMessageWidget) FinalizeReasoning(ordinal int, fullText string) {
	mv, ok := w.reasoningViews[ordinal]
	if !ok {
		if strings.TrimSpace(fullText) == "" {
			return
		}
		mv = w.newReasoningView()
		w.reasoningViews[ordinal] = mv
	}
	mv.Finalize(fullText)
}

// AppendTextDelta appends streaming delta text to an assistant markdown view.
func (w *AssistantMessageWidget) AppendTextDelta(ordinal int, delta string) {
	mv, ok := w.textViews[ordinal]
	if !ok {
		mv = mdstream.NewView(w.ctx)
		w.textViews[ordinal] = mv
		w.partsBox.Append(mv)
	}
	mv.AppendDelta(delta)
}

// FinalizeText finalizes streaming for an assistant markdown view with full text.
func (w *AssistantMessageWidget) FinalizeText(ordinal int, fullText string) {
	mv, ok := w.textViews[ordinal]
	if !ok {
		if strings.TrimSpace(fullText) == "" {
			return
		}
		mv = mdstream.NewView(w.ctx)
		w.textViews[ordinal] = mv
		w.partsBox.Append(mv)
	}
	mv.Finalize(fullText)
}

// EnsureTool returns an existing or newly created ToolWidget for the given tool ID.
func (w *AssistantMessageWidget) EnsureTool(id, name string) *ToolWidget {
	if tw, ok := w.tools[id]; ok {
		if name != "" && (tw.toolName == "" || tw.toolName == "tool") {
			tw.toolName = name
			tw.titleLabel.SetLabel(formatToolTitle(name))
			setInitialToolIcon(tw.toolIcon, name)
			tw.applyToolKind()
		}
		return tw
	}
	if name == "" {
		name = "tool"
	}
	tw := NewToolWidget(w.ctx, id, name, w.onOpenSubagent)
	w.tools[id] = tw
	w.partsBox.Append(tw)
	return tw
}

// CloseStreams cancels and cleans up all active streaming markdown workers in this message.
func (w *AssistantMessageWidget) CloseStreams() {
	for _, mv := range w.reasoningViews {
		mv.Close()
	}
	for _, mv := range w.textViews {
		mv.Close()
	}
}

// CompactionWidget renders a compaction divider and inline summary.
type CompactionWidget struct {
	*gtk.Box
	ctx        context.Context
	titleLabel *gtk.Label
	bodyBox    *gtk.Box
	hasSummary bool
}

// NewCompactionWidget creates a new CompactionWidget displaying session compaction status.
func NewCompactionWidget(ctx context.Context, msg opencode.Message) *CompactionWidget {
	root := gtk.NewBox(gtk.OrientationVertical, 6)
	root.AddCSSClass("glup-compaction-entry")

	hdr := gtk.NewBox(gtk.OrientationHorizontal, 10)
	hdr.AddCSSClass("glup-compaction-header")

	sepLeft := gtk.NewSeparator(gtk.OrientationHorizontal)
	sepLeft.SetHExpand(true)
	sepLeft.SetVAlign(gtk.AlignCenter)

	titleLabel := gtk.NewLabel("")
	titleLabel.AddCSSClass("glup-compaction-title")

	sepRight := gtk.NewSeparator(gtk.OrientationHorizontal)
	sepRight.SetHExpand(true)
	sepRight.SetVAlign(gtk.AlignCenter)

	hdr.Append(sepLeft)
	hdr.Append(titleLabel)
	hdr.Append(sepRight)
	root.Append(hdr)

	bodyBox := gtk.NewBox(gtk.OrientationVertical, 0)
	bodyBox.AddCSSClass("glup-compaction-body")
	bodyBox.AddCSSClass("glup-compaction-summary")
	bodyBox.SetVisible(false)
	root.Append(bodyBox)

	cw := &CompactionWidget{
		Box:        root,
		ctx:        ctx,
		titleLabel: titleLabel,
		bodyBox:    bodyBox,
	}
	cw.Update(msg)
	return cw
}

// Update updates the compaction widget with the latest message state and summary.
func (cw *CompactionWidget) Update(msg opencode.Message) {
	tokStr := opencode.FormatCompactionTokens(msg.Tokens)
	if msg.Status == "running" {
		cw.titleLabel.SetText("Compacting…")
	} else if tokStr != "" {
		cw.titleLabel.SetText(fmt.Sprintf("Compaction · %s", tokStr))
	} else {
		cw.titleLabel.SetText("Compaction")
	}

	summary := strings.TrimSpace(msg.Summary)
	if summary != "" && !cw.hasSummary {
		cw.hasSummary = true
		mv := mdstream.NewView(cw.ctx)
		mv.SetStatic(summary)
		cw.bodyBox.Append(mv)
		cw.bodyBox.SetVisible(true)
	}
}

var (
	shellCmdRe        = regexp.MustCompile(`command=["']([^"']+)["']`)
	subagentSessionRe = regexp.MustCompile(`sessionID=["']([^"']+)["']`)
)

func stripSubagentTags(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "<subagent") || strings.HasPrefix(text, "<shell") {
		if idx := strings.Index(text, ">"); idx != -1 {
			text = strings.TrimSpace(text[idx+1:])
		}
	}
	if idx := strings.LastIndex(text, "</subagent>"); idx != -1 {
		text = strings.TrimSpace(text[:idx])
	} else if idx := strings.LastIndex(text, "</shell>"); idx != -1 {
		text = strings.TrimSpace(text[:idx])
	}
	return text
}

// SyntheticSubagentWidget renders a subagent or shell completion notice.
type SyntheticSubagentWidget struct {
	*gtk.Box
	expander *gtk.Expander
}

// NewSyntheticSubagentWidget creates a new SyntheticSubagentWidget for a completed subagent or shell.
func NewSyntheticSubagentWidget(_ context.Context, msg opencode.Message, onOpenSubagent func(string)) *SyntheticSubagentWidget {
	root := gtk.NewBox(gtk.OrientationVertical, 2)
	root.AddCSSClass("glup-tool-inline")

	trimmedText := strings.TrimSpace(msg.Text)
	isShell := strings.HasPrefix(trimmedText, "<shell")

	agentName := "Subagent"
	iconName := "system-users-symbolic"
	titleText := ""
	desc := msg.Description

	if isShell {
		iconName = "terminal-symbolic"
		titleText = "Shell finished"
		if matches := shellCmdRe.FindStringSubmatch(trimmedText); len(matches) > 1 {
			desc = matches[1]
		} else if desc == "" {
			desc = "Command execution completed"
		}
	} else {
		if a, ok := msg.Metadata["agent"].(string); ok && a != "" {
			agentName = formatAgentDisplayName(a)
		} else if msg.Agent != "" {
			agentName = formatAgentDisplayName(msg.Agent)
		}
		titleText = fmt.Sprintf("%s finished", agentName)
		if desc == "" {
			desc = "Completed task"
		}
	}

	var childID string
	if c, ok := msg.Metadata["childID"].(string); ok && c != "" {
		childID = c
	} else if c, ok := msg.Metadata["sessionID"].(string); ok && c != "" {
		childID = c
	} else if matches := subagentSessionRe.FindStringSubmatch(trimmedText); len(matches) > 1 {
		childID = matches[1]
	} else {
		childID = extractSessionID(trimmedText)
	}

	summaryRow := gtk.NewBox(gtk.OrientationHorizontal, 6)
	summaryRow.SetHExpand(true)

	subArrow := gtk.NewImageFromIconName("subdirectory-arrow-right-symbolic")
	subArrow.AddCSSClass("glup-tool-icon")
	subArrow.SetVAlign(gtk.AlignCenter)

	icon := gtk.NewImageFromIconName(iconName)
	icon.AddCSSClass("glup-tool-icon")
	icon.SetVAlign(gtk.AlignCenter)

	titleLbl := gtk.NewLabel(titleText)
	titleLbl.AddCSSClass("glup-tool-title")
	titleLbl.SetXAlign(0)
	titleLbl.SetVAlign(gtk.AlignCenter)

	descLbl := gtk.NewLabel(desc)
	descLbl.SetHExpand(true)
	descLbl.SetXAlign(0)
	descLbl.SetVAlign(gtk.AlignCenter)
	descLbl.SetEllipsize(pango.EllipsizeEnd)
	descLbl.AddCSSClass("glup-tool-subtitle")

	summaryRow.Append(subArrow)
	summaryRow.Append(icon)
	summaryRow.Append(titleLbl)
	summaryRow.Append(descLbl)

	headerRow := gtk.NewBox(gtk.OrientationHorizontal, 6)
	headerRow.SetHExpand(true)

	var expander *gtk.Expander
	if isShell {
		expander = gtk.NewExpander("")
		expander.AddCSSClass("glup-tool-expander")
		expander.SetHExpand(true)
		expander.SetVAlign(gtk.AlignCenter)
		expander.SetLabelWidget(summaryRow)
		headerRow.Append(expander)
	} else {
		summaryRow.AddCSSClass("glup-tool-expander")
		summaryRow.SetVAlign(gtk.AlignCenter)
		headerRow.Append(summaryRow)
	}

	state := strings.ToLower(strings.TrimSpace(msg.Status))
	if state == "" {
		state = "completed"
	}
	statusLabel := gtk.NewLabel(state)
	statusLabel.SetVAlign(gtk.AlignCenter)
	statusLabel.AddCSSClass("glup-tool-status")
	if state == "completed" {
		statusLabel.SetVisible(false)
	} else {
		statusLabel.SetVisible(true)
		if state == "running" || state == "streaming" {
			statusLabel.AddCSSClass("glup-tool-status-running")
		} else if state == "error" || state == "failed" {
			statusLabel.AddCSSClass("glup-tool-status-error")
		}
	}
	headerRow.Append(statusLabel)

	if !isShell && childID != "" && onOpenSubagent != nil {
		headerRow.AddCSSClass("glup-tool-clickable")
		headerRow.SetCursorFromName("pointer")
		click := gtk.NewGestureClick()
		click.SetButton(1)
		click.ConnectReleased(func(_ int, _, _ float64) {
			onOpenSubagent(childID)
		})
		headerRow.AddController(click)

		btn := gtk.NewButton()
		setViewSessionButtonContent(btn)
		btn.SetVAlign(gtk.AlignCenter)
		btn.AddCSSClass("flat")
		btn.AddCSSClass("glup-tool-action-btn")
		btn.ConnectClicked(func() {
			onOpenSubagent(childID)
		})
		headerRow.Append(btn)
	}

	root.Append(headerRow)

	if isShell && expander != nil {
		body := stripSubagentTags(msg.Text)
		if body != "" {
			bodyBox := gtk.NewBox(gtk.OrientationVertical, 4)
			bodyBox.AddCSSClass("glup-tool-body")
			bodyBox.SetMarginTop(4)
			bodyBox.SetVisible(false)
			tv := gtk.NewTextView()
			tv.SetEditable(false)
			tv.SetCursorVisible(false)
			tv.SetMonospace(true)
			tv.SetWrapMode(gtk.WrapWordChar)
			tv.Buffer().SetText(body)
			scroll := gtk.NewScrolledWindow()
			scroll.SetHExpand(true)
			scroll.SetMaxContentHeight(200)
			scroll.SetPropagateNaturalHeight(true)
			scroll.SetChild(tv)
			bodyBox.Append(scroll)
			expander.NotifyProperty("expanded", func() {
				bodyBox.SetVisible(expander.Expanded())
			})
			root.Append(bodyBox)
		}
	}

	return &SyntheticSubagentWidget{
		Box:      root,
		expander: expander,
	}
}

// isSubagentOrShellSynthetic reports whether a synthetic message represents a completed
// background subagent or shell job rather than a general system notice.
func isSubagentOrShellSynthetic(msg opencode.Message) bool {
	if msg.Metadata != nil {
		if src, _ := msg.Metadata["source"].(string); src == "subagent" || src == "shell" {
			return true
		}
		if c, _ := msg.Metadata["childID"].(string); c != "" {
			return true
		}
		if s, _ := msg.Metadata["shellID"].(string); s != "" {
			return true
		}
	}
	trimmed := strings.TrimSpace(msg.Text)
	return strings.HasPrefix(trimmed, "<subagent") || strings.HasPrefix(trimmed, "<shell")
}

// SystemNoticeWidget renders an inline system/synthetic/skill notice in the conversation timeline
// (e.g. "Continuing after restart", "Instructions updated: core/codemode, ...").
type SystemNoticeWidget struct {
	*gtk.Box
	expander *gtk.Expander
}

func formatSystemNoticeSummary(msg opencode.Message) (iconName, title, subtitle string) {
	iconName = "channel-broadcast-symbolic"
	if msg.Type == "skill" {
		iconName = "arrow-forward-symbolic"
		skillName := strings.TrimSpace(msg.Name)
		if skillName == "" {
			skillName = strings.TrimSpace(msg.Skill)
		}
		if skillName != "" {
			return iconName, "Skill", skillName
		}
		return iconName, "Skill", ""
	}

	noticeKind, _ := msg.Metadata["notice"].(string)
	if noticeKind == "restart" {
		iconName = "refresh-symbolic"
	}

	desc := strings.TrimSpace(msg.Description)
	if desc == "" {
		if noticeKind == "restart" {
			desc = "Continuing after restart"
		} else if msg.Type == "system" || noticeKind == "instructions" {
			var sources []string
			if rawSrcs, ok := msg.Metadata["instructionSources"].([]any); ok {
				for _, s := range rawSrcs {
					if str, ok := s.(string); ok && strings.TrimSpace(str) != "" {
						sources = append(sources, strings.TrimSpace(str))
					}
				}
			}
			if len(sources) > 0 {
				desc = "Instructions updated: " + strings.Join(sources, ", ")
			} else {
				desc = "Instructions updated"
			}
		} else if line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(msg.Text), "\n", 2)[0]); line != "" {
			desc = line
		} else {
			desc = "System notice"
		}
	}

	if strings.Contains(strings.ToLower(desc), "restart") {
		iconName = "refresh-symbolic"
	}

	if prefix, rest, ok := strings.Cut(desc, ":"); ok && strings.TrimSpace(rest) != "" {
		return iconName, strings.TrimSpace(prefix) + ":", strings.TrimSpace(rest)
	}
	return iconName, desc, ""
}

// NewSystemNoticeWidget creates an inline notice widget for a system, general synthetic, or skill message.
func NewSystemNoticeWidget(ctx context.Context, msg opencode.Message) *SystemNoticeWidget {
	root := gtk.NewBox(gtk.OrientationVertical, 2)
	root.AddCSSClass("glup-tool-inline")
	root.AddCSSClass("glup-system-notice")

	iconName, titleText, subtitleText := formatSystemNoticeSummary(msg)

	summaryRow := gtk.NewBox(gtk.OrientationHorizontal, 6)
	summaryRow.SetHExpand(true)

	icon := gtk.NewImageFromIconName(iconName)
	icon.AddCSSClass("glup-tool-icon")
	icon.SetVAlign(gtk.AlignCenter)
	summaryRow.Append(icon)

	titleLbl := gtk.NewLabel(titleText)
	titleLbl.AddCSSClass("glup-tool-title")
	titleLbl.SetXAlign(0)
	titleLbl.SetVAlign(gtk.AlignCenter)
	summaryRow.Append(titleLbl)

	if subtitleText != "" {
		subLbl := gtk.NewLabel(subtitleText)
		subLbl.AddCSSClass("glup-tool-subtitle")
		subLbl.SetHExpand(true)
		subLbl.SetXAlign(0)
		subLbl.SetVAlign(gtk.AlignCenter)
		subLbl.SetEllipsize(pango.EllipsizeEnd)
		summaryRow.Append(subLbl)
	}

	body := strings.TrimSpace(msg.Text)
	if body == "" || body == titleText {
		summaryRow.SetMarginStart(4)
		root.Append(summaryRow)
		return &SystemNoticeWidget{Box: root}
	}

	expander := gtk.NewExpander("")
	expander.AddCSSClass("glup-tool-expander")
	expander.SetHExpand(true)
	expander.SetVAlign(gtk.AlignCenter)
	expander.SetLabelWidget(summaryRow)
	root.Append(expander)

	bodyBox := gtk.NewBox(gtk.OrientationVertical, 4)
	bodyBox.AddCSSClass("glup-tool-body")
	bodyBox.SetMarginTop(4)
	bodyBox.SetVisible(false)

	tv := gtk.NewTextView()
	tv.SetEditable(false)
	tv.SetCursorVisible(false)
	tv.SetMonospace(true)
	tv.SetWrapMode(gtk.WrapWordChar)
	tv.Buffer().SetText(body)

	scroll := gtk.NewScrolledWindow()
	scroll.SetMaxContentHeight(220)
	scroll.SetPropagateNaturalHeight(true)
	scroll.SetChild(tv)
	bodyBox.Append(scroll)

	expander.NotifyProperty("expanded", func() {
		bodyBox.SetVisible(expander.Expanded())
	})
	root.Append(bodyBox)

	return &SystemNoticeWidget{
		Box:      root,
		expander: expander,
	}
}

// AgentSwitchedWidget renders a single-line subtle notice when the agent is changed.
type AgentSwitchedWidget struct {
	*gtk.Box
}

// NewAgentSwitchedWidget creates a new notice indicating an agent switch.
func NewAgentSwitchedWidget(msg opencode.Message) *AgentSwitchedWidget {
	root := gtk.NewBox(gtk.OrientationHorizontal, 6)
	root.AddCSSClass("glup-timeline-notice")

	var text string
	if msg.Previous != "" {
		text = fmt.Sprintf("Switched agent from %s to %s", formatAgentDisplayName(msg.Previous), formatAgentDisplayName(msg.Agent))
	} else {
		text = fmt.Sprintf("Agent: %s", formatAgentDisplayName(msg.Agent))
	}

	lbl := gtk.NewLabel(text)
	lbl.SetXAlign(0)
	root.Append(lbl)

	return &AgentSwitchedWidget{Box: root}
}

// NewModelSwitchedWidget creates a subtle notice indicating a model switch.
func NewModelSwitchedWidget(msg opencode.Message) *AgentSwitchedWidget {
	root := gtk.NewBox(gtk.OrientationHorizontal, 6)
	root.AddCSSClass("glup-timeline-notice")

	var text string
	if msg.PreviousModel != nil && msg.PreviousModel.ID != "" {
		text = fmt.Sprintf("Switched model from %s to %s", msg.PreviousModel.String(), msg.Model.String())
	} else {
		text = fmt.Sprintf("Switched model to %s", msg.Model.String())
	}

	lbl := gtk.NewLabel(text)
	lbl.SetXAlign(0)
	root.Append(lbl)

	return &AgentSwitchedWidget{Box: root}
}

// NewLocationSwitchedWidget creates a subtle notice indicating a session directory move.
func NewLocationSwitchedWidget(msg opencode.Message) *AgentSwitchedWidget {
	root := gtk.NewBox(gtk.OrientationHorizontal, 6)
	root.AddCSSClass("glup-timeline-notice")

	dir := strings.TrimSpace(msg.Location.Directory)
	text := fmt.Sprintf("Moved to %s", shortDir(dir))

	lbl := gtk.NewLabel(text)
	lbl.SetXAlign(0)
	root.Append(lbl)

	return &AgentSwitchedWidget{Box: root}
}

// RevertBannerWidget renders a banner indicating that subsequent messages are staged for revert.
type RevertBannerWidget struct {
	*gtk.Box
	ctx          context.Context
	titleLabel   *gtk.Label
	diffBadge    *gtk.Label
	diffExpander *gtk.Expander
	diffView     *gtk.TextView
	redoBtn      *gtk.Button
}

// NewRevertBannerWidget creates a new banner showing staged revert status and a Redo button.
func NewRevertBannerWidget(ctx context.Context, onRedo func()) *RevertBannerWidget {
	root := gtk.NewBox(gtk.OrientationVertical, 6)
	root.AddCSSClass("glup-revert-banner")

	row := gtk.NewBox(gtk.OrientationHorizontal, 8)
	row.SetHExpand(true)
	row.SetVAlign(gtk.AlignCenter)

	icon := gtk.NewImageFromIconName("edit-undo-symbolic")
	icon.AddCSSClass("glup-revert-banner-icon")
	row.Append(icon)

	titleLabel := gtk.NewLabel("1 reverted message")
	titleLabel.AddCSSClass("glup-revert-banner-title")
	titleLabel.SetXAlign(0)
	row.Append(titleLabel)

	diffBadge := gtk.NewLabel("")
	diffBadge.AddCSSClass("glup-revert-banner-diff")
	diffBadge.SetVisible(false)
	row.Append(diffBadge)

	spacer := gtk.NewBox(gtk.OrientationHorizontal, 0)
	spacer.SetHExpand(true)
	row.Append(spacer)

	redoBtn := gtk.NewButton()
	redoBox := gtk.NewBox(gtk.OrientationHorizontal, 6)
	redoBox.SetHAlign(gtk.AlignCenter)
	redoBox.SetVAlign(gtk.AlignCenter)
	redoBox.Append(gtk.NewImageFromIconName("edit-redo-symbolic"))
	redoBox.Append(gtk.NewLabel("Redo"))
	redoBtn.SetChild(redoBox)
	redoBtn.AddCSSClass("suggested-action")
	redoBtn.AddCSSClass("pill")
	if onRedo != nil {
		redoBtn.ConnectClicked(onRedo)
	}
	row.Append(redoBtn)

	root.Append(row)

	diffExpander := gtk.NewExpander("Reverted Changes")
	diffExpander.AddCSSClass("glup-revert-diff-expander")
	diffExpander.SetVisible(false)

	diffView := gtk.NewTextView()
	diffView.SetEditable(false)
	diffView.SetMonospace(true)
	diffView.AddCSSClass("glup-revert-diff-text")

	scroll := gtk.NewScrolledWindow()
	scroll.SetMaxContentHeight(180)
	scroll.SetPropagateNaturalHeight(true)
	scroll.SetChild(diffView)

	diffExpander.SetChild(scroll)
	root.Append(diffExpander)

	return &RevertBannerWidget{
		Box:          root,
		ctx:          ctx,
		titleLabel:   titleLabel,
		diffBadge:    diffBadge,
		diffExpander: diffExpander,
		diffView:     diffView,
		redoBtn:      redoBtn,
	}
}

// Update updates the banner title, diff badge, and diff content for staged reverts.
func (b *RevertBannerWidget) Update(revertedCount int, diff string) {
	count := revertedCount
	if count <= 0 {
		count = 1
	}
	if count == 1 {
		b.titleLabel.SetText("1 reverted message")
	} else {
		b.titleLabel.SetText(fmt.Sprintf("%d reverted messages", count))
	}

	trimmedDiff := strings.TrimSpace(diff)
	if trimmedDiff != "" {
		added, deleted := opencode.CountDiffChanges(trimmedDiff)
		if added > 0 || deleted > 0 {
			b.diffBadge.SetText(fmt.Sprintf("+%d -%d", added, deleted))
			b.diffBadge.SetVisible(true)
		} else {
			b.diffBadge.SetVisible(false)
		}
		b.diffView.Buffer().SetText(trimmedDiff)
		b.diffExpander.SetVisible(true)
	} else {
		b.diffBadge.SetVisible(false)
		b.diffExpander.SetVisible(false)
	}
}

type questionOptionState struct {
	value string
	btn   *gtk.CheckButton
}

type questionFieldState struct {
	field       opencode.FormField
	options     []questionOptionState
	customCheck *gtk.CheckButton
	customEntry *gtk.Entry
}

// QuestionFormWidget renders an interactive OpenCode V2 question / form wizard card.
type QuestionFormWidget struct {
	*gtk.Box
	FormID      string
	fields      []questionFieldState
	stepBtns    []*gtk.Button
	stack       *gtk.Stack
	currentStep int
	busy        bool
	backBtn     *gtk.Button
	nextBtn     *gtk.Button
	submitBtn   *gtk.Button
	dismissBtn  *gtk.Button
	onSubmit    func(map[string]any)
	onDismiss   func()
}

// NewQuestionFormWidget creates an interactive step-by-step wizard card for answering or dismissing a FormInfo.
func NewQuestionFormWidget(
	form opencode.FormInfo,
	onSubmit func(map[string]any),
	onDismiss func(),
) *QuestionFormWidget {
	root := gtk.NewBox(gtk.OrientationVertical, 10)
	root.AddCSSClass("glup-question-card")

	hdr := gtk.NewBox(gtk.OrientationHorizontal, 8)
	hdr.SetVAlign(gtk.AlignCenter)

	icon := gtk.NewImageFromIconName("forum-symbolic")
	icon.AddCSSClass("glup-question-icon")
	hdr.Append(icon)

	cardTitle := strings.TrimSpace(form.Title)
	if cardTitle == "" {
		cardTitle = "Questions"
	}
	titleLbl := gtk.NewLabel(cardTitle)
	titleLbl.AddCSSClass("glup-question-card-title")
	titleLbl.SetXAlign(0)
	titleLbl.SetHExpand(true)
	hdr.Append(titleLbl)

	root.Append(hdr)

	if desc := strings.TrimSpace(form.Description); desc != "" {
		descLbl := gtk.NewLabel(desc)
		descLbl.AddCSSClass("dim-label")
		descLbl.SetXAlign(0)
		descLbl.SetWrap(true)
		descLbl.SetWrapMode(pango.WrapWordChar)
		root.Append(descLbl)
	}

	stack := gtk.NewStack()
	stack.SetTransitionType(gtk.StackTransitionTypeSlideLeftRight)
	stack.SetTransitionDuration(140)
	stack.SetHhomogeneous(true)
	stack.SetVhomogeneous(false)

	qw := &QuestionFormWidget{
		Box:       root,
		FormID:    form.ID,
		stack:     stack,
		onSubmit:  onSubmit,
		onDismiss: onDismiss,
	}

	if len(form.Fields) > 0 {
		stepsBar := gtk.NewBox(gtk.OrientationHorizontal, 6)
		stepsBar.AddCSSClass("glup-question-steps")
		for i, f := range form.Fields {
			stepIdx := i
			tabTitle := strings.TrimSpace(f.Title)
			if tabTitle == "" {
				tabTitle = fmt.Sprintf("Step %d", i+1)
			} else if len(form.Fields) > 1 {
				tabTitle = fmt.Sprintf("%d. %s", i+1, tabTitle)
			}
			stepBtn := gtk.NewButtonWithLabel(tabTitle)
			stepBtn.AddCSSClass("flat")
			stepBtn.AddCSSClass("glup-question-step-tab")
			stepBtn.ConnectClicked(func() {
				qw.SetStep(stepIdx)
			})
			stepsBar.Append(stepBtn)
			qw.stepBtns = append(qw.stepBtns, stepBtn)
		}
		root.Append(stepsBar)
	}

	for fieldIdx, f := range form.Fields {
		stepIdx := fieldIdx
		fieldBox := gtk.NewBox(gtk.OrientationVertical, 6)
		fieldBox.AddCSSClass("glup-question-field")

		if f.Type == "multiselect" {
			fHdr := gtk.NewBox(gtk.OrientationHorizontal, 8)
			fHdr.SetVAlign(gtk.AlignCenter)
			multiLbl := gtk.NewLabel("Select all that apply")
			multiLbl.AddCSSClass("dim-label")
			multiLbl.AddCSSClass("caption")
			multiLbl.SetXAlign(0)
			fHdr.Append(multiLbl)
			fieldBox.Append(fHdr)
		}

		if qText := strings.TrimSpace(f.Description); qText != "" {
			qLbl := gtk.NewLabel(qText)
			qLbl.AddCSSClass("glup-question-prompt")
			qLbl.SetXAlign(0)
			qLbl.SetWrap(true)
			qLbl.SetWrapMode(pango.WrapWordChar)
			qLbl.SetSelectable(true)
			fieldBox.Append(qLbl)
		}

		fs := questionFieldState{field: f}
		isMulti := f.Type == "multiselect"
		var groupLeader *gtk.CheckButton

		optsBox := gtk.NewBox(gtk.OrientationVertical, 4)
		for idx, opt := range f.Options {
			val := opt.Value
			if val == "" {
				val = opt.Label
			}
			lblText := opt.Label
			if lblText == "" {
				lblText = val
			}

			chk := gtk.NewCheckButton()
			if !isMulti {
				if groupLeader == nil {
					groupLeader = chk
				} else {
					chk.SetGroup(groupLeader)
				}
				if idx == 0 {
					chk.SetActive(true)
				}
			}

			optContent := gtk.NewBox(gtk.OrientationVertical, 2)
			optContent.SetHExpand(true)
			optLbl := gtk.NewLabel(lblText)
			optLbl.AddCSSClass("glup-question-opt-label")
			optLbl.SetXAlign(0)
			optLbl.SetWrap(true)
			optLbl.SetWrapMode(pango.WrapWordChar)
			optContent.Append(optLbl)

			if optDesc := strings.TrimSpace(opt.Description); optDesc != "" {
				dLbl := gtk.NewLabel(optDesc)
				dLbl.AddCSSClass("glup-question-opt-desc")
				dLbl.AddCSSClass("dim-label")
				dLbl.SetXAlign(0)
				dLbl.SetWrap(true)
				dLbl.SetWrapMode(pango.WrapWordChar)
				optContent.Append(dLbl)
			}

			chk.SetChild(optContent)
			chk.AddCSSClass("glup-question-option-row")
			chk.ConnectToggled(func() {
				qw.syncValidState()
			})
			optsBox.Append(chk)

			fs.options = append(fs.options, questionOptionState{
				value: val,
				btn:   chk,
			})
		}

		if f.Custom || len(f.Options) == 0 {
			customRow := gtk.NewBox(gtk.OrientationHorizontal, 0)
			customRow.SetVAlign(gtk.AlignCenter)
			customRow.AddCSSClass("glup-question-option-row")
			customRow.AddCSSClass("glup-question-custom-row")

			var customChk *gtk.CheckButton
			if len(f.Options) > 0 {
				customChk = gtk.NewCheckButton()
				customChk.AddCSSClass("glup-question-custom-check")
				if !isMulti && groupLeader != nil {
					customChk.SetGroup(groupLeader)
				}
				customRow.Append(customChk)
			}

			entry := gtk.NewEntry()
			entry.SetHExpand(true)
			entry.AddCSSClass("glup-question-custom-entry")
			if customChk == nil {
				entry.AddCSSClass("glup-question-custom-entry-standalone")
			}
			entry.SetPlaceholderText("Type a custom answer…")
			entry.ConnectChanged(func() {
				if customChk != nil && strings.TrimSpace(entry.Text()) != "" && !customChk.Active() {
					customChk.SetActive(true)
				}
				qw.syncValidState()
			})
			entry.ConnectActivate(func() {
				if stepIdx < len(qw.fields)-1 {
					qw.NextStep()
				} else {
					qw.Submit()
				}
			})
			if customChk != nil {
				customChk.ConnectToggled(func() {
					if customChk.Active() {
						entry.GrabFocus()
					}
					qw.syncValidState()
				})
			}

			customRow.Append(entry)
			optsBox.Append(customRow)

			fs.customCheck = customChk
			fs.customEntry = entry
		}

		fieldBox.Append(optsBox)
		stack.AddNamed(fieldBox, fmt.Sprintf("step_%d", stepIdx))
		qw.fields = append(qw.fields, fs)
	}

	root.Append(stack)

	actionsRow := gtk.NewBox(gtk.OrientationHorizontal, 8)
	actionsRow.SetHExpand(true)

	dismissBtn := gtk.NewButtonWithLabel("Dismiss")
	dismissBtn.ConnectClicked(func() {
		qw.Dismiss()
	})
	actionsRow.Append(dismissBtn)

	spacer := gtk.NewBox(gtk.OrientationHorizontal, 0)
	spacer.SetHExpand(true)
	actionsRow.Append(spacer)

	backBtn := gtk.NewButtonWithLabel("Back")
	backBtn.ConnectClicked(func() {
		qw.PrevStep()
	})
	actionsRow.Append(backBtn)

	nextBtn := gtk.NewButtonWithLabel("Next")
	nextBtn.AddCSSClass("suggested-action")
	nextBtn.ConnectClicked(func() {
		qw.NextStep()
	})
	actionsRow.Append(nextBtn)

	submitBtn := gtk.NewButtonWithLabel("Submit")
	submitBtn.AddCSSClass("suggested-action")
	submitBtn.ConnectClicked(func() {
		qw.Submit()
	})
	actionsRow.Append(submitBtn)

	root.Append(actionsRow)

	qw.backBtn = backBtn
	qw.nextBtn = nextBtn
	qw.submitBtn = submitBtn
	qw.dismissBtn = dismissBtn
	qw.SetStep(0)
	return qw
}

// CurrentStep returns the 0-based index of the currently visible question step.
func (qw *QuestionFormWidget) CurrentStep() int {
	return qw.currentStep
}

// StepCount returns the number of question steps in the form.
func (qw *QuestionFormWidget) StepCount() int {
	return len(qw.fields)
}

// SetStep switches the wizard view to the given 0-based step index.
func (qw *QuestionFormWidget) SetStep(idx int) {
	if len(qw.fields) == 0 {
		qw.syncValidState()
		return
	}
	if idx < 0 {
		idx = 0
	}
	if idx >= len(qw.fields) {
		idx = len(qw.fields) - 1
	}
	qw.currentStep = idx
	if qw.stack != nil {
		qw.stack.SetVisibleChildName(fmt.Sprintf("step_%d", idx))
	}
	qw.syncValidState()
}

// NextStep advances to the next question step if available.
func (qw *QuestionFormWidget) NextStep() bool {
	if qw.currentStep >= len(qw.fields)-1 {
		return false
	}
	if _, ok := qw.collectFieldAnswer(qw.fields[qw.currentStep]); !ok {
		return false
	}
	qw.SetStep(qw.currentStep + 1)
	return true
}

// PrevStep moves back to the previous question step if available.
func (qw *QuestionFormWidget) PrevStep() bool {
	if qw.currentStep <= 0 {
		return false
	}
	qw.SetStep(qw.currentStep - 1)
	return true
}

// Submit validates and submits the current answers if valid.
func (qw *QuestionFormWidget) Submit() bool {
	if qw.busy || (qw.submitBtn != nil && !qw.submitBtn.Sensitive()) {
		return false
	}
	ans, ok := qw.CollectAnswers()
	if !ok {
		return false
	}
	qw.SetBusy(true)
	if qw.onSubmit != nil {
		qw.onSubmit(ans)
	}
	return true
}

// Dismiss cancels/dismisses the form.
func (qw *QuestionFormWidget) Dismiss() {
	if qw.busy || (qw.dismissBtn != nil && !qw.dismissBtn.Sensitive()) {
		return
	}
	qw.SetBusy(true)
	if qw.onDismiss != nil {
		qw.onDismiss()
	}
}

// SetBusy enables or disables the form buttons while a request is in flight.
func (qw *QuestionFormWidget) SetBusy(busy bool) {
	qw.busy = busy
	if qw.dismissBtn != nil {
		qw.dismissBtn.SetSensitive(!busy)
	}
	for _, b := range qw.stepBtns {
		b.SetSensitive(!busy)
	}
	if busy {
		if qw.backBtn != nil {
			qw.backBtn.SetSensitive(false)
		}
		if qw.nextBtn != nil {
			qw.nextBtn.SetSensitive(false)
		}
		if qw.submitBtn != nil {
			qw.submitBtn.SetSensitive(false)
		}
	} else {
		qw.syncValidState()
	}
}

func (qw *QuestionFormWidget) syncValidState() {
	multiStep := len(qw.fields) > 1
	isLast := len(qw.fields) <= 1 || qw.currentStep >= len(qw.fields)-1

	for i, btn := range qw.stepBtns {
		if i == qw.currentStep {
			btn.AddCSSClass("glup-question-step-tab-active")
		} else {
			btn.RemoveCSSClass("glup-question-step-tab-active")
		}
		if _, ok := qw.collectFieldAnswer(qw.fields[i]); ok {
			btn.AddCSSClass("glup-question-step-tab-done")
		} else {
			btn.RemoveCSSClass("glup-question-step-tab-done")
		}
	}

	if qw.backBtn != nil {
		qw.backBtn.SetVisible(multiStep)
		qw.backBtn.SetSensitive(!qw.busy && qw.currentStep > 0)
	}
	if qw.nextBtn != nil {
		qw.nextBtn.SetVisible(multiStep && !isLast)
		curValid := true
		if qw.currentStep >= 0 && qw.currentStep < len(qw.fields) {
			_, curValid = qw.collectFieldAnswer(qw.fields[qw.currentStep])
		}
		qw.nextBtn.SetSensitive(!qw.busy && curValid)
	}
	if qw.submitBtn != nil {
		qw.submitBtn.SetVisible(isLast)
		_, allValid := qw.CollectAnswers()
		qw.submitBtn.SetSensitive(!qw.busy && allValid)
	}
}

func (qw *QuestionFormWidget) collectFieldAnswer(fs questionFieldState) (any, bool) {
	isMulti := fs.field.Type == "multiselect"
	if isMulti {
		var vals []string
		for _, opt := range fs.options {
			if opt.btn != nil && opt.btn.Active() {
				vals = append(vals, opt.value)
			}
		}
		if fs.customEntry != nil {
			customText := strings.TrimSpace(fs.customEntry.Text())
			if customText != "" && (fs.customCheck == nil || fs.customCheck.Active()) {
				vals = append(vals, customText)
			}
		}
		if len(vals) == 0 && !fs.field.Optional {
			return nil, false
		}
		return vals, true
	}

	var val string
	if fs.customCheck != nil && fs.customCheck.Active() {
		if fs.customEntry != nil {
			val = strings.TrimSpace(fs.customEntry.Text())
		}
	} else if fs.customCheck == nil && fs.customEntry != nil && len(fs.options) == 0 {
		val = strings.TrimSpace(fs.customEntry.Text())
	} else {
		for _, opt := range fs.options {
			if opt.btn != nil && opt.btn.Active() {
				val = opt.value
				break
			}
		}
	}
	if val == "" && !fs.field.Optional {
		return nil, false
	}
	return val, true
}

// CollectAnswers extracts the current field values and reports whether all required fields have answers.
func (qw *QuestionFormWidget) CollectAnswers() (map[string]any, bool) {
	out := make(map[string]any, len(qw.fields))
	for _, fs := range qw.fields {
		ans, ok := qw.collectFieldAnswer(fs)
		if !ok {
			return nil, false
		}
		out[fs.field.Key] = ans
	}
	return out, true
}
