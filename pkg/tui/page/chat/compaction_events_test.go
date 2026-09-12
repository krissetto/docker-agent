package chat

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// compactionEvent builds a completed compaction event for the given session.
func compactionEvent(sessionID, outcome string) *runtime.SessionCompactionEvent {
	return &runtime.SessionCompactionEvent{
		Type:         "session_compaction",
		SessionID:    sessionID,
		Status:       "completed",
		Outcome:      outcome,
		AgentContext: runtime.AgentContext{AgentName: "worker"},
	}
}

// TestSubSessionCompactionKeepsRootWorkState pins the #3439 event-routing
// contract: a sub-agent session finishing a compaction must not flip the
// root chat idle, clear its stream cancel func, or process root queues.
func TestSubSessionCompactionKeepsRootWorkState(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(queueTestRuntime{})), service.NewSessionState(sess)).(*chatPage)

	_, cancel := context.WithCancel(t.Context())
	defer cancel()
	p.msgCancel = cancel
	p.working = true
	p.messageQueue = []queuedMessage{{content: "queued while working"}}

	handled, _ := p.handleRuntimeEvent(compactionEvent("sub-session-1", runtime.CompactionOutcomeApplied))
	require.True(t, handled)

	assert.True(t, p.working, "a sub-session compaction must not mark the root chat idle")
	assert.NotNil(t, p.msgCancel, "the root stream cancel func must stay intact")
	assert.Len(t, p.messageQueue, 1, "root queued messages must not be processed")
}

func TestStandaloneRootCompactionStartedThenCompletedProjectsWorkState(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(queueTestRuntime{})), service.NewSessionState(sess)).(*chatPage)
	p.working = false

	handled, _ := p.handleRuntimeEvent(&runtime.SessionCompactionEvent{SessionID: sess.ID, Status: "started"})
	require.True(t, handled)
	assert.True(t, p.working)

	handled, _ = p.handleRuntimeEvent(compactionEvent(sess.ID, runtime.CompactionOutcomeApplied))
	require.True(t, handled)
	assert.False(t, p.working)
}

// TestStandaloneRootCompactionResetsWorkState pins the explicit /compact
// path: it runs without a surrounding stream (Summarize emits no
// StreamStarted), so its completion event is the terminal signal that
// cleans up the work state.
func TestStandaloneRootCompactionResetsWorkState(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(queueTestRuntime{})), service.NewSessionState(sess)).(*chatPage)

	_, cancel := context.WithCancel(t.Context())
	defer cancel()
	p.msgCancel = cancel
	p.working = true
	require.Zero(t, p.lifecycle.Depth(), "explicit /compact runs outside any stream")

	handled, _ := p.handleRuntimeEvent(compactionEvent(sess.ID, runtime.CompactionOutcomeApplied))
	require.True(t, handled)

	assert.False(t, p.working, "the standalone compaction completion marks the chat idle")
	assert.Nil(t, p.msgCancel)
}

// TestAutoRootCompactionMidStreamKeepsWorkState pins the #3872 contract: an
// automatic (threshold-triggered) root compaction completes inside an active
// session turn, so whatever the outcome it only updates presentation — the
// outer stream's cancel func, working flag, depth and queued messages stay
// intact until StreamStopped.
func TestAutoRootCompactionMidStreamKeepsWorkState(t *testing.T) {
	t.Parallel()

	outcomes := []string{
		runtime.CompactionOutcomeApplied,
		runtime.CompactionOutcomeSkipped,
		runtime.CompactionOutcomeFailed,
	}
	for _, outcome := range outcomes {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()

			sess := session.New()
			p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(queueTestRuntime{})), service.NewSessionState(sess)).(*chatPage)

			_, cancel := context.WithCancel(t.Context())
			defer cancel()
			p.msgCancel = cancel
			p.messageQueue = []queuedMessage{{content: "queued while working"}}

			handled, _ := p.handleRuntimeEvent(runtime.StreamStarted(sess.ID, "root"))
			require.True(t, handled)
			require.Equal(t, 1, p.lifecycle.Depth())
			require.True(t, p.working)

			handled, _ = p.handleRuntimeEvent(compactionEvent(sess.ID, outcome))
			require.True(t, handled)

			assert.True(t, p.working, "a mid-stream compaction must not mark the chat idle")
			assert.NotNil(t, p.msgCancel, "the outer stream's cancel func must stay intact")
			assert.Equal(t, 1, p.lifecycle.Depth(), "a compaction event is not a stream boundary")
			assert.Len(t, p.messageQueue, 1, "queued messages must wait for the stream to stop")
		})
	}
}

