package sidebar

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
)

// timeAgo reads the real clock (no injection), so the age sits 30s inside its
// minute: "10m ago" holds for the test duration without flaking at a boundary.
// Hover reveal may replace a name with its ID and age, but the name's hit span
// is the resting one. Narrow rows keep the age and a bounded ID; wide rows keep
// the full name beside the full ID.
func TestSubagentHoverRevealKeepsRestingNameTargetAndWideFullName(t *testing.T) {
	node := subagent.Node{ID: "grand-full-canonical-node", SessionID: "grand", Agent: "reviewer", Name: "NestedReviewer", State: subagent.NodeCompleted, CreatedAt: time.Now().Add(-10*time.Minute - 30*time.Second)}
	for _, width := range []int{20, 120} {
		m := newSubagentTestModel(t)
		restText, resting := m.subagentRowIdentity(node, "", width, -1, false)
		m.hoveredSubagent = node.ID
		m.branchSpans = map[subagent.NodeID]branchSpans{node.ID: {idValue: 1, idTarget: 1}}
		text, hovered := m.subagentRowIdentity(node, "", width, -1, false)
		require.Equal(t, resting, hovered, "width %d: hover reveal cannot move the name hit span", width)
		require.Equal(t, width, ansi.StringWidth(text), "width %d: hovered row keeps its width", width)

		// The resting span is exactly what the production truncator shows for its
		// width, read from the rendered unrevealed row rather than a re-derived budget.
		span := resting.nameEnd - resting.nameStart
		require.Equal(t, toolcommon.TruncateText(node.Name, span), ansi.Cut(ansi.Strip(restText), resting.nameStart, resting.nameEnd), "width %d: resting name span", width)

		plain := ansi.Strip(text)
		require.Contains(t, plain, "10m ago", "width %d: hover keeps the node's age", width)
		require.Contains(t, plain, "(grand-full", "width %d: hover keeps the canonical ID prefix", width)
		if width == 120 {
			require.Contains(t, plain, node.Name, "wide hover keeps the full name")
			require.Contains(t, plain, "(grand-full-canonical-node)", "wide hover reveals the full canonical ID")
		}
	}
}

// The root BranchToggle fixture (name, ID, agent, idle state, zero CreatedAt) at
// the sidebar content widths a 120-column root produces, plus wide widths. The
// painted disclosure glyph must sit on the recorded hit cell at every width;
// hover may move that cell only where the ID needs the name's cells (logged).
func TestSubagentBranchControlPaintMatchesHitCell(t *testing.T) {
	node := subagent.Node{ID: "branch-full-node", SessionID: "branch-child", Agent: "worker", Name: "BranchWorker", State: subagent.NodeIdle}
	const row = 7
	for _, width := range []int{33, 34, 35, 36, 37, 38, 60, 120} {
		m := newSubagentTestModel(t)
		restText, _ := m.subagentRowIdentity(node, "", width, row, true)
		require.Len(t, m.treeControls[row], 1, "width %d: resting branch has one disclosure control", width)
		resting := m.treeControls[row][0]

		m.treeControls = make(map[int][]treeControl)
		m.hoveredSubagent = node.ID
		m.hoverTarget = "node:" + string(node.ID)
		m.branchSpans = map[subagent.NodeID]branchSpans{node.ID: {idValue: 1, idTarget: 1}}
		text, _ := m.subagentRowIdentity(node, "", width, row, true)
		require.Len(t, m.treeControls[row], 1, "width %d: hovered branch keeps one disclosure control", width)
		hovered := m.treeControls[row][0]
		plain := ansi.Strip(text)
		painted := strings.Index(plain, "⌄")
		require.GreaterOrEqual(t, painted, 0, "width %d: hover paints the disclosure glyph; row=%q", width, plain)
		require.Equal(t, ansi.StringWidth(plain[:painted]), hovered.x, "width %d: painted disclosure cell equals hit cell; row=%q", width, plain)
		require.Equal(t, node.ID, hovered.id, "width %d: disclosure cell still addresses its node", width)
		t.Logf("CONTROL width=%d resting_x=%d hovered_x=%d resting_row=%q hovered_row=%q", width, resting.x, hovered.x, ansi.Strip(restText), plain)
		if width == 60 || width == 120 {
			require.Equal(t, resting.x, hovered.x, "width %d: hover reveal cannot move the disclosure cell", width)
		}
	}
}
