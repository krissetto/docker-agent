package statusbar

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/core"
)

type footerHelp []key.Binding

func (h footerHelp) Help() help.KeyMap { return core.NewSimpleHelp(h) }

func TestFooterFitsWholePrimaryHints(t *testing.T) {
	hints := footerHelp{
		key.NewBinding(key.WithKeys("ctrl+h"), key.WithHelp("Ctrl+h", "help")),
		key.NewBinding(key.WithKeys("ctrl+k"), key.WithHelp("Ctrl+k", "commands")),
		key.NewBinding(key.WithKeys("shift+enter"), key.WithHelp("Shift+Enter", "newline")),
		key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("Ctrl+c", "quit")),
	}
	s := New(hints, WithTitle("agent 界"))
	for _, width := range []int{30, 48, 80, 120, 48} {
		s.SetWidth(width)
		view := s.View()
		require.Equal(t, width, ansi.StringWidth(view))
		require.NotContains(t, view, "\n")
		plain := ansi.Strip(view)
		require.Contains(t, plain, "Ctrl+h help")
		require.NotContains(t, plain, "...")
		require.True(t, strings.HasSuffix(plain, "agent 界 "))
		for _, hint := range hints {
			if strings.Contains(plain, hint.Help().Key) {
				require.Contains(t, plain, hint.Help().Key+" "+hint.Help().Desc)
			}
		}
	}
	s.SetWidth(80)
	require.Contains(t, ansi.Strip(s.View()), "Shift+Enter newline")
	require.Contains(t, ansi.Strip(s.View()), "Ctrl+c quit")
}

func TestFooterNewTabHitboxFollowsFittedHints(t *testing.T) {
	s := New(footerHelp{key.NewBinding(key.WithKeys("f1"), key.WithHelp("F1", "help"))}, WithTitle("agent 界"))
	s.SetShowNewTab(true)
	for _, width := range []int{40, 80, 40} {
		s.SetWidth(width)
		plain := ansi.Strip(s.View())
		require.Equal(t, width, ansi.StringWidth(plain))
		buttonIndex := strings.Index(plain, " │ ")
		require.NotEqual(t, -1, buttonIndex, "rendered footer has a new-tab button")
		start := ansi.StringWidth(plain[:buttonIndex])
		end := start + ansi.StringWidth(" │ + new tab")
		for x := range width {
			require.Equal(t, x >= start && x < end, s.ClickedNewTab(x), "width=%d x=%d", width, x)
		}
	}
	s.SetShowNewTab(false)
	_ = s.View()
	for x := range 40 {
		require.False(t, s.ClickedNewTab(x))
	}
}
