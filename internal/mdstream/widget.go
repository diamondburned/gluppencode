package mdstream

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/diamondburned/chatkit/md/hl"
	"github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"github.com/diamondburned/gotkit/app"
	"github.com/diamondburned/gotkit/gtkutil/cssutil"
	"github.com/yuin/goldmark/ast"
	east "github.com/yuin/goldmark/extension/ast"
)

var _ = cssutil.WriteCSS(`
.glup-md-paragraph {
	margin-top: 2px;
	margin-bottom: 2px;
}
.glup-md-heading {
	margin-top: 10px;
	margin-bottom: 4px;
	font-weight: 700;
}
.glup-md-list {
	margin-top: 3px;
	margin-bottom: 3px;
}
.glup-md-listitem {
	margin-top: 2px;
	margin-bottom: 2px;
}
.glup-md-bullet {
	font-weight: 600;
	opacity: 0.75;
}
.glup-md-codeblock {
	margin-top: 6px;
	margin-bottom: 6px;
	border: 1px solid alpha(@borders, 0.6);
	border-radius: 6px;
	background-color: alpha(@theme_fg_color, 0.04);
}
.glup-md-codeblock-bar,
.glup-md-code-header {
	padding: 4px 8px 4px 12px;
	background-color: alpha(@theme_fg_color, 0.06);
	border-bottom: 1px solid alpha(@borders, 0.4);
	border-top-left-radius: 5px;
	border-top-right-radius: 5px;
}
.glup-md-codeblock-lang,
.glup-md-code-lang {
	font-family: monospace;
	font-size: 0.78em;
	opacity: 0.7;
	font-weight: 600;
}
.glup-md-codeblock-copy,
.glup-md-code-copy {
	padding: 2px;
	min-height: 24px;
	min-width: 24px;
}
.glup-md-codeblock-text {
	font-family: monospace;
	font-size: 0.86em;
	padding: 8px 12px;
	background: transparent;
}
.glup-md-codeblock-text text,
.md-codeblock-text text {
	background: transparent;
	background-color: transparent;
}
.glup-md-blockquote {
	border-left: 3px solid alpha(@theme_fg_color, 0.28);
	padding-left: 12px;
	margin-top: 4px;
	margin-bottom: 4px;
	opacity: 0.85;
}
.glup-md-table-scroll {
	margin-top: 6px;
	margin-bottom: 6px;
}
.glup-md-table {
	margin-top: 6px;
	margin-bottom: 6px;
	border: 1px solid alpha(@borders, 0.6);
	border-radius: 6px;
	background-color: alpha(@theme_fg_color, 0.03);
}
.glup-md-table-header {
	font-weight: 700;
	background-color: alpha(@theme_fg_color, 0.07);
	padding: 6px 10px;
	border-bottom: 1px solid alpha(@borders, 0.6);
}
.glup-md-table-cell {
	padding: 5px 10px;
	border-top: 1px solid alpha(@borders, 0.3);
}
.glup-md-thematic-break {
	margin-top: 8px;
	margin-bottom: 8px;
	opacity: 0.5;
}
/* compatibility aliases */
.md-codeblock {
	margin-top: 6px;
	margin-bottom: 6px;
	border-radius: 6px;
}
.md-codeblock-text {
	font-family: monospace;
	font-size: 0.86em;
	padding: 8px 10px;
}
.md-listitem {
	margin-left: 8px;
	margin-top: 3px;
	margin-bottom: 3px;
}
textview.glup-md-inline-tv,
textview.glup-md-inline-tv text {
	background: transparent;
	background-color: transparent;
	color: inherit;
	padding: 0;
}
.glup-md-link-icon {
	color: @accent_color;
	margin-right: 4px;
	margin-bottom: -2px;
	min-width: 14px;
	min-height: 14px;
}
`)

// SessionLinkPrefix is the URI prefix used for clickable OpenCode session links.
const SessionLinkPrefix = "opencode://session/"

// SessionResolver resolves an OpenCode session ID (ses_...) to its conversation title.
// If the session is unknown or has no title, it returns "".
type SessionResolver func(sessionID string) string

// AsyncSessionResolver resolves an OpenCode session ID synchronously if cached, or
// invokes onResolved on the GTK main loop once fetched asynchronously.
type AsyncSessionResolver func(sessionID string, onResolved func(title string)) string

// LinkHandler handles activation of a clicked link URI. It returns true if handled.
type LinkHandler func(uri string) bool

type sessionResolverKey struct{}
type asyncSessionResolverKey struct{}
type linkHandlerKey struct{}

// WithSessionResolver returns a child context configured with fn to resolve ses_... IDs.
func WithSessionResolver(ctx context.Context, fn SessionResolver) context.Context {
	return context.WithValue(ctx, sessionResolverKey{}, fn)
}

// WithAsyncSessionResolver returns a child context configured with fn to resolve ses_... IDs
// synchronously or asynchronously.
func WithAsyncSessionResolver(ctx context.Context, fn AsyncSessionResolver) context.Context {
	return context.WithValue(ctx, asyncSessionResolverKey{}, fn)
}

// WithLinkHandler returns a child context configured with fn to handle link activations.
func WithLinkHandler(ctx context.Context, fn LinkHandler) context.Context {
	return context.WithValue(ctx, linkHandlerKey{}, fn)
}

