package sidebar

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func newLegacyParticipantSidebar(t *testing.T) *model {
	t.Helper()
	ar := animation.NewRuntimeWithScheduler(&placementClock{now: time.Unix(1, 0), step: 50 * time.Millisecond})
	m := New(ar, t.Context(), &service.SessionState{}).(*model)
	m.treeCollapsed = false // Participant tests exercise visible inspector rows.
	m.subagentSpinner = &fakeSpinner{m.subagentSpinner}
	m.rootSessionID = "collapse"
	m.SetPosition(5, 3)
	cmd := m.SetSubagentTree(collapseFixture())
	settleTreePresentation(t, m, tea.Batch(cmd, m.SetSize(60, 70)))
	t.Cleanup(m.StopAnimation)
	return m
}

func clickLegacySummary(t *testing.T, m *model) {
	t.Helper()
	row := requirePlaced(t, m, "tree-summary")
	x := m.layoutCfg.PaddingLeft + m.contentWidth(m.cachedNeedsScrollbar) - 1
	y := int(row.y) - m.scrollview.ScrollOffset()
	require.Equal(t, m.summaryLine-m.scrollview.ScrollOffset(), y)
	_, cmd := m.Update(tea.MouseClickMsg{X: m.xPos + x, Y: m.yPos + y, Button: tea.MouseLeft})
	settlePlacement(t, m, cmd)
}

func TestLegacyBackgroundUsageRowInspectorAndWholeCollapse(t *testing.T) {
	t.Parallel()
	m := newLegacyParticipantSidebar(t)
	snap := collapseFixture()
	snap.Nodes[0].Children = nil
	cmd := m.SetSubagentTree(snap)
	settleTreePresentation(t, m, tea.Batch(cmd, m.ReconcileLayout()))
	assert.NotContains(t, ansi.Strip(m.View()), "worker")
	m.SetTokenUsage(&runtime.TokenUsageEvent{SessionID: "legacy-session", AgentContext: runtime.AgentContext{AgentName: "worker"}, Usage: &runtime.Usage{ContextLength: 450, ContextLimit: 1000}})
	settleTreePresentation(t, m, m.ReconcileLayout())
	view := ansi.Strip(m.View())
	require.Contains(t, view, "45%")
	require.Contains(t, view, "worker")
	require.Equal(t, []string{"worker"}, m.participantNames())
	summary := ansi.Strip(m.treeSummary(80))
	assert.Contains(t, summary, "1 subagents")
	assert.NotContains(t, summary, "active", "usage alone must not fabricate async state")
	assert.NotContains(t, summary, "attention")
	var row int
	for y, name := range m.agentClickZones {
		if name == "worker" {
			row = y
		}
	}
	require.Positive(t, row)
	result, name := m.HandleClickType(m.layoutCfg.PaddingLeft, row)
	assert.Equal(t, ClickAgent, result, "legacy row uses existing right-click/Ctrl-click inspector dispatch")
	assert.Equal(t, "worker", name)
	_, mapped := m.subagentHoverZone[row]
	assert.False(t, mapped, "legacy row must not invent an async attachment ID")
	_, hoverCmd := m.Update(tea.MouseMotionMsg{X: m.xPos + m.layoutCfg.PaddingLeft, Y: m.yPos + row})
	settlePlacement(t, m, hoverCmd)
	assert.Empty(t, m.hoveredSubagent)
	clickLegacySummary(t, m)
	assert.True(t, m.treeCollapsed)
	summary = strings.TrimSpace(ansi.Strip(strings.Split(m.View(), "\n")[m.summaryLine-m.scrollview.ScrollOffset()]))
	assert.Equal(t, "1 subagents", strings.TrimSpace(strings.TrimSuffix(summary, "›")))
	assert.True(t, strings.HasSuffix(summary, "›"), "whole-tree control stays at the right edge")
	assert.NotContains(t, summary, "active")
	assert.NotContains(t, summary, "attention")
	assert.Empty(t, m.agentClickZones, "collapsed participants leave no agent inspector hit targets")
	assert.Empty(t, m.subagentHoverZone, "collapsed participants leave no async attachment hit targets")
	assert.Contains(t, ansi.Strip(requirePlaced(t, m, "usage:0").text), "45%", "tree collapse preserves the independent Session Token Usage reading")
	m.SetTokenUsage(&runtime.TokenUsageEvent{SessionID: "legacy-session", AgentContext: runtime.AgentContext{AgentName: "worker"}, Usage: &runtime.Usage{ContextLength: 0, ContextLimit: 1000}})
	settleTreePresentation(t, m, m.ReconcileLayout())
	clickLegacySummary(t, m)
	require.False(t, m.treeCollapsed)
	after := ansi.Strip(m.View())
	assert.NotContains(t, after, "45%")
	require.Contains(t, after, "worker", "expansion restores the legacy inspector row even at zero usage")
	for line := range strings.SplitSeq(after, "\n") {
		if strings.Contains(line, "worker") {
			assert.Contains(t, line, "0%")
		}
	}
	usage, ok := m.sessionState.AgentUsage("worker")
	require.True(t, ok)
	assert.Zero(t, usage.ContextLength, "an authoritative zero remains visible to the inspector")
}

