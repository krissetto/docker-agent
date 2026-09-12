package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
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
