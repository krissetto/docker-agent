package latest

import (
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContextSizeFromProviderOpts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts map[string]any
		want int64
	}{
		{"absent", map[string]any{}, 0},
		{"nil map", nil, 0},
		{"int", map[string]any{"context_size": 32768}, 32768},
		{"int64", map[string]any{"context_size": int64(65536)}, 65536},
		{"int32", map[string]any{"context_size": int32(4096)}, 4096},
		// goccy/go-yaml decodes a positive YAML integer as uint64 (see #3387).
		{"uint64", map[string]any{"context_size": uint64(262144)}, 262144},
		{"uint", map[string]any{"context_size": uint(8192)}, 8192},
		{"float64 (json)", map[string]any{"context_size": float64(8192)}, 8192},
		{"string decimal", map[string]any{"context_size": " 16384 "}, 16384},
		{"zero", map[string]any{"context_size": 0}, 0},
		{"negative", map[string]any{"context_size": int64(-1)}, 0},
		{"unparseable string", map[string]any{"context_size": "big"}, 0},
		{"wrong type", map[string]any{"context_size": true}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ContextSizeFromProviderOpts(tt.opts))
		})
	}
}

// TestContextSizeFromProviderOpts_YAMLRoundTrip guards the real decode path:
// a context_size declared in YAML must resolve to its value. goccy/go-yaml
// produces uint64 for positive integers, which a naive int/int64 switch drops
// to 0 (the latent bug behind #3387's failed context_window workaround).
func TestContextSizeFromProviderOpts_YAMLRoundTrip(t *testing.T) {
	t.Parallel()

	var opts map[string]any
	require.NoError(t, yaml.Unmarshal([]byte("context_size: 262144\n"), &opts))
	assert.Equal(t, int64(262144), ContextSizeFromProviderOpts(opts))
}

func TestDeclaredContextWindow(t *testing.T) {
	t.Parallel()

	size := func(n int64) *int64 { return &n }
	tests := []struct {
		name string
		cfg  *ModelConfig
		want int64
	}{
		{"nil model", nil, 0},
		{"neither set", &ModelConfig{}, 0},
		{"model-level", &ModelConfig{ContextWindow: size(200_000)}, 200_000},
		{"non-positive model-level", &ModelConfig{ContextWindow: size(0)}, 0},
		{"provider_opts is never read", &ModelConfig{Provider: "dmr", ProviderOpts: map[string]any{"context_size": 8192}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.cfg.DeclaredContextWindow())
		})
	}
}

func TestContextWindow(t *testing.T) {
	t.Parallel()

	size := func(n int64) *int64 { return &n }
	tests := []struct {
		name string
		cfg  *ModelConfig
		want int64
	}{
		{"nil model", nil, 0},
		{"neither set", &ModelConfig{Provider: "dmr"}, 0},
		{"model-level on any provider", &ModelConfig{Provider: "openai", ContextWindow: size(200_000)}, 200_000},
		{"DMR provider_opts", &ModelConfig{Provider: "dmr", ProviderOpts: map[string]any{"context_size": 8192}}, 8192},
		{"model-level wins over DMR provider_opts", &ModelConfig{Provider: "dmr", ContextWindow: size(200_000), ProviderOpts: map[string]any{"context_size": 8192}}, 200_000},
		{"non-positive model-level falls back to DMR provider_opts", &ModelConfig{Provider: "dmr", ContextWindow: size(0), ProviderOpts: map[string]any{"context_size": 8192}}, 8192},
		{"provider_opts ignored on openai", &ModelConfig{Provider: "openai", ProviderOpts: map[string]any{"context_size": 8192}}, 0},
		{"provider_opts ignored on an openai_responses gateway", &ModelConfig{Provider: "gateway", ProviderOpts: map[string]any{"api_type": "openai_responses", "context_size": 8192}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.cfg.EffectiveContextWindow())
		})
	}
}

// TestContextWindow_YAML checks the model-level field decodes from YAML as a
// plain integer and stays out of provider_opts (so it is never forwarded).
func TestContextWindow_YAML(t *testing.T) {
	t.Parallel()

	var cfg Config
	src := `
version: "16"
models:
  gateway_model:
    provider: openai_responses_gateway
    model: my-model
    context_window: 128000
agents:
  root:
    model: gateway_model
    instruction: hi
`
	require.NoError(t, yaml.Unmarshal([]byte(src), &cfg))
	m := cfg.Models["gateway_model"]
	require.NotNil(t, m.ContextWindow)
	assert.Equal(t, int64(128_000), *m.ContextWindow)
	assert.Empty(t, m.ProviderOpts, "model-level context_window must not be copied into provider_opts")
	assert.Equal(t, int64(128_000), m.DeclaredContextWindow())
}

