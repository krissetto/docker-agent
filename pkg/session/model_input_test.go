package session

import (
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
)

func TestGetMessagesWithProjectionAssemblyParity(t *testing.T) {
	for _, includeContext := range []bool{false, true} {
		t.Run(strconv.FormatBool(includeContext), func(t *testing.T) {
			sess := New()
			sess.AddMessage(UserMessage("compacted"))
			sess.Messages = append(sess.Messages, Item{Summary: "summary"})
			sess.PrepareInstructionContext([]InstructionSource{{Key: "context", Content: "initial", Available: true}})
			message := UserMessage("included")
			message.InputOrigin = InputOriginAgent
			sess.AddMessage(message)
			sess.PrepareInstructionContext([]InstructionSource{{Key: "context", Content: "updated", Available: true}})
			pending := UserMessage("pending")
			pending.Pending = true
			sess.AddMessage(pending)
			sess.AddSubSession(New(WithUserMessage("descendant")))
			a := agent.New("root", "prompt")
			extra := chat.Message{Role: chat.MessageRoleSystem, Content: "legacy"}
			before := sess.Clone()
			want := sess.Clone()
			want.Messages[2].Message.Message.Content = "projected"
			var expected []chat.Message
			if includeContext {
				expected = want.GetMessages(a, extra)
			} else {
				expected = want.GetMessagesWithoutInstructionContext(a, extra)
			}
			var visited []string
			got := sess.GetMessagesWithProjection(a, includeContext, func(message *Message) {
				visited = append(visited, message.Message.Content)
				assert.Equal(t, InputOriginAgent, message.InputOrigin)
				message.Message.Content = "projected"
			}, extra)
			assert.Equal(t, []string{"included"}, visited, "skip compacted, pending, descendant and synthetic messages")
			assert.Equal(t, expected, got, "instruction positions and existing assembly remain unchanged")
			assert.Equal(t, before.MessagesSnapshot(), sess.MessagesSnapshot())
			assert.Equal(t, before.InstructionContext, sess.InstructionContext)
			assert.Equal(t, sess.GetMessages(a), sess.GetMessagesWithProjection(a, true, nil))
		})
	}
}

func TestGetMessagesWithProjectionDetachedPayloads(t *testing.T) {
	sess := New()
	msg := UserMessage("original", chat.MessagePart{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "original-image"}})
	sess.AddMessage(msg)
	before := sess.MessagesSnapshot()
	got := sess.GetMessagesWithProjection(agent.New("root", ""), true, func(message *Message) {
		message.Message.Content = "projected"
		message.Message.MultiContent[0].ImageURL.URL = "projected-image"
	})
	require.Len(t, got, 1)
	assert.Equal(t, "projected-image", got[0].MultiContent[0].ImageURL.URL)
	got[0].MultiContent[0].ImageURL.URL = "caller-mutation"
	assert.Equal(t, before, sess.MessagesSnapshot(), "neither projection nor caller may mutate persisted payload")
}

func TestGetMessagesWithProjectionDoesNotHoldSessionLock(t *testing.T) {
	sess := New(WithUserMessage("first"))
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 100 {
			sess.AddMessage(UserMessage(strconv.Itoa(i)))
		}
	})
	for range 100 {
		got := sess.GetMessagesWithProjection(agent.New("root", ""), true, func(message *Message) {
			// A projection may inspect session metadata without reentering its read lock.
			sess.SetTitle("projected")
			message.Message.Content = "detached"
		})
		for _, message := range got {
			assert.Equal(t, "detached", message.Content)
		}
	}
	wg.Wait()
	for _, item := range sess.MessagesSnapshot() {
		assert.NotEqual(t, "detached", item.Message.Message.Content)
	}
}

func TestGetMessagesWithProjectionPreservesTrimmingAndToolCaps(t *testing.T) {
	for _, historyLimit := range []int{0, 3, 4} {
		t.Run(strconv.Itoa(historyLimit), func(t *testing.T) {
			sess := New(WithMaxOldToolCallTokens(10), WithMaxToolResultTokens(100))
			sess.Messages = append(sess.Messages, reloadedToolExchangeItems("old", strings.Repeat("old-result-", 15000), nil)...)
			sess.Messages = append(sess.Messages, reloadedToolExchangeItems("new", strings.Repeat("new-result-", 15000), nil)...)
			before := sess.MessagesSnapshot()
			a := agent.New("root", "prompt", agent.WithNumHistoryItems(historyLimit))
			want := sess.Clone()
			for _, item := range want.Messages {
				if item.Message.Message.Role == chat.MessageRoleUser {
					item.Message.Message.Content = "projected " + item.Message.Message.Content
				}
			}
			got := sess.GetMessagesWithProjection(a, true, func(message *Message) {
				if message.Message.Role == chat.MessageRoleUser {
					message.Message.Content = "projected " + message.Message.Content
				}
			})
			assert.Equal(t, want.GetMessages(a), got)
			assert.Equal(t, before, sess.MessagesSnapshot())
			if historyLimit == 3 {
				// Two protected user messages leave only one slot; removing the
				// new assistant also removes its otherwise orphaned tool result.
				for _, message := range sess.GetMessages(a) {
					assert.NotEqual(t, chat.MessageRoleTool, message.Role)
				}
				for _, message := range got {
					assert.NotEqual(t, chat.MessageRoleTool, message.Role)
				}
			} else {
				assert.LessOrEqual(t, toolResultTextTokens(toolResultMessage(t, got, "new")), 100)
			}
			if historyLimit == 0 {
				assert.Equal(t, toolContentPlaceholder, toolResultContent(t, got, "old"))
			}
		})
	}
}
