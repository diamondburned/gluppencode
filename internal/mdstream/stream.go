// Package mdstream provides an incremental prefix-caching streaming Markdown parser and GTK4 widget.
package mdstream

import (
	"bytes"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// DefaultInterval is the default batch coalescing window for streaming markdown deltas.
const DefaultInterval = 35 * time.Millisecond

var mdParser = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM,
	),
).Parser()

// Parser returns the shared Goldmark GFM parser used by mdstream.
func Parser() parser.Parser {
	return mdParser
}

// BlockAST holds a parsed markdown block (or tail block set) along with its
// 0-indexed source byte slice and raw markdown string.
type BlockAST struct {
	Source []byte   // slice of source for this block (or full source up to block end)
	Node   ast.Node // the top-level ast.Node (or synthetic document wrapping it)
	Raw    string   // raw markdown text of this block
}

// RenderPatch represents an incremental update emitted by the streaming markdown engine.
type RenderPatch struct {
	Committed      []BlockAST // Newly frozen prefix blocks since last patch (append once, never touch again)
	Tail           *BlockAST  // Current active tail block(s) to replace the single tail widget (nil if empty or finalized)
	Final          bool
	ResetCommitted bool
}

// ParseBlock parses a raw markdown snippet into a BlockAST whose Source and
// Node segment offsets are self-contained.
func ParseBlock(raw string) (BlockAST, bool) {
	trimmed := trimRightNewlines(raw)
	if len(bytes.TrimSpace([]byte(trimmed))) == 0 {
		return BlockAST{}, false
	}
	src := []byte(trimmed)
	doc := mdParser.Parse(text.NewReader(src))
	if doc.ChildCount() == 0 {
		return BlockAST{}, false
	}
	var node ast.Node = doc
	if doc.FirstChild() != nil && doc.FirstChild().NextSibling() == nil {
		child := doc.FirstChild()
		doc.RemoveChild(doc, child)
		node = child
	}
	return BlockAST{
		Source: src,
		Node:   node,
		Raw:    trimmed,
	}, true
}

// ParseStatic parses a complete static markdown document into its top-level BlockASTs.
func ParseStatic(markdown string) []BlockAST {
	e := NewEngine()
	patch := e.Finalize(markdown)
	return patch.Committed
}

// Engine is a deterministic, pure-Go prefix-caching markdown stream state machine.
// It re-parses only the uncommitted suffix of the markdown stream and freezes
// completed top-level prefix blocks once a subsequent sibling block begins.
type Engine struct {
	buf             []byte
	committedOffset int
	pending         bool
	lastTailRaw     string
	hasTail         bool
}

// NewEngine creates a new prefix-caching markdown stream Engine.
func NewEngine() *Engine {
	return &Engine{}
}

// Reset clears the engine state for reuse.
func (e *Engine) Reset() {
	e.buf = e.buf[:0]
	e.committedOffset = 0
	e.pending = false
	e.lastTailRaw = ""
	e.hasTail = false
}

// Append appends a new markdown token delta to the uncommitted buffer.
func (e *Engine) Append(delta string) {
	if delta == "" {
		return
	}
	e.buf = append(e.buf, delta...)
	e.pending = true
}

// Step appends delta and immediately evaluates a RenderPatch if changes occurred.
func (e *Engine) Step(delta string) (RenderPatch, bool) {
	e.Append(delta)
	return e.Flush()
}

// Flush computes a RenderPatch for any pending appended deltas since the last Flush.
// It returns false if no new blocks were committed and the active tail did not change.
func (e *Engine) Flush() (RenderPatch, bool) {
	if !e.pending {
		return RenderPatch{}, false
	}
	e.pending = false

	uncommitted := e.buf[e.committedOffset:]
	if len(bytes.TrimSpace(uncommitted)) == 0 {
		return RenderPatch{}, false
	}

	committed, consumed, tail := splitUncommitted(uncommitted, false)
	e.committedOffset += consumed

	if len(committed) == 0 {
		if tail == nil && !e.hasTail {
			return RenderPatch{}, false
		}
		if tail != nil && e.hasTail && tail.Raw == e.lastTailRaw {
			return RenderPatch{}, false
		}
	}

	if tail != nil {
		e.hasTail = true
		e.lastTailRaw = tail.Raw
	} else {
		e.hasTail = false
		e.lastTailRaw = ""
	}

	return RenderPatch{
		Committed: committed,
		Tail:      tail,
		Final:     false,
	}, true
}

