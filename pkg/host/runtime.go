package host

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/host/lifecycle"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
)

// RuntimeOptions controls host-specific session runtime behavior while keeping
// canonical loader/model/budget wiring consistent across CLI and embedders.
type RuntimeOptions struct {
	WorkingDir                string
	ManagedOAuth              bool
	UnmanagedOAuthRedirectURI string
	Tracer                    trace.Tracer
	LoaderOptions             []teamloader.Opt
}

// NewSessionRuntime loads one source and returns an owned session runtime.
// Shutdown always stops the runtime before stopping all loaded toolsets.
func NewSessionRuntime(ctx context.Context, source config.Source, runConfig *config.RuntimeConfig, store session.Store, options RuntimeOptions) (runtime.SessionRuntimeSupervisor, error) {
	if runConfig == nil {
		runConfig = &config.RuntimeConfig{}
	}
	cfg := runConfig
	if options.WorkingDir != "" {
		cfg = runConfig.Clone()
		cfg.WorkingDir = options.WorkingDir
	}
	loaderOpts := options.LoaderOptions
	if loaderOpts == nil {
		loaderOpts = loaderdefaults.Opts()
	}
	loaded, err := teamloader.LoadWithConfig(ctx, source, cfg, loaderOpts...)
	if err != nil {
		return nil, fmt.Errorf("loading session team: %w", err)
	}
	fail := func(err error) (runtime.SessionRuntimeSupervisor, error) {
		_ = loaded.Team.StopToolSets(context.WithoutCancel(ctx))
		return nil, err
	}
	defaultAgent, err := loaded.Team.DefaultAgent()
	if err != nil {
		return fail(fmt.Errorf("resolving default session agent: %w", err))
	}
	modelSwitcher := &runtime.ModelSwitcherConfig{Models: loaded.Models, Providers: loaded.Providers, ModelsGateway: cfg.ModelsGateway, EncryptedConfig: loaded.EncryptedConfig, EnvProvider: cfg.EnvProvider(), ProviderRegistry: loaded.ProviderRegistry, AgentDefaultModels: loaded.AgentDefaultModels}
	if modelsStore, storeErr := cfg.ModelsDevStore(); storeErr == nil {
		modelSwitcher.ModelsStore = modelsStore
	} else {
		slog.WarnContext(ctx, "Failed to obtain shared models.dev store", "error", storeErr)
	}
	runtimeOpts := []runtime.Opt{runtime.WithSessionStore(store), runtime.WithCurrentAgent(defaultAgent.Name()), runtime.WithWorkingDir(cfg.WorkingDir), runtime.WithManagedOAuth(options.ManagedOAuth), runtime.WithUnmanagedOAuthRedirectURI(options.UnmanagedOAuthRedirectURI), runtime.WithModelSwitcherConfig(modelSwitcher), runtime.WithBudget(loaded.Budget), runtime.WithNamedBudgets(loaded.Budgets, loaded.AgentBudgets)}
	if options.Tracer != nil {
		runtimeOpts = append(runtimeOpts, runtime.WithTracer(options.Tracer))
	}
	r, err := runtime.New(ctx, loaded.Team, runtimeOpts...)
	if err != nil {
		return fail(fmt.Errorf("creating session runtime: %w", err))
	}
	return lifecycle.OwnRuntime(runtime.NewSessionRuntimeSupervisor(r), loaded.Team), nil
}
