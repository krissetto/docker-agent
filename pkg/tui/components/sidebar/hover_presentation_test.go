package sidebar

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestHoverPresentationMatchesFullRefresh(t *testing.T) {
	for _, width := range []int{80, 40, 20, 8} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			m := newHoverSidebar(t)
			m.SetSize(width, 30)
			m.SetQueuedMessages([]QueuedMessage{{ID: "queued", Text: "界面 café é queued work"}})
			m.CancelPresentation()
			m.ReconcileLayout()
			for _, id := range []string{"directory", "node:child-full-id", "tree-summary", "queue:queued:0", "usage:0"} {
				row, ok := findPlaced(m.placement.rows, id)
				if !ok {
					continue
				}
				_, cmd := m.Update(tea.MouseMotionMsg{X: m.layoutCfg.PaddingLeft, Y: int(row.y)})
				cmd = hoverTickCommand(t, sidebarOwnerCommand(m, cmd))
				for range 24 {
					fast := m.View()
					m.refreshPlacementPresentation()
					assert.Equal(t, m.View(), fast, "full-render parity for %s", id)
					if cmd == nil {
						break
					}
					tick, accepted := m.ar.Accept(cmd().(animation.TickMsg))
					require.True(t, accepted)
					m.Update(tick)
					cmd = m.ar.Continue()
				}
			}
		})
	}
}

func TestLongTreeHoverDoesNotRenderSections(t *testing.T) {
	m := newHoverSidebar(t)
	root := subagent.NodeSnapshot{Node: subagent.Node{ID: "root:hover", Agent: "root", State: subagent.NodeIdle}}
	for i := range 160 {
		root.Children = append(root.Children, subagent.NodeSnapshot{Node: subagent.Node{ID: subagent.NodeID(fmt.Sprintf("node-%03d", i)), Agent: "worker界面", State: subagent.NodeIdle}})
	}
	m.SetSubagentTree(subagent.Snapshot{Root: "root:hover", Nodes: []subagent.NodeSnapshot{root}})
	m.ReconcileLayout()
	m.CancelPresentation()
	m.ReconcileLayout()
	row, ok := findPlaced(m.placement.rows, "node:node-000")
	require.True(t, ok)
	_, before := m.CacheStats()
	_, cmd := m.Update(tea.MouseMotionMsg{X: m.layoutCfg.PaddingLeft, Y: int(row.y)})
	settleSidebarHover(t, m, cmd)
	m.View()
	_, after := m.CacheStats()
	assert.Equal(t, before, after, "hover must not reassemble the complete long sidebar")
	assert.Zero(t, m.ar.ActiveCount())
	m.ClearSubagentHover()
	m.ReconcileLayout()
	settleSidebarHover(t, m, nil)
	assert.Zero(t, m.ar.ActiveCount())
	beforeView := m.View()
	_, idleCmd := m.Update(tea.MouseMotionMsg{X: -1, Y: -1})
	assert.Nil(t, hoverTickCommand(t, sidebarOwnerCommand(m, idleCmd)), "settled outside pointer needs no owner tick")
	assert.Equal(t, beforeView, m.View(), "settled outside motion preserves the painted frame")
}

func TestPlacementScrollbarPaintsCapturedThumbAcrossThemes(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	for _, name := range []string{"default", "default-light"} {
		t.Run(name, func(t *testing.T) {
			theme, err := styles.LoadTheme(name)
			require.NoError(t, err)
			styles.ApplyTheme(theme)
			m := newHoverSidebar(t)
			m.SetSize(40, 5)
			m.CancelPresentation()
			m.ReconcileLayout()
			m.SetPosition(0, 0)
			require.True(t, m.cachedNeedsScrollbar)
			thumb := func() color.Color {
				cells := sidebarCells(strings.Split(m.View(), "\n")[0])
				return cells[len(cells)-1].fg
			}
			idle := thumb()
			before := m.VisualGeneration()
			x := m.width - m.layoutCfg.PaddingRight - 1
			m.Update(tea.MouseClickMsg{X: x, Y: 0, Button: tea.MouseLeft})
			require.True(t, m.scrollview.IsDragging())
			assert.Greater(t, m.VisualGeneration(), before)
			active := thumb()
			assert.Equal(t, color.NRGBAModel.Convert(styles.ThumbActiveStyle.GetForeground()), color.NRGBAModel.Convert(active))
			assert.NotEqual(t, color.NRGBAModel.Convert(idle), color.NRGBAModel.Convert(active))
			m.Update(tea.MouseMotionMsg{X: -20, Y: 0, Button: tea.MouseLeft})
			assert.Equal(t, color.NRGBAModel.Convert(active), color.NRGBAModel.Convert(thumb()))
			require.True(t, m.scrollview.IsDragging(), "painting and outside motion preserve capture")
			before = m.VisualGeneration()
			m.Update(tea.MouseReleaseMsg{X: -20, Y: 0, Button: tea.MouseLeft})
			assert.Greater(t, m.VisualGeneration(), before)
			assert.False(t, m.scrollview.IsDragging())
			assert.Zero(t, m.scrollview.ScrollOffset())
			assert.Equal(t, color.NRGBAModel.Convert(idle), color.NRGBAModel.Convert(thumb()))
			assert.Zero(t, m.ar.ActiveCount())
		})
	}
}

