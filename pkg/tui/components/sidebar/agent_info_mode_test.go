package sidebar

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// newCompactPanelSidebar builds a sidebar like newAgentPanelSidebar but keeps
// the default (zero-value) agent info mode: the compact two-line roster.
func newCompactPanelSidebar(t *testing.T, width int, agents ...runtime.AgentDetails) *model {
	t.Helper()
	sess := session.New()
	ss := service.NewSessionState(sess)
	ss.SetCurrentAgentName("root")
	m := New(animation.NewRuntime(), t.Context(), ss).(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.sessionTitle = "Test"
	m.currentAgent = "root"
	m.availableAgents = agents
	m.width = width
	m.height = 200
	t.Cleanup(m.transferAnimation.Stop)
	return m
}

// infoModeFixtureRoster is the fixed roster used by the exact-output tests:
// an effort level, a disabled thinking config, a token budget, and a model
// with no thinking configuration.
func infoModeFixtureRoster() []runtime.AgentDetails {
	return []runtime.AgentDetails{
		{Name: "root", Provider: "anthropic", Model: "claude-opus-4-8", Thinking: "high"},
		{Name: "helper", Provider: "openai", Model: "gpt-5.4-mini", Thinking: "off"},
		{Name: "budget", Provider: "openai", Model: "gpt-4o", Thinking: "8192"},
		{Name: "plain", Provider: "google", Model: "gemini-flash"},
	}
}

// recordInfoModeFixtureUsage feeds usage for root (30%) and helper (91%) with
// attributed costs; budget and plain never run.
func recordInfoModeFixtureUsage(m *model) {
	recordAgentUsageWithCost(m, "s-root", "root", 30_000, 100_000, 0.13)
	recordAgentUsageWithCost(m, "s-helper", "helper", 91_000, 100_000, 0.02)
}

// trimmedAgentBody returns the ANSI-stripped panel body lines with trailing
// padding removed, for exact-output comparisons.
func trimmedAgentBody(m *model) []string {
	body := agentBody(m)
	trimmed := make([]string, len(body))
	for i, line := range body {
		trimmed[i] = strings.TrimRight(line, " ")
	}
	return trimmed
}

// TestAgentInfoModeExactOutputs pins the exact roster rendering at the default
// (40) and minimum (20) sidebar widths. Both info modes render the same two
// rows per agent — the marker and name, then the bare model — because the
// labeled metrics moved to the expanded per-agent usage block (see
// agentUsageDetails). Every model fits at both widths, so none is truncated.
func TestAgentInfoModeExactOutputs(t *testing.T) {
	t.Parallel()

	want := []string{
		"▶ root",
		"  claude-opus-4-8",
		"",
		"  helper",
		"  gpt-5.4-mini",
		"",
		"  budget",
		"  gpt-4o",
		"",
		"  plain",
		"  gemini-flash",
	}
	tests := []struct {
		name  string
		mode  AgentInfoMode
		width int
	}{
		{name: "compact width 40", mode: AgentInfoCompact, width: 40},
		{name: "compact width 20", mode: AgentInfoCompact, width: 20},
		{name: "detailed width 40", mode: AgentInfoDetailed, width: 40},
		{name: "detailed width 20", mode: AgentInfoDetailed, width: 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newCompactPanelSidebar(t, tt.width, infoModeFixtureRoster()...)
			m.SetAgentInfoMode(tt.mode)
			recordInfoModeFixtureUsage(m)

			assert.Equal(t, want, trimmedAgentBody(m))
		})
	}
}

// / TestCompactRosterIsDefault verifies the zero-value sidebar renders the
// two-row roster: two owned rows per agent, with no labeled metric, cost,
// context percent, or shortcut on any roster row.
func TestCompactRosterIsDefault(t *testing.T) {
	t.Parallel()

	m := newCompactPanelSidebar(t, 40, infoModeFixtureRoster()...)
	recordInfoModeFixtureUsage(m)

	body := strings.Join(agentBody(m), "\n")
	assert.NotContains(t, body, "Effort", "compact roster carries no labeled effort metric")
	assert.NotContains(t, body, "Eff ", "compact roster carries no compact effort metric")
	assert.NotContains(t, body, "Cost", "compact roster carries no cost display")
	assert.NotContains(t, body, "Context", "compact roster carries no context label")
	assert.NotContains(t, body, "30%", "compact roster carries no context percent")
	assert.NotContains(t, body, "^", "compact roster carries no shortcut")

	counts := map[string]int{}
	for _, owner := range m.agentLineOwners {
		if owner != "" {
			counts[owner]++
		}
	}
	for _, name := range []string{"root", "helper", "budget", "plain"} {
		assert.Equalf(t, 2, counts[name], "compact agent %q owns exactly its two rows", name)
	}
}

