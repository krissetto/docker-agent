package sidebar

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

// activeAgentsRoster is a 3-agent team whose current agent is "root" (set by
// the panel-sidebar constructors), used across the active-agents-only tests.
func activeAgentsRoster() []runtime.AgentDetails {
	return []runtime.AgentDetails{
		{Name: "root", Provider: "anthropic", Model: "opus"},
		{Name: "planner", Provider: "openai", Model: "gpt-5.4"},
		{Name: "reviewer", Provider: "openai", Model: "gpt-4o"},
	}
}

// rosterOwners renders the Agents panel and returns the deduped agent names
// owning its body lines, in display order.
func rosterOwners(m *model) []string {
	renderAgentPanel(m)
	var names []string
	for _, owner := range m.agentLineOwners {
		if owner == "" {
			continue
		}
		if len(names) == 0 || names[len(names)-1] != owner {
			names = append(names, owner)
		}
	}
	return names
}

// TestActiveAgentsOnly_FiltersRosterToSessionParticipants verifies the filter
// drops team agents without any session participation while keeping the
// current agent and agents with recorded usage — and that the whole team
// renders with the filter off (the default).
func TestActiveAgentsOnly_FiltersRosterToSessionParticipants(t *testing.T) {
	t.Parallel()

	m := newCompactPanelSidebar(t, 40, activeAgentsRoster()...)
	recordAgentUsageWithCost(m, "s-planner", "planner", 10_000, 100_000, 0.01)

	assert.Equal(t, []string{"root", "planner", "reviewer"}, rosterOwners(m),
		"the filter is off by default: the whole team renders")

	m.SetActiveAgentsOnly(true)
	assert.Equal(t, []string{"root", "planner"}, rosterOwners(m),
		"only the current agent and session participants remain")

	m.SetActiveAgentsOnly(false)
	assert.Equal(t, []string{"root", "planner", "reviewer"}, rosterOwners(m),
		"turning the filter off restores the whole team")
}

// TestActiveAgentsOnly_CurrentAgentFallback verifies the roster never goes
// empty: with no participation recorded anywhere, the current agent still
// renders, and the header reads the singular "Agent" for the filtered roster.
func TestActiveAgentsOnly_CurrentAgentFallback(t *testing.T) {
	t.Parallel()

	m := newCompactPanelSidebar(t, 40, activeAgentsRoster()...)
	m.SetActiveAgentsOnly(true)

	assert.Equal(t, []string{"root"}, rosterOwners(m))

	title := renderAgentPanel(m)[0]
	assert.Contains(t, title, "Agent")
	assert.NotContains(t, title, "Agents", "the header counts the filtered roster")
}

// TestActiveAgentsOnly_WorkingAgentVisible verifies an agent that is
// currently working counts as active even before its first usage snapshot.
func TestActiveAgentsOnly_WorkingAgentVisible(t *testing.T) {
	t.Parallel()

	m := newCompactPanelSidebar(t, 40, activeAgentsRoster()...)
	m.SetActiveAgentsOnly(true)
	m.workingAgent = "reviewer"

	assert.Equal(t, []string{"root", "reviewer"}, rosterOwners(m))
}

// TestActiveAgentsOnly_RestoredCostCountsAsParticipation verifies an agent
// whose participation only exists as restored per-message cost (a reloaded
// session) stays on the filtered roster.
func TestActiveAgentsOnly_RestoredCostCountsAsParticipation(t *testing.T) {
	t.Parallel()

	m := newCompactPanelSidebar(t, 40, activeAgentsRoster()...)
	m.SetActiveAgentsOnly(true)

	sess := session.New()
	sess.Messages = append(sess.Messages, restoredItem("reviewer", 0.05))
	m.LoadFromSession(sess)

	assert.Equal(t, []string{"root", "reviewer"}, rosterOwners(m),
		"restored attributed cost is participation evidence; planner stays hidden")
}

