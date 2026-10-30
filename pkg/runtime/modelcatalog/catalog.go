// Package modelcatalog holds the model picker value types and the
// configuration needed to build them. Discovery (gateway, DMR, models.dev
// listing) and the lazy store live in pkg/runtime; this package has no
// dependency on the runtime so consumers can import the types directly.
package modelcatalog

import (
	"context"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// ModelChoice represents a model available for selection in the model picker.
//
// JSON tags are part of the public wire format used by
// GET /api/sessions/:id/models; renaming a tag is a breaking change.
type ModelChoice struct {
	// Name is the display name (config key)
	Name string `json:"name"`
	// Ref is the model reference used internally (e.g., "my_model" or "openai/gpt-4o")
	Ref string `json:"ref"`
	// Provider is the provider name (e.g., "openai", "anthropic")
	Provider string `json:"provider,omitempty"`
	// Model is the specific model name (e.g., "gpt-4o", "claude-sonnet-4-0")
	Model string `json:"model,omitempty"`
	// ModelID is the canonical model ID, distinct from a configured display alias.
	ModelID string `json:"model_id,omitempty"`
	// ModelName is the friendly catalog name, falling back to the canonical ID.
	ModelName string `json:"model_name,omitempty"`
	// IsDefault indicates this is the agent's configured default model
	IsDefault bool `json:"is_default,omitempty"`
	// IsCurrent indicates this is the currently active model for the agent
	IsCurrent bool `json:"is_current,omitempty"`
	// IsCustom indicates this is a custom model from the session history (not from config)
	IsCustom bool `json:"is_custom,omitempty"`
	// IsCatalog indicates this is a model from the models.dev catalog
	IsCatalog bool `json:"is_catalog,omitempty"`

	// The fields below are populated (best-effort) from the models.dev
	// catalog. They are optional and may all be zero/empty when no
	// catalog entry is found for the model.

	// Family is the model family (e.g., "claude", "gpt").
	Family string `json:"family,omitempty"`
	// InputCost is the price (in USD) per 1M input tokens.
	InputCost float64 `json:"input_cost,omitempty"`
	// OutputCost is the price (in USD) per 1M output tokens.
	OutputCost float64 `json:"output_cost,omitempty"`
	// CacheReadCost is the price (in USD) per 1M cached input tokens.
	CacheReadCost float64 `json:"cache_read_cost,omitempty"`
	// CacheWriteCost is the price (in USD) per 1M cache-write tokens.
	CacheWriteCost float64 `json:"cache_write_cost,omitempty"`
	// ContextLimit is the maximum context window size in tokens.
	ContextLimit int `json:"context_limit,omitempty"`
	// OutputLimit is the maximum number of tokens the model can produce
	// in a single response.
	OutputLimit int64 `json:"output_limit,omitempty"`
	// InputModalities lists the input modalities supported by the model
	// (e.g., "text", "image", "audio").
	InputModalities []string `json:"input_modalities,omitempty"`
	// OutputModalities lists the output modalities the model can produce.
	OutputModalities []string `json:"output_modalities,omitempty"`
}

// SessionModelsResponse is the response returned by
// GET /api/sessions/:id/models. CurrentModelRef is the active override for
// the named agent (empty when the agent is using its configured default).
type SessionModelsResponse struct {
	Agent           string        `json:"agent"`
	CurrentModelRef string        `json:"current_model_ref,omitempty"`
	Models          []ModelChoice `json:"models"`
}

// DecorateModelChoices marks the active selection with IsCurrent and
// appends any custom (provider/model) refs from the session history that
// the runtime does not already expose. It is used by every consumer that
// wants to render a model picker (the TUI App, the HTTP /sessions/:id/models
// endpoint, …) so they all agree on which entry is current and what the
// final list looks like.
//
// currentRef is the model override active for the agent ("" when none),
// and customRefs is the session's CustomModelsUsed history.
//
// The input slice is never mutated: callers can safely pass a slice that
// is shared with or backed by an internal cache.
func DecorateModelChoices(models []ModelChoice, currentRef string, customRefs []string) []ModelChoice {
	// Defensive copy: AvailableModels implementations may return a slice
	// backed by an internal cache. Mutating its IsCurrent flag in place
	// would leak picker state across sessions/agents.
	result := make([]ModelChoice, len(models), len(models)+len(customRefs)+1)
	copy(result, models)

	existingRefs := make(map[string]bool, len(result))
	for _, m := range result {
		existingRefs[m.Ref] = true
	}

	currentFound := currentRef == ""
	for i := range result {
		if currentRef != "" {
			if result[i].Ref == currentRef {
				result[i].IsCurrent = true
				currentFound = true
			}
		} else {
			result[i].IsCurrent = result[i].IsDefault
		}
	}

	for _, ref := range customRefs {
		if existingRefs[ref] {
			continue
		}
		existingRefs[ref] = true

		prov, name, _ := strings.Cut(ref, "/")
		isCurrent := ref == currentRef
		if isCurrent {
			currentFound = true
		}
		result = append(result, ModelChoice{
			Name:      ref,
			Ref:       ref,
			Provider:  prov,
			Model:     name,
			ModelID:   name,
			ModelName: name,
			IsCurrent: isCurrent,
			IsCustom:  true,
		})
	}

	// If the override points at an inline provider/model not in the
	// runtime's list nor in the session's history, fabricate a synthetic
	// choice so the picker can still highlight the active selection.
	if !currentFound && strings.Contains(currentRef, "/") {
		prov, name, _ := strings.Cut(currentRef, "/")
		result = append(result, ModelChoice{
			Name:      currentRef,
			Ref:       currentRef,
			Provider:  prov,
			Model:     name,
			ModelID:   name,
			ModelName: name,
			IsCurrent: true,
			IsCustom:  true,
		})
	}

	return result
}

// Config holds the configuration needed for model switching.
// This is populated by the app layer when creating the runtime.
type Config struct {
	// Models is the map of model names to configurations from the loaded config
	Models map[string]latest.ModelConfig
	// Providers is the map of custom provider configurations
	Providers map[string]latest.ProviderConfig
	// ModelsGateway is the gateway URL if configured
	ModelsGateway string
	// EncryptedConfig is the opaque encrypted agent YAML forwarded to a trusted
	// Docker models gateway on every request (X-Cagent-Encrypted-Config header).
	// Empty disables forwarding.
	EncryptedConfig string
	// EnvProvider provides access to environment variables
	EnvProvider environment.Provider
	// ProviderRegistry instantiates providers for runtime model switching.
	ProviderRegistry *provider.Registry
	// AgentDefaultModels maps agent names to their configured default model references
	AgentDefaultModels map[string]string
	// ModelsStore is the models.dev catalog store used for the picker's
	// pricing/context/modality metadata and the catalog listing. When set, the
	// runtime adopts it instead of building its own (cold) lazy store, so a
	// store the team loader already warmed is reused. Optional: a nil store
	// falls back to the lazy default. An explicit WithModelStore takes
	// precedence over this field.
	ModelsStore Store
}

// Store provides model metadata and the complete catalog.
type Store interface {
	GetModel(ctx context.Context, id modelsdev.ID) (*modelsdev.Model, error)
	GetDatabase(ctx context.Context) (*modelsdev.Database, error)
}
