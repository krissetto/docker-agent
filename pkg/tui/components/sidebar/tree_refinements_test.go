package sidebar

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
)

func TestSubagentRowKeepsHoveredIDAndHierarchyAcrossAnimation(t *testing.T) {
	t.Parallel()
	m := newCollapseSidebar(t)
	node := subagent.Node{
		ID:        "branch-full-identity",
		Agent:     "planner",
		Name:      "品質計画🌿",
		State:     subagent.NodeIdle,
		CreatedAt: time.Now().Add(-10 * time.Minute),
	}
	m.hoveredSubagent = node.ID
	m.hoverTarget = "node:" + string(node.ID)
	m.branchSpans[node.ID] = branchSpans{count: 5, idTarget: 1}

	for _, width := range []int{24, 60} {
		var lines []string
		for _, progress := range []float64{0, .5, 1} {
			span := m.branchSpans[node.ID]
			span.idValue = progress
			m.branchSpans[node.ID] = span
			line, identity := m.subagentRowIdentity(node, "│ └ ", width, 0, true)
			plain := ansi.Strip(line)
			require.LessOrEqual(t, ansi.StringWidth(plain), width)
			require.Contains(t, plain, "└ ", "branch connector survives before the identity")
			require.NotContains(t, plain, "5", "the hovered ID takes precedence over the child-count badge")
			if width == 24 {
				assert.NotContains(t, plain, "planner", "the narrow row spends its identity cells on the hovered ID")
				if progress > 0 {
					assert.Contains(t, plain, "bran", "a narrow hovered ID remains visible during and after its reveal")
				}
			} else if progress == 1 {
				assert.Contains(t, plain, "branch-full-identity", "wide row shows the complete ID after animation settles")
			}
			if progress == 1 {
				if width == 24 {
					assert.Positive(t, identity.nameEnd-identity.nameStart, "the narrow row keeps the resting name hit under the hovered ID")
				} else {
					assert.Greater(t, identity.nameEnd, identity.nameStart, "the rendered name has the only identity hit span")
					assert.Contains(t, plain, "品質計画🌿", "wide rows preserve their name when both name and ID fit")
				}
			}
			lines = append(lines, plain)
		}
		assert.Equal(t, ansi.StringWidth(lines[0]), ansi.StringWidth(lines[1]), "hover phases reserve stable row width")
		assert.Equal(t, ansi.StringWidth(lines[1]), ansi.StringWidth(lines[2]), "hover phases reserve stable row width")
	}
}

func TestDeepHoveredIdentityKeepsConnectorAndIDThroughAnimation(t *testing.T) {
	t.Parallel()
	m := newCollapseSidebar(t)
	node := subagent.Node{ID: "branch-full-identity", Agent: "worker", Name: "very-long-agent-name", State: subagent.NodeIdle}
	guides := "│ │ │ │ │ │ │ └ "
	m.hoveredSubagent = node.ID
	m.hoverTarget = "node:" + string(node.ID)
	m.branchSpans[node.ID] = branchSpans{count: 4, idTarget: 1}

	for _, width := range []int{24, 48} {
		var rowWidths []int
		for _, progress := range []float64{0, .5, 1} {
			span := m.branchSpans[node.ID]
			span.idValue = progress
			m.branchSpans[node.ID] = span
			line, _ := m.subagentRowIdentity(node, guides, width, 0, true)
			plain := ansi.Strip(line)
			rowWidths = append(rowWidths, ansi.StringWidth(plain))
			require.LessOrEqual(t, ansi.StringWidth(plain), width)
			require.Contains(t, plain, "└ ", "current hierarchy connector survives ID reservation")
			if progress > 0 {
				require.Contains(t, plain, "bra", "hovered ID remains visible during and after animation")
			}
			assert.NotContains(t, plain, "very-long-agent-name", "deep hierarchy and hovered ID outrank the display name")
		}
		assert.Equal(t, rowWidths[0], rowWidths[1], "ID animation reserves stable row width")
		assert.Equal(t, rowWidths[1], rowWidths[2], "settled ID retains its reserved width")
	}
}

func TestSubagentRowUnicodeAndDeepGuidesFitBeforeName(t *testing.T) {
	t.Parallel()
	guides := "│ │ │ │ │ └ "
	for _, width := range []int{8, 12, 20, 36} {
		line, _, _, _ := subagentLineParts("長い名前🌿 with a suffix", "", width, 1)
		assert.LessOrEqual(t, ansi.StringWidth(line), width)
		guideBudget := min(max(2, width/4), width-1)
		guide := subagentGuideTail(guides, guideBudget)
		assert.LessOrEqual(t, ansi.StringWidth(guide), guideBudget)
		if guideBudget > 0 {
			assert.True(t, strings.ContainsAny(guide, "├└"), "deep guide preserves its current connector at width %d: %q", width, guide)
		}
		row := guide + ansi.Truncate("長い名前🌿 with a suffix", max(0, width-ansi.StringWidth(guide)), "…")
		assert.LessOrEqual(t, ansi.StringWidth(row), width)
	}

	m := newCollapseSidebar(t)
	node := subagent.Node{ID: "deep-id", Agent: "worker", Name: "very-long-unicode-name-品質🌿", State: subagent.NodeIdle}
	fullGuides := "│ │ │ │ │ └ "
	line, _ := m.subagentRowIdentity(node, fullGuides, 36, 0, false)
	plain := ansi.Strip(line)
	assert.True(t, strings.HasPrefix(plain, fullGuides), "retain every hierarchy cell when the row can fit them: %q", plain)
	assert.NotContains(t, plain, "very-long-unicode-name", "truncate the display name after preserving deep connectors")
	assert.LessOrEqual(t, ansi.StringWidth(plain), 36)
}

