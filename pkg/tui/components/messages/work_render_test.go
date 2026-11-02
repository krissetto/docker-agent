package messages

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/components/workblock"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestCompactWorkLogGroupsNoticesAcrossLongCoordinationTranscriptAndExpandsChronologically(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 72, 30, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations() })
	m.SetCompactWorkLog(true)
	m.SetVisibleReplies(msgtypes.VisibleRepliesLatest)
	m.suppressReplyHandoff = true
	defer func() { m.suppressReplyHandoff = false }()
	m.addMessage(types.User("Help with a long-running task"))
	for i := range 24 {
		if i%5 == 0 {
			input := types.Input(&session.Message{InputOrigin: session.InputOriginRuntime, InputMode: "steer", SenderID: "worker-id", SenderName: "worker", Message: chat.Message{Content: "Subagent \"worker\" (worker-id) finished its turn."}})
			input.ReceivedBody = "Subagent \"worker\" (worker-id) finished its turn."
			m.addMessage(input)
		}
		if i%4 == 0 {
			m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "Progress update"))
			m.responseGroups[m.messages[len(m.messages)-1]] = 1
		}
		call := types.ToolCallMessage("root", tools.ToolCall{ID: fmt.Sprintf("shell-%d", i), Function: tools.FunctionCall{Name: "shell"}}, tools.Tool{Name: "shell"}, types.ToolStatusCompleted)
		m.addMessage(call)
		m.addMessage(types.Agent(types.MessageTypeAssistantReasoningBlock, "root", fmt.Sprintf("Thinking paragraph %d", i)))
	}
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "Final answer"))
	m.responseGroups[m.messages[len(m.messages)-1]] = 2
	m.workProjectionDirty = true
	m.invalidateAllItems()
	m.ensureAllItemsRendered()
	frame := ansi.Strip(m.View())
	require.Equal(t, 1, m.MessageTypeCount(types.MessageTypeUser))
	require.Len(t, m.workHeaderBySource, 1, "all notices and work items stay in a single human-bounded Work block")
	require.Equal(t, 24, firstWorkBlock(m).Status().ToolCount)
	require.Contains(t, frame, "Work [+] (24 tools)")
	require.Contains(t, frame, "Final answer")
	require.NotContains(t, frame, "Thinking paragraph")
	require.NotContains(t, frame, "turn finished")

	block := firstWorkBlock(m)
	require.NotNil(t, block)
	block.SetExpandedImmediately(true)
	block.SetSize(m.contentWidth()+1, 0)
	m.invalidateAllItems()
	m.ensureAllItemsRendered()
	expanded := ansi.Strip(m.View())
	require.Contains(t, expanded, "Thinking paragraph 0")
	require.Contains(t, expanded, "worker")
	require.Contains(t, expanded, "Thinking paragraph 1")
}

func TestWorkHeaderCountsFinishedToolsAcrossLiveStatusUpdates(t *testing.T) {
	for _, embedded := range []bool{false, true} {
		t.Run(fmt.Sprintf("embedded=%t", embedded), func(t *testing.T) {
			m := NewScrollableView(animation.NewRuntime(), 80, 20, &service.SessionState{}).(*model)
			t.Cleanup(m.StopAnimations)
			m.SetCompactWorkLog(true)
			m.addMessage(types.User("request"))
			if embedded {
				m.AppendReasoning("root", "Private reasoning")
			}
			call := tools.ToolCall{ID: "first-call", Function: tools.FunctionCall{Name: "private_tool_name"}}
			definition := tools.Tool{Name: "private_tool_name"}
			m.AddOrUpdateToolCall("root", call, definition, types.ToolStatusPending)
			m.SetWorkStatus(true, 0)
			m.ensureAllItemsRendered()
			block := firstWorkBlock(m)
			require.NotNil(t, block)
			require.Zero(t, block.Status().ToolCount)
			require.NotContains(t, ansi.Strip(block.View()), " tool")

			for _, status := range []types.ToolStatus{types.ToolStatusConfirmation, types.ToolStatusRunning} {
				m.AddOrUpdateToolCall("root", call, definition, status)
				m.ensureAllItemsRendered()
				require.Zero(t, block.Status().ToolCount, "in-flight calls have not finished")
			}
			m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: call.ID, Response: "done"}, types.ToolStatusCompleted)
			m.ensureAllItemsRendered()
			require.Equal(t, 1, block.Status().ToolCount)
			require.Contains(t, ansi.Strip(block.View()), "Working [+] (1 tool)")

			call.ID = "second-call"
			m.AddOrUpdateToolCall("root", call, definition, types.ToolStatusRunning)
			m.ensureAllItemsRendered()
			require.Equal(t, 1, block.Status().ToolCount)
			m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: call.ID, Response: "failed"}, types.ToolStatusError)
			m.ensureAllItemsRendered()
			require.Equal(t, 2, block.Status().ToolCount, "failed calls count as tool uses")
			require.Contains(t, ansi.Strip(block.View()), "Working [+] (2 tools)")
			require.NotContains(t, ansi.Strip(block.View()), "private_tool_name")
			require.NotContains(t, ansi.Strip(block.View()), "Reasoning")
			require.NotContains(t, ansi.Strip(block.View()), "actions")

			m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: call.ID, Response: "failed"}, types.ToolStatusError)
			m.ensureAllItemsRendered()
			require.Equal(t, 2, block.Status().ToolCount, "repeated results do not count twice")
			m.SetWorkStatus(false, 4*time.Second)
			m.ensureAllItemsRendered()
			require.Contains(t, ansi.Strip(block.View()), "Work [+] (2 tools) · 4s")
		})
	}
}