// Finalize flushes any remaining tokens and commits all remaining top-level blocks.
// If fullText is non-empty, the engine ensures the accumulated buffer matches fullText.
func (e *Engine) Finalize(fullText string) RenderPatch {
	var resetCommitted bool
	if fullText != "" {
		fullBytes := []byte(fullText)
		if e.committedOffset > len(fullBytes) || !bytes.Equal(e.buf[:e.committedOffset], fullBytes[:e.committedOffset]) {
			resetCommitted = true
			e.committedOffset = 0
		} else if e.committedOffset == 0 && len(e.buf) > 0 && string(e.buf) != fullText {
			resetCommitted = true
		}
		e.buf = fullBytes
	}
	e.pending = false

	uncommitted := e.buf[e.committedOffset:]
	committed, consumed, _ := splitUncommitted(uncommitted, true)
	e.committedOffset += consumed
	e.hasTail = false
	e.lastTailRaw = ""

	return RenderPatch{
		Committed:      committed,
		Tail:           nil,
		Final:          true,
		ResetCommitted: resetCommitted,
	}
}

type blockSpan struct {
	node   ast.Node
	start  int
	end    int
	closed bool
}

// splitUncommitted parses the uncommitted markdown slice and splits it into
// newly-committable prefix blocks and the remaining active tail block(s).
func splitUncommitted(src []byte, final bool) (committed []BlockAST, consumed int, tail *BlockAST) {
	if len(bytes.TrimSpace(src)) == 0 {
		return nil, len(src), nil
	}

	// If the uncommitted slice starts with an unclosed top-level fenced code block,
	// keep the entire uncommitted slice in Tail until the fence closes and a sibling follows
	// (or Finalize is called).
	firstNonBlank := skipBlankLines(src, 0)
	if !final {
		if fenceEnd, isFence, isClosed := scanTopLevelFence(src, firstNonBlank); isFence {
			if !isClosed {
				t, ok := ParseBlock(string(src[firstNonBlank:]))
				if !ok {
					return nil, 0, nil
				}
				return nil, 0, &t
			}
			// Even if the fence line is closed, it remains in Tail until a subsequent
			// non-blank block starts after fenceEnd.
			afterFence := skipBlankLines(src, fenceEnd)
			if afterFence >= len(src) {
				t, ok := ParseBlock(string(src[firstNonBlank:]))
				if !ok {
					return nil, 0, nil
				}
				return nil, 0, &t
			}
		}
	}

	doc := mdParser.Parse(text.NewReader(src))
	if doc.ChildCount() == 0 {
		return nil, 0, nil
	}

	var nodes []ast.Node
	for c := doc.FirstChild(); c != nil; c = c.NextSibling() {
		nodes = append(nodes, c)
	}

	spans := computeBlockSpans(src, nodes)
	if len(spans) == 0 {
		return nil, 0, nil
	}

	commitCount := 0
	if final {
		commitCount = len(spans)
	} else if len(spans) >= 2 {
		// Commit stable prefix blocks that are followed by another sibling block.
		for i := 0; i < len(spans)-1; i++ {
			if !canCommitSpan(src, spans, i) {
				break
			}
			commitCount = i + 1
		}
	}

	for i := 0; i < commitCount; i++ {
		sp := spans[i]
		raw := string(src[sp.start:sp.end])
		if blk, ok := ParseBlock(raw); ok {
			committed = append(committed, blk)
		}
		consumed = sp.end
	}

	if final {
		consumed = len(src)
		return committed, consumed, nil
	}

	// Advance consumed past any blank lines immediately following the last committed block
	// so future parses start cleanly at the tail block.
	if commitCount > 0 {
		nextStart := spans[commitCount].start
		if nextStart > consumed {
			consumed = nextStart
		}
	}

	tailSlice := src[consumed:]
	if blk, ok := ParseBlock(string(tailSlice)); ok {
		tail = &blk
	}
	return committed, consumed, tail
}

