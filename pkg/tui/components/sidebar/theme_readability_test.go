package sidebar

import (
	"image/color"
	"math"
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/scrollbar"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestOffThinkingTextUsesReadableMutedSemanticStyle(t *testing.T) { //nolint:paralleltest // theme globals
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	styles.ApplyTheme(styles.DefaultTheme())

	m := newAgentPanelSidebar(t, 56, runtime.AgentDetails{Name: "root", Provider: "openai", Model: "model", Thinking: "off"})
	panel := m.agentInfo(m.contentWidth(false))
	var found bool
	for _, line := range strings.Split(panel, "\n") {
		plain := ansi.Strip(line)
		at := strings.Index(plain, "off")
		if at < 0 {
			continue
		}
		found = true
		assert.False(t, sidebarFaintAt(line, ansi.StringWidth(plain[:at])), "off remains readable; don't apply terminal faint over muted semantic color")
		cells := sidebarCells(line)
		cellX := ansi.StringWidth(plain[:at])
		require.Less(t, cellX, len(cells))
		assertColorEqual(t, styles.MutedStyle.GetForeground(), cells[cellX].fg)
	}
	require.True(t, found, "rendered sidebar card contains the off value")
}

func TestRenderedScrollbarSemanticColorsAcrossBuiltinThemes(t *testing.T) { //nolint:paralleltest // theme globals
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })

	refs, err := styles.ListThemeRefs()
	require.NoError(t, err)
	var builtins []string
	for _, ref := range refs {
		if styles.IsBuiltinTheme(ref) {
			builtins = append(builtins, ref)
		}
	}
	require.NotEmpty(t, builtins)
	for _, ref := range builtins {
		t.Run(ref, func(t *testing.T) {
			theme, err := styles.LoadTheme(ref)
			require.NoError(t, err)
			styles.ApplyTheme(theme)

			for name, style := range map[string]lipgloss.Style{
				"track": styles.TrackStyle, "thumb": styles.ThumbStyle, "active thumb": styles.ThumbActiveStyle,
			} {
				assert.NotEqual(t, lipgloss.NoColor{}, style.GetForeground(), "%s has a semantic foreground", name)
				assert.Equal(t, lipgloss.NoColor{}, style.GetBackground(), "%s inherits the parent surface", name)
			}

			sb := scrollbar.New()
			sb.SetDimensions(5, 30)
			sb.SetPosition(7, 11)
			idleLines := sb.ViewLines()
			require.Len(t, idleLines, 5)
			idleCells := renderedScrollbarCells(t, idleLines)
			thumbRow := -1
			trackRows := 0
			for row, cell := range idleCells {
				assert.Nil(t, cell.bg, "rendered track/thumb on row %d inherits the parent surface", row)
				if sameColor(cell.fg, styles.ThumbStyle.GetForeground()) {
					thumbRow = row
					assertColorEqual(t, styles.ThumbStyle.GetForeground(), cell.fg)
				} else {
					trackRows++
					assertColorEqual(t, styles.TrackStyle.GetForeground(), cell.fg)
				}
			}
			require.GreaterOrEqual(t, thumbRow, 0, "actual rendered frame contains a usable thumb")
			require.Positive(t, trackRows, "actual rendered frame contains track cells")
			if ref == "catppuccin-latte" {
				assert.Equal(t, "#9ca0b0", theme.Colors.ScrollbarTrack, "Latte retains its established visible track color")
			}
			assert.GreaterOrEqual(t, minSurfaceContrast(idleCells[thumbRow].fg, theme.Colors.ShellBg, theme.Colors.CardBg, theme.Colors.EditorBg), 3.0,
				"idle thumb remains perceivable on shell/card/editor surfaces")

			clicked, _ := sb.Update(tea.MouseClickMsg{X: 7, Y: 11 + thumbRow, Button: tea.MouseLeft})
			sb = clicked
			require.True(t, sb.IsDragging(), "clicking rendered thumb starts drag capture")
			activeLines := sb.ViewLines()
			activeCells := renderedScrollbarCells(t, activeLines)
			activeRow := -1
			for row, cell := range activeCells {
				assert.Nil(t, cell.bg, "active rendered row %d inherits parent background", row)
				if sameColor(cell.fg, styles.ThumbActiveStyle.GetForeground()) {
					activeRow = row
					assertColorEqual(t, styles.ThumbActiveStyle.GetForeground(), cell.fg)
				}
			}
			require.GreaterOrEqual(t, activeRow, 0, "actual drag frame contains the active thumb")
			assert.NotEqual(t, idleLines, activeLines, "rendered active thumb differs while dragging")
			assert.GreaterOrEqual(t, minSurfaceContrast(activeCells[activeRow].fg, theme.Colors.ShellBg, theme.Colors.CardBg, theme.Colors.EditorBg), 3.0,
				"active thumb remains perceivable on shell/card/editor surfaces")

			sb, _ = sb.Update(tea.MouseMotionMsg{X: -20, Y: 11 + thumbRow, Button: tea.MouseLeft})
			require.True(t, sb.IsDragging(), "outside motion retains pointer capture")
			sb, _ = sb.Update(tea.MouseReleaseMsg{X: -20, Y: 11 + thumbRow, Button: tea.MouseLeft})
			assert.False(t, sb.IsDragging(), "release ends capture")
			releasedLines := sb.ViewLines()
			assert.Equal(t, idleLines, releasedLines, "release restores exact rendered idle frame")
		})
	}
}

