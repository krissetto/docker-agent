package messages

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestShellDisclosureMouseToggleRefreshesTranscriptGeometry(t *testing.T) {
	scheduler := &shellDisclosureScheduler{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	t.Cleanup(ar.Stop)
	m := NewScrollableView(ar, 100, 40, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: `{"cmd":"printf first; printf second"}`}}
	def := tools.Tool{Name: builtinshell.ToolNameShell}
	m.AddOrUpdateToolCall("root", call, def, types.ToolStatusRunning)
	m.AppendToolOutput(&runtime.ToolCallOutputEvent{ToolCallID: call.ID, Output: "first output\nsecond output\n"})
	m.AddUserMessage("following message")
	_ = m.View()

	updated := call
	updated.Function.Arguments = `{"cmd":"printf updated command"}`
	m.AddOrUpdateToolCall("root", updated, def, types.ToolStatusRunning)
	_ = m.View()
	require.Contains(t, ansi.Strip(m.renderedLine(m.lineOffsets[0])), "printf updated command", "collapsed summary follows live argument updates")
	beforeHeight, beforeFollowing := m.totalHeight, m.lineOffsets[1]
	summary := m.renderedLine(m.lineOffsets[0])
	toggle := m.views[0].(interface{ IsToggleAt(int, int) bool })
	clickCol := 0
	require.True(t, toggle.IsToggleAt(0, clickCol))
	m.handleMouseClick(tea.MouseClickMsg{X: clickCol, Y: 0, Button: tea.MouseLeft})
	require.True(t, shellIsExpanded(m, 0))
	advanceShellDisclosure(t, ar, scheduler, m)
	_ = m.View()
	require.Greater(t, m.totalHeight, beforeHeight)
	require.Greater(t, m.lineOffsets[1], beforeFollowing, "cached following range moves with expanded shell output")
	require.Contains(t, ansi.Strip(strings.Join(m.renderedLines, "\n")), "second output")

	clickCol = max(0, ansi.StringWidth(summary)-3)
	require.True(t, toggle.IsToggleAt(0, clickCol), "the status end of the header is also clickable")
	m.handleMouseClick(tea.MouseClickMsg{X: clickCol, Y: 0, Button: tea.MouseLeft})
	require.False(t, shellIsExpanded(m, 0))
	advanceShellDisclosure(t, ar, scheduler, m)
	_ = m.View()
	require.Less(t, m.totalHeight, beforeHeight+4)
	require.NotContains(t, ansi.Strip(strings.Join(m.renderedLines, "\n")), "first output")

	m.AppendToolOutput(&runtime.ToolCallOutputEvent{ToolCallID: call.ID, Output: "third output\n"})
	_ = m.View()
	require.NotContains(t, ansi.Strip(strings.Join(m.renderedLines, "\n")), "third output", "live output remains folded until shell disclosure is opened")
	m.handleMouseClick(tea.MouseClickMsg{X: clickCol, Y: 0, Button: tea.MouseLeft})
	advanceShellDisclosure(t, ar, scheduler, m)
	_ = m.View()
	require.Contains(t, ansi.Strip(strings.Join(m.renderedLines, "\n")), "third output", "live output is not lost while collapsed")
}

