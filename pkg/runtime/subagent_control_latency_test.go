package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/stretchr/testify/require"
)

func controlManager(t *testing.T) (*subagentManager, *sessionDriver, *sessionDriver) {
	t.Helper()
	m := newTestSubagentManager(t)
	store := session.NewInMemorySessionStore()
	m.r.sessionStore = store
	m.coord = store.(session.CoordinationStore)
	parent := session.New(session.WithID("target-root"))
	child := session.New(session.WithID("target-child"), session.WithParentID(parent.ID))
	require.NoError(t, store.AddSession(t.Context(), parent))
	node := subagent.Node{ID: "target-node", Parent: subagent.SessionRootID(parent.ID), SessionID: child.ID, Agent: "worker", State: subagent.NodeIdle}
	record := session.ChildRecord{RootSessionID: parent.ID, ParentSessionID: parent.ID, Node: node, Revision: 1}
	require.NoError(t, m.coord.AdmitChild(t.Context(), session.ChildAdmission{Child: child, Record: record}))
	pd := m.r.sessionDrivers.Get(parent)
	cd := m.r.sessionDrivers.Get(child)
	m.mu.Lock()
	m.ensureSessionLocked(parent, "root", "")
	require.NoError(t, m.tree.Add(node))
	m.ensureSessionLocked(child, "worker", node.ID)
	m.children[node.ID] = &childRecord{name: "worker", parentSession: parent.ID, sessionID: child.ID, durable: record}
	m.mu.Unlock()
	m.persistSnapshot()
	t.Cleanup(func() { pd.closeOwner(); cd.closeOwner() })
	return m, pd, cd
}

func controlWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("barrier not reached")
	}
}

func TestStopBypassesUnrelatedOwnerProjection(t *testing.T) {
	m, _, child := controlManager(t)
	unrelated := session.New(session.WithID("unrelated-root"))
	ud := m.r.sessionDrivers.Get(unrelated)
	t.Cleanup(ud.closeOwner)
	m.ensureRoot(unrelated, "root")
	m.persistSnapshot()
	entered, release, ownerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		_ = ud.ownerCall(t.Context(), func() error { close(entered); <-release; return nil })
		close(ownerDone)
	}()
	controlWait(t, entered)
	require.NoError(t, m.tree.Update(subagent.SessionRootID(unrelated.ID), func(n *subagent.Node) { n.ToolCalls++ }))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { _, err := m.stopChildContext(ctx, "target-root", "target-node"); stopped <- err }()
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(time.Second):
		close(release)
		t.Fatal("stop waited for unrelated owner")
	}
	require.True(t, child.isStopped())
	controlWait(t, child.Done())
	records, err := m.coord.LoadChildren(t.Context(), "target-root")
	require.NoError(t, err)
	require.Equal(t, subagent.NodeStopped, records[0].Node.State)
	require.True(t, m.metricsMu.TryLock())
	m.metricsMu.Unlock()
	close(release)
	controlWait(t, ownerDone)
}

func TestStopSuccessRequiresToolDrain(t *testing.T) {
	m, _, child := controlManager(t)
	runCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.NoError(t, child.ownerCall(t.Context(), func() error { child.cancel = cancel; return nil }))
	child.wg.Add(1)
	stopped := make(chan error, 1)
	go func() { _, err := m.stopChildContext(t.Context(), "target-root", "target-node"); stopped <- err }()
	controlWait(t, runCtx.Done())
	require.True(t, child.isStopped())
	select {
	case <-child.Done():
		child.wg.Done()
		t.Fatal("running fake tool counted as drained")
	default:
	}
	select {
	case err := <-stopped:
		child.wg.Done()
		t.Fatalf("stop returned before fake tool drained: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	t.Log("cancel accepted and tombstone exists; successful stop waits for uncooperative fake tool/workgroup release")
	child.wg.Done()
	require.NoError(t, <-stopped)
}

func TestStopCancelsBusyTargetBeforeOwnerAdmission(t *testing.T) {
	m, _, child := controlManager(t)
	runCtx, cancelRun := context.WithCancel(t.Context())
	defer cancelRun()
	require.NoError(t, child.ownerCall(t.Context(), func() error { child.cancel = cancelRun; return nil }))
	child.wg.Add(1)
	entered, release, ownerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		_ = child.ownerCall(t.Context(), func() error { close(entered); <-release; return nil })
		close(ownerDone)
	}()
	controlWait(t, entered)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { _, err := m.stopChildContext(ctx, "target-root", "target-node"); stopped <- err }()
	controlWait(t, runCtx.Done())
	records, err := m.coord.LoadChildren(t.Context(), "target-root")
	require.NoError(t, err)
	require.Equal(t, subagent.NodeStopped, records[0].Node.State)
	cancel()
	select {
	case err := <-stopped:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		close(release)
		child.wg.Done()
		t.Fatal("stop ignored canceled caller while target owner busy")
	}
	close(release)
	controlWait(t, ownerDone)
	require.NoError(t, child.awaitStop(t.Context()))
	child.wg.Done()
}

func TestTreeProjectionCoalescesNewestNestedView(t *testing.T) {
	m, root, child := controlManager(t)
	entered, release, ownerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		_ = root.ownerCall(t.Context(), func() error { close(entered); <-release; return nil })
		close(ownerDone)
	}()
	controlWait(t, entered)
	for i := range 20 {
		require.NoError(t, m.tree.Update("target-node", func(n *subagent.Node) { n.ToolCalls = int64(i + 1) }))
		m.persistSnapshot()
	}
	close(release)
	controlWait(t, ownerDone)
	for _, d := range []*sessionDriver{root, child} {
		require.NoError(t, d.ownerCall(t.Context(), d.applyTreeProjectionLocked))
		owned, err := d.ownerSnapshot(t.Context())
		require.NoError(t, err)
		tree := owned.GetSubagentTree()
		node, ok := subtreeForSession(*tree, "target-child")
		require.True(t, ok)
		require.Equal(t, int64(20), node.Node.ToolCalls)
	}
}

