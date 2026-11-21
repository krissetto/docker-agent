package messages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestWhitespaceReplyStreamingReconcileAndReplay(t *testing.T) {
	for _, compact := range []bool{false, true} {
		for _, text := range []string{"", " ", "\t", "\n", " \t\n\r"} {
			m := newAttachTestModel(t)
			m.SetCompactWorkLog(compact)
			m.AddUserMessage("request")
			m.View()
			baseline := m.geom.Total()
			m.AppendAssistantContent("s", "reply", "root", text)
			m.View()
			require.Equal(t, baseline, m.geom.Total(), "whitespace must not add rows")
			require.False(t, m.actionRowEligible(1))
			require.False(t, m.isSelectableMessage(1))
			require.Equal(t, -1, m.findLastAssistantMessage())
			require.False(t, m.displayNeedsSeparator(0))
			require.Zero(t, m.slotBase[1])
			require.Equal(t, text, m.messages[1].Content)
			m.AppendAssistantContent("s", "reply", "root", "\tanswer")
			m.View()
			require.Equal(t, text+"\tanswer", m.messages[1].Content)
			require.Positive(t, m.slotBase[1])
			require.Equal(t, "reply", m.messages[1].MessageID)
			m.ReconcileAssistantContent("s", "reply", "root", text)
			m.View()
			require.Equal(t, baseline, m.geom.Total(), "canonical whitespace removes old prose geometry")
			require.Equal(t, text, m.messages[1].Content)
			m.ReconcileAssistantContent("s", "reply", "root", "")
			m.View()
			require.Equal(t, baseline, m.geom.Total())

			for _, ordered := range []bool{false, true} {
				sess := session.New(session.WithID("s"))
				sess.Messages = []session.Item{{Message: session.UserMessage("request")}, {Message: session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, MessageID: "reply", Content: text})}}
				if ordered {
					sess.Messages[1].Message.Message.Presentation = []chat.AssistantPart{{Type: chat.AssistantPartContent, Text: text}}
				}
				replay := newAttachTestModel(t)
				replay.SetCompactWorkLog(compact)
				replay.LoadFromSession(sess, nil)
				replay.View()
				require.Equal(t, baseline, replay.geom.Total())
				require.Equal(t, text, sess.Messages[1].Message.Message.Content)
			}
		}
	}
}

func TestEmptyIncomingAgentPresentationIsProvenanceAware(t *testing.T) {
	for _, compact := range []bool{false, true} {
		m := newAttachTestModel(t)
		m.SetCompactWorkLog(compact)
		m.AddUserMessage("request")
		m.View()
		baseline := m.geom.Total()
		input := session.UserMessage(" \t\n")
		input.InputOrigin = session.InputOriginAgent
		input.TurnID = "incoming"
		m.AddInputMessage(input, -1)
		m.View()
		require.Equal(t, baseline, m.geom.Total())
		require.Len(t, m.messages, 2, "presentation suppression retains source identity")
		require.Equal(t, "incoming", m.messages[1].TurnID)
		human := types.User("")
		require.False(t, human.PresentationHidden())
		input.Pending = true
		require.False(t, types.Input(input).PresentationHidden())
		input.Pending = false
		input.ReportOutcome = session.ReportOutcomeFinished
		require.False(t, types.Input(input).PresentationHidden())
		media := types.Agent(types.MessageTypeAssistant, "root", "")
		media.AssistantMedia = []types.AssistantMedia{{Fallback: "generated image"}}
		require.False(t, media.PresentationHidden())
	}
}

func TestWhitespaceKeepsReasoningPreviewUntilProseOrCompletion(t *testing.T) {
	m := newSpinnerTestModel(t)
	m.SetCompactWorkLog(true)
	m.AddUserMessage("request")
	m.SetWorkStatus(true, 0)
	m.AppendReasoningContent("s", "reply", "root", "private reasoning")
	child := m.views[1].(*reasoningblock.Model)
	m.AppendAssistantContent("s", "reply", "root", " \t\n")
	m.View()
	require.True(t, child.ReasoningActive(), "whitespace is not a prose boundary")
	require.Zero(t, m.slotBase[2])
	m.AppendAssistantContent("s", "reply", "root", "answer")
	require.False(t, child.ReasoningActive())
	msg := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, MessageID: "reply", Content: " \t\n"})
	m.CompleteAssistant(runtime.MessageAdded("s", msg, "root").(*runtime.MessageAddedEvent))
	m.View()
	require.Zero(t, m.slotBase[2])
	require.Equal(t, " \t\n", m.messages[2].Content)
	require.Nil(t, m.liveTailMsg)
}

