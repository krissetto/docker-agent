package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// TestHarnessEffortFooterClickAndShiftTabAgree pins that a claude-code harness
// agent's sidebar effort is clickable and Shift+Tab cycles it through the same
// session-scoped path, with the stale-origin guard rejecting a foreign ModelRef.
func TestHarnessEffortFooterClickAndShiftTabAgree(t *testing.T) {
	sess := session.New(session.WithID("harness-owner"), session.WithAgentName("worker"))
	handle := &primaryThinkingHandle{sidebarModelHandle: &sidebarModelHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}, id: sess.ID}, capable: true}
	application := app.New(t.Context(), &primaryThinkingSessions{openSubagentSessions: &openSubagentSessions{}, handle: handle}, sess, runtime.SessionBinding{AgentName: "worker"}, app.WithRuntimeServices(stubRuntime{}))
	root := newSidebarProgramRoot(t, application)
	generation, exists := root.supervisor.RouteGeneration(root.paneFocus())
	require.True(t, exists)

	root.Update(runtime.TeamInfo([]runtime.AgentDetails{{
		Name: "worker", Model: "claude-code", ModelID: "claude-code", ModelName: "claude-code",
		ThinkingMode: "adaptive", CanCycleThinking: true,
	}}, "worker"))
	require.Equal(t, "claude-code", root.thinkingModelReference())

	before := handle.cycles
	root.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	require.Equal(t, before+1, handle.cycles, "Shift+Tab cycles a harness effort")

	msg := messages.CycleThinkingLevelMsg{SessionID: sess.ID, AgentName: "worker", ModelRef: "claude-code", RouteGeneration: generation}
	root.handleScopedThinkingCycle(msg)
	require.Equal(t, before+2, handle.cycles, "the footer click reaches the same session-scoped cycle")

	stale := msg
	stale.ModelRef = "openai/gpt-5"
	root.handleScopedThinkingCycle(stale)
	require.Equal(t, before+2, handle.cycles, "a foreign model reference cannot mutate the harness effort")
}
