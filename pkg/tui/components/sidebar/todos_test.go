package sidebar

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
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
	settlePlacement(t, m, m.updateRegionHover(x+4, y))
	_, cmd = m.Update(tea.MouseClickMsg{X: m.layoutCfg.PaddingLeft + m.contentWidth(m.cachedNeedsScrollbar) - 1, Y: y, Button: tea.MouseLeft})
	require.Empty(t, find(cmd).ID)
	settlePlacement(t, m, m.updateRegionHover(x+4, y))
	_, cmd = m.Update(tea.MouseClickMsg{X: m.layoutCfg.PaddingLeft + m.contentWidth(m.cachedNeedsScrollbar) - 1, Y: y, Button: tea.MouseLeft})
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
			require.Nil(t, cmd, "single-click continuation is inert, never a hidden status/remove control")
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
	settlePlacement(t, m, m.updateRegionHover(x+4, y))
	cmd, handled := m.todoClick(x+width-3, y)
	require.True(t, handled)
	require.Equal(t, messages.OpenTodoEditMsg{Scope: scope, ID: "id"}, cmd())
	m.CancelHover()
	m.ReconcileLayout()
	baseline := m.View()
	row := requirePlaced(t, m, "todo:id:0")
	require.True(t, strings.HasPrefix(ansi.Strip(row.text), "  ● long task"))
	require.NotContains(t, ansi.Strip(row.text), "✎")
	for _, tc := range []struct {
		col  int
		part string
	}{{2, "status"}, {width - 1, "remove"}, {width - 3, "edit"}, {4, "text"}} {
		cmd = m.updateRegionHover(x+tc.col, y)
		settlePlacement(t, m, cmd)
		require.Equal(t, "todo:id:"+tc.part, m.hoverTarget)
		require.NotEqual(t, baseline, m.View())
		require.Zero(t, m.ar.ActiveCount(), "settled hover owns no idle lease")
	}
	beforeInvalidations, beforeRenders := m.CacheStats()
	for range 20 {
		m.updateRegionHover(x+4, y)
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
				settlePlacement(t, m, m.updateRegionHover(m.layoutCfg.PaddingLeft+col, int(row.y)-m.scrollview.ScrollOffset()))
				part := actions.PartAt(col-indent, row.todoControls)
				require.Equal(t, "todo:todo:"+part, m.todoHoverKey(row, col))
				cmd, handled := m.todoClick(m.layoutCfg.PaddingLeft+col, int(row.y)-m.scrollview.ScrollOffset())
				require.True(t, handled)
				switch part {
				case "status":
					require.IsType(t, messages.EditTodoMsg{}, cmd())
					require.Equal(t, "○", ansi.Strip(ansi.Cut(m.placementText(row, w), col, col+1)))
				case "edit":
					require.IsType(t, messages.OpenTodoEditMsg{}, cmd())
					require.Equal(t, "✎", ansi.Strip(ansi.Cut(m.placementText(row, w), col, col+1)))
				case "remove":
					require.Nil(t, cmd)
					require.Equal(t, "×", ansi.Strip(ansi.Cut(m.placementText(row, w), col, col+1)))
				default:
					require.Nil(t, cmd, "single-click body is inert")
				}
			}
		}
	}
}

