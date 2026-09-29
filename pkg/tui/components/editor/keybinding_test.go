package editor

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/history"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// TestConfigureNewlineKeybinding verifies the editor wires its newline keys
// from the configurable EditorNewline binding and still layers shift+enter on
// terminals that report keyboard enhancements (issue #1626). Expectations are
// derived from the resolved config so the test holds whether or not a user has
// remapped editor_newline.
func TestConfigureNewlineKeybinding(t *testing.T) {
	t.Parallel()
	core.ResetKeys()
	t.Cleanup(core.ResetKeys)
	want := core.GetKeys().EditorNewline.Keys()

	h, err := history.New(t.TempDir())
	require.NoError(t, err)
	e := New(h).(*editor)

	e.keyboardEnhancementsSupported = false
	e.configureNewlineKeybinding()
	assert.Equal(t, want, e.textarea.NewlineBinding().Keys(),
		"without keyboard enhancements the newline keys should match the configured binding")

	e.keyboardEnhancementsSupported = true
	e.configureNewlineKeybinding()
	got := e.textarea.NewlineBinding().Keys()
	require.NotEmpty(t, got)
	assert.Equal(t, "shift+enter", got[0], "shift+enter should be offered first on capable terminals")
	assert.Subset(t, got, want, "configured newline keys must remain available")
}

func TestAltEnterSubmitsFollowUp(t *testing.T) {
	t.Parallel()
	h, err := history.New(t.TempDir())
	require.NoError(t, err)
	e := New(h).(*editor)
	e.SetValue("do this next")
	e.Focus()

	_, cmd := e.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt})
	require.NotNil(t, cmd)
	msg, ok := cmd().(messages.SendMsg)
	require.True(t, ok)
	assert.Equal(t, "do this next", msg.Content)
	assert.True(t, msg.FollowUp)
	assert.Empty(t, e.textarea.Value())
}
