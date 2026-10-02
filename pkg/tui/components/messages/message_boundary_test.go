package messages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestCommittedAssistantBoundaryPreservesChunkBytes(t *testing.T) {
	for _, reasoning := range []bool{false, true} {
		t.Run(map[bool]string{false: "content", true: "reasoning"}[reasoning], func(t *testing.T) {
			m := newAttachTestModel(t, taskItem())
			appendChunk := m.AppendToLastMessage
			kind := types.MessageTypeAssistant
			if reasoning {
				appendChunk = m.AppendReasoning
				kind = types.MessageTypeAssistantReasoningBlock
			}
			commit := func(text string) {
				if reasoning {
					m.FinalizeStreamedAssistant("root", "", text, false)
				} else {
					m.FinalizeStreamedAssistant("root", text, "", false)
				}
			}
			appendChunk("root", "staff mee")
			appendChunk("root", "ting.")
			commit("staff meeting.")
			appendChunk("root", "I’m Sh")
			appendChunk("root", "elly:  exact\tbytes.")
			commit("I’m Shelly:  exact\tbytes.")
			var texts []string
			for _, msg := range m.messages {
				if msg.Type == kind {
					texts = append(texts, msg.Content)
				}
			}
			require.Len(t, texts, 2, "committed messages must not be overwritten or concatenated")
			assert.Equal(t, []string{"staff meeting.", "I’m Shelly:  exact\tbytes."}, texts)
		})
	}
}

func TestAssistantBoundaryRetiresOnlyUncommittedPartialTools(t *testing.T) {
	for _, reasoning := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "reasoning"}[reasoning], func(t *testing.T) {
			m := newAttachTestModel(t)
			if reasoning {
				m.AppendReasoning("root", "exact reasoning.")
			}
			for _, call := range []struct {
				id     string
				status types.ToolStatus
			}{{"orphan", types.ToolStatusPending}, {"published", types.ToolStatusPending}, {"running", types.ToolStatusRunning}, {"completed", types.ToolStatusCompleted}} {
				m.AddOrUpdateToolCall("root", tools.ToolCall{ID: call.id, Function: tools.FunctionCall{Name: call.id}}, tools.Tool{Name: call.id}, call.status)
			}
			m.CommitAssistant("root", []string{"published"})
			var ids []string
			for i, msg := range m.messages {
				if msg.Type == types.MessageTypeToolCall {
					ids = append(ids, msg.ToolCall.ID)
				}
				if block, ok := m.views[i].(*reasoningblock.Model); ok {
					assert.False(t, block.HasToolCall("orphan"))
					for _, id := range []string{"published", "running", "completed"} {
						assert.True(t, block.HasToolCall(id))
					}
				}
			}
			if !reasoning {
				assert.Equal(t, []string{"published", "running", "completed"}, ids)
			}
		})
	}
}

func TestAssistantBoundaryRestoredReasoningAndDeferredBytes(t *testing.T) {
	m := newAttachTestModel(t, assistantItem("root", "", "first."), assistantItem("root", "", "second."))
	assert.Equal(t, []string{"first.", "second."}, contents(m))
	m.AppendToLastMessage("root", "word")
	m.userHasScrolled = true
	m.AppendToLastMessage("root", "part")
	m.CompleteAssistant(runtime.MessageAddedAt("s", session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "wordpart"}), "root", 2).(*runtime.MessageAddedEvent))
	m.AppendToLastMessage("root", "next")
	assert.Equal(t, []string{"first.", "second.", "wordpart", "next"}, contents(m))
}

func TestRestoreUnfinishedToolWaitsForAuthoritativeState(t *testing.T) {
	call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "check", Arguments: "canonical"}}
	item := session.NewMessageItem(&session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}}})
	m := newAttachTestModel(t, item)
	require.Len(t, m.messages, 1)
	assert.Equal(t, types.ToolStatusPending, m.messages[0].ToolStatus)
	m.AddOrUpdateToolCall("worker", call, tools.Tool{}, types.ToolStatusRunning)
	assert.Equal(t, types.ToolStatusRunning, m.messages[0].ToolStatus)
	assert.Equal(t, "canonical", m.messages[0].ToolCall.Function.Arguments)
	m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: "call", Response: "done", Result: &tools.ToolCallResult{Output: "done"}}, types.ToolStatusCompleted)
	assert.Equal(t, types.ToolStatusCompleted, m.messages[0].ToolStatus)
}
