package anthropic

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func TestHaikuThinkingRequests(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		model        string
		budget       *latest.ThinkingBudget
		display      string
		noThinking   bool
		wantThinking map[string]any
		wantEffort   string
	}{
		// No budget: adaptive thinking with the summarized display and no effort (upstream wire shape).
		{name: "default", wantThinking: map[string]any{"type": "adaptive", "display": "summarized"}},
		{name: "adaptive high", budget: &latest.ThinkingBudget{Effort: "adaptive/high"}, wantEffort: "high"},
		{name: "adaptive xhigh", budget: &latest.ThinkingBudget{Effort: "adaptive/xhigh"}, wantEffort: "xhigh"},
		{name: "Vertex alias", model: "claude-haiku-5-5@default", budget: &latest.ThinkingBudget{Effort: "adaptive/xhigh"}, wantEffort: "xhigh"},
		{name: "dated snapshot", model: "claude-haiku-5-5-20260901", budget: &latest.ThinkingBudget{Effort: "adaptive/high"}, wantEffort: "high"},
		{name: "plain xhigh", budget: &latest.ThinkingBudget{Effort: "xhigh"}, wantEffort: "xhigh"},
		{name: "max", budget: &latest.ThinkingBudget{Effort: "adaptive/max"}, wantEffort: "max"},
		// A token budget coerced to adaptive on Claude 5.x leaves effort to the model default (upstream rule).
		{name: "token budget", budget: &latest.ThinkingBudget{Tokens: 32768}, wantThinking: map[string]any{"type": "adaptive", "display": "summarized"}},
		{name: "omitted display", budget: &latest.ThinkingBudget{Effort: "high"}, display: "omitted", wantThinking: map[string]any{"type": "adaptive", "display": "omitted"}, wantEffort: "high"},
		{name: "explicit summarized", budget: &latest.ThinkingBudget{Effort: "adaptive/xhigh"}, display: "summarized", wantEffort: "xhigh"},
		{name: "none", budget: &latest.ThinkingBudget{Effort: "none"}, wantThinking: map[string]any{"type": "disabled"}},
		{name: "zero", budget: &latest.ThinkingBudget{}, wantThinking: map[string]any{"type": "disabled"}},
		// NoThinking applies only to a nil budget (upstream); an explicit budget still sends thinking.
		{name: "no thinking keeps explicit xhigh", budget: &latest.ThinkingBudget{Effort: "adaptive/xhigh"}, noThinking: true, wantEffort: "xhigh"},
		{name: "no thinking keeps explicit max", budget: &latest.ThinkingBudget{Effort: "max"}, noThinking: true, wantEffort: "max"},
		{name: "older Haiku effort", model: "claude-haiku-4-5", budget: &latest.ThinkingBudget{Effort: "adaptive/high"}, wantThinking: map[string]any{"type": "enabled", "budget_tokens": float64(16384)}},
		{name: "older Haiku default", model: "claude-haiku-4-5"},
	}
	for _, beta := range []bool{false, true} {
		path := "standard"
		if beta {
			path = "beta"
		}
		for _, tc := range cases {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				type request struct {
					body  []byte
					betas string
				}
				requests := make(chan request, 1)
				srv := anthropicTestServer(t, func(r *http.Request, body []byte) {
					requests <- request{body: body, betas: strings.Join(r.Header.Values("anthropic-beta"), ",")}
				})
				model := tc.model
				if model == "" {
					model = "claude-haiku-5-5"
				}
				cfg := &latest.ModelConfig{Provider: "anthropic", Model: model, BaseURL: srv.URL, ThinkingBudget: tc.budget, ProviderOpts: map[string]any{"interleaved_thinking": beta}}
				if tc.display != "" {
					cfg.ProviderOpts["thinking_display"] = tc.display
				}
				var opts []options.Opt
				if tc.noThinking {
					opts = append(opts, options.WithNoThinking())
				}
				client, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "test-key"}), opts...)
				require.NoError(t, err)
				stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
				require.NoError(t, err)
				drain(stream)
				require.Len(t, requests, 1, "expected an outgoing request")
				got := <-requests
				// Haiku 5.5+ (checks thinking prefixes) routes through the Beta Messages API
				// even with thinking_prefix_mismatch unset, which adds the block binding. The
				// interleaved-thinking header follows the explicit interleaved_thinking option.
				assert.Equal(t, beta, strings.Contains(got.betas, "interleaved-thinking-2025-05-14"))
				var body map[string]any
				require.NoError(t, json.Unmarshal(got.body, &body))
				want := tc.wantThinking
				if want == nil && tc.wantEffort != "" {
					want = map[string]any{"type": "adaptive", "display": "summarized"}
				}
				if want != nil && want["type"] != "disabled" && checksThinkingPrefix(model) {
					want = map[string]any{
						"type":          want["type"],
						"display":       want["display"],
						"block_binding": map[string]any{"prefix_mismatch_behavior": "drop_block"},
					}
				}
				if want == nil {
					assert.NotContains(t, body, "thinking")
				} else {
					assert.Equal(t, want, body["thinking"])
				}
				if tc.wantEffort == "" {
					assert.NotContains(t, body, "output_config")
				} else {
					assert.Equal(t, map[string]any{"effort": tc.wantEffort}, body["output_config"])
				}
				if model == "claude-haiku-5-5" {
					assert.InDelta(t, 8192, body["max_tokens"], 0.001)
				}
				assert.Same(t, tc.budget, cfg.ThinkingBudget)
			})
		}
	}
}

func TestHaikuRejectsFullThinkingDisplay(t *testing.T) {
	t.Parallel()
	for _, model := range []string{"claude-haiku-5-5", "claude-haiku-5-5@default"} {
		t.Run(model, func(t *testing.T) {
			t.Parallel()
			cfg := &latest.ModelConfig{Provider: "anthropic", Model: model, ProviderOpts: map[string]any{"thinking_display": "display"}}
			_, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(nil))
			require.ErrorContains(t, err, "does not support thinking_display")
		})
	}
	cfg := &latest.ModelConfig{Model: "claude-haiku-4-5", ProviderOpts: map[string]any{"thinking_display": "display", "fallbacks": []string{"claude-haiku-5-5"}}}
	require.ErrorContains(t, validateThinkingDisplay(cfg), `model "claude-haiku-5-5"`)
}
