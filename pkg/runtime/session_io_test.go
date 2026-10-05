package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

func TestDurableMessageEditUsesRowIdentity(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			store := session.NewInMemorySessionStore()
			if kind == "sqlite" {
				var err error
				store, err = sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "edit.db"))
				require.NoError(t, err)
			}
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			h := pendingRecallHandle(t, store)
			require.NoError(t, h.driver.ownerCall(t.Context(), func() error { h.driver.phase = sessionIdle; h.driver.activeRequestID = ""; return nil }))
			for _, text := range []string{"first", "second"} {
				snapshot, err := h.Edit(t.Context(), SessionEdit{Kind: SessionEditMessage, MessageIndex: -1, Message: session.UserMessage(text)})
				require.NoError(t, err)
				require.Positive(t, snapshot.Messages[len(snapshot.Messages)-1].Message.ID)
			}
			before, err := h.Snapshot(t.Context())
			require.NoError(t, err)
			id := before.Messages[1].Message.ID
			for _, text := range []string{"edited", "edited again"} {
				after, err := h.Edit(t.Context(), SessionEdit{Kind: SessionEditMessage, MessageIndex: id, Message: session.UserMessage(text)})
				require.NoError(t, err)
				require.Equal(t, id, after.Messages[1].Message.ID)
				require.Equal(t, "first", after.Messages[0].Message.Message.Content)
				loaded, err := store.GetSession(t.Context(), h.ID())
				require.NoError(t, err)
				require.Equal(t, text, loaded.Messages[1].Message.Message.Content)
				require.Equal(t, id, loaded.Messages[1].Message.ID)
			}
			_, err = h.Edit(t.Context(), SessionEdit{Kind: SessionEditMessage, MessageIndex: id + 10000, Message: session.UserMessage("invalid")})
			require.Error(t, err)
		})
	}
}

type laneBlockedStore struct {
	session.Store
	entered chan struct{}
	release chan struct{}
}

func (s *laneBlockedStore) AddMessage(ctx context.Context, id string, message *session.Message) (int64, error) {
	close(s.entered)
	<-s.release
	return s.Store.AddMessage(context.WithoutCancel(ctx), id, message)
}

func (s *laneBlockedStore) DeletePendingUserMessage(ctx context.Context, id, turnID string) error {
	return s.Store.(session.PendingMessageDeleter).DeletePendingUserMessage(ctx, id, turnID)
}

func TestDurableAppendDeadlineRetainsReservationAndResponsiveOwner(t *testing.T) {
	base := session.NewInMemorySessionStore()
	h := pendingRecallHandle(t, base)
	blocked := &laneBlockedStore{Store: base, entered: make(chan struct{}), release: make(chan struct{})}
	h.driver.r.sessionStore = blocked
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := h.Submit(ctx, TurnInput{Content: "accepted late"}); result <- err }()
	<-blocked.entered
	require.ErrorIs(t, <-result, context.DeadlineExceeded)
	statusCtx, statusCancel := context.WithTimeout(t.Context(), time.Second)
	defer statusCancel()
	_, err := h.Status(statusCtx)
	require.NoError(t, err)
	require.NoError(t, h.driver.ownerCall(t.Context(), func() error { h.driver.durableStopRequested = true; return nil }))
	h.driver.StopAll()
	stopCtx, stopCancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer stopCancel()
	require.ErrorIs(t, h.driver.withdrawStoppedInputs(stopCtx), context.DeadlineExceeded)
	close(blocked.release)
	require.NoError(t, h.driver.durableIO(t.Context(), func() (sessionIOReservation, error) { return sessionIOReservation{}, nil }))
	loaded, err := base.GetSession(t.Context(), h.ID())
	require.NoError(t, err)
	restored := newSessionDriver(h.driver.r, loaded)
	t.Cleanup(restored.closeOwner)
	require.Empty(t, restored.pending)
	for _, item := range loaded.Messages {
		require.True(t, item.Message == nil || !item.Message.Pending)
	}
}

type promotionBlockedStore struct {
	session.Store
	entered chan struct{}
	release chan struct{}
}

func (s *promotionBlockedStore) PromotePendingUserMessage(ctx context.Context, id, turnID string) error {
	close(s.entered)
	<-s.release
	return s.Store.PromotePendingUserMessage(context.WithoutCancel(ctx), id, turnID)
}

