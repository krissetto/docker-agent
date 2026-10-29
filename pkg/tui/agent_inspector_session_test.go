package tui

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// inspectorDialogView opens the inspector through the root handler and renders
// the dialog it requests.
func inspectorDialogView(t *testing.T, root *appModel, msg messages.ShowAgentDetailsMsg) string {
	t.Helper()
	_, cmd := root.handleShowAgentDetails(msg)
	require.NotNil(t, cmd, "a roster agent opens the inspector")
	for _, out := range collectMsgs(cmd) {
		if open, ok := out.(dialog.OpenDialogMsg); ok {
			open.Model.SetSize(120, 60)
			return ansi.Strip(open.Model.View())
		}
	}
	t.Fatal("the inspector dialog was not opened")
	return ""
}

// Two instances of one agent keep their own context and cost. A session with no
// recorded usage shows neither, and never falls back to the agent's aggregate.
func TestAgentInspectorUsesOwnSessionAndNeverAgentFallback(t *testing.T) {
	root := sidebarTransitionRoot(t).root
	root.sessionState.SetAvailableAgents([]runtime.AgentDetails{{Name: "worker", Provider: "p", Model: "m"}})
	// The active page's sidebar observes unopened subagent sessions too, so both
	// instances are recorded into it rather than into separate pages.
	record := func(sessionID string, contextLength int64, cost float64) {
		usage := runtime.NewTokenUsageEvent(sessionID, "worker", &runtime.Usage{ContextLength: contextLength, ContextLimit: 100_000, Cost: cost})
		_, _ = root.chatPage.Update(messages.SessionRuntimeEventMsg{Event: usage})
	}
	record("second", 45_000, 1.5)
	record("third", 80_000, 4)

	a := inspectorDialogView(t, root, messages.ShowAgentDetailsMsg{AgentName: "worker", SessionID: "second"})
	require.Contains(t, a, "Context: 45.0K of 100.0K tokens (45%)")
	require.Contains(t, a, toolcommon.FormatCostPrecise(1.5))
	require.NotContains(t, a, "(80%)")

	b := inspectorDialogView(t, root, messages.ShowAgentDetailsMsg{AgentName: "worker", SessionID: "third"})
	require.Contains(t, b, "Context: 80.0K of 100.0K tokens (80%)")
	require.Contains(t, b, toolcommon.FormatCostPrecise(4))
	require.NotContains(t, b, "(45%)")

	missing := inspectorDialogView(t, root, messages.ShowAgentDetailsMsg{AgentName: "worker", SessionID: "session-missing"})
	require.NotContains(t, missing, "Context:", "a missing session shows unknown usage")
	require.NotContains(t, missing, "(45%)", "no fallback to a same-named session")
	require.NotContains(t, missing, "(80%)", "no fallback to the agent's latest snapshot")
	require.NotContains(t, missing, "Cost", "a missing session shows no cost")
}
