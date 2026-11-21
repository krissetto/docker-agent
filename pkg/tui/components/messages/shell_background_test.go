package messages

import (
	"fmt"
	"reflect"

	"github.com/charmbracelet/x/ansi"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func shellBackgroundList(t *testing.T, reasoning bool) (*model, *types.Message) {
	t.Helper()
	ar := animation.NewRuntime()
	m := New(ar, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetSize(90, 30)
	m.SetCompactWorkLog(false)
	if reasoning {
		m.AppendReasoning("root", "thinking")
	}
	m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"echo test"}`}}, tools.Tool{Name: "shell"}, types.ToolStatusRunning)
	m.AppendToolOutput(&runtime.ToolCallOutputEvent{ToolCallID: "call", Output: "before\n"})
	m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: "call", Response: tools.ShellBackgroundHandoffPrefix + "job_1_1. guidance"}, types.ToolStatusCompleted)
	var target *types.Message
	m.shellCards(func(_ int, msg *types.Message, _ layout.Model) { target = msg })
	require.NotNil(t, target)
	return m, target
}

func completionNotice(id, status string, code int, timeout bool) *session.Message {
	text := fmt.Sprintf("Shell command %s with status %s (exit code %d).\n", id, status, code)
	if timeout {
		text += "Error: command timed out after 1s\n"
	}
	text += "\n--- Untrusted command output ---\nafter\n"
	input := session.UserMessage(text)
	input.InputOrigin = session.InputOriginRuntime
	input.InputMode = "steer"
	return input
}

func TestShellBackgroundHandoffCompletionAndTerminalCache(t *testing.T) {
	for _, reasoning := range []bool{false, true} {
		t.Run(fmt.Sprint(reasoning), func(t *testing.T) {
			m, msg := shellBackgroundList(t, reasoning)
			require.Equal(t, "before\n", msg.Content)
			require.Equal(t, types.ToolStatusCompleted, msg.ToolStatus)
			require.False(t, msg.Background.Authoritative)
			process := tools.BackgroundProcess{ID: "job_1_1", SessionID: "owner", ToolCallID: "call", State: tools.BackgroundProcessRunning, StartedAt: time.Now().Add(-3 * time.Second)}
			m.SetShellBackgroundProcesses("owner", []tools.BackgroundProcess{process}, nil)
			require.Equal(t, tools.BackgroundProcessRunning, msg.Background.State)
			before := m.scrollOffset
			require.Nil(t, m.AddInputMessage(completionNotice("job_1_1", "failed", 7, false), -1))
			require.Equal(t, "after", msg.Content)
			require.Equal(t, 7, msg.Background.ExitCode)
			require.Equal(t, tools.BackgroundProcessFailed, msg.Background.State)
			m.SetShellBackgroundProcesses("owner", nil, nil)
			require.Equal(t, tools.BackgroundProcessFailed, msg.Background.State)
			require.Equal(t, before, m.scrollOffset)
			status := m.workStatusForIndices([]int{0})
			require.False(t, status.Active)
			require.True(t, status.BackgroundFailed)
		})
	}
}

func TestShellBackgroundEndedStoppedTimeoutAndUnmatched(t *testing.T) {
	for _, state := range []tools.BackgroundProcessState{tools.BackgroundProcessRunning, tools.BackgroundProcessStopped, tools.BackgroundProcessFailed} {
		t.Run(string(state), func(t *testing.T) {
			m, msg := shellBackgroundList(t, false)
			p := tools.BackgroundProcess{ID: "job_1_1", SessionID: "owner", ToolCallID: "call", State: state, TimedOut: state == tools.BackgroundProcessFailed}
			m.SetShellBackgroundProcesses("owner", []tools.BackgroundProcess{p}, nil)
			m.SetShellBackgroundProcesses("owner", nil, nil)
			if state == tools.BackgroundProcessRunning {
				require.Equal(t, tools.BackgroundProcessState("unknown"), msg.Background.State)
			} else {
				require.Equal(t, state, msg.Background.State)
			}
			require.False(t, m.applyShellCompletion(completionNotice("job_2_2", "completed", 0, false)))
			count := len(m.messages)
			m.AddInputMessage(completionNotice("job_2_2", "completed", 0, false), -1)
			require.Len(t, m.messages, count+1)
		})
	}
	m, msg := shellBackgroundList(t, false)
	require.True(t, m.applyShellCompletion(completionNotice("job_1_1", "failed", -1, true)))
	require.True(t, msg.Background.TimedOut)
	duplicate := *msg
	m.messages = append(m.messages, &duplicate)
	m.views = append(m.views, m.createToolCallView(&duplicate))
	require.False(t, m.applyShellCompletion(completionNotice("job_1_1", "completed", 0, false)))
}

func TestShellBackgroundLiveGatingCatchupAndIsolation(t *testing.T) {
	m, msg := shellBackgroundList(t, false)
	snapshots := 0
	source := func(id string) (tools.BackgroundProcessOutput, error) {
		snapshots++
		return tools.BackgroundProcessOutput{SessionID: "owner", ProcessID: id, ToolCallID: "call", State: tools.BackgroundProcessRunning, Output: "before\nmissed\n", Total: 14}, nil
	}
	process := tools.BackgroundProcess{ID: "job_1_1", SessionID: "owner", ToolCallID: "call", State: tools.BackgroundProcessRunning}
	m.SetShellBackgroundProcesses("owner", []tools.BackgroundProcess{process}, source)
	event := &runtime.BackgroundProcessOutputEvent{SessionID: "owner", ProcessID: "job_1_1", ToolCallID: "call", Output: "hidden"}
	m.AppendBackgroundOutput(event)
	require.Equal(t, "before\n", msg.Content)
	require.True(t, m.NavigateToToolCall("call"))
	cmd := m.syncBackgroundOutput()
	require.NotNil(t, cmd)
	runBackgroundCommand(m, cmd)
	require.Equal(t, 1, snapshots)
	require.Equal(t, "before\nmissed\n", msg.Content)
	before := m.scrollOffset
	event.Offset = 14
	event.Output = "live\n"
	require.Nil(t, m.AppendBackgroundOutput(event))
	require.Contains(t, msg.Content, "live\n")
	require.Equal(t, before, m.scrollOffset)
	for _, other := range []runtime.BackgroundProcessOutputEvent{{SessionID: "child", ProcessID: "job_1_1", ToolCallID: "call", Offset: 19, Output: "wrong"}, {SessionID: "owner", ProcessID: "job_2_2", ToolCallID: "call", Offset: 19, Output: "wrong"}, {SessionID: "owner", ProcessID: "job_1_1", ToolCallID: "different", Offset: 19, Output: "wrong"}} {
		m.AppendBackgroundOutput(&other)
	}
	require.NotContains(t, msg.Content, "wrong")
	m.views[0].(interface{ SetExpanded(bool) }).SetExpanded(false)
	m.syncBackgroundOutput()
	event.Offset = 19
	event.Output = "while collapsed"
	m.AppendBackgroundOutput(event)
	require.NotContains(t, msg.Content, "while collapsed")
	m.views[0].(interface{ SetExpanded(bool) }).SetExpanded(true)
	require.NotNil(t, m.syncBackgroundOutput())
	m.scrollOffset = 1000
	event.Output = "offscreen"
	m.AppendBackgroundOutput(event)
	require.NotContains(t, msg.Content, "offscreen")
}

func runBackgroundCommand(m *model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	result := cmd()
	if batch, ok := result.(tea.BatchMsg); ok {
		for _, nested := range batch {
			runBackgroundCommand(m, nested)
		}
		return
	}
	if result != nil && reflect.TypeOf(result).Kind() == reflect.Slice {
		list := reflect.ValueOf(result)
		for i := 0; i < list.Len(); i++ {
			runBackgroundCommand(m, list.Index(i).Interface().(tea.Cmd))
		}
		return
	}
	if snapshot, ok := result.(backgroundSnapshotMsg); ok {
		m.applyBackgroundSnapshot(snapshot)
	}
	if interest, ok := result.(backgroundInterestMsg); ok {
		m.Update(interest)
	}
}

func TestShellBackgroundHistoryRebuild(t *testing.T) {
	for _, reasoning := range []bool{false, true} {
		t.Run(fmt.Sprint(reasoning), func(t *testing.T) {
			m, _ := shellBackgroundList(t, false)
			sess := session.New(session.WithID("owner"))
			assistant := &chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"echo hi"}`}}}, ToolDefinitions: []tools.Tool{{Name: "shell"}}}
			if reasoning {
				assistant.ReasoningContent = "thinking"
			}
			sess.AddMessage(session.NewAgentMessage("root", assistant))
			sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: "call", Content: tools.ShellBackgroundHandoffPrefix + "job_1_1. guidance"}})
			sess.AddMessage(completionNotice("job_1_1", "completed", 0, false))
			m.LoadFromSession(sess, nil)
			calls := 0
			m.shellCards(func(_ int, msg *types.Message, _ layout.Model) {
				calls++
				require.NotNil(t, msg.Background)
				require.Equal(t, tools.BackgroundProcessCompleted, msg.Background.State)
				require.Equal(t, "after", msg.Content)
			})
			require.Equal(t, 1, calls)
			for _, msg := range m.messages {
				require.NotEqual(t, types.MessageTypeRuntimeNotice, msg.Type)
			}
		})
	}
}

