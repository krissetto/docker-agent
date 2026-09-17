package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

type contentionStartupStore struct {
	session.Store

	blocked  atomic.Bool
	attempts atomic.Int32
}

func (s *contentionStartupStore) UpdateSession(ctx context.Context, sess *session.Session) error {
	if s.blocked.Load() {
		s.attempts.Add(1)
		return &session.TemporaryError{Err: errors.New("database is locked")}
	}
	return s.Store.UpdateSession(ctx, sess)
}

func TestSQLiteContentionStartupRetainsExecution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &contentionStartupStore{Store: session.NewInMemorySessionStore()}
		var calls atomic.Int32
		provider := coordinationReply("unused")
		provider.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
			calls.Add(1)
			return newStreamBuilder().AddContent("recovered").AddStopWithUsage(1, 1).Build(), nil
		}
		_, owner := coordinationRuntime(t, store, provider, coordinationReply("child"))
		h := coordinationCreate(t, owner.Runtime(), "contention-startup", "")
		store.blocked.Store(true)
		defer store.blocked.Store(false)
		turn, err := h.Submit(t.Context(), TurnInput{Content: "accepted", RequestID: "accepted"})
		require.NoError(t, err)
		synctest.Wait()
		assert.Positive(t, store.attempts.Load())
		assert.Zero(t, calls.Load())
		store.blocked.Store(false)
		coordinationAwait(t, h, turn.TurnID)
		assert.Equal(t, int32(1), calls.Load(), "contention delays the accepted turn rather than canceling or replaying it")
		stored, err := store.GetSession(t.Context(), h.ID())
		require.NoError(t, err)
		assert.Equal(t, "recovered", stored.GetLastAssistantMessageContent())
	})
}

func TestSQLiteContentionStreamingDoesNotCancelAndDrainsFIFO(t *testing.T) {
	r := newDriverTestRuntime(t)
	r.sessionDrivers.closed = true
	sess := session.New(session.WithID("streaming"))
	d := newSessionDriver(r, sess)
	r.sessionDrivers.drivers[sess.ID] = d
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d.cancel = cancel
	d.generation, d.activeRequestID = 1, "accepted"
	d.phase = sessionRunning
	d.events.SetRequest(sess.ID, "accepted", 1)
	p := newPersistenceObserver(session.NewInMemorySessionStore())
	p.lifetime = t.Context()
	r.observers = []EventObserver{p}
	blocked := true
	failure := &session.TemporaryError{Err: errors.New("database is locked")}
	var writes []string
	j := p.journal(sess.ID)
	j.pending = []persistenceEffect{
		{write: func(context.Context) error {
			if blocked {
				return failure
			}
			writes = append(writes, "first")
			return nil
		}},
		{write: func(context.Context) error { writes = append(writes, "second"); return nil }},
	}
	j.failure = failure
	inner := make(chan Event, 2)
	inner <- StreamStarted(sess.ID, "root")
	inner <- StreamStopped(sess.ID, "root", "")
	close(inner)
	for range r.observe(ctx, sess, inner) {
	}
	require.NoError(t, ctx.Err(), "a temporary persistence error must not cancel execution")
	var retryWarnings int
	for _, event := range canonicalReplay(d) {
		if warning, ok := event.Event.(*WarningEvent); ok {
			retryWarnings++
			assert.Contains(t, warning.Message, "retry")
			assert.NotContains(t, warning.Message, "new turns are blocked")
			assert.Equal(t, "accepted", event.RequestID)
		}
	}
	assert.Equal(t, 1, retryWarnings, "one correlated retry warning per failure streak")
	d.finishRun(1, "")
	h := &sessionHandle{driver: d, sessionID: sess.ID}
	require.ErrorIs(t, h.AwaitTurn(t.Context(), "accepted"), failure)
	assert.Empty(t, writes)
	blocked = false
	d.finishRun(1, "")
	require.NoError(t, h.AwaitTurn(t.Context(), "accepted"))
	assert.Equal(t, []string{"first", "second"}, writes)
	assert.Len(t, canonicalSettlements(canonicalReplay(d)), 1)
}

