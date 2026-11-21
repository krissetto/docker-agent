package chat

import (
	"context"
	"net"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/app"
	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/server"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type backgroundRemoteSession struct {
	*sessionTestSession
	lock      sync.Mutex
	initial   *session.Session
	processes []tools.BackgroundProcess
	output    string
	events    chan runtime.SessionEvent
	interest  []string
}

func (s *backgroundRemoteSession) Observe(_ context.Context, opts runtime.ObserveOptions) (runtime.Observation, error) {
	if opts.Tree {
		return runtime.Observation{}, runtime.UnsupportedSessionOperation(s.id, "tree")
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	s.interest = slices.Clone(opts.BackgroundOutputInterest[s.id])
	s.events = make(chan runtime.SessionEvent, 32)
	return runtime.Observation{
		Initial: []runtime.SessionSnapshot{{Session: s.initial.Clone(), Status: runtime.SessionStatus{SessionID: s.id, AgentName: "root", State: runtime.SessionStateRunning}, Epoch: "epoch", BackgroundProcesses: slices.Clone(s.processes), TranscriptPosition: s.initial.ItemCount()}},
		Events:  s.events, Cancel: func() {},
		SetBackgroundOutputInterest: func(id string, ids []string) error {
			s.lock.Lock()
			defer s.lock.Unlock()
			s.interest = slices.Clone(ids)
			return nil
		},
	}, nil
}
func (s *backgroundRemoteSession) BackgroundProcessOutput(_ context.Context, id string, _ int) (tools.BackgroundProcessOutput, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	return tools.BackgroundProcessOutput{SessionID: s.id, ProcessID: id, ToolCallID: "call", State: tools.BackgroundProcessRunning, Output: s.output, Total: int64(len(s.output))}, nil
}

type backgroundRemoteRuntime struct {
	*sessionTestRuntime
	handle *backgroundRemoteSession
}

func (r *backgroundRemoteRuntime) SessionByID(string) (runtime.SessionHandle, error) {
	return r.handle, nil
}

func TestBackgroundShellManagedRemoteLinkAndOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sess := session.New(session.WithID("remote-owner"), session.WithAgentName("root"))
	local := &backgroundRemoteSession{sessionTestSession: &sessionTestSession{id: sess.ID}, initial: sess, output: "initial\n"}
	registry := &backgroundRemoteRuntime{sessionTestRuntime: &sessionTestRuntime{}, handle: local}
	manager := server.NewSessionManager(ctx, config.Sources{}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{}, server.WithSessionRuntime(registry))
	srv := server.NewWithManager(manager, "", server.WithCacheWarmup(false))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, listener) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	client, err := runtime.NewClient("http://" + listener.Addr().String())
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	application := app.New(ctx, transport, sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(&queueTestServices{}))
	ar := animation.NewRuntime()
	page := New(ar, ctx, application, service.NewSessionState(sess), WithCompactWorkLog(false)).(*chatPage)
	page.SetRoutingID(sess.ID)
	page.SetSize(120, 40)
	t.Cleanup(func() { Cleanup(page); ar.Stop() })
	received := make(chan any, 128)
	ready := make(chan struct{})
	go application.Subscribe(ctx, func(msg any) { received <- msg }, app.SubscribeOptions{Ready: ready})
	<-ready
	application.Start(ctx)
	var applyCommand func(tea.Cmd)
	applyCommand = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		msg := cmd()
		if msg == nil {
			return
		}
		value := reflect.ValueOf(msg)
		if value.Kind() == reflect.Slice {
			for i := 0; i < value.Len(); i++ {
				applyCommand(value.Index(i).Interface().(tea.Cmd))
			}
			return
		}
		// Commands from the output component return private snapshot/interest messages.
		_, next := page.messages.Update(msg)
		applyCommand(next)
	}
	until := func(predicate func(any) bool) {
		t.Helper()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case msg := <-received:
				if event, ok := msg.(runtime.Event); ok {
					_, cmd := page.handleRuntimeEvent(event)
					applyCommand(cmd)
				}
				if predicate(msg) {
					return
				}
			case <-timer.C:
				t.Fatal("timed out waiting for remote event")
			}
		}
	}
	until(func(msg any) bool { _, ok := msg.(*app.SessionResetEvent); return ok })
	sequence := uint64(0)
	send := func(event runtime.Event, position int) {
		sequence++
		local.events <- runtime.SessionEvent{Version: api.SessionAPIVersion, SessionID: sess.ID, Epoch: "epoch", Sequence: sequence, TranscriptPosition: position, Event: event}
		until(func(msg any) bool { return reflect.TypeOf(msg) == reflect.TypeOf(event) })
	}
	call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"fixture"}`}}
	definition := tools.Tool{Name: "shell"}
	send(runtime.ToolCall(call, definition, "root"), -1)
	committed := session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}})
	send(runtime.MessageAddedAt(sess.ID, committed, "root", 0), 0)
	require.Zero(t, application.Session().ItemCount(), "remote commits deliberately omit the message body")
	process := backgroundProcess("job_1_1", sess.ID, call.ID, "fixture")
	local.lock.Lock()
	local.processes = []tools.BackgroundProcess{process}
	local.lock.Unlock()
	send(&runtime.BackgroundProcessesEvent{Type: "background_processes", SessionID: sess.ID, Processes: []tools.BackgroundProcess{process}}, -1)
	send(runtime.ToolCallResponse(call.ID, definition, &tools.ToolCallResult{}, tools.ShellBackgroundHandoffPrefix+process.ID+". guidance", "root"), -1)
	rendered := func() string { return ansi.Strip(page.messages.View()) }
	require.NotContains(t, rendered(), "ended")
	require.NotContains(t, rendered(), "in background")
	require.NotContains(t, rendered(), "initial")
	require.Contains(t, page.backgroundProcessToolCalls, call.ID)
	require.True(t, page.messages.NavigateToToolCall(call.ID))
	applyCommand(page.messages.SyncShellBackgroundOutput())
	require.Contains(t, rendered(), "initial")
	local.lock.Lock()
	require.Equal(t, []string{process.ID}, local.interest)
	local.lock.Unlock()
	live := &runtime.BackgroundProcessOutputEvent{Type: "background_process_output", SessionID: sess.ID, ProcessID: process.ID, ToolCallID: call.ID, Offset: 8, Output: "LIVE\n"}
	local.events <- runtime.SessionEvent{Version: api.SessionAPIVersion, SessionID: sess.ID, Epoch: "epoch", TranscriptPosition: -1, Event: live}
	until(func(msg any) bool { _, ok := msg.(*runtime.BackgroundProcessOutputEvent); return ok })
	require.Contains(t, rendered(), "LIVE")
	// Hide the card, append output without interest, then fetch the retained tail on reopen.
	applyCommand(page.messages.SetShellBackgroundOutputVisible(false))
	local.lock.Lock()
	local.output = "initial\nLIVE\nCATCHUP\n"
	require.Empty(t, local.interest)
	local.lock.Unlock()
	require.NotContains(t, rendered(), "CATCHUP")
	applyCommand(page.messages.SetShellBackgroundOutputVisible(true))
	require.Contains(t, rendered(), "CATCHUP")
	notice := session.UserMessage("Shell command job_1_1 with status failed (exit code 7).\n\n--- Untrusted command output ---\nFINAL")
	notice.InputOrigin, notice.InputMode = session.InputOriginRuntime, "steer"
	completion := runtime.UserMessage(notice.Message.Content, sess.ID, nil, 2).(*runtime.UserMessageEvent)
	completion.InputOrigin, completion.InputMode = notice.InputOrigin, notice.InputMode
	send(completion, 2)
	process.State, process.ExitCode = tools.BackgroundProcessFailed, 7
	process.FinishedAt = time.Now()
	send(&runtime.BackgroundProcessesEvent{Type: "background_processes", SessionID: sess.ID, Processes: []tools.BackgroundProcess{process}}, -1)
	require.Contains(t, rendered(), "FINAL")
	require.Contains(t, rendered(), "exit 7")
	require.Equal(t, 1, page.messages.MessageTypeCount(types.MessageTypeToolCall))
}
