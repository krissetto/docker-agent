package chat

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	tuibanner "github.com/docker/docker-agent/pkg/tui/banner"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestWhaleBannerThemeSwitch(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	p.SetSize(100, 30)
	sl := p.computeSidebarLayout()

	var previous string
	for _, ref := range []string{"default", "default-light", "nord"} {
		theme, err := styles.LoadTheme(ref)
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		got := p.messagesView(sl)
		want := lipgloss.Place(sl.chatWidth, sl.chatHeight, lipgloss.Center, lipgloss.Center,
			styles.MutedStyle.Render(strings.Join(tuibanner.Lines, "\n")))
		require.Equal(t, want, got, "banner uses the current theme's muted foreground, without an explicit surface")
		require.NotEqual(t, previous, got, "live theme switches must not retain stale banner colors")
		require.Equal(t, got, p.messagesView(sl), "static artwork needs no animation updates")
		previous = got
	}
}

func TestWhaleBannerResizeAndSidebarLayouts(t *testing.T) {
	t.Parallel()
	for _, position := range []msgtypes.SidebarPosition{msgtypes.SidebarRight, msgtypes.SidebarLeft, msgtypes.SidebarTop, msgtypes.SidebarBottom} {
		t.Run(string(position), func(t *testing.T) {
			p := newLayoutTestPage(t, position)
			for _, size := range [][2]int{{160, 40}, {12, 5}, {80, 24}, {24, 8}, {160, 40}} {
				p.SetSize(size[0], size[1])
				sl := p.computeSidebarLayout()
				got := p.messagesView(sl)
				if sl.chatWidth < tuibanner.Width || sl.chatHeight < tuibanner.Height {
					require.Empty(t, got, "small panes hide rather than wrap or crop the logo")
					continue
				}
				require.Contains(t, ansi.Strip(got), tuibanner.Lines[0])
				require.Len(t, strings.Split(got, "\n"), sl.chatHeight)
				for line := range strings.SplitSeq(got, "\n") {
					require.Equal(t, sl.chatWidth, ansi.StringWidth(line), "logo remains within the transcript pane")
				}
			}
		})
	}
}

func TestWhaleBannerNeverDecoratesPopulatedTranscript(t *testing.T) {
	t.Parallel()
	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	p.SetSize(160, 40)
	require.Contains(t, ansi.Strip(p.messagesView(p.computeSidebarLayout())), tuibanner.Lines[0])
	p.messages.AddUserMessage("Read this conversation, not a watermark")
	p.messages.AppendToLastMessage("root", strings.Repeat("Transcript row\n", 80)+"Last reachable row")

	for _, size := range [][2]int{{160, 40}, {40, 12}, {160, 40}} {
		p.SetSize(size[0], size[1])
		got := p.messagesView(p.computeSidebarLayout())
		require.NotEmpty(t, got)
		require.Equal(t, p.messages.View(), got, "populated transcript bytes and scroll/click geometry stay untouched")
		require.NotContains(t, ansi.Strip(got), tuibanner.Lines[0])
	}
}

func TestWhaleBannerExactFit(t *testing.T) {
	t.Parallel()
	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	p.SetSize(100, 30)
	for _, size := range [][2]int{
		{tuibanner.Width, tuibanner.Height},
		{tuibanner.Width - 1, tuibanner.Height},
		{tuibanner.Width, tuibanner.Height - 1},
		{tuibanner.Width, tuibanner.Height},
	} {
		got := p.messagesView(sidebarLayout{chatWidth: size[0], chatHeight: size[1]})
		if size[0] < tuibanner.Width || size[1] < tuibanner.Height {
			require.Empty(t, got)
			continue
		}
		rows := strings.Split(ansi.Strip(got), "\n")
		require.Len(t, rows, tuibanner.Height)
		for i, row := range rows {
			require.Equal(t, tuibanner.Width, ansi.StringWidth(row))
			require.Equal(t, tuibanner.Lines[i], strings.TrimRight(row, " "))
		}
	}
}