func TestWorkHeaderCountsReplayedToolsInsideThinkingBlocks(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 80, 20, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	m.SetCompactWorkLog(true)
	sess := session.New()
	sess.AddMessage(session.UserMessage("request"))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role: chat.MessageRoleAssistant,
		Presentation: []chat.AssistantPart{
			{Type: chat.AssistantPartReasoning, Text: "Private reasoning"},
			{Type: chat.AssistantPartToolCall, ToolCallID: "first-call"},
			{Type: chat.AssistantPartToolCall, ToolCallID: "second-call"},
		},
		ToolCalls: []tools.ToolCall{
			{ID: "first-call", Function: tools.FunctionCall{Name: "private_tool_name"}},
			{ID: "second-call", Function: tools.FunctionCall{Name: "private_tool_name"}},
		},
	}))
	for _, id := range []string{"first-call", "second-call"} {
		sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleTool, ToolCallID: id, Content: "done"}))
	}
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "Final answer"}))
	BeginReplay(m, PrepareReplay(sess), nil)
	finishPreparedReplay(t, m)
	finishAllHistoryForTest(t, m)
	m.ensureAllItemsRendered()
	require.Zero(t, m.MessageTypeCount(types.MessageTypeToolCall), "all tools belong to the thinking block")
	block := firstWorkBlock(m)
	require.NotNil(t, block)
	require.Equal(t, 2, block.Status().ToolCount)
	require.Contains(t, ansi.Strip(block.View()), "Work [+] (2 tools)")
}

func TestWorkDisplayOffsetsKeepFollowingMessageHitTargetMonotonic(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 70, 20, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations() })
	m.SetCompactWorkLog(true)
	m.addMessage(types.User("first"))
	m.addMessage(types.Agent(types.MessageTypeAssistantReasoningBlock, "root", "hidden work"))
	m.addMessage(types.User("following user"))
	m.workProjectionDirty = true
	m.ensureAllItemsRendered()
	start := strings.Index(ansi.Strip(strings.Join(m.renderedLines, "\n")), "following user")
	require.GreaterOrEqual(t, start, 0)
	line := strings.Count(ansi.Strip(strings.Join(m.renderedLines, "\n"))[:start], "\n")
	msg, _ := m.globalLineToMessageLineCached(line)
	require.Equal(t, 2, msg, "the display index after a collapsed Work block maps to the source user, not a zero-height child")
}

func TestExpandedWorkChildHitDoesNotToggleTheParentHeader(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 70, 20, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations() })
	m.SetCompactWorkLog(true)
	m.addMessage(types.User("request"))
	m.addMessage(types.Agent(types.MessageTypeAssistantReasoningBlock, "root", "reasoning child"))
	m.workProjectionDirty = true
	m.invalidateAllItems()
	m.syncWorkProjection()
	block := firstWorkBlock(m)
	require.NotNil(t, block)
	block.SetExpandedImmediately(true)
	block.SetSize(m.contentWidth()+1, 0)
	m.invalidateAllItems()
	m.ensureAllItemsRendered()
	rangeInfo := m.workRangeList()[0]
	childRow := rangeInfo.start + block.Rows()[0].Start
	source, childLine := m.globalLineToMessageLineCached(childRow)
	require.Equal(t, 1, source)
	require.Equal(t, 0, childLine)
	updated, _ := m.handleMouseClick(tea.MouseClickMsg{X: 1, Y: childRow, Button: tea.MouseLeft})
	require.Same(t, m, updated)
	require.True(t, block.Expanded(), "first child line is not the composite header")
}

func TestHoveringWorkHeaderDoesNotReplaceCompositeRowsWithSourceView(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 70, 20, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations() })
	m.SetCompactWorkLog(true)
	m.addMessage(types.User("request"))
	m.addMessage(types.Agent(types.MessageTypeAssistantReasoningBlock, "root", "reasoning child"))
	m.workProjectionDirty = true
	m.ensureAllItemsRendered()
	before := ansi.Strip(strings.Join(m.renderedLines, "\n"))
	_, _ = m.handleMouseMotion(tea.MouseMotionMsg{X: 2, Y: 4})
	m.ensureAllItemsRendered()
	after := ansi.Strip(strings.Join(m.renderedLines, "\n"))
	require.Contains(t, before, "Work [+]")
	require.Contains(t, after, "Work [+]")
	require.NotContains(t, after, "reasoning child")
}

