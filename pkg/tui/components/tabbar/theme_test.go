package tabbar

import (
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type themeCell struct {
	glyph  string
	fg, bg color.Color
}

func themeCells(line string) []themeCell {
	p := ansi.GetParser()
	defer ansi.PutParser(p)
	var fg, bg color.Color
	var state byte
	var cells []themeCell
	for line != "" {
		seq, width, n, next := ansi.DecodeSequence(line, state, p)
		if n == 0 {
			break
		}
		if ansi.HasCsiPrefix(seq) && p.Command() == 'm' {
			params := p.Params()
			if len(params) == 0 {
				fg, bg = nil, nil
			}
			for i := 0; i < len(params); i++ {
				switch param := params[i].Param(0); param {
				case 0:
					fg, bg = nil, nil
				case 39:
					fg = nil
				case 49:
					bg = nil
				case 38, 48, 58:
					var c color.Color
					if consumed := ansi.ReadStyleColor(params[i:], &c); consumed > 0 {
						if param == 38 {
							fg = c
						}
						if param == 48 {
							bg = c
						}
						i += consumed - 1
					}
				}
			}
		}
		for range width {
			cells = append(cells, themeCell{seq, fg, bg})
		}
		state, line = next, line[n:]
	}
	return cells
}

func requireCellColor(t *testing.T, want, got color.Color) {
	t.Helper()
	require.NotNil(t, got)
	assert.Equal(t, styles.RGBToHex(styles.ColorToRGB(want)), styles.RGBToHex(styles.ColorToRGB(got)))
}

func TestWarmThemeSwitchTabCellsAndGeometry(t *testing.T) { //nolint:paralleltest // theme globals
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	tb := New(newMotionRuntime(), 12)
	defer tb.StopAnimations()
	tb.SetWidth(80)
	tabs := []messages.TabInfo{
		{SessionID: "full-session-one", Title: "Active", AgentName: "developer", AgentNodeID: "complete-node-identity", IsActive: true},
		{SessionID: "full-session-two", Title: "Inactive", AgentName: "reviewer", AgentNodeID: "another-complete-node"},
	}
	tb.SetTabs(tabs, 0)
	tb.View()
	for _, ref := range []string{"default", "default-light", "nord", "one-dark", "calm-roots", "catppuccin-latte", "catppuccin-mocha", "dracula", "gruvbox-dark", "gruvbox-light", "neon-pink", "solarized-dark", "tokyo-night", "default"} {
		theme, err := styles.LoadTheme(ref)
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		warm := tb.View() // intentionally no ThemeChangedMsg
		cold := New(newMotionRuntime(), 12)
		cold.SetWidth(80)
		cold.SetTabs(tabs, 0)
		require.Equal(t, cold.View(), warm, ref)
		assert.Equal(t, cold.zones, tb.zones, ref)
		cold.StopAnimations()
		cells := themeCells(warm)
		require.Len(t, cells, 80)
		plain := ansi.Strip(warm)
		activeIndex := strings.Index(plain, "Active")
		inactiveIndex := strings.Index(plain, "Inactive")
		initialIndex := strings.Index(plain, "D")
		require.GreaterOrEqual(t, activeIndex, 0)
		require.GreaterOrEqual(t, inactiveIndex, 0)
		require.GreaterOrEqual(t, initialIndex, 0)
		requireCellColor(t, styles.EditorBg, cells[ansi.StringWidth(plain[:activeIndex])].bg)
		requireCellColor(t, styles.TabBg, cells[ansi.StringWidth(plain[:inactiveIndex])].bg)
		requireCellColor(t, styles.TabInactiveFg, cells[ansi.StringWidth(plain[:inactiveIndex])].fg)
		requireCellColor(t, styles.AgentIdentityStyle("developer", false).GetForeground(), cells[ansi.StringWidth(plain[:initialIndex])].bg)
		assert.Equal(t, "complete-node-identity", tb.tabs[0].AgentNodeID)
	}
}

func TestSingleTabPlusHoverPressReleaseAndLeave(t *testing.T) { //nolint:paralleltest // theme globals
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	styles.ApplyTheme(styles.DefaultTheme())
	for _, width := range []int{1, 2, 3, 8, 30, 80} {
		tb := New(newMotionRuntime(), 8)
		tb.SetWidth(width)
		tb.SetTabs(motionTabs(1, 0), 0)
		normal := tb.View()
		plain := ansi.Strip(normal)
		idx := strings.Index(plain, "+")
		require.GreaterOrEqual(t, idx, 0)
		x := ansi.StringWidth(plain[:idx])
		require.Equal(t, 1, tb.Height())
		finishFeedback(t, tb, tb.Update(tea.MouseMotionMsg{X: x}))
		hover := tb.View()
		require.NotEqual(t, normal, hover)
		requireCellColor(t, blendColors(styles.Background, styles.TabHoverBg, 0.3), themeCells(hover)[x].bg)
		require.Equal(t, []tea.Msg{messages.SpawnSessionMsg{}}, commandMessages(tb.Update(tea.MouseClickMsg{X: x, Button: tea.MouseLeft})))
		pressed := tb.View()
		require.NotEqual(t, hover, pressed)
		requireCellColor(t, blendColors(blendColors(styles.Background, styles.TabHoverBg, 0.3), styles.TabDragBg, 0.7), themeCells(pressed)[x].bg)
		require.True(t, tb.HasPointerCapture())
		tb.Update(tea.MouseMotionMsg{X: x, Y: -1})
		assert.NotEqual(t, normal, tb.View(), "initiated feedback finishes after leave")
		require.True(t, tb.HasPointerCapture())
		require.Empty(t, commandMessages(tb.Update(tea.MouseReleaseMsg{X: x, Y: -1, Button: tea.MouseLeft})))
		require.False(t, tb.HasPointerCapture())
		advanceTabRuntime(t, tb.ar, tb, tb.ar.Now()+plusFeedbackDuration)
		assert.Equal(t, normal, tb.View())
		require.Empty(t, commandMessages(tb.Update(tea.MouseClickMsg{X: x, Y: 1, Button: tea.MouseLeft})))
		require.Empty(t, commandMessages(tb.Update(tea.MouseClickMsg{X: x, Button: tea.MouseRight})))
		tb.Update(tea.MouseClickMsg{X: x, Button: tea.MouseLeft})
		tb.Update(tea.BlurMsg{})
		assert.False(t, tb.HasPointerCapture())
		assert.Equal(t, normal, tb.View())
		tb.StopAnimations()
	}
}

func TestTabPillUsesCurrentAgentInitialWithoutIdentityHeuristics(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, initial string }{
		{"developer", "D"},
		{"reviewer", "R"},
		{"équipe", "É"},
		{"界面", "界"},
		{"6dc19173-5678-4a90-8190-abcdefabcdef", "6"},
	} {
		info := messages.TabInfo{Title: "Session", AgentName: tc.name, AgentNodeID: "full-unshortened-node-id"}
		tab := renderTab(info, 20, dragRoleNone, 0)
		assert.Contains(t, ansi.Strip(tab.View()), " "+tc.initial+" ")
		assert.Equal(t, FixedTabWidth(20), tab.Width())
		assert.Equal(t, "full-unshortened-node-id", info.AgentNodeID)
	}
	assert.NotContains(t, ansi.Strip(renderTab(messages.TabInfo{Title: "Session"}, 20, dragRoleNone, 0).View()), " R ")
}

