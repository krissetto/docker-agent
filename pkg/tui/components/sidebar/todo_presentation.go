package sidebar

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func (m *model) todoSummary(width int) string {
	completed, total := m.todoComp.Counts()
	glyph := "⌄"
	if m.todosCollapsed {
		glyph = "›"
	}
	if width <= 1 {
		return styles.MutedStyle.Render(glyph)
	}
	text := styles.TabPrimaryStyle.Render(fmt.Sprintf("%d/%d todos", completed, total))

	text = ansi.Truncate(text, width-2, "…")
	return m.hoverText(text+strings.Repeat(" ", max(1, width-ansi.StringWidth(text)-1))+styles.MutedStyle.Render(glyph), "todo-summary")
}

func (m *model) todoHoverKey(row placedRow, col int) string {
	key := "todo:" + row.payload + ":text"
	indent := min(2, max(0, m.contentWidth(m.cachedNeedsScrollbar)-1))
	if row.todoControls {
		switch col - indent {
		case 0:
			key = "todo:" + row.payload + ":status"
		case 2:
			key = "todo:" + row.payload + ":remove"
		case 4:
			key = "todo:" + row.payload + ":edit"
		}
	}
	return key
}

func (m *model) todoHoverText(row placedRow) string {
	text := row.text
	if row.todoControls && m.todoRemoveArmed == row.payload {
		indent := min(2, max(0, m.contentWidth(m.cachedNeedsScrollbar)-1))
		col := indent + 2
		text = ansi.Cut(text, 0, col) + styles.ErrorStyle.Render("×") + ansi.Cut(text, col+1, m.contentWidth(m.cachedNeedsScrollbar))
	}
	base := "todo:" + row.payload + ":"
	indent := min(2, max(0, m.contentWidth(m.cachedNeedsScrollbar)-1))
	for _, part := range []struct {
		name       string
		start, end int
	}{{"status", indent, indent + 1}, {"remove", indent + 2, indent + 3}, {"edit", indent + 4, indent + 5}, {"text", indent + 6, m.contentWidth(m.cachedNeedsScrollbar)}} {
		value := m.hoverValues[base+part.name].value
		if value == 0 || (part.name != "text" && !row.todoControls) {
			continue
		}
		start, end := part.start, part.end
		if part.name == "text" && !row.todoControls {
			start = 0
		}
		text = ansi.Cut(text, 0, start) + styles.HoverText(ansi.Cut(text, start, end), value, styles.TextPrimary) + ansi.Cut(text, end, m.contentWidth(m.cachedNeedsScrollbar))
	}
	return text
}
