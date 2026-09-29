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
	if !m.todosCollapsed && width > 22 {
		hint := " · ◯ status · /todos"
		if m.todoRemoveArmed != "" {
			hint = " · click × again to remove"
		}
		text += styles.MutedStyle.Render(hint)
	}
	text = ansi.Truncate(text, width-2, "…")
	return m.hoverText(text+strings.Repeat(" ", max(1, width-ansi.StringWidth(text)-1))+styles.MutedStyle.Render(glyph), "todo-summary")
}
