package sidebar

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestSectionsInitiallyCollapsedAndIndependentlyExpandable(t *testing.T) {
	t.Parallel()
	ar := animation.NewRuntimeWithScheduler(&immediateScheduler{now: time.Unix(1, 0)})
	t.Cleanup(ar.Stop)
	m := New(ar, t.Context(), &service.SessionState{}).(*model)
	require.True(t, m.treeCollapsed)
	require.True(t, m.todosCollapsed)
	snap := subagentSnapshot()
	snap.Nodes[0].Children[0].Node.State = subagent.NodeIdle
	m.SetSubagentTree(snap)
	require.NoError(t, m.SetTodos(makeTodos(2)))
	m.SetSize(60, 50)
	m.CancelPresentation()
	m.View()
	assert.Empty(t, m.subagentHoverZone)
	assert.Equal(t, m.todoSummaryLine+1, m.todoEnd)

	settleTreePresentation(t, m, m.toggleTreeControl(treeControl{whole: true}))
	m.View()
	assert.False(t, m.treeCollapsed)
	assert.True(t, m.todosCollapsed)
	assert.NotEmpty(t, m.subagentHoverZone)
	assert.Empty(t, strings.TrimSpace(ansi.Strip(m.cachedLines[m.summaryLine+1])), "expanded tree retains its breathing row")

	settleTreePresentation(t, m, m.toggleTreeControl(treeControl{whole: true, todos: true}))
	m.View()
	assert.False(t, m.todosCollapsed)
	assert.Empty(t, strings.TrimSpace(ansi.Strip(m.cachedLines[m.todoSummaryLine+1])), "expanded todos retain their breathing row")
	assert.Greater(t, m.todoEnd, m.todoSummaryLine+2)
	m.SetSubagentTree(snap)
	require.NoError(t, m.SetTodos(makeTodos(3)))
	assert.False(t, m.treeCollapsed, "updates preserve user expansion")
	assert.False(t, m.todosCollapsed)

	settleTreePresentation(t, m, m.toggleTreeControl(treeControl{whole: true}))
	settleTreePresentation(t, m, m.toggleTreeControl(treeControl{whole: true, todos: true}))
	assert.True(t, m.treeCollapsed)
	assert.True(t, m.todosCollapsed)
}

func TestCollapsedSubagentSummaryActivity(t *testing.T) {
	t.Parallel()
	for _, state := range []subagent.NodeState{subagent.NodeStarting, subagent.NodeRunning, subagent.NodeIdle, subagent.NodeCompleted, subagent.NodeFailed, subagent.NodeStopped} {
		for _, collapsed := range []bool{false, true} {
			t.Run(string(state)+map[bool]string{false: "/expanded", true: "/collapsed"}[collapsed], func(t *testing.T) {
				ar := animation.NewRuntime()
				t.Cleanup(ar.Stop)
				m := New(ar, t.Context(), &service.SessionState{}).(*model)
				m.treeCollapsed = collapsed
				snap := collapseFixture()
				// Only the grandchild works; idle parents must not hide its activity.
				snap.Nodes[0].Children[0].Children[0].Node.State = state
				m.SetSubagentTree(snap)
				header := m.treeSummary(60)
				frame := m.subagentSpinner.RawFrame()
				if collapsed && state == subagent.NodeRunning {
					assert.True(t, strings.HasPrefix(ansi.Strip(header), "3 subagents") && strings.HasSuffix(ansi.Strip(header), frame+" ›"))
					assert.Contains(t, header, styles.MutedStyle.Render(frame), "header spinner is subdued")
				} else {
					assert.NotContains(t, ansi.Strip(header), frame)
				}
				assert.Equal(t, state == subagent.NodeRunning, m.subagentSpinnerOn)
				for _, width := range []int{1, 2, 8, 20, 60} {
					assert.LessOrEqual(t, ansi.StringWidth(m.treeSummary(width)), width)
				}
			})
		}
	}
}

func TestCollapsedSubagentSummaryUsesExistingParticipantSpinner(t *testing.T) {
	t.Parallel()
	ar := animation.NewRuntimeWithScheduler(&immediateScheduler{now: time.Unix(1, 0)})
	t.Cleanup(ar.Stop)
	m := New(ar, t.Context(), &service.SessionState{}).(*model)
	m.titleGenerated = true
	m.rootSessionID = "main"
	m.sessionStack = []string{"main"}
	m.SetTokenUsage(&runtime.TokenUsageEvent{SessionID: "legacy-child", AgentContext: runtime.AgentContext{AgentName: "worker"}, Usage: &runtime.Usage{ContextLength: 10, ContextLimit: 100}})
	assert.NotContains(t, ansi.Strip(m.treeSummary(60)), m.spinner.RawFrame(), "usage alone is not activity")
	m.Update(&runtime.StreamStartedEvent{SessionID: "legacy-child", AgentContext: runtime.AgentContext{AgentName: "worker"}})
	assert.False(t, m.subagentSpinnerOn, "no extra spinner lease for synchronous work")
	require.EqualValues(t, 1, ar.ActiveCount())
	assert.True(t, strings.HasPrefix(ansi.Strip(m.treeSummary(60)), "1 subagents") && strings.HasSuffix(ansi.Strip(m.treeSummary(60)), m.spinner.RawFrame()+" ›"))
	before := m.treeSummary(60)
	cmd := ar.Continue()
	for range 20 {
		require.NotNil(t, cmd)
		tick, ok := ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		m.Update(tick)
		cmd = ar.Continue()
		if before != m.treeSummary(60) {
			break
		}
	}
	assert.NotEqual(t, before, m.treeSummary(60))
	m.Update(&runtime.StreamStoppedEvent{})
	assert.NotContains(t, ansi.Strip(m.treeSummary(60)), m.spinner.RawFrame())
	settleTreePresentation(t, m, m.ReconcileLayout())
	assert.Zero(t, ar.ActiveCount())
	assert.Nil(t, ar.Continue())
}
