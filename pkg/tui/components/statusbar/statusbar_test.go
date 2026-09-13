package statusbar

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type footerHelp []key.Binding

func (h footerHelp) Help() help.KeyMap { return core.NewSimpleHelp(h) }

func TestFooterOnlyCommandShortcut(t *testing.T) {
	hints := footerHelp{
		key.NewBinding(key.WithKeys("ctrl+h"), key.WithHelp("Ctrl+h", "help")),
		key.NewBinding(key.WithKeys("ctrl+k"), key.WithHelp("Ctrl+k", "commands")),
		key.NewBinding(key.WithKeys("shift+enter"), key.WithHelp("Shift+Enter", "newline")),
		key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("Ctrl+c", "quit")),
	}
	s := New(hints)
	for _, width := range []int{30, 48, 80, 120, 48} {
		s.SetWidth(width)
		view := s.View()
		require.Equal(t, width, ansi.StringWidth(view))
		require.Equal(t, "  Ctrl+k"+strings.Repeat(" ", width-8), ansi.Strip(view))
		require.Equal(t, 1, s.Height())
	}
	for _, hint := range hints {
		require.True(t, hint.Enabled(), "footer filtering must not disable any shortcuts")
	}
}

func TestFooterCommandHitboxTracksVisibleLabel(t *testing.T) {
	s := New(footerHelp{key.NewBinding(key.WithKeys("ctrl+k"), key.WithHelp("Ctrl+k", "commands"))})
	for _, width := range []int{80, 10, 9, 4, 0, 40} {
		s.SetWidth(width)
		for x := -1; x <= width; x++ {
			visible := width >= 2*styles.EditorHMargin+6
			require.Equal(t, visible && x >= styles.EditorHMargin && x < styles.EditorHMargin+6, s.ClickedCommands(x), "width=%d x=%d", width, x)
		}
		plain := ansi.Strip(s.View())
		require.Equal(t, width, ansi.StringWidth(plain))
		if width < 10 {
			require.Equal(t, strings.Repeat(" ", width), plain, "narrow footers never show partial labels or invisible targets")
		}
	}
}

func TestFooterHelpChangesClearObsoleteTargets(t *testing.T) {
	binding := key.NewBinding(key.WithKeys("f2"), key.WithHelp("F2", "commands"))
	s := New(footerHelp{binding})
	s.SetWidth(30)
	require.Equal(t, "F2", strings.TrimSpace(ansi.Strip(s.View())))
	require.True(t, s.ClickedCommands(2))
	require.False(t, s.ClickedCommands(4))
	binding.SetEnabled(false)
	for _, hints := range []core.KeyMapHelp{footerHelp{binding}, footerHelp{}, nil} {
		s.SetHelp(hints)
		require.False(t, s.ClickedCommands(2))
		require.Empty(t, strings.TrimSpace(ansi.Strip(s.View())))
	}
}
