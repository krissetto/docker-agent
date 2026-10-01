package messages

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type countedHistoryView struct {
	renders int
	content string
}

func (*countedHistoryView) SetSize(int, int) tea.Cmd                 { return nil }
func (*countedHistoryView) Init() tea.Cmd                            { return nil }
func (v *countedHistoryView) Update(tea.Msg) (layout.Model, tea.Cmd) { return v, nil }
func (v *countedHistoryView) View() string                           { v.renders++; return v.content }

func TestLiveHistoryDirtyRebuildReusesSingleLineOwner(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
	var views []*countedHistoryView
	for i := range 600 {
		view := &countedHistoryView{content: fmt.Sprintf("message %d", i)}
		views = append(views, view)
		m.messages = append(m.messages, types.User(view.content))
		m.views = append(m.views, view)
	}
	m.ensureAllItemsRendered()
	lines := strings.Join(m.renderedLines, "\n")
	m.invalidateItem(599)
	m.ensureAllItemsRendered()
	assert.Equal(t, lines, strings.Join(m.renderedLines, "\n"))
	for _, view := range views[:599] {
		require.Equal(t, 1, view.renders, "history must not be rendered again above former LRU capacity")
	}
	require.Equal(t, 2, views[599].renders)
	for _, item := range m.renderedItems {
		assert.LessOrEqual(t, item.start+item.height, len(m.renderedLines))
	}
	m.AddAssistantMessage("root", "")
	m.ensureAllItemsRendered()
	m.RemoveSpinner()
	m.ensureAllItemsRendered()
	for _, view := range views[:599] {
		require.Equal(t, 1, view.renders, "spinner removal must preserve history")
	}
}

func TestResetFromSessionPreservesReaderPosition(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 80, 8, &service.SessionState{}).(*model)
	sess := session.New(session.WithID("s"))
	for i := range 30 {
		sess.AddMessage(session.UserMessage(fmt.Sprintf("line %d", i)))
	}
	m.LoadFromSession(sess, nil)
	m.ensureAllItemsRendered()
	m.scrollOffset, m.userHasScrolled, m.selectedMessageIndex = 7, true, 3
	m.focused = true
	sess.AddMessage(session.UserMessage("recovered"))
	m.ResetFromSession(sess, nil)
	assert.Equal(t, 7, m.scrollOffset)
	assert.True(t, m.userHasScrolled)
	assert.Equal(t, 3, m.selectedMessageIndex)
	assert.Contains(t, strings.Join(m.renderedLines, "\n"), "recovered")
}

func BenchmarkLiveHistoryDirtyRebuild(b *testing.B) {
	for _, count := range []int{500, 600, 1000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			m := NewScrollableView(animation.NewRuntime(), 120, 40, &service.SessionState{}).(*model)
			for i := range count {
				m.addMessage(types.Agent(types.MessageTypeAssistant, "root", strconv.Itoa(i)+strings.Repeat("content ", 20)))
			}
			m.View()
			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				m.invalidateItem(count - 1)
				m.View()
			}
		})
	}
}

func TestHistoricalAssistantSegmentsDoNotRetainHeavyRenderers(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
	for range 600 {
		m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "**historical** paragraph\n\n"))
	}
	m.ensureAllItemsRendered()
	for _, view := range m.views[:599] {
		require.False(t, view.(interface{ HasLiveRenderState() bool }).HasLiveRenderState())
	}
	m.invalidateItem(1)
	m.ensureAllItemsRendered()
	require.False(t, m.views[1].(interface{ HasLiveRenderState() bool }).HasLiveRenderState())
}

func TestTypedInputHistoryAndLiveRendering(t *testing.T) {
	const literal = "<system_info>user literal</system_info>"
	sess := session.New()
	for _, origin := range []session.InputOrigin{session.InputOriginRuntime, session.InputOriginAgent, session.InputOriginUser, "", "future"} {
		body := literal
		if origin == session.InputOriginRuntime {
			body = "<system_info>runtime secret</system_info>"
		}
		if origin == session.InputOriginAgent {
			body = "clean agent body <system_info>agent literal</system_info>"
		}
		msg := session.UserMessage(body)
		msg.InputOrigin, msg.InputMode, msg.SenderID, msg.SenderName = origin, "steer", "child", "worker"
		sess.AddMessage(msg)
	}
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "assistant literal <system_info>kept</system_info>", ToolCalls: []tools.ToolCall{{ID: "tool", Function: tools.FunctionCall{Name: "read", Arguments: "{}"}}}}))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleTool, ToolCallID: "tool", Content: "tool literal <system_info>kept</system_info>"}))
	m := NewScrollableView(animation.NewRuntime(), 100, 24, &service.SessionState{}).(*model)
	m.LoadFromSession(sess, nil)
	m.ensureAllItemsRendered()
	out := ansi.Strip(strings.Join(m.renderedLines, "\n"))
	assert.NotContains(t, out, "runtime secret")
	assert.Contains(t, out, literal)
	assert.Contains(t, out, "clean agent body <system_info>agent literal</system_info>")
	assert.Contains(t, out, "worker")
	assert.Contains(t, out, "child")
	assert.Contains(t, out, "assistant literal")
	assert.Equal(t, 3, m.MessageTypeCount(types.MessageTypeUser))
	var toolBody string
	for _, message := range m.messages {
		if message.Type == types.MessageTypeToolCall {
			toolBody = message.Content
		}
	}
	assert.Contains(t, toolBody, "tool literal <system_info>kept</system_info>")
	hidden := session.UserMessage("runtime live secret")
	hidden.InputOrigin = session.InputOriginRuntime
	before := len(m.messages)
	m.AddInputMessage(hidden, 7)
	assert.Len(t, m.messages, before+1)
	assert.Equal(t, types.MessageTypeRuntimeNotice, m.messages[before].Type)
	agent := session.UserMessage("live clean agent")
	agent.InputOrigin, agent.InputMode, agent.SenderID, agent.SenderName = session.InputOriginAgent, "steer", "child", "worker"
	m.AddInputMessage(agent, 8)
	assert.Equal(t, types.MessageTypeAgentInput, m.messages[len(m.messages)-1].Type)
	assert.Equal(t, "worker", m.messages[len(m.messages)-1].SenderName)
	assert.Equal(t, "child", m.messages[len(m.messages)-1].SenderID)
}

