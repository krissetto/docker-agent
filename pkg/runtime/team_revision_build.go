package runtime

import (
	"context"
	"fmt"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/teamdef"
	"github.com/docker/docker-agent/pkg/teamloader"
)

type TeamEnvironmentBuilder func(context.Context, config.Source, string, string, string, *config.RuntimeConfig, []teamloader.Opt) (*TeamEnvironment, error)

// BuildTeamEnvironment uses the caller's trusted loader configuration and
// returns a candidate whose resources remain builder-owned until publication.
func BuildTeamEnvironment(ctx context.Context, source config.Source, revisionID, sourceID, baselineID string, runConfig *config.RuntimeConfig, loaderOptions []teamloader.Opt) (*TeamEnvironment, error) {
	if source == nil || revisionID == "" || sourceID == "" || baselineID == "" {
		return nil, fmt.Errorf("team environment source and revision identity are required")
	}
	var loaded *teamloader.LoadResult
	var err error
	if runConfig == nil {
		runConfig = &config.RuntimeConfig{}
	}
	loaded, err = teamloader.LoadWithConfig(ctx, source, runConfig, loaderOptions...)
	if err != nil {
		return nil, err
	}
	modelSwitcher := &ModelSwitcherConfig{Models: loaded.Models, Providers: loaded.Providers, ModelsGateway: runConfig.ModelsGateway, EncryptedConfig: loaded.EncryptedConfig, EnvProvider: runConfig.EnvProvider(), ProviderRegistry: loaded.ProviderRegistry, AgentDefaultModels: loaded.AgentDefaultModels}
	if store, storeErr := runConfig.ModelsDevStore(); storeErr == nil {
		modelSwitcher.ModelsStore = store
	}
	definitions := loaded.Definitions
	dispose := func(disposeCtx context.Context) error { return loaded.Team.StopToolSets(disposeCtx) }
	environment, err := NewTeamEnvironment(revisionID, sourceID, baselineID, definitions.TeamID, loaded.Team, *modelSwitcher, dispose, WithTeamEnvironmentBudgetConfig(BudgetPolicyConfig{Run: loaded.Budget, Named: loaded.Budgets, AgentBudgets: loaded.AgentBudgets}))
	if err != nil {
		_ = dispose(context.WithoutCancel(ctx))
		return nil, err
	}
	return environment, nil
}

type teamEditRuntime struct {
	editor            *teamdef.Editor
	builder           TeamEnvironmentBuilder
	grant             config.TeamEditGrant
	runConfig         *config.RuntimeConfig
	loaderOptions     []teamloader.Opt
	toolsetPool       *teamloader.ToolsetLeasePool
	poolScope         string
	globalPermissions *permissions.Checker
	workingDir        string
	persistence       *TeamEditPersistence
	exportRoot        string
}

func TeamEnvironmentFromLoaded(revisionID, sourceID string, loaded *teamloader.LoadResult, runConfig *config.RuntimeConfig) (*TeamEnvironment, error) {
	if loaded == nil {
		return nil, fmt.Errorf("loaded team and revision identity are required")
	}
	return TeamEnvironmentFromLoadedWithBaseline(revisionID, sourceID, loaded.Definitions.TeamDigest, loaded, runConfig)
}

// TeamEnvironmentFromLoadedWithBaseline pins editor-authored baseline identity
// separately from the full authored-definition digest retained by the loader.
func TeamEnvironmentFromLoadedWithBaseline(revisionID, sourceID, baselineID string, loaded *teamloader.LoadResult, runConfig *config.RuntimeConfig) (*TeamEnvironment, error) {
	if loaded == nil || loaded.Team == nil || revisionID == "" || sourceID == "" || baselineID == "" {
		return nil, fmt.Errorf("loaded team and revision identity are required")
	}
	cfg := ModelSwitcherConfig{Models: loaded.Models, Providers: loaded.Providers, ProviderRegistry: loaded.ProviderRegistry, AgentDefaultModels: loaded.AgentDefaultModels, EncryptedConfig: loaded.EncryptedConfig}
	if runConfig != nil {
		cfg.ModelsGateway, cfg.EnvProvider = runConfig.ModelsGateway, runConfig.EnvProvider()
		if store, err := runConfig.ModelsDevStore(); err == nil {
			cfg.ModelsStore = store
		}
	}
	return NewTeamEnvironment(revisionID, sourceID, baselineID, loaded.Definitions.TeamID, loaded.Team, cfg, func(ctx context.Context) error { return loaded.Team.StopToolSets(ctx) }, WithTeamEnvironmentBudgetConfig(BudgetPolicyConfig{Run: loaded.Budget, Named: loaded.Budgets, AgentBudgets: loaded.AgentBudgets}))
}
