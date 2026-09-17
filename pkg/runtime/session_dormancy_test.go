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

func TestSessionDormancyExplicitSubmitPreservesArchivedFIFO(t *testing.T) {
	_, owner, _, root := viewRootFixture(t)
	prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), root.ID)
	require.NoError(t, err)
	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	accepted, err := committed.SessionHandle.Submit(t.Context(), TurnInput{Content: "new instruction", RequestID: "new"})
	require.NoError(t, err)
	coordinationAwait(t, committed.SessionHandle, "accepted-archived")
	coordinationAwait(t, committed.SessionHandle, accepted.TurnID)
	driver := committed.SessionHandle.(*sessionHandle).driver
	assert.False(t, driver.Status().Dormant)
	events := canonicalSettlements(canonicalReplay(driver))
	require.Len(t, events, 2)
	assert.Equal(t, "accepted-archived", events[0].RequestID)
	assert.Equal(t, accepted.TurnID, events[1].RequestID)
}

func TestSessionDormancyFailedAdmissionAndReplayDoNotAuthorize(t *testing.T) {
	rt, owner, _, root := viewRootFixture(t)
	prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), root.ID)
	require.NoError(t, err)
	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	driver := committed.SessionHandle.(*sessionHandle).driver
	rt.maxPendingMailbox = 1
	_, err = committed.SessionHandle.Submit(t.Context(), TurnInput{Content: "too much"})
	require.Error(t, err)
	assert.True(t, driver.Status().Dormant)
	_, err = committed.SessionHandle.Retry(t.Context())
	require.Error(t, err)
	assert.True(t, driver.Status().Dormant)
	message := driver.pending[0]
	queued, err := driver.post(t.Context(), message, true)
	require.NoError(t, err)
	assert.True(t, queued)
	assert.True(t, driver.Status().Dormant, "replayed admission does not authorize execution")
}

func TestSessionDormancyAutomaticInputAndEmptyResume(t *testing.T) {
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	require.NoError(t, store.AddSession(t.Context(), root))
	rt, owner := coordinationRuntime(t, store, coordinationReply("done"), coordinationReply("child"))
	prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), root.ID)
	require.NoError(t, err)
	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	handle := committed.SessionHandle
	_, err = handle.Edit(t.Context(), SessionEdit{Kind: SessionEditResume})
	require.NoError(t, err)
	assert.Empty(t, canonicalSettlements(canonicalReplay(handle.(*sessionHandle).driver)), "empty resume creates no turn")
	// A separate restored owner stays dormant even when trusted runtime input
	// wakes the normal scheduler for another session.
	other := session.New(session.WithID("other"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	require.NoError(t, store.AddSession(t.Context(), other))
	prepared, err = owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), other.ID)
	require.NoError(t, err)
	second, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	driver := second.SessionHandle.(*sessionHandle).driver
	require.True(t, driver.PostReliable(t.Context(), QueuedMessage{Content: "automatic", RequestID: "automatic"}))
	rt.sessionDrivers.signalWork()
	assert.True(t, driver.Status().Dormant)
	assert.Equal(t, 1, driver.Status().Pending)
	assert.Empty(t, canonicalSettlements(canonicalReplay(driver)))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, second.SessionHandle.AwaitTurn(ctx, "automatic"), context.Canceled)
}

func TestSessionDormancyProtectsEmptyOwnerFromPruneAndCapacityEviction(t *testing.T) {
	store := session.NewInMemorySessionStore()
	archived := session.New(session.WithID("archived"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	require.NoError(t, store.AddSession(t.Context(), archived))
	rt, owner := coordinationRuntime(t, store, coordinationReply("done"), coordinationReply("child"))
	rt.maxSessions = 1
	prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), archived.ID)
	require.NoError(t, err)
	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	driver := committed.SessionHandle.(*sessionHandle).driver
	driver.mu.Lock()
	driver.lastActive = time.Time{}
	driver.mu.Unlock()
	rt.idleRetention = time.Nanosecond
	func() {
		rt.sessionDrivers.mu.Lock()
		defer rt.sessionDrivers.mu.Unlock()
		rt.sessionDrivers.pruneIdleLocked(time.Now())
		assert.False(t, rt.sessionDrivers.evictSettledForCapacityLocked())
		assert.Same(t, driver, rt.sessionDrivers.drivers[archived.ID])
	}()
	_, err = owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("pressure")), SessionBinding{AgentName: "root"})
	require.ErrorIs(t, err, ErrSessionCapacity)
	assert.True(t, driver.Status().Dormant)
	_, err = committed.SessionHandle.Edit(t.Context(), SessionEdit{Kind: SessionEditResume})
	require.NoError(t, err)
	func() {
		rt.sessionDrivers.mu.Lock()
		defer rt.sessionDrivers.mu.Unlock()
		assert.True(t, rt.sessionDrivers.evictSettledForCapacityLocked(), "explicitly authorized empty session reuses ordinary reclaim policy")
	}()
}

func TestSessionDormancyProtectsReportOnlyOwnerAndAncestorPin(t *testing.T) {
	r := newDriverTestRuntime(t)
	r.sessionDrivers.closed = true // isolated admission policy test has no worker
	root := session.New(session.WithID("root"))
	child := session.New(session.WithID("child"), session.WithParentID(root.ID))
	parentDriver := newSessionDriver(r, root)
	childDriver := newSessionDriver(r, child)
	childDriver.viewDormant = true
	r.sessionDrivers.drivers[root.ID] = parentDriver
	r.sessionDrivers.drivers[child.ID] = childDriver
	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), root))
	require.NoError(t, store.AddSession(t.Context(), child))
	grandchild := session.New(session.WithID("grandchild"), session.WithParentID(child.ID))
	record := session.ChildRecord{RootSessionID: root.ID, ParentSessionID: child.ID, Node: subagent.Node{ID: "grandchild-node", SessionID: grandchild.ID, Agent: "worker"}}
	coordination := store.(session.CoordinationStore)
	require.NoError(t, coordination.AdmitChild(t.Context(), session.ChildAdmission{Child: grandchild, Record: record}))
	record.Revision = 1
	report := session.ChildReport{ID: "durable-report", ParentSessionID: child.ID, ChildSessionID: grandchild.ID, TurnID: "done"}
	require.NoError(t, coordination.CommitChild(t.Context(), session.ChildCommit{ExpectedRevision: 1, Record: record, Reports: []session.ChildReport{report}}))
	r.sessionDrivers.mu.Lock()
	assert.True(t, r.sessionDrivers.ancestorResidentLocked(root.ID))
	assert.False(t, r.sessionDrivers.evictSettledForCapacityLocked())
	assert.Len(t, r.sessionDrivers.drivers, 2)
	r.sessionDrivers.mu.Unlock()
	pending, err := coordination.PendingReports(t.Context(), child.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, report.ID, pending[0].ID)
	assert.Zero(t, childDriver.Status().Pending, "report-only owner has no accepted inbox count")
}
