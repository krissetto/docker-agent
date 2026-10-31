package facadetest

import (
	"context"
	"testing"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	todotool "github.com/docker/docker-agent/pkg/tools/builtin/todo"
)

// Names of the fixture's two agents and the model each one is configured with.
// The root runs a reasoning model, so it exposes thinking levels; the writer runs
// a non-reasoning model, so it exposes none.
const (
	RootAgent   = "root"
	WriterAgent = "writer"
	RootModel   = "gpt-5"
	WriterModel = "gpt-4o"
)

// routedModel reports the configuration of the model it stands for while sharing
// the scripted model's behaviour. The runtime builds one per configured model.
type routedModel struct {
	*Model

	cfg latest.ModelConfig
}

// BaseConfig gives the fixture the rebuild path a production provider has, so
// compaction can clone it. The clone is another routedModel over the same
// scripted Model, so queued scripts (and the summary a compaction consumes)
// stay shared with the original.
func (r routedModel) BaseConfig() base.Config {
	return base.Config{
		ModelConfig: r.cfg,
		RebuildProvider: func(_ context.Context, cfg *latest.ModelConfig, _ ...options.Opt) (provider.Provider, error) {
			return routedModel{Model: r.Model, cfg: *cfg}, nil
		},
	}
}

// emptyStore is a model catalog with no network lookup. Every model resolves to a
// fixed definition with a context window, which compaction requires.
type emptyStore struct{ runtime.ModelStore }

func (emptyStore) GetModel(context.Context, modelsdev.ID) (*modelsdev.Model, error) {
	return &modelsdev.Model{Name: "scripted", Limit: modelsdev.Limit{Context: 200000, Output: 4096}}, nil
}

func (emptyStore) GetDatabase(context.Context) (*modelsdev.Database, error) {
	return &modelsdev.Database{}, nil
}

// Harness is a real in-process runtime with the scripted models that drive it.
// Model drives the root agent; Worker drives the writer agent, which the root
// can spawn as an async subagent, so the two never share a script queue.
type Harness struct {
	Runtime runtime.SessionRuntime
	Model   *Model
	Worker  *Model
}

// NewHarness builds the runtime over store. A server that fronts the runtime must
// share the store, or it lists and opens a different set of sessions than the
// runtime created.
//
// Only ToolNeedsApproval asks for confirmation; the other tools are allowed. The
// root can spawn the writer as an async subagent. Both agents report different
// configurations, and model switching resolves through a provider registry that
// needs no network.
func NewHarness(t *testing.T, cfg Config, store session.Store) Harness {
	t.Helper()
	model, worker := NewModel(), NewModel()
	rootCfg := latest.ModelConfig{Provider: "openai", Model: RootModel}
	writerCfg := latest.ModelConfig{Provider: "openai", Model: WriterModel}
	opts := []agent.Opt{
		agent.WithModel(routedModel{Model: model, cfg: rootCfg}),
		agent.WithToolSets(newToolSet(), todotool.New(), subagent.NewToolSet()),
		agent.WithAsyncSubagents(latest.SubagentRef{Agent: WriterAgent}),
	}
	if cfg.MaxIterations > 0 {
		opts = append(opts, agent.WithMaxIterations(cfg.MaxIterations))
	}
	root := agent.New(RootAgent, "You are a test agent.", opts...)
	writer := agent.New(WriterAgent, "You write things down.", agent.WithModel(routedModel{Model: worker, cfg: writerCfg}), agent.WithToolSets(newToolSet()))
	checker := permissions.NewCheckerFromRules([]string{ToolNoop, ToolElicit, ToolSpawnSubagent, ToolCreateTodos}, []string{ToolNeedsApproval}, nil)
	tm := team.New(team.WithAgents(root, writer), team.WithPermissions(checker))
	switcher := &runtime.ModelSwitcherConfig{
		Models: map[string]latest.ModelConfig{RootModel: rootCfg, WriterModel: writerCfg},
		AgentDefaultModels: map[string]string{
			RootAgent:   RootModel,
			WriterAgent: WriterModel,
		},
		ProviderRegistry: registry(model, worker),
		EnvProvider:      environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "sk-test"}),
	}
	runtimeOpts := []runtime.Opt{
		runtime.WithSessionStore(store),
		runtime.WithSessionCompaction(false),
		runtime.WithModelStore(emptyStore{}),
		runtime.WithModelSwitcherConfig(switcher),
	}
	if cfg.ReplayEvents > 0 {
		policy := runtime.DefaultSessionResourcePolicy()
		policy.ReplayEvents = cfg.ReplayEvents
		runtimeOpts = append(runtimeOpts, runtime.WithSessionResourcePolicy(policy))
	}
	rt, err := runtime.NewLocalRuntime(t.Context(), tm, runtimeOpts...)
	requireNoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	return Harness{Runtime: runtime.NewSessionRuntimeSupervisor(rt).Runtime(), Model: model, Worker: worker}
}

// NewLocalRuntime is NewHarness over a fresh in-memory store, for callers that
// need only the registry.
func NewLocalRuntime(t *testing.T, cfg Config) Harness {
	t.Helper()
	return NewHarness(t, cfg, session.NewInMemorySessionStore())
}

// registry instantiates the routed model for any configured provider. The writer's
// configuration resolves to the worker's script queue, matching its agent.
func registry(model, worker *Model) *provider.Registry {
	factory := func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (provider.Provider, error) {
		if cfg.Model == WriterModel {
			return routedModel{Model: worker, cfg: *cfg}, nil
		}
		return routedModel{Model: model, cfg: *cfg}, nil
	}
	return provider.NewRegistry(map[string]provider.Factory{
		"openai":                 factory,
		"openai_chatcompletions": factory,
		"openai_responses":       factory,
	})
}