func TestShellBackgroundSnapshotFallbackAndSessionSwitch(t *testing.T) {
	m, msg := shellBackgroundList(t, false)
	process := tools.BackgroundProcess{ID: "job_1_1", SessionID: "owner", ToolCallID: "call", State: tools.BackgroundProcessRunning}
	source := func(string) (tools.BackgroundProcessOutput, error) {
		return tools.BackgroundProcessOutput{}, tools.ErrBackgroundProcessOutputUnsupported
	}
	m.SetShellBackgroundProcesses("owner", []tools.BackgroundProcess{process}, source)
	m.NavigateToToolCall("call")
	runBackgroundCommand(m, m.syncBackgroundOutput())
	require.True(t, msg.Background.OutputUnavailable)
	require.Equal(t, tools.BackgroundProcessRunning, msg.Background.State)
	before := msg.Content
	m.SetShellBackgroundProcesses("new-session", nil, source)
	m.applyBackgroundSnapshot(backgroundSnapshotMsg{sessionID: "owner", processID: "job_1_1", snapshot: tools.BackgroundProcessOutput{SessionID: "owner", ProcessID: "job_1_1", ToolCallID: "call", Output: "old session", Total: 100}})
	require.Equal(t, before, msg.Content)
}

func TestShellBackgroundOffsetGapConcurrentJobsAndFinishedFold(t *testing.T) {
	m, first := shellBackgroundList(t, false)
	m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "second", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"second"}`}}, tools.Tool{Name: "shell"}, types.ToolStatusRunning)
	m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: "second", Response: tools.ShellBackgroundHandoffPrefix + "job_1_2. guidance"}, types.ToolStatusCompleted)
	processes := []tools.BackgroundProcess{{ID: "job_1_1", SessionID: "owner", ToolCallID: "call", State: tools.BackgroundProcessRunning}, {ID: "job_1_2", SessionID: "owner", ToolCallID: "second", State: tools.BackgroundProcessRunning}}
	source := func(id string) (tools.BackgroundProcessOutput, error) {
		return tools.BackgroundProcessOutput{SessionID: "owner", ProcessID: id, ToolCallID: "call", State: tools.BackgroundProcessRunning, Output: "catchup", Total: 90, Offset: 83}, nil
	}
	m.SetShellBackgroundProcesses("owner", processes, source)
	status := m.workStatusForIndices([]int{0, 1})
	require.False(t, status.Active)
	require.Equal(t, 2, status.BackgroundRunning)
	m.NavigateToToolCall("call")
	runBackgroundCommand(m, m.syncBackgroundOutput())
	require.Equal(t, "catchup", first.Content)
	event := &runtime.BackgroundProcessOutputEvent{SessionID: "owner", ProcessID: "job_1_1", ToolCallID: "call", Offset: 100, Output: "gap"}
	require.NotNil(t, m.AppendBackgroundOutput(event))
	require.Equal(t, "catchup", first.Content)
	require.True(t, m.applyShellCompletion(completionNotice("job_1_2", "completed", 0, false)))
	require.Equal(t, tools.BackgroundProcessRunning, first.Background.State)
	status = m.workStatusForIndices([]int{0, 1})
	require.False(t, status.Active)
	require.Equal(t, 1, status.BackgroundRunning)
}

func TestShellBackgroundNestedCardCatchesUp(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprint(compact), func(t *testing.T) {
			m, msg := shellBackgroundList(t, true)
			m.SetCompactWorkLog(compact)
			process := tools.BackgroundProcess{ID: "job_1_1", SessionID: "owner", ToolCallID: "call", State: tools.BackgroundProcessRunning}
			source := func(id string) (tools.BackgroundProcessOutput, error) {
				return tools.BackgroundProcessOutput{SessionID: "owner", ProcessID: id, ToolCallID: "call", State: tools.BackgroundProcessRunning, Output: "nested output", Total: 13}, nil
			}
			m.SetShellBackgroundProcesses("owner", []tools.BackgroundProcess{process}, source)
			require.True(t, m.NavigateToToolCall("call"))
			runBackgroundCommand(m, m.syncBackgroundOutput())
			require.Equal(t, "nested output", msg.Content)
		})
	}
}

func TestShellBackgroundHistoryNoticeBeforeHandoffStaysVisible(t *testing.T) {
	m, _ := shellBackgroundList(t, false)
	sess := session.New(session.WithID("owner"))
	call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"echo hi"}`}}
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}, ToolDefinitions: []tools.Tool{{Name: "shell"}}}))
	sess.AddMessage(completionNotice("job_1_1", "completed", 0, false))
	sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: "call", Content: tools.ShellBackgroundHandoffPrefix + "job_1_1. guidance"}})
	m.LoadFromSession(sess, nil)
	require.Equal(t, 1, m.MessageTypeCount(types.MessageTypeRuntimeNotice))
}

