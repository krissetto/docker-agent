package sidebar

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// idleRowText paints the sidebar through the production View path and returns one placed row.
func idleRowText(t *testing.T, m *model, id string) string {
	t.Helper()
	m.View()
	return requirePlaced(t, m, id).text
}

// quiesce drops tick leases whose commands setup discarded, so the next hover start owns the clock.
func quiesce(m *model) {
	m.StopAnimation()
	_ = m.ar.Continue()
}

// hoverFrames runs the shared ramp on real ticks and records the painted row after each frame.
// A lease is scheduled only while the ramp is still running, so none is left dangling.
func hoverFrames(t *testing.T, m *model, cmd tea.Cmd, id string) []string {
	t.Helper()
	frames := []string{idleRowText(t, m, id)}
	running := func() bool { return m.hoverAnimation.IsActive() || m.branchSpansRunning() }
	if !running() {
		return frames
	}
	cmd = hoverTickCommand(t, sidebarOwnerCommand(m, cmd))
	for range 60 {
		require.NotNil(t, cmd, "the shared ramp owns a tick while running")
		tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		m.Update(tick)
		frames = append(frames, idleRowText(t, m, id))
		if !running() {
			return frames
		}
		cmd = m.ar.Continue()
	}
	t.Fatal("hover ramp did not settle")
	return nil
}

// rampFromRest captures the resting row, starts a hover on key, and runs the ramp to its endpoint.
func rampFromRest(t *testing.T, m *model, key, id string) (string, []string) {
	t.Helper()
	quiesce(m)
	rest := idleRowText(t, m, id)
	return rest, hoverFrames(t, m, m.setHoverTarget(key), id)
}

func uniqueFrames(frames []string) int {
	seen := map[string]bool{}
	for _, f := range frames {
		seen[f] = true
	}
	return len(seen)
}

// cellsOfSubstring returns the rendered cells covering needle in line, matched on stripped text.
func cellsOfSubstring(line, needle string) []sidebarCell {
	offset := strings.Index(ansi.Strip(line), needle)
	if offset < 0 {
		return nil
	}
	start := lipgloss.Width(ansi.Strip(line)[:offset])
	cells := sidebarCells(line)
	return cells[start : start+ansi.StringWidth(needle)]
}

// summaryTotal is the production count shown in the subagent summary row.
func summaryTotal(m *model) int {
	total, _, _ := m.treeCounts()
	return total
}

// Subagent and todo summaries rest muted and share one hover ramp, including partial frames from real ticks.
func TestSidebarSummariesRestMutedAndShareOneRamp(t *testing.T) {
	for _, id := range []string{"tree-summary", "todo-summary"} {
		m := newHoverSidebar(t)
		require.NoError(t, m.SetTodos(&tools.ToolCallResult{Meta: []todo.Todo{{ID: "todo", Description: "idle todo", Status: "pending"}}}))
		m.CancelPresentation()
		m.ReconcileLayout()
		require.Contains(t, idleRowText(t, m, "tree-summary"), styles.MutedStyle.Render(fmt.Sprintf("%d subagents", summaryTotal(m))))
		require.Contains(t, idleRowText(t, m, "todo-summary"), styles.MutedStyle.Render("0/1 todos"))

		rest, frames := rampFromRest(t, m, id, id)
		assert.Equal(t, rest, frames[0], "%s rests before pointer entry", id)
		assert.Greater(t, uniqueFrames(frames), 2, "%s passes through partial hover frames", id)
		assert.NotEqual(t, rest, frames[len(frames)-1], "%s brightens at full hover", id)
		left := hoverFrames(t, m, m.setHoverTarget(""), id)
		assert.Equal(t, rest, left[len(left)-1], "%s leave restores the exact rest row", id)
	}
}