// / TestRosterRowsOmitThinkingBadge verifies the roster carries no thinking badge:
// effort gauges, token budgets and "auto" stay off the name row. The effort
// vocabulary itself is pinned on the expanded usage block (see
// TestEffortVocabularyOnCard).
func TestRosterRowsOmitThinkingBadge(t *testing.T) {
	t.Parallel()

	m := newCompactPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
		runtime.AgentDetails{Name: "alpha", Provider: "openai", Model: "gpt-5.4-mini", Thinking: "off"},
		runtime.AgentDetails{Name: "beta", Provider: "openai", Model: "gpt-5.4", Thinking: "high"},
		runtime.AgentDetails{Name: "gamma", Provider: "openai", Model: "gpt-4o", Thinking: "8192"},
		runtime.AgentDetails{Name: "delta", Provider: "google", Model: "gemini", Thinking: "adaptive"},
	)

	rows := map[string]string{
		"root":  "▶ root",
		"alpha": "  alpha",
		"beta":  "  beta",
		"gamma": "  gamma",
		"delta": "  delta",
	}
	for name, row := range rows {
		line1, _ := agentLines(m, name)
		assert.Equalf(t, row, strings.TrimRight(line1, " "), "name row for %q carries no badge", name)
	}
	body := strings.Join(agentBody(m), "\n")
	assert.NotContains(t, body, gaugePattern(0), "roster carries no effort gauge")
	assert.NotContains(t, body, styles.TokenGlyph, "roster carries no token budget glyph")
	assert.NotContains(t, body, "auto", "roster carries no adaptive label")
}

// / TestRosterRowsNearMinWidth verifies that near the minimum width the roster
// rows still render the bare name and the full model, with no badge to collapse.
func TestRosterRowsNearMinWidth(t *testing.T) {
	t.Parallel()

	m := newCompactPanelSidebar(t, 21,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
		runtime.AgentDetails{Name: "agent2", Provider: "anthropic", Model: "claude-sonnet-4-6", Thinking: "high"},
	)

	line1, line2 := agentLines(m, "agent2")
	assert.Equal(t, "  agent2", strings.TrimRight(line1, " "))
	assert.Equal(t, "  claude-sonnet-4-6", strings.TrimRight(line2, " "))
}

// TestCompactClickZonesEveryLine verifies both compact lines of every agent
// resolve to that agent through the full View/click pipeline.
func TestCompactClickZonesEveryLine(t *testing.T) {
	t.Parallel()

	sess := session.New()
	ss := service.NewSessionState(sess)
	ss.SetCurrentAgentName("root")
	sb := New(animation.NewRuntime(), t.Context(), ss)
	m := sb.(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.sessionTitle = "Test"
	m.currentAgent = "root"
	m.availableAgents = []runtime.AgentDetails{
		{Name: "first", Provider: "openai", Model: "gpt-5.4-mini", Thinking: "off"},
		{Name: "root", Provider: "anthropic", Model: "claude-opus-4-8", Thinking: "high"},
	}
	m.width = 40
	m.height = 200

	_ = sb.View()

	clicks := map[string]int{}
	for y := range len(m.cachedLines) {
		if result, name := sb.HandleClickType(m.layoutCfg.PaddingLeft+2, y); result == ClickAgent {
			clicks[name]++
		}
	}
	assert.Equal(t, 2, clicks["root"], "both compact lines of the current agent are clickable")
	assert.Equal(t, 2, clicks["first"], "both compact lines of the other agent are clickable")
}

// / TestSetAgentInfoMode_LiveSwitch verifies switching modes invalidates the
// render cache, that costs tracked while compact surface immediately in the
// expanded usage details after switching to detailed, and that the roster rows
// stay identical in both modes.
func TestSetAgentInfoMode_LiveSwitch(t *testing.T) {
	t.Parallel()

	m := newCompactPanelSidebar(t, 40, infoModeFixtureRoster()...)
	recordInfoModeFixtureUsage(m)

	rendered := func() string {
		return ansi.Strip(strings.Join(m.renderSections(m.contentWidth(false)), "\n"))
	}
	ownedBy := func(name string) int {
		count := 0
		for _, owner := range m.agentLineOwners {
			if owner == name {
				count++
			}
		}
		return count
	}

	require.NotContains(t, rendered(), "Cost $0.13", "compact mode shows no per-agent cost")
	require.Equal(t, 2, ownedBy("root"))

	m.cacheDirty = false
	m.SetAgentInfoMode(AgentInfoDetailed)
	assert.True(t, m.cacheDirty, "switching the mode must invalidate the cache")

	assert.Contains(t, rendered(), "Cost $0.13",
		"costs tracked while compact surface in the expanded usage details after switching to detailed")
	assert.NotContains(t, strings.Join(agentBody(m), "\n"), "Cost",
		"the roster rows stay identical in detailed mode")
	assert.Equal(t, 2, ownedBy("root"), "click-zone ownership of the roster stays two rows per agent")

	m.SetAgentInfoMode(AgentInfoCompact)
	assert.NotContains(t, rendered(), "Cost $0.13", "switching back removes the per-agent usage details")
}

// TestSetAgentInfoMode_NoopWhenUnchanged verifies reapplying the current mode
// does not invalidate the render cache.
func TestSetAgentInfoMode_NoopWhenUnchanged(t *testing.T) {
	t.Parallel()

	s := newTestSidebar(t)
	s.renderSections(40)
	s.cacheDirty = false

	s.SetAgentInfoMode(AgentInfoCompact)
	assert.False(t, s.cacheDirty, "identical mode must not invalidate the cache")

	s.SetAgentInfoMode(AgentInfoDetailed)
	assert.True(t, s.cacheDirty)
}
