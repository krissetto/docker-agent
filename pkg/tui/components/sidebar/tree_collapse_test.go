package sidebar

import (
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func collapseFixture() subagent.Snapshot {
	leaf := subagent.NodeSnapshot{Node: subagent.Node{ID: "leaf-full-identity", Agent: "reviewer", State: subagent.NodeStarting}}
	nested := subagent.NodeSnapshot{Node: subagent.Node{ID: "nested-full-identity", Agent: "worker", State: subagent.NodeRunning, NeedsAttention: true}, Children: []subagent.NodeSnapshot{leaf}}
	branch := subagent.NodeSnapshot{Node: subagent.Node{ID: "branch-full-identity", Agent: "planner", State: subagent.NodeIdle}, Children: []subagent.NodeSnapshot{nested}}
	root := subagent.NodeSnapshot{Node: subagent.Node{ID: "root:collapse", Agent: "root", State: subagent.NodeIdle}, Children: []subagent.NodeSnapshot{branch}}
	return subagent.Snapshot{Root: root.Node.ID, Nodes: []subagent.NodeSnapshot{root}}
}

func newCollapseSidebar(t *testing.T) *model {
	t.Helper()
	m := newSubagentTestModel(t)
	m.rootSessionID = "collapse"
	m.SetSize(60, 70)
	m.SetPosition(5, 3)
	m.SetSubagentTree(collapseFixture())
	m.View()
	return m
}

func clickTreeControl(t *testing.T, m *model, id subagent.NodeID, whole bool) {
	t.Helper()
	m.View()
	row := -1
	var control treeControl
	for r, controls := range m.treeControls {
		for _, c := range controls {
			if c.whole == whole && (whole || c.id == id) {
				row, control = r, c
				break
			}
		}
	}
	require.GreaterOrEqual(t, row, 0, "requested branch must have a control")
	contentY := m.treeSectionStart + row
	m.scrollview.SetScrollOffset(max(0, contentY-m.height+1))
	m.View()
	y := contentY - m.scrollview.ScrollOffset()
	m.Update(tea.MouseMotionMsg{X: m.xPos + m.layoutCfg.PaddingLeft, Y: m.yPos + y})
	m.View()
	x := m.layoutCfg.PaddingLeft + control.x
	result, _ := m.HandleClickType(x, y)
	assert.Equal(t, ClickNone, result, "control hit must not dispatch an agent action")
	m.Update(tea.MouseClickMsg{X: m.xPos + x, Y: m.yPos + y, Button: tea.MouseLeft})
	m.View()
}

func TestTreeCollapseRestoresBranchesAndTracksHiddenUpdates(t *testing.T) {
	t.Parallel()
	m := newCollapseSidebar(t)
	clickTreeControl(t, m, "nested-full-identity", false)
	assert.True(t, m.collapsedBranches["nested-full-identity"])
	assert.NotContains(t, ansi.Strip(m.View()), "reviewer")
	clickTreeControl(t, m, "branch-full-identity", false)
	clickTreeControl(t, m, "", true)
	require.True(t, m.treeCollapsed)
	assert.Contains(t, ansi.Strip(m.View()), "4 total · 1 active · 1 attention")
	assert.Empty(t, m.subagentHoverZone, "hidden identities leave no attach targets")
	assert.Empty(t, m.agentClickZones, "hidden root leaves no identity target")

	snap := collapseFixture()
	snap.Nodes[0].Children[0].Children[0].Node.State = subagent.NodeIdle
	snap.Nodes[0].Children[0].Children = append(snap.Nodes[0].Children[0].Children, subagent.NodeSnapshot{Node: subagent.Node{ID: "arrival-full-id", Agent: "arrival", State: subagent.NodeRunning, NeedsAttention: true}})
	m.SetSubagentTree(snap)
	assert.Contains(t, ansi.Strip(m.View()), "5 total · 1 active · 2 attention")
	require.True(t, m.collapsedBranches["branch-full-identity"])
	y := m.treeSectionStart - m.scrollview.ScrollOffset()
	m.Update(tea.MouseClickMsg{X: m.xPos + m.layoutCfg.PaddingLeft, Y: m.yPos + y, Button: tea.MouseLeft})
	require.False(t, m.treeCollapsed)
	assert.Contains(t, ansi.Strip(m.View()), "planner")
	assert.NotContains(t, ansi.Strip(m.View()), "arrival")
	clickTreeControl(t, m, "branch-full-identity", false)
	assert.Contains(t, ansi.Strip(m.View()), "arrival")
	assert.NotContains(t, ansi.Strip(m.View()), "reviewer", "nested branch retains its independent folded state")
	clickTreeControl(t, m, "nested-full-identity", false)
	assert.Contains(t, ansi.Strip(m.View()), "reviewer")

	clickTreeControl(t, m, "branch-full-identity", false)
	snap.Nodes[0].Children = nil
	m.SetSubagentTree(snap)
	assert.NotContains(t, m.collapsedBranches, subagent.NodeID("branch-full-identity"), "authoritatively removed identities are pruned")
}

func TestTreeChevronHoverIdentityAndUnchangedMotion(t *testing.T) {
	t.Parallel()
	m := newCollapseSidebar(t)
	before := ansi.Strip(m.View())
	assert.NotContains(t, before, "⌄")
	assert.NotContains(t, before, "−")
	var y int
	for row, id := range m.subagentHoverZone {
		if id == "branch-full-identity" {
			y = row
		}
	}
	require.Positive(t, y)
	m.Update(tea.MouseMotionMsg{X: m.xPos + m.layoutCfg.PaddingLeft, Y: m.yPos + y})
	assert.Contains(t, ansi.Strip(m.View()), "⌄")
	generation := m.VisualGeneration()
	m.Update(tea.MouseMotionMsg{X: m.xPos + m.layoutCfg.PaddingLeft, Y: m.yPos + y})
	assert.Equal(t, generation, m.VisualGeneration(), "unchanged motion must not invalidate warm view")
	result, id := m.HandleClickType(m.layoutCfg.PaddingLeft, y)
	assert.Equal(t, ClickSubagent, result)
	assert.Equal(t, "branch-full-identity", id)
	assert.False(t, m.collapsedBranches["branch-full-identity"])
	m.ClearSubagentHover()
	assert.NotContains(t, ansi.Strip(m.View()), "⌄")
}

func TestTreeCollapseNarrowScrolledMouseControls(t *testing.T) {
	t.Parallel()
	for _, width := range []int{8, 16, 20, 40} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			m := newCollapseSidebar(t)
			m.SetSize(width, 4)
			m.View()
			clickTreeControl(t, m, "root:collapse", false)
			assert.True(t, m.collapsedBranches["root:collapse"])
			require.Len(t, m.agentClickZones, 1)
			for row := range m.agentClickZones {
				y := row - m.scrollview.ScrollOffset()
				result, name := m.HandleClickType(m.layoutCfg.PaddingLeft, y)
				assert.Equal(t, ClickAgent, result)
				assert.Equal(t, "root", name)
			}
			clickTreeControl(t, m, "", true)
			require.True(t, m.treeCollapsed)
			for line := range strings.SplitSeq(m.View(), "\n") {
				assert.LessOrEqual(t, lipgloss.Width(line), width)
			}
			m.Update(tea.MouseClickMsg{X: m.xPos + width, Y: m.yPos, Button: tea.MouseLeft})
			assert.True(t, m.treeCollapsed, "outside clipped content cannot activate summary")
			m.Update(tea.MouseClickMsg{X: m.xPos + m.layoutCfg.PaddingLeft, Y: m.yPos + m.height, Button: tea.MouseLeft})
			assert.True(t, m.treeCollapsed, "below viewport cannot activate stale tree rectangles")
			y := m.treeSectionStart - m.scrollview.ScrollOffset()
			m.Update(tea.MouseClickMsg{X: m.xPos + m.layoutCfg.PaddingLeft, Y: m.yPos + y, Button: tea.MouseLeft})
			require.False(t, m.treeCollapsed)
			assert.True(t, m.collapsedBranches["root:collapse"])
		})
	}
}

