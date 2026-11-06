package window

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/diamondburned/gotk4-sourceview/pkg/gtksource/v5"
	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"libdb.so/gluppencode/internal/opencode"
)

// ProposalKind specifies what category a completion proposal represents.
type ProposalKind string

const (
	ProposalCommand ProposalKind = "command"
	ProposalSkill   ProposalKind = "skill"
	ProposalAgent   ProposalKind = "agent"
	ProposalFile    ProposalKind = "file"
)

// CompletionProposal is a proposal item shown in the GtkSourceCompletion popup.
type CompletionProposal struct {
	coreglib.Object
	Kind        ProposalKind
	IconName    string
	TriggerText string
	Label       string
	Detail      string
	SkillID     string
	AgentName   string
	FilePath    string
}

func (p *CompletionProposal) TypedText() string {
	return p.TriggerText
}

func (p *CompletionProposal) KindBadge() string {
	switch p.Kind {
	case ProposalCommand:
		return "cmd"
	case ProposalSkill:
		return "skill"
	case ProposalAgent:
		return "agent"
	case ProposalFile:
		return "file"
	default:
		return ""
	}
}

var proposalType = coreglib.RegisterSubclass[*CompletionProposal](
	coreglib.WithOverrides(func(p *CompletionProposal) gtksource.CompletionProposalOverrides {
		return gtksource.CompletionProposalOverrides{
			TypedText: func() string {
				return p.TriggerText
			},
		}
	}),
)

func newProposal(kind ProposalKind, iconName, triggerText, label, detail string) *CompletionProposal {
	p := proposalType.New()
	p.Kind = kind
	p.IconName = iconName
	p.TriggerText = triggerText
	p.Label = label
	p.Detail = detail
	return p
}

func proposalFromProposaller(prop gtksource.CompletionProposaller) *CompletionProposal {
	if prop == nil {
		return nil
	}
	if p, ok := prop.(*CompletionProposal); ok {
		return p
	}
	if obj := coreglib.BaseObject(prop); obj != nil {
		if p, ok := obj.Cast().(*CompletionProposal); ok {
			return p
		}
	}
	return nil
}

// CompletionProvider provides slash command and mention completions.
type CompletionProvider struct {
	coreglib.Object
	window *Window
}

var completionProviderType = coreglib.RegisterSubclass[*CompletionProvider](
	coreglib.WithOverrides(func(p *CompletionProvider) gtksource.CompletionProviderOverrides {
		return gtksource.CompletionProviderOverrides{
			Title: func() string {
				return "OpenCode"
			},
			Priority: func(context *gtksource.CompletionContext) int {
				return 100
			},
			IsTrigger: func(iter *gtk.TextIter, ch uint32) bool {
				return p.isTrigger(iter, ch)
			},
			KeyActivates: func(context *gtksource.CompletionContext, proposal gtksource.CompletionProposaller, keyval uint, state gdk.ModifierType) bool {
				return false
			},
			Populate: func(context *gtksource.CompletionContext) (*gio.ListModel, error) {
				return p.populate(context)
			},
			Display: func(context *gtksource.CompletionContext, proposal gtksource.CompletionProposaller, cell *gtksource.CompletionCell) {
				p.display(context, proposal, cell)
			},
			Activate: func(context *gtksource.CompletionContext, proposal gtksource.CompletionProposaller) {
				p.activate(context, proposal)
			},
		}
	}),
)

func newCompletionProvider(w *Window) *gtksource.CompletionProvider {
	cp := completionProviderType.New()
	cp.window = w
	return cp.Cast().(*gtksource.CompletionProvider)
}

func (p *CompletionProvider) isTrigger(iter *gtk.TextIter, ch uint32) bool {
	r := rune(ch)
	if r != '/' && r != '@' {
		return false
	}
	if iter == nil || iter.IsStart() {
		return true
	}
	cur := *iter
	if rune(cur.Char()) == r {
		if cur.IsStart() {
			return true
		}
		cur.BackwardChar()
	} else {
		cur.BackwardChar()
		if rune(cur.Char()) == r {
			if cur.IsStart() {
				return true
			}
			cur.BackwardChar()
		}
	}
	return unicode.IsSpace(rune(cur.Char()))
}