func TestDurablePromotionDeadlineKeepsOwnerResponsive(t *testing.T) {
	base := session.NewInMemorySessionStore()
	h := pendingRecallHandle(t, base)
	accepted, err := h.Submit(t.Context(), TurnInput{Content: "queued"})
	require.NoError(t, err)
	blocked := &promotionBlockedStore{Store: base, entered: make(chan struct{}), release: make(chan struct{})}
	h.driver.r.sessionStore = blocked
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- h.driver.promoteInput(ctx, accepted.TurnID, nil) }()
	<-blocked.entered
	require.ErrorIs(t, <-result, context.DeadlineExceeded)
	statusCtx, statusCancel := context.WithTimeout(t.Context(), time.Second)
	defer statusCancel()
	_, err = h.Status(statusCtx)
	require.NoError(t, err)
	close(blocked.release)
	require.NoError(t, h.driver.durableIO(t.Context(), func() (sessionIOReservation, error) { return sessionIOReservation{}, nil }))
	snapshot, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	require.False(t, snapshot.Messages[0].Message.Pending)
}

func TestDurableRootStopWithdrawsPendingAndSteeringOnReload(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			store := session.NewInMemorySessionStore()
			if kind == "sqlite" {
				var err error
				store, err = sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "stop.db"))
				require.NoError(t, err)
			}
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			h := pendingRecallHandle(t, store)
			first, err := h.Submit(t.Context(), TurnInput{Content: "pending"})
			require.NoError(t, err)
			second, err := h.Steer(t.Context(), TurnInput{Content: "steering"})
			require.NoError(t, err)
			h.driver.StopAll()
			require.NoError(t, h.driver.withdrawStoppedInputs(t.Context()))
			loaded, err := store.GetSession(t.Context(), h.ID())
			require.NoError(t, err)
			restored := newSessionDriver(h.driver.r, loaded)
			t.Cleanup(restored.closeOwner)
			require.Empty(t, restored.pending)
			require.Equal(t, string(TurnCanceled), loaded.TurnOutcome(first.TurnID))
			require.Equal(t, string(TurnCanceled), loaded.TurnOutcome(second.TurnID))
		})
	}
}

func TestPersistedAssistantMessageIDCanBeEdited(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, kind := range []string{"memory", "sqlite"} {
			t.Run(kind+"/"+map[bool]string{false: "append", true: "streaming"}[streaming], func(t *testing.T) {
				store := session.NewInMemorySessionStore()
				if kind == "sqlite" {
					var err error
					store, err = sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "assistant.db"))
					require.NoError(t, err)
				}
				t.Cleanup(func() { require.NoError(t, store.Close()) })
				h := pendingRecallHandle(t, store)
				d := h.driver
				require.NoError(t, d.ownerCall(t.Context(), func() error { d.generation = 1; return nil }))
				// A stored row in another session makes SQLite row IDs differ from positions.
				other := session.New(session.WithID("other"))
				require.NoError(t, store.AddSession(t.Context(), other))
				_, err := store.AddMessage(t.Context(), other.ID, session.UserMessage("unrelated"))
				require.NoError(t, err)
				observer := newPersistenceObserver(store)
				observer.owner = func(id string) *sessionDriver {
					if id == h.ID() {
						return d
					}
					return nil
				}
				if streaming {
					observer.OnEvent(t.Context(), d.session(), AgentChoice("root", h.ID(), "generated"))
				}
				message := session.UserMessage("generated")
				message.Message.Role = "assistant"
				message.Message.CacheControl = true
				message.AgentName = "root"
				event := MessageAdded(h.ID(), message, "root")
				require.NoError(t, d.commitExecutionEvent(t.Context(), 1, event))
				observer.OnEvent(t.Context(), d.session(), event)
				require.NoError(t, observer.completionErrorContext(t.Context(), h.ID()))
				snapshot, err := h.Snapshot(t.Context())
				require.NoError(t, err)
				id := snapshot.Messages[len(snapshot.Messages)-1].Message.ID
				require.Positive(t, id)
				loaded, err := store.GetSession(t.Context(), h.ID())
				require.NoError(t, err)
				require.Equal(t, loaded.Messages[len(loaded.Messages)-1].Message.ID, id)
				require.NoError(t, d.ownerCall(t.Context(), func() error { d.phase = sessionIdle; d.activeRequestID = ""; return nil }))
				replacement := session.UserMessage("revised")
				replacement.Message.Role = "assistant"
				_, err = h.Edit(t.Context(), SessionEdit{Kind: SessionEditMessage, MessageIndex: id, Message: replacement})
				require.NoError(t, err)
				loaded, err = store.GetSession(t.Context(), h.ID())
				require.NoError(t, err)
				require.Equal(t, "revised", loaded.Messages[len(loaded.Messages)-1].Message.Message.Content)
			})
		}
	}
}

