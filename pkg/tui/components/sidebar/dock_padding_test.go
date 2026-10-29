package sidebar

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestDockPaddingKeepsBothEdgesAndContentHitGeometry(t *testing.T) {
	for _, dock := range []struct {
		name string
		left bool
	}{{"right", false}, {"left", true}} {
		t.Run(dock.name, func(t *testing.T) {
			m := New(animation.NewRuntime(), t.Context(), &service.SessionState{}).(*model)
			m.SetMode(ModeVertical)
			m.sessionState.SetYoloMode(true)
			m.workingDirectory = "/work/app"
			m.SetSize(40, 20)
			m.SetMirroredPadding(dock.left)
			if dock.left {
				require.Equal(t, LayoutConfig{PaddingLeft: 1, PaddingRight: 1, ScrollbarGap: 1}, m.layoutCfg)
			} else {
				require.Equal(t, DefaultLayoutConfig(), m.layoutCfg)
			}
			footer := ansi.Strip(m.footerView(m.contentWidth(false)))
			yoloColumn := strings.Index(footer, "YOLO")
			require.NotEqual(t, -1, yoloColumn)
			if dock.left {
				require.Equal(t, 1, yoloColumn, "left-docked status badge aligns to the left of its content row")
			} else {
				require.Greater(t, yoloColumn, m.width/2, "right dock keeps its right-aligned badge")
			}
			view := strings.Split(m.View(), "\n")[0]
			cells := ansi.StringWidth(view)
			require.Equal(t, m.width, cells)
			require.Equal(t, " ", ansi.Strip(ansi.Cut(view, 0, 1)), "terminal edge keeps one blank column")
			if dock.left {
				require.Equal(t, " ", ansi.Strip(ansi.Cut(view, m.width-1, m.width)), "left dock retains one chat-edge gap")
			}
			row, ok := findPlaced(m.placement.rows, "directory")
			require.True(t, ok, "directory row is a content hit target")
			y := int(row.targetY)
			require.Equal(t, ClickWorkingDir, hitAt(m, m.layoutCfg.PaddingLeft, y))
			require.Equal(t, ClickNone, hitAt(m, 0, y), "terminal-edge padding is not content")
			if dock.left {
				require.Equal(t, ClickNone, hitAt(m, m.width-1, y), "chat-edge gap is not content")
			}
		})
	}
}

func hitAt(m *model, x, y int) ClickResult {
	result, _ := m.HandleClickType(x, y)
	return result
}
