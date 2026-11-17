package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime/modelcatalog"
	"github.com/docker/docker-agent/pkg/session"
)

// The catalog value types live in pkg/runtime/modelcatalog. These aliases keep
// existing runtime callers compiling unchanged.
type (
	ModelChoice           = modelcatalog.ModelChoice
	SessionModelsResponse = modelcatalog.SessionModelsResponse
	ModelSwitcherConfig   = modelcatalog.Config
	ModelStore            = modelcatalog.Store
)

// DecorateModelChoices marks the active selection and appends custom refs; see
// [modelcatalog.DecorateModelChoices].
func DecorateModelChoices(models []ModelChoice, currentRef string, customRefs []string) []ModelChoice {
	return modelcatalog.DecorateModelChoices(models, currentRef, customRefs)
}

// resolveSessionModelBinding computes the model binding a session runs
// with: the explicit ref when given, otherwise the session's stored override
// for its agent; the providers are resolved through the model switcher when
// one is configured and fall back to the agent's configured models. It is the
// single resolution rule shared by session creation, lazy initialization and
// restore, so all three publish identical bindings.
// modelSwitcherConfigForContext selects immutable model configuration from the
// operation's pinned team revision. Revision-enabled runtimes fail closed when
// a caller did not capture an environment; only legacy runtimes use the global
// startup config.
func (r *LocalRuntime) modelSwitcherConfigForContext(ctx context.Context) (*ModelSwitcherConfig, error) {
	if env := teamEnvironmentFromContext(ctx); env != nil {
		cfg, err := env.ModelSwitcherConfig()
		if err != nil {
			return nil, err
		}
		return &cfg, nil
	}
	if r.teamEnvironments != nil {
		return nil, errors.New("operation has no pinned team environment")
	}
	if r.modelSwitcherCfg == nil {
		return nil, ErrUnsupported
	}
	return r.modelSwitcherCfg, nil
}

func (r *LocalRuntime) resolveModelProviders(ctx context.Context, agentName, modelRef string) ([]provider.Provider, error) {
	cfg, err := r.modelSwitcherConfigForContext(ctx)
	if err != nil {
		if modelRef == "" && errors.Is(err, ErrUnsupported) {
			return nil, err
		}
		return nil, err
	}
	if modelRef == "" {
		tm := r.team
		if env := teamEnvironmentFromContext(ctx); env != nil {
			tm = env.Team()
		}
		a, err := tm.Agent(agentName)
		if err != nil {
			return nil, err
		}
		return a.ConfiguredModels(), nil
	}
	return r.resolveModelProvidersForConfig(ctx, agentName, modelRef, cfg)
}

func (r *LocalRuntime) resolveSessionModelBinding(ctx context.Context, sess *session.Session, modelRef string) (string, []provider.Provider, error) {
	ref, providers, err := r.resolveSessionModelProviders(ctx, sess, modelRef)
	if err != nil || !r.SupportsModelSwitching() {
		return ref, providers, err
	}
	return ref, r.applyStoredThinkingLevel(ctx, sess, providers), nil
}

// applyStoredThinkingLevel re-applies the effort the user picked for the
// session's agent onto freshly built providers. A level the resolved model no
// longer supports is ignored so the YAML default applies instead.
func (r *LocalRuntime) applyStoredThinkingLevel(ctx context.Context, sess *session.Session, providers []provider.Provider) []provider.Provider {
	level := effort.Level(sess.AgentThinkingLevel(sess.AgentName))
	if level == "" || len(providers) == 0 {
		return providers
	}
	supported, _, err := r.resolveThinkingLevelsForModels(ctx, providers)
	if err != nil || !slices.Contains(supported, level) {
		return providers
	}
	applied, err := r.withThinkingLevel(ctx, providers, level)
	if err != nil {
		slog.WarnContext(ctx, "Failed to re-apply stored thinking level; using configured default", "agent", sess.AgentName, "level", level, "error", err)
		return providers
	}
	return applied
}

// withThinkingLevel re-creates each model with level as its thinking budget.
func (r *LocalRuntime) withThinkingLevel(ctx context.Context, models []provider.Provider, level effort.Level) ([]provider.Provider, error) {
	updated := make([]provider.Provider, 0, len(models))
	for _, model := range models {
		baseCfg := model.BaseConfig().ModelConfig
		cfg := baseCfg.Clone()
		cfg.ThinkingBudget = &latest.ThinkingBudget{Effort: string(level)}
		p, err := r.createProviderFromConfig(ctx, cfg)
		if err != nil {
			return nil, err
		}
		updated = append(updated, p)
	}
	return updated, nil
}