func TestPersistenceMessageReceiptsDisambiguateIdenticalPayloads(t *testing.T) {
	h := pendingRecallHandle(t, session.NewInMemorySessionStore())
	d := h.driver
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.generation = 1; return nil }))
	events := make([]*MessageAddedEvent, 2)
	for i := range events {
		message := session.UserMessage("identical")
		message.Message.Role = "assistant"
		events[i] = MessageAdded(h.ID(), message, "root").(*MessageAddedEvent)
		require.NoError(t, d.commitExecutionEvent(t.Context(), 1, events[i]))
	}
	p := newPersistenceObserver(d.r.sessionStore)
	p.owner = func(string) *sessionDriver { return d }
	// Delayed writes complete out of publication order; receipt, not content,
	// determines which authoritative item receives each row identity.
	require.NoError(t, p.publishMessageID(t.Context(), h.ID(), 92, events[1].ownerMessage))
	require.NoError(t, p.publishMessageID(t.Context(), h.ID(), 91, events[0].ownerMessage))
	snapshot, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(91), snapshot.Messages[0].Message.ID)
	require.Equal(t, int64(92), snapshot.Messages[1].Message.ID)
	require.NoError(t, d.ownerCall(t.Context(), func() error {
		d.sess = d.sess.OwnSnapshot()
		d.sess.Messages[0].Message.ID = 0
		return nil
	}))
	require.NoError(t, p.publishMessageID(t.Context(), h.ID(), 99, events[0].ownerMessage))
	snapshot, err = h.Snapshot(t.Context())
	require.NoError(t, err)
	require.Zero(t, snapshot.Messages[0].Message.ID, "replaced/compacted transcript invalidates old append receipt")
}

type canceledAttemptTitleStore struct {
	session.Store
	fail  bool
	calls int
}

func (s *canceledAttemptTitleStore) UpdateSessionTitle(ctx context.Context, id, title string) error {
	s.calls++
	if s.fail {
		return context.Canceled
	}
	return s.Store.UpdateSessionTitle(ctx, id, title)
}

func TestPersistenceCanceledAttemptRetriesWithFreshDurabilityContext(t *testing.T) {
	base := session.NewInMemorySessionStore()
	value := session.New(session.WithID("canceled-attempt"))
	require.NoError(t, base.AddSession(t.Context(), value))
	store := &canceledAttemptTitleStore{Store: base, fail: true}
	p := newPersistenceObserver(store)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	p.OnEvent(canceled, value, SessionTitle(value.ID, "first"))
	require.ErrorIs(t, p.pendingError(value.ID), context.Canceled)
	require.True(t, session.IsTemporary(p.pendingError(value.ID)))
	p.OnEvent(canceled, value, SessionTitle(value.ID, "second"))
	store.fail = false
	require.NoError(t, p.completionErrorContext(t.Context(), value.ID))
	require.NoError(t, p.pendingError(value.ID))
	loaded, err := base.GetSession(t.Context(), value.ID)
	require.NoError(t, err)
	require.Equal(t, "second", loaded.TitleSnapshot())
	// An unexplained storage cancellation does not become caller retry policy.
	store.fail = true
	p.OnEvent(t.Context(), value, SessionTitle(value.ID, "third"))
	require.ErrorIs(t, p.pendingError(value.ID), context.Canceled)
	require.False(t, session.IsTemporary(p.pendingError(value.ID)))
}

type boundedSettlementFailureStore struct {
	session.Store
	session.CoordinationStore
	fail     atomic.Bool
	attempts atomic.Int32
}

func (s *boundedSettlementFailureStore) CommitChild(ctx context.Context, commit session.ChildCommit) error {
	s.attempts.Add(1)
	if s.fail.Load() {
		return errors.New("bounded completion failure")
	}
	return s.CoordinationStore.CommitChild(ctx, commit)
}

