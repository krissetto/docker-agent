package gemini

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
)

// Diagnostic: pins CURRENT buildConfig output for effort strings on Gemini 2.5 vs Gemini 3.
func TestInvestigation_GeminiEffortThinkingWire(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		model string
		level string
	}{
		{name: "gemini-2.5-flash high", model: "gemini-2.5-flash", level: "high"},
		{name: "gemini-2.5-pro medium", model: "gemini-2.5-pro", level: "medium"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &Client{Config: base.Config{ModelConfig: latest.ModelConfig{
				Provider:       "google",
				Model:          tc.model,
				ThinkingBudget: &latest.ThinkingBudget{Effort: tc.level},
			}}}
			config := mustBuildConfig(t, client)
			require.NotNil(t, config.ThinkingConfig)
			assert.True(t, config.ThinkingConfig.IncludeThoughts)
			assert.Equal(t, genai.ThinkingLevel(""), config.ThinkingConfig.ThinkingLevel)
			require.NotNil(t, config.ThinkingConfig.ThinkingBudget, "effort on Gemini 2.5 currently sends thinkingBudget")
			assert.Equal(t, int32(0), *config.ThinkingConfig.ThinkingBudget)
		})
	}

	for _, tc := range []struct {
		name  string
		model string
		level string
		want  genai.ThinkingLevel
	}{
		{name: "gemini-3-pro high", model: "gemini-3-pro", level: "high", want: genai.ThinkingLevelHigh},
		{name: "gemini-3-flash medium", model: "gemini-3-flash", level: "medium", want: genai.ThinkingLevelMedium},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &Client{Config: base.Config{ModelConfig: latest.ModelConfig{
				Provider:       "google",
				Model:          tc.model,
				ThinkingBudget: &latest.ThinkingBudget{Effort: tc.level},
			}}}
			config := mustBuildConfig(t, client)
			require.NotNil(t, config.ThinkingConfig)
			assert.True(t, config.ThinkingConfig.IncludeThoughts)
			assert.Equal(t, tc.want, config.ThinkingConfig.ThinkingLevel)
			assert.Nil(t, config.ThinkingConfig.ThinkingBudget, "Gemini 3 effort maps to thinkingLevel, not thinkingBudget")
		})
	}
}
