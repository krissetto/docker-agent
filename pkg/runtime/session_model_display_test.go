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
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/modelpicker"
)

// newModelDisplayRuntime builds a real runtime whose sessions run through
// drivers, with model switching enabled, so per-session overrides exercise
// the same path the TUI does.
func newModelDisplayRuntime(t *testing.T) *LocalRuntime {
	t.Helper()
	root := agent.New("root", "test", agent.WithModel(newConfigProvider(latest.ModelConfig{Provider: "openai", Model: "gpt-5"})))
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false),
		WithModelStore(emptyCatalogStore{}),
		WithModelSwitcherConfig(&ModelSwitcherConfig{
			Models:             map[string]latest.ModelConfig{"fast": {Provider: "openai", Model: "gpt-4o"}},
			AgentDefaultModels: map[string]string{"root": "openai/gpt-5"},
			ProviderRegistry:   testProviderRegistry(),
			EnvProvider:        environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "sk-test"}),
		}),
	)
	require.NoError(t, err)
	return r
}

// collectAgentInfo drains the events emit produces and returns the AgentInfo
// and TeamInfo it carried.
func collectAgentInfo(emit func(EventSink)) (*AgentInfoEvent, *TeamInfoEvent) {
	var info *AgentInfoEvent
	var teamInfo *TeamInfoEvent
	emit(EventSinkFunc(func(ev Event) {
		switch e := ev.(type) {
		case *AgentInfoEvent:
			info = e
		case *TeamInfoEvent:
			teamInfo = e
		}
	}))
	return info, teamInfo
}

