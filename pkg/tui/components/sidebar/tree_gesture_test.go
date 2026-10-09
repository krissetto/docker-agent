package sidebar

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
)

func TestSubagentRowDisclosureScrolledNarrowAndLeaf(t *testing.T) {
	m := newCollapseSidebar(t)
	for _, width := range []int{8, 20, 60} {
		m.SetSize(width, 4)
		m.CancelPresentation()
		m.ReconcileLayout()
		var row int
		for y, id := range m.subagentHoverZone {
			if id == "branch-full-identity" {
				row = y
			}
		}
		m.scrollview.SetScrollOffset(row)
		m.View()
		x := m.layoutCfg.PaddingLeft
		for i := range 2 {
			m.scrollview.SetScrollOffset(row)
			y := row - m.scrollview.ScrollOffset()
			_, cmd := m.Update(tea.MouseClickMsg{X: m.xPos + x, Y: m.yPos + y, Button: tea.MouseLeft})
			settleTreePresentation(t, m, cmd)
			require.Equal(t, i == 0, m.collapsedBranches["branch-full-identity"], "indent toggles the scrolled row without requiring hover")
		}
		if width == 60 {
			y := row - m.scrollview.ScrollOffset()
			line := strings.Split(ansi.Strip(m.View()), "\n")[y]
			before, _, found := strings.Cut(line, "planner")
			require.True(t, found)
			node, ok := m.SubagentIdentityAt(ansi.StringWidth(before), y)
			require.True(t, ok)
			require.Equal(t, subagent.NodeID("branch-full-identity"), node.ID)
			beforeNameClick := m.collapsedBranches[node.ID]
			m.Update(tea.MouseClickMsg{X: m.xPos + ansi.StringWidth(before), Y: m.yPos + y, Button: tea.MouseLeft})
			require.Equal(t, beforeNameClick, m.collapsedBranches[node.ID], "name never toggles disclosure in the component")
			_, ok = m.SubagentIdentityAt(m.layoutCfg.PaddingLeft+m.contentWidth(m.cachedNeedsScrollbar)-1, y)
			require.False(t, ok, "right status does not become an attachment identity")
		}
	}
	m.SetSize(60, 70)
	m.CancelPresentation()
	m.ReconcileLayout()
	var leafRow int
	for row, id := range m.subagentHoverZone {
		if id == "leaf-full-identity" {
			leafRow = row
		}
	}
	m.scrollview.SetScrollOffset(leafRow)
	m.Update(tea.MouseClickMsg{X: m.xPos + m.layoutCfg.PaddingLeft + 2, Y: m.yPos + leafRow - m.scrollview.ScrollOffset(), Button: tea.MouseLeft})
	require.NotContains(t, m.collapsedBranches, subagent.NodeID("leaf-full-identity"), "leaf rows have no branch state")
}

func TestSubagentNameHoverOnlyWithinRenderedName(t *testing.T) {
	for _, width := range []int{12, 20, 60} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := newCollapseSidebar(t)
			snapshot := collapseFixture()
			snapshot.Nodes[0].Children[0].Node.Name = "作業planner"
			m.SetSubagentTree(snapshot)
			m.SetSize(width, 4)
			m.CancelPresentation()
			m.ReconcileLayout()
			var contentY int
			for y, id := range m.subagentHoverZone {
				if id == "branch-full-identity" {
					contentY = y
				}
			}
			m.scrollview.SetScrollOffset(contentY)
			m.View()
			y := contentY - m.scrollview.ScrollOffset()
			motion := func(x int) {
				t.Helper()
				_, cmd := m.Update(tea.MouseMotionMsg{X: m.xPos + x, Y: m.yPos + y})
				settleSidebarHover(t, m, cmd)
				m.ReconcileLayout()
			}
			motion(m.layoutCfg.PaddingLeft)
			row, ok := m.placementRowAt(m.layoutCfg.PaddingLeft, y)
			require.True(t, ok)
			require.Greater(t, row.identity.nameEnd, row.identity.nameStart)
			nameKey := "node-name:branch-full-identity"
			require.Zero(t, m.hoverValues[nameKey].value, "indent leaves the name unhighlighted")
			require.Equal(t, 1.0, m.branchSpans["branch-full-identity"].idValue, "indent still reveals the ID")
			motion(m.layoutCfg.PaddingLeft + row.identity.nameStart)
			require.Equal(t, 1.0, m.hoverValues[nameKey].value)
			for _, col := range []int{row.identity.nameStart - 1, row.identity.nameEnd, m.contentWidth(m.cachedNeedsScrollbar) - 1} {
				motion(m.layoutCfg.PaddingLeft + col)
				require.Zero(t, m.hoverValues[nameKey].value, "non-name column %d", col)
				require.Equal(t, "node:branch-full-identity", m.hoverTarget)
				require.Equal(t, 1.0, m.branchSpans["branch-full-identity"].idValue)
			}
			row, ok = m.placementRowAt(m.layoutCfg.PaddingLeft, y)
			require.True(t, ok)
			if width == 60 {
				before, _, found := strings.Cut(ansi.Strip(row.text), "(branch-full-identity)")
				require.True(t, found)
				idX := m.layoutCfg.PaddingLeft + ansi.StringWidth(before)
				motion(idX)
				_, linked := m.SubagentIdentityAt(idX, y)
				require.False(t, linked, "visible ID is disclosure-only")
				require.Zero(t, m.hoverValues[nameKey].value, "visible ID leaves the name unhighlighted")
				_, cmd := m.Update(tea.MouseClickMsg{X: m.xPos + m.layoutCfg.PaddingLeft, Y: m.yPos + y, Button: tea.MouseLeft})
				settleTreePresentation(t, m, cmd)
				y = contentY - m.scrollview.ScrollOffset()
				motion(m.layoutCfg.PaddingLeft)
				row, ok = m.placementRowAt(m.layoutCfg.PaddingLeft, y)
				require.True(t, ok)
				motion(m.layoutCfg.PaddingLeft + row.identity.nameEnd + 1)
				require.Zero(t, m.hoverValues[nameKey].value, "count leaves the name unhighlighted")
				require.True(t, m.collapsedBranches["branch-full-identity"])
			}
			for _, control := range row.controls {
				motion(m.layoutCfg.PaddingLeft + control.x)
				require.Zero(t, m.hoverValues[nameKey].value, "chevron leaves the name unhighlighted")
				require.Equal(t, "node:branch-full-identity", m.hoverTarget)
			}
			motion(m.layoutCfg.PaddingLeft + row.identity.nameStart)
			require.Equal(t, 1.0, m.hoverValues[nameKey].value, "name reentry restores its highlight")
			settleSidebarHover(t, m, m.ClearSubagentHover())
			require.Empty(t, m.hoverValues)
		})
	}
}
