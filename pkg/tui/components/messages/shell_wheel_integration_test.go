package messages

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	tuimessages "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestShellWheelRoutesExpandedBodyAndFallsBackAtHeader(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	m := NewScrollableView(ar, 100, 8, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	call := shellCall("wheel", `{"cmd":"printf stream"}`)
	m.AddOrUpdateToolCall("root", call, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusRunning)
	m.AppendToolOutput(&runtime.ToolCallOutputEvent{ToolCallID: call.ID, Output: strings.Repeat("output line\n", 100)})
	view := m.views[0].(interface {
		SetExpanded(bool)
		IsExpanded() bool
	})
	view.SetExpanded(true)
	m.userHasScrolled = true
	m.scrollOffset = 0
	_ = m.View()

	before := ansi.Strip(m.views[0].View())
	m.Update(tuimessages.WheelCoalescedMsg{X: 5, Y: 5, Delta: 1})
	after := ansi.Strip(m.views[0].View())
	require.NotEqual(t, before, after, "body wheel changes the visible shell output window")
	require.Equal(t, 0, m.scrollOffset, "consumed body wheel preserves transcript offset")
	require.True(t, m.userHasScrolled, "consumed body wheel preserves transcript scroll ownership")

	m.Update(tuimessages.WheelCoalescedMsg{X: 5, Y: 0, Delta: -1})
	require.Equal(t, 0, m.scrollOffset, "header wheel falls through at transcript top")
	m.userHasScrolled = false
	_ = m.View()
	m.Update(tuimessages.WheelCoalescedMsg{X: 5, Y: 0, Delta: 1})
	require.Positive(t, m.scrollOffset, "header wheel falls back to transcript scrolling")
	m.userHasScrolled = true
	m.scrollOffset = 0
	_ = m.View()
	for i := range 100 {
		m.Update(tuimessages.WheelCoalescedMsg{X: 5, Y: 5, Delta: 1})
		_ = m.View()
		if i == 99 {
			break
		}
	}
	m.Update(tuimessages.WheelCoalescedMsg{X: 5, Y: 5, Delta: 1})
	require.Zero(t, m.scrollOffset, "a body at its boundary consumes wheel instead of scrolling the transcript")
}

