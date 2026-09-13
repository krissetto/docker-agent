package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	openaiprovider "github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type fullUIChunkProvider struct {
	stream *fullUIChunkStream
	calls  int
}

func (*fullUIChunkProvider) ID() modelsdev.ID        { return modelsdev.ParseIDOrZero("test/full-ui") }
func (*fullUIChunkProvider) BaseConfig() base.Config { return base.Config{} }
func (*fullUIChunkProvider) MaxTokens() int          { return 0 }

func (p *fullUIChunkProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.stream.canceled = ctx.Done()
	p.stream.cancelErr = ctx.Err
	p.calls++
	if p.calls > 1 {
		p.stream.postTool = true
	}
	return p.stream, nil
}

type fullUIChunkStream struct {
	canceled                     <-chan struct{}
	cancelErr                    func() error
	gates                        [3]chan struct{}
	index                        int
	calls                        atomic.Int32
	reasoning                    bool
	tool, postTool, toolFinished bool
	burst                        int
}

func (s *fullUIChunkStream) Recv() (chat.MessageStreamResponse, error) {
	s.calls.Add(1)
	if s.tool && s.index == 2 && !s.postTool {
		if s.toolFinished {
			return chat.MessageStreamResponse{}, io.EOF
		}
		s.toolFinished = true
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Index: 0, FinishReason: chat.FinishReasonToolCalls}}}, nil
	}
	if s.index == 1 && s.burst > 0 {
		select {
		case <-s.gates[1]:
		case <-s.canceled:
			return chat.MessageStreamResponse{}, s.cancelErr()
		}
		s.burst--
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Index: 0, Delta: chat.MessageDelta{Content: "word "}}}}, nil
	}
	if s.index == 3 {
		s.index++
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Index: 0, FinishReason: chat.FinishReasonStop}}, Usage: &chat.Usage{InputTokens: 1, OutputTokens: 3}}, nil
	}
	if s.index > 3 {
		return chat.MessageStreamResponse{}, io.EOF
	}
	select {
	case <-s.gates[s.index]:
	case <-s.canceled:
		return chat.MessageStreamResponse{}, s.cancelErr()
	}
	marker := []string{"FIRST-CHUNK-MARKER\n\n", "SECOND-CHUNK-MARKER\n\n", "FINAL-CHUNK-MARKER\n\n"}[s.index]
	s.index++
	delta := chat.MessageDelta{Content: marker}
	if s.reasoning && s.index < 3 {
		delta = chat.MessageDelta{ReasoningContent: marker}
	}
	if s.tool && s.index == 2 {
		delta.ToolCalls = []tools.ToolCall{{ID: "first-tool", Type: "function", Function: tools.FunctionCall{Name: "check", Arguments: `{}`}}}
	}
	return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Index: 0, Delta: delta}}}, nil
}

func (*fullUIChunkStream) Close() {}

type fullUIFrameProbe struct {
	canceled <-chan struct{}
	updated  chan struct{}
	visible  [3]atomic.Bool
	root     *appModel
	chunks   atomic.Int32
	stopped  atomic.Bool
	idle     atomic.Bool
	errors   atomic.Int32
	slow     bool
}

var fullUIChunkMarkers = [...]string{"FIRST-CHUNK-MARKER", "SECOND-CHUNK-MARKER", "FINAL-CHUNK-MARKER"}

func (m *fullUIFrameProbe) Init() tea.Cmd { return m.root.Init() }

func (m *fullUIFrameProbe) View() tea.View {
	view := m.root.View()
	for index, marker := range fullUIChunkMarkers {
		if strings.Contains(view.Content, marker) {
			m.visible[index].Store(true)
		}
	}
	// Observe Bubble Tea's real frames without injecting extra Update messages.
	select {
	case m.updated <- struct{}{}:
	default:
	}
	return view
}

