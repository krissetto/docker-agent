package messages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func newAttachTestModel(t *testing.T, items ...session.Item) *model {
	t.Helper()
	m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
	m.SetSize(80, 24)
	if len(items) > 0 {
		sess := &session.Session{ID: "s", Messages: items}
		m.LoadFromSession(sess, nil)
	}
	return m
}

func assistantItem(agent, content, reasoning string) session.Item {
	return session.NewMessageItem(&session.Message{
		AgentName: agent,
		Message:   chat.Message{Role: chat.MessageRoleAssistant, Content: content, ReasoningContent: reasoning},
	})
}

func taskItem() session.Item {
	const content = "task"
	return session.NewMessageItem(&session.Message{
		Message: chat.Message{Role: chat.MessageRoleUser, Content: content},
	})
}

func contents(m *model) []string {
	out := make([]string, len(m.messages))
	for i, msg := range m.messages {
		out[i] = msg.Content
	}
	return out
}

// A commit for a message already shown from the transcript snapshot removes
// the streamed duplicate bubbles — and only those: snapshot bubbles are never
// touched, even from the same agent.
func TestFinalizeDropsAlreadyShownTail(t *testing.T) {
	t.Parallel()

	m := newAttachTestModel(t,
		taskItem(),
		assistantItem("planner", "the full answer", "thought"),
	)
	rendered := len(m.messages)

	// The attach seed + live deltas duplicate the already-committed message.
	m.AppendReasoning("planner", "thought")
	m.AppendToLastMessage("planner", "the full ")
	m.AppendToLastMessage("planner", "answer")
	require.Greater(t, len(m.messages), rendered)

	m.FinalizeStreamedAssistant("planner", "the full answer", "thought", true)
	assert.Len(t, m.messages, rendered, "duplicate streamed tail removed, snapshot intact: %v", contents(m))
}

// A commit past the snapshot finalizes the streamed bubbles in place with the
// canonical committed content — healing any missed deltas.
func TestFinalizeSettlesStreamedTail(t *testing.T) {
	t.Parallel()

	m := newAttachTestModel(t, taskItem())

	// Viewer only saw part of the stream.
	m.AppendReasoning("planner", "tho")
	m.AppendToLastMessage("planner", "the full ans")

	m.FinalizeStreamedAssistant("planner", "the full answer", "thought", false)

	require.NotEmpty(t, m.messages)
	var text, reasoning string
	for _, msg := range m.messages {
		switch msg.Type {
		case types.MessageTypeAssistant:
			text = msg.Content
		case types.MessageTypeAssistantReasoningBlock:
			reasoning = msg.Content
		}
	}
	assert.Equal(t, "the full answer", text)
	assert.Equal(t, "thought", reasoning)
}

// A commit past the snapshot with no streamed bubbles at all (every delta missed)
// creates them from the canonical content.
func TestFinalizeCreatesMissedMessage(t *testing.T) {
	t.Parallel()

	m := newAttachTestModel(t, taskItem())
	m.FinalizeStreamedAssistant("planner", "the answer", "", false)

	require.NotEmpty(t, m.messages)
	last := m.messages[len(m.messages)-1]
	assert.Equal(t, types.MessageTypeAssistant, last.Type)
	assert.Equal(t, "the answer", last.Content)
}

// The snapshot boundary is authoritative: when the snapshot itself ends with
// an assistant message from the same agent, dropping a duplicate removes only the
// bubbles streamed after the snapshot.
func TestFinalizeRespectsSnapshotBoundary(t *testing.T) {
	t.Parallel()

	m := newAttachTestModel(t,
		assistantItem("planner", "earlier turn", ""),
		assistantItem("planner", "current turn", ""),
	)
	rendered := len(m.messages)

	m.AppendToLastMessage("planner", "current turn")
	m.FinalizeStreamedAssistant("planner", "current turn", "", true)

	assert.Len(t, m.messages, rendered)
	assert.Equal(t, "earlier turn", m.messages[len(m.messages)-2].Content)
	assert.Equal(t, "current turn", m.messages[len(m.messages)-1].Content)
}

// Finalization must not reach across message boundaries: only the LAST
// streamed message's bubbles are canonicalized.
func TestFinalizeOnlyLastMessage(t *testing.T) {
	t.Parallel()

	m := newAttachTestModel(t, taskItem())
	m.AppendToLastMessage("planner", "first streamed")
	// A second streamed message begins (previous one already settled).
	m.FinalizeStreamedAssistant("planner", "first streamed!", "", false)
	m.addMessage(types.Agent(types.MessageTypeAssistant, "planner", "second stre"))
	m.FinalizeStreamedAssistant("planner", "second streamed", "", false)

	texts := []string{}
	for _, msg := range m.messages {
		if msg.Type == types.MessageTypeAssistant {
			texts = append(texts, msg.Content)
		}
	}
	assert.Equal(t, []string{"first streamed!", "second streamed"}, texts)
}

func TestPendingSnapshotWithdrawalDoesNotRenderOrPromoteInput(t *testing.T) {
	pending := taskItem()
	pending.Message.Pending = true
	pending.Message.TurnID = "pending"
	later := taskItem()
	later.Message.Message.Content = "later user"
	m := newAttachTestModel(t, pending, later)
	assert.NotContains(t, contents(m), "task")
	require.Len(t, m.messages, 1)
	require.NotNil(t, m.messages[0].SessionPosition)
	assert.Equal(t, 1, *m.messages[0].SessionPosition)
	m.RemovePendingSessionPosition(0)
	assert.Equal(t, 0, *m.messages[0].SessionPosition)
	assert.Equal(t, 1, m.LoadedItemCount())
	assert.Equal(t, []string{"later user"}, contents(m))
}