func (r *LocalRuntime) resolveSessionModelProviders(ctx context.Context, sess *session.Session, modelRef string) (string, []provider.Provider, error) {
	cfg, err := r.modelSwitcherConfigForContext(ctx)
	if err != nil {
		if errors.Is(err, ErrUnsupported) && modelRef == "" {
			a, teamErr := r.team.Agent(sess.AgentName)
			if teamErr != nil {
				return "", nil, teamErr
			}
			return modelRef, a.ConfiguredModels(), nil
		}
		return "", nil, err
	}
	if modelRef == "" {
		modelRef = sess.AgentModelOverrides[sess.AgentName]
	}
	if modelRef == "" {
		tm := r.team
		if env := teamEnvironmentFromContext(ctx); env != nil {
			tm = env.Team()
		}
		a, err := tm.Agent(sess.AgentName)
		if err != nil {
			return "", nil, &SessionError{Kind: SessionErrorNotFound, SessionID: sess.ID, Operation: "resolve_active_agent"}
		}
		return modelRef, a.ConfiguredModels(), nil
	}
	providers, err := r.resolveModelProvidersForConfig(ctx, sess.AgentName, modelRef, cfg)
	if err != nil {
		return "", nil, err
	}
	return modelRef, providers, nil
}

func (r *LocalRuntime) resolveModelProvidersForConfig(ctx context.Context, agentName, modelRef string, cfg *ModelSwitcherConfig) ([]provider.Provider, error) {
	if cfg == nil {
		return nil, ErrUnsupported
	}
	if model, ok := cfg.Models[modelRef]; ok && isAlloyModelConfig(model) {
		return r.resolveModelRefsWithConfig(ctx, model.Model, cfg)
	}
	if isInlineAlloySpec(modelRef) {
		return r.resolveModelRefsWithConfig(ctx, modelRef, cfg)
	}
	model, ok := cfg.Models[modelRef]
	if !ok {
		parsed, err := latest.ParseModelRef(modelRef)
		if err != nil {
			return nil, err
		}
		model = parsed
	} else {
		model.Name = modelRef
	}
	created, err := r.createProviderFromConfigWithConfig(ctx, &model, cfg)
	if err != nil {
		return nil, err
	}
	return []provider.Provider{created}, nil
}

func (r *LocalRuntime) resolveModelRefsWithConfig(ctx context.Context, refs string, cfg *ModelSwitcherConfig) ([]provider.Provider, error) {
	var out []provider.Provider
	for ref := range strings.SplitSeq(refs, ",") {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		model, ok := cfg.Models[ref]
		if !ok {
			var err error
			model, err = latest.ParseModelRef(ref)
			if err != nil {
				return nil, err
			}
		} else {
			model.Name = ref
		}
		provider, err := r.createProviderFromConfigWithConfig(ctx, &model, cfg)
		if err != nil {
			return nil, err
		}
		out = append(out, provider)
	}
	return out, nil
}

// SupportsModelSwitching reports whether the runtime was built with a
// [ModelSwitcherConfig], i.e. whether sessions can list and change
// models instead of hitting the no-config empty path.
func (r *LocalRuntime) SupportsModelSwitching() bool {
	return r.modelSwitcherCfg != nil || r.teamEnvironments != nil
}

// RefreshModelsCatalog forces a refetch of the models.dev catalog backing
// the model picker, bypassing the store's refresh interval. It returns
// [ErrUnsupported] when the runtime's model store cannot refresh (e.g. an
// in-memory test store).
type modelCatalogRefresher interface {
	Refresh(ctx context.Context) error
}

func modelStoreCanRefresh(store ModelStore) bool {
	_, ok := store.(modelCatalogRefresher)
	return ok
}

func (r *LocalRuntime) RefreshModelsCatalog(ctx context.Context) error {
	modelConfig, configErr := r.modelSwitcherConfigForContext(ctx)
	if configErr != nil {
		return configErr
	}
	store := r.modelsStoreForConfig(modelConfig)
	refresher, ok := store.(modelCatalogRefresher)
	if !ok {
		return ErrUnsupported
	}
	return refresher.Refresh(ctx)
}

// levelNames renders levels as a comma-separated list for error messages.
func levelNames(levels []effort.Level) string {
	names := make([]string, len(levels))
	for i, l := range levels {
		names[i] = string(l)
	}
	return strings.Join(names, ", ")
}

