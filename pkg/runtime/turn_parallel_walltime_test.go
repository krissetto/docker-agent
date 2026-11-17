package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// parallelWallClock is a goroutine-safe fake clock: tool goroutines run on the
// runtime clock while the test advances it between observable steps.
type parallelWallClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *parallelWallClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *parallelWallClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// parallelToolProvider asks for the named tools in one response, then answers
// once any tool result is in the transcript.
type parallelToolProvider struct {
	mockProvider

	toolNames []string
}

func (p *parallelToolProvider) CreateChatCompletionStream(_ context.Context, messages []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	for _, m := range messages {
		if m.Role == chat.MessageRoleTool {
			return newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build(), nil
		}
	}
	b := newStreamBuilder()
	for _, name := range p.toolNames {
		b.AddToolCallName(name, name).AddToolCallArguments(name, "{}")
	}
	return b.AddToolCallStopWithUsage(1, 1).Build(), nil
}

// gatedTool reports entry, then blocks until release closes or the call is canceled.
func gatedTool(name string, readOnly bool, entered chan<- string, release <-chan struct{}) tools.Tool {
	return tools.Tool{
		Name:        name,
		Parameters:  map[string]any{"type": "object"},
		Annotations: tools.ToolAnnotations{ReadOnlyHint: readOnly},
		Handler: func(ctx context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			entered <- name
			select {
			case <-release:
				return tools.ResultSuccess(name + " done"), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
}

// awaitEnvelope returns the next observed event matching want, failing the test on timeout.
func awaitEnvelope(t *testing.T, events <-chan SessionEvent, want func(SessionEvent) bool) SessionEvent {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			require.True(t, ok, "observation closed before the expected event")
			if want(ev) {
				return ev
			}
		case <-timeout:
			t.Fatal("timed out waiting for expected event")
		}
	}
}

func awaitToolEntered(t *testing.T, entered <-chan string, name string) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case got := <-entered:
			if got == name {
				return
			}
		case <-timeout:
			t.Fatalf("tool %q never started", name)
		}
	}
}

// runParallelTurn submits one turn through the real session handle and returns
// the observed stream plus the settlement event's timing.
func runParallelTurn(t *testing.T, clock *parallelWallClock, tools []tools.Tool, toolNames []string) (handle SessionHandle, observation Observation, submission Submission) {
	t.Helper()
	prov := &parallelToolProvider{mockProvider: mockProvider{id: "test/mock-model"}, toolNames: toolNames}
	tm := team.New(team.WithAgents(agent.New("root", "Use the tools.", agent.WithModel(prov), agent.WithToolSets(newStubToolSet(nil, tools, nil)))))
	rt, err := NewLocalRuntime(t.Context(), tm, WithClock(clock.Now), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	handle, err = rt.CreateSession(t.Context(), session.New(session.WithID(t.Name()+"/session")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	observation, err = handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	t.Cleanup(observation.Cancel)
	submission, err = handle.Submit(t.Context(), TurnInput{Content: "go"})
	require.NoError(t, err)
	return handle, observation, submission
}

// Two concurrent tool calls span 1s of elapsed clock time; the turn reports 1s, not the 2s sum.
func TestTurnTimingParallelToolsCountElapsedWallTime(t *testing.T) {
	t.Parallel()
	clock := &parallelWallClock{t: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)}
	entered := make(chan string, 4)
	release := make(chan struct{})
	toolset := []tools.Tool{
		gatedTool("slow_a", true, entered, release),
		gatedTool("slow_b", true, entered, release),
	}
	_, observation, submission := runParallelTurn(t, clock, toolset, []string{"slow_a", "slow_b"})

	// Both calls must be in flight together: each blocks until release, so
	// reaching the second entry proves they overlap.
	awaitToolEntered(t, entered, "slow_a")
	awaitToolEntered(t, entered, "slow_b")
	clock.Advance(time.Second)
	close(release)

	settled := awaitEnvelope(t, observation.Events, func(ev SessionEvent) bool {
		_, ok := ev.Event.(*TurnSettledEvent)
		return ok && ev.TurnID == submission.TurnID
	}).Event.(*TurnSettledEvent)
	require.NotNil(t, settled.Timing)
	assert.Equal(t, TurnCompleted, settled.Outcome)
	assert.Equal(t, int64(1000), settled.Timing.DurationMs, "parallel calls count elapsed wall time once, not the 2s sum")
	assert.Zero(t, settled.Timing.WaitingMs)
}

// An approval wait overlapping a still-running concurrent call is subtracted
// only for the blocked interval: 3s elapsed minus the 2s approval wait is 1s.
func TestTurnTimingApprovalWaitOverlappingParallelWork(t *testing.T) {
	t.Parallel()
	clock := &parallelWallClock{t: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)}
	entered := make(chan string, 4)
	slowRelease := make(chan struct{})
	gatedRelease := make(chan struct{})
	close(gatedRelease)
	toolset := []tools.Tool{
		gatedTool("slow", true, entered, slowRelease),
		// Not read-only, so the default policy asks before running it.
		gatedTool("gated", false, entered, gatedRelease),
	}
	handle, observation, submission := runParallelTurn(t, clock, toolset, []string{"slow", "gated"})

	approval := awaitEnvelope(t, observation.Events, func(ev SessionEvent) bool {
		_, ok := ev.Event.(*ToolCallConfirmationEvent)
		return ok
	})
	awaitToolEntered(t, entered, "slow")
	// Approval opens at elapsed 0 while slow is already running.
	clock.Advance(2 * time.Second)
	require.NoError(t, handle.Respond(t.Context(), InteractionResponse{InteractionID: approval.InteractionID, Kind: InteractionConfirmation, Resume: ResumeApprove()}))
	awaitToolEntered(t, entered, "gated")
	clock.Advance(time.Second)
	close(slowRelease)

	settled := awaitEnvelope(t, observation.Events, func(ev SessionEvent) bool {
		_, ok := ev.Event.(*TurnSettledEvent)
		return ok && ev.TurnID == submission.TurnID
	}).Event.(*TurnSettledEvent)
	require.NotNil(t, settled.Timing)
	assert.Equal(t, TurnCompleted, settled.Outcome)
	assert.Equal(t, int64(2000), settled.Timing.WaitingMs, "only the blocked approval interval is waiting")
	assert.Equal(t, int64(1000), settled.Timing.DurationMs, "3s elapsed minus the 2s approval wait; concurrent work is not subtracted")
}
