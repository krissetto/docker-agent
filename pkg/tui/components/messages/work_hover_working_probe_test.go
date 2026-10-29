package messages

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

// An active spinner never reaches zero leases, so these probes step fixed frames.
const workingProbeFrames = 60

func workingProbeRepro(t *testing.T, width int) *hoverRepro {
	t.Helper()
	long := "HOVER_FINAL " + strings.Repeat("wrapped reply text keeps the final row multi-line. ", 4)
	r := newHoverRepro(t, width, long)
	// Settle first so the hover path is measured, not a status change during a fade.
	for range 100 {
		if !r.m.replyHandoff.Active() {
			break
		}
		r.tickOnce()
	}
	require.False(t, r.m.replyHandoff.Active(), "fixture handoff settles before the status change")
	r.m.SetWorkStatus(true, 3*time.Second)
	_ = r.m.View()
	block := firstWorkBlock(r.m)
	require.NotNil(t, block, "a Work block exists for the active exchange")
	require.True(t, block.Status().Active, "the block reports active status")
	require.Contains(t, strings.Join(r.frame(), "\n"), "Working", "active header label is Working")
	return r
}

// Matches the [+]/[-] indicator so both collapsed and expanded headers resolve.
func workingHeaderRow(lines []string) int {
	for i, line := range lines {
		if strings.Contains(line, "Work") && (strings.Contains(line, "[+]") || strings.Contains(line, "[-]")) {
			return i
		}
	}
	return -1
}

func (r *hoverRepro) clickWorkingHeader() {
	r.t.Helper()
	lines := r.frame()
	y := workingHeaderRow(lines)
	require.GreaterOrEqual(r.t, y, 0, "Work header is visible; frame:\n%s", strings.Join(lines, "\n"))
	_, _ = r.m.handleMouseClick(tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
	_ = r.m.View()
}

func workingMarkerRows(lines []string) [3]int {
	return [3]int{rowOf(lines, "HOVER_REQUEST"), workingHeaderRow(lines), rowOf(lines, "HOVER_FINAL")}
}

func (r *hoverRepro) probeFrame(label string) {
	r.t.Helper()
	incremental := workingMarkerRows(r.frame())
	incrementalTotal := r.m.totalHeight
	r.m.invalidateAllItems()
	_ = r.m.View()
	fresh := workingMarkerRows(r.frame())
	require.Equal(r.t, fresh, incremental, "%s: incremental rows differ from fresh layout", label)
	require.Equal(r.t, r.m.totalHeight, incrementalTotal, "%s: incremental total height differs from fresh layout", label)
}

func TestWorkingHoverDuringCollapseKeepsGeometry(t *testing.T) {
	for _, width := range []int{80, 44} {
		t.Run(fmt.Sprintf("width=%d", width), func(t *testing.T) {
			r := workingProbeRepro(t, width)
			r.clickWorkingHeader()
			for range workingProbeFrames {
				r.tickOnce()
				r.probeFrame("expand")
			}
			require.True(t, firstWorkBlock(r.m).Expanded())
			r.clickWorkingHeader()
			for i := range workingProbeFrames {
				r.tickOnce()
				r.probeFrame(fmt.Sprintf("collapse frame %d", i))
				before := workingMarkerRows(r.frame())
				beforeTotal := r.m.totalHeight
				for _, marker := range []string{"HOVER_REQUEST", "Work", "HOVER_FINAL"} {
					r.hoverRow(marker)
					require.Equal(t, before, workingMarkerRows(r.frame()), "frame %d: hovering %s moved rows", i, marker)
					require.Equal(t, beforeTotal, r.m.totalHeight, "frame %d: hovering %s changed total height", i, marker)
				}
				r.probeFrame(fmt.Sprintf("collapse frame %d after hover", i))
				require.True(t, firstWorkBlock(r.m).Status().Active, "frame %d: Work stays active while collapsing", i)
				require.Contains(t, strings.Join(r.frame(), "\n"), "Working", "frame %d: header stays Working", i)
			}
			require.False(t, firstWorkBlock(r.m).Expanded())
		})
	}
}
