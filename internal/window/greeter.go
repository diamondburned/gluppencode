package window

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"github.com/diamondburned/gotkit/gtkutil"
	"libdb.so/gluppencode/internal/opencode"
)

const (
	defaultGreeterTitle    = "How can I help today?"
	defaultGreeterSubtitle = "Ask a question, explore the codebase, or start building."
)

type greeterSuggestion struct {
	icon   string
	label  string
	prompt string
}

var defaultGreeterSuggestions = []greeterSuggestion{
	{
		icon:   "system-search-symbolic",
		label:  "Explore codebase",
		prompt: "Explore this repository and summarize its architecture and key components.",
	},
	{
		icon:   "document-edit-symbolic",
		label:  "Review recent changes",
		prompt: "Review the current uncommitted changes and check for bugs or edge cases.",
	},
	{
		icon:   "terminal-symbolic",
		label:  "Fix a bug or test",
		prompt: "Run the test suite and help diagnose any failing tests or issues.",
	},
}

// GreeterWidget displays an Antigravity-inspired empty-state greeting card
// when a conversation has no messages yet.
type GreeterWidget struct {
	*gtk.Box
	titleLabel    *gtk.Label
	subtitleLabel *gtk.Label
	chipsBox      *adw.WrapBox
}

// NewGreeterWidget creates a new GreeterWidget.
func NewGreeterWidget(profile *GreeterProfile, onSuggest func(prompt string)) *GreeterWidget {
	root := gtk.NewBox(gtk.OrientationVertical, 16)
	root.SetHAlign(gtk.AlignCenter)
	root.SetVAlign(gtk.AlignCenter)
	root.SetVExpand(true)
	root.SetHExpand(true)
	root.AddCSSClass("glup-greeter")

	badge := gtk.NewBox(gtk.OrientationHorizontal, 0)
	badge.SetHAlign(gtk.AlignCenter)
	badge.SetVAlign(gtk.AlignCenter)
	badge.SetOverflow(gtk.OverflowHidden)
	badge.AddCSSClass("glup-greeter-badge")
	icon := gtk.NewImageFromIconName("sparkle-symbolic")
	icon.SetPixelSize(28)
	icon.SetHExpand(true)
	icon.SetVExpand(true)
	icon.SetHAlign(gtk.AlignCenter)
	icon.SetVAlign(gtk.AlignCenter)
	icon.AddCSSClass("glup-greeter-icon")
	badge.Append(icon)

	secretPic := gtk.NewPictureForResource("/so/libdb/gluppencode/icons/scalable/actions/gluppencode-eyes.png")
	secretPic.SetCanShrink(true)
	secretPic.SetContentFit(gtk.ContentFitContain)
	secretPic.SetSizeRequest(166, 160)
	secretPic.SetVisible(false)
	secretPic.AddCSSClass("glup-greeter-secret-pic")
	badge.Append(secretPic)

	badgeClick := gtk.NewGestureClick()
	secretShown := false
	badgeClick.ConnectReleased(func(nPress int, x, y float64) {
		secretShown = !secretShown
		icon.SetVisible(!secretShown)
		secretPic.SetVisible(secretShown)
		if secretShown {
			badge.RemoveCSSClass("glup-greeter-badge")
			badge.AddCSSClass("glup-greeter-secret")
		} else {
			badge.RemoveCSSClass("glup-greeter-secret")
			badge.AddCSSClass("glup-greeter-badge")
		}
	})
	badge.AddController(badgeClick)
	root.Append(badge)

	textBox := gtk.NewBox(gtk.OrientationVertical, 8)
	textBox.SetHAlign(gtk.AlignCenter)

	titleLabel := gtk.NewLabel(defaultGreeterTitle)
	titleLabel.SetWrap(true)
	titleLabel.SetWrapMode(pango.WrapWordChar)
	titleLabel.SetJustify(gtk.JustifyCenter)
	titleLabel.SetMaxWidthChars(44)
	titleLabel.AddCSSClass("glup-greeter-title")

	subtitleLabel := gtk.NewLabel(defaultGreeterSubtitle)
	subtitleLabel.SetWrap(true)
	subtitleLabel.SetWrapMode(pango.WrapWordChar)
	subtitleLabel.SetJustify(gtk.JustifyCenter)
	subtitleLabel.SetMaxWidthChars(58)
	subtitleLabel.AddCSSClass("glup-greeter-subtitle")
	subtitleLabel.AddCSSClass("dim-label")

	textBox.Append(titleLabel)
	textBox.Append(subtitleLabel)
	root.Append(textBox)

	chipsBox := adw.NewWrapBox()
	chipsBox.SetHAlign(gtk.AlignCenter)
	chipsBox.SetChildSpacing(8)
	chipsBox.SetLineSpacing(8)
	chipsBox.SetJustify(adw.JustifyNone)
	chipsBox.AddCSSClass("glup-greeter-chips")
	root.Append(chipsBox)

	g := &GreeterWidget{
		Box:           root,
		titleLabel:    titleLabel,
		subtitleLabel: subtitleLabel,
		chipsBox:      chipsBox,
	}
	g.ApplyProfile(profile)

	for _, sug := range defaultGreeterSuggestions {
		prompt := sug.prompt
		btn := gtk.NewButton()
		btn.AddCSSClass("flat")
		btn.AddCSSClass("glup-greeter-chip")

		row := gtk.NewBox(gtk.OrientationHorizontal, 6)
		img := gtk.NewImageFromIconName(sug.icon)
		img.SetPixelSize(14)
		img.AddCSSClass("glup-greeter-chip-icon")
		lbl := gtk.NewLabel(sug.label)
		row.Append(img)
		row.Append(lbl)
		btn.SetChild(row)

		btn.ConnectClicked(func() {
			if onSuggest != nil {
				onSuggest(prompt)
			}
		})
		g.chipsBox.Append(btn)
	}
	return g
}

