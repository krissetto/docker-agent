package sidebar

import (
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/stretchr/testify/require"
)

func refreshSubsetTreeSnapshot(total int) subagent.Snapshot {
	rootID := subagent.NodeID("root:hover")
	root := subagent.NodeSnapshot{Node: subagent.Node{ID: rootID, Agent: "root", State: subagent.NodeIdle}}
	remaining := total
	for top := 0; top < 10 && remaining > 0; top++ {
		id := subagent.NodeID(fmt.Sprintf("branch-%03d", top))
		branch := subagent.NodeSnapshot{Node: subagent.Node{ID: id, Agent: "worker", State: subagent.NodeIdle, Parent: rootID, SessionID: "session"}}
		for child := 0; child < 10 && remaining > 1; child++ {
			childID := subagent.NodeID(fmt.Sprintf("leaf-%03d-%03d", top, child))
			branch.Children = append(branch.Children, subagent.NodeSnapshot{Node: subagent.Node{ID: childID, Agent: "leaf 界面", State: subagent.NodeIdle, Parent: id, SessionID: "session", CreatedAt: time.Unix(1, 0)}})
			remaining--
		}
		root.Children = append(root.Children, branch)
		remaining--
	}
	for remaining > 0 {
		top := len(root.Children)
		id := subagent.NodeID(fmt.Sprintf("branch-%03d", top))
		root.Children = append(root.Children, subagent.NodeSnapshot{Node: subagent.Node{ID: id, Agent: "worker", State: subagent.NodeIdle, Parent: rootID, SessionID: "session"}})
		remaining--
	}
	return subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{root}}
}

type subsetClock struct{ now time.Time }

func (c *subsetClock) Now() time.Time { return c.now }
func (c *subsetClock) Tick(d time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { c.now = c.now.Add(d); return f(c.now) }
}

func refreshSubsetSidebar(t *testing.T, snapshot subagent.Snapshot) *model {
	t.Helper()
	ar := animation.NewRuntimeWithScheduler(&subsetClock{now: time.Unix(1, 0)})
	m := New(ar, t.Context(), &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimation)
	m.rootSessionID = "hover"
	m.treeCollapsed = false
	m.SetMode(ModeVertical)
	m.SetSize(40, 12)
	m.SetSubagentTree(snapshot)
	testOpenAllSubagentBranches(m)
	m.ReconcileLayout()
	m.CancelPresentation()
	m.ReconcileLayout()
	m.View()
	settleSubsetPlacement(t, m)
	return m
}

func settleSubsetPlacement(t *testing.T, m *model) {
	t.Helper()
	for range 50 {
		if m.placement == nil || !m.placement.running {
			return
		}
		cmd := m.ar.Continue()
		require.NotNil(t, cmd)
		tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		m.Update(tick)
	}
	t.Fatal("placement animation did not settle")
}

func moveRefreshSubsetPointer(t *testing.T, m *model, id subagent.NodeID, name bool) tea.Cmd {
	t.Helper()
	index, ok := m.placement.treeNodeRows[id]
	require.True(t, ok, "target node has a placement index")
	row := m.placement.rows[index]
	m.scrollview.SetScrollOffset(max(0, int(row.targetY)-m.viewportHeight()/2))
	m.View()
	m.placement.treeRowRefreshes = 0
	x := m.layoutCfg.PaddingLeft + 4
	if name {
		x = m.layoutCfg.PaddingLeft + row.identity.nameStart
	}
	y := int(row.targetY) - m.scrollview.ScrollOffset()
	_, cmd := m.Update(tea.MouseMotionMsg{X: x, Y: y})
	return cmd
}