// resolveThinkingLevelsForModels returns the thinking-effort levels
// supported by models[0] (the agent's primary effective model) plus its
// currently active level. It is the pure resolution step shared by
// resolveAgentThinkingLevels (the read-only getter path) and the session
// session's thinking-level setter (which must reuse the exact same snapshot it
// later re-creates providers from — see resolveAgentThinkingLevels for why
// that sharing matters).
func (r *LocalRuntime) resolveThinkingLevelsForModels(ctx context.Context, models []provider.Provider) ([]effort.Level, effort.Level, error) {
	if len(models) == 0 {
		return nil, "", errors.New("agent has no model configured")
	}

	baseCfg := models[0].BaseConfig().ModelConfig
	if !r.modelSupportsThinking(ctx, &baseCfg) {
		return nil, "", fmt.Errorf("model %q does not support thinking levels: %w", baseCfg.DisplayOrModel(), ErrUnsupported)
	}

	return modelinfo.SupportedThinkingLevels(baseCfg.Provider, baseCfg.Model), currentThinkingLevel(&baseCfg), nil
}

// resolveAgentThinkingLevels resolves agentName's current effective model
// and returns the thinking-effort levels it supports plus its currently
// active level. It backs CurrentAgentThinkingLevels (read-only, for /effort
// argument completion) and shares resolveThinkingLevelsForModels with the
// session's setter, so a level offered by one can never be rejected by
// the other (#3731).
//
// It is read-only, so a single EffectiveModels snapshot is correct here. The
// setter must NOT call this: it needs the same snapshot for both resolution
// and provider recreation (a second, later snapshot could race a concurrent
// /model change or scoped override and validate against one model while
// applying to another).
func (r *LocalRuntime) resolveAgentThinkingLevels(ctx context.Context, agentName string) ([]effort.Level, effort.Level, error) {
	if _, err := r.modelSwitcherConfigForContext(ctx); err != nil {
		return nil, "", err
	}
	tm := r.team
	if env := teamEnvironmentFromContext(ctx); env != nil {
		tm = env.Team()
	}
	a, err := tm.Agent(agentName)
	if err != nil {
		return nil, "", fmt.Errorf("agent not found: %w", err)
	}

	return r.resolveThinkingLevelsForModels(ctx, a.EffectiveModels(ctx))
}

// CurrentAgentThinkingLevels returns the thinking-effort levels the current
// agent's active model supports, or nil when the runtime has no model
// switcher configured, the agent/model can't be resolved, or the model does
// not support thinking at all. Backs the /effort argument completer
// (#3731): candidates must come from this resolution, not the static effort
// vocabulary, because the thinking-level setter hard-rejects any level
// outside it.
func (r *LocalRuntime) CurrentAgentThinkingLevels(ctx context.Context) []effort.Level {
	supported, _, err := r.resolveAgentThinkingLevels(ctx, r.currentAgentName())
	if err != nil {
		return nil
	}
	return supported
}

// modelSupportsThinking reports whether cfg names a model that accepts a
// user-selectable thinking-effort level. It first trusts an explicit,
// enabled thinking_budget (covers reasoning models the heuristics below
// don't recognise), then falls back to per-provider model heuristics.
func (r *LocalRuntime) modelSupportsThinking(ctx context.Context, cfg *latest.ModelConfig) bool {
	if cfg.ThinkingBudget != nil && !cfg.ThinkingBudget.IsDisabled() {
		return true
	}
	if modelinfo.UsesReasoningEffort(cfg.Model) || modelinfo.UsesThinkingLevel(cfg.Model) {
		return true
	}
	model := strings.ToLower(cfg.Model)
	if strings.HasPrefix(model, "gemini-2.5") {
		return true
	}
	// Claude family (Anthropic / Bedrock / Vertex) supports adaptive thinking.
	if modelinfo.IsBedrockClaudeID(cfg.Model) || strings.HasPrefix(model, "claude-") {
		return true
	}
	store := r.modelsStore
	if modelConfig, err := r.modelSwitcherConfigForContext(ctx); err == nil {
		store = r.modelsStoreForConfig(modelConfig)
	}
	if store != nil {
		if m, err := store.GetModel(ctx, modelsdev.NewID(cfg.Provider, cfg.Model)); err == nil && m != nil {
			return modelinfo.IsClaudeFamily(m.Family)
		}
	}
	return false
}

// currentThinkingLevel maps a model config's thinking_budget onto an
// [effort.Level]. A nil, disabled, token-based, or adaptive budget is
// reported as [effort.None] so the first cycle step lands on a concrete
// effort level.
func currentThinkingLevel(cfg *latest.ModelConfig) effort.Level {
	if l, ok := cfg.ThinkingBudget.EffortLevel(); ok {
		return l
	}
	return effort.None
}