func TestShellDisclosureReplayAndReasoningDisclosureAreIndependent(t *testing.T) {
	scheduler := &shellDisclosureScheduler{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	t.Cleanup(ar.Stop)
	state := &service.SessionState{}
	m := NewScrollableView(ar, 100, 40, state).(*model)
	t.Cleanup(m.StopAnimations)
	call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: `{"cmd":"printf replay"}`}}
	def := tools.Tool{Name: builtinshell.ToolNameShell}
	sess := session.New(session.WithID("shell-replay"))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}, ToolDefinitions: []tools.Tool{def}}))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleTool, ToolCallID: call.ID, Content: "replayed first\nreplayed second"}))
	m.LoadFromSession(sess, nil)
	_ = m.View()
	require.NotContains(t, ansi.Strip(strings.Join(m.renderedLines, "\n")), "replayed first")
	row := m.renderedLine(m.lineOffsets[0])
	col := max(0, ansi.StringWidth(row)-3)
	require.True(t, m.views[0].(interface{ IsToggleAt(int, int) bool }).IsToggleAt(0, col))
	m.handleMouseClick(tea.MouseClickMsg{X: col, Y: 0, Button: tea.MouseLeft})
	require.True(t, shellIsExpanded(m, 0))
	advanceShellDisclosure(t, ar, scheduler, m)
	_ = m.View()
	require.Contains(t, ansi.Strip(strings.Join(m.renderedLines, "\n")), "replayed second")

	blockModel := NewScrollableView(ar, 100, 40, state).(*model)
	t.Cleanup(blockModel.StopAnimations)
	blockModel.AppendReasoning("root", "Working through the shell request.")
	blockModel.AddOrUpdateToolCall("root", call, def, types.ToolStatusRunning)
	blockModel.AppendToolOutput(&runtime.ToolCallOutputEvent{ToolCallID: call.ID, Output: "nested full output"})
	block := blockModel.views[0].(*reasoningblock.Model)
	headerX, headerY := reasoningHeaderClick(t, block)
	blockModel.handleMouseClick(tea.MouseClickMsg{X: headerX, Y: headerY, Button: tea.MouseLeft})
	advanceShellDisclosure(t, ar, scheduler, blockModel)
	_ = blockModel.View()
	require.NotContains(t, ansi.Strip(strings.Join(blockModel.renderedLines, "\n")), "nested full output", "opening reasoning reveals only the shell summary")

	localLines := strings.Split(block.View(), "\n")
	toolLine, toolCol := -1, -1
	for line, text := range localLines {
		if !strings.Contains(ansi.Strip(text), "printf replay") {
			continue
		}
		toolLine = line
		toolCol = max(0, ansi.StringWidth(text)-3)
		require.True(t, block.IsToggleAt(line, toolCol), "clicking shell's status end expands the row")
		break
	}
	require.GreaterOrEqual(t, toolLine, 0, "nested shell row exposes its own disclosure hit target")
	blockModel.handleMouseClick(tea.MouseClickMsg{X: toolCol, Y: toolLine, Button: tea.MouseLeft})
	advanceShellDisclosure(t, ar, scheduler, blockModel)
	_ = blockModel.View()
	require.Contains(t, ansi.Strip(strings.Join(blockModel.renderedLines, "\n")), "nested full output")

	headerX, headerY = reasoningHeaderClick(t, block)
	blockModel.handleMouseClick(tea.MouseClickMsg{X: headerX, Y: headerY, Button: tea.MouseLeft})
	advanceShellDisclosure(t, ar, scheduler, blockModel)
	_ = blockModel.View()
	require.NotContains(t, ansi.Strip(strings.Join(blockModel.renderedLines, "\n")), "nested full output", "collapsing parent hides children without changing the shell's own disclosure state")
	blockModel.handleMouseClick(tea.MouseClickMsg{X: headerX, Y: headerY, Button: tea.MouseLeft})
	advanceShellDisclosure(t, ar, scheduler, blockModel)
	_ = blockModel.View()
	require.Contains(t, ansi.Strip(strings.Join(blockModel.renderedLines, "\n")), "nested full output", "reopening reasoning restores the explicitly expanded shell detail")
}

type shellDisclosureScheduler struct {
	now  time.Time
	tick func(time.Time) tea.Msg
}

func (s *shellDisclosureScheduler) Now() time.Time { return s.now }
func (s *shellDisclosureScheduler) Tick(_ time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	s.tick = create
	return nil
}
func (s *shellDisclosureScheduler) fire(delta time.Duration) tea.Msg {
	s.now = s.now.Add(delta)
	msg := s.tick(s.now)
	s.tick = nil
	return msg
}

func advanceShellDisclosure(t *testing.T, ar *animation.Runtime, scheduler *shellDisclosureScheduler, m *model) {
	t.Helper()
	ar.Continue()
	msg, ok := scheduler.fire(200 * time.Millisecond).(animation.TickMsg)
	require.True(t, ok)
	accepted, ok := ar.Accept(msg)
	require.True(t, ok)
	m.Update(accepted)
}

func reasoningHeaderClick(t *testing.T, block *reasoningblock.Model) (x, y int) {
	t.Helper()
	for line, text := range strings.Split(block.View(), "\n") {
		if !block.IsHeaderLine(line) {
			continue
		}
		for col := range ansi.StringWidth(text) {
			if block.IsToggleAt(line, col) {
				return col, line
			}
		}
	}
	t.Fatal("reasoning block header has no toggle target")
	return -1, -1
}

func shellIsExpanded(m *model, index int) bool {
	view, ok := m.views[index].(interface{ IsExpanded() bool })
	return ok && view.IsExpanded()
}
