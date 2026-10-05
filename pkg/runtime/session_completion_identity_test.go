package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/subagent"
)

func TestCompletionIdentityStableAcrossRepeatedSettlement(t *testing.T) {
	var parentCalls atomic.Int32
	provider := coordinationReply("received")
	provider.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
		parentCalls.Add(1)
		return newStreamBuilder().AddContent("received").AddStopWithUsage(1, 1).Build(), nil
	}
	store := coordinationSQLite(t)
	rt, owner := coordinationRuntime(t, store, provider, coordinationReply("identical result"))
	parent := coordinationCreate(t, owner.Runtime(), "root", "")
	child := coordinationCreate(t, owner.Runtime(), "child", parent.ID())
	d, ok := rt.sessionDrivers.Lookup(child.ID())
	require.True(t, ok)
	for i, turn := range []string{"first", "second"} {
		submission, err := child.Submit(t.Context(), TurnInput{Content: "identical input", RequestID: turn})
		require.NoError(t, err)
		coordinationAwait(t, child, submission.TurnID)
		require.Eventually(t, func() bool { return parentCalls.Load() == int32(i+1) }, completionIdentityTimeout, completionIdentityPoll)
		coordinationSettled(t, parent)
		d.mu.Lock()
		generation := d.generation
		d.mu.Unlock()
		for range 3 {
			_, _, again := d.finishRun(generation, "")
			assert.False(t, again)
			require.NoError(t, rt.subagents.completeSessionTurn(d, submission.TurnID, ""))
		}
		records, err := store.(session.CoordinationStore).LoadChildren(t.Context(), parent.ID())
		require.NoError(t, err)
		require.Len(t, records, 1)
		assert.Equal(t, uint64(i+2), records[0].Revision)
		assert.Equal(t, submission.TurnID, records[0].LastTurnID)
		assert.Equal(t, int32(i+1), parentCalls.Load())
	}
	snapshot, err := parent.Snapshot(t.Context())
	require.NoError(t, err)
	var reports int
	for _, item := range snapshot.MessagesSnapshot() {
		if item.Message != nil && item.Message.InputOrigin == session.InputOriginRuntime {
			reports++
			assert.Equal(t, "steer", item.Message.InputMode)
		}
	}
	assert.Equal(t, 2, reports, "equal content from distinct accepted turns remains distinct")
}

const completionIdentityTimeout = 5 * time.Second

const completionIdentityPoll = time.Millisecond

type blockedCompletionStore struct {
	session.Store
	session.CoordinationStore

	entered chan struct{}
	release chan struct{}
	commits atomic.Int32
}

func (s *blockedCompletionStore) CommitChild(ctx context.Context, commit session.ChildCommit) error {
	s.commits.Add(1)
	close(s.entered)
	<-s.release
	return s.CoordinationStore.CommitChild(ctx, commit)
}

func TestCompletionIdentitySingleFlight(t *testing.T) {
	base := session.NewInMemorySessionStore()
	store := &blockedCompletionStore{Store: base, CoordinationStore: base.(session.CoordinationStore), entered: make(chan struct{}), release: make(chan struct{})}
	rt, owner := coordinationRuntime(t, store, coordinationReply("received"), coordinationReply("done"))
	parent := coordinationCreate(t, owner.Runtime(), "root", "")
	child := coordinationCreate(t, owner.Runtime(), "child", parent.ID())
	submission, err := child.Submit(t.Context(), TurnInput{Content: "work"})
	require.NoError(t, err)
	coordinationWait(t, store.entered)
	d, ok := rt.sessionDrivers.Lookup(child.ID())
	require.True(t, ok)
	d.mu.Lock()
	generation := d.generation
	d.mu.Unlock()
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() { d.finishRun(generation, "") })
	}
	group.Wait()
	assert.Equal(t, int32(1), store.commits.Load())
	close(store.release)
	coordinationAwait(t, child, submission.TurnID)
}

