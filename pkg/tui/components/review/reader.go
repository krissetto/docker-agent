package review

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"

	textlayout "github.com/docker/docker-agent/pkg/tui/text"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	tabWidth = 4
	// splitMinWidth is the narrowest reader that renders side by side; below it
	// the split preference is kept but unified rows are shown.
	splitMinWidth = 100
)

// item is one cursor stop in the reader. Unified items show one line; split
// items pair an old (left) and new (right) line. Equal sides span the width.
type item struct{ left, right int }

type textPoint struct{ line, column int }
type readerCharSelection struct{ start, end textPoint }
type commentNote struct {
	ID, Preview, Body string
	Expanded          bool
}

func (it item) full() bool { return it.left == it.right }

type readerState struct {
	doc       *Document
	text      []string // sanitized display text, aligned with doc.Lines
	hl        highlighter
	highlight highlightJob // pending tokenization for the current document

	split    bool // effective layout of items
	items    []item
	lineItem []int // doc line → item

	line   int // cursor item
	top    int // first visual row
	anchor int // selection anchor item, -1 none

	matches []int // items

	digits         int
	hunks          []int // items that start a hunk
	added, removed int

	comments      map[int][]commentNote // item → expandable comment previews shown under it
	charSelection *readerCharSelection
	inlineAfter   int
	inlineLines   []string
	noCursor      bool // read-only text such as the send preview

	keepPosition bool

	cacheWidth int
	cacheSplit bool
	cacheValid bool
	starts     []int // per item, first visual row; len(items)+1
	total      int
}

func (r *readerState) setDocument(doc *Document, same bool) {
	r.doc = doc
	r.text = make([]string, len(doc.Lines))
	r.added, r.removed = 0, 0
	maxNum := 0
	for i, l := range doc.Lines {
		r.text[i] = sanitize(l.Text)
		maxNum = max(maxNum, l.Old, l.New)
		switch l.Kind {
		case LineAdd:
			r.added++
		case LineRemove:
			r.removed++
		}
	}
	r.digits = max(3, len(strconv.Itoa(maxNum)))
	r.highlight = r.hl.prepare(doc, r.text)
	keep := r.keepPosition || same
	r.keepPosition = false
	r.buildItems(r.split)
	if !keep {
		r.charSelection = nil
	}
	if keep {
		r.line = min(r.line, max(0, len(r.items)-1))
		if r.anchor >= len(r.items) {
			r.anchor = -1
		}
		return
	}
	r.line, r.top, r.anchor = 0, 0, -1
	for i, it := range r.items {
		if k := doc.Lines[r.primary(it)].Kind; k == LineAdd || k == LineRemove || k == LineContext {
			r.line = i
			break
		}
	}
}

// primary is the doc line that represents an item (new side first).
func (r *readerState) primary(it item) int {
	if it.right >= 0 {
		return it.right
	}
	return it.left
}

// buildItems maps doc lines to cursor items for the requested layout.
func (r *readerState) buildItems(split bool) {
	r.split = split
	r.items = r.items[:0]
	r.lineItem = make([]int, len(r.text))
	r.hunks = r.hunks[:0]
	if r.doc == nil {
		return
	}
	lines := r.doc.Lines
	add := func(it item) {
		for _, i := range []int{it.left, it.right} {
			if i >= 0 {
				r.lineItem[i] = len(r.items)
			}
		}
		if lines[r.primary(it)].Kind == LineHunk {
			r.hunks = append(r.hunks, len(r.items))
		}
		r.items = append(r.items, it)
	}
	for i := 0; i < len(lines); {
		if !split || (lines[i].Kind != LineRemove && lines[i].Kind != LineAdd) {
			add(item{i, i})
			i++
			continue
		}
		// Pair a run of removals with the following run of additions.
		var removes, adds []int
		for i < len(lines) && lines[i].Kind == LineRemove {
			removes = append(removes, i)
			i++
		}
		for i < len(lines) && lines[i].Kind == LineAdd {
			adds = append(adds, i)
			i++
		}
		for k := range max(len(removes), len(adds)) {
			it := item{-1, -1}
			if k < len(removes) {
				it.left = removes[k]
			}
			if k < len(adds) {
				it.right = adds[k]
			}
			add(it)
		}
	}
	r.invalidate()
}