// setAgentModelInternal applies modelRef as the shared agent's model override
// and returns a snapshot of the value that was just stored. The snapshot is
// captured atomically with the store (it is the pointer returned by
// SetModelOverride itself), so there is no window where another caller
// could intervene and the snapshot would refer to a different value.
//
// This is the runtime-global override used for scoped per-toolset models
// (see WithAgentModel); user-facing model changes are session-scoped and go
// through the session instead.
func (r *LocalRuntime) setAgentModelInternal(ctx context.Context, agentName, modelRef string) (agent.ModelOverrideSnapshot, error) {
	if r.teamEnvironments != nil {
		return agent.ModelOverrideSnapshot{}, fmt.Errorf("runtime-global model overrides are unavailable in revision-bound mode: %w", ErrUnsupported)
	}
	if _, err := r.modelSwitcherConfigForContext(ctx); err != nil {
		return agent.ModelOverrideSnapshot{}, fmt.Errorf("model switching not configured for this operation: %w", err)
	}
	tm := r.team
	if env := teamEnvironmentFromContext(ctx); env != nil {
		tm = env.Team()
	}
	a, err := tm.Agent(agentName)
	if err != nil {
		return agent.ModelOverrideSnapshot{}, fmt.Errorf("agent not found: %w", err)
	}
	if modelRef == "" {
		snap := a.SetModelOverride()
		slog.InfoContext(ctx, "Cleared agent model override (using default)", "agent", agentName)
		return snap, nil
	}
	providers, err := r.resolveModelProviders(ctx, agentName, modelRef)
	if err != nil {
		return agent.ModelOverrideSnapshot{}, err
	}
	snap := a.SetModelOverride(providers...)
	slog.InfoContext(ctx, "Set agent model override", "agent", agentName, "model", modelRef, "model_count", len(providers))
	return snap, nil
}

// WithAgentModel applies modelRef as a model override on the named agent
// and returns a function that restores the previous override safely.
//
// The returned restore func is always non-nil. On success it uses
// pointer-identity compare-and-swap on the agent's override, so a
// concurrent change made between the apply and the restore (e.g. by the
// TUI model picker) is preserved instead of being clobbered. The post-
// apply snapshot is captured atomically with the store inside
// SetModelOverride, so there is no window where a concurrent change
// could be misattributed to this scope. On error the agent is left
// untouched and restore is a no-op, so callers can always defer it
// without nil-checking.
func (r *LocalRuntime) WithAgentModel(ctx context.Context, agentName, modelRef string) (restore func(), err error) {
	noop := func() {}
	if r.teamEnvironments != nil {
		return noop, fmt.Errorf("runtime-global model overrides are unavailable in revision-bound mode: %w", ErrUnsupported)
	}
	tm := r.team
	if env := teamEnvironmentFromContext(ctx); env != nil {
		tm = env.Team()
	} else if r.teamEnvironments != nil {
		return noop, errors.New("operation has no pinned team environment")
	}
	a, err := tm.Agent(agentName)
	if err != nil {
		return noop, fmt.Errorf("agent not found: %w", err)
	}
	prev := a.SnapshotModelOverride()
	ours, err := r.setAgentModelInternal(ctx, agentName, modelRef)
	if err != nil {
		return noop, err
	}
	return func() { a.RestoreModelOverride(prev, ours) }, nil
}

// resolveModelRef resolves a model reference to a single provider.
// The reference can be a named model from the config or an inline
// "provider/model" spec (e.g. "openai/gpt-4o-mini").
func (r *LocalRuntime) resolveModelRef(ctx context.Context, modelRef string) (provider.Provider, error) {
	config, err := r.modelSwitcherConfigForContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("model switching not configured for this operation: %w", err)
	}

	// Try named model from config first.
	if modelCfg, exists := config.Models[modelRef]; exists {
		if isAlloyModelConfig(modelCfg) {
			return nil, fmt.Errorf("model reference %q is an alloy (multi-model) config and cannot be used as a single model override", modelRef)
		}
		modelCfg.Name = modelRef
		return r.createProviderFromConfigWithConfig(ctx, &modelCfg, config)
	}

	// Try inline "provider/model" format.
	inlineCfg, err := latest.ParseModelRef(modelRef)
	if err != nil {
		return nil, fmt.Errorf("invalid model reference %q: expected a model name from config or 'provider/model' format", modelRef)
	}

	return r.createProviderFromConfigWithConfig(ctx, &inlineCfg, config)
}

// isAlloyModelConfig checks if a model config is an alloy model (multiple models).
func isAlloyModelConfig(cfg latest.ModelConfig) bool {
	return cfg.Provider == "" && strings.Contains(cfg.Model, ",")
}

