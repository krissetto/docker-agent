package sidebar

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// recordUsageWithThreshold feeds a usage snapshot that carries the agent's
// configured compaction threshold, as live TokenUsageEvents do.
func (s *testSidebar) recordUsageWithThreshold(sessionID, agentName string, contextLen, contextLimit int64, threshold float64) {
	s.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    sessionID,
		AgentContext: runtime.AgentContext{AgentName: agentName},
		Usage: &runtime.Usage{
			InputTokens:         contextLen / 2,
			OutputTokens:        contextLen - contextLen/2,
			ContextLength:       contextLen,
			ContextLimit:        contextLimit,
			CompactionThreshold: threshold,
		},
	})
}

func TestUsageStats_ContextGaugeLevelEscalates(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.startStream("session-1", "root")

	// Default threshold (0.9): 50% is normal, 70% warns, 90% is critical.
	m.recordUsage("session-1", "root", 50_000, 100_000)
	assert.Equal(t, styles.ContextGaugeNormal, m.computeUsageStats().contextLevel)

	m.recordUsage("session-1", "root", 70_000, 100_000)
	assert.Equal(t, styles.ContextGaugeWarning, m.computeUsageStats().contextLevel)

	m.recordUsage("session-1", "root", 90_000, 100_000)
	assert.Equal(t, styles.ContextGaugeCritical, m.computeUsageStats().contextLevel)
}

func TestUsageStats_ContextGaugeLevelHonorsConfiguredThreshold(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.startStream("session-1", "root")

	// 50% of the window is well below the default threshold but past the
	// critical band of a 0.5 compaction_threshold.
	m.recordUsageWithThreshold("session-1", "root", 50_000, 100_000, 0.5)
	assert.Equal(t, styles.ContextGaugeCritical, m.computeUsageStats().contextLevel)
}

func TestAgentContextGaugeLevel_PerAgentSnapshots(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.SetAgentContextReadings(true) // readings are off by default
	m.startStream("session-root", "root")
	m.recordUsage("session-root", "root", 20_000, 100_000)
	m.recordUsageWithThreshold("session-child", "developer", 88_000, 100_000, 0.9)

	assert.Equal(t, styles.ContextGaugeNormal, m.agentContextGaugeLevel("root"))
	assert.Equal(t, styles.ContextGaugeCritical, m.agentContextGaugeLevel("developer"))
	assert.Equal(t, styles.ContextGaugeNormal, m.agentContextGaugeLevel("idle"), "agents that never ran stay normal")
}

// TestTokenUsageLine_CompactingIndicator verifies the gauge switches to a
// "compacting…" reading for the started→completed window of a compaction.
func TestTokenUsageLine_CompactingIndicator(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.startStream("session-1", "root")
	m.recordUsage("session-1", "root", 30_000, 100_000)

	// Pretend the spinner is already registered so Update's start/stop
	// paths never touch the global animation coordinator (startStream set
	// workingAgent, so needsSpinner stays true throughout).
	m.spinnerActive = true

	m.Update(&runtime.SessionCompactionEvent{SessionID: "session-1", Status: "started"})
	assert.True(t, m.compacting)
	line := ansi.Strip(m.tokenUsageLine())
	assert.Contains(t, line, "(compacting…)")
	assert.NotContains(t, line, "%", "the percentage yields to the compacting indicator")

	m.Update(&runtime.SessionCompactionEvent{SessionID: "session-1", Status: "completed", Outcome: runtime.CompactionOutcomeApplied})
	assert.False(t, m.compacting)
	line = ansi.Strip(m.tokenUsageLine())
	assert.Contains(t, line, "(30%)")
	assert.NotContains(t, line, "compacting")
}

func TestStreamCancelClearsCompacting(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.startStream("session-1", "root")
	m.compacting = true

	m.Update(messages.StreamCancelledMsg{})

	assert.False(t, m.compacting, "ESC cancel must not leave the gauge stuck on compacting…")
}

// TestCompactingGaugeIgnoresOtherSessions pins the #3439 sidebar contract:
// the "compacting…" gauge describes the displayed session only, so a
// targeted compaction of a session that is not displayed (e.g. an idle
// background agent session) must not flip it in either direction.
func TestCompactingGaugeIgnoresOtherSessions(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.startStream("session-1", "root")
	m.spinnerActive = true

	m.Update(&runtime.SessionCompactionEvent{SessionID: "background-1", Status: "started"})
	assert.False(t, m.compacting, "another session's compaction must not show the root gauge as compacting")

	m.Update(&runtime.SessionCompactionEvent{SessionID: "session-1", Status: "started"})
	assert.True(t, m.compacting, "the displayed session's compaction still drives the gauge")

	m.Update(&runtime.SessionCompactionEvent{SessionID: "background-1", Status: "completed", Outcome: runtime.CompactionOutcomeApplied})
	assert.True(t, m.compacting, "another session's completion must not clear the running gauge")

	m.Update(&runtime.SessionCompactionEvent{SessionID: "session-1", Status: "completed", Outcome: runtime.CompactionOutcomeApplied})
	assert.False(t, m.compacting)
}

// TestCompactingGaugeBeforeAnySessionKnown keeps the legacy behavior when no
// session has been recorded yet (fresh session, /compact before any stream):
// the event is applied rather than dropped.
func TestCompactingGaugeBeforeAnySessionKnown(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.workingAgent = "root" // keep needsSpinner true so Update never touches the coordinator
	m.spinnerActive = true

	m.Update(&runtime.SessionCompactionEvent{SessionID: "session-1", Status: "started"})
	assert.True(t, m.compacting)
}