func resolveSessionTitle(ctx context.Context, sessionID string, onResolved func(title string)) string {
	if ctx != nil {
		if asyncFn, ok := ctx.Value(asyncSessionResolverKey{}).(AsyncSessionResolver); ok && asyncFn != nil {
			if title := strings.TrimSpace(asyncFn(sessionID, onResolved)); title != "" {
				return title
			}
		}
		if fn, ok := ctx.Value(sessionResolverKey{}).(SessionResolver); ok && fn != nil {
			if title := strings.TrimSpace(fn(sessionID)); title != "" {
				return title
			}
		}
	}
	return ""
}

// ActivateLink dispatches uri to the context's LinkHandler if present, or opens external
// URIs via the system's default application.
func ActivateLink(ctx context.Context, uri string) bool {
	uri = normalizeLinkDestination(strings.TrimSpace(uri))
	if uri == "" {
		return false
	}
	if ctx != nil {
		if fn, ok := ctx.Value(linkHandlerKey{}).(LinkHandler); ok && fn != nil {
			if fn(uri) {
				return true
			}
		}
	}
	if strings.HasPrefix(uri, SessionLinkPrefix) {
		return true
	}
	if ctx != nil {
		app.OpenURI(ctx, uri)
	}
	return true
}

// View is a GTK4 widget that renders streaming or static Markdown using
// prefix-cached committed block widgets and a single replaceable tail widget.
type View struct {
	*gtk.Box
	ctx context.Context

	committedBox *gtk.Box
	tailBox      *gtk.Box

	committedWidgets []gtk.Widgetter
	tailWidgets      []gtk.Widgetter

	mu       sync.Mutex
	engine   *Engine
	streamer *Streamer
	closed   atomic.Bool
	epoch    uint64
}

// NewView creates a new streaming Markdown GTK4 View.
func NewView(ctx context.Context) *View {
	root := gtk.NewBox(gtk.OrientationVertical, 6)
	root.AddCSSClass("glup-mdstream-view")
	root.AddCSSClass("glup-md-view")

	committedBox := gtk.NewBox(gtk.OrientationVertical, 8)
	committedBox.AddCSSClass("glup-mdstream-committed")
	committedBox.SetVisible(false)

	tailBox := gtk.NewBox(gtk.OrientationVertical, 6)
	tailBox.AddCSSClass("glup-mdstream-tail")
	tailBox.SetVisible(false)

	root.Append(committedBox)
	root.Append(tailBox)

	return &View{
		Box:          root,
		ctx:          ctx,
		committedBox: committedBox,
		tailBox:      tailBox,
	}
}

// AppendDelta appends a streaming markdown delta to be parsed and rendered in batches.
func (v *View) AppendDelta(delta string) {
	if delta == "" || v.closed.Load() {
		return
	}
	v.mu.Lock()
	if v.streamer == nil {
		if v.engine == nil {
			v.engine = NewEngine()
		}
		epoch := v.epoch
		v.streamer = NewStreamerWithEngine(DefaultInterval, v.engine, func(patch RenderPatch) {
			v.schedulePatch(epoch, patch)
		})
	}
	s := v.streamer
	v.mu.Unlock()

	s.Append(delta)
}

// SetStatic renders an already-completed markdown message immediately on the caller
// (GTK main loop) thread, replacing any prior content.
func (v *View) SetStatic(markdown string) {
	if v.closed.Load() {
		return
	}
	v.stopStreamer()

	v.clearCommitted()
	v.clearTail()

	v.mu.Lock()
	v.engine = NewEngine()
	v.engine.Append(markdown)
	p, _ := v.engine.Flush()
	v.mu.Unlock()

	v.applyPatch(p)
}

// Finalize flushes any pending streaming tokens and commits all remaining blocks.
func (v *View) Finalize(fullText string) {
	if v.closed.Load() {
		return
	}
	v.mu.Lock()
	s := v.streamer
	v.streamer = nil
	eng := v.engine
	v.mu.Unlock()

	if s != nil {
		s.Finalize(fullText)
		return
	}

	if eng == nil {
		eng = NewEngine()
		v.mu.Lock()
		v.engine = eng
		v.mu.Unlock()
	}
	patch := eng.Finalize(fullText)
	v.applyPatch(patch)
}

// Close stops any active background streaming worker and prevents further UI updates.
func (v *View) Close() {
	if v.closed.Swap(true) {
		return
	}
	v.stopStreamer()
}

func (v *View) stopStreamer() {
	v.mu.Lock()
	s := v.streamer
	v.streamer = nil
	v.epoch++
	v.mu.Unlock()

	if s != nil {
		s.Close()
	}
}

func (v *View) schedulePatch(epoch uint64, patch RenderPatch) {
	if v.closed.Load() {
		return
	}
	glib.IdleAdd(func() {
		if v.closed.Load() {
			return
		}
		v.mu.Lock()
		currentEpoch := v.epoch
		v.mu.Unlock()
		if epoch != currentEpoch {
			return
		}
		v.applyPatch(patch)
	})
}

