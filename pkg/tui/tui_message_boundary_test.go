package tui

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type boundaryProvider struct {
	calls     int
	reasoning bool
	steering  bool
	partial   bool
}

func (*boundaryProvider) ID() modelsdev.ID        { return modelsdev.ParseIDOrZero("test/boundary") }
func (*boundaryProvider) BaseConfig() base.Config { return base.Config{} }
func (*boundaryProvider) MaxTokens() int          { return 0 }
func (p *boundaryProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.calls++
	if p.calls == 1 && !p.steering {
		return &boundaryStream{deltas: []chat.MessageDelta{{ToolCalls: []tools.ToolCall{{ID: "capture", Type: "function", Function: tools.FunctionCall{Name: "capture", Arguments: `{}`}}}}}, finish: chat.FinishReasonToolCalls}, nil
	}
	parts := []string{"staff mee", "ting."}
	if p.calls > 2 || (p.steering && p.calls > 1) {
		parts = []string{"I’m Sh", "elly."}
	}
	var deltas []chat.MessageDelta
	if p.reasoning {
		for _, part := range parts {
			deltas = append(deltas, chat.MessageDelta{ReasoningContent: part})
		}
	}
	for _, part := range parts {
		deltas = append(deltas, chat.MessageDelta{Content: part})
	}
	if p.steering && p.calls == 1 {
		if p.partial {
			deltas = append(deltas, chat.MessageDelta{ToolCalls: []tools.ToolCall{{ID: "orphan", Type: "function", Function: tools.FunctionCall{Name: "orphan_partial", Arguments: `{`}}}})
		}
		return &boundaryStream{deltas: deltas, blocked: ctx.Done(), cancelErr: ctx.Err}, nil
	}
	return &boundaryStream{deltas: deltas, finish: chat.FinishReasonStop}, nil
}

type boundaryStream struct {
	deltas    []chat.MessageDelta
	finish    chat.FinishReason
	index     int
	blocked   <-chan struct{}
	cancelErr func() error
}

func (s *boundaryStream) Recv() (chat.MessageStreamResponse, error) {
	if s.blocked != nil && s.index == len(s.deltas) {
		<-s.blocked
		return chat.MessageStreamResponse{}, s.cancelErr()
	}
	if s.index > len(s.deltas) {
		return chat.MessageStreamResponse{}, io.EOF
	}
	choice := chat.MessageStreamChoice{Index: 0}
	if s.index == len(s.deltas) {
		choice.FinishReason = s.finish
	} else {
		choice.Delta = s.deltas[s.index]
	}
	s.index++
	return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{choice}}, nil
}
func (*boundaryStream) Close() {}

type boundaryFrameProbe struct {
	root        *appModel
	mu          sync.Mutex
	frame       string
	stops       int
	partialSeen bool
	updated     chan struct{}
}

func (p *boundaryFrameProbe) Init() tea.Cmd { return p.root.Init() }
func (p *boundaryFrameProbe) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := p.root.Update(msg)
	p.root = next.(*appModel)
	if routed, ok := msg.(messages.RoutedMsg); ok {
		if event, ok := routed.Inner.(messages.SessionRuntimeEventMsg); ok {
			if _, ok := event.Event.(*runtime.PartialToolCallEvent); ok {
				p.mu.Lock()
				p.partialSeen = true
				p.mu.Unlock()
			}
			if _, ok := event.Event.(*runtime.StreamStoppedEvent); ok {
				p.mu.Lock()
				p.stops++
				p.mu.Unlock()
			}
		}
	}
	return p, cmd
}

func (p *boundaryFrameProbe) View() tea.View {
	view := p.root.View()
	p.mu.Lock()
	p.frame = ansi.Strip(view.Content)
	p.mu.Unlock()
	select {
	case p.updated <- struct{}{}:
	default:
	}
	return view
}

func (p *boundaryFrameProbe) await(t *testing.T, stops int, marker string) string {
	t.Helper()
	deadline := time.NewTimer(6 * time.Second)
	defer deadline.Stop()
	for {
		p.mu.Lock()
		frame, count := p.frame, p.stops
		p.mu.Unlock()
		if count >= stops && strings.Contains(frame, marker) {
			return frame
		}
		select {
		case <-p.updated:
		case <-deadline.C:
			t.Fatalf("waiting for stop %d and %q; stops=%d\n%s", stops, marker, count, frame)
		}
	}
}

