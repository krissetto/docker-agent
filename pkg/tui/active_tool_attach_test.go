package tui

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/messages"
	chatpage "github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type attachToolModelStore struct{}

func (attachToolModelStore) GetModel(context.Context, modelsdev.ID) (*modelsdev.Model, error) {
	return nil, nil
}
func (attachToolModelStore) GetDatabase(context.Context) (*modelsdev.Database, error) {
	return &modelsdev.Database{}, nil
}

type attachToolPacket struct {
	response chatmsg.MessageStreamResponse
	err      error
}
type attachToolProvider struct {
	packets chan attachToolPacket
	calls   atomic.Int32
}

func (*attachToolProvider) ID() modelsdev.ID {
	return modelsdev.ParseIDOrZero("test/attach-tools")
}
func (*attachToolProvider) BaseConfig() base.Config { return base.Config{} }
func (*attachToolProvider) MaxTokens() int          { return 0 }
func (p *attachToolProvider) CreateChatCompletionStream(ctx context.Context, _ []chatmsg.Message, _ []tools.Tool) (chatmsg.MessageStream, error) {
	p.calls.Add(1)
	return &attachToolStream{ctx: ctx, p: p}, nil
}

type attachToolStream struct {
	ctx context.Context
	p   *attachToolProvider
}

func (s *attachToolStream) Recv() (chatmsg.MessageStreamResponse, error) {
	select {
	case p := <-s.p.packets:
		return p.response, p.err
	case <-s.ctx.Done():
		return chatmsg.MessageStreamResponse{}, s.ctx.Err()
	}
}
func (*attachToolStream) Close() {}
func (p *attachToolProvider) delta(d chatmsg.MessageDelta) {
	p.packets <- attachToolPacket{response: chatmsg.MessageStreamResponse{Choices: []chatmsg.MessageStreamChoice{{Delta: d}}}}
}
func (p *attachToolProvider) finish(reason chatmsg.FinishReason) {
	p.packets <- attachToolPacket{response: chatmsg.MessageStreamResponse{Choices: []chatmsg.MessageStreamChoice{{FinishReason: reason}}}}
	p.packets <- attachToolPacket{err: io.EOF}
}

type attachToolState struct {
	view                                              string
	working, loading                                  bool
	spinners, tools                                   int
	args, output                                      string
	status                                            int
	resets, starts, partials, outputs, results, stops int
}
type attachToolQuery struct{ reply chan attachToolState }
type attachToolModel struct {
	root  *appModel
	state attachToolState
}

func (m *attachToolModel) Init() tea.Cmd  { return m.root.Init() }
func (m *attachToolModel) View() tea.View { return m.root.View() }
func (m *attachToolModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if q, ok := msg.(attachToolQuery); ok {
		s := m.state
		s.view = ansi.Strip(m.root.View().Content)
		s.working = m.root.chatPage.IsWorking()
		s.loading = chatpage.Loading(m.root.chatPage)
		// Query private render state only on the Bubble Tea owner loop.
		list := reflect.ValueOf(m.root.chatPage).Elem().FieldByName("messages").Elem().Elem().FieldByName("messages")
		for i := 0; i < list.Len(); i++ {
			item := list.Index(i).Elem()
			switch types.MessageType(item.FieldByName("Type").Int()) {
			case types.MessageTypeSpinner:
				s.spinners++
			case types.MessageTypeToolCall:
				s.tools++
				s.args = item.FieldByName("ToolCall").FieldByName("Function").FieldByName("Arguments").String()
				s.output = item.FieldByName("Content").String()
				s.status = int(item.FieldByName("ToolStatus").Int())
			}
		}
		q.reply <- s
		return m, nil
	}
	if routed, ok := msg.(messages.RoutedMsg); ok {
		if wrapped, ok := routed.Inner.(messages.SessionRuntimeEventMsg); ok {
			switch wrapped.Event.(type) {
			case *app.SessionResetEvent:
				m.state.resets++
			case *runtime.StreamStartedEvent:
				m.state.starts++
			case *runtime.PartialToolCallEvent:
				m.state.partials++
			case *runtime.ToolCallOutputEvent:
				m.state.outputs++
			case *runtime.ToolCallResponseEvent:
				m.state.results++
			case *runtime.StreamStoppedEvent:
				m.state.stops++
			}
		}
	}
	_, cmd := m.root.Update(msg)
	return m, cmd
}
func attachToolQueryState(t *testing.T, p *tea.Program) attachToolState {
	t.Helper()
	ch := make(chan attachToolState, 1)
	p.Send(attachToolQuery{ch})
	select {
	case s := <-ch:
		return s
	case <-time.After(3 * time.Second):
		t.Fatal("owner loop query timeout")
		return attachToolState{}
	}
}
func attachToolAwait(t *testing.T, p *tea.Program, f func(attachToolState) bool) attachToolState {
	t.Helper()
	var s attachToolState
	require.Eventually(t, func() bool { s = attachToolQueryState(t, p); return f(s) }, 5*time.Second, time.Millisecond)
	return s
}
func attachToolWaitEvent(t *testing.T, obs runtime.Observation, accept func(runtime.Event) bool) runtime.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e, ok := <-obs.Events:
			require.True(t, ok, "canonical observer closed")
			if accept(e.Event) {
				return e.Event
			}
		case <-deadline:
			t.Fatal("canonical event timeout")
			return nil
		}
	}
}