func TestRepeatedOutsideMotionDoesNotInvalidateWarmTabbar(t *testing.T) {
	t.Parallel()
	tb := New(newMotionRuntime(), 8)
	tb.SetWidth(80)
	tb.SetTabs(motionTabs(1, 0), 0)
	view := tb.View()
	generation := tb.VisualGeneration()
	for range 100 {
		assert.Nil(t, tb.Update(tea.MouseMotionMsg{X: 40, Y: -1}))
		assert.False(t, tb.viewDirty)
		assert.Equal(t, generation, tb.VisualGeneration())
		assert.Equal(t, view, tb.View())
	}
	tb.StopAnimations()
}

func TestPlusEntirePaddedHitZoneAndHideReset(t *testing.T) {
	t.Parallel()
	tb := New(newMotionRuntime(), 8)
	tb.SetWidth(80)
	tb.SetTabs(motionTabs(1, 0), 0)
	tb.View()
	var plus clickZone
	for _, zone := range tb.zones {
		if zone.isPlus {
			plus = zone
			break
		}
	}
	require.Equal(t, plusButtonWidth, plus.endX-plus.startX)
	for x := plus.startX; x < plus.endX; x++ {
		require.Equal(t, []tea.Msg{messages.SpawnSessionMsg{}}, commandMessages(tb.Update(tea.MouseClickMsg{X: x, Button: tea.MouseLeft})))
		require.True(t, tb.HasPointerCapture())
		require.Empty(t, commandMessages(tb.Update(tea.MouseReleaseMsg{X: x, Button: tea.MouseLeft})))
		require.False(t, tb.HasPointerCapture())
	}
	tb.Update(tea.MouseClickMsg{X: plus.startX, Button: tea.MouseLeft})
	tb.SetVisible(false)
	require.False(t, tb.HasPointerCapture())
	require.False(t, tb.plusHovered)
	require.False(t, tb.plusPressed)
	tb.StopAnimations()
}

