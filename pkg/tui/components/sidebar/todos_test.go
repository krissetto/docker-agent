package sidebar

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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
	x := m.xPos + m.layoutCfg.PaddingLeft + m.contentWidth(m.cachedNeedsScrollbar) - 5
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
	_, cmd = m.Update(tea.MouseClickMsg{X: x + 4, Y: y, Button: tea.MouseLeft})
	require.Empty(t, find(cmd).ID)
	_, cmd = m.Update(tea.MouseClickMsg{X: x + 4, Y: y, Button: tea.MouseLeft})
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
		for _, offset := range []int{2, m.contentWidth(m.cachedNeedsScrollbar) - 5, m.contentWidth(m.cachedNeedsScrollbar) - 3, m.contentWidth(m.cachedNeedsScrollbar) - 1} {
			cmd, handled := m.todoClick(m.layoutCfg.PaddingLeft+offset, y)
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
	x := m.layoutCfg.PaddingLeft
	width := m.contentWidth(m.cachedNeedsScrollbar)
	y := m.todoSummaryLine + 2 - m.scrollview.ScrollOffset()
	cmd, handled := m.todoClick(x+width-3, y)
	require.True(t, handled)
	require.Equal(t, messages.OpenTodoEditMsg{Scope: scope, ID: "id"}, cmd())
	baseline := m.View()
	row := requirePlaced(t, m, "todo:id:0")
	require.True(t, strings.HasPrefix(ansi.Strip(row.text), "  long task"))
	require.True(t, strings.HasSuffix(ansi.Strip(row.text), "● ✎ ×"))
	for _, tc := range []struct {
		col  int
		part string
	}{{width - 5, "status"}, {width - 1, "remove"}, {width - 3, "edit"}, {2, "text"}} {
		cmd = m.updateRegionHover(x+tc.col, y)
		settlePlacement(t, m, cmd)
		require.Equal(t, "todo:id:"+tc.part, m.hoverTarget)
		require.NotEqual(t, baseline, m.View())
		require.Zero(t, m.ar.ActiveCount(), "settled hover owns no idle lease")
	}
	beforeInvalidations, beforeRenders := m.CacheStats()
	for range 20 {
		m.updateRegionHover(x+2, y)
		m.View()
	}
	afterInvalidations, afterRenders := m.CacheStats()
	require.Equal(t, beforeInvalidations, afterInvalidations)
	require.Equal(t, beforeRenders, afterRenders)
}

func TestTodoActionsNarrowGeometry(t *testing.T) {
	for _, width := range []int{1, 2, 3, 7, 9, 10, 12, 20, 80} {
		m := newPlacementSidebar(t, false)
		m.SetSize(width, 60)
		_, cmd := m.Update(messages.TodosSnapshotMsg{Scope: messages.TodoScope{SessionID: "s"}, Todos: []session.Todo{{ID: "todo", Description: "界é 👩‍💻 text\ncontinuation", Status: "pending"}}})
		settlePlacement(t, m, cmd)
		w := m.contentWidth(m.cachedNeedsScrollbar)
		indent, actions := rowActions(w, true)
		for _, row := range m.placement.rows {
			if row.payload != "todo" || !strings.HasPrefix(row.id, "todo:") {
				continue
			}
			require.LessOrEqual(t, ansi.StringWidth(row.text), w)
			for col := indent; col < w; col++ {
				part := actions.PartAt(col-indent, row.todoControls)
				require.Equal(t, "todo:todo:"+part, m.todoHoverKey(row, col))
				cmd, handled := m.todoClick(m.layoutCfg.PaddingLeft+col, int(row.y)-m.scrollview.ScrollOffset())
				require.True(t, handled)
				switch part {
				case "status":
					require.IsType(t, messages.EditTodoMsg{}, cmd())
					require.Equal(t, "○", ansi.Strip(ansi.Cut(row.text, col, col+1)))
				case "edit":
					require.IsType(t, messages.OpenTodoEditMsg{}, cmd())
					require.Equal(t, "✎", ansi.Strip(ansi.Cut(row.text, col, col+1)))
				case "remove":
					require.Nil(t, cmd)
					require.Equal(t, "×", ansi.Strip(ansi.Cut(row.text, col, col+1)))
				default:
					require.IsType(t, messages.OpenTodosMsg{}, cmd())
				}
			}
		}
	}
}
