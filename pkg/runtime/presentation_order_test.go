package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/harness"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestAssistantPresentationStreamAndPersistence(t *testing.T) {
	t.Parallel()
	for _, finish := range []chat.FinishReason{"", chat.FinishReasonStop, chat.FinishReasonRefusal} {
		t.Run(string(finish), func(t *testing.T) {
			builder := newStreamBuilder().AddContent("before").AddReasoning("think").AddToolCallName("call", "read").AddContent("between").AddToolCallArguments("call", "{}").AddReasoning("again").AddContent("after")
			if finish != "" {
				builder.responses = append(builder.responses, chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{FinishReason: finish}}})
			}
			sess := session.New()
			store := session.NewInMemorySessionStore()
			require.NoError(t, store.AddSession(t.Context(), sess))
			obs := newPersistenceObserver(store)
			a := agent.New("root", "test")
			var emitted []chat.AssistantPart
			sink := EventSinkFunc(func(ev Event) {
				emitted = captureAssistantPresentation(emitted, ev)
				obs.OnEvent(t.Context(), sess, ev)
			})
			res, err := handleStream(t.Context(), nil, builder.Build(), a, []tools.Tool{{Name: "read"}}, sess, nil, defaultTelemetry{}, sink, time.Second)
			require.NoError(t, err)
			expected := []chat.AssistantPart{
				{Type: chat.AssistantPartContent, Text: "before"}, {Type: chat.AssistantPartReasoning, Text: "think"},
				{Type: chat.AssistantPartToolCall, ToolCallID: "call"}, {Type: chat.AssistantPartContent, Text: "between"},
				{Type: chat.AssistantPartReasoning, Text: "again"}, {Type: chat.AssistantPartContent, Text: "after"},
			}
			got, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			require.Len(t, got.Messages, 1)
			placeholder := got.Messages[0].Message.Message
			assert.Equal(t, emitted, placeholder.Presentation)
			assert.Empty(t, placeholder.ToolCalls)
			assert.Empty(t, placeholder.ToolDefinitions)
			require.NotNil(t, placeholder.Presentation[2].Tool)
			assert.Equal(t, "{}", placeholder.Presentation[2].Tool.Call.Function.Arguments)
			assert.Equal(t, "read", placeholder.Presentation[2].Tool.Definition.Name)
			if finish == chat.FinishReasonRefusal {
				expected = append(expected[:2], expected[3:]...)
				assert.Empty(t, res.Calls)
			}
			assert.Equal(t, expected, res.Presentation)
			rt := &LocalRuntime{now: time.Now}
			rt.recordAssistantMessage(t.Context(), sess, a, res, nil, "test", nil, sink)
			assert.Equal(t, res.Presentation, sess.OwnMessages()[0].Message.Presentation)
			got, err = store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			require.Len(t, got.Messages, 1)
			assert.Equal(t, res.Presentation, got.Messages[0].Message.Message.Presentation)
			assert.Equal(t, res.Calls, got.Messages[0].Message.Message.ToolCalls)
		})
	}
}

func TestAssistantPresentationMixedDelta(t *testing.T) {
	t.Parallel()
	for _, finish := range []chat.FinishReason{"", chat.FinishReasonStop} {
		t.Run(string(finish), func(t *testing.T) {
			stream := &mockStream{responses: []chat.MessageStreamResponse{{Choices: []chat.MessageStreamChoice{{FinishReason: finish, Delta: chat.MessageDelta{
				ToolCalls:        []tools.ToolCall{{ID: "call", Type: "function", Function: tools.FunctionCall{Name: "read", Arguments: "{}"}}},
				ReasoningContent: "think", Content: "answer", ThinkingSignature: "signature",
			}}}}}}
			res, err := handleStream(t.Context(), nil, stream, agent.New("root", "test"), nil, session.New(), nil, defaultTelemetry{}, EventSinkFunc(func(Event) {}), time.Second)
			require.NoError(t, err)
			assert.Equal(t, []chat.AssistantPart{{Type: chat.AssistantPartToolCall, ToolCallID: "call"}, {Type: chat.AssistantPartReasoning, Text: "think"}, {Type: chat.AssistantPartContent, Text: "answer"}}, res.Presentation)
			assert.Equal(t, "answer", res.Content)
			assert.Equal(t, "think", res.ReasoningContent)
			assert.Equal(t, "signature", res.ThinkingSignature)
		})
	}
}

