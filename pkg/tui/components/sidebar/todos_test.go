package sidebar

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestSidebarCanonicalTodosStatusAndRemoval(t *testing.T) {
	m := newPlacementSidebar(t, false)
	m.SetSize(60, 50)
	scope := messages.TodoScope{Owner: "owner", SessionID: "session", Generation: 1}
	_, cmd := m.Update(messages.TodosSnapshotMsg{Scope: scope, Todos: []session.Todo{{ID: "opaque", Description: "Editable 世界", Status: "pending"}}})
	settlePlacement(t, m, cmd)
	x := m.xPos + m.layoutCfg.PaddingLeft + 2
	y := m.yPos + m.todoSummaryLine + 2 - m.scrollview.ScrollOffset()
	_, cmd = m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.NotNil(t, cmd)
	// Layout reconciliation may batch the canonical action with animation work.
	var find func(tea.Cmd) messages.EditTodoMsg
	find = func(cmd tea.Cmd) messages.EditTodoMsg {
		if cmd == nil {
			return messages.EditTodoMsg{}
		}
		switch msg := cmd().(type) {
		case messages.EditTodoMsg:
			return msg
		case tea.BatchMsg:
			for _, child := range msg {
				if edit := find(child); edit.ID != "" {
					return edit
				}
			}
		}
		return messages.EditTodoMsg{}
	}
	require.Equal(t, messages.EditTodoMsg{Scope: scope, ID: "opaque", Status: "in-progress"}, find(cmd))
	_, cmd = m.Update(tea.MouseClickMsg{X: x + 2, Y: y, Button: tea.MouseLeft})
	require.Empty(t, find(cmd).ID)
	_, cmd = m.Update(tea.MouseClickMsg{X: x + 2, Y: y, Button: tea.MouseLeft})
	require.Equal(t, messages.EditTodoMsg{Scope: scope, ID: "opaque", Remove: true}, find(cmd))
}

func TestSidebarTodoContinuationNeverHasInvisibleControls(t *testing.T) {
	for _, placement := range []bool{false, true} {
		m := newPlacementSidebar(t, false)
		m.SetSize(40, 50)
		scope := messages.TodoScope{SessionID: "session"}
		_, cmd := m.Update(messages.TodosSnapshotMsg{Scope: scope, Todos: []session.Todo{{ID: "wrapped", Description: strings.Repeat("界é long task ", 5), Status: "pending"}}})
		settlePlacement(t, m, cmd)
		if !placement {
			m.placement = nil
		}
		y := m.todoSummaryLine + 3 - m.scrollview.ScrollOffset()
		for _, offset := range []int{0, 2} {
			cmd, handled := m.todoClick(m.layoutCfg.PaddingLeft+2+offset, y)
			require.True(t, handled)
			require.NotNil(t, cmd)
			require.IsType(t, messages.OpenTodosMsg{}, cmd(), "continuation indentation is body, never a hidden status/remove control")
		}
	}
}

func TestTodoSidebarEditorAndHoverUseVisibleGeometry(t *testing.T) {
	m := newPlacementSidebar(t, false)
	m.SetSize(60, 50)
	scope := messages.TodoScope{Owner: "owner", SessionID: "s"}
	_, cmd := m.Update(messages.TodosSnapshotMsg{Scope: scope, Todos: []session.Todo{{ID: "id", Status: "completed", Description: "long task description for hover"}}})
	settlePlacement(t, m, cmd)
	x := m.layoutCfg.PaddingLeft + 2
	y := m.todoSummaryLine + 2 - m.scrollview.ScrollOffset()
	cmd, handled := m.todoClick(x+4, y)
	require.True(t, handled)
	require.Equal(t, messages.OpenTodoEditMsg{Scope: scope, ID: "id"}, cmd())
	baseline := m.View()
	for _, tc := range []struct {
		col  int
		part string
	}{{0, "status"}, {2, "remove"}, {4, "edit"}, {8, "text"}} {
		cmd = m.updateRegionHover(x+tc.col, y)
		settlePlacement(t, m, cmd)
		require.Equal(t, "todo:id:"+tc.part, m.hoverTarget)
		require.NotEqual(t, baseline, m.View())
		require.Zero(t, m.ar.ActiveCount(), "settled hover owns no idle lease")
	}
	beforeInvalidations, beforeRenders := m.CacheStats()
	for range 20 {
		m.updateRegionHover(x+8, y)
		m.View()
	}
	afterInvalidations, afterRenders := m.CacheStats()
	require.Equal(t, beforeInvalidations, afterInvalidations)
	require.Equal(t, beforeRenders, afterRenders)
}
