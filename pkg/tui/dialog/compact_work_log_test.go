package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestSettingsCompactWorkLogCanPreviewDisabled(t *testing.T) {
	d := NewSettingsDialog(messages.Preferences{CompactWorkLog: true}, true).(*settingsDialog)
	d.SetSize(100, 50)
	d.focus = settingsControls
	d.selected[tabAppearance] = rowCompactWorkLog
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.False(t, d.current.CompactWorkLog)
	msgs := collectMsgs(cmd)
	require.True(t, hasMsg[messages.PreviewSettingsMsg](msgs))
	require.Contains(t, d.View(), "Compact work log")
}