// setSplit switches layout while keeping the cursor on the same doc line.
func (r *readerState) setSplit(split bool) {
	if r.doc == nil {
		r.split = split
		return
	}
	if split == r.split {
		return
	}
	cursor := r.primary(r.itemAt(r.line))
	anchor := -1
	if r.anchor >= 0 {
		anchor = r.primary(r.itemAt(r.anchor))
	}
	r.buildItems(split)
	if cursor >= 0 && cursor < len(r.lineItem) {
		r.line = r.lineItem[cursor]
	} else {
		r.line = min(max(0, r.line), max(0, len(r.items)-1))
	}
	if anchor >= 0 && anchor < len(r.lineItem) {
		r.anchor = r.lineItem[anchor]
	} else {
		r.anchor = -1
	}
}

func (r *readerState) itemAt(i int) item {
	if i < 0 || i >= len(r.items) {
		return item{-1, -1}
	}
	return r.items[i]
}

func (r *readerState) invalidate() { r.cacheValid = false }

func expandTabs(s string) string {
	var b strings.Builder
	col := 0
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		cluster := g.Str()
		if cluster == "\t" {
			n := tabWidth - col%tabWidth
			b.WriteString(strings.Repeat(" ", n))
			col += n
			continue
		}
		b.WriteString(cluster)
		col += g.Width()
	}
	return b.String()
}

func sanitize(s string) string {
	s = expandTabs(ansi.Strip(s))
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || isBidiControl(r) {
			return ' '
		}
		return r
	}, s)
}

// isBidiControl rejects direction overrides that could visually disguise paths.
func isBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) || r == 0x200E || r == 0x200F || r == 0x061C
}

type gutter struct {
	digits  int
	numbers int // 0, 1 or 2 line-number columns
	marker  bool
}

func (g gutter) width() int {
	w := 1 + g.numbers*(g.digits+1)
	if g.marker {
		w += 2
	}
	return w
}

func (r *readerState) gutter(width int) gutter {
	if r.doc == nil {
		return gutter{}
	}
	g := gutter{digits: r.digits, numbers: 2, marker: r.doc.Key.Mode == ModeChanges || len(r.doc.Indicators) > 0}
	if !g.marker {
		g.numbers = 1
	}
	if g.numbers == 2 && width < 60 {
		g.numbers = 1
	}
	if width-g.width() < 16 {
		g.numbers = 0
	}
	return g
}

func (r *readerState) contentWidth(width int) int { return max(1, width-r.gutter(width).width()) }

// splitGeometry is the per-side layout of a split row: cursor mark, then
// number+sign+text on each side around a one-cell divider.
func (r *readerState) splitGeometry(width int) (side, text int) {
	side = max(1, (width-2)/2)
	return side, max(1, side-(r.digits+1)-2)
}

func rowsFor(text string, width int) int {
	return max(1, (ansi.StringWidth(text)+width-1)/width)
}

func (r *readerState) textRows(i, width int) int {
	it := r.items[i]
	if !r.split {
		return rowsFor(r.text[it.left], r.contentWidth(width))
	}
	_, tw := r.splitGeometry(width)
	n := 1
	for _, side := range []int{it.left, it.right} {
		if side >= 0 {
			n = max(n, rowsFor(r.text[side], tw))
		}
	}
	return n
}

// layout computes visual rows per item, including comment rows; it is cached
// by width, layout and comment revision.
func (r *readerState) layout(width int) {
	if r.doc == nil || (r.cacheValid && r.cacheWidth == width && r.cacheSplit == r.split) {
		return
	}
	r.starts = make([]int, len(r.items)+1)
	row := 0
	for i := range r.items {
		r.starts[i] = row
		row += r.textRows(i, width) + r.commentRows(i)
		if i == r.inlineAfter {
			row += len(r.inlineLines)
		}
	}
	r.starts[len(r.items)] = row
	r.total = row
	r.cacheWidth, r.cacheSplit, r.cacheValid = width, r.split, true
}

func (r *readerState) itemAtRow(row int) int {
	i := sort.Search(len(r.items), func(i int) bool { return r.starts[i+1] > row })
	return min(i, max(0, len(r.items)-1))
}