func computeBlockSpans(src []byte, nodes []ast.Node) []blockSpan {
	spans := make([]blockSpan, len(nodes))
	cursor := 0

	for i, n := range nodes {
		start := skipBlankLines(src, cursor)
		if minSeg, _, ok := nodeSegmentBounds(n); ok {
			segLineStart := lineStart(src, minSeg)
			if n.Kind() == ast.KindFencedCodeBlock {
				// If FencedCodeBlock has no Info, minSeg is on the first content line
				// after the opening fence; start from skipBlankLines is the opening fence line.
				f := n.(*ast.FencedCodeBlock)
				if f.Info != nil {
					start = lineStart(src, f.Info.Segment.Start)
				}
			} else if segLineStart > start && !isContainerWithPreMarker(n) {
				start = segLineStart
			}
		}

		end, closed := blockEndOffset(src, n, start)
		// A container whose last child is a fence implicitly closed by the container
		// ending (e.g. "- ```\nx\n```") scans to EOF; it cannot extend past the line
		// where Goldmark started the next sibling.
		if i+1 < len(nodes) {
			if ns, ok := firstLineStart(src, nodes[i+1]); ok && ns > start && end > ns {
				end, closed = ns, true
			}
		}
		spans[i] = blockSpan{
			node:   n,
			start:  start,
			end:    end,
			closed: closed,
		}
		cursor = end
	}

	// Ensure spans do not overlap if a subsequent sibling's start is known.
	for i := 0; i < len(spans)-1; i++ {
		if spans[i].end > spans[i+1].start && spans[i+1].start > spans[i].start {
			spans[i].end = spans[i+1].start
		}
	}

	return spans
}

// firstLineStart returns the offset of the line on which n begins, as far as its
// segments tell (the opening fence line for fenced code blocks).
func firstLineStart(src []byte, n ast.Node) (int, bool) {
	if f, ok := n.(*ast.FencedCodeBlock); ok {
		switch {
		case f.Info != nil:
			return lineStart(src, f.Info.Segment.Start), true
		case f.Lines().Len() > 0:
			if first := lineStart(src, f.Lines().At(0).Start); first > 0 {
				return lineStart(src, first-1), true
			}
		}
		return 0, false
	}
	minSeg, _, ok := nodeSegmentBounds(n)
	if !ok {
		return 0, false
	}
	return lineStart(src, minSeg), true
}

func isContainerWithPreMarker(n ast.Node) bool {
	switch n.Kind() {
	case ast.KindList, ast.KindListItem, ast.KindBlockquote, ast.KindFencedCodeBlock:
		return true
	default:
		return false
	}
}

// canCommitSpan checks whether spans[i] (which is followed by spans[i+1]) is
// guaranteed to be stable and unable to be mutated by future tokens at EOF.
func canCommitSpan(src []byte, spans []blockSpan, i int) bool {
	sp := spans[i]
	if !sp.closed {
		return false
	}

	// If there is another block after i+1 (i.e. i+1 is not the last block),
	// then spans[i] is at least 2 blocks behind the active cursor and is completely stable.
	if i+1 < len(spans)-1 {
		return true
	}

	next := spans[i+1]
	nextRaw := bytes.TrimRight(src[next.start:next.end], "\r\n")
	hasBlankBetween := hasBlankLineBetween(src, sp.end, next.start) || next.node.HasBlankPreviousLines()

	// 1. If next directly follows a block that accepts lazy continuation lines and
	// next's first line is still being typed at EOF, its interpretation is not final:
	// `#` parses as an empty heading until `#123` arrives, `-` as a Setext underline
	// until `-foo`, `<div` as an HTML block until `<divx`, all of which fold back
	// into sp.
	switch sp.node.Kind() {
	case ast.KindParagraph, ast.KindList, ast.KindBlockquote:
		if !hasBlankBetween && bytes.IndexByte(src[next.start:], '\n') == -1 {
			return false
		}
	}

	// 2. If sp is a List or Blockquote, check if next is an incomplete list marker or
	// continuation prefix right at EOF (e.g. `-`, `*`, `+`, `1`, `1.`, `>`) before the space.
	if sp.node.Kind() == ast.KindList || sp.node.Kind() == ast.KindBlockquote {
		if isIncompleteContinuationMarker(nextRaw, src[next.start:]) {
			return false
		}
	}

	return true
}

