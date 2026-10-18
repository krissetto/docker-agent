package provider

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func TestHaikuDisabledBudgetDefaults(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{"anthropic", "amazon-bedrock", "google", "openai"} {
		for _, model := range []string{"claude-haiku-5-5", "claude-haiku-4-5"} {
			for _, budget := range []*latest.ThinkingBudget{{Effort: "none"}, {}} {
				t.Run(provider+"/"+model+"/"+budget.Effort, func(t *testing.T) {
					t.Parallel()
					cfg := &latest.ModelConfig{Provider: provider, Model: model, ThinkingBudget: budget, ProviderOpts: map[string]any{"publisher": "anthropic"}}
					applyModelDefaults(cfg)
					if model == "claude-haiku-5-5" && provider != "openai" {
						assert.Same(t, budget, cfg.ThinkingBudget)
					} else {
						assert.Nil(t, cfg.ThinkingBudget)
					}
				})
			}
		}
	}
}

func TestHaikuRegistryAndNoThinkingCloneRequests(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"default", "high", "xhigh", "none", "zero", "no thinking clone"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			requests := make(chan []byte, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				requests <- body
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			t.Cleanup(srv.Close)
			cfg := &latest.ModelConfig{Provider: "anthropic", Model: "claude-haiku-5-5", BaseURL: srv.URL}
			switch mode {
			case "high", "xhigh":
				cfg.ThinkingBudget = &latest.ThinkingBudget{Effort: "adaptive/" + mode}
			case "none":
				cfg.ThinkingBudget = &latest.ThinkingBudget{Effort: "none"}
			case "zero":
				cfg.ThinkingBudget = &latest.ThinkingBudget{}
			case "no thinking clone":
				cfg.ThinkingBudget = &latest.ThinkingBudget{Effort: "adaptive/xhigh"}
			}
			registry := NewRegistry(map[string]Factory{"anthropic": Adapt(anthropic.NewClient)})
			client, err := registry.New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "test-key"}))
			require.NoError(t, err)
			if mode == "no thinking clone" {
				client = CloneWithOptions(t.Context(), client, options.WithNoThinking())
			}
			stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, nil)
			require.NoError(t, err)
			for {
				if _, err := stream.Recv(); err != nil {
					break
				}
			}
			stream.Close()
			require.Len(t, requests, 1, "expected an outgoing request")
			var body map[string]any
			require.NoError(t, json.Unmarshal(<-requests, &body))
			switch mode {
			case "none", "zero", "no thinking clone":
				assert.Equal(t, map[string]any{"type": "disabled"}, body["thinking"])
				assert.NotContains(t, body, "output_config")
			default:
				effort := mode
				if mode == "default" {
					effort = "medium"
				}
				assert.Equal(t, map[string]any{"type": "adaptive", "display": "summarized"}, body["thinking"])
				assert.Equal(t, map[string]any{"effort": effort}, body["output_config"])
			}
		})
	}
}

func TestHaikuThinkingDefaults(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{"anthropic", "amazon-bedrock", "google", "openai"} {
		for _, model := range []string{"claude-haiku-5-5", "claude-haiku-4-5"} {
			t.Run(provider+"/"+model, func(t *testing.T) {
				t.Parallel()
				cfg := &latest.ModelConfig{Provider: provider, Model: model, ProviderOpts: map[string]any{"publisher": "anthropic"}}
				applyModelDefaults(cfg)
				if model == "claude-haiku-5-5" && provider != "openai" {
					assert.Equal(t, &latest.ThinkingBudget{Effort: "medium"}, cfg.ThinkingBudget)
				} else {
					assert.Nil(t, cfg.ThinkingBudget)
				}
			})
		}
	}
}
