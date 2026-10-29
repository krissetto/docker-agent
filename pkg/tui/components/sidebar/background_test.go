package sidebar

import (
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func backgroundFixture(id, toolCallID string, state tools.BackgroundProcessState, started, finished time.Time) tools.BackgroundProcess {
	return tools.BackgroundProcess{
		ID: id, SessionID: "bg-session", AgentName: "worker", ToolCallID: toolCallID,
		Command: "printf 'first line'\necho hidden", StartedAt: started, FinishedAt: finished, State: state,
	}
}

func backgroundSidebar(t *testing.T) (*model, *placementClock) {
	t.Helper()
	clock := &placementClock{now: time.Unix(1, 0), step: animation.Chat.DefaultFrameDuration()}
	ar := animation.NewRuntimeWithScheduler(clock)
	state := &service.SessionState{}
	m := New(ar, testContext(t), state).(*model)
	t.Cleanup(m.StopAnimation)
	m.rootSessionID = "bg-session"
	m.backgroundNowFunc = func() time.Time { return clock.now }
	m.SetSize(64, 45)
	m.View()
	return m, clock
}

func TestBackgroundProcessesPresentationOrderingAndActions(t *testing.T) {
	t.Parallel()
	m, _ := backgroundSidebar(t)
	started := time.Unix(10, 0)
	processes := []tools.BackgroundProcess{
		backgroundFixture("later", "call-later", tools.BackgroundProcessCompleted, started.Add(time.Minute), time.Time{}),
		backgroundFixture("first", "call-first", tools.BackgroundProcessRunning, started, time.Time{}),
		backgroundFixture("tie-b", "call-tie-b", tools.BackgroundProcessStopped, started, time.Time{}),
		backgroundFixture("tie-a", "call-tie-a", tools.BackgroundProcessFailed, started, time.Time{}),
	}
	processes[0].Command = "echo later"
	processes[2].Command = "echo tie-b"
	processes[3].Command = "echo tie-a"
	cmd := m.SetBackgroundProcesses("bg-session", processes)
	settleTreePresentation(t, m, cmd)
	out := ansi.Strip(strings.Join(m.renderSections(m.contentWidth(false)), "\n"))
	assert.Contains(t, out, "Background")
	assert.Less(t, strings.Index(out, "first line"), strings.Index(out, "tie-a"), "rows use start time then process ID ordering")
	assert.Contains(t, out, "✓")
	assert.Contains(t, out, "■")
	assert.Contains(t, out, "✗")
	assert.NotContains(t, out, "echo hidden", "only the first command line is shown")

	require.GreaterOrEqual(t, m.backgroundStart, 0)
	result, target := m.HandleClickType(m.layoutCfg.PaddingLeft+3, m.backgroundStart+1)
	assert.Equal(t, ClickBackgroundProcess, result, "placed hit geometry exposes process clicks")
	assert.Equal(t, "call-first", target, "navigation is keyed by ToolCallID, not process ID")
	m.placement = nil
	result, target = m.HandleClickType(m.layoutCfg.PaddingLeft+3, m.backgroundStart+1)
	assert.Equal(t, ClickBackgroundProcess, result, "continuous pane hit geometry exposes process clicks")
	assert.Equal(t, "call-first", target)
	for index := range m.backgroundProcesses {
		if m.backgroundProcesses[index].ID == "first" {
			m.backgroundProcesses[index].ToolCallID = ""
			m.backgroundProcessesByID["first"] = m.backgroundProcesses[index]
			rowIndex := index
			m.backgroundRowTools[rowIndex] = ""
		}
	}
	result, target = m.HandleClickType(m.layoutCfg.PaddingLeft+3, m.backgroundStart+1)
	assert.NotEqual(t, ClickBackgroundProcess, result)
	assert.Empty(t, target)
}

func TestBackgroundProcessesAreScopedToOwningSession(t *testing.T) {
	t.Parallel()
	m, _ := backgroundSidebar(t)
	other := backgroundFixture("foreign", "call-foreign", tools.BackgroundProcessRunning, time.Now(), time.Time{})
	other.SessionID = "subsession"
	m.SetBackgroundProcesses("subsession", []tools.BackgroundProcess{other})
	assert.Empty(t, m.backgroundProcesses, "the root sidebar does not aggregate child-session jobs")
	m.rootSessionID = "subsession"
	m.SetBackgroundProcesses("subsession", []tools.BackgroundProcess{other})
	assert.Len(t, m.backgroundProcesses, 1, "an attached page owns and displays its session's jobs")
}

func TestBackgroundProcessesInitialCapAndDisclosureChoice(t *testing.T) {
	t.Parallel()
	m, _ := backgroundSidebar(t)
	started := time.Unix(10, 0)
	processes := make([]tools.BackgroundProcess, 8)
	for i := range processes {
		processes[i] = backgroundFixture(string(rune('a'+i)), "call", tools.BackgroundProcessRunning, started.Add(time.Duration(i)*time.Second), time.Time{})
	}
	m.SetBackgroundProcesses("bg-session", processes)
	m.backgroundCollapsed = false
	m.backgroundManual = true
	out := ansi.Strip(m.backgroundInfo(40))
	assert.Contains(t, out, "… +3 more")
	assert.Len(t, m.backgroundRowIDs, backgroundInitialRows)

	m.backgroundShowAll = true
	m.SetBackgroundProcesses("bg-session", processes[:7])
	assert.True(t, m.backgroundShowAll, "same-session updates preserve the disclosure choice")
	assert.True(t, m.backgroundManual, "same-session snapshots preserve manual section choice")
	assert.False(t, m.backgroundCollapsed)

	m.SetBackgroundProcesses("bg-session", nil)
	assert.Empty(t, m.backgroundProcesses, "empty replacement clears immediately")
	assert.True(t, m.backgroundManual, "empty snapshot does not erase the user's per-session choice")
}

func TestBackgroundProcessesGraceAndHiddenResume(t *testing.T) {
	t.Parallel()
	m, clock := backgroundSidebar(t)
	clock.now = time.Unix(100, 0)
	m.backgroundNowFunc = func() time.Time { return clock.now }
	m.backgroundCollapsed = false
	m.SetBackgroundProcesses("bg-session", []tools.BackgroundProcess{
		backgroundFixture("done", "call-done", tools.BackgroundProcessCompleted, clock.now.Add(-time.Minute), clock.now),
	})
	assert.Len(t, m.backgroundProcesses, 1)
	m.SetPresentationActive(false)
	clock.now = clock.now.Add(backgroundGrace + time.Millisecond)
	m.SetPresentationActive(true)
	assert.Empty(t, m.backgroundProcesses, "a hidden sidebar prunes an elapsed grace period when it resumes")
}

func TestBackgroundProcessNoopSnapshotAndAnimationStaySectionScoped(t *testing.T) {
	m, clock := backgroundSidebar(t)
	m.scrollview.SetAnimationRuntime(nil)
	clock.now = time.Unix(200, 0)
	m.backgroundNowFunc = func() time.Time { return clock.now }
	m.backgroundCollapsed = false
	m.SetBackgroundProcesses("bg-session", []tools.BackgroundProcess{
		backgroundFixture("run", "call-run", tools.BackgroundProcessRunning, clock.now, time.Time{}),
	})
	m.View()

	processes := []tools.BackgroundProcess{m.backgroundProcesses[0]}
	allocs := testing.AllocsPerRun(100, func() { require.Nil(t, m.SetBackgroundProcesses("bg-session", processes)) })
	assert.LessOrEqual(t, allocs, 0.1, "identical snapshots allocate nothing")

	// Add a wide, unrelated prepared tree to catch per-tick full-tree/placement scans.
	tree := subagent.Snapshot{Root: subagent.SessionRootID("bg-session"), Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: subagent.SessionRootID("bg-session"), Agent: "root", State: subagent.NodeIdle}}}}
	for i := range 1000 {
		tree.Nodes[0].Children = append(tree.Nodes[0].Children, subagent.NodeSnapshot{Node: subagent.Node{ID: subagent.NodeID(fmt.Sprintf("tree-%04d", i)), Agent: "worker", State: subagent.NodeIdle, CreatedAt: time.Unix(int64(i), 0)}})
	}
	m.treeCollapsed = false
	m.SetSubagentTree(tree)
	m.renderSections(m.contentWidth(false))
	m.ReconcileLayout()
	m.CancelPresentation()
	m.ReconcileLayout()
	m.scrollview.SetScrollOffset(m.backgroundStart)
	m.syncBackgroundAnimation()
	m.View()
	require.True(t, m.backgroundAnimation.IsActive(), "visible background section owns an animation lease")
	m.View() // sync agent/theme state before the measured update
	beforeTreeRows := m.placement.treeRowRefreshes
	before := make(map[string]uint64, len(m.sectionRenderCounts))
	maps.Copy(before, m.sectionRenderCounts)
	beforeInvalidations, _ := m.CacheStats()
	beforeRowRenders := m.backgroundRowRenders
	clock.now = clock.now.Add(clock.step)
	// Each accepted tick crosses exactly one known Chat frame.
	beforeFrame := m.backgroundFrame
	cmd := m.ar.Continue()
	require.NotNil(t, cmd)
	delivered, ok := m.ar.Accept(cmd().(animation.TickMsg))
	require.True(t, ok)
	m.Update(delivered)
	updatedInvalidations, _ := m.CacheStats()
	assert.Equal(t, beforeInvalidations, updatedInvalidations, "a running process tick doesn't invalidate the sidebar cache")
	assert.NotEqual(t, beforeFrame, m.backgroundFrame, "the test tick advances a visible background spinner frame")
	m.View()
	for section, count := range before {
		if section != "background" {
			assert.Equal(t, count, m.sectionRenderCounts[section], "background animation does not render unrelated section %q", section)
		}
	}
	assert.Greater(t, m.backgroundRowRenders, beforeRowRenders, "the visible process row updates on a spinner-frame change")
	assert.Equal(t, beforeTreeRows, m.placement.treeRowRefreshes, "background motion does not refresh unrelated tree rows")
}

