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
	indent, actions := rowActions(m.contentWidth(m.cachedNeedsScrollbar), true)
	localX := x - m.layoutCfg.PaddingLeft - indent
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
		item, ok = m.todoComp.TodoByID(row.payload)
	} else {
		line := y + m.scrollview.ScrollOffset() - m.todoSummaryLine - 2
		item, ok = m.todoComp.TodoAtLine(line)
		controls = m.todoComp.ControlsAtLine(line)
	}
	if !ok {
		return nil, false
	}
	if m.queueRemoveArmed != "" {
		m.queueRemoveArmed = ""
		m.invalidateHover()
	}
	part := actions.PartAt(localX, controls)
	if (part == "edit" || part == "remove") && m.hoverValues["todo:"+item.ID+":row"].value <= 0 {
		part = "text"
	}
	if part == "remove" {
		if m.todoRemoveArmed != item.ID {
			m.todoRemoveArmed = item.ID
			m.invalidateHover()
			return nil, true
		}
		return core.CmdHandler(messages.EditTodoMsg{Scope: m.todoScope, ID: item.ID, Remove: true}), true
	}
	if m.todoRemoveArmed != "" {
		m.todoRemoveArmed = ""
		m.invalidateHover()
	}
	if part == "status" {
		return core.CmdHandler(messages.EditTodoMsg{Scope: m.todoScope, ID: item.ID, Status: todotool.NextStatus(item.Status)}), true
	}
	if part == "edit" {
		return core.CmdHandler(messages.OpenTodoEditMsg{ID: item.ID, Scope: m.todoScope}), true
	}
	return core.CmdHandler(messages.OpenTodosMsg{ID: item.ID, Scope: m.todoScope}), true
}