// isInlineAlloySpec checks if a model reference is an inline alloy specification.
// An inline alloy is comma-separated provider/model specs like "openai/gpt-4o,anthropic/claude-sonnet-4-0".
func isInlineAlloySpec(modelRef string) bool {
	if !strings.Contains(modelRef, ",") {
		return false
	}
	// Check that each part looks like a provider/model spec
	// and count valid parts (need at least 2 for an alloy)
	validParts := 0
	for part := range strings.SplitSeq(modelRef, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			return false
		}
		validParts++
	}
	return validParts >= 2
}

// resolveModelRefs resolves a comma-separated list of model references into
// providers. Each reference is first looked up in the config by name; if not
// found it is parsed as an inline "provider/model" spec.
func (r *LocalRuntime) resolveModelRefs(ctx context.Context, commaSeparatedRefs string) ([]provider.Provider, error) {
	config, err := r.modelSwitcherConfigForContext(ctx)
	if err != nil {
		return nil, err
	}
	var providers []provider.Provider

	for ref := range strings.SplitSeq(commaSeparatedRefs, ",") {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}

		// Check if this ref exists as a named model in config
		if modelCfg, exists := config.Models[ref]; exists {
			modelCfg.Name = ref
			prov, err := r.createProviderFromConfigWithConfig(ctx, &modelCfg, config)
			if err != nil {
				return nil, fmt.Errorf("failed to create provider for %q: %w", ref, err)
			}
			providers = append(providers, prov)
			continue
		}

		// Parse as provider/model
		inlineCfg, parseErr := latest.ParseModelRef(ref)
		if parseErr != nil {
			return nil, fmt.Errorf("invalid model reference %q: expected 'provider/model' format or a named model from config", ref)
		}

		prov, err := r.createProviderFromConfigWithConfig(ctx, &inlineCfg, config)
		if err != nil {
			return nil, fmt.Errorf("failed to create provider for %q: %w", ref, err)
		}
		providers = append(providers, prov)
	}

	if len(providers) == 0 {
		return nil, errors.New("no valid models found in model reference list")
	}

	return providers, nil
}

func (r *LocalRuntime) availableModels(ctx context.Context, agentName string) []ModelChoice {
	start := time.Now()
	modelConfig, err := r.modelSwitcherConfigForContext(ctx)
	if err != nil {
		slog.DebugContext(ctx, "Runtime available models skipped; model switching not configured", "duration", time.Since(start))
		return nil
	}

	// Get the current agent's default model reference
	currentAgentDefault := ""
	if modelConfig.AgentDefaultModels != nil {
		currentAgentDefault = modelConfig.AgentDefaultModels[agentName]
	}

	var choices []ModelChoice

	configuredStart := time.Now()
	// Add all configured models, marking the current agent's default
	for name, cfg := range modelConfig.Models {
		choice := ModelChoice{
			Name:      name,
			Ref:       name,
			Provider:  cfg.Provider,
			Model:     cfg.DisplayOrModel(),
			ModelID:   cfg.Model,
			ModelName: cfg.Model,
			IsDefault: name == currentAgentDefault,
		}
		// Best-effort lookup of pricing / context information from models.dev.
		if cfg.Provider != "" && cfg.Model != "" {
			r.populateCatalogMetadataForStore(r.modelsStoreForConfig(modelConfig), &choice, cfg.Provider, cfg.Model)
		}
		// A config-declared price table overrides the catalogue's.
		if cfg.Cost != nil {
			choice.InputCost = cfg.Cost.Input
			choice.OutputCost = cfg.Cost.Output
			choice.CacheReadCost = cfg.Cost.CacheRead
			choice.CacheWriteCost = cfg.Cost.CacheWrite
		}
		if n := configuredContextWindow(cfg, modelConfig.Providers); n > 0 {
			choice.ContextLimit = int(n)
		}
		choices = append(choices, choice)
	}
	configuredDuration := time.Since(configuredStart)

	// Prefer live gateway discovery when a models gateway is configured:
	// the picker then only shows the models actually served by the
	// gateway, merged with the explicitly configured ones above. When the
	// gateway doesn't expose /v1/models, fall back to the models.dev
	// catalog filtered by available credentials.
	if modelConfig.ModelsGateway != "" {
		gatewayStart := time.Now()
		gatewayChoices, ok := r.buildGatewayChoicesForConfig(ctx, modelConfig)
		slog.DebugContext(ctx, "Runtime available models gateway discovery completed",
			"duration", time.Since(gatewayStart),
			"ok", ok,
			"models", len(gatewayChoices),
		)
		if ok {
			result := append(choices, gatewayChoices...)
			slog.DebugContext(ctx, "Runtime available models completed",
				"duration", time.Since(start),
				"configured_duration", configuredDuration,
				"configured_models", len(choices),
				"gateway_models", len(gatewayChoices),
				"catalog_models", 0,
				"dmr_models", 0,
				"models", len(result),
			)
			return result
		}
	}

	// Surface models pulled locally in Docker Model Runner. They are not part
	// of the models.dev catalog, so without this a working local Model Runner
	// would show nothing selectable in the picker.
	dmrStart := time.Now()
	dmrChoices := r.buildDMRChoicesForConfig(ctx, modelConfig)
	r.populateDMRChoices(ctx, modelConfig, choices)
	dmrDuration := time.Since(dmrStart)
	choices = append(choices, dmrChoices...)

	// Append models.dev catalog entries filtered by available credentials.
	// A directly constructed runtime may intentionally omit a catalog store.
	catalogStart := time.Now()
	var catalogChoices []ModelChoice
	if r.modelsStoreForConfig(modelConfig) != nil {
		catalogChoices = r.buildCatalogChoicesForConfig(ctx, modelConfig)
	}
	catalogDuration := time.Since(catalogStart)
	choices = append(choices, catalogChoices...)

	slog.DebugContext(ctx, "Runtime available models completed",
		"duration", time.Since(start),
		"configured_duration", configuredDuration,
		"dmr_duration", dmrDuration,
		"catalog_duration", catalogDuration,
		"configured_models", len(modelConfig.Models),
		"dmr_models", len(dmrChoices),
		"catalog_models", len(catalogChoices),
		"models", len(choices),
	)
	return choices
}

