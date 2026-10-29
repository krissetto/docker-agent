package tui

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
	mu         sync.Mutex
	plain      string
	// headerSeen holds the latest compact-Work header count plus one; zero means no header.
	headerSeen atomic.Int64
	// headerSnap captures headerSeen at each provider gate, before that gate opens.
	headerSnap [3]atomic.Int64
	// headerActive reports an active (Working) header for the current batch.
	headerActive atomic.Bool
	// headerDrop latches if the current header's count drops, including to no count.
	headerDrop atomic.Bool
	// lastCount is the highest current-batch count observed.
	lastCount atomic.Int64
	// partialSeen marks a UI-processed partial tool call for each milestone.
	partialSeen [3]atomic.Bool
}

var spawnHeaderPresence = regexp.MustCompile(`\b(?:Working|Work) \[[+-]\]`)
var spawnCountTail = regexp.MustCompile(`\((\d+) tools?\)`)

// spawnCurrentHeader reads the bottom-most Work header: older exchanges render above
// the current batch, so the newest header is the one that counts for this batch.
func spawnCurrentHeader(plain string) (present, active bool, count int64) {
	count = -1
	lines := strings.Split(plain, "\n")
	for _, line := range slices.Backward(lines) {
		loc := spawnHeaderPresence.FindStringIndex(line)
		if loc == nil {
			continue
		}
		header := line[loc[0]:]
		if match := spawnCountTail.FindStringSubmatch(header); match != nil {
			if n, err := strconv.Atoi(match[1]); err == nil {
				count = int64(n)
			}
		}
		return true, strings.HasPrefix(header, "Working"), count
	}
	return false, false, count
}

// lastFrame returns the most recent stripped frame for failure diagnostics.
func (m *spawnBatchFrameProbe) lastFrame() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.plain
}

func (m *spawnBatchFrameProbe) Init() tea.Cmd { return m.root.Init() }
func (m *spawnBatchFrameProbe) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ping, ok := msg.(spawnBatchHeartbeat); ok {
		ping.done <- time.Now()
		return m, nil
	}
	if routed, ok := msg.(messages.RoutedMsg); ok {
		if event, ok := routed.Inner.(messages.SessionRuntimeEventMsg); ok {
			if partial, ok := event.Event.(*runtime.PartialToolCallEvent); ok {
				for i, index := range []int{0, m.count/2 - 1, m.count - 1} {
					if partial.ToolCall.ID == fmt.Sprintf("spawn-%d", index) {
						m.partialSeen[i].Store(true)
					}
				}
			}
		}
	}
	next, cmd := m.root.Update(msg)
	m.root = next.(*appModel)
	return m, cmd
}
func (m *spawnBatchFrameProbe) View() tea.View {
	view := m.root.View()
	plain := ansi.Strip(view.Content)
	m.mu.Lock()
	m.plain = plain
	m.mu.Unlock()
	present, active, count := spawnCurrentHeader(plain)
	m.headerActive.Store(present && active)
	m.headerSeen.Store(count + 1)
	if present {
		if last := m.lastCount.Load(); last > 0 && count < last {
			m.headerDrop.Store(true)
		}
		if count > m.lastCount.Load() {
			m.lastCount.Store(count)
		}
	}
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
			for _, compact := range []bool{false, true} {
				t.Run(map[bool]string{false: "noncompact", true: "compact"}[compact], func(t *testing.T) {
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
					want := len(sess.Messages)
					handle, err := owner.Runtime().CreateSession(t.Context(), sess, runtime.SessionBinding{AgentName: "root"})
					require.NoError(t, err)
					sess, err = store.GetSession(t.Context(), sess.ID)
					require.NoError(t, err)
					require.Len(t, sess.Messages, want, "fixture history survives canonical creation")
					require.False(t, handle.Metadata().Historical, "fixture must be a live session")
					require.True(t, handle.Metadata().ExecutionAvailable, "fixture must execute turns")
					a := app.New(t.Context(), owner.Runtime(), sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(rt))
					root := New(t.Context(), nil, a, t.TempDir(), nil).(*appModel)
					setter, ok := root.chatPage.(interface{ SetCompactWorkLog(enabled bool) tea.Cmd })
					require.True(t, ok, "chat page exposes the compact Work mode")
					setter.SetCompactWorkLog(compact)
					root.compactWorkLog = compact
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
					if compact {
						runCompactSpawnStages(t, probe, stream, program, variant.continuous, count)
					} else {
						for stage := range probe.markers {
							probe.headerSnap[stage].Store(probe.headerSeen.Load())
							if !variant.continuous {
								close(stream.gates[stage])
							}
							deadline := time.NewTimer(45 * time.Second)
							for probe.frames[stage].Load() == 0 {
								select {
								case <-probe.updated:
								case <-deadline.C:
									t.Logf("provider arrivals=[%d %d %d] stage=%d", stream.arrivals[0].Load(), stream.arrivals[1].Load(), stream.arrivals[2].Load(), stage)
									t.Logf("last frame before failure:\n%s", probe.lastFrame())
									t.Logf("header snapshots (count+1, 0=none) at gates: %d %d %d", probe.headerSnap[0].Load(), probe.headerSnap[1].Load(), probe.headerSnap[2].Load())
									t.Fatalf("no progressive Spawning frame for %s before provider completion", probe.markers[stage])
								}
							}
							deadline.Stop()
							t.Logf("stage %d header snapshot (count+1, 0=none) before gate: %d", stage, probe.headerSnap[stage].Load())
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
					}
					if variant.pace > 0 {
						// Record arrival-relative progress instead of asserting wall-clock
						// speed: race instrumentation can exceed the provider's 27ms pace.
						t.Logf("painted before final call arrival: first=%v midpoint=%v", probe.frames[0].Load() < stream.arrivals[2].Load(), probe.frames[1].Load() < stream.arrivals[2].Load())
					}
				})
			}
		})
	}
}

