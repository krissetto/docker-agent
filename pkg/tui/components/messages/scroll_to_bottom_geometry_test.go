package messages

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// Geometry invalidated by a background toggle must not let a bottom-scroll clamp to zero.
func TestScrollToBottomReconcilesInvalidatedGeometry(t *testing.T) {
	t.Parallel()

	for _, hover := range []bool{false, true} {
		t.Run(map[bool]string{false: "without hover", true: "with hover"}[hover], func(t *testing.T) {
			m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
			t.Cleanup(m.ar.Stop)
			m.AppendToLastMessage("root", "START-OF-CONVERSATION\n\n"+strings.Repeat("history paragraph\n\n", 40))
			m.AddAssistantMessage("root", "")
			m.View()
			require.Positive(t, m.scrollOffset)

			m.AppendToLastMessage("root", "LATEST-RESPONSE\n\n")
			_, _ = m.Update(messages.SessionToggleChangedMsg{})
			require.Zero(t, m.totalHeight)
			_, _ = m.Update(m.ScrollToBottom()())
			require.Positive(t, m.scrollOffset, "bottom-scroll must not clamp against invalidated height")
			if hover {
				_, _ = m.Update(tea.MouseMotionMsg{X: 20, Y: 6})
			}
			frame := m.View()
			require.Contains(t, frame, "LATEST-RESPONSE")
			require.NotContains(t, frame, "START-OF-CONVERSATION")
			require.False(t, m.userHasScrolled)
			require.Equal(t, m.totalScrollableHeight()-m.height, m.scrollOffset)
		})
	}
}

func TestWheelUpSurvivesNewContentAndBottomScroll(t *testing.T) {
	t.Parallel()

	for _, invalidate := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean geometry", true: "invalidated geometry"}[invalidate], func(t *testing.T) {
			m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
			t.Cleanup(m.ar.Stop)
			m.AppendToLastMessage("root", "START-OF-CONVERSATION\n\n"+strings.Repeat("history paragraph\n\n", 40))
			m.AddAssistantMessage("root", "")
			m.View()

			_, _ = m.Update(messages.WheelCoalescedMsg{X: 20, Y: 6, Delta: -1})
			require.True(t, m.userHasScrolled)
			offset := m.scrollOffset
			require.Less(t, offset, m.totalScrollableHeight()-m.height)

			m.AppendToLastMessage("root", "LATEST-RESPONSE\n\n")
			if invalidate {
				_, _ = m.Update(messages.SessionToggleChangedMsg{})
			}
			_, _ = m.Update(m.ScrollToBottom()())
			frame := m.View()

			require.True(t, m.userHasScrolled, "bottom-scroll must not clear a manual scroll")
			require.Equal(t, offset, m.scrollOffset)
			require.NotContains(t, frame, "LATEST-RESPONSE")
		})
	}
}

func TestHoverTransitionsSurviveBottomScroll(t *testing.T) {
	t.Parallel()

	m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
	t.Cleanup(m.ar.Stop)
	m.AppendToLastMessage("root", "START-OF-CONVERSATION\n\n"+strings.Repeat("history paragraph\n\n", 40))
	m.AddAssistantMessage("root", "")
	m.AppendToLastMessage("root", "LATEST-RESPONSE\n\n")
	m.View()

	_, _ = m.Update(tea.MouseMotionMsg{X: 20, Y: 6})
	_, _ = m.Update(m.ScrollToBottom()())
	m.View()
	require.Equal(t, m.totalScrollableHeight()-m.height, m.scrollOffset)

	_, _ = m.Update(tea.MouseMotionMsg{X: 20, Y: 6})
	line, _ := m.mouseToLineCol(20, 6)
	msgIdx, _ := m.globalLineToMessageLineCached(line)
	require.GreaterOrEqual(t, msgIdx, 0)
	require.Equal(t, msgIdx, m.hoveredMessageIndex, "hover must follow the pointer after bottom-scroll")
}
