package messages

import (
	"testing"
	"testing/synctest"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/widgets/cursor"
)

func TestInlineEditCursorRepaintsRetainedSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newAttachTestModel(t, taskItem(), assistantItem("agent", "answer", ""))
		m.StartInlineEdit(0, 0, "edited text")
		input := m.CursorInput()
		require.NotNil(t, input)
		cmd, _ := input.SetCursorExposed(true)
		shown := m.View()
		beforeHeight, beforeOffset := m.RenderedContentHeight(), m.scrollOffset
		require.Contains(t, ansi.Strip(shown), "edited text")

		_, changed := input.ApplyCursorBlink(cmd().(cursor.BlinkMsg))
		require.True(t, changed)
		m.InvalidateCursorView()
		hidden := m.View()
		require.NotEqual(t, shown, hidden)
		require.Equal(t, ansi.Strip(shown), ansi.Strip(hidden))
		require.Equal(t, beforeHeight, m.RenderedContentHeight())
		require.Equal(t, beforeOffset, m.scrollOffset)
	})
}