func TestLegacyUsageMatchesCanonicalSessionNotAgentName(t *testing.T) {
	t.Parallel()
	m := newLegacyParticipantSidebar(t)
	snap := collapseFixture()
	snap.Nodes[0].Children[0].Node.Agent = "worker"
	snap.Nodes[0].Children[0].Node.SessionID = "canonical-session"
	settleTreePresentation(t, m, tea.Batch(m.SetSubagentTree(snap), m.ReconcileLayout()))
	m.SetTokenUsage(&runtime.TokenUsageEvent{SessionID: "canonical-session", AgentContext: runtime.AgentContext{AgentName: "worker"}, Usage: &runtime.Usage{ContextLength: 100, ContextLimit: 1000}})
	settleTreePresentation(t, m, m.ReconcileLayout())
	assert.Empty(t, m.participantNames(), "canonical usage must not create a duplicate legacy row")
	m.SetTokenUsage(&runtime.TokenUsageEvent{SessionID: "distinct-legacy-session", AgentContext: runtime.AgentContext{AgentName: "worker"}, Usage: &runtime.Usage{ContextLength: 450, ContextLimit: 1000}})
	settleTreePresentation(t, m, m.ReconcileLayout())
	assert.Equal(t, []string{"worker"}, m.participantNames(), "same configured agent in a distinct noncanonical session remains an actual participant")
	assert.Contains(t, ansi.Strip(m.treeSummary(80)), "4 subagents")
	snap.Nodes[0].Children = append(snap.Nodes[0].Children, subagent.NodeSnapshot{Node: subagent.Node{ID: "new-full-node", Agent: "worker", SessionID: "distinct-legacy-session", State: subagent.NodeIdle}})
	settleTreePresentation(t, m, tea.Batch(m.SetSubagentTree(snap), m.ReconcileLayout()))
	assert.Empty(t, m.participantNames(), "later canonical adoption removes only its matching legacy session row")
	assert.Contains(t, ansi.Strip(m.treeSummary(80)), "4 subagents", "adoption does not double count the actual instance")
}

func TestAttachedLeafDoesNotInheritMainLegacyParticipants(t *testing.T) {
	t.Parallel()
	m := newLegacyParticipantSidebar(t)
	m.SetTokenUsage(&runtime.TokenUsageEvent{SessionID: "main-legacy", AgentContext: runtime.AgentContext{AgentName: "legacy"}, Usage: &runtime.Usage{ContextLength: 45, ContextLimit: 100}})
	settleTreePresentation(t, m, m.ReconcileLayout())
	require.Equal(t, []string{"legacy"}, m.participantNames())
	m.SetSubagentContext("leaf-full-id", "root", "collapse")
	leaf := session.New(session.WithID("leaf-session"))
	leaf.SetSubagentTree(&subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "leaf-full-id", Agent: "leaf", SessionID: leaf.ID, State: subagent.NodeIdle}}}})
	m.LoadFromSession(leaf)
	settleTreePresentation(t, m, m.ReconcileLayout())
	assert.Empty(t, m.participantNames(), "page-owned usage attribution is cleared on perspective change")
	assert.Empty(t, m.usageOwners)
	assert.NotContains(t, ansi.Strip(m.View()), "subagents")
	assert.NotContains(t, ansi.Strip(m.View()), "legacy")
	result, payload := m.HandleClickType(m.layoutCfg.PaddingLeft, m.parentLineZone)
	assert.Equal(t, ClickSubagentParent, result, "parent navigation remains while empty recap is omitted")
	assert.Equal(t, "collapse", payload)
	assert.Empty(t, m.subagentHoverZone)
	assert.Empty(t, m.treeControls)
	_, cmd := m.Update(tea.MouseClickMsg{X: m.xPos + m.layoutCfg.PaddingLeft, Y: m.yPos + m.treeSectionStart - m.scrollview.ScrollOffset(), Button: tea.MouseLeft})
	assert.Nil(t, cmd)
	assert.False(t, m.treeCollapsed, "zero descendants have no effective expand/collapse control")
}
