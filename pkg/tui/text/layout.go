package text

import (
	"strings"
	"unicode"

	"github.com/rivo/uniseg"
)

type wrapKey struct {
	width, tab int
	wrap       bool
	mask       rune
	hidden     bool
}

func normalizedConfig(c Config) Config {
	c.Width = max(1, c.Width)
	if c.TabWidth <= 0 {
		c.TabWidth = 4
	}
	return c
}
func display(g cluster, cell int, c Config) (string, int) {
	if c.EchoNone {
		return "", 0
	}
	if c.Mask != 0 {
		v := string(c.Mask)
		return v, uniseg.StringWidth(v)
	}
	if g.text == "\t" {
		n := c.TabWidth - cell%c.TabWidth
		return strings.Repeat(" ", n), n
	}
	return g.text, g.width
}
func whitespace(g cluster) bool {
	for _, r := range g.text {
		return unicode.IsSpace(r)
	}
	return false
}
func (l *logicalLine) wrapped(c Config) []Row {
	key := wrapKey{c.Width, c.TabWidth, c.Wrap, c.Mask, c.EchoNone}
	if cached, ok := l.wraps.Load(key); ok {
		return cached.([]Row)
	}
	rows := make([]Row, 0, 1)
	for start := 0; start < len(l.clusters); {
		end, cell, lastBreak := start, 0, -1
		for end < len(l.clusters) {
			g := l.clusters[end]
			_, w := display(g, cell, c)
			if c.Wrap && cell+w > c.Width && end > start {
				break
			}
			cell += w
			end++
			if whitespace(g) {
				lastBreak = end
			}
			if c.Wrap && cell >= c.Width {
				break
			}
		}
		if c.Wrap && end < len(l.clusters) && lastBreak > start && lastBreak < end {
			end = lastBreak
		}
		row := Row{StartColumn: l.clusters[start].start, EndColumn: l.clusters[end-1].end}
		var b strings.Builder
		for _, g := range l.clusters[start:end] {
			v, w := display(g, row.Cells, c)
			row.Runs = append(row.Runs, Run{Text: v, StartColumn: g.start, EndColumn: g.end, Cell: row.Cells, Width: w})
			b.WriteString(v)
			row.Cells += w
		}
		row.Text = b.String()
		rows = append(rows, row)
		start = end
	}
	if len(rows) == 0 {
		rows = append(rows, Row{})
	}
	// Reserve a real insertion position when a logical line exactly fills the
	// wrapping area. It belongs to this logical line, not the following newline.
	if c.Wrap && rows[len(rows)-1].Cells >= c.Width {
		rows = append(rows, Row{StartColumn: l.runes, EndColumn: l.runes})
	}
	// Bound memoization on a long-lived document during repeated resizing.
	count := 0
	l.wraps.Range(func(_, _ any) bool { count++; return count < 8 })
	if count >= 8 {
		l.wraps.Clear()
	}
	actual, _ := l.wraps.LoadOrStore(key, rows)
	return actual.([]Row)
}

func rowCursor(rows []Row, p Position) Cell {
	index := 0
	for i, r := range rows {
		if r.Line > p.Line {
			break
		}
		if r.Line == p.Line && r.StartColumn <= p.Column {
			index = i
		}
	}
	r := rows[index]
	cell := 0
	for _, run := range r.Runs {
		if run.EndColumn <= p.Column {
			cell = run.Cell + run.Width
		} else {
			break
		}
	}
	return Cell{Row: index, Column: cell}
}
func (s State) rows(c Config) []Row {
	rows := make([]Row, 0, s.LineCount())
	for line := 0; line < s.LineCount(); line++ {
		for _, r := range s.line(line).wrapped(c) {
			r.Line = line
			rows = append(rows, r)
		}
	}
	return rows
}

