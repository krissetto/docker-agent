package sidebar

import (
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