func (p *CompletionProvider) populate(context *gtksource.CompletionContext) (*gio.ListModel, error) {
	trigger, query, _ := p.extractTriggerAndQuery(context)
	if trigger == 0 {
		return nil, nil
	}

	store := gio.NewListStore(gtksource.GTypeCompletionProposal)
	lowerQuery := strings.ToLower(query)

	switch trigger {
	case '/':
		// Built-in slash commands
		builtins := []struct {
			name string
			desc string
			icon string
		}{
			{"/undo", "Undo last message & file changes", "edit-undo-symbolic"},
			{"/redo", "Redo undone message", "edit-redo-symbolic"},
			{"/new", "Create a new session", "document-new-symbolic"},
			{"/compact", "Compact session history", "compress-symbolic"},
		}

		seen := make(map[string]bool)
		for _, b := range builtins {
			seen[strings.TrimPrefix(b.name, "/")] = true
			if lowerQuery == "" || strings.Contains(strings.ToLower(b.name), lowerQuery) || strings.Contains(strings.ToLower(b.desc), lowerQuery) {
				prop := newProposal(ProposalCommand, b.icon, b.name, b.name, b.desc)
				store.Append(&prop.Object)
			}
		}

		// Server commands
		if p.window != nil {
			for _, cmd := range p.window.commands {
				cname := strings.TrimPrefix(cmd.Name, "/")
				if seen[cname] {
					continue
				}
				seen[cname] = true
				trig := "/" + cname
				if lowerQuery == "" || strings.Contains(strings.ToLower(trig), lowerQuery) || strings.Contains(strings.ToLower(cmd.Description), lowerQuery) {
					prop := newProposal(ProposalCommand, "terminal-symbolic", trig, trig, cmd.Description)
					store.Append(&prop.Object)
				}
			}
		}

	case '@':
		if p.window != nil {
			// Skills
			for _, sk := range p.window.skills {
				trig := "@" + sk.ID
				label := "@" + sk.Name
				if label == "@" {
					label = trig
				}
				if lowerQuery == "" || strings.Contains(strings.ToLower(trig), lowerQuery) || strings.Contains(strings.ToLower(sk.Name), lowerQuery) || strings.Contains(strings.ToLower(sk.Description), lowerQuery) {
					prop := newProposal(ProposalSkill, "system-run-symbolic", trig, label, sk.Description)
					prop.SkillID = sk.ID
					store.Append(&prop.Object)
				}
			}

			// Agents
			for _, ag := range p.window.agents {
				trig := "@" + ag.Name
				if lowerQuery == "" || strings.Contains(strings.ToLower(trig), lowerQuery) || strings.Contains(strings.ToLower(ag.Description), lowerQuery) {
					prop := newProposal(ProposalAgent, "system-users-symbolic", trig, trig, ag.Description)
					prop.AgentName = ag.Name
					store.Append(&prop.Object)
				}
			}

			// Files
			dir := p.window.activeDirectory()
			if p.window.client != nil {
				files, err := p.window.client.FindFiles(p.window.ctx, dir, query, 15)
				if err == nil {
					for _, f := range files {
						trig := "@" + f.Path
						icon := "text-x-generic-symbolic"
						if f.Type == "directory" {
							icon = "folder-symbolic"
						}
						prop := newProposal(ProposalFile, icon, trig, trig, f.Type)
						prop.FilePath = f.Path
						store.Append(&prop.Object)
					}
				}
			}
		}
	}

	return &store.ListModel, nil
}

func (p *CompletionProvider) display(context *gtksource.CompletionContext, proposal gtksource.CompletionProposaller, cell *gtksource.CompletionCell) {
	prop := proposalFromProposaller(proposal)
	if prop == nil {
		return
	}

	switch cell.Column() {
	case gtksource.SourceCompletionColumnIcon:
		if prop.IconName != "" {
			cell.SetIconName(prop.IconName)
		}
	case gtksource.SourceCompletionColumnBefore:
		cell.SetText(prop.KindBadge())
	case gtksource.SourceCompletionColumnTypedText:
		cell.SetMarkup(fmt.Sprintf("<b>%s</b>", html.EscapeString(prop.TriggerText)))
	case gtksource.SourceCompletionColumnComment, gtksource.SourceCompletionColumnAfter:
		cell.SetText(prop.Detail)
	}
}

func (p *CompletionProvider) activate(context *gtksource.CompletionContext, proposal gtksource.CompletionProposaller) {
	prop := proposalFromProposaller(proposal)
	if prop == nil {
		return
	}

	buf := context.Buffer()
	if buf == nil && p.window != nil {
		buf = p.window.inputBuffer
	}
	if buf == nil {
		return
	}

	begin, end, ok := context.Bounds()
	if !ok || begin == nil || end == nil {
		return
	}

	startIter := *begin
	if !startIter.IsStart() {
		prev := startIter
		prev.BackwardChar()
		ch := prev.Char()
		if ch == '/' || ch == '@' {
			startIter = prev
		}
	}

	replacement := prop.TriggerText + " "
	buf.BeginUserAction()
	buf.Delete(&startIter, end)
	buf.Insert(&startIter, replacement)
	buf.EndUserAction()
}

