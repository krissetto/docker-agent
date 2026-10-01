package commands

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestSubagentCommands(t *testing.T) {
	t.Parallel()
	parser := newTestParser()
	for _, name := range []string{"subagent-view", "subagent-attach"} {
		command := parser.Parse("/" + name + "  a0b1c ")
		assert.Nil(t, command)
	}
	command := parser.Parse("/subagents")
	require.NotNil(t, command)
	assert.IsType(t, messages.ShowSubagentSessionsMsg{}, command())
	command = parser.Parse("/back")
	require.NotNil(t, command)
	assert.IsType(t, messages.ReturnToPreviousSessionMsg{}, command())
	for _, item := range builtInSessionCommands() {
		switch item.SlashCommand {
		case "/subagents", "/back":
			assert.True(t, item.Immediate)
			assert.False(t, item.Hidden)
			assert.NotEmpty(t, item.Description)
		}
	}
}

func TestResumeCommandUsesOriginStampableMessage(t *testing.T) {
	t.Parallel()
	command := newTestParser().Parse("/resume")
	require.NotNil(t, command)
	assert.Equal(t, messages.ResumeSessionMsg{}, command())
}
