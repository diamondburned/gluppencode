package window

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/diamondburned/gotk4-sourceview/pkg/gtksource/v5"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
	"libdb.so/gluppencode/internal/opencode"
)

const (
	composerCollapsedMaxHeight = 140
	composerExpandedMinHeight  = 220
	composerExpandedMaxHeight  = 360
	composerMarkdownMaxBytes   = 128 * 1024
)

func (w *Window) setupComposerMultiline() {
	if w.composerExpandBtn != nil {
		w.composerExpandBtn.ConnectClicked(func() {
			w.setComposerExpanded(!w.composerExpanded)
			if w.inputView != nil {
				w.inputView.GrabFocus()
			}
		})
	}
}

func (w *Window) setComposerExpanded(expanded bool) {
	if w.composerExpanded == expanded {
		return
	}
	w.setComposerExpandedUI(expanded)
	if d := w.activeDraft(); d != nil {
		d.expanded = expanded
		w.markDraftDirtyAndScheduleSave(d)
	}
}

func (w *Window) setComposerExpandedUI(expanded bool) {
	w.composerExpanded = expanded

	if w.composerExpandBtn != nil {
		if expanded {
			w.composerExpandBtn.SetIconName("unfold-less-symbolic")
			w.composerExpandBtn.SetTooltipText("Collapse Composer")
		} else {
			w.composerExpandBtn.SetIconName("unfold-more-symbolic")
			w.composerExpandBtn.SetTooltipText("Expand Composer (Multiline Mode)")
		}
	}
	if w.inputScroll != nil {
		if expanded {
			w.inputScroll.SetMaxContentHeight(composerExpandedMaxHeight)
			w.inputScroll.SetMinContentHeight(composerExpandedMinHeight)
		} else {
			w.inputScroll.SetMinContentHeight(-1)
			w.inputScroll.SetMaxContentHeight(composerCollapsedMaxHeight)
		}
	}
	w.syncInputScrollPolicy()
	w.applySendShortcutUI()
}

// composerEnterAction resolves Enter / Shift+Enter / Alt+Enter / Ctrl+Enter key presses for
// the capture and bubble key controllers on inputView.
func (w *Window) composerEnterAction(capture bool, keyval uint, state gdk.ModifierType) (opencode.InboxDelivery, bool) {
	if keyval != gdk.KEY_Return && keyval != gdk.KEY_KP_Enter {
		return "", false
	}
	hasCtrl := state.Has(gdk.ControlMask)
	hasShift := state.Has(gdk.ShiftMask)
	hasAlt := state.Has(gdk.AltMask)
	if hasCtrl && !hasShift && !hasAlt {
		if w.composerExpanded || !w.prefs.CtrlEnterToSend {
			return opencode.DeliverySteer, true
		}
		return opencode.DeliveryQueue, true
	}
	if w.composerExpanded && hasAlt && !hasCtrl && !hasShift {
		return opencode.DeliveryQueue, true
	}
	if capture || w.composerExpanded || hasShift || hasCtrl || hasAlt {
		return "", false
	}
	if w.prefs.CtrlEnterToSend {
		return "", false
	}
	return opencode.DeliveryQueue, true
}

func (w *Window) syncInputScrollPolicy() {
	if w.inputScroll == nil || w.inputBuffer == nil {
		return
	}
	if w.composerExpanded || w.inputBuffer.LineCount() > 4 {
		w.inputScroll.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	} else {
		w.inputScroll.SetPolicy(gtk.PolicyNever, gtk.PolicyExternal)
	}
}

func (w *Window) setupComposerMarkdown() {
	w.setupComposerBufferMarkdown(w.inputBuffer)
}

func (w *Window) setupComposerBufferMarkdown(buf *gtksource.Buffer) {
	if buf == nil {
		return
	}
	buf.SetHighlightSyntax(false)
	buf.SetHighlightMatchingBrackets(false)
	w.ensureBufferMarkdownTags(buf)
	w.applyComposerMarkdownHighlightingToBuffer(buf)
}

func (w *Window) ensureComposerMarkdownTags() {
	w.ensureBufferMarkdownTags(w.inputBuffer)
}

