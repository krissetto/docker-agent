package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// newSwitchableEffortRuntime builds a native and a harness agent with model
// switching enabled, so session thinking operations are supported.
func newSwitchableEffortRuntime(t *testing.T) *LocalRuntime {
	t.Helper()
	native := agent.New("native", "", agent.WithModel(&mockProvider{id: "test/model"}))
	external := agent.New("external", "", agent.WithHarness(&latest.HarnessConfig{Type: "claude-code"}))
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(native, external)),
		WithSessionCompaction(false),
		WithModelStore(emptyCatalogStore{}),
		WithModelSwitcherConfig(&ModelSwitcherConfig{
			ProviderRegistry: testProviderRegistry(),
			EnvProvider:      environment.NewMapEnvProvider(map[string]string{}),
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	return r
}

// TestHarnessEffortSwitchRejectsOldBindingAndKeepsPerAgentOverride checks the
// in-place switch: a mutation prepared against the binding before the switch is
// rejected as stale, and the harness effort chosen for the harness agent survives
// switch-away and switch-back without applying to the other agent.
func TestHarnessEffortSwitchRejectsOldBindingAndKeepsPerAgentOverride(t *testing.T) {
	t.Parallel()
	rt := newSwitchableEffortRuntime(t)
	handle, err := rt.CreateSession(t.Context(), session.New(session.WithAgentName("external")), SessionBinding{AgentName: "external"})
	require.NoError(t, err)
	h := handle.(*sessionHandle)

	_, err = h.SetThinkingLevel(t.Context(), effort.High)
	require.NoError(t, err)
	beforeSwitch := h.driver.activeBinding()
	require.Equal(t, "external", beforeSwitch.agentName)

	require.NoError(t, h.SwitchActiveAgent(t.Context(), "native"))

	err = h.driver.setHarnessEffort(t.Context(), beforeSwitch, effort.Max)
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr, "a mutation against the pre-switch binding must be rejected")
	assert.Equal(t, SessionErrorStale, sessionErr.Kind)

	require.NoError(t, h.SwitchActiveAgent(t.Context(), "external"))
	assert.Equal(t, effort.High, h.CurrentThinkingLevel(t.Context()), "the per-agent override survives switch-away and switch-back, and the rejected stale choice did not overwrite it")
}
