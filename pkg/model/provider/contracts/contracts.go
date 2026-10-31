// Package contracts defines the interfaces and shared state used by model providers.
// It has no dependency on provider construction, allowing implementations and
// orchestration packages to share one canonical contract.
package contracts

import (
	"context"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/catalog"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/tools"
)

// Provider defines the interface for model providers.
type Provider interface {
	// ID returns the provider-qualified model identity. Returning a
	// [modelsdev.ID] (rather than a bare string) prevents callers from
	// silently forgetting to namespace the model when it crosses an API
	// boundary; use [modelsdev.ID.String] when a textual representation
	// is required.
	ID() modelsdev.ID
	// CreateChatCompletionStream creates a streaming chat completion request.
	// It returns a stream that can be iterated over to get completion chunks.
	CreateChatCompletionStream(
		ctx context.Context,
		messages []chat.Message,
		tools []tools.Tool,
	) (chat.MessageStream, error)
	// BaseConfig returns the base configuration of this provider.
	BaseConfig() Config
}

// ServingModel identifies the endpoint that produced a completion stream.
type ServingModel struct {
	ID     modelsdev.ID
	Config Config
}

// ServingStream reports the serving endpoint when it differs from the dispatched provider.
// The metadata is available at stream creation and remains stable after Close.
type ServingStream interface {
	chat.MessageStream
	ServingModel() ServingModel
}

// ConversationCompactor is a provider that can compact a conversation
// server-side, returning a readable summary plus an opaque block it can
// later replay in place of the compacted history.
type ConversationCompactor interface {
	Provider
	// CompactConversation compacts messages, the fully assembled prompt the
	// conversation model would receive (system messages included), with the
	// same request tools. instructions is optional user steering for the
	// summary. Returning a result with an empty Summary means nothing was
	// compacted and the caller leaves the transcript untouched.
	CompactConversation(ctx context.Context, messages []chat.Message, requestTools []tools.Tool, instructions string) (*chat.CompactionResult, error)
}

// ServedStream reports, once the stream has ended, the model that served the
// response when it isn't the dispatched one (e.g. a server-side fallback).
// Wrappers must forward it rather than snapshot it at creation.
type ServedStream interface {
	chat.MessageStream
	ServedBy() (ServingModel, bool)
}

// EmbeddingProvider defines the interface for providers that support embeddings.
type EmbeddingProvider interface {
	Provider
	// CreateEmbedding generates an embedding vector for the given text with usage tracking.
	CreateEmbedding(ctx context.Context, text string) (*EmbeddingResult, error)
}

// BatchEmbeddingProvider defines the interface for providers that support batch embeddings.
type BatchEmbeddingProvider interface {
	EmbeddingProvider
	// CreateBatchEmbedding generates embedding vectors for multiple texts with usage tracking.
	// Returns embeddings in the same order as input texts.
	CreateBatchEmbedding(ctx context.Context, texts []string) (*BatchEmbeddingResult, error)
}

// RerankingProvider defines the interface for providers that support reranking.
// Reranking models score query-document pairs to assess relevance.
type RerankingProvider interface {
	Provider
	// Rerank scores documents by relevance to the query.
	// Returns relevance scores in the same order as input documents.
	// Scores are typically in [0, 1] range where higher means more relevant.
	// criteria: Optional domain-specific guidance for relevance scoring (appended to base prompt)
	// documents: Array of types.Document with content and metadata
	Rerank(ctx context.Context, query string, documents []types.Document, criteria string) ([]float64, error)
}

// RebuildProviderFunc reconstructs a provider with adjusted options.
type RebuildProviderFunc func(
	ctx context.Context,
	cfg *latest.ModelConfig,
	opts ...options.Opt,
) (Provider, error)

// Config is a common base configuration shared by all provider clients.
// It can be embedded in provider-specific Client structs to avoid code duplication.
type Config struct {
	ModelConfig  latest.ModelConfig
	ModelOptions options.ModelOptions
	Env          environment.Provider
	// DispatchConfig is the configuration the provider was constructed from,
	// before built-in defaults such as catalogue base URLs were applied. Clones
	// rebuild from it so defaults are not mistaken for user overrides.
	DispatchConfig *latest.ModelConfig
	// Models stores the full models map for providers that need it (e.g., routers).
	// This enables proper cloning of providers that reference other models.
	Models map[string]latest.ModelConfig
	// ProviderRegistry stores the registry that created this provider. It is kept
	// as any to avoid a package cycle; pkg/model/provider type-asserts it when
	// cloning providers with adjusted options.
	ProviderRegistry any
	// RebuildProvider reconstructs this provider with adjusted options. The
	// provider package sets it at construction so clones do not depend on the
	// registry type.
	RebuildProvider RebuildProviderFunc
	// BaseURL is the resolved HTTP base URL the client talks to, when
	// the provider is reachable over a configurable HTTP endpoint.
	// Distinct from [latest.ModelConfig.BaseURL] (the user-typed input):
	// providers fill BaseURL with the URL they actually use after auto
	// discovery / fallback (e.g. Docker Model Runner picking between
	// MODEL_RUNNER_HOST, the desktop socket, and a localhost fallback).
	// Surfaced through [Config.BaseConfig] so generic, runtime-free
	// consumers like hooks can address the endpoint without duplicating
	// resolution logic. Empty for providers that don't expose a stable
	// per-instance URL.
	BaseURL string
}