func TestShellBackgroundPresentationOnlyHistory(t *testing.T) {
	m, _ := shellBackgroundList(t, false)
	sess := session.New(session.WithID("owner"))
	text := tools.ShellBackgroundHandoffPrefix + "job_1_1. guidance"
	call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"echo hi"}`}}
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Presentation: []chat.AssistantPart{{Type: chat.AssistantPartToolCall, ToolCallID: call.ID, Tool: &chat.AssistantTool{Call: call, Definition: tools.Tool{Name: "shell"}, Result: &text}}}}))
	sess.AddMessage(completionNotice("job_1_1", "completed", 0, false))
	m.LoadFromSession(sess, nil)
	require.Equal(t, 0, m.MessageTypeCount(types.MessageTypeRuntimeNotice))
	m.shellCards(func(_ int, msg *types.Message, _ layout.Model) {
		require.Equal(t, tools.BackgroundProcessCompleted, msg.Background.State)
		require.Equal(t, "after", msg.Content)
	})
}

func TestShellBackgroundOutputInterestLifecycle(t *testing.T) {
	for _, mode := range []string{"plain", "reasoning", "work"} {
		t.Run(mode, func(t *testing.T) {
			m, msg := shellBackgroundList(t, mode != "plain")
			m.SetCompactWorkLog(mode == "work")
			var calls [][]string
			setter := func(ids []string) error { calls = append(calls, append([]string{}, ids...)); return nil }
			runBackgroundCommand(m, m.SetShellBackgroundOutputInterest("owner", setter))
			fetches := 0
			source := func(id string) (tools.BackgroundProcessOutput, error) {
				fetches++
				return tools.BackgroundProcessOutput{SessionID: "owner", ProcessID: id, ToolCallID: "call", State: tools.BackgroundProcessRunning, Output: "tail", Total: 4}, nil
			}
			process := tools.BackgroundProcess{ID: "job_1_1", SessionID: "owner", ToolCallID: "call", State: tools.BackgroundProcessRunning}
			runBackgroundCommand(m, m.SetShellBackgroundProcesses("owner", []tools.BackgroundProcess{process}, source))
			require.Empty(t, calls)
			require.Zero(t, fetches)
			require.True(t, m.NavigateToToolCall("call"))
			cmd := m.syncBackgroundOutput()
			require.Empty(t, calls, "Update does not call the setter synchronously")
			runBackgroundCommand(m, cmd)
			require.Equal(t, [][]string{{"job_1_1"}}, calls)
			require.Equal(t, 1, fetches)
			require.Equal(t, "tail", msg.Content)
			require.Nil(t, m.syncBackgroundOutput(), "unchanged set is a no-op")
			var card layout.Model
			m.shellCards(func(_ int, _ *types.Message, view layout.Model) { card = view })
			card.(interface{ SetExpanded(bool) }).SetExpanded(false)
			runBackgroundCommand(m, m.syncBackgroundOutput())
			require.Equal(t, [][]string{{"job_1_1"}, {}}, calls)
			card.(interface{ SetExpanded(bool) }).SetExpanded(true)
			m.scrollOffset = 10000
			runBackgroundCommand(m, m.syncBackgroundOutput())
			require.Len(t, calls, 2)
			require.True(t, m.NavigateToToolCall("call"))
			runBackgroundCommand(m, m.syncBackgroundOutput())
			require.Equal(t, 2, fetches)
			runBackgroundCommand(m, m.SetShellBackgroundOutputVisible(false))
			require.Empty(t, calls[len(calls)-1])
			runBackgroundCommand(m, m.SetShellBackgroundOutputVisible(true))
			require.Equal(t, 3, fetches)
			m.scrollOffset = 10000
			runBackgroundCommand(m, m.syncBackgroundOutput())
			require.Empty(t, calls[len(calls)-1], "expanded offscreen card loses interest")
			require.True(t, m.NavigateToToolCall("call"))
			runBackgroundCommand(m, m.syncBackgroundOutput())
			require.Equal(t, 4, fetches)
			require.True(t, m.applyShellCompletion(completionNotice("job_1_1", "completed", 0, false)))
			runBackgroundCommand(m, m.syncBackgroundOutput())
			require.Empty(t, calls[len(calls)-1])
			require.Nil(t, m.syncBackgroundOutput())
		})
	}
}

func TestShellBackgroundOutputInterestSessionSwitchAndSupersededCommands(t *testing.T) {
	m, _ := shellBackgroundList(t, false)
	var calls []string
	old := func(ids []string) error { calls = append(calls, fmt.Sprint("old:", ids)); return nil }
	runBackgroundCommand(m, m.SetShellBackgroundOutputInterest("owner", old))
	process := tools.BackgroundProcess{ID: "job_1_1", SessionID: "owner", ToolCallID: "call", State: tools.BackgroundProcessRunning}
	m.SetShellBackgroundProcesses("owner", []tools.BackgroundProcess{process}, nil)
	m.NavigateToToolCall("call")
	stale := m.syncBackgroundOutput()
	m.SetShellBackgroundOutputVisible(false)
	runBackgroundCommand(m, stale)
	require.Empty(t, calls, "late expand must not restore collapsed interest")
	runBackgroundCommand(m, m.SetShellBackgroundOutputVisible(true))
	require.Equal(t, []string{"old:[job_1_1]"}, calls)
	var newIDs []string
	clear := m.SetShellBackgroundOutputInterest("new", func(ids []string) error { newIDs = append(newIDs, ids...); calls = append(calls, "new"); return nil })
	runBackgroundCommand(m, m.SetShellBackgroundProcesses("new", nil, nil))
	runBackgroundCommand(m, clear)
	require.Equal(t, []string{"old:[job_1_1]", "old:[]"}, calls)
	require.Empty(t, newIDs)
}

func TestShellBackgroundOutputInterestClearsOldBeforeNew(t *testing.T) {
	m, _ := shellBackgroundList(t, false)
	var calls []string
	runBackgroundCommand(m, m.SetShellBackgroundOutputInterest("owner", func(ids []string) error { calls = append(calls, fmt.Sprint("old:", ids)); return nil }))
	processes := []tools.BackgroundProcess{{ID: "job_1_1", SessionID: "owner", ToolCallID: "call", State: tools.BackgroundProcessRunning}}
	m.SetShellBackgroundProcesses("owner", processes, nil)
	m.NavigateToToolCall("call")
	runBackgroundCommand(m, m.syncBackgroundOutput())
	delayedClear := m.SetShellBackgroundOutputInterest("new", func(ids []string) error { calls = append(calls, fmt.Sprint("new:", ids)); return nil })
	processes[0].SessionID = "new"
	runBackgroundCommand(m, m.SetShellBackgroundProcesses("new", processes, nil))
	runBackgroundCommand(m, delayedClear)
	require.Equal(t, []string{"old:[job_1_1]", "old:[]", "new:[job_1_1]"}, calls)
}

func TestShellBackgroundOutputInterestConcurrentCards(t *testing.T) {
	m, _ := shellBackgroundList(t, false)
	m.SetSize(90, 100)
	m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "second", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"second"}`}}, tools.Tool{Name: "shell"}, types.ToolStatusRunning)
	m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: "second", Response: tools.ShellBackgroundHandoffPrefix + "job_1_2. guidance"}, types.ToolStatusCompleted)
	var calls [][]string
	m.SetShellBackgroundOutputInterest("owner", func(ids []string) error { calls = append(calls, append([]string{}, ids...)); return nil })
	processes := []tools.BackgroundProcess{{ID: "job_1_1", SessionID: "owner", ToolCallID: "call", State: tools.BackgroundProcessRunning}, {ID: "job_1_2", SessionID: "owner", ToolCallID: "second", State: tools.BackgroundProcessRunning}}
	m.SetShellBackgroundProcesses("owner", processes, nil)
	m.NavigateToToolCall("call")
	m.NavigateToToolCall("second")
	runBackgroundCommand(m, m.syncBackgroundOutput())
	require.Equal(t, [][]string{{"job_1_1", "job_1_2"}}, calls)
	processes[0].State = tools.BackgroundProcessCompleted
	runBackgroundCommand(m, m.SetShellBackgroundProcesses("owner", processes, nil))
	require.Equal(t, []string{"job_1_2"}, calls[len(calls)-1])
	require.Nil(t, m.syncBackgroundOutput())
}