func assertRefreshSubsetFullParity(t *testing.T, m *model) {
	t.Helper()
	fast := m.View()
	index := m.placement.treeNodeRows["child-full-id"]
	var before string
	if index < len(m.placement.rows) {
		before = ansi.Strip(m.placement.rows[index].text)
	}
	m.refreshPlacementPresentation()
	full := m.View()
	if fast != full && index < len(m.placement.rows) {
		item := m.preparedTrees[m.placement.width].rows[m.preparedTrees[m.placement.width].rowByID["child-full-id"]]
		t.Logf("parity mismatch before=%q after=%q span=%+v itemspan=%+v hover=%+v name=%+v pending=%v", before, ansi.Strip(m.placement.rows[index].text), m.branchSpans["child-full-id"], item.span, item.hover, item.nameHover, m.placement.pendingTreeRows)
	}
	require.Equal(t, fast, full, "candidate refresh must match a cold/full tree render")
}

func TestTreeHoverRefreshTouchesOnlyOldAndNewRows(t *testing.T) {
	snapshot := refreshSubsetTreeSnapshot(180)
	m := refreshSubsetSidebar(t, snapshot)
	first, second := subagent.NodeID("leaf-000-000"), subagent.NodeID("leaf-001-000")
	m.placement.treeRowRefreshes = 0
	cmd := moveRefreshSubsetPointer(t, m, first, true)
	require.LessOrEqual(t, m.placement.treeRowRefreshes, uint64(2), "pointer entry should repaint only its hovered row")
	assertRefreshSubsetFullParity(t, m)
	cmd = hoverTickCommand(t, sidebarOwnerCommand(m, cmd))
	for range 4 {
		if cmd == nil {
			break
		}
		tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		m.placement.treeRowRefreshes = 0
		m.Update(tick)
		require.LessOrEqual(t, m.placement.treeRowRefreshes, uint64(2), "hover fade should retain only current/previous rows")
		assertRefreshSubsetFullParity(t, m)
		cmd = m.ar.Continue()
	}
	m.placement.treeRowRefreshes = 0
	cmd = moveRefreshSubsetPointer(t, m, second, false)
	require.LessOrEqual(t, m.placement.treeRowRefreshes, uint64(3), "motion should repaint only old/new tree rows plus the active branch span")
	assertRefreshSubsetFullParity(t, m)
	cmd = hoverTickCommand(t, sidebarOwnerCommand(m, cmd))
	for range 24 {
		if cmd == nil {
			break
		}
		tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		m.placement.treeRowRefreshes = 0
		m.Update(tick)
		require.LessOrEqual(t, m.placement.treeRowRefreshes, uint64(3), "fade-out must not sweep the rest of the tree")
		assertRefreshSubsetFullParity(t, m)
		cmd = m.ar.Continue()
	}
}

func TestTreeAnimationRefreshIsVisibleAndLazy(t *testing.T) {
	snapshot := refreshSubsetTreeSnapshot(180)
	m := refreshSubsetSidebar(t, snapshot)
	offscreen := subagent.NodeID("leaf-009-009")
	if !setHugeTreeNodeRunning(snapshot.Nodes, offscreen) {
		t.Fatal("offscreen fixture node missing")
	}
	m.SetSubagentTree(snapshot)
	m.ReconcileLayout()
	m.View()
	offset := m.scrollview.ScrollOffset()
	index, ok := m.placement.treeNodeRows[offscreen]
	require.True(t, ok)
	offscreenRow := m.placement.rows[index]
	require.True(t, int(offscreenRow.targetY) < offset || int(offscreenRow.targetY) >= offset+m.viewportHeight(), "running node is outside the visible viewport")
	cmd := m.ar.Continue()
	require.NotNil(t, cmd, "running subagent should keep the spinner clock alive")
	before := m.ar.Now()
	tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
	require.True(t, ok)
	m.placement.treeRowRefreshes = 0
	m.Update(tick)
	require.LessOrEqual(t, m.placement.treeRowRefreshes, uint64(m.viewportHeight()), "offscreen active row should not be rendered during ticks")
	require.Greater(t, m.ar.Now(), before)
	prepared := m.preparedTrees[m.placement.width]
	line := prepared.rowByID[offscreen]
	require.NotEqual(t, m.subagentSpinner.RawFrame(), prepared.rows[line].frame, "offscreen spinner row is deferred")
	// Moving the viewport to the active row must lazily paint the latest frame.
	m.scrollview.SetScrollOffset(int(offscreenRow.targetY) - m.viewportHeight()/2)
	m.placement.visibleTreeOffset = -1
	m.placement.treeRowRefreshes = 0
	m.View()
	require.LessOrEqual(t, m.placement.treeRowRefreshes, uint64(m.viewportHeight()))
	require.Equal(t, m.subagentSpinner.RawFrame()+m.spinner.RawFrame(), prepared.rows[line].frame)
	assertRefreshSubsetFullParity(t, m)
}

