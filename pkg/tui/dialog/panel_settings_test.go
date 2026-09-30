package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestSettingsPanelReorderPreviewApplyCancel(t *testing.T) {
	t.Parallel()
	input := messages.DefaultPanelSettings()
	d := NewSettingsDialog(messages.Preferences{Panel: input}, false).(*settingsDialog)
	d.SetSize(100, 40)
	d.tab = tabPanel
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModCtrl})
	previews := collectMsgs(cmd)
	require.Len(t, previews, 1)
	preview := previews[0].(messages.PreviewPanelMsg)
	want := []messages.PanelElement{messages.PanelSubagents, messages.PanelWorkspace, messages.PanelTodos}
	assert.Equal(t, want, preview.Panel.Elements)
	assert.Equal(t, messages.DefaultPanelSettings(), input, "draft cannot mutate caller or cancel baseline")
	preview.Panel.Elements[0] = messages.PanelTodos
	assert.Equal(t, want, d.current.Panel.Elements, "preview payload is detached")
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	cancelled := collectMsgs(cmd)
	require.Len(t, cancelled, 2)
	assert.Equal(t, messages.CancelPanelPreviewMsg{Original: messages.DefaultPanelSettings()}, cancelled[1])
	_, cmd = d.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	applied := collectMsgs(cmd)
	require.Len(t, applied, 2)
	assert.Equal(t, want, applied[1].(messages.ApplySettingsMsg).Preferences.Panel.Elements)
}

func TestSettingsPanelAllOffAndReenable(t *testing.T) {
	t.Parallel()
	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.tab = tabPanel
	for row := range d.panelOrder {
		d.selected[tabPanel] = row
		_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
		require.IsType(t, messages.PreviewPanelMsg{}, collectMsgs(cmd)[0])
	}
	require.NotNil(t, d.current.Panel.Elements)
	assert.Empty(t, d.current.Panel.Elements)
	applied := collectMsgs(d.apply())
	require.NotNil(t, applied[1].(messages.ApplySettingsMsg).Preferences.Panel.Elements)
	d.selected[tabPanel] = 0
	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	assert.Equal(t, []messages.PanelElement{messages.PanelWorkspace}, d.current.Panel.Elements)
	assert.Contains(t, d.View(), "Workspace")
}

func TestSettingsPanelCancelBothPreviews(t *testing.T) {
	t.Parallel()
	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.current.Layout.HideTodos = true
	d.tab = tabPanel
	d.togglePanel()
	msgs := collectMsgs(d.cancel())
	require.Len(t, msgs, 3)
	require.IsType(t, CloseDialogMsg{}, msgs[0])
	require.IsType(t, messages.CancelLayoutPreviewMsg{}, msgs[1])
	require.IsType(t, messages.CancelPanelPreviewMsg{}, msgs[2])
}

func TestSettingsPanelPreviewRestoredAfterRoundTrip(t *testing.T) {
	t.Parallel()
	for _, apply := range []bool{false, true} {
		d := newTestSettingsDialog(t, messages.LayoutSettings{})
		d.tab = tabPanel
		d.togglePanel()
		d.togglePanel()
		assert.True(t, d.current.Equal(d.original))
		cmd := d.cancel()
		if apply {
			cmd = d.apply()
		}
		msgs := collectMsgs(cmd)
		require.Len(t, msgs, 2, "clear the root preview baseline even when edits return to the original")
		require.IsType(t, messages.CancelPanelPreviewMsg{}, msgs[1])
	}
}

func TestSettingsPanelMouseReorderAndBounds(t *testing.T) {
	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.tab = tabPanel
	d.SetSize(40, 20)
	found := false
	for _, hit := range d.rowHits {
		if hit.kind != settingsMovePanel {
			continue
		}
		assert.False(t, hit.row == 0 && hit.delta == -1, "first row cannot move up")
		assert.False(t, hit.row == len(d.panelOrder)-1 && hit.delta == 1, "last row cannot move down")
		if hit.row == 0 && hit.delta == 1 {
			msgs := collectMsgs(clickSettingsHit(t, d, hit))
			assert.True(t, hasMsg[messages.PreviewPanelMsg](msgs))
			assert.Equal(t, messages.PanelWorkspace, d.panelOrder[1])
			assert.Equal(t, 1, d.selected[tabPanel])
			found = true
		}
	}
	require.True(t, found)
}
