package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestCollapsedPreviewReservesHeaderPlusThreeRowsInSmallViewports(t *testing.T) {
	for _, width := range []int{3, 12, 60} {
		for _, height := range []int{4, 5, 10} {
			m := newSpinnerTestModel(t)
			m.SetCompactWorkLog(true)
			m.SetSize(width, height)
			m.AddUserMessage("request")
			m.SetWorkStatus(true, 0)
			m.AppendReasoningContent("s", "r", "root", "one\n\ntwo\n\nthree")
			frame := ansi.Strip(m.View())
			work := firstWorkBlock(m)
			require.NotNil(t, work)
			require.Equal(t, 4, work.Height())
			if width >= 12 {
				require.Equal(t, 3, strings.Count(frame, "one")+strings.Count(frame, "two")+strings.Count(frame, "three"))
			}
		}
	}
}