func TestAssistantPresentationSteeringDropsTools(t *testing.T) {
	t.Parallel()
	signal := make(chan struct{})
	ctx := context.WithValue(t.Context(), steeringBoundaryContextKey{}, signal)
	stream := newStreamBuilder().AddContent("before").AddToolCallName("call", "read").AddContent("after").Build()
	res, err := handleStream(ctx, nil, stream, agent.New("root", "test"), nil, session.New(), nil, defaultTelemetry{}, EventSinkFunc(func(ev Event) {
		if e, ok := ev.(*AgentChoiceEvent); ok && e.Content == "after" {
			close(signal)
		}
	}), time.Second)
	require.NoError(t, err)
	assert.True(t, res.Steered)
	assert.Empty(t, res.Calls)
	assert.Equal(t, []chat.AssistantPart{{Type: chat.AssistantPartContent, Text: "before"}, {Type: chat.AssistantPartContent, Text: "after"}}, res.Presentation)
}

func TestAssistantPresentationEmptyRefusalClearsPlaceholder(t *testing.T) {
	t.Parallel()
	sess := session.New()
	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), sess))
	obs := newPersistenceObserver(store)
	sink := EventSinkFunc(func(ev Event) { obs.OnEvent(t.Context(), sess, ev) })
	a := agent.New("root", "test")
	res, err := handleStream(t.Context(), nil, newStreamBuilder().AddToolCallName("call", "read").AddRefusal().Build(), a, nil, sess, nil, defaultTelemetry{}, sink, time.Second)
	require.NoError(t, err)
	(&LocalRuntime{now: time.Now}).recordAssistantMessage(t.Context(), sess, a, res, nil, "test", nil, sink)
	assert.Empty(t, sess.OwnMessages())
	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.Empty(t, got.Messages[0].Message.Message.ToolCalls)
	assert.Empty(t, got.Messages[0].Message.Message.Presentation)
	assert.Equal(t, chat.FinishReasonRefusal, got.Messages[0].Message.Message.FinishReason)
}

type presentationHarness struct{ events []harness.Event }

func (p presentationHarness) Name() string { return "test" }
func (p presentationHarness) Run(_ context.Context, _ string, emit func(harness.Event)) error {
	for _, ev := range p.events {
		emit(ev)
	}
	return nil
}
func (p presentationHarness) Resume(ctx context.Context, _ string, prompt string, emit func(harness.Event)) error {
	return p.Run(ctx, prompt, emit)
}

