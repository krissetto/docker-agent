package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func requireWorkBefore(t *testing.T, m *model, label string, answers ...string) {
	t.Helper()
	m.ensureAllItemsRendered()
	var rows []string
	for line := 0; line < m.layoutTotal(); line++ {
		rows = append(rows, m.renderedLineAt(line))
	}
	text := ansi.Strip(strings.Join(rows, "\n"))
	header := strings.Index(text, label)
	require.GreaterOrEqual(t, header, 0, text)
	last := header
	for _, answer := range answers {
		at := strings.Index(text, answer)
		require.Greater(t, at, last, text)
		require.Equal(t, 1, strings.Count(text, answer), text)
		last = at
	}
}

func TestWorkOrderLiveProseBeforeActivity(t *testing.T) {
	for _, kind := range []string{"reasoning", "tool"} {
		t.Run(kind, func(t *testing.T) {
			m := newSpinnerTestModel(t)
			t.Cleanup(m.StopAnimations)
			m.SetCompactWorkLog(true)
			m.suppressReplyHandoff = true
			m.AddUserMessage("request")
			m.SetWorkStatus(true, 0)
			m.AppendAssistantContent("s", "one", "root", "firstanswer")
			require.Contains(t, ansi.Strip(m.View()), "firstanswer")
			require.NotContains(t, ansi.Strip(m.View()), "Working")
			if kind == "reasoning" {
				m.AppendReasoningContent("s", "one", "root", "privatework")
			} else {
				m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "probe", Arguments: "{}"}}, tools.Tool{Name: "probe"}, types.ToolStatusRunning)
			}
			requireWorkBefore(t, m, "Working", "firstanswer")
			m.BreakMessageGroup()
			m.AppendAssistantContent("s", "two", "root", "secondanswer")
			requireWorkBefore(t, m, "Working", "firstanswer", "secondanswer")
			m.SetWorkStatus(false, 0)
			requireWorkBefore(t, m, "Work", "firstanswer", "secondanswer")
			for _, block := range m.workBlocks {
				block.SetExpandedImmediately(true)
			}
			m.invalidateAllItems()
			requireWorkBefore(t, m, "Work", "firstanswer", "secondanswer")
		})
	}
}

func TestWorkOrderPlainOnlyNeverShowsWorking(t *testing.T) {
	m := newSpinnerTestModel(t)
	t.Cleanup(m.StopAnimations)
	m.SetCompactWorkLog(true)
	m.suppressReplyHandoff = true
	m.AddUserMessage("request")
	m.SetWorkStatus(true, 0)
	m.AppendAssistantContent("s", "one", "root", "plainanswer")
	require.Contains(t, ansi.Strip(m.View()), "plainanswer")
	require.NotContains(t, ansi.Strip(m.View()), "Working")
	m.SetWorkStatus(false, 0)
	m.ensureAllItemsRendered()
	require.Empty(t, m.workBlocks)
	require.Empty(t, m.transientWork)
	require.Contains(t, ansi.Strip(m.View()), "plainanswer")
	require.NotContains(t, ansi.Strip(m.View()), "Working")
}

func TestWorkOrderCommittedAndReplayCombinedParts(t *testing.T) {
	for _, kind := range []string{"reasoning", "tool"} {
		for _, replay := range []bool{false, true} {
			t.Run(kind+map[bool]string{true: "/replay", false: "/committed"}[replay], func(t *testing.T) {
				content := &chat.Message{Role: chat.MessageRoleAssistant, MessageID: "one", Content: "firstanswer"}
				if kind == "reasoning" {
					content.ReasoningContent = "privatework"
					content.Presentation = []chat.AssistantPart{{Type: chat.AssistantPartContent, Text: "firstanswer"}, {Type: chat.AssistantPartReasoning, Text: "privatework"}}
				} else {
					content.ToolCalls = []tools.ToolCall{{ID: "call", Function: tools.FunctionCall{Name: "probe", Arguments: "{}"}}}
				}
				sess := session.New(session.WithID("s"))
				sess.Messages = []session.Item{session.NewMessageItem(session.UserMessage("request")), session.NewMessageItem(session.NewAgentMessage("root", content))}
				m := newSpinnerTestModel(t)
				t.Cleanup(m.StopAnimations)
				m.SetCompactWorkLog(true)
				m.suppressReplyHandoff = true
				if replay {
					BeginReplay(m, PrepareReplay(sess), nil)
					finishPreparedReplay(t, m)
				} else {
					m.AddUserMessage("request")
					m.SetWorkStatus(true, 0)
					m.AppendAssistantContent("s", "one", "root", "firstanswer")
					if kind == "reasoning" {
						m.AppendReasoningContent("s", "one", "root", "privatework")
					} else {
						m.AddOrUpdateToolCall("root", content.ToolCalls[0], tools.Tool{Name: "probe"}, types.ToolStatusRunning)
					}
					m.CompleteAssistant(runtime.MessageAdded("s", session.NewAgentMessage("root", content), "root").(*runtime.MessageAddedEvent))
				}
				requireWorkBefore(t, m, "Work", "firstanswer")
				require.Equal(t, "firstanswer", content.Content)
			})
		}
	}
}

