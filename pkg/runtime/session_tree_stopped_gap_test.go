package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// A descendant stopped between two baselines of the same watch must not fail the
// second baseline: it has no live observation, and its stop must be in the tree.
func TestTreeObservationAdmitsDescendantStoppedInGap(t *testing.T) {
	rt, _, root := newRestoreFixture(t)
	planner, err := rt.team.Agent("planner")
	require.NoError(t, err)
	child := session.New(session.WithID("gap-child"), session.WithAgentName("planner"), session.WithParentID(root.ID))
	child.AsyncSubagent = true
	require.NoError(t, rt.subagents.registerIdleChild(root, "root", child, planner, subagent.AllowedSubagent{Agent: "planner"}))
	grand := session.New(session.WithID("gap-grand"), session.WithAgentName("planner"), session.WithParentID(child.ID))
	grand.AsyncSubagent = true
	require.NoError(t, rt.subagents.registerIdleChild(child, "planner", grand, planner, subagent.AllowedSubagent{Agent: "planner"}))
	sibling := session.New(session.WithID("gap-sibling"), session.WithAgentName("planner"), session.WithParentID(child.ID))
	sibling.AsyncSubagent = true
	require.NoError(t, rt.subagents.registerIdleChild(child, "planner", sibling, planner, subagent.AllowedSubagent{Agent: "planner"}))

	childDriver, ok := rt.sessionDrivers.Lookup(child.ID)
	require.True(t, ok)
	grandDriver, ok := rt.sessionDrivers.Lookup(grand.ID)
	require.True(t, ok)
	siblingDriver, ok := rt.sessionDrivers.Lookup(sibling.ID)
	require.True(t, ok)
	childHandle := &sessionHandle{runtime: rt, driver: childDriver, sessionID: child.ID, agentName: "planner"}
	grandHandle := &sessionHandle{runtime: rt, driver: grandDriver, sessionID: grand.ID, agentName: "planner"}

	// First baseline: the grandchild has a live observation, which the watch
	// then cancels, as an unplaced rebaseline does.
	first, err := childHandle.Observe(t.Context(), ObserveOptions{Tree: true, OrderedTree: true})
	require.NoError(t, err)
	require.Equal(t, 1, liveSubscribers(grandDriver, grand.ID), "the first baseline observes the grandchild live")
	first.Cancel()
	drainUntilClosed(t, first.TreeUpdates)
	drainUntilClosed(t, first.Errors)
	require.Zero(t, liveSubscribers(grandDriver, grand.ID), "cancelling the baseline releases the grandchild observation")
	require.Zero(t, liveSubscribers(childDriver, child.ID))

	// The stop lands in the gap, before the second baseline.
	require.NoError(t, grandHandle.StopSubtree(t.Context()))

	second, err := childHandle.Observe(t.Context(), ObserveOptions{Tree: true, OrderedTree: true})
	require.NoError(t, err, "a descendant stopped in the gap must not fail the rebaseline")
	require.Zero(t, liveSubscribers(grandDriver, grand.ID), "a stopped descendant gets no live observation")
	require.Equal(t, 1, liveSubscribers(childDriver, child.ID), "the watch root is observed live")
	require.Equal(t, 1, liveSubscribers(siblingDriver, sibling.ID), "live siblings are still observed")

	var childBaseline *SessionSnapshot
	var grandStopped bool
	for i := range second.Initial {
		switch second.Initial[i].Status.SessionID {
		case child.ID:
			childBaseline = &second.Initial[i]
		case grand.ID:
			grandStopped = second.Initial[i].Status.Stopped
		}
	}
	require.NotNil(t, childBaseline, "the baseline carries the watch root")
	require.True(t, grandStopped, "the stopped descendant is still part of the baseline, marked stopped")
	require.NotNil(t, childBaseline.Session)
	tree := childBaseline.Session.GetSubagentTree()
	require.NotNil(t, tree, "the baseline carries the watch root's live subtree")
	node, ok := findSubtreeNode(tree.Nodes, grand.ID)
	require.True(t, ok, "the stopped grandchild is in the carried subtree")
	assert.Equal(t, subagent.NodeStopped, node.State, "the stop is in the baseline without waiting for a later event")

	// Live siblings keep flowing after the skip.
	siblingDriver.events.Publish(sibling.ID, StreamStarted(sibling.ID, "planner"))
	select {
	case update := <-second.TreeUpdates:
		require.NotNil(t, update.Event)
		assert.Equal(t, sibling.ID, update.Event.SessionID)
	case <-time.After(time.Second):
		t.Fatal("live sibling event was not delivered after the stopped-descendant skip")
	}

	second.Cancel()
	drainUntilClosed(t, second.TreeUpdates)
	drainUntilClosed(t, second.Errors)
	assert.Zero(t, liveSubscribers(childDriver, child.ID), "cancel releases the watch root")
	assert.Zero(t, liveSubscribers(siblingDriver, sibling.ID), "cancel releases the sibling")
	assert.Zero(t, liveSubscribers(grandDriver, grand.ID), "cancel leaves no grandchild subscription")

	// A stopped watch root is still terminal.
	_, err = grandHandle.Observe(t.Context(), ObserveOptions{Tree: true, OrderedTree: true})
	require.Error(t, err)
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionErrorStopped, sessionErr.Kind)
}