func TestAssistantPresentationHarness(t *testing.T) {
	t.Parallel()
	provider := presentationHarness{events: []harness.Event{
		{Type: harness.EventText, Text: "before"}, {Type: harness.EventReasoning, Reasoning: "think"},
		{Type: harness.EventToolCallStart, ToolID: "a", ToolName: "read"}, {Type: harness.EventText, Text: "after"},
		{Type: harness.EventToolCallDelta, ToolID: "a", ToolArgs: "{}"}, {Type: harness.EventToolCall, ToolID: "a", ToolName: "read", ToolArgs: "{}"},
		{Type: harness.EventToolResult, ToolID: "a", ToolOutput: "result", ToolError: true},
	}}
	a := agent.New("root", "test", agent.WithHarness(&latest.HarnessConfig{Type: "test"}))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithHarnessFactory(func(*latest.HarnessConfig) (harness.Provider, error) { return provider, nil }))
	require.NoError(t, err)
	sess := session.New(session.WithUserMessage("go"))
	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), sess))
	obs := newPersistenceObserver(store)
	var placeholder chat.Message
	end := rt.runHarnessAgent(t.Context(), sess, a, EventSinkFunc(func(ev Event) {
		obs.OnEvent(t.Context(), sess, ev)
		if _, ok := ev.(*ToolCallResponseEvent); ok {
			got, e := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, e)
			placeholder = got.Messages[len(got.Messages)-1].Message.Message
		}
	}))
	assert.Equal(t, turnEndReasonNormal, end)
	messages := sess.OwnMessages()
	message := messages[len(messages)-1].Message
	require.Len(t, message.Presentation, 4)
	assert.Equal(t, chat.AssistantPartReasoning, message.Presentation[1].Type)
	tool := message.Presentation[2].Tool
	require.NotNil(t, tool)
	assert.Equal(t, "harness-a", tool.Call.ID)
	assert.Equal(t, "{}", tool.Call.Function.Arguments)
	assert.Equal(t, "read", tool.Definition.Name)
	require.NotNil(t, tool.Result)
	assert.Equal(t, "result", *tool.Result)
	assert.True(t, tool.IsError)
	assert.Empty(t, message.ToolCalls)
	assert.Empty(t, message.ReasoningContent)
	assert.Equal(t, message.Presentation, placeholder.Presentation)
	assert.Empty(t, placeholder.ToolCalls)
	assert.Empty(t, placeholder.ToolDefinitions)
}

func TestAssistantPresentationSnapshotIsolation(t *testing.T) {
	t.Parallel()
	parts := captureAssistantPresentation(nil, PartialToolCall(tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "read", Arguments: "{"}}, tools.Tool{Name: "read"}, "root"))
	snapshot := cloneAssistantPresentation(parts)
	parts = captureAssistantPresentation(parts, PartialToolCall(tools.ToolCall{ID: "call", Function: tools.FunctionCall{Arguments: "}"}}, tools.Tool{}, "root"))
	parts = captureAssistantPresentation(parts, ToolCallResponse("call", tools.Tool{}, tools.ResultSuccess("done"), "done", "root"))
	assert.Equal(t, "{", snapshot[0].Tool.Call.Function.Arguments)
	assert.Nil(t, snapshot[0].Tool.Result)
	assert.Equal(t, "{}", parts[0].Tool.Call.Function.Arguments)
}

func TestAssistantPresentationXMLFallback(t *testing.T) {
	t.Parallel()
	stream := newStreamBuilder().AddReasoning("think").AddContent(`before<tool_call>{"name":"read","arguments":{}}</tool_call>`).Build()
	var emitted []chat.AssistantPart
	res, err := handleStream(t.Context(), nil, stream, agent.New("root", "test"), nil, session.New(), nil, defaultTelemetry{}, EventSinkFunc(func(ev Event) { emitted = captureAssistantPresentation(emitted, ev) }), time.Second)
	require.NoError(t, err)
	require.Len(t, res.Calls, 1)
	assert.Equal(t, []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "think"}, {Type: chat.AssistantPartContent, Text: "before"}, {Type: chat.AssistantPartToolCall, ToolCallID: res.Calls[0].ID}}, res.Presentation)
	for i := range emitted {
		emitted[i].Tool = nil
	}
	assert.Equal(t, emitted, res.Presentation)
}

func TestAssistantPresentationLegacyResultStaysNil(t *testing.T) {
	t.Parallel()
	sess := session.New()
	a := agent.New("root", "test")
	(&LocalRuntime{now: time.Now}).recordAssistantMessage(t.Context(), sess, a, streamResult{Content: "legacy"}, nil, "test", nil, EventSinkFunc(func(Event) {}))
	require.Len(t, sess.OwnMessages(), 1)
	assert.Nil(t, sess.OwnMessages()[0].Message.Presentation)
}

