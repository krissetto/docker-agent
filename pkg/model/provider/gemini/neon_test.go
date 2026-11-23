package gemini

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
)

func TestNeonThinkingConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		budget latest.ThinkingBudget
		want   genai.ThinkingLevel
	}{
		{"legacy token budget", latest.ThinkingBudget{Tokens: 8192}, ""},
		{"dynamic budget", latest.ThinkingBudget{Tokens: -1}, ""},
		{"explicit xhigh", latest.ThinkingBudget{Effort: "xhigh"}, genai.ThinkingLevelXhigh},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{Config: base.Config{ModelConfig: latest.ModelConfig{Model: "gemini-4-neon", ThinkingBudget: &tc.budget}}}
			cfg := mustBuildConfig(t, c)
			require.NotNil(t, cfg.ThinkingConfig)
			assert.Nil(t, cfg.ThinkingConfig.ThinkingBudget)
			assert.Equal(t, tc.want, cfg.ThinkingConfig.ThinkingLevel)
		})
	}
}

func TestNeonNumericThinkingOnWire(t *testing.T) {
	for _, surface := range geminiSurfaces {
		t.Run(surface.name, func(t *testing.T) {
			server, captured := newBodyCapturingGeminiServer(t, writeGeminiSSEResponse)
			c := surface.newClient(t, server.URL, nil)
			c.ModelConfig.Model = "gemini-4-neon"
			c.ModelConfig.ThinkingBudget = &latest.ThinkingBudget{Tokens: 8192}
			stream, err := c.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
			require.NoError(t, err)
			drainStream(t, stream)
			bodies := captured.all()
			require.Len(t, bodies, 1)
			var req struct {
				GenerationConfig struct {
					ThinkingConfig map[string]any `json:"thinkingConfig"`
				} `json:"generationConfig"`
			}
			require.NoError(t, json.Unmarshal(bodies[0], &req))
			assert.Equal(t, map[string]any{"includeThoughts": true}, req.GenerationConfig.ThinkingConfig)
		})
	}
}
