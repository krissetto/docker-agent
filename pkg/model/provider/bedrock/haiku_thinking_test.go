package bedrock

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func TestHaikuThinkingFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		budget     *latest.ThinkingBudget
		noThinking bool
		display    string
		wantEffort string
		// adaptiveNoEffort: adaptive thinking with no output_config.effort (upstream
		// rule for a Claude 5.x token budget coerced to adaptive).
		adaptiveNoEffort bool
	}{
		// No budget: adaptive thinking with the summarized display and no effort (upstream wire shape).
		{name: "default", adaptiveNoEffort: true},
		{name: "high", budget: &latest.ThinkingBudget{Effort: "adaptive/high"}, wantEffort: "high"},
		{name: "xhigh", budget: &latest.ThinkingBudget{Effort: "adaptive/xhigh"}, wantEffort: "xhigh"},
		{name: "max", budget: &latest.ThinkingBudget{Effort: "max"}, wantEffort: "max"},
		{name: "tokens", budget: &latest.ThinkingBudget{Tokens: 32768}, adaptiveNoEffort: true},
		{name: "omitted", budget: &latest.ThinkingBudget{Effort: "high"}, display: "omitted", wantEffort: "high"},
		{name: "none", budget: &latest.ThinkingBudget{Effort: "none"}},
		{name: "zero", budget: &latest.ThinkingBudget{}},
		// NoThinking applies only to a nil budget (upstream); an explicit budget still sends thinking.
		{name: "no thinking keeps explicit xhigh", budget: &latest.ThinkingBudget{Effort: "xhigh"}, noThinking: true, wantEffort: "xhigh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := bedrockThinkingClient("global.anthropic.claude-haiku-5-5-v1:0", tc.budget)
			if tc.noThinking {
				options.WithNoThinking()(&client.ModelOptions)
			}
			if tc.display != "" {
				client.ModelConfig.ProviderOpts = map[string]any{"thinking_display": tc.display}
			}
			result := client.buildAdditionalModelRequestFields()
			require.NotNil(t, result)
			fields := decodeRequestFields(t, result)
			if tc.wantEffort == "" && !tc.adaptiveNoEffort {
				assert.Equal(t, map[string]any{"type": "disabled"}, fields["thinking"])
				assert.NotContains(t, fields, "output_config")
				assert.False(t, client.isThinkingEnabled())
			} else {
				display := tc.display
				if display == "" {
					display = "summarized"
				}
				assert.Equal(t, map[string]any{"type": "adaptive", "display": display}, fields["thinking"])
				if tc.adaptiveNoEffort {
					assert.NotContains(t, fields, "output_config")
				} else {
					assert.Equal(t, map[string]any{"effort": tc.wantEffort}, fields["output_config"])
				}
				assert.True(t, client.isThinkingEnabled())
			}
			assert.NotContains(t, fields, "anthropic_beta")
			assert.Same(t, tc.budget, client.ModelConfig.ThinkingBudget)
		})
	}
}