// TestStreamStoppedAfterMidStreamCompactionCleansUp proves the cleanup the
// mid-stream compaction deferred actually happens at the outer
// StreamStopped: the chat goes idle and the cancel func is released.
func TestStreamStoppedAfterMidStreamCompactionCleansUp(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(queueTestRuntime{})), service.NewSessionState(sess)).(*chatPage)

	_, cancel := context.WithCancel(t.Context())
	defer cancel()
	p.msgCancel = cancel

	handled, _ := p.handleRuntimeEvent(runtime.StreamStarted(sess.ID, "root"))
	require.True(t, handled)
	handled, _ = p.handleRuntimeEvent(compactionEvent(sess.ID, runtime.CompactionOutcomeApplied))
	require.True(t, handled)
	require.True(t, p.working)

	handled, cmd := p.handleRuntimeEvent(runtime.StreamStopped(sess.ID, "root", "normal"))
	require.True(t, handled)
	require.NotNil(t, cmd)

	assert.Zero(t, p.lifecycle.Depth())
	assert.False(t, p.working, "the outermost StreamStopped marks the chat idle")
	assert.Nil(t, p.msgCancel, "the outermost StreamStopped releases the cancel func")
}

// TestStreamStoppedDoesNotAdvanceSessionProjectedQueue proves stream boundaries
// do not mutate session-owned accepted input. Promotion is the sole transition.
func TestStreamStoppedDoesNotAdvanceSessionProjectedQueue(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(queueTestRuntime{})), service.NewSessionState(sess)).(*chatPage)

	_, cancel := context.WithCancel(t.Context())
	defer cancel()
	p.msgCancel = cancel
	p.messageQueue = []queuedMessage{{content: "queued while working"}}

	handled, _ := p.handleRuntimeEvent(runtime.StreamStarted(sess.ID, "root"))
	require.True(t, handled)
	handled, _ = p.handleRuntimeEvent(compactionEvent(sess.ID, runtime.CompactionOutcomeApplied))
	require.True(t, handled)
	require.Len(t, p.messageQueue, 1, "the compaction must leave the queue to StreamStopped")

	handled, cmd := p.handleRuntimeEvent(runtime.StreamStopped(sess.ID, "root", "normal"))
	require.True(t, handled)
	require.NotNil(t, cmd)

	assert.Len(t, p.messageQueue, 1, "StreamStopped cannot pop session-projected input")
	assert.False(t, p.working, "the completed turn settles until session promotion starts its successor")
	assert.Nil(t, p.msgCancel)
	assert.Zero(t, p.lifecycle.Depth())
}

// TestEscCancelsStreamAfterMidStreamCompaction proves Esc stays wired after
// an automatic mid-stream compaction: the page still reads as working and
// the preserved cancel func cancels the outer stream. InterruptModeNone
// skips the confirmation dialog, making the cancellation synchronous.
func TestEscCancelsStreamAfterMidStreamCompaction(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(queueTestRuntime{})), service.NewSessionState(sess),
		WithInterruptMode(msgtypes.InterruptModeNone)).(*chatPage)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p.msgCancel = cancel

	handled, _ := p.handleRuntimeEvent(runtime.StreamStarted(sess.ID, "root"))
	require.True(t, handled)
	handled, _ = p.handleRuntimeEvent(compactionEvent(sess.ID, runtime.CompactionOutcomeApplied))
	require.True(t, handled)

	_, cmd := p.handleKeyPress(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Nil(t, cmd, "local render context alone cannot cancel session execution")
	select {
	case <-ctx.Done():
		t.Fatal("Esc must not treat the local view context as session lifecycle")
	default:
	}
	assert.NotNil(t, p.msgCancel, "local callback remains until a canonical active turn is cancelled")
}

// TestSubSessionCompactionNotice verifies the agent-scoped feedback for each
// terminal outcome and the silence of non-terminal events.
func TestSubSessionCompactionNotice(t *testing.T) {
	t.Parallel()

	assert.Nil(t, subSessionCompactionNotice(&runtime.SessionCompactionEvent{Status: "started"}),
		"the request was already announced; started stays silent")

	for _, outcome := range []string{"", runtime.CompactionOutcomeApplied, runtime.CompactionOutcomeSkipped, runtime.CompactionOutcomeFailed} {
		assert.NotNil(t, subSessionCompactionNotice(compactionEvent("sub-1", outcome)),
			"outcome %q must produce user feedback", outcome)
	}
}

// TestIsSubSessionEvent covers the root/sub classification, including the
// empty-session-ID backward-compatibility case.
func TestIsSubSessionEvent(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(queueTestRuntime{})), service.NewSessionState(sess)).(*chatPage)

	assert.False(t, p.isSubSessionEvent(""), "events without a session ID belong to the root")
	assert.False(t, p.isSubSessionEvent(sess.ID))
	assert.True(t, p.isSubSessionEvent("some-other-session"))
}
