package session

import (
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestScalarHistoryMatchesMessageSnapshot(t *testing.T) {
	t.Parallel()
	sess := New()
	child := New()
	grandchild := New()
	sess.AddMessage(UserMessage(" parent request "))
	child.AddMessage(UserMessage(" child request "))
	grandchild.AddMessage(&Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: " nested answer "}})
	child.AddSubSession(grandchild)
	sess.AddLiveSubSession(child)
	sess.AddError(&Error{Message: "not a message"})
	sess.AddTermination(&Termination{Reason: TerminationReasonBudgetExceeded})
	sess.Messages = append(sess.Messages, Item{Summary: "not a message"}, Item{})
	sess.AddMessage(&Message{Message: chat.Message{Role: chat.MessageRoleSystem, Content: "not conversation"}})
	assertMatches := func() {
		t.Helper()
		messages := sess.GetAllMessages()
		require.Equal(t, len(messages), sess.AllMessageCount())
		for role, got := range map[chat.MessageRole]string{
			chat.MessageRoleUser: sess.GetLastUserMessageContent(), chat.MessageRoleAssistant: sess.GetLastAssistantMessageContent(),
		} {
			want := ""
			for i := len(messages) - 1; i >= 0; i-- {
				if messages[i].Message.Role == role {
					want = strings.TrimSpace(messages[i].Message.Content)
					break
				}
			}
			require.Equal(t, want, got)
		}
	}
	assertMatches()
	require.Equal(t, "nested answer", sess.GetLastAssistantMessageContent())
	grandchild.AddMessage(UserMessage(" \t "))
	assertMatches()
	require.Empty(t, sess.GetLastUserMessageContent(), "a blank last match must not fall back to earlier content")
	sess.AddMessage(&Message{DisplayOnly: true, Message: chat.Message{Role: chat.MessageRoleAssistant, Content: " own final answer "}})
	assertMatches()
	require.Equal(t, "own final answer", sess.GetLastAssistantMessageContent())
	require.Zero(t, New().AllMessageCount())
	require.Empty(t, New().GetLastUserMessageContent())
}

func TestScalarHistoryAndAttributeConcurrentReads(t *testing.T) {
	t.Parallel()
	sess, child := New(), New()
	sess.AddLiveSubSession(child)
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 100 {
			sess.SetAttribute("policy", "true")
			child.AddMessage(UserMessage(" request "))
			child.AddMessage(&Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: " answer "}})
			sess.DeleteAttribute("policy")
		}
	})
	for range 100 {
		_, _ = sess.Attribute("policy")
		_ = sess.AllMessageCount()
		_ = sess.GetLastUserMessageContent()
		_ = sess.GetLastAssistantMessageContent()
	}
	wg.Wait()
	require.Equal(t, 200, sess.AllMessageCount())
	require.Equal(t, "request", sess.GetLastUserMessageContent())
	require.Equal(t, "answer", sess.GetLastAssistantMessageContent())
	sess.SetAttribute("policy", "")
	value, exists := sess.Attribute("policy")
	require.True(t, exists)
	require.Empty(t, value)
	value, exists = sess.Attribute("missing")
	require.False(t, exists)
	require.Empty(t, value)
}

func TestScalarHistoryReadsObservePendingPayloadReplacement(t *testing.T) {
	t.Parallel()
	sess := New()
	pending := UserMessage("before")
	pending.Pending = true
	pending.Accepted = true
	pending.InputOrigin = InputOriginUser
	pending.TurnID = "pending-turn"
	sess.AddMessage(pending)
	before := sess.GetLastUserMessageContent()
	snapshot := sess.GetAllMessages()
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 100 {
			sess.ReplacePendingUserMessagePayload("pending-turn", "after", nil)
			sess.ReplacePendingUserMessagePayload("pending-turn", "before", nil)
		}
	})
	for range 100 {
		content := sess.GetLastUserMessageContent()
		require.Contains(t, []string{"before", "after"}, content)
	}
	wg.Wait()
	require.True(t, sess.ReplacePendingUserMessagePayload("pending-turn", "final", nil))
	require.Equal(t, "final", sess.GetLastUserMessageContent())
	require.Equal(t, "before", before)
	require.Equal(t, "before", snapshot[0].Message.Content)
	require.Equal(t, 1, sess.AllMessageCount())
}