func TestBranchSpanCandidateIndexTracksCompletion(t *testing.T) {
	m := refreshSubsetSidebar(t, refreshSubsetTreeSnapshot(180))
	spanID := subagent.NodeID("branch-000")
	span := m.branchSpans[spanID]
	span.idRunning = true
	span.idValue, span.idTarget, span.idFrom = 0, 1, 0
	m.branchSpans[spanID] = span
	m.refreshBranchSpanIndex()
	require.Equal(t, []subagent.NodeID{spanID}, m.placement.runningSpanIDs)
	require.True(t, m.placement.runningSpanSet[spanID])
	require.True(t, m.tickBranchSpans(animation.ShortDuration))
	require.Empty(t, m.placement.runningSpanIDs)
	require.False(t, m.placement.runningSpanSet[spanID])
	require.InDelta(t, 1, m.branchSpans[spanID].idValue, 0.0001)
}

func TestTreeBranchHoverRefreshKeepsChevronAndFinalZeroParity(t *testing.T) {
	m := refreshSubsetSidebar(t, refreshSubsetTreeSnapshot(180))
	first, second := subagent.NodeID("branch-000"), subagent.NodeID("branch-001")
	for _, id := range []subagent.NodeID{first, second} {
		index, ok := m.placement.treeNodeRows[id]
		require.True(t, ok)
		require.NotEmpty(t, m.placement.rows[index].controls)
	}
	cmd := moveRefreshSubsetPointer(t, m, first, false)
	require.Contains(t, m.placement.pendingTreeRows, first)
	settleSidebarHover(t, m, cmd)
	index := m.placement.treeNodeRows[first]
	row := m.placement.rows[index]
	require.Contains(t, ansi.Strip(row.text), string(first), "branch hover reveals its stable ID")
	require.NotEmpty(t, row.controls, "the disclosure remains a clickable control after hover refresh")
	cmd = moveRefreshSubsetPointer(t, m, second, true)
	settleSidebarHover(t, m, cmd)
	index = m.placement.treeNodeRows[second]
	row = m.placement.rows[index]
	require.Contains(t, ansi.Strip(row.text), string(second))
	require.NotEmpty(t, row.controls)
}

func TestRapidBranchHoverChangesResetPreviousSpanWithoutView(t *testing.T) {
	m := refreshSubsetSidebar(t, refreshSubsetTreeSnapshot(180))
	first, second := subagent.NodeID("branch-000"), subagent.NodeID("branch-001")
	moveRefreshSubsetPointer(t, m, first, false)
	moveRefreshSubsetPointer(t, m, second, false)
	moveRefreshSubsetPointer(t, m, subagent.NodeID("leaf-000-000"), false)
	require.False(t, m.branchSpans[first].idTarget > 0, "first branch target must be canceled even before a view")
	require.False(t, m.branchSpans[second].idTarget > 0, "second branch target must be canceled even before a view")
	require.Equal(t, subagent.NodeID("leaf-000-000"), m.placement.hoveredBranchID)
	assertRefreshSubsetFullParity(t, m)
}