func TestSessionMetadataCachesModelBindingProjections(t *testing.T) {
	r := newModelDisplayRuntime(t)
	handle, err := r.CreateSession(t.Context(), session.New(session.WithAgentName("root")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	h := handle.(*sessionHandle)

	first := h.Metadata()
	h.metadataMu.Lock()
	firstModels := h.metadataModels
	firstLevels := h.metadataLevels
	h.metadataMu.Unlock()
	second := h.Metadata()
	h.metadataMu.Lock()
	secondModels := h.metadataModels
	secondLevels := h.metadataLevels
	h.metadataMu.Unlock()

	assert.Equal(t, first, second)
	assert.Same(t, &firstModels[0], &secondModels[0], "unchanged binding reuses cached model choices")
	assert.Same(t, &firstLevels[0], &secondLevels[0], "unchanged binding reuses cached thinking levels")

	require.NoError(t, h.SetModel(t.Context(), "fast"))
	updated := h.Metadata()
	assert.Equal(t, "fast", updated.Model)
	h.metadataMu.Lock()
	updatedModels := h.metadataModels
	updatedLevels := h.metadataLevels
	h.metadataMu.Unlock()
	assert.NotSame(t, &firstModels[0], &updatedModels[0], "binding changes replace cached model choices")
	assert.Equal(t, updated.ThinkingLevels, updatedLevels)
}

// TestPinnedAgentInfoReflectsSessionModelOverride pins the sidebar contract
// behind /model: the info re-emitted after a per-session switch must name
// the model the session actually runs with, not the agent's YAML default —
// and the switch must stay invisible to the shared team agent.
func TestPinnedAgentInfoReflectsSessionModelOverride(t *testing.T) {
	t.Parallel()

	r := newModelDisplayRuntime(t)
	handle, err := r.CreateSession(t.Context(), session.New(session.WithAgentName("root")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	h := handle.(*sessionHandle)

	info, _ := collectAgentInfo(func(sink EventSink) { h.EmitPinnedAgentInfo(t.Context(), sink) })
	require.NotNil(t, info)
	assert.Equal(t, "openai/gpt-5", info.Model, "before any switch the default model is shown")

	require.NoError(t, h.SetModel(t.Context(), "fast"))

	info, teamInfo := collectAgentInfo(func(sink EventSink) { h.EmitPinnedAgentInfo(t.Context(), sink) })
	require.NotNil(t, info)
	assert.Equal(t, "openai/gpt-4o", info.Model, "the re-emitted info must show the session's override")
	require.NotNil(t, teamInfo)
	require.Len(t, teamInfo.AvailableAgents, 1)
	assert.Equal(t, "gpt-4o", teamInfo.AvailableAgents[0].Model)

	rootAgent, err := r.team.Agent("root")
	require.NoError(t, err)
	assert.False(t, rootAgent.HasModelOverride(), "a session override must not leak into the shared agent")
}

// TestPinnedAgentInfoReflectsSessionThinkingLevel pins the same contract for
// the reasoning-effort controls (shift+tab, /effort): the sidebar's thinking
// label must follow the session's own level.
func TestPinnedAgentInfoReflectsSessionThinkingLevel(t *testing.T) {
	t.Parallel()

	r := newModelDisplayRuntime(t)
	handle, err := r.CreateSession(t.Context(), session.New(session.WithAgentName("root")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	h := handle.(*sessionHandle)

	applied, err := h.SetThinkingLevel(t.Context(), effort.High)
	require.NoError(t, err)
	assert.Equal(t, effort.High, applied)
	assert.Equal(t, effort.High, h.CurrentThinkingLevel(t.Context()))

	_, teamInfo := collectAgentInfo(func(sink EventSink) { h.EmitPinnedAgentInfo(t.Context(), sink) })
	require.NotNil(t, teamInfo)
	require.Len(t, teamInfo.AvailableAgents, 1)
	assert.Equal(t, "high", teamInfo.AvailableAgents[0].Thinking, "the team row must show the session's thinking level")
}

// TestStartupInfoReflectsStoredModelOverride covers reloading a session in
// which the model had been changed: the persisted override is what the
// restored session runs with, so the startup info must describe it too.
func TestStartupInfoReflectsStoredModelOverride(t *testing.T) {
	t.Parallel()

	r := newModelDisplayRuntime(t)
	restored := session.New(session.WithAgentName("root"))
	restored.AgentModelOverrides = map[string]string{"root": "fast"}
	_, err := r.CreateSession(t.Context(), restored, SessionBinding{AgentName: "root"})
	require.NoError(t, err)

	info, _ := collectAgentInfo(func(sink EventSink) { r.EmitStartupInfo(t.Context(), restored, sink) })
	require.NotNil(t, info)
	assert.Equal(t, "openai/gpt-4o", info.Model, "startup info for a reloaded session must show its stored override")
}

// TestModelPickerToolChangesTheCallingSessionOnly pins the model_picker tool
// to the same per-session path /model uses: the agent switching its own model
// changes (and persists) the calling session's binding, other sessions of the
// same agent keep theirs, and the shared team agent is never mutated.
func TestModelPickerToolChangesTheCallingSessionOnly(t *testing.T) {
	t.Parallel()

	root := agent.New("root", "test",
		agent.WithModel(newConfigProvider(latest.ModelConfig{Provider: "openai", Model: "gpt-5"})),
		agent.WithToolSets(modelpicker.New([]string{"fast"})),
	)
	store := session.NewInMemorySessionStore()
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false),
		WithSessionStore(store),
		WithModelStore(emptyCatalogStore{}),
		WithModelSwitcherConfig(&ModelSwitcherConfig{
			Models:             map[string]latest.ModelConfig{"fast": {Provider: "openai", Model: "gpt-4o"}},
			AgentDefaultModels: map[string]string{"root": "openai/gpt-5"},
			ProviderRegistry:   testProviderRegistry(),
			EnvProvider:        environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "sk-test"}),
		}),
	)
	require.NoError(t, err)

	callerSess := session.New(session.WithAgentName("root"))
	caller, err := r.CreateSession(t.Context(), callerSess, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	other, err := r.CreateSession(t.Context(), session.New(session.WithAgentName("root")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)

	var infos []*AgentInfoEvent
	sink := EventSinkFunc(func(ev Event) {
		if info, ok := ev.(*AgentInfoEvent); ok {
			infos = append(infos, info)
		}
	})
	result, err := r.handleChangeModel(t.Context(), callerSess, tools.ToolCall{Function: tools.FunctionCall{Name: "change_model", Arguments: `{"model":"fast"}`}}, sink, nil)
	require.NoError(t, err)
	require.False(t, result.IsError, result.Output)

	_, callerModels := caller.(*sessionHandle).driver.ModelSnapshot()
	require.Len(t, callerModels, 1)
	assert.Equal(t, "gpt-4o", callerModels[0].BaseConfig().ModelConfig.Model, "the calling session switches")
	_, otherModels := other.(*sessionHandle).driver.ModelSnapshot()
	require.Len(t, otherModels, 1)
	assert.Equal(t, "gpt-5", otherModels[0].BaseConfig().ModelConfig.Model, "sibling sessions keep their model")
	assert.False(t, root.HasModelOverride(), "the shared agent is not mutated")
	require.Len(t, infos, 1)
	assert.Equal(t, "openai/gpt-4o", infos[0].Model, "the emitted info describes the switched session")

	stored, err := store.GetSession(t.Context(), callerSess.ID)
	require.NoError(t, err)
	assert.Equal(t, "fast", stored.AgentModelOverrides["root"], "the switch is persisted with the session")

	result, err = r.handleRevertModel(t.Context(), callerSess, tools.ToolCall{}, sink, nil)
	require.NoError(t, err)
	require.False(t, result.IsError, result.Output)
	_, callerModels = caller.(*sessionHandle).driver.ModelSnapshot()
	assert.Equal(t, "gpt-5", callerModels[0].BaseConfig().ModelConfig.Model, "revert returns to the agent's default")
}