func TestShellBackgroundHistoryExpiredOutputAndReusedProcessID(t *testing.T) {
	for _, variant := range []string{"notice", "expired", "reused"} {
		t.Run(variant, func(t *testing.T) {
			m, _ := shellBackgroundList(t, false)
			sess := session.New(session.WithID("owner"))
			add := func(id string) {
				call := tools.ToolCall{ID: id, Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"echo hi"}`}}
				sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}, ToolDefinitions: []tools.Tool{{Name: "shell"}}}))
				sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: id, Content: tools.ShellBackgroundHandoffPrefix + "job_1_1. guidance"}})
			}
			add("call")
			if variant != "expired" {
				sess.AddMessage(completionNotice("job_1_1", "completed", 0, false))
			}
			if variant == "reused" {
				add("second")
				sess.AddMessage(completionNotice("job_1_1", "completed", 0, false))
			}
			m.LoadFromSession(sess, nil)
			source := func(string) (tools.BackgroundProcessOutput, error) {
				return tools.BackgroundProcessOutput{}, tools.ErrBackgroundProcessNotFound
			}
			var processes []tools.BackgroundProcess
			if variant == "reused" {
				processes = []tools.BackgroundProcess{{ID: "job_1_1", SessionID: "owner", ToolCallID: "second", State: tools.BackgroundProcessRunning}}
			}
			runBackgroundCommand(m, m.SetShellBackgroundProcesses("owner", processes, source))
			if variant == "reused" {
				require.Equal(t, 2, m.MessageTypeCount(types.MessageTypeRuntimeNotice))
			}
			m.shellCards(func(_ int, msg *types.Message, view layout.Model) {
				require.False(t, msg.Background.Authoritative && msg.Background.State == tools.BackgroundProcessRunning)
				if variant == "notice" {
					require.Equal(t, "after", msg.Content)
				}
				if variant == "expired" {
					view.(interface{ SetExpanded(bool) }).SetExpanded(false)
					require.Contains(t, ansi.Strip(view.View()), "in background")
				}
			})
		})
	}
}

func TestShellBackgroundOutputInterestUnsupported(t *testing.T) {
	m, msg := shellBackgroundList(t, false)
	m.SetShellBackgroundOutputInterest("owner", func([]string) error { return tools.ErrBackgroundProcessOutputUnsupported })
	source := func(string) (tools.BackgroundProcessOutput, error) {
		return tools.BackgroundProcessOutput{}, tools.ErrBackgroundProcessOutputUnsupported
	}
	m.SetShellBackgroundProcesses("owner", []tools.BackgroundProcess{{ID: "job_1_1", SessionID: "owner", ToolCallID: "call", State: tools.BackgroundProcessRunning}}, source)
	m.NavigateToToolCall("call")
	runBackgroundCommand(m, m.syncBackgroundOutput())
	require.True(t, msg.Background.OutputUnavailable)
	require.Equal(t, tools.BackgroundProcessRunning, msg.Background.State)
	require.Nil(t, m.syncBackgroundOutput())
}

func TestShellBackgroundUnseenListsDoNotEndHandoff(t *testing.T) {
	m, msg := shellBackgroundList(t, false)
	m.SetShellBackgroundProcesses("owner", nil, nil)
	require.Empty(t, msg.Background.State)
	require.False(t, msg.Background.Authoritative)
	process := tools.BackgroundProcess{ID: "job_1_1", SessionID: "owner", ToolCallID: "call", State: tools.BackgroundProcessRunning}
	m.SetShellBackgroundProcesses("owner", []tools.BackgroundProcess{process}, nil)
	require.Equal(t, tools.BackgroundProcessRunning, msg.Background.State)
	require.True(t, msg.Background.Authoritative)
}