func firstWorkBlock(m *model) *workblock.Model {
	for _, block := range m.workHeaderBySource {
		return block
	}
	return nil
}

type workVisibilityClock struct{ now time.Time }

func (c *workVisibilityClock) Now() time.Time { return c.now }
func (c *workVisibilityClock) Tick(_ time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { c.now = c.now.Add(50 * time.Millisecond); return create(c.now) }
}

func TestScrollbarPresentationIsIndependentOfWorkGrouping(t *testing.T) {
	clock := &workVisibilityClock{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(clock)
	m := NewScrollableView(ar, 40, 5, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetCompactWorkLog(false)
	m.SetScrollbarsActive(true)
	for i := range 10 {
		m.addMessage(types.User(fmt.Sprintf("message %d", i)))
	}
	m.ensureAllItemsRendered()
	_ = m.View()
	m.setScrollOffset(1)
	require.EqualValues(t, 1, ar.ActiveCount(), "scrollbars animate even with Work grouping disabled")
	for range 30 {
		if ar.ActiveCount() == 0 {
			break
		}
		cmd := ar.Continue()
		msg, ok := ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		m.Update(msg)
	}
	require.Zero(t, ar.ActiveCount(), "fade releases its animation lease")
	require.NotContains(t, m.View(), "│", "scrollbar remains hidden after its hold and fade")
	m.SetScrollbarsActive(false)
	require.False(t, m.scrollbarsActive)
}

func TestSelectedOrScrolledCandidateWaitsInPlaceThenDemotesAsEarlierReply(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 54, 12, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations() })
	m.SetCompactWorkLog(true)
	m.SetVisibleReplies(msgtypes.VisibleRepliesLatest)
	m.addMessage(types.User("continue if needed"))
	candidate := types.Agent(types.MessageTypeAssistant, "root", strings.Repeat("Interim reply content stays readable.\n", 45))
	m.addMessage(candidate)
	m.workProjectionDirty = true
	m.ensureAllItemsRendered()
	candidateIndex := len(m.messages) - 1
	m.selectedMessageIndex = candidateIndex
	m.addMessage(types.ToolCallMessage("root", tools.ToolCall{ID: "next", Function: tools.FunctionCall{Name: "shell"}}, tools.Tool{Name: "shell"}, types.ToolStatusCompleted))
	m.ensureAllItemsRendered()
	require.NotContains(t, m.workSourceToHeader, candidateIndex, "selected output is deferred outside Work")
	require.Contains(t, ansi.Strip(m.View()), "Interim reply")

	m.selectedMessageIndex = -1
	m.userHasScrolled = false
	for _, answer := range []string{"answer two", "answer three", "answer four"} {
		m.addMessage(types.Agent(types.MessageTypeAssistant, "root", answer))
	}
	m.ensureAllItemsRendered()
	require.Contains(t, m.workSourceToHeader, candidateIndex, "a later prose group beyond the policy evicts the older answer into Work")
	block := m.workBlockForSource(candidateIndex)
	require.NotNil(t, block)
	require.NotContains(t, ansi.Strip(block.View()), "Earlier reply", "superseded response content stays visible without a repeated heading")
}

func TestLongCandidateCanRemainVisibleWhileReaderIsScrolledUp(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 54, 8, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations() })
	m.SetCompactWorkLog(true)
	m.addMessage(types.User("request"))
	candidate := types.Agent(types.MessageTypeAssistant, "root", strings.Repeat("Long candidate remains in place.\n", 80))
	m.addMessage(candidate)
	m.workProjectionDirty = true
	m.ensureAllItemsRendered()
	candidateIndex := len(m.messages) - 1
	m.userHasScrolled = true
	m.scrollOffset = min(max(0, m.totalHeight-4), max(0, m.lineOffsets[candidateIndex]+5))
	m.addMessage(types.ToolCallMessage("root", tools.ToolCall{ID: "continue", Function: tools.FunctionCall{Name: "shell"}}, tools.Tool{Name: "shell"}, types.ToolStatusCompleted))
	m.ensureAllItemsRendered()
	require.NotContains(t, m.workSourceToHeader, candidateIndex)
	require.Contains(t, ansi.Strip(m.View()), "Long candidate remains in place")
}

func TestGroupedReplayKeepsFinalAnswerVisibleAfterHistoryPass(t *testing.T) {
	m, _ := replayWorkHistory(t, 8)
	finishAllHistoryForTest(t, m)
	m.scrollToBottom()
	frame := ansi.Strip(m.View())
	require.Contains(t, frame, "FINAL_ANSWER_REMAINS_VISIBLE")
	require.NotContains(t, m.workSourceToHeader, len(m.messages)-1)
	require.Equal(t, 40, firstWorkBlock(m).Status().ToolCount)
}