// awaitSpawnCondition polls frames until cond holds or the 45s window expires.
func awaitSpawnCondition(t *testing.T, probe *spawnBatchFrameProbe, stream *spawnBatchStream, cond func() bool, label string) {
	t.Helper()
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	for !cond() {
		select {
		case <-probe.updated:
		case <-deadline.C:
			t.Logf("provider arrivals=[%d %d %d] partialSeen=[%v %v %v] headerActive=%v headerSeen=%d", stream.arrivals[0].Load(), stream.arrivals[1].Load(), stream.arrivals[2].Load(), probe.partialSeen[0].Load(), probe.partialSeen[1].Load(), probe.partialSeen[2].Load(), probe.headerActive.Load(), probe.headerSeen.Load())
			t.Logf("last frame before failure:\n%s", probe.lastFrame())
			t.Fatalf("compact spawn batch stalled waiting for %s", label)
		}
	}
}

// runCompactSpawnStages gates compact batches on UI-processed partial tool calls with an
// active Working header, not on the hidden row or on provider Recv alone. Gate 0 opens
// before any header wait. Count assertions run only after completion.
func runCompactSpawnStages(t *testing.T, probe *spawnBatchFrameProbe, stream *spawnBatchStream, program *tea.Program, continuous bool, count int) {
	t.Helper()
	for stage := range probe.markers {
		probe.headerSnap[stage].Store(probe.headerSeen.Load())
		if stage > 0 {
			awaitSpawnCondition(t, probe, stream, func() bool { return probe.partialSeen[stage-1].Load() && probe.headerActive.Load() }, "the previous milestone processed with the active Working header")
		}
		if !continuous {
			close(stream.gates[stage])
		}
		awaitSpawnCondition(t, probe, stream, func() bool { return probe.partialSeen[stage].Load() && probe.headerActive.Load() }, "the milestone "+probe.markers[stage]+" processed with the active Working header")
		require.False(t, stream.completed.Load(), "provider batch unfinished at stage %d", stage)
		ping := spawnBatchHeartbeat{done: make(chan time.Time, 1)}
		start := time.Now()
		program.Send(ping)
		select {
		case at := <-ping.done:
			t.Logf("compact stage %d: headerActive=%v milestone=%s heartbeat=%s", stage, probe.headerActive.Load(), probe.markers[stage], at.Sub(start))
		case <-time.After(5 * time.Second):
			t.Fatal("event loop unresponsive while provider batch unfinished")
		}
	}
	// Completion needs the final gate; the settled count is asserted only afterwards.
	close(stream.gates[3])
	awaitSpawnCondition(t, probe, stream, stream.completed.Load, "provider completion")
	awaitSpawnCondition(t, probe, stream, func() bool { return probe.headerSeen.Load() == int64(count+1) }, "the final current-batch count")
	require.False(t, probe.headerDrop.Load(), "the current-batch header count never decreases, including to no count")
	require.LessOrEqual(t, probe.headerSnap[0].Load(), probe.headerSnap[1].Load())
	require.LessOrEqual(t, probe.headerSnap[1].Load(), probe.headerSnap[2].Load())
	t.Logf("compact header snapshots (count+1) at gates: %d %d %d final=%d", probe.headerSnap[0].Load(), probe.headerSnap[1].Load(), probe.headerSnap[2].Load(), probe.headerSeen.Load())
}