// TestActiveAgentsOnly_TransferParticipantsVisibleWhileRelevant verifies both
// ends of an in-flight transfer stay on the filtered roster, through the
// Return presentation, and drop off once the presentation clears (absent any
// other participation evidence).
func TestActiveAgentsOnly_TransferParticipantsVisibleWhileRelevant(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, activeAgentsRoster()...)
	m.SetActiveAgentsOnly(true)
	require.Equal(t, []string{"root"}, rosterOwners(m))

	m.SetAgentSwitching(true, "root", "reviewer")
	assert.Equal(t, []string{"root", "reviewer"}, rosterOwners(m),
		"the destination of an in-flight hop is active")

	m.SetAgentSwitching(false, "reviewer", "root")
	assert.Equal(t, []string{"root", "reviewer"}, rosterOwners(m),
		"the Return presentation keeps the participant visible")

	expireReturn(t, m)
	assert.Equal(t, []string{"root"}, rosterOwners(m),
		"once the presentation clears, an evidence-less participant drops off")
}

// TestActiveAgentsOnly_PreservesShortcutIndices verifies a filtered roster
// keeps each agent's original team index for the ^N switch shortcut, so the
// shortcuts keep addressing the same team positions.
func TestActiveAgentsOnly_PreservesShortcutIndices(t *testing.T) {
	t.Parallel()

	m := newCompactPanelSidebar(t, 40, activeAgentsRoster()...)
	recordAgentUsageWithCost(m, "s-reviewer", "reviewer", 10_000, 100_000, 0.01)
	m.SetActiveAgentsOnly(true)

	require.Equal(t, []string{"root", "reviewer"}, rosterOwners(m))
	line1, _ := agentLines(m, "reviewer")
	assert.Contains(t, line1, "^3", "reviewer keeps its original team shortcut")
	assert.NotContains(t, line1, "^2")
}

// TestActiveAgentsOnly_CollapsedBandFiltered verifies the top/bottom band's
// agent summary honors the filter like the vertical panel.
func TestActiveAgentsOnly_CollapsedBandFiltered(t *testing.T) {
	t.Parallel()

	m := newCompactPanelSidebar(t, 40, activeAgentsRoster()...)
	recordAgentUsageWithCost(m, "s-planner", "planner", 10_000, 100_000, 0.01)

	info := ansi.Strip(m.collapsedInfoLine(120))
	require.NotContains(t, info, "reviewer", "the band does not list unused team agents")

	m.SetActiveAgentsOnly(true)
	info = ansi.Strip(m.collapsedInfoLine(120))
	assert.Contains(t, info, "▶ root")
	assert.NotContains(t, info, "planner", "historical participation does not invent a tree node")
	assert.NotContains(t, info, "reviewer")
}

// TestActiveAgentsOnly_DetailedSizingUsesFilteredRoster verifies the detailed
// cards' roster-wide vocabulary choice scans only the filtered roster: a
// hidden agent whose joined metric line would overflow no longer forces every
// visible card into the compact vocabulary.
func TestActiveAgentsOnly_DetailedSizingUsesFilteredRoster(t *testing.T) {
	t.Parallel()

	// At width 40 the joined wide metrics of a thinking agent overflow the
	// metric column while a no-thinking agent's fit.
	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus"},
		runtime.AgentDetails{Name: "verbose", Provider: "openai", Model: "gpt-5.4", Thinking: "high"},
	)

	assert.Contains(t, agentMetrics(m, "root"), "Ctx —",
		"the overflowing teammate forces the compact vocabulary")

	m.SetActiveAgentsOnly(true)
	assert.Contains(t, agentMetrics(m, "root"), "Context —",
		"with the teammate filtered out, the wide vocabulary fits again")
}

// TestSetActiveAgentsOnly_NoopWhenUnchanged mirrors the other section-setting
// setters: reapplying the same value must not invalidate the render cache.
func TestSetActiveAgentsOnly_NoopWhenUnchanged(t *testing.T) {
	t.Parallel()

	s := newTestSidebar(t)
	s.renderSections(40)
	s.cacheDirty = false

	s.SetActiveAgentsOnly(false)
	assert.False(t, s.cacheDirty, "identical value must not invalidate the cache")

	s.SetActiveAgentsOnly(true)
	assert.True(t, s.cacheDirty)
}