// SetDispatchConfig records the construction configuration used for clones.
func (c *Config) SetDispatchConfig(cfg *latest.ModelConfig) {
	c.DispatchConfig = cfg
}

// CloneSource returns the construction configuration when one was recorded,
// and the resolved model configuration otherwise.
func (c *Config) CloneSource() latest.ModelConfig {
	if c.DispatchConfig != nil {
		return *c.DispatchConfig
	}
	return c.ModelConfig
}

// SetProviderRegistry records the registry that created this provider.
func (c *Config) SetProviderRegistry(registry any) {
	c.ProviderRegistry = registry
}

// SetProviderRebuilder records the function that reconstructs this provider.
func (c *Config) SetProviderRebuilder(rebuild RebuildProviderFunc) {
	c.RebuildProvider = rebuild
}

// ID returns the provider and model identity as a [modelsdev.ID] so
// callers cannot accidentally pass a bare model string where a
// provider-qualified identity is required. The model component uses
// DisplayModel (the original user-configured name) when available,
// falling back to Model (the resolved/pinned name).
func (c *Config) ID() modelsdev.ID {
	return modelsdev.NewID(c.ModelConfig.Provider, c.ModelConfig.DisplayOrModel())
}

// BaseConfig returns a copy of the shared configuration.
func (c *Config) BaseConfig() Config {
	return *c
}

// TrackUsageEnabled reports whether token-usage tracking is enabled for this
// model. Tracking defaults to on and is disabled only when the config
// explicitly sets track_usage: false.
func (c *Config) TrackUsageEnabled() bool {
	return c.ModelConfig.TrackUsage == nil || *c.ModelConfig.TrackUsage
}

// CapsOverride returns the model's explicit attachment-capability override
// derived from its config, or nil when the config declares none (the common
// case, in which capabilities are detected from models.dev). Provider clients
// pass the result to [modelinfo.ResolveCaps] so a user-declared override wins
// over a models.dev lookup that would otherwise miss for custom/aliased
// providers and degrade attachments to text-only (issue #2741).
//
// All four fields (Image, PDF, Audio, Video) are read from the config's
// capabilities block when one is present.
func (c *Config) CapsOverride() *modelinfo.CapsOverride {
	caps := c.ModelConfig.Capabilities
	if caps == nil {
		return nil
	}
	return &modelinfo.CapsOverride{Image: caps.Image, PDF: caps.PDF, Audio: caps.Audio, Video: caps.Video}
}

// ToolCallSupport resolves the model's tool-call capability from models.dev.
func (c *Config) ToolCallSupport(ctx context.Context) modelinfo.ToolCallSupport {
	return modelinfo.ResolveToolCallSupport(ctx, c.ModelOptions.ModelsDevStore(), c.ID())
}

// ImageOutputEnabled resolves the model's image-output capability from its
// explicit tri-state configuration and, when unset, the models.dev catalogue.
func (c *Config) ImageOutputEnabled(ctx context.Context) bool {
	var override *bool
	if caps := c.ModelConfig.OutputCapabilities; caps != nil {
		override = caps.Image
	}
	return modelinfo.ResolveOutputImage(ctx, c.ModelOptions.ModelsDevStore(), c.ID(), override)
}

// NativeToolSearchEnabled reports whether provider_opts.native_tool_search
// opts this model into provider-hosted tool search. Strictly opt-in and gated
// to verified endpoints (see [modelinfo.SupportsHostedToolSearch]); only then
// do [tools.Tool.InCatalog] tools reach the provider through tool search. A
// user-supplied base_url means an OpenAI-compatible server, not
// api.openai.com, and is excluded like elsewhere in the openai provider; the
// resolved [Config.BaseURL] is deliberately not consulted so tests can point
// the SDK at a local server. An explicit api_type of openai_chatcompletions
// routes off the Responses API, which alone serves tool_search.
func (c *Config) NativeToolSearchEnabled() bool {
	if !catalog.IsDefaultEndpoint(c.ModelConfig.Provider, c.ModelConfig.BaseURL) || c.ModelConfig.ProviderOpts["api_type"] == "openai_chatcompletions" {
		return false
	}
	enabled, _ := providerutil.GetProviderOptBool(c.ModelConfig.ProviderOpts, "native_tool_search")
	return enabled && modelinfo.SupportsHostedToolSearch(c.ModelConfig.Provider, c.ModelConfig.Model)
}

// EmbeddingResult contains the embedding and usage information
type EmbeddingResult struct {
	Embedding   []float64
	InputTokens int64
	TotalTokens int64
	Cost        float64
}

// BatchEmbeddingResult contains multiple embeddings and usage information
type BatchEmbeddingResult struct {
	Embeddings  [][]float64
	InputTokens int64
	TotalTokens int64
	Cost        float64
}