// Subagent summary keeps the active count visible; the total rests muted like the active count.
func TestSubagentSummaryRestsMutedWithActiveCount(t *testing.T) {
	m := newHoverSidebar(t)
	m.SetSubagentTree(subagent.Snapshot{Root: "root:hover", Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "root:hover", Agent: "root", State: subagent.NodeIdle}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-full-id", Agent: "worker", State: subagent.NodeRunning}}}}}})
	// Sync counters first, then snap the fade-in to its settled alpha so the rest frame shows the active count.
	m.ReconcileLayout()
	m.CancelPresentation()
	m.ReconcileLayout()
	require.Contains(t, idleRowText(t, m, "tree-summary"), styles.MutedStyle.Render(fmt.Sprintf("%d subagents", summaryTotal(m))))
	_, active, _ := m.treeCounts()
	require.Positive(t, active)
	require.Contains(t, ansi.Strip(idleRowText(t, m, "tree-summary")), fmt.Sprintf("%d active", active))
}

// Clicking the summary expands/collapses it; pointer leave then restores the rest frame exactly.
func TestTreeSummaryClickThenLeaveRestoresRest(t *testing.T) {
	m := newHoverSidebar(t)
	m.CancelPresentation()
	m.ReconcileLayout()
	collapsed := m.treeCollapsed
	rest, frames := rampFromRest(t, m, "tree-summary", "tree-summary")
	require.Greater(t, uniqueFrames(frames), 1)

	clickTreeControl(t, m, "", true)
	require.NotEqual(t, collapsed, m.treeCollapsed, "click toggles the summary")
	m.ReconcileLayout()

	left := hoverFrames(t, m, m.setHoverTarget(""), "tree-summary")
	assert.Empty(t, m.hoverTarget)
	assert.Equal(t, idleRowText(t, m, "tree-summary"), left[len(left)-1])
	_ = rest
	afterLeave := m.View()
	m.CancelHover()
	m.ReconcileLayout()
	require.Equal(t, m.View(), afterLeave, "post-leave frame equals the hover-free frame")
}

// Rendered model, context, and cost segments rest muted, ramp through partial frames on real ticks, and restore on leave.
func TestRenderedModelContextCostHoverRampFromTicks(t *testing.T) {
	for _, c := range []struct{ name, id, key string }{
		{"model", "model:root:0", "model"},
		{"context", "usage:0", "context"},
		{"cost", "usage:0", "cost"},
	} {
		m := newHoverSidebar(t)
		m.SetAgentInfo("root", "openai/gpt-idle", "", 0, "", 0)
		setUsageReadyForTest(m)
		m.CancelPresentation()
		m.ReconcileLayout()

		rest, frames := rampFromRest(t, m, c.key, c.id)
		require.Equal(t, rest, frames[0], "%s rests before pointer entry", c.name)
		assert.Greater(t, uniqueFrames(frames), 2, "%s passes through partial hover frames", c.name)
		assert.NotEqual(t, rest, frames[len(frames)-1], "%s brightens at full hover", c.name)

		left := hoverFrames(t, m, m.setHoverTarget(""), c.id)
		assert.Equal(t, rest, left[len(left)-1], "%s restores the exact rest row after leave", c.name)
	}
}

// A todo summary click toggles the list; leave returns to the exact rest frame without latched hover.
func TestTodoSummaryClickThenLeaveRestsMuted(t *testing.T) {
	m := newHoverSidebar(t)
	require.NoError(t, m.SetTodos(&tools.ToolCallResult{Meta: []todo.Todo{{ID: "todo", Description: "idle todo", Status: "pending"}}}))
	m.CancelPresentation()
	m.ReconcileLayout()

	rest, frames := rampFromRest(t, m, "todo-summary", "todo-summary")
	require.Greater(t, uniqueFrames(frames), 2, "todo summary passes through partial hover frames")
	require.NotEqual(t, rest, frames[len(frames)-1])

	row := requirePlaced(t, m, "todo-summary")
	x := m.layoutCfg.PaddingLeft + m.contentWidth(m.cachedNeedsScrollbar) - 1
	collapsed := m.todosCollapsed
	_, cmd := m.Update(tea.MouseClickMsg{X: x, Y: int(row.y), Button: tea.MouseLeft})
	// Keep the pending tick the click settle returns so the leave ramp owns a live lease.
	pending := settleSidebarHover(t, m, cmd)
	require.NotEqual(t, collapsed, m.todosCollapsed, "click toggles the todo summary")
	m.ReconcileLayout()

	left := hoverFrames(t, m, tea.Batch(pending, m.setHoverTarget("")), "todo-summary")
	assert.Empty(t, m.hoverTarget)
	// The click flips the disclosure glyph, so the restored row must equal the hover-free rendering of the collapsed state.
	afterLeave := left[len(left)-1]
	m.CancelHover()
	m.ReconcileLayout()
	assert.Equal(t, idleRowText(t, m, "todo-summary"), afterLeave, "leave restores the hover-free post-click row")
	assert.NotEqual(t, rest, afterLeave, "the click changes the todo disclosure")
}

// Git expand → collapse → leave with retained keyboard focus returns to the collapsed muted header.
func TestGitExpandCollapseLeaveRetainsMutedHeader(t *testing.T) {
	m := newHoverSidebar(t)
	m.SetGitChanges(GitChanges{Available: true})
	m.CancelPresentation()
	m.ReconcileLayout()
	m.git.focused, m.git.cursor = true, 0
	require.Contains(t, idleRowText(t, m, "git:summary"), styles.MutedStyle.Render("Files - Git"))

	// The chat page routes a header click on the disclosure to ActivateGitLine("0").
	_ = m.ActivateGitLine("0")
	m.ReconcileLayout()
	require.True(t, m.git.open(), "first click expands the section")
	_ = m.ActivateGitLine("0")
	m.ReconcileLayout()
	require.False(t, m.git.open(), "second click collapses the section")
	require.True(t, m.git.focused, "keyboard focus is retained across the click")

	collapsedRest, frames := rampFromRest(t, m, "git:git:summary", "git:summary")
	require.Greater(t, uniqueFrames(frames), 1, "collapsed header passes through partial hover frames")
	assert.NotEqual(t, collapsedRest, frames[len(frames)-1], "hover brightens the collapsed header")

	left := hoverFrames(t, m, m.setHoverTarget(""), "git:summary")
	assert.Equal(t, collapsedRest, left[len(left)-1], "leave restores the collapsed muted header with focus retained")
	assert.NotContains(t, left[len(left)-1], styles.HighlightWhiteStyle.Render("Files - Git"))
}
