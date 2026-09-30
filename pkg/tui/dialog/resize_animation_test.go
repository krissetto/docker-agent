package dialog

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/core/layout"
)

type resizeContentMsg struct{ width, height int }

type resizeProbeDialog struct {
	lifecycleDialog
	views int
}

func (d *resizeProbeDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if size, ok := msg.(resizeContentMsg); ok {
		d.view = resizeProbeView(size.width, size.height)
	}
	return d, nil
}

func (d *resizeProbeDialog) View() string { d.views++; return d.view }

func resizeProbeView(width, height int) string {
	rows := make([]string, height)
	for i := range rows {
		rows[i] = fmt.Sprintf("%02d", i) + strings.Repeat("x", width-2)
	}
	return strings.Join(rows, "\n")
}

func resizeProbeManager(t *testing.T, width, height int) (*manager, *resizeProbeDialog) {
	t.Helper()
	r := newDialogRuntime()
	mgr := &manager{runtime: r, width: 100, height: 40}
	d := &resizeProbeDialog{lifecycleDialog: lifecycleDialog{view: resizeProbeView(width, height)}}
	mgr.handleOpen(OpenDialogMsg{Model: d})
	mgr.handleTick(advanceDialog(r, r.Continue(), dialogOpenDuration))
	t.Cleanup(mgr.Cleanup)
	return mgr, d
}

func resizeRect(mgr *manager) [4]int {
	e := &mgr.stack[0]
	row, col := e.position(mgr.width, mgr.height)
	return [4]int{col, row, e.renderWidth, e.renderHeight}
}

func advanceResize(mgr *manager, duration time.Duration) {
	r := mgr.runtime
	mgr.handleTick(advanceDialog(r, r.Continue(), r.Now()+duration))
}

func TestResizeContentInterpolatesBothDimensionsFromDisplayedRectangle(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to [2]int
	}{
		{"shrink", [2]int{40, 20}, [2]int{20, 8}},
		{"grow", [2]int{20, 8}, [2]int{40, 20}},
		{"width shrink", [2]int{40, 8}, [2]int{20, 8}},
		{"width grow", [2]int{20, 8}, [2]int{40, 8}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr, d := resizeProbeManager(t, tc.from[0], tc.from[1])
			before := resizeRect(mgr)
			mgr.Update(resizeContentMsg{tc.to[0], tc.to[1]})
			e := &mgr.stack[0]
			require.Equal(t, before, resizeRect(mgr), "event frame preserves the entire displayed rectangle")

			require.True(t, mgr.pointerSuppressed())
			views, measurements := d.views, e.boundsMeasurementCount
			advanceResize(mgr, dialogResizeDuration/2)
			middle := resizeRect(mgr)
			for axis := 2; axis < 4; axis++ {
				from, to := tc.from[axis-2], tc.to[axis-2]
				if from != to {
					require.Greater(t, middle[axis], min(from, to))
					require.Less(t, middle[axis], max(from, to))
				}
			}
			require.Equal(t, 1.0, e.opacity(), "resize does not substitute a fade")

			advanceResize(mgr, dialogResizeDuration)
			row, col := CenterPosition(mgr.width, mgr.height, tc.to[0], tc.to[1])
			require.Equal(t, [4]int{col, row, tc.to[0], tc.to[1]}, resizeRect(mgr))
			require.Equal(t, d.view, e.view())
			require.Equal(t, measurements, e.boundsMeasurementCount)
			require.Equal(t, views, d.views, "ticks use prepared source and destination")
			require.False(t, mgr.pointerSuppressed())
			require.Zero(t, mgr.runtime.ActiveCount())
		})
	}
}

func TestResizeTerminalMovesExistingRectangleWithoutInstantRecentering(t *testing.T) {
	for _, size := range [][2]int{{140, 60}, {80, 30}, {12, 4}, {0, 0}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			mgr, _ := resizeProbeManager(t, 30, 10)
			before := resizeRect(mgr)
			mgr.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			if size[0] > 0 {
				require.Equal(t, before, resizeRect(mgr))
				info := mgr.GetLayerInfos()[0]
				require.LessOrEqual(t, info.X+lipgloss.Width(info.Content), size[0])
				require.LessOrEqual(t, info.Y+lipgloss.Height(info.Content), size[1])
				advanceResize(mgr, dialogResizeDuration/2)
				require.NotEqual(t, before, resizeRect(mgr))
				advanceResize(mgr, dialogResizeDuration)
			}
			mgr.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
			advanceResize(mgr, dialogResizeDuration)
			require.Equal(t, before, resizeRect(mgr), "tiny terminal restores intrinsic content bounds")
			require.Zero(t, mgr.runtime.ActiveCount())
		})
	}
}

func TestResizeReversalAndClosePreserveCurrentRectangle(t *testing.T) {
	mgr, _ := resizeProbeManager(t, 40, 20)
	mgr.Update(resizeContentMsg{20, 8})
	advanceResize(mgr, dialogResizeDuration/3)
	before := resizeRect(mgr)
	mgr.Update(resizeContentMsg{50, 24})
	require.Equal(t, before, resizeRect(mgr))
	require.Equal(t, int32(1), mgr.runtime.ActiveCount())
	advanceResize(mgr, dialogResizeDuration/3)
	before, frame := resizeRect(mgr), mgr.stack[0].view()
	mgr.beginClose(false)
	require.Equal(t, before, resizeRect(mgr))
	require.Equal(t, frame, mgr.stack[0].view())
	advanceResize(mgr, dialogCloseDuration)
	require.False(t, mgr.Open())
	require.Zero(t, mgr.runtime.ActiveCount())
}

