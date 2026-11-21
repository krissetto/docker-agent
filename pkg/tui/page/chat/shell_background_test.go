package chat

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestShellBackgroundRuntimeEventsKeepOriginalCardAndIsolateChild(t *testing.T) {
	sess := session.New(session.WithID("owner"))
	call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"echo ORIGINAL"}`}}
	sess.AddMessage(session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}, ToolDefinitions: []tools.Tool{{Name: "shell"}}}))
	sess.AddMessage(&session.Message{Message: chatmsg.Message{Role: chatmsg.MessageRoleTool, ToolCallID: call.ID, Content: tools.ShellBackgroundHandoffPrefix + "job_1_1. guidance"}})
	page := newBackgroundProcessPage(t, sess)
	process := backgroundProcess("job_1_1", sess.ID, call.ID, "echo ORIGINAL")
	page.handleRuntimeEvent(&runtime.BackgroundProcessesEvent{SessionID: sess.ID, Processes: []tools.BackgroundProcess{process}})
	require.True(t, page.messages.NavigateToToolCall(call.ID))
	handled, cmd := page.handleRuntimeEvent(&runtime.BackgroundProcessOutputEvent{SessionID: "child", ProcessID: process.ID, ToolCallID: call.ID, Output: "CHILD OUTPUT"})
	require.True(t, handled)
	require.Nil(t, cmd)
	require.NotContains(t, ansi.Strip(page.messages.View()), "CHILD OUTPUT")
	notice := session.UserMessage("Shell command job_1_1 with status failed (exit code 9).\n\n--- Untrusted command output ---\nFINAL OUTPUT")
	notice.InputOrigin, notice.InputMode = session.InputOriginRuntime, "steer"
	page.messages.AddInputMessage(notice, -1)
	rendered := ansi.Strip(page.messages.View())
	require.Contains(t, rendered, "FINAL OUTPUT")
	require.Contains(t, rendered, "exit 9")
	require.Equal(t, 0, page.messages.MessageTypeCount(types.MessageTypeRuntimeNotice))
	page.handleRuntimeEvent(&runtime.BackgroundProcessesEvent{SessionID: sess.ID})
	require.Contains(t, ansi.Strip(page.messages.View()), "exit 9")
}

func TestShellBackgroundReusedProcessIDDoesNotLinkOrSuppressNotices(t *testing.T) {
	sess := session.New(session.WithID("owner"))
	for _, id := range []string{"first", "second"} {
		call := tools.ToolCall{ID: id, Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"echo hi"}`}}
		sess.AddMessage(session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}, ToolDefinitions: []tools.Tool{{Name: "shell"}}}))
		sess.AddMessage(&session.Message{Message: chatmsg.Message{Role: chatmsg.MessageRoleTool, ToolCallID: id, Content: tools.ShellBackgroundHandoffPrefix + "job_1_1. guidance"}})
		notice := session.UserMessage("Shell command job_1_1 with status completed (exit code 0).\n\n--- Untrusted command output ---\nnotice " + id)
		notice.InputOrigin, notice.InputMode = session.InputOriginRuntime, "steer"
		sess.AddMessage(notice)
	}
	page := newBackgroundProcessPage(t, sess)
	page.setBackgroundProcesses(sess.ID, []tools.BackgroundProcess{backgroundProcess("job_1_1", sess.ID, "second", "echo hi")})
	require.Empty(t, page.backgroundProcessToolCalls)
	require.False(t, page.navigateToBackgroundProcess("second"))
	require.Equal(t, 2, page.messages.MessageTypeCount(types.MessageTypeRuntimeNotice))
}
