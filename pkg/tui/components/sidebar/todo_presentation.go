package sidebar

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/components/tool/todotool"
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

func rowActions(width int, todo bool) (int, todotool.RowActions) {
	indent := min(2, max(0, width-1))
	return indent, todotool.RightActions(width-indent, todo)
}

func (m *model) todoHoverKey(row placedRow, col int) string {
	indent, actions := rowActions(m.contentWidth(m.cachedNeedsScrollbar), true)
	return "todo:" + row.payload + ":" + actions.PartAt(col-indent, row.todoControls)
}

func (m *model) todoHoverText(row placedRow) string {
	return m.actionRowText(row.text, "todo:"+row.payload+":", row.todoControls, true, m.todoRemoveArmed == row.payload)
}

func (m *model) actionRowText(text, base string, first, todo, armed bool) string {
	width := m.contentWidth(m.cachedNeedsScrollbar)
	indent, actions := rowActions(width, todo)
	if first && armed && actions.Remove >= 0 {
		col := indent + actions.Remove
		text = ansi.Cut(text, 0, col) + styles.ErrorStyle.Render("×") + ansi.Cut(text, col+1, width)
	}
	for _, part := range []struct {
		name       string
		start, end int
	}{{"text", 0, indent + actions.TextWidth}, {"status", actions.Status + indent, actions.Status + indent + 1}, {"edit", actions.Edit + indent, actions.Edit + indent + 1}, {"remove", actions.Remove + indent, actions.Remove + indent + 1}} {
		if part.name != "text" && (!first || part.start < indent) {
			continue
		}
		value := m.hoverValues[base+part.name].value
		if value == 0 {
			continue
		}
		text = ansi.Cut(text, 0, part.start) + styles.HoverText(ansi.Cut(text, part.start, part.end), value, styles.TextPrimary) + ansi.Cut(text, part.end, width)
	}
	return text
}
