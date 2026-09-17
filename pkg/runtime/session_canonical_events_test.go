package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

func canonicalSettlements(events []SequencedSessionEvent) []SequencedSessionEvent {
	var settled []SequencedSessionEvent
	for _, event := range events {
		if _, ok := event.Event.(*TurnSettledEvent); ok {
			settled = append(settled, event)
		}
	}
	return settled
}

func canonicalReplay(d *sessionDriver) []SequencedSessionEvent {
	zero := uint64(0)
	replay, _, cancel, _ := d.events.SubscribeSequenced(d.sessionID(), &zero, 128)
	cancel()
	return replay
}

func TestCanonicalSettlementOutcomesAndSingleFlight(t *testing.T) {
	for _, tc := range []struct {
		name     string
		runErr   string
		canceled bool
		stopped  bool
		shutdown bool
		outcome  TurnOutcome
	}{
		{name: "completed", outcome: TurnCompleted},
		{name: "failed", runErr: "model failed", outcome: TurnFailed},
		{name: "canceled", runErr: "context canceled", canceled: true, outcome: TurnCanceled},
		{name: "stopped", stopped: true, outcome: TurnCanceled},
		{name: "shutdown", shutdown: true, outcome: TurnCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newDriverTestRuntime(t)
			// These isolated drivers have no scheduled work: exercise the settlement
			// barrier directly without creating a background scheduler.
			r.sessionDrivers.closed = true
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r.lifecycleCtx = ctx
			if tc.shutdown {
				cancel()
			}
			d := newSessionDriver(r, session.New(session.WithID("owner")))
			d.generation, d.activeRequestID = 1, "accepted"
			d.phase = sessionRunning
			if tc.canceled {
				d.phase = sessionCancelling
			}
			d.stopped = tc.stopped
			d.events.SetRequest("owner", "accepted", 1)
			d.events.Publish("owner", StreamStopped("owner", "root", ""))
			assert.Empty(t, canonicalSettlements(canonicalReplay(d)), "stream stop is not settlement")
			var group sync.WaitGroup
			for range 8 {
				group.Go(func() { d.finishRun(1, tc.runErr) })
			}
			group.Wait()
			events := canonicalSettlements(canonicalReplay(d))
			require.Len(t, events, 1)
			event := events[0].Event.(*TurnSettledEvent)
			assert.Equal(t, "owner", event.SessionID)
			assert.Equal(t, "accepted", event.TurnID)
			assert.Equal(t, "accepted", events[0].RequestID)
			assert.Equal(t, tc.outcome, event.Outcome)
			assert.Positive(t, events[0].Sequence)
		})
	}
}

func TestCanonicalSettlementPersistenceFailureRetryAndWithdrawal(t *testing.T) {
	r := newDriverTestRuntime(t)
	r.sessionDrivers.closed = true
	d := newSessionDriver(r, session.New(session.WithID("owner")))
	d.generation, d.activeRequestID = 1, "accepted"
	d.phase = sessionRunning
	observer := newPersistenceObserver(session.NewInMemorySessionStore())
	r.observers = []EventObserver{observer}
	failure := errors.New("durable write rejected")
	attempts := 0
	observer.journal("owner").pending = []persistenceEffect{{write: func(context.Context) error {
		attempts++
		if attempts == 1 {
			return failure
		}
		return nil
	}}}
	d.finishRun(1, "")
	require.ErrorIs(t, d.completionErr, failure)
	assert.Empty(t, canonicalSettlements(canonicalReplay(d)))
	d.finishRun(1, "")
	d.finishRun(1, "")
	events := canonicalSettlements(canonicalReplay(d))
	require.Len(t, events, 1)
	assert.Equal(t, TurnCompleted, events[0].Event.(*TurnSettledEvent).Outcome)
	assert.Equal(t, 2, attempts)
	require.NoError(t, d.completionErr)
	d.mu.Lock()
	d.completeTurnLocked("withdrawn")
	d.mu.Unlock()
	assert.Len(t, canonicalSettlements(canonicalReplay(d)), 1, "waiter completion is not business completion")
}

