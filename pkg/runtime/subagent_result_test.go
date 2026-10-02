package runtime

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestReadSubagentPrefersLatestResponseWithoutToolCalls(t *testing.T) {
	response := func(content string) chat.Message {
		return chat.Message{Role: chat.MessageRoleAssistant, Content: content}
	}
	toolCall := func(content string) chat.Message {
		return chat.Message{Role: chat.MessageRoleAssistant, Content: content, ToolCalls: []tools.ToolCall{{ID: "call"}}}
	}
	for _, tc := range []struct {
		name     string
		messages []chat.Message
		want     string
	}{
		{"no assistant", []chat.Message{{Role: chat.MessageRoleUser, Content: "task"}}, ""},
		{"newest genuine response", []chat.Message{response("old"), response("new")}, "new"},
		{"skip tool call with text", []chat.Message{response("answer"), toolCall("working")}, "answer"},
		{"skip empty tool call", []chat.Message{response("answer"), toolCall("")}, "answer"},
		{"tool call fallback", []chat.Message{toolCall("older"), toolCall("newest")}, "newest"},
		{"empty newest fallback", []chat.Message{toolCall("older"), toolCall("")}, ""},
		{"empty genuine response", []chat.Message{toolCall("working"), response("")}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestSubagentManager(t)
			parent := session.New(session.WithID("parent"))
			child := session.New(session.WithID("child"))
			for _, message := range tc.messages {
				child.AddMessage(session.NewAgentMessage("worker", &message))
			}
			m.registerChild(parent, "root", "abcde", "worker", child)
			m.children["abcde"].durable.Result = "durable preview"
			rec, err := m.readChild(parent.ID, "abcde")
			require.NoError(t, err)
			require.Equal(t, subagent.NodeRunning, rec.state)
			require.Equal(t, tc.want, rec.result)
		})
	}
}
