package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/harness"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func nativeExecutionProfile() *ExecutionCapabilities {
	return &ExecutionCapabilities{
		Mode: "native", ToolExecution: "docker-agent", ToolApproval: "docker-agent-policy",
		BudgetSupported: true, CompactionSupported: true, MidTurnSteeringSupported: true, PromptHookContextSupported: true,
	}
}

func harnessExecutionProfile() *ExecutionCapabilities {
	return &ExecutionCapabilities{Mode: "harness", ToolExecution: "harness-reported", ToolApproval: "external"}
}

func TestExecutionCapabilitiesProfiles(t *testing.T) {
	t.Parallel()
	assert.Nil(t, executionCapabilities(nil))
	assert.Nil(t, (*LocalRuntime)(nil).AgentExecutionCapabilities("native"))
	assert.Nil(t, (&LocalRuntime{}).AgentExecutionCapabilities("native"))
	assert.Equal(t, nativeExecutionProfile(), executionCapabilities(agent.New("native", "")))
	assert.Equal(t, harnessExecutionProfile(), executionCapabilities(agent.New("harness", "", agent.WithHarness(&latest.HarnessConfig{Type: "codex"}))))
	profile := executionCapabilities(agent.New("native", ""))
	profile.ToolApproval = "changed"
	assert.Equal(t, nativeExecutionProfile(), executionCapabilities(agent.New("native", "")), "profiles must not share mutable values")
}

type capabilityProbeToolset struct {
	calls int
}

func (s *capabilityProbeToolset) Start(context.Context) error { s.calls++; return nil }
func (s *capabilityProbeToolset) Stop(context.Context) error  { return nil }
func (s *capabilityProbeToolset) Tools(context.Context) ([]tools.Tool, error) {
	s.calls++
	return nil, nil
}

// TestExecutionCapabilitiesInspectionIsLazy pins that describing the roster
// starts no toolset, builds no harness and reads only the agents' profiles.
func TestExecutionCapabilitiesInspectionIsLazy(t *testing.T) {
	t.Parallel()
	toolset := &capabilityProbeToolset{}
	native := agent.New("native", "", agent.WithModel(&mockProvider{id: "test/model"}), agent.WithToolSets(toolset))
	external := agent.New("external", "", agent.WithHarness(&latest.HarnessConfig{Type: "codex"}), agent.WithToolSets(toolset))
	factoryCalls := 0
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(native, external)),
		WithModelStore(mockModelStore{}), WithSessionCompaction(false),
		WithHarnessFactory(func(*latest.HarnessConfig) (harness.Provider, error) {
			factoryCalls++
			return nil, nil
		}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	for _, tc := range []struct {
		name string
		want *ExecutionCapabilities
	}{
		{"native", nativeExecutionProfile()}, {"external", harnessExecutionProfile()}, {"missing", nil},
	} {
		assert.Equal(t, tc.want, rt.AgentExecutionCapabilities(tc.name))
		assert.Equal(t, tc.want, rt.AgentConfigInfo(t.Context(), tc.name).ExecutionCapabilities)
	}
	var events []Event
	rt.EmitAgentInfo(t.Context(), EventSinkFunc(func(event Event) { events = append(events, event) }))
	require.Len(t, events, 2)
	assert.Equal(t, nativeExecutionProfile(), events[0].(*AgentInfoEvent).ExecutionCapabilities)
	roster := events[1].(*TeamInfoEvent)
	require.Len(t, roster.AvailableAgents, 2)
	assert.Equal(t, nativeExecutionProfile(), roster.AvailableAgents[0].ExecutionCapabilities)
	assert.Equal(t, harnessExecutionProfile(), roster.AvailableAgents[1].ExecutionCapabilities)
	assert.Zero(t, toolset.calls)
	assert.Zero(t, factoryCalls)
}

// TestExecutionCapabilitiesStreamRoster checks that a run's roster carries the
// same per-agent profiles as local inspection.
func TestExecutionCapabilitiesStreamRoster(t *testing.T) {
	t.Parallel()
	root := agent.New("root", "", agent.WithModel(&mockProvider{id: "test/model", stream: newStreamBuilder().AddContent("reply").AddStopWithUsage(1, 1).Build()}))
	external := agent.New("external", "", agent.WithHarness(&latest.HarnessConfig{Type: "codex"}))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, external)), WithModelStore(mockModelStore{}), WithSessionCompaction(false))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	var roster *TeamInfoEvent
	for _, event := range collectRuntimeEvents(t, rt, session.New(session.WithUserMessage("hello"))) {
		if info, ok := event.(*TeamInfoEvent); ok {
			roster = info
		}
	}
	require.NotNil(t, roster)
	require.Len(t, roster.AvailableAgents, 2)
	assert.Equal(t, nativeExecutionProfile(), roster.AvailableAgents[0].ExecutionCapabilities)
	assert.Equal(t, harnessExecutionProfile(), roster.AvailableAgents[1].ExecutionCapabilities)
}