func TestShellHoverDisclosureOnlyOnHeaderAndClearsOnLeaveScrollResize(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	m := NewScrollableView(ar, 80, 30, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	m.AddOrUpdateToolCall("root", shellCall("hover", `{"cmd":"printf hover"}`), tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusRunning)
	_ = m.View()
	m.handleMouseMotion(tea.MouseMotionMsg{X: 5, Y: 0})
	_ = m.View()
	_ = m.View()
	require.Contains(t, ansi.Strip(m.renderedLine(m.lineOffsets[0])), ">", "header disclosure remains visible")
	m.handleMouseMotion(tea.MouseMotionMsg{X: 5, Y: 1})
	_ = m.View()
	require.Contains(t, ansi.Strip(m.renderedLine(m.lineOffsets[0])), ">", "body hover leaves disclosure available")
	m.handleMouseMotion(tea.MouseMotionMsg{X: 5, Y: 0})
	_ = m.View()
	require.Contains(t, ansi.Strip(m.renderedLine(m.lineOffsets[0])), ">")
	m.handleMouseMotion(tea.MouseMotionMsg{X: 1000, Y: 1000})
	_ = m.View()
	require.Contains(t, ansi.Strip(m.renderedLine(m.lineOffsets[0])), ">", "leaving transcript preserves disclosure")
	m.handleMouseMotion(tea.MouseMotionMsg{X: 5, Y: 0})
	_ = m.View()
	m.Update(tuimessages.WheelCoalescedMsg{X: 5, Y: 0, Delta: 1})
	_ = m.View()
	require.Contains(t, ansi.Strip(m.renderedLine(m.lineOffsets[0])), ">", "wheel preserves disclosure")
	m.handleMouseMotion(tea.MouseMotionMsg{X: 5, Y: 0})
	_ = m.View()
	m.SetSize(79, 30)
	_ = m.View()
	require.Contains(t, ansi.Strip(m.renderedLine(m.lineOffsets[0])), ">", "resize preserves disclosure")
}

func TestNestedThinkingWheelForwardsToExpandedShellChild(t *testing.T) {
	scheduler := &shellDisclosureScheduler{}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	t.Cleanup(ar.Stop)
	m := NewScrollableView(ar, 100, 50, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	m.AppendReasoning("root", "thinking")
	m.AddOrUpdateToolCall("root", shellCall("nested", `{"cmd":"printf nested"}`), tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusRunning)
	m.AppendToolOutput(&runtime.ToolCallOutputEvent{ToolCallID: "nested", Output: strings.Repeat("nested output\n", 100)})
	block := m.views[0].(*reasoningblock.Model)
	block.SetExpanded(true)
	findShellHeader := func() int {
		for line, rendered := range strings.Split(ansi.Strip(block.View()), "\n") {
			if strings.Contains(rendered, "shell") {
				return line
			}
		}
		return -1
	}
	header := findShellHeader()
	require.Positive(t, header+1, "block view=%q", ansi.Strip(block.View()))
	toolLine := header
	m.handleMouseMotion(tea.MouseMotionMsg{X: 5, Y: toolLine})
	_ = m.View()
	require.Contains(t, ansi.Strip(block.View()), ">", "Thinking forwards shell header hover")
	m.handleMouseMotion(tea.MouseMotionMsg{X: 5, Y: header})
	_ = m.View()
	block.ToggleAt(header, 1)
	advanceShellDisclosure(t, ar, scheduler, m)
	m.invalidateItem(0)
	_ = m.View()
	before := ansi.Strip(block.View())
	toolLine = -1
	for line, rendered := range strings.Split(before, "\n") {
		if strings.Contains(rendered, "shell") {
			toolLine = line
			break
		}
	}
	require.GreaterOrEqual(t, toolLine, 0, "expanded block=%q", before)
	m.userHasScrolled = true
	m.scrollOffset = 0
	m.Update(tuimessages.WheelCoalescedMsg{X: 5, Y: toolLine + 5, Delta: 1})
	_ = m.View()
	after := ansi.Strip(block.View())
	require.NotEqual(t, before, after, "Thinking forwards wheel to child shell viewport")
	require.Equal(t, 0, m.scrollOffset, "nested body wheel leaves transcript scroll offset unchanged")
}

func TestShellHoverKeepsTranscriptSelectionAndClickToggleAvailable(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	m := NewScrollableView(ar, 80, 30, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	m.AddOrUpdateToolCall("root", shellCall("click", `{"cmd":"printf click"}`), tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusRunning)
	_ = m.View()
	m.handleMouseMotion(tea.MouseMotionMsg{X: 5, Y: 0})
	_ = m.View()
	m.handleMouseClick(tea.MouseClickMsg{X: 5, Y: 0, Button: tea.MouseLeft})
	require.True(t, m.views[0].(interface{ IsExpanded() bool }).IsExpanded(), "hover affordance does not consume existing header click toggle")
	// Expand the same card and exercise the transcript's ordinary text drag:
	// clicks inside the header still toggle, while the command body remains selectable/copyable.
	view := m.views[0].(interface{ SetExpanded(bool) })
	m.AppendToolOutput(&runtime.ToolCallOutputEvent{ToolCallID: "click", Output: "copyable output with words"})
	view.SetExpanded(true)
	m.invalidateItem(0)
	_ = m.View()
	bodyLine, startCol := -1, -1
	for local, row := range strings.Split(ansi.Strip(m.views[0].View()), "\n") {
		if col := strings.Index(row, "copyable output"); col >= 0 {
			bodyLine, startCol = m.lineOffsets[0]+local, col+1
			break
		}
	}
	require.Positive(t, bodyLine+1)
	m.handleMouseClick(tea.MouseClickMsg{X: startCol, Y: bodyLine, Button: tea.MouseLeft})
	m.handleMouseMotion(tea.MouseMotionMsg{X: startCol + 10, Y: bodyLine})
	_, cmd := m.handleMouseRelease(tea.MouseReleaseMsg{X: startCol + 10, Y: bodyLine, Button: tea.MouseLeft})
	require.NotNil(t, cmd, "expanded shell output/body drag still dispatches copy")
	require.True(t, m.selection.active, "body selection remains highlighted after release")
	require.True(t, m.views[0].(interface{ IsExpanded() bool }).IsExpanded(), "body drag does not toggle shell disclosure")
	require.NotEmpty(t, m.extractSelectedText(), "drag selects visible shell content")
}

func TestShellHoverClearsAcrossStreamingResultViewReplacement(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	m := NewScrollableView(ar, 80, 20, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	call := shellCall("replace-hover", `{"cmd":"printf stream"}`)
	definition := tools.Tool{Name: builtinshell.ToolNameShell}
	m.AddOrUpdateToolCall("root", call, definition, types.ToolStatusRunning)
	_ = m.View()
	m.handleMouseMotion(tea.MouseMotionMsg{X: 5, Y: 0})
	_ = m.View()
	require.Contains(t, ansi.Strip(m.renderedLine(m.lineOffsets[0])), ">")
	result := &tools.ToolCallResult{}
	m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: call.ID, Response: "completed output", Result: result}, types.ToolStatusCompleted)
	_ = m.View()
	require.Contains(t, ansi.Strip(m.renderedLine(m.lineOffsets[0])), ">", "pointer hover is reapplied to the replacement completed view")
	m.handleMouseMotion(tea.MouseMotionMsg{X: 5, Y: 0})
	_ = m.View()
	require.Contains(t, ansi.Strip(m.renderedLine(m.lineOffsets[0])), ">", "completed row remains hoverable")
}

func shellCall(id, args string) tools.ToolCall {
	return tools.ToolCall{ID: id, Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: args}}
}

func TestStandaloneShellScrollStateSurvivesResultReplacement(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	m := NewScrollableView(ar, 80, 30, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	call := shellCall("scroll-preserve", `{"cmd":"printf preserve"}`)
	definition := tools.Tool{Name: builtinshell.ToolNameShell}
	m.AddOrUpdateToolCall("root", call, definition, types.ToolStatusRunning)
	msg := m.messages[0]
	msg.Content = strings.Repeat("output line\n", 60)
	old := m.views[0]
	expanded := old.(interface{ SetExpanded(bool) })
	expanded.SetExpanded(true)
	old.(interface{ SetSize(int, int) tea.Cmd }).SetSize(80, 0)
	old.(interface{ WheelAt(int, int, int) bool }).WheelAt(5, 1, 20)
	beforeOffset, beforeBottom, ok := old.(interface{ BodyScrollState() (int, bool, bool) }).BodyScrollState()
	require.True(t, ok)
	require.Positive(t, beforeOffset)
	require.False(t, beforeBottom)

	m.AddToolResult(&runtime.ToolCallResponseEvent{
		ToolCallID: call.ID,
		Response:   msg.Content + "finished",
		Result:     &tools.ToolCallResult{},
	}, types.ToolStatusCompleted)
	newView := m.views[0].(interface{ BodyScrollState() (int, bool, bool) })
	afterOffset, afterBottom, ok := newView.BodyScrollState()
	require.True(t, ok)
	require.Equal(t, beforeOffset, afterOffset, "mid-scroll position remains anchored across replacement")
	require.Equal(t, beforeBottom, afterBottom)
}

func TestNestedShellScrollPositionSurvivesResultReplacement(t *testing.T) {
	scheduler := &shellDisclosureScheduler{}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	t.Cleanup(ar.Stop)
	m := NewScrollableView(ar, 100, 50, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	m.AppendReasoning("root", "thinking")
	call := shellCall("nested-scroll-preserve", `{"cmd":"printf nested"}`)
	definition := tools.Tool{Name: builtinshell.ToolNameShell}
	m.AddOrUpdateToolCall("root", call, definition, types.ToolStatusRunning)
	output := strings.Builder{}
	for i := range 60 {
		fmt.Fprintf(&output, "nested line %02d\n", i)
	}
	m.AppendToolOutput(&runtime.ToolCallOutputEvent{ToolCallID: call.ID, Output: output.String()})
	block := m.views[0].(*reasoningblock.Model)
	block.SetExpanded(true)
	_ = block.View()
	toolLine := -1
	for line, row := range strings.Split(ansi.Strip(block.View()), "\n") {
		if strings.Contains(row, "shell") {
			toolLine = line
			break
		}
	}
	require.Positive(t, toolLine+1)
	block.ToggleAt(toolLine, 1)
	advanceShellDisclosure(t, ar, scheduler, m)
	m.invalidateItem(0)
	require.True(t, block.WheelAt(toolLine+5, 1, 12), "nested shell body scrolls to a mid-output position")
	before := ansi.Strip(block.View())
	visibleBefore := nestedOutputRows(before)
	require.NotEmpty(t, visibleBefore, "nested view=%q", before)
	result := &runtime.ToolCallResponseEvent{ToolCallID: call.ID, Response: output.String(), Result: &tools.ToolCallResult{}}
	m.AddToolResult(result, types.ToolStatusCompleted)
	block = m.views[0].(*reasoningblock.Model)
	after := ansi.Strip(block.View())
	visibleAfter := nestedOutputRows(after)
	require.Equal(t, visibleBefore, visibleAfter, "nested shell mid-scroll rows survive result view replacement")
}

func nestedOutputRows(rendered string) []string {
	var rows []string
	for _, line := range strings.Split(rendered, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "┃"))
		if strings.HasPrefix(line, "nested line ") {
			rows = append(rows, line)
		}
	}
	return rows
}

func TestShellHoverClearedWhenPendingInputIsInsertedAroundToolRows(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	m := NewScrollableView(ar, 100, 30, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	m.AddUserMessage("prompt")
	m.AddOrUpdateToolCall("root", shellCall("hover-a", `{"cmd":"printf a"}`), tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusRunning)
	m.AddOrUpdateToolCall("root", shellCall("hover-b", `{"cmd":"printf b"}`), tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusRunning)
	_ = m.View()
	second := m.views[2].(interface{ SetHoveredAt(int, bool) bool })
	require.True(t, second.SetHoveredAt(0, true))
	m.hoveredToolIndex, m.hoveredToolLine = 2, 0
	m.invalidateItem(2)
	_ = m.View()
	pending := types.Agent(types.MessageTypeUser, "root", "pending")
	pending.Pending = true
	pending.TurnID = "pending-turn"
	m.addPendingInput(pending)
	require.Equal(t, -1, m.hoveredToolIndex, "structural insertion clears view-local hover rather than leaving stale index")
	_ = m.View()
	toolIndex := 0
	for i, message := range m.messages {
		if message.Type == types.MessageTypeToolCall && message.ToolCall.ID == "hover-b" {
			toolIndex = i
		}
	}
	require.Positive(t, toolIndex)
	m.handleMouseMotion(tea.MouseMotionMsg{X: 4, Y: m.lineOffsets[toolIndex]})
	_ = m.View()
	require.Contains(t, ansi.Strip(m.renderedLine(m.lineOffsets[toolIndex])), ">", "tool row can receive hover after insertion")
}