func (v *View) applyPatch(patch RenderPatch) {
	if patch.ResetCommitted {
		v.clearCommitted()
		v.clearTail()
	}

	if len(patch.Committed) > 0 {
		for _, blk := range patch.Committed {
			if w := renderBlock(v.ctx, blk.Node, blk.Source); w != nil {
				v.committedWidgets = append(v.committedWidgets, w)
				v.committedBox.Append(w)
			}
		}
		v.committedBox.SetVisible(true)
	}

	v.clearTail()
	if patch.Tail != nil && !patch.Final {
		if w := renderBlock(v.ctx, patch.Tail.Node, patch.Tail.Source); w != nil {
			v.tailWidgets = append(v.tailWidgets, w)
			v.tailBox.Append(w)
			v.tailBox.SetVisible(true)
		}
	}
}

func (v *View) clearCommitted() {
	for _, w := range v.committedWidgets {
		v.committedBox.Remove(w)
	}
	v.committedWidgets = v.committedWidgets[:0]
	v.committedBox.SetVisible(false)
}

func (v *View) clearTail() {
	for _, w := range v.tailWidgets {
		v.tailBox.Remove(w)
	}
	v.tailWidgets = v.tailWidgets[:0]
	v.tailBox.SetVisible(false)
}

// CommittedWidgets returns the rendered widgets for all visible blocks (committed and active tail).
func (v *View) CommittedWidgets() []gtk.Widgetter {
	total := len(v.committedWidgets) + len(v.tailWidgets)
	if total == 0 {
		return nil
	}
	res := make([]gtk.Widgetter, 0, total)
	res = append(res, v.committedWidgets...)
	res = append(res, v.tailWidgets...)
	return res
}

// CodeBlock represents a rendered fenced or indented code block widget.
type CodeBlock struct {
	*gtk.Box
	LangLabel *gtk.Label
	CopyBtn   *gtk.Button
	TextView  *gtk.TextView
}

// ListBlock represents a rendered markdown list widget.
type ListBlock struct {
	*gtk.Box
	Items []*ListItemBlock
}

// ListItemBlock represents an individual list item in a rendered markdown list.
type ListItemBlock struct {
	*gtk.Box
	Bullet  *gtk.Label
	Content *gtk.Box
}

// InlineBlock represents a rendered inline block containing website links with
// inline earth-symbolic icon widgets.
type InlineBlock struct {
	*gtk.TextView
	Buffer    *gtk.TextBuffer
	LinkIcons []*gtk.Image
	Links     []string
}

// TableGrid represents a rendered markdown table widget.
type TableGrid struct {
	*gtk.Grid
	Labels []*gtk.Label
}

func renderBlock(ctx context.Context, n ast.Node, src []byte) gtk.Widgetter {
	if n == nil {
		return nil
	}

	if n.Kind() == east.KindTable {
		return renderTable(ctx, n, src)
	}

	switch n := n.(type) {
	case *ast.Document:
		box := gtk.NewBox(gtk.OrientationVertical, 8)
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			if w := renderBlock(ctx, c, src); w != nil {
				box.Append(w)
			}
		}
		return box

	case *ast.Paragraph, *ast.TextBlock:
		return renderInlineWidget(ctx, n, src, 0, "glup-md-paragraph")

	case *ast.Heading:
		var cls string
		switch n.Level {
		case 1:
			cls = "title-1"
		case 2:
			cls = "title-2"
		case 3:
			cls = "title-3"
		case 4:
			cls = "title-4"
		default:
			cls = "heading"
		}
		return renderInlineWidget(ctx, n, src, 0, "glup-md-heading", cls)

	case *ast.List:
		listBox := gtk.NewBox(gtk.OrientationVertical, 4)
		listBox.AddCSSClass("glup-md-list")
		idx := n.Start
		var items []*ListItemBlock
		for item := n.FirstChild(); item != nil; item = item.NextSibling() {
			if listItem, ok := item.(*ast.ListItem); ok {
				itemBox := gtk.NewBox(gtk.OrientationHorizontal, 8)
				itemBox.AddCSSClass("glup-md-listitem")
				itemBox.AddCSSClass("md-listitem")

				var bulletText string
				if n.IsOrdered() {
					bulletText = fmt.Sprintf("%d.", idx)
					idx++
				} else {
					bulletText = "•"
				}
				bullet := gtk.NewLabel(bulletText)
				bullet.SetVAlign(gtk.AlignStart)
				bullet.AddCSSClass("glup-md-bullet")

				contentBox := gtk.NewBox(gtk.OrientationVertical, 4)
				contentBox.SetHExpand(true)
				for c := listItem.FirstChild(); c != nil; c = c.NextSibling() {
					if w := renderBlock(ctx, c, src); w != nil {
						contentBox.Append(w)
					}
				}

				itemBox.Append(bullet)
				itemBox.Append(contentBox)
				listBox.Append(itemBox)
				items = append(items, &ListItemBlock{
					Box:     itemBox,
					Bullet:  bullet,
					Content: contentBox,
				})
			}
		}
		return &ListBlock{
			Box:   listBox,
			Items: items,
		}

	case *ast.FencedCodeBlock, *ast.CodeBlock:
		codeText, lang := extractCodeText(n, src)
		return newCodeBlockWidget(ctx, codeText, lang)

	case *ast.Blockquote:
		quoteBox := gtk.NewBox(gtk.OrientationVertical, 4)
		quoteBox.AddCSSClass("glup-md-blockquote")
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			if w := renderBlock(ctx, c, src); w != nil {
				quoteBox.Append(w)
			}
		}
		return quoteBox

	case *ast.ThematicBreak:
		sep := gtk.NewSeparator(gtk.OrientationHorizontal)
		sep.AddCSSClass("glup-md-thematic-break")
		return sep

	default:
		if n.HasChildren() {
			box := gtk.NewBox(gtk.OrientationVertical, 4)
			for c := n.FirstChild(); c != nil; c = c.NextSibling() {
				if w := renderBlock(ctx, c, src); w != nil {
					box.Append(w)
				}
			}
			return box
		}
		return nil
	}
}

