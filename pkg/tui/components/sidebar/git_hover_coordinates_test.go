package sidebar

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/review"
)

func TestGitHoverHitGeometryAcrossDocksAndScroll(t *testing.T) {
	for _, leftDock := range []bool{false, true} {
		t.Run(fmt.Sprintf("left-dock=%t", leftDock), func(t *testing.T) {
			m := gitSidebar(t)
			m.scrollview.SetAnimationRuntime(nil)
			m.SetMode(ModeVertical)
			m.SetPosition(7, 3)
			m.SetSize(44, 9)
			m.SetMirroredPadding(leftDock)

			files := []review.FileChange{
				{Path: "src/first.go", Group: review.GroupStaged, Status: "M", Added: 2},
				{Path: "src/second.go", Group: review.GroupStaged, Status: "M", Removed: 1},
				{Path: "src/third.go", Group: review.GroupStaged, Status: "A", Added: 5},
			}
			snapshot := &review.Snapshot{Repositories: []review.Repository{{
				ID: "repo", Name: "repo", Worktrees: []review.Worktree{{
					ID: "wt", Branch: "main", State: review.StateReady,
					CountsState: review.StateReady, Files: files,
				}},
			}}}
			m.SetGitChanges(GitChanges{Available: true, Snapshot: snapshot})
			m.git.expanded = map[string]bool{
				gitSectionKey: true,
				"repo:repo":   true,
				"wt:wt":       true,
				"grp:wt:2":    true,
			}
			m.git.rebuild()
			m.invalidateCache()
			m.ReconcileLayout()
			m.CancelPresentation()
			m.ReconcileLayout()
			m.View()

			var fileRows []placedRow
			for _, row := range m.placement.rows {
				if row.action == ClickGit && strings.HasPrefix(row.id, "git:file:") {
					fileRows = append(fileRows, row)
				}
			}
			require.Len(t, fileRows, 3)
			first, second := fileRows[0], fileRows[1]
			m.scrollview.SetScrollOffset(int(first.targetY) - 2)
			offset := m.scrollview.ScrollOffset()
			require.Greater(t, offset, 0, "fixture exercises scrolled row coordinates")
			firstY, secondY := int(first.targetY)-offset, int(second.targetY)-offset
			require.GreaterOrEqual(t, firstY, 0)
			require.Less(t, secondY, m.viewportHeight())

			beforeFirst, beforeSecond := first.text, second.text
			x := m.xPos + m.layoutCfg.PaddingLeft + 2
			_, cmd := m.Update(tea.MouseMotionMsg{X: x, Y: m.yPos + firstY})
			require.Equal(t, "git:"+first.id, m.hoverTarget, "Git row joins shared hover channel")
			require.Equal(t, int(first.targetY)-m.gitStart, m.git.hovered, "the hovered Git row is tracked in section coordinates")
			settleSidebarHover(t, m, cmd)
			m.View()
			firstPainted := requirePlaced(t, m, first.id)
			secondPainted := requirePlaced(t, m, second.id)
			require.NotEqual(t, beforeFirst, firstPainted.text, "pointer coordinates must hit the first file on either dock")
			require.Equal(t, beforeSecond, secondPainted.text, "adjacent scrolled file remains unhovered")
			require.NotContains(t, firstPainted.text, "48;2;", "Git hover uses shared foreground emphasis, not a filled background")
			require.NotEqual(t, beforeFirst, firstPainted.text, "the hovered file receives the shared hover treatment")

			_, cmd = m.Update(tea.MouseMotionMsg{X: x, Y: m.yPos + secondY})
			settleSidebarHover(t, m, cmd)
			m.View()
			firstPainted = requirePlaced(t, m, first.id)
			secondPainted = requirePlaced(t, m, second.id)
			require.Equal(t, beforeFirst, firstPainted.text, "moving hover restores the previous file")
			require.NotEqual(t, beforeSecond, secondPainted.text, "second scrolled file becomes hovered")

			_, cmd = m.Update(tea.MouseMotionMsg{X: -1, Y: -1})
			settleSidebarHover(t, m, cmd)
			m.View()
			require.Equal(t, beforeFirst, requirePlaced(t, m, first.id).text, "leaving the sidebar clears row hover")
			require.Equal(t, beforeSecond, requirePlaced(t, m, second.id).text)
		})
	}
}
