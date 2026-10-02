package tui

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// The provider cannot complete its batch while the test observes real frames.
// Short-call variants stream continuously (paced like the observed provider or
// ASAP); the fragmented stress variant gates prefixes separately. The 27ms
// provider cadence models arrival rate, not a sleep used to await UI progress.
type spawnBatchProvider struct {
	stream *spawnBatchStream
}

func (*spawnBatchProvider) ID() modelsdev.ID        { return modelsdev.ParseIDOrZero("test/spawn-batch") }
func (*spawnBatchProvider) BaseConfig() base.Config { return base.Config{} }
func (*spawnBatchProvider) MaxTokens() int          { return 0 }
func (p *spawnBatchProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.stream.ctx = ctx
	return p.stream, nil
}

type spawnBatchStream struct {
	ctx                               context.Context
	gates                             [4]chan struct{}
	arrivals                          [3]atomic.Int64
	count, index, fragment, fragments int
	pace                              time.Duration
	completed                         atomic.Bool
}

func (s *spawnBatchStream) Recv() (chat.MessageStreamResponse, error) {
	stage := 0
	if s.index >= s.count {
		stage = 3
	} else if s.index >= s.count/2 {
		stage = 2
	} else if s.index > 0 {
		stage = 1
	}
	select {
	case <-s.gates[stage]:
	case <-s.ctx.Done():
		return chat.MessageStreamResponse{}, s.ctx.Err()
	}
	if stage == 3 {
		s.completed.Store(true)
		return chat.MessageStreamResponse{}, io.EOF
	}
	if s.pace > 0 && s.fragment == 0 {
		timer := time.NewTimer(s.pace)
		select {
		case <-timer.C:
		case <-s.ctx.Done():
			timer.Stop()
			return chat.MessageStreamResponse{}, s.ctx.Err()
		}
	}
	name := fmt.Sprintf("worker-%03d", s.index)
	args := strings.Repeat("x", 100)
	if s.fragment == 0 {
		args = fmt.Sprintf(`{"agent":%q,"task":"`, name) + args
	}
	if s.fragment == s.fragments-1 {
		args += `"}`
	}
	call := tools.ToolCall{ID: fmt.Sprintf("spawn-%d", s.index), Type: "function", Function: tools.FunctionCall{Arguments: args}}
	if s.fragment == 0 {
		call.Function.Name = "spawn_subagent"
	}
	if s.index == 0 || s.index == s.count/2-1 || s.index == s.count-1 {
		milestone := 0
		if s.index == s.count/2-1 {
			milestone = 1
		} else if s.index == s.count-1 {
			milestone = 2
		}
		s.arrivals[milestone].CompareAndSwap(0, time.Now().UnixNano())
	}
	s.fragment++
	if s.fragment == s.fragments {
		s.fragment = 0
		s.index++
	}
	return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{call}}}}}, nil
}
func (*spawnBatchStream) Close() {}

type spawnBatchHeartbeat struct{ done chan time.Time }
type spawnBatchFrameProbe struct {
	root       *appModel
	markers    [3]string
	frames     [3]atomic.Int64
	updated    chan struct{}
	count      int
	continuous bool
}

func (m *spawnBatchFrameProbe) Init() tea.Cmd { return m.root.Init() }
func (m *spawnBatchFrameProbe) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ping, ok := msg.(spawnBatchHeartbeat); ok {
		ping.done <- time.Now()
		return m, nil
	}
	next, cmd := m.root.Update(msg)
	m.root = next.(*appModel)
	return m, cmd
}
func (m *spawnBatchFrameProbe) View() tea.View {
	view := m.root.View()
	plain := ansi.Strip(view.Content)
	for i, marker := range m.markers {
		for _, line := range strings.Split(plain, "\n") {
			if !strings.Contains(line, "Spawning") {
				continue
			}
			visible := strings.Contains(line, marker)
			if m.continuous {
				_, suffix, found := strings.Cut(line, "worker-")
				if found && len(suffix) >= 3 {
					index, err := strconv.Atoi(suffix[:3])
					threshold := []int{0, m.count/2 - 1, m.count - 1}[i]
					visible = err == nil && index >= threshold
				}
			}
			if visible {
				m.frames[i].CompareAndSwap(0, time.Now().UnixNano())
			}
		}
	}
	select {
	case m.updated <- struct{}{}:
	default:
	}
	return view
}