func newCodeBlockWidget(ctx context.Context, codeText, lang string) gtk.Widgetter {
	codeBox := gtk.NewBox(gtk.OrientationVertical, 0)
	codeBox.AddCSSClass("glup-md-codeblock")
	codeBox.AddCSSClass("md-codeblock")

	topBar := gtk.NewBox(gtk.OrientationHorizontal, 0)
	topBar.AddCSSClass("glup-md-codeblock-bar")
	topBar.AddCSSClass("glup-md-code-header")

	langLabel := gtk.NewLabel(lang)
	langLabel.AddCSSClass("glup-md-codeblock-lang")
	langLabel.AddCSSClass("glup-md-code-lang")
	langLabel.SetHExpand(true)
	langLabel.SetXAlign(0)
	langLabel.SetVAlign(gtk.AlignCenter)
	topBar.Append(langLabel)

	copyBtn := gtk.NewButtonFromIconName("edit-copy-symbolic")
	copyBtn.AddCSSClass("flat")
	copyBtn.AddCSSClass("glup-md-codeblock-copy")
	copyBtn.AddCSSClass("glup-md-code-copy")
	copyBtn.SetVAlign(gtk.AlignCenter)
	copyBtn.SetTooltipText("Copy code")
	copyBtn.ConnectClicked(func() {
		if d := gdk.DisplayGetDefault(); d != nil {
			d.Clipboard().SetText(codeText)
		}
	})
	topBar.Append(copyBtn)
	codeBox.Append(topBar)

	buf := gtk.NewTextBuffer(nil)
	buf.SetText(codeText)
	if lang != "" {
		hl.Highlight(ctx, buf.StartIter(), buf.EndIter(), lang)
	}

	tv := gtk.NewTextViewWithBuffer(buf)
	tv.SetEditable(false)
	tv.SetMonospace(true)
	tv.SetCursorVisible(false)
	tv.AddCSSClass("glup-md-codeblock-text")
	tv.AddCSSClass("md-codeblock-text")

	scroll := gtk.NewScrolledWindow()
	scroll.SetPolicy(gtk.PolicyAutomatic, gtk.PolicyNever)
	scroll.SetPropagateNaturalHeight(true)
	scroll.SetChild(tv)

	codeBox.Append(scroll)
	return &CodeBlock{
		Box:       codeBox,
		LangLabel: langLabel,
		CopyBtn:   copyBtn,
		TextView:  tv,
	}
}

func renderTable(ctx context.Context, n ast.Node, src []byte) gtk.Widgetter {
	grid := gtk.NewGrid()
	grid.AddCSSClass("glup-md-table")
	grid.SetColumnSpacing(0)
	grid.SetRowSpacing(0)
	grid.SetHExpand(true)

	var labels []*gtk.Label
	rowIdx := 0
	for row := n.FirstChild(); row != nil; row = row.NextSibling() {
		isHeader := row.Kind() == east.KindTableHeader
		colIdx := 0
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			var xalign float32
			if c, ok := cell.(*east.TableCell); ok {
				switch c.Alignment {
				case east.AlignCenter:
					xalign = 0.5
				case east.AlignRight:
					xalign = 1
				}
			}
			cls := "glup-md-table-cell"
			if isHeader {
				cls = "glup-md-table-header"
			}
			w := renderInlineWidget(ctx, cell, src, xalign, cls)
			gtk.BaseWidget(w).SetHExpand(true)
			grid.Attach(w, colIdx, rowIdx, 1, 1)
			if lbl, ok := w.(*gtk.Label); ok {
				labels = append(labels, lbl)
			}
			colIdx++
		}
		rowIdx++
	}

	return &TableGrid{
		Grid:   grid,
		Labels: labels,
	}
}

func extractCodeText(n ast.Node, src []byte) (string, string) {
	var lang string
	if fcb, ok := n.(*ast.FencedCodeBlock); ok {
		lang = string(fcb.Language(src))
	}
	var b strings.Builder
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		b.Write(seg.Value(src))
	}
	return strings.TrimSuffix(b.String(), "\n"), lang
}

func extractNodePlainText(n ast.Node, src []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(child ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch child := child.(type) {
		case *ast.Text:
			b.Write(child.Segment.Value(src))
		case *ast.String:
			b.Write(child.Value)
		}
		return ast.WalkContinue, nil
	})
	if b.Len() == 0 && n.Lines().Len() > 0 {
		for i := 0; i < n.Lines().Len(); i++ {
			seg := n.Lines().At(i)
			b.Write(seg.Value(src))
		}
	}
	return b.String()
}

