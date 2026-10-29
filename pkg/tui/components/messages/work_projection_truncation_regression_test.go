package messages

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// projectionReferences reports whether any Work exchange still names index.
func projectionReferences(m *model, index int) bool {
	for _, exchange := range m.workProjection.exchanges {
		if slices.Contains(exchange.work, index) {
			return true
		}
	}
	return false
}

func assertProjectionBounded(t *testing.T, m *model) {
	t.Helper()
	m.syncWorkProjection()
	for _, exchange := range m.workProjection.exchanges {
		for _, index := range exchange.work {
			assert.Less(t, index, len(m.messages), "projection must not reference removed messages")
		}
	}
}

// Case 1: FinalizeStreamedAssistant removes a streamed tail and must invalidate Work.
func TestStreamedTailTruncationInvalidatesWorkProjection(t *testing.T) {
	for _, workEnabled := range []bool{true, false} {
		name := "work-disabled"
		if workEnabled {
			name = "work-enabled"
		}
		t.Run(name, func(t *testing.T) {
			m := newAttachTestModel(t, taskItem(), assistantItem("planner", "the full answer", "thought"))
			_ = m.SetCompactWorkLog(workEnabled)
			rendered := len(m.messages)

			m.AppendReasoning("planner", "thought")
			m.AppendToLastMessage("planner", "the full ")
			m.AppendToLastMessage("planner", "answer")
			require.Greater(t, len(m.messages), rendered)
			m.syncWorkProjection()
			tail := rendered
			if workEnabled {
				require.True(t, projectionReferences(m, tail), "pre-sync projection must reference the streamed tail")
			}

			m.FinalizeStreamedAssistant("planner", "the full answer", "thought", true)
			require.Len(t, m.messages, rendered, "streamed tail removed: %v", contents(m))
			if workEnabled {
				assert.True(t, m.workProjectionDirty, "truncation must invalidate the Work projection")
			}
			for source := range m.messages {
				m.refreshWorkStatusForSource(source)
			}
			assertProjectionBounded(t, m)
		})
	}
}

// Case 2: the canonical mB2 sequence. Streamed prose ends the active reasoning block, so a
// pending partial tool call becomes a top-level message at the tail (a Work member). Its
// commit removes that tail (slices.Delete, then invalidateAllItems), and the presentation
// replacement then invalidates the reasoning block whose Work projection still names the
// removed index.
func TestCompleteAssistantRetiresOrphanPartialTailWithWorkProjection(t *testing.T) {
	m := newSpinnerTestModel(t)
	_ = m.SetCompactWorkLog(true)
	m.AddUserMessage("ask the team")
	m.AppendReasoning("planner", "thought")
	m.AppendToLastMessage("planner", "staff meeting.")
	call := tools.ToolCall{ID: "partial-1", Type: "function", Function: tools.FunctionCall{Name: "check", Arguments: `{}`}}
	m.AddOrUpdateToolCall("planner", call, tools.Tool{Name: "check"}, types.ToolStatusPending)
	tail := len(m.messages) - 1
	require.Equal(t, types.MessageTypeToolCall, m.messages[tail].Type, "pending partial must be a top-level tail message")
	require.Equal(t, types.ToolStatusPending, m.messages[tail].ToolStatus)
	m.syncWorkProjection()
	require.True(t, projectionReferences(m, tail), "pre-commit projection must reference the pending partial tool")

	event := runtime.MessageAddedAt("s", session.NewAgentMessage("planner", &chat.Message{
		Role:         chat.MessageRoleAssistant,
		Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "thought, finalized"}},
	}), "planner", 2).(*runtime.MessageAddedEvent)
	require.NotPanics(t, func() { m.CompleteAssistant(event) })

	for _, msg := range m.messages {
		require.NotEqual(t, types.MessageTypeToolCall, msg.Type, "orphan partial must be retired")
	}
	assertProjectionBounded(t, m)
}
