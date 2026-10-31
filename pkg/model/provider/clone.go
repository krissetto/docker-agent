package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/internal/deepcopy"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

// ErrNoRebuilder means a provider has neither a rebuild function nor the
// registry that created it, so it cannot be cloned.
var ErrNoRebuilder = errors.New("provider cannot be cloned without its rebuild function or factory registry")

// ErrUncopyableConfig means mutable configuration cannot be safely copied.
var ErrUncopyableConfig = errors.New("provider clone requires copyable configuration")

// CloneError identifies the provider whose reconstruction failed.
type CloneError struct {
	ID  modelsdev.ID
	Err error
}

func (e *CloneError) Error() string { return fmt.Sprintf("cloning provider %q: %v", e.ID, e.Err) }
func (e *CloneError) Unwrap() error { return e.Err }

// Clone reconstructs a provider with ordered option overrides, or returns an
// error. Non-zero token caps also constrain routed children. The last non-zero
// cap wins; zero retains the latest explicit cap.
func Clone(ctx context.Context, p Provider, opts ...options.Opt) (Provider, error) {
	if p == nil {
		return nil, errors.New("cannot clone a nil provider")
	}
	cfg := p.BaseConfig()
	modelConfig, mergedOpts, err := mergeCloneOptions(cfg, opts)
	if err != nil {
		return nil, &CloneError{ID: p.ID(), Err: err}
	}
	clone, err := rebuildProvider(ctx, cfg, &modelConfig, mergedOpts)
	if err != nil {
		return nil, &CloneError{ID: p.ID(), Err: err}
	}
	return clone, nil
}

// rebuildProvider prefers the construction function captured by the factory
// and falls back to the registry that created the provider.
func rebuildProvider(ctx context.Context, cfg base.Config, modelConfig *latest.ModelConfig, opts []options.Opt) (Provider, error) {
	if cfg.RebuildProvider != nil {
		clone, err := cfg.RebuildProvider(ctx, modelConfig, opts...)
		if err == nil && clone == nil {
			err = errors.New("rebuild returned a nil provider")
		}
		return clone, err
	}
	registry, _ := cfg.ProviderRegistry.(*Registry)
	if registry == nil {
		return nil, ErrNoRebuilder
	}
	return registry.NewWithModels(ctx, modelConfig, cfg.Models, cfg.Env, opts...)
}

// CloneWithOptions reconstructs a provider, returning an error-only provider on
// failure. The failure is reported when the clone creates its completion
// stream, so callers that ignore the returned error still see it.
//
// Deprecated: use Clone to handle reconstruction errors before making a request.
func CloneWithOptions(ctx context.Context, p Provider, opts ...options.Opt) Provider {
	clone, requested, err := cloneProvider(ctx, p, opts)
	if err == nil {
		return clone
	}
	slog.WarnContext(ctx, "Failed to clone provider", "error", err)
	var id modelsdev.ID
	if p != nil {
		id = p.ID()
	}
	return &failedCloneProvider{config: requested, id: id, err: err}
}

// cloneProvider reconstructs p and also returns the configuration that was
// requested, so a failed clone still reports what it was asked to do.
func cloneProvider(ctx context.Context, p Provider, opts []options.Opt) (Provider, base.Config, error) {
	if p == nil {
		return nil, base.Config{}, errors.New("cannot clone a nil provider")
	}
	cfg := p.BaseConfig()
	modelConfig, mergedOpts, err := mergeCloneOptions(cfg, opts)
	if err != nil {
		return nil, base.Config{}, &CloneError{ID: p.ID(), Err: err}
	}
	requested := base.Config{ModelConfig: modelConfig, ModelOptions: options.Apply(mergedOpts...), Env: cfg.Env, BaseURL: cfg.BaseURL}
	clone, err := rebuildProvider(ctx, cfg, &modelConfig, mergedOpts)
	if err != nil {
		return nil, requested, &CloneError{ID: p.ID(), Err: err}
	}
	return clone, requested, nil
}

// failedCloneProvider is returned by CloneWithOptions when reconstruction fails.
type failedCloneProvider struct {
	config base.Config
	id     modelsdev.ID
	err    error
}

func (p *failedCloneProvider) ID() modelsdev.ID        { return p.id }
func (p *failedCloneProvider) BaseConfig() base.Config { return p.config }
func (p *failedCloneProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, p.err
}

// mergeCloneOptions snapshots mutable state before reconstructing a provider.
// Later opts take precedence.
func mergeCloneOptions(cfg base.Config, opts []options.Opt) (latest.ModelConfig, []options.Opt, error) {
	baseOptions, err := cfg.ModelOptions.Clone()
	if err != nil {
		return latest.ModelConfig{}, nil, fmt.Errorf("%w: %w", ErrUncopyableConfig, err)
	}
	mergedOpts := slices.Concat(options.FromModelOptions(baseOptions), opts)

	// The last explicit non-zero cap applies to routed children as well.
	var requested options.ModelOptions
	var explicitCap int64
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(&requested)
		if mt := requested.MaxTokens(); mt != 0 {
			explicitCap = mt
		}
	}
	if explicitCap != 0 {
		mergedOpts = append(mergedOpts, options.WithAuxiliaryMaxTokens(explicitCap))
	} else if baseOptions.MaxTokensInheritable() && baseOptions.MaxTokens() != 0 {
		mergedOpts = append(mergedOpts, options.WithAuxiliaryMaxTokens(baseOptions.MaxTokens()))
	}

	merged := options.Apply(mergedOpts...)
	modelConfig, err := deepcopy.Value(cfg.CloneSource())
	if err != nil {
		return latest.ModelConfig{}, nil, fmt.Errorf("%w: copying model configuration: %w", ErrUncopyableConfig, err)
	}
	if mt := merged.MaxTokens(); mt != 0 {
		modelConfig.MaxTokens = &mt
	}
	if merged.NoThinking() {
		// Write the disabled sentinel instead of nil so the downstream
		// applyProviderDefaults pass cannot revive a provider-level
		// thinking_budget via its setIfNil merge.
		modelConfig.ThinkingBudget = &latest.ThinkingBudget{Effort: "none"}
	}
	return modelConfig, mergedOpts, nil
}

// copyModelConfig returns an isolated copy of a model configuration.
func copyModelConfig(source latest.ModelConfig) (latest.ModelConfig, error) {
	copied, err := deepcopy.Value(source)
	if err != nil {
		return latest.ModelConfig{}, fmt.Errorf("%w: copying model configuration: %w", ErrUncopyableConfig, err)
	}
	return copied, nil
}