// ApplyProfile updates the greeter's title and subtitle from the given profile,
// falling back to default copy when empty.
func (g *GreeterWidget) ApplyProfile(profile *GreeterProfile) {
	if g == nil {
		return
	}
	title := defaultGreeterTitle
	subtitle := defaultGreeterSubtitle
	if profile != nil {
		if t := strings.TrimSpace(profile.Title); t != "" {
			title = t
		}
		if s := strings.TrimSpace(profile.Subtitle); s != "" {
			subtitle = s
		}
	}
	g.titleLabel.SetLabel(title)
	g.subtitleLabel.SetLabel(subtitle)
}

func (c *convTimeline) isEmptyState() bool {
	if c == nil {
		return false
	}
	// Unloaded conversations are not known to be empty; showing the greeter
	// while history loads would flash it on every existing conversation.
	return c.loaded && len(c.messages) == 0 && len(c.pendingPrompts) == 0 && len(c.pendingForms) == 0 && len(c.pendingPermissions) == 0 && len(c.slashBubbles) == 0 && !c.active
}

func (c *convTimeline) updateGreeterVisibility() {
	if c == nil || c.greeter == nil {
		return
	}
	c.greeter.SetVisible(c.isEmptyState())
}

// selectLightweightGreeterModel picks a lightweight Flash model for reading the system prompt.
func selectLightweightGreeterModel(models []opencode.ModelInfo) *opencode.ModelRef {
	preferred := []opencode.ModelRef{
		{ProviderID: "cloudflare-workers-ai", ID: "@cf/zai-org/glm-5.3-flash"},
		{ProviderID: "google", ID: "antigravity-gemini-3.5-flash-lite"},
		{ProviderID: "google", ID: "antigravity-gemini-3.8-flash-medium"},
	}
	if len(models) == 0 {
		ref := preferred[0]
		return &ref
	}
	for _, pref := range preferred {
		for _, m := range models {
			ref := m.Ref()
			if ref.ProviderID == pref.ProviderID && ref.ID == pref.ID {
				return &ref
			}
		}
	}
	for _, m := range models {
		ref := m.Ref()
		if strings.Contains(strings.ToLower(ref.ID), "flash") {
			return &ref
		}
	}
	ref := preferred[0]
	return &ref
}

func collectSystemPromptExcerpt(agents []opencode.Agent) string {
	// Prefer the build agent's system prompt, else the first non-empty one.
	var system string
	for _, a := range agents {
		sys := strings.TrimSpace(a.System)
		if sys == "" {
			continue
		}
		if a.ID == "build" || a.Name == "build" {
			system = sys
			break
		}
		if system == "" {
			system = sys
		}
	}
	if system == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			for _, rel := range []string{
				filepath.Join(".config", "opencode", "agents", "build.md"),
				filepath.Join(".config", "opencode", "AGENTS.md"),
			} {
				if data, err := os.ReadFile(filepath.Join(home, rel)); err == nil {
					if s := strings.TrimSpace(string(data)); s != "" {
						system = s
						break
					}
				}
			}
		}
	}
	if len(system) > 3200 {
		system = strings.ToValidUTF8(system[:3200], "")
	}
	return strings.TrimSpace(system)
}

func hashSystemPrompt(excerpt string) string {
	sum := sha256.Sum256([]byte(excerpt))
	return hex.EncodeToString(sum[:16])
}

func parseGreeterProfileJSON(raw string, sysHash string) (*GreeterProfile, bool) {
	trimmed := strings.TrimSpace(raw)
	if start := strings.IndexByte(trimmed, '{'); start >= 0 {
		if end := strings.LastIndexByte(trimmed, '}'); end > start {
			trimmed = trimmed[start : end+1]
		}
	}
	var prof GreeterProfile
	if err := json.Unmarshal([]byte(trimmed), &prof); err != nil {
		return nil, false
	}
	prof.UserName = strings.TrimSpace(prof.UserName)
	prof.Pronouns = strings.TrimSpace(prof.Pronouns)
	prof.Tone = strings.TrimSpace(prof.Tone)
	prof.Title = strings.TrimSpace(prof.Title)
	prof.Subtitle = strings.TrimSpace(prof.Subtitle)
	if prof.Title == "" || prof.Subtitle == "" {
		return nil, false
	}
	prof.SystemHash = sysHash
	return &prof, true
}

