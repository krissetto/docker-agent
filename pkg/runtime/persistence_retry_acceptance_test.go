package runtime

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

type ambiguousAppendStore struct {
	session.Store
	session.CoordinationStore
	session.ItemAppender

	tree subagent.Store

	mu            sync.Mutex
	attempts      map[string]int
	failed        bool
	targetSession string
	failure       error
}

func (s *ambiguousAppendStore) AppendItem(ctx context.Context, sessionID, writeID string, item session.Item) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	id, err := s.ItemAppender.AppendItem(ctx, sessionID, writeID, item)
	if err != nil {
		return id, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempts == nil {
		s.attempts = map[string]int{}
	}
	s.attempts[sessionID+":"+writeID]++
	if !s.failed && sessionID == s.targetSession {
		s.failed = true
		return 0, s.failure
	}
	return id, nil
}

func (s *ambiguousAppendStore) SaveTree(ctx context.Context, id string, snapshot subagent.Snapshot) error {
	return s.tree.SaveTree(ctx, id, snapshot)
}

func (s *ambiguousAppendStore) LoadTree(ctx context.Context, id string) (*subagent.Snapshot, error) {
	return s.tree.LoadTree(ctx, id)
}
func (*ambiguousAppendStore) Durability() subagent.Durability { return subagent.DurabilityDurable }

func newAmbiguousAppendStore(t *testing.T, id string) *ambiguousAppendStore {
	t.Helper()
	base := coordinationSQLite(t)
	return &ambiguousAppendStore{Store: base, CoordinationStore: base.(session.CoordinationStore), ItemAppender: base.(session.ItemAppender), tree: base.(subagent.Store), targetSession: id, failure: &session.TemporaryError{Err: errors.New("committed append acknowledgment lost")}}
}

func TestPersistenceJournalAmbiguousAppendKeepsOriginalWriteID(t *testing.T) {
	for _, kind := range []string{"assistant", "summary"} {
		t.Run(kind, func(t *testing.T) {
			store := newAmbiguousAppendStore(t, "journal")
			sess := session.New(session.WithID("journal"), session.WithTitle("test"))
			require.NoError(t, store.AddSession(t.Context(), sess))
			observer := newPersistenceObserver(store)
			observer.lifetime = t.Context()
			if kind == "assistant" {
				observer.OnEvent(t.Context(), sess, AgentChoice("worker", sess.ID, "first"))
			} else {
				observer.OnEvent(t.Context(), sess, SessionSummary(sess.ID, "summary", "worker", 7, 0.25, "test/model", &chat.Usage{InputTokens: 3, OutputTokens: 2}))
			}
			require.ErrorIs(t, observer.pendingError(sess.ID), store.failure)
			first, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			require.Len(t, first.MessagesSnapshot(), 1, "the uncertain append actually committed")
			require.NoError(t, observer.completionError(sess.ID))
			if kind == "assistant" {
				observer.OnEvent(t.Context(), sess, AgentChoice("worker", sess.ID, " second"))
				message := session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "first second"})
				observer.OnEvent(t.Context(), sess, MessageAdded(sess.ID, message, "worker"))
			}
			require.NoError(t, observer.completionError(sess.ID))
			stored, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			rows := stored.MessagesSnapshot()
			require.Len(t, rows, 1, "retry must reuse the original row, never append a duplicate")
			if kind == "assistant" {
				assert.Equal(t, "first second", rows[0].Message.Message.Content)
			} else {
				assert.Equal(t, "summary", rows[0].Summary)
				assert.Equal(t, 7, rows[0].FirstKeptEntry)
				assert.Equal(t, "test/model", rows[0].Model)
				assert.InDelta(t, 0.25, rows[0].Cost, 0.000001)
			}
			store.mu.Lock()
			attempts := maps.Clone(store.attempts)
			store.mu.Unlock()
			require.Len(t, attempts, 1, "the observer must retain the original immutable write identity")
			for id, count := range attempts {
				assert.Equal(t, 2, count, "write %s", id)
			}
		})
	}
}

