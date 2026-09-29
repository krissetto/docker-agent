// Package text provides a terminal-independent, grapheme-aware editing model.
// State values may be copied: documents and their layout caches are immutable
// apart from synchronized memoization. Rendering never changes editor state.
package text

import (
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

// Position identifies a logical line and a rune column, not a byte or cell.
// Positions inside a grapheme are normalized to its preceding boundary.
type Position struct{ Line, Column int }
type Options struct {
	Multiline bool
	CharLimit int
}
type Config struct {
	// Width and Height describe the editable cell area, excluding decorations.
	// Width <= 0 means one cell; Height <= 0 means all visual rows.
	Width, Height int
	Wrap          bool
	TabWidth      int  // Defaults to four cells.
	Mask          rune // Nonzero replaces each grapheme, including tabs, with this rune.
	EchoNone      bool // Hides all text and its display width.
}
type Cell struct{ Row, Column int }
type CellRange struct{ Row, Start, End int }
type Run struct {
	Text                   string
	StartColumn, EndColumn int
	Cell, Width            int
	Selected               bool
}
type Row struct {
	Text                                string
	Runs                                []Run
	Line, StartColumn, EndColumn, Cells int
}

// Layout is a detached snapshot. Rows includes all visual rows; Cursor uses
// absolute visual coordinates. Subtract ScrollX/ScrollY for viewport coordinates.
type Layout struct {
	Rows                            []Row
	Cursor                          Cell
	Selection                       []CellRange
	Width, Height, ScrollX, ScrollY int
}
type LineInfo struct {
	Width, CharWidth, Height                         int
	StartColumn, ColumnOffset, CharOffset, RowOffset int
}

type cluster struct {
	text                                  string
	start, end, byteStart, byteEnd, width int
}
type logicalLine struct {
	text     string
	runes    int
	clusters []cluster
	wraps    sync.Map // map[wrapKey][]Row; published rows are never modified
}
type document struct {
	value string
	lines []*logicalLine
	runes int
}

// State owns editing and viewport state. Its zero value is a single-line editor.
// Copying State (or using Clone) produces independent editing state.
type State struct {
	doc                       *document
	options                   Options
	cursor, anchor            Position
	selecting                 bool
	revision, contentRevision uint64
	scrollX, scrollY          int
	preferredX                int
	hasPreferredX             bool
}

func New(options Options) State { return State{options: options, doc: makeDocument("", nil)} }
func (s State) Clone() State    { return s }
func (s State) Value() string {
	if s.doc == nil {
		return ""
	}
	return s.doc.value
}
func (s State) Cursor() Position        { return s.cursor }
func (s State) Revision() uint64        { return s.revision }
func (s State) ContentRevision() uint64 { return s.contentRevision }
func (s State) Line() int               { return s.cursor.Line }
func (s State) Column() int             { return s.cursor.Column }
func (s State) CharLimit() int          { return s.options.CharLimit }
func (s State) LineCount() int {
	if s.doc == nil {
		return 1
	}
	return len(s.doc.lines)
}
func (s State) ScrollYOffset() int { return s.scrollY }
func (s State) ScrollXOffset() int { return s.scrollX }
func (s State) line(n int) *logicalLine {
	if s.doc == nil {
		return emptyLine
	}
	return s.doc.lines[max(0, min(n, len(s.doc.lines)-1))]
}

var emptyLine = makeLine("")

func makeLine(value string) *logicalLine {
	l := &logicalLine{text: value, runes: utf8.RuneCountInString(value)}
	g := uniseg.NewGraphemes(value)
	col := 0
	for g.Next() {
		a, b := g.Positions()
		text := g.Str()
		end := col + utf8.RuneCountInString(text)
		l.clusters = append(l.clusters, cluster{text: text, start: col, end: end, byteStart: a, byteEnd: b, width: g.Width()})
		col = end
	}
	return l
}
func makeDocument(value string, old *document) *document {
	d := &document{value: value, runes: utf8.RuneCountInString(value)}
	// Reuse unchanged logical lines and their wraps across edits, including lines
	// shifted by newline insertion. The document index is rebuilt, but only new
	// logical lines require grapheme segmentation and wrapping.
	available := make(map[string]*logicalLine)
	if old != nil {
		for _, l := range old.lines {
			available[l.text] = l
		}
	}
	for _, text := range strings.Split(value, "\n") {
		l := available[text]
		if l == nil {
			l = makeLine(text)
			available[text] = l
		}
		d.lines = append(d.lines, l)
	}
	return d
}
func (s State) normalize(p Position) Position {
	p.Line = max(0, min(p.Line, s.LineCount()-1))
	l := s.line(p.Line)
	p.Column = max(0, min(p.Column, l.runes))
	for _, g := range l.clusters {
		if p.Column > g.start && p.Column < g.end {
			p.Column = g.start
			break
		}
	}
	return p
}
func less(a, b Position) bool { return a.Line < b.Line || (a.Line == b.Line && a.Column < b.Column) }
func (s State) selection() (Position, Position, bool) {
	if !s.selecting || s.anchor == s.cursor {
		return s.cursor, s.cursor, false
	}
	if less(s.anchor, s.cursor) {
		return s.anchor, s.cursor, true
	}
	return s.cursor, s.anchor, true
}
func (s State) byteOffset(p Position) int {
	p = s.normalize(p)
	n := 0
	for i := 0; i < p.Line; i++ {
		n += len(s.line(i).text) + 1
	}
	return n + RuneToByte(s.line(p.Line).text, p.Column)
}
func (s State) SelectedText() string {
	a, b, ok := s.selection()
	if !ok {
		return ""
	}
	return s.Value()[s.byteOffset(a):s.byteOffset(b)]
}
func (s *State) Select(anchor, head Position) {
	anchor = s.normalize(anchor)
	head = s.normalize(head)
	if s.anchor != anchor || s.cursor != head || !s.selecting {
		s.anchor = anchor
		s.cursor = head
		s.selecting = true
		s.hasPreferredX = false
		s.revision++
	}
}
func (s *State) SelectAll() {
	s.Select(Position{}, Position{s.LineCount() - 1, s.line(s.LineCount() - 1).runes})
}
func (s *State) ClearSelection() {
	if s.selecting {
		s.selecting = false
		s.anchor = s.cursor
		s.revision++
	}
}
func (s *State) SetCursor(p Position) { s.move(p, false, false) }
func (s *State) move(p Position, selecting, vertical bool) {
	p = s.normalize(p)
	before := s.cursor
	oldAnchor := s.anchor
	oldSelecting := s.selecting
	if selecting {
		if !s.selecting {
			s.anchor = s.cursor
		}
		s.selecting = true
	} else {
		s.selecting = false
		s.anchor = p
	}
	s.cursor = p
	if !vertical {
		s.hasPreferredX = false
	}
	if before != p || oldAnchor != s.anchor || oldSelecting != s.selecting {
		s.revision++
	}
}
func (s *State) SetCharLimit(limit int) {
	if s.options.CharLimit == limit {
		return
	}
	s.options.CharLimit = limit
	s.revision++
	// A limit governs subsequent insertion, not existing text.
}
func sanitize(value string, multiline bool) string {
	value = strings.ToValidUTF8(value, "�")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	var b strings.Builder
	for _, r := range value {
		switch {
		case r == '\n' || r == '\r':
			if multiline {
				b.WriteByte('\n')
			} else {
				b.WriteByte(' ')
			}
		case r == '\t':
			b.WriteRune(r)
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
func truncate(value string, limit int) string {
	if limit < 0 {
		return value
	}
	count, end := 0, 0
	g := uniseg.NewGraphemes(value)
	for g.Next() {
		n := utf8.RuneCountInString(g.Str())
		if count+n > limit {
			break
		}
		count += n
		_, end = g.Positions()
	}
	return value[:end]
}
func (s *State) SetValue(value string) {
	value = sanitize(value, s.options.Multiline)
	if s.options.CharLimit > 0 {
		value = truncate(value, s.options.CharLimit)
	}
	changed := value != s.Value()
	if changed {
		s.doc = makeDocument(value, s.doc)
		s.contentRevision++
		s.revision++
	}
	s.move(Position{s.LineCount() - 1, s.line(s.LineCount() - 1).runes}, false, false)
	if s.scrollX != 0 || s.scrollY != 0 {
		s.scrollX = 0
		s.scrollY = 0
		s.revision++
	}
}
func (s *State) replace(a, b Position, value string) {
	a = s.normalize(a)
	b = s.normalize(b)
	if less(b, a) {
		a, b = b, a
	}
	start, end := s.byteOffset(a), s.byteOffset(b)
	next := s.Value()[:start] + value + s.Value()[end:]
	if next != s.Value() {
		s.doc = makeDocument(next, s.doc)
		s.contentRevision++
		s.revision++
	}
	parts := strings.Split(value, "\n")
	p := Position{a.Line + len(parts) - 1, utf8.RuneCountInString(parts[len(parts)-1])}
	if len(parts) == 1 {
		p.Column += a.Column
	}
	// Insertion can join adjacent graphemes. Prefer the end of the newly joined
	// cluster instead of leaving the caret inside it.
	l := s.line(p.Line)
	for _, g := range l.clusters {
		if p.Column > g.start && p.Column < g.end {
			p.Column = g.end
			break
		}
	}
	s.move(p, false, false)
}
func (s *State) Insert(value string) {
	value = sanitize(value, s.options.Multiline)
	a, b, _ := s.selection()
	if s.options.CharLimit > 0 {
		remaining := s.options.CharLimit - utf8.RuneCountInString(s.Value()) + utf8.RuneCountInString(s.SelectedText())
		value = truncate(value, max(0, remaining))
	}
	if value == "" {
		return
	}
	s.replace(a, b, value)
}
func (s *State) Newline() {
	if s.options.Multiline {
		s.Insert("\n")
	}
}
func (s State) previous(p Position) Position {
	p = s.normalize(p)
	if p.Column == 0 {
		if p.Line > 0 {
			return Position{p.Line - 1, s.line(p.Line - 1).runes}
		}
		return p
	}
	for _, g := range s.line(p.Line).clusters {
		if g.end >= p.Column {
			return Position{p.Line, g.start}
		}
	}
	return p
}
func (s State) next(p Position) Position {
	p = s.normalize(p)
	for _, g := range s.line(p.Line).clusters {
		if g.end > p.Column {
			return Position{p.Line, g.end}
		}
	}
	if p.Line+1 < s.LineCount() {
		return Position{p.Line + 1, 0}
	}
	return p
}
func (s *State) Backspace() {
	a, b, selected := s.selection()
	if !selected {
		a = s.previous(s.cursor)
	}
	s.replace(a, b, "")
}
func (s *State) Delete() {
	a, b, selected := s.selection()
	if !selected {
		b = s.next(s.cursor)
	}
	s.replace(a, b, "")
}

// RuneToByte and ByteToRune clamp out-of-range indices and never split UTF-8.
// Editing additionally snaps these rune positions to grapheme boundaries.
func RuneToByte(value string, column int) int {
	if column <= 0 {
		return 0
	}
	n := 0
	for i := range value {
		if n == column {
			return i
		}
		n++
	}
	return len(value)
}
func ByteToRune(value string, offset int) int {
	offset = max(0, min(offset, len(value)))
	n := 0
	for i, r := range value {
		if i+utf8.RuneLen(r) > offset {
			break
		}
		n++
	}
	return n
}