func renderedScrollbarCells(t *testing.T, lines []string) []sidebarCell {
	t.Helper()
	cells := make([]sidebarCell, len(lines))
	for i, line := range lines {
		parsed := sidebarCells(line)
		require.Len(t, parsed, 1, "scrollbar row is exactly one rendered cell")
		cells[i] = parsed[0]
	}
	return cells
}

func sameColor(a, b color.Color) bool {
	if a == nil || b == nil || a == (lipgloss.NoColor{}) || b == (lipgloss.NoColor{}) {
		return a == nil && b == nil || a == (lipgloss.NoColor{}) && b == (lipgloss.NoColor{})
	}
	return color.NRGBAModel.Convert(a) == color.NRGBAModel.Convert(b)
}

func assertColorEqual(t *testing.T, want, got color.Color) {
	t.Helper()
	require.NotNil(t, got)
	assert.Equal(t, color.NRGBAModel.Convert(want), color.NRGBAModel.Convert(got))
}

func minSurfaceContrast(fg color.Color, surfaces ...string) float64 {
	minRatio := math.Inf(1)
	for _, surface := range surfaces {
		bg := lipgloss.Color(surface)
		minRatio = math.Min(minRatio, colorContrast(fg, bg))
	}
	return minRatio
}

func colorContrast(a, b color.Color) float64 {
	lum := func(c color.Color) float64 {
		r, g, bl, _ := c.RGBA()
		component := func(v uint32) float64 {
			x := float64(v) / 65535
			if x <= 0.04045 {
				return x / 12.92
			}
			return math.Pow((x+0.055)/1.055, 2.4)
		}
		return .2126*component(r) + .7152*component(g) + .0722*component(bl)
	}
	l1, l2 := lum(a), lum(b)
	if l1 < l2 {
		l1, l2 = l2, l1
	}
	return (l1 + .05) / (l2 + .05)
}

func sidebarFaintAt(rendered string, target int) bool {
	p := ansi.GetParser()
	defer ansi.PutParser(p)
	var state byte
	faint := false
	for rendered != "" {
		seq, width, n, next := ansi.DecodeSequence(rendered, state, p)
		if n == 0 {
			break
		}
		if ansi.HasCsiPrefix(seq) && p.Command() == 'm' {
			params := p.Params()
			if len(params) == 0 {
				faint = false
			}
			for i := 0; i < len(params); i++ {
				code := params[i].Param(0)
				switch code {
				case 0, 22:
					faint = false
				case 2:
					faint = true
				case 38, 48, 58:
					var c color.Color
					if consumed := ansi.ReadStyleColor(params[i:], &c); consumed > 0 {
						i += consumed - 1
					}
				}
			}
		}
		if width > 0 {
			if target < width {
				return faint
			}
			target -= width
		}
		state, rendered = next, rendered[n:]
	}
	return false
}