func TestBackgroundProcessesResumeKeepsRunningJobs(t *testing.T) {
	t.Parallel()
	m, clock := backgroundSidebar(t)
	clock.now = time.Unix(500, 0)
	m.backgroundNowFunc = func() time.Time { return clock.now }
	running := backgroundFixture("live", "call-live", tools.BackgroundProcessRunning, clock.now.Add(-time.Second), time.Time{})
	finished := backgroundFixture("done", "call-done", tools.BackgroundProcessCompleted, clock.now.Add(-time.Minute), clock.now)
	m.SetBackgroundProcesses("bg-session", []tools.BackgroundProcess{running, finished})
	require.Equal(t, 1, m.backgroundRunningCount)
	m.SetPresentationActive(false)
	clock.now = clock.now.Add(backgroundGrace + time.Millisecond)
	m.SetPresentationActive(true)
	require.Len(t, m.backgroundProcesses, 1)
	assert.Equal(t, "live", m.backgroundProcesses[0].ID)
	assert.Equal(t, 1, m.backgroundRunningCount, "pruning terminal rows preserves live process activity")
	assert.True(t, m.backgroundAnimation.IsActive(), "the surviving visible running row resumes its spinner")
}

func TestBackgroundCollapsedHeaderHoverDoesNotMutateGitRows(t *testing.T) {
	t.Parallel()
	m, _ := backgroundSidebar(t)
	m.git.expanded = map[string]bool{gitSectionKey: true}
	m.SetGitChanges(GitChanges{Available: true, Snapshot: gitSnapshot(review.StateUnscanned)})
	m.backgroundCollapsed = true
	m.SetBackgroundProcesses("bg-session", []tools.BackgroundProcess{backgroundFixture("live", "call", tools.BackgroundProcessRunning, time.Now(), time.Time{})})
	m.placement = nil
	lines := m.renderSections(m.contentWidth(false))
	require.Greater(t, m.gitStart, -1)
	before := append([]string(nil), lines[m.gitStart:m.gitEnd]...)
	_ = m.updateRegionHover(m.layoutCfg.PaddingLeft+2, m.backgroundHeader)
	m.hoverValues["background:header"] = hoverValue{value: 1, target: 1}
	m.refreshBackgroundMotion()
	assert.Equal(t, before, m.cachedLines[m.gitStart:m.gitEnd], "a collapsed header hover must not patch the following Git section")
	assert.NotEmpty(t, m.backgroundLines)
}

