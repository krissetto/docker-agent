package runtime

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
)

func TestOpenAIResponseStateReachesSession(t *testing.T) {
	t.Parallel()
	state := &chat.OpenAIResponse{ID: "resp_1", Source: "openai/gpt-5.6", Output: []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque"}`),
	}}
	stream := newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build()
	stream.responses[len(stream.responses)-1].Choices[0].Delta.OpenAIResponse = state
	root := agent.New("root", "test", agent.WithModel(&mockProvider{id: "openai/gpt-5.6", stream: stream}))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	sess := session.New(session.WithUserMessage("go"), session.WithTitle("test"))
	for range rt.Drive(t.Context(), sess) {
	}
	var found *chat.OpenAIResponse
	for _, item := range sess.Messages {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleAssistant {
			found = item.Message.Message.OpenAIResponse
		}
	}
	require.NotNil(t, found)
	assert.Equal(t, state, found)
}

func TestOpenAIResponseStateDiscardedOnRefusedTools(t *testing.T) {
	t.Parallel()
	stream := newStreamBuilder().AddToolCallName("call_1", "shell").AddToolCallArguments("call_1", `{}`).AddRefusal().Build()
	stream.responses[len(stream.responses)-1].Choices[0].Delta.OpenAIResponse = &chat.OpenAIResponse{ID: "refused"}
	root := agent.New("root", "test")
	result, err := handleStream(t.Context(), nil, stream, root, nil, session.New(), nil, defaultTelemetry{}, NewChannelSink(make(chan Event, 10)), defaultStreamIdleTimeout)
	require.NoError(t, err)
	assert.Nil(t, result.OpenAIResponse)
	assert.Empty(t, result.Calls)
}

func TestOpenAIResponseStateDurableReloadAndAttach(t *testing.T) {
	for _, visible := range []bool{false, true} {
		t.Run(fmt.Sprintf("visible=%t", visible), func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "sessions.db")
			store, err := sqlitestore.New(t.Context(), dbPath)
			require.NoError(t, err)
			state := &chat.OpenAIResponse{ID: "resp_opaque", Source: "openai/gpt-5.6", ReasoningContent: new(string), Output: []json.RawMessage{json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque"}`)}}
			stream := newStreamBuilder()
			if visible {
				stream.AddContent("done")
			}
			completion := stream.AddStopWithUsage(3, 5).Build()
			completion.responses[len(completion.responses)-1].Choices[0].Delta.OpenAIResponse = state
			root := agent.New("root", "prompt", agent.WithModel(&mockProvider{id: "openai/gpt-5.6", stream: completion}))
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
			require.NoError(t, err)
			sess := session.New(session.WithAgentName("root"), session.WithTitle("state"), session.WithWorkingDir(t.TempDir()))
			h, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			observation, err := h.Observe(t.Context(), ObserveOptions{Buffer: 64})
			require.NoError(t, err)
			submitted, err := h.Submit(t.Context(), TurnInput{Content: "go"})
			require.NoError(t, err)
			for envelope := range observation.Events {
				if envelope.TurnID == submitted.TurnID {
					if _, ok := envelope.Event.(*StreamStoppedEvent); ok {
						break
					}
				}
			}
			observation.Cancel()
			require.NoError(t, h.AwaitTurn(t.Context(), submitted.TurnID))
			require.NoError(t, rt.Close())
			require.NoError(t, store.Close())
			reopened, err := sqlitestore.New(t.Context(), dbPath)
			require.NoError(t, err)
			defer reopened.Close()
			loaded, err := reopened.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			require.Equal(t, string(TurnCompleted), loaded.TurnOutcome(submitted.TurnID))
			var found *session.Message
			for _, msg := range loaded.OwnMessages() {
				require.False(t, msg.Pending)
				if msg.Message.OpenAIResponse != nil {
					copied := msg
					found = &copied
				}
			}
			require.NotNil(t, found)
			assert.Equal(t, state, found.Message.OpenAIResponse)
			assert.False(t, found.DisplayOnly, "opaque-only output is provider input, not presentation-only")
			require.Contains(t, loaded.GetMessages(root), found.Message)
			resumed, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionStore(reopened), WithSessionCompaction(false))
			require.NoError(t, err)
			defer resumed.Close()
			attached, err := resumed.CreateSession(t.Context(), loaded, SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			snapshot, err := attached.Snapshot(t.Context())
			require.NoError(t, err)
			require.Equal(t, state, snapshot.OwnMessages()[1].Message.OpenAIResponse)
			snapshot.Messages[1].Message.Message.OpenAIResponse.Output[0][0] = '['
			again, err := attached.Snapshot(t.Context())
			require.NoError(t, err)
			require.Equal(t, byte('{'), again.OwnMessages()[1].Message.OpenAIResponse.Output[0][0])
		})
	}
}