func TestTodoItemHoverKeepsTextHighlightedAcrossControls(t *testing.T) {
	for _, status := range []string{"pending", "in-progress", "completed"} {
		m := newPlacementSidebar(t, false)
		m.SetSize(40, 60)
		items := []session.Todo{
			{ID: "first", Status: status, Description: strings.Repeat("世界 é 👩‍💻 long description ", 4)},
			{ID: "next", Status: "pending", Description: "Adjacent unchanged item"},
		}
		_, cmd := m.Update(messages.TodosSnapshotMsg{Scope: messages.TodoScope{SessionID: "s"}, Todos: items})
		settlePlacement(t, m, cmd)
		first := requirePlaced(t, m, "todo:first:0")
		next := requirePlaced(t, m, "todo:first:1")
		adjacent := requirePlaced(t, m, "todo:next:0")
		width := m.contentWidth(m.cachedNeedsScrollbar)
		indent, actions := rowActions(width, true)
		idle := m.View()
		require.Equal(t, first.text, m.placementText(first, width), "idle uses full-width canonical text")
		require.Greater(t, ansi.StringWidth(first.text), width-4, "fixture fills the action overlay cells")
		_, renders := m.CacheStats()
		settlePlacement(t, m, m.updateRegionHover(m.layoutCfg.PaddingLeft+indent+2, int(first.y)))
		highlight := sidebarCells(m.placementText(first, width))[:width-5]
		continuation := m.placementText(next, width)
		for _, col := range []int{indent + actions.Status, indent + actions.Edit, indent + actions.Remove, indent + 2} {
			cmd = m.updateRegionHover(m.layoutCfg.PaddingLeft+col, int(first.y))
			cmd = advancePlacement(t, m, cmd)
			require.Equal(t, 1.0, m.hoverValues["todo:first:row"].value)
			require.Equal(t, highlight, sidebarCells(m.placementText(first, width))[:width-5], "action hover keeps the same row highlight even mid-transition")
			require.Equal(t, continuation, m.placementText(next, width), "every continuation retains whole-item highlight")
			settlePlacement(t, m, cmd)
			require.Equal(t, first, requirePlaced(t, m, first.id), "hover never replaces or rewraps canonical rows")
			require.Equal(t, adjacent, requirePlaced(t, m, adjacent.id))
			require.Equal(t, adjacent.text, m.placementText(adjacent, width), "adjacent item never reveals commands")
			require.Zero(t, m.ar.ActiveCount())
		}
		_, after := m.CacheStats()
		require.Equal(t, renders, after)
		item, ok := m.todoComp.TodoByID("first")
		require.True(t, ok)
		require.Equal(t, items[0], item, "overlay cannot truncate canonical text")
		settlePlacement(t, m, m.ClearSubagentHover())
		require.Equal(t, idle, m.View(), "leaving restores all occluded text without layout changes")
	}
}

func TestIdleTodoTextUnderOverlayDoesNotTriggerHiddenCommands(t *testing.T) {
	m := newPlacementSidebar(t, false)
	m.SetSize(40, 60)
	_, cmd := m.Update(messages.TodosSnapshotMsg{Scope: messages.TodoScope{SessionID: "s"}, Todos: []session.Todo{{ID: "id", Status: "pending", Description: strings.Repeat("long words ", 10)}}})
	settlePlacement(t, m, cmd)
	row := requirePlaced(t, m, "todo:id:0")
	width := m.contentWidth(m.cachedNeedsScrollbar)
	for _, col := range []int{width - 3, width - 1} {
		cmd, handled := m.todoClick(m.layoutCfg.PaddingLeft+col, int(row.y))
		require.True(t, handled)
		require.Nil(t, cmd, "idle text is not an invisible edit/remove button")
	}
	cmd, handled := m.todoClick(m.layoutCfg.PaddingLeft+2, int(row.y))
	require.True(t, handled)
	require.Equal(t, "in-progress", cmd().(messages.EditTodoMsg).Status, "persistent left status remains clickable without hover")
}