func TestExecutionCapabilitiesJSONCompatibility(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		profile *ExecutionCapabilities
	}{
		{"unknown", nil}, {"native", nativeExecutionProfile()}, {"harness", harnessExecutionProfile()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			original := AgentDetails{Name: "root", ExecutionCapabilities: tc.profile}
			data, err := json.Marshal(original)
			require.NoError(t, err)
			var decoded AgentDetails
			require.NoError(t, json.Unmarshal(data, &decoded))
			assert.Equal(t, original, decoded)
			if tc.profile == nil {
				assert.NotContains(t, string(data), "execution_capabilities")
			} else if tc.name == "harness" {
				assert.Contains(t, string(data), `"budget_supported":false`)
				assert.Contains(t, string(data), `"prompt_hook_context_supported":false`)
			}
		})
	}
	var legacy TeamInfoEvent
	require.NoError(t, json.Unmarshal([]byte(`{"type":"team_info","available_agents":[{"name":"root","model":"codex"}],"current_agent":"root"}`), &legacy))
	require.Len(t, legacy.AvailableAgents, 1)
	assert.Nil(t, legacy.AvailableAgents[0].ExecutionCapabilities)
	update := AgentInfo("root", "test/model", "", "")
	data, err := json.Marshal(update)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "execution_capabilities", "model-only updates must not guess capabilities")
}

// newCapabilitySessionRuntime builds a native and a harness agent so each
// session-bound projection can be checked against the profile of its own agent.
func newCapabilitySessionRuntime(t *testing.T) *LocalRuntime {
	t.Helper()
	native := agent.New("native", "", agent.WithModel(&mockProvider{id: "test/model"}))
	external := agent.New("external", "", agent.WithHarness(&latest.HarnessConfig{Type: "codex"}))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(native, external)),
		WithModelStore(mockModelStore{}), WithSessionCompaction(false))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	return rt
}

// TestExecutionCapabilitiesPinnedSessionInfo checks the per-session projection:
// the session's own agent reports its profile in AgentInfo, and the roster
// carries each agent's profile.
func TestExecutionCapabilitiesPinnedSessionInfo(t *testing.T) {
	t.Parallel()
	rt := newCapabilitySessionRuntime(t)

	for _, tc := range []struct {
		agentName string
		want      *ExecutionCapabilities
	}{
		{"external", harnessExecutionProfile()},
		{"native", nativeExecutionProfile()},
	} {
		handle, err := rt.CreateSession(t.Context(), session.New(session.WithAgentName(tc.agentName)), SessionBinding{AgentName: tc.agentName})
		require.NoError(t, err)
		info, roster := collectAgentInfo(func(sink EventSink) { handle.(*sessionHandle).EmitPinnedAgentInfo(t.Context(), sink) })
		require.NotNil(t, info)
		require.NotNil(t, roster)
		assert.Equal(t, tc.want, info.ExecutionCapabilities, "AgentInfo for %s", tc.agentName)
		require.Len(t, roster.AvailableAgents, 2)
		byName := map[string]*ExecutionCapabilities{}
		for _, detail := range roster.AvailableAgents {
			byName[detail.Name] = detail.ExecutionCapabilities
		}
		assert.Equal(t, nativeExecutionProfile(), byName["native"])
		assert.Equal(t, harnessExecutionProfile(), byName["external"])
	}
}

// TestExecutionCapabilitiesObservationPresentation checks the observed snapshot,
// which is what the TUI renders: its presentation carries the same profiles.
func TestExecutionCapabilitiesObservationPresentation(t *testing.T) {
	t.Parallel()
	rt := newCapabilitySessionRuntime(t)
	handle, err := rt.CreateSession(t.Context(), session.New(session.WithAgentName("external")), SessionBinding{AgentName: "external"})
	require.NoError(t, err)
	obs, err := handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	t.Cleanup(obs.Cancel)
	require.NotEmpty(t, obs.Initial)

	var info *AgentInfoEvent
	var roster *TeamInfoEvent
	for _, event := range obs.Initial[0].Presentation {
		switch event := event.(type) {
		case *AgentInfoEvent:
			info = event
		case *TeamInfoEvent:
			roster = event
		}
	}
	require.NotNil(t, info, "observed presentation must describe the bound agent")
	require.NotNil(t, roster)
	assert.Equal(t, harnessExecutionProfile(), info.ExecutionCapabilities)
	require.Len(t, roster.AvailableAgents, 2)
	byName := map[string]*ExecutionCapabilities{}
	for _, detail := range roster.AvailableAgents {
		byName[detail.Name] = detail.ExecutionCapabilities
	}
	assert.Equal(t, nativeExecutionProfile(), byName["native"])
	assert.Equal(t, harnessExecutionProfile(), byName["external"])
}
