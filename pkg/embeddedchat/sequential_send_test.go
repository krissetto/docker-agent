package embeddedchat

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type sequentialReplyProvider struct{ stubProvider }

func (sequentialReplyProvider) CreateChatCompletionStream(_ context.Context, messages []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	var prompt string
	for _, message := range messages {
		if message.Role == chat.MessageRoleUser {
			prompt = message.Content
		}
	}
	return &sequentialReplyStream{responses: []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "reply: " + prompt}}}},
		{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}, Usage: &chat.Usage{InputTokens: 1, OutputTokens: 1}},
	}}, nil
}

type sequentialReplyStream struct {
	responses []chat.MessageStreamResponse
}

func (s *sequentialReplyStream) Recv() (chat.MessageStreamResponse, error) {
	if len(s.responses) == 0 {
		return chat.MessageStreamResponse{}, io.EOF
	}
	response := s.responses[0]
	s.responses = s.responses[1:]
	return response, nil
}

func (*sequentialReplyStream) Close() {}

func TestSequentialSendsWithPersistentHistoryBeyondMailboxLimit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	store, err := sqlitestore.New(ctx, filepath.Join(t.TempDir(), "history.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	const historySize = 70
	for index := range historySize {
		root := session.New(session.WithID(fmt.Sprintf("history-%d", index)), session.WithAgentName("root"))
		root.AddMessage(session.UserMessage("historical prompt"))
		require.NoError(t, store.AddSession(ctx, root))
	}
	initial := session.New(session.WithID("resumed-root"), session.WithAgentName("root"))
	for index := range historySize {
		initial.AddMessage(session.UserMessage(fmt.Sprintf("past prompt %d", index)))
		initial.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: fmt.Sprintf("past reply %d", index)}})
	}
	require.NoError(t, store.AddSession(ctx, initial))
	loaded, err := store.GetSession(ctx, initial.ID)
	require.NoError(t, err)
	require.Len(t, loaded.GetAllMessages(), 2*historySize)
	tm := team.New(team.WithAgents(agent.New("root", "Reply to the latest message.", agent.WithModel(sequentialReplyProvider{}))))
	s, err := New(ctx, Config{Team: tm, InitialSession: loaded, RuntimeOptions: []runtime.Opt{runtime.WithSessionStore(store)}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	handle := s.handle
	require.Equal(t, initial.ID, handle.ID())
	var turnIDs []string
	for index, prompt := range []string{"first live message", "second live message"} {
		out, err := s.Send(ctx, prompt)
		require.NoError(t, err, "sequential send %d must not report session start capacity", index+1)
		var text strings.Builder
		var done bool
		for event := range out {
			require.NoError(t, event.Err)
			text.WriteString(event.Text)
			done = done || event.Done
		}
		require.True(t, done)
		assert.Equal(t, "reply: "+prompt, text.String())
		snapshot, err := s.Conversation(ctx)
		require.NoError(t, err)
		assert.Equal(t, initial.ID, snapshot.ID)
		assert.Len(t, snapshot.GetAllMessages(), 2*historySize+2*(index+1))
		status, err := handle.Status(ctx)
		require.NoError(t, err)
		assert.Equal(t, runtime.SessionStateSettled, status.State)
		assert.Zero(t, status.Pending)
		require.Same(t, handle, s.handle, "both sends reuse the same canonical handle")
		var since uint64
		observation, err := handle.Observe(ctx, runtime.ObserveOptions{Since: &since})
		require.NoError(t, err)
		var stoppedTurn string
		for _, event := range observation.Replay {
			if _, stopped := event.Event.(*runtime.StreamStoppedEvent); stopped {
				stoppedTurn = event.TurnID
			}
		}
		observation.Cancel()
		require.NotEmpty(t, stoppedTurn)
		turnIDs = append(turnIDs, stoppedTurn)
	}
	assert.NotEqual(t, turnIDs[0], turnIDs[1], "distinct accepted requests must not reuse a turn ID")
	persisted, err := store.GetSession(ctx, initial.ID)
	require.NoError(t, err)
	assert.Len(t, persisted.GetAllMessages(), 2*historySize+4)
	assert.Equal(t, "past prompt 0", persisted.GetAllMessages()[0].Message.Content)
	assert.Equal(t, "reply: second live message", persisted.GetLastAssistantMessageContent())
	roots, err := store.GetSessions(ctx)
	require.NoError(t, err)
	assert.Len(t, roots, historySize+1, "sends must not recreate sessions or clear stored history")
	assert.Len(t, loaded.GetAllMessages(), 2*historySize, "the supplied snapshot remains detached")
}
