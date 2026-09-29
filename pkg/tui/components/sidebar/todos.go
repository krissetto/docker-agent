package sidebar

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/tool/todotool"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func (m *model) todoClick(x, y int) (tea.Cmd, bool) {
	if m.todosCollapsed || m.sectionVisibility.HideTodos || m.todoScope.SessionID == "" || y < 0 || y >= m.viewportHeight() {
		return nil, false
	}
	localX := x - m.layoutCfg.PaddingLeft - min(2, max(0, m.contentWidth(m.cachedNeedsScrollbar)-1))
	if localX < 0 || x >= m.layoutCfg.PaddingLeft+m.contentWidth(m.cachedNeedsScrollbar) {
		return nil, false
	}
	var item session.Todo
	var ok bool
	controls := false
	if m.placement != nil {
		row, found := m.placementRowAt(x, y)
		if !found || !strings.HasPrefix(row.id, "todo:") || row.payload == "" {
			return nil, false
		}
		controls = row.todoControls
		for line := 0; line < m.todoEnd-m.todoSummaryLine; line++ {
			candidate, exists := m.todoComp.TodoAtLine(line)
			if exists && candidate.ID == row.payload {
				item, ok = candidate, true
				break
			}
		}
	} else {
		line := y + m.scrollview.ScrollOffset() - m.todoSummaryLine - 2
		item, ok = m.todoComp.TodoAtLine(line)
		controls = m.todoComp.ControlsAtLine(line)
	}
	if !ok {
		return nil, false
	}
	if localX == 0 && controls {
		if m.todoRemoveArmed != item.ID {
			m.todoRemoveArmed = item.ID
			m.invalidateCache()
			return nil, true
		}
		return core.CmdHandler(messages.EditTodoMsg{Scope: m.todoScope, ID: item.ID, Remove: true}), true
	}
	m.todoRemoveArmed = ""
	if localX == 2 && controls {
		return core.CmdHandler(messages.EditTodoMsg{Scope: m.todoScope, ID: item.ID, Status: todotool.NextStatus(item.Status)}), true
	}
	return core.CmdHandler(messages.OpenTodosMsg{ID: item.ID, Scope: m.todoScope}), true
}
