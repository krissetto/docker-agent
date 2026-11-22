package sidebar

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

// recordAgentUsageWithCost feeds a TokenUsageEvent carrying a cumulative cost
// snapshot through the sidebar's normal entry point.
func recordAgentUsageWithCost(m *model, sessionID, agentName string, contextLen, contextLimit int64, cost float64) {
	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    sessionID,
		AgentContext: runtime.AgentContext{AgentName: agentName},
		Usage: &runtime.Usage{
			InputTokens:   contextLen / 2,
			OutputTokens:  contextLen - contextLen/2,
			ContextLength: contextLen,
			ContextLimit:  contextLimit,
			Cost:          cost,
		},
	})
}

// restoredItem returns a persisted assistant-message item: agent name,
// per-message cost, and its usage record (present even at zero cost, as the
// runtime persists one for every assistant response).
func restoredItem(agentName string, cost float64) session.Item {
	return session.Item{Message: &session.Message{
		AgentName: agentName,
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "done",
			Cost:    cost,
			Usage:   &chat.Usage{InputTokens: 1000, OutputTokens: 200},
		},
	}}
}

// TestAgentCardShowsPerAgentCost verifies each card carries the agent's own
// labeled cumulative cost: repeated cumulative snapshots of one session
// replace rather than add, distinct sessions (sub-sessions, background tasks)
// add, an agent that ran at zero cost reads $0.00, and an agent that never
// ran reads an explicit —.
func TestAgentCardShowsPerAgentCost(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
		runtime.AgentDetails{Name: "worker", Provider: "openai", Model: "gpt-5.4"},
		runtime.AgentDetails{Name: "idle", Provider: "openai", Model: "gpt-4o"},
	)

	// Two cumulative snapshots of the same session, then a second session.
	recordAgentUsageWithCost(m, "session-1", "root", 10_000, 100_000, 0.10)
	recordAgentUsageWithCost(m, "session-1", "root", 30_000, 100_000, 0.30)
	recordAgentUsageWithCost(m, "session-2", "root", 5_000, 100_000, 0.05)

	// A background worker that ran for free.
	recordAgentUsageWithCost(m, "bg-session", "worker", 8_000, 100_000, 0)

	rootMetrics := agentMetrics(m, "root")
	assert.Contains(t, rootMetrics, "Cost $0.35",
		"same-session snapshots replace, distinct sessions add: got %q", rootMetrics)

	assert.Contains(t, agentMetrics(m, "worker"), "Cost $0.00",
		"an agent that ran at zero cost reads $0.00, not —")

	idleMetrics := agentMetrics(m, "idle")
	assert.Contains(t, idleMetrics, "Cost —", "an agent that never ran reads —")
	assert.NotContains(t, idleMetrics, "$", "an agent that never ran shows no dollar amount")
}

// TestAgentCardCostPrecision verifies the card uses the shared precise cost
// formatting: four decimals for non-zero amounts below one cent, two decimals
// at or above.
func TestAgentCardCostPrecision(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus"},
		runtime.AgentDetails{Name: "cheap", Provider: "openai", Model: "gpt-5.4-mini"},
	)

	recordAgentUsageWithCost(m, "session-root", "root", 30_000, 100_000, 0.1284)
	recordAgentUsageWithCost(m, "session-cheap", "cheap", 2_000, 100_000, 0.0042)

	assert.Contains(t, agentMetrics(m, "root"), "Cost $0.13")
	assert.Contains(t, agentMetrics(m, "cheap"), "Cost $0.0042")
}

