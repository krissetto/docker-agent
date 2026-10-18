package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func revisionModelEnvironment(t *testing.T, revision, ref, model string) *TeamEnvironment {
	t.Helper()
	configured := latest.ModelConfig{Provider: "openai", Model: model}
	root := agent.New("root", "instructions", agent.WithModel(newConfigProvider(configured)))
	tm := team.New(team.WithAgents(root))
	cfg := ModelSwitcherConfig{
		Models:             map[string]latest.ModelConfig{ref: configured},
		AgentDefaultModels: map[string]string{"root": ref},
		ProviderRegistry:   testProviderRegistry(),
		EnvProvider:        environment.NewMapEnvProvider(nil),
	}
	env, err := NewTeamEnvironment(revision, "model-source", "model-baseline", "team", tm, cfg, func(context.Context) error { return nil })
	require.NoError(t, err)
	return env
}

func TestModelOperationsUsePinnedRevisionAndFailClosedWithoutOne(t *testing.T) {
	t.Parallel()
	legacyTeam := team.New(team.WithAgents(agent.New("root", "legacy", agent.WithModel(newConfigProvider(latest.ModelConfig{Provider: "openai", Model: "legacy-model"})))))
	legacyCfg := &ModelSwitcherConfig{
		Models:           map[string]latest.ModelConfig{"legacy-choice": {Provider: "openai", Model: "legacy-model"}},
		ProviderRegistry: testProviderRegistry(), EnvProvider: environment.NewMapEnvProvider(nil),
	}
	oldEnv := revisionModelEnvironment(t, "revision-old", "old-choice", "old-model")
	store, err := NewTeamEnvironmentStore(oldEnv)
	require.NoError(t, err)
	r := &LocalRuntime{team: legacyTeam, teamEnvironments: store, modelSwitcherCfg: legacyCfg, providerRegistry: testProviderRegistry()}
	oldLease, err := store.AcquireCurrent()
	require.NoError(t, err)
	oldCtx := environmentScopedContext(t.Context(), oldLease)

	oldChoices := refsOf(r.availableModels(oldCtx, "root"))
	require.Equal(t, []string{"old-choice"}, oldChoices, "a pinned revision never lists startup-global model choices")
	oldProviders, err := r.resolveModelProviders(oldCtx, "root", "old-choice")
	require.NoError(t, err)
	require.Equal(t, "old-model", oldProviders[0].BaseConfig().ModelConfig.Model)
	_, err = r.resolveModelProviders(t.Context(), "root", "legacy-choice")
	require.ErrorContains(t, err, "no pinned team environment")
	require.Empty(t, r.availableModels(t.Context(), "root"), "revision-enabled listing fails closed without captured environment")

	newEnv := revisionModelEnvironment(t, "revision-new", "new-choice", "new-model")
	require.NoError(t, store.Publish("model-source", "revision-old", newEnv))
	// The retired revision remains valid for an operation holding its lease.
	require.Equal(t, []string{"old-choice"}, refsOf(r.availableModels(oldCtx, "root")))
	stillOld, err := r.resolveModelProviders(oldCtx, "root", "old-choice")
	require.NoError(t, err)
	require.Equal(t, "old-model", stillOld[0].BaseConfig().ModelConfig.Model)

	newLease, err := store.AcquireCurrent()
	require.NoError(t, err)
	newCtx := environmentScopedContext(t.Context(), newLease)
	require.Equal(t, []string{"new-choice"}, refsOf(r.availableModels(newCtx, "root")))
	newProviders, err := r.resolveModelProviders(newCtx, "root", "new-choice")
	require.NoError(t, err)
	require.Equal(t, "new-model", newProviders[0].BaseConfig().ModelConfig.Model)
	_, err = r.resolveModelProviders(newCtx, "root", "old-choice")
	require.Error(t, err, "the new revision cannot resolve a model present only in its predecessor")

	require.NoError(t, newLease.Release(t.Context()))
	require.NoError(t, oldLease.Release(t.Context()))
	require.NoError(t, store.Close(t.Context()))
}