func TestAssistantPresentationModelExecutionDoesNotCreatePlaceholder(t *testing.T) {
	t.Parallel()
	sess := session.New()
	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), sess))
	obs := newPersistenceObserver(store)
	call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "read"}}
	obs.OnEvent(t.Context(), sess, ToolCall(call, tools.Tool{Name: "read"}, "root"))
	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Empty(t, got.Messages)
}

func TestAssistantPresentationCommittedToolIDs(t *testing.T) {
	t.Parallel()
	event := &MessageAddedEvent{Message: session.NewAgentMessage("root", &chat.Message{
		ToolCalls:    []tools.ToolCall{{ID: "model"}, {ID: "model"}},
		Presentation: []chat.AssistantPart{{Type: chat.AssistantPartToolCall, ToolCallID: "model"}, {Type: chat.AssistantPartToolCall, ToolCallID: "harness", Tool: &chat.AssistantTool{}}, {Type: chat.AssistantPartToolCall, ToolCallID: "harness"}, {Type: chat.AssistantPartToolCall}},
	})}
	assert.Equal(t, []string{"model", "harness"}, event.CommittedToolCallIDs())
	event.Message = nil
	event.ToolCallIDs = []string{"serialized"}
	assert.Equal(t, []string{"serialized"}, event.CommittedToolCallIDs())
}

func TestAssistantPresentationLateToolCommitPreservesPlaceholder(t *testing.T) {
	t.Parallel()
	sess := session.New()
	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), sess))
	obs := newPersistenceObserver(store)
	obs.OnEvent(t.Context(), sess, AgentChoiceReasoning("root", sess.ID, "think"))
	tool := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleTool, ToolCallID: "previous", Content: "result"})
	obs.OnEvent(t.Context(), sess, MessageAdded(sess.ID, tool, "root"))
	obs.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, "answer"))
	final := chat.Message{Role: chat.MessageRoleAssistant, Content: "answer", ReasoningContent: "think", Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "think"}, {Type: chat.AssistantPartContent, Text: "answer"}}}
	obs.OnEvent(t.Context(), sess, MessageAdded(sess.ID, session.NewAgentMessage("root", &final), "root"))
	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 2)
	assert.Equal(t, final, got.Messages[0].Message.Message)
	assert.Equal(t, chat.MessageRoleTool, got.Messages[1].Message.Message.Role)
	assert.Equal(t, "result", got.Messages[1].Message.Message.Content)
}

func TestAssistantPresentationWireCommittedToolIDs(t *testing.T) {
	t.Parallel()
	message := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Presentation: []chat.AssistantPart{{Type: chat.AssistantPartToolCall, ToolCallID: "harness-a", Tool: &chat.AssistantTool{Call: tools.ToolCall{ID: "harness-a"}}}}})
	event := MessageAddedAt("sess", message, "root", 4)
	data, err := json.Marshal(event)
	require.NoError(t, err)
	var wire MessageAddedEvent
	require.NoError(t, json.Unmarshal(data, &wire))
	assert.Nil(t, wire.Message)
	assert.Equal(t, []string{"harness-a"}, wire.CommittedToolCallIDs())
	assert.Equal(t, chat.MessageRoleAssistant, wire.CommittedRole())
	assert.Equal(t, 4, wire.SessionPosition)
}