func TestFullTUIActiveToolAttach(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	paths.SetRoot(home)
	t.Cleanup(func() { paths.SetRoot("") })
	for _, phase := range []string{"partial-arguments", "tool-running"} {
		t.Run(phase, func(t *testing.T) {
			store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "synthetic.db"))
			require.NoError(t, err)
			defer store.(*session.SQLiteSessionStore).Close()
			p := &attachToolProvider{packets: make(chan attachToolPacket, 16)}
			parentProvider := &attachToolProvider{packets: make(chan attachToolPacket, 16)}
			outputGate, resultGate := make(chan struct{}), make(chan struct{})
			handler := func(ctx context.Context, _ tools.ToolCall, r tools.Runtime) (*tools.ToolCallResult, error) {
				r.EmitOutput(ctx, "PREATTACH-OUTPUT\n")
				select {
				case <-outputGate:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				r.EmitOutput(ctx, "POSTATTACH-OUTPUT\n")
				select {
				case <-resultGate:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return &tools.ToolCallResult{Output: "FINAL-RESULT"}, nil
			}
			tm := team.New(team.WithAgents(
				agent.New("root", "synthetic", agent.WithModel(parentProvider),
					agent.WithToolSets(subagent.NewToolSet()), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
				agent.New("worker", "synthetic", agent.WithModel(p), agent.WithTools(tools.Tool{
					Name: "attach_check", Handler: handler,
					Parameters: map[string]any{"type": "object", "properties": map[string]any{"marker": map[string]any{"type": "string"}}},
				})),
			))
			rt, err := runtime.NewLocalRuntime(t.Context(), tm, runtime.WithSessionStore(store),
				runtime.WithSessionCompaction(false), runtime.WithModelStore(attachToolModelStore{}))
			require.NoError(t, err)
			owner := runtime.NewSessionRuntimeSupervisor(rt)
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				require.NoError(t, owner.Shutdown(ctx))
			}()
			sess := session.New(session.WithID("attachTool-"+phase), session.WithAgentName("root"), session.WithTitle("Synthetic attach"), session.WithNonInteractive(true), session.WithToolsApproved(true))
			require.NoError(t, store.AddSession(t.Context(), sess))
			h, err := owner.Runtime().CreateSession(t.Context(), sess, runtime.SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			parentObs, err := h.Observe(t.Context(), runtime.ObserveOptions{})
			require.NoError(t, err)
			defer parentObs.Cancel()
			_, err = h.Submit(t.Context(), runtime.TurnInput{Content: "spawn synthetic child"})
			require.NoError(t, err)
			parentProvider.delta(chatmsg.MessageDelta{ToolCalls: []tools.ToolCall{{ID: "spawn", Type: "function", Function: tools.FunctionCall{Name: subagent.ToolSpawnSubagent, Arguments: `{"agent":"worker","task":"run synthetic check"}`}}}})
			parentProvider.finish(chatmsg.FinishReasonToolCalls)
			created := attachToolWaitEvent(t, parentObs, func(e runtime.Event) bool { _, ok := e.(*runtime.SubagentCreatedEvent); return ok }).(*runtime.SubagentCreatedEvent)
			h, err = owner.Runtime().SessionByID(created.ChildSessionID)
			require.NoError(t, err)
			obs, err := h.Observe(t.Context(), runtime.ObserveOptions{})
			require.NoError(t, err)
			defer obs.Cancel()

			p.delta(chatmsg.MessageDelta{ToolCalls: []tools.ToolCall{{ID: "diag-call", Type: "function", Function: tools.FunctionCall{Name: "attach_check", Arguments: `{"marker":"PREFIX`}}}})
			attachToolWaitEvent(t, obs, func(e runtime.Event) bool { _, ok := e.(*runtime.PartialToolCallEvent); return ok })
			if phase == "tool-running" {
				p.delta(chatmsg.MessageDelta{ToolCalls: []tools.ToolCall{{ID: "diag-call", Function: tools.FunctionCall{Arguments: `-SUFFIX"}`}}}})
				p.finish(chatmsg.FinishReasonToolCalls)
				attachToolWaitEvent(t, obs, func(e runtime.Event) bool { _, ok := e.(*runtime.ToolCallOutputEvent); return ok })
			}
			attached, err := h.Observe(t.Context(), runtime.ObserveOptions{})
			require.NoError(t, err)
			require.IsType(t, &runtime.StreamStartedEvent{}, attached.Replay[0].Event)
			if phase == "partial-arguments" {
				require.Len(t, attached.Replay, 2)
				require.Equal(t, `{"marker":"PREFIX`, attached.Replay[1].Event.(*runtime.PartialToolCallEvent).ToolCall.Function.Arguments)
			} else {
				require.Len(t, attached.Replay, 3)
				require.IsType(t, &runtime.ToolCallEvent{}, attached.Replay[1].Event)
				require.Equal(t, "PREATTACH-OUTPUT\n", attached.Replay[2].Event.(*runtime.ToolCallOutputEvent).Output)
			}

			attached.Cancel()
			open := func() (*tea.Program, *appModel, func()) {
				prepared, err := owner.Runtime().(runtime.SessionViewPreparer).PrepareSessionView(t.Context(), created.ChildSessionID)
				require.NoError(t, err)
				defer prepared.Abort()
				committed, err := prepared.Commit(t.Context())
				require.NoError(t, err)
				require.NotNil(t, committed.Info.Attach)
				a, err := app.NewResolved(t.Context(), owner.Runtime(), committed, app.WithRuntimeServices(rt), app.WithSubagentAttach(*committed.Info.Attach))
				require.NoError(t, err)
				require.NotNil(t, a.AttachedSubagent())

				root := New(t.Context(), nil, a, home, nil, WithHideSidebar()).(*appModel)
				root.sessionState.SetHideToolResults(false)
				chatpage.EnableAsyncReplay(root.chatPage)
				program := tea.NewProgram(&attachToolModel{root: root}, tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithWindowSize(120, 40))
				root.SetProgram(program)
				done := make(chan error, 1)
				go func() { _, err := program.Run(); done <- err }()
				closeView := sync.OnceFunc(func() { program.Quit(); require.NoError(t, <-done); root.supervisor.Shutdown(); root.ar.Stop() })
				t.Cleanup(closeView)
				return program, root, closeView
			}
			program, _, closeView := open()
			s := attachToolAwait(t, program, func(s attachToolState) bool {
				return s.resets > 0 && s.starts > 0 && !s.loading && s.tools == 1 && (s.partials > 0 || s.outputs > 0)
			})
			require.Zero(t, s.spinners, "an active attachment is not awaiting its first response")
			require.True(t, s.working)
			if phase == "partial-arguments" {
				require.Equal(t, int(types.ToolStatusPending), s.status)
				require.Equal(t, `{"marker":"PREFIX`, s.args)
				p.delta(chatmsg.MessageDelta{ToolCalls: []tools.ToolCall{{ID: "diag-call", Function: tools.FunctionCall{Arguments: `-SUFFIX"}`}}}})
				s = attachToolAwait(t, program, func(s attachToolState) bool { return s.partials == 2 })
				require.Equal(t, `{"marker":"PREFIX-SUFFIX"}`, s.args)
				p.finish(chatmsg.FinishReasonToolCalls)
				attachToolWaitEvent(t, obs, func(e runtime.Event) bool { _, ok := e.(*runtime.ToolCallOutputEvent); return ok })
				s = attachToolAwait(t, program, func(s attachToolState) bool { return s.outputs > 0 })
			}
			require.Equal(t, int(types.ToolStatusRunning), s.status)
			require.Equal(t, `{"marker":"PREFIX-SUFFIX"}`, s.args)
			require.Equal(t, "PREATTACH-OUTPUT\n", s.output)
			require.Zero(t, s.spinners)

			beforeOutputs := s.outputs
			close(outputGate)
			s = attachToolAwait(t, program, func(s attachToolState) bool { return s.outputs > beforeOutputs })
			require.Contains(t, s.output, "POSTATTACH-OUTPUT")
			require.Equal(t, "PREATTACH-OUTPUT\nPOSTATTACH-OUTPUT\n", s.output)
			require.Equal(t, int(types.ToolStatusRunning), s.status)
			require.Zero(t, s.spinners)

			close(resultGate)
			s = attachToolAwait(t, program, func(s attachToolState) bool { return s.results > 0 })
			require.Contains(t, s.output, "FINAL-RESULT")
			require.Zero(t, s.spinners)
			require.Equal(t, int(types.ToolStatusCompleted), s.status)

			p.delta(chatmsg.MessageDelta{Content: "FINAL-ASSISTANT"})
			p.finish(chatmsg.FinishReasonStop)
			s = attachToolAwait(t, program, func(s attachToolState) bool { return s.stops > 0 && !s.working })
			require.Zero(t, s.spinners)
			snapshot, err := h.Snapshot(t.Context())
			require.NoError(t, err)
			durable, err := store.GetSession(t.Context(), created.ChildSessionID)
			require.NoError(t, err)
			for _, source := range []*session.Session{snapshot, durable} {
				var dump strings.Builder
				for _, item := range source.ItemsSnapshot() {
					if item.Message != nil {
						fmt.Fprintf(&dump, "%s %s %+v\n", item.Message.Message.Role, item.Message.Message.Content, item.Message.Message.ToolCalls)
					}
				}
				require.Contains(t, dump.String(), "PREFIX-SUFFIX")
				require.Contains(t, dump.String(), "FINAL-RESULT")
				require.Contains(t, dump.String(), "FINAL-ASSISTANT")
			}
			t.Log("canonical Snapshot and isolated SQLite both contain full arguments, final result, final assistant")
			closeView()
			program, _, closeView = open()
			defer closeView()
			s = attachToolAwait(t, program, func(s attachToolState) bool {
				return s.resets > 0 && !s.loading && strings.Contains(s.view, "FINAL-ASSISTANT")
			})
			require.False(t, s.working)
			require.Zero(t, s.spinners)
			require.Equal(t, 1, s.tools)
			require.Contains(t, s.args, "PREFIX-SUFFIX")
			require.Contains(t, s.output, "FINAL-RESULT")
			t.Log("completed reattach control: full call/result visible, no pending spinner, no runtime restart")
			require.EqualValues(t, 2, p.calls.Load())
			_, err = h.Submit(t.Context(), runtime.TurnInput{Content: "new manual request"})
			require.NoError(t, err)
			s = attachToolAwait(t, program, func(s attachToolState) bool { return s.starts > 0 && s.spinners == 1 })
			require.True(t, s.working, "a genuine start must still show first-response pending")
			p.delta(chatmsg.MessageDelta{Content: "manual response"})
			p.finish(chatmsg.FinishReasonStop)
			attachToolAwait(t, program, func(s attachToolState) bool { return s.stops > 0 && !s.working && s.spinners == 0 })
		})
	}
}