func TestCanonicalSettlementWaitsForChildCommitReconciliation(t *testing.T) {
	base := session.NewInMemorySessionStore()
	store := &uncertainCompletionStore{Store: base, CoordinationStore: base.(session.CoordinationStore), entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-store.release:
		default:
			close(store.release)
		}
	}()
	_, owner := coordinationRuntime(t, store, coordinationReply("received"), coordinationReply("done"))
	parent := coordinationCreate(t, owner.Runtime(), "root", "")
	child := coordinationCreate(t, owner.Runtime(), "child", parent.ID())
	accepted, err := child.Submit(t.Context(), TurnInput{Content: "work", RequestID: "accepted"})
	require.NoError(t, err)
	coordinationWait(t, store.entered)
	d := child.(*sessionHandle).driver
	assert.Empty(t, canonicalSettlements(canonicalReplay(d)), "uncertain durable commit cannot publish settlement")
	require.EqualError(t, child.AwaitTurn(t.Context(), accepted.TurnID), "commit acknowledgement lost")
	close(store.release)
	coordinationWait(t, d.Done())
	coordinationAwait(t, child, accepted.TurnID)
	events := canonicalSettlements(canonicalReplay(d))
	require.Len(t, events, 1)
	assert.Equal(t, accepted.TurnID, events[0].RequestID)
	assert.Equal(t, TurnCompleted, events[0].Event.(*TurnSettledEvent).Outcome)
	assert.EqualValues(t, 2, store.attempts.Load())
	records, err := store.LoadChildren(t.Context(), parent.ID())
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, accepted.TurnID, records[0].LastTurnID)
}

func TestCanonicalSettlementConcurrentOwnersAndDistinctTurns(t *testing.T) {
	_, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), coordinationReply("identical"), coordinationReply("child"))
	first := coordinationCreate(t, owner.Runtime(), "first", "")
	second := coordinationCreate(t, owner.Runtime(), "second", "")
	accepted := make(map[string][]string)
	for i, requestID := range []string{"one", "two"} {
		for _, handle := range []SessionHandle{first, second} {
			submission, err := handle.Submit(t.Context(), TurnInput{Content: "identical", RequestID: requestID})
			require.NoError(t, err)
			require.Equal(t, handle.ID(), submission.SessionID)
			require.NotEmpty(t, submission.TurnID)
			assert.NotEqual(t, requestID, submission.TurnID, "caller request key is not the accepted execution identity")
			accepted[handle.ID()] = append(accepted[handle.ID()], submission.TurnID)
		}
		for _, handle := range []SessionHandle{first, second} {
			coordinationAwait(t, handle, accepted[handle.ID()][i])
		}
	}
	for _, handle := range []SessionHandle{first, second} {
		events := canonicalSettlements(canonicalReplay(handle.(*sessionHandle).driver))
		require.Len(t, events, 2)
		for i, turn := range accepted[handle.ID()] {
			event := events[i].Event.(*TurnSettledEvent)
			assert.Equal(t, handle.ID(), event.SessionID)
			assert.Equal(t, turn, event.TurnID)
		}
		assert.NotEqual(t, accepted[handle.ID()][0], accepted[handle.ID()][1])
		assert.Less(t, events[0].Sequence, events[1].Sequence)
	}
}

