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
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// readingsNode is a same-named "developer" subagent running in its own session.
func readingsNode(id, sessionID, name string, state subagent.NodeState) subagent.Node {
	return subagent.Node{ID: subagent.NodeID(id), SessionID: sessionID, Agent: "developer", Name: name, State: state, CreatedAt: time.Now().Add(-5 * time.Minute)}
}

// Readings are off by default: no row, participant line, or roster card shows a
// percentage, and the roster reserves no context cell.
func TestAgentContextReadingsDefaultOffRendersNoPercent(t *testing.T) {
	t.Parallel()

	m := New(animation.NewRuntime(), t.Context(), &service.SessionState{}).(*model)
	m.subagentSpinner = &fakeSpinner{m.subagentSpinner}
	recordAgentUsage(m, "s-a", "developer", 42_000, 100_000)

	text, _ := m.subagentRowIdentity(readingsNode("n1", "s-a", "alpha", subagent.NodeRunning), "", 60, -1, false)
	assert.NotContains(t, ansi.Strip(text), "%")
	assert.Contains(t, ansi.Strip(text), "running", "the state still shows")
	assert.NotContains(t, ansi.Strip(m.participantLine("developer", 40)), "%")

	roster := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus"},
		runtime.AgentDetails{Name: "developer", Provider: "openai", Model: "gpt-5.4"},
	)
	roster.SetAgentContextReadings(false)
	recordAgentUsage(roster, "s-b", "developer", 42_000, 100_000)
	metrics := agentMetrics(roster, "developer")
	assert.NotContains(t, metrics, "%")
	assert.NotContains(t, metrics, "Ctx")
	assert.NotContains(t, metrics, "Context", "the card reserves no context segment")
	assert.NotContains(t, ansi.Strip(strings.Join(agentCard(roster, "developer"), "\n")), "%")
}

// The reading sits directly after the name; the state stays right-aligned.
func TestAgentContextReadingSitsAfterNameWhenEnabled(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	recordAgentUsage(m, "s-a", "developer", 42_000, 100_000)

	text, _ := m.subagentRowIdentity(readingsNode("n1", "s-a", "alpha", subagent.NodeCompleted), "", 60, -1, false)
	plain := ansi.Strip(text)
	require.Contains(t, plain, "alpha 42% ", "the reading follows the name directly")
	require.True(t, strings.HasSuffix(strings.TrimRight(plain, " "), "completed"), "the state stays right-aligned")
	require.Contains(t, text, contextGaugeStyle(styles.ContextGaugeNormal, styles.MutedStyle).Render("42%"))
}

// Three same-named agents each show their own session's reading, and a session
// with no usage yet shows none rather than a sibling's value.
func TestSameNamedSubagentsShowTheirOwnSessionReading(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	recordAgentUsage(m, "s-a", "developer", 10_000, 100_000)
	recordAgentUsage(m, "s-b", "developer", 55_000, 100_000)
	recordAgentUsage(m, "s-c", "developer", 92_000, 100_000)

	rows := []struct{ name, session, want string }{
		{"alpha", "s-a", "10%"}, {"beta", "s-b", "55%"}, {"gamma", "s-c", "92%"},
	}
	for _, row := range rows {
		text, _ := m.subagentRowIdentity(readingsNode("n-"+row.name, row.session, row.name, subagent.NodeCompleted), "", 60, -1, false)
		plain := ansi.Strip(text)
		require.Contains(t, plain, row.name+" "+row.want+" ", "%s shows its own session's reading", row.name)
		for _, other := range rows {
			if other.want != row.want {
				require.NotContains(t, plain, other.want, "%s must not show a sibling's reading", row.name)
			}
		}
	}
	gamma, _ := m.subagentRowIdentity(readingsNode("n-gamma", "s-c", "gamma", subagent.NodeCompleted), "", 60, -1, false)
	require.Contains(t, gamma, styles.ErrorStyle.Render("92%"), "critical usage uses the shared warning gauge")

	idle, _ := m.subagentRowIdentity(readingsNode("n-d", "s-d", "delta", subagent.NodeCompleted), "", 60, -1, false)
	plain := ansi.Strip(idle)
	require.Contains(t, plain, "delta ✓", "a session with no usage yet shows no reading")
	require.NotContains(t, plain, "%")
}

