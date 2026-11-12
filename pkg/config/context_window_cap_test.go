package config

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// capStore is a models.dev store backed by a fixed catalogue. Its fetcher always
// fails, so nothing reaches the network and only this catalogue is consulted.
func capStore(t *testing.T) *modelsdev.Store {
	t.Helper()
	db := &modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"openai": {Models: map[string]modelsdev.Model{
			"gpt-4o":      {Limit: modelsdev.Limit{Context: 128_000}},
			"gpt-4o-mini": {Limit: modelsdev.Limit{Context: 128_000}},
		}},
	}}
	store, err := modelsdev.NewStore(
		modelsdev.WithCache(filepath.Join(t.TempDir(), "catalog.json")),
		modelsdev.WithFetcher(func(context.Context, string) (*modelsdev.Database, string, error) {
			return nil, "", assert.AnError
		}),
		modelsdev.WithFallback(func() *modelsdev.Database { return db }),
	)
	require.NoError(t, err)
	return store
}

func TestValidateContextWindowCaps(t *testing.T) {
	t.Parallel()

	size := func(n int64) *int64 { return &n }
	tests := []struct {
		name      string
		models    map[string]latest.ModelConfig
		providers map[string]latest.ProviderConfig
		wantErr   string
	}{
		{
			name:   "at the catalogue max is allowed",
			models: map[string]latest.ModelConfig{"m": {Provider: "openai", Model: "gpt-4o", ContextWindow: size(128_000)}},
		},
		{
			name:   "below the catalogue max is allowed",
			models: map[string]latest.ModelConfig{"m": {Provider: "openai", Model: "gpt-4o", ContextWindow: size(32_000)}},
		},
		{
			name:    "above the catalogue max is rejected naming the model, field and max",
			models:  map[string]latest.ModelConfig{"big": {Provider: "openai", Model: "gpt-4o", ContextWindow: size(200_000)}},
			wantErr: "models.big: context_window 200000 exceeds the context window of openai/gpt-4o, which is 128000 tokens in the models.dev catalogue; set models.big.context_window to at most 128000",
		},
		{
			name:   "a model the catalogue does not describe accepts any positive value",
			models: map[string]latest.ModelConfig{"m": {Provider: "openai", Model: "not-in-catalogue", ContextWindow: size(10_000_000)}},
		},
		{
			name:   "an openai_responses gateway with no catalogue identity accepts any positive value",
			models: map[string]latest.ModelConfig{"m": {Provider: "gateway", Model: "gpt-4o", ContextWindow: size(10_000_000)}},
			providers: map[string]latest.ProviderConfig{"gateway": {
				APIType: "openai_responses", BaseURL: "https://gateway.example.com/v1",
			}},
		},
		{
			name:   "a custom provider backed by openai is checked against openai's catalogue",
			models: map[string]latest.ModelConfig{"m": {Provider: "proxy", Model: "gpt-4o", ContextWindow: size(200_000)}},
			providers: map[string]latest.ProviderConfig{"proxy": {
				Provider: "openai", BaseURL: "https://proxy.example.com/v1",
			}},
			wantErr: "models.m: context_window 200000 exceeds the context window of openai/gpt-4o",
		},
		{
			name: "a dedicated compaction model entry is checked like any other model",
			models: map[string]latest.ModelConfig{
				"primary":    {Provider: "openai", Model: "gpt-4o", CompactionModel: "summarizer"},
				"summarizer": {Provider: "openai", Model: "gpt-4o-mini", ContextWindow: size(200_000)},
			},
			wantErr: "models.summarizer: context_window 200000 exceeds the context window of openai/gpt-4o-mini",
		},
		{
			name:    "the raw catalogue limit is compared, so a larger declared value cannot hide the conflict",
			models:  map[string]latest.ModelConfig{"m": {Provider: "openai", Model: "gpt-4o", ContextWindow: size(129_000)}},
			wantErr: "exceeds the context window of openai/gpt-4o, which is 128000 tokens",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &latest.Config{Models: tt.models, Providers: tt.providers}
			err := ValidateContextWindowCaps(t.Context(), cfg, capStore(t))
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidateContextWindowCapsWithoutCatalogue(t *testing.T) {
	t.Parallel()

	size := int64(8192)
	cfg := &latest.Config{Models: map[string]latest.ModelConfig{"m": {Provider: "openai", Model: "gpt-4o", ContextWindow: &size}}}
	err := ValidateContextWindowCaps(t.Context(), cfg, nil)
	require.Error(t, err, "a declared window must not be accepted unchecked when the catalogue is unavailable")
	assert.Contains(t, err.Error(), "models.m: cannot check context_window")

	none := &latest.Config{Models: map[string]latest.ModelConfig{"m": {Provider: "openai", Model: "gpt-4o"}}}
	require.NoError(t, ValidateContextWindowCaps(t.Context(), none, nil), "no declared window needs no catalogue")
}
