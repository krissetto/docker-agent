package shellprocess

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestShellJobCompletionIsBoundedAndKeepsOutcome(t *testing.T) {
	job := &shellProcess{id: "proc_1", cmd: "build"}
	got := formatShellJobCompletion(job, statusFailed, 42, strings.Repeat("x", maxShellProcessOutputBytes))
	assert.LessOrEqual(t, len(got), 50*1024)
	assert.Contains(t, got, "proc_1 with status failed (exit code 42)")
	assert.Contains(t, got, "Command: build")
	assert.Contains(t, got, "Tool call result was too large")
	assert.Contains(t, got, "Command: build")
	assert.Contains(t, got, "status failed (exit code 42)")
	assert.NotContains(t, got, "inspect_running_process")
	assert.Contains(t, got, strings.Repeat("x", 100), "output tail remains useful")
}