type controlCheckpointStore struct {
	subagent.Store
	entered chan struct{}
	release chan struct{}
}

func (s *controlCheckpointStore) SaveTree(ctx context.Context, id string, tree subagent.Snapshot) error {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
		return s.Store.SaveTree(ctx, id, tree)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestChildAdmissionBypassesUnrelatedCheckpointAndOwner(t *testing.T) {
	m, _, _ := controlManager(t)
	store := &controlCheckpointStore{Store: subagent.NewInMemoryStore(), entered: make(chan struct{}, 1), release: make(chan struct{})}
	m.r.subagentStore = store
	unrelated := session.New(session.WithID("checkpoint-unrelated"))
	ud := m.r.sessionDrivers.Get(unrelated)
	t.Cleanup(ud.closeOwner)
	m.ensureRoot(unrelated, "root")
	entered, release, ownerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		_ = ud.ownerCall(t.Context(), func() error { close(entered); <-release; return nil })
		close(ownerDone)
	}()
	controlWait(t, entered)
	m.persistSnapshot()
	controlWait(t, store.entered)
	parent := session.New(session.WithID("target-root"))
	child := session.New(session.WithID("new-child"), session.WithParentID(parent.ID))
	target := agent.New("worker", "fake")
	admitted := make(chan error, 1)
	go func() {
		_, err := m.admitChildContext(t.Context(), parent, "root", child, target, subagent.AllowedSubagent{Agent: "worker"}, "", false)
		admitted <- err
	}()
	select {
	case err := <-admitted:
		require.NoError(t, err)
	case <-time.After(time.Second):
		close(release)
		close(store.release)
		t.Fatal("child admission waited for unrelated checkpoint or owner")
	}
	records, err := m.coord.LoadChildren(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Len(t, records, 2, "admission remains authoritative before returning")
	close(release)
	close(store.release)
	controlWait(t, ownerDone)
	if d, ok := m.r.sessionDrivers.Lookup(child.ID); ok {
		t.Cleanup(d.closeOwner)
	}
}

func TestRepeatedStopDuringManualResumeRequiresFreshReceipt(t *testing.T) {
	_, _, child := controlManager(t)
	child.requestStop(false)
	require.NoError(t, child.awaitStop(t.Context()))
	previous := child.stopRequest.Load()
	entered, release, ownerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		_ = child.ownerCall(t.Context(), func() error {
			child.stopped = false
			close(entered)
			<-release
			return nil
		})
		close(ownerDone)
	}()
	controlWait(t, entered)
	child.requestStop(false)
	require.NotSame(t, previous, child.stopRequest.Load(), "applied request cannot acknowledge a later stop")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, child.awaitStop(ctx), context.Canceled)
	close(release)
	controlWait(t, ownerDone)
	require.NoError(t, child.awaitStop(t.Context()))
	require.True(t, child.isStopped(), "manual resume publication cannot discard newer stop")
}

func TestCanceledRootStopRetainsDurableWithdrawalIntent(t *testing.T) {
	m, root, _ := controlManager(t)
	entered, release, ownerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		_ = root.ownerCall(t.Context(), func() error { close(entered); <-release; return nil })
		close(ownerDone)
	}()
	controlWait(t, entered)
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan error, 1)
	h := &sessionHandle{runtime: m.r, driver: root, sessionID: "target-root"}
	go func() { stopped <- h.stopRootTree(ctx) }()
	// Cancellation must leave only the caller, not withdraw the owner's intent.
	deadline := time.After(time.Second)
	for root.stopRequest.Load() == nil {
		select {
		case <-deadline:
			close(release)
			t.Fatal("root stop request not published")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	require.ErrorIs(t, <-stopped, context.Canceled)
	close(release)
	controlWait(t, ownerDone)
	require.NoError(t, root.awaitStop(t.Context()))
	require.NoError(t, root.ownerCall(t.Context(), func() error {
		require.True(t, root.durableStopRequested, "late durable writes must still reconcile root stop")
		return nil
	}))
}

func TestRootStopIntentSurvivesReplacementBeforePublication(t *testing.T) {
	_, root, _ := controlManager(t)
	require.NoError(t, root.ownerCall(t.Context(), func() error {
		// Hold the owner so the mailbox cannot apply or publish between these steps.
		root.requestStopWithIntent(false, true)
		root.applyStopLocked()
		previous := root.stopRequest.Load()
		root.requestStop(false)
		require.NotSame(t, previous, root.stopRequest.Load())
		root.publishRegistryStateLocked()
		require.True(t, root.durableStopRequested, "replacing the applied receipt must retain root withdrawal intent")
		return nil
	}))
	require.NoError(t, root.awaitStop(t.Context()))
}
