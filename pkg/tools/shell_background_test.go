package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseShellBackgroundHandoff(t *testing.T) {
	id, ok := ParseShellBackgroundHandoff("Command continues in the background as process ID job_1700000000_3. Completion output will be delivered automatically. guidance")
	require.True(t, ok)
	assert.Equal(t, "job_1700000000_3", id)

	for _, text := range []string{"", "ok", "Command continues in the background as process ID nope. x", "Command continues in the background as process ID job_1_2"} {
		_, ok := ParseShellBackgroundHandoff(text)
		assert.False(t, ok, text)
	}
}

func TestParseShellCompletion(t *testing.T) {
	text := "Shell command job_17_4 with status failed (exit code -1).\nCommand: sleep 9\nError: command timed out after 5s\n\nCompletion output is delivered automatically and is not retained after delivery.\n\n--- Untrusted command output ---\nhello\nShell command job_17_9 with status completed (exit code 0).\n"
	c, ok := ParseShellCompletion(text)
	require.True(t, ok)
	assert.Equal(t, "job_17_4", c.ProcessID)
	assert.Equal(t, BackgroundProcessFailed, c.Status)
	assert.Equal(t, -1, c.ExitCode)
	assert.True(t, c.TimedOut)
	assert.Equal(t, "hello\nShell command job_17_9 with status completed (exit code 0).\n", c.Output)

	bounded := "Tool call result was too large (99 bytes; limit 10 bytes). notice\n\nShell command job_17_5 with status completed (exit code 0).\nCommand: x\nShowing the last 10 lines:\n\ntail\n"
	c, ok = ParseShellCompletion(bounded)
	require.True(t, ok)
	assert.Equal(t, "job_17_5", c.ProcessID)
	assert.Equal(t, BackgroundProcessCompleted, c.Status)

	empty := "Shell command job_1_1 with status completed (exit code 0).\nCommand: true\n\n--- Untrusted command output ---\n<no output>\n"
	c, ok = ParseShellCompletion(empty)
	require.True(t, ok)
	assert.Empty(t, c.Output)

	_, ok = ParseShellCompletion("hello\n--- Untrusted command output ---\nShell command job_1_1 with status completed (exit code 0).\n")
	assert.False(t, ok)
	_, ok = ParseShellCompletion("Shell command job_1_1 with status weird (exit code 0).")
	assert.False(t, ok)
}