func TestCompletionIdentityEmptyGenerationDoesNotReuseOldAnswer(t *testing.T) {
	var calls atomic.Int32
	worker := coordinationReply("unused")
	worker.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
		if calls.Add(1) == 1 {
			return newStreamBuilder().AddContent("old answer").AddStopWithUsage(1, 1).Build(), nil
		}
		return newStreamBuilder().AddStopWithUsage(1, 0).Build(), nil
	}
	store := coordinationSQLite(t)
	_, owner := coordinationRuntime(t, store, coordinationReply("received"), worker)
	parent := coordinationCreate(t, owner.Runtime(), "root", "")
	child := coordinationCreate(t, owner.Runtime(), "child", parent.ID())
	for _, id := range []string{"first", "empty"} {
		submission, err := child.Submit(t.Context(), TurnInput{Content: id})
		require.NoError(t, err)
		coordinationAwait(t, child, submission.TurnID)
	}
	records, err := store.(session.CoordinationStore).LoadChildren(t.Context(), parent.ID())
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Empty(t, records[0].Result)
}

func TestCompletionIdentityLegacyAcceptanceReplayAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	store, err := sqlitestore.New(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	coord := store.(session.CoordinationStore)
	parent := session.New(session.WithID("root"))
	require.NoError(t, store.AddSession(t.Context(), parent))
	child := session.New(session.WithID("child"))
	record := session.ChildRecord{RootSessionID: parent.ID, ParentSessionID: parent.ID, Node: subagent.Node{ID: "legacy-node", SessionID: child.ID}}
	require.NoError(t, coord.AdmitChild(t.Context(), session.ChildAdmission{Child: child, Record: record}))
	record.Revision = 1
	report := session.ChildReport{ID: "legacy-node:2", ParentSessionID: parent.ID, ChildSessionID: child.ID, TurnID: "legacy-turn", Content: "original report"}
	require.NoError(t, coord.CommitChild(t.Context(), session.ChildCommit{ExpectedRevision: 1, Record: record, Reports: []session.ChildReport{report}}))
	message := session.UserMessage(report.Content)
	message.Pending, message.Accepted, message.TurnID = true, true, "report:"+report.ID
	message.InputOrigin, message.InputMode = session.InputOriginRuntime, "steer"
	accepted, err := coord.AcceptReport(t.Context(), parent.ID, report.ID, message)
	require.NoError(t, err)
	require.True(t, accepted.Created)
	require.NoError(t, store.Close())
	store, err = sqlitestore.New(t.Context(), path)
	require.NoError(t, err)
	coord = store.(session.CoordinationStore)

	r := newDriverTestRuntime(t)
	r.sessionStore = store
	r.subagents = &subagentManager{r: r, coord: coord}
	d := r.sessionDrivers.Get(parent)
	d.phase = sessionRunning
	require.NoError(t, d.acceptReport(t.Context(), report))
	require.NoError(t, d.acceptReport(t.Context(), report))
	require.Len(t, d.pending, 1)
	assert.Equal(t, message.TurnID, d.pending[0].RequestID)
	assert.Len(t, d.DrainRuntimeNotes(), 1)
	// Reconstructed driver with no local transcript must consult persisted state,
	// never recreate an already-consumed acceptance from the stale report body.
	restored := newSessionDriver(r, session.New(session.WithID(parent.ID)))
	restored.phase = sessionRunning
	r.maxPendingMailbox = 1
	restored.pending = []QueuedMessage{{RequestID: "unrelated"}}
	require.NoError(t, restored.acceptReport(t.Context(), report))
	assert.Len(t, restored.pending, 1)
	assert.Empty(t, restored.sess.MessagesSnapshot())
	probe, err := coord.AcceptReport(t.Context(), parent.ID, report.ID, nil)
	require.NoError(t, err)
	assert.Equal(t, accepted.MessageID, probe.MessageID)
	require.NotNil(t, probe.Message)
	assert.False(t, probe.Message.Pending)
}

func TestCompletionIdentityStableEncoding(t *testing.T) {
	assert.Equal(t, "turn:Y2hpbGQ:dHVybg", childReportID("child", "turn"))
	assert.NotEqual(t, childReportID("child", "turn"), childReportID("child", "turn-two"))
	assert.NotEqual(t, childReportID("child", "turn"), childReportID("child-two", "turn"))
	assert.NotEqual(t, childReportID("a:b", "c"), childReportID("a", "b:c"))
}

type uncertainCompletionStore struct {
	session.Store
	session.CoordinationStore

	attempts atomic.Int32
	entered  chan struct{}
	release  chan struct{}
}