func hasBlankLineBetween(src []byte, aEnd, bStart int) bool {
	if bStart > aEnd {
		return len(bytes.TrimSpace(src[aEnd:bStart])) == 0
	}
	return false
}

func isIncompleteContinuationMarker(nextRaw, tailFromNext []byte) bool {
	// If the trailing block already contains a newline, its first line is complete;
	// if it were going to merge into the preceding List/Blockquote, Goldmark would
	// already have merged it.
	if bytes.IndexByte(tailFromNext, '\n') != -1 {
		return false
	}
	s := bytes.TrimLeft(nextRaw, " \t")
	if len(s) == 0 {
		return true
	}
	// Single-char bullet or blockquote marker without space yet: "-", "*", "+", ">"
	if len(s) == 1 && (s[0] == '-' || s[0] == '*' || s[0] == '+' || s[0] == '>') {
		return true
	}
	// Ordered list marker prefix at EOF: digits optionally followed by '.' or ')'
	digits := 0
	for digits < len(s) && s[digits] >= '0' && s[digits] <= '9' {
		digits++
	}
	if digits > 0 {
		rest := s[digits:]
		if len(rest) == 0 || (len(rest) == 1 && (rest[0] == '.' || rest[0] == ')')) {
			return true
		}
	}
	return false
}

// blockEndOffset finds the ending byte offset (at a line boundary) of block node n
// starting at start, and reports whether the block (including any nested fenced code block)
// is properly closed.
func blockEndOffset(src []byte, n ast.Node, start int) (end int, closed bool) {
	switch n.Kind() {
	case ast.KindFencedCodeBlock:
		fenceEnd, isFence, isClosed := scanFenceFromNode(src, n.(*ast.FencedCodeBlock), start)
		if isFence {
			return fenceEnd, isClosed
		}
		return len(src), false

	case ast.KindList, ast.KindListItem, ast.KindBlockquote:
		if last := n.LastChild(); last != nil && last.Type() == ast.TypeBlock {
			lastStart := start
			if minSeg, _, ok := nodeSegmentBounds(last); ok {
				if last.Kind() == ast.KindFencedCodeBlock {
					fc := last.(*ast.FencedCodeBlock)
					if fc.Info != nil {
						lastStart = lineStart(src, fc.Info.Segment.Start)
					} else if fc.Lines().Len() > 0 {
						firstContentLine := lineStart(src, fc.Lines().At(0).Start)
						if firstContentLine > 0 {
							lastStart = lineStart(src, firstContentLine-1)
						}
					}
				} else {
					ls := lineStart(src, minSeg)
					if ls > lastStart {
						lastStart = ls
					}
				}
			}
			return blockEndOffset(src, last, lastStart)
		}
		if _, maxSeg, ok := nodeSegmentBounds(n); ok && maxSeg > 0 {
			return lineEnd(src, maxSeg-1), true
		}
		return lineEnd(src, start), true

	case ast.KindHeading:
		endLine := lineEnd(src, start)
		if _, maxSeg, ok := nodeSegmentBounds(n); ok && maxSeg > 0 {
			endLine = lineEnd(src, maxSeg-1)
		}
		firstLine := trimLeadingSpacesUpTo3(src[start:lineEnd(src, start)])
		if len(firstLine) == 0 || firstLine[0] != '#' {
			// Setext heading: include the underline line immediately following the heading text.
			if endLine < len(src) {
				endLine = lineEnd(src, endLine)
			}
		}
		return endLine, true

	case ast.KindThematicBreak:
		return lineEnd(src, start), true

	default:
		// Check if it's a GFM Table (Kind.String() == "Table").
		if n.Kind().String() == "Table" {
			headerEnd := lineEnd(src, start)
			delimEnd := headerEnd
			if headerEnd < len(src) {
				delimEnd = lineEnd(src, headerEnd)
			}
			if _, maxSeg, ok := nodeSegmentBounds(n); ok && maxSeg > 0 {
				segEnd := lineEnd(src, maxSeg-1)
				if segEnd > delimEnd {
					return segEnd, true
				}
			}
			return delimEnd, true
		}

		if _, maxSeg, ok := nodeSegmentBounds(n); ok && maxSeg > 0 {
			return lineEnd(src, maxSeg-1), true
		}
		return lineEnd(src, start), true
	}
}