func (w *Window) effectiveGreeterProfile() *GreeterProfile {
	var out GreeterProfile
	if w.greeterProfile != nil {
		out = *w.greeterProfile
	}
	if t := strings.TrimSpace(w.prefs.GreeterTitleOverride); t != "" {
		out.Title = t
	}
	if s := strings.TrimSpace(w.prefs.GreeterSubOverride); s != "" {
		out.Subtitle = s
	}
	return &out
}

func (w *Window) applyGreeterProfileToAllConvs() {
	prof := w.effectiveGreeterProfile()
	if w.emptyGreeter != nil {
		w.emptyGreeter.ApplyProfile(prof)
	}
	for _, tab := range w.tabs {
		if tab.main != nil && tab.main.greeter != nil {
			tab.main.greeter.ApplyProfile(prof)
		}
		for _, sub := range tab.subTimelines {
			if sub != nil && sub.greeter != nil {
				sub.greeter.ApplyProfile(prof)
			}
		}
	}
}

func (w *Window) populateComposerSuggestion(prompt string) {
	if w.inputBuffer == nil {
		return
	}
	w.inputBuffer.SetText(prompt)
	_, end := w.inputBuffer.Bounds()
	w.inputBuffer.PlaceCursor(end)
	if w.inputView != nil {
		w.inputView.GrabFocus()
	}
}

func parseModelOverride(raw string) *opencode.ModelRef {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	providerID, id, ok := strings.Cut(raw, "/")
	if !ok || providerID == "" || id == "" {
		return nil
	}
	return &opencode.ModelRef{ProviderID: providerID, ID: id}
}

func (w *Window) refreshGreeterProfileAsync(force ...bool) {
	w.refreshGreeterProfileWithCallback(len(force) > 0 && force[0], nil)
}

func (w *Window) refreshGreeterProfileWithCallback(force bool, onDone func(err error)) {
	if w.client == nil || w.greeterRefreshing {
		if onDone != nil {
			onDone(nil)
		}
		return
	}
	excerpt := collectSystemPromptExcerpt(w.agents)
	if excerpt == "" {
		if onDone != nil {
			onDone(nil)
		}
		return
	}
	sysHash := hashSystemPrompt(excerpt)
	if !force && w.greeterProfile != nil && w.greeterProfile.SystemHash == sysHash && w.greeterProfile.Title != "" {
		if onDone != nil {
			onDone(nil)
		}
		return
	}

	model := parseModelOverride(w.prefs.GreeterModelOverride)
	if model == nil {
		model = selectLightweightGreeterModel(w.models)
	}
	w.greeterRefreshing = true

	prompt := "Read the following user system prompt / instructions and extract how the user prefers to be addressed and spoken to.\n" +
		"Then write a warm, helpful assistant greeting (inspired by Google Antigravity's empty-state greeter) strictly obeying every rule in the system prompt:\n" +
		"- Honor the user's exact name casing (e.g., all lowercase if specified).\n" +
		"- Honor the user's pronouns and person rule (e.g., if the prompt says to refer to the user in the third person with it/its and never use 'you'/'your', strictly use third person in both title and subtitle and never write 'you' or 'your').\n" +
		"- Honor the requested tone (e.g., friendly, informal, encouraging, softer/feminine, direct, no empty hype/glazing).\n\n" +
		"Return ONLY a raw JSON object (no markdown fences) with these exact keys:\n" +
		`{"userName":"...","pronouns":"...","tone":"...","title":"short greeting headline (<= 48 chars)","subtitle":"helpful 1-sentence assistant prompt (<= 90 chars)"}` + "\n\n" +
		"System prompt:\n" + excerpt

	gtkutil.Async(w.ctx, func() func() {
		respText, err := w.client.GenerateText(w.ctx, prompt, model)
		return func() {
			w.greeterRefreshing = false
			if err != nil {
				slog.Warn("greeter profile generation failed", "model", model.ProviderID+"/"+model.ID, "err", err)
				if onDone != nil {
					onDone(err)
				}
				return
			}
			prof, ok := parseGreeterProfileJSON(respText, sysHash)
			if !ok {
				slog.Warn("greeter profile response was not valid JSON", "model", model.ProviderID+"/"+model.ID)
				if onDone != nil {
					onDone(nil)
				}
				return
			}
			prof.Model = model.ProviderID + "/" + model.ID
			w.greeterProfile = prof
			w.saveState()
			w.applyGreeterProfileToAllConvs()
			if onDone != nil {
				onDone(nil)
			}
		}
	})
}