func TestHoverSemanticRefreshKeepsStatusAgeQueueAndTodosCurrent(t *testing.T) {
	m := newHoverSidebar(t)
	snap := subagent.Snapshot{Root: "root:hover", Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "root:hover", Agent: "root", State: subagent.NodeIdle}}}}
	for i := range 160 {
		snap.Nodes[0].Children = append(snap.Nodes[0].Children, subagent.NodeSnapshot{Node: subagent.Node{ID: subagent.NodeID(fmt.Sprintf("child-%03d", i)), Agent: "worker界面", State: subagent.NodeIdle, CreatedAt: time.Now().Add(-10*time.Minute - 30*time.Second)}})
	}
	m.SetSubagentTree(snap)
	m.SetQueuedMessages([]QueuedMessage{{ID: "queue", Text: "old queue"}})
	require.NoError(t, m.SetTodos(&tools.ToolCallResult{Meta: []todo.Todo{{ID: "todo", Description: "old todo", Status: "pending"}}}))
	m.ReconcileLayout()
	m.CancelPresentation()
	m.ReconcileLayout()
	row, ok := findPlaced(m.placement.rows, "node:child-000")
	require.True(t, ok)
	m.scrollview.EnsureLineVisible(int(row.y))
	y := int(row.y) - m.scrollview.ScrollOffset()
	painted, ok := m.placementRowAt(m.layoutCfg.PaddingLeft, y)
	require.True(t, ok)
	require.Equal(t, row.id, painted.id, "fixture must hover the painted child, not an offscreen document row")
	_, cmd := m.Update(tea.MouseMotionMsg{X: m.layoutCfg.PaddingLeft, Y: y})
	require.Equal(t, subagent.NodeID("child-000"), m.hoveredSubagent)
	settleSidebarHover(t, m, cmd)
	row, _ = findPlaced(m.placement.rows, row.id)
	assert.Contains(t, ansi.Strip(row.text), "10m ago")
	// Semantic updates must not be masked by a pending narrow hover refresh.
	m.invalidateHover()
	snap.Nodes[0].Children[0].Node.State = subagent.NodeCompleted
	snap.Nodes[0].Children[0].Node.CreatedAt = time.Now().Add(-20*time.Minute - 30*time.Second)
	m.SetSubagentTree(snap)
	m.SetQueuedMessages([]QueuedMessage{{ID: "queue", Text: "new queue界面"}})
	require.NoError(t, m.SetTodos(&tools.ToolCallResult{Meta: []todo.Todo{{ID: "todo", Description: "new todo界面", Status: "completed"}}}))
	m.ReconcileLayout()
	m.CancelPresentation()
	m.ReconcileLayout()
	m.hoveredSubagent = "child-000"
	m.invalidateHover()
	m.ReconcileLayout()
	row, _ = findPlaced(m.placement.rows, row.id)
	assert.Contains(t, ansi.Strip(row.text), "20m ago")
	all := strings.Join(m.cachedLines, "\n")
	assert.Contains(t, ansi.Strip(all), "new queue界面")
	assert.Contains(t, ansi.Strip(all), "new todo界面")
	m.ClearSubagentHover()
	m.ReconcileLayout()
	settleSidebarHover(t, m, nil)
	row, _ = findPlaced(m.placement.rows, row.id)
	assert.Contains(t, ansi.Strip(row.text), "completed")
	fast := m.View()
	m.refreshPlacementPresentation()
	assert.Equal(t, m.View(), fast)
}
