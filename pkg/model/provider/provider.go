// Package provider builds and dispatches to LLM provider clients.
//
// The package is organised across several files:
//
//   - provider.go (this file): the public Provider interfaces and the entry
//     points [New] and [NewWithModels] that callers use to construct a
//     provider from a model config.
//   - aliases.go: the built-in provider alias table (OpenAI-compatible
//     gateways such as ollama, mistral, xai, ...) and the helpers that expose
//     it to other packages without leaking the underlying map.
//   - defaults.go: pure config-merging logic that fills in defaults from
//     custom providers, built-in aliases, and model-specific rules
//     (thinking budget, interleaved thinking, ...).
//   - factory.go: shared dispatch from a resolved provider type to the
//     concrete client constructor, plus the rule-based router.
//
// Optional SDK-backed providers live outside this package's default import
// graph. YAML-loading applications that need Docker Agent's full provider set
// should import pkg/model/provider/providers and pass its explicit registry;
// embedders that build agents manually can import only the concrete provider
// packages they use and pass their factories to [NewRegistry].
package provider

import (
	"context"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/contracts"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// Provider is the common interface implemented by model providers.
type Provider = contracts.Provider

// ServingModel identifies the endpoint that produced a completion stream.
type ServingModel = contracts.ServingModel

// ServingStream reports the serving endpoint when it differs from the dispatched provider.
type ServingStream = contracts.ServingStream

// ServedStream reports a model that served the response in place of the dispatched one.
type ServedStream = contracts.ServedStream

// ServedBy reports the model that served a finished stream in place of the
// dispatched one; false when the stream doesn't say or nothing changed.
func ServedBy(stream chat.MessageStream) (ServingModel, bool) {
	if served, ok := stream.(ServedStream); ok {
		return served.ServedBy()
	}
	return ServingModel{}, false
}

// EmbeddingProvider is a provider that supports embeddings.
type EmbeddingProvider = contracts.EmbeddingProvider

// BatchEmbeddingProvider is an embedding provider that supports batches.
type BatchEmbeddingProvider = contracts.BatchEmbeddingProvider

// RerankingProvider is a provider that can score documents by relevance.
type RerankingProvider = contracts.RerankingProvider

// New creates a provider with an empty registry and therefore returns an
// unknown-provider error for every concrete provider.
//
// Deprecated: construct a Registry explicitly and call Registry.New.
func New(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return EmptyRegistry().New(ctx, cfg, env, opts...)
}

// NewWithModels creates a provider with an empty registry and therefore
// returns an unknown-provider error for every concrete provider.
//
// Deprecated: construct a Registry explicitly and call Registry.NewWithModels.
func NewWithModels(ctx context.Context, cfg *latest.ModelConfig, models map[string]latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return EmptyRegistry().NewWithModels(ctx, cfg, models, env, opts...)
}

// ContextWindowResolver returns an optional live context-window lookup, preserving
// access through instrumentation wrappers. Providers without discovery return nil.
func ContextWindowResolver(p Provider) func(context.Context) (int64, error) {
	if resolver, ok := unwrapProvider(p).(interface {
		ContextWindow(ctx context.Context) (int64, error)
	}); ok {
		return resolver.ContextWindow
	}
	return nil
}
