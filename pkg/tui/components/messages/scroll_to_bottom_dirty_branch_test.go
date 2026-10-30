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

// Branch-adapted form of upstream TestScrollToBottomReconcilesDirtyGeometry.
// The branch invalidates geometry through SessionToggleChangedMsg, not
// upstream's blurred-view deferral (B3). Every upstream assertion is kept.
func TestScrollToBottomReconcilesDirtyGeometry(t *testing.T) {
	t.Parallel()

	for _, hover := range []bool{false, true} {
		t.Run(map[bool]string{false: "without hover", true: "with hover"}[hover], func(t *testing.T) {
			m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
			t.Cleanup(m.ar.Stop)
			m.AppendToLastMessage("root", "START-OF-CONVERSATION\n\n"+strings.Repeat("history paragraph\n\n", 40))
			m.AddAssistantMessage("root", "")
			m.View()
			require.Positive(t, m.scrollOffset)

			// Replacing the spinner invalidates geometry; the branch's trigger is the session toggle.
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

			m.AppendToLastMessage("root", strings.Repeat("new paragraph\n\n", 20)+"STILL-FOLLOWING\n\n")
			require.Contains(t, m.View(), "STILL-FOLLOWING")
		})
	}
}
