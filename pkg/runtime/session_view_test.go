package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

func viewRootFixture(t *testing.T) (*LocalRuntime, SessionRuntimeSupervisor, session.Store, *session.Session) {
	t.Helper()
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("archived-root"), session.WithTitle("archived"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	root.WorkingDir = "/synthetic/workspace"
	pending := session.UserMessage("archived FIFO")
	pending.TurnID, pending.Pending, pending.Accepted = "accepted-archived", true, true
	pending.InputOrigin = session.InputOriginUser
	root.AddMessage(pending)
	require.NoError(t, store.AddSession(t.Context(), root))
	rt, owner := coordinationRuntime(t, store, coordinationReply("done"), coordinationReply("child"), WithWorkingDir(root.WorkingDir))
	return rt, owner, store, root
}

func TestSessionViewPrepareIsDetachedAbortAndInfoDefensive(t *testing.T) {
	rt, owner, _, root := viewRootFixture(t)
	preparer := owner.Runtime().(SessionViewPreparer)
	prepared, err := preparer.PrepareSessionView(t.Context(), root.ID)
	require.NoError(t, err)
	info := prepared.Info()
	assert.Equal(t, root.ID, info.SessionID)
	assert.Equal(t, root.WorkingDir, info.WorkingDir)
	info.Session.SetTitle("mutated")
	info.Session.SetAttribute("mutated", "yes")
	assert.Equal(t, "archived", prepared.Info().Session.TitleSnapshot())
	assert.NotContains(t, prepared.Info().Session.AttributesSnapshot(), "mutated")
	_, found := rt.sessionDrivers.Lookup(root.ID)
	assert.False(t, found)
	assert.Empty(t, rt.subagents.tree.Snapshot().Nodes)
	prepared.Abort()
	prepared.Abort()
	_, err = prepared.Commit(t.Context())
	require.ErrorIs(t, err, context.Canceled)
	func() {
		rt.sessionDrivers.mu.Lock()
		defer rt.sessionDrivers.mu.Unlock()
		assert.Empty(t, rt.sessionDrivers.reservations)
	}()
}

func TestSessionViewCommitDormantResumeFIFOAndIdempotence(t *testing.T) {
	rt, owner, _, root := viewRootFixture(t)
	prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), root.ID)
	require.NoError(t, err)
	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	prepared.Abort()
	again, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	assert.Same(t, committed.SessionHandle, again.SessionHandle)
	handle := committed.SessionHandle
	status, err := handle.Status(t.Context())
	require.NoError(t, err)
	assert.True(t, status.Dormant)
	assert.Equal(t, 1, status.Pending)
	assert.Equal(t, root.WorkingDir, committed.Info.WorkingDir)
	for range 3 {
		rt.sessionDrivers.signalWork()
	}
	driver := handle.(*sessionHandle).driver
	assert.False(t, driver.WakePending())
	assert.Empty(t, canonicalSettlements(canonicalReplay(driver)))
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = handle.Edit(canceled, SessionEdit{Kind: SessionEditResume})
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, driver.Status().Dormant)
	_, err = handle.Edit(t.Context(), SessionEdit{Kind: SessionEditResume})
	require.NoError(t, err)
	coordinationAwait(t, handle, "accepted-archived")
	assert.False(t, driver.Status().Dormant)
	_, err = handle.Edit(t.Context(), SessionEdit{Kind: SessionEditResume})
	require.NoError(t, err)
	var transitions int
	for _, envelope := range canonicalReplay(driver) {
		if event, ok := envelope.Event.(*DormancyChangedEvent); ok {
			transitions++
			assert.False(t, event.Dormant)
		}
	}
	assert.Equal(t, 1, transitions)
	assert.Len(t, canonicalSettlements(canonicalReplay(driver)), 1)
}

func TestSessionViewDeleteRejectsAndCapacityChangePreservesReservation(t *testing.T) {
	for _, deletion := range []bool{false, true} {
		t.Run(map[bool]string{false: "capacity", true: "delete"}[deletion], func(t *testing.T) {
			rt, owner, store, root := viewRootFixture(t)
			before, err := store.GetSession(t.Context(), root.ID)
			require.NoError(t, err)
			prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), root.ID)
			require.NoError(t, err)
			if deletion {
				require.NoError(t, owner.Runtime().DeleteSession(t.Context(), root.ID))
			} else {
				rt.maxSessions = 0
			}
			_, err = prepared.Commit(t.Context())
			_, found := rt.sessionDrivers.Lookup(root.ID)
			if deletion {
				require.Error(t, err)
				assert.False(t, found)
				assert.Empty(t, rt.subagents.tree.Snapshot().Nodes)
			} else {
				require.NoError(t, err)
				assert.True(t, found)
				driver, _ := rt.sessionDrivers.Lookup(root.ID)
				assert.True(t, driver.Status().Dormant)
				stored, loadErr := store.GetSession(t.Context(), root.ID)
				require.NoError(t, loadErr)
				assert.Equal(t, before.OwnSnapshot(), stored.OwnSnapshot())
			}
			func() {
				rt.sessionDrivers.mu.Lock()
				defer rt.sessionDrivers.mu.Unlock()
				assert.Empty(t, rt.sessionDrivers.reservations)
			}()
			if deletion {
				_, err := store.GetSession(t.Context(), root.ID)
				require.ErrorIs(t, err, session.ErrNotFound)
			}
		})
	}
}