func TestCoordinationPersistenceAppendRetryIsIdempotent(t *testing.T) {
	store := newAmbiguousAppendStore(t, "child")
	_, owner := coordinationRuntime(t, store, coordinationReply("received"), coordinationReply("assistant output"))
	root := coordinationCreate(t, owner.Runtime(), "root", "")
	child := coordinationCreate(t, owner.Runtime(), "child", root.ID())
	turn, err := child.Submit(t.Context(), TurnInput{Content: "write", RequestID: "write"})
	require.NoError(t, err)
	coordinationAwait(t, child, turn.TurnID)
	snapshot, err := child.Snapshot(t.Context())
	require.NoError(t, err)
	persisted, err := store.GetSession(t.Context(), child.ID())
	require.NoError(t, err)
	for name, sess := range map[string]*session.Session{"runtime": snapshot, "store": persisted} {
		assistants := 0
		for _, item := range sess.MessagesSnapshot() {
			if item.Message != nil && item.Message.Message.Role == chat.MessageRoleAssistant {
				assistants++
			}
		}
		assert.Equal(t, 1, assistants, "%s must contain one assistant row despite an uncertain append", name)
	}
	store.mu.Lock()
	retried := 0
	for id, count := range store.attempts {
		if count > 1 {
			retried++
			assert.Equal(t, 2, count, "write %s", id)
		}
	}
	failed := store.failed
	store.mu.Unlock()
	assert.True(t, failed, "the model turn must reach the injected append failure")
	assert.Equal(t, 1, retried)
	records, err := store.LoadChildren(t.Context(), root.ID())
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, turn.TurnID, records[0].LastTurnID, "settlement includes the durable completion commit")
	status, err := child.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, SessionStateSettled, status.State)
	if records[0].Node.State == subagent.NodeFailed {
		assert.NotEmpty(t, records[0].Node.Error, "canceled/error execution records its explicit outcome")
	} else {
		assert.Equal(t, subagent.NodeIdle, records[0].Node.State, "a provider that completed before cancellation may settle cleanly")
	}
	require.Eventually(t, func() bool {
		stored, err := store.GetSession(t.Context(), root.ID())
		if err != nil {
			return false
		}
		reports := 0
		for _, item := range stored.MessagesSnapshot() {
			if item.Message != nil && item.Message.TurnID == "report:"+childReportID(child.ID(), turn.TurnID) {
				reports++
			}
		}
		return reports == 1
	}, 5*time.Second, time.Millisecond, "committed completion delivers one correlated report, even if execution was canceled")
}

func TestPersistenceJournalCanceledRetryPreservesFailureAndFIFO(t *testing.T) {
	storageFailure := errors.New("no such column: write_hash")
	distinctFailure := errors.New("storage permission denied")
	canceledWrite := errors.Join(errors.New("append interrupted"), context.Canceled)
	for _, tc := range []struct {
		name           string
		initialFailure error
		retryFailure   error
		cancelBefore   bool
		cancelDuring   bool
		want           error
	}{
		{name: "already canceled", initialFailure: storageFailure, retryFailure: canceledWrite, cancelBefore: true, want: storageFailure},
		{name: "canceled during write", initialFailure: storageFailure, retryFailure: canceledWrite, cancelDuring: true, want: storageFailure},
		{name: "first cancellation", retryFailure: canceledWrite, cancelBefore: true, want: context.Canceled},
		{name: "distinct healthy failure", initialFailure: storageFailure, retryFailure: distinctFailure, want: distinctFailure},
		{name: "distinct canceled failure", initialFailure: storageFailure, retryFailure: distinctFailure, cancelDuring: true, want: distinctFailure},
		{name: "cancellation with healthy context", initialFailure: storageFailure, retryFailure: canceledWrite, want: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observer := newPersistenceObserver(session.NewInMemorySessionStore())
			observer.lifetime = t.Context()
			journal := observer.journal("retry")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancelBefore {
				cancel()
			}
			recovered := false
			var writes []string
			func() {
				journal.mu.Lock()
				defer journal.mu.Unlock()
				journal.failure = tc.initialFailure
				journal.bytes = 3
				journal.pending = []persistenceEffect{
					{bytes: 1, write: func(context.Context) error {
						if !recovered {
							if tc.cancelDuring {
								cancel()
							}
							return tc.retryFailure
						}
						writes = append(writes, "first")
						return nil
					}},
					{bytes: 2, write: func(context.Context) error {
						writes = append(writes, "second")
						return nil
					}},
				}
				err := observer.flushLocked(ctx, journal)
				require.ErrorIs(t, err, tc.want)
				require.ErrorIs(t, journal.failure, tc.want)
				assert.Len(t, journal.pending, 2, "a failed retry must retain both FIFO effects")
				assert.Equal(t, 3, journal.bytes)
				assert.Empty(t, writes, "later effects must not overtake the failed head")
			}()
			require.ErrorIs(t, observer.pendingError("retry"), tc.want)

			recovered = true
			require.NoError(t, observer.completionError("retry"))
			require.NoError(t, observer.completionError("retry"))
			require.NoError(t, observer.pendingError("retry"))
			assert.Equal(t, []string{"first", "second"}, writes, "recovery drains FIFO exactly once")
			func() {
				journal.mu.Lock()
				defer journal.mu.Unlock()
				assert.Empty(t, journal.pending)
				assert.Zero(t, journal.bytes)
			}()
		})
	}
}

func TestPersistenceJournalBoundsAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		size, count int
	}{
		{name: "event count", size: 1, count: persistenceJournalEvents},
		{name: "byte count", size: persistenceJournalBytes / 2, count: 2},
		{name: "oversized effect", size: persistenceJournalBytes + 1, count: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lifetime, cancel := context.WithCancel(t.Context())
			defer cancel()
			observer := newPersistenceObserver(session.NewInMemorySessionStore())
			observer.lifetime = lifetime
			journal := observer.journal("bounded")
			failure := &session.TemporaryError{Err: errors.New("storage unavailable")}
			var calls atomic.Int64
			var armed atomic.Bool
			retryEntered := make(chan struct{})
			var once sync.Once
			effect := func(ctx context.Context) error {
				calls.Add(1)
				if _, bounded := ctx.Deadline(); bounded && armed.Load() {
					once.Do(func() { close(retryEntered) })
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				return failure
			}
			journal.mu.Lock()
			for range tc.count {
				observer.enqueueLocked(t.Context(), journal, tc.size, effect)
			}
			assert.LessOrEqual(t, len(journal.pending), persistenceJournalEvents)
			assert.LessOrEqual(t, journal.bytes, persistenceJournalBytes)
			beforeCount, beforeBytes := len(journal.pending), journal.bytes
			journal.mu.Unlock()
			armed.Store(true)
			done := make(chan struct{})
			go func() {
				defer close(done)
				journal.mu.Lock()
				defer journal.mu.Unlock()
				observer.enqueueLocked(t.Context(), journal, tc.size, effect)
			}()
			coordinationWait(t, retryEntered)
			select {
			case <-done:
				t.Fatal("an effect beyond the hard bound must backpressure, not be dropped or enqueued")
			default:
			}
			cancel()
			coordinationWait(t, done)
			journal.mu.Lock()
			assert.Len(t, journal.pending, beforeCount, "blocked effect must not grow the bounded journal")
			assert.Equal(t, beforeBytes, journal.bytes)
			terminal := journal.terminal
			pendingFailure := journal.failure
			journal.mu.Unlock()
			require.Error(t, terminal, "canceling the supervisor lifetime must retain a terminal persistence error")
			require.Error(t, pendingFailure)
			assert.Greater(t, calls.Load(), int64(tc.count), "backpressure retries storage under a bounded context")
			require.Error(t, observer.completionError("bounded"), "aborted backpressure must never appear durably settled")
		})
	}
}

type blockedAppendStore struct {
	*ambiguousAppendStore

	blocked   atomic.Bool
	schemaErr error
}

func (s *blockedAppendStore) AppendItem(ctx context.Context, sessionID, writeID string, item session.Item) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if sessionID == s.targetSession && s.blocked.Load() {
		return 0, s.schemaErr
	}
	return s.ItemAppender.AppendItem(ctx, sessionID, writeID, item)
}