func TestHoveredIDKeepsNameAndDisclosureHitSpansDistinct(t *testing.T) {
	t.Parallel()
	m := newCollapseSidebar(t)
	m.SetSize(48, 70)
	m.CancelPresentation()
	m.ReconcileLayout()
	var rowY int
	for y, id := range m.subagentHoverZone {
		if id == "branch-full-identity" {
			rowY = y
		}
	}
	m.scrollview.SetScrollOffset(rowY)
	m.View()
	visibleY := rowY - m.scrollview.ScrollOffset()
	_, cmd := m.Update(tea.MouseMotionMsg{X: m.xPos + m.layoutCfg.PaddingLeft, Y: m.yPos + visibleY})
	settleSidebarHover(t, m, cmd)
	row, ok := m.placementRowAt(m.layoutCfg.PaddingLeft, visibleY)
	require.True(t, ok)
	require.Greater(t, row.identity.nameEnd, row.identity.nameStart)
	nameX := m.layoutCfg.PaddingLeft + row.identity.nameStart
	_, isName := m.SubagentIdentityAt(nameX, visibleY)
	require.True(t, isName)
	_, isDisclosure := m.subagentDisclosureAt(nameX, visibleY)
	assert.False(t, isDisclosure, "the name remains an identity click, not a disclosure click")
	_, isDisclosure = m.subagentDisclosureAt(m.layoutCfg.PaddingLeft, visibleY)
	assert.True(t, isDisclosure, "the tree prefix remains the independent disclosure hit target")
}

func TestStoppedSubagentBranchesAutoCollapseWithoutOverridingManualChoice(t *testing.T) {
	t.Parallel()
	m := newHoverSidebar(t)
	m.rootSessionID = "collapse"
	snapshot := collapseFixture()
	branch := &snapshot.Nodes[0].Children[0]
	nested := &branch.Children[0]
	branch.Node.State = subagent.NodeIdle
	nested.Node.State = subagent.NodeIdle
	nested.Children[0].Node.State = subagent.NodeIdle
	m.SetSubagentTree(snapshot)
	require.True(t, m.collapsedBranches[branch.Node.ID], "inactive branches start collapsed")

	nested.Node.State = subagent.NodeRunning
	m.SetSubagentTree(snapshot)
	require.False(t, m.collapsedBranches[branch.Node.ID], "running descendants automatically reveal their ancestors")
	assert.Equal(t, 1, activeSubagentCount(m.subagentNodes))
	nested.Node.BackgroundProcesses = 2
	assert.True(t, hasRunningSubagent(m.subagentNodes), "background work on a running node keeps the spinner active")

	nested.Node.State = subagent.NodeStopped
	nested.Children[0].Node.State = subagent.NodeStopped
	m.SetSubagentTree(snapshot)
	require.True(t, m.collapsedBranches[branch.Node.ID], "a stopped auto-opened branch folds after work ends")
	assert.Zero(t, activeSubagentCount(m.subagentNodes), "stopped descendants are not active")
	assert.False(t, hasRunningSubagent(m.subagentNodes), "a stopped node with a stale background count cannot retain the active spinner")

	// Explicit choices take precedence over both auto-open and stopped cleanup.
	nested.Node.State = subagent.NodeRunning
	m.SetSubagentTree(snapshot)
	m.toggleTreeControl(treeControl{id: branch.Node.ID})
	require.True(t, m.collapsedBranches[branch.Node.ID], "manual close overrides running descendants")
	nested.Node.State = subagent.NodeStopped
	m.SetSubagentTree(snapshot)
	require.True(t, m.collapsedBranches[branch.Node.ID], "manual close survives the stopped transition")

	m.toggleTreeControl(treeControl{id: branch.Node.ID})
	require.False(t, m.collapsedBranches[branch.Node.ID])
	nested.Node.State = subagent.NodeRunning
	m.SetSubagentTree(snapshot)
	nested.Node.State = subagent.NodeStopped
	m.SetSubagentTree(snapshot)
	require.False(t, m.collapsedBranches[branch.Node.ID], "manual expansion survives the stopped transition")
}

func activeSubagentCount(nodes []subagent.NodeSnapshot) int {
	count := 0
	for _, node := range nodes {
		if isActiveSubagentState(node.Node.State) {
			count++
		}
		count += activeSubagentCount(node.Children)
	}
	return count
}