func TestMainSpinnerOnDelegationRootUsesTreeSubset(t *testing.T) {
	snapshot := refreshSubsetTreeSnapshot(180)
	m := refreshSubsetSidebar(t, snapshot)
	id := subagent.NodeID("branch-000")
	if !setHugeTreeNodeRunning(snapshot.Nodes, id) {
		t.Fatal("delegation root fixture node missing")
	}
	m.SetSubagentTree(snapshot)
	m.ReconcileLayout()
	m.delegationRoot = &subagent.Node{ID: id, Agent: "worker", State: subagent.NodeRunning}
	m.workingAgent = "worker"
	m.startSpinner()
	m.ReconcileLayout()
	require.True(t, m.treeAnimationOnly(), "main spinner is consumed by the visible delegation-root tree row")
	index := m.placement.treeNodeRows[id]
	row := m.placement.rows[index]
	m.scrollview.SetScrollOffset(max(0, int(row.targetY)-m.viewportHeight()/2))
	m.View()
	prepared := m.preparedTrees[m.placement.width]
	line := prepared.rowByID[id]
	before := prepared.rows[line].frame
	_, beforeRenders := m.CacheStats()
	cmd := m.ar.Continue()
	require.NotNil(t, cmd)
	for range 12 {
		tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		m.Update(tick)
		m.View()
		if prepared.rows[line].frame != before {
			break
		}
		cmd = m.ar.Continue()
	}
	require.NotEqual(t, before, prepared.rows[line].frame)
	_, afterRenders := m.CacheStats()
	require.Equal(t, beforeRenders+1, afterRenders, "only the affected tree section should be refreshed")
	assertRefreshSubsetFullParity(t, m)
}

func TestOffscreenMainSpinnerDelegationRootStaysTreeOnly(t *testing.T) {
	snapshot := refreshSubsetTreeSnapshot(5_000)
	m := refreshSubsetSidebar(t, snapshot)

	var targetID subagent.NodeID
	var target preparedTreeRow
	for _, row := range m.placement.rows {
		if row.action != ClickSubagent || int(row.targetY) < m.viewportHeight()*2 {
			continue
		}
		id := subagent.NodeID(row.payload)
		line, ok := m.preparedTrees[m.placement.width].rowByID[id]
		if !ok {
			continue
		}
		targetID = id
		target = m.preparedTrees[m.placement.width].rows[line]
	}
	require.NotEmpty(t, targetID, "fixture must have an offscreen delegation root")
	require.True(t, setHugeTreeNodeRunning(snapshot.Nodes, targetID))
	m.SetSubagentTree(snapshot)
	m.ReconcileLayout()
	m.CancelPresentation()
	m.ReconcileLayout()
	m.delegationRoot = &subagent.Node{ID: targetID, Agent: target.node.Agent, State: subagent.NodeRunning}
	m.workingAgent = target.node.Agent
	m.startSpinner()
	m.ReconcileLayout()

	targetIndex := m.placement.treeNodeRows[targetID]
	targetRow := m.placement.rows[targetIndex]
	require.True(t, int(targetRow.targetY) >= m.viewportHeight(), "delegation root must be offscreen")
	require.True(t, m.treeAnimationOnly(), "offscreen root spinner still uses the tree-only path")
	prepared := m.preparedTrees[m.placement.width]
	line := prepared.rowByID[targetID]
	beforeFrame := m.spinner.RawFrame()
	m.placement.treeRowRefreshes = 0
	for range 30 {
		cmd := m.ar.Continue()
		require.NotNil(t, cmd, "working root spinner must keep the animation clock active")
		tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		m.Update(tick)
		m.View()
		if m.spinner.RawFrame() != beforeFrame {
			break
		}
	}
	require.NotEqual(t, beforeFrame, m.spinner.RawFrame(), "main spinner advances for an offscreen delegation root")
	require.LessOrEqual(t, m.placement.treeRowRefreshes, uint64(m.viewportHeight()), "offscreen root must not be rendered during ticks")
	require.NotEqual(t, m.spinner.RawFrame()+m.subagentSpinner.RawFrame(), prepared.rows[line].frame, "offscreen row rendering remains deferred")

	m.scrollview.SetScrollOffset(int(targetRow.targetY) - m.viewportHeight()/2)
	m.placement.visibleTreeOffset = -1
	m.View()
	require.Equal(t, m.spinner.RawFrame()+m.subagentSpinner.RawFrame(), prepared.rows[line].frame, "the current main and subagent spinner frames paint on first visibility")
}