var (
	inlineTokenRe = regexp.MustCompile(`(https?://[^\s<>"]+|www\.[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}[^\s<>"]*|\bses_[A-Za-z0-9]{10,}\b)`)
	sessionIDRe   = regexp.MustCompile(`^ses_[A-Za-z0-9]{10,}$`)
)

func isWebsiteLink(dest string) bool {
	lower := strings.ToLower(strings.TrimSpace(dest))
	return strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "www.")
}

func normalizeLinkDestination(dest string) string {
	trimmed := strings.TrimSpace(dest)
	if strings.HasPrefix(strings.ToLower(trimmed), "www.") {
		return "https://" + trimmed
	}
	return trimmed
}

func trimTrailingURLPunct(raw string) (clean, trailing string) {
	end := len(raw)
	for end > 0 {
		ch := raw[end-1]
		switch ch {
		case '.', ',', ';', ':', '!', '?', '\'', '"':
			end--
		case ')':
			open := strings.Count(raw[:end], "(")
			close := strings.Count(raw[:end], ")")
			if close > open {
				end--
			} else {
				return raw[:end], raw[end:]
			}
		case ']':
			open := strings.Count(raw[:end], "[")
			close := strings.Count(raw[:end], "]")
			if close > open {
				end--
			} else {
				return raw[:end], raw[end:]
			}
		default:
			return raw[:end], raw[end:]
		}
	}
	return raw, ""
}

type inlineStyle struct {
	italic  bool
	bold    bool
	strike  bool
	code    bool
	linkURI string
}

type inlineTokenKind int

const (
	inlineTokenText inlineTokenKind = iota
	inlineTokenWebsiteIcon
)

type inlineToken struct {
	kind  inlineTokenKind
	text  string
	style inlineStyle
	uri   string
}

func trimInlineTokens(tokens []inlineToken) []inlineToken {
	for len(tokens) > 0 && tokens[0].kind == inlineTokenText {
		tokens[0].text = strings.TrimLeft(tokens[0].text, " \t\r\n")
		if tokens[0].text == "" {
			tokens = tokens[1:]
		} else {
			break
		}
	}
	for len(tokens) > 0 && tokens[len(tokens)-1].kind == inlineTokenText {
		last := len(tokens) - 1
		tokens[last].text = strings.TrimRight(tokens[last].text, " \t\r\n")
		if tokens[last].text == "" {
			tokens = tokens[:last]
		} else {
			break
		}
	}
	return tokens
}

func tokenizeInline(ctx context.Context, n ast.Node, src []byte, onAsyncResolve func()) []inlineToken {
	var tokens []inlineToken
	if n.FirstChild() == nil && n.Lines().Len() > 0 {
		var b strings.Builder
		for i := 0; i < n.Lines().Len(); i++ {
			seg := n.Lines().At(i)
			b.Write(seg.Value(src))
		}
		tokenizePlainSegment(ctx, &tokens, b.String(), inlineStyle{}, onAsyncResolve)
		return trimInlineTokens(tokens)
	}
	tokenizeInlineChildren(ctx, &tokens, n, src, inlineStyle{}, onAsyncResolve)
	return trimInlineTokens(tokens)
}

func tokenizeInlineChildren(ctx context.Context, tokens *[]inlineToken, parent ast.Node, src []byte, style inlineStyle, onAsyncResolve func()) {
	var plainBuf strings.Builder
	flushPlain := func() {
		if plainBuf.Len() == 0 {
			return
		}
		tokenizePlainSegment(ctx, tokens, plainBuf.String(), style, onAsyncResolve)
		plainBuf.Reset()
	}
	for c := parent.FirstChild(); c != nil; c = c.NextSibling() {
		switch cn := c.(type) {
		case *ast.Text:
			plainBuf.Write(cn.Segment.Value(src))
			if cn.HardLineBreak() || cn.SoftLineBreak() {
				plainBuf.WriteByte('\n')
			}
		case *ast.String:
			plainBuf.Write(cn.Value)
		default:
			flushPlain()
			tokenizeInlineNode(ctx, tokens, c, src, style, onAsyncResolve)
		}
	}
	flushPlain()
}

