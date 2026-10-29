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
	text := styles.MutedStyle.Render(fmt.Sprintf("%d/%d todos", completed, total))

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
	return m.actionRowText(row.text, "todo:"+row.payload+":", row.todoControls, true)
}

func (m *model) actionRowText(text, base string, first, todo bool) string {
	if todo {
		return m.todoRowText(text, base, first)
	}
	width := m.contentWidth(m.cachedNeedsScrollbar)
	indent, actions := rowActions(width, false)
	progress := m.hoverValues[base+"row"].value
	if (!first || actions.Remove < 0) && progress == 0 {
		return text
	}
	var out strings.Builder
	end := indent + actions.TextWidth
	out.WriteString(styles.HoverText(ansi.Cut(text, 0, end), progress, styles.TextPrimary))
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
		// One ramp per control: its own hover or the row's, whichever is further along.
		own := max(m.hoverValues[base+part.name].value, progress)
		if part.name == "remove" {
			out.WriteString(removeAction(own))
		} else {
			out.WriteString(hoverAction(ansi.Cut(text, col, col+1), own))
		}
		end = col + 1
	}
	out.WriteString(ansi.Cut(text, end, width))
	return out.String()
}

func (m *model) todoRowText(text, base string, first bool) string {
	width := m.contentWidth(m.cachedNeedsScrollbar)
	_, actions := rowActions(width, true)
	progress := m.hoverValues[base+"row"].value
	text = styles.HoverText(text, progress, styles.TextPrimary)
	if !first || actions.Remove < 0 || progress <= 0 {
		return text
	}
	// Only paint is occluded: wrapping, canonical text and continuation rows stay intact.
	text = padRight(ansi.Truncate(text, width-4, "…"), width-4)
	edit := hoverAction(styles.MutedStyle.Render("✎"), max(m.hoverValues[base+"edit"].value, progress))
	remove := removeAction(max(m.hoverValues[base+"remove"].value, progress))
	return text + " " + edit + " " + remove
}

// removeAction paints the destructive control in the stable semantic error
// color on the shared hover ramp. Hidden-at-rest cells stay blank.
func removeAction(reveal float64) string {
	if reveal <= 0 {
		return " "
	}
	return styles.HoverText(styles.BaseStyle.Foreground(styles.Error).Render("×"), reveal, styles.TextPrimary)
}