func TestModelSwitchResolutionFailureDoesNotMutateAgentOrSession(t *testing.T) {
	t.Parallel()
	factoryError := errors.New("provider build failed")
	registry := provider.NewRegistry(map[string]provider.Factory{
		"openai": func(_ context.Context, _ *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (provider.Provider, error) {
			return nil, factoryError
		},
	})
	root := agent.New("root", "instructions", agent.WithModel(newConfigProvider(latest.ModelConfig{Provider: "openai", Model: "default-model"})))
	tm := team.New(team.WithAgents(root))
	badConfig := ModelSwitcherConfig{
		Models:           map[string]latest.ModelConfig{"bad": {Provider: "openai", Model: "broken-model"}},
		ProviderRegistry: registry,
		EnvProvider:      environment.NewMapEnvProvider(nil),
	}
	env, err := NewTeamEnvironment("revision-bad", "bad-source", "bad-baseline", "bad-team", tm, badConfig, func(context.Context) error { return nil })
	require.NoError(t, err)
	store, err := NewTeamEnvironmentStore(env)
	require.NoError(t, err)
	lease, err := store.AcquireCurrent()
	require.NoError(t, err)
	ctx := environmentScopedContext(t.Context(), lease)
	r := &LocalRuntime{team: team.New(team.WithAgents(agent.New("root", "legacy", agent.WithModel(newConfigProvider(latest.ModelConfig{Provider: "openai", Model: "legacy-model"}))))), teamEnvironments: store, modelSwitcherCfg: &ModelSwitcherConfig{Models: map[string]latest.ModelConfig{"legacy": {Provider: "openai", Model: "legacy-model"}}}, providerRegistry: registry}
	sess := session.New(session.WithAgentName("root"))
	_, _, err = r.resolveSessionModelBinding(ctx, sess, "bad")
	require.ErrorIs(t, err, factoryError)
	require.Empty(t, sess.AgentModelOverrides)
	configured, err := env.Team().Agent("root")
	require.NoError(t, err)
	require.Equal(t, "default-model", configured.ConfiguredModels()[0].BaseConfig().ModelConfig.Model)
	require.NoError(t, lease.Release(t.Context()))
	require.NoError(t, store.Close(t.Context()))
}

func TestDMRDiscoveryCacheIsRevisionScoped(t *testing.T) {
	t.Parallel()
	oldEnv := revisionModelEnvironment(t, "dmr-old", "old-choice", "old-model")
	store, err := NewTeamEnvironmentStore(oldEnv)
	require.NoError(t, err)
	calls := 0
	r := &LocalRuntime{
		teamEnvironments: store,
		dmrModelLister: func(ctx context.Context) ([]string, error) {
			calls++
			env := teamEnvironmentFromContext(ctx)
			if env == nil {
				return nil, errors.New("missing revision")
			}
			if env.RevisionID() == "dmr-old" {
				return []string{"model-old"}, nil
			}
			return []string{"model-new"}, nil
		},
	}
	oldLease, err := store.AcquireCurrent()
	require.NoError(t, err)
	oldCtx := environmentScopedContext(t.Context(), oldLease)
	oldModels, err := r.listDMRModels(oldCtx)
	require.NoError(t, err)
	require.Equal(t, []string{"model-old"}, oldModels)
	newEnv := revisionModelEnvironment(t, "dmr-new", "new-choice", "new-model")
	require.NoError(t, store.Publish("model-source", "dmr-old", newEnv))
	newLease, err := store.AcquireCurrent()
	require.NoError(t, err)
	newModels, err := r.listDMRModels(environmentScopedContext(t.Context(), newLease))
	require.NoError(t, err)
	require.Equal(t, []string{"model-new"}, newModels)
	require.Equal(t, 2, calls, "new revision must not reuse old revision discovery cache")
	require.NoError(t, newLease.Release(t.Context()))
	require.NoError(t, oldLease.Release(t.Context()))
	require.NoError(t, store.Close(t.Context()))
}

func TestGatewayDiscoveryCacheIsRevisionScoped(t *testing.T) {
	t.Parallel()
	serve := func(id string) (*httptest.Server, *atomic.Int32) {
		calls := &atomic.Int32{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			_, _ = fmt.Fprintf(w, `{"data":[{"id":%q}]}`, id)
		}))
		return server, calls
	}
	oldServer, oldCalls := serve("openai/old")
	defer oldServer.Close()
	newServer, newCalls := serve("openai/new")
	defer newServer.Close()
	modelConfig := func(gateway string, name string) ModelSwitcherConfig {
		return ModelSwitcherConfig{
			Models:        map[string]latest.ModelConfig{name: {Provider: "openai", Model: name}},
			ModelsGateway: gateway,
			EnvProvider:   environment.NewMapEnvProvider(map[string]string{environment.DockerDesktopTokenEnv: "test-token"}),
		}
	}
	makeEnv := func(revision, gateway, name string) *TeamEnvironment {
		root := agent.New("root", "instructions", agent.WithModel(newConfigProvider(latest.ModelConfig{Provider: "openai", Model: name})))
		env, err := NewTeamEnvironment(revision, "gateway-source", "gateway-baseline", "team", team.New(team.WithAgents(root)), modelConfig(gateway, name), func(context.Context) error { return nil })
		require.NoError(t, err)
		return env
	}
	oldEnv := makeEnv("gateway-r1", oldServer.URL, "configured-old")
	store, err := NewTeamEnvironmentStore(oldEnv)
	require.NoError(t, err)
	oldLease, err := store.AcquireCurrent()
	require.NoError(t, err)
	r := &LocalRuntime{teamEnvironments: store, modelsStore: stubModelStore{}}
	oldCtx := environmentScopedContext(t.Context(), oldLease)
	oldIDs, err := r.listGatewayModels(oldCtx)
	require.NoError(t, err)
	require.Equal(t, []string{"openai/old"}, oldIDs)

	newEnv := makeEnv("gateway-r2", newServer.URL, "configured-new")
	require.NoError(t, store.Publish("gateway-source", "gateway-r1", newEnv))
	newLease, err := store.AcquireCurrent()
	require.NoError(t, err)
	newCtx := environmentScopedContext(t.Context(), newLease)
	newIDs, err := r.listGatewayModels(newCtx)
	require.NoError(t, err)
	require.Equal(t, []string{"openai/new"}, newIDs)
	oldIDs, err = r.listGatewayModels(oldCtx)
	require.NoError(t, err)
	require.Equal(t, []string{"openai/old"}, oldIDs)
	require.EqualValues(t, 1, oldCalls.Load())
	require.EqualValues(t, 1, newCalls.Load())
	require.NoError(t, newLease.Release(t.Context()))
	require.NoError(t, oldLease.Release(t.Context()))
	require.NoError(t, store.Close(t.Context()))
}

