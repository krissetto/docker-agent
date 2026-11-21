package messages

import (
	"fmt"
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

func TestWorkPreviewPersistsAcrossSemanticBoundariesAndPartialArguments(t *testing.T) {
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
			// Resizing remeasures the retained preview without live padding.
			m.SetSize(81, 30)
			m.syncWorkProjection()
			m.View()
			require.Contains(t, ansi.Strip(work.View()), "still thinking")
			preview, _ := child.PreviewLines()
			require.Equal(t, 1+len(preview), work.Height())
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

func TestRetainedPreviewKeepsCanonicalGeometryAndScrollOnSend(t *testing.T) {
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
			require.Equal(t, 2, work.Height())
			require.Contains(t, ansi.Strip(m.View()), "private preview")
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
			require.Zero(t, ar.ActiveCount(), "retained preview and bottom slack leave no idle tick lease")
		})
	}
}

func TestSettledReasoningRemainsInSameExchangeWhenWorkResumes(t *testing.T) {
	m := newSpinnerTestModel(t)
	m.SetCompactWorkLog(true)
	m.AddUserMessage("request")
	m.SetWorkStatus(true, 0)
	m.AppendReasoningContent("s", "r", "root", "old reasoning")
	m.syncWorkProjection()
	m.SetWorkStatus(false, 0)
	m.SetSize(81, 30)
	m.View()
	require.Equal(t, 2, firstWorkBlock(m).Height())
	m.SetWorkStatus(true, 0)
	m.View()
	require.Equal(t, 2, firstWorkBlock(m).Height(), "turn activation alone keeps the existing preview")
	m.AppendReasoningContent("s", "next", "root", "new reasoning")
	m.View()
	require.Contains(t, ansi.Strip(firstWorkBlock(m).View()), "new reasoning")
}

func TestToolBearingCompletionsKeepPreviewAfterSettlement(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		for _, boundary := range []string{"prose", "settle"} {
			t.Run(fmt.Sprintf("resumed=%v/%s", resumed, boundary), func(t *testing.T) {
				ar := animation.NewRuntimeWithScheduler(&replyHandoffScheduler{now: time.Unix(1, 0)})
				m := NewScrollableView(ar, 80, 40, &service.SessionState{}).(*model)
				t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
				m.SetCompactWorkLog(true)
				m.AddUserMessage("request")
				m.SetWorkStatus(true, 0)
				m.AppendReasoningContent("s", "r", "root", "one\n\ntwo\n\nthree")
				m.View()
				work := firstWorkBlock(m)
				child := m.views[1].(*reasoningblock.Model)
				tickFor := func(d time.Duration) {
					deadline := ar.Now() + d
					for ar.Now() < deadline && ar.ActiveCount() > 0 {
						acceptAndUpdateHandoffTick(t, ar, m)
						m.View()
					}
				}
				assertLive := func() {
					t.Helper()
					tickFor(500 * time.Millisecond)
					require.True(t, work.Status().Active)
					require.True(t, child.ReasoningActive())
					require.Equal(t, 4, work.Height(), "tools preserve the live preview")
					frame := ansi.Strip(work.View())
					for _, line := range []string{"one", "two", "three"} {
						require.Contains(t, frame, line)
					}
				}
				assertLive()
				calls := make([]tools.ToolCall, 3)
				for round := range calls {
					call := tools.ToolCall{ID: fmt.Sprintf("call-%d", round), Function: tools.FunctionCall{Name: "probe", Arguments: `{}`}}
					calls[round] = call
					m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "probe"}, types.ToolStatusPending)
					m.View()
					assertLive()
					m.AddOrUpdateToolCall("root", call, tools.Tool{}, types.ToolStatusRunning)
					m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: call.ID, Response: "done"}, types.ToolStatusCompleted)
					m.View()
					assertLive()
				}
				for range 3 {
					m.CompleteAssistant(runtime.MessageAdded("s", session.NewAgentMessage("root", &chat.Message{
						Role: chat.MessageRoleAssistant, MessageID: "r", ToolCalls: calls,
						Presentation: []chat.AssistantPart{{Type: chat.AssistantPartToolCall, ToolCallID: calls[2].ID}},
					}), "root").(*runtime.MessageAddedEvent))
					m.View()
					assertLive()
				}
				want := "one\n\ntwo\n\nthree"
				if resumed {
					for _, chunk := range []string{"", "after", " ", "tools"} {
						m.AppendReasoningContent("s", "r", "root", chunk)
					}
					want += "\n\nafter tools"
				}
				require.Equal(t, want, child.Reasoning())
				require.Equal(t, want, m.messages[1].Content)
				m.View()
				tickFor(500 * time.Millisecond)
				require.Equal(t, 4, work.Height())
				if boundary == "prose" {
					m.AppendAssistantContent("s", "answer", "root", "final answer")
				} else {
					m.SetWorkStatus(false, 0)
				}
				m.View()
				require.False(t, child.ReasoningActive())
				require.Equal(t, 4, work.Height(), "three actual lines retain their geometry")
				tickFor(175 * time.Millisecond)
				require.Equal(t, 4, work.Height(), "preview remains after a prose boundary")
				tickFor(200 * time.Millisecond)
				require.Equal(t, 4, work.Height(), "displayable reasoning remains after animations finish")
				m.SetWorkStatus(false, 0)
				tickFor(time.Second)
				require.Equal(t, 4, work.Height())
				if resumed {
					require.Contains(t, ansi.Strip(work.View()), "after tools")
				}
			})
		}
	}
}
