package messages

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func newSendScroll(t *testing.T) *model {
	t.Helper()
	ar := animation.NewRuntimeWithScheduler(&replyHandoffScheduler{now: time.Unix(1, 0)})
	m := NewScrollableView(ar, 64, 12, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.addMessage(types.User("history"))
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", strings.Repeat("old reply\n\n", 20)))
	m.ensureAllItemsRendered()
	settleSentScroll(t, m)
	return m
}

func addSentPrompt(m *model, id, text string) *types.Message {
	msg := types.User(text)
	msg.TurnID = id
	m.addMessage(msg)
	return msg
}

func TestScrollOnSendDefaultOffAndExplicitReceiptOrdering(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(map[bool]string{true: "receipt-first", false: "input-first"}[before], func(t *testing.T) {
			m := newSendScroll(t)
			m.ScrollToSentInput("disabled")
			require.Empty(t, m.scrollOnSend.pending)
			m.SetScrollOnSend(true)
			if before {
				m.ScrollToSentInput("local")
			}
			prompt := addSentPrompt(m, "local", "new prompt")
			if !before {
				m.ScrollToSentInput("local")
			}
			settleSentScroll(t, m)
			require.Same(t, prompt, m.scrollOnSend.target)
			require.Positive(t, m.scrollOnSend.padding)
			index := m.messagePointerIndex(prompt)
			require.InDelta(t, m.slotOffset(index), m.scrollOffset, 1)
			m.Update(scrollToBottomMsg{})
			require.Same(t, prompt, m.scrollOnSend.target, "stale automatic bottom command cannot override new send")
			m.scrollUp()
			offset := m.scrollOffset
			require.Nil(t, m.scrollOnSend.target)
			settleSentScroll(t, m)
			require.Equal(t, offset, m.scrollOffset, "taking control does not clamp away visible reserve")
			m.ScrollToSentInput("local")
			require.Nil(t, m.scrollOnSend.target, "duplicate receipt cannot recapture reader")
		})
	}
}

func TestScrollOnSendGrowthConsumesReserveAndResumesFollow(t *testing.T) {
	m := newSendScroll(t)
	m.SetScrollOnSend(true)
	addSentPrompt(m, "local", "prompt")
	m.ScrollToSentInput("local")
	settleSentScroll(t, m)
	offset, padding := m.scrollOffset, m.scrollOnSend.padding
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "short reply"))
	settleSentScroll(t, m)
	require.Equal(t, offset, m.scrollOffset)
	require.Less(t, m.scrollOnSend.padding, padding)
	m.AppendToLastMessage("root", strings.Repeat("\n\nmore reply", 30))
	settleSentScroll(t, m)
	require.Zero(t, m.scrollOnSend.padding)
	require.Nil(t, m.scrollOnSend.target)
	require.False(t, m.userHasScrolled)
	require.Equal(t, m.totalScrollableHeight()-m.height, m.scrollOffset)
}

func TestScrollOnSendLongPromptResizeAndSmallViewport(t *testing.T) {
	m := newSendScroll(t)
	m.SetScrollOnSend(true)
	prompt := addSentPrompt(m, "local", strings.Repeat("long prompt line\n\n", 20))
	m.ScrollToSentInput("local")
	settleSentScroll(t, m)
	index := m.messagePointerIndex(prompt)
	end := m.slotOffset(index) + m.slotBase[index]
	require.Equal(t, end+max(3, m.height/3)-m.height, m.scrollOffset)
	m.SetSize(40, 18)
	settleSentScroll(t, m)
	end = m.slotOffset(index) + m.slotBase[index]
	require.Equal(t, max(m.slotOffset(index)-1, end+max(3, m.height/3)-m.height), m.scrollOffset)
	m.SetSize(40, 5)
	settleSentScroll(t, m)
	require.Nil(t, m.scrollOnSend.target)
	require.Zero(t, m.scrollOnSend.padding)
	require.Equal(t, max(0, m.totalScrollableHeight()-m.height), m.scrollOffset)
}

func TestScrollOnSendDoesNotInferOrNavigateDuringReplay(t *testing.T) {
	m := newSendScroll(t)
	m.SetScrollOnSend(true)
	addSentPrompt(m, "remote", "not my send")
	settleSentScroll(t, m)
	require.Nil(t, m.scrollOnSend.target)
	m.replay = &sessionReplay{}
	m.ScrollToSentInput("replayed")
	require.Empty(t, m.scrollOnSend.pending)
	m.replay = nil
	m.ScrollToSentInput("next")
	require.NotEmpty(t, m.scrollOnSend.pending)
	m.scrollPageUp()
	require.Empty(t, m.scrollOnSend.pending)
}

func TestScrollOnSendHarmlessClickKeepsHoldAndFailureClearsMissingReceipt(t *testing.T) {
	m := newSendScroll(t)
	m.SetScrollOnSend(true)
	addSentPrompt(m, "local", "prompt")
	m.ScrollToSentInput("local")
	settleSentScroll(t, m)
	target := m.scrollOnSend.target
	m.handleMouseClick(tea.MouseClickMsg{X: m.xPos + 4, Y: m.yPos + 2, Button: tea.MouseRight})
	require.Same(t, target, m.scrollOnSend.target)
	m.handleMouseClick(tea.MouseClickMsg{X: m.xPos + 4, Y: m.yPos + 2, Button: tea.MouseLeft})
	require.Same(t, target, m.scrollOnSend.target, "selection press without drag is not viewport ownership")
	m.handleMouseMotion(tea.MouseMotionMsg{X: m.xPos + 8, Y: m.yPos + 3, Button: tea.MouseLeft})
	require.Nil(t, m.scrollOnSend.target)
	m.ScrollToSentInput("missing")
	require.NotEmpty(t, m.scrollOnSend.pending)
	m.SetWorkStatus(false, 0)
	require.Empty(t, m.scrollOnSend.pending)
}