func TestTodoBodyDoubleClickUsesIdentityScopeAndTiming(t *testing.T) {
	for _, placement := range []bool{false, true} {
		m := newPlacementSidebar(t, false)
		m.SetSize(60, 50)
		scope := messages.TodoScope{Owner: "owner", SessionID: "session", Generation: 7, Epoch: 3}
		_, cmd := m.Update(messages.TodosSnapshotMsg{Scope: scope, Todos: []session.Todo{{ID: "opaque", Description: "body", Status: "pending"}, {ID: "other", Description: "body", Status: "pending"}}})
		settlePlacement(t, m, cmd)
		if !placement {
			m.placement = nil
		}
		x, y := m.layoutCfg.PaddingLeft+6, m.todoSummaryLine+2-m.scrollview.ScrollOffset()
		cmd, handled := m.todoClick(x, y)
		require.True(t, handled)
		require.Nil(t, cmd)
		require.False(t, m.lastTodoClick.at.IsZero())
		cmd, handled = m.todoClick(x, y)
		require.True(t, handled)
		require.NotNil(t, cmd)
		require.Equal(t, messages.OpenTodoEditMsg{Scope: scope, ID: "opaque"}, cmd())
		require.True(t, m.lastTodoClick.at.IsZero(), "pair is consumed")
		cmd, _ = m.todoClick(x, y)
		require.Nil(t, cmd)
		m.lastTodoClick.at = time.Now().Add(-styles.DoubleClickThreshold)
		cmd, _ = m.todoClick(x, y)
		require.Nil(t, cmd, "expired click starts a new pair")
		cmd, _ = m.todoClick(x, y+1)
		require.Nil(t, cmd, "adjacent item cannot finish a pair")
		cmd, _ = m.todoClick(x, y+1)
		require.NotNil(t, cmd)
		require.Equal(t, messages.OpenTodoEditMsg{Scope: scope, ID: "other"}, cmd())
	}
}

func TestTodoBodyDoubleClickInvalidation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		interrupt func(*model, int, int)
	}{
		{"motion", func(m *model, x, y int) {
			m.Update(tea.MouseMotionMsg{X: x + 1, Y: y})
			m.Update(tea.MouseMotionMsg{X: x, Y: y})
		}},
		{"drag", func(m *model, x, y int) { m.Update(tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft}) }},
		{"release elsewhere", func(m *model, x, y int) { m.Update(tea.MouseReleaseMsg{X: x + 1, Y: y, Button: tea.MouseLeft}) }},
		{"right click", func(m *model, x, y int) { m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseRight}) }},
		{"outside click", func(m *model, _, _ int) { m.todoClick(-1, -1) }},
		{"modal", func(m *model, _, _ int) { m.ClearSubagentHover() }},
		{"hidden", func(m *model, _, _ int) { m.SetPresentationActive(false); m.SetPresentationActive(true) }},
		{"resize", func(m *model, _, _ int) { m.SetSize(61, 50); m.SetSize(60, 50) }},
		{"position", func(m *model, _, _ int) { m.SetPosition(1, 0); m.SetPosition(0, 0) }},
		{"scope", func(m *model, _, _ int) { m.todoScope.Epoch++ }},
		{"geometry", func(m *model, _, _ int) { m.lastTodoClick.rowY++ }},
		{"snapshot", func(m *model, _, _ int) {
			m.Update(messages.TodosSnapshotMsg{Scope: m.todoScope, Todos: []session.Todo{{ID: "opaque", Description: "new", Status: "pending"}}})
		}},
		{"disabled", func(m *model, x, y int) { m.todosCollapsed = true; m.todoClick(x, y); m.todosCollapsed = false }},
		{"status", func(m *model, _, y int) {
			cmd, handled := m.todoClick(m.layoutCfg.PaddingLeft+2, y)
			require.True(t, handled)
			require.IsType(t, messages.EditTodoMsg{}, cmd())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newPlacementSidebar(t, false)
			m.SetSize(60, 50)
			_, cmd := m.Update(messages.TodosSnapshotMsg{Scope: messages.TodoScope{SessionID: "s"}, Todos: []session.Todo{{ID: "opaque", Description: "body", Status: "pending"}}})
			settlePlacement(t, m, cmd)
			x, y := m.layoutCfg.PaddingLeft+6, m.todoSummaryLine+2-m.scrollview.ScrollOffset()
			cmd, handled := m.todoClick(x, y)
			require.True(t, handled)
			require.Nil(t, cmd)
			tc.interrupt(m, x, y)
			cmd, _ = m.todoClick(x, y)
			require.Nil(t, cmd, "interrupted gesture cannot edit")
		})
	}
}