func scanFenceFromNode(src []byte, n *ast.FencedCodeBlock, start int) (end int, isFence bool, isClosed bool) {
	openLineStart := start
	if n.Info != nil {
		openLineStart = lineStart(src, n.Info.Segment.Start)
	} else if n.Lines().Len() > 0 {
		firstContentLine := lineStart(src, n.Lines().At(0).Start)
		if firstContentLine > 0 {
			openLineStart = lineStart(src, firstContentLine-1)
		}
	}
	openLineEnd := lineEnd(src, openLineStart)
	fenceChar, fenceLen, ok := parseOpeningFenceLine(src[openLineStart:openLineEnd])
	if !ok {
		return len(src), false, false
	}
	// Opening line must end with a newline before a closing line can appear.
	if openLineEnd == 0 || src[openLineEnd-1] != '\n' {
		return len(src), true, false
	}

	afterContent := openLineEnd
	if lines := n.Lines(); lines.Len() > 0 {
		lastSeg := lines.At(lines.Len() - 1)
		if lastSeg.Stop > 0 {
			afterContent = lineEnd(src, lastSeg.Stop-1)
		}
	}

	if afterContent >= len(src) {
		return len(src), true, false
	}

	closeLineEnd := lineEnd(src, afterContent)
	closeLine := src[afterContent:closeLineEnd]
	if isClosingFenceLine(closeLine, fenceChar, fenceLen) {
		hasNewline := closeLineEnd > afterContent && src[closeLineEnd-1] == '\n'
		return closeLineEnd, true, hasNewline || n.NextSibling() != nil
	}

	return len(src), true, false
}

func scanTopLevelFence(src []byte, start int) (end int, isFence bool, isClosed bool) {
	if start >= len(src) {
		return len(src), false, false
	}
	openLineEnd := lineEnd(src, start)
	fenceChar, fenceLen, ok := parseOpeningFenceLine(src[start:openLineEnd])
	if !ok {
		return len(src), false, false
	}
	if openLineEnd == 0 || src[openLineEnd-1] != '\n' {
		return len(src), true, false
	}

	pos := openLineEnd
	for pos < len(src) {
		lEnd := lineEnd(src, pos)
		line := src[pos:lEnd]
		if isClosingFenceLine(line, fenceChar, fenceLen) {
			hasNewline := lEnd > pos && src[lEnd-1] == '\n'
			return lEnd, true, hasNewline
		}
		pos = lEnd
	}
	return len(src), true, false
}

func parseOpeningFenceLine(line []byte) (fenceChar byte, fenceLen int, ok bool) {
	s := stripBlockPrefix(line)
	if len(s) < 3 {
		return 0, 0, false
	}
	ch := s[0]
	if ch != '`' && ch != '~' {
		return 0, 0, false
	}
	count := 0
	for count < len(s) && s[count] == ch {
		count++
	}
	if count < 3 {
		return 0, 0, false
	}
	// Backtick fence info strings cannot contain backticks in CommonMark.
	if ch == '`' && bytes.IndexByte(s[count:], '`') != -1 {
		return 0, 0, false
	}
	return ch, count, true
}

