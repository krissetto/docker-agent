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
	if todo {
		return indent, todotool.SidebarActions(width - indent)
	}
	return indent, todotool.RightActions(width-indent, false)
}

func (m *model) todoHoverKey(row placedRow, col int) string {
	indent, actions := rowActions(m.contentWidth(m.cachedNeedsScrollbar), true)
	return "todo:" + row.payload + ":" + actions.PartAt(col-indent, row.todoControls)
}

func (m *model) todoHoverText(row placedRow) string {
	return m.actionRowText(row.text, "todo:"+row.payload+":", row.todoControls, true, m.todoRemoveArmed == row.payload)
}

func (m *model) actionRowText(text, base string, first, todo, armed bool) string {
	if todo {
		return m.todoRowText(text, base, first, armed)
	}
	width := m.contentWidth(m.cachedNeedsScrollbar)
	indent, actions := rowActions(width, false)
	if (!first || actions.Remove < 0) && m.hoverValues[base+"text"].value == 0 {
		return text
	}
	var out strings.Builder
	end := indent + actions.TextWidth
	out.WriteString(styles.HoverText(ansi.Cut(text, 0, end), m.hoverValues[base+"text"].value, styles.TextPrimary))
	if !first || actions.Remove < 0 {
		out.WriteString(ansi.Cut(text, end, width))
		return out.String()
	}
	for _, part := range []struct {
		name string
		col  int
	}{{"status", actions.Status}, {"edit", actions.Edit}, {"remove", actions.Remove}} {
		if part.col < 0 {
			continue
		}
		col := indent + part.col
		out.WriteString(ansi.Cut(text, end, col))
		paint := ansi.Cut(text, col, col+1)
		if part.name == "remove" && armed {
			paint = styles.ErrorStyle.Render("×")
		}
		paint = styles.HoverText(paint, m.hoverValues[base+part.name].value, styles.TextPrimary)
		out.WriteString(hoverAction(paint, m.hoverValues[base+"row"].value))
		end = col + 1
	}
	out.WriteString(ansi.Cut(text, end, width))
	return out.String()
}

func (m *model) todoRowText(text, base string, first, armed bool) string {
	width := m.contentWidth(m.cachedNeedsScrollbar)
	_, actions := rowActions(width, true)
	progress := m.hoverValues[base+"row"].value
	text = styles.HoverText(text, progress, styles.TextPrimary)
	if !first || actions.Remove < 0 || progress <= 0 {
		return text
	}
	// Only paint is occluded: wrapping, canonical text and continuation rows stay intact.
	text = padRight(ansi.Truncate(text, width-4, "…"), width-4)
	edit := styles.HoverText(styles.MutedStyle.Render("✎"), m.hoverValues[base+"edit"].value, styles.TextPrimary)
	remove := styles.MutedStyle.Render("×")
	if armed {
		remove = styles.ErrorStyle.Render("×")
	}
	remove = styles.HoverText(remove, m.hoverValues[base+"remove"].value, styles.TextPrimary)
	return text + " " + hoverAction(edit, progress) + " " + hoverAction(remove, progress)
}