func TestSessionViewLegacyNestedBatchDormancyAndResumeAddressedOnly(t *testing.T) {
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	child := session.New(session.WithID("child"), session.WithParentID(root.ID), session.WithAttributes(map[string]string{SessionAgentAttribute: "worker", SessionParentAgentAttribute: "root"}))
	input := session.UserMessage("work")
	input.Pending, input.Accepted, input.TurnID = true, true, "child-pending"
	child.AddMessage(input)
	require.NoError(t, store.AddSession(t.Context(), root))
	require.NoError(t, store.AddSession(t.Context(), child))
	rt, owner := coordinationRuntime(t, store, coordinationReply("received"), coordinationReply("done"))
	rootNode := subagent.SessionRootID(root.ID)
	tree := subagent.Snapshot{Version: subagent.SnapshotVersion, Root: rootNode, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: rootNode, Agent: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-node", SessionID: child.ID, Parent: rootNode, Agent: "worker", State: subagent.NodeIdle}}}}}}
	require.NoError(t, rt.subagentStore.SaveTree(t.Context(), root.ID, tree))
	prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), child.ID)
	require.NoError(t, err)
	info := prepared.Info()
	require.NotNil(t, info.Attach)
	info.Attach.Session.SetTitle("mutated")
	assert.NotEqual(t, "mutated", prepared.Info().Attach.Session.TitleSnapshot())
	before, err := store.(session.CoordinationStore).LoadChildren(t.Context(), root.ID)
	require.NoError(t, err)
	assert.Empty(t, before, "prepare never migrates legacy records")
	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	parent, err := owner.Runtime().SessionByID(root.ID)
	require.NoError(t, err)
	assert.True(t, parent.(*sessionHandle).driver.Status().Dormant)
	assert.True(t, committed.SessionHandle.(*sessionHandle).driver.Status().Dormant)
	_, err = committed.SessionHandle.Edit(t.Context(), SessionEdit{Kind: SessionEditResume})
	require.NoError(t, err)
	coordinationAwait(t, committed.SessionHandle, "child-pending")
	assert.True(t, parent.(*sessionHandle).driver.Status().Dormant)
	reports, err := store.(session.CoordinationStore).PendingReports(t.Context(), root.ID)
	require.NoError(t, err)
	require.Len(t, reports, 1, "dormant parent leaves completion report unacknowledged")
	_, err = parent.Edit(t.Context(), SessionEdit{Kind: SessionEditResume})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		remaining, err := store.(session.CoordinationStore).PendingReports(t.Context(), root.ID)
		return err == nil && len(remaining) == 0
	}, time.Second, time.Millisecond)
	coordinationAwait(t, parent, "report:"+reports[0].ID)
}

func TestSessionViewConcurrentCommittedOwnerReuse(t *testing.T) {
	_, owner, _, root := viewRootFixture(t)
	first, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), root.ID)
	require.NoError(t, err)
	committed, err := first.Commit(t.Context())
	require.NoError(t, err)
	var group sync.WaitGroup
	results := make(chan CommittedSessionView, 8)
	errors := make(chan error, 8)
	for range 8 {
		group.Go(func() {
			prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), root.ID)
			if err != nil {
				errors <- err
				return
			}
			defer prepared.Abort()
			result, err := prepared.Commit(t.Context())
			errors <- err
			if err == nil {
				results <- result
			}
		})
	}
	group.Wait()
	close(results)
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	var count int
	for result := range results {
		count++
		assert.Same(t, committed.SessionHandle.(*sessionHandle).driver, result.SessionHandle.(*sessionHandle).driver)
	}
	assert.Equal(t, 8, count)
}

