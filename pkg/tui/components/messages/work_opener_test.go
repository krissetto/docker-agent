package messages

import (
	"fmt"
	"slices"
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
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func openerIncomingAssignment(content string) *types.Message {
	return &types.Message{Type: types.MessageTypeAgentInput, InputOrigin: session.InputOriginAgent, Content: content}
}

func openerProse(content string) *types.Message {
	return &types.Message{Type: types.MessageTypeAssistant, Content: content}
}

func openerTool() *types.Message {
	return &types.Message{Type: types.MessageTypeToolCall, ToolStatus: types.ToolStatusCompleted}
}

func openerAgentInput(content string) *session.Message {
	input := session.UserMessage(content)
	input.InputOrigin, input.InputMode, input.SenderID, input.SenderName = session.InputOriginAgent, "steer", "child-reviewer", "reviewer"
	return input
}

// openerModel uses a fake clock so reply handoffs and disclosures can be
// driven to rest, as the program loop would, before observing rendered frames.
func openerModel(t *testing.T) *model {
	t.Helper()
	ar := animation.NewRuntimeWithScheduler(&disclosureClock{now: time.Unix(1, 0)})
	m := NewScrollableView(ar, 90, 30, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	return m
}

// openerSettle delivers every pending frame through Update and View until no
// animation lease remains, so in-flight reply handoffs cannot paint frozen rows.
func openerSettle(t *testing.T, ar *animation.Runtime, m *model) {
	t.Helper()
	for range 500 {
		cmd := ar.Continue()
		if cmd == nil {
			break
		}
		tick, ok := ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		m.Update(tick)
		_ = m.View()
	}
	require.Zero(t, ar.ActiveCount(), "animations settle")
	require.False(t, m.replyHandoff.Active(), "reply handoff settles before Work is toggled")
}

// Incoming reports render collapsed: the sender header is the visible opener marker.
const openerHeader = "From reviewer"

func openerCount(plain, text string) int { return strings.Count(plain, text) }

func openerSourceIndex(t *testing.T, m *model, text string) int {
	t.Helper()
	for i, msg := range m.messages {
		if strings.Contains(msg.Content, text) {
			return i
		}
	}
	t.Fatalf("no source contains %q", text)
	return -1
}

func TestSessionOpenerIndexUsesOnlyPublicRows(t *testing.T) {
	pending := openerIncomingAssignment("pending receipt")
	pending.Pending = true
	pendingUser := &types.Message{Type: types.MessageTypeUser, Content: "pending human", Pending: true}
	for _, test := range []struct {
		name  string
		items []*types.Message
		want  int
	}{
		{name: "runtime notice and pending receipt are skipped", items: []*types.Message{{Type: types.MessageTypeRuntimeNotice}, pending, openerIncomingAssignment("assignment")}, want: 2},
		{name: "empty assistant prefix is skipped", items: []*types.Message{{Type: types.MessageTypeAssistant}, openerProse("answer")}, want: 1},
		{name: "pending human input is skipped", items: []*types.Message{pendingUser, openerIncomingAssignment("assignment")}, want: 1},
		{name: "ordinary user opens the session", items: []*types.Message{{Type: types.MessageTypeUser, Content: "hi"}, openerIncomingAssignment("later")}, want: 0},
		{name: "only synthetic rows have no opener", items: []*types.Message{{Type: types.MessageTypeRuntimeNotice}, pending}, want: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, slices.Index(test.items, resolveSessionOpener(test.items, nil)))
		})
	}
}

func TestEstablishedOpenerSurvivesEarlierDeferredProse(t *testing.T) {
	// An empty assistant prefix can gain prose later (deferred stream
	// materialization) without replacing the report already shown as opener.
	prefix := &types.Message{Type: types.MessageTypeAssistant}
	report := openerIncomingAssignment("assignment")
	items := []*types.Message{prefix, report}
	opener := resolveSessionOpener(items, nil)
	require.Same(t, report, opener)
	prefix.Content = "late prose"
	require.Same(t, report, resolveSessionOpener(items, opener), "prose flip must not replace the established opener")
	projection := projectWorkWithVisibleReplies(items, nil, msgtypes.VisibleRepliesLatest, opener)
	require.Equal(t, []int{0, 1}, projection.exchanges[0].response)
	require.Empty(t, projection.exchanges[0].work)
}

func TestOpenerPinsOnlyItsOwnSourceRow(t *testing.T) {
	items := []*types.Message{openerProse("opener reply"), openerProse("same logical reply tail"), openerTool(), openerProse("latest")}
	projection := projectWorkWithVisibleReplies(items, nil, msgtypes.VisibleRepliesLatest, resolveSessionOpener(items, nil))
	exchange := projection.exchanges[0]
	require.Equal(t, []int{0, 3}, exchange.response, "only the opener row is exempt from the limit")
	require.Equal(t, []int{1, 2}, exchange.work, "later consecutive parts follow the normal policy")
}

