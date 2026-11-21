package tabbar

import (
	"image/color"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestSingleTabFitsAndKeepsPlusReachable(t *testing.T) {
	t.Parallel()
	bar := New(animation.NewRuntime(), 20)
	require.Zero(t, bar.Height())
	bar.SetTabs([]messages.TabInfo{{SessionID: "root", Title: "界 Wide Session", IsActive: true}}, 0)
	require.Equal(t, 1, bar.Height())
	for _, width := range []int{80, 11, 5, 3, 2, 1, 0} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			bar.SetWidth(width)
			view := ansi.Strip(bar.View())
			require.LessOrEqual(t, ansi.StringWidth(view), width)
			require.Nil(t, bar.Update(tea.MouseClickMsg{X: width, Button: tea.MouseLeft}))
			if width == 0 {
				require.Empty(t, view)
				return
			}
			plus := strings.Index(view, "+")
			require.GreaterOrEqual(t, plus, 0)
			x := ansi.StringWidth(view[:plus])
			cmd := bar.Update(tea.MouseClickMsg{X: x, Button: tea.MouseLeft})
			require.True(t, hasSpawn(cmd))
			bar.CancelHover()
			if width == 11 {
				require.NotContains(t, view, "×")
			}
		})
	}
}

func hasSpawn(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	switch msg := cmd().(type) {
	case messages.SpawnSessionMsg:
		return true
	case tea.BatchMsg:
		// Animation tick commands may sleep; they need not be run to check
		// a click. The spawn command is appended after the animations.
		return hasSpawn(msg[len(msg)-1])
	default:
		return false
	}
}

func TestTabActivityUsesEditorSurfaceOnlyForActiveTab(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	for _, ref := range []string{"default", "default-light", "custom"} {
		t.Run(ref, func(t *testing.T) {
			theme := styles.DefaultTheme()
			if ref == "custom" {
				theme.Colors.Background = "#102030"
				theme.Colors.EditorBg = "#304050"
			} else {
				var err error
				theme, err = styles.LoadTheme(ref)
				require.NoError(t, err)
			}
			styles.ApplyTheme(theme)
			active := renderTab(messages.TabInfo{Title: "Session", IsActive: true}, 20, dragRoleNone, 0, 0)
			inactive := renderTab(messages.TabInfo{Title: "Session"}, 20, dragRoleNone, 0, 0)
			require.Equal(t, ansi.Strip(active.View()), ansi.Strip(inactive.View()))
			a := uv.NewStyledString(active.View()).Lines(ansi.GraphemeWidth)[0][2]
			i := uv.NewStyledString(inactive.View()).Lines(ansi.GraphemeWidth)[0][2]
			require.Equal(t, color.NRGBAModel.Convert(styles.EditorBg), color.NRGBAModel.Convert(a.Style.Bg))
			require.Equal(t, color.NRGBAModel.Convert(styles.Background), color.NRGBAModel.Convert(i.Style.Bg))
			require.NotEqual(t, a.Style.Bg, i.Style.Bg)
			require.NotEqual(t, a.Style.Attrs, i.Style.Attrs)
		})
	}
}