// TestAgentCardCostRestoredFromSession verifies costs reconstructed from a
// restored session tree appear on the cards immediately: every agent that
// left per-message records — including in nested sub-sessions — shows its
// exact historical spend, a zero-cost run stays distinct from never-ran,
// unattributed compaction spend appears on no card but stays in the
// aggregate total, and per-agent context remains unknown rather than
// fabricated.
func TestAgentCardCostRestoredFromSession(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus"},
		runtime.AgentDetails{Name: "developer", Provider: "openai", Model: "gpt-5.4"},
		runtime.AgentDetails{Name: "writer", Provider: "google", Model: "gemini-flash"},
		runtime.AgentDetails{Name: "local", Provider: "dmr", Model: "llama3.2"},
		runtime.AgentDetails{Name: "idle", Provider: "openai", Model: "gpt-4o"},
	)

	// root ($0.10), a zero-cost local run and a compaction summary ($0.01) in
	// the root session; developer ($0.05) in a sub-session holding a nested
	// sub-sub-session for writer ($0.02).
	sess := session.New()
	sess.Messages = append(sess.Messages,
		restoredItem("root", 0.10),
		restoredItem("local", 0),
		session.Item{Summary: "compacted", Cost: 0.01},
	)
	subSub := session.New()
	subSub.Messages = append(subSub.Messages, restoredItem("writer", 0.02))
	sub := session.New(session.WithParentID(sess.ID))
	sub.Messages = append(sub.Messages, restoredItem("developer", 0.05), session.Item{SubSession: subSub})
	sess.Messages = append(sess.Messages, session.Item{SubSession: sub})

	m.LoadFromSession(sess)

	assert.Contains(t, agentMetrics(m, "root"), "Cost $0.10")
	assert.Contains(t, agentMetrics(m, "developer"), "Cost $0.05", "sub-session spend shows on its agent")
	assert.Contains(t, agentMetrics(m, "writer"), "Cost $0.02", "nested sub-sub-session spend shows too")
	assert.Contains(t, agentMetrics(m, "local"), "Cost $0.00", "a restored zero-cost run reads $0.00")
	idle := agentMetrics(m, "idle")
	assert.Contains(t, idle, "Cost —", "an agent absent from the restored tree reads —")
	assert.NotContains(t, idle, "$")

	for _, name := range []string{"root", "developer", "writer", "local", "idle"} {
		assert.Containsf(t, agentMetrics(m, name), "Context —",
			"restored context is unknown for %q, never fabricated", name)
	}

	assert.InDelta(t, 0.18, m.computeUsageStats().totalCost, 1e-9,
		"the aggregate keeps the unattributed compaction spend no card shows")
}

// TestAgentCardCostRestoredThenLive verifies restored totals and live
// cumulative snapshots coexist without double counting: the resumed root
// session's snapshots cover the whole restored tree, so its agent gains only
// the new spend, while a fresh live sub-session adds in full.
func TestAgentCardCostRestoredThenLive(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus"},
		runtime.AgentDetails{Name: "developer", Provider: "openai", Model: "gpt-5.4"},
	)

	// Restored: root $0.10 + developer sub-session $0.05 + compaction $0.01.
	sess := session.New()
	sess.Messages = append(sess.Messages,
		restoredItem("root", 0.10),
		session.Item{Summary: "compacted", Cost: 0.01},
	)
	sub := session.New(session.WithParentID(sess.ID))
	sub.Messages = append(sub.Messages, restoredItem("developer", 0.05))
	sess.Messages = append(sess.Messages, session.Item{SubSession: sub})
	m.LoadFromSession(sess)

	// The root session resumes and spends $0.02 more: its live snapshot is
	// cumulative over the restored tree ($0.16) plus the new spend.
	recordAgentUsageWithCost(m, sess.ID, "root", 40_000, 100_000, 0.18)
	assert.Contains(t, agentMetrics(m, "root"), "Cost $0.12",
		"restored total plus only the spend beyond the restored baseline")
	assert.Contains(t, agentMetrics(m, "developer"), "Cost $0.05",
		"restored sub-session spend keeps its agent")
	assert.InDelta(t, 0.18, m.computeUsageStats().totalCost, 1e-9)

	// A fresh delegation runs in a new sub-session: new spend in full.
	recordAgentUsageWithCost(m, "live-sub", "developer", 8_000, 200_000, 0.03)
	assert.Contains(t, agentMetrics(m, "developer"), "Cost $0.08")
	assert.InDelta(t, 0.21, m.computeUsageStats().totalCost, 1e-9)
}

