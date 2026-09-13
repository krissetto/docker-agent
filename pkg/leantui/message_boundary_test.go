package leantui

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestCommittedAssistantBoundaryFlushesLeanPending(t *testing.T) {
	m := bareModel(24)
	m.handleEvent(t.Context(), runtime.AgentChoice("root", "s", "staff mee"))
	m.handleEvent(t.Context(), runtime.AgentChoice("root", "s", "ting."))
	m.handleEvent(t.Context(), runtime.MessageAddedAt("s", session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "staff meeting."}), "root", 0))
	assert.Equal(t, 1, m.screen.Transcript.BlockCount(), "assistant commit must seal pending content")
	m.handleEvent(t.Context(), runtime.AgentChoice("root", "s", "I’m Sh"))
	m.handleEvent(t.Context(), runtime.AgentChoice("root", "s", "elly."))
	m.handleEvent(t.Context(), runtime.MessageAddedAt("s", session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "I’m Shelly."}), "root", 1))
	assert.Equal(t, 2, m.screen.Transcript.BlockCount())
}
