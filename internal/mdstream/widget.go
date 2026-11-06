package mdstream

import (
	"context"
	"fmt"
	"html"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/diamondburned/chatkit/md/hl"
	"github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
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
	font-size: 0.85em;
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
	padding: 8px 12px;
	background: transparent;
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
	padding: 8px 10px;
}
.md-listitem {
	margin-left: 8px;
	margin-top: 3px;
	margin-bottom: 3px;
}
`)

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
		markup := renderInline(n, src)
		lbl := gtk.NewLabel("")
		lbl.SetUseMarkup(true)
		lbl.SetMarkup(markup)
		lbl.SetXAlign(0)
		lbl.SetWrap(true)
		lbl.SetWrapMode(pango.WrapWordChar)
		lbl.SetSelectable(true)
		lbl.AddCSSClass("glup-md-paragraph")
		return lbl

	case *ast.Heading:
		markup := renderInline(n, src)
		lbl := gtk.NewLabel("")
		lbl.SetUseMarkup(true)
		lbl.SetMarkup(markup)
		lbl.SetXAlign(0)
		lbl.SetWrap(true)
		lbl.SetWrapMode(pango.WrapWordChar)
		lbl.SetSelectable(true)
		lbl.AddCSSClass("glup-md-heading")
		switch n.Level {
		case 1:
			lbl.AddCSSClass("title-1")
		case 2:
			lbl.AddCSSClass("title-2")
		case 3:
			lbl.AddCSSClass("title-3")
		case 4:
			lbl.AddCSSClass("title-4")
		default:
			lbl.AddCSSClass("heading")
		}
		return lbl

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
			markup := renderInline(cell, src)
			lbl := gtk.NewLabel("")
			lbl.SetUseMarkup(true)
			lbl.SetMarkup(markup)
			lbl.SetHExpand(true)
			lbl.SetXAlign(0)
			if c, ok := cell.(*east.TableCell); ok {
				switch c.Alignment {
				case east.AlignCenter:
					lbl.SetXAlign(0.5)
				case east.AlignRight:
					lbl.SetXAlign(1)
				}
			}
			lbl.SetSelectable(true)
			lbl.SetWrap(true)
			lbl.SetWrapMode(pango.WrapWordChar)
			if isHeader {
				lbl.AddCSSClass("glup-md-table-header")
			} else {
				lbl.AddCSSClass("glup-md-table-cell")
			}
			grid.Attach(lbl, colIdx, rowIdx, 1, 1)
			labels = append(labels, lbl)
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

func renderInline(n ast.Node, src []byte) string {
	if n.FirstChild() == nil && n.Lines().Len() > 0 {
		var b strings.Builder
		for i := 0; i < n.Lines().Len(); i++ {
			seg := n.Lines().At(i)
			b.Write(seg.Value(src))
		}
		return html.EscapeString(strings.TrimSpace(b.String()))
	}
	var b strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		renderInlineNode(&b, c, src)
	}
	return strings.TrimSpace(b.String())
}

func renderInlineNode(b *strings.Builder, n ast.Node, src []byte) {
	switch n := n.(type) {
	case *ast.Text:
		b.WriteString(html.EscapeString(string(n.Segment.Value(src))))
		if n.HardLineBreak() || n.SoftLineBreak() {
			b.WriteByte('\n')
		}
	case *ast.String:
		b.WriteString(html.EscapeString(string(n.Value)))
	case *ast.CodeSpan:
		plain := extractNodePlainText(n, src)
		b.WriteString(`<tt><span background="#787c88" bgalpha="20%" size="92%">&#160;`)
		b.WriteString(html.EscapeString(plain))
		b.WriteString(`&#160;</span></tt>`)
	case *ast.Emphasis:
		tag := "i"
		if n.Level == 2 {
			tag = "b"
		}
		b.WriteString("<" + tag + ">")
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			renderInlineNode(b, c, src)
		}
		b.WriteString("</" + tag + ">")
	case *ast.Link:
		b.WriteString(`<a href="`)
		b.WriteString(html.EscapeString(string(n.Destination)))
		b.WriteString(`">`)
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			renderInlineNode(b, c, src)
		}
		b.WriteString("</a>")
	case *ast.Image:
		b.WriteString(`<a href="`)
		b.WriteString(html.EscapeString(string(n.Destination)))
		b.WriteString(`">`)
		if n.FirstChild() != nil {
			for c := n.FirstChild(); c != nil; c = c.NextSibling() {
				renderInlineNode(b, c, src)
			}
		} else {
			b.WriteString(html.EscapeString(string(n.Destination)))
		}
		b.WriteString("</a>")
	case *ast.AutoLink:
		u := string(n.URL(src))
		b.WriteString(`<a href="`)
		b.WriteString(html.EscapeString(u))
		b.WriteString(`">`)
		b.WriteString(html.EscapeString(u))
		b.WriteString("</a>")
	default:
		if n.Kind() == east.KindStrikethrough {
			b.WriteString("<s>")
			for c := n.FirstChild(); c != nil; c = c.NextSibling() {
				renderInlineNode(b, c, src)
			}
			b.WriteString("</s>")
			return
		}
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			renderInlineNode(b, c, src)
		}
	}
}
