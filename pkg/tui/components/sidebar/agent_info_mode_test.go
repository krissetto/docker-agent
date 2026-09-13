package sidebar

import (
	"strings"
	"testing"

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

// TestAgentInfoModeExactOutputs pins the exact roster rendering of both info
// modes at the default (40) and minimum (20) sidebar widths. The compact
// lines reproduce the pre-card roster: two lines per agent, the thinking
// badge right-aligned on line 1 (single-cell glyph near MinWidth), the bare
// context percent right-aligned on line 2, and no cost display. The detailed
// cards adapt their metric vocabulary to the roster: root's preferred wide
// joined line ("Effort <gauge> high · Context 30% · Cost $0.13", 45 columns)
// overflows both widths, so every card compacts — at 40 each card joins its
// metrics on a single line (a three-line card), at 20 the compact segments
// pack greedily across lines.
func TestAgentInfoModeExactOutputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		mode  AgentInfoMode
		width int
		want  []string
	}{
		{
			name: "compact width 40", mode: AgentInfoCompact, width: 40,
			want: []string{
				"▶ root                        ▰▰▰▰▱▱ ^1",
				"  anthropic/claude-opus-4-8         30%",
				"",
				"  helper                      ▱▱▱▱▱▱ ^2",
				"  openai/gpt-5.4-mini               91%",
				"",
				"  budget                      ◉ 8.2K ^3",
				"  openai/gpt-4o",
				"",
				"  plain                              ^4",
				"  google/gemini-flash",
			},
		},
		{
			name: "compact width 20", mode: AgentInfoCompact, width: 20,
			want: []string{
				"▶ root         ▰ ^1",
				"  …ude-opus-4-8 30%",
				"",
				"  helper       ▱ ^2",
				"  …gpt-5.4-mini 91%",
				"",
				"  budget       ◉ ^3",
				"  openai/gpt-4o",
				"",
				"  plain          ^4",
				"  …gle/gemini-flash",
			},
		},
		{
			name: "detailed width 40", mode: AgentInfoDetailed, width: 40,
			want: []string{
				"▶ root                               ^1",
				"  anthropic/claude-opus-4-8",
				"  Eff high · Ctx 30% · Cost $0.13",
				"",
				"  helper                             ^2",
				"  openai/gpt-5.4-mini",
				"  Eff off · Ctx 91% · Cost $0.02",
				"",
				"  budget                             ^3",
				"  openai/gpt-4o",
				"  Eff ◉ 8.2K · Ctx — · Cost —",
				"",
				"  plain                              ^4",
				"  google/gemini-flash",
				"  Ctx — · Cost —",
			},
		},
		{
			name: "detailed width 20", mode: AgentInfoDetailed, width: 20,
			want: []string{
				"▶ root           ^1",
				"  …/claude-opus-4-8",
				"  Eff high",
				"  Ctx 30%",
				"  Cost $0.13",
				"",
				"  helper         ^2",
				"  …nai/gpt-5.4-mini",
				"  Eff off · Ctx 91%",
				"  Cost $0.02",
				"",
				"  budget         ^3",
				"  openai/gpt-4o",
				"  Eff ◉ 8.2K",
				"  Ctx — · Cost —",
				"",
				"  plain          ^4",
				"  …gle/gemini-flash",
				"  Ctx — · Cost —",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newCompactPanelSidebar(t, tt.width, infoModeFixtureRoster()...)
			m.SetAgentInfoMode(tt.mode)
			recordInfoModeFixtureUsage(m)

			assert.Equal(t, tt.want, trimmedAgentBody(m))
		})
	}
}

// TestCompactRosterIsDefault verifies the zero-value sidebar renders the
// compact roster: two owned lines per agent and none of the detailed card's
// labeled metrics.
func TestCompactRosterIsDefault(t *testing.T) {
	t.Parallel()

	m := newCompactPanelSidebar(t, 40, infoModeFixtureRoster()...)
	recordInfoModeFixtureUsage(m)

	body := strings.Join(agentBody(m), "\n")
	assert.NotContains(t, body, "Effort", "compact roster carries no labeled effort metric")
	assert.NotContains(t, body, "Cost", "compact roster carries no cost display")
	assert.NotContains(t, body, "Context", "compact roster shows the bare percent, not a label")
	assert.Contains(t, body, "30%", "context percent still shows on the model line")

	counts := map[string]int{}
	for _, owner := range m.agentLineOwners {
		if owner != "" {
			counts[owner]++
		}
	}
	for _, name := range []string{"root", "helper", "budget", "plain"} {
		assert.Equalf(t, 2, counts[name], "compact agent %q owns exactly its two lines", name)
	}
}

