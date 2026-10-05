package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/harness"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func outboundTestPolicy(t *testing.T, deny bool, originID *string, purpose string, calls *int) MessageTransform {
	t.Helper()
	return func(_ context.Context, in *hooks.Input, msgs []chat.Message) ([]chat.Message, error) {
		if in.CallPurpose != purpose {
			return msgs, nil
		}
		*calls++
		require.Equal(t, *originID, in.SessionID)
		require.Equal(t, "worker", in.AgentName)
		require.NotEmpty(t, in.ModelID)
		if deny {
			return nil, errors.New("outbound denied")
		}
		for i := range msgs {
			msgs[i].Content = strings.ReplaceAll(msgs[i].Content, "PRIVATE", "REDACTED")
			for j := range msgs[i].MultiContent {
				msgs[i].MultiContent[j].Text = strings.ReplaceAll(msgs[i].MultiContent[j].Text, "PRIVATE", "REDACTED")
			}
		}
		return msgs, nil
	}
}

func TestOutboundPolicyCompactionOrigin(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		for _, deny := range []bool{false, true} {
			t.Run(map[bool]string{false: "public", true: "automatic"}[automatic]+"/"+map[bool]string{false: "redact", true: "deny"}[deny], func(t *testing.T) {
				p := &recordingMsgProvider{mockProvider: mockProvider{id: "test/fake", stream: newStreamBuilder().AddContent("summary").AddStopWithUsage(1, 1).Build()}}
				a := agent.New("worker", "", agent.WithModel(p))
				var id string
				calls := 0
				r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithModelStore(mockModelStoreWithLimit{limit: 100000}), WithMessagePolicy("scope", outboundTestPolicy(t, deny, &id, "compaction", &calls)))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, r.Close()) })
				seed := session.New(session.WithMessages([]session.Item{session.NewMessageItem(session.UserMessage("PRIVATE marker")), session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "previous answer"}})}))
				if automatic {
					seed.InputTokens = 100000
				}
				h, err := r.CreateSession(t.Context(), seed, SessionBinding{AgentName: "worker"})
				require.NoError(t, err)
				id = h.ID()
				if automatic {
					accepted, err := h.Submit(t.Context(), TurnInput{Retry: true})
					require.NoError(t, err)
					require.NoError(t, h.AwaitTurn(t.Context(), accepted.TurnID))
				} else {
					done := make(chan *SessionCompactionEvent, 1)
					require.NoError(t, h.Compact(t.Context(), "", EventSinkFunc(func(e Event) {
						if completed, ok := e.(*SessionCompactionEvent); ok && completed.Status == "completed" {
							done <- completed
						}
					})))
					select {
					case completed := <-done:
						if deny {
							require.Equal(t, CompactionOutcomeFailed, completed.Outcome)
						} else {
							require.Equal(t, CompactionOutcomeApplied, completed.Outcome)
						}
					case <-time.After(10 * time.Second):
						t.Fatal("compaction did not settle")
					}
				}
				require.Equal(t, 1, calls, "summary must invoke originating policy")
				if deny {
					if automatic {
						require.Len(t, p.got, 1, "only the ordinary completion may be delivered")
					} else {
						require.Empty(t, p.got)
					}
				}
				if !deny {
					require.NotEmpty(t, p.got)
					for _, msg := range p.got[0] {
						require.NotContains(t, msg.Content, "PRIVATE")
					}
				}
			})
		}
	}
}

func TestOutboundPolicyScopedSampling(t *testing.T) {
	for _, withTools := range []bool{false, true} {
		for _, deny := range []bool{false, true} {
			t.Run(map[bool]string{false: "sampling", true: "tools"}[withTools]+"/"+map[bool]string{false: "redact", true: "deny"}[deny], func(t *testing.T) {
				var id string
				calls := 0
				invoked := false
				samplingSet := &outboundSamplingToolSet{}
				tool := tools.Tool{Name: "sample", Parameters: map[string]any{}, Handler: func(ctx context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
					invoked = true
					require.True(t, tools.HasHandlerScope(ctx))
					var err error
					if withTools {
						require.NotNil(t, samplingSet.withTools)
						_, err = samplingSet.withTools(ctx, &mcp.CreateMessageWithToolsParams{Messages: []*mcp.SamplingMessageV2{{Role: "user", Content: []mcp.Content{&mcp.TextContent{Text: "PRIVATE marker"}}}}, MaxTokens: 10}) //nolint:staticcheck // Sampling remains supported during the MCP deprecation window.
					} else {
						require.NotNil(t, samplingSet.sampling)
						_, err = samplingSet.sampling(ctx, &mcp.CreateMessageParams{Messages: []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "PRIVATE marker"}}}, MaxTokens: 10}) //nolint:staticcheck // Sampling remains supported during the MCP deprecation window.
					}
					if deny {
						require.ErrorContains(t, err, "outbound denied")
					} else {
						require.NoError(t, err)
					}
					return tools.ResultSuccess("done"), nil
				}}
				ordinary := &queueRecordingProvider{recordingMsgProvider: &recordingMsgProvider{mockProvider: mockProvider{id: "test/ordinary"}}, queue: []chat.MessageStream{newStreamBuilder().AddToolCallName("call", "sample").AddToolCallArguments("call", "{}").AddToolCallStopWithUsage(1, 1).Build(), newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build()}}
				samplingSet.ToolSet = newStubToolSet(nil, []tools.Tool{tool}, nil)
				a := agent.New("worker", "", agent.WithModel(ordinary), agent.WithToolSets(samplingSet))
				r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithMessagePolicy("scope", outboundTestPolicy(t, deny, &id, "sampling", &calls)))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, r.Close()) })
				h, err := r.CreateSession(t.Context(), session.New(session.WithToolsApproved(true)), SessionBinding{AgentName: "worker"})
				require.NoError(t, err)
				id = h.ID()
				accepted, err := h.Submit(t.Context(), TurnInput{Content: "sample please"})
				require.NoError(t, err)
				require.NoError(t, h.AwaitTurn(t.Context(), accepted.TurnID))
				require.True(t, invoked)
				require.Equal(t, 1, calls)
				if deny {
					require.Len(t, ordinary.got, 2, "sampling denial must not deliver a completion")
				}
				if !deny {
					require.Len(t, ordinary.got, 3)
					require.Equal(t, "REDACTED marker", ordinary.got[1][0].Content)
				}
			})
		}
	}
}