func TestSQLiteContentionShutdownRetainsUnsettledJournal(t *testing.T) {
	r := newDriverTestRuntime(t)
	r.sessionDrivers.closed = true
	d := newSessionDriver(r, session.New(session.WithID("drain")))
	r.sessionDrivers.drivers["drain"] = d
	d.generation, d.activeRequestID = 1, "accepted"
	d.phase = sessionRunning
	p := newPersistenceObserver(session.NewInMemorySessionStore())
	r.observers = []EventObserver{p}
	failure := &session.TemporaryError{Err: errors.New("database is locked")}
	blocked := true
	writes := 0
	j := p.journal("drain")
	j.pending = []persistenceEffect{{write: func(ctx context.Context) error {
		if blocked {
			return failure
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		writes++
		return nil
	}}}
	d.finishRun(1, "")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.ErrorIs(t, r.sessionDrivers.CloseContext(ctx), failure)
	retained, ok := r.sessionDrivers.Lookup("drain")
	require.True(t, ok)
	require.Same(t, d, retained)
	require.Same(t, j, p.journal("drain"))
	assert.Empty(t, canonicalSettlements(canonicalReplay(d)))
	blocked = false
	require.NoError(t, r.sessionDrivers.CloseContext(ctx))
	assert.Equal(t, 1, writes)
	assert.Len(t, canonicalSettlements(canonicalReplay(d)), 1)
	require.NoError(t, r.sessionDrivers.CloseContext(ctx))
	assert.Equal(t, 1, writes)
}

func TestSQLiteContentionRegistryDrainUsesCallerContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newDriverTestRuntime(t)
		r.sessionDrivers.closed = true
		d := newSessionDriver(r, session.New(session.WithID("caller-drain")))
		r.sessionDrivers.drivers["caller-drain"] = d
		d.generation, d.activeRequestID = 1, "accepted"
		d.phase = sessionRunning
		p := newPersistenceObserver(session.NewInMemorySessionStore())
		r.observers = []EventObserver{p}
		blocked := true
		attempts, writes := 0, 0
		p.journal("caller-drain").pending = []persistenceEffect{{write: func(ctx context.Context) error {
			attempts++
			if attempts == 1 {
				return &session.TemporaryError{Err: errors.New("database is locked")}
			}
			if blocked {
				<-ctx.Done()
				return ctx.Err()
			}
			writes++
			return nil
		}}}
		d.finishRun(1, "")
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		require.Error(t, r.sessionDrivers.CloseContext(ctx))
		assert.LessOrEqual(t, time.Since(start), 50*time.Millisecond, "direct registry drain must use caller context, not the observer lifetime")
		retained, ok := r.sessionDrivers.Lookup("caller-drain")
		require.True(t, ok)
		require.Same(t, d, retained)
		blocked = false
		require.NoError(t, r.sessionDrivers.CloseContext(t.Context()))
		assert.Equal(t, 1, writes)
		assert.Len(t, canonicalSettlements(canonicalReplay(d)), 1)
	})
}

func TestSQLiteContentionStoppedTurnStillAwaitsDurability(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := newSessionDriver(r, session.New(session.WithID("stopped")))
	d.generation, d.activeRequestID = 1, "accepted"
	d.phase = sessionRunning
	d.StopAll()
	h := &sessionHandle{driver: d, sessionID: "stopped"}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, h.AwaitTurn(ctx, "accepted"), context.Canceled)
	d.finishRun(1, "")
	require.NoError(t, h.AwaitTurn(t.Context(), "accepted"))
}

type contentionOutputStore struct {
	session.Store

	blocked atomic.Bool
}

func (s *contentionOutputStore) AddMessage(ctx context.Context, id string, message *session.Message) (int64, error) {
	if message.Message.Role == chat.MessageRoleAssistant && s.blocked.Load() {
		return 0, &session.TemporaryError{Err: errors.New("database is locked")}
	}
	return s.Store.AddMessage(ctx, id, message)
}

