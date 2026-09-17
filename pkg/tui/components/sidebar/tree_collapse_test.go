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
	"github.com/docker/docker-agent/pkg/tui/animation"
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
	m := newHoverSidebar(t)
	m.subagentSpinner = &fakeSpinner{m.subagentSpinner}
	m.rootSessionID = "collapse"
	m.SetSize(60, 70)
	m.SetPosition(5, 3)
	m.SetSubagentTree(collapseFixture())
	m.ReconcileLayout()
	m.CancelPresentation()
	m.View()
	return m
}

func clickTreeControl(t *testing.T, m *model, id subagent.NodeID, whole bool) {
	t.Helper()
	m.View()
	if whole {
		m.scrollview.SetScrollOffset(m.summaryLine)
		m.View()
		x := m.layoutCfg.PaddingLeft + m.contentWidth(m.cachedNeedsScrollbar) - 1
		y := m.summaryLine - m.scrollview.ScrollOffset()
		result, _ := m.HandleClickType(x, y)
		assert.Equal(t, ClickNone, result)
		_, cmd := m.Update(tea.MouseClickMsg{X: m.xPos + x, Y: m.yPos + y, Button: tea.MouseLeft})
		settleTreePresentation(t, m, cmd)
		m.View()
		return
	}
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
	_, hoverCmd := m.Update(tea.MouseMotionMsg{X: m.xPos + m.layoutCfg.PaddingLeft, Y: m.yPos + y})
	settleSidebarHover(t, m, hoverCmd)
	m.View()
	for _, controls := range m.treeControls {
		for _, current := range controls {
			if current.id == id {
				control = current
			}
		}
	}
	x := m.layoutCfg.PaddingLeft + control.x
	result, _ := m.HandleClickType(x, y)
	assert.Equal(t, ClickNone, result, "control hit must not dispatch an agent action")
	_, cmd := m.Update(tea.MouseClickMsg{X: m.xPos + x, Y: m.yPos + y, Button: tea.MouseLeft})
	settleTreePresentation(t, m, cmd)
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
	assert.Contains(t, ansi.Strip(m.View()), "3 subagents 1 active 1 attention")
	assert.Empty(t, m.subagentHoverZone, "hidden identities leave no attach targets")
	assert.Empty(t, m.agentClickZones, "hidden root leaves no identity target")

	snap := collapseFixture()
	snap.Nodes[0].Children[0].Children[0].Node.State = subagent.NodeIdle
	snap.Nodes[0].Children[0].Children = append(snap.Nodes[0].Children[0].Children, subagent.NodeSnapshot{Node: subagent.Node{ID: "arrival-full-id", Agent: "arrival", State: subagent.NodeRunning, NeedsAttention: true}})
	m.SetSubagentTree(snap)
	settleTreePresentation(t, m, m.ReconcileLayout())
	assert.Contains(t, ansi.Strip(m.View()), "4 subagents 1 active 2 attention")
	require.True(t, m.collapsedBranches["branch-full-identity"])
	clickTreeControl(t, m, "", true)
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
	assert.Equal(t, 1, strings.Count(before, "⌄"), "only independent summary toggle is visible before branch hover")
	assert.NotContains(t, before, "−", "title has no tree control")
	assert.Contains(t, before, "3 subagents 1 active 1 attention")
	var y int
	for row, id := range m.subagentHoverZone {
		if id == "branch-full-identity" {
			y = row
		}
	}
	require.Positive(t, y)
	_, hoverCmd := m.Update(tea.MouseMotionMsg{X: m.xPos + m.layoutCfg.PaddingLeft, Y: m.yPos + y})
	settleSidebarHover(t, m, hoverCmd)
	assert.Contains(t, ansi.Strip(m.View()), "⌄")
	generation := m.VisualGeneration()
	m.Update(tea.MouseMotionMsg{X: m.xPos + m.layoutCfg.PaddingLeft, Y: m.yPos + y})
	assert.Equal(t, generation, m.VisualGeneration(), "unchanged motion must not invalidate warm view")
	result, id := m.HandleClickType(m.layoutCfg.PaddingLeft+2, y)
	assert.Equal(t, ClickSubagent, result)
	assert.Equal(t, "branch-full-identity", id)
	assert.False(t, m.collapsedBranches["branch-full-identity"])
	settleSidebarHover(t, m, m.ClearSubagentHover())
	m.ReconcileLayout()
	assert.Equal(t, 1, strings.Count(ansi.Strip(m.View()), "⌄"), "leaving branch hides only its chevron")
}

