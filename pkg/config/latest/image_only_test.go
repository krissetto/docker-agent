package latest

import (
	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestIsImageOnly(t *testing.T) {
	assert.False(t, (*ModelConfig)(nil).IsImageOnly())
	assert.False(t, (&ModelConfig{Provider: "openai_images"}).IsImageOnly())
	assert.True(t, (&ModelConfig{ProviderOpts: map[string]any{"api_type": "openai_images"}}).IsImageOnly())
	assert.False(t, (&ModelConfig{OutputCapabilities: &OutputCapabilitiesConfig{Image: new(true)}}).IsImageOnly())
}

func TestValidateImageGeneration(t *testing.T) {
	providers := map[string]ProviderConfig{"images": {APIType: "openai_images"}, "responses": {APIType: "openai_responses"}, "chat": {APIType: "openai_chatcompletions"}}
	for _, tc := range []struct {
		name string
		cfg  ModelConfig
		want string
	}{
		{"alias", ModelConfig{Provider: "openai_images"}, "named provider"},
		{"model protocol", ModelConfig{ProviderOpts: map[string]any{"api_type": "openai_images"}}, "named provider"},
		{"old settings", ModelConfig{Provider: "openai", ProviderOpts: map[string]any{"image_generation": map[string]any{}}}, "move settings"},
		{"disabled images", ModelConfig{Provider: "images"}, "requires output_capabilities.image: true"},
		{"settings never enable", ModelConfig{Provider: "openai", ImageGeneration: map[string]any{}}, "requires output_capabilities.image: true"},
		{"images", ModelConfig{Provider: "images", OutputCapabilities: &OutputCapabilitiesConfig{Image: new(true)}}, ""},
		{"responses", ModelConfig{Provider: "responses", OutputCapabilities: &OutputCapabilitiesConfig{Image: new(true)}, ImageGeneration: map[string]any{"model": "gpt-image-1"}}, ""},
		{"chat output", ModelConfig{Provider: "chat", OutputCapabilities: &OutputCapabilitiesConfig{Image: new(true)}}, ""},
		{"chat settings", ModelConfig{Provider: "chat", OutputCapabilities: &OutputCapabilitiesConfig{Image: new(true)}, ImageGeneration: map[string]any{}}, "requires openai_images or openai_responses"},
		{"reserved", ModelConfig{Provider: "openai", OutputCapabilities: &OutputCapabilitiesConfig{Image: new(true)}, ImageGeneration: map[string]any{"prompt": "override"}}, "cannot override prompt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.ValidateImageGeneration(providers)
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

func TestImageGenerationRoundTrip(t *testing.T) {
	var cfg FlexibleModelConfig
	require.NoError(t, yaml.Unmarshal([]byte("provider: openai\nmodel: test\noutput_capabilities:\n  image: true\nimage_generation: {}\n"), &cfg))
	require.NotNil(t, cfg.ImageGeneration)
	require.NotNil(t, cfg.Clone().ImageGeneration, "empty settings must survive JSON cloning")
	encoded, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	var roundtrip FlexibleModelConfig
	require.NoError(t, yaml.Unmarshal(encoded, &roundtrip))
	require.NotNil(t, roundtrip.ImageGeneration, "empty settings must survive YAML roundtrip")
	assert.False(t, cfg.isShorthandOnly())
	cfg.ImageGeneration["size"] = "512x512"
	clone := cfg.Clone()
	clone.ImageGeneration["size"] = "other"
	assert.Equal(t, "512x512", cfg.ImageGeneration["size"])
	cfg.FirstAvailable = []string{"openai/test"}
	cfg.Provider = ""
	cfg.Model = ""
	cfg.OutputCapabilities = nil
	require.ErrorContains(t, cfg.validateFirstAvailable(), "image_generation")
}