func TestWorkOpenerStaysOutsideWorkAndLaterReportsFold(t *testing.T) {
	items := []*types.Message{
		openerIncomingAssignment("assignment"),
		openerTool(),
		openerProse("first reply"),
		openerIncomingAssignment("later report"),
		openerTool(),
	}
	projection := projectWorkWithVisibleReplies(items, nil, msgtypes.VisibleRepliesLatest, resolveSessionOpener(items, nil))
	require.Len(t, projection.exchanges, 1)
	exchange := projection.exchanges[0]
	require.Equal(t, -1, exchange.boundary)
	require.Equal(t, []int{0, 2}, exchange.response)
	require.Equal(t, []int{1, 3, 4}, exchange.work)
	require.NotContains(t, projection.bySource, 0, "the opener is never a source of a collapsed Work row")
}

func TestWorkOpenerExemptFromRetentionButOtherRepliesStillLimited(t *testing.T) {
	items := []*types.Message{openerProse("opener reply"), openerTool(), openerProse("middle"), openerTool(), openerProse("latest")}
	groups := map[*types.Message]uint64{items[0]: 1, items[2]: 2, items[4]: 3}
	projection := projectWorkWithVisibleReplies(items, groups, msgtypes.VisibleRepliesLatest, resolveSessionOpener(items, nil))
	exchange := projection.exchanges[0]
	require.Equal(t, []int{0, 4}, exchange.response, "the opener and the latest allowed reply are visible")
	require.Equal(t, []int{1, 2, 3}, exchange.work, "older non-opener replies follow the normal limit")
}

func TestWorkOpenerSkipsRuntimeNoticeAndEmptyAssistantPrefix(t *testing.T) {
	items := []*types.Message{
		{Type: types.MessageTypeRuntimeNotice},
		{Type: types.MessageTypeAssistant},
		openerIncomingAssignment("assignment"),
		openerProse("reply"),
	}
	projection := projectWorkWithVisibleReplies(items, nil, msgtypes.VisibleRepliesLatest, resolveSessionOpener(items, nil))
	exchange := projection.exchanges[0]
	require.Equal(t, []int{2, 3}, exchange.response, "the assignment is the pinned opener; the empty prefix has no presentation")
	require.Equal(t, []int{0}, exchange.work)
}

func TestWorkOpenerDoesNotChangeHumanBoundedExchanges(t *testing.T) {
	items := []*types.Message{
		{Type: types.MessageTypeUser, Content: "first human"},
		openerProse("first answer"),
		openerIncomingAssignment("report in turn one"),
		openerProse("second answer"),
		{Type: types.MessageTypeUser, Content: "second human"},
		openerIncomingAssignment("report in turn two"),
		openerTool(),
		openerProse("final answer"),
	}
	groups := map[*types.Message]uint64{items[1]: 1, items[3]: 2, items[7]: 3}
	projection := projectWorkWithVisibleReplies(items, groups, msgtypes.VisibleRepliesLatest, resolveSessionOpener(items, nil))
	require.Len(t, projection.exchanges, 2)
	require.Equal(t, 0, projection.exchanges[0].boundary)
	require.Equal(t, []int{1, 2}, projection.exchanges[0].work, "reports after a human input stay folded")
	require.Equal(t, []int{3}, projection.exchanges[0].response)
	require.Equal(t, 4, projection.exchanges[1].boundary)
	require.Equal(t, []int{5, 6}, projection.exchanges[1].work)
	require.Equal(t, []int{7}, projection.exchanges[1].response)
}