// Through the rendered Agents tree: same-named rows keep their own readings, and
// the setting toggles them off without a stale frame.
func TestSameNamedTreeRowsRenderOwnReadingsAndToggle(t *testing.T) {
	t.Parallel()

	ar := animation.NewRuntimeWithScheduler(&placementClock{now: time.Unix(1, 0), step: 50 * time.Millisecond})
	m := New(ar, t.Context(), &service.SessionState{}).(*model)
	m.treeCollapsed = false
	m.subagentSpinner = &fakeSpinner{m.subagentSpinner}
	m.SetPosition(5, 3)
	t.Cleanup(m.StopAnimation)
	m.SetAgentContextReadings(true)
	m.rootSessionID = "root-session"
	recordAgentUsage(m, "s-a", "developer", 10_000, 100_000)
	recordAgentUsage(m, "s-b", "developer", 55_000, 100_000)
	recordAgentUsage(m, "s-c", "developer", 92_000, 100_000)

	base := time.Now().Add(-5 * time.Minute)
	child := func(name, session string) subagent.NodeSnapshot {
		return subagent.NodeSnapshot{Node: subagent.Node{ID: subagent.NodeID("n-" + name), SessionID: session, Agent: "developer", Name: name, Parent: "root-node", State: subagent.NodeCompleted, CreatedAt: base}}
	}
	snap := subagent.Snapshot{Root: "root-node", Nodes: []subagent.NodeSnapshot{{
		Node:     subagent.Node{ID: "root-node", SessionID: "root-session", Agent: "root", State: subagent.NodeRunning},
		Children: []subagent.NodeSnapshot{child("alpha", "s-a"), child("beta", "s-b"), child("gamma", "s-c")},
	}}}
	cmd := m.SetSubagentTree(snap)
	settleTreePresentation(t, m, tea.Batch(cmd, m.SetSize(60, 70)))

	view := ansi.Strip(m.View())
	require.Equal(t, "10%", readingOnLine(t, view, "alpha"))
	require.Equal(t, "55%", readingOnLine(t, view, "beta"))
	require.Equal(t, "92%", readingOnLine(t, view, "gamma"))

	m.SetAgentContextReadings(false)
	settleTreePresentation(t, m, m.ReconcileLayout())
	assert.NotContains(t, ansi.Strip(m.View()), "%", "toggling off removes every reading")
}

// readingOnLine returns the percentage token printed on the first view line that
// names the row, or "" when that line has none.
func readingOnLine(t *testing.T, view, name string) string {
	t.Helper()
	for line := range strings.SplitSeq(view, "\n") {
		if !strings.Contains(line, name) {
			continue
		}
		for field := range strings.FieldsSeq(line) {
			if strings.HasSuffix(field, "%") {
				return field
			}
		}
		return ""
	}
	t.Fatalf("no view line names %q", name)
	return ""
}

// Narrow rows drop the reading before they truncate the name: the reading shows
// only while the full name is shown, and the state always stays.
func TestAgentContextReadingDropsBeforeNameTruncation(t *testing.T) {
	t.Parallel()

	node := readingsNode("n1", "s-a", "NestedReviewer", subagent.NodeCompleted)
	for width := 16; width <= 60; width++ {
		m := newSubagentTestModel(t)
		recordAgentUsage(m, "s-a", "developer", 42_000, 100_000)
		text, _ := m.subagentRowIdentity(node, "", width, -1, true)
		plain := ansi.Strip(text)
		require.Contains(t, plain, "completed", "width %d: the state stays", width)
		hasReading := strings.Contains(plain, "42%")
		fullName := strings.Contains(plain, node.DisplayName())
		if hasReading {
			require.True(t, fullName, "width %d: a reading never truncates the name", width)
		}
		if !fullName {
			require.False(t, hasReading, "width %d: a truncated name keeps no reading", width)
		}
	}
}