func (m *fullUIFrameProbe) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if routed, ok := msg.(messages.RoutedMsg); ok {
		if event, ok := routed.Inner.(messages.SessionRuntimeEventMsg); ok {
			switch event.Event.(type) {
			case *runtime.AgentChoiceEvent, *runtime.AgentChoiceReasoningEvent:
				m.chunks.Add(1)
				if m.slow {
					// Pace chunk handling to exercise backpressure, not to await progress.
					timer := time.NewTimer(time.Millisecond)
					select {
					case <-timer.C:
					case <-m.canceled:
					}
					timer.Stop()
				}
			case *runtime.ErrorEvent:
				m.errors.Add(1)
			case *runtime.StreamStoppedEvent:
				m.stopped.Store(true)
			}
		}
	}
	next, cmd := m.root.Update(msg)
	m.root = next.(*appModel)
	if m.stopped.Load() {
		m.idle.Store(!m.root.chatPage.IsWorking())
	}
	return m, cmd
}

func fullUIStackDeadline(t *testing.T, milestone string, probe *fullUIFrameProbe, stream *fullUIChunkStream) {
	t.Helper()
	stack := make([]byte, 2<<20)
	n := goruntime.Stack(stack, true)
	t.Fatalf("%s: provider Recv=%d UI chunks=%d stopped=%v\n%s", milestone, stream.calls.Load(), probe.chunks.Load(), probe.stopped.Load(), stack[:n])
}

