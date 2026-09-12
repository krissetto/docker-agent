package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
)

func TestReplaceSessionAcrossAgentsUsesTargetBindingAndOverride(t *testing.T) {
	worker := agent.New("worker", "prompt", agent.WithModel(stubProvider{}))
	root := agent.New("root", "prompt", agent.WithModel(stubProvider{}))
	registry := provider.NewRegistry(map[string]provider.Factory{
		"test": func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (provider.Provider, error) {
			return configuredStubProvider{stubProvider: stubProvider{}, cfg: *cfg}, nil
		},
	})
	sessions, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(worker, root)), runtime.WithModelSwitcherConfig(&runtime.ModelSwitcherConfig{
		Models:           map[string]latest.ModelConfig{"root-model": {Provider: "test", Model: "root-model"}},
		ProviderRegistry: registry,
		EnvProvider:      environment.NewMapEnvProvider(nil),
	}))
	require.NoError(t, err)
	workerSession := session.New(session.WithID("worker-session"), session.WithAgentName("worker"))
	a := New(t.Context(), sessions, workerSession, runtime.SessionBinding{Durability: subagent.DurabilityVolatile}, WithRuntimeServices(sessions))
	require.NoError(t, a.state().err)

	rootSession := session.New(session.WithID("new-root-session"), session.WithAgentName("root"))
	rootSession.AgentModelOverrides = map[string]string{"root": "root-model"}
	a.ReplaceSession(t.Context(), rootSession)

	require.NoError(t, a.state().err)
	assert.Equal(t, "root", a.state().handle.AgentName())
	assert.Equal(t, "root", a.Binding().AgentName)
	assert.Equal(t, subagent.DurabilityVolatile, a.Binding().Durability)
	assert.Equal(t, "root-model", a.state().handle.Metadata().Model)
	assert.False(t, root.HasModelOverride(), "session binding must not mutate shared agent provider state")
	assert.False(t, worker.HasModelOverride())

	rootDefault := session.New(session.WithID("new-root-default"), session.WithAgentName("root"))
	a.ReplaceSession(t.Context(), rootDefault)
	require.NoError(t, a.state().err)
	assert.Equal(t, "root", a.state().handle.AgentName())
	assert.False(t, root.HasModelOverride())
}

func TestReplaceSessionReconcilesSameAgentModelOverride(t *testing.T) {
	worker := agent.New("worker", "prompt", agent.WithModel(stubProvider{}))
	registry := provider.NewRegistry(map[string]provider.Factory{
		"test": func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (provider.Provider, error) {
			return configuredStubProvider{stubProvider: stubProvider{}, cfg: *cfg}, nil
		},
	})
	sessions, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(worker)), runtime.WithModelSwitcherConfig(&runtime.ModelSwitcherConfig{
		Models: map[string]latest.ModelConfig{
			"x": {Provider: "test", Model: "x"},
			"y": {Provider: "test", Model: "y"},
		},
		ProviderRegistry: registry,
		EnvProvider:      environment.NewMapEnvProvider(nil),
	}))
	require.NoError(t, err)

	makeSession := func(id, ref string) *session.Session {
		sess := session.New(session.WithID(id), session.WithAgentName("worker"))
		if ref != "" {
			sess.AgentModelOverrides = map[string]string{"worker": ref}
		}
		return sess
	}
	a := New(t.Context(), sessions, makeSession("model-x", "x"), runtime.SessionBinding{}, WithRuntimeServices(sessions))
	require.NoError(t, a.state().err)
	assert.Equal(t, "x", a.state().handle.Metadata().Model)
	assert.False(t, worker.HasModelOverride())

	a.ReplaceSession(t.Context(), makeSession("model-y", "y"))
	require.NoError(t, a.state().err)
	assert.Equal(t, "y", a.state().handle.Metadata().Model)
	assert.False(t, worker.HasModelOverride())

	a.ReplaceSession(t.Context(), makeSession("model-default", ""))
	require.NoError(t, a.state().err)
	assert.False(t, worker.HasModelOverride())
}

type configuredStubProvider struct {
	stubProvider

	cfg latest.ModelConfig
}

func (p configuredStubProvider) BaseConfig() base.Config { return base.Config{ModelConfig: p.cfg} }