func (s *uncertainCompletionStore) CommitChild(ctx context.Context, commit session.ChildCommit) error {
	attempt := s.attempts.Add(1)
	if attempt == 2 {
		close(s.entered)
		<-s.release
	}
	err := s.CoordinationStore.CommitChild(ctx, commit)
	if attempt == 1 && err == nil {
		return errors.New("commit acknowledgement lost")
	}
	return err
}

func TestCompletionIdentityUncertainCommitReconcilesSameReport(t *testing.T) {
	base := coordinationSQLite(t)
	store := &uncertainCompletionStore{
		Store: base, CoordinationStore: base.(session.CoordinationStore),
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	defer func() {
		select {
		case <-store.release:
		default:
			close(store.release)
		}
	}()
	var parentCalls atomic.Int32
	provider := coordinationReply("received")
	provider.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
		parentCalls.Add(1)
		return newStreamBuilder().AddContent("received").AddStopWithUsage(1, 1).Build(), nil
	}
	_, owner := coordinationRuntime(t, store, provider, coordinationReply("done"))
	parent := coordinationCreate(t, owner.Runtime(), "root", "")
	child := coordinationCreate(t, owner.Runtime(), "child", parent.ID())
	submission, err := child.Submit(t.Context(), TurnInput{Content: "work"})
	require.NoError(t, err)
	coordinationWait(t, store.entered)
	d := child.(*sessionHandle).driver
	// The blocked retry already owns driver work; join it through reconciliation.
	retryDone := d.Done()
	require.EqualError(t, child.AwaitTurn(t.Context(), submission.TurnID), "commit acknowledgement lost")
	close(store.release)
	coordinationWait(t, retryDone)
	require.True(t, d.Settled())
	coordinationAwait(t, child, submission.TurnID)
	assert.Equal(t, int32(2), store.attempts.Load())
	records, err := store.LoadChildren(t.Context(), parent.ID())
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, uint64(2), records[0].Revision)
	assert.Equal(t, submission.TurnID, records[0].LastTurnID)
	require.Eventually(t, func() bool {
		snapshot, err := parent.Snapshot(t.Context())
		if err != nil {
			return false
		}
		count := 0
		for _, item := range snapshot.MessagesSnapshot() {
			if item.Message != nil && item.Message.TurnID == "report:"+childReportID(child.ID(), submission.TurnID) {
				count++
			}
		}
		return count == 1
	}, completionIdentityTimeout, completionIdentityPoll)
	coordinationAwait(t, parent, "report:"+childReportID(child.ID(), submission.TurnID))
	assert.Equal(t, int32(1), parentCalls.Load(), "uncertain commit reconciliation activates the parent once")
}

func TestCompletionIdentitySaturationRetainsUnacknowledgedReport(t *testing.T) {
	m := newTestSubagentManager(t)
	parent := session.New(session.WithID("root"))
	child := session.New(session.WithID("child"))
	m.registerChild(parent, "root", "worker", "worker", child)
	d := m.r.sessionDrivers.Get(parent)
	m.r.maxPendingMailbox = 1
	d.phase = sessionRunning
	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "older user", RequestID: "user", InputOrigin: session.InputOriginUser}, false))
	m.children["worker"].durable.Result = "done"
	m.reportTurn(t, "worker", subagent.NodeIdle, "")
	reports, err := m.coordination().PendingReports(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	assert.Equal(t, "user", d.pending[0].RequestID)
	require.NoError(t, d.promoteInput(t.Context(), "user", func(QueuedMessage) { d.pending = nil }))
	require.NoError(t, d.acceptReport(t.Context(), reports[0]))
	require.Len(t, d.pending, 1)
	require.NoError(t, d.acceptReport(t.Context(), reports[0]), "acknowledged replay bypasses full mailbox")
	assert.Len(t, d.pending, 1)
	assert.Len(t, d.DrainRuntimeNotes(), 1)
	require.NoError(t, d.acceptReport(t.Context(), reports[0]))
	assert.Empty(t, d.pending)
}

type uncertainReportAcceptanceStore struct {
	session.Store
	session.CoordinationStore

	failed atomic.Bool
}

func (s *uncertainReportAcceptanceStore) AcceptReport(ctx context.Context, parent, id string, message *session.Message) (session.ReportAcceptance, error) {
	accepted, err := s.CoordinationStore.AcceptReport(ctx, parent, id, message)
	if err == nil && accepted.Created && s.failed.CompareAndSwap(false, true) {
		return session.ReportAcceptance{}, errors.New("acceptance acknowledgement lost")
	}
	return accepted, err
}

