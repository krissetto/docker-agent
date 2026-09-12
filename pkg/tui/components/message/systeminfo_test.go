package message

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/subagent"
)

func TestRenderSystemInfoUser(t *testing.T) {
	t.Parallel()

	out := renderSystemInfoUser(subagent.WrapSystemInfo(`Subagent "worker" (a1b2c) finished.`))
	assert.Contains(t, out, "worker")
	assert.Contains(t, out, "(a1b2c) replied")
	assert.NotContains(t, out, "finished", "note body must not leak into the compact line")
	assert.NotContains(t, out, "<system_info>")

	fallback := renderSystemInfoUser(subagent.WrapSystemInfo("no attribution here"))
	assert.Contains(t, fallback, "system info received")
}