func TestActualFullTUIMultipleProviderChunks(t *testing.T) {
	for _, variant := range []struct {
		name            string
		slow, reasoning bool
		tool            bool
		responses       bool
		history, burst  int
		terminal        string
	}{{name: "normal"}, {name: "slow", slow: true}, {name: "reasoning", reasoning: true}, {name: "reasoning-slow", reasoning: true, slow: true}, {name: "tool", tool: true}, {name: "history-burst", history: 600, burst: 1500}, {name: "history-burst-slow", history: 600, burst: 1500, slow: true}, {name: "history-reasoning-tool", history: 600, reasoning: true, tool: true}, {name: "openai-responses", responses: true}, {name: "openai-reasoning-history", responses: true, reasoning: true, history: 600}, {name: "openai-responses-slow", responses: true, slow: true, burst: 1500}, {name: "openai-failed", responses: true, terminal: "failed"}, {name: "openai-incomplete", responses: true, terminal: "incomplete"}} {
		slow := variant.slow
		t.Run(variant.name, func(t *testing.T) {
			store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
			require.NoError(t, err)
			defer store.(*session.SQLiteSessionStore).Close()
			stream := &fullUIChunkStream{reasoning: variant.reasoning, tool: variant.tool, burst: variant.burst, gates: [3]chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}}
			var model provider.Provider = &fullUIChunkProvider{stream: stream}
			if variant.responses {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					emit := func(event any) {
						data, _ := json.Marshal(event)
						fmt.Fprintf(w, "data: %s\n\n", data)
						w.(http.Flusher).Flush()
					}
					emit(map[string]any{"type": "response.created", "response": map[string]any{"id": "response-1", "status": "in_progress"}})
					for index, marker := range []string{"FIRST-CHUNK-MARKER\n\n", "SECOND-CHUNK-MARKER\n\n", "FINAL-CHUNK-MARKER\n\n"} {
						select {
						case <-stream.gates[index]:
						case <-r.Context().Done():
							return
						}
						stream.calls.Add(1)
						kind := "response.output_text.delta"
						if variant.reasoning && index < 2 {
							kind = "response.reasoning_summary_text.delta"
						}
						if index == 1 {
							for range variant.burst {
								emit(map[string]any{"type": kind, "item_id": "message-1", "output_index": 0, "content_index": 0, "delta": "word "})
							}
						}
						emit(map[string]any{"type": kind, "item_id": "message-1", "output_index": 0, "content_index": 0, "delta": marker})
					}
					switch variant.terminal {
					case "failed":
						emit(map[string]any{"type": "response.failed", "response": map[string]any{"id": "response-1", "status": "failed", "error": map[string]any{"code": "invalid_request_error", "message": "TEST-STREAM-FAILED"}}})
					case "incomplete":
						emit(map[string]any{"type": "response.incomplete", "response": map[string]any{"id": "response-1", "status": "incomplete", "incomplete_details": map[string]any{"reason": "max_output_tokens"}, "output": []any{}}})
					default:
						emit(map[string]any{"type": "response.completed", "response": map[string]any{"id": "response-1", "status": "completed", "output": []any{}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 3, "total_tokens": 4}}})
					}
					fmt.Fprint(w, "data: [DONE]\n\n")
				}))
				defer server.Close()
				client, clientErr := openaiprovider.NewClient(t.Context(), &latest.ModelConfig{Provider: "openai", Model: "gpt-6-astra", BaseURL: server.URL, TokenKey: "TEST_TOKEN", ProviderOpts: map[string]any{"api_type": "openai_responses"}}, environment.NewMapEnvProvider(map[string]string{"TEST_TOKEN": "test"}))
				require.NoError(t, clientErr)
				model = client
			}
			rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "answer", agent.WithModel(model), agent.WithTools(tools.Tool{Name: "check", Parameters: map[string]any{"type": "object"}, Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
				return &tools.ToolCallResult{Output: "checked"}, nil
			}})))), runtime.WithSessionStore(store))
			require.NoError(t, err)
			owner := runtime.NewSessionRuntimeSupervisor(rt)
			defer func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
				defer cancel()
				if err := owner.Shutdown(ctx); err != nil {
					t.Errorf("shutdown runtime: %v", err)
				}
			}()
			sess := session.New(session.WithID("full-ui-chunks"), session.WithAgentName("root"), session.WithTitle("chunks"), session.WithNonInteractive(true))
			for range variant.history {
				sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "**history**\n\n```go\nfmt.Println(123)\n```\n\n"}))
			}
			require.NoError(t, store.AddSession(t.Context(), sess))
			sess, err = store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			a := app.New(t.Context(), owner.Runtime(), sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(rt))
			root := New(t.Context(), nil, a, t.TempDir(), nil).(*appModel)
			probe := &fullUIFrameProbe{canceled: t.Context().Done(), updated: make(chan struct{}, 1), root: root, slow: slow}
			program := tea.NewProgram(probe, tea.WithContext(t.Context()), tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithWindowSize(120, 40))
			root.SetProgram(program)
			done := make(chan error, 1)
			sendDone := make(chan struct{})
			go func() { _, err := program.Run(); done <- err }()
			defer func() {
				quitDone := make(chan struct{})
				go func() { program.Quit(); close(quitDone) }()
				select {
				case <-done:
				case <-time.After(time.Second):
					program.Kill()
					<-done
				}
				<-quitDone
				<-sendDone
				root.supervisor.Shutdown()
				root.ar.Stop()
			}()
			go func() { program.Send(messages.SendMsg{Content: "stream three chunks"}); close(sendDone) }()
			select {
			case <-sendDone:
			case <-time.After(3 * time.Second):
				fullUIStackDeadline(t, "submit", probe, stream)
			}
			for index, marker := range fullUIChunkMarkers {
				close(stream.gates[index])
				if variant.tool && index == 1 {
					continue
				}
				deadline := time.NewTimer(6 * time.Second)
				for !probe.visible[index].Load() {
					select {
					case <-probe.updated:
					case <-deadline.C:
						fullUIStackDeadline(t, marker, probe, stream)
					}
				}
				deadline.Stop()
				if index < 2 {
					require.False(t, probe.stopped.Load(), "chunk must be rendered before stream completion")
				}
			}
			deadline := time.NewTimer(6 * time.Second)
			defer deadline.Stop()
			for !probe.idle.Load() {
				select {
				case <-deadline.C:
					fullUIStackDeadline(t, "completion", probe, stream)
				case <-probe.updated:
				}
			}
			snapshot, err := a.SessionHandle().Snapshot(t.Context())
			require.NoError(t, err)
			if variant.terminal != "failed" {
				require.Contains(t, snapshot.GetLastAssistantMessageContent(), "FINAL-CHUNK-MARKER")
			} else {
				require.Positive(t, probe.errors.Load(), "provider failure must be visible instead of indefinite spinner")
			}
		})
	}
}