func TestCompletionIdentityUncertainAcceptanceRetainsRetryIdentity(t *testing.T) {
	m := newTestSubagentManager(t)
	parent := session.New(session.WithID("root"))
	child := session.New(session.WithID("child"))
	m.registerChild(parent, "root", "worker", "worker", child)
	m.r.sessionStore = session.NewInMemorySessionStore()
	store := &uncertainReportAcceptanceStore{CoordinationStore: m.r.sessionStore.(session.CoordinationStore)}
	m.coord = store
	m.children["worker"].durable.Result = "done"
	m.reportTurn(t, "worker", subagent.NodeIdle, "")
	d := m.r.sessionDrivers.Get(parent)
	require.NotNil(t, d.reportRetry)
	assert.Empty(t, d.pending)
	reports, err := store.PendingReports(t.Context(), parent.ID)
	require.NoError(t, err)
	assert.Empty(t, reports, "outbox was acknowledged before the transport error")
	assert.False(t, m.r.sessionDrivers.deliverReports(d))
	assert.Nil(t, d.reportRetry)
	require.Len(t, d.pending, 1)
	assert.False(t, m.r.sessionDrivers.deliverReports(d))
	assert.Len(t, d.pending, 1)
}

func TestCompletionIdentityUncertainAcceptanceAutomaticallyRecovers(t *testing.T) {
	base := coordinationSQLite(t)
	store := &uncertainReportAcceptanceStore{Store: base, CoordinationStore: base.(session.CoordinationStore)}
	var parentCalls atomic.Int32
	provider := coordinationReply("received")
	provider.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
		parentCalls.Add(1)
		return newStreamBuilder().AddContent("received").AddStopWithUsage(1, 1).Build(), nil
	}
	_, owner := coordinationRuntime(t, store, provider, coordinationReply("done"))
	parent := coordinationCreate(t, owner.Runtime(), "root", "")
	child := coordinationCreate(t, owner.Runtime(), "child", parent.ID())
	for i := range 2 {
		turn, err := child.Submit(t.Context(), TurnInput{Content: "same child result"})
		require.NoError(t, err)
		coordinationAwait(t, child, turn.TurnID)
		require.Eventually(t, func() bool { return parentCalls.Load() == int32(i+1) }, completionIdentityTimeout, completionIdentityPoll)
		coordinationSettled(t, parent)
	}
	assert.True(t, store.failed.Load())
	assert.Equal(t, int32(2), parentCalls.Load(), "uncertain acceptance and later report each activate once")
}

func TestCompletionIdentityAncestorWakeDoesNotRepeatLeafReport(t *testing.T) {
	m := newTestSubagentManager(t)
	root := session.New(session.WithID("root"))
	parent := session.New(session.WithID("parent"), session.WithParentID(root.ID))
	leaf := session.New(session.WithID("leaf"))
	m.registerChild(root, "root", "parent-node", "parent", parent)
	m.registerChild(parent, "parent", "leaf-node", "leaf", leaf)
	m.children["leaf-node"].durable.Result = "leaf answer"
	m.reportTurn(t, "leaf-node", subagent.NodeIdle, "")
	leafDriver := m.r.sessionDrivers.Get(leaf)
	leafDriver.mu.Lock()
	leafDriver.leave(sessionRunning)
	leafDriver.mu.Unlock()
	parentDriver := m.r.sessionDrivers.Get(parent)
	guidance := parentDriver.DrainRuntimeNotes()
	require.Len(t, guidance, 1)
	m.children["parent-node"].durable.Result = "parent answer"
	m.reportTurn(t, "parent-node", subagent.NodeIdle, "")
	rootDriver := m.r.sessionDrivers.Get(root)
	require.Len(t, rootDriver.pending, 1)
	leafTurn := m.children["leaf-node"].durable.LastTurnID
	require.NoError(t, m.completeSessionTurn(leafDriver, leafTurn, ""))
	assert.False(t, m.r.sessionDrivers.deliverReports(parentDriver))
	assert.Empty(t, parentDriver.pending)
	assert.Len(t, rootDriver.pending, 1)
	assert.Equal(t, uint64(2), m.children["leaf-node"].durable.Revision)
}