func isClosingFenceLine(line []byte, fenceChar byte, minLen int) bool {
	s := stripBlockPrefix(line)
	count := 0
	for count < len(s) && s[count] == fenceChar {
		count++
	}
	if count < minLen {
		return false
	}
	for _, b := range s[count:] {
		if b != ' ' && b != '\t' && b != '\r' && b != '\n' {
			return false
		}
	}
	return true
}

// stripBlockPrefix strips optional blockquote '>' markers and up to 3 leading spaces.
func stripBlockPrefix(line []byte) []byte {
	s := line
	for {
		s = trimLeadingSpacesUpTo3(s)
		if len(s) > 0 && s[0] == '>' {
			s = s[1:]
			if len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
				s = s[1:]
			}
			continue
		}
		break
	}
	// Also allow list item indentation if inside a list item.
	s = bytes.TrimLeft(s, " \t")
	return s
}

func trimLeadingSpacesUpTo3(b []byte) []byte {
	i := 0
	for i < len(b) && i < 3 && b[i] == ' ' {
		i++
	}
	return b[i:]
}

func nodeSegmentBounds(n ast.Node) (minPos, maxPos int, ok bool) {
	first := true
	update := func(start, stop int) {
		if start < 0 || stop < start {
			return
		}
		if first {
			minPos = start
			maxPos = stop
			first = false
			return
		}
		if start < minPos {
			minPos = start
		}
		if stop > maxPos {
			maxPos = stop
		}
	}

	_ = ast.Walk(n, func(curr ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if curr.Type() == ast.TypeBlock {
			segs := curr.Lines()
			for i := 0; i < segs.Len(); i++ {
				seg := segs.At(i)
				update(seg.Start, seg.Stop)
			}
		}
		switch c := curr.(type) {
		case *ast.FencedCodeBlock:
			if c.Info != nil {
				update(c.Info.Segment.Start, c.Info.Segment.Stop)
			}
		case *ast.Text:
			update(c.Segment.Start, c.Segment.Stop)
		}
		return ast.WalkContinue, nil
	})

	return minPos, maxPos, !first
}

func skipBlankLines(src []byte, pos int) int {
	for pos < len(src) {
		lEnd := lineEnd(src, pos)
		if len(bytes.TrimSpace(src[pos:lEnd])) != 0 {
			return pos
		}
		pos = lEnd
	}
	return pos
}

func lineStart(src []byte, pos int) int {
	if pos > len(src) {
		pos = len(src)
	}
	for pos > 0 && src[pos-1] != '\n' {
		pos--
	}
	return pos
}

func lineEnd(src []byte, pos int) int {
	if pos < 0 {
		pos = 0
	}
	for pos < len(src) {
		if src[pos] == '\n' {
			return pos + 1
		}
		pos++
	}
	return len(src)
}

