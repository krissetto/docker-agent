package tui

import (
	"cmp"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// composeRootLayers composites the shell's flat, positioned layers using the
// same ANSI row spans as panes. The inherited canvas serializer drops combining
// marks; retaining grapheme strings also avoids allocating a cell buffer for
// every overlay. Shell layers have no children, and input keeps their original
// coordinates. Equal Z layers retain their existing paint order.
func composeRootLayers(layers []*lipgloss.Layer, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	ordered := slices.Clone(layers)
	slices.SortStableFunc(ordered, func(a, b *lipgloss.Layer) int {
		return cmp.Compare(a.GetZ(), b.GetZ())
	})
	rows := make([][]paneRowSpan, height)
	for _, layer := range ordered {
		content := layer.GetContent()
		if content == "" {
			continue
		}
		x := layer.GetX()
		layerWidth := lipgloss.Width(content)
		left, right := max(0, x), min(width, x+layerWidth)
		if left >= right {
			continue
		}
		for offset, line := range strings.Split(content, "\n") {
			y := layer.GetY() + offset
			if y < 0 || y >= height {
				continue
			}
			var spans []paneRowSpan
			for _, previous := range rows[y] {
				end := previous.x + previous.width
				if end <= left || previous.x >= right {
					spans = append(spans, previous)
					continue
				}
				if previous.x < left {
					spans = append(spans, paneRowSpan{x: previous.x, width: left - previous.x, content: rootCellSlice(previous.content, 0, left-previous.x)})
				}
				if end > right {
					spans = append(spans, paneRowSpan{x: right, width: end - right, content: rootCellSlice(previous.content, right-previous.x, previous.width)})
				}
			}
			spans = append(spans, paneRowSpan{x: left, width: right - left, content: rootCellSlice(line, left-x, right-x)})
			rows[y] = spans
		}
	}
	return renderPaneRows(rows, width)
}

// rootCellSlice retains complete graphemes and their ANSI context, replacing a
// partially covered wide glyph with exactly its surviving number of cells.
// ansi.Cut keeps a glyph straddling the left edge whole, shifting later cells.
func rootCellSlice(line string, start, end int) string {
	if start >= end {
		return ""
	}
	parser := ansi.GetParser()
	defer ansi.PutParser(parser)
	var out strings.Builder
	var state byte
	column := 0
	ownedBase := false
	for line != "" {
		sequence, width, consumed, next := ansi.DecodeSequence(line, state, parser)
		if consumed == 0 {
			break
		}
		state, line = next, line[consumed:]
		if width == 0 {
			// ANSI changes do not break a grapheme: a cursor's inverse/reset
			// may separate its base from an acute mark. Keep marks only when
			// their preceding base belongs to this slice, never on edge padding.
			if ansi.Strip(sequence) == "" || ownedBase {
				out.WriteString(sequence)
			}
			continue
		}
		right := column + width
		ownedBase = column >= start && right <= end
		if ownedBase {
			out.WriteString(sequence)
		} else if column < end && right > start {
			out.WriteString(strings.Repeat(" ", min(right, end)-max(column, start)))
		}
		column = right
	}
	return out.String()
}
