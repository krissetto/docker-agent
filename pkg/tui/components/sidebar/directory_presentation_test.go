package sidebar

import (
	"image/color"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestDirectoryPaletteAndAnimatedRevealAcrossThemes(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	refs, err := styles.ListThemeRefs()
	require.NoError(t, err)
	for _, ref := range refs {
		if !styles.IsBuiltinTheme(ref) {
			continue
		}
		t.Run(ref, func(t *testing.T) {
			theme, err := styles.LoadTheme(ref)
			require.NoError(t, err)
			styles.ApplyTheme(theme)
			m := newPlacementSidebar(t, false)
			width := m.contentWidth(m.cachedNeedsScrollbar)
			y := m.workingDirRow
			x := m.layoutCfg.PaddingLeft
			iconColor := func(col int) color.Color {
				return sidebarCells(strings.Split(m.View(), "\n")[y])[x+col].fg
			}
			sameColor := func(expected, actual color.Color) {
				t.Helper()
				assert.Equal(t, color.NRGBAModel.Convert(expected), color.NRGBAModel.Convert(actual))
			}
			initial := m.View()
			assert.Equal(t, " ", sidebarCells(m.directoryRow(width))[width-5].glyph)
			assert.Equal(t, " ", sidebarCells(m.directoryRow(width))[width-2].glyph)
			_, cmd := m.Update(tea.MouseMotionMsg{X: x, Y: y})
			cmd = advancePlacement(t, m, cmd)
			progress := animation.HoverStep(0, 1, 50*time.Millisecond)
			assert.InDelta(t, progress, m.hoverValues["directory"].value, 1e-9)
			first := m.View()
			assert.NotEqual(t, initial, first, "owner ticks change painted icon colors")
			midpoint := iconColor(width - 2)
			assert.NotEqual(t, color.NRGBAModel.Convert(styles.Background), color.NRGBAModel.Convert(midpoint))
			assert.NotEqual(t, color.NRGBAModel.Convert(styles.MutedStyle.GetForeground()), color.NRGBAModel.Convert(midpoint))

			// Reverse before reveal completes, without replacing the owner's pending tick.
			m.ClearSubagentHover()
			assert.InDelta(t, progress, m.hoverValues["directory"].value, 1e-9)
			cmd = advancePlacement(t, m, cmd)
			assert.Less(t, m.hoverValues["directory"].value, progress)
			assert.NotEqual(t, first, m.View())
			settlePlacement(t, m, cmd)
			assert.Equal(t, " ", sidebarCells(m.directoryRow(width))[width-5].glyph)
			assert.Equal(t, " ", sidebarCells(m.directoryRow(width))[width-2].glyph)

			_, cmd = m.Update(tea.MouseMotionMsg{X: x, Y: y})
			settlePlacement(t, m, cmd)
			sameColor(styles.Brighten(styles.TextPrimary, .25), iconColor(width-5))
			sameColor(styles.Brighten(styles.MutedStyle.GetForeground(), .25), iconColor(width-2))
			line := ansi.Strip(m.directoryRow(width))
			assert.True(t, strings.HasSuffix(line, directoryCopyIcon+"  "+directoryIcon+" "))
			for col := width - 5; col < width; col++ {
				action, _ := m.HandleClickType(x+col, y)
				expected := ClickWorkingDir
				if col == width-2 {
					expected = ClickOpenWorkingDir
				}
				if col == width-1 {
					expected = ClickNone
				}
				assert.Equal(t, expected, action, "column %d", col)
			}
			_, cmd = m.Update(tea.MouseMotionMsg{X: x + width - 2, Y: y})
			settlePlacement(t, m, cmd)
			sameColor(styles.Brighten(styles.MutedStyle.GetForeground(), .25), iconColor(width-5))
			sameColor(styles.Brighten(styles.TextPrimary, .25), iconColor(width-2))
			settled := m.View()
			cmd = advancePlacement(t, m, m.ClearSubagentHover())
			assert.NotEqual(t, settled, m.View(), "exit follows the shared highlight before removing the glyphs")
			require.Greater(t, m.hoverValues["directory"].value, 0.0)
			settlePlacement(t, m, cmd)
			assert.Equal(t, " ", sidebarCells(m.directoryRow(width))[width-5].glyph)
			assert.Equal(t, " ", sidebarCells(m.directoryRow(width))[width-2].glyph)
			assert.Zero(t, m.ar.ActiveCount())
		})
	}
}

func TestDirectoryHideDuringRevealReleasesMotion(t *testing.T) {
	t.Parallel()
	m := newPlacementSidebar(t, false)
	cmd := advancePlacement(t, m, m.setHoverTarget("directory"))
	require.Greater(t, m.hoverValues["directory"].value, 0.0)
	m.SetPresentationActive(false)
	assert.Empty(t, m.hoverValues)
	assert.Zero(t, m.ar.ActiveCount())
	_, accepted := m.ar.Accept(placementTickMessage(t, cmd))
	assert.False(t, accepted, "hidden page rejects its outstanding tick")
	require.Nil(t, m.setHoverTarget("directory"))
}