func TestAgentInputIsNotAnAssistantStreamingTail(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 100, 24, &service.SessionState{}).(*model)
	input := session.UserMessage("agent body")
	input.InputOrigin, input.SenderName = session.InputOriginAgent, "worker"
	m.AddInputMessage(input, 0)
	m.AppendToLastMessage("worker", "assistant body")
	require.Len(t, m.messages, 2)
	assert.Equal(t, "agent body", m.messages[0].Content)
	assert.Equal(t, "assistant body", m.messages[1].Content)
	assert.Equal(t, session.InputOriginAgent, m.messages[0].InputOrigin)
	assert.Empty(t, m.messages[1].InputOrigin)
}

func TestTypedInputPresentationModesLiveAndReload(t *testing.T) {
	const senderID = "12345678-aaaa-bbbb-cccc-123456789abc"
	for _, mode := range []string{"turn", "steer", ""} {
		t.Run("agent_"+mode, func(t *testing.T) {
			input := session.UserMessage("clean delegation **literal** <system_info>user text</system_info>")
			input.InputOrigin, input.InputMode = session.InputOriginAgent, mode
			input.SenderName, input.SenderID, input.TurnID = "director", senderID, "input"
			sess := session.New()
			sess.AddMessage(input)
			live := NewScrollableView(animation.NewRuntime(), 100, 40, &service.SessionState{}).(*model)
			live.AddInputMessage(input, 0)
			restored := NewScrollableView(animation.NewRuntime(), 100, 40, &service.SessionState{}).(*model)
			restored.LoadFromSession(sess, nil)
			for _, m := range []*model{live, restored} {
				require.Len(t, m.messages, 1)
				assert.NotEqual(t, types.MessageTypeAssistant, m.messages[0].Type)
				assert.Equal(t, senderID, m.messages[0].SenderID)
				assert.Equal(t, "director", m.messages[0].SenderName)
				assert.Equal(t, mode, m.messages[0].InputMode)
				assert.Equal(t, types.MessageTypeAgentInput, m.messages[0].Type)
				assert.Nil(t, m.messages[0].SessionPosition, "agent inputs must not expose the user editor")
				m.ensureAllItemsRendered()
				out := ansi.Strip(strings.Join(m.renderedLines, "\n"))
				assert.Contains(t, out, input.Message.Content)
				assert.NotContains(t, out, senderID)
				if mode == "steer" {
					assert.Contains(t, out, "director (12345)")
				}
			}
			assert.Equal(t, live.messages[0].Type, restored.messages[0].Type)
			assert.Equal(t, live.renderedLines, restored.renderedLines)
		})
	}
	for _, sender := range []string{senderID, ""} {
		t.Run("runtime_"+sender, func(t *testing.T) {
			input := session.UserMessage("<system_info>private model envelope and report body</system_info>")
			input.InputOrigin, input.InputMode, input.SenderID = session.InputOriginRuntime, "steer", sender
			if sender != "" {
				input.SenderName = "worker"
			}
			sess := session.New()
			sess.AddMessage(input)
			for _, restore := range []bool{false, true} {
				m := NewScrollableView(animation.NewRuntime(), 100, 40, &service.SessionState{}).(*model)
				if restore {
					m.LoadFromSession(sess, nil)
				} else {
					m.AddInputMessage(input, 0)
				}
				require.Len(t, m.messages, 1, "runtime controls need a clean visible notice")
				assert.NotEqual(t, types.MessageTypeUser, m.messages[0].Type)
				assert.NotEqual(t, types.MessageTypeAssistant, m.messages[0].Type)
				assert.Nil(t, m.messages[0].SessionPosition)
				m.ensureAllItemsRendered()
				out := ansi.Strip(strings.Join(m.renderedLines, "\n"))
				assert.NotContains(t, out, "system_info")
				assert.NotContains(t, out, "private model")
				if sender != "" {
					assert.Contains(t, out, "worker (12345) has finished their work")
				} else {
					assert.Contains(t, out, "Runtime update received")
				}
			}
		})
	}
}