func TestBackgroundCollapsedSummaryAndNarrowLayout(t *testing.T) {
	t.Parallel()
	m, _ := backgroundSidebar(t)
	m.SetBackgroundProcesses("bg-session", []tools.BackgroundProcess{backgroundFixture("live", "call", tools.BackgroundProcessRunning, time.Now(), time.Time{})})
	m.SetMode(ModeCollapsed)
	assert.Contains(t, ansi.Strip(m.backgroundSummaryCollapsed()), "background")
	assert.Contains(t, ansi.Strip(m.backgroundSummaryCollapsed()), animation.Chat.FrameAt(m.ar.Now()))
	m.SetMode(ModeVertical)
	m.SetSize(20, 30)
	view := m.View()
	for line := range strings.SplitSeq(view, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 20)
	}
}

func TestBackgroundDisclosureFollowsStreamUnlessManuallyChosen(t *testing.T) {
	t.Parallel()
	m, clock := backgroundSidebar(t)
	m.SetBackgroundProcesses("bg-session", []tools.BackgroundProcess{backgroundFixture("live", "call", tools.BackgroundProcessRunning, clock.now, time.Time{})})
	assert.False(t, m.backgroundCollapsed, "idle snapshots default open")
	m.Update(&runtime.StreamStartedEvent{SessionID: "bg-session", AgentName: "root"})
	assert.True(t, m.backgroundCollapsed, "default section collapses while the stream is active")
	m.Update(&runtime.StreamStoppedEvent{SessionID: "bg-session"})
	assert.False(t, m.backgroundCollapsed, "default section opens when the stream returns idle")
	m.backgroundManual = true
	m.backgroundCollapsed = true
	m.Update(&runtime.StreamStartedEvent{SessionID: "bg-session", AgentName: "root"})
	m.Update(&runtime.StreamStoppedEvent{SessionID: "bg-session"})
	assert.True(t, m.backgroundCollapsed, "manual disclosure is respected for the session")
}