// buildCatalogChoices builds ModelChoice entries from the models.dev catalog,
// filtered by supported providers and available credentials.
func (r *LocalRuntime) buildCatalogChoices(ctx context.Context) []ModelChoice {
	modelConfig, err := r.modelSwitcherConfigForContext(ctx)
	if err != nil {
		return nil
	}
	return r.buildCatalogChoicesForConfig(ctx, modelConfig)
}

func (r *LocalRuntime) buildCatalogChoicesForConfig(ctx context.Context, modelConfig *ModelSwitcherConfig) []ModelChoice {
	if modelConfig == nil {
		return nil
	}
	start := time.Now()
	store := r.modelsStoreForConfig(modelConfig)
	if store == nil {
		return nil
	}
	dbStart := time.Now()
	db, err := store.GetDatabase(ctx)
	dbDuration := time.Since(dbStart)
	if err != nil {
		slog.DebugContext(ctx, "Failed to get models.dev database for catalog", "duration", time.Since(start), "database_duration", dbDuration, "error", err)
		return nil
	}

	// Build set of existing model refs to avoid duplicates
	existingRefs := make(map[string]bool)
	for name, cfg := range modelConfig.Models {
		existingRefs[name] = true
		if cfg.Provider != "" && cfg.Model != "" {
			existingRefs[cfg.Provider+"/"+cfg.Model] = true
		}
	}

	// Check which providers the user has credentials for
	providerStart := time.Now()
	availableProviders := r.getAvailableProvidersForConfig(ctx, modelConfig)
	providerDuration := time.Since(providerStart)
	if len(availableProviders) == 0 {
		slog.DebugContext(ctx, "No provider credentials available, skipping catalog",
			"duration", time.Since(start),
			"database_duration", dbDuration,
			"provider_duration", providerDuration,
		)
		return nil
	}

	iterateStart := time.Now()
	var choices []ModelChoice
	var providerCount, modelCount, skippedProvider, skippedNonText, skippedEmbedding, skippedDuplicate int
	for providerID, prov := range db.Providers {
		providerCount++
		// Check if this provider is supported and user has credentials
		dockerAgentProvider, supported := mapModelsDevProvider(providerID)
		if !supported {
			skippedProvider++
			continue
		}
		if !availableProviders[dockerAgentProvider] {
			skippedProvider++
			continue
		}

		for modelID, model := range prov.Models {
			modelCount++
			// Skip models that don't output text (not suitable for chat)
			if !slices.Contains(model.Modalities.Output, "text") {
				skippedNonText++
				continue
			}
			// Skip embedding models (not suitable for chat)
			if isEmbeddingModel(model.Family, model.Name) {
				skippedEmbedding++
				continue
			}

			ref := dockerAgentProvider + "/" + modelID
			if existingRefs[ref] {
				skippedDuplicate++
				continue
			}
			existingRefs[ref] = true

			choice := ModelChoice{
				Name:      model.Name,
				Ref:       ref,
				Provider:  dockerAgentProvider,
				Model:     modelID,
				ModelID:   modelID,
				ModelName: modelID,
				IsCatalog: true,
			}
			applyCatalogMetadata(&choice, &model)
			choices = append(choices, choice)
		}
	}

	slog.DebugContext(ctx, "Built catalog choices",
		"duration", time.Since(start),
		"database_duration", dbDuration,
		"provider_duration", providerDuration,
		"iterate_duration", time.Since(iterateStart),
		"providers", providerCount,
		"models_seen", modelCount,
		"models", len(choices),
		"available_providers", len(availableProviders),
		"skipped_provider", skippedProvider,
		"skipped_non_text", skippedNonText,
		"skipped_embedding", skippedEmbedding,
		"skipped_duplicate", skippedDuplicate,
	)
	return choices
}

