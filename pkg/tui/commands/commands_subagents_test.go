package commands

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/notification"
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

func TestSubagentPolicyCommandsExactArgumentsAndCompletion(t *testing.T) {
	t.Parallel()
	parser := newTestParser()
	for _, enabled := range []bool{true, false} {
		arg := "off"
		if enabled {
			arg = "on"
		}
		require.Equal(t, messages.SetUseSubagentsMsg{Enabled: enabled}, parser.Parse("/subagents  "+arg+"  ")())
	}
	for _, arg := range []string{"ON", "false", "on extra", "off on", "toggle"} {
		msg := parser.Parse("/subagents " + arg)()
		notice, ok := msg.(notification.ShowMsg)
		require.True(t, ok)
		require.Equal(t, notification.TypeError, notice.Type)
		require.Equal(t, "Usage: /subagents [on|off]", notice.Text)
	}
	for _, item := range builtInSessionCommands() {
		if item.ID == "session.subagents" {
			require.Equal(t, []ArgumentCandidate{{Label: "on", Description: "Enable new subagent delegation"}, {Label: "off", Description: "Disable new subagent delegation"}}, item.CompleteArgument())
		}
	}
}