func TestTreeCollapseNarrowScrolledMouseControls(t *testing.T) {
	t.Parallel()
	for _, width := range []int{8, 16, 20, 40} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			m := newCollapseSidebar(t)
			settleTreePresentation(t, m, m.SetSize(width, 4))
			m.View()
			clickTreeControl(t, m, "branch-full-identity", false)
			assert.True(t, m.collapsedBranches["branch-full-identity"])
			assert.Empty(t, m.agentClickZones, "current agent is not rendered")
			var branchRow int
			for row, id := range m.subagentHoverZone {
				if id == "branch-full-identity" {
					branchRow = row
				}
			}
			result, id := m.HandleClickType(m.layoutCfg.PaddingLeft+2, branchRow-m.scrollview.ScrollOffset())
			assert.Equal(t, ClickSubagent, result)
			assert.Equal(t, "branch-full-identity", id)
			clickTreeControl(t, m, "", true)
			require.True(t, m.treeCollapsed)
			for line := range strings.SplitSeq(m.View(), "\n") {
				assert.LessOrEqual(t, lipgloss.Width(line), width)
			}
			m.Update(tea.MouseClickMsg{X: m.xPos + width, Y: m.yPos, Button: tea.MouseLeft})
			assert.True(t, m.treeCollapsed, "outside clipped content cannot activate summary")
			m.Update(tea.MouseClickMsg{X: m.xPos + m.layoutCfg.PaddingLeft, Y: m.yPos + m.height, Button: tea.MouseLeft})
			assert.True(t, m.treeCollapsed, "below viewport cannot activate stale tree rectangles")
			m.scrollview.SetScrollOffset(m.treeSectionStart)
			m.View()
			y := m.treeSectionStart - m.scrollview.ScrollOffset()
			require.GreaterOrEqual(t, y, 0)
			require.Less(t, y, m.height)
			clickTreeControl(t, m, "", true)
			require.False(t, m.treeCollapsed)
			assert.True(t, m.collapsedBranches["branch-full-identity"])
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
	m.ReconcileLayout()
	m.CancelPresentation()
	m.transferAnimation.Stop() // relation remains; this test owns no backend timer loop
	assert.Equal(t, []string{"Scout", "Coder"}, m.transferParticipants())
	assert.Contains(t, ansi.Strip(m.treeSummary(80)), "5 subagents 2 active 1 attention")
	clickTreeControl(t, m, "branch-full-identity", false)
	assert.Contains(t, ansi.Strip(m.View()), "Scout", "collapsing one canonical branch does not hide sibling sync participants")
	assert.Contains(t, ansi.Strip(m.View()), "Coder")
	assert.Contains(t, ansi.Strip(m.treeSummary(80)), "5 subagents 2 active 1 attention")
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
	assert.Contains(t, after, styles.TabPrimaryStyle.Render("3 subagents"))
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

func TestPersistentTreeSummaryPositionCountsAndBranchRestore(t *testing.T) {
	t.Parallel()
	m := newCollapseSidebar(t)
	clickTreeControl(t, m, "branch-full-identity", false)
	before := ansi.Strip(m.View())
	row := m.treeSectionStart
	text := strings.TrimSpace(strings.Split(before, "\n")[row])
	clickTreeControl(t, m, "", true)
	assert.Equal(t, row, m.treeSectionStart, "summary remains at the same content row")
	assert.Equal(t, strings.TrimSpace(strings.TrimSuffix(text, "⌄")), strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(ansi.Strip(m.cachedLines[row])), "›")), "summary data persists while direction reflects state")
	assert.NotContains(t, ansi.Strip(m.View()), "planner")
	snap := collapseFixture()
	snap.Nodes[0].Children[0].Children[0].Node.State = subagent.NodeIdle
	m.SetSubagentTree(snap)
	settleTreePresentation(t, m, m.ReconcileLayout())
	assert.Contains(t, ansi.Strip(m.View()), "3 subagents 1 attention")
	clickTreeControl(t, m, "", true)
	assert.Equal(t, row, m.treeSectionStart)
	assert.True(t, m.collapsedBranches["branch-full-identity"])
	assert.Contains(t, ansi.Strip(m.View()), "planner")
	assert.NotContains(t, ansi.Strip(m.View()), "reviewer")
}