func (w *Window) ensureBufferMarkdownTags(buf *gtksource.Buffer) (bold, italic, strike, mono, codeBlock *gtk.TextTag) {
	if buf == nil {
		return nil, nil, nil, nil, nil
	}
	table := buf.TagTable()
	if table == nil {
		return nil, nil, nil, nil, nil
	}
	ensureTag := func(name string, configure func(t *gtk.TextTag)) *gtk.TextTag {
		if existing := table.Lookup(name); existing != nil {
			return existing
		}
		t := gtk.NewTextTag(name)
		configure(t)
		table.Add(t)
		return t
	}
	bold = ensureTag("glup-md-bold", func(t *gtk.TextTag) {
		t.SetObjectProperty("weight", int(pango.WeightBold))
	})
	italic = ensureTag("glup-md-italic", func(t *gtk.TextTag) {
		t.SetObjectProperty("style", pango.StyleItalic)
	})
	strike = ensureTag("glup-md-strike", func(t *gtk.TextTag) {
		t.SetObjectProperty("strikethrough", true)
	})
	mono = ensureTag("glup-md-mono", func(t *gtk.TextTag) {
		t.SetObjectProperty("family", "Monospace")
		t.SetObjectProperty("background", "#787c8826")
	})
	codeBlock = ensureTag("glup-md-codeblock", func(t *gtk.TextTag) {
		t.SetObjectProperty("family", "Monospace")
		t.SetObjectProperty("paragraph-background", "#787c8818")
	})
	if buf == w.inputBuffer {
		w.mdTagBold = bold
		w.mdTagItalic = italic
		w.mdTagStrike = strike
		w.mdTagMono = mono
		w.mdTagCodeBlock = codeBlock
	}
	return bold, italic, strike, mono, codeBlock
}

func (w *Window) applyComposerMarkdownHighlighting() {
	w.applyComposerMarkdownHighlightingToBuffer(w.inputBuffer)
}

func (w *Window) applyComposerMarkdownHighlightingToBuffer(buf *gtksource.Buffer) {
	if buf == nil {
		return
	}
	bold, italic, strike, mono, codeBlock := w.ensureBufferMarkdownTags(buf)
	start, end := buf.Bounds()
	if start == nil || end == nil {
		return
	}
	for _, tag := range []*gtk.TextTag{
		bold,
		italic,
		strike,
		mono,
		codeBlock,
	} {
		if tag != nil {
			buf.RemoveTag(tag, start, end)
		}
	}

	text := buf.Text(start, end, false)
	if text == "" || len(text) > composerMarkdownMaxBytes {
		return
	}

	for _, sp := range scanComposerMarkdownSpans(text) {
		var tag *gtk.TextTag
		switch sp.kind {
		case mdSpanBold:
			tag = bold
		case mdSpanItalic:
			tag = italic
		case mdSpanStrike:
			tag = strike
		case mdSpanMono:
			tag = mono
		case mdSpanCodeBlock:
			tag = codeBlock
		}
		if tag == nil {
			continue
		}
		sIter := buf.IterAtOffset(sp.start)
		eIter := buf.IterAtOffset(sp.end)
		if sIter != nil && eIter != nil {
			buf.ApplyTag(tag, sIter, eIter)
		}
	}
}

type mdSpanKind uint8

const (
	mdSpanBold mdSpanKind = iota + 1
	mdSpanItalic
	mdSpanStrike
	mdSpanMono
	mdSpanCodeBlock
)

type mdSpan struct {
	kind  mdSpanKind
	start int // rune offset (inclusive)
	end   int // rune offset (exclusive)
}

type mdLineRange struct {
	start int // byte offset (inclusive)
	end   int // byte offset (exclusive, before \r?\n)
}

