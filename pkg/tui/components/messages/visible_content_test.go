package messages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

const toolCallXML = `<tool_call>{"name":"shell","arguments":{"cmd":"ls"}}</tool_call>`

// The live stream hides <tool_call> XML; a reloaded transcript must hide it too.
func TestSessionReloadHidesToolCallXML(t *testing.T) {
	t.Parallel()

	m := newAttachTestModel(t,
		taskItem(),
		assistantItem("planner", "Checking the files. "+toolCallXML, ""),
	)

	visible := contents(m)
	joined := strings.Join(visible, "\n")
	assert.Contains(t, joined, "Checking the files.")
	assert.NotContains(t, joined, "<tool_call>")
}

// A committed answer without tool calls (for example a response-cache replay)
// finalizes without its tool-call XML, matching the live stream.
func TestCompleteAssistantFinalizesWithoutToolCallXML(t *testing.T) {
	t.Parallel()

	m := newAttachTestModel(t)
	event := &runtime.MessageAddedEvent{
		AgentContext: runtime.AgentContext{AgentName: "planner"},
		Message: &session.Message{
			AgentName: "planner",
			Message:   chat.Message{Role: chat.MessageRoleAssistant, Content: "Answer. " + toolCallXML},
		},
	}
	m.CompleteAssistant(event)

	joined := strings.Join(contents(m), "\n")
	require.NotEmpty(t, joined)
	assert.Contains(t, joined, "Answer.")
	assert.NotContains(t, joined, "<tool_call>")
}