// Layout returns a detached snapshot. Its slices may be modified by callers
// without affecting this State, its copies, or future layouts. It never scrolls.
func (s State) Layout(config Config) Layout {
	c := normalizedConfig(config)
	rows := s.rows(c)
	out := Layout{Rows: rows, Width: c.Width, Height: c.Height, ScrollX: s.scrollX, ScrollY: s.scrollY}
	if out.Height <= 0 {
		out.Height = len(rows)
	}
	out.Cursor = rowCursor(rows, s.cursor)
	a, b, selected := s.selection()
	for i := range out.Rows {
		r := &out.Rows[i]
		r.Runs = append([]Run(nil), r.Runs...)
		start, end := -1, -1
		for j := range r.Runs {
			run := &r.Runs[j]
			if selected && less(Position{r.Line, run.StartColumn}, b) && less(a, Position{r.Line, run.EndColumn}) {
				run.Selected = true
				if start < 0 {
					start = run.Cell
				}
				end = run.Cell + run.Width
			}
		}
		// A selected newline has an occupied cell after the last grapheme.
		lastSegment := i+1 == len(rows) || rows[i+1].Line != r.Line
		if selected && lastSegment && a.Line <= r.Line && b.Line > r.Line {
			if start < 0 {
				start = r.Cells
			}
			end = r.Cells + 1
		}
		if start >= 0 {
			out.Selection = append(out.Selection, CellRange{Row: i, Start: start, End: end})
		}
	}
	return out
}
func (s State) LineInfo(config Config) LineInfo {
	c := normalizedConfig(config)
	rows := s.line(s.cursor.Line).wrapped(c)
	index := 0
	for i, r := range rows {
		if r.StartColumn <= s.cursor.Column {
			index = i
		}
	}
	r := rows[index]
	offset := 0
	for _, run := range r.Runs {
		if run.EndColumn <= s.cursor.Column {
			offset = run.Cell + run.Width
		}
	}
	return LineInfo{Width: r.EndColumn - r.StartColumn, CharWidth: r.Cells, Height: len(rows), StartColumn: r.StartColumn, ColumnOffset: s.cursor.Column - r.StartColumn, CharOffset: offset, RowOffset: index}
}
func positionInRow(row Row, column int) Position {
	p := Position{row.Line, row.StartColumn}
	for _, run := range row.Runs {
		// The leading cell (and the left half of a wide grapheme) selects its start.
		// This rounds a click in the right half to the following insertion position.
		if column < run.Cell+(run.Width+1)/2 {
			return Position{row.Line, run.StartColumn}
		}
		p.Column = run.EndColumn
	}
	return p
}

// PositionAtCell maps viewport-local cells to grapheme insertion boundaries.
func (s State) PositionAtCell(config Config, row, column int) Position {
	c := normalizedConfig(config)
	rows := s.rows(c)
	row = max(0, min(row+s.scrollY, len(rows)-1))
	column = max(0, column+s.scrollX)
	return positionInRow(rows[row], column)
}

// NormalizeViewport explicitly keeps the cursor visible and clamps scrolling
// after edits or resizing. Neither Layout nor hit testing invokes this method.
func (s *State) NormalizeViewport(config Config) {
	c := normalizedConfig(config)
	rows := s.rows(c)
	cursor := rowCursor(rows, s.cursor)
	height := c.Height
	if height <= 0 {
		height = len(rows)
	}
	x, y := s.scrollX, s.scrollY
	y = max(0, min(y, max(0, len(rows)-height)))
	if cursor.Row < y {
		y = cursor.Row
	}
	if cursor.Row >= y+height {
		y = cursor.Row - height + 1
	}
	if c.Wrap {
		x = 0
	} else {
		x = min(x, max(0, rows[cursor.Row].Cells-c.Width+1))
		if cursor.Column < x {
			x = cursor.Column
		}
		if cursor.Column >= x+c.Width {
			x = cursor.Column - c.Width + 1
		}
		x = max(0, x)
		// Do not start the viewport in the middle of a wide grapheme.
		for _, run := range rows[cursor.Row].Runs {
			if x > run.Cell && x < run.Cell+run.Width {
				x = run.Cell + run.Width
				break
			}
		}
	}
	if x != s.scrollX || y != s.scrollY {
		s.scrollX = x
		s.scrollY = y
		s.revision++
	}
}

// ScrollBy scrolls vertically without moving the cursor or following it back.
func (s *State) ScrollBy(delta int, config Config) {
	c := normalizedConfig(config)
	n := len(s.rows(c))
	height := c.Height
	if height <= 0 {
		height = n
	}
	y := max(0, min(s.scrollY+delta, max(0, n-height)))
	if y != s.scrollY {
		s.scrollY = y
		s.revision++
	}
}

// RuneToCell returns the display column at or before a grapheme boundary.
// CellToRune returns the nearest rune boundary in display cells. They use the
// same tab, mask and hidden-text policy as Layout, without wrapping.
func RuneToCell(value string, column int, config Config) int {
	c := normalizedConfig(config)
	l := makeLine(value)
	cell := 0
	for _, g := range l.clusters {
		if g.end > column {
			break
		}
		_, w := display(g, cell, c)
		cell += w
	}
	return cell
}
func CellToRune(value string, cell int, config Config) int {
	c := normalizedConfig(config)
	c.Wrap = false
	l := makeLine(value)
	return positionInRow(l.wrapped(c)[0], max(0, cell)).Column
}