func TestOutboundPolicyModelHook(t *testing.T) {
	for _, deny := range []bool{false, true} {
		t.Run(map[bool]string{false: "redact", true: "deny"}[deny], func(t *testing.T) {
			p := &recordingMsgProvider{mockProvider: mockProvider{id: "test/hook", stream: newStreamBuilder().AddContent("context").AddStopWithUsage(1, 1).Build()}}
			registry := provider.NewRegistry(map[string]provider.Factory{"test": func(context.Context, *latest.ModelConfig, environment.Provider, ...options.Opt) (provider.Provider, error) {
				return p, nil
			}})
			a := agent.New("worker", "", agent.WithModel(&mockProvider{id: "test/main", stream: &mockStream{}}), agent.WithHooks(&hooks.Config{BeforeLLMCall: []hooks.Hook{{Type: hooks.HookTypeModel, Model: "test/hook", Prompt: "PRIVATE marker"}}}))
			var id string
			calls := 0
			r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithModelStore(mockModelStore{}), WithProviderRegistry(registry), WithMessagePolicy("scope", outboundTestPolicy(t, deny, &id, "model_hook", &calls)))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, r.Close()) })
			h, err := r.CreateSession(t.Context(), session.New(), SessionBinding{AgentName: "worker"})
			require.NoError(t, err)
			id = h.ID()
			accepted, err := h.Submit(t.Context(), TurnInput{Content: "hello"})
			require.NoError(t, err)
			require.NoError(t, h.AwaitTurn(t.Context(), accepted.TurnID))
			require.Equal(t, 1, calls, "model hook must not recursively dispatch before_llm_call")
			if deny {
				require.Empty(t, p.got)
			} else {
				require.Len(t, p.got, 1)
				require.Equal(t, "REDACTED marker", p.got[0][1].Content)
			}
		})
	}
}

type outboundFakeHarness struct{ prompts []string }

func (p *outboundFakeHarness) Name() string { return "fake" }
func (p *outboundFakeHarness) Run(_ context.Context, prompt string, handle func(harness.Event)) error {
	p.prompts = append(p.prompts, prompt)
	handle(harness.Event{Type: harness.EventResult, Result: "done"})
	return nil
}

func (p *outboundFakeHarness) Resume(ctx context.Context, _, prompt string, handle func(harness.Event)) error {
	return p.Run(ctx, prompt, handle)
}

func TestOutboundPolicyHarness(t *testing.T) {
	for _, deny := range []bool{false, true} {
		t.Run(map[bool]string{false: "redact", true: "deny"}[deny], func(t *testing.T) {
			p := &outboundFakeHarness{}
			a := agent.New("worker", "", agent.WithHarness(&latest.HarnessConfig{Type: "fake"}))
			var id string
			calls := 0
			r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithModelStore(mockModelStore{}), WithHarnessFactory(func(*latest.HarnessConfig) (harness.Provider, error) { return p, nil }), WithMessagePolicy("scope", outboundTestPolicy(t, deny, &id, "ordinary", &calls)))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, r.Close()) })
			h, err := r.CreateSession(t.Context(), session.New(), SessionBinding{AgentName: "worker"})
			require.NoError(t, err)
			id = h.ID()
			accepted, err := h.Submit(t.Context(), TurnInput{Content: "PRIVATE marker"})
			require.NoError(t, err)
			require.NoError(t, h.AwaitTurn(t.Context(), accepted.TurnID))
			require.Equal(t, 1, calls)
			if deny {
				require.Empty(t, p.prompts)
			} else {
				require.Equal(t, []string{"REDACTED marker"}, p.prompts)
			}
		})
	}
}