func TestActualFullTUICommittedAssistantBoundaries(t *testing.T) {
	for _, variant := range []struct {
		name                                 string
		reasoning, steering, partial, replay bool
		mode                                 string
	}{
		{name: "content"},
		{name: "reasoning-content", reasoning: true},
		{name: "steer-content", steering: true},
		{name: "steer-partial", steering: true, partial: true, mode: "noncompact"},
		{name: "steer-partial-compact", steering: true, partial: true, mode: "compact"},
		{name: "steer-reasoning-partial", steering: true, partial: true, reasoning: true, mode: "noncompact"},
		{name: "steer-reasoning-partial-compact", steering: true, partial: true, reasoning: true, mode: "compact"},
		{name: "replay-content", replay: true},
	} {
		t.Run(variant.name, func(t *testing.T) {
			store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
			require.NoError(t, err)
			defer store.(*session.SQLiteSessionStore).Close()
			model := &boundaryProvider{reasoning: variant.reasoning, steering: variant.steering, partial: variant.partial}
			rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "answer", agent.WithModel(model), agent.WithTools(tools.Tool{Name: "capture", Parameters: map[string]any{"type": "object"}, Handler: func(_ context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
				return &tools.ToolCallResult{Output: "captured"}, nil
			}}, tools.Tool{Name: "orphan_partial", Parameters: map[string]any{"type": "object"}})))), runtime.WithSessionStore(store))
			require.NoError(t, err)
			owner := runtime.NewSessionRuntimeSupervisor(rt)
			defer func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
				defer cancel()
				require.NoError(t, owner.Shutdown(ctx))
			}()
			sess := session.New(session.WithID("boundary"), session.WithAgentName("root"), session.WithTitle("boundary"), session.WithNonInteractive(true), session.WithToolsApproved(true))
			handle, err := owner.Runtime().CreateSession(t.Context(), sess, runtime.SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			sess, err = store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			require.False(t, handle.Metadata().Historical, "fixture must be a live session")
			require.True(t, handle.Metadata().ExecutionAvailable, "fixture must execute turns")
			sessions := owner.Runtime()
			if variant.replay {
				sessions = &boundaryReplayRuntime{SessionRuntime: sessions}
			}
			a := app.New(t.Context(), sessions, sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(rt))
			root := New(t.Context(), nil, a, t.TempDir(), nil).(*appModel)
			if variant.mode != "" {
				setter, ok := root.chatPage.(interface{ SetCompactWorkLog(enabled bool) tea.Cmd })
				require.True(t, ok, "chat page exposes the compact Work mode")
				setter.SetCompactWorkLog(variant.mode == "compact")
				// Replacement pages (createSessionComponents) read the root field, not the chat page.
				root.compactWorkLog = variant.mode == "compact"
			}
			if variant.replay {
				_, wrapped := a.SessionHandle().(*boundaryReplayHandle)
				require.True(t, wrapped, "app must bind the replay wrapper handle")
			}
			probe := &boundaryFrameProbe{root: root, updated: make(chan struct{}, 1)}
			program := tea.NewProgram(probe, tea.WithContext(t.Context()), tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithWindowSize(120, 40))
			root.SetProgram(program)
			done := make(chan error, 1)
			go func() { _, err := program.Run(); done <- err }()
			defer func() {
				program.Quit()
				select {
				case <-done:
				case <-time.After(time.Second):
					program.Kill()
					<-done
				}
				root.supervisor.Shutdown()
				root.ar.Stop()
			}()
			program.Send(messages.SendMsg{Content: "capture a background callback"})
			if variant.replay {
				require.Eventually(t, func() bool { return sessions.(*boundaryReplayRuntime).observed.Load() > 0 }, 6*time.Second, 10*time.Millisecond, "app must observe through the replay wrapper")
			}
			stops := 2
			if variant.steering {
				probe.await(t, 0, "staff meeting.")
				if variant.partial && variant.mode == "compact" {
					awaitCompactPartialHidden(t, probe)
				} else if variant.partial {
					probe.await(t, 0, "orphan_partial")
				}
				_, err = a.SessionHandle().Steer(t.Context(), runtime.TurnInput{Content: "continue separately"})
				stops = 1
			} else {
				probe.await(t, 1, "staff meeting.")
				_, err = a.SessionHandle().Retry(t.Context())
			}
			require.NoError(t, err)
			frame := probe.await(t, stops, "elly.")
			if variant.replay {
				replay := sessions.(*boundaryReplayRuntime)
				require.Equal(t, int32(1), replay.disconnects.Load(), "exactly one root AgentChoice must disconnect the primary observation")
				require.Positive(t, replay.resumes.Load(), "app must resume with Since and the primary epoch")
			}
			assert.NotContains(t, frame, "orphan_partial", "aborted partial tool must retire")
			snapshot, err := a.SessionHandle().Snapshot(t.Context())
			require.NoError(t, err)
			var texts []string
			for _, item := range snapshot.ItemsSnapshot() {
				if item.Message != nil && item.Message.Message.Role == chat.MessageRoleAssistant && item.Message.Message.Content != "" {
					texts = append(texts, item.Message.Message.Content)
				}
			}
			require.Equal(t, []string{"staff meeting.", "I’m Shelly."}, texts, "provider bytes and canonical messages must remain separate")
			assert.NotContains(t, frame, "meeting.I’m", "actual App.New 50ms/full-TUI path must respect committed boundaries")
			assert.Contains(t, frame, "staff meeting.")
			assert.Contains(t, frame, "I’m Shelly.")
		})
	}
}

