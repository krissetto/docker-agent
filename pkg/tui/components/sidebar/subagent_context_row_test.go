package sidebar

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func contextRowNode() subagent.Node {
	return subagent.Node{ID: "grand-full-canonical-node", SessionID: "grand", Agent: "worker", Name: "NestedReviewer", State: subagent.NodeCompleted, CreatedAt: time.Now().Add(-10*time.Minute - 30*time.Second)}
}

// Unknown agent: the status cell keeps the node state, as before.
func TestSubagentRowUnknownContextKeepsStateStatus(t *testing.T) {
	m := newSubagentTestModel(t)
	text, _ := m.subagentRowIdentity(contextRowNode(), "", 60, -1, false)
	require.Contains(t, ansi.Strip(text), "completed")
}

// Known agent: the gauge-styled reading sits directly after the name, and the
// state stays right-aligned as the status cell.
func TestSubagentRowKnownContextAppendsAfterState(t *testing.T) {
	m := newSubagentTestModel(t)
	recordAgentUsage(m, "grand", "worker", 30_000, 100_000)
	reading := m.agentContextPercent("worker")
	require.NotEmpty(t, reading, "the recorded usage yields a reading")
	text, _ := m.subagentRowIdentity(contextRowNode(), "", 60, -1, false)
	plain := ansi.Strip(text)
	require.Contains(t, plain, "completed")
	require.True(t, strings.HasSuffix(strings.TrimRight(plain, " "), "completed"), "the state is the rightmost cell")
	require.Contains(t, plain, "NestedReviewer "+reading, "the reading sits directly after the name")
	require.Contains(t, text, contextGaugeStyle(m.agentContextGaugeLevel("worker"), styles.MutedStyle).Render(reading))
}

// The state is never dropped for the reading, at any width.
func TestSubagentRowStateNeverDroppedForContext(t *testing.T) {
	for _, width := range []int{16, 20, 30, 60, 120} {
		m := newSubagentTestModel(t)
		recordAgentUsage(m, "grand", "worker", 30_000, 100_000)
		text, _ := m.subagentRowIdentity(contextRowNode(), "", width, -1, false)
		require.Contains(t, ansi.Strip(text), "completed", "width %d: the state stays", width)
	}
}

// Hover reveals the age and drops the reading first, as the ID and age priority requires.
func TestSubagentRowHoverDropsContextReadingForAge(t *testing.T) {
	m := newSubagentTestModel(t)
	recordAgentUsage(m, "grand", "worker", 30_000, 100_000)
	reading := m.agentContextPercent("worker")
	m.hoveredSubagent = contextRowNode().ID
	text, _ := m.subagentRowIdentity(contextRowNode(), "", 60, -1, false)
	plain := ansi.Strip(text)
	require.Contains(t, plain, "10m ago")
	// The reading stays beside the name on hover: hover must not move the name's cells.
	require.Contains(t, plain, "NestedReviewer "+reading)
}

// Critical usage styles the reading with the shared warning colors.
func TestSubagentRowCriticalContextUsesWarningGauge(t *testing.T) {
	m := newSubagentTestModel(t)
	recordAgentUsage(m, "grand", "worker", 99_000, 100_000)
	require.Equal(t, styles.ContextGaugeCritical, m.agentContextGaugeLevel("worker"))
	reading := m.agentContextPercent("worker")
	text, _ := m.subagentRowIdentity(contextRowNode(), "", 60, -1, false)
	require.Contains(t, text, styles.ErrorStyle.Render(reading))
}

// The reading never costs name cells: at every width the resting name span is
// identical with and without a known reading, whether shown or dropped.
func TestSubagentRowContextNeverShrinksRestingNameSpan(t *testing.T) {
	for _, width := range []int{16, 20, 30, 60, 120} {
		without := newSubagentTestModel(t)
		_, base := without.subagentRowIdentity(contextRowNode(), "", width, -1, true)
		with := newSubagentTestModel(t)
		recordAgentUsage(with, "grand", "worker", 30_000, 100_000)
		_, got := with.subagentRowIdentity(contextRowNode(), "", width, -1, true)
		require.Equal(t, base, got, "width %d: the reading cannot move the resting name hit", width)
	}
}

// A reading that would truncate the name is dropped in favor of the state.
func TestSubagentRowNarrowContextDropsBeforeNameTruncation(t *testing.T) {
	m := newSubagentTestModel(t)
	recordAgentUsage(m, "grand", "worker", 30_000, 100_000)
	reading := m.agentContextPercent("worker")
	text, _ := m.subagentRowIdentity(contextRowNode(), "", 16, -1, false)
	plain := ansi.Strip(text)
	require.NotContains(t, plain, reading, "a narrow row drops the reading first")
	require.Contains(t, plain, "completed", "the state stays when the reading is dropped")
}