func TestCanonicalCreationCommittedIdentityReplayAndRestoreBaseline(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	provider := coordinationReply("done")
	provider.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
		close(entered)
		select {
		case <-release:
			return newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	store := session.NewInMemorySessionStore()
	rt, owner := coordinationRuntime(t, store, provider, coordinationReply("child"))
	parent := coordinationCreate(t, owner.Runtime(), "parent", "")
	accepted, err := parent.Submit(t.Context(), TurnInput{Content: "create", RequestID: "parent-turn"})
	require.NoError(t, err)
	coordinationWait(t, entered)
	obs, err := parent.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer obs.Cancel()
	_, err = owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("proposed")), SessionBinding{AgentName: "missing", ParentSessionID: parent.ID()})
	require.Error(t, err)
	child := coordinationCreate(t, owner.Runtime(), "child", parent.ID())
	var envelope SessionEvent
	select {
	case envelope = <-obs.Events:
	case <-time.After(time.Second):
		t.Fatal("missing creation event")
	}
	created, ok := envelope.Event.(*SubagentCreatedEvent)
	require.True(t, ok)
	assert.Equal(t, parent.ID(), envelope.SessionID)
	assert.Equal(t, accepted.TurnID, envelope.TurnID)
	assert.Greater(t, envelope.Sequence, obs.Primary().Cursor)
	assert.Equal(t, parent.ID(), created.SessionID)
	assert.Equal(t, parent.ID(), created.ParentSessionID)
	assert.Equal(t, child.ID(), created.ChildSessionID)
	records, err := store.(session.CoordinationStore).LoadChildren(t.Context(), parent.ID())
	require.NoError(t, err)
	require.Len(t, records, 1)
	node, ok := rt.subagents.tree.Node(created.NodeID)
	require.True(t, ok)
	require.False(t, created.CreatedAt.IsZero())
	assert.True(t, created.CreatedAt.Equal(records[0].Node.CreatedAt))
	assert.True(t, created.CreatedAt.Equal(node.CreatedAt))
	_, err = owner.Runtime().SessionByID(created.ChildSessionID)
	require.NoError(t, err, "creation is published after registry activation")
	_, err = owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("child")), SessionBinding{AgentName: "worker", ParentSessionID: parent.ID()})
	require.Error(t, err)
	close(release)
	coordinationAwait(t, parent, accepted.TurnID)
	cursor := obs.Primary().Cursor
	replay, err := parent.Observe(t.Context(), ObserveOptions{Since: &cursor})
	require.NoError(t, err)
	defer replay.Cancel()
	var creations []SessionEvent
	for _, event := range replay.Replay {
		if _, ok := event.Event.(*SubagentCreatedEvent); ok {
			creations = append(creations, event)
		}
	}
	require.Len(t, creations, 1)
	assert.Equal(t, envelope, creations[0])
	fresh, err := parent.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	fresh.Cancel()
	assert.Empty(t, fresh.Replay, "fresh attachment is a baseline, not historical creations")
	snapshot := rt.subagents.tree.Snapshot()
	require.NoError(t, owner.Shutdown(t.Context()))
	restoredRuntime, restoredOwner := coordinationRuntime(t, store, coordinationReply("restored"), coordinationReply("child"))
	root, err := store.GetSession(t.Context(), parent.ID())
	require.NoError(t, err)
	rootHandle, err := restoredOwner.Runtime().CreateSession(t.Context(), root, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	_, err = restoredRuntime.subagents.Restore(t.Context(), root, snapshot)
	require.NoError(t, err)
	node, ok = restoredRuntime.subagents.tree.Node(created.NodeID)
	require.True(t, ok)
	assert.True(t, created.CreatedAt.Equal(node.CreatedAt))
	for _, event := range canonicalReplay(rootHandle.(*sessionHandle).driver) {
		_, created := event.Event.(*SubagentCreatedEvent)
		assert.False(t, created, "restore/import cannot mint creation events")
	}
}

func TestCanonicalSettlementRealCancelAndShutdown(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "shutdown"}[shutdown], func(t *testing.T) {
			entered := make(chan struct{})
			provider := coordinationReply("unused")
			provider.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
				close(entered)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			_, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), provider, coordinationReply("child"))
			handle := coordinationCreate(t, owner.Runtime(), "owner", "")
			accepted, err := handle.Submit(t.Context(), TurnInput{Content: "work"})
			require.NoError(t, err)
			coordinationWait(t, entered)
			if shutdown {
				require.NoError(t, owner.Shutdown(t.Context()))
			} else {
				result, err := handle.Cancel(t.Context(), accepted.TurnID)
				require.NoError(t, err)
				assert.Equal(t, CancelAccepted, result.Outcome)
			}
			d := handle.(*sessionHandle).driver
			coordinationWait(t, d.Done())
			events := canonicalSettlements(canonicalReplay(d))
			require.Len(t, events, 1)
			assert.Equal(t, TurnCanceled, events[0].Event.(*TurnSettledEvent).Outcome)
		})
	}
}

