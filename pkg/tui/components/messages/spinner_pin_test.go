package messages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func newSpinnerTestModel(t *testing.T) *model {
	t.Helper()
	m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
	m.SetSize(80, 24)
	return m
}

func messageTypes(m *model) []types.MessageType {
	out := make([]types.MessageType, len(m.messages))
	for i, msg := range m.messages {
		out[i] = msg.Type
	}
	return out
}

// Content arriving while the pending-response spinner is shown (e.g. a
// runtime-injected subagent note) must slot in above the spinner, keeping it
// pinned at the tail so removeSpinner's fast path stays valid.
func TestAddMessageKeepsSpinnerAtTail(t *testing.T) {
	t.Parallel()

	m := newSpinnerTestModel(t)
	m.AddUserMessage("ask the team")
	m.AddAssistantMessage("", "") // pending-response spinner

	m.ReplaceLoadingWithUser("<system_info>reviewer replied</system_info>", 3)

	require.Len(t, m.messages, 3)
	assert.Equal(t, []types.MessageType{
		types.MessageTypeUser,
		types.MessageTypeUser,
		types.MessageTypeSpinner,
	}, messageTypes(m))
	require.Len(t, m.views, 3)

	m.RemoveSpinner()
	assert.Equal(t, []types.MessageType{
		types.MessageTypeUser,
		types.MessageTypeUser,
	}, messageTypes(m))
}

// A reasoning block created while the spinner is visible slots in above it too
// (FinalizeStreamedAssistant reaches addReasoningBlock without removeSpinner).
func TestAddReasoningBlockKeepsSpinnerAtTail(t *testing.T) {
	t.Parallel()

	m := newSpinnerTestModel(t)
	m.AddAssistantMessage("", "")

	m.FinalizeStreamedAssistant("root", "", "some thinking", false)

	require.Len(t, m.messages, 2)
	assert.Equal(t, []types.MessageType{
		types.MessageTypeAssistantReasoningBlock,
		types.MessageTypeSpinner,
	}, messageTypes(m))
}

// A spinner buried mid-list (tail invariant broken by a direct append) is
// still found and removed instead of getting stuck in the transcript forever.
func TestRemoveSpinnerFindsBuriedSpinner(t *testing.T) {
	t.Parallel()

	m := newSpinnerTestModel(t)
	m.AddAssistantMessage("", "")

	// Simulate an append path that bypasses the tail pinning.
	msg := types.User("landed after the spinner")
	m.messages = append(m.messages, msg)
	m.views = append(m.views, m.createMessageView(msg))
	require.Equal(t, types.MessageTypeSpinner, m.messages[0].Type)

	m.RemoveSpinner()

	require.Len(t, m.messages, 1)
	assert.Equal(t, types.MessageTypeUser, m.messages[0].Type)
	assert.Len(t, m.views, 1)
}