func (r *readerState) commentRows(i int) int {
	n := 0
	for _, c := range r.comments[i] {
		n++
		if c.Expanded {
			n += max(1, len(strings.Split(c.Body, "\n")))
		}
	}
	return n
}

func (r *readerState) ensureVisible(width, height int) {
	if r.doc == nil || len(r.items) == 0 || height <= 0 {
		return
	}
	r.layout(width)
	r.line = max(0, min(r.line, len(r.items)-1))
	start, end := r.starts[r.line], r.starts[r.line+1]
	if end > r.top+height {
		r.top = end - height
	}
	if start < r.top {
		r.top = start
	}
	r.clampTop(height)
}

func (r *readerState) clampTop(height int) {
	r.top = max(0, min(r.top, max(0, r.total-height)))
}

// scroll moves the viewport. At either end it also moves the cursor to the
// first or last item, so the final hunk is always reachable and counted.
func (r *readerState) scroll(width, height, delta int) {
	if r.doc == nil || len(r.items) == 0 {
		return
	}
	r.layout(width)
	before := r.top
	r.top += delta
	r.clampTop(height)
	switch {
	case delta > 0 && r.top == max(0, r.total-height) && (r.top == before || r.top < before+delta):
		r.line = len(r.items) - 1
	case delta < 0 && r.top == 0 && (r.top == before || r.top > before+delta):
		r.line = 0
	case r.starts[r.line+1] <= r.top:
		r.line = r.itemAtRow(r.top)
	case r.starts[r.line] >= r.top+height:
		r.line = r.itemAtRow(r.top + height - 1)
	}
}

// hunkPosition returns the 1-based hunk containing the cursor and the hunk count.
func (r *readerState) hunkPosition() (int, int) {
	return sort.SearchInts(r.hunks, r.line+1), len(r.hunks)
}

func (r *readerState) jumpHunk(dir int) bool {
	if dir > 0 {
		for _, s := range r.hunks {
			if s > r.line {
				r.line = s
				return true
			}
		}
		return false
	}
	cur := -1
	for i, s := range r.hunks {
		if s <= r.line {
			cur = i
		}
	}
	// From inside a hunk, go to its header first; from a header, go to the previous one.
	if cur >= 0 && r.hunks[cur] < r.line {
		r.line = r.hunks[cur]
		return true
	}
	if cur > 0 {
		r.line = r.hunks[cur-1]
		return true
	}
	return false
}

func (r *readerState) recomputeMatches(f matcher) {
	r.matches = r.matches[:0]
	if !f.active() || r.doc == nil {
		return
	}
	for i, it := range r.items {
		for _, side := range []int{it.left, it.right} {
			if side >= 0 && r.doc.Lines[side].Kind != LineNote && f.match(r.text[side]) {
				r.matches = append(r.matches, i)
				break
			}
		}
	}
}

func (r *readerState) jumpMatch(dir int, inclusive bool) bool {
	if len(r.matches) == 0 {
		return false
	}
	if dir > 0 {
		for _, i := range r.matches {
			if i > r.line || (inclusive && i == r.line) {
				r.line = i
				return true
			}
		}
		r.line = r.matches[0]
		return true
	}
	for _, i := range slices.Backward(r.matches) {
		if i < r.line {
			r.line = i
			return true
		}
	}
	r.line = r.matches[len(r.matches)-1]
	return true
}

func (r *readerState) matchPosition() (int, int) {
	return sort.SearchInts(r.matches, r.line+1), len(r.matches)
}

func (r *readerState) selection() (int, int) {
	if r.anchor < 0 {
		return r.line, r.line
	}
	return min(r.anchor, r.line), max(r.anchor, r.line)
}

// selectedLines returns the doc lines under the selected items, in order.
func (r *readerState) selectedLines() []int {
	from, to := r.selection()
	var out []int
	for i := from; i <= to && i < len(r.items); i++ {
		it := r.items[i]
		sides := []int{it.left, it.right}
		if r.split {
			sides = []int{it.right}
		}
		for _, side := range sides {
			if side >= 0 && !slices.Contains(out, side) {
				out = append(out, side)
			}
		}
	}
	slices.Sort(out)
	return out
}

