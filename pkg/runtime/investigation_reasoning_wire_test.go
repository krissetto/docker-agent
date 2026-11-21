package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type manualReasoningRequest struct {
	Model     string `json:"model"`
	Effort    string `json:"reasoning_effort"`
	Reasoning struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
}

func TestManualReasoningWirePersistence(t *testing.T) {
	for _, tc := range []struct {
		model, api       string
		changeDuringTool bool
		revision         bool
	}{
		{"gpt-6-astra", "openai_responses", false, false},
		{"gpt-6-sol", "openai_responses", false, false},
		{"gpt-6.2-astra", "openai_responses", false, false},
		{"gpt-6.2-sol", "openai_responses", false, false},
		{"gpt-5.6", "openai_responses", false, false},
		{"gpt-5.2", "openai_chatcompletions", false, false},
		{"gpt-6-astra", "openai_chatcompletions", false, false},
		{"gpt-6-sol", "openai_chatcompletions", false, false},
		{"gpt-6-astra", "openai_responses", true, false},
		{"gpt-6-astra", "openai_responses", false, true},
	} {
		t.Run(fmt.Sprintf("%s/%s/midtool=%t/revision=%t", tc.model, tc.api, tc.changeDuringTool, tc.revision), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			var mu sync.Mutex
			var requests []manualReasoningRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req manualReasoningRequest
				if err := json.NewDecoder(r.Body).Decode(&req); !assert.NoError(t, err) {
					w.WriteHeader(400)
					return
				}
				mu.Lock()
				requests = append(requests, req)
				n := len(requests)
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.api == "openai_responses" {
					if n == 1 {
						fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"probe\",\"arguments\":\"\"}}\n\n")
						fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{}\"}\n\n")
						fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"probe\",\"arguments\":\"{}\"}]}}\n\n")
					} else {
						fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
						fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_done\",\"output\":[]}}\n\n")
					}
				} else if n == 1 {
					fmt.Fprint(w, "data: {\"id\":\"test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"probe\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\n")
					fmt.Fprint(w, "data: {\"id\":\"test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
				} else {
					fmt.Fprint(w, "data: {\"id\":\"test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer server.Close()
			cfg := latest.ModelConfig{Provider: "openai", Model: tc.model, BaseURL: server.URL, ThinkingBudget: &latest.ThinkingBudget{Effort: "low"}, ProviderOpts: map[string]any{"api_type": tc.api}}
			env := environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "offline-test"})
			factory := func(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
				return openai.NewClient(ctx, cfg, env, opts...)
			}
			registry := provider.NewRegistry(map[string]provider.Factory{"openai": factory, "openai_chatcompletions": factory, "openai_responses": factory})
			initial, err := registry.New(ctx, &cfg, env)
			require.NoError(t, err)
			var handle *sessionHandle
			toolCalls := 0
			probe := tools.Tool{Name: "probe", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}, Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
				toolCalls++
				if tc.changeDuringTool {
					_, err := handle.SetThinkingLevel(ctx, effort.Medium)
					if err != nil {
						return nil, err
					}
				}
				return tools.ResultSuccess("ok"), nil
			}}
			root := agent.New("root", "offline", agent.WithModel(initial), agent.WithToolSets(newStubToolSet(nil, []tools.Tool{probe}, nil)))
			worker := agent.New("worker", "offline", agent.WithModel(initial))
			store := session.NewInMemorySessionStore()
			modelConfig := ModelSwitcherConfig{ProviderRegistry: registry, EnvProvider: env, Models: map[string]latest.ModelConfig{"configured": cfg}}
			newRuntime := func() (*LocalRuntime, error) {
				tm := team.New(team.WithAgents(root, worker))
				opts := []Opt{WithSessionCompaction(false), WithModelStore(emptyCatalogStore{}), WithSessionStore(store), WithModelSwitcherConfig(&modelConfig)}
				if tc.revision {
					environment, err := NewTeamEnvironment("reasoning-r1", "reasoning-source", "reasoning-baseline", "offline", tm, modelConfig, func(context.Context) error { return nil })
					if err != nil {
						return nil, err
					}
					revisions, err := NewTeamEnvironmentStore(environment)
					if err != nil {
						return nil, err
					}
					opts = append(opts, WithTeamEnvironmentStore(revisions))
				}
				return NewLocalRuntime(ctx, tm, opts...)
			}
			rt, err := newRuntime()
			require.NoError(t, err)
			defer rt.Close()
			sess := session.New(session.WithAgentName("root"), session.WithTitle("offline"), session.WithToolsApproved(true))
			h, err := rt.CreateSession(ctx, sess, SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			handle = h.(*sessionHandle)
			_, err = handle.SetThinkingLevel(ctx, effort.High)
			require.NoError(t, err)
			wireClient, err := NewClient(server.URL, WithHTTPClient(server.Client()))
			require.NoError(t, err)
			var firstTurnLevels []string
			submit := func(h SessionHandle) {
				t.Helper()
				observation, err := h.Observe(ctx, ObserveOptions{Buffer: 256})
				require.NoError(t, err)
				defer observation.Cancel()
				s, err := h.Submit(ctx, TurnInput{Content: "go"})
				require.NoError(t, err)
				for envelope := range observation.Events {
					if envelope.TurnID != s.TurnID {
						continue
					}
					if info, ok := envelope.Event.(*TeamInfoEvent); ok {
						encoded, err := json.Marshal(info)
						require.NoError(t, err)
						decoded, err := wireClient.decodeSessionEvent(encoded)
						require.NoError(t, err)
						require.Equal(t, info.AvailableAgents, decoded.(*TeamInfoEvent).AvailableAgents, "remote SSE decoding preserves reasoning presentation")
						for _, a := range info.AvailableAgents {
							if a.Name == "root" {
								firstTurnLevels = append(firstTurnLevels, a.ThinkingLevel)
								require.Equal(t, a.ThinkingLevel, a.Thinking, "agent card and structured reasoning control agree")
							}
						}
					}
					if _, ok := envelope.Event.(*StreamStoppedEvent); ok {
						break
					}
				}
				require.NoError(t, h.AwaitTurn(ctx, s.TurnID))
			}
			submit(handle)
			require.Equal(t, 1, toolCalls)
			submit(handle)
			expected := effort.High
			if tc.changeDuringTool {
				expected = effort.Medium
			}
			require.Equal(t, expected, handle.CurrentThinkingLevel(ctx))
			reconnected, err := handle.Observe(ctx, ObserveOptions{})
			require.NoError(t, err)
			primary := reconnected.Primary()
			reconnected.Cancel()
			roster := primary.Presentation[1].(*TeamInfoEvent)
			require.Equal(t, string(expected), roster.AvailableAgents[0].ThinkingLevel, "reconnect describes the effective session binding")
			require.Equal(t, string(expected), roster.AvailableAgents[0].Thinking, "reconnect restores the agent-card effort label")
			t.Logf("reconnect projection: ThinkingLevel=%q legacy Thinking=%q", roster.AvailableAgents[0].ThinkingLevel, roster.AvailableAgents[0].Thinking)
			require.NoError(t, handle.SwitchActiveAgent(ctx, "worker"))
			require.Equal(t, effort.Low, handle.CurrentThinkingLevel(ctx))
			require.NoError(t, handle.SwitchActiveAgent(ctx, "root"))
			require.Equal(t, expected, handle.CurrentThinkingLevel(ctx))
			submit(handle)
			_, err = handle.SetThinkingLevel(ctx, effort.None)
			require.NoError(t, err)
			submit(handle)
			noneInfo, err := handle.SessionAgentInfo(ctx)
			require.NoError(t, err)
			require.Equal(t, effort.None, handle.CurrentThinkingLevel(ctx))
			require.Equal(t, "none", handle.driver.session().AgentThinkingLevel("root"))
			require.Equal(t, "off", noneInfo.Team.AvailableAgents[0].ThinkingControl().Mode)
			require.Equal(t, "off", noneInfo.Team.AvailableAgents[0].Thinking)
			require.NoError(t, handle.SetModel(ctx, "configured"))
			require.Empty(t, handle.driver.session().AgentThinkingLevel("root"))
			submit(handle)
			_, err = handle.SetThinkingLevel(ctx, effort.High)
			require.NoError(t, err)
			sibling, err := rt.CreateSession(ctx, session.New(session.WithAgentName("root"), session.WithTitle("sibling")), SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			require.Equal(t, effort.Low, sibling.(*sessionHandle).CurrentThinkingLevel(ctx))
			submit(sibling)
			loaded, err := store.GetSession(ctx, sess.ID)
			require.NoError(t, err)
			require.NoError(t, rt.Close())
			restoredRuntime, err := newRuntime()
			require.NoError(t, err)
			defer restoredRuntime.Close()
			restored, err := restoredRuntime.CreateSession(ctx, loaded, SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			require.Equal(t, effort.High, restored.(*sessionHandle).CurrentThinkingLevel(ctx))
			submit(restored)
			mu.Lock()
			captured := append([]manualReasoningRequest(nil), requests...)
			mu.Unlock()
			require.Len(t, captured, 8)
			got := make([]string, len(captured))
			for i, req := range captured {
				got[i] = req.Effort
				if tc.api == "openai_responses" {
					got[i] = req.Reasoning.Effort
				}
			}
			want := []string{"high", string(expected), string(expected), string(expected), "none", "low"}
			if tc.model == "gpt-5.2" || tc.model == "gpt-6-astra" || tc.model == "gpt-6.2-astra" || tc.model == "gpt-6.2-sol" {
				want[4] = "low"
			}
			// Forced Chat Completions deliberately drops effort when this model has function tools.
			if tc.api == "openai_chatcompletions" && tc.model != "gpt-5.2" {
				want = []string{"", "", "", "", "none", ""}
				if tc.model == "gpt-6-astra" {
					want[4] = ""
				}
			}
			if tc.api == "openai_chatcompletions" && tc.model != "gpt-5.2" {
				want = append(want, "", "")
			} else {
				want = append(want, "low", "high")
			}
			assert.Equal(t, want, got)
			require.Len(t, firstTurnLevels, 7)
			assert.Equal(t, []string{"high", string(expected), string(expected), "off", "low", "low", "high"}, firstTurnLevels,
				"every turn presents its own binding, including explicit none, model reset, sibling and restore")
			t.Logf("emitted per-run root TeamInfo levels: %q", firstTurnLevels)
			t.Logf("outbound efforts [first tool-continuation later agent-roundtrip explicit-none model-reset sibling-default resumed]: %q", got)
		})
	}
}