// mapModelsDevProvider maps a models.dev provider ID to a docker agent provider name.
// Returns the docker agent provider name and whether it's supported.
// Uses provider.IsCatalogProvider to dynamically include all core providers
// and aliases with defined base URLs.
func mapModelsDevProvider(providerID string) (string, bool) {
	if provider.IsCatalogProvider(providerID) {
		return providerID, true
	}
	return "", false
}

// modelsStoreForConfig selects the models.dev store for a model-switching
// operation. An explicit WithModelStore always wins. Otherwise the store carried
// on the operation's ModelSwitcherConfig (the team revision's warmed store) is
// used, then the runtime's own store. Team-environment runtimes without a
// configured store return nil rather than falling back to the constructor store.
func (r *LocalRuntime) modelsStoreForConfig(config *ModelSwitcherConfig) ModelStore {
	if r.explicitModelsStore != nil {
		return r.explicitModelsStore
	}
	if config != nil && config.ModelsStore != nil {
		return config.ModelsStore
	}
	if r.teamEnvironments != nil {
		return nil
	}
	return r.modelsStore
}

func (r *LocalRuntime) populateCatalogMetadataForStore(store ModelStore, choice *ModelChoice, providerID, modelID string) {
	choice.ModelID = modelID
	choice.ModelName = modelID
	if m, ok := cachedModelDatabase(store).LookupModel(modelsdev.NewID(providerID, modelID)); ok {
		applyCatalogMetadata(choice, m)
	}
}

func (r *LocalRuntime) populateCatalogMetadata(_ context.Context, choice *ModelChoice, providerID, modelID string) {
	r.populateCatalogMetadataForStore(r.modelsStore, choice, providerID, modelID)
}

// applyCatalogMetadata copies pricing/limit/modality information from a
// models.dev Model entry onto a ModelChoice.
func applyCatalogMetadata(choice *ModelChoice, m *modelsdev.Model) {
	if m == nil {
		return
	}
	if strings.TrimSpace(m.Name) != "" {
		choice.ModelName = m.Name
	}
	choice.Family = m.Family
	if m.Cost != nil {
		choice.InputCost = m.Cost.Input
		choice.OutputCost = m.Cost.Output
		choice.CacheReadCost = m.Cost.CacheRead
		choice.CacheWriteCost = m.Cost.CacheWrite
	}
	choice.ContextLimit = m.Limit.Context
	choice.OutputLimit = m.Limit.Output
	choice.InputModalities = slices.Clone(m.Modalities.Input)
	choice.OutputModalities = slices.Clone(m.Modalities.Output)
}

// isEmbeddingModel returns true if the model is an embedding model
// based on its family or name fields from models.dev.
func isEmbeddingModel(family, name string) bool {
	familyLower := strings.ToLower(family)
	nameLower := strings.ToLower(name)
	return strings.Contains(familyLower, "embed") || strings.Contains(nameLower, "embed")
}

// getAvailableProviders returns the providers that model discovery/listing
// may surface for this runtime. Availability is credential-based (shared
// with the CLI models-listing fallback via
// [config.DiscoveryAvailableProviders]): a configured models gateway no
// longer replaces it with a hardcoded provider list, so when gateway
// discovery fails — or the gateway is a generic one without a Docker token —
// the catalog fallback still reflects the providers the user can actually
// reach directly.
func (r *LocalRuntime) getAvailableProviders(ctx context.Context) map[string]bool {
	modelConfig, err := r.modelSwitcherConfigForContext(ctx)
	if err != nil {
		return nil
	}
	return r.getAvailableProvidersForConfig(ctx, modelConfig)
}

