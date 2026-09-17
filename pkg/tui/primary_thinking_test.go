package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type primaryThinkingHandle struct {
	*sidebarModelHandle

	capable bool
	cycles  int
}

func (h *primaryThinkingHandle) Metadata() runtime.SessionMetadata {
	return runtime.SessionMetadata{SessionID: h.ID(), AgentName: "worker", Capabilities: runtime.SessionCapabilities{ModelSwitching: true, ThinkingLevels: h.capable}}
}

func (h *primaryThinkingHandle) CycleThinkingLevel(context.Context) (effort.Level, error) {
	h.cycles++
	return effort.High, nil
}

type primaryThinkingSessions struct {
	*openSubagentSessions

	handle *primaryThinkingHandle
}

func (s *primaryThinkingSessions) SessionByID(string) (runtime.SessionHandle, error) {
	return s.handle, nil
}

func TestPrimaryThinkingClickAndShiftTabAgreeDuringFallback(t *testing.T) {
	for _, lean := range []bool{false, true} {
		for _, capable := range []bool{true, false} {
			sess := session.New(session.WithID("reasoning-owner"), session.WithAgentName("worker"))
			handle := &primaryThinkingHandle{sidebarModelHandle: &sidebarModelHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}, id: sess.ID}, capable: capable}
			application := app.New(t.Context(), &primaryThinkingSessions{openSubagentSessions: &openSubagentSessions{}, handle: handle}, sess, runtime.SessionBinding{AgentName: "worker"}, app.WithRuntimeServices(stubRuntime{}))
			require.Same(t, handle, application.SessionHandle())
			root := newSidebarProgramRoot(t, application)
			root.leanMode = lean
			generation, exists := root.supervisor.RouteGeneration(root.paneFocus())
			require.True(t, exists)
			for _, fallback := range []bool{false, true, false} {
				model := "primary"
				if fallback {
					model = "fallback"
				}
				root.Update(runtime.AgentInfo("worker", "p/"+model, "", ""))
				before := handle.cycles
				root.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
				want := before
				if capable {
					want++
				}
				require.Equal(t, want, handle.cycles, "AgentInfo-to-TeamInfo gap uses primary capability")
				details := runtime.AgentDetails{Name: "worker", Provider: "p", ModelID: model, ThinkingMode: "unsupported", CanCycleThinking: capable}
				if fallback {
					details.CanCycleThinking = !capable
					details.PrimaryThinking = &runtime.ThinkingDetails{ModelRef: "p/primary", Mode: "effort", Level: "high", CanCycle: capable}
				}
				root.Update(runtime.TeamInfo([]runtime.AgentDetails{details}, "worker"))
				require.Equal(t, "p/primary", root.thinkingModelReference())
				msg := messages.CycleThinkingLevelMsg{SessionID: sess.ID, AgentName: "worker", ModelRef: "p/primary", RouteGeneration: generation}
				if fallback {
					msg.DisplayedModelRef = "p/fallback"
				}
				before = handle.cycles
				root.handleScopedThinkingCycle(msg)
				root.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
				want = before
				if capable {
					want += 2
				}
				require.Equal(t, want, handle.cycles, "click and ShiftTab use P, not F support")
				for _, mutate := range []func(*messages.CycleThinkingLevelMsg){
					func(m *messages.CycleThinkingLevelMsg) { m.SessionID = "other" },
					func(m *messages.CycleThinkingLevelMsg) { m.AgentName = "other" },
					func(m *messages.CycleThinkingLevelMsg) { m.ModelRef = "p/replacement" },
					func(m *messages.CycleThinkingLevelMsg) { m.DisplayedModelRef = "p/other-fallback" },
					func(m *messages.CycleThinkingLevelMsg) { m.RouteGeneration++ },
					func(m *messages.CycleThinkingLevelMsg) { m.RouteGeneration = 0 },
				} {
					stale := msg
					mutate(&stale)
					root.handleScopedThinkingCycle(stale)
					require.Equal(t, want, handle.cycles, "stale origin cannot mutate primary")
				}
				if fallback {
					root.application.TrackCurrentAgentModel("p/new-fallback")
					root.handleScopedThinkingCycle(msg)
					require.Equal(t, want, handle.cycles, "F replacement invalidates click even with stable P")
					root.application.TrackCurrentAgentModel("p/fallback")
					replacement := *details.PrimaryThinking
					replacement.ModelRef = "p/new-primary"
					details.PrimaryThinking = &replacement
					root.sessionState.SetAvailableAgents([]runtime.AgentDetails{details})
					root.handleScopedThinkingCycle(msg)
					require.Equal(t, want, handle.cycles, "P replacement invalidates click even with stable F")
				}
			}
		}
	}
}
