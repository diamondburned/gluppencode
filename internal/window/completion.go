package window

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
	"unsafe"

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

var proposalRegistry sync.Map

func newProposal(kind ProposalKind, iconName, triggerText, label, detail string) *CompletionProposal {
	p := proposalType.New()
	p.Kind = kind
	p.IconName = iconName
	p.TriggerText = triggerText
	p.Label = label
	p.Detail = detail
	if ptr := p.Object.Native(); ptr != 0 {
		proposalRegistry.Store(ptr, p)
	}
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
		if v, ok := proposalRegistry.Load(obj.Native()); ok {
			return v.(*CompletionProposal)
		}
	}
	return nil
}

// CompletionProvider provides slash command and mention completions.
type CompletionProvider struct {
	coreglib.Object
	window *Window

	cachedDir       string
	cachedQuery     string
	cachedFiles     []opencode.FoundFile
	fetching        bool
	activeProposals []uintptr
	mu              sync.Mutex
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
			Refilter: func(context *gtksource.CompletionContext, model gio.ListModeller) {
				if context == nil {
					return
				}
				proposals, err := p.populate(context)
				if err != nil {
					return
				}
				context.SetProposalsForProvider(wrapGTKSourceCompletionProvider(&p.Object), proposals)
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

type rawCompletionProvider struct {
	*coreglib.Object
}

func wrapGTKSourceCompletionProvider(obj *coreglib.Object) *gtksource.CompletionProvider {
	if obj == nil {
		return nil
	}
	raw := &rawCompletionProvider{Object: obj}
	return (*gtksource.CompletionProvider)(unsafe.Pointer(raw))
}

type rawCompletionProposal struct {
	*coreglib.Object
}

func wrapGTKSourceCompletionProposal(obj *coreglib.Object) gtksource.CompletionProposaller {
	if obj == nil {
		return nil
	}
	raw := &rawCompletionProposal{Object: obj}
	return (*gtksource.CompletionProposal)(unsafe.Pointer(raw))
}

func newCompletionProvider(w *Window) *gtksource.CompletionProvider {
	cp := completionProviderType.New()
	cp.window = w
	return wrapGTKSourceCompletionProvider(&cp.Object)
}

func (p *CompletionProvider) isTrigger(iter *gtk.TextIter, ch uint32) bool {
	r := rune(ch)
	if r == 0 || unicode.IsSpace(r) {
		return false
	}
	if iter == nil {
		return r == '/' || r == '@'
	}
	if p.window != nil && p.window.inputBuffer != nil {
		end := iter.Copy()
		if rune(end.Char()) == r {
			end.ForwardChar()
		}
		trig, _, _ := extractTriggerAndQueryAt(p.window.inputBuffer, end)
		return trig != 0
	}
	if r != '/' && r != '@' {
		return false
	}
	if iter.IsStart() {
		return true
	}
	cur := iter.Copy()
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

func (p *CompletionProvider) populate(compCtx *gtksource.CompletionContext) (*gio.ListModel, error) {
	trigger, query, _ := p.extractTriggerAndQuery(compCtx)
	if trigger == 0 {
		return nil, nil
	}

	p.mu.Lock()
	for _, ptr := range p.activeProposals {
		proposalRegistry.Delete(ptr)
	}
	p.activeProposals = p.activeProposals[:0]
	p.mu.Unlock()

	store := gio.NewListStore(gtksource.GTypeCompletionProposal)
	lowerQuery := strings.ToLower(query)

	addProposal := func(prop *CompletionProposal) {
		if ptr := prop.Object.Native(); ptr != 0 {
			p.mu.Lock()
			p.activeProposals = append(p.activeProposals, ptr)
			p.mu.Unlock()
		}
		store.Append(&prop.Object)
	}

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
			if lowerQuery == "" || strings.Contains(strings.ToLower(b.name), lowerQuery) {
				prop := newProposal(ProposalCommand, b.icon, b.name, b.name, b.desc)
				addProposal(prop)
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
				if lowerQuery == "" || strings.Contains(strings.ToLower(trig), lowerQuery) {
					prop := newProposal(ProposalCommand, "terminal-symbolic", trig, trig, cmd.Description)
					addProposal(prop)
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
				if lowerQuery == "" || strings.Contains(strings.ToLower(trig), lowerQuery) || strings.Contains(strings.ToLower(sk.Name), lowerQuery) {
					prop := newProposal(ProposalSkill, "system-run-symbolic", trig, label, sk.Description)
					prop.SkillID = sk.ID
					addProposal(prop)
				}
			}

			// Agents
			for _, ag := range p.window.agents {
				trig := "@" + ag.Name
				if lowerQuery == "" || strings.Contains(strings.ToLower(trig), lowerQuery) || strings.Contains(strings.ToLower(ag.Name), lowerQuery) {
					prop := newProposal(ProposalAgent, "system-users-symbolic", trig, trig, ag.Description)
					prop.AgentName = ag.Name
					addProposal(prop)
				}
			}

			// Files
			dir := p.window.activeDirectory()
			p.mu.Lock()
			cached := p.cachedFiles
			needFetch := (dir != p.cachedDir || query != p.cachedQuery) && !p.fetching
			if needFetch {
				p.fetching = true
			}
			p.mu.Unlock()

			if needFetch && p.window.client != nil {
				go func(fetchDir, fetchQuery string) {
					ctx, cancel := context.WithTimeout(p.window.ctx, 3*time.Second)
					defer cancel()
					files, err := p.window.client.FindFiles(ctx, fetchDir, fetchQuery, 15)
					coreglib.IdleAdd(func() {
						p.mu.Lock()
						p.fetching = false
						p.cachedDir = fetchDir
						p.cachedQuery = fetchQuery
						if err == nil {
							p.cachedFiles = files
						} else {
							p.cachedFiles = nil
						}
						p.mu.Unlock()
						if err == nil && p.window != nil && p.window.ctx.Err() == nil {
							p.window.syncComposerCompletionPopup()
						}
					})
				}(dir, query)
			}

			for _, f := range cached {
				trig := "@" + f.Path
				icon := "text-x-generic-symbolic"
				if f.Type == "directory" {
					icon = "folder-symbolic"
				}
				prop := newProposal(ProposalFile, icon, trig, trig, f.Type)
				prop.FilePath = f.Path
				addProposal(prop)
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
		cell.SetText("")
	case gtksource.SourceCompletionColumnTypedText:
		displayText := prop.TriggerText
		if utf8.RuneCountInString(displayText) > 40 {
			runes := []rune(displayText)
			displayText = string(runes[:16]) + "…" + string(runes[len(runes)-22:])
		}
		cell.SetMarkup(fmt.Sprintf("<b>%s</b>", html.EscapeString(displayText)))
	case gtksource.SourceCompletionColumnAfter:
		singleLine := strings.Join(strings.Fields(prop.Detail), " ")
		if utf8.RuneCountInString(singleLine) > 28 {
			singleLine = string([]rune(singleLine)[:27]) + "…"
		}
		if singleLine != "" {
			cell.SetMarkup(fmt.Sprintf("<span alpha=\"65%%\" size=\"small\">%s</span>", html.EscapeString(singleLine)))
		} else {
			cell.SetText("")
		}
	case gtksource.SourceCompletionColumnComment, gtksource.SourceCompletionColumnDetails:
		cell.SetText("")
	}
}

func (p *CompletionProvider) activate(context *gtksource.CompletionContext, proposal gtksource.CompletionProposaller) {
	prop := proposalFromProposaller(proposal)
	if prop == nil {
		return
	}

	var buf *gtksource.Buffer
	var end *gtk.TextIter
	if context != nil {
		buf = context.Buffer()
		if _, endPtr, ok := context.Bounds(); ok && endPtr != nil {
			end = endPtr
		}
	}
	if buf == nil && p.window != nil {
		buf = p.window.inputBuffer
	}
	if buf == nil {
		return
	}

	_, _, startIter := p.extractTriggerAndQuery(context)
	if startIter == nil {
		return
	}
	if end == nil {
		if mark := buf.GetInsert(); mark != nil {
			end = buf.IterAtMark(mark)
		}
	}
	if end == nil {
		_, end = buf.Bounds()
	}
	if end == nil {
		return
	}

	replacement := prop.TriggerText + " "
	buf.BeginUserAction()
	buf.Delete(startIter, end)
	buf.Insert(startIter, replacement)
	buf.PlaceCursor(startIter)
	buf.EndUserAction()
}

func (p *CompletionProvider) extractTriggerAndQuery(ctx *gtksource.CompletionContext) (trigger rune, query string, startIter *gtk.TextIter) {
	var buf *gtksource.Buffer
	var end *gtk.TextIter
	if ctx != nil {
		buf = ctx.Buffer()
		if _, endPtr, ok := ctx.Bounds(); ok && endPtr != nil {
			end = endPtr
		}
	}
	if buf == nil && p.window != nil {
		buf = p.window.inputBuffer
	}
	if buf == nil {
		return 0, "", nil
	}
	if end == nil {
		if mark := buf.GetInsert(); mark != nil {
			end = buf.IterAtMark(mark)
		}
	}
	if end == nil {
		_, end = buf.Bounds()
	}
	return extractTriggerAndQueryAt(buf, end)
}

func extractTriggerAndQueryAt(buf *gtksource.Buffer, end *gtk.TextIter) (trigger rune, query string, startIter *gtk.TextIter) {
	if buf == nil || end == nil {
		return 0, "", nil
	}

	lineStart := end.Copy()
	lineStart.SetLineOffset(0)
	lineText := buf.Text(lineStart, end, false)
	if len(lineText) == 0 {
		return 0, "", nil
	}
	if r, _ := utf8.DecodeLastRuneInString(lineText); unicode.IsSpace(r) {
		return 0, "", nil
	}

	lastSpace := strings.LastIndexFunc(lineText, unicode.IsSpace)
	wordStartByte := 0
	if lastSpace != -1 {
		_, spaceSize := utf8.DecodeRuneInString(lineText[lastSpace:])
		wordStartByte = lastSpace + spaceSize
	}
	word := lineText[wordStartByte:]
	if strings.HasPrefix(word, "@") {
		triggerIter := lineStart.Copy()
		triggerIter.SetLineOffset(utf8.RuneCountInString(lineText[:wordStartByte]))
		return '@', word[1:], triggerIter
	}
	if strings.HasPrefix(word, "/") {
		bufStart, _ := buf.Bounds()
		atBufferStart := strings.TrimSpace(lineText[:wordStartByte]) == "" &&
			(bufStart == nil || strings.TrimSpace(buf.Text(bufStart, lineStart, false)) == "")
		if atBufferStart {
			triggerIter := lineStart.Copy()
			triggerIter.SetLineOffset(utf8.RuneCountInString(lineText[:wordStartByte]))
			return '/', word[1:], triggerIter
		}
	}

	return 0, "", nil
}

func utf16Len(s string) int {
	var count int
	for _, r := range s {
		if r >= 0x10000 {
			count += 2
		} else {
			count++
		}
	}
	return count
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
		if i > 0 {
			prevRune, _ := utf8.DecodeLastRuneInString(text[:i])
			if !unicode.IsSpace(prevRune) {
				continue
			}
		}

		end := i + 1
		for end < n {
			r, size := utf8.DecodeRuneInString(text[end:])
			if unicode.IsSpace(r) || r == ',' || r == ';' {
				break
			}
			end += size
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
						Start: utf16Len(text[:i]),
						End:   utf16Len(text[:end]),
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
						Start: utf16Len(text[:i]),
						End:   utf16Len(text[:end]),
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
				absPath := candidatePath
				if !filepath.IsAbs(absPath) {
					if p, err := filepath.Abs(absPath); err == nil {
						absPath = p
					}
				}
				uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(absPath)}).String()
				opts.Paths = append(opts.Paths, opencode.PromptFileInput{
					URI:  uri,
					Name: filepath.Base(candidatePath),
					Mention: &opencode.PromptMention{
						Start: utf16Len(text[:i]),
						End:   utf16Len(text[:end]),
						Text:  token,
					},
				})
				i = end
				continue
			}
		}

		i = end
	}

	return opts
}