func trimRightNewlines(s string) string {
	end := len(s)
	for end > 0 && (s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[:end]
}

// Streamer runs a per-stream background goroutine that coalesces incoming Append
// calls over a configurable batch interval, computes prefix-cached RenderPatches
// off the GTK main thread, and delivers them to onPatch.
type Streamer struct {
	interval time.Duration
	onPatch  func(RenderPatch)
	engine   *Engine

	mu         sync.Mutex
	pendingBuf bytes.Buffer

	notifyCh   chan struct{}
	finalizeCh chan string
	closeCh    chan struct{}
	doneCh     chan struct{}

	closeOnce  sync.Once
	inCallback atomic.Bool
}

// Batcher is an alias for Streamer.
type Batcher = Streamer

// NewStreamerWithEngine creates and starts a new background markdown Streamer with a pre-initialized Engine.
func NewStreamerWithEngine(interval time.Duration, engine *Engine, onPatch func(RenderPatch)) *Streamer {
	if interval <= 0 {
		interval = DefaultInterval
	}
	if engine == nil {
		engine = NewEngine()
	}
	s := &Streamer{
		interval:   interval,
		onPatch:    onPatch,
		engine:     engine,
		notifyCh:   make(chan struct{}, 1),
		finalizeCh: make(chan string, 1),
		closeCh:    make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
	go s.loop()
	return s
}

// NewStreamer creates and starts a new background markdown Streamer.
func NewStreamer(interval time.Duration, onPatch func(RenderPatch)) *Streamer {
	return NewStreamerWithEngine(interval, nil, onPatch)
}

// NewBatcher creates and starts a new background markdown Batcher.
func NewBatcher(interval time.Duration, onPatch func(RenderPatch)) *Batcher {
	return NewStreamer(interval, onPatch)
}

// Append enqueues a markdown token delta for batched parsing without blocking.
func (s *Streamer) Append(delta string) {
	if delta == "" {
		return
	}
	select {
	case <-s.doneCh:
		return
	case <-s.closeCh:
		return
	default:
	}

	s.mu.Lock()
	s.pendingBuf.WriteString(delta)
	s.mu.Unlock()

	select {
	case s.notifyCh <- struct{}{}:
	default:
	}
}

// Finalize flushes any pending tokens immediately, commits all remaining blocks,
// invokes onPatch with Final: true, and stops the worker goroutine.
func (s *Streamer) Finalize(fullText string) {
	select {
	case <-s.doneCh:
		return
	case <-s.closeCh:
		return
	case s.finalizeCh <- fullText:
	}
	if !s.inCallback.Load() {
		<-s.doneCh
	}
}

// Close stops the background worker goroutine without emitting further patches.
func (s *Streamer) Close() {
	s.closeOnce.Do(func() {
		close(s.closeCh)
	})
	if !s.inCallback.Load() {
		<-s.doneCh
	}
}

func (s *Streamer) drainPending() bool {
	s.mu.Lock()
	if s.pendingBuf.Len() == 0 {
		s.mu.Unlock()
		return false
	}
	chunk := s.pendingBuf.String()
	s.pendingBuf.Reset()
	s.mu.Unlock()

	s.engine.Append(chunk)
	return true
}

func (s *Streamer) emit(patch RenderPatch) {
	if s.onPatch == nil {
		return
	}
	s.inCallback.Store(true)
	s.onPatch(patch)
	s.inCallback.Store(false)
}

func (s *Streamer) loop() {
	defer close(s.doneCh)

	var (
		timer   *time.Timer
		timerC  <-chan time.Time
		pending bool
	)
	stopTimer := func() {
		if timer != nil {
			timer.Stop()
		}
		timerC = nil
		pending = false
	}

	for {
		select {
		case <-s.closeCh:
			stopTimer()
			return

		case fullText := <-s.finalizeCh:
			stopTimer()
			s.drainPending()
			patch := s.engine.Finalize(fullText)
			s.emit(patch)
			return

		case <-s.notifyCh:
			s.drainPending()
			select {
			case <-s.closeCh:
				stopTimer()
				return
			case fullText := <-s.finalizeCh:
				stopTimer()
				s.drainPending()
				patch := s.engine.Finalize(fullText)
				s.emit(patch)
				return
			default:
			}
			if !pending {
				pending = true
				if timer == nil {
					timer = time.NewTimer(s.interval)
				} else {
					timer.Reset(s.interval)
				}
				timerC = timer.C
			}

		case <-timerC:
			timerC = nil
			pending = false
			s.drainPending()
			select {
			case <-s.closeCh:
				return
			case fullText := <-s.finalizeCh:
				s.drainPending()
				patch := s.engine.Finalize(fullText)
				s.emit(patch)
				return
			default:
			}
			if patch, ok := s.engine.Flush(); ok {
				s.emit(patch)
			}
		}
	}
}