func tokenizePlainSegment(ctx context.Context, tokens *[]inlineToken, s string, style inlineStyle, onAsyncResolve func()) {
	if s == "" {
		return
	}
	if style.linkURI != "" {
		if sessionIDRe.MatchString(strings.TrimSpace(s)) {
			sid := strings.TrimSpace(s)
			if title := resolveSessionTitle(ctx, sid, func(string) {
				if onAsyncResolve != nil {
					onAsyncResolve()
				}
			}); title != "" {
				s = title
			}
		}
		*tokens = append(*tokens, inlineToken{
			kind:  inlineTokenText,
			text:  s,
			style: style,
		})
		return
	}

	matches := inlineTokenRe.FindAllStringIndex(s, -1)
	if len(matches) == 0 {
		*tokens = append(*tokens, inlineToken{
			kind:  inlineTokenText,
			text:  s,
			style: style,
		})
		return
	}

	lastIdx := 0
	for _, m := range matches {
		if m[0] > lastIdx {
			*tokens = append(*tokens, inlineToken{
				kind:  inlineTokenText,
				text:  s[lastIdx:m[0]],
				style: style,
			})
		}
		tok := s[m[0]:m[1]]
		if strings.HasPrefix(tok, "ses_") {
			title := resolveSessionTitle(ctx, tok, func(string) {
				if onAsyncResolve != nil {
					onAsyncResolve()
				}
			})
			display := tok
			if title != "" {
				display = title
			}
			linkStyle := style
			linkStyle.linkURI = SessionLinkPrefix + tok
			if title != "" {
				linkStyle.code = false
			}
			*tokens = append(*tokens, inlineToken{
				kind:  inlineTokenText,
				text:  display,
				style: linkStyle,
			})
		} else {
			cleanURL, trailing := trimTrailingURLPunct(tok)
			if cleanURL != "" {
				dest := normalizeLinkDestination(cleanURL)
				linkStyle := style
				linkStyle.linkURI = dest
				if isWebsiteLink(dest) {
					*tokens = append(*tokens, inlineToken{
						kind:  inlineTokenWebsiteIcon,
						uri:   dest,
						style: linkStyle,
					})
				}
				*tokens = append(*tokens, inlineToken{
					kind:  inlineTokenText,
					text:  cleanURL,
					style: linkStyle,
				})
				if trailing != "" {
					*tokens = append(*tokens, inlineToken{
						kind:  inlineTokenText,
						text:  trailing,
						style: style,
					})
				}
			} else {
				*tokens = append(*tokens, inlineToken{
					kind:  inlineTokenText,
					text:  tok,
					style: style,
				})
			}
		}
		lastIdx = m[1]
	}
	if lastIdx < len(s) {
		*tokens = append(*tokens, inlineToken{
			kind:  inlineTokenText,
			text:  s[lastIdx:],
			style: style,
		})
	}
}

func tokenizeInlineNode(ctx context.Context, tokens *[]inlineToken, n ast.Node, src []byte, style inlineStyle, onAsyncResolve func()) {
	switch n := n.(type) {
	case *ast.Text:
		tokenizePlainSegment(ctx, tokens, string(n.Segment.Value(src)), style, onAsyncResolve)
		if n.HardLineBreak() || n.SoftLineBreak() {
			*tokens = append(*tokens, inlineToken{
				kind:  inlineTokenText,
				text:  "\n",
				style: style,
			})
		}
	case *ast.String:
		tokenizePlainSegment(ctx, tokens, string(n.Value), style, onAsyncResolve)
	case *ast.CodeSpan:
		plain := strings.TrimSpace(extractNodePlainText(n, src))
		if sessionIDRe.MatchString(plain) {
			title := resolveSessionTitle(ctx, plain, func(string) {
				if onAsyncResolve != nil {
					onAsyncResolve()
				}
			})
			linkStyle := style
			linkStyle.linkURI = SessionLinkPrefix + plain
			if title != "" {
				*tokens = append(*tokens, inlineToken{
					kind:  inlineTokenText,
					text:  title,
					style: linkStyle,
				})
				return
			}
			linkStyle.code = true
			*tokens = append(*tokens, inlineToken{
				kind:  inlineTokenText,
				text:  plain,
				style: linkStyle,
			})
			return
		}
		codeStyle := style
		codeStyle.code = true
		*tokens = append(*tokens, inlineToken{
			kind:  inlineTokenText,
			text:  extractNodePlainText(n, src),
			style: codeStyle,
		})
	case *ast.Emphasis:
		emStyle := style
		if n.Level == 2 {
			emStyle.bold = true
		} else {
			emStyle.italic = true
		}
		tokenizeInlineChildren(ctx, tokens, n, src, emStyle, onAsyncResolve)
	case *ast.Link:
		rawDest := strings.TrimSpace(string(n.Destination))
		dest := normalizeLinkDestination(rawDest)
		if sessionIDRe.MatchString(rawDest) {
			dest = SessionLinkPrefix + rawDest
		}
		linkStyle := style
		linkStyle.linkURI = dest
		if isWebsiteLink(dest) {
			*tokens = append(*tokens, inlineToken{
				kind:  inlineTokenWebsiteIcon,
				uri:   dest,
				style: linkStyle,
			})
		}
		if n.FirstChild() != nil {
			tokenizeInlineChildren(ctx, tokens, n, src, linkStyle, onAsyncResolve)
		} else {
			*tokens = append(*tokens, inlineToken{
				kind:  inlineTokenText,
				text:  rawDest,
				style: linkStyle,
			})
		}
	case *ast.Image:
		rawDest := strings.TrimSpace(string(n.Destination))
		dest := normalizeLinkDestination(rawDest)
		linkStyle := style
		linkStyle.linkURI = dest
		if isWebsiteLink(dest) {
			*tokens = append(*tokens, inlineToken{
				kind:  inlineTokenWebsiteIcon,
				uri:   dest,
				style: linkStyle,
			})
		}
		if n.FirstChild() != nil {
			tokenizeInlineChildren(ctx, tokens, n, src, linkStyle, onAsyncResolve)
		} else {
			*tokens = append(*tokens, inlineToken{
				kind:  inlineTokenText,
				text:  rawDest,
				style: linkStyle,
			})
		}
	case *ast.AutoLink:
		u := strings.TrimSpace(string(n.URL(src)))
		dest := normalizeLinkDestination(u)
		linkStyle := style
		linkStyle.linkURI = dest
		if isWebsiteLink(dest) {
			*tokens = append(*tokens, inlineToken{
				kind:  inlineTokenWebsiteIcon,
				uri:   dest,
				style: linkStyle,
			})
		}
		*tokens = append(*tokens, inlineToken{
			kind:  inlineTokenText,
			text:  u,
			style: linkStyle,
		})
	default:
		if n.Kind() == east.KindStrikethrough {
			stStyle := style
			stStyle.strike = true
			tokenizeInlineChildren(ctx, tokens, n, src, stStyle, onAsyncResolve)
			return
		}
		tokenizeInlineChildren(ctx, tokens, n, src, style, onAsyncResolve)
	}
}