func TestSQLiteContentionSuccessorWaitsForDurableOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &contentionOutputStore{Store: session.NewInMemorySessionStore()}
		store.blocked.Store(true)
		defer store.blocked.Store(false)
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		var overtook atomic.Bool
		provider := coordinationReply("unused")
		provider.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
			if calls.Add(1) == 1 {
				close(entered)
				<-release
				return newStreamBuilder().AddContent("first output").AddStopWithUsage(1, 1).Build(), nil
			}
			stored, err := store.GetSession(ctx, "successor")
			overtook.Store(err != nil || stored.GetLastAssistantMessageContent() != "first output")
			return newStreamBuilder().AddContent("second output").AddStopWithUsage(1, 1).Build(), nil
		}
		_, owner := coordinationRuntime(t, store, provider, coordinationReply("child"))
		h := coordinationCreate(t, owner.Runtime(), "successor", "")
		first, err := h.Submit(t.Context(), TurnInput{Content: "first", RequestID: "first"})
		require.NoError(t, err)
		coordinationWait(t, entered)
		second, err := h.Submit(t.Context(), TurnInput{Content: "second", RequestID: "second"})
		require.NoError(t, err)
		close(release)
		synctest.Wait()
		assert.Equal(t, int32(1), calls.Load())
		require.Error(t, h.AwaitTurn(t.Context(), first.TurnID))
		store.blocked.Store(false)
		time.Sleep(2 * time.Second) //nolint:forbidigo // advance fake time inside synctest so the existing scheduler retries persistence
		coordinationAwait(t, h, first.TurnID)
		coordinationAwait(t, h, second.TurnID)
		assert.Equal(t, int32(2), calls.Load(), "each accepted provider execution runs once")
		assert.False(t, overtook.Load())
		stored, err := store.GetSession(t.Context(), h.ID())
		require.NoError(t, err)
		var output []string
		var turns []string
		for _, message := range stored.OwnMessages() {
			if message.Message.Role == chat.MessageRoleAssistant {
				output = append(output, message.Message.Content)
			} else if message.Accepted {
				turns = append(turns, message.TurnID)
			}
		}
		assert.Equal(t, []string{"first output", "second output"}, output)
		assert.Equal(t, []string{first.TurnID, second.TurnID}, turns)
	})
}

func TestSQLiteContentionSteeringContinuationKeepsIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newDriverTestRuntime(t)
		r.sessionDrivers.closed = true
		sess := session.New(session.WithID("steering-contention"))
		d := newSessionDriver(r, sess)
		r.sessionDrivers.drivers[sess.ID] = d
		d.generation, d.activeRequestID = 1, "accepted"
		d.phase = sessionRunning
		d.steering = []QueuedMessage{{RequestID: "steer", Content: "guidance"}}
		store := session.NewInMemorySessionStore()
		require.NoError(t, store.AddSession(t.Context(), sess))
		p := newPersistenceObserver(store)
		r.observers = []EventObserver{p}
		var blocked atomic.Bool
		blocked.Store(true)
		defer blocked.Store(false)
		var writes atomic.Int32
		p.journal(sess.ID).pending = []persistenceEffect{{write: func(context.Context) error {
			if blocked.Load() {
				return &session.TemporaryError{Err: errors.New("database is locked")}
			}
			writes.Add(1)
			return nil
		}}}
		ctx, generation, again := d.finishRun(1, "")
		require.True(t, again)
		require.Equal(t, uint64(1), generation)
		done := make(chan struct{})
		go func() {
			defer close(done)
			r.observeRunStart(ctx, sess)
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Error("continuation startup must wait for the previous output journal")
		default:
		}
		require.NoError(t, ctx.Err())
		blocked.Store(false)
		coordinationWait(t, done)
		assert.Equal(t, int32(1), writes.Load())
		d.mu.Lock()
		assert.Equal(t, "accepted", d.activeRequestID)
		require.Len(t, d.steering, 1)
		assert.Equal(t, "steer", d.steering[0].RequestID)
		d.mu.Unlock()
		assert.Empty(t, canonicalSettlements(canonicalReplay(d)))
	})
}

func TestSQLiteContentionStartupCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &contentionStartupStore{Store: session.NewInMemorySessionStore()}
		var calls atomic.Int32
		provider := coordinationReply("unused")
		provider.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
			calls.Add(1)
			return newStreamBuilder().AddContent("later turn").AddStopWithUsage(1, 1).Build(), nil
		}
		_, owner := coordinationRuntime(t, store, provider, coordinationReply("child"))
		h := coordinationCreate(t, owner.Runtime(), "startup-cancel", "")
		store.blocked.Store(true)
		defer store.blocked.Store(false)
		turn, err := h.Submit(t.Context(), TurnInput{Content: "cancel me"})
		require.NoError(t, err)
		synctest.Wait()
		_, err = h.Cancel(t.Context(), turn.TurnID)
		require.NoError(t, err)
		synctest.Wait()
		assert.Zero(t, calls.Load())
		store.blocked.Store(false)
		d := h.(*sessionHandle).driver
		coordinationWait(t, d.Done())
		d.mu.Lock()
		generation, runErr := d.generation, d.completionRunErr
		d.mu.Unlock()
		d.finishRun(generation, runErr)
		coordinationAwait(t, h, turn.TurnID)
		assert.Zero(t, calls.Load(), "an explicitly canceled startup must not execute after storage recovery")
		next, err := h.Submit(t.Context(), TurnInput{Content: "new work"})
		require.NoError(t, err)
		coordinationAwait(t, h, next.TurnID)
		assert.Equal(t, int32(1), calls.Load())
	})
}

