package messages

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestWorkPreviewRetiresAtSemanticBoundaryNotPartialArguments(t *testing.T) {
	for _, boundary := range []string{"prose", "commit", "settle"} {
		t.Run(boundary, func(t *testing.T) {
			m := newSpinnerTestModel(t)
			m.SetCompactWorkLog(true)
			m.AddUserMessage("request")
			m.SetWorkStatus(true, 0)
			m.AppendReasoningContent("s", "r", "root", "one\n\ntwo")
			m.syncWorkProjection()
			work := firstWorkBlock(m)
			require.NotNil(t, work)
			call := tools.ToolCall{ID: "tool", Function: tools.FunctionCall{Name: "probe", Arguments: `{`}}
			m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "probe"}, types.ToolStatusPending)
			m.AppendReasoningContent("s", "r", "root", " still thinking")
			call.Function.Arguments = `}`
			m.AddOrUpdateToolCall("root", call, tools.Tool{}, types.ToolStatusPending)
			child := m.views[1].(*reasoningblock.Model)
			require.True(t, child.ReasoningActive(), "partial args can coexist with reasoning")
			require.Contains(t, ansi.Strip(work.View()), "still thinking")
			// An unrelated canonical completion must not retire this message.
			other := runtime.MessageAdded("s", session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, MessageID: "older", Presentation: []chat.AssistantPart{}}), "root").(*runtime.MessageAddedEvent)
			m.CompleteAssistant(other)
			require.True(t, child.ReasoningActive())
			switch boundary {
			case "prose":
				m.AppendAssistantContent("s", "r", "root", "answer")
			case "commit":
				event := runtime.MessageAdded("s", session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, MessageID: "r", Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "one\n\ntwo still thinking"}}}), "root").(*runtime.MessageAddedEvent)
				m.CompleteAssistant(event)
			case "settle":
				m.SetWorkStatus(false, 0)
			}
			// Resizing snaps retirement without waiting on wall time and remeasures geometry.
			m.SetSize(81, 30)
			m.syncWorkProjection()
			m.View()
			require.NotContains(t, ansi.Strip(work.View()), "still thinking")
			require.Equal(t, 1, work.Height())
			if boundary != "settle" {
				require.True(t, work.Status().Active)
			}
			work.SetExpandedImmediately(true)
			require.Contains(t, ansi.Strip(work.View()), "still thinking", "full history survives")
			work.SetExpandedImmediately(false)
			if boundary != "settle" {
				m.AppendReasoningContent("s", "next", "root", "resumed reasoning")
				m.syncWorkProjection()
				require.Contains(t, ansi.Strip(firstWorkBlock(m).View()), "resumed reasoning")
			}
		})
	}
}

func TestPreviewRetirementTicksKeepCanonicalGeometryAndScrollOnSend(t *testing.T) {
	for _, send := range []bool{false, true} {
		t.Run(map[bool]string{false: "autofollow", true: "scroll-on-send"}[send], func(t *testing.T) {
			ar := animation.NewRuntimeWithScheduler(&disclosureClock{now: time.Unix(1, 0)})
			m := NewScrollableView(ar, 64, 12, &service.SessionState{}).(*model)
			t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
			m.SetCompactWorkLog(true)
			m.suppressReplyHandoff = true
			m.AddUserMessage("history")
			m.AppendToLastMessage("root", strings.Repeat("old answer\n\n", 20))
			m.SetScrollOnSend(send)
			prompt := addSentPrompt(m, "sent", "new request")
			if send {
				m.ScrollToSentInput("sent")
			}
			m.SetWorkStatus(true, 0)
			m.AppendReasoningContent("s", "r", "root", "private preview")
			m.View()
			work := firstWorkBlock(m)
			require.NotNil(t, work)
			for range 30 {
				if ar.ActiveCount() == 0 {
					break
				}
				acceptAndUpdateHandoffTick(t, ar, m)
				m.View()
			}
			before := m.slotBase[m.workSourceForBlock(work)]
			require.Equal(t, 4, before)
			m.AppendAssistantContent("s", "r", "root", "answer")
			m.View()
			for range 60 {
				acceptAndUpdateHandoffTick(t, ar, m)
				m.View()
				anchor := m.workSourceForBlock(work)
				require.Equal(t, work.Height(), m.slotBase[anchor], "slot geometry follows painted preview on every tick")
				require.Empty(t, work.Rows(), "collapsed fading text never acquires source hit rows")
				if send {
					require.Same(t, prompt, m.scrollOnSend.target)
				}
			}
			require.Equal(t, 1, work.Height())
			require.NotContains(t, ansi.Strip(m.View()), "private preview")
			if !send {
				require.InDelta(t, max(0, m.totalScrollableHeight()-m.height), m.scrollOffset, 1)
			}
			m.SetWorkStatus(false, 0)
			for range 80 {
				if ar.ActiveCount() == 0 {
					break
				}
				acceptAndUpdateHandoffTick(t, ar, m)
				m.View()
			}
			require.Zero(t, ar.ActiveCount(), "retirement and bottom slack leave no idle tick lease")
		})
	}
}

func TestSettledReasoningCannotReappearOnNextTurn(t *testing.T) {
	m := newSpinnerTestModel(t)
	m.SetCompactWorkLog(true)
	m.AddUserMessage("request")
	m.SetWorkStatus(true, 0)
	m.AppendReasoningContent("s", "r", "root", "old reasoning")
	m.syncWorkProjection()
	m.SetWorkStatus(false, 0)
	m.SetSize(81, 30)
	m.View()
	require.Equal(t, 1, firstWorkBlock(m).Height())
	m.SetWorkStatus(true, 0)
	m.View()
	require.Equal(t, 1, firstWorkBlock(m).Height(), "turn activation alone cannot resurrect old reasoning")
	m.AppendReasoningContent("s", "next", "root", "new reasoning")
	m.View()
	require.Contains(t, ansi.Strip(firstWorkBlock(m).View()), "new reasoning")
}