// Disconnect one genuine runtime observation after a chunk; the ordinary App
// adapter must reconnect from its cursor and consume the owner's retained replay.
type boundaryReplayRuntime struct {
	runtime.SessionRuntime

	observed    atomic.Int32
	disconnects atomic.Int32
	resumes     atomic.Int32

	mu           sync.Mutex
	primaryEpoch string
}

func (r *boundaryReplayRuntime) wrap(h runtime.SessionHandle) runtime.SessionHandle {
	if h == nil {
		return nil
	}
	return &boundaryReplayHandle{SessionHandle: h, owner: r}
}

func (r *boundaryReplayRuntime) SessionByID(id string) (runtime.SessionHandle, error) {
	h, err := r.SessionRuntime.SessionByID(id)
	if err != nil {
		return nil, err
	}
	return r.wrap(h), nil
}

func (r *boundaryReplayRuntime) CreateSession(ctx context.Context, s *session.Session, b runtime.SessionBinding) (runtime.SessionHandle, error) {
	h, err := r.SessionRuntime.CreateSession(ctx, s, b)
	if err != nil {
		return nil, err
	}
	return r.wrap(h), nil
}

type boundaryReplayHandle struct {
	runtime.SessionHandle
	owner *boundaryReplayRuntime

	mu           sync.Mutex
	disconnected bool
}

func (h *boundaryReplayHandle) Observe(ctx context.Context, opts runtime.ObserveOptions) (runtime.Observation, error) {
	h.owner.observed.Add(1)
	if opts.Tree {
		// Tree topology is outside the replay contract: pass it through so the
		// app's tree bridge sees the same capability as the ordinary variants.
		return h.SessionHandle.Observe(ctx, opts)
	}
	observation, err := h.SessionHandle.Observe(ctx, opts)
	if err != nil {
		return observation, err
	}
	if opts.Since != nil && opts.SinceEpoch != "" {
		h.owner.mu.Lock()
		primary := h.owner.primaryEpoch
		h.owner.mu.Unlock()
		if opts.SinceEpoch == primary {
			h.owner.resumes.Add(1)
		}
	}
	h.mu.Lock()
	disconnect := !h.disconnected
	h.disconnected = true
	h.mu.Unlock()
	if disconnect {
		h.owner.mu.Lock()
		h.owner.primaryEpoch = observation.Primary().Epoch
		h.owner.mu.Unlock()
	}
	if !disconnect {
		return observation, nil
	}
	out := make(chan runtime.SessionEvent)
	original := observation.Events
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-original:
				if !ok {
					return
				}
				select {
				case out <- event:
				case <-ctx.Done():
					return
				}
				if _, ok := event.Event.(*runtime.AgentChoiceEvent); ok && event.SessionID == h.ID() {
					h.owner.disconnects.Add(1)
					observation.Cancel()
					return
				}
			}
		}
	}()
	observation.Events = out
	return observation, nil
}

// awaitCompactPartialHidden is the compact-Work counterpart of the partial visibility wait:
// the pending tool call is processed by the UI and stays inside the active collapsed Work
// header until steering retires it. Noncompact variants keep the original visible wait.
func awaitCompactPartialHidden(t *testing.T, probe *boundaryFrameProbe) {
	t.Helper()
	deadline := time.NewTimer(6 * time.Second)
	defer deadline.Stop()
	for {
		probe.mu.Lock()
		frame, seen := probe.frame, probe.partialSeen
		probe.mu.Unlock()
		if seen && strings.Contains(frame, "Working [+]") {
			require.NotContains(t, frame, "orphan_partial", "pending partial tool must stay inside the collapsed Work group")
			return
		}
		select {
		case <-probe.updated:
		case <-deadline.C:
			t.Fatalf("compact partial never reached the collapsed Work header; frame:\n%s", frame)
		}
	}
}

// PrepareSessionView keeps the optional preparer capability that the ordinary
// App open path requires, and wraps the committed handle so Observe reaches the
// replay disconnect. Omitting it makes persisted sessions fail as unsupported.
func (r *boundaryReplayRuntime) PrepareSessionView(ctx context.Context, id string) (runtime.PreparedSessionView, error) {
	preparer, ok := r.SessionRuntime.(runtime.SessionViewPreparer)
	if !ok {
		return nil, runtime.UnsupportedSessionOperation(id, "prepare_view")
	}
	prepared, err := preparer.PrepareSessionView(ctx, id)
	if err != nil {
		return nil, err
	}
	return boundaryReplayPrepared{PreparedSessionView: prepared, owner: r}, nil
}

type boundaryReplayPrepared struct {
	runtime.PreparedSessionView

	owner *boundaryReplayRuntime
}

func (p boundaryReplayPrepared) Commit(ctx context.Context) (runtime.CommittedSessionView, error) {
	committed, err := p.PreparedSessionView.Commit(ctx)
	if err != nil {
		return committed, err
	}
	committed.SessionHandle = p.owner.wrap(committed.SessionHandle)
	return committed, nil
}
