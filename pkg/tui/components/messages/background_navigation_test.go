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
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestNavigateToUniqueToolCallAndRejectAmbiguousOrMissing(t *testing.T) {
	call := tools.ToolCall{ID: "shell-call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"echo hi"}`}}
	message := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}})
	sess := session.New(session.WithID("owner"))
	sess.AddMessage(message)
	ar := animation.NewRuntime()
	list := New(ar, &service.SessionState{}).(*model)
	t.Cleanup(func() { list.StopAnimations(); ar.Stop() })
	list.SetSize(80, 8)
	list.LoadFromSession(sess, nil)

	require.True(t, list.HasUniqueToolCall("shell-call"))
	require.True(t, list.NavigateToToolCall("shell-call"))
	require.GreaterOrEqual(t, list.scrollOffset, 0)
	require.False(t, list.NavigateToToolCall("not-in-transcript"))

	duplicate := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}})
	sess.AddMessage(duplicate)
	list.LoadFromSession(sess, nil)
	require.False(t, list.HasUniqueToolCall("shell-call"))
	require.False(t, list.NavigateToToolCall("shell-call"))
}

func TestNavigateToToolCardSettlesCollapsedWorkAndThinkingGeometry(t *testing.T) {
	call := tools.ToolCall{ID: "target-call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"UNIQUE-TARGET"}`}}
	cases := []struct {
		name     string
		build    func(*model)
		wantWork bool
	}{
		{
			name: "work",
			build: func(m *model) {
				_ = m.SetCompactWorkLog(true)
				m.addMessage(types.User("first user"))
				m.AppendReasoning("root", strings.Repeat("earlier content\n", 12))
				m.CommitAssistant("root", nil)
				m.addMessage(types.User("second user"))
				m.AppendReasoning("root", "thinking before tool")
				m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "shell"}, types.ToolStatusRunning)
				m.CommitAssistant("root", []string{call.ID})
				m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: call.ID, Response: "UNIQUE-TARGET", Result: &tools.ToolCallResult{Output: "UNIQUE-TARGET"}}, types.ToolStatusCompleted)
				m.workProjectionDirty = true
				m.ensureAllItemsRendered()
			},
			wantWork: true,
		},
		{
			name: "thinking",
			build: func(m *model) {
				_ = m.SetCompactWorkLog(false)
				m.AddUserMessage("first user")
				_ = m.addMessage(types.Agent(types.MessageTypeAssistant, "root", strings.Repeat("earlier content\n", 12)))
				m.AddUserMessage("second user")
				m.AppendReasoning("root", "reasoning before target")
				m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "shell"}, types.ToolStatusCompleted)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ar := animation.NewRuntime()
			m := New(ar, &service.SessionState{}).(*model)
			t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
			m.SetSize(64, 8)
			tc.build(m)
			m.ensureAllItemsRendered()
			target := m.toolCallSource(call.ID)
			require.GreaterOrEqual(t, target, 0)
			m.focused = true
			m.selectedMessageIndex = 0
			m.userHasScrolled = true
			m.setScrollOffset(0)
			m.selection = selectionState{active: true, startLine: 0, startCol: 0, endLine: 0, endCol: 4}
			priorSelection, priorTextSelection := m.selectedMessageIndex, m.selection
			require.True(t, m.NavigateToToolCall(call.ID))
			require.Equal(t, priorSelection, m.selectedMessageIndex, "navigation preserves selected message")
			require.Equal(t, priorTextSelection, m.selection, "navigation preserves selected user text")
			require.True(t, m.userHasScrolled, "navigation must not re-enable follow-tail behavior")
			require.Positive(t, m.scrollOffset, "older work is scrolled to the selected tool")
			require.Contains(t, ansi.Strip(m.View()), "UNIQUE-TARGET", "the original tool content itself must be visible")
			if tc.wantWork {
				block := m.workBlockForSource(target)
				require.NotNil(t, block)
				require.True(t, block.Expanded(), "target rows need settled expanded geometry")
				visible := false
				reasoning, hasThinking := m.views[target].(*reasoningblock.Model)
				toolLine, hasToolLine := 0, false
				if hasThinking {
					toolLine, hasToolLine = reasoning.ToolCallLine(call.ID)
				}
				for _, row := range block.Rows() {
					if row.SourceIndex == target {
						line := row.Start + m.workRangeStart(target)
						if hasToolLine {
							line += toolLine
						}
						visible = line >= m.scrollOffset && line < m.scrollOffset+m.height
					}
				}
				require.True(t, visible, "the target row is actually in view")
			} else {
				reasoning, ok := m.views[target].(*reasoningblock.Model)
				require.True(t, ok)
				require.True(t, reasoning.IsExpanded())
				line, found := reasoning.ToolCallLine(call.ID)
				require.True(t, found)
				line += m.lineOffsets[target]
				require.True(t, line >= m.scrollOffset && line < m.scrollOffset+m.height, "the target tool row is actually in view")
			}
			offset := m.scrollOffset
			_ = m.AddAssistantMessage("root", "APPENDED-AFTER-NAVIGATION")
			require.Equal(t, offset, m.scrollOffset, "new streaming content must not snap navigation to the tail")
		})
	}
}