func TestBackgroundDisclosurePreservedOnSameSessionLoad(t *testing.T) {
	t.Parallel()
	m, clock := backgroundSidebar(t)
	process := backgroundFixture("live", "call", tools.BackgroundProcessRunning, clock.now, time.Time{})
	m.SetBackgroundProcesses("bg-session", []tools.BackgroundProcess{process})
	m.backgroundManual = true
	m.backgroundCollapsed = true
	m.backgroundShowAll = true

	m.LoadFromSession(session.New(session.WithID("bg-session")))
	assert.True(t, m.backgroundManual)
	assert.True(t, m.backgroundCollapsed)
	assert.True(t, m.backgroundShowAll)
	m.SetBackgroundProcesses("bg-session", []tools.BackgroundProcess{process})
	assert.True(t, m.backgroundManual, "a same-session snapshot retains explicit disclosure after reload")
	assert.True(t, m.backgroundCollapsed)
	assert.True(t, m.backgroundShowAll)

	m.LoadFromSession(session.New(session.WithID("other")))
	assert.False(t, m.backgroundManual, "a new session resets the prior session's choice")
	assert.False(t, m.backgroundShowAll)
}

func TestBackgroundThemeChangeDropsPreparedRowStyle(t *testing.T) {
	t.Parallel()
	m, clock := backgroundSidebar(t)
	m.backgroundCollapsed = false
	m.SetBackgroundProcesses("bg-session", []tools.BackgroundProcess{backgroundFixture("live", "call", tools.BackgroundProcessRunning, clock.now, time.Time{})})
	m.backgroundInfo(40)
	require.NotEmpty(t, m.backgroundRowCache)
	before := m.sectionRenderCounts["background"]
	m.Update(messages.ThemeChangedMsg{})
	assert.Greater(t, m.sectionRenderCounts["background"], before, "theme changes rebuild styled process rows")
	assert.Contains(t, ansi.Strip(m.backgroundInfo(40)), "Background")
}

func TestBackgroundCommandSanitization(t *testing.T) {
	t.Parallel()
	got := sanitizeBackgroundCommand("printf ok\x1b[31m\x07\u009b\nsecond")
	assert.Equal(t, "printf ok[31m", got)
}

func TestIdleSubagentBackgroundActivityUsesInlineSpinner(t *testing.T) {
	t.Parallel()
	m, _ := backgroundSidebar(t)
	m.treeCollapsed = false
	m.subagentSpinner = &fakeSpinner{m.subagentSpinner}
	snapshot := subagent.Snapshot{
		Root: subagent.SessionRootID("bg-session"),
		Nodes: []subagent.NodeSnapshot{{
			Node:     subagent.Node{ID: subagent.SessionRootID("bg-session"), Agent: "root", State: subagent.NodeIdle},
			Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "worker-node", Agent: "worker", State: subagent.NodeIdle}}},
		}},
	}
	m.SetSubagentTree(snapshot)
	idleText := m.subagentsInfo(60)
	assert.Len(t, m.subagentNodes, 1)
	assert.Contains(t, idleText, "worker")
	assert.NotContains(t, ansi.Strip(idleText), "background")

	snapshot.Nodes[0].Children[0].Node.BackgroundProcesses = 2
	m.SetSubagentTree(snapshot)
	m.subagentsInfo(60)
	assert.True(t, m.subagentSpinnerOn, "background work is animated without changing the child lifecycle state")
	rowIndex, ok := m.preparedTrees[60].rowByID["worker-node"]
	require.True(t, ok)
	row := m.preparedTrees[60].rows[rowIndex]
	assert.Equal(t, subagent.NodeIdle, row.node.State)
	assert.Contains(t, ansi.Strip(row.text), "2 background")
	assert.NotEmpty(t, m.subagentGlyph(row.node))

	snapshot.Nodes[0].Children[0].Node.BackgroundProcesses = 0
	m.SetSubagentTree(snapshot)
	m.subagentsInfo(60)
	assert.False(t, m.subagentSpinnerOn)
	rowIndex, ok = m.preparedTrees[60].rowByID["worker-node"]
	require.True(t, ok)
	assert.NotContains(t, ansi.Strip(m.preparedTrees[60].rows[rowIndex].text), "background")
}
