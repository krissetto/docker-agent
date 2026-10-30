package runtime

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

// Adapted from upstream 386454d2f: the branch has no userMessageEvent and no
// harness message ID, so user rows are checked by role instead of identity.

func newIdentityStore(t *testing.T) session.Store {
	t.Helper()
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "session.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func persistedMessages(t *testing.T, store session.Store, sessionID string) []session.Message {
	t.Helper()
	saved, err := store.GetSession(t.Context(), sessionID)
	require.NoError(t, err)
	return saved.OwnMessages()
}

func TestPersistentMessageIDsSeparateStreamAttempts(t *testing.T) {
	t.Parallel()
	store := newIdentityStore(t)
	sess := session.New()
	require.NoError(t, store.AddSession(t.Context(), sess))
	obs := newPersistenceObserver(store)
	for _, event := range []Event{
		UserMessage("user", sess.ID, nil, 0),
		AgentChoice("root", sess.ID, "partial", "attempt-one"),
		AgentChoiceReasoning("root", sess.ID, "think", "attempt-two"),
		AgentChoice("root", sess.ID, "answer", "attempt-two"),
		MessageAdded(sess.ID, session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, MessageID: "attempt-two", Content: "answer", ReasoningContent: "think"}), "root"),
		AgentChoice("root", sess.ID, "unfinished", "attempt-three"),
		MessageAdded(sess.ID, session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleTool, Content: "tool result"}), "root"),
	} {
		obs.OnEvent(t.Context(), sess, event)
	}
	msgs := persistedMessages(t, store, sess.ID)
	require.Len(t, msgs, 5)
	assert.Equal(t, chat.MessageRoleUser, msgs[0].Message.Role)
	assert.Equal(t, "attempt-one", msgs[1].Message.MessageID)
	assert.Equal(t, "partial", msgs[1].Message.Content)
	assert.Equal(t, "attempt-two", msgs[2].Message.MessageID)
	assert.Equal(t, "think", msgs[2].Message.ReasoningContent)
	assert.Equal(t, "attempt-three", msgs[3].Message.MessageID)
	assert.Equal(t, "unfinished", msgs[3].Message.Content)
	assert.Equal(t, chat.MessageRoleTool, msgs[4].Message.Role)
}

func TestPersistentMessageIDsStreamEvents(t *testing.T) {
	t.Parallel()
	a := agent.New("root", "test")
	sess := session.New()
	var ids []string
	for range 2 {
		var events []Event
		stream := newStreamBuilder().AddReasoning("thinking").AddContent("hello ").AddContent("world").AddStopWithUsage(1, 1).Build()
		result, err := handleStream(t.Context(), nil, stream, a, nil, sess, nil, defaultTelemetry{}, EventSinkFunc(func(e Event) { events = append(events, e) }), time.Second)
		require.NoError(t, err)
		require.NotEmpty(t, result.MessageID)
		var count int
		for _, ev := range events {
			switch e := ev.(type) {
			case *AgentChoiceEvent:
				assert.Equal(t, result.MessageID, e.MessageID)
				count++
			case *AgentChoiceReasoningEvent:
				assert.Equal(t, result.MessageID, e.MessageID)
				count++
			}
		}
		assert.Equal(t, 3, count)
		ids = append(ids, result.MessageID)
	}
	assert.NotEqual(t, ids[0], ids[1])
}

// The persisted row identity must equal the emitted identity, so a settled
// normal stream keeps one ID from its first chunk to its canonical row.
func TestPersistedRowIDMatchesEmittedOnNormalSettle(t *testing.T) {
	t.Parallel()
	store := newIdentityStore(t)
	sess := session.New()
	require.NoError(t, store.AddSession(t.Context(), sess))
	obs := newPersistenceObserver(store)
	obs.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, "Hel", "normal-id"))
	obs.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, "lo", "normal-id"))
	obs.OnEvent(t.Context(), sess, MessageAdded(sess.ID, session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, MessageID: "normal-id", Content: "Hello"}), "root"))
	obs.OnEvent(t.Context(), sess, StreamStopped(sess.ID, "root", "normal"))
	msgs := persistedMessages(t, store, sess.ID)
	require.Len(t, msgs, 1)
	assert.Equal(t, "normal-id", msgs[0].Message.MessageID)
	assert.Equal(t, "Hello", msgs[0].Message.Content)
}