func TestActualFullTUIProgressiveSpawnBatch(t *testing.T) {
	for _, variant := range []struct {
		name             string
		count, fragments int
		pace             time.Duration
		continuous       bool
	}{
		{name: "short-paced", count: 30, fragments: 1, pace: 27 * time.Millisecond, continuous: true},
		{name: "short-burst", count: 300, fragments: 1, continuous: true},
		{name: "fragmented-gated", count: 30, fragments: 64},
	} {
		t.Run(variant.name, func(t *testing.T) {
			count, fragments := variant.count, variant.fragments
			stream := &spawnBatchStream{count: count, fragments: fragments, pace: variant.pace}
			for i := range stream.gates {
				stream.gates[i] = make(chan struct{})
			}
			store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
			require.NoError(t, err)
			defer store.(*session.SQLiteSessionStore).Close()
			model := &spawnBatchProvider{stream: stream}
			rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "answer", agent.WithModel(model), agent.WithTools(tools.Tool{Name: "spawn_subagent", Parameters: map[string]any{"type": "object"}, Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
				return &tools.ToolCallResult{Output: "spawned"}, nil
			}})))), runtime.WithSessionStore(store))
			require.NoError(t, err)
			owner := runtime.NewSessionRuntimeSupervisor(rt)
			defer func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
				defer cancel()
				require.NoError(t, owner.Shutdown(ctx))
			}()
			sess := session.New(session.WithID("spawn-batch"), session.WithAgentName("root"), session.WithNonInteractive(true))
			for range 300 {
				sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "**history**\n\n```go\nfmt.Println(123)\n```\n\n"}))
			}
			tree := subagent.Snapshot{Root: subagent.SessionRootID(sess.ID), Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: subagent.SessionRootID(sess.ID), Agent: "root", State: subagent.NodeIdle}}}}
			for i := range count {
				tree.Nodes[0].Children = append(tree.Nodes[0].Children, subagent.NodeSnapshot{Node: subagent.Node{ID: subagent.NodeID(fmt.Sprintf("prior-%d", i)), Parent: tree.Root, Agent: "worker", State: subagent.NodeCompleted}})
			}
			sess.SetSubagentTree(&tree)
			require.NoError(t, store.AddSession(t.Context(), sess))
			a := app.New(t.Context(), owner.Runtime(), sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(rt))
			root := New(t.Context(), nil, a, t.TempDir(), nil).(*appModel)
			probe := &spawnBatchFrameProbe{root: root, count: count, continuous: variant.continuous, markers: [3]string{"worker-000", fmt.Sprintf("worker-%03d", count/2-1), fmt.Sprintf("worker-%03d", count-1)}, updated: make(chan struct{}, 1)}
			program := tea.NewProgram(probe, tea.WithContext(t.Context()), tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithWindowSize(120, 40))
			root.SetProgram(program)
			done := make(chan error, 1)
			go func() { _, err := program.Run(); done <- err }()
			defer func() {
				program.Kill()
				<-done
				root.supervisor.Shutdown()
				root.ar.Stop()
			}()
			if variant.continuous {
				for i := range 3 {
					close(stream.gates[i])
				}
			}
			program.Send(messages.SendMsg{Content: "spawn a parallel batch"})
			for stage := range probe.markers {
				if !variant.continuous {
					close(stream.gates[stage])
				}
				deadline := time.NewTimer(45 * time.Second)
				for probe.frames[stage].Load() == 0 {
					select {
					case <-probe.updated:
					case <-deadline.C:
						t.Fatalf("no progressive Spawning frame for %s before provider completion", probe.markers[stage])
					}
				}
				deadline.Stop()
				require.False(t, stream.completed.Load())
				ping := spawnBatchHeartbeat{done: make(chan time.Time, 1)}
				start := time.Now()
				program.Send(ping)
				select {
				case at := <-ping.done:
					t.Logf("calls=%d fragments=%d milestone=%s arrival-to-frame=%s heartbeat=%s", count, fragments, probe.markers[stage], time.Duration(probe.frames[stage].Load()-stream.arrivals[stage].Load()), at.Sub(start))
				case <-time.After(5 * time.Second):
					t.Fatal("event loop unresponsive while provider batch unfinished")
				}
			}
			if variant.pace > 0 {
				// Record arrival-relative progress instead of asserting wall-clock
				// speed: race instrumentation can exceed the provider's 27ms pace.
				t.Logf("painted before final call arrival: first=%v midpoint=%v", probe.frames[0].Load() < stream.arrivals[2].Load(), probe.frames[1].Load() < stream.arrivals[2].Load())
			}
		})
	}
}
