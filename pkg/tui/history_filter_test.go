package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/history"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestSendMsgHistoryPreservesUserSystemInfoLiteral(t *testing.T) {
	m, _ := newTestModel(t)
	dir := t.TempDir()
	hist, err := history.New(dir)
	require.NoError(t, err)
	m.history = hist

	const literal = "<system_info>user literal</system_info>"
	want := []string{"fix the tests", "a real question mentioning <system_info> mid-text", literal}
	for _, content := range want {
		_, _ = m.Update(messages.SendMsg{Content: content})
	}
	_, _ = m.Update(messages.SendMsg{Content: "bypassed command output", BypassQueue: true})
	assert.Equal(t, want, hist.Messages)

	reloaded, err := history.New(dir)
	require.NoError(t, err)
	assert.Equal(t, want, reloaded.Messages)

	ed := editor.New(reloaded)
	ed.SetSize(100, 5)
	_, _ = ed.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	assert.Equal(t, literal, ed.Value())
	assert.Contains(t, ansi.Strip(ed.View()), literal)
}

func TestRuntimeInputEventsDoNotEnterPromptHistory(t *testing.T) {
	m, _ := newTestModel(t)
	hist, err := history.New(t.TempDir())
	require.NoError(t, err)
	m.history = hist

	const content = "runtime steering without tags"
	for _, event := range []runtime.Event{
		&runtime.PendingUserMessageAcceptedEvent{TurnID: "internal", Message: content, InputOrigin: session.InputOriginRuntime},
		&runtime.PendingUserMessagePromotedEvent{TurnID: "internal", Message: content, InputOrigin: session.InputOriginRuntime},
		&runtime.UserMessageEvent{TurnID: "internal", Message: content, InputOrigin: session.InputOriginRuntime},
	} {
		_, _ = m.Update(messages.SessionRuntimeEventMsg{Event: event})
	}
	assert.Empty(t, hist.Messages)
	assert.Empty(t, hist.Previous())
}