func TestPersistentSummaryHoverSettlesAndParentRemainsAccessible(t *testing.T) {
	t.Parallel()
	m := newHoverSidebar(t)
	m.SetSubagentContext("child-full-id", "root", "hover")
	m.SetSubagentTree(subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-full-id", Agent: "worker", State: subagent.NodeIdle}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "grandchild", Agent: "helper", State: subagent.NodeIdle}}}}}})
	m.View()
	settleTreePresentation(t, m, m.ReconcileLayout())
	y := m.summaryLine
	_, cmd := m.Update(tea.MouseMotionMsg{X: m.layoutCfg.PaddingLeft, Y: y})
	settleSidebarHover(t, m, cmd)
	assert.Zero(t, m.ar.ActiveCount())
	assert.Contains(t, ansi.Strip(m.cachedLines[m.summaryLine]), "1 subagents")
	generation := m.VisualGeneration()
	for range 100 {
		m.Update(tea.MouseMotionMsg{X: m.layoutCfg.PaddingLeft, Y: y})
	}
	assert.Equal(t, generation, m.VisualGeneration())
	clickTreeControl(t, m, "", true)
	require.True(t, m.treeCollapsed)
	m.View()
	result, payload := m.HandleClickType(m.layoutCfg.PaddingLeft, m.parentLineZone)
	assert.Equal(t, ClickSubagentParent, result, "whole-tree collapse preserves parent navigation")
	assert.Equal(t, "hover", payload)
	assert.Empty(t, m.agentClickZones)
	assert.Empty(t, m.subagentHoverZone)
	settleSidebarHover(t, m, m.ClearSubagentHover())
	assert.Zero(t, m.ar.ActiveCount())
}

func TestTrailingChevronAfterUnicodeNameAndSpinner(t *testing.T) {
	t.Parallel()
	for _, state := range []subagent.NodeState{subagent.NodeIdle, subagent.NodeRunning} {
		t.Run(string(state), func(t *testing.T) {
			m := newSubagentTestModel(t)
			m.SetSize(50, 30)
			node := subagent.Node{ID: "branch-full-id", Agent: "worker", Name: "作業worker", State: state}
			m.treeControls = make(map[int][]treeControl)
			before := m.subagentRow(node, "└ ", 49, 0, true)
			require.Len(t, m.treeControls[0], 1)
			x := m.treeControls[0][0].x
			want := ansi.StringWidth("└ "+node.DisplayName()) + 1
			if state == subagent.NodeRunning {
				want += 2
			}
			assert.Equal(t, want, x, "name then optional spinner then dedicated chevron")
			m.treeControls = make(map[int][]treeControl)
			m.hoveredTreeRow = 0
			m.hoveredSubagent = node.ID
			m.hoverTarget = "node:" + string(node.ID)
			after := m.subagentRow(node, "└ ", 49, 0, true)
			assert.Equal(t, x, m.treeControls[0][0].x)
			assert.Equal(t, "⌄", ansi.Strip(ansi.Cut(after, x, x+1)))
			assert.Equal(t, ansi.Strip(ansi.Cut(before, 0, x)), ansi.Strip(ansi.Cut(after, 0, x)), "hover never shifts the name or spinner")
			assert.Equal(t, 49, ansi.StringWidth(before))
			assert.Equal(t, 49, ansi.StringWidth(after))
			m.treeControls = make(map[int][]treeControl)
			m.subagentRow(node, "└ ", 49, 0, false)
			assert.Empty(t, m.treeControls, "leaf rows reserve no chevron")
		})
	}
}

func TestRepeatedTrailingChevronClicksKeepIdentityHoverAndNeverAttach(t *testing.T) {
	t.Parallel()
	for _, width := range []int{8, 20, 60} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			m := newHoverSidebar(t)
			settleTreePresentation(t, m, m.SetSize(width, 20))
			snap := collapseFixture()
			snap.Nodes[0].Children[0].Children[0].Node.State = subagent.NodeIdle // isolate hover lease; running spinner cadence is covered separately
			m.rootSessionID = "collapse"
			m.SetSubagentTree(snap)
			settleTreePresentation(t, m, m.ReconcileLayout())
			m.View()
			var row int
			var control treeControl
			for r, controls := range m.treeControls {
				for _, candidate := range controls {
					if candidate.id == "branch-full-identity" {
						row = m.treeSectionStart + r
						control = candidate
					}
				}
			}
			require.NotEmpty(t, control.id)
			m.scrollview.SetScrollOffset(row)
			m.View()
			y := row - m.scrollview.ScrollOffset()
			x := m.layoutCfg.PaddingLeft + control.x
			_, cmd := m.Update(tea.MouseMotionMsg{X: x, Y: y})
			settleSidebarHover(t, m, cmd)
			require.InDelta(t, 1, m.hoverValues["node:branch-full-identity"].value, 0, "chevron hover highlights its agent name")
			for _, controls := range m.treeControls {
				for _, current := range controls {
					if current.id == control.id {
						control = current
					}
				}
			}
			x = m.layoutCfg.PaddingLeft + control.x
			assert.Equal(t, control.id, m.hoveredSubagent)
			assert.Equal(t, "node:"+string(control.id), m.hoverTarget)
			var layoutCmd tea.Cmd
			for _, collapsed := range []bool{true, false, true, false} {
				result, payload := m.HandleClickType(x, y)
				assert.Equal(t, ClickNone, result, "chevron never dispatches attach")
				assert.Empty(t, payload)
				_, next := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				if next != nil {
					require.Nil(t, layoutCmd, "retarget must not allocate a second clock")
					layoutCmd = next
				}
				assert.Equal(t, collapsed, m.collapsedBranches[control.id], "same coordinates toggle without a motion or view")
				assert.Equal(t, control.id, m.hoveredSubagent, "control click retains actualrow identity")
				assert.Equal(t, "node:"+string(control.id), m.hoverTarget)
				row, ok := findPlaced(m.placement.rows, "node:"+string(control.id))
				require.True(t, ok)
				chevron := "⌄"
				if collapsed {
					chevron = "›"
				}
				assert.Equal(t, chevron, ansi.Strip(ansi.Cut(row.text, control.x, control.x+1)), "own hovered chevron remains visible aftertoggle")
			}
			settleTreePresentation(t, m, layoutCmd)
			m.View()
			actualRow := -1
			for contentRow, id := range m.subagentHoverZone {
				if id == control.id {
					actualRow = contentRow
				}
			}
			require.GreaterOrEqual(t, actualRow, 0)
			result, payload := m.HandleClickType(m.layoutCfg.PaddingLeft, actualRow-m.scrollview.ScrollOffset())
			assert.Equal(t, ClickSubagent, result, "name remains attachable separately")
			assert.Equal(t, string(control.id), payload)
			settleSidebarHover(t, m, m.ClearSubagentHover())
			assert.Zero(t, m.ar.ActiveCount())
		})
	}
}

