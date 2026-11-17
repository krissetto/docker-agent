package dialog

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestSettingsScrollOnSendOptInPreview(t *testing.T) {
	d := NewSettingsDialog(messages.Preferences{}, true).(*settingsDialog)
	d.tab = tabBehavior
	d.selected[tabBehavior] = rowScrollOnSend
	require.False(t, d.current.ScrollOnSend)
	cmd := d.changeValue(1)
	require.NotNil(t, cmd)
	preview := cmd().(messages.PreviewSettingsMsg)
	require.True(t, preview.Preferences.ScrollOnSend)
	d.changeValue(-1)
	require.False(t, d.current.ScrollOnSend)
}
