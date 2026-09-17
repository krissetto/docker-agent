package chat

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
)

type capturedScrollbarMessages struct {
	messages.Model

	dragging                   bool
	motions, releases, cancels int
}

func (m *capturedScrollbarMessages) IsScrollbarDragging() bool { return m.dragging }
func (m *capturedScrollbarMessages) CancelScrollbarDrag() {
	m.cancels++
	m.dragging = false
}

func (m *capturedScrollbarMessages) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg.(type) {
	case tea.MouseMotionMsg:
		m.motions++
	case tea.MouseReleaseMsg:
		m.releases++
		m.dragging = false
	}
	return m, nil
}

type captureCountingSidebar struct {
	sidebar.Model

	pointerEvents int
	clears        int
}

func (s *captureCountingSidebar) ClearSubagentHover() tea.Cmd {
	s.clears++
	return s.Model.ClearSubagentHover()
}

func (s *captureCountingSidebar) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg.(type) {
	case tea.MouseMotionMsg, tea.MouseReleaseMsg:
		s.pointerEvents++
	}
	return s.Model.Update(msg)
}

func TestMessagesScrollbarCapturePrecedesPageSectionHitTesting(t *testing.T) {
	for _, split := range []bool{false, true} {
		p := newLayoutTestPage(t, msgtypes.SidebarRight)
		p.SetSize(160, 40)
		if split {
			p.SetSplitPresentation(&SplitPresentationGeometry{
				Transcript:  PresentationRect{20, 10, 40, 20},
				Shell:       screenShell(p.MeasureSplitShell(160, 40), 0, 0),
				ShowSidebar: true,
			})
		}
		tracked := &capturedScrollbarMessages{Model: p.messages, dragging: true}
		p.messages = tracked
		sb := &captureCountingSidebar{Model: p.sidebar}
		p.sidebar = sb
		for _, point := range [][2]int{{159, 5}, {-100, -100}, {400, 500}} {
			_, _ = p.Update(tea.MouseMotionMsg{X: point[0], Y: point[1], Button: tea.MouseLeft})
		}
		_, _ = p.Update(tea.MouseReleaseMsg{X: 159, Y: 5, Button: tea.MouseLeft})
		require.Equal(t, 3, tracked.motions)
		require.Equal(t, 1, tracked.releases)
		require.Zero(t, sb.pointerEvents, "captured pointer must never reach sidebar")
		require.Equal(t, 3, sb.clears, "captured motion still clears stale sidebar hover")
		require.False(t, p.IsMessagesScrollbarDragging())
		tracked.dragging = true
		p.CancelMessagesScrollbarDrag()
		require.Equal(t, 1, tracked.cancels)
		require.Equal(t, 1, tracked.releases, "cancel must not dispatch through generic Update")
		require.Zero(t, sb.pointerEvents)
	}
}
