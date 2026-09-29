package sidebar

import (
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func settleTreePresentation(t *testing.T, m *model, cmd tea.Cmd) {
	t.Helper()
	cmd = hoverTickCommand(t, sidebarOwnerCommand(m, cmd))
	for range 80 {
		running := (m.placement != nil && m.placement.running) || m.branchSpansRunning()
		for i := range m.treeCounters {
			running = running || m.treeCounters[i].running
		}
		if !running {
			return
		}
		require.NotNil(t, cmd, "caller must retain the shared runtime continuation")
		tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		m.Update(tick)
		cmd = m.ar.Continue()
	}
	t.Fatal("tree presentation did not settle")
}

func TestWholeTreeAnimatedRevealRetargetAndPinnedFooter(t *testing.T) {
	t.Parallel()
	m := newHoverSidebar(t)
	snap := collapseFixture()
	snap.Nodes[0].Children[0].Children[0].Node.State = subagent.NodeIdle
	m.rootSessionID = "collapse"
	m.SetSubagentTree(snap)
	m.sessionState.SetYoloMode(true)
	setupCmd := m.SetSize(40, 8)
	settleTreePresentation(t, m, setupCmd)
	first := m.View()
	footer := strings.Split(first, "\n")[7]
	assert.Contains(t, footer, "YOLO")
	x := m.layoutCfg.PaddingLeft + m.contentWidth(m.cachedNeedsScrollbar) - 1
	m.scrollview.SetScrollOffset(m.treeSectionStart)
	m.View()
	y := m.treeSectionStart - m.scrollview.ScrollOffset()
	result, _ := m.HandleClickType(x, y)
	assert.Equal(t, ClickNone, result)
	_, cmd := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.True(t, m.placement.running)
	initial := m.placement.rows[len(m.placement.rows)-1].alpha
	cmd = hoverTickCommand(t, sidebarOwnerCommand(m, cmd))
	require.NotNil(t, cmd)
	tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
	require.True(t, ok)
	m.Update(tick)
	next := m.ar.Continue()
	assert.Less(t, m.placement.rows[len(m.placement.rows)-1].alpha, initial)
	assert.Greater(t, m.placement.rows[len(m.placement.rows)-1].alpha, 0.0)
	assert.Equal(t, footer, strings.Split(m.View(), "\n")[7])
	current := m.placement.rows[len(m.placement.rows)-1].alpha
	_, restart := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	assert.Nil(t, restart, "retarget reuses active shared clock")
	require.InDelta(t, current, m.placement.rows[len(m.placement.rows)-1].alpha, 0, "rapid reverse starts at displayed opacity")
	settleTreePresentation(t, m, next)
	assert.False(t, m.treeCollapsed)
	assert.Zero(t, m.ar.ActiveCount())
	assert.Equal(t, footer, strings.Split(m.View(), "\n")[7])
	result, _ = m.HandleClickType(m.layoutCfg.PaddingLeft, 7)
	assert.Equal(t, ClickNone, result, "passive pinned footer cannot become a tree action")
	m.SetSize(8, 1)
	view := m.View()
	require.Len(t, strings.Split(view, "\n"), 1)
	assert.Contains(t, ansi.Strip(view), "YOLO")
	assert.Zero(t, m.viewportHeight())
	result, _ = m.HandleClickType(1, 0)
	assert.Equal(t, ClickNone, result)
}

func TestSummaryCountersFadeWithoutZeroFragments(t *testing.T) {
	t.Parallel()
	m := newHoverSidebar(t)
	settleTreePresentation(t, m, m.ReconcileLayout())
	m.View()
	assert.Contains(t, ansi.Strip(m.treeSummary(60)), "1 subagents")
	assert.NotContains(t, ansi.Strip(m.treeSummary(60)), "active")
	snap := subagent.Snapshot{Root: "root:hover", Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "root:hover", Agent: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-full-id", Agent: "worker", State: subagent.NodeIdle, NeedsAttention: true}}}}}}
	m.SetSubagentTree(snap)
	cmd := m.ReconcileLayout()
	assert.NotContains(t, ansi.Strip(m.treeSummary(60)), "0 attention")
	settleTreePresentation(t, m, cmd)
	assert.Contains(t, ansi.Strip(m.treeSummary(60)), "1 attention")
	snap.Nodes[0].Children[0].Node.NeedsAttention = false
	m.SetSubagentTree(snap)
	cmd = m.ReconcileLayout()
	assert.Contains(t, ansi.Strip(m.treeSummary(60)), "1 attention", "outgoing nonzero value fades before removal")
	settleTreePresentation(t, m, cmd)
	assert.NotContains(t, ansi.Strip(m.treeSummary(60)), "attention")
	assert.Zero(t, m.ar.ActiveCount())
}

func TestHiddenPresentationUpdatesSnapWithoutStealingSharedClock(t *testing.T) {
	t.Parallel()
	m := newHoverSidebar(t)
	other := m.ar.Subscribe()
	require.Nil(t, other.Start())
	original := m.ar.Continue()
	require.NotNil(t, original)
	require.Nil(t, m.SetPresentationActive(false))
	assert.EqualValues(t, 1, m.ar.ActiveCount())
	require.Nil(t, m.SetQueuedMessages([]QueuedMessage{{ID: "hidden-a", Text: "hidden queued"}}))
	assert.Contains(t, ansi.Strip(m.View()), "hidden queued")
	snap := subagent.Snapshot{Root: "root:hover", Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "root:hover", Agent: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-full-id", Agent: "worker", NeedsAttention: true, State: subagent.NodeIdle}}}}}}
	m.SetSubagentTree(snap)
	require.Nil(t, m.ReconcileLayout())
	assert.Contains(t, ansi.Strip(m.treeSummary(60)), "1 attention")
	assert.EqualValues(t, 1, m.ar.ActiveCount(), "hidden queue/counter changes keep only the unrelated clock lease")
	assert.Nil(t, m.setHoverTarget("directory"))
	tick, ok := m.ar.Accept(hoverTickCommand(t, original)().(animation.TickMsg))
	require.True(t, ok, "original unrelated tick remains valid")
	m.Update(tick)
	other.Stop()
	assert.Zero(t, m.ar.ActiveCount())
	require.Nil(t, m.SetPresentationActive(true))
	assert.Zero(t, m.ar.ActiveCount(), "reactivation does not replay hidden changes")
	cmd := m.SetQueuedMessages([]QueuedMessage{{ID: "hidden-a", Text: "hidden queued"}, {ID: "visible-b", Text: "new visible"}})
	cmd = sidebarOwnerCommand(m, cmd)
	require.NotNil(t, cmd, "next foreground change is committed by the owner")
	settleTreePresentation(t, m, cmd)
	assert.Contains(t, ansi.Strip(m.View()), "new visible")
	assert.Zero(t, m.ar.ActiveCount())
}

func TestSettledTreeExpandMirrorsExistingCollapseEndpoints(t *testing.T) {
	t.Parallel()
	m := newHoverSidebar(t)
	snap := collapseFixture()
	snap.Nodes[0].Children[0].Children[0].Node.State = subagent.NodeIdle
	m.rootSessionID = "collapse"
	m.SetSubagentTree(snap)
	m.ReconcileLayout()
	m.CancelPresentation()
	before, ok := findPlaced(m.placement.rows, "node:branch-full-identity")
	require.True(t, ok)
	closeCmd := m.toggleTreeControl(treeControl{whole: true})
	closing, ok := findPlaced(m.placement.rows, before.id)
	require.True(t, ok)
	require.InDelta(t, before.y, closing.fromY, 0)
	closeMidY := closing.fromY + (closing.targetY-closing.fromY)*animation.EaseOutCubic(.5)
	closeMidAlpha := closing.fromAlpha + (closing.targetAlpha-closing.fromAlpha)*animation.EaseOutCubic(.5)
	settleTreePresentation(t, m, closeCmd)
	m.invalidateAnimation()
	require.Nil(t, m.ReconcileLayout()) // prune fully exited cached rows, as later theme/spinner updates do
	_, exists := findPlaced(m.placement.rows, before.id)
	assert.False(t, exists)
	openCmd := m.toggleTreeControl(treeControl{whole: true})
	opening, ok := findPlaced(m.placement.rows, before.id)
	require.True(t, ok)
	require.InDelta(t, closing.targetY, opening.fromY, 0, "expand starts at the actual collapse endpoint")
	require.InDelta(t, closing.fromY, opening.targetY, 0, "expand ends at the prior full allocation")
	require.InDelta(t, 0, opening.fromAlpha, 0)
	require.InDelta(t, 1, opening.targetAlpha, 0)
	openMidY := opening.fromY + (opening.targetY-opening.fromY)*animation.EaseOutCubic(.5)
	openMidAlpha := opening.fromAlpha + (opening.targetAlpha-opening.fromAlpha)*animation.EaseOutCubic(.5)
	require.InDelta(t, closing.fromY+closing.targetY, openMidY+closeMidY, 1e-12, "matched normalized progress reverses geometry")
	require.InDelta(t, 1, openMidAlpha+closeMidAlpha, 1e-12, "matched progress reverses fade")
	result, _ := m.HandleClickType(m.layoutCfg.PaddingLeft, int(opening.y)-m.scrollview.ScrollOffset())
	assert.NotEqual(t, ClickSubagent, result, "zero-opacity entering row cannot attach")
	settleTreePresentation(t, m, openCmd)
	assert.Zero(t, m.ar.ActiveCount())
}

func TestSummaryLabelUsesRegularThemeForeground(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := newHoverSidebar(t)
	for _, ref := range []string{"default", "default-light"} {
		theme, err := styles.LoadTheme(ref)
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		label := m.treeSummary(60)
		cells := sidebarCells(label)
		require.NotEmpty(t, cells)
		assert.Equal(t, color.NRGBAModel.Convert(styles.TextPrimary), color.NRGBAModel.Convert(cells[0].fg))
		m.hoverValues = map[string]hoverValue{"tree-summary": {value: 1, target: 1}}
		hover := sidebarCells(m.hoverText(label, "tree-summary"))
		assert.Equal(t, color.NRGBAModel.Convert(styles.Brighten(styles.TextPrimary, .25)), color.NRGBAModel.Convert(hover[0].fg))
		m.hoverValues = nil
		assert.Equal(t, label, m.hoverText(m.treeSummary(60), "tree-summary"), "exit returns to original regular text role")
	}
}

func TestExpandingChildrenNeverCoverPersistentRecap(t *testing.T) {
	t.Parallel()
	m := newHoverSidebar(t)
	snap := collapseFixture()
	snap.Nodes[0].Children[0].Children[0].Node.State = subagent.NodeIdle
	snap.Nodes[0].Children[0].Children[0].Node.NeedsAttention = false // isolate stable recap paint from the collapsed-only warning label
	m.rootSessionID = "collapse"
	m.SetSubagentTree(snap)
	m.ReconcileLayout()
	m.CancelPresentation()
	settleTreePresentation(t, m, m.toggleTreeControl(treeControl{whole: true}))
	m.invalidateAnimation()
	m.ReconcileLayout()
	summary, ok := findPlaced(m.placement.rows, "tree-summary")
	require.True(t, ok)
	y := int(summary.y) - m.scrollview.ScrollOffset()
	_, hoverCmd := m.Update(tea.MouseMotionMsg{X: m.layoutCfg.PaddingLeft, Y: y})
	settleSidebarHover(t, m, hoverCmd)
	before := sidebarCells(strings.Split(m.View(), "\n")[y])
	_, cmd := m.Update(tea.MouseClickMsg{X: m.layoutCfg.PaddingLeft, Y: y, Button: tea.MouseLeft})
	check := func() {
		t.Helper()
		row, ok := m.placementRowAt(m.layoutCfg.PaddingLeft, y)
		require.True(t, ok)
		assert.Equal(t, "tree-summary", row.id, "emerging child/gap never owns recap paint or hits")
		require.InDelta(t, 1, row.alpha, 0)
		require.InDelta(t, summary.y, row.y, 0)
		control, ok := m.placementControlAt(m.layoutCfg.PaddingLeft, y)
		require.True(t, ok)
		assert.True(t, control.whole)
		cells := sidebarCells(strings.Split(m.View(), "\n")[y])
		require.Len(t, cells, len(before))
		for x := range cells {
			assert.Equal(t, before[x].fg, cells[x].fg, "recap foreground stable column%d", x)
			assert.Equal(t, before[x].bg, cells[x].bg, "recap background stable column%d", x)
			if before[x].glyph != "›" {
				assert.Equal(t, before[x].glyph, cells[x].glyph, "only direction glyph changes")
			}
		}
	}
	check() // click/update frame before first accepted tick
	cmd = hoverTickCommand(t, sidebarOwnerCommand(m, cmd))
	for i := range 12 {
		require.NotNil(t, cmd)
		tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		m.Update(tick)
		cmd = m.ar.Continue()
		check()
		if i == 5 {
			_, retarget := m.Update(tea.MouseClickMsg{X: m.layoutCfg.PaddingLeft, Y: y, Button: tea.MouseLeft})
			assert.Nil(t, retarget, "midflight reverse retains original clock")
			before = sidebarCells(strings.Split(m.View(), "\n")[y])
		}
	}
	settleTreePresentation(t, m, cmd)
	assert.Zero(t, m.ar.ActiveCount())
}

func TestParentNavigationRegularLabelSeparatedFromRecapOnly(t *testing.T) {
	t.Parallel()
	m := newHoverSidebar(t)
	m.SetSubagentContext("child", "parent", "canonical-parent-session")
	snap := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child", Agent: "child", State: subagent.NodeIdle}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "grandchild", Agent: "worker", State: subagent.NodeIdle}}}}}}
	m.SetSubagentTree(snap)
	settleTreePresentation(t, m, m.ReconcileLayout())
	cells := sidebarCells(m.parentLine())
	for i := range ansi.StringWidth("parent: ") {
		assert.Equal(t, color.NRGBAModel.Convert(styles.TextPrimary), color.NRGBAModel.Convert(cells[i].fg), "label/punctuation use regular text")
	}
	assert.Equal(t, color.NRGBAModel.Convert(styles.AgentIdentityStyle("parent", false).GetForeground()), color.NRGBAModel.Convert(cells[ansi.StringWidth("parent: ")].fg))
	m.View()
	assert.Equal(t, m.parentLineZone+2, m.summaryLine)
	gap := m.parentLineZone + 1
	result, _ := m.HandleClickType(m.layoutCfg.PaddingLeft, gap)
	assert.Equal(t, ClickNone, result)
	result, payload := m.HandleClickType(m.layoutCfg.PaddingLeft, m.parentLineZone)
	assert.Equal(t, ClickSubagentParent, result)
	assert.Equal(t, "canonical-parent-session", payload)
	snap.Nodes[0].Children = nil
	m.SetSubagentTree(snap)
	settleTreePresentation(t, m, m.ReconcileLayout())
	for _, row := range m.placement.rows {
		if row.target {
			assert.NotEqual(t, "tree-summary", row.id)
			assert.NotEqual(t, "gap:tree-summary", row.id, "leaf parent retains no recap spacer")
		}
	}
	assert.Contains(t, ansi.Strip(m.View()), "parent: parent")
}
