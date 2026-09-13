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

func quitHelp() footerHelp {
	return footerHelp{key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("Ctrl+c", "quit"))}
}

func TestFooterOnlyQuitShortcutAndTitle(t *testing.T) {
	hints := footerHelp{
		key.NewBinding(key.WithKeys("ctrl+h"), key.WithHelp("Ctrl+h", "help")),
		key.NewBinding(key.WithKeys("ctrl+k"), key.WithHelp("Ctrl+k", "commands")),
		key.NewBinding(key.WithKeys("shift+enter"), key.WithHelp("Shift+Enter", "newline")),
		quitHelp()[0],
	}
	const title = "docker agent v1.2.3"
	s := New(hints, WithTitle(title))
	for _, width := range []int{33, 48, 80, 120, 48} {
		s.SetWidth(width)
		view := s.View()
		require.Equal(t, width, ansi.StringWidth(view))
		require.Equal(t, " Ctrl+c quit"+strings.Repeat(" ", width-13-len(title))+title+" ", ansi.Strip(view))
		require.Equal(t, view, s.View(), "cached footer retains its layout")
		require.Equal(t, 1, s.Height())
	}
	for _, hint := range hints {
		require.True(t, hint.Enabled(), "footer filtering must not disable any shortcuts")
	}
}

func TestFooterNewTabHitboxTracksVisibleButton(t *testing.T) {
	const title = "docker agent v1.2.3"
	const button = " │ + new tab"
	s := New(quitHelp(), WithTitle(title))
	for _, show := range []bool{true, false, true} {
		s.SetShowNewTab(show)
		for _, width := range []int{80, 47, 46, 45, 20, 2, 0, 60} {
			s.SetWidth(width)
			// Hit-testing must refresh stale geometry even before the next View.
			visible := show && width >= 2+len("Ctrl+c quit")+1+ansi.StringWidth(button)+2+len(title)
			start := width - 1 - len(title) - 2 - ansi.StringWidth(button)
			for x := -1; x <= width; x++ {
				require.Equal(t, visible && x >= start && x < start+ansi.StringWidth(button), s.ClickedNewTab(x), "show=%v width=%d x=%d", show, width, x)
			}
			view := ansi.Strip(s.View())
			require.Equal(t, width, ansi.StringWidth(view))
			require.Equal(t, visible, strings.Contains(view, button))
		}
	}
}

func TestFooterNarrowWidthsPreserveWholeQuitAndMargins(t *testing.T) {
	for _, title := range []string{"docker agent", "代理 v1.2.3", ""} {
		s := New(quitHelp(), WithTitle(title))
		s.SetShowNewTab(true)
		for width := -1; width <= 60; width++ {
			s.SetWidth(width)
			view := ansi.Strip(s.View())
			require.Equal(t, max(width, 0), ansi.StringWidth(view), "width=%d title=%q", width, title)
			if width >= 2+len("Ctrl+c quit") {
				require.True(t, strings.HasPrefix(view, " Ctrl+c quit"), "width=%d view=%q", width, view)
			} else {
				require.NotContains(t, view, "Ctrl")
			}
			if width > 0 {
				require.True(t, strings.HasPrefix(view, " "))
				require.True(t, strings.HasSuffix(view, " "))
			}
		}
	}
}

func TestFooterHelpChangesPreserveTitleAndClearQuit(t *testing.T) {
	s := New(quitHelp())
	s.SetWidth(60)
	require.Contains(t, ansi.Strip(s.View()), "Ctrl+c quit")
	disabled := quitHelp()[0]
	disabled.SetEnabled(false)
	for _, hints := range []core.KeyMapHelp{footerHelp{disabled}, footerHelp{}, nil} {
		s.SetHelp(hints)
		require.Equal(t, "docker agent", strings.TrimSpace(ansi.Strip(s.View())))
	}
	s.SetHelp(quitHelp())
	require.Contains(t, ansi.Strip(s.View()), "Ctrl+c quit")
}

func TestFooterThemeChangesKeepSemanticStylesAndGeometry(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates style globals.
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	s := New(quitHelp(), WithTitle("docker agent v1.2.3"))
	s.SetShowNewTab(true)
	s.SetWidth(80)
	var plain string
	for _, ref := range []string{styles.DefaultThemeRef, styles.DefaultLightThemeRef, styles.DefaultThemeRef} {
		theme, err := styles.LoadTheme(ref)
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		s.InvalidateCache()
		view := s.View()
		require.Contains(t, view, styles.HighlightWhiteStyle.Render("Ctrl+c"))
		require.Contains(t, view, styles.SecondaryStyle.Render("quit"))
		require.Contains(t, view, styles.MutedStyle.Render(s.title))
		require.Contains(t, view, styles.HighlightWhiteStyle.Render("+"))
		require.Equal(t, 80, ansi.StringWidth(view))
		if plain != "" {
			require.Equal(t, plain, ansi.Strip(view))
		}
		plain = ansi.Strip(view)
		require.True(t, s.ClickedNewTab(s.newTabStartX+3))
	}
}