func TestWorkOrderTurnBoundariesAndDisabledGrouping(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{true: "compact", false: "plain"}[enabled], func(t *testing.T) {
			m := newSpinnerTestModel(t)
			t.Cleanup(m.StopAnimations)
			m.SetCompactWorkLog(enabled)
			m.suppressReplyHandoff = true
			m.AddUserMessage("firstrequest")
			m.AppendAssistantContent("s", "one", "root", "firstanswer")
			m.AppendReasoningContent("s", "one", "root", "firstwork")
			m.AddUserMessage("secondrequest")
			m.AppendAssistantContent("s", "two", "root", "secondanswer")
			m.AppendReasoningContent("s", "two", "root", "secondwork")
			sourceOrder := append([]*types.Message(nil), m.messages...)
			m.ensureAllItemsRendered()
			require.Equal(t, sourceOrder, m.messages)
			if enabled {
				require.Len(t, m.workHeaderBySource, 2)
				for _, exchange := range m.workProjection.exchanges {
					if len(exchange.work) > 0 && len(exchange.response) > 0 {
						require.Less(t, m.slotOffset(exchange.work[0]), m.slotOffset(exchange.response[0]))
					}
				}
				require.Less(t, m.slotOffset(2), m.slotOffset(3))
			} else {
				require.Empty(t, m.workHeaderBySource)
				require.Less(t, m.slotOffset(1), m.slotOffset(2))
			}
			m.SetCompactWorkLog(!enabled)
			m.ensureAllItemsRendered()
			require.Equal(t, sourceOrder, m.messages)
		})
	}
}

func TestLayoutIndexDisplayOrder(t *testing.T) {
	var index layoutIndex
	index.setOrder([]int{0, 2, 1, 3})
	index.Rebuild([]int{3, 5, 2, 1})
	require.Equal(t, 3, index.Offset(2))
	require.Equal(t, 5, index.Offset(1))
	require.Equal(t, 2, index.Find(3))
	require.Equal(t, 1, index.Find(5))
	index.Set(2, 4)
	require.Equal(t, 7, index.Offset(1))
	require.Equal(t, 4, index.Height(2))
}

func TestWorkOrderLiveFrameKeepsProseDuringActivityHandoff(t *testing.T) {
	m := newSpinnerTestModel(t)
	t.Cleanup(m.StopAnimations)
	m.SetSize(90, 40)
	m.SetCompactWorkLog(true)
	m.AddUserMessage("request")
	m.SetWorkStatus(true, 0)
	m.AppendAssistantContent("s", "one", "root", "visibleanswer")
	require.NotContains(t, ansi.Strip(m.View()), "Working")
	require.Contains(t, ansi.Strip(m.View()), "visibleanswer")
	m.AppendReasoningContent("s", "one", "root", "privatework")
	for range 20 {
		frame := ansi.Strip(m.View())
		require.Contains(t, frame, "visibleanswer")
		if strings.Contains(frame, "Working") {
			require.Less(t, strings.Index(frame, "Working"), strings.Index(frame, "visibleanswer"))
		}
		if !m.replyHandoff.Active() {
			break
		}
		acceptAndUpdateHandoffTick(t, m.ar, m)
	}
	requireWorkBefore(t, m, "Working", "visibleanswer")
}