func TestRepeatedTrailingChevronWithRunningSharedClock(t *testing.T) {
	t.Parallel()
	for _, width := range []int{8, 20, 60} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			m := newHoverSidebar(t)
			settleTreePresentation(t, m, m.SetSize(width, 20))
			m.rootSessionID = "collapse"
			snap := collapseFixture()
			snap.Nodes[0].Children[0].Node.State = subagent.NodeRunning
			chain := sidebarOwnerCommand(m, m.SetSubagentTree(snap))
			m.ReconcileLayout()
			require.NotNil(t, chain, "owner commits the first running tree node")
			for range 40 {
				if !m.placement.running && !m.countersRunning() {
					break
				}
				tick, ok := m.ar.Accept(chain().(animation.TickMsg))
				require.True(t, ok)
				m.Update(tick)
				chain = m.ar.Continue()
				require.NotNil(t, chain)
			}
			m.View()
			var row int
			var control treeControl
			for r, controls := range m.treeControls {
				for _, candidate := range controls {
					if candidate.id == "branch-full-identity" {
						row = m.treeSectionStart + r
						control = candidate
					}
				}
			}
			require.NotEmpty(t, control.id)
			m.scrollview.SetScrollOffset(row)
			m.View()
			y := row - m.scrollview.ScrollOffset()
			x := m.layoutCfg.PaddingLeft + control.x
			_, hoverCmd := m.Update(tea.MouseMotionMsg{X: x, Y: y})
			assert.Nil(t, hoverTickCommand(t, hoverCmd), "hover joins the existing shared lease without starting a parallel clock")
			chain = settleSidebarHover(t, m, chain)
			require.NotNil(t, chain, "running spinner retains the continuation after hover settles")
			for _, controls := range m.treeControls {
				for _, current := range controls {
					if current.id == control.id {
						control = current
					}
				}
			}
			x = m.layoutCfg.PaddingLeft + control.x
			for range 2 {
				for _, collapsed := range []bool{true, false} {
					result, payload := m.HandleClickType(x, y)
					assert.Equal(t, ClickNone, result)
					assert.Empty(t, payload)
					m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
					assert.Equal(t, collapsed, m.collapsedBranches[control.id], "same coordinates toggle without intervening motion or frame")
				}
				before := m.subagentSpinner.RawFrame()
				for range 20 {
					tick, ok := m.ar.Accept(chain().(animation.TickMsg))
					require.True(t, ok)
					m.Update(tick)
					chain = m.ar.Continue()
					require.NotNil(t, chain)
					if before != m.subagentSpinner.RawFrame() {
						break
					}
				}
				assert.NotEqual(t, before, m.subagentSpinner.RawFrame(), "repeat click pair spans actual spinner frames")
				m.View()
				found := false
				for _, controls := range m.treeControls {
					for _, candidate := range controls {
						if candidate.id != control.id {
							continue
						}
						assert.GreaterOrEqual(t, candidate.x, 0)
						assert.Less(t, candidate.x, m.contentWidth(m.cachedNeedsScrollbar))
						row, ok := findPlaced(m.placement.rows, "node:"+string(control.id))
						require.True(t, ok)
						assert.Contains(t, "⌄›", ansi.Strip(ansi.Cut(row.text, candidate.x, candidate.x+1)), "actualpainted control—not full untruncatednamewidth—defines hit cell")
						found = true
					}
				}
				assert.True(t, found)
			}
			assert.Nil(t, m.ClearSubagentHover(), "exit also joins the spinner clock")
			chain = settleSidebarHover(t, m, chain)
			require.NotNil(t, chain)
			snap.Nodes[0].Children[0].Node.State = subagent.NodeIdle
			snap.Nodes[0].Children[0].Children[0].Node.State = subagent.NodeIdle
			m.SetSubagentTree(snap)
			require.Nil(t, m.ReconcileLayout(), "counter fade only registers with the owner")
			// Stopping the last spinner invalidates its queued tick before the
			// counter fade registers. The owner commits a new lease for that fade;
			// queued delivery of the old spinner command must remain rejected.
			fresh := m.ar.Continue()
			require.NotNil(t, fresh, "owner schedules the newly registered counter fade")
			_, accepted := m.ar.Accept(chain().(animation.TickMsg))
			require.False(t, accepted, "idle spinner's queued lease is stale")
			settleTreePresentation(t, m, fresh)
			assert.Zero(t, m.ar.ActiveCount(), "counter fadeout and idle descendants release all leases")
		})
	}
}

