package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func requireUserWorkGap(t *testing.T, m *model, prompt string) {
	t.Helper()
	m.ensureAllItemsRendered()
	index := -1
	for i, msg := range m.messages {
		if msg.Type == types.MessageTypeUser && msg.Content == prompt {
			index = i
			break
		}
	}
	require.GreaterOrEqual(t, index, 0)
	end := m.slotOffset(index) + m.slotBase[index]
	require.Equal(t, "┃", strings.TrimSpace(ansi.Strip(m.renderedLineAt(end-1))), "bottom padding belongs to the rendered user bubble")
	require.Empty(t, strings.TrimSpace(ansi.Strip(m.renderedLineAt(end))), "one blank terminal row follows the bubble")
	require.Contains(t, ansi.Strip(m.renderedLineAt(end+1)), "Work", "the header immediately follows that blank row")
}

func TestUserWorkGapLive(t *testing.T) {
	for _, kind := range []string{"reasoning", "tool"} {
		t.Run(kind, func(t *testing.T) {
			m := newSpinnerTestModel(t)
			t.Cleanup(m.StopAnimations)
			m.SetSize(80, 40)
			m.SetCompactWorkLog(true)
			m.suppressReplyHandoff = true
			m.AddUserMessage("request")
			m.SetWorkStatus(true, 0)
			m.AppendAssistantContent("s", "one", "root", "firstanswer")
			require.NotContains(t, ansi.Strip(m.View()), "Working")
			if kind == "reasoning" {
				m.AppendReasoningContent("s", "one", "root", "privatework")
			} else {
				m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "probe", Arguments: "{}"}}, tools.Tool{Name: "probe"}, types.ToolStatusRunning)
			}
			requireUserWorkGap(t, m, "request")
			m.SetWorkStatus(false, 0)
			requireUserWorkGap(t, m, "request")
		})
	}
}

func TestUserWorkGapPlainOnlyHasNoExtraSeparator(t *testing.T) {
	m := newSpinnerTestModel(t)
	t.Cleanup(m.StopAnimations)
	m.SetSize(80, 40)
	m.SetCompactWorkLog(true)
	m.suppressReplyHandoff = true
	m.AddUserMessage("request")
	m.SetWorkStatus(true, 0)
	m.AppendAssistantContent("s", "one", "root", "plainanswer")
	require.NotContains(t, ansi.Strip(m.View()), "Working")
	m.AppendAssistantContent("s", "one", "root", " more")
	require.NotContains(t, ansi.Strip(m.View()), "Working")
	m.SetWorkStatus(false, 0)
	m.ensureAllItemsRendered()
	require.Empty(t, m.transientWork)
	require.False(t, m.slotSep[0], "ordinary assistant action row resumes owning the gap")
	end := m.slotOffset(0) + m.slotBase[0]
	require.Empty(t, strings.TrimSpace(ansi.Strip(m.renderedLineAt(end))))
	require.Contains(t, ansi.Strip(m.renderedLineAt(end+1)), "plainanswer")
}

func TestUserWorkGapReplayAndExpanded(t *testing.T) {
	sess := session.New(session.WithID("s"))
	sess.Messages = []session.Item{
		session.NewMessageItem(session.UserMessage("request")),
		session.NewMessageItem(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "answer", ReasoningContent: "privatework"})),
	}
	m := newSpinnerTestModel(t)
	t.Cleanup(m.StopAnimations)
	m.SetSize(80, 40)
	m.SetCompactWorkLog(true)
	BeginReplay(m, PrepareReplay(sess), nil)
	finishPreparedReplay(t, m)
	requireUserWorkGap(t, m, "request")
	for _, block := range m.workBlocks {
		block.SetExpandedImmediately(true)
	}
	m.invalidateAllItems()
	requireUserWorkGap(t, m, "request")
}

func TestUserWorkGapDuringSendScrollAndActivityHandoff(t *testing.T) {
	m := newSendScroll(t)
	m.SetCompactWorkLog(true)
	m.SetScrollOnSend(true)
	addSentPrompt(m, "local", "request")
	m.ScrollToSentInput("local")
	m.SetWorkStatus(true, 0)
	m.AppendAssistantContent("s", "one", "root", "visibleanswer")
	m.View()
	require.True(t, m.scrollOnSend.transition.Running())
	offsets := map[int]bool{}
	assertFrame := func() {
		t.Helper()
		frame := strings.Split(ansi.Strip(m.View()), "\n")
		offsets[m.scrollOffset] = true
		for y, row := range frame {
			if strings.Contains(row, "Working") && y >= 2 {
				require.Empty(t, strings.TrimSpace(strings.TrimSuffix(frame[y-1], "│")))
				require.Equal(t, "┃", strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(frame[y-2]), "│")), "painted header stays one row below the bubble")
			}
		}
		if strings.Contains(strings.Join(frame, "\n"), "Working") {
			requireUserWorkGap(t, m, "request")
		}
	}
	for range 5 {
		tickSentScroll(t, m)
		assertFrame()
	}
	m.AppendReasoningContent("s", "one", "root", "privatework")
	for range 100 {
		assertFrame()
		if !m.scrollOnSend.transition.Running() && !m.replyHandoff.Active() {
			break
		}
		tickSentScroll(t, m)
	}
	require.False(t, m.scrollOnSend.transition.Running())
	require.False(t, m.replyHandoff.Active())
	require.Greater(t, len(offsets), 3, "smooth scroll still traverses intermediate rows")
	requireWorkBefore(t, m, "Working", "visibleanswer")
}
