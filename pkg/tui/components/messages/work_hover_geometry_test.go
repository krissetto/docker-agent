package messages

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// The driver mirrors the program loop: every event or accepted tick goes to
// Update and is followed by View. Nothing forces a full transcript sync, so a
// stale frame is observed exactly as the user would see it.
type hoverRepro struct {
	t  *testing.T
	ar *animation.Runtime
	m  *model
}

func newHoverRepro(t *testing.T, width int, finalText string) *hoverRepro {
	t.Helper()
	ar := animation.NewRuntimeWithScheduler(&disclosureClock{now: time.Unix(1, 0)})
	m := NewScrollableView(ar, width, 30, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetCompactWorkLog(true)
	m.addMessage(types.User("HOVER_REQUEST"))
	for i := range 3 {
		call := tools.ToolCall{ID: fmt.Sprintf("hover-call-%d", i), Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"true"}`}}
		m.addMessage(types.ToolCallMessage("root", call, tools.Tool{Name: "shell"}, types.ToolStatusCompleted))
	}
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", finalText))
	_ = m.View()
	return &hoverRepro{t: t, ar: ar, m: m}
}

// tickOnce delivers a single accepted frame, leaving the disclosure mid-flight.
func (r *hoverRepro) tickOnce() {
	r.t.Helper()
	cmd := r.ar.Continue()
	require.NotNil(r.t, cmd, "a disclosure in flight holds an animation lease")
	tick, ok := r.ar.Accept(cmd().(animation.TickMsg))
	require.True(r.t, ok)
	r.m.Update(tick)
	_ = r.m.View()
}

// settle delivers accepted ticks through Update and View until no animation
// lease remains; the work block's disclosure and any hover fade run to rest.
// hoverSettleTicks bounds settling; animations are expected to rest well before it.
const hoverSettleTicks = 500

func (r *hoverRepro) settle() {
	r.t.Helper()
	for range hoverSettleTicks {
		cmd := r.ar.Continue()
		if cmd == nil {
			return
		}
		tick, ok := r.ar.Accept(cmd().(animation.TickMsg))
		require.True(r.t, ok)
		r.m.Update(tick)
		_ = r.m.View()
	}
	require.Zero(r.t, r.ar.ActiveCount(), "animations must settle")
}

func (r *hoverRepro) frame() []string {
	return strings.Split(ansi.Strip(r.m.View()), "\n")
}

// rowOf returns the first visible screen row containing marker, or -1.
func rowOf(lines []string, marker string) int {
	for i, line := range lines {
		if strings.Contains(line, marker) {
			return i
		}
	}
	return -1
}

// markerRows captures only screen geometry; hover affordance text is ignored.
func markerRows(lines []string) [3]int {
	return [3]int{rowOf(lines, "HOVER_REQUEST"), rowOf(lines, "Work ["), rowOf(lines, "HOVER_FINAL")}
}

func (r *hoverRepro) clickHeader() {
	r.t.Helper()
	y := rowOf(r.frame(), "Work [")
	require.GreaterOrEqual(r.t, y, 0, "Work header is visible")
	_, _ = r.m.handleMouseClick(tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
	_ = r.m.View()
}

func (r *hoverRepro) enterHeader() {
	r.t.Helper()
	block := firstWorkBlock(r.m)
	require.NotNil(r.t, block)
	r.m.Focus()
	r.m.selectedMessageIndex = r.m.workSourceForBlock(block)
	require.GreaterOrEqual(r.t, r.m.selectedMessageIndex, 0)
	_, _ = r.m.handleKeyPress(tea.KeyPressMsg{Code: tea.KeyEnter})
	_ = r.m.View()
}

func (r *hoverRepro) hoverRow(marker string) {
	r.t.Helper()
	y := rowOf(r.frame(), marker)
	require.GreaterOrEqual(r.t, y, 0, "hover target %q is visible", marker)
	_, _ = r.m.handleMouseMotion(tea.MouseMotionMsg{X: 5, Y: y})
	_ = r.m.View()
}

// runHoverGeometry walks expand→collapse (complete or reversed mid-animation),
// then hovers the request and final rows with real animation ticks, asserting
// the screen geometry never moves and matches a fresh ground-truth frame.
func runHoverGeometry(t *testing.T, width int, finalText string, toggle func(*hoverRepro), reversed bool) {
	t.Helper()
	r := newHoverRepro(t, width, finalText)
	toggle(r)
	if reversed {
		// Reverse while the expand disclosure is still in flight.
		r.tickOnce()
		require.NotZero(t, r.ar.ActiveCount(), "reversal happens mid-animation")
	} else {
		r.settle()
	}
	toggle(r)
	r.settle()
	block := firstWorkBlock(r.m)
	require.NotNil(t, block)
	require.False(t, block.Expanded(), "the sequence ends with Work collapsed")

	before := r.frame()
	beforeRows := markerRows(before)
	require.Positive(t, beforeRows[0])
	require.Positive(t, beforeRows[2])
	beforeTotal := r.m.totalHeight

	for _, marker := range []string{"HOVER_REQUEST", "Work [", "HOVER_FINAL"} {
		r.hoverRow(marker)
		r.settle()
		after := r.frame()
		require.Equal(t, beforeRows, markerRows(after), "hovering %s moved visible rows", marker)
		require.Equal(t, beforeTotal, r.m.totalHeight, "hovering %s changed total height", marker)
	}

	// Ground truth from the settled state; the observed frames above were produced
	// without forcing a sync.
	r.m.invalidateAllItems()
	_ = r.m.View()
	require.Equal(t, beforeRows, markerRows(r.frame()), "observed collapsed frame matches a fresh layout")
	require.Equal(t, beforeTotal, r.m.totalHeight)
}

func TestHoverAfterMouseExpandCollapseKeepsGeometry(t *testing.T) {
	long := "HOVER_FINAL " + strings.Repeat("wrapped reply text keeps the final row multi-line. ", 4)
	for _, width := range []int{80, 44} {
		t.Run(fmt.Sprintf("width=%d", width), func(t *testing.T) {
			runHoverGeometry(t, width, long, (*hoverRepro).clickHeader, false)
		})
	}
}

func TestHoverAfterReversedMouseToggleKeepsGeometry(t *testing.T) {
	long := "HOVER_FINAL " + strings.Repeat("wrapped reply text keeps the final row multi-line. ", 4)
	runHoverGeometry(t, 44, long, (*hoverRepro).clickHeader, true)
}

func TestHoverAfterKeyboardExpandCollapseKeepsGeometry(t *testing.T) {
	long := "HOVER_FINAL " + strings.Repeat("wrapped reply text keeps the final row multi-line. ", 4)
	runHoverGeometry(t, 80, long, (*hoverRepro).enterHeader, false)
}

// The final reply starts a reply handoff. Expanding and collapsing Work while
// that handoff is still in flight must settle to a fresh layout: no frozen
// pre-toggle rows may survive the handoff's final tick, and hovering afterwards
// must not move any row.
func TestHoverAfterHandoffInFlightExpandCollapseKeepsGeometry(t *testing.T) {
	long := "HOVER_FINAL " + strings.Repeat("wrapped reply text keeps the final row multi-line. ", 4)
	for _, width := range []int{80, 44} {
		t.Run(fmt.Sprintf("width=%d", width), func(t *testing.T) {
			r := newHoverRepro(t, width, long)
			require.True(t, r.m.replyHandoff.Active(), "the final reply starts a reply handoff")
			r.tickOnce()
			require.True(t, r.m.replyHandoff.Active(), "the handoff is still in flight before the toggles")
			r.clickHeader()
			midExpand := markerRows(r.frame())
			r.tickOnce()
			r.clickHeader()
			midCollapse := markerRows(r.frame())
			t.Logf("in-flight rows: after expand %v, after collapse %v", midExpand, midCollapse)

			r.settle()
			require.False(t, r.m.replyHandoff.Active(), "the handoff reaches its final tick")
			require.Nil(t, r.m.replyHandoffFrameLines, "no frozen handoff rows survive the final tick")
			require.False(t, firstWorkBlock(r.m).Expanded(), "the sequence ends with Work collapsed")
			settledRows := markerRows(r.frame())
			settledTotal := r.m.totalHeight

			for _, marker := range []string{"HOVER_REQUEST", "Work [", "HOVER_FINAL"} {
				r.hoverRow(marker)
				r.settle()
				require.Equal(t, settledRows, markerRows(r.frame()), "hovering %s moved visible rows", marker)
				require.Equal(t, settledTotal, r.m.totalHeight, "hovering %s changed total height", marker)
			}

			// Ground truth is taken last so the observed frames were not repaired by a forced sync.
			r.m.invalidateAllItems()
			_ = r.m.View()
			require.Equal(t, settledRows, markerRows(r.frame()), "settled frame matches a fresh layout")
			require.Equal(t, settledTotal, r.m.totalHeight)
		})
	}
}