func (r *LocalRuntime) getAvailableProvidersForConfig(ctx context.Context, modelConfig *ModelSwitcherConfig) map[string]bool {
	if modelConfig == nil {
		return nil
	}
	env := modelConfig.EnvProvider
	if env == nil {
		// A config without an environment provider has no credentials to
		// report; treat it as an empty environment rather than panicking.
		env = environment.NewNoEnvProvider()
	}

	available := config.DiscoveryAvailableProviders(ctx, env)

	// Mark anthropic available when any model or referenced provider in the
	// workspace has a non-API-key auth scheme configured (e.g. Workload
	// Identity Federation). We deliberately do not eagerly probe the token
	// source here: doing so would slow down startup for the common case
	// (file/env are fine, gcloud/az may take seconds, IMDS endpoints may
	// hang on non-cloud hosts). A misconfigured source surfaces as a clear
	// error on the first request via federation.RequestOptions.
	for _, m := range modelConfig.Models {
		if modelHasAnthropicAuth(m, modelConfig.Providers) {
			available["anthropic"] = true
			break
		}
	}

	// DMR and ollama don't require credentials (local models)
	available["dmr"] = true
	available["ollama"] = true

	return available
}

// modelHasAnthropicAuth reports whether the model (or its referenced
// ProviderConfig) declares a non-API-key auth scheme that targets the
// anthropic provider. Used by getAvailableProviders so that workspaces
// configured with Workload Identity Federation surface their Anthropic
// models without requiring ANTHROPIC_API_KEY.
func modelHasAnthropicAuth(m latest.ModelConfig, providers map[string]latest.ProviderConfig) bool {
	return latest.EffectiveProviderType(m, providers) == "anthropic" &&
		latest.EffectiveAuth(m, providers) != nil
}

// createProviderFromConfig creates a provider from a ModelConfig using the runtime's configuration.
func (r *LocalRuntime) createProviderFromConfig(ctx context.Context, cfg *latest.ModelConfig) (provider.Provider, error) {
	config, err := r.modelSwitcherConfigForContext(ctx)
	if err != nil {
		return nil, err
	}
	return r.createProviderFromConfigWithConfig(ctx, cfg, config)
}

func (r *LocalRuntime) createProviderFromConfigWithConfig(ctx context.Context, cfg *latest.ModelConfig, config *ModelSwitcherConfig) (provider.Provider, error) {
	if config == nil {
		return nil, ErrUnsupported
	}
	modelsStore := r.modelsStoreForConfig(config)
	opts := []options.Opt{
		options.WithGateway(config.ModelsGateway),
		options.WithEncryptedConfig(config.EncryptedConfig),
		options.WithProviders(config.Providers),
	}

	// Use max_tokens from config if specified, otherwise look up from models.dev
	if cfg.MaxTokens != nil {
		opts = append(opts, options.WithMaxTokens(*cfg.MaxTokens))
	} else if modelsStore != nil {
		m, err := modelsStore.GetModel(ctx, modelsdev.NewID(cfg.Provider, cfg.Model))
		if err == nil && m != nil {
			opts = append(opts, options.WithMaxTokens(m.Limit.Output))
		}
	}

	if store, ok := modelsStore.(*modelsdev.Store); ok && store != nil {
		opts = append(opts, options.WithModelsDevStore(store))
	}

	registry := config.ProviderRegistry
	if registry == nil {
		registry = r.providerRegistry
	}
	if registry == nil {
		registry = provider.DefaultRegistry()
	}
	return registry.NewWithModels(ctx,
		cfg,
		config.Models,
		config.EnvProvider,
		opts...,
	)
}

// WithModelSwitcherConfig sets the model switcher configuration for the runtime.
func WithModelSwitcherConfig(cfg *ModelSwitcherConfig) Opt {
	return func(r *LocalRuntime) {
		if cfg != nil && cfg.ProviderRegistry == nil {
			cfgCopy := *cfg
			cfgCopy.ProviderRegistry = r.providerRegistry
			cfg = &cfgCopy
		}
		if cfg != nil && cfg.ProviderRegistry != nil {
			r.providerRegistry = cfg.ProviderRegistry
		}
		r.modelSwitcherCfg = cfg
	}
}

// configuredContextWindow resolves aliases and inherited DMR sizing without mutating config.
func configuredContextWindow(cfg latest.ModelConfig, providers map[string]latest.ProviderConfig) int64 {
	resolved := cfg
	resolved.Provider = provider.ResolveType(&cfg, providers)
	resolved.ProviderOpts = maps.Clone(cfg.ProviderOpts)
	if _, present := resolved.ProviderOpts["context_size"]; !present {
		if size, inherited := providers[cfg.Provider].ProviderOpts["context_size"]; inherited {
			if resolved.ProviderOpts == nil {
				resolved.ProviderOpts = make(map[string]any)
			}
			resolved.ProviderOpts["context_size"] = size
		}
	}
	return resolved.EffectiveContextWindow()
}