func TestAssistantPresentationReasoningOnlyBoundary(t *testing.T) {
	t.Parallel()
	for _, reason := range []chat.FinishReason{chat.FinishReasonStop, chat.FinishReasonRefusal} {
		t.Run(string(reason), func(t *testing.T) {
			a := agent.New("root", "test")
			sess := session.New(session.WithUserMessage("go"))
			before := sess.GetMessages(a)
			store := session.NewInMemorySessionStore()
			require.NoError(t, store.AddSession(t.Context(), sess))
			obs := newPersistenceObserver(store)
			var boundary *MessageAddedEvent
			sink := EventSinkFunc(func(ev Event) {
				if e, ok := ev.(*MessageAddedEvent); ok {
					boundary = e
				}
				obs.OnEvent(t.Context(), sess, ev)
			})
			obs.OnEvent(t.Context(), sess, AgentChoiceReasoning("root", sess.ID, "think"))
			rt := &LocalRuntime{now: time.Now}
			usage := rt.recordAssistantMessage(t.Context(), sess, a, streamResult{ReasoningContent: "think", Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "think"}}, FinishReason: reason, Usage: &chat.Usage{OutputTokens: 1}}, nil, "test", nil, sink)
			require.NotNil(t, usage)
			require.NotNil(t, boundary)
			assert.False(t, boundary.boundaryOnly)
			assert.Equal(t, 1, boundary.SessionPosition)
			assert.True(t, boundary.Message.DisplayOnly)
			assert.Equal(t, before, sess.GetMessages(a))
			got, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			require.Len(t, got.Messages, 2)
			assert.True(t, got.Messages[1].Message.DisplayOnly)
			call := tools.ToolCall{ID: "next", Type: "function", Function: tools.FunctionCall{Name: "read", Arguments: "{}"}}
			rt.recordAssistantMessage(t.Context(), sess, a, streamResult{Calls: []tools.ToolCall{call}, Presentation: []chat.AssistantPart{{Type: chat.AssistantPartToolCall, ToolCallID: "next"}}}, nil, "test", nil, sink)
			projected := sess.GetMessages(a)
			require.Len(t, projected, len(before)+2)
			assert.Equal(t, before, projected[:len(before)])
			assert.Equal(t, []tools.ToolCall{call}, projected[len(before)].ToolCalls)
			assert.Equal(t, chat.MessageRoleTool, projected[len(before)+1].Role)
		})
	}
}

func TestAssistantPresentationHarnessDisplayOnly(t *testing.T) {
	t.Parallel()
	a := agent.New("root", "test")
	sess := session.New(session.WithUserMessage("go"))
	before := sess.GetMessages(a)
	var boundary *MessageAddedEvent
	parts := []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "think"}, {Type: chat.AssistantPartToolCall, ToolCallID: "harness-a", Tool: &chat.AssistantTool{Call: tools.ToolCall{ID: "harness-a"}}}}
	(&LocalRuntime{now: time.Now}).recordHarnessAssistantMessage(sess, a, "", "test", nil, 0, parts, EventSinkFunc(func(ev Event) {
		if e, ok := ev.(*MessageAddedEvent); ok {
			boundary = e
		}
	}))
	require.NotNil(t, boundary)
	assert.True(t, boundary.Message.DisplayOnly)
	assert.Equal(t, 1, boundary.SessionPosition)
	assert.False(t, boundary.boundaryOnly)
	assert.Equal(t, before, sess.GetMessages(a))
	assert.Equal(t, parts, sess.OwnMessages()[1].Message.Presentation)
}

func TestAssistantPresentationUserBoundaryKeepsPlaceholder(t *testing.T) {
	t.Parallel()
	sess := session.New()
	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), sess))
	obs := newPersistenceObserver(store)
	obs.OnEvent(t.Context(), sess, AgentChoiceReasoning("root", sess.ID, "prior reasoning"))
	obs.OnEvent(t.Context(), sess, MessageAdded(sess.ID, session.UserMessage("next task"), "root"))
	obs.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, "new answer"))
	got, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 3)
	assert.Equal(t, []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "prior reasoning"}}, got.Messages[0].Message.Message.Presentation)
	assert.Equal(t, chat.MessageRoleUser, got.Messages[1].Message.Message.Role)
	assert.Equal(t, "next task", got.Messages[1].Message.Message.Content)
	assert.Equal(t, []chat.AssistantPart{{Type: chat.AssistantPartContent, Text: "new answer"}}, got.Messages[2].Message.Message.Presentation)
}