func TestTreeSummaryDeduplicatesSynchronousParticipants(t *testing.T) {
	t.Parallel()
	m := newCollapseSidebar(t)
	m.SetAgentSwitching(true, "root", "Scout")
	m.SetAgentSwitching(true, "Scout", "Coder")
	t.Cleanup(m.transferAnimation.Stop)
	m.workingAgent = "Coder"
	assert.Equal(t, []string{"Scout", "Coder"}, m.transferParticipants())
	assert.Contains(t, ansi.Strip(m.treeSummary(80)), "6 total · 2 active · 1 attention")
	clickTreeControl(t, m, "root:collapse", false)
	assert.NotContains(t, ansi.Strip(m.View()), "Scout")
	assert.NotContains(t, ansi.Strip(m.View()), "Coder")
	assert.Contains(t, ansi.Strip(m.treeSummary(80)), "6 total · 2 active · 1 attention")
}

func TestTreeCollapseWarmThemeRetainsStateAndColors(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := newCollapseSidebar(t)
	clickTreeControl(t, m, "branch-full-identity", false)
	clickTreeControl(t, m, "", true)
	before := m.View()
	theme := *original
	theme.Colors.TextMuted = "#68a2bf"
	styles.ApplyTheme(&theme)
	after := m.View()
	assert.Equal(t, ansi.Strip(before), ansi.Strip(after))
	assert.NotEqual(t, before, after)
	assert.True(t, m.treeCollapsed)
	assert.True(t, m.collapsedBranches["branch-full-identity"])
	assert.Contains(t, after, styles.MutedStyle.Render("› 4 total · 1 active · 1 attention"))
	assert.Equal(t, after, m.View())
}

func TestSidebarScrollbarDragAdmitsFramesWithoutNoopMotion(t *testing.T) {
	t.Parallel()
	for _, width := range []int{8, 20, 40} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			m := newCollapseSidebar(t)
			m.SetSize(width, 4)
			before := m.View()
			require.True(t, m.cachedNeedsScrollbar)
			x := m.xPos + width - 1
			generation := m.VisualGeneration()
			m.Update(tea.MouseClickMsg{X: x, Y: m.yPos, Button: tea.MouseLeft})
			require.True(t, m.IsScrollbarDragging())
			assert.Greater(t, m.VisualGeneration(), generation, "drag start must admit a sidebar frame")
			generation = m.VisualGeneration()
			m.Update(tea.MouseMotionMsg{X: x, Y: m.yPos, Button: tea.MouseLeft})
			assert.Equal(t, generation, m.VisualGeneration(), "stationary drag does not redraw")
			m.Update(tea.MouseMotionMsg{X: x, Y: m.yPos + 2, Button: tea.MouseLeft})
			assert.Positive(t, m.scrollview.ScrollOffset())
			assert.Greater(t, m.VisualGeneration(), generation, "effective drag must escape the parent view cache")
			assert.NotEqual(t, before, m.View())
			generation = m.VisualGeneration()
			m.Update(tea.MouseMotionMsg{X: x, Y: m.yPos + 2, Button: tea.MouseLeft})
			assert.Equal(t, generation, m.VisualGeneration())
			m.Update(tea.MouseReleaseMsg{X: x, Y: m.yPos + 2, Button: tea.MouseLeft})
			assert.False(t, m.IsScrollbarDragging())
			assert.Greater(t, m.VisualGeneration(), generation)
		})
	}
}