// liveSubscribers counts live observers of a session on its own driver's hub.
// If the settled-descendant lookup fails after the stop is observed, the baseline
// fails with the original observe error, exactly as before the settled path existed.
func TestTreeObservationStoppedDescendantLookupFailureKeepsObserveError(t *testing.T) {
	rt, _, root := newRestoreFixture(t)
	planner, err := rt.team.Agent("planner")
	require.NoError(t, err)
	child := session.New(session.WithID("lookup-child"), session.WithAgentName("planner"), session.WithParentID(root.ID))
	child.AsyncSubagent = true
	require.NoError(t, rt.subagents.registerIdleChild(root, "root", child, planner, subagent.AllowedSubagent{Agent: "planner"}))
	grand := session.New(session.WithID("lookup-grand"), session.WithAgentName("planner"), session.WithParentID(child.ID))
	grand.AsyncSubagent = true
	require.NoError(t, rt.subagents.registerIdleChild(child, "planner", grand, planner, subagent.AllowedSubagent{Agent: "planner"}))
	grandDriver, ok := rt.sessionDrivers.Lookup(grand.ID)
	require.True(t, ok)
	grandHandle := &sessionHandle{runtime: rt, driver: grandDriver, sessionID: grand.ID, agentName: "planner"}
	require.NoError(t, grandHandle.StopSubtree(t.Context()))

	childDriver, ok := rt.sessionDrivers.Lookup(child.ID)
	require.True(t, ok)
	childHandle := &sessionHandle{runtime: rt, driver: childDriver, sessionID: child.ID, agentName: "planner"}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observeErr := &SessionError{Kind: SessionErrorStopped, SessionID: grand.ID, Operation: "observe"}
	observe := func(ctx context.Context, id string, options ObserveOptions) (Observation, error) {
		if id != grand.ID {
			handle, err := rt.SessionByID(id)
			if err != nil {
				return Observation{}, err
			}
			return handle.Observe(ctx, options)
		}
		// Cancelling here makes the settled-descendant lookup fail on its own context.
		cancel()
		return Observation{}, observeErr
	}
	observation, err := childHandle.observeTreeWith(ctx, ObserveOptions{Tree: true, OrderedTree: true}, observe)
	require.Error(t, err)
	assert.Same(t, observeErr, err, "a failed lookup must return the original observe error")
	assert.Empty(t, observation.Initial, "no settled baseline entry is admitted when the lookup fails")
	assert.Zero(t, liveSubscribers(grandDriver, grand.ID))
}

func liveSubscribers(d *sessionDriver, sessionID string) int {
	d.events.mu.Lock()
	defer d.events.mu.Unlock()
	return len(d.events.publicSubs[sessionID])
}

// drainUntilClosed discards buffered values until the channel closes. Closing
// happens after every observer goroutine exits, so it is a sync point, not a sleep.
func drainUntilClosed[T any](t *testing.T, ch <-chan T) {
	t.Helper()
	timeout := time.After(time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("observation channel did not close after cancel")
		}
	}
}

func findSubtreeNode(nodes []subagent.NodeSnapshot, sessionID string) (subagent.Node, bool) {
	for _, node := range nodes {
		if node.Node.SessionID == sessionID {
			return node.Node, true
		}
		if found, ok := findSubtreeNode(node.Children, sessionID); ok {
			return found, true
		}
	}
	return subagent.Node{}, false
}