func scanComposerMarkdownSpans(text string) []mdSpan {
	if text == "" {
		return nil
	}

	byteToRune := make([]int, len(text)+1)
	runeIdx := 0
	for i := range text {
		byteToRune[i] = runeIdx
		runeIdx++
	}
	byteToRune[len(text)] = runeIdx

	var out []mdSpan
	var paraLines []mdLineRange
	inFence := false
	var fenceChar byte
	fenceLen := 0
	fenceStart := 0

	flushPara := func() {
		if len(paraLines) == 0 {
			return
		}
		scanParagraphInline(text, paraLines, byteToRune, &out)
		paraLines = paraLines[:0]
	}

	pos := 0
	for pos < len(text) {
		lineStart := pos
		nl := strings.IndexByte(text[pos:], '\n')
		var lineEnd int
		if nl == -1 {
			lineEnd = len(text)
			pos = len(text)
		} else {
			lineEnd = pos + nl
			pos = lineEnd + 1
			if lineEnd > lineStart && text[lineEnd-1] == '\r' {
				lineEnd--
			}
		}

		line := text[lineStart:lineEnd]
		if !inFence {
			if fc, fl, ok := parseFenceOpener(line); ok {
				flushPara()
				inFence = true
				fenceChar = fc
				fenceLen = fl
				fenceStart = lineStart
				continue
			}
			if isBlankLine(line) {
				flushPara()
				continue
			}
			if isATXHeading(line) {
				flushPara()
				out = append(out, mdSpan{
					kind:  mdSpanBold,
					start: byteToRune[lineStart],
					end:   byteToRune[lineEnd],
				})
				scanParagraphInline(text, []mdLineRange{{start: lineStart, end: lineEnd}}, byteToRune, &out)
				continue
			}
			paraLines = append(paraLines, mdLineRange{start: lineStart, end: lineEnd})
			continue
		}

		if isFenceCloser(line, fenceChar, fenceLen) {
			out = append(out, mdSpan{
				kind:  mdSpanCodeBlock,
				start: byteToRune[fenceStart],
				end:   byteToRune[lineEnd],
			})
			inFence = false
		}
	}

	if inFence {
		out = append(out, mdSpan{
			kind:  mdSpanCodeBlock,
			start: byteToRune[fenceStart],
			end:   byteToRune[len(text)],
		})
	}
	flushPara()
	return out
}

func skipLeadingSpaces(line string, max int) (idx int, ok bool) {
	for idx < len(line) && idx < max && line[idx] == ' ' {
		idx++
	}
	return idx, idx < len(line)
}

func parseFenceOpener(line string) (char byte, length int, ok bool) {
	i, hasContent := skipLeadingSpaces(line, 3)
	if !hasContent {
		return 0, 0, false
	}
	ch := line[i]
	if ch != '`' && ch != '~' {
		return 0, 0, false
	}
	j := i
	for j < len(line) && line[j] == ch {
		j++
	}
	if j-i < 3 {
		return 0, 0, false
	}
	if ch == '`' && strings.IndexByte(line[j:], '`') != -1 {
		return 0, 0, false
	}
	return ch, j - i, true
}

func isFenceCloser(line string, fenceChar byte, fenceLen int) bool {
	i, hasContent := skipLeadingSpaces(line, 3)
	if !hasContent || line[i] != fenceChar {
		return false
	}
	j := i
	for j < len(line) && line[j] == fenceChar {
		j++
	}
	if j-i < fenceLen {
		return false
	}
	for k := j; k < len(line); k++ {
		if line[k] != ' ' && line[k] != '\t' {
			return false
		}
	}
	return true
}

func isBlankLine(line string) bool {
	for i := 0; i < len(line); i++ {
		if line[i] != ' ' && line[i] != '\t' {
			return false
		}
	}
	return true
}

func isATXHeading(line string) bool {
	i, hasContent := skipLeadingSpaces(line, 3)
	if !hasContent || line[i] != '#' {
		return false
	}
	j := i
	for j < len(line) && line[j] == '#' {
		j++
	}
	if j-i < 1 || j-i > 6 {
		return false
	}
	return j < len(line) && (line[j] == ' ' || line[j] == '\t')
}

type mdDelim struct {
	ch        byte
	start     int
	end       int
	count     int
	origCount int
	canOpen   bool
	canClose  bool
}