type contentionChildCommitStore struct {
	session.Store
	session.CoordinationStore

	blocked   atomic.Bool
	committed atomic.Bool
	attempts  atomic.Int32
}

func (s *contentionChildCommitStore) CommitChild(ctx context.Context, commit session.ChildCommit) error {
	s.attempts.Add(1)
	if s.committed.Load() && s.blocked.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	err := s.CoordinationStore.CommitChild(ctx, commit)
	if err == nil && s.committed.CompareAndSwap(false, true) {
		return &session.TemporaryError{Err: errors.New("commit acknowledgment lost")}
	}
	return err
}

func TestSQLiteContentionShutdownChildCommitDrainContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := session.NewInMemorySessionStore()
		store := &contentionChildCommitStore{Store: base, CoordinationStore: base.(session.CoordinationStore)}
		store.blocked.Store(true)
		defer store.blocked.Store(false)
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		worker := coordinationReply("unused")
		worker.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
			calls.Add(1)
			close(entered)
			<-release
			return newStreamBuilder().AddContent("child result").AddStopWithUsage(1, 1).Build(), nil
		}
		r, owner := coordinationRuntime(t, store, coordinationReply("parent"), worker)
		parent := coordinationCreate(t, owner.Runtime(), "drain-parent", "")
		child := coordinationCreate(t, owner.Runtime(), "drain-child", parent.ID())
		turn, err := child.Submit(t.Context(), TurnInput{Content: "work"})
		require.NoError(t, err)
		coordinationWait(t, entered)
		// Pause the existing scheduler so only shutdown owns reconciliation.
		r.sessionDrivers.mu.Lock()
		r.sessionDrivers.closed = true
		r.sessionDrivers.mu.Unlock()
		close(release)
		d := child.(*sessionHandle).driver
		coordinationWait(t, d.Done())
		require.True(t, store.committed.Load())
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		require.ErrorIs(t, owner.Shutdown(ctx), context.DeadlineExceeded)
		for _, h := range []SessionHandle{parent, child} {
			retained, ok := r.sessionDrivers.Lookup(h.ID())
			require.True(t, ok, "failed drain keeps ancestors and descendants routable")
			require.Same(t, h.(*sessionHandle).driver, retained)
		}
		assert.Empty(t, canonicalSettlements(canonicalReplay(d)))
		_, err = parent.Submit(t.Context(), TurnInput{Content: "closed"})
		require.Error(t, err, "failed shutdown must not reopen admission")
		store.blocked.Store(false)
		require.NoError(t, owner.Shutdown(t.Context()))
		coordinationAwait(t, child, turn.TurnID)
		assert.Equal(t, int32(1), calls.Load())
		assert.Len(t, canonicalSettlements(canonicalReplay(d)), 1)
		reports, err := store.PendingReports(t.Context(), parent.ID())
		require.NoError(t, err)
		require.Len(t, reports, 1, "CAS reconciliation must preserve exactly one original child report")
		assert.Equal(t, turn.TurnID, reports[0].TurnID)
		attempts := store.attempts.Load()
		require.NoError(t, owner.Shutdown(t.Context()))
		assert.Equal(t, attempts, store.attempts.Load(), "successful cleanup is not repeated")
	})
}

func TestSQLiteContentionShutdownClosesAdmissionBeforeCancellation(t *testing.T) {
	r, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), coordinationReply("root"), coordinationReply("child"))
	coordinationCreate(t, owner.Runtime(), "admission", "")
	cancel := r.lifecycleCancel
	r.lifecycleCancel = func() {
		r.sessionDrivers.mu.Lock()
		closed := r.sessionDrivers.closed
		r.sessionDrivers.mu.Unlock()
		assert.True(t, closed, "shutdown must fence admission before canceling persistence and execution")
		cancel()
	}
	require.NoError(t, owner.Shutdown(t.Context()))
}