// Hover reveal keeps the resting geometry with a reading shown: the name hit span,
// the row width, and the branch control's cell all stay put.
func TestAgentContextReadingKeepsHoverGeometry(t *testing.T) {
	t.Parallel()

	const row = 4
	for _, width := range []int{40, 60, 120} {
		for _, branch := range []bool{false, true} {
			m := newSubagentTestModel(t)
			recordAgentUsage(m, "s-a", "developer", 42_000, 100_000)
			node := readingsNode("node-a1", "s-a", "alpha", subagent.NodeCompleted)

			restText, resting := m.subagentRowIdentity(node, "", width, row, branch)
			restControls := append([]treeControl(nil), m.treeControls[row]...)
			require.Contains(t, ansi.Strip(restText), "alpha 42% ", "width %d: resting reading after the name", width)

			m.treeControls = make(map[int][]treeControl)
			m.hoveredSubagent = node.ID
			m.branchSpans = map[subagent.NodeID]branchSpans{node.ID: {idValue: 1, idTarget: 1}}
			text, hovered := m.subagentRowIdentity(node, "", width, row, branch)
			require.Equal(t, resting, hovered, "width %d branch %v: hover cannot move the name hit span", width, branch)
			require.Equal(t, width, ansi.StringWidth(text), "width %d: hovered row keeps its width", width)
			require.Contains(t, ansi.Strip(text), "alpha 42% ", "width %d: hover keeps the reading beside the name", width)
			if branch {
				require.Equal(t, restControls, m.treeControls[row], "width %d: hover cannot move the branch control", width)
			}
		}
	}
}

// A participant row puts the reading directly after the name and drops it,
// before the name is truncated, when the whole name does not fit beside it.
// Hover restyles the name without moving the reading.
func TestParticipantLineReadingFollowsNameAndDropsFirst(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	recordAgentUsage(m, "s-w", "worker", 42_000, 100_000)

	rest := ansi.Strip(m.participantLine("worker", 40))
	require.Equal(t, "  worker 42%", strings.TrimRight(rest, " "))
	require.Equal(t, "  worker", strings.TrimRight(ansi.Strip(m.participantLine("worker", 10)), " "), "a narrow row drops the reading before truncating the name")

	m.hoverValues = map[string]hoverValue{"agent:worker": {value: 1}}
	require.Equal(t, rest, ansi.Strip(m.participantLine("worker", 40)), "hover cannot move the reading")
}

// A roster line puts the reading directly after the name on line 1, keeps the
// shortcut right-aligned, and drops the reading before truncating the name.
func TestRosterLineReadingFollowsNameAndDropsFirst(t *testing.T) {
	t.Parallel()

	m := newSubagentTestModel(t)
	recordAgentUsage(m, "s-d", "developer", 42_000, 100_000)
	agent := runtime.AgentDetails{Name: "developer", Provider: "openai", Model: "gpt-5.4"}

	wide := m.renderAgentLine(agent, 0, 40, 30, 0, false, false)
	require.Contains(t, ansi.Strip(wide[0]), "developer 42% ", "the reading follows the name")
	require.True(t, strings.HasSuffix(strings.TrimRight(ansi.Strip(wide[0]), " "), "^1"), "the shortcut stays right-aligned")
	require.NotContains(t, ansi.Strip(wide[1]), "%", "line 2 carries no reading")

	narrow := m.renderAgentLine(agent, 0, 40, 12, 0, false, false)
	require.NotContains(t, ansi.Strip(narrow[0]), "%", "a name that cannot hold the reading drops it")
	require.Contains(t, ansi.Strip(narrow[0]), "developer", "the name stays whole: the reading drops first")
}