func TestHiddenRepliesDoNotOwnSeparatorsOrHitTargets(t *testing.T) {
	for _, compact := range []bool{false, true} {
		m := newAttachTestModel(t)
		m.SetCompactWorkLog(compact)
		m.AddUserMessage("request")
		m.AppendAssistantContent("s", "first", "root", "first answer")
		m.AppendAssistantContent("s", "empty", "root", " \t\n")
		m.AppendAssistantContent("s", "next", "root", "next answer")
		m.ensureAllItemsRendered()
		require.Zero(t, m.slotBase[2])
		require.False(t, m.slotSep[2])
		for line := 0; line < m.geom.Total(); line++ {
			source, _ := m.globalLineToMessageLineCached(line)
			require.NotEqual(t, 2, source, "no transcript row or click belongs to hidden prose")
		}
		m.ReconcileAssistantContent("s", "next", "root", "")
		m.ensureAllItemsRendered()
		require.False(t, m.slotSep[1], "trailing empty rows leave no separator after last visible answer")
		m.AppendAssistantContent("s", "last", "root", "last answer")
		m.ensureAllItemsRendered()
		require.Equal(t, !compact, m.slotSep[1], "separator follows the next visible answer across hidden slots")
	}
}

func TestPendingEmptyAgentReceiptHidesWhenConsumed(t *testing.T) {
	m := newAttachTestModel(t)
	m.AddUserMessage("request")
	m.View()
	baseline := m.geom.Total()
	input := session.UserMessage("")
	input.InputOrigin = session.InputOriginAgent
	input.TurnID = "receipt"
	input.Pending = true
	m.AddInputMessage(input, -1)
	m.View()
	require.Greater(t, m.geom.Total(), baseline)
	input.Pending = false
	m.AddInputMessage(input, -1)
	m.View()
	require.Equal(t, baseline, m.geom.Total())
	require.Len(t, m.messages, 2)
	require.Equal(t, "receipt", m.messages[1].TurnID)
}

func TestScrollOnSendKeepsGapAcrossHiddenTail(t *testing.T) {
	m := newSendScroll(t)
	m.AppendAssistantContent("s", "empty", "root", "\n\t")
	m.SetScrollOnSend(true)
	prompt := addSentPrompt(m, "sent", "new prompt")
	m.ScrollToSentInput("sent")
	settleSentScroll(t, m)
	index := m.messagePointerIndex(prompt)
	require.True(t, m.slotSep[m.separatorOwnerFrom(index-1)])
	require.Equal(t, m.slotOffset(index)-1, m.scrollOffset)
}

func TestDeferredWhitespaceFlipPreservesBytesAndReader(t *testing.T) {
	for _, compact := range []bool{false, true} {
		m := newSendScroll(t)
		m.SetCompactWorkLog(compact)
		m.AppendAssistantContent("s", "empty", "root", " \t\n")
		m.View()
		m.scrollToTop()
		offset := m.scrollOffset
		m.AppendAssistantContent("s", "empty", "root", "\tanswer")
		require.NotEmpty(t, m.deferredTail)
		m.materializeDeferredTail()
		m.View()
		require.Equal(t, " \t\n\tanswer", m.messages[len(m.messages)-1].Content)
		require.Equal(t, offset, m.scrollOffset)
		require.True(t, m.userHasScrolled)
		require.False(t, m.hiddenSlot(len(m.messages)-1))
	}
}

func TestToolWhitespaceProseSeparatorAndHitMap(t *testing.T) {
	m := newAttachTestModel(t)
	call := tools.ToolCall{ID: "probe", Function: tools.FunctionCall{Name: "probe"}}
	m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "probe"}, types.ToolStatusCompleted)
	m.AppendAssistantContent("s", "empty", "root", "\n")
	m.AppendAssistantContent("s", "answer", "root", "answer")
	m.ensureAllItemsRendered()
	require.True(t, m.slotSep[0])
	require.Zero(t, m.slotBase[1])
	require.False(t, m.slotSep[1])
	source, local := m.globalLineToMessageLineCached(m.slotOffset(2) + 1)
	require.Equal(t, 2, source)
	require.Equal(t, 1, local)
}

func TestWhitespaceTailDoesNotChangeStickyWorkOverlay(t *testing.T) {
	m, anchors := stickyWorkHistory(t)
	m.setScrollOffset(m.slotOffset(anchors[0]) + 3)
	before := m.View()
	beforeHeight, beforeOffset := m.layoutTotal(), m.scrollOffset
	m.AppendAssistantContent("s", "empty", "root", strings.Repeat(" \t\n", 3))
	require.Equal(t, before, m.View())
	require.Equal(t, beforeHeight, m.layoutTotal())
	require.Equal(t, beforeOffset, m.scrollOffset)
	_, _, ok := m.stickyWork()
	require.True(t, ok)
}

func TestOrderedWhitespaceCompletionClearsOnlyItsOwnProse(t *testing.T) {
	m := newAttachTestModel(t)
	m.AppendAssistantContent("s", "reply", "root", "stale prose")
	m.AppendAssistantContent("s", "newer", "root", "newer answer")
	msg := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, MessageID: "reply", Content: " \t\n", Presentation: []chat.AssistantPart{{Type: chat.AssistantPartContent, Text: " \t\n"}}})
	m.CompleteAssistant(runtime.MessageAdded("s", msg, "root").(*runtime.MessageAddedEvent))
	m.ensureAllItemsRendered()
	require.Equal(t, " \t\n", m.messages[0].Content)
	require.Zero(t, m.slotBase[0])
	require.Equal(t, "newer answer", m.messages[1].Content)
	require.Positive(t, m.slotBase[1])
}
