package messages

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/subagentindex"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestPendingAgentReceiptPromotesStableDisclosureInConsumptionOrder(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "restored"}[restored], func(t *testing.T) {
			ar := animation.NewRuntimeWithScheduler(&disclosureClock{now: time.Unix(1, 0)})
			index := subagentindex.New()
			tree := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "node", SessionID: "child-session", Agent: "worker", Name: "Worker"}}}}
			index.Reset(tree)
			m := newModel(ar, 100, 200, &service.SessionState{}, index)
			t.Cleanup(m.StopAnimations)
			input := session.UserMessage("literal steering payload")
			input.TurnID, input.Pending = "steering", true
			input.InputOrigin, input.InputMode, input.SenderID, input.SenderName = session.InputOriginAgent, "steer", "child-session", "worker"
			if restored {
				sess := session.New()
				sess.SetSubagentTree(&tree)
				sess.AddMessage(input)
				m.LoadFromSession(sess, nil)
			} else {
				m.AddInputMessage(input, 0)
			}
			require.Equal(t, 1, m.MessageTypeCount(types.MessageTypeAgentInput))
			require.Contains(t, ansi.Strip(m.View()), "accepted · awaiting consumption")
			require.NotContains(t, ansi.Strip(m.View()), "sent a message")
			card, view := m.messages[0], m.views[0]
			m.AddInputMessage(input, 0)
			require.Len(t, m.messages, 1, "exact acceptance replay does not duplicate receipt")
			m.focused, m.selectedMessageIndex = true, 0
			m.handleKeyPress(tea.KeyPressMsg{Code: tea.KeyEnter})
			for m.itemNeedsTick(0) {
				tick, ok := ar.Accept(ar.Continue()().(animation.TickMsg))
				require.True(t, ok)
				m.Update(tick)
				m.View()
			}
			require.Contains(t, ansi.Strip(m.View()), input.Message.Content)
			m.AddUserMessage("intervening history")
			m.AddAssistantMessage("root", "")
			input.Pending = false
			m.AddInputMessage(input, 1)
			require.Same(t, card, m.messages[1], "promotion retains card identity")
			require.Same(t, view, m.views[1], "promotion retains disclosure view")
			require.Equal(t, 1, m.selectedMessageIndex)
			require.Equal(t, types.MessageTypeSpinner, m.messages[2].Type)
			m.AddInputMessage(input, 1)
			require.Equal(t, 1, m.MessageTypeCount(types.MessageTypeAgentInput))
			out := ansi.Strip(m.View())
			require.NotContains(t, out, "awaiting consumption")
			require.Contains(t, out, "Worker (node) sent a message v")
			require.Contains(t, out, "literal steering payload", "expanded state survives promotion")
			require.Equal(t, 1, strings.Count(out, "literal steering payload"))
			m.RemovePendingInput(input.TurnID)
			require.Equal(t, 1, m.MessageTypeCount(types.MessageTypeAgentInput), "late cancellation cannot remove consumed card")
		})
	}
}

func TestPendingAgentReceiptWithdrawsWithoutRevivingOrShiftingOtherCards(t *testing.T) {
	m := newSpinnerTestModel(t)
	for _, id := range []string{"first", "second"} {
		input := session.UserMessage("same payload")
		input.TurnID, input.Pending, input.InputOrigin = id, true, session.InputOriginAgent
		m.AddInputMessage(input, 0)
	}
	require.Equal(t, 2, m.MessageTypeCount(types.MessageTypeAgentInput), "concurrent equal content uses identity, not body")
	second := m.messages[1]
	m.selectedMessageIndex = 1
	m.RemovePendingInput("first")
	m.RemovePendingInput("first")
	require.Len(t, m.messages, 1)
	require.Same(t, second, m.messages[0])
	require.Zero(t, m.selectedMessageIndex)
}

func TestRestoredPendingReceiptPromotionPreservesLiveAssistantBoundary(t *testing.T) {
	m := newSpinnerTestModel(t)
	sess := session.New()
	input := session.UserMessage("steering")
	input.TurnID, input.Pending, input.InputOrigin = "pending", true, session.InputOriginAgent
	sess.AddMessage(input)
	sess.AddMessage(session.UserMessage("saved history"))
	m.LoadFromSession(sess, nil)
	m.AppendToLastMessage("root", "live assistant")
	input.Pending = false
	m.AddInputMessage(input, 2)
	require.Equal(t, 1, m.loadedMessageCount, "moved receipt cannot capture a live tail as loaded history")
	require.Equal(t, "live assistant", m.messages[1].Content)
	require.Equal(t, types.MessageTypeAgentInput, m.messages[2].Type)
	m.AppendToLastMessage("root", "new live tail")
	m.AppendToLastMessage("root", " continued")
	require.Equal(t, "new live tail continued", m.messages[3].Content)
}

func TestPendingReceiptPreservesDeferredAssistantAndWithdrawal(t *testing.T) {
	for _, withdrawn := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "withdrawn"}[withdrawn], func(t *testing.T) {
			m := newAttachTestModel(t, taskItem())
			m.AppendToLastMessage("root", "exact ")
			assistant := m.messages[1]
			m.userHasScrolled = true
			m.AppendToLastMessage("root", "deferred ")
			receipt := session.UserMessage("receipt")
			receipt.TurnID, receipt.Pending, receipt.InputOrigin = "incoming", true, session.InputOriginAgent
			m.AddInputMessage(receipt, 1)
			m.AppendToLastMessage("root", "tail")
			require.Same(t, assistant, m.messages[m.deferredTailIndex], "deferred index still names streamed assistant")
			if withdrawn {
				m.RemovePendingInput(receipt.TurnID)
			}
			require.Same(t, assistant, m.messages[m.deferredTailIndex], "withdrawal cannot retarget deferred assistant bytes")
			m.CompleteAssistant(runtime.MessageAddedAt("s", session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: "exact deferred tail"}), "root", 2).(*runtime.MessageAddedEvent))
			require.Equal(t, 1, m.MessageTypeCount(types.MessageTypeAssistant))
			require.Equal(t, "exact deferred tail", assistant.Content)
			require.Nil(t, m.deferredTail)
		})
	}
}

func TestPendingReceiptPreservesReasoningAndTextCommit(t *testing.T) {
	m := newAttachTestModel(t, taskItem())
	m.AppendReasoning("root", "partial reasoning")
	receipt := session.UserMessage("receipt")
	receipt.TurnID, receipt.Pending, receipt.InputOrigin = "incoming", true, session.InputOriginAgent
	m.AddInputMessage(receipt, 1)
	m.AppendReasoning("root", " continued")
	m.AppendToLastMessage("root", "partial answer")
	receipt.TurnID = "second"
	m.AddInputMessage(receipt, 2)
	m.CompleteAssistant(runtime.MessageAddedAt("s", session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: "canonical answer", ReasoningContent: "canonical reasoning"}), "root", 3).(*runtime.MessageAddedEvent))
	require.Equal(t, 1, m.MessageTypeCount(types.MessageTypeAssistantReasoningBlock))
	require.Equal(t, 1, m.MessageTypeCount(types.MessageTypeAssistant))
	var answer, reasoning string
	for _, msg := range m.messages {
		if msg.Type == types.MessageTypeAssistant {
			answer = msg.Content
		}
		if msg.Type == types.MessageTypeAssistantReasoningBlock {
			reasoning = msg.Content
		}
	}
	require.Equal(t, "canonical answer", answer)
	require.Equal(t, "canonical reasoning", reasoning)
	require.Equal(t, 2, m.MessageTypeCount(types.MessageTypeAgentInput))
}
