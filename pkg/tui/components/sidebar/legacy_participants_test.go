package sidebar

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
)

func TestLegacyBackgroundUsageRowInspectorAndWholeCollapse(t *testing.T) {
	t.Parallel()
	m := newCollapseSidebar(t)
	snap := collapseFixture()
	snap.Nodes[0].Children = nil
	m.SetSubagentTree(snap)
	assert.NotContains(t, ansi.Strip(m.View()), "worker")
	m.SetTokenUsage(&runtime.TokenUsageEvent{SessionID: "legacy-session", AgentContext: runtime.AgentContext{AgentName: "worker"}, Usage: &runtime.Usage{ContextLength: 450, ContextLimit: 1000}})
	view := ansi.Strip(m.View())
	require.Contains(t, view, "45%")
	require.Contains(t, view, "worker")
	require.Equal(t, []string{"worker"}, m.participantNames())
	assert.Contains(t, ansi.Strip(m.treeSummary(80)), "2 total · 0 active · 0 attention", "usage alone must not fabricate async state")
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
	m.Update(tea.MouseMotionMsg{X: m.xPos + m.layoutCfg.PaddingLeft, Y: m.yPos + row})
	assert.Empty(t, m.hoveredSubagent)
	clickTreeControl(t, m, "", true)
	assert.Contains(t, ansi.Strip(m.View()), "2 total · 0 active · 0 attention")
	assert.Equal(t, "› 2 total · 0 active · 0 attention", strings.TrimSpace(ansi.Strip(m.cachedLines[m.treeSectionStart])))
	assert.Empty(t, m.agentClickZones, "collapsed participants leave no agent inspector hit targets")
	assert.Empty(t, m.subagentHoverZone, "collapsed participants leave no async attachment hit targets")
	assert.Contains(t, ansi.Strip(m.cachedLines[m.usageReadingLine]), "45%", "tree collapse preserves the independent Session Token Usage reading")
	m.SetTokenUsage(&runtime.TokenUsageEvent{SessionID: "legacy-session", AgentContext: runtime.AgentContext{AgentName: "worker"}, Usage: &runtime.Usage{ContextLength: 0, ContextLimit: 1000}})
	y := m.treeSectionStart - m.scrollview.ScrollOffset()
	m.Update(tea.MouseClickMsg{X: m.xPos + m.layoutCfg.PaddingLeft, Y: m.yPos + y, Button: tea.MouseLeft})
	after := ansi.Strip(m.View())
	assert.NotContains(t, after, "45%")
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
	m := newCollapseSidebar(t)
	snap := collapseFixture()
	snap.Nodes[0].Children[0].Node.Agent = "worker"
	snap.Nodes[0].Children[0].Node.SessionID = "canonical-session"
	m.SetSubagentTree(snap)
	m.SetTokenUsage(&runtime.TokenUsageEvent{SessionID: "canonical-session", AgentContext: runtime.AgentContext{AgentName: "worker"}, Usage: &runtime.Usage{ContextLength: 100, ContextLimit: 1000}})
	assert.Empty(t, m.participantNames(), "canonical usage must not create a duplicate legacy row")
	m.SetTokenUsage(&runtime.TokenUsageEvent{SessionID: "distinct-legacy-session", AgentContext: runtime.AgentContext{AgentName: "worker"}, Usage: &runtime.Usage{ContextLength: 450, ContextLimit: 1000}})
	assert.Equal(t, []string{"worker"}, m.participantNames(), "same configured agent in a distinct noncanonical session remains an actual participant")
	assert.Contains(t, ansi.Strip(m.treeSummary(80)), "5 total")
	snap.Nodes[0].Children = append(snap.Nodes[0].Children, subagent.NodeSnapshot{Node: subagent.Node{ID: "new-full-node", Agent: "worker", SessionID: "distinct-legacy-session", State: subagent.NodeIdle}})
	m.SetSubagentTree(snap)
	assert.Empty(t, m.participantNames(), "later canonical adoption removes only its matching legacy session row")
	assert.Contains(t, ansi.Strip(m.treeSummary(80)), "5 total", "adoption does not double count the actual instance")
}
