package sidebar

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/tool/todotool"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type todoBodyClick struct {
	at                  time.Time
	scope               messages.TodoScope
	item                session.Todo
	row                 string
	rowY                float64
	x, y, width, offset int
}

// ResetTodoClick cancels a pending body gesture when another surface takes input.
func (m *model) ResetTodoClick() { m.lastTodoClick = todoBodyClick{} }

func (m *model) todoClick(x, y int) (tea.Cmd, bool) {
	previous := m.lastTodoClick
	m.ResetTodoClick()
	if !m.presentationActive || m.todosCollapsed || m.sectionVisibility.HideTodos || m.todoScope.SessionID == "" || y < 0 || y >= m.viewportHeight() {
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
	rowID := ""
	rowY := 0.0
	if m.placement != nil {
		row, found := m.placementRowAt(x, y)
		if !found || !strings.HasPrefix(row.id, "todo:") || row.payload == "" {
			return nil, false
		}
		rowID, rowY = row.id, row.y
		controls = row.todoControls
		item, ok = m.todoComp.TodoByID(row.payload)
	} else {
		line := y + m.scrollview.ScrollOffset() - m.todoSummaryLine - 2
		rowY = float64(line)
		item, ok = m.todoComp.TodoAtLine(line)
		controls = m.todoComp.ControlsAtLine(line)
	}
	if !ok {
		return nil, false
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
	now := time.Now()
	current := todoBodyClick{scope: m.todoScope, item: item, row: rowID, rowY: rowY, x: x, y: y, width: m.contentWidth(m.cachedNeedsScrollbar), offset: m.scrollview.ScrollOffset()}
	elapsed := now.Sub(previous.at)
	previous.at = time.Time{}
	if previous == current && elapsed >= 0 && elapsed < styles.DoubleClickThreshold {
		return core.CmdHandler(messages.OpenTodoEditMsg{ID: item.ID, Scope: m.todoScope}), true
	}
	current.at = now
	m.lastTodoClick = current
	return nil, true
}