func scanParagraphInline(text string, lines []mdLineRange, byteToRune []int, out *[]mdSpan) {
	paraStart := lines[0].start
	paraEnd := lines[len(lines)-1].end

	type byteRange struct {
		start int
		end   int
	}
	var opaque []byteRange

	// Pass 1: inline code spans (`...`, ``...``, etc.).
	for i := paraStart; i < paraEnd; {
		if text[i] == '\\' && i+1 < paraEnd {
			i += 2
			continue
		}
		if text[i] != '`' {
			i++
			continue
		}
		n := 0
		for i+n < paraEnd && text[i+n] == '`' {
			n++
		}
		closeStart := -1
		for j := i + n; j < paraEnd; {
			if text[j] != '`' {
				j++
				continue
			}
			m := 0
			for j+m < paraEnd && text[j+m] == '`' {
				m++
			}
			if m == n {
				closeStart = j
				break
			}
			j += m
		}
		if closeStart != -1 {
			closeEnd := closeStart + n
			*out = append(*out, mdSpan{
				kind:  mdSpanMono,
				start: byteToRune[i],
				end:   byteToRune[closeEnd],
			})
			opaque = append(opaque, byteRange{start: i, end: closeEnd})
			i = closeEnd
			continue
		}
		i += n
	}

	// Pass 2: collect emphasis and strikethrough delimiter runs outside code spans.
	var delims []mdDelim
	opIdx := 0
	for i := paraStart; i < paraEnd; {
		for opIdx < len(opaque) && i >= opaque[opIdx].end {
			opIdx++
		}
		if opIdx < len(opaque) && i >= opaque[opIdx].start {
			i = opaque[opIdx].end
			continue
		}
		if text[i] == '\\' && i+1 < paraEnd {
			i += 2
			continue
		}
		ch := text[i]
		if ch != '*' && ch != '_' && ch != '~' {
			i++
			continue
		}
		runEnd := i
		for runEnd < paraEnd && text[runEnd] == ch {
			runEnd++
		}
		runLen := runEnd - i
		if ch == '~' && runLen != 2 {
			i = runEnd
			continue
		}

		prevSpace := true
		prevPunct := false
		if i > paraStart {
			r, _ := utf8.DecodeLastRuneInString(text[paraStart:i])
			prevSpace = unicode.IsSpace(r)
			prevPunct = isMarkdownPunct(r)
		}

		nextSpace := true
		nextPunct := false
		if runEnd < paraEnd {
			r, _ := utf8.DecodeRuneInString(text[runEnd:paraEnd])
			nextSpace = unicode.IsSpace(r)
			nextPunct = isMarkdownPunct(r)
		}

		leftFlanking := !nextSpace && (!nextPunct || prevSpace || prevPunct)
		rightFlanking := !prevSpace && (!prevPunct || nextSpace || nextPunct)

		var canOpen, canClose bool
		switch ch {
		case '_':
			canOpen = leftFlanking && (!rightFlanking || prevPunct)
			canClose = rightFlanking && (!leftFlanking || nextPunct)
		default:
			canOpen = leftFlanking
			canClose = rightFlanking
		}

		delims = append(delims, mdDelim{
			ch:        ch,
			start:     i,
			end:       runEnd,
			count:     runLen,
			origCount: runLen,
			canOpen:   canOpen,
			canClose:  canClose,
		})
		i = runEnd
	}

	// Pass 3: match closers with nearest compatible openers in O(N).
	delimChIdx := func(ch byte) int {
		switch ch {
		case '_':
			return 1
		case '~':
			return 2
		default:
			return 0
		}
	}
	var openersBottom [3][6]int
	for ci := 0; ci < len(delims); ci++ {
		for delims[ci].canClose && delims[ci].count > 0 {
			slot := (delims[ci].origCount % 3) * 2
			if delims[ci].canOpen {
				slot++
			}
			bottom := openersBottom[delimChIdx(delims[ci].ch)][slot]
			oi := -1
			for k := ci - 1; k >= bottom; k-- {
				if !delims[k].canOpen || delims[k].count == 0 || delims[k].ch != delims[ci].ch {
					continue
				}
				if delims[ci].ch == '~' && (delims[k].count < 2 || delims[ci].count < 2) {
					continue
				}
				if delims[ci].ch != '~' &&
					((delims[k].canOpen && delims[k].canClose) || (delims[ci].canOpen && delims[ci].canClose)) &&
					(delims[k].origCount+delims[ci].origCount)%3 == 0 &&
					(delims[k].origCount%3 != 0 || delims[ci].origCount%3 != 0) {
					continue
				}
				oi = k
				break
			}
			if oi == -1 {
				openersBottom[delimChIdx(delims[ci].ch)][slot] = ci
				if !delims[ci].canOpen {
					delims[ci].count = 0
				}
				break
			}

			use := 1
			kind := mdSpanItalic
			if delims[ci].ch == '~' {
				use = 2
				kind = mdSpanStrike
			} else if delims[oi].count >= 2 && delims[ci].count >= 2 {
				use = 2
				kind = mdSpanBold
			}

			spanStart := delims[oi].end - use
			spanEnd := delims[ci].start + use
			*out = append(*out, mdSpan{
				kind:  kind,
				start: byteToRune[spanStart],
				end:   byteToRune[spanEnd],
			})

			for m := oi + 1; m < ci; m++ {
				delims[m].count = 0
			}
			delims[oi].count -= use
			delims[oi].end -= use
			delims[ci].count -= use
			delims[ci].start += use
		}
	}
}

func isMarkdownPunct(r rune) bool {
	return unicode.IsPunct(r) || unicode.IsSymbol(r)
}