// copyText returns the selected lines exactly as shown, minus UI gutters, keeping diff markers.
func (r *readerState) copyText() (string, int) {
	if r.doc == nil || len(r.items) == 0 {
		return "", 0
	}
	var b strings.Builder
	n := 0
	for _, i := range r.selectedLines() {
		l := r.doc.Lines[i]
		if l.Kind == LineNote {
			continue
		}
		text := l.Text
		if r.charSelection != nil {
			lo, hi := selectionColumnsForLine(*r.charSelection, i, runeCount(text))
			text = runeSlice(text, lo, hi)
		}
		if r.doc.Key.Mode == ModeChanges {
			switch l.Kind {
			case LineAdd:
				text = "+" + text
			case LineRemove:
				text = "-" + text
			default:
				text = " " + text
			}
		}
		b.WriteString(text)
		b.WriteByte('\n')
		n++
	}
	return b.String(), n
}

func diffLine(l Line, mode Mode) string {
	switch {
	case l.Kind == LineHunk || mode == ModeFiles:
		return l.Text
	case l.Kind == LineAdd:
		return "+" + l.Text
	case l.Kind == LineRemove:
		return "-" + l.Text
	}
	return " " + l.Text
}

// anchorFor describes the selected lines for a review comment. Ranges come
// from the doc's own line numbers, so display rows never shift anchors.
func (r *readerState) anchorFor() (Anchor, bool) {
	if r.doc == nil || len(r.items) == 0 {
		return Anchor{}, false
	}
	a := Anchor{Path: r.doc.Key.Path, Mode: r.doc.Key.Mode, Group: r.doc.Key.Group, WorktreeID: r.doc.Key.WorktreeID}
	selected := r.selectedLines()
	if len(selected) == 0 {
		return Anchor{}, false
	}
	if r.charSelection != nil {
		start, end := r.charSelection.start, r.charSelection.end
		if pointLess(end, start) {
			start, end = end, start
		}
		a.CharacterRange = true
		a.StartColumn, a.EndColumn = start.column, end.column
	}
	for _, i := range selected {
		l := r.doc.Lines[i]
		if l.Kind == LineHunk || l.Kind == LineNote {
			continue
		}
		text := l.Text
		if r.charSelection != nil {
			lo, hi := selectionColumnsForLine(*r.charSelection, i, runeCount(text))
			text = runeSlice(text, lo, hi)
		}
		if l.Old > 0 {
			a.OldStart, a.OldEnd = minNonZero(a.OldStart, l.Old), max(a.OldEnd, l.Old)
		}
		if l.New > 0 {
			a.NewStart, a.NewEnd = minNonZero(a.NewStart, l.New), max(a.NewEnd, l.New)
		}
		if r.doc.Key.Mode == ModeChanges {
			switch l.Kind {
			case LineAdd:
				text = "+" + text
			case LineRemove:
				text = "-" + text
			default:
				text = " " + text
			}
		}
		a.Context = append(a.Context, text)
	}
	if len(a.Context) == 0 {
		return Anchor{}, false
	}
	a.Version = anchorVersion(a)
	return a, true
}

func runeCount(s string) int { return len([]rune(s)) }
func runeSlice(s string, start, end int) string {
	r := []rune(s)
	start = max(0, min(start, len(r)))
	end = max(start, min(end, len(r)))
	return string(r[start:end])
}
func pointLess(a, b textPoint) bool {
	return a.line < b.line || a.line == b.line && a.column < b.column
}
func selectionColumnsForLine(s readerCharSelection, line, count int) (int, int) {
	a, b := s.start, s.end
	if pointLess(b, a) {
		a, b = b, a
	}
	if line < a.line || line > b.line {
		return 0, 0
	}
	start, end := 0, count
	if line == a.line {
		start = a.column
	}
	if line == b.line {
		end = b.column
	}
	return max(0, min(start, count)), max(0, min(end, count))
}

func minNonZero(a, b int) int {
	if a == 0 {
		return b
	}
	return min(a, b)
}

