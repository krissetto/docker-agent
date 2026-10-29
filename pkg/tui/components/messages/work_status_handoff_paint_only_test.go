package messages

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A status change during a handoff only repaints header text; the frame rows
// keep their geometry, sources and scroll position.
func TestSetWorkStatusDuringHandoffIsPaintOnly(t *testing.T) {
	long := "HOVER_FINAL " + strings.Repeat("wrapped reply text keeps the final row multi-line. ", 4)
	r := newHoverRepro(t, 80, long)
	require.True(t, r.m.replyHandoff.Active(), "the final reply starts a handoff")

	before := slices.Clone(r.m.replyHandoffFrameRows)
	offset := r.m.scrollOffset
	r.m.SetWorkStatus(true, 3*time.Second)

	require.Equal(t, offset, r.m.scrollOffset, "status repaint must not move the viewport")
	require.Len(t, r.m.replyHandoffFrameRows, len(before))
	for i, row := range r.m.replyHandoffFrameRows {
		require.Equal(t, before[i].Y, row.Y, "row %d geometry", i)
		require.Equal(t, before[i].Height, row.Height, "row %d geometry", i)
		require.Equal(t, before[i].LocalRow, row.LocalRow, "row %d local row", i)
		require.Equal(t, before[i].Source, row.Source, "row %d source", i)
		if row.Composite == nil {
			require.Equal(t, before[i].Text, row.Text, "row %d source text", i)
		}
	}
}