func TestScrollOnSendNewRequestReplacesOldAndSettleHoldsReserve(t *testing.T) {
	m := newSendScroll(t)
	m.SetScrollOnSend(true)
	addSentPrompt(m, "first", "first prompt")
	m.ScrollToSentInput("first")
	settleSentScroll(t, m)
	offset, padding := m.scrollOffset, m.scrollOnSend.padding
	m.SetWorkStatus(false, 0)
	settleSentScroll(t, m)
	require.Equal(t, offset, m.scrollOffset)
	require.Equal(t, padding, m.scrollOnSend.padding)
	m.ScrollToSentInput("new")
	require.Equal(t, []string{"new"}, m.scrollOnSend.pending)
	addSentPrompt(m, "new", "second prompt")
	settleSentScroll(t, m)
	require.Equal(t, "new", m.scrollOnSend.target.TurnID)
	require.Greater(t, m.scrollOffset, offset)
}

func settleSentScroll(t *testing.T, m *model) {
	t.Helper()
	m.View()
	for range 100 {
		if !m.scrollOnSend.transition.Running() {
			return
		}
		tickSentScroll(t, m)
	}
	t.Fatal("send transition did not finish")
}

func tickSentScroll(t *testing.T, m *model) {
	t.Helper()
	cmd := m.ar.Continue()
	require.NotNil(t, cmd)
	tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
	require.True(t, ok)
	m.Update(tick)
	m.View()
}

func TestScrollOnSendAnimatesAndManualControlKeepsReplyRoom(t *testing.T) {
	m := newSendScroll(t)
	m.SetScrollOnSend(true)
	addSentPrompt(m, "local", "prompt")
	from := m.scrollOffset
	m.ScrollToSentInput("local")
	require.Equal(t, from, m.scrollOffset, "receipt must not jump")
	require.True(t, m.scrollOnSend.transition.Running())
	m.View()
	require.Equal(t, from, m.scrollOffset, "repeated composition cannot advance the clock")
	for range 10 {
		tickSentScroll(t, m)
	}
	require.Greater(t, m.scrollOffset, from)
	middle := m.scrollOffset
	settleSentScroll(t, m)
	top, padding := m.scrollOffset, m.scrollOnSend.padding
	require.Greater(t, top, middle)
	for range 30 {
		m.scrollByWheel(-1)
		m.View()
	}
	require.Equal(t, padding, m.scrollOnSend.padding, "history navigation retains room")
	for range 60 {
		m.scrollByWheel(1)
		m.View()
	}
	require.Equal(t, top, m.scrollOffset)
	require.Equal(t, padding, m.scrollOnSend.padding)
	require.Nil(t, m.scrollOnSend.target, "returning does not recapture navigation")
}

func TestScrollOnSendAnimationCanBeInterrupted(t *testing.T) {
	m := newSendScroll(t)
	m.SetScrollOnSend(true)
	addSentPrompt(m, "local", "prompt")
	m.ScrollToSentInput("local")
	for range 8 {
		tickSentScroll(t, m)
	}
	m.scrollUp()
	offset := m.scrollOffset
	require.False(t, m.scrollOnSend.transition.Running())
	m.View()
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "short reply"))
	m.View()
	require.Equal(t, offset, m.scrollOffset)
	require.Positive(t, m.scrollOnSend.padding)
}

func TestScrollOnSendFadeExemptionFollowsOnlyAcceptedPrompt(t *testing.T) {
	m := newSendScroll(t)
	m.SetScrollOnSend(true)
	prompt := addSentPrompt(m, "local", "prompt")
	m.ScrollToSentInput("local")
	settleSentScroll(t, m)
	start, end := m.sentInputFadeRows()
	index := m.messagePointerIndex(prompt)
	require.Equal(t, m.slotOffset(index)-m.scrollOffset, start)
	require.Equal(t, m.slotBase[index], end-start)
	m.scrollByWheel(-3)
	m.View()
	moved, _ := m.sentInputFadeRows()
	require.Equal(t, start+6, moved)
	m.SetScrollOnSend(false)
	m.View()
	start, end = m.sentInputFadeRows()
	require.Zero(t, start)
	require.Zero(t, end)
}

func TestScrollOnSendPauseResumeAndReplyGrowthAfterManualControl(t *testing.T) {
	m := newSendScroll(t)
	m.SetScrollOnSend(true)
	addSentPrompt(m, "local", "prompt")
	m.ScrollToSentInput("local")
	for range 8 {
		tickSentScroll(t, m)
	}
	offset := m.scrollOffset
	m.StopAnimations()
	require.False(t, m.scrollOnSend.transition.Running())
	m.ResumeAnimations()
	require.True(t, m.scrollOnSend.transition.Running())
	require.Equal(t, offset, m.scrollOffset, "resume does not navigate by itself")
	settleSentScroll(t, m)
	top, padding := m.scrollOffset, m.scrollOnSend.padding
	m.scrollPageUp()
	offset = m.scrollOffset
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "short reply"))
	m.View()
	require.Equal(t, offset, m.scrollOffset, "growth cannot recapture the reader")
	require.Positive(t, m.scrollOnSend.padding)
	require.Less(t, m.scrollOnSend.padding, padding)
	m.scrollToBottom()
	m.View()
	require.InDelta(t, top, m.scrollOffset, 1, "reply growth preserves the prompt anchor across separator remeasurement")
	m.AppendToLastMessage("root", strings.Repeat("\n\nmore reply", 30))
	m.View()
	require.Zero(t, m.scrollOnSend.padding)
	require.Nil(t, m.scrollOnSend.reserve)
	require.False(t, m.userHasScrolled)
}
