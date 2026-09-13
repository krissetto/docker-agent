package tabbar

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestVisibleTabbarNewTabButtonSpawnsOnLeftClick(t *testing.T) {
	for _, width := range []int{8, 30, 80} {
		tb := New(newMotionRuntime(), 8)
		tb.SetWidth(width)
		tb.SetTabs(motionTabs(3, 0), 0)
		plain := ansi.Strip(tb.View())
		plus := strings.Index(plain, "+")
		require.NotEqual(t, -1, plus, "visible strip retains its new-tab button")
		x := ansi.StringWidth(plain[:plus])
		require.Equal(t, width, ansi.StringWidth(plain))
		msgs := commandMessages(tb.Update(tea.MouseClickMsg{X: x, Button: tea.MouseLeft}))
		require.Equal(t, []tea.Msg{messages.SpawnSessionMsg{}}, msgs)
		require.False(t, tb.IsDragging(), "new-tab button does not initiate a tab drag")
		require.Zero(t, tb.activeIdx, "spawning is routed to the root, not an active-tab mutation")
		require.Empty(t, commandMessages(tb.Update(tea.MouseReleaseMsg{X: x, Button: tea.MouseLeft})), "release must not spawn twice")
		require.Empty(t, commandMessages(tb.Update(tea.MouseClickMsg{X: width, Button: tea.MouseLeft})))
		tb.StopAnimations()
	}
}

func TestHiddenTabbarNewTabButtonRejectsStaleClicks(t *testing.T) {
	for _, single := range []bool{false, true} {
		tb := New(newMotionRuntime(), 8)
		tb.SetWidth(80)
		tb.SetTabs(motionTabs(3, 0), 0)
		plain := ansi.Strip(tb.View())
		plus := strings.Index(plain, "+")
		require.GreaterOrEqual(t, plus, 0, "visible strip retains its new-tab button")
		x := ansi.StringWidth(plain[:plus])
		if single {
			tb.SetTabs(motionTabs(1, 0), 0)
		} else {
			tb.SetVisible(false)
		}
		require.Zero(t, tb.Height())
		require.Empty(t, commandMessages(tb.Update(tea.MouseClickMsg{X: x, Button: tea.MouseLeft})))
		require.Empty(t, tb.View())
		require.Empty(t, tb.zones)
		// Hiding a pointer affordance must not disable its keyboard command.
		require.Equal(t, []tea.Msg{messages.SpawnSessionMsg{}}, commandMessages(tb.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})))
		tb.StopAnimations()
	}
}