func TestNewNormalizesDefaultSessionAgentBinding(t *testing.T) {
	sessions, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("default", "prompt", agent.WithModel(stubProvider{})))))
	require.NoError(t, err)
	sess := session.New()
	sess.AgentModelOverrides = map[string]string{"default": "provider/model"}

	a := New(t.Context(), sessions, sess, runtime.SessionBinding{}, WithRuntimeServices(sessions))
	require.NoError(t, a.state().err)
	assert.Equal(t, "default", a.Binding().AgentName)
	assert.Equal(t, "provider/model", a.Binding().Model)
	assert.NotContains(t, sess.AgentModelOverrides, "")
}

func TestNewUsesInjectedSharedSessionRuntimeAndImmutableBinding(t *testing.T) {
	sessions, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("worker", "prompt", agent.WithModel(replyProvider{})))))
	require.NoError(t, err)

	sess := session.New(session.WithID("shared-session"), session.WithAgentName("worker"))
	a := New(t.Context(), sessions, sess, runtime.SessionBinding{AgentName: "worker", Model: "provider/model"}, WithRuntimeServices(&mockRuntime{}))

	require.NoError(t, a.state().err)
	require.NotNil(t, a.state().session)
	assert.Equal(t, "provider/model", a.state().handle.Metadata().Model)

	assert.Same(t, sessions, a.SessionRuntime())
	assert.Equal(t, runtime.SessionBinding{AgentName: "worker", Model: "provider/model"}, a.Binding())
	assert.NotSame(t, a.Runtime(), a.SessionRuntime(), "presentation services must not be the session registry boundary")

	shared, err := sessions.SessionByID(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, a.state().handle.ID(), shared.ID())
	assert.Equal(t, a.state().handle.AgentName(), shared.AgentName())
}

func TestNewDerivesBindingModelFromPinnedAgentOverride(t *testing.T) {
	sessions, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("worker", "prompt", agent.WithModel(stubProvider{})))))
	require.NoError(t, err)
	sess := session.New(session.WithID("derived-binding"), session.WithAgentName("worker"))
	sess.AgentModelOverrides = map[string]string{"worker": "provider/derived"}

	a := New(t.Context(), sessions, sess, runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{}))
	require.NoError(t, a.state().err)
	assert.Equal(t, "worker", a.state().handle.AgentName())
	assert.Equal(t, "provider/derived", a.state().handle.Metadata().Model)
}

func TestReplaceSessionReusesPersistedSessionHandleAndCanSend(t *testing.T) {
	sessions, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("worker", "prompt", agent.WithModel(replyProvider{})))))
	require.NoError(t, err)
	original := session.New(session.WithID("reloaded"), session.WithAgentName("worker"))
	a := New(t.Context(), sessions, original, runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{}))
	require.NoError(t, a.state().err)
	firstID := a.state().handle.ID()

	for range 3 {
		a.ReplaceSession(t.Context(), original)
		require.NoError(t, a.state().err)
		assert.Equal(t, firstID, a.state().handle.ID())
		assert.Equal(t, original.AgentName, a.state().handle.AgentName())
	}
	_, err = a.FollowUpMessage(t.Context(), "after reload", nil)
	require.NoError(t, err)
}

func TestReplaceSessionAfterReleasedSessionBindsNewGeneration(t *testing.T) {
	sessions, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("worker", "prompt", agent.WithModel(stubProvider{})))))
	require.NoError(t, err)
	sess := session.New(session.WithID("stopped-reload"), session.WithAgentName("worker"))
	a := New(t.Context(), sessions, sess, runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{}))
	require.NoError(t, a.state().err)
	require.NoError(t, a.state().handle.Release(t.Context()))

	a.ReplaceSession(t.Context(), sess)
	require.NoError(t, a.state().err, "reload binds a new session generation")
	_, err = a.FollowUpMessage(t.Context(), "after release", nil)
	require.NoError(t, err)
}

func TestCancelRunDoesNotStopIdleSessionLifetime(t *testing.T) {
	sessions, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("worker", "prompt", agent.WithModel(stubProvider{})))))
	require.NoError(t, err)
	sess := session.New(session.WithID("idle-wake-stop"), session.WithAgentName("worker"))
	a := New(t.Context(), sessions, sess, runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{}))
	require.NoError(t, a.state().err)

	assert.Equal(t, runtime.CancelNotActive, a.CancelRun(), "there is no active run to cancel")
	status, err := a.state().handle.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, runtime.SessionStateSettled, status.State)
}
