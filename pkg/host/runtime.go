package host

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/host/lifecycle"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamdef"
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
)

// RuntimeOptions controls host-specific session runtime behavior while keeping
// canonical loader/model/budget wiring consistent across CLI and embedders.
type RuntimeOptions struct {
	Snapshots                 bool
	SessionService            *runtime.SessionService
	WorkingDir                string
	ManagedOAuth              bool
	UnmanagedOAuthRedirectURI string
	Tracer                    trace.Tracer
	LoaderOptions             []teamloader.Opt
	TeamEditEditor            *teamdef.Editor
	TeamEditSourceID          string
	TeamEditPool              *teamloader.ToolsetLeasePool
	TeamEditPoolScope         string
	TeamEditBuilder           runtime.TeamEnvironmentBuilder
	TeamEditGlobalPermissions *permissions.Checker
	TeamEditPersistence       *runtime.TeamEditPersistence
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
	loaderOpts := append([]teamloader.Opt(nil), options.LoaderOptions...)
	if loaderOpts == nil {
		loaderOpts = loaderdefaults.Opts()
	}
	pool := options.TeamEditPool
	poolScope := options.TeamEditPoolScope
	var err error
	if cfg.TeamEditGrant().Mode != "off" {
		if poolScope == "" {
			poolScope = poolScopeForSource(source, cfg.WorkingDir)
		}
		if pool == nil {
			pool, err = teamloader.NewToolsetLeasePool(poolScope)
			if err != nil {
				return nil, err
			}
		}
		loaderOpts = append(loaderOpts, teamloader.WithToolsetLeasePool(pool, poolScope))
	}
	editPool := pool
	editPoolScope := poolScope
	loaded, err := teamloader.LoadWithConfig(ctx, source, cfg, loaderOpts...)
	if err != nil {
		if pool != nil {
			_ = pool.Close(context.WithoutCancel(ctx))
		}
		return nil, fmt.Errorf("loading session team: %w", err)
	}
	mergeHostRuntimePermissions(loaded.Team, options.TeamEditGlobalPermissions)
	fail := func(err error) (runtime.SessionRuntimeSupervisor, error) {
		_ = loaded.Team.StopToolSets(context.WithoutCancel(ctx))
		if pool != nil {
			_ = pool.Close(context.WithoutCancel(ctx))
		}
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
	var environmentStore *runtime.TeamEnvironmentStore
	if editPool != nil {
		sourceID := options.TeamEditSourceID
		if sourceID == "" {
			if typed, ok := source.(interface{ SourceID() string }); ok {
				sourceID = typed.SourceID()
			} else {
				sourceID = teamdef.SourceIdentity(source.Name())
			}
		}
		editor := options.TeamEditEditor
		persistence := options.TeamEditPersistence
		if persistence == nil {
			persistence = &runtime.TeamEditPersistence{}
		}
		if editor == nil {
			authority := teamdef.EditAuthorityMode(cfg.TeamEditGrant().Mode)
			if authority == "full" {
				authority = teamdef.EditAuthorityFull
			}
			if authority == "restricted" {
				authority = teamdef.EditAuthorityRestricted
			}
			editor, err = teamdef.NewEditor(ctx, teamdef.EditableSourceOptions{Source: source, Authority: authority, AllowedAgentIDs: cfg.TeamEditGrant().AllowedAgentIDs, WritablePath: localWritableSourcePath(source), ExportRoot: trustedExportRoot(cfg.WorkingDir), BuildValidator: func(ctx context.Context, candidate config.Source) error {
				_, err := config.Load(ctx, candidate)
				return err
			}})
			if err != nil {
				return fail(err)
			}
		}
		baseline := editor.BaselineRevisionID()
		initial, buildErr := runtime.TeamEnvironmentFromLoadedWithBaseline(baseline, sourceID, editor.BaselineRevisionID(), loaded, cfg)
		if buildErr != nil {
			return fail(buildErr)
		}
		environmentStore, buildErr = runtime.NewTeamEnvironmentStore(initial)
		if buildErr != nil {
			_ = initial.Discard(context.WithoutCancel(ctx))
			return fail(buildErr)
		}
		builder := options.TeamEditBuilder
		if builder == nil {
			builder = func(ctx context.Context, candidate config.Source, revision, sourceID, baselineID string, runConfig *config.RuntimeConfig, candidateOpts []teamloader.Opt) (*runtime.TeamEnvironment, error) {
				loaded, err := teamloader.LoadWithConfig(ctx, candidate, runConfig, candidateOpts...)
				if err != nil {
					return nil, err
				}
				mergeHostRuntimePermissions(loaded.Team, options.TeamEditGlobalPermissions)
				return runtime.TeamEnvironmentFromLoadedWithBaseline(revision, sourceID, baseline, loaded, runConfig)
			}
		}
		runtimeOpts = append(runtimeOpts, runtime.WithTeamEditInitialEnvironment(editor, environmentStore, builder, cfg.TeamEditGrant(), cfg, loaderOpts, editPool, editPoolScope, options.TeamEditGlobalPermissions, cfg.WorkingDir), runtime.WithTeamEditPersistence(persistence, trustedExportRoot(cfg.WorkingDir)))
	}
	if options.Snapshots {
		registry := hooks.NewRegistry()
		controller, err := builtins.RegisterSnapshot(registry, true)
		if err != nil {
			return fail(err)
		}
		runtimeOpts = append(runtimeOpts, runtime.WithHooksRegistry(registry), runtime.WithAutoInjector(controller), runtime.WithSnapshotController(controller))
	}
	if options.SessionService != nil {
		runtimeOpts = append(runtimeOpts, runtime.WithSessionService(options.SessionService))
	}
	if options.Tracer != nil {
		runtimeOpts = append(runtimeOpts, runtime.WithTracer(options.Tracer))
	}
	r, err := runtime.New(ctx, loaded.Team, runtimeOpts...)
	if err != nil {
		if environmentStore != nil {
			_ = environmentStore.Close(context.WithoutCancel(ctx))
		}
		return fail(fmt.Errorf("creating session runtime: %w", err))
	}
	return lifecycle.OwnRuntime(runtime.NewSessionRuntimeSupervisor(r), loaded.Team), nil
}

func poolScopeForSource(source config.Source, workingDir string) string {
	name := ""
	if source != nil {
		name = source.Name()
	}
	return "host:" + name + ":" + workingDir
}

func localWritableSourcePath(source config.Source) string {
	path, ok := config.FileSourcePath(source)
	if !ok || strings.Contains(path, "://") {
		return ""
	}
	return path
}

func trustedExportRoot(workingDir string) string {
	if workingDir == "" {
		return ""
	}
	return filepath.Clean(workingDir)
}

func mergeHostRuntimePermissions(tm *team.Team, global *permissions.Checker) {
	if tm != nil && global != nil && !global.IsEmpty() {
		tm.SetPermissions(permissions.Merge(tm.Permissions(), global))
	}
}
