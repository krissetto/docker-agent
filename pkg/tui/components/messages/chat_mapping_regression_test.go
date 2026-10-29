package messages

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// chatFixtureReply builds the chat sequence: user prompt, spinner, streamed chunk,
// finalize. The reply enters through the incoming fade, so its rows are concealed
// until the handoff ticks run.
func chatFixtureReply(t *testing.T, compact bool) (*model, *animation.Runtime, int) {
	t.Helper()
	ar := animation.NewRuntimeWithScheduler(&replyHandoffScheduler{now: testHandoffTime()})
	m := NewScrollableView(ar, 116, 32, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetPosition(1, 1)
	m.SetCompactWorkLog(compact)
	m.AddUserMessage("What's 2+2?")
	m.AddAssistantMessage("root", "")
	m.AppendToLastMessage("root", "2 + 2 equals 4.")
	m.FinalizeStreamedAssistant("root", "2 + 2 equals 4.", "", false)
	m.View()
	reply := -1
	for i, msg := range m.messages {
		if msg.Type == types.MessageTypeAssistant && msg.Content == "2 + 2 equals 4." {
			reply = i
		}
	}
	require.GreaterOrEqual(t, reply, 0, "reply message exists")
	return m, ar, reply
}

// An external clear (dialog or presentation occlusion) forgets the tracked cell, so
// the fade completing under the same cell must not re-hover the reply.
func TestChatFixtureExternalClearDoesNotResurrectHoverAfterFade(t *testing.T) {
	m, ar, _ := chatFixtureReply(t, true)
	x, y := chatFixtureTextCell(t, m)
	_, _ = m.Update(tea.MouseMotionMsg{X: x, Y: y})
	require.Equal(t, -1, m.hoveredMessageIndex, "concealed rows are not hit-testable at opacity zero")
	m.ClearPointerHover()

	advanceModelHandoffPhase(t, ar, m, replyHandoffFadeIn)
	m.View()
	require.Equal(t, -1, m.hoveredMessageIndex, "an occluded transcript keeps no hover after the fade")
}

// chatFixtureTextCell returns the screen cell of the reply text in the current frame.
func chatFixtureTextCell(t *testing.T, m *model) (x, y int) {
	t.Helper()
	for row, plain := range strings.Split(ansi.Strip(m.View()), "\n") {
		if before, _, ok := strings.Cut(plain, "2 + 2 equals 4."); ok {
			return m.xPos + ansi.StringWidth(before) + 1, m.yPos + row
		}
	}
	require.FailNow(t, "reply text not visible")
	return 0, 0
}

// Once the incoming fade completes, every painted reply row maps back to the reply,
// and a motion onto the painted text hovers it.
func TestChatFixtureReplyRowsMapBackToReply(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(map[bool]string{false: "expanded", true: "compact-work"}[compact], func(t *testing.T) {
			m, ar, reply := chatFixtureReply(t, compact)
			advanceModelHandoffPhase(t, ar, m, replyHandoffFadeIn)
			require.False(t, m.replyHandoff.Active())
			m.View()

			painted := 0
			for line := range m.totalHeight {
				if !strings.Contains(ansi.Strip(m.renderedLine(line)), "2 + 2 equals 4.") {
					continue
				}
				painted++
				src, _ := m.globalLineToMessageLineCached(line)
				require.Equal(t, reply, src, "painted reply row %d must map to the reply", line)
			}
			require.Positive(t, painted, "the reply is painted")

			x, y := chatFixtureTextCell(t, m)
			_, _ = m.Update(tea.MouseMotionMsg{X: x, Y: y})
			require.Equal(t, reply, m.hoveredMessageIndex, "motion onto the painted reply hovers it")
		})
	}
}

// A pointer resting on the reply while its fade is still running must hover it once
// the fade makes the row interactive, without a further mouse motion.
func TestChatFixtureHoverSurvivesIncomingFadeWithStationaryPointer(t *testing.T) {
	m, ar, reply := chatFixtureReply(t, true)
	require.Equal(t, replyHandoffFadeIn, m.replyHandoff.Phase(), "compact-work enters the reply through its fade")
	x, y := chatFixtureTextCell(t, m)
	_, _ = m.Update(tea.MouseMotionMsg{X: x, Y: y})
	require.Equal(t, -1, m.hoveredMessageIndex, "concealed rows are not hit-testable at opacity zero")

	advanceModelHandoffPhase(t, ar, m, replyHandoffFadeIn)
	require.False(t, m.replyHandoff.Active())
	m.View()
	require.Equal(t, reply, m.hoveredMessageIndex, "the stationary pointer hovers the reply once it is interactive")
}

// ReplyHandoffActive is true only while the finite incoming fade runs.
func TestReplyHandoffActiveTracksFiniteFade(t *testing.T) {
	m, ar, _ := chatFixtureReply(t, true)
	require.True(t, m.ReplyHandoffActive(), "compact-work reply starts a finite fade")
	advanceModelHandoffPhase(t, ar, m, replyHandoffFadeIn)
	require.False(t, m.ReplyHandoffActive(), "the fade settles to idle")
}

// A scroll that lands during the fade (the auto-scroll behind a new reply) must not
// forget a still pointer; the fade completing under it must hover the reply.
func TestChatFixtureScrollDuringFadeKeepsStationaryHover(t *testing.T) {
	m, ar, reply := chatFixtureReply(t, true)
	require.Equal(t, replyHandoffFadeIn, m.replyHandoff.Phase(), "compact-work enters the reply through its fade")
	x, y := chatFixtureTextCell(t, m)
	_, _ = m.Update(tea.MouseMotionMsg{X: x, Y: y})
	require.Equal(t, -1, m.hoveredMessageIndex, "concealed rows are not hit-testable at opacity zero")

	m.scrollToBottom()
	advanceModelHandoffPhase(t, ar, m, replyHandoffFadeIn)
	m.View()
	require.Equal(t, reply, m.hoveredMessageIndex, "a scroll during the fade must not forget the stationary pointer")
}