func TestExplicitZeroAllocationSuppressesCellsHitsAndAnimations(t *testing.T) {
	t.Parallel()
	runtime := newMotionRuntime()
	tb := New(runtime, 8)
	tabs := motionTabs(2, 0)
	tabs[0].IsRunning = true
	tb.SetTabs(tabs, 0)
	require.Equal(t, fallbackWidth, ansi.StringWidth(tb.View()), "unset width retains default")
	require.Positive(t, runtime.ActiveCount())
	tb.Update(tea.MouseClickMsg{X: 2, Button: tea.MouseLeft})
	hold := DragHoldMsg{seq: tb.drag.seq}
	tb.SetWidth(0)
	require.Equal(t, 1, tb.Height(), "the root still owns the allocated strip row")
	require.Empty(t, tb.View())
	require.Empty(t, tb.zones)
	require.Empty(t, tb.dragBounds)
	require.Nil(t, tb.GetDragLayerInfo(80, 0))
	require.False(t, tb.HasPointerCapture())
	require.Zero(t, runtime.ActiveCount())
	tb.Update(hold)
	require.False(t, tb.IsDragging())
	require.Empty(t, commandMessages(tb.Update(tea.MouseClickMsg{X: 0, Button: tea.MouseLeft})))
	tb.SetTabs(tabs, 0)
	require.Empty(t, tb.View())
	require.Zero(t, runtime.ActiveCount())
	tb.SetWidth(80)
	require.Equal(t, 80, ansi.StringWidth(tb.View()))
	require.Positive(t, runtime.ActiveCount())
	tb.SetWidth(-1)
	require.Empty(t, tb.View())
	require.Zero(t, runtime.ActiveCount())
	tb.StopAnimations()
}

func TestViewRefreshesPaletteWithoutInstallingPointerGeometry(t *testing.T) { //nolint:paralleltest // theme globals
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	tb := New(newMotionRuntime(), 8)
	defer tb.StopAnimations()
	require.NotEmpty(t, tb.zones, "constructor prepares initial plus hits")
	tb.SetWidth(2)
	require.Equal(t, []clickZone{{startX: 0, endX: 2, tabIdx: noTab, isPlus: true}}, tb.zones)
	zones := append([]clickZone(nil), tb.zones...)
	generation := tb.VisualGeneration()
	before := tb.View()
	theme := styles.DefaultTheme()
	theme.Colors.Background = "#abcdef"
	styles.ApplyTheme(theme)
	require.Greater(t, tb.VisualGeneration(), generation, "palette rebuild invalidates global identity colors")
	generation = tb.VisualGeneration()
	localGeneration := tb.visualGeneration
	require.NotEqual(t, before, tb.View())
	assert.Equal(t, zones, tb.zones)
	assert.Equal(t, generation, tb.VisualGeneration(), "rendering itself does not advance generation")
	assert.Equal(t, localGeneration, tb.visualGeneration)
	tb.SetWidth(0)
	require.Empty(t, tb.zones)
	require.Empty(t, tb.View())
	require.Empty(t, tb.zones)
}

func TestWarmTabPillRecolorsImmediatelyOnAgentRegistration(t *testing.T) {
	styles.SetAgentOrder(nil)
	t.Cleanup(func() { styles.SetAgentOrder(nil) })
	tb := New(newMotionRuntime(), 12)
	defer tb.StopAnimations()
	tb.SetWidth(60)
	tb.SetTabs([]messages.TabInfo{{SessionID: "full-session", AgentName: "late-agent", AgentNodeID: "full-node", Title: "session", IsActive: true}}, 0)
	before := tb.View()
	generation := tb.VisualGeneration()
	styles.SetAgentOrder([]string{"first-agent", "late-agent"})
	require.Greater(t, tb.VisualGeneration(), generation)
	after := tb.View()
	require.NotEqual(t, before, after, "registration alone refreshes warmed fallback pill")
	plain := ansi.Strip(after)
	index := strings.Index(plain, "L")
	require.GreaterOrEqual(t, index, 0)
	requireCellColor(t, styles.AgentIdentityStyle("late-agent", false).GetForeground(), themeCells(after)[ansi.StringWidth(plain[:index])].bg)
	generation = tb.VisualGeneration()
	styles.SetAgentOrder([]string{"first-agent", "late-agent"})
	assert.Equal(t, generation, tb.VisualGeneration())
	assert.Equal(t, after, tb.View())
}