// A stopped or canceled partial compacts to its own canonical row, keeping its
// own identity. A retry is a new model call with a new identity and a second row.
func TestPersistedRowIDsForStopCancelAndRetry(t *testing.T) {
	t.Parallel()
	store := newIdentityStore(t)
	sess := session.New()
	require.NoError(t, store.AddSession(t.Context(), sess))
	obs := newPersistenceObserver(store)
	obs.OnEvent(t.Context(), sess, UserMessage("go", sess.ID, nil, 0))
	obs.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, "partial ", "stopped-id"))
	obs.OnEvent(t.Context(), sess, StreamStopped(sess.ID, "root", "cancelled"))
	obs.OnEvent(t.Context(), sess, UserMessage("retry", sess.ID, nil, 2))
	obs.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, "retried", "retry-id"))
	obs.OnEvent(t.Context(), sess, MessageAdded(sess.ID, session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, MessageID: "retry-id", Content: "retried"}), "root"))
	obs.OnEvent(t.Context(), sess, StreamStopped(sess.ID, "root", "normal"))
	msgs := persistedMessages(t, store, sess.ID)
	require.Len(t, msgs, 4)
	assert.Equal(t, "stopped-id", msgs[1].Message.MessageID)
	assert.Equal(t, "partial ", msgs[1].Message.Content)
	assert.Equal(t, "retry-id", msgs[3].Message.MessageID)
	assert.Equal(t, "retried", msgs[3].Message.Content)
	assert.NotEqual(t, msgs[1].Message.MessageID, msgs[3].Message.MessageID, "a retry never reuses a row identity")
}

// Each read of an open row extends the previous read: the store never rewrites
// content that was already published.
func TestPersistedOpenRowIsAppendOnlyPrefix(t *testing.T) {
	t.Parallel()
	store := newIdentityStore(t)
	sess := session.New()
	require.NoError(t, store.AddSession(t.Context(), sess))
	obs := newPersistenceObserver(store)
	var previous string
	for _, chunk := range []string{"one ", "two ", "three"} {
		obs.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, chunk, "prefix-id"))
		msgs := persistedMessages(t, store, sess.ID)
		require.Len(t, msgs, 1)
		assert.Equal(t, "prefix-id", msgs[0].Message.MessageID)
		assert.True(t, strings.HasPrefix(msgs[0].Message.Content, previous), "published content must not change: %q is not a prefix of %q", previous, msgs[0].Message.Content)
		previous = msgs[0].Message.Content
	}
	assert.Equal(t, "one two three", previous)
}

// Two sessions can stream at once through one observer without sharing identities.
func TestPersistedMessageIDsAreIsolatedPerSession(t *testing.T) {
	t.Parallel()
	store := newIdentityStore(t)
	first, second := session.New(), session.New()
	require.NoError(t, store.AddSession(t.Context(), first))
	require.NoError(t, store.AddSession(t.Context(), second))
	obs := newPersistenceObserver(store)
	obs.OnEvent(t.Context(), first, AgentChoice("root", first.ID, "a1", "first-id"))
	obs.OnEvent(t.Context(), second, AgentChoice("root", second.ID, "b1", "second-id"))
	obs.OnEvent(t.Context(), first, AgentChoice("root", first.ID, "a2", "first-id"))
	obs.OnEvent(t.Context(), second, StreamStopped(second.ID, "root", "normal"))
	obs.OnEvent(t.Context(), first, StreamStopped(first.ID, "root", "normal"))
	firstMsgs := persistedMessages(t, store, first.ID)
	secondMsgs := persistedMessages(t, store, second.ID)
	require.Len(t, firstMsgs, 1)
	require.Len(t, secondMsgs, 1)
	assert.Equal(t, "first-id", firstMsgs[0].Message.MessageID)
	assert.Equal(t, "a1a2", firstMsgs[0].Message.Content)
	assert.Equal(t, "second-id", secondMsgs[0].Message.MessageID)
	assert.Equal(t, "b1", secondMsgs[0].Message.Content)
}

// Producers without IDs keep the legacy behaviour: each run ends its stream.
func TestPersistentMessageIDsObserverStateIsRunLocal(t *testing.T) {
	t.Parallel()
	store := newIdentityStore(t)
	sess := session.New()
	require.NoError(t, store.AddSession(t.Context(), sess))
	obs := newPersistenceObserver(store)
	for _, content := range []string{"one", "two"} {
		obs.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, content))
		obs.OnEvent(t.Context(), sess, StreamStopped(sess.ID, "root", "normal"))
	}
	msgs := persistedMessages(t, store, sess.ID)
	require.Len(t, msgs, 2)
	assert.Equal(t, "one", msgs[0].Message.Content)
	assert.Equal(t, "two", msgs[1].Message.Content)
}
