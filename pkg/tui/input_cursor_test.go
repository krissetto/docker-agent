package tui

import (
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/history"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/widgets/cursor"
)

func TestInputCursorWakesWithoutReplacingTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root, _ := newTestModel(t)
		composer := editor.New(&history.History{})
		root.editor, root.focusedPanel = composer, PanelEditor
		composer.SetSize(80, 5)
		composer.SetValue("draft")
		pending := root.syncInputCursor()
		shown := composer.View()
		_, pending = root.Update(inputCursorTick(t, pending))
		require.NotEqual(t, shown, composer.View())

		<-time.After(200 * time.Millisecond)
		root.Update(tea.KeyPressMsg{Code: tea.KeyRight}) // no movement at end of draft
		require.Equal(t, "draft", composer.Value())
		require.Equal(t, shown, composer.View())
		woke := time.Now()

		root.viewCacheValid = true
		_, pending = root.Update(inputCursorTick(t, pending))
		require.Equal(t, 300*time.Millisecond, time.Since(woke))
		require.True(t, root.viewCacheValid, "early tick must not invalidate the frame")
		_, pending = root.Update(inputCursorTick(t, pending))
		require.Equal(t, 500*time.Millisecond, time.Since(woke))
		require.False(t, root.viewCacheValid)
		require.NotEqual(t, shown, composer.View())

		root.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
		require.Equal(t, "draft\n", composer.Value())
		shown = composer.View()
		root.Update(inputCursorTick(t, pending))
		require.NotEqual(t, shown, composer.View(), "Ctrl+J keeps the cursor blinking")
	})
}

func TestInputCursorOnlyAdvancesExposedInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root, _ := newTestModel(t)
		composer := editor.New(&history.History{})
		root.editor, root.focusedPanel = composer, PanelEditor
		composer.SetSize(80, 5)
		composer.SetValue("draft")
		composerPending := root.syncInputCursor()

		d := dialog.NewPendingMessageEditDialog("session", "turn", "pending", 1, nil)
		root.dialogMgr.Update(tea.WindowSizeMsg{Width: 100, Height: 35})
		_, open := root.Update(dialog.OpenDialogMsg{Model: d})
		require.Same(t, cursor.Of(d), root.exposedInput)
		root.viewCacheValid = true
		_, stale := root.Update(inputCursorTick(t, composerPending))
		require.Nil(t, stale)
		require.True(t, root.viewCacheValid)
		require.Equal(t, "draft", composer.Value())

		shown := d.View()
		_, pending := root.Update(inputCursorTick(t, open))
		// Animation commands batch with the first blink; process them only to
		// obtain the blink command, without advancing any dialog animation.
		require.NotEqual(t, shown, d.View())
		root.Update(tea.BlurMsg{})
		require.Nil(t, root.exposedInput)
		root.viewCacheValid = true
		_, stale = root.Update(inputCursorTick(t, pending))
		require.Nil(t, stale)
		require.True(t, root.viewCacheValid)
		_, focus := root.Update(tea.FocusMsg{})
		require.Same(t, cursor.Of(d), root.exposedInput)
		require.Equal(t, shown, d.View(), "refocus starts a fresh visible period")
		root.Update(inputCursorTick(t, focus))
		require.NotEqual(t, shown, d.View())
	})
}

func inputCursorTick(t *testing.T, cmd tea.Cmd) cursor.BlinkMsg {
	t.Helper()
	for _, msg := range collectMsgs(cmd) {
		if tick, ok := msg.(cursor.BlinkMsg); ok {
			return tick
		}
	}
	t.Fatal("no cursor blink command")
	return cursor.BlinkMsg{}
}
