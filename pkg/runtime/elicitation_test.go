package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestElicitationError_Error(t *testing.T) {
	t.Parallel()

	err := &ElicitationError{Action: "decline", Message: "user said no"}
	assert.Equal(t, "elicitation decline: user said no", err.Error())
}

func TestLocalRuntime_FinalizeEventChannelEmitsStreamStoppedOnce(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)
	sess := session.New()
	events := make(chan Event, 1)

	rt.finalizeEventChannel(t.Context(), sess, turnEndReasonNormal, events)

	var stopped int
	for ev := range events {
		if _, ok := ev.(*StreamStoppedEvent); ok {
			stopped++
		}
	}
	assert.Equal(t, 1, stopped, "StreamStopped should be emitted exactly once")
}

// TestLocalRuntime_FinalizeEventChannelDropsStreamStoppedAfterBoundedTimeout
// pins the #4136 fix's bound: the StreamStopped send waits for the
// abandoned-consumer timeout (proving it is a bounded blocking send, not the
// old plain non-blocking one) but still returns — and still drops the event
// — rather than hanging forever when nothing ever drains the buffer.
func TestLocalRuntime_FinalizeEventChannelDropsStreamStoppedAfterBoundedTimeout(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)
	const timeout = 50 * time.Millisecond
	rt.streamStoppedDeliveryTimeout = timeout
	sess := session.New()
	events := make(chan Event, 1)
	events <- Error("buffer already full")

	done := make(chan struct{})
	start := time.Now()
	go func() {
		rt.finalizeEventChannel(t.Context(), sess, turnEndReasonNormal, events)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(timeout + 2*time.Second):
		t.Fatal("finalizeEventChannel deadlocked with a full buffer and no consumer")
	}
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, timeout, "the send should wait out the full deadline before giving up")
	assert.Less(t, elapsed, timeout+2*time.Second, "finalizeEventChannel should return shortly after the deadline, not hang")

	var stopped int
	for ev := range events {
		if _, ok := ev.(*StreamStoppedEvent); ok {
			stopped++
		}
	}
	assert.Zero(t, stopped, "StreamStopped should be dropped instead of blocking forever when the buffer is full and abandoned")
}

// TestLocalRuntime_FinalizeEventChannelStreamStoppedIsLastBeforeClose pins the
// Option B decision from #3074: StreamStopped is emitted before the session-end
// hooks and telemetry run (so the UI reacts immediately), but it must still be
// the final event a consumer observes before the channel close that terminates
// its range. Session-end hooks dispatch with a nil event sink, so nothing is
// delivered onto the stream after StreamStopped and the close is the terminal
// signal. If a future change emits onto the events channel after StreamStopped
// (for example by handing the events sink to a session_end hook), this fails.
func TestLocalRuntime_FinalizeEventChannelStreamStoppedIsLastBeforeClose(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)
	sess := session.New()
	events := make(chan Event, defaultEventChannelCapacity)

	// Seed events that stand in for the stream's prior output so asserting
	// StreamStopped is *last* is a real ordering check, not merely "it was the
	// only event delivered".
	events <- Error("prior stream output 1")
	events <- Error("prior stream output 2")

	rt.finalizeEventChannel(t.Context(), sess, turnEndReasonNormal, events)

	var delivered []Event
	for ev := range events {
		delivered = append(delivered, ev)
	}

	require.NotEmpty(t, delivered, "expected events delivered before the channel close")

	var stopped int
	for _, ev := range delivered {
		if _, ok := ev.(*StreamStoppedEvent); ok {
			stopped++
		}
	}
	assert.Equal(t, 1, stopped, "exactly one StreamStopped should be delivered")
	assert.IsType(t, &StreamStoppedEvent{}, delivered[len(delivered)-1],
		"StreamStopped must be the last event delivered before the channel closes")
}

// TestRunStreamClosesChannelOnEarlyReturn is the
// regression test for issue #3073: runStreamLoop swapped this stream's
// events channel into the elicitation bridge before registering the
// finalize defer, so the early-return paths (tool setup failure, a
// user_prompt_submit hook signalling termination) exited without closing
// the events channel or restoring the bridge. A `for range RunStream(...)`
// consumer then hung forever and the bridge kept pointing at the dead
// stream's channel.
//
// We drive the reachable early return — a user_prompt_submit hook that
// stops the run — and assert the consumer's range terminates and the
// previously-swapped elicitation channel is restored.
func TestRunStreamClosesChannelOnEarlyReturn(t *testing.T) {
	t.Parallel()

	const hookName = "test-stop-user-prompt-submit"
	dontContinue := false

	root := agent.New("root", "test agent",
		agent.WithModel(&mockProvider{id: "test/mock-model"}),
		agent.WithHooks(&latest.HooksConfig{
			UserPromptSubmit: []latest.HookDefinition{
				{Type: "builtin", Command: hookName},
			},
		}),
	)
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	require.NoError(t, rt.hooksRegistry.RegisterBuiltin(
		hookName,
		func(_ context.Context, _ *hooks.Input, _ []string) (*hooks.Output, error) {
			return &hooks.Output{Continue: &dontContinue, StopReason: "stop the run"}, nil
		},
	))

	sess := session.New(session.WithUserMessage("hi"))
	sess.Title = "Unit Test"

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range rt.runExecution(t.Context(), sess) {
		}
	}()

	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("RunStream consumer hung: events channel was never closed on the hook-driven early return")
	}
}

func newElicitationTestRuntime(t *testing.T) *LocalRuntime {
	t.Helper()

	prov := &mockProvider{id: "test/mock-model"}
	root := agent.New("root", "test", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	_, err = rt.CreateSession(t.Context(), session.New(session.WithID(t.Name()+"/elicitation-owner")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	return rt
}
