package messages

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// Handoff text is frozen at fade start; without a repaint the header stays stale until the fade ends.
func TestWorkStatusChangeDuringReplyHandoffRepaintsHeader(t *testing.T) {
	long := "HOVER_FINAL " + strings.Repeat("wrapped reply text keeps the final row multi-line. ", 4)
	r := newHoverRepro(t, 80, long)
	require.True(t, r.m.replyHandoff.Active(), "the final reply starts a handoff")

	r.m.SetWorkStatus(true, 3*time.Second)
	require.Contains(t, ansi.Strip(r.m.View()), "Working", "the header repaints mid-handoff")

	for range 100 {
		if !r.m.replyHandoff.Active() {
			break
		}
		r.tickOnce()
		require.Contains(t, ansi.Strip(r.m.View()), "Working", "every handoff frame shows the active header")
	}
	require.False(t, r.m.replyHandoff.Active(), "the handoff completes")
	require.Contains(t, ansi.Strip(r.m.View()), "Working", "the settled frame shows the active header")
}