func TestOutboundPolicyFallbackAttemptIdentity(t *testing.T) {
	for _, deny := range []bool{false, true} {
		t.Run(map[bool]string{false: "redact", true: "deny_fallback"}[deny], func(t *testing.T) {
			primary := &outboundFailingRecorder{recordingMsgProvider: recordingMsgProvider{mockProvider: mockProvider{id: "test/primary"}}}
			fallback := &recordingMsgProvider{mockProvider: mockProvider{id: "test/fallback", stream: newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build()}}
			a := agent.New("worker", "", agent.WithModel(primary), agent.WithFallbackModel(fallback))
			var id string
			var attempts []string
			r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithMessagePolicy("scope", func(_ context.Context, in *hooks.Input, msgs []chat.Message) ([]chat.Message, error) {
				require.Equal(t, id, in.SessionID)
				require.Equal(t, "worker", in.AgentName)
				require.Equal(t, "ordinary", in.CallPurpose)
				attempts = append(attempts, in.ModelID)
				if deny && in.ModelID == "test/fallback" {
					return nil, errors.New("fallback denied")
				}
				for i := range msgs {
					msgs[i].Content = strings.ReplaceAll(msgs[i].Content, "PRIVATE", "REDACTED")
				}
				return msgs, nil
			}))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, r.Close()) })
			h, err := r.CreateSession(t.Context(), session.New(), SessionBinding{AgentName: "worker"})
			require.NoError(t, err)
			id = h.ID()
			accepted, err := h.Submit(t.Context(), TurnInput{Content: "PRIVATE marker"})
			require.NoError(t, err)
			require.NoError(t, h.AwaitTurn(t.Context(), accepted.TurnID))
			require.Equal(t, []string{"test/primary", "test/fallback"}, attempts)
			require.Len(t, primary.got, 1)
			for _, msg := range primary.got[0] {
				require.NotContains(t, msg.Content, "PRIVATE")
			}
			if deny {
				require.Empty(t, fallback.got)
			} else {
				require.Len(t, fallback.got, 1)
				for _, msg := range fallback.got[0] {
					require.NotContains(t, msg.Content, "PRIVATE")
				}
			}
		})
	}
}

type outboundFailingRecorder struct{ recordingMsgProvider }

func (p *outboundFailingRecorder) CreateChatCompletionStream(ctx context.Context, msgs []chat.Message, available []tools.Tool) (chat.MessageStream, error) {
	_, _ = p.recordingMsgProvider.CreateChatCompletionStream(ctx, msgs, available)
	return nil, &modelerrors.ContextOverflowError{}
}

func TestOutboundPolicyMissingOriginAndFrozenChain(t *testing.T) {
	p := &recordingMsgProvider{mockProvider: mockProvider{id: "test/model", stream: &mockStream{}}}
	a := agent.New("worker", "", agent.WithModel(p))
	calls := 0
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithModelStore(mockModelStore{}), WithMessagePolicy("original", func(_ context.Context, _ *hooks.Input, msgs []chat.Message) ([]chat.Message, error) {
		calls++
		return msgs, nil
	}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	_, err = r.samplingHandler(t.Context(), &mcp.CreateMessageParams{Messages: []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "PRIVATE marker"}}}}) //nolint:staticcheck // Sampling remains supported during the MCP deprecation window.
	require.ErrorContains(t, err, "originating session and agent")
	require.Empty(t, p.got)
	require.Zero(t, calls)
	_, err = r.samplingWithToolsHandler(t.Context(), &mcp.CreateMessageWithToolsParams{Messages: []*mcp.SamplingMessageV2{{Role: "user", Content: []mcp.Content{&mcp.TextContent{Text: "PRIVATE marker"}}}}}) //nolint:staticcheck // Sampling remains supported during the MCP deprecation window.
	require.ErrorContains(t, err, "originating session and agent")
	require.Empty(t, p.got)
	registry := provider.NewRegistry(map[string]provider.Factory{"test": func(context.Context, *latest.ModelConfig, environment.Provider, ...options.Opt) (provider.Provider, error) {
		return p, nil
	}})
	_, err = (providerModelClient{registry: registry, runtime: r}).Ask(t.Context(), "test/model", "", "PRIVATE", nil)
	require.ErrorContains(t, err, "originating session and agent")
	require.Empty(t, p.got)
	require.ErrorContains(t, r.runCompactionAgent(t.Context(), a, session.New()), "originating session and agent")
	WithMessagePolicy("late", func(context.Context, *hooks.Input, []chat.Message) ([]chat.Message, error) {
		return nil, errors.New("late must not run")
	})(r)
	_, err = r.applyMessagePolicies(t.Context(), session.New(), a, "test/model", nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
}

// This fake MCP toolset uses the installed stable dispatchers during real tool execution.
type outboundSamplingToolSet struct {
	tools.ToolSet

	sampling  tools.SamplingHandler
	withTools tools.SamplingWithToolsHandler
}

func (s *outboundSamplingToolSet) SetSamplingHandler(handler tools.SamplingHandler) {
	s.sampling = handler
}

func (s *outboundSamplingToolSet) SetSamplingWithToolsHandler(handler tools.SamplingWithToolsHandler) {
	s.withTools = handler
}
