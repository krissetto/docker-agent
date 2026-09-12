package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/history"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// Runtime-authored notes (subagent turn reports, agent-to-agent relays)
// must never enter the user's prompt history, no matter which path resends
// them (e.g. inline-editing a system_info bubble).
func TestSendMsgHistoryExcludesSystemInfo(t *testing.T) {
	m, _ := newTestModel(t)
	hist, err := history.New(t.TempDir())
	require.NoError(t, err)
	m.history = hist

	send := func(content string, bypass bool) {
		_, _ = m.Update(messages.SendMsg{Content: content, BypassQueue: bypass})
	}

	send("fix the tests", false)
	send(subagent.WrapSystemInfo(`Subagent "worker" (aaaaa) finished its turn.`), false)
	send("a real question mentioning <system_info> mid-text", false)
	send("bypassed command output", true)

	assert.Equal(t,
		[]string{"fix the tests", "a real question mentioning <system_info> mid-text"},
		hist.Messages)
}