type canonicalAdmissionFailureStore struct {
	session.Store
	session.CoordinationStore
}

func (s *canonicalAdmissionFailureStore) AdmitChild(context.Context, session.ChildAdmission) error {
	return errors.New("child admission commit failed")
}

func TestCanonicalCreationRequiresAdmissionCommit(t *testing.T) {
	base := session.NewInMemorySessionStore()
	store := &canonicalAdmissionFailureStore{Store: base, CoordinationStore: base.(session.CoordinationStore)}
	rt, owner := coordinationRuntime(t, store, coordinationReply("parent"), coordinationReply("child"))
	parent := coordinationCreate(t, owner.Runtime(), "parent", "")
	for range 2 {
		_, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("child")), SessionBinding{AgentName: "worker", ParentSessionID: parent.ID()})
		require.EqualError(t, err, "child admission commit failed")
	}
	_, found := rt.sessionDrivers.Lookup("child")
	assert.False(t, found)
	records, err := store.LoadChildren(t.Context(), parent.ID())
	require.NoError(t, err)
	assert.Empty(t, records)
	for _, event := range canonicalReplay(parent.(*sessionHandle).driver) {
		_, created := event.Event.(*SubagentCreatedEvent)
		assert.False(t, created, "failed or proposed admission cannot publish creation")
	}
}

func TestCanonicalSettlementPrecedesQueuedSuccessorPromotion(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls int
	provider := coordinationReply("same answer")
	provider.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
		calls++
		if calls == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return newStreamBuilder().AddContent("same answer").AddStopWithUsage(1, 1).Build(), nil
	}
	_, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), provider, coordinationReply("child"))
	handle := coordinationCreate(t, owner.Runtime(), "owner", "")
	first, err := handle.Submit(t.Context(), TurnInput{Content: "same", RequestID: "one"})
	require.NoError(t, err)
	coordinationWait(t, entered)
	second, err := handle.Submit(t.Context(), TurnInput{Content: "same", RequestID: "two"})
	require.NoError(t, err)
	close(release)
	coordinationAwait(t, handle, first.TurnID)
	coordinationAwait(t, handle, second.TurnID)
	var settled, promoted uint64
	events := canonicalReplay(handle.(*sessionHandle).driver)
	for _, envelope := range events {
		switch event := envelope.Event.(type) {
		case *TurnSettledEvent:
			if event.TurnID == first.TurnID {
				settled = envelope.Sequence
				assert.Equal(t, first.TurnID, envelope.RequestID)
			}
		case *PendingUserMessagePromotedEvent:
			if event.TurnID == second.TurnID {
				promoted = envelope.Sequence
			}
		}
	}
	assert.Positive(t, settled)
	assert.Greater(t, promoted, settled)
	assert.Len(t, canonicalSettlements(events), 2)
}

func TestCanonicalSpawnPublishesSuccessfulCreationOnce(t *testing.T) {
	rt, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), coordinationReply("received"), coordinationReply("done"))
	parent := coordinationCreate(t, owner.Runtime(), "parent", "")
	parentSession := parent.(*sessionHandle).driver.session()
	allowed := rt.allowedFromAgent(rt.resolveSessionAgent(parentSession))
	require.Len(t, allowed, 1)
	nodeID, err := rt.subagents.Spawn(parentSession, "root", allowed[0], "work")
	require.NoError(t, err)
	var creations []*SubagentCreatedEvent
	for _, event := range canonicalReplay(parent.(*sessionHandle).driver) {
		if created, ok := event.Event.(*SubagentCreatedEvent); ok {
			creations = append(creations, created)
		}
	}
	require.Len(t, creations, 1)
	assert.Equal(t, nodeID, creations[0].NodeID)
	assert.Equal(t, parent.ID(), creations[0].ParentSessionID)
	_, err = owner.Runtime().SessionByID(creations[0].ChildSessionID)
	require.NoError(t, err)
}