func TestResizeStackPreservesDragOffsets(t *testing.T) {
	mgr, _ := resizeProbeManager(t, 30, 10)
	mgr.stack[0].offsetX, mgr.stack[0].offsetY = -4, 3
	mgr.handleOpen(OpenDialogMsg{Model: &resizeProbeDialog{lifecycleDialog: lifecycleDialog{view: resizeProbeView(20, 8)}}})
	advanceResize(mgr, dialogOpenDuration)
	mgr.stack[1].offsetX, mgr.stack[1].offsetY = 5, -2
	before := mgr.GetLayerInfos()
	mgr.Update(tea.WindowSizeMsg{Width: 120, Height: 50})
	zero := mgr.GetLayerInfos()
	for i := range before {
		require.Equal(t, before[i].X, zero[i].X)
		require.Equal(t, before[i].Y, zero[i].Y)
	}
	advanceResize(mgr, dialogResizeDuration)
	for i, offsets := range [][2]int{{-4, 3}, {5, -2}} {
		e := &mgr.stack[i]
		require.Equal(t, offsets, [2]int{e.offsetX, e.offsetY})
		row, col := CenterPosition(mgr.width, mgr.height, e.targetWidth, e.targetHeight)
		info := mgr.GetLayerInfos()[i]
		require.Equal(t, col+offsets[0], info.X)
		require.Equal(t, row+offsets[1], info.Y)
	}
	require.Zero(t, mgr.runtime.ActiveCount())
}

func TestResizeParityKeepsSourceAnchorMonotonic(t *testing.T) {
	for _, screenHeight := range []int{40, 41} {
		for _, heights := range [][2]int{{20, 8}, {21, 8}, {20, 9}, {21, 9}, {8, 21}, {9, 20}} {
			t.Run(fmt.Sprint(screenHeight, heights), func(t *testing.T) {
				mgr, _ := resizeProbeManager(t, 20, heights[0])
				mgr.height = screenHeight
				mgr.stack[0].viewportHeight = screenHeight
				mgr.Update(resizeContentMsg{20, heights[1]})
				e := &mgr.stack[0]
				from := e.fromRow
				to := e.targetRow
				previous := from
				for e.anim.Running() {
					top, _ := e.position(mgr.width, mgr.height)
					anchor := top
					if to >= from {
						require.GreaterOrEqual(t, anchor, previous)
					} else {
						require.LessOrEqual(t, anchor, previous)
					}
					require.GreaterOrEqual(t, anchor, min(from, to))
					require.LessOrEqual(t, anchor, max(from, to))
					previous = anchor
					mgr.handleTick(acceptedDialogTick(mgr.runtime, mgr.runtime.Continue()))
				}
				require.Zero(t, mgr.runtime.ActiveCount())
			})
		}
	}
}

func TestResizeHideReopenKeepsSampledGeometryAndLease(t *testing.T) {
	mgr, d := resizeProbeManager(t, 40, 20)
	mgr.Update(resizeContentMsg{20, 8})
	advanceResize(mgr, dialogResizeDuration/3)
	mgr.beginClose(true)
	advanceResize(mgr, dialogCloseDuration/3)
	before := resizeRect(mgr)
	alpha := mgr.stack[0].opacity()
	mgr.handleOpen(OpenDialogMsg{Model: d})
	require.Equal(t, before, resizeRect(mgr))
	require.Equal(t, alpha, mgr.stack[0].opacity())
	require.Equal(t, int32(1), mgr.runtime.ActiveCount())
	advanceResize(mgr, dialogOpenDuration)
	require.Len(t, mgr.stack, 1)
	require.Equal(t, d.view, mgr.stack[0].view())
	require.Zero(t, mgr.runtime.ActiveCount())
}

func TestResizeCommandPaletteKeepsLatestQueryAndResultsLive(t *testing.T) {
	r := newDialogRuntime()
	mgr := &manager{runtime: r, width: 100, height: 40}
	d := NewCommandPaletteDialog(concretePaletteCommands(3, 8)).(*commandPaletteDialog)
	mgr.handleOpen(OpenDialogMsg{Model: d})
	advanceResize(mgr, dialogOpenDuration)
	t.Cleanup(mgr.Cleanup)
	before := resizeRect(mgr)
	for _, query := range []string{"Command 0 0", "Command 1 1", "no result exists"} {
		d.textInput.SetValue(query)
		mgr.forwardToTop(tea.PasteMsg{})
		require.Equal(t, before, resizeRect(mgr), "input does not jump the displayed rectangle")
		require.True(t, mgr.stack[0].anim.Running())
		frame := ansi.Strip(mgr.stack[0].view())
		require.Contains(t, frame, query, "typing appears on the next frame, not at transition completion")
		require.NotContains(t, frame, "Command 2 7", "filtered-out rows cannot survive in a shrinking snapshot")
		if query != "Command 0 0" {
			require.NotContains(t, frame, "Command 0 0", "same-sized target updates must replace prepared content too")
		}
	}
	advanceResize(mgr, dialogResizeDuration)
	require.Zero(t, r.ActiveCount())
}