func tokensHaveWebsiteIcon(tokens []inlineToken) bool {
	for _, t := range tokens {
		if t.kind == inlineTokenWebsiteIcon {
			return true
		}
	}
	return false
}

func tokensToPangoMarkup(tokens []inlineToken) string {
	var b strings.Builder
	for _, tok := range tokens {
		if tok.kind != inlineTokenText || tok.text == "" {
			continue
		}
		var openTags, closeTags string
		if tok.style.linkURI != "" {
			openTags += `<a href="` + html.EscapeString(tok.style.linkURI) + `">`
			closeTags = `</a>` + closeTags
		}
		if tok.style.bold {
			openTags += "<b>"
			closeTags = "</b>" + closeTags
		}
		if tok.style.italic {
			openTags += "<i>"
			closeTags = "</i>" + closeTags
		}
		if tok.style.strike {
			openTags += "<s>"
			closeTags = "</s>" + closeTags
		}
		if tok.style.code {
			openTags += `<tt><span background="#787c88" bgalpha="20%" size="86%">&#160;`
			closeTags = `&#160;</span></tt>` + closeTags
		}
		b.WriteString(openTags)
		b.WriteString(html.EscapeString(tok.text))
		b.WriteString(closeTags)
	}
	return b.String()
}

func renderInline(n ast.Node, src []byte) string {
	return renderInlineWithContext(context.Background(), n, src)
}

func renderInlineWithContext(ctx context.Context, n ast.Node, src []byte) string {
	tokens := tokenizeInline(ctx, n, src, nil)
	return tokensToPangoMarkup(tokens)
}

func renderInlineWidget(ctx context.Context, n ast.Node, src []byte, xalign float32, cssClasses ...string) gtk.Widgetter {
	var refresh func()
	tokens := tokenizeInline(ctx, n, src, func() {
		if refresh != nil {
			refresh()
		}
	})
	if tokensHaveWebsiteIcon(tokens) {
		ib := newInlineBlock(ctx, tokens, xalign, cssClasses...)
		refresh = func() {
			updated := tokenizeInline(ctx, n, src, nil)
			ib.populate(ctx, updated)
		}
		return ib
	}
	lbl := gtk.NewLabel("")
	lbl.SetUseMarkup(true)
	lbl.SetMarkup(tokensToPangoMarkup(tokens))
	lbl.SetXAlign(xalign)
	lbl.SetWrap(true)
	lbl.SetWrapMode(pango.WrapWordChar)
	lbl.SetSelectable(true)
	for _, cls := range cssClasses {
		if cls != "" {
			lbl.AddCSSClass(cls)
		}
	}
	lbl.ConnectActivateLink(func(uri string) bool {
		return ActivateLink(ctx, uri)
	})
	refresh = func() {
		updated := tokenizeInline(ctx, n, src, nil)
		lbl.SetMarkup(tokensToPangoMarkup(updated))
	}
	return lbl
}

const urlTagPrefix = "glup-url:"

func newInlineBlock(ctx context.Context, tokens []inlineToken, xalign float32, cssClasses ...string) *InlineBlock {
	buf := gtk.NewTextBuffer(nil)
	buf.SetEnableUndo(false)

	tv := gtk.NewTextViewWithBuffer(buf)
	tv.SetEditable(false)
	tv.SetCursorVisible(false)
	tv.SetWrapMode(gtk.WrapWordChar)
	switch {
	case xalign >= 0.9:
		tv.SetJustification(gtk.JustifyRight)
	case xalign >= 0.4:
		tv.SetJustification(gtk.JustifyCenter)
	default:
		tv.SetJustification(gtk.JustifyLeft)
	}
	tv.AddCSSClass("glup-md-inline-tv")
	for _, cls := range cssClasses {
		if cls != "" {
			tv.AddCSSClass(cls)
		}
	}

	ib := &InlineBlock{
		TextView: tv,
		Buffer:   buf,
	}
	ib.bindLinkEvents(ctx)
	ib.populate(ctx, tokens)
	return ib
}