func TestSessionViewCanceledPublishedResidencyIsBoundedWithoutEviction(t *testing.T) {
	store := session.NewInMemorySessionStore()
	rt, owner := coordinationRuntime(t, store, coordinationReply("done"), coordinationReply("child"))
	rt.maxSessions = 2
	var committed []SessionHandle
	for i, id := range []string{"one", "two", "three"} {
		sess := session.New(session.WithID(id), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
		require.NoError(t, store.AddSession(t.Context(), sess))
		prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), id)
		if i == 2 {
			require.ErrorIs(t, err, ErrSessionCapacity)
			require.Nil(t, prepared)
			_, visible := rt.sessionDrivers.Lookup(id)
			assert.False(t, visible)
			stored, loadErr := store.GetSession(t.Context(), id)
			require.NoError(t, loadErr)
			assert.Equal(t, sess.OwnSnapshot(), stored.OwnSnapshot())
			continue
		}
		require.NoError(t, err)
		result, err := prepared.Commit(t.Context())
		prepared.Abort()
		require.NoError(t, err)
		committed = append(committed, result.SessionHandle)
	}
	for _, handle := range committed {
		canonical, err := owner.Runtime().SessionByID(handle.ID())
		require.NoError(t, err)
		assert.Same(t, handle.(*sessionHandle).driver, canonical.(*sessionHandle).driver)
		assert.True(t, handle.(*sessionHandle).driver.Status().Dormant)
	}
	func() {
		rt.sessionDrivers.mu.Lock()
		defer rt.sessionDrivers.mu.Unlock()
		assert.Len(t, rt.sessionDrivers.drivers, 2)
		assert.Empty(t, rt.sessionDrivers.reservations)
	}()
}

func TestSessionViewCanceledPreparationSelfRetiresReservations(t *testing.T) {
	rt, owner, _, root := viewRootFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	_, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(ctx, root.ID)
	require.NoError(t, err)
	cancel()
	require.Eventually(t, func() bool {
		rt.sessionDrivers.mu.Lock()
		defer rt.sessionDrivers.mu.Unlock()
		return len(rt.sessionDrivers.reservations) == 0
	}, time.Second, time.Millisecond)
	_, found := rt.sessionDrivers.Lookup(root.ID)
	assert.False(t, found)
}

func TestSessionViewShutdownDoesNotActivateDormantAcceptedWork(t *testing.T) {
	rt, owner, _, root := viewRootFixture(t)
	prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), root.ID)
	require.NoError(t, err)
	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	driver := committed.SessionHandle.(*sessionHandle).driver
	require.NoError(t, owner.Shutdown(t.Context()))
	assert.Empty(t, canonicalSettlements(canonicalReplay(driver)))
	func() {
		rt.sessionDrivers.mu.Lock()
		defer rt.sessionDrivers.mu.Unlock()
		assert.Empty(t, rt.sessionDrivers.drivers)
	}()
}

type blockedViewBatchStore struct {
	session.Store
	session.CoordinationStore
	session.ChildAdmissionBatchStore

	entered        chan struct{}
	release        chan struct{}
	batchErr       func() error
	afterAdmission bool
}

func (s *blockedViewBatchStore) AdmitChildren(ctx context.Context, admissions []session.ChildAdmission) error {
	s.batchErr = ctx.Err
	if s.afterAdmission {
		if err := s.ChildAdmissionBatchStore.AdmitChildren(ctx, admissions); err != nil {
			return err
		}
	}
	close(s.entered)
	<-s.release
	if s.afterAdmission {
		return nil
	}
	return s.ChildAdmissionBatchStore.AdmitChildren(ctx, admissions)
}