func TestFirstSettlementFailureSchedulesBoundedRecovery(t *testing.T) {
	base := coordinationSQLite(t)
	store := &boundedSettlementFailureStore{Store: base, CoordinationStore: base.(session.CoordinationStore)}
	store.fail.Store(true)
	_, owner := coordinationRuntime(t, store, coordinationReply("received"), coordinationReply("child done"))
	coordinationCreate(t, owner.Runtime(), "root", "")
	child := coordinationCreate(t, owner.Runtime(), "child", "root")
	turn, err := child.Submit(t.Context(), TurnInput{Content: "work"})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.EqualError(t, child.AwaitTurn(ctx, turn.TurnID), "bounded completion failure")
	require.Eventually(t, func() bool { return store.attempts.Load() >= 2 }, time.Second, time.Millisecond)
	// Observe the scheduler's backoff window without blocking test cancellation.
	window := time.NewTimer(350 * time.Millisecond)
	defer window.Stop()
	select {
	case <-window.C:
	case <-t.Context().Done():
		t.Fatal("test canceled while observing settlement retry backoff")
	}
	require.LessOrEqual(t, store.attempts.Load(), int32(5), "retry signal must not cause immediate failure spin")
	store.fail.Store(false)
	require.Eventually(t, func() bool { return child.AwaitTurn(t.Context(), turn.TurnID) == nil }, 3*time.Second, time.Millisecond)
}

func TestCancelStartingPromotionIsAcknowledgedOnceAndNeverExecutes(t *testing.T) {
	base := session.NewInMemorySessionStore()
	h := pendingRecallHandle(t, base)
	turn, err := h.Submit(t.Context(), TurnInput{Content: "cancel before execution"})
	require.NoError(t, err)
	d := h.driver
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.phase = sessionIdle; d.activeRequestID = ""; return nil }))
	blocked := &promotionBlockedStore{Store: base, entered: make(chan struct{}), release: make(chan struct{})}
	d.r.sessionStore = blocked
	started := make(chan error, 1)
	go func() { _, _, _, err := d.prepareStart(t.Context(), true); started <- err }()
	<-blocked.entered
	results := make(chan CancelOutcome, 2)
	cancelErrors := make(chan error, 2)
	for range 2 {
		go func() {
			outcome, err := h.Cancel(t.Context(), turn.TurnID)
			cancelErrors <- err
			results <- outcome.Outcome
		}()
	}
	first, second := <-results, <-results
	require.NoError(t, <-cancelErrors)
	require.NoError(t, <-cancelErrors)
	require.ElementsMatch(t, []CancelOutcome{CancelAccepted, CancelAlreadyCancelling}, []CancelOutcome{first, second})
	close(blocked.release)
	require.ErrorIs(t, <-started, context.Canceled)
	require.NoError(t, h.AwaitTurn(t.Context(), turn.TurnID))
	status, err := h.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, SessionStateSettled, status.State)
	loaded, err := base.GetSession(t.Context(), h.ID())
	require.NoError(t, err)
	require.Equal(t, string(TurnCanceled), loaded.TurnOutcome(turn.TurnID))
	restored := newSessionDriver(d.r, loaded)
	t.Cleanup(restored.closeOwner)
	require.Empty(t, restored.pending)
}

func TestEmptyIdentityGuidancePublishesOwnedTranscriptWithoutDuplicatingSeed(t *testing.T) {
	r := newDriverTestRuntime(t)
	value := session.New(session.WithID("guidance"), session.WithUserMessage("seed"))
	d := r.sessionDrivers.Get(value)
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.generation = 1; return nil }))
	mirror := UserMessage("seed", value.ID, nil, 0)
	require.NoError(t, d.commitExecutionEvent(t.Context(), 1, mirror))
	message := UserMessage("guidance", value.ID, nil, 1).(*UserMessageEvent)
	message.ownerAppend = true
	require.NoError(t, d.commitExecutionEvent(t.Context(), 1, message))
	p := newPersistenceObserver(session.NewInMemorySessionStore())
	p.owner = func(string) *sessionDriver { return d }
	require.NoError(t, p.publishMessageID(t.Context(), value.ID, 123, message.ownerMessage))
	snapshot, err := d.ownerSnapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Messages, 2)
	require.Equal(t, "seed", snapshot.Messages[0].Message.Message.Content)
	require.Equal(t, "guidance", snapshot.Messages[1].Message.Message.Content)
	require.Equal(t, int64(123), snapshot.Messages[1].Message.ID)
}