// TestCompactThinkingBadgeVocabularyOnLine verifies the compact roster's
// thinking badge vocabulary renders on the agent's name line: effort levels
// become the gauge, token budgets keep the token glyph, adaptive becomes
// "auto", and off becomes an empty gauge.
func TestCompactThinkingBadgeVocabularyOnLine(t *testing.T) {
	t.Parallel()

	m := newCompactPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
		runtime.AgentDetails{Name: "alpha", Provider: "openai", Model: "gpt-5.4-mini", Thinking: "off"},
		runtime.AgentDetails{Name: "beta", Provider: "openai", Model: "gpt-5.4", Thinking: "high"},
		runtime.AgentDetails{Name: "gamma", Provider: "openai", Model: "gpt-4o", Thinking: "8192"},
		runtime.AgentDetails{Name: "delta", Provider: "google", Model: "gemini", Thinking: "adaptive"},
	)

	want := map[string]string{
		"alpha": gaugePattern(0),
		"beta":  gaugePattern(4),
		"gamma": styles.TokenGlyph + " 8.2K",
		"delta": "auto",
	}
	for name, badge := range want {
		line1, _ := agentLines(m, name)
		require.NotEmptyf(t, line1, "row for %q should render", name)
		assert.Containsf(t, line1, badge, "row %q should show badge %q", name, badge)
	}
}

// TestCompactNameTruncatesNearMinWidth verifies that at a narrow width the
// compact roster collapses the gauge to a single cell while the model still
// occupies line 2.
func TestCompactNameTruncatesNearMinWidth(t *testing.T) {
	t.Parallel()

	m := newCompactPanelSidebar(t, 21,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
		runtime.AgentDetails{Name: "agent2", Provider: "anthropic", Model: "claude-sonnet-4-6", Thinking: "high"},
	)

	line1, line2 := agentLines(m, "agent2")
	require.NotEmpty(t, line1)
	// glyph-only step keeps a single filled gauge cell, not the full six-cell gauge.
	assert.NotContains(t, line1, gaugePattern(4), "narrow layout collapses the full gauge")
	assert.Contains(t, line1, styles.GaugeFilled, "narrow layout keeps a single gauge cell")
	assert.Contains(t, line2, "…", "narrow layout left-truncates the model on line 2")
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
	assert.Zero(t, clicks["root"], "the active agent does not replace the canonical root")
	assert.Zero(t, clicks["first"], "configured agents are not descendant rows")
}

// TestSetAgentInfoMode_LiveSwitch verifies switching modes invalidates the
// render cache, that costs tracked while compact surface immediately in
// detailed, and that click-zone ownership follows the active mode.
func TestSetAgentInfoMode_LiveSwitch(t *testing.T) {
	t.Parallel()

	m := newCompactPanelSidebar(t, 40, infoModeFixtureRoster()...)
	recordInfoModeFixtureUsage(m)

	require.NotContains(t, strings.Join(agentBody(m), "\n"), "Cost",
		"compact mode shows no cost")
	compactOwned := 0
	for _, owner := range m.agentLineOwners {
		if owner == "root" {
			compactOwned++
		}
	}
	require.Equal(t, 2, compactOwned)

	m.cacheDirty = false
	m.SetAgentInfoMode(AgentInfoDetailed)
	assert.True(t, m.cacheDirty, "switching the mode must invalidate the cache")

	body := strings.Join(agentBody(m), "\n")
	assert.Contains(t, body, "Cost $0.13",
		"costs tracked while compact surface immediately after switching to detailed")
	detailedOwned := 0
	for _, owner := range m.agentLineOwners {
		if owner == "root" {
			detailedOwned++
		}
	}
	assert.Equal(t, 3, detailedOwned,
		"click-zone ownership follows the detailed card lines: name, model, one joined metric line")

	m.SetAgentInfoMode(AgentInfoCompact)
	assert.NotContains(t, strings.Join(agentBody(m), "\n"), "Cost",
		"switching back restores the compact roster")
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
