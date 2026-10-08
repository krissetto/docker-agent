package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestPersistenceObserver_StreamingStateIsPerSession(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	obs := newPersistenceObserver(store)
	require.NotNil(t, obs)

	s1 := session.New(session.WithID("s1"))
	s2 := session.New(session.WithID("s2"))
	require.NoError(t, store.AddSession(t.Context(), s1))
	require.NoError(t, store.AddSession(t.Context(), s2))

	obs.OnEvent(t.Context(), s1, AgentChoice("root", "s1", "one"))
	obs.OnEvent(t.Context(), s2, AgentChoice("root", "s2", "two"))
	obs.OnEvent(t.Context(), s1, MessageAdded("s1", session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "one"}), "root"))
	obs.OnEvent(t.Context(), s2, MessageAdded("s2", session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "two"}), "root"))

	got1, err := store.GetSession(t.Context(), "s1")
	require.NoError(t, err)
	got2, err := store.GetSession(t.Context(), "s2")
	require.NoError(t, err)

	require.Len(t, got1.Messages, 1)
	require.Len(t, got2.Messages, 1)
	assert.Equal(t, "one", got1.Messages[0].Message.Message.Content)
	assert.Equal(t, "two", got2.Messages[0].Message.Message.Content)
}

func TestPersistenceObserverPartialToolBoundary(t *testing.T) {
	for _, output := range []string{"partial_only", "content", "reasoning", "media", "committed_tool", "committed_response"} {
		t.Run(output, func(t *testing.T) {
			store := session.NewInMemorySessionStore()
			sess := session.New(session.WithID("partial-boundary"))
			require.NoError(t, store.AddSession(t.Context(), sess))
			obs := newPersistenceObserver(store)
			call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "read", Arguments: "{}"}}
			obs.OnEvent(t.Context(), sess, PartialToolCall(call, tools.Tool{Name: "read"}, "root"))
			draft, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			require.Empty(t, draft.MessagesSnapshot(), "uncommitted tool-only deltas must not create durable transcript items")
			final := &chat.Message{Role: chat.MessageRoleAssistant}
			switch output {
			case "content":
				obs.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, "answer"))
				final.Content = "answer"
			case "reasoning":
				obs.OnEvent(t.Context(), sess, AgentChoiceReasoning("root", sess.ID, "think"))
				final.ReasoningContent = "think"
				final.Presentation = []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "think"}}
			case "media":
				final.MultiContent = []chat.MessagePart{{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "image.png", MimeType: "image/png"}}}
			case "committed_tool", "committed_response":
				if output == "committed_tool" {
					obs.OnEvent(t.Context(), sess, ToolCall(call, tools.Tool{Name: "read"}, "root"))
				}
				obs.OnEvent(t.Context(), sess, ToolCallResponse("call", tools.Tool{Name: "read"}, tools.ResultSuccess("result"), "result", "root"))
				draft, err = store.GetSession(t.Context(), sess.ID)
				require.NoError(t, err)
				require.Len(t, draft.MessagesSnapshot(), 1)
				parts := draft.MessagesSnapshot()[0].Message.Message.Presentation
				require.Len(t, parts, 1)
				require.NotNil(t, parts[0].Tool.Result)
				require.Equal(t, "result", *parts[0].Tool.Result)
				final.ToolCalls, final.Presentation = []tools.ToolCall{call}, parts
			}
			boundary := MessageAddedAt(sess.ID, session.NewAgentMessage("root", final), "root", -1).(*MessageAddedEvent)
			boundary.boundaryOnly = output == "partial_only"
			obs.OnEvent(t.Context(), sess, boundary)
			got, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			if output == "partial_only" {
				require.Empty(t, got.MessagesSnapshot())
			} else {
				require.Len(t, got.MessagesSnapshot(), 1)
				require.Equal(t, *final, got.MessagesSnapshot()[0].Message.Message)
			}
		})
	}
}