func TestCanceledRevisionModelSwitchLeavesSessionUnchanged(t *testing.T) {
	t.Parallel()
	registry := provider.NewRegistry(map[string]provider.Factory{
		"openai": func(ctx context.Context, _ *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (provider.Provider, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	root := agent.New("root", "instructions", agent.WithModel(newConfigProvider(latest.ModelConfig{Provider: "openai", Model: "default-model"})))
	badConfig := ModelSwitcherConfig{Models: map[string]latest.ModelConfig{"slow": {Provider: "openai", Model: "slow-model"}}, ProviderRegistry: registry, EnvProvider: environment.NewMapEnvProvider(nil)}
	env, err := NewTeamEnvironment("revision-canceled", "cancel-source", "cancel-baseline", "cancel-team", team.New(team.WithAgents(root)), badConfig, func(context.Context) error { return nil })
	require.NoError(t, err)
	store, err := NewTeamEnvironmentStore(env)
	require.NoError(t, err)
	lease, err := store.AcquireCurrent()
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(environmentScopedContext(t.Context(), lease))
	cancel()
	r := &LocalRuntime{teamEnvironments: store, providerRegistry: registry}
	sess := session.New(session.WithAgentName("root"))
	_, _, err = r.resolveSessionModelBinding(ctx, sess, "slow")
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, sess.AgentModelOverrides)
	configured, err := env.Team().Agent("root")
	require.NoError(t, err)
	require.Equal(t, "default-model", configured.ConfiguredModels()[0].BaseConfig().ModelConfig.Model)
	require.NoError(t, lease.Release(t.Context()))
	require.NoError(t, store.Close(t.Context()))
}