func (ib *InlineBlock) populate(ctx context.Context, tokens []inlineToken) {
	buf := ib.Buffer
	buf.Delete(buf.StartIter(), buf.EndIter())
	ib.LinkIcons = nil
	ib.Links = nil

	table := buf.TagTable()
	ensureTag := func(name string, configure func(t *gtk.TextTag)) *gtk.TextTag {
		if t := table.Lookup(name); t != nil {
			return t
		}
		t := gtk.NewTextTag(name)
		configure(t)
		table.Add(t)
		return t
	}

	boldTag := ensureTag("b", func(t *gtk.TextTag) {
		t.SetObjectProperty("weight", int(pango.WeightBold))
	})
	italicTag := ensureTag("i", func(t *gtk.TextTag) {
		t.SetObjectProperty("style", pango.StyleItalic)
	})
	strikeTag := ensureTag("s", func(t *gtk.TextTag) {
		t.SetObjectProperty("strikethrough", true)
	})
	codeTag := ensureTag("code", func(t *gtk.TextTag) {
		t.SetObjectProperty("family", "Monospace")
		t.SetObjectProperty("scale", 0.86)
		t.SetObjectProperty("background", "#787c8833")
	})
	linkTag := ensureTag("a", func(t *gtk.TextTag) {
		t.SetObjectProperty("underline", pango.UnderlineSingle)
		t.SetObjectProperty("foreground", "#3584e4")
	})

	urlTagFor := func(uri string) *gtk.TextTag {
		name := urlTagPrefix + uri
		return ensureTag(name, func(t *gtk.TextTag) {})
	}

	seenLinks := make(map[string]bool)
	for _, tok := range tokens {
		switch tok.kind {
		case inlineTokenWebsiteIcon:
			uri := tok.uri
			if uri != "" && !seenLinks[uri] {
				seenLinks[uri] = true
				ib.Links = append(ib.Links, uri)
			}
			endIter := buf.EndIter()
			anchor := buf.CreateChildAnchor(endIter)
			img := gtk.NewImageFromIconName("earth-symbolic")
			img.SetPixelSize(14)
			img.AddCSSClass("glup-md-link-icon")
			img.SetCursorFromName("pointer")
			if uri != "" {
				click := gtk.NewGestureClick()
				click.SetButton(gdk.BUTTON_PRIMARY)
				click.ConnectReleased(func(nPress int, x, y float64) {
					if nPress == 1 {
						click.SetState(gtk.EventSequenceClaimed)
						ActivateLink(ctx, uri)
					}
				})
				img.AddController(click)
			}
			ib.TextView.AddChildAtAnchor(img, anchor)
			ib.LinkIcons = append(ib.LinkIcons, img)

		case inlineTokenText:
			if tok.text == "" {
				continue
			}
			startOffset := buf.EndIter().Offset()
			endIter := buf.EndIter()
			if tok.style.code {
				buf.Insert(endIter, "\u00a0"+tok.text+"\u00a0")
			} else {
				buf.Insert(endIter, tok.text)
			}
			startIter := buf.IterAtOffset(startOffset)
			endIter = buf.EndIter()
			if tok.style.bold {
				buf.ApplyTag(boldTag, startIter, endIter)
			}
			if tok.style.italic {
				buf.ApplyTag(italicTag, startIter, endIter)
			}
			if tok.style.strike {
				buf.ApplyTag(strikeTag, startIter, endIter)
			}
			if tok.style.code {
				buf.ApplyTag(codeTag, startIter, endIter)
			}
			if tok.style.linkURI != "" {
				uri := tok.style.linkURI
				if !seenLinks[uri] {
					seenLinks[uri] = true
					ib.Links = append(ib.Links, uri)
				}
				buf.ApplyTag(linkTag, startIter, endIter)
				buf.ApplyTag(urlTagFor(uri), startIter, endIter)
			}
		}
	}
}

func (ib *InlineBlock) linkAtCoords(x, y float64) string {
	bx, by := ib.TextView.WindowToBufferCoords(gtk.TextWindowWidget, int(x), int(y))
	it, ok := ib.TextView.IterAtLocation(bx, by)
	if !ok {
		return ""
	}
	for _, tag := range it.Tags() {
		name, _ := tag.ObjectProperty("name").(string)
		if strings.HasPrefix(name, urlTagPrefix) {
			return strings.TrimPrefix(name, urlTagPrefix)
		}
	}
	return ""
}

func (ib *InlineBlock) bindLinkEvents(ctx context.Context) {
	motion := gtk.NewEventControllerMotion()
	hovering := false
	setHover := func(h bool) {
		if hovering == h {
			return
		}
		hovering = h
		if h {
			ib.TextView.SetCursorFromName("pointer")
		} else {
			ib.TextView.SetCursorFromName("text")
		}
	}
	motion.ConnectMotion(func(x, y float64) {
		setHover(ib.linkAtCoords(x, y) != "")
	})
	motion.ConnectLeave(func() {
		setHover(false)
	})
	ib.TextView.AddController(motion)

	click := gtk.NewGestureClick()
	click.SetButton(gdk.BUTTON_PRIMARY)
	click.ConnectReleased(func(nPress int, x, y float64) {
		if nPress != 1 {
			return
		}
		if _, _, hasSel := ib.Buffer.SelectionBounds(); hasSel {
			return
		}
		if uri := ib.linkAtCoords(x, y); uri != "" {
			click.SetState(gtk.EventSequenceClaimed)
			ActivateLink(ctx, uri)
		}
	})
	ib.TextView.AddController(click)
}