func TestHoverBeforeExpandedWorkKeepsCompositeAndFollowingStreamGeometry(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations() })
	m.SetCompactWorkLog(true)
	prior := &countedHistoryView{content: "prior line"}
	m.messages = append(m.messages, types.Cancelled())
	m.views = append(m.views, prior)
	m.addMessage(types.User("first request"))
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "PRECEDING_REPLY_MARKER"))
	m.addMessage(types.User("second request"))
	shellCall := types.ToolCallMessage("root", tools.ToolCall{ID: "work-shell", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"printf child"}`}}, tools.Tool{Name: "shell"}, types.ToolStatusCompleted)
	m.messages = append(m.messages, shellCall)
	m.views = append(m.views, m.createToolCallView(shellCall))
	m.workProjectionDirty = true
	m.addMessage(types.Agent(types.MessageTypeAssistantReasoningBlock, "root", "WORK_REASONING_MARKER\nWORK_SECOND_LINE"))
	m.AppendToLastMessage("root", "FINAL_CANDIDATE_MARKER")
	m.workProjectionDirty = true
	m.ensureAllItemsRendered()

	block := firstWorkBlock(m)
	require.NotNil(t, block)
	block.SetExpandedImmediately(true)
	block.SetSize(m.contentWidth()+1, 0)
	m.invalidateAllItems()
	m.ensureAllItemsRendered()
	require.Len(t, m.workRangeList(), 1)
	require.NotNil(t, m.activeSegments)
	beforeTotal := m.totalHeight
	workStart := m.workRangeList()[0].start
	workHeader := ansi.Strip(m.renderedLine(workStart))
	require.Contains(t, workHeader, "  Work [-]", "expanded Work aligns to the shared two-cell tool gutter")
	require.Contains(t, ansi.Strip(m.View()), "FINAL_CANDIDATE_MARKER")

	// This user row immediately precedes the Work composite; showing its hover
	// affordance must not consume the composite header as a source separator.
	hoverLine := m.lineOffsets[3] + 1
	_, _ = m.handleMouseMotion(tea.MouseMotionMsg{X: 5, Y: hoverLine})
	require.Equal(t, 3, m.hoveredMessageIndex, "the user row immediately before Work is hovered")
	require.False(t, m.renderDirty, "hover should take the incremental refresh path")
	require.Equal(t, beforeTotal, m.totalHeight, "reserved hover actions cannot change row height")
	require.Equal(t, workStart, m.workRangeList()[0].start)
	require.Contains(t, ansi.Strip(m.renderedLine(workStart)), "Work")
	require.Equal(t, m.totalHeight, m.activeSegments.start+m.activeSegments.height())
	require.Contains(t, ansi.Strip(m.View()), "WORK_REASONING_MARKER")
	require.Contains(t, ansi.Strip(m.View()), "FINAL_CANDIDATE_MARKER")

	var shellRow *workblock.Row
	for _, row := range block.Rows() {
		if row.SourceIndex == 4 {
			matched := row
			shellRow = &matched
			break
		}
	}
	require.NotNil(t, shellRow, "expanded Work exposes the shell source row: %#v", block.Rows())
	shellLine := m.workRangeList()[0].start + shellRow.Start
	_, _ = m.handleMouseMotion(tea.MouseMotionMsg{X: 4, Y: shellLine})
	require.Equal(t, 4, m.hoveredToolIndex, "hover follows the Work child source identity")
	require.Contains(t, ansi.Strip(m.View()), "printf child")
	require.Equal(t, m.totalHeight, m.activeSegments.start+m.activeSegments.height())
	_, _ = m.handleMouseMotion(tea.MouseMotionMsg{X: m.width + 1, Y: shellLine})
	require.Equal(t, -1, m.hoveredToolIndex, "leaving Work clears the child tool hover")

	m.ensureAllItemsRendered()
	prior.content = "prior row one\nprior row two\nprior row three"
	oldWorkStart, oldTailStart, oldTotal := m.workRangeList()[0].start, m.activeSegments.start, m.totalHeight
	require.True(t, m.refreshRenderedItem(0), "height change uses the incremental source refresh")
	require.Equal(t, oldWorkStart+2, m.workRangeList()[0].start, "composite start shifts by the changed preceding source row")
	require.Equal(t, oldTailStart+2, m.activeSegments.start, "active streamed suffix start shifts with the same delta")
	require.Equal(t, oldTotal+2, m.totalHeight)
	require.Equal(t, m.totalHeight, m.activeSegments.start+m.activeSegments.height())
	require.Contains(t, ansi.Strip(m.renderedLine(m.workRangeList()[0].start)), "Work")
	require.Contains(t, ansi.Strip(m.View()), "FINAL_CANDIDATE_MARKER")

	workLine := m.workRangeList()[0].start
	updated, _ := m.handleMouseClick(tea.MouseClickMsg{X: 4, Y: workLine, Button: tea.MouseLeft})
	require.Same(t, m, updated)
	require.False(t, block.Expanded(), "the visible Work header remains the click target after hover")
	finalLine := m.activeSegments.start + m.activeSegments.height() - 1
	m.selectRange(finalLine, 0, finalLine, m.width)
	require.Contains(t, m.extractSelectedText(), "FINAL_CANDIDATE_MARKER", "selection still maps into the active final response")
}

func TestExpandedWorkAssistantKeepsSelectableSourceTextInsideInsetGeometry(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 72, 20, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations() })
	m.SetCompactWorkLog(true)
	m.SetVisibleReplies(msgtypes.VisibleRepliesLatest)
	m.suppressReplyHandoff = true
	defer func() { m.suppressReplyHandoff = false }()
	m.addMessage(types.User("request"))
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "WORK_SELECTABLE_MARKER"))
	call := tools.ToolCall{ID: "work-selection-tool", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"true"}`}}
	m.addMessage(types.ToolCallMessage("root", call, tools.Tool{Name: "shell"}, types.ToolStatusCompleted))
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "latest answer"))
	m.workProjectionDirty = true
	m.syncWorkProjection()
	block := firstWorkBlock(m)
	require.NotNil(t, block)
	block.SetExpandedImmediately(true)
	m.invalidateItem(1)
	m.ensureAllItemsRendered()

	assistant := 1
	childSize, ok := m.views[assistant].(interface{ GetSize() (int, int) })
	require.True(t, ok)
	childWidth, _ := childSize.GetSize()
	require.Equal(t, workblock.SourceContentWidth(m.contentWidth()), childWidth, "the source message itself renders at its inset width")
	var line, col int
	found := false
	for _, row := range block.Rows() {
		if row.SourceIndex != assistant {
			continue
		}
		for local := row.Start; local < row.End; local++ {
			plain := ansi.Strip(m.renderedLine(m.workRangeStart(assistant) + local))
			if offset := strings.Index(plain, "WORK_SELECTABLE_MARKER"); offset >= 0 {
				line = m.workRangeStart(assistant) + local
				col = ansi.StringWidth(plain[:offset])
				found = true
				break
			}
		}
	}
	require.True(t, found, "the expanded source message is visibly present in its Work row")
	m.selection = selectionState{active: true, startLine: line, startCol: col, endLine: line, endCol: col + ansi.StringWidth("WORK_SELECTABLE_MARKER")}
	require.Contains(t, m.extractSelectedText(), "WORK_SELECTABLE_MARKER", "selection and clipboard extraction target the inset source text")
}