func (p *CompletionProvider) extractTriggerAndQuery(ctx *gtksource.CompletionContext) (trigger rune, query string, startIter *gtk.TextIter) {
	buf := ctx.Buffer()
	if buf == nil {
		return 0, "", nil
	}

	var end *gtk.TextIter
	begin, endPtr, ok := ctx.Bounds()
	if ok && begin != nil && endPtr != nil {
		str := buf.Text(begin, endPtr, false)
		if strings.HasPrefix(str, "/") {
			return '/', strings.TrimPrefix(str, "/"), begin
		}
		if strings.HasPrefix(str, "@") {
			return '@', strings.TrimPrefix(str, "@"), begin
		}
		end = endPtr
	}

	if end == nil {
		end = buf.IterAtMark(buf.GetInsert())
	}
	if end == nil {
		_, end = buf.Bounds()
	}
	if end == nil {
		return 0, "", nil
	}

	lineStart := *end
	lineStart.SetLineOffset(0)
	lineText := buf.Text(&lineStart, end, false)
	for i := len(lineText) - 1; i >= 0; i-- {
		ch := lineText[i]
		if ch == '/' || ch == '@' {
			if i == 0 || unicode.IsSpace(rune(lineText[i-1])) {
				triggerIter := lineStart
				triggerIter.SetLineOffset(i)
				return rune(ch), lineText[i+1:], &triggerIter
			}
		}
		if unicode.IsSpace(rune(ch)) {
			break
		}
	}

	return 0, "", nil
}

// parseMentions extracts @-prefixed tokens from text and builds PromptOptions mentions.
func parseMentions(text string, skills []opencode.SkillInfo, agents []opencode.Agent, workspaceDir string) opencode.PromptOptions {
	opts := opencode.PromptOptions{
		Text: text,
	}

	n := len(text)
	for i := 0; i < n; i++ {
		if text[i] != '@' {
			continue
		}
		if i > 0 && !unicode.IsSpace(rune(text[i-1])) {
			continue
		}

		end := i + 1
		for end < n && !unicode.IsSpace(rune(text[end])) && text[end] != ',' && text[end] != ';' {
			end++
		}
		token := text[i:end]
		name := strings.TrimPrefix(token, "@")
		if name == "" {
			continue
		}

		lowerName := strings.ToLower(name)

		// Check agents
		var matchedAgent bool
		for _, ag := range agents {
			if strings.EqualFold(ag.Name, lowerName) {
				opts.Agents = append(opts.Agents, opencode.PromptAgentInput{
					Name: ag.Name,
					Mention: &opencode.PromptMention{
						Start: i,
						End:   end,
						Text:  token,
					},
				})
				matchedAgent = true
				break
			}
		}
		if matchedAgent {
			i = end
			continue
		}

		// Check skills
		var matchedSkill bool
		for _, sk := range skills {
			if strings.EqualFold(sk.ID, lowerName) || strings.EqualFold(sk.Name, lowerName) {
				opts.Skills = append(opts.Skills, opencode.PromptSkillInput{
					ID: sk.ID,
					Mention: &opencode.PromptMention{
						Start: i,
						End:   end,
						Text:  token,
					},
				})
				matchedSkill = true
				break
			}
		}
		if matchedSkill {
			i = end
			continue
		}

		// Check workspace file/path
		if workspaceDir != "" {
			candidatePath := filepath.Join(workspaceDir, name)
			if _, err := os.Stat(candidatePath); err == nil {
				opts.Paths = append(opts.Paths, opencode.PromptFileInput{
					URI:  "file://" + candidatePath,
					Name: filepath.Base(candidatePath),
					Mention: &opencode.PromptMention{
						Start: i,
						End:   end,
						Text:  token,
					},
				})
				i = end
				continue
			}
		}

		// Fallback for files with path separators or file extensions
		if strings.Contains(name, "/") || strings.Contains(name, ".") {
			abs := name
			if !filepath.IsAbs(abs) && workspaceDir != "" {
				abs = filepath.Join(workspaceDir, name)
			}
			opts.Paths = append(opts.Paths, opencode.PromptFileInput{
				URI:  "file://" + abs,
				Name: filepath.Base(name),
				Mention: &opencode.PromptMention{
					Start: i,
					End:   end,
					Text:  token,
				},
			})
		}

		i = end
	}

	return opts
}
