package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestEffectiveCompactionModelRef(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Providers: map[string]latest.ProviderConfig{
			"corp": {Provider: "anthropic", CompactionModel: "provider-default"},
		},
		Models: map[string]latest.ModelConfig{
			"plain":     {Provider: "anthropic", Model: "claude-sonnet-4-5"},
			"with":      {Provider: "anthropic", Model: "claude-sonnet-4-5", CompactionModel: "model-level"},
			"corporate": {Provider: "corp", Model: "claude-sonnet-4-5"},
		},
	}

	tests := []struct {
		name  string
		agent latest.AgentConfig
		want  string
	}{
		{name: "agent-level wins", agent: latest.AgentConfig{Model: "with", CompactionModel: "agent-level"}, want: "agent-level"},
		{name: "model-level wins over provider-level", agent: latest.AgentConfig{Model: "corporate,with"}, want: "model-level"},
		{name: "provider-level default", agent: latest.AgentConfig{Model: "corporate"}, want: "provider-default"},
		{name: "first model with a value wins", agent: latest.AgentConfig{Model: "plain,with"}, want: "model-level"},
		{name: "none set", agent: latest.AgentConfig{Model: "plain"}, want: ""},
		{name: "unknown model", agent: latest.AgentConfig{Model: "missing"}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, EffectiveCompactionModelRef(cfg, &tt.agent))
		})
	}
}

func TestEffectiveCompactionPrompt(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Providers: map[string]latest.ProviderConfig{
			"corp": {Provider: "anthropic", CompactionPrompt: "provider prompt"},
		},
		Models: map[string]latest.ModelConfig{
			"plain":     {Provider: "anthropic", Model: "claude-sonnet-4-5"},
			"with":      {Provider: "anthropic", Model: "claude-sonnet-4-5", CompactionPrompt: "model prompt"},
			"corporate": {Provider: "corp", Model: "claude-sonnet-4-5"},
		},
	}

	tests := []struct {
		name  string
		agent latest.AgentConfig
		want  string
	}{
		{name: "agent-level wins", agent: latest.AgentConfig{Model: "with", CompactionPrompt: "agent prompt"}, want: "agent prompt"},
		{name: "model-level wins over provider-level", agent: latest.AgentConfig{Model: "corporate,with"}, want: "model prompt"},
		{name: "provider-level default", agent: latest.AgentConfig{Model: "corporate"}, want: "provider prompt"},
		{name: "first model with a value wins", agent: latest.AgentConfig{Model: "plain,with"}, want: "model prompt"},
		{name: "none set", agent: latest.AgentConfig{Model: "plain"}, want: ""},
		{name: "unknown model", agent: latest.AgentConfig{Model: "missing"}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, EffectiveCompactionPrompt(cfg, &tt.agent))
		})
	}
}

// A first_available selector referenced only through a provider-level
// compaction_model default must still be resolved at load time.
func TestResolveFirstAvailableModels_ProviderCompactionModel(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Agents: []latest.AgentConfig{{Name: "root", Model: "primary"}},
		Providers: map[string]latest.ProviderConfig{
			"corp": {Provider: "anthropic", CompactionModel: "summarizer"},
		},
		Models: map[string]latest.ModelConfig{
			"primary":    {Provider: "corp", Model: "claude-sonnet-4-5"},
			"summarizer": {FirstAvailable: []string{"openai/gpt-4o-mini"}},
		},
	}

	env := environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test-key"})
	require.NoError(t, ResolveFirstAvailableModels(t.Context(), cfg, "", env))

	got := cfg.Models["summarizer"]
	assert.Equal(t, "openai", got.Provider)
	assert.Equal(t, "gpt-4o-mini", got.Model)
}

// Same for a selector referenced through an agent-level compaction_model.
func TestResolveFirstAvailableModels_AgentCompactionModel(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Agents: []latest.AgentConfig{{Name: "root", Model: "primary", CompactionModel: "summarizer"}},
		Models: map[string]latest.ModelConfig{
			"primary":    {Provider: "anthropic", Model: "claude-sonnet-4-5"},
			"summarizer": {FirstAvailable: []string{"openai/gpt-4o-mini"}},
		},
	}

	env := environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test-key"})
	require.NoError(t, ResolveFirstAvailableModels(t.Context(), cfg, "", env))

	got := cfg.Models["summarizer"]
	assert.Equal(t, "openai", got.Provider)
	assert.Equal(t, "gpt-4o-mini", got.Model)
}

// The credentials of the effective compaction model must surface in the same
// consolidated preflight as the primary model's, for named references, inline
// specs, and provider-level defaults alike.
func TestGatherEnvVarsForModels_CompactionModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *latest.Config
		want []string
	}{
		{
			name: "inline agent-level compaction model",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{{Name: "root", Model: "primary", CompactionModel: "openai/gpt-4o-mini"}},
				Models: map[string]latest.ModelConfig{
					"primary": {Provider: "anthropic", Model: "claude-sonnet-4-5"},
				},
			},
			want: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"},
		},
		{
			name: "named model-level compaction model",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{{Name: "root", Model: "primary"}},
				Models: map[string]latest.ModelConfig{
					"primary": {Provider: "anthropic", Model: "claude-sonnet-4-5", CompactionModel: "fast"},
					"fast":    {Provider: "openai", Model: "gpt-4o-mini"},
				},
			},
			want: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"},
		},
		{
			name: "provider-level compaction model default",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{{Name: "root", Model: "primary"}},
				Providers: map[string]latest.ProviderConfig{
					"corp": {Provider: "anthropic", CompactionModel: "openai/gpt-4o-mini"},
				},
				Models: map[string]latest.ModelConfig{
					"primary": {Provider: "corp", Model: "claude-sonnet-4-5"},
				},
			},
			want: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := GatherEnvVarsForModels(t.Context(), tt.cfg, environment.NewNoEnvProvider())
			assert.Equal(t, tt.want, got)
		})
	}
}