func TestContextWindowValidation(t *testing.T) {
	t.Parallel()

	size := func(n int64) *int64 { return &n }
	openResponses := ProviderConfig{APIType: "openai_responses", BaseURL: "https://gateway.example.com/v1"}
	tests := []struct {
		name      string
		models    map[string]ModelConfig
		providers map[string]ProviderConfig
		wantErr   string
	}{
		{
			name:   "model-level value on a non-DMR model is accepted",
			models: map[string]ModelConfig{"m": {Provider: "openai", Model: "gpt-5", ContextWindow: size(200_000)}},
		},
		{
			name:    "zero is rejected",
			models:  map[string]ModelConfig{"m": {Provider: "openai", Model: "gpt-5", ContextWindow: size(0)}},
			wantErr: "models.m: context_window must be greater than 0, got 0",
		},
		{
			name:    "negative is rejected",
			models:  map[string]ModelConfig{"m": {Provider: "openai", Model: "gpt-5", ContextWindow: size(-1)}},
			wantErr: "models.m: context_window must be greater than 0, got -1",
		},
		{
			name:    "provider_opts.context_size on a non-DMR model is rejected with a pointer to context_window",
			models:  map[string]ModelConfig{"m": {Provider: "openai", Model: "gpt-5", ProviderOpts: map[string]any{"context_size": 8192}}},
			wantErr: `models.m: provider_opts.context_size is only used by the dmr provider, not "openai"; move it to context_window on this model`,
		},
		{
			name: "provider_opts.context_size on a custom openai_responses alias is rejected by resolved type",
			models: map[string]ModelConfig{"m": {
				Provider: "gateway", Model: "my-model", ProviderOpts: map[string]any{"context_size": 8192},
			}},
			providers: map[string]ProviderConfig{"gateway": openResponses},
			wantErr:   `provider_opts.context_size is only used by the dmr provider, not "openai_responses"`,
		},
		{
			name: "provider_opts.context_size inherited from a non-DMR custom provider is rejected",
			models: map[string]ModelConfig{"m": {
				Provider: "gateway", Model: "my-model",
			}},
			providers: map[string]ProviderConfig{"gateway": {
				APIType:      "openai_responses",
				ProviderOpts: map[string]any{"context_size": 8192},
			}},
			wantErr: `providers.gateway.provider_opts.context_size is only used by the dmr provider, not "openai_responses"`,
		},
		{
			name: "a custom alias backed by dmr may keep provider_opts.context_size",
			models: map[string]ModelConfig{"m": {
				Provider: "local", Model: "ai/qwen3", ProviderOpts: map[string]any{"context_size": 8192},
			}},
			providers: map[string]ProviderConfig{"local": {Provider: "dmr"}},
		},
		{
			name: "a custom alias backed by dmr with a model-level context_window that matches",
			models: map[string]ModelConfig{"m": {
				Provider: "local", Model: "ai/qwen3", ContextWindow: size(8192),
				ProviderOpts: map[string]any{"context_size": 8192},
			}},
			providers: map[string]ProviderConfig{"local": {Provider: "dmr"}},
		},
		{
			name: "dmr model-level context_window equal to provider_opts is accepted",
			models: map[string]ModelConfig{"m": {
				Provider: "dmr", Model: "ai/qwen3", ContextWindow: size(8192),
				ProviderOpts: map[string]any{"context_size": 8192},
			}},
		},
		{
			name: "dmr model-level context_window conflicting with provider_opts is rejected",
			models: map[string]ModelConfig{"m": {
				Provider: "dmr", Model: "ai/qwen3", ContextWindow: size(8192),
				ProviderOpts: map[string]any{"context_size": 4096},
			}},
			wantErr: "context_window 8192 conflicts with provider_opts.context_size 4096",
		},
		{
			name: "dmr conflict with provider_opts inherited from a custom provider is rejected",
			models: map[string]ModelConfig{"m": {
				Provider: "local", Model: "ai/qwen3", ContextWindow: size(8192),
			}},
			providers: map[string]ProviderConfig{"local": {
				Provider: "dmr", ProviderOpts: map[string]any{"context_size": 4096},
			}},
			wantErr: "context_window 8192 conflicts with providers.local.provider_opts.context_size 4096",
		},
		{
			name: "model's own provider_opts override the inherited one for DMR",
			models: map[string]ModelConfig{"m": {
				Provider: "local", Model: "ai/qwen3", ContextWindow: size(8192),
				ProviderOpts: map[string]any{"context_size": 8192},
			}},
			providers: map[string]ProviderConfig{"local": {
				Provider: "dmr", ProviderOpts: map[string]any{"context_size": 4096},
			}},
		},
		{
			name: "first_available cannot combine with context_window",
			models: map[string]ModelConfig{"sel": {
				FirstAvailable: []string{"a", "b"}, ContextWindow: size(8192),
			}},
			wantErr: "first_available cannot be combined with context_window",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{Models: tt.models, Providers: tt.providers}
			err := cfg.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