func TestParentAboveSummaryFullRowToggleAndExpandedGap(t *testing.T) {
	t.Parallel()
	for _, fraction := range []int{0, 1, 2} {
		m := newHoverSidebar(t)
		m.SetSubagentContext("child", "parent", "parent-session")
		m.SetSubagentTree(subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child", Agent: "child", State: subagent.NodeIdle}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "grandchild", Agent: "worker", State: subagent.NodeIdle}}}}}})
		m.ReconcileLayout()
		m.CancelPresentation()
		m.View()
		require.Equal(t, m.parentLineZone+2, m.summaryLine)
		assert.Empty(t, strings.TrimSpace(ansi.Strip(m.cachedLines[m.parentLineZone+1])))
		assert.Empty(t, strings.TrimSpace(ansi.Strip(m.cachedLines[m.summaryLine+1])), "expanded recap has one breathing row before children")
		width := m.contentWidth(m.cachedNeedsScrollbar)
		x := m.layoutCfg.PaddingLeft + fraction*(width-1)/2
		result, payload := m.HandleClickType(x, m.summaryLine)
		assert.Equal(t, ClickNone, result)
		assert.Empty(t, payload)
		_, cmd := m.Update(tea.MouseClickMsg{X: x, Y: m.summaryLine, Button: tea.MouseLeft})
		require.True(t, m.treeCollapsed, "left text, middle blank, and right glyph toggle the same recap")
		settleTreePresentation(t, m, cmd)
		m.View()
		result, payload = m.HandleClickType(m.layoutCfg.PaddingLeft, m.parentLineZone)
		assert.Equal(t, ClickSubagentParent, result)
		assert.Equal(t, "parent-session", payload)
		for _, row := range m.placement.rows {
			if row.target {
				assert.NotEqual(t, "node:grandchild", row.id)
				assert.NotEqual(t, "gap:node:grandchild", row.id, "collapsed recap leaves no gap target")
			}
		}
	}
}

func TestCollapsedBranchPlainCountMovesInlineSpans(t *testing.T) {
	t.Parallel()
	m := newCollapseSidebar(t)
	node := m.subagentNodes[0].Node
	m.collapsedBranches = make(map[subagent.NodeID]bool)
	m.hoveredSubagent = node.ID
	m.hoverTarget = "node:" + string(node.ID)
	var nameX int
	for _, progress := range []float64{0, .5, 1} {
		m.branchSpans[node.ID] = branchSpans{count: 2, value: progress, target: 1, idValue: progress, idTarget: 1}
		m.collapsedBranches[node.ID] = true
		m.treeControls = make(map[int][]treeControl)
		line := m.subagentRow(node, "", 60, 0, true)
		plain := ansi.Strip(line)
		assert.NotContains(t, plain, "(2)")
		require.Len(t, m.treeControls[0], 1)
		x := strings.Index(plain, "planner")
		if progress == 0 {
			nameX = x
			assert.Contains(t, plain, "planner ›")
		} else {
			assert.Equal(t, nameX, x)
		}
		if progress == 1 {
			assert.Contains(t, plain, "planner 2 (branch-full-identity) ›")
			assert.Contains(t, line, neutralSpan(" 2", 1, 2), "plain count stays neutral")
		}
	}
}

func TestWholeExpandDoesNotReuseStaleBranchHoverIndex(t *testing.T) {
	t.Parallel()
	m := newCollapseSidebar(t)
	m.hoveredTreeRow = 0 // stale position from a previously hovered branch
	m.hoveredSubagent = "branch-full-identity"
	m.hoverTarget = "tree-summary" // pointer is now on the independently clickable recap
	m.invalidateCache()
	m.ReconcileLayout()
	m.CancelPresentation()
	settleTreePresentation(t, m, m.toggleTreeControl(treeControl{whole: true}))
	cmd := m.toggleTreeControl(treeControl{whole: true})
	cmd = hoverTickCommand(t, sidebarOwnerCommand(m, cmd))
	for range 10 {
		for _, row := range m.placement.rows {
			if row.target && strings.HasPrefix(row.id, "node:") {
				assert.NotContains(t, ansi.Strip(row.text), "⌄", "unhovered revealed branches have no glyph")
				assert.NotContains(t, ansi.Strip(row.text), "›")
			}
		}
		require.NotNil(t, cmd)
		tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		m.Update(tick)
		cmd = m.ar.Continue()
	}
	settleTreePresentation(t, m, cmd)
}

func TestCountAndNewIDSpansShareElapsedReveal(t *testing.T) {
	t.Parallel()
	m := newHoverSidebar(t)
	snap := collapseFixture()
	snap.Nodes[0].Children[0].Children[0].Node.State = subagent.NodeIdle
	m.rootSessionID = "collapse"
	m.SetSubagentTree(snap)
	m.ReconcileLayout()
	m.CancelPresentation()
	id := subagent.NodeID("branch-full-identity")
	m.hoveredSubagent = id
	m.hoverTarget = "node:" + string(id)
	m.collapsedBranches = map[subagent.NodeID]bool{id: true}
	m.invalidateCache()
	cmd := m.ReconcileLayout()
	span := m.branchSpans[id]
	require.Zero(t, span.value)
	require.Zero(t, span.idValue)
	cmd = hoverTickCommand(t, sidebarOwnerCommand(m, cmd))
	require.NotNil(t, cmd)
	tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
	require.True(t, ok)
	m.Update(tick)
	next := m.ar.Continue()
	span = m.branchSpans[id]
	assert.Greater(t, span.value, 0.0)
	assert.Less(t, span.value, 1.0)
	require.InDelta(t, span.value, span.idValue, 0, "count and newlyappearing ID share acceptedelapsed and easing")
	settleTreePresentation(t, m, next)
	span = m.branchSpans[id]
	require.InDelta(t, 1, span.value, 0)
	require.InDelta(t, 1, span.idValue, 0)
	assert.Zero(t, m.ar.ActiveCount())
	m.collapsedBranches[id] = false
	m.invalidateCache()
	cmd = m.ReconcileLayout()
	require.InDelta(t, 1, m.branchSpans[id].idValue, 0, "alreadyvisible hovered ID does not blink when count exits")
	settleTreePresentation(t, m, cmd)
	require.Zero(t, m.branchSpans[id].value)
	require.InDelta(t, 1, m.branchSpans[id].idValue, 0)
	assert.Zero(t, m.ar.ActiveCount())
}

func TestCapturedChevronNeverFallsThroughWhenTemporarilyOccluded(t *testing.T) {
	t.Parallel()
	m := newCollapseSidebar(t)
	clickTreeControl(t, m, "branch-full-identity", false)
	capture := m.branchCapture
	require.NotEmpty(t, capture.id)
	row, ok := findPlaced(m.placement.rows, "node:"+string(capture.id))
	require.True(t, ok)
	// A moving target can temporarily paint over the captured row; it must not
	// turn the same physical press into an attachment for a different identity.
	m.placement.rows = append(m.placement.rows, placedRow{id: "node:occluding", y: row.y, alpha: 1, target: true, action: ClickSubagent, payload: "occluding"})
	result, payload := m.HandleClickType(capture.x, capture.y)
	assert.Equal(t, ClickNone, result)
	assert.Empty(t, payload)
	_, valid := m.capturedBranchAt(capture.x, capture.y)
	assert.False(t, valid, "occluded capture does not toggle a different row")
}