func TestWorkInsetPreservesIncomingReplyAndNestedReasoningToolDisclosures(t *testing.T) {
	ar := animation.NewRuntime()
	m := NewScrollableView(ar, 100, 80, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetPosition(0, 0)
	m.SetCompactWorkLog(true)
	m.addMessage(types.User("request"))
	anchor := types.ToolCallMessage("root", tools.ToolCall{ID: "anchor", Function: tools.FunctionCall{Name: "probe"}}, tools.Tool{Name: "probe"}, types.ToolStatusCompleted)
	m.addMessage(anchor)
	incoming := session.UserMessage(strings.Repeat("A long child reply stays selectable and expandable. ", 36))
	incoming.InputOrigin, incoming.InputMode, incoming.SenderID, incoming.SenderName = session.InputOriginAgent, "steer", "child-session", "worker"
	m.addMessage(types.Input(incoming))
	reasoningTool := tools.ToolCall{ID: "nested-tool", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"printf nested"}`}}
	m.AppendReasoning("root", "A second reasoning section with a nested shell tool.")
	m.AddOrUpdateToolCall("root", reasoningTool, tools.Tool{Name: "shell"}, types.ToolStatusRunning)
	m.AppendToolOutput(&runtime.ToolCallOutputEvent{ToolCallID: reasoningTool.ID, Output: strings.Repeat("nested output line\n", 50)})
	m.workProjectionDirty = true
	m.syncWorkProjection()
	work := firstWorkBlock(m)
	require.NotNil(t, work)
	work.SetExpandedImmediately(true)
	m.ensureAllItemsRendered()

	incomingView := m.views[2].(interface {
		IsExpanded() bool
		IsToggleAt(line, col int) bool
	})
	require.False(t, incomingView.IsExpanded())
	var incomingRow *workblock.Row
	for _, row := range work.Rows() {
		if row.SourceIndex == 2 {
			row := row
			incomingRow = &row
			break
		}
	}
	require.NotNil(t, incomingRow, "the source-owned incoming card is visible inside Work")
	incomingHeader := m.workRangeList()[0].start + incomingRow.Start
	for x := range 2 {
		_, _ = m.handleMouseClick(tea.MouseClickMsg{X: x, Y: incomingHeader - m.scrollOffset, Button: tea.MouseLeft})
		require.False(t, incomingView.IsExpanded(), "Work margin x=%d is not forwarded to its child disclosure", x)
	}
	_, _ = m.handleMouseClick(tea.MouseClickMsg{X: 2, Y: incomingHeader - m.scrollOffset, Button: tea.MouseLeft})
	require.True(t, incomingView.IsExpanded(), "the nested incoming reply still expands from its inset header")
	incomingView.(interface{ SetExpanded(expanded bool) }).SetExpanded(true)
	m.invalidateItem(2)
	m.ensureAllItemsRendered()
	require.True(t, work.Expanded(), "expanding a source child does not collapse Work")
	require.Contains(t, ansi.Strip(work.View()), "From worker", "the sender header is visible inside Work")
	require.Contains(t, ansi.Strip(work.View()), "A long child reply stays selectable")
	var replyLine, replyCol int
	var replyRow workblock.Row
	for _, row := range work.Rows() {
		if row.SourceIndex == 2 {
			replyRow = row
			for local := row.Start; local < row.End; local++ {
				global := m.workRangeStart(2) + local
				plain := ansi.Strip(m.renderedLine(global))
				if offset := strings.Index(plain, "From worker"); offset >= 0 {
					replyLine, replyCol = global, ansi.StringWidth(plain[:offset])+ansi.StringWidth("From ")
					break
				}
			}
		}
	}
	require.NotZero(t, replyLine)
	m.selection = selectionState{active: true, startLine: replyLine, startCol: replyCol, endLine: replyLine, endCol: replyCol + ansi.StringWidth("Worker")}
	selected := m.extractSelectedText()
	require.Contains(t, selected, "worker", "the expanded incoming child keeps selectable sender text inside Work")
	require.Equal(t, 2, replyRow.SourceIndex)
	m.selection.clear()

	reasoning := m.views[3].(*reasoningblock.Model)
	reasoning.SetExpandedImmediately(false)
	m.invalidateItem(3)
	m.ensureAllItemsRendered()
	var reasoningRow *workblock.Row
	for _, row := range work.Rows() {
		if row.SourceIndex == 3 {
			row := row
			reasoningRow = &row
			break
		}
	}
	require.NotNil(t, reasoningRow)
	reasoningHeader := m.workRangeList()[0].start + reasoningRow.Start
	for x := range 2 {
		_, _ = m.handleMouseClick(tea.MouseClickMsg{X: x, Y: reasoningHeader - m.scrollOffset, Button: tea.MouseLeft})
		require.False(t, reasoning.IsExpanded(), "Work margin x=%d does not hit a line-only reasoning header", x)
	}
	_, _ = m.handleMouseClick(tea.MouseClickMsg{X: 2, Y: reasoningHeader - m.scrollOffset, Button: tea.MouseLeft})
	require.True(t, reasoning.IsExpanded(), "source column zero still toggles reasoning")
	require.True(t, work.Expanded())

	nestedLine, ok := reasoning.ToolCallLine(reasoningTool.ID)
	require.True(t, ok)
	nestedHeader := m.workRangeList()[0].start + reasoningRow.Start + nestedLine
	before := ansi.Strip(work.View())
	_, _ = m.handleMouseClick(tea.MouseClickMsg{X: 2, Y: nestedHeader - m.scrollOffset, Button: tea.MouseLeft})
	after := ansi.Strip(work.View())
	require.NotEqual(t, before, after, "nested shell disclosure changes its own rendered source output")
	require.Contains(t, after, "v", "nested tool header reflects its own expansion state")
	require.True(t, work.Expanded(), "nested tool expansion leaves outer Work expanded")
	reasoning.SetToolCallExpandedImmediately(reasoningTool.ID, true)
	m.invalidateItem(3)
	m.ensureAllItemsRendered()
	before = ansi.Strip(work.View())
	bodyLine := m.workRangeList()[0].start + reasoningRow.Start + nestedLine + 3
	m.Update(msgtypes.WheelCoalescedMsg{X: 0, Y: bodyLine - m.scrollOffset, Delta: 1})
	m.ensureAllItemsRendered()
	require.Equal(t, before, ansi.Strip(work.View()), "the outermost gutter does not scroll nested tool output")
	m.Update(msgtypes.WheelCoalescedMsg{X: 1, Y: bodyLine - m.scrollOffset, Delta: 1})
	m.ensureAllItemsRendered()
	require.Equal(t, before, ansi.Strip(work.View()), "the second gutter cell does not scroll nested tool output")
	m.scrollOffset = 0
	m.userHasScrolled = true
	m.Update(msgtypes.WheelCoalescedMsg{X: 2, Y: bodyLine - m.scrollOffset, Delta: 1})
	m.ensureAllItemsRendered()
	require.NotEqual(t, before, ansi.Strip(work.View()), "source-width column zero scrolls the nested tool card")
	require.True(t, work.Expanded())

	_, _ = m.handleMouseClick(tea.MouseClickMsg{X: 2, Y: m.workRangeList()[0].start - m.scrollOffset, Button: tea.MouseLeft})
	require.False(t, work.Expanded(), "the outer header retains its separate disclosure")
	reasoning.SetToolCallExpandedImmediately(reasoningTool.ID, true)
	work.SetExpandedImmediately(true)
	m.invalidateAllItems()
	m.ensureAllItemsRendered()
	require.True(t, reasoning.IsExpanded(), "reopening Work preserves the independently expanded reasoning source")
	require.Contains(t, ansi.Strip(work.View()), "nested output line", "reopening Work keeps the tool's own expansion")
}

func TestWorkRoutesSecondReasoningChildToggleWheelAndInvalidationBySource(t *testing.T) {
	ar := animation.NewRuntime()
	m := NewScrollableView(ar, 100, 80, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetCompactWorkLog(true)
	m.addMessage(types.User("request"))
	m.AppendReasoning("root", "first reasoning child")
	anchor := tools.ToolCall{ID: "first-work-source", Function: tools.FunctionCall{Name: "probe"}}
	m.addMessage(types.ToolCallMessage("root", anchor, tools.Tool{Name: "probe"}, types.ToolStatusCompleted))
	m.AppendReasoning("root", "second reasoning child")
	nestedCall := tools.ToolCall{ID: "nested-real-shell", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: `{"cmd":"printf nested"}`}}
	m.AddOrUpdateToolCall("root", nestedCall, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusRunning)
	m.AppendToolOutput(&runtime.ToolCallOutputEvent{ToolCallID: nestedCall.ID, Output: strings.Repeat("visible nested output\n", 80)})
	m.workProjectionDirty = true
	m.syncWorkProjection()
	work := firstWorkBlock(m)
	require.NotNil(t, work)
	work.SetExpandedImmediately(true)
	m.ensureAllItemsRendered()
	secondReasoning := m.views[3].(*reasoningblock.Model)
	secondReasoning.SetExpandedImmediately(false)
	m.invalidateItem(3)
	m.ensureAllItemsRendered()

	var secondRow *workblock.Row
	for _, row := range work.Rows() {
		if row.SourceIndex == 3 {
			row := row
			secondRow = &row
			break
		}
	}
	require.NotNil(t, secondRow)
	header := m.workRangeStart(3) + secondRow.Start
	for x := range 2 {
		_, _ = m.handleMouseClick(tea.MouseClickMsg{X: x, Y: header - m.scrollOffset, Button: tea.MouseLeft})
		require.False(t, secondReasoning.IsExpanded(), "Work margin x=%d cannot line-toggle reasoning", x)
	}
	_, _ = m.handleMouseClick(tea.MouseClickMsg{X: 2, Y: header - m.scrollOffset, Button: tea.MouseLeft})
	require.True(t, secondReasoning.IsExpanded())
	require.True(t, work.Expanded())
	toolLine, ok := secondReasoning.ToolCallLine(nestedCall.ID)
	require.True(t, ok)
	childHeader := header + toolLine
	before := ansi.Strip(work.View())
	_, _ = m.handleMouseClick(tea.MouseClickMsg{X: 2, Y: childHeader - m.scrollOffset, Button: tea.MouseLeft})
	after := ansi.Strip(work.View())
	require.NotEqual(t, before, after, "a non-anchor reasoning source's nested tool toggles and invalidates its own cached output")
	secondReasoning.SetToolCallExpandedImmediately(nestedCall.ID, true)
	m.invalidateItem(3)
	m.ensureAllItemsRendered()
	after = ansi.Strip(work.View())
	require.Contains(t, after, "visible nested output", "nested shell's independent expansion reveals its source output")
	require.True(t, work.Expanded(), "child toggle never collapses outer Work")

	sourceLine := secondRow.Start + toolLine + 4
	require.False(t, work.WheelAt(sourceLine, 0, 1), "first body margin is inert")
	require.False(t, work.WheelAt(sourceLine, 1, 1), "second body margin is inert")
	before = ansi.Strip(work.View())
	require.True(t, work.WheelAt(sourceLine, 2, 5), "child column zero reaches nested shell viewport")
	work.InvalidateSource(3)
	after = ansi.Strip(work.View())
	require.NotEqual(t, before, after, "source-indexed invalidation refreshes non-anchor content")
	require.True(t, work.Expanded())

	_, _ = m.handleMouseClick(tea.MouseClickMsg{X: 2, Y: m.workRangeList()[0].start - m.scrollOffset, Button: tea.MouseLeft})
	require.False(t, work.Expanded())
	work.SetExpandedImmediately(true)
	m.invalidateAllItems()
	m.ensureAllItemsRendered()
	require.True(t, secondReasoning.IsExpanded(), "reasoning expansion survives outer collapse")
	require.Contains(t, ansi.Strip(work.View()), "nested output", "nested shell expansion survives outer collapse")

	for _, width := range []int{64, 36, 20, 12, 8, 4} {
		m.SetSize(width, 80)
		m.ensureAllItemsRendered()
		require.Len(t, m.workRangeList(), 1)
		composite := m.workRangeList()[0]
		compositeWidth, _ := composite.block.GetSize()
		childWidth := workblock.SourceContentWidth(compositeWidth)
		require.True(t, work.Expanded(), "viewport width %d does not alter the outer Work disclosure", width)
		require.True(t, secondReasoning.IsExpanded(), "viewport width %d does not alter its reasoning disclosure", width)
		for _, row := range work.Rows() {
			for local := row.Start; local < row.End; local++ {
				line := ansi.Strip(m.renderedLine(composite.start + local))
				require.LessOrEqualf(t, ansi.StringWidth(line), m.contentWidth(), "width %d source %d row %d stays inside the Work body: %q", width, row.SourceIndex, local, line)
			}
			if row.SourceIndex != 3 {
				continue
			}
			firstChildRow := row.Start
			margin := workblock.BodyMargin(compositeWidth)
			for col := range margin {
				require.False(t, composite.block.IsToggleAt(firstChildRow, col), "width %d gutter col %d remains inert for the nested disclosure", width, col)
				_, inside := composite.block.SourceColumnAt(firstChildRow, col)
				require.False(t, inside, "width %d gutter col %d has no child column", width, col)
			}
			if childWidth > 0 {
				childCol, inside := composite.block.SourceColumnAt(firstChildRow, margin)
				require.True(t, inside, "width %d exposes the first source cell after both margins", width)
				require.Zero(t, childCol)
				require.True(t, composite.block.IsToggleAt(firstChildRow, margin), "width %d preserves the source-owned reasoning disclosure control", width)
			}
		}
		m.SetSize(100, 80)
		m.ensureAllItemsRendered()
	}
}

func TestWorkCountsOutgoingReplyOnceAndExcludesIncomingCompletions(t *testing.T) {
	for _, embedded := range []bool{false, true} {
		for _, replay := range []bool{false, true} {
			t.Run(fmt.Sprintf("embedded=%t/replay=%t", embedded, replay), func(t *testing.T) {
				ar := animation.NewRuntime()
				m := NewScrollableView(ar, 80, 30, &service.SessionState{}).(*model)
				t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
				m.SetCompactWorkLog(true)
				call := tools.ToolCall{ID: "outgoing-reply", Function: tools.FunctionCall{Name: subagent.ToolSendMessage, Arguments: `{"to":"parent","message":"result"}`}}
				reply := session.UserMessage("incoming child reply")
				reply.InputOrigin, reply.InputMode, reply.SenderID, reply.SenderName = session.InputOriginAgent, "steer", "child", "worker"
				completion := session.UserMessage("background shell command completed")
				completion.InputOrigin, completion.InputMode = session.InputOriginRuntime, "steer"
				if replay {
					sess := session.New()
					sess.AddMessage(session.UserMessage("request"))
					assistant := &chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}}
					if embedded {
						assistant.Presentation = []chat.AssistantPart{
							{Type: chat.AssistantPartReasoning, Text: "Sending the result"},
							{Type: chat.AssistantPartToolCall, ToolCallID: call.ID},
						}
					}
					sess.AddMessage(session.NewAgentMessage("root", assistant))
					sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleTool, ToolCallID: call.ID, Content: "delivered"}))
					sess.AddMessage(reply)
					sess.AddMessage(completion)
					BeginReplay(m, PrepareReplay(sess), nil)
					finishPreparedReplay(t, m)
					finishAllHistoryForTest(t, m)
				} else {
					m.addMessage(types.User("request"))
					if embedded {
						m.AppendReasoning("root", "Sending the result")
					}
					m.AddOrUpdateToolCall("root", call, tools.Tool{Name: call.Function.Name}, types.ToolStatusRunning)
					m.ensureAllItemsRendered()
					require.Zero(t, firstWorkBlock(m).Status().ToolCount)
					result := &runtime.ToolCallResponseEvent{ToolCallID: call.ID, Response: "delivered"}
					m.AddToolResult(result, types.ToolStatusCompleted)
					m.AddToolResult(result, types.ToolStatusCompleted)
					m.addMessage(types.Input(reply))
					m.addMessage(types.Input(completion))
				}
				m.ensureAllItemsRendered()
				block := firstWorkBlock(m)
				require.NotNil(t, block)
				require.Equal(t, 1, block.Status().ToolCount, "only the outgoing send_message call counts, not its result or incoming notifications")
				require.Contains(t, ansi.Strip(block.View()), "(1 tool)")
			})
		}
	}
}
