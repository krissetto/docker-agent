package runtime

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// A failed async child completion must publish the actionable failed state in
// one step; NeedsAttention without WaitingOn is a transient half-state that a
// later refresh turns into "failed".
func TestFailedChildCompletionPublishesFailedAttentionAtomically(t *testing.T) {
	m, _, _, id := attentionFixture(t)
	m.reportTurn(t, id, subagent.NodeFailed, "boom")

	node, ok := m.tree.Node(id)
	require.True(t, ok)
	assert.Equal(t, subagent.NodeFailed, node.State)
	assert.Equal(t, "boom", node.Error)
	assert.True(t, node.NeedsAttention)
	assert.Equal(t, "failed", node.WaitingOn)
}

// Restarting a child must not carry the previous failure's error or attention
// into the running projection.
func TestMarkChildRunningClearsStaleFailureProjection(t *testing.T) {
	m, _, _, id := attentionFixture(t)
	require.NoError(t, m.tree.Update(id, func(n *subagent.Node) {
		n.State = subagent.NodeFailed
		n.Error = "boom"
		n.NeedsAttention = true
		n.WaitingOn = "failed"
	}))

	m.markChildRunning(id)

	node, ok := m.tree.Node(id)
	require.True(t, ok)
	assert.Equal(t, subagent.NodeRunning, node.State)
	assert.Empty(t, node.Error)
	assert.False(t, node.NeedsAttention)
	assert.Empty(t, node.WaitingOn)
}

// A turn finished while cancelling whose stream surfaces a non-context error
// must settle as canceled, not as an actionable failure. Drives the real
// finishRun path with the phase the cancel request leaves behind.
func TestCanceledChildRunDoesNotBecomeActionableFailure(t *testing.T) {
	m := newTestSubagentManager(t)
	parent := session.New(session.WithID("cparent"), session.WithAgentName("root"))
	child := session.New(session.WithID("cchild"), session.WithParentID(parent.ID), session.WithAgentName("worker"), session.WithAsyncSubagent(true))
	const id = subagent.NodeID("cchild-node")
	m.registerChild(parent, "root", id, "worker", child)
	d, ok := m.r.sessionDrivers.Lookup(child.ID)
	require.True(t, ok)
	d.mu.Lock()
	d.phase = sessionCancelling
	d.generation = 1
	d.activeRequestID = "cancel-turn"
	d.mu.Unlock()

	d.finishRun(1, errors.New("request aborted").Error())

	node, ok := m.tree.Node(id)
	require.True(t, ok)
	assert.NotEqual(t, subagent.NodeFailed, node.State, "cancelled turn is not a failure")
	assert.False(t, node.NeedsAttention)
	assert.NotEqual(t, "failed", node.WaitingOn)
}

// A capacity-denied wake arms a scheduler retry through the real Post path.
// A stop that lands afterwards must release that flag when the scheduler
// maintains the stopped driver; a stopped driver can never retry, so a stale
// flag would protect it from reclaim and idle detection forever.
func TestDeniedWakeStopMaintainReleasesRetry(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := r.sessionDrivers.Get(session.New(session.WithID("denied-child"), session.WithParentID("root"), session.WithAsyncSubagent(true)))
	d.SetPreStartGate(func() bool { return false }, func() {})

	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "denied", RequestID: "accepted"}, true))
	require.True(t, d.HasPending())
	d.mu.Lock()
	armed := d.retryRunning
	d.mu.Unlock()
	require.True(t, armed, "capacity-denied wake arms a retry")

	d.StopAll()
	r.sessionDrivers.maintainDriver(d)

	d.mu.Lock()
	defer d.mu.Unlock()
	assert.False(t, d.retryRunning, "stopped driver must not keep a stale retry armed")
}

// A non-retryable start error must not keep a retry armed, and repeated
// signals must cost one attempt each: no self-sustaining retry timer.
func TestPermanentStartErrorIsNotRetriedBySchedulerTimer(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := r.sessionDrivers.Get(session.New(session.WithID("permanent-child"), session.WithParentID("root"), session.WithAsyncSubagent(true)))
	var attempts atomic.Int64
	d.SetPreStartErrorGate(func() error {
		attempts.Add(1)
		return errors.New("permanent denial")
	}, nil)

	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "rejected", RequestID: "accepted"}, true))
	require.Eventually(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return !d.retryRunning
	}, 2*time.Second, 5*time.Millisecond, "permanent error must release the retry flag")
	require.True(t, d.HasPending(), "rejected input stays queued for a later wake")

	settled := attempts.Load()
	time.Sleep(1300 * time.Millisecond) // longer than the scheduler's maximum backoff
	assert.Equal(t, settled, attempts.Load(), "no timer re-attempts a permanent error")

	r.sessionDrivers.signalWork()
	require.Eventually(t, func() bool { return attempts.Load() == settled+1 }, 2*time.Second, 5*time.Millisecond)
	time.Sleep(1300 * time.Millisecond)
	assert.Equal(t, settled+1, attempts.Load(), "one signal costs exactly one attempt")
}

// A cancelled root turn whose stream error is kept as LastError must settle the
// root projection as idle, not as an actionable failure.
func TestCancelledRootSettlesWithoutFailedAttention(t *testing.T) {
	m := newTestSubagentManager(t)
	root := session.New(session.WithID("cancel-root"), session.WithAgentName("root"))
	d := m.r.sessionDrivers.Get(root)
	m.ensureRoot(root, "root")
	d.mu.Lock()
	d.phase = sessionCancelling
	d.generation = 1
	d.activeRequestID = "root-cancel-turn"
	d.mu.Unlock()

	d.finishRun(1, errors.New("request aborted").Error())

	node, ok := m.tree.Node(subagent.SessionRootID(root.ID))
	require.True(t, ok)
	assert.NotEqual(t, subagent.NodeFailed, node.State)
	assert.False(t, node.NeedsAttention)
	assert.NotEqual(t, "failed", node.WaitingOn)
}