func TestCoordinationPermanentPersistenceFailureBlocksNewTurnsAndRetainsFIFO(t *testing.T) {
	store := &blockedAppendStore{ambiguousAppendStore: newAmbiguousAppendStore(t, "child"), schemaErr: errors.New("no such column: write_hash")}
	store.blocked.Store(true)
	entered, release := make(chan struct{}), make(chan struct{})
	var providerCalls atomic.Int32
	worker := coordinationReply("unused")
	worker.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
		if providerCalls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return newStreamBuilder().AddContent("assistant response").AddStopWithUsage(1, 1).Build(), nil
	}
	_, owner := coordinationRuntime(t, store, coordinationReply("received"), worker)
	coordinationCreate(t, owner.Runtime(), "root", "")
	child := coordinationCreate(t, owner.Runtime(), "child", "root")
	observation, err := child.Observe(t.Context(), ObserveOptions{Buffer: 128})
	require.NoError(t, err)
	defer observation.Cancel()
	var mu sync.Mutex
	var persistenceErrors []SessionEvent
	visible := make(chan struct{}, 1)
	observed := make(chan struct{})
	go func() {
		defer close(observed)
		for event := range observation.Events {
			failure, ok := event.Event.(*ErrorEvent)
			if !ok || !strings.Contains(failure.Error, store.schemaErr.Error()) {
				continue
			}
			mu.Lock()
			persistenceErrors = append(persistenceErrors, event)
			mu.Unlock()
			select {
			case visible <- struct{}{}:
			default:
			}
		}
	}()
	first, err := child.Submit(t.Context(), TurnInput{Content: "first", RequestID: "first"})
	require.NoError(t, err)
	coordinationWait(t, entered)
	queued, err := child.Submit(t.Context(), TurnInput{Content: "accepted before failure", RequestID: "queued"})
	require.NoError(t, err)
	close(release)
	coordinationWait(t, visible)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.ErrorIs(t, child.AwaitTurn(ctx, first.TurnID), store.schemaErr)
	before, err := child.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, before.Pending)
	for range 2 {
		rejected, submitErr := child.Submit(ctx, TurnInput{Content: "must not be accepted", RequestID: "after-failure"})
		var persistence *SessionError
		require.ErrorAs(t, submitErr, &persistence)
		assert.Equal(t, SessionErrorPersistence, persistence.Kind)
		assert.Contains(t, persistence.Detail, store.schemaErr.Error())
		assert.Empty(t, rejected.TurnID)
	}
	after, err := child.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, before.Pending, after.Pending)
	stored, err := store.GetSession(t.Context(), child.ID())
	require.NoError(t, err)
	pending := 0
	for _, item := range stored.MessagesSnapshot() {
		if item.Message == nil {
			continue
		}
		assert.NotEqual(t, "must not be accepted", item.Message.Message.Content)
		if item.Message.TurnID == queued.TurnID {
			pending++
			assert.True(t, item.Message.Pending)
			assert.True(t, item.Message.Accepted)
		}
	}
	assert.Equal(t, 1, pending, "previously accepted FIFO identity remains durable")
	store.blocked.Store(false)
	require.Eventually(t, func() bool {
		records, loadErr := store.LoadChildren(t.Context(), "root")
		return loadErr == nil && len(records) == 1 && records[0].LastTurnID == queued.TurnID
	}, 5*time.Second, time.Millisecond, "storage recovery retries completion and drains already accepted FIFO")
	coordinationAwait(t, child, first.TurnID)
	coordinationAwait(t, child, queued.TurnID)
	assert.Equal(t, int32(2), providerCalls.Load(), "rejected new inputs never execute")
	observation.Cancel()
	coordinationWait(t, observed)
	mu.Lock()
	errorsSeen := append([]SessionEvent(nil), persistenceErrors...)
	mu.Unlock()
	require.Len(t, errorsSeen, 1, "persistent retries must not flood observers with duplicate failures")
	assert.Equal(t, child.ID(), errorsSeen[0].SessionID)
	assert.Equal(t, first.TurnID, errorsSeen[0].TurnID, "visible failure is correlated to the affected accepted turn")
}
