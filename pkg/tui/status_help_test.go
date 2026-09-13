package tui

import (
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestPrimaryHelpKeepsCompleteKeyboardHelp(t *testing.T) {
	m, _ := newTestModel(t)
	m.focusedPanel = PanelEditor
	for _, enhanced := range []bool{false, true} {
		m.keyboardEnhancementsSupported = enhanced
		bindings := m.Bindings()
		require.Len(t, bindings, 1)
		require.Equal(t, "commands", bindings[0].Help().Desc)
		require.Equal(t, []string{"ctrl+k"}, bindings[0].Keys())
		nl := "ctrl+j"
		if enhanced {
			nl = "shift+enter"
		}
		var fullKeys []string
		for _, binding := range m.AllBindings() {
			fullKeys = append(fullKeys, binding.Keys()...)
		}
		require.Equal(t, []string{
			"ctrl+c", "tab", "ctrl+t", "ctrl+w", "ctrl+p", "ctrl+n",
			"ctrl+k", "ctrl+h", "f1", "ctrl+?", "ctrl+y", "ctrl+o", "ctrl+s", "ctrl+m", "ctrl+z",
			"shift+tab", "ctrl+b", nl, "ctrl+g", "ctrl+r",
		}, fullKeys, "complete keyboard shortcut inventory is unchanged")

		_, cmd := m.Update(tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl})
		open, ok := firstOfType[dialog.OpenDialogMsg](collectMsgs(cmd))
		require.True(t, ok, "primary help shortcut opens the help dialog")
		expected := dialog.NewHelpDialog(m.AllBindings())
		open.Model.SetSize(160, 100)
		expected.SetSize(160, 100)
		require.Equal(t, expected.View(), open.Model.View(), "keyboard help uses full bindings, not footer subset")
		require.Contains(t, ansi.Strip(open.Model.View()), "new/close tab")
		require.Contains(t, ansi.Strip(open.Model.View()), "history search")
	}
	m.leanMode = true
	require.Len(t, m.Bindings(), 1)
	require.Equal(t, "quit", m.Bindings()[0].Help().Desc)
}

func TestHiddenFooterShortcutsStillDispatch(t *testing.T) {
	m, _, _ := wallClockRoot(t, 120, 40)
	_, _ = m.Update(messages.TabsUpdatedMsg{Tabs: layoutTabs(), ActiveIdx: 0})
	m.focusedPanel = PanelContent
	cases := []struct {
		code rune
		want tea.Msg
	}{
		{'t', messages.SpawnSessionMsg{}},
		{'w', messages.CloseTabMsg{SessionID: layoutTabs()[0].SessionID}},
		{'p', messages.SwitchTabMsg{SessionID: layoutTabs()[1].SessionID}},
		{'n', messages.SwitchTabMsg{SessionID: layoutTabs()[1].SessionID}},
		{'y', messages.ToggleYoloMsg{}},
		{'o', messages.ToggleHideToolResultsMsg{}},
	}
	for _, tc := range cases {
		msg := tea.KeyPressMsg{Code: tc.code, Mod: tea.ModCtrl}
		for _, hint := range m.Bindings() {
			require.False(t, key.Matches(msg, hint), "shortcut deliberately absent from primary footer")
		}
		_, cmd := m.Update(msg)
		require.Contains(t, collectMsgs(cmd), tc.want, "shortcut %s still dispatches", msg.String())
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})
	_, ok := firstOfType[dialog.OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok, "commands shortcut still opens palette")
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, PanelEditor, m.focusedPanel, "hidden focus shortcut still works")

	for _, enhanced := range []bool{false, true} {
		if enhanced {
			_, _ = m.Update(tea.KeyboardEnhancementsMsg{Flags: 1})
		}
		m.editor.SetValue("first")
		msg := tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl}
		if enhanced {
			msg = tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift}
		}
		for _, hint := range m.Bindings() {
			require.False(t, key.Matches(msg, hint), "newline is absent from the command-only footer")
		}
		var newline key.Binding
		for _, binding := range m.AllBindings() {
			if binding.Help().Desc == "newline" {
				newline = binding
			}
		}
		require.True(t, key.Matches(msg, newline), "complete help retains the dispatched newline key")
		_, _ = m.Update(msg)
		require.Equal(t, "first\n", m.editor.Value())
	}
}