// TestAgentCardCostRepeatedRestoreAndSwitch verifies the replace semantics of
// LoadFromSession: a repeated load of the same session doubles nothing, and
// loading a different session drops every trace of the previous one — cards
// and aggregate alike.
func TestAgentCardCostRepeatedRestoreAndSwitch(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus"},
		runtime.AgentDetails{Name: "developer", Provider: "openai", Model: "gpt-5.4"},
	)

	first := session.New()
	first.Messages = append(first.Messages, restoredItem("root", 0.10))
	sub := session.New(session.WithParentID(first.ID))
	sub.Messages = append(sub.Messages, restoredItem("developer", 0.05))
	first.Messages = append(first.Messages, session.Item{SubSession: sub})

	m.LoadFromSession(first)
	m.LoadFromSession(first)
	assert.Contains(t, agentMetrics(m, "root"), "Cost $0.10", "no doubling on repeated load")
	assert.Contains(t, agentMetrics(m, "developer"), "Cost $0.05")
	assert.InDelta(t, 0.15, m.computeUsageStats().totalCost, 1e-9, "aggregate not doubled either")

	// Switching to another session leaves nothing of the first behind.
	second := session.New()
	second.Messages = append(second.Messages, restoredItem("root", 0.04))
	m.LoadFromSession(second)
	assert.Contains(t, agentMetrics(m, "root"), "Cost $0.04")
	developer := agentMetrics(m, "developer")
	assert.Contains(t, developer, "Cost —", "the previous session's spend is gone")
	assert.NotContains(t, developer, "$")
	assert.InDelta(t, 0.04, m.computeUsageStats().totalCost, 1e-9)
}

// / TestAgentCardSingleMetricsLineAtWideWidth verifies an agent's labeled metrics
// share one line in the expanded usage details when the sidebar is wide enough
// to hold them, beneath the agent's label row.
func TestAgentCardSingleMetricsLineAtWideWidth(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 56,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "claude-opus-4-8", Thinking: "high"},
	)
	recordAgentUsageWithCost(m, "session-root", "root", 30_000, 100_000, 0.1284)

	metrics := agentMetricLines(m, "root")
	require.Len(t, metrics, 1, "one combined metrics line in the usage details")
	assert.Equal(t, "Effort "+gaugePattern(4)+" high · Context 30% · Cost $0.13",
		strings.TrimSpace(metrics[0]))
}

// previewSidebar builds the deterministic three-agent roster used by the
// exact-output preview tests: the current agent with usage, a zero-effort
// agent with sub-cent usage, and an agent that never ran.
func previewSidebar(t *testing.T, width int) *model {
	t.Helper()
	m := newAgentPanelSidebar(t, width,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "claude-opus-4-8", Thinking: "high"},
		runtime.AgentDetails{Name: "scout", Provider: "openai", Model: "gpt-5.4-mini", Thinking: "off"},
		runtime.AgentDetails{Name: "writer", Provider: "google", Model: "gemini-flash"},
	)
	recordAgentUsageWithCost(m, "session-root", "root", 30_000, 100_000, 0.1284)
	recordAgentUsageWithCost(m, "session-scout", "scout", 12_000, 200_000, 0.0042)
	return m
}

// previewLines renders the Agents panel and strips the trailing-space padding
// the agent panel adds to some lines, leaving the visible content for exact
// comparison.
func previewLines(m *model) []string {
	lines := renderAgentPanel(m)
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.TrimRight(l, " ")
	}
	return out
}

