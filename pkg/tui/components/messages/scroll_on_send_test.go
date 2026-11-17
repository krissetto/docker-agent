package messages

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func newSendScroll(t *testing.T) *model {
	t.Helper()
	ar := animation.NewRuntime()
	m := NewScrollableView(ar, 64, 12, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.addMessage(types.User("history"))
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", strings.Repeat("old reply\n\n", 20)))
	m.ensureAllItemsRendered()
	m.View()
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
			m.View()
			require.Same(t, prompt, m.scrollOnSend.target)
			require.Positive(t, m.scrollOnSend.padding)
			index := m.messagePointerIndex(prompt)
			require.InDelta(t, m.slotOffset(index), m.scrollOffset, 1)
			m.Update(scrollToBottomMsg{})
			require.Same(t, prompt, m.scrollOnSend.target, "stale automatic bottom command cannot override new send")
			m.scrollUp()
			offset := m.scrollOffset
			require.Nil(t, m.scrollOnSend.target)
			m.View()
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
	m.View()
	offset, padding := m.scrollOffset, m.scrollOnSend.padding
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "short reply"))
	m.View()
	require.Equal(t, offset, m.scrollOffset)
	require.Less(t, m.scrollOnSend.padding, padding)
	m.AppendToLastMessage("root", strings.Repeat("\n\nmore reply", 30))
	m.View()
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
	m.View()
	index := m.messagePointerIndex(prompt)
	end := m.slotOffset(index) + m.slotBase[index]
	require.Equal(t, end+max(3, m.height/3)-m.height, m.scrollOffset)
	m.SetSize(40, 18)
	m.View()
	end = m.slotOffset(index) + m.slotBase[index]
	require.Equal(t, max(m.slotOffset(index)-1, end+max(3, m.height/3)-m.height), m.scrollOffset)
	m.SetSize(40, 5)
	m.View()
	require.Nil(t, m.scrollOnSend.target)
	require.Zero(t, m.scrollOnSend.padding)
	require.Equal(t, max(0, m.totalScrollableHeight()-m.height), m.scrollOffset)
}

func TestScrollOnSendDoesNotInferOrNavigateDuringReplay(t *testing.T) {
	m := newSendScroll(t)
	m.SetScrollOnSend(true)
	addSentPrompt(m, "remote", "not my send")
	m.View()
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
	m.View()
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
	m.View()
	offset, padding := m.scrollOffset, m.scrollOnSend.padding
	m.SetWorkStatus(false, 0)
	m.View()
	require.Equal(t, offset, m.scrollOffset)
	require.Equal(t, padding, m.scrollOnSend.padding)
	m.ScrollToSentInput("new")
	require.Equal(t, []string{"new"}, m.scrollOnSend.pending)
	addSentPrompt(m, "new", "second prompt")
	m.View()
	require.Equal(t, "new", m.scrollOnSend.target.TurnID)
	require.Greater(t, m.scrollOffset, offset)
}
