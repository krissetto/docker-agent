package sidebar

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
)

func TestTodoRecapCollapsePreservesCanonicalUpdatesAndIndent(t *testing.T) {
	t.Parallel()
	m := newPlacementSidebar(t, false)
	m.SetSize(40, 50)
	require.NoError(t, m.SetTodos(&tools.ToolCallResult{Meta: []todo.Todo{{ID: "one", Description: "界 Unicode task with enough words to wrap onto several lines under the recap", Status: "pending"}}}))
	settlePlacement(t, m, m.ReconcileLayout())
	lines := m.cachedLines
	require.Greater(t, m.todoSummaryLine, m.summaryLine)
	assert.Empty(t, strings.TrimSpace(ansi.Strip(lines[m.todoSummaryLine-1])), "tree and todos have an explicit gap")
	assert.Contains(t, ansi.Strip(lines[m.todoSummaryLine]), "0/1 todos")
	for y := m.todoSummaryLine + 1; y < m.todoEnd; y++ {
		assert.True(t, strings.HasPrefix(ansi.Strip(lines[y]), "  "), "wrapped todo row %d is indented", y)
	}
	for y := range m.subagentHoverZone {
		assert.True(t, strings.HasPrefix(ansi.Strip(lines[y]), "  "), "tree body row %d is indented", y)
	}
	x, y := m.layoutCfg.PaddingLeft, m.todoSummaryLine-m.scrollview.ScrollOffset()
	_, cmd := m.Update(tea.MouseClickMsg{X: m.xPos + x, Y: m.yPos + y, Button: tea.MouseLeft})
	settlePlacement(t, m, cmd)
	require.True(t, m.todosCollapsed)
	assert.Equal(t, m.todoSummaryLine+1, m.todoEnd)
	assert.False(t, m.treeCollapsed, "independent recap controls")
	require.NoError(t, m.SetTodos(&tools.ToolCallResult{Meta: []todo.Todo{{ID: "one", Description: "updated canonical text", Status: "completed"}}}))
	settlePlacement(t, m, m.ReconcileLayout())
	assert.Contains(t, ansi.Strip(m.View()), "1/1 todos")
	assert.NotContains(t, ansi.Strip(m.View()), "updated canonical text")
	_, cmd = m.Update(tea.MouseClickMsg{X: m.xPos + x, Y: m.yPos + m.todoSummaryLine - m.scrollview.ScrollOffset(), Button: tea.MouseLeft})
	settlePlacement(t, m, cmd)
	assert.Contains(t, ansi.Strip(m.View()), "updated canonical text")
}

func TestFooterBreathingRowBoundedAndPassive(t *testing.T) {
	t.Parallel()
	for _, width := range []int{1, 2, 8, 40} {
		for _, height := range []int{0, 1, 2, 3, 12, 50} {
			for _, yolo := range []bool{false, true} {
				t.Run(fmt.Sprintf("%dx%d/yolo=%t", width, height, yolo), func(t *testing.T) {
					m := newPlacementSidebar(t, false)
					m.sessionState.SetYoloMode(yolo)
					require.NoError(t, m.SetTodos(makeTodos(4)))
					m.SetSize(width, height)
					m.CancelPresentation()
					view := m.View()
					if height == 0 {
						require.Empty(t, view)
						return
					}
					lines := strings.Split(view, "\n")
					require.Len(t, lines, height)
					for _, line := range lines {
						assert.LessOrEqual(t, ansi.StringWidth(line), width)
					}
					if height > 2 {
						assert.Empty(t, strings.TrimSpace(ansi.Strip(lines[height-2])))
						for x := range width {
							action, _ := m.HandleClickType(x, height-2)
							assert.Equal(t, ClickNone, action)
							_, hit := m.treeControlAt(x, height-2)
							assert.False(t, hit)
						}
					}
					assert.Equal(t, max(0, height-m.footerHeight()-m.footerGapHeight()), m.viewportHeight())
				})
			}
		}
	}
}

func TestTabSnapshotRetargetIsInertAndSessionLocal(t *testing.T) {
	t.Parallel()
	a, b, c := newPlacementSidebar(t, false), newPlacementSidebar(t, true), newPlacementSidebar(t, false)
	a.sessionTitle, b.sessionTitle, c.sessionTitle = "source A", "destination B", "destination C"
	for _, m := range []*model{a, b, c} {
		m.invalidateCache()
		m.ReconcileLayout()
		m.CancelPresentation()
	}
	a.Update(tea.MouseMotionMsg{X: a.xPos + a.layoutCfg.PaddingLeft, Y: a.yPos + a.summaryLine})
	snapshot := a.CapturePresentation()
	a.SetPresentationActive(false)
	require.Zero(t, a.ar.ActiveCount())
	for _, row := range snapshot.rows {
		assert.Equal(t, ClickNone, row.action)
		assert.Empty(t, row.controls)
		assert.False(t, row.target)
	}
	require.Nil(t, b.TransitionFrom(snapshot), "registration never schedules a timer")
	require.True(t, b.placement.running)
	require.True(t, b.treeCollapsed)
	assert.Contains(t, ansi.Strip(b.View()), "source A", "first frame starts at outgoing painted content")
	cmd := advancePlacement(t, b, nil)
	mixed := b.CapturePresentation()
	require.NotEmpty(t, mixed.rows)
	b.SetPresentationActive(false)
	require.Zero(t, b.ar.ActiveCount())
	c.TransitionFrom(mixed)
	require.True(t, c.placement.running)
	assert.False(t, c.treeCollapsed)
	for y := range c.viewportHeight() {
		row, hit := c.placementRowAt(c.layoutCfg.PaddingLeft, y)
		if hit {
			assert.True(t, row.target)
			assert.NotContains(t, row.id, "exit:")
		}
	}
	settlePlacement(t, c, nil)
	assert.Contains(t, ansi.Strip(c.View()), "destination C")
	assert.NotContains(t, ansi.Strip(c.View()), "source A")
	assert.NotContains(t, ansi.Strip(c.View()), "destination B")
	assert.Zero(t, c.ar.ActiveCount())
	_ = cmd // outstanding owner tick becomes stale after b is hidden
}

func TestHoverRetainsPreparedTopologyAndSettledMotionDoesNoRender(t *testing.T) {
	t.Parallel()
	m := newPlacementSidebar(t, false)
	width := m.contentWidth(m.cachedNeedsScrollbar)
	prepared := m.preparedTrees[width]
	require.NotEmpty(t, prepared.rows)
	var y int
	for row := range m.subagentHoverZone {
		y = row
		break
	}
	_, cmd := m.Update(tea.MouseMotionMsg{X: m.xPos + m.layoutCfg.PaddingLeft, Y: m.yPos + y})
	settlePlacement(t, m, cmd)
	// Map identity is stable; semantic preparation is not repeated by hover.
	assert.Equal(t, fmt.Sprintf("%p", prepared.rows), fmt.Sprintf("%p", m.preparedTrees[width].rows))
	beforeInvalidations, beforeRenders := m.CacheStats()
	for range 100 {
		m.Update(tea.MouseMotionMsg{X: m.xPos + m.layoutCfg.PaddingLeft, Y: m.yPos + y})
		m.View()
	}
	afterInvalidations, afterRenders := m.CacheStats()
	assert.Equal(t, beforeInvalidations, afterInvalidations)
	assert.Equal(t, beforeRenders, afterRenders)
	assert.Zero(t, m.ar.ActiveCount())
}