// / TestAgentCardPreview_DefaultWidth pins the exact ANSI-stripped roster and the
// per-agent metric lines at the default sidebar width (40 → 39 content columns,
// 37 metric columns). The roster is the two-row entry for every agent; root's
// preferred wide joined metric line (45 columns) cannot fit, so the usage
// details adopt the compact vocabulary and join each agent's metrics on one line.
func TestAgentCardPreview_DefaultWidth(t *testing.T) {
	t.Parallel()

	m := previewSidebar(t, DefaultWidth)

	wantRoster := []string{
		"▶ root",
		"  claude-opus-4-8",
		"",
		"  scout",
		"  gpt-5.4-mini",
		"",
		"  writer",
		"  gemini-flash",
	}
	require.Equal(t, wantRoster, previewLines(m), "default-width roster changed")

	wantMetrics := map[string][]string{
		"root":   {"  Eff high · Ctx 30% · Cost $0.13"},
		"scout":  {"  Eff off · Ctx 6% · Cost $0.0042"},
		"writer": {"  Ctx — · Cost —"},
	}
	for name, want := range wantMetrics {
		assert.Equalf(t, want, agentMetricLines(m, name), "default-width usage details for %q", name)
	}
}

// / TestAgentCardPreview_MinWidth pins the exact ANSI-stripped roster and the
// per-agent metric lines at the minimum sidebar width (20 → 19 content columns,
// 17 metric columns): the roster rows stay the same two-row entries, while the
// usage details compact their labels to Eff/Ctx, keep the effort value in place
// of the gauge, and pack greedily — scout's "Eff off · Ctx 6%" shares a line
// while root's "Eff high · Ctx 30%" would overflow and wraps.
func TestAgentCardPreview_MinWidth(t *testing.T) {
	t.Parallel()

	m := previewSidebar(t, MinWidth)

	wantRoster := []string{
		"▶ root",
		"  claude-opus-4-8",
		"",
		"  scout",
		"  gpt-5.4-mini",
		"",
		"  writer",
		"  gemini-flash",
	}
	require.Equal(t, wantRoster, previewLines(m), "min-width roster changed")

	wantMetrics := map[string][]string{
		"root":   {"  Eff high", "  Ctx 30%", "  Cost $0.13"},
		"scout":  {"  Eff off · Ctx 6%", "  Cost $0.0042"},
		"writer": {"  Ctx — · Cost —"},
	}
	for name, want := range wantMetrics {
		assert.Equalf(t, want, agentMetricLines(m, name), "min-width usage details for %q", name)
	}
}

// / TestAgentCardPreviewLineInvariants guards the layout invariants behind the
// pinned previews at both reference widths: every roster row and every
// usage-details line fits the content width, and each roster entry carries
// exactly one model row.
func TestAgentCardPreviewLineInvariants(t *testing.T) {
	t.Parallel()

	// Informative model-name tails: they survive the right-truncation of the
	// model row at narrow widths and appear on no other roster row.
	modelTails := map[string]string{
		"root":   "claude-opus-4-8",
		"scout":  "gpt-5.4-mini",
		"writer": "gemini-flash",
	}
	for _, width := range []int{DefaultWidth, MinWidth} {
		m := previewSidebar(t, width)
		contentWidth := m.contentWidth(false)
		for _, line := range previewLines(m) {
			assert.LessOrEqualf(t, lipgloss.Width(line), contentWidth,
				"width %d: roster line %q must fit the content width", width, line)
		}
		for _, line := range agentUsageLinesOnly(m) {
			assert.LessOrEqualf(t, lipgloss.Width(line), contentWidth,
				"width %d: usage line %q must fit the content width", width, line)
		}
		for name, tail := range modelTails {
			modelRows := 0
			for _, line := range agentCard(m, name) {
				if strings.Contains(line, tail) {
					modelRows++
				}
			}
			assert.Equalf(t, 1, modelRows,
				"width %d: roster entry %q must contain exactly one model row", width, name)
		}
	}
}

// agentUsageLinesOnly returns just the usage-details lines, for width checks.
func agentUsageLinesOnly(m *model) []string {
	lines, _ := agentUsageLines(m)
	return lines
}