// anchorVersion fingerprints the anchor's identity and exact context.
func anchorVersion(a Anchor) string {
	h := sha256.New()
	parts := []string{
		a.WorktreeID, a.Path, strconv.Itoa(int(a.Mode)), strconv.Itoa(int(a.Group)),
		strconv.Itoa(a.OldStart), strconv.Itoa(a.OldEnd), strconv.Itoa(a.NewStart), strconv.Itoa(a.NewEnd), strconv.Itoa(a.StartColumn), strconv.Itoa(a.EndColumn), strconv.FormatBool(a.CharacterRange),
	}
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	for _, line := range a.Context {
		h.Write([]byte(line))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// commentItem finds the item a comment is shown under, by its last line.
func (r *readerState) matchesAnchor(a Anchor) bool {
	if r.doc == nil {
		return false
	}
	var lines []Line
	for _, line := range r.doc.Lines {
		if line.Kind == LineHunk || line.Kind == LineNote {
			continue
		}
		old := a.OldStart > 0 && line.Old >= a.OldStart && line.Old <= a.OldEnd
		newer := a.NewStart > 0 && line.New >= a.NewStart && line.New <= a.NewEnd
		if old || newer {
			lines = append(lines, line)
		}
	}
	if len(lines) != len(a.Context) {
		return false
	}
	for i, line := range lines {
		text := line.Text
		if a.CharacterRange {
			lo, hi := 0, runeCount(text)
			if i == 0 {
				lo = a.StartColumn
			}
			if i == len(lines)-1 {
				hi = a.EndColumn
			}
			text = runeSlice(text, lo, hi)
		}
		if r.doc.Key.Mode == ModeChanges {
			text = string(diffLine(line, r.doc.Key.Mode)[0]) + text
		}
		if text != a.Context[i] {
			return false
		}
	}
	return true
}

func (r *readerState) commentItem(a Anchor) (int, bool) {
	if r.doc == nil {
		return 0, false
	}
	for i, l := range r.doc.Lines {
		if (a.NewEnd > 0 && l.New == a.NewEnd) || (a.NewEnd == 0 && a.OldEnd > 0 && l.Old == a.OldEnd && l.New == 0) {
			return r.lineItem[i], true
		}
	}
	return 0, false
}

// patchDisabled explains why whole-hunk patch copies are unavailable.
func (r *readerState) patchDisabled() string {
	switch {
	case r.doc == nil:
		return "No document open"
	case r.doc.Key.Mode != ModeChanges:
		return "No patch in Files"
	case r.doc.Binary:
		return "No patch: binary"
	case r.doc.Truncated:
		return "No patch: truncated"
	case r.doc.Key.Group == GroupConflicts:
		return "No patch: conflict"
	case r.doc.PatchHeader == "" || len(r.hunks) == 0:
		return "No patch available"
	}
	return ""
}

// copyPatch returns every complete hunk touching the selection, or all hunks when whole is set.
func (r *readerState) copyPatch(whole bool) (string, int) {
	var hunkLines []int
	for i, l := range r.doc.Lines {
		if l.Kind == LineHunk {
			hunkLines = append(hunkLines, i)
		}
	}
	from, to := 0, len(r.doc.Lines)-1
	if !whole {
		from, to = len(r.doc.Lines), -1
		start, end := r.selection()
		for itemIndex := start; itemIndex <= end && itemIndex < len(r.items); itemIndex++ {
			it := r.items[itemIndex]
			for _, line := range []int{it.left, it.right} {
				if line >= 0 {
					from = min(from, line)
					to = max(to, line)
				}
			}
		}
		if to < from {
			return "", 0
		}
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(r.doc.PatchHeader, "\n"))
	b.WriteByte('\n')
	n := 0
	for i, s := range hunkLines {
		end := len(r.doc.Lines) - 1
		if i+1 < len(hunkLines) {
			end = hunkLines[i+1] - 1
		}
		if !whole && (end < from || s > to) {
			continue
		}
		for j := s; j <= end; j++ {
			if l := r.doc.Lines[j]; l.Kind != LineNote {
				b.WriteString(diffLine(l, ModeChanges))
				b.WriteByte('\n')
			}
		}
		n++
	}
	return b.String(), n
}

// render draws exactly height rows of width cells.
func (r *readerState) render(width, height int, focused bool, query matcher) []string {
	out := make([]string, 0, height)
	if r.doc == nil || len(r.items) == 0 {
		return out
	}
	r.layout(width)
	from, to := r.selection()
	selecting := r.anchor >= 0
	for row := r.top; row < r.top+height && row < r.total; row++ {
		i := r.itemAtRow(row)
		seg := row - r.starts[i]
		textRows := r.textRows(i, width)
		st := rowState{cursor: i == r.line && !r.noCursor, selected: selecting && i >= from && i <= to, focused: focused, commented: len(r.comments[i]) > 0}
		if seg >= textRows {
			commentOffset := seg - textRows
			painted := false
			for _, note := range r.comments[i] {
				rows := 1
				if note.Expanded {
					rows += max(1, len(strings.Split(note.Body, "\n")))
				}
				if commentOffset < rows {
					out = append(out, r.commentNoteRow(note, commentOffset, width))
					painted = true
					break
				}
				commentOffset -= rows
			}
			if painted {
				continue
			}
			if i == r.inlineAfter && commentOffset < len(r.inlineLines) {
				out = append(out, r.inlineLines[commentOffset])
				continue
			}
		}
		it := r.items[i]
		switch {
		case r.split:
			out = append(out, r.splitRow(it, seg, width, st, query))
		default:
			out = append(out, r.unifiedRow(it.left, seg, width, st, query))
		}
	}
	return out
}

type rowState struct{ cursor, selected, focused, commented bool }

func (s rowState) mark() string {
	switch {
	case s.cursor && s.focused:
		return styles.InfoStyle.Render("›")
	case s.selected:
		return styles.InfoStyle.Render("▐")
	case s.cursor:
		return styles.MutedStyle.Render("›")
	case s.commented:
		return styles.InfoStyle.Render("•")
	}
	return " "
}

func (s rowState) numberStyle() lipgloss.Style {
	if s.selected {
		return styles.BaseStyle.Background(styles.Selected).Foreground(styles.SelectedFg)
	}
	return styles.LineNumberStyle
}

func (r *readerState) commentNoteRow(note commentNote, offset, width int) string {
	if offset == 0 {
		affordance := "▸"
		if note.Expanded {
			affordance = "⌄"
		}
		return fit(styles.InfoStyle.Render(" ┊ ")+ansi.Truncate(note.Preview, max(1, width-8), "…")+styles.MutedStyle.Render("  "+affordance), width)
	}
	lines := strings.Split(note.Body, "\n")
	body := lines[min(offset-1, len(lines)-1)]
	return fit(styles.InfoStyle.Render("   ")+styles.BaseStyle.Background(styles.CardBg).Foreground(styles.TextPrimary).Render(ansi.Truncate("   "+sanitize(body), max(1, width-3), "…")), width)
}

func (r *readerState) indicator(line int) ChangeKind {
	if r.doc == nil {
		return ChangeNone
	}
	for _, i := range r.doc.Indicators {
		if i.Line == line {
			return i.Kind
		}
	}
	return ChangeNone
}
func indicatorStyle(k ChangeKind) lipgloss.Style {
	switch k {
	case ChangeAdded:
		return styles.SuccessStyle
	case ChangeModified:
		return styles.WarningStyle
	case ChangeDeleted:
		return styles.ErrorStyle
	}
	return styles.MutedStyle
}
func indicatorGlyph(k ChangeKind) string {
	switch k {
	case ChangeAdded:
		return "+"
	case ChangeModified:
		return "~"
	case ChangeDeleted:
		return "−"
	}
	return " "
}

func (r *readerState) number(n int, show bool) string {
	s := ""
	if show && n > 0 {
		s = strconv.Itoa(n)
	}
	return strings.Repeat(" ", r.digits-len(s)) + s + " "
}

func (r *readerState) unifiedRow(i, seg, width int, st rowState, query matcher) string {
	l := r.doc.Lines[i]
	g := r.gutter(width)
	cw := r.contentWidth(width)
	first := seg == 0
	cells := st.mark()
	switch g.numbers {
	case 2:
		cells += st.numberStyle().Render(r.number(l.Old, first) + r.number(l.New, first))
	case 1:
		n := l.New
		if n == 0 {
			n = l.Old
		}
		cells += st.numberStyle().Render(r.number(n, first))
	}
	if g.marker {
		marker := sign(l.Kind, first)
		style := lineStyle(l.Kind)
		if kind := r.indicator(l.New); kind != ChangeNone {
			marker, style = indicatorGlyph(kind), indicatorStyle(kind)
		}
		cells += style.Render(marker + " ")
	}
	return cells + r.content(i, seg, cw, query, st.cursor && st.focused)
}

func (r *readerState) splitRow(it item, seg, width int, st rowState, query matcher) string {
	side, tw := r.splitGeometry(width)
	render := func(i int, oldSide bool) string {
		if i < 0 {
			return styles.LineNumberStyle.Render(strings.Repeat(" ", side))
		}
		l := r.doc.Lines[i]
		n := l.New
		if oldSide {
			n = l.Old
		}
		first := seg == 0
		if seg >= rowsFor(r.text[i], tw) {
			return st.numberStyle().Render(r.number(0, false)) + lineStyle(l.Kind).Render(strings.Repeat(" ", side-r.digits-1))
		}
		return st.numberStyle().Render(r.number(n, first)) + lineStyle(l.Kind).Render(sign(l.Kind, first)+" ") + r.contentSelected(i, seg, tw, query, st.cursor && st.focused, !oldSide)
	}
	divider := styles.MutedStyle.Render("│")
	row := st.mark() + render(it.left, true) + divider + render(it.right, false)
	return row + strings.Repeat(" ", max(0, width-1-2*side-1))
}

func sign(kind LineKind, first bool) string {
	if first {
		switch kind {
		case LineAdd:
			return "+"
		case LineRemove:
			return "-"
		}
	}
	return " "
}

func lineStyle(kind LineKind) lipgloss.Style {
	switch kind {
	case LineAdd:
		return styles.DiffAddStyle
	case LineRemove:
		return styles.DiffRemoveStyle
	case LineHunk:
		return styles.LineNumberStyle
	case LineNote:
		return styles.MutedStyle.Italic(true)
	}
	return styles.BaseStyle
}

// content renders one wrapped segment of a line: syntax tokens over the diff
// background, with search matches taking precedence.
func (r *readerState) content(i, seg, width int, query matcher, current bool) string {
	return r.contentSelected(i, seg, width, query, current, true)
}

func (r *readerState) contentSelected(i, seg, width int, query matcher, current, selection bool) string {
	l := r.doc.Lines[i]
	base := lineStyle(l.Kind)
	text := ansi.Cut(r.text[i], seg*width, (seg+1)*width)
	pad := strings.Repeat(" ", max(0, width-ansi.StringWidth(text)))
	if selection && r.charSelection != nil {
		line := r.doc.Lines[i].Text
		start, end := selectionColumnsForLine(*r.charSelection, i, runeCount(line))
		if end > start {
			cellStart := textlayout.RuneToCell(line, start, textlayout.Config{TabWidth: tabWidth})
			cellEnd := textlayout.RuneToCell(line, end, textlayout.Config{TabWidth: tabWidth})
			lo, hi := max(0, cellStart-seg*width), min(width, cellEnd-seg*width)
			if hi > lo {
				before := ansi.Cut(r.text[i], seg*width, seg*width+lo)
				middle := ansi.Cut(r.text[i], seg*width+lo, seg*width+hi)
				after := ansi.Cut(r.text[i], seg*width+hi, (seg+1)*width)
				style := styles.BaseStyle.Background(styles.Selected).Foreground(styles.SelectedFg)
				return base.Render(before) + style.Render(middle) + base.Render(after+pad)
			}
		}
	}
	if spans := query.index(text); len(spans) > 0 {
		match := base.Reverse(true)
		if current {
			match = match.Bold(true)
		}
		var b strings.Builder
		at := 0
		for _, s := range spans {
			b.WriteString(base.Render(text[at:s[0]]))
			b.WriteString(match.Render(text[s[0]:s[1]]))
			at = s[1]
		}
		b.WriteString(base.Render(text[at:] + pad))
		return b.String()
	}
	if l.Kind == LineHunk || l.Kind == LineNote {
		return base.Render(text + pad)
	}
	return r.hl.render(i, r.text[i], seg*width, (seg+1)*width, base) + base.Render(pad)
}
