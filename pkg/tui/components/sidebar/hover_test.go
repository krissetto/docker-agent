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
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func hoverTickCommand(t *testing.T, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, next := range batch {
			if found := hoverTickCommand(t, next); found != nil {
				return found
			}
		}
		return nil
	}
	if _, ok := msg.(animation.TickMsg); ok {
		return func() tea.Msg { return msg }
	}
	return nil
}

// sidebarOwnerCommand commits registrations after a standalone fixture action.
// A supplied command may already carry the owner's pending lease.
func sidebarOwnerCommand(m *model, cmd tea.Cmd) tea.Cmd {
	return tea.Batch(cmd, m.ar.Continue())
}

func settleSidebarHover(t *testing.T, m *model, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	cmd = hoverTickCommand(t, sidebarOwnerCommand(m, cmd))
	for range 40 {
		if !m.hoverAnimation.IsActive() && !m.branchSpansRunning() {
			return cmd
		}
		require.NotNil(t, cmd)
		tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		m.Update(tick)
		cmd = m.ar.Continue()
	}
	t.Fatal("hover did not settle")
	return nil
}

func newHoverSidebar(t *testing.T) *model {
	t.Helper()
	ar := animation.NewRuntimeWithScheduler(&immediateScheduler{now: time.Unix(1, 0)})
	m := New(ar, t.Context(), &service.SessionState{}).(*model)
	m.treeCollapsed = false // Hover fixtures need visible descendant rows.
	m.todosCollapsed = false
	m.SetSize(80, 30)
	m.rootSessionID = "hover"
	m.SetSubagentTree(subagent.Snapshot{Root: "root:hover", Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "root:hover", Agent: "root", State: subagent.NodeIdle}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-full-id", Agent: "worker", State: subagent.NodeIdle}}}}}})
	m.ReconcileLayout()
	m.CancelPresentation()
	t.Cleanup(m.StopAnimation)
	return m
}

func TestHoverEntryExitElapsedAndNeutralIdentityParts(t *testing.T) {
	t.Parallel()
	m := newHoverSidebar(t)
	m.View()
	var row int
	for y, id := range m.subagentHoverZone {
		if id == "child-full-id" {
			row = y
		}
	}
	_, cmd := m.Update(tea.MouseMotionMsg{X: m.layoutCfg.PaddingLeft + 4, Y: row})
	initial := m.View()
	require.Zero(t, m.hoverValues["node:child-full-id"].value)
	cmd = hoverTickCommand(t, sidebarOwnerCommand(m, cmd))
	require.NotNil(t, cmd)
	tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
	require.True(t, ok)
	m.Update(tick)
	progress := m.hoverValues["node:child-full-id"].value
	assert.Greater(t, progress, 0.0)
	assert.Less(t, progress, 1.0)
	assert.NotEqual(t, initial, m.View())
	settleSidebarHover(t, m, m.ar.Continue())
	require.InDelta(t, 1, m.hoverValues["node:child-full-id"].value, 0, "settled hover endpoint is clamped to exactly one")
	assert.Zero(t, m.ar.ActiveCount())
	hovered := strings.Split(m.View(), "\n")[row]
	assert.Contains(t, hovered, styles.MutedStyle.Render(" (child-full-id)"), "IDs remain neutral")
	assert.Contains(t, hovered, styles.MutedStyle.Render(timeAgo(time.Time{})), "status/time remains neutral")
	assert.Contains(t, ansi.Strip(hovered), "worker")
	settledGen := m.VisualGeneration()
	for range 100 {
		m.Update(tea.MouseMotionMsg{X: m.layoutCfg.PaddingLeft + 4, Y: row})
	}
	assert.Equal(t, settledGen, m.VisualGeneration())
	assert.Zero(t, m.ar.ActiveCount())
	exit := sidebarOwnerCommand(m, m.ClearSubagentHover())
	require.NotNil(t, exit, "settled idle hover must start its exit via the owner command")
	require.InDelta(t, 1, m.hoverValues["node:child-full-id"].value, 0, "settled hover endpoint is clamped to exactly one")
	settleSidebarHover(t, m, exit)
	assert.Empty(t, m.hoverValues)
	assert.Zero(t, m.ar.ActiveCount())
}

func TestCancelHoverForHiddenPageDoesNotStopOtherLeases(t *testing.T) {
	t.Parallel()
	for _, settled := range []bool{false, true} {
		m := newHoverSidebar(t)
		cmd := m.setHoverTarget("directory")
		if settled {
			settleSidebarHover(t, m, cmd)
			m.ClearSubagentHover()
		}
		other := m.ar.Subscribe()
		other.Start()
		m.CancelHover()
		assert.Empty(t, m.hoverValues)
		assert.Empty(t, m.hoverTarget)
		assert.EqualValues(t, 1, m.ar.ActiveCount(), "hiding cancels only hover, not other work")
		gen := m.VisualGeneration()
		m.CancelHover()
		assert.Equal(t, gen, m.VisualGeneration(), "already-cleared hidden hover is a no-op")
		other.Stop()
		assert.Zero(t, m.ar.ActiveCount())
	}
}

func TestEveryActionableIdentityUsesSharedHoverText(t *testing.T) {
	t.Parallel()
	m := newHoverSidebar(t)
	m.parentAgent = "parent"
	m.parentSessionID = "parent-session"
	m.hoverValues = map[string]hoverValue{
		"parent:parent-session": {value: .5, target: 1},
		"agent:legacy-worker":   {value: .5, target: 1},
		"node:child-full-id":    {value: .5, target: 1},
	}
	expected := func(name string) string {
		return styles.HoverText(styles.AgentIdentityStyle(name, false).Render(name), .5, styles.TextPrimary)
	}
	assert.Contains(t, m.parentLine(), expected("parent"))
	assert.Contains(t, m.participantLine("legacy-worker", 60), expected("legacy-worker"))
	assert.Contains(t, m.subagentLine(subagent.Node{ID: "child-full-id", Agent: "worker", State: subagent.NodeIdle}, "", 60), expected("worker"))
	assert.Contains(t, m.subagentLine(subagent.Node{ID: "child-full-id", Agent: "worker", State: subagent.NodeIdle}, "", 60), styles.MutedStyle.Render("idle"))
	before := m.hoverValues["node:child-full-id"]
	for range 100 {
		m.View()
	}
	assert.Equal(t, before, m.hoverValues["node:child-full-id"], "rendering never advances transition values")
}