func TestCompactWorkKeepsSessionOpenerOutsideExpandedWorkAndRendersOnce(t *testing.T) {
	ar := animation.NewRuntimeWithScheduler(&disclosureClock{now: time.Unix(1, 0)})
	m := NewScrollableView(ar, 90, 30, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetCompactWorkLog(true)
	m.addMessage(types.Input(openerAgentInput("assignment body")))
	for i := range 3 {
		call := tools.ToolCall{ID: fmt.Sprintf("opener-call-%d", i), Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"true"}`}}
		m.addMessage(types.ToolCallMessage("root", call, tools.Tool{Name: "shell"}, types.ToolStatusCompleted))
	}
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "FINAL_REPLY_AFTER_WORK"))
	openerSettle(t, ar, m)
	m.syncWorkProjection()
	block := firstWorkBlock(m)
	require.NotNil(t, block, "the tool calls still collapse into Work")
	opener := openerSourceIndex(t, m, "assignment body")

	_, hidden := m.workSourceToHeader[opener]
	require.False(t, hidden, "the opener is not hidden behind the Work header")
	for _, item := range block.Items() {
		require.NotEqual(t, opener, item.SourceIndex, "the opener is not duplicated inside Work")
	}

	plain := ansi.Strip(m.View())
	require.Equal(t, 1, openerCount(plain, openerHeader))
	require.Less(t, strings.Index(plain, openerHeader), strings.Index(plain, "Work ["), "the opener stays above the collapsed Work header")

	m.toggleWork(block)
	require.True(t, block.Expanded())
	m.ensureAllItemsRendered()
	plain = ansi.Strip(m.View())
	require.Contains(t, plain, "Work [-]")
	require.Equal(t, 1, openerCount(plain, openerHeader), "expanded Work does not repeat the opener")
	require.Less(t, strings.Index(plain, openerHeader), strings.Index(plain, "Work [-]"))
	require.Equal(t, 1, openerCount(plain, "FINAL_REPLY_AFTER_WORK"))
}

func TestSessionOpenerRemainsSingleAcrossCompactToggle(t *testing.T) {
	m := openerModel(t)
	m.addMessage(types.Input(openerAgentInput("assignment body")))
	m.addMessage(openerTool())
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "TOGGLE_FINAL"))
	for _, enabled := range []bool{false, true, false, true} {
		m.SetCompactWorkLog(enabled)
		plain := ansi.Strip(m.View())
		require.Equal(t, 1, openerCount(plain, openerHeader), "compact=%v", enabled)
		require.Equal(t, 1, openerCount(plain, "TOGGLE_FINAL"), "compact=%v", enabled)
		require.Less(t, strings.Index(plain, openerHeader), strings.Index(plain, "TOGGLE_FINAL"), "compact=%v", enabled)
	}
}

func TestStreamingReplyAfterSessionOpenerKeepsOpenerOutsideWork(t *testing.T) {
	m := openerModel(t)
	m.SetCompactWorkLog(true)
	m.addMessage(types.Input(openerAgentInput("assignment body")))
	call := tools.ToolCall{ID: "stream-call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"true"}`}}
	m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "shell"}, types.ToolStatusRunning)
	m.AppendToLastMessage("root", "STREAM_REPLY_HEAD ")
	m.AppendToLastMessage("root", "STREAM_REPLY_TAIL")
	m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: call.ID, Response: "ok", Result: &tools.ToolCallResult{Output: "ok"}}, types.ToolStatusCompleted)
	m.syncWorkProjection()
	require.Len(t, m.workProjection.exchanges, 1)
	require.Contains(t, m.workProjection.exchanges[0].response, openerSourceIndex(t, m, "assignment body"))
	plain := ansi.Strip(m.View())
	require.Equal(t, 1, openerCount(plain, openerHeader))
	require.Equal(t, 1, openerCount(plain, "STREAM_REPLY_HEAD"))
}

func TestPromotedPendingAssignmentBecomesOpenerOnce(t *testing.T) {
	m := openerModel(t)
	m.SetCompactWorkLog(true)
	pending := openerAgentInput("assignment body")
	pending.Pending, pending.TurnID = true, "turn-opener"
	m.AddInputMessage(pending, -1)
	call := tools.ToolCall{ID: "promoted-call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"true"}`}}
	m.addMessage(types.ToolCallMessage("root", call, tools.Tool{Name: "shell"}, types.ToolStatusCompleted))
	accepted := openerAgentInput("assignment body")
	accepted.TurnID = "turn-opener"
	m.AddInputMessage(accepted, -1)
	m.syncWorkProjection()
	index := openerSourceIndex(t, m, "assignment body")
	require.Contains(t, m.workProjection.exchanges[0].response, index)
	require.NotContains(t, m.workProjection.bySource, index)
	plain := ansi.Strip(m.View())
	require.Equal(t, 1, openerCount(plain, openerHeader))
}

func TestReplayedSessionOpenerRendersOnceOutsideWork(t *testing.T) {
	sess := session.New(session.WithID("opener-replay"))
	sess.AddMessage(openerAgentInput("replay body"))
	call := tools.ToolCall{ID: "replay-opener-call", Function: tools.FunctionCall{Name: "probe"}}
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role:            chat.MessageRoleAssistant,
		ToolCalls:       []tools.ToolCall{call},
		ToolDefinitions: []tools.Tool{{Name: "probe"}},
		Presentation:    []chat.AssistantPart{{Type: chat.AssistantPartToolCall, ToolCallID: call.ID}},
	}))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role:         chat.MessageRoleAssistant,
		Content:      "REPLAY_FINAL",
		Presentation: []chat.AssistantPart{{Type: chat.AssistantPartContent, Text: "REPLAY_FINAL"}},
	}))
	m := openerModel(t)
	m.SetCompactWorkLog(true)
	BeginReplay(m, PrepareReplay(sess), nil)
	finishPreparedReplay(t, m)
	m.syncWorkProjection()
	require.NotNil(t, firstWorkBlock(m), "the replayed tool call collapses into Work")
	plain := ansi.Strip(m.View())
	require.Equal(t, 1, openerCount(plain, openerHeader))
	require.Equal(t, 1, openerCount(plain, "replay body"), "the replayed opener body is shown once")
	require.Less(t, strings.Index(plain, openerHeader), strings.Index(plain, "Work ["))
}
