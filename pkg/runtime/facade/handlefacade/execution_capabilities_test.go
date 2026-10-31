package handlefacade_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/facade"
	"github.com/docker/docker-agent/pkg/runtime/facade/facadetest"
	"github.com/docker/docker-agent/pkg/runtime/facade/handlefacade"
)

// TestExecutionCapabilitiesPassThroughFacade checks that the facade forwards the
// agent and roster events unchanged, so the profile of the agent a session
// switches to reaches the observer without being rebuilt or inferred.
func TestExecutionCapabilitiesPassThroughFacade(t *testing.T) {
	t.Parallel()
	harness := facadetest.NewLocalRuntime(t, facadetest.Config{})
	host, err := handlefacade.NewHost(harness.Runtime)
	require.NoError(t, err)
	t.Cleanup(func() { _ = host.Close(t.Context()) })

	sess, err := host.Create(t.Context(), facade.CreateOptions{AgentName: facadetest.RootAgent})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.Close(t.Context()) })
	observer, ok := sess.Observer()
	require.True(t, ok)
	observation, err := observer.Observe(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(observation.Close)

	switcher, ok := sess.AgentSwitch()
	require.True(t, ok)
	require.NoError(t, switcher.SwitchAgent(t.Context(), facadetest.WriterAgent))

	var info *runtime.AgentInfoEvent
	var roster *runtime.TeamInfoEvent
	deadline := time.After(5 * time.Second)
	for info == nil || roster == nil {
		select {
		case ev := <-observation.Events:
			passthrough, ok := ev.Payload.(facade.Passthrough)
			if !ok {
				continue
			}
			switch event := passthrough.Event.(type) {
			case *runtime.AgentInfoEvent:
				if event.AgentName == facadetest.WriterAgent {
					info = event
				}
			case *runtime.TeamInfoEvent:
				roster = event
			}
		case <-deadline:
			t.Fatal("observer did not receive the switched agent's AgentInfo and TeamInfo")
		}
	}

	native := &runtime.ExecutionCapabilities{
		Mode: runtime.ExecutionModeNative, ToolExecution: runtime.ToolExecutionDockerAgent,
		ToolApproval: runtime.ToolApprovalDockerAgentPolicy, BudgetSupported: true,
		CompactionSupported: true, MidTurnSteeringSupported: true, PromptHookContextSupported: true,
	}
	assert.Equal(t, native, info.ExecutionCapabilities, "AgentInfo passes the writer's profile through unchanged")
	byName := map[string]*runtime.ExecutionCapabilities{}
	for _, detail := range roster.AvailableAgents {
		byName[detail.Name] = detail.ExecutionCapabilities
	}
	assert.Equal(t, native, byName[facadetest.WriterAgent], "roster passes each agent's profile through unchanged")
}
