package tui

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/tabbar"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func layoutTabs() []messages.TabInfo {
	return []messages.TabInfo{
		{SessionID: "layout-first-full-id", Title: "界 e\u0301", IsActive: true},
		{SessionID: "layout-second-full-id", Title: "Second"},
	}
}

// StyledString.Lines returns graphemes, so expand wide cells before indexing by X.
func layoutTerminalCells(view string) []uv.Line {
	lines := uv.NewStyledString(view).Lines(ansi.GraphemeWidth)
	for y, graphemes := range lines {
		width := 0
		for _, cell := range graphemes {
			width += cell.Width
		}
		columns := uv.NewLine(width)
		x := 0
		for i := range graphemes {
			columns.Set(x, &graphemes[i])
			x += graphemes[i].Width
		}
		lines[y] = columns
	}
	return lines
}

func TestTabFrameAlignsWithEditorBorderCells(t *testing.T) {
	m, _, _ := wallClockRoot(t, 120, 40)
	_, _ = m.Update(messages.TabsUpdatedMsg{Tabs: layoutTabs(), ActiveIdx: 0})
	for _, width := range []int{120, 40, 80, 120} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			_, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 40})
			view := m.View().Content
			lines := strings.Split(view, "\n")
			tabY := m.contentHeight + 1
			editorY := tabY + m.tabBar.Height() + m.editor.BannerHeight()
			require.Equal(t, width, ansi.StringWidth(lines[tabY]))
			require.Equal(t, width, ansi.StringWidth(lines[editorY]))
			cells := layoutTerminalCells(view)
			local := layoutTerminalCells(m.tabBar.View())[0]
			left, right := styles.EditorStyle.GetMarginLeft(), width-styles.EditorStyle.GetMarginRight()
			require.Len(t, local, right-left)
			for x := left; x < right; x++ {
				require.True(t, local[x-left].Equal(&cells[tabY][x]), "tab cell %d", x)
			}
			for _, x := range []int{left, right - 1} {
				require.Equal(t, color.NRGBAModel.Convert(styles.EditorBg), color.NRGBAModel.Convert(cells[editorY][x].Style.Bg), "editor frame edge %d", x)
			}
			for _, x := range []int{left - 1, right} {
				require.Equal(t, " ", cells[tabY][x].Content)
				require.NotEqual(t, cells[editorY][left].Style.Bg, cells[editorY][x].Style.Bg, "editor outer margin %d", x)
			}
			require.Equal(t, "▎", cells[tabY][left].Content, "tab accent starts at editor frame, not text padding")
		})
	}
}

func TestTabFramePointerRoutes(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprintf("background=%t", background), func(t *testing.T) {
			m, _, _ := wallClockRoot(t, 100, 40)
			_, _ = m.Update(messages.TabsUpdatedMsg{Tabs: layoutTabs(), ActiveIdx: 0})
			_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
			_ = m.View()
			if background {
				_, _ = m.Update(dialog.OpenDialogMsg{Model: dialog.NewHelpDialog(nil), OriginatingEvent: "layout-background"})
				require.True(t, m.dialogMgr.TopIsBackground())
			}
			x, y := styles.EditorStyle.GetMarginLeft(), m.contentHeight+1
			_, cmd := m.Update(tea.MouseClickMsg{X: x - 1, Y: y, Button: tea.MouseLeft})
			require.False(t, m.tabBar.IsDragging(), "outer margin must not hit first tab")
			require.Empty(t, collectMsgs(cmd))
			_, _ = m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			require.True(t, m.tabBar.IsDragging(), "accent column is inside tab")
			_, cmd = m.Update(tea.MouseReleaseMsg{X: x - 1, Y: y, Button: tea.MouseLeft})
			require.Empty(t, collectMsgs(cmd), "release in outer margin must not select tab")
			require.False(t, m.tabBar.IsDragging())

			plain := ansi.Strip(m.tabBar.View())
			closeIndex := strings.Index(plain, "×")
			require.NotEqual(t, -1, closeIndex, "rendered tab has a close button")
			closeX := x + ansi.StringWidth(plain[:closeIndex])
			_, cmd = m.Update(tea.MouseClickMsg{X: closeX, Y: y, Button: tea.MouseLeft})
			closeMsg, ok := firstOfType[messages.CloseTabMsg](collectMsgs(cmd))
			require.True(t, ok)
			require.Equal(t, layoutTabs()[0].SessionID, closeMsg.SessionID)

			_, cmd = m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			for _, msg := range collectMsgs(cmd) {
				if _, ok := msg.(tabbar.DragHoldMsg); ok {
					_, _ = m.Update(msg)
				}
			}
			_, _ = m.Update(tea.MouseMotionMsg{X: x + 8, Y: y, Button: tea.MouseLeft})
			layer := m.tabBar.GetDragLayerInfo(tabFrameWidth(m.width), y)
			require.NotNil(t, layer)
			require.Equal(t, 8, layer.X, "motion preserves tab-local grab point")
			if !background {
				cells := layoutTerminalCells(m.View().Content)
				overlay := layoutTerminalCells(layer.Content)[0]
				for i := range overlay {
					require.True(t, overlay[i].Equal(&cells[y][x+8+i]), "overlay cell %d", i)
				}
			}
			_, _ = m.Update(tea.MouseReleaseMsg{X: x + 8, Y: y, Button: tea.MouseLeft})
			require.False(t, m.tabBar.IsDragging())
			layer = m.tabBar.GetDragLayerInfo(tabFrameWidth(m.width), y)
			require.NotNil(t, layer)
			require.Equal(t, 8, layer.X, "release retains the exact drop position")
		})
	}
}