func TestSessionViewAbortDoesNotWaitForCommitStorage(t *testing.T) {
	base := session.NewInMemorySessionStore()
	store := &blockedViewBatchStore{Store: base, CoordinationStore: base.(session.CoordinationStore), ChildAdmissionBatchStore: base.(session.ChildAdmissionBatchStore), entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-store.release:
		default:
			close(store.release)
		}
	}()
	root := session.New(session.WithID("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	child := session.New(session.WithID("child"), session.WithParentID(root.ID), session.WithAttributes(map[string]string{SessionAgentAttribute: "worker", SessionParentAgentAttribute: "root"}))
	require.NoError(t, store.AddSession(t.Context(), root))
	require.NoError(t, store.AddSession(t.Context(), child))
	rt, owner := coordinationRuntime(t, store, coordinationReply("root"), coordinationReply("child"))
	node := subagent.SessionRootID(root.ID)
	require.NoError(t, rt.subagentStore.SaveTree(t.Context(), root.ID, subagent.Snapshot{Version: subagent.SnapshotVersion, Root: node, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: node, Agent: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-node", Parent: node, Agent: "worker", SessionID: child.ID, State: subagent.NodeIdle}}}}}}))
	prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), child.ID)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { _, err := prepared.Commit(t.Context()); done <- err }()
	coordinationWait(t, store.entered)
	aborted := make(chan struct{})
	go func() { prepared.Abort(); close(aborted) }()
	select {
	case <-aborted:
	case <-time.After(time.Second):
		t.Fatal("Abort blocked behind storage commit")
	}
	require.ErrorIs(t, store.batchErr(), context.Canceled, "Abort synchronously cancels the batch before it returns")
	close(store.release)
	// Cancellation before storage linearization must discard reservations,
	// while the batch's own commit context decides whether bytes committed.
	require.ErrorIs(t, <-done, context.Canceled)
	records, err := store.LoadChildren(t.Context(), root.ID)
	require.NoError(t, err)
	assert.Empty(t, records, "abort before admission must not migrate children")
	assert.Empty(t, rt.subagents.tree.Snapshot().Nodes)
	_, found := rt.sessionDrivers.Lookup(child.ID)
	assert.False(t, found)
	_, found = rt.sessionDrivers.Lookup(root.ID)
	assert.False(t, found)
	func() {
		rt.sessionDrivers.mu.Lock()
		defer rt.sessionDrivers.mu.Unlock()
		assert.Empty(t, rt.sessionDrivers.reservations)
	}()
}

func TestSessionViewAbortAfterAdmissionPublishesDormantOwners(t *testing.T) {
	base := session.NewInMemorySessionStore()
	store := &blockedViewBatchStore{Store: base, CoordinationStore: base.(session.CoordinationStore), ChildAdmissionBatchStore: base.(session.ChildAdmissionBatchStore), entered: make(chan struct{}), release: make(chan struct{}), afterAdmission: true}
	defer func() {
		select {
		case <-store.release:
		default:
			close(store.release)
		}
	}()
	root := session.New(session.WithID("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	child := session.New(session.WithID("child"), session.WithParentID(root.ID), session.WithAttributes(map[string]string{SessionAgentAttribute: "worker", SessionParentAgentAttribute: "root"}))
	pending := session.UserMessage("archived FIFO")
	pending.TurnID, pending.Pending, pending.Accepted = "accepted-child", true, true
	pending.InputOrigin = session.InputOriginUser
	child.AddMessage(pending)
	require.NoError(t, store.AddSession(t.Context(), root))
	require.NoError(t, store.AddSession(t.Context(), child))
	before, err := store.GetSession(t.Context(), child.ID)
	require.NoError(t, err)
	rt, owner := coordinationRuntime(t, store, coordinationReply("root"), coordinationReply("child"))
	node := subagent.SessionRootID(root.ID)
	require.NoError(t, rt.subagentStore.SaveTree(t.Context(), root.ID, subagent.Snapshot{Version: subagent.SnapshotVersion, Root: node, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: node, Agent: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-node", Parent: node, Agent: "worker", SessionID: child.ID, State: subagent.NodeIdle}}}}}}))
	prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), child.ID)
	require.NoError(t, err)
	done := make(chan error, 1)
	var committed CommittedSessionView
	go func() {
		var commitErr error
		committed, commitErr = prepared.Commit(t.Context())
		done <- commitErr
	}()
	coordinationWait(t, store.entered)
	records, err := store.LoadChildren(t.Context(), root.ID)
	require.NoError(t, err)
	require.Len(t, records, 1, "the batch has already crossed durable admission")
	aborted := make(chan struct{})
	go func() { prepared.Abort(); close(aborted) }()
	select {
	case <-aborted:
	case <-time.After(time.Second):
		t.Fatal("Abort blocked behind completed admission's response")
	}
	require.ErrorIs(t, store.batchErr(), context.Canceled)
	close(store.release)
	require.NoError(t, <-done, "late cancellation must not revoke successful admission")
	require.NotNil(t, committed.SessionHandle)
	assert.Equal(t, child.ID, committed.SessionHandle.ID())
	for _, id := range []string{root.ID, child.ID} {
		driver, found := rt.sessionDrivers.Lookup(id)
		require.True(t, found)
		assert.True(t, driver.Status().Dormant)
		assert.False(t, driver.WakePending())
		assert.Empty(t, canonicalSettlements(canonicalReplay(driver)))
	}
	after, err := store.GetSession(t.Context(), child.ID)
	require.NoError(t, err)
	assert.Equal(t, before.OwnSnapshot(), after.OwnSnapshot(), "late Abort must not promote or rewrite accepted FIFO")
	again, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	assert.Same(t, committed.SessionHandle, again.SessionHandle)
	func() {
		rt.sessionDrivers.mu.Lock()
		defer rt.sessionDrivers.mu.Unlock()
		assert.Empty(t, rt.sessionDrivers.reservations)
	}()
}
