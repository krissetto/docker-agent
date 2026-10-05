package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/stretchr/testify/require"
)

type inputAckStore struct {
	session.Store
	session.ItemAppender
	fail    atomic.Bool
	block   <-chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (s *inputAckStore) AddMessage(ctx context.Context, id string, msg *session.Message) (int64, error) {
	n, err := s.Store.AddMessage(ctx, id, msg)
	if err == nil && s.fail.Swap(false) {
		return 0, &session.TemporaryError{Err: errors.New("lost input acknowledgment")}
	}
	return n, err
}
func (s *inputAckStore) AppendItem(ctx context.Context, id, key string, item session.Item) (int64, error) {
	n, err := s.ItemAppender.AppendItem(ctx, id, key, item)
	if err == nil && s.fail.Swap(false) {
		return 0, &session.TemporaryError{Err: errors.New("lost input acknowledgment")}
	}
	return n, err
}
func (s *inputAckStore) PromotePendingUserMessage(ctx context.Context, id, turn string) error {
	if id == "blocked" && s.block != nil {
		s.once.Do(func() { close(s.entered) })
		<-s.block
	}
	return s.Store.PromotePendingUserMessage(ctx, id, turn)
}
func TestInputAppendAmbiguousAcknowledgment(t *testing.T) {
	base := coordinationSQLite(t)
	store := &inputAckStore{Store: base, ItemAppender: base.(session.ItemAppender)}
	r := newDriverTestRuntime(t)
	r.sessionStore = store
	sess := session.New(session.WithID("input-ack"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	d := r.sessionDrivers.Get(sess)
	d.compactReserved = true
	msg := QueuedMessage{RequestID: "same", Content: "one"}
	store.fail.Store(true)
	_, err := d.post(t.Context(), msg, true)
	require.Error(t, err)
	_, err = d.post(t.Context(), msg, true)
	require.NoError(t, err)
	stored, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, stored.MessagesSnapshot(), 1)
	require.Len(t, sess.MessagesSnapshot(), 1)
}
func TestDurablePendingInputsRestoreFIFO(t *testing.T) {
	base := coordinationSQLite(t)
	store := &inputAckStore{Store: base, ItemAppender: base.(session.ItemAppender)}
	r := newDriverTestRuntime(t)
	r.sessionStore = store
	sess := session.New(session.WithID("durable-pending"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	for _, id := range []string{"first", "second"} {
		msg := session.UserMessage(id)
		msg.Pending, msg.Accepted, msg.TurnID = true, true, id
		_, err := store.AddMessage(t.Context(), sess.ID, msg)
		require.NoError(t, err)
		sess.AddMessage(msg)
	}
	d, err := r.sessionDrivers.GetInitialized(t.Context(), sess)
	require.NoError(t, err)
	require.Len(t, d.pending, 2, "publication restores durable pending inputs in FIFO order")
	ctx, generation, _, err := d.prepareStart(t.Context(), true)
	require.NoError(t, err)
	require.NotNil(t, ctx)
	require.Equal(t, "first", d.activeRequestID)
	r.sessionDrivers.mu.Lock()
	r.sessionDrivers.closed = true
	r.sessionDrivers.mu.Unlock()
	d.StopAll()
	d.finishRun(generation, "")
	d.wg.Done()
}
func TestPromotionDoesNotBlockIndependentAdmission(t *testing.T) {
	base := coordinationSQLite(t)
	release := make(chan struct{})
	defer close(release)
	store := &inputAckStore{Store: base, ItemAppender: base.(session.ItemAppender), block: release, entered: make(chan struct{})}
	r := newDriverTestRuntime(t)
	r.sessionStore = store
	var ds []*sessionDriver
	for _, id := range []string{"blocked", "independent"} {
		sess := session.New(session.WithID(id))
		require.NoError(t, store.AddSession(t.Context(), sess))
		d := r.sessionDrivers.Get(sess)
		msg := QueuedMessage{RequestID: id, Content: id}
		_, err := d.admitInput(t.Context(), msg, SessionOperationPost, true, false)
		require.NoError(t, err)
		ds = append(ds, d)
	}
	r.sessionDrivers.closed = true
	go func() {
		_, generation, _, err := ds[0].prepareStart(t.Context(), true)
		if err == nil {
			ds[0].StopAll()
			ds[0].finishRun(generation, "")
			ds[0].wg.Done()
		}
	}()
	<-store.entered
	done := make(chan error, 1)
	go func() {
		_, generation, _, err := ds[1].prepareStart(t.Context(), true)
		if err == nil {
			ds[1].StopAll()
			ds[1].finishRun(generation, "")
			ds[1].wg.Done()
		}
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("independent start blocked by storage")
	}
}

func TestAsyncAdmissionContentionIsConservativeAndRetryable(t *testing.T) {
	r := newDriverTestRuntime(t)
	r.maxActiveDescendants = 1
	a := r.sessionDrivers.Get(session.New(session.WithID("a"), session.WithParentID("root"), session.WithAsyncSubagent(true)))
	b := r.sessionDrivers.Get(session.New(session.WithID("b"), session.WithParentID("root"), session.WithAsyncSubagent(true)))
	require.NoError(t, a.ownerCall(t.Context(), func() error { a.phase = sessionStarting; return nil }))
	result := make(chan error, 1)
	go func() { _, _, _, err := b.prepareStart(t.Context(), false); result <- err }()
	select {
	case err := <-result:
		require.ErrorIs(t, err, ErrSessionCapacity)
	case <-time.After(time.Second):
		t.Fatal("admission waited on another driver's lock")
	}
	require.NoError(t, a.ownerCall(t.Context(), func() error { a.phase = sessionIdle; return nil }))
	_, generation, _, err := b.prepareStart(t.Context(), false)
	require.NoError(t, err)
	r.sessionDrivers.closed = true
	b.StopAll()
	b.finishRun(generation, "")
	b.wg.Done()
}

func TestSchedulerSettlesIndependentSessionDuringBlockedPersistence(t *testing.T) {
	base := coordinationSQLite(t)
	release := make(chan struct{})
	entered := make(chan struct{})
	store := &settlementBlockStore{Store: base, ItemAppender: base.(session.ItemAppender), release: release, entered: entered}
	r := newDriverTestRuntime(t)
	ctx, cancel := context.WithCancel(t.Context())
	r.lifecycleCtx = ctx
	observer := newPersistenceObserver(store)
	observer.lifetime = ctx
	r.observers = []EventObserver{observer}
	var drivers []*sessionDriver
	for _, id := range []string{"a-blocked", "b-independent"} {
		sess := session.New(session.WithID(id))
		require.NoError(t, store.AddSession(t.Context(), sess))
		d := r.sessionDrivers.Get(sess)
		if id == "a-blocked" {
			observer.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, "output"))
		}
		d.mu.Lock()
		d.phase = sessionSettling
		d.generation = 1
		d.completionErr = &session.TemporaryError{Err: errors.New("retry")}
		d.mu.Unlock()
		drivers = append(drivers, d)
	}
	store.block.Store(true)
	defer func() { close(release); cancel() }()
	r.sessionDrivers.signalWork()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not attempt blocked settlement")
	}
	require.Eventually(t, func() bool {
		drivers[1].mu.Lock()
		defer drivers[1].mu.Unlock()
		return drivers[1].settledGeneration == 1
	}, time.Second, time.Millisecond, "independent settlement must not await the first store")
}

type settlementBlockStore struct {
	session.Store
	session.ItemAppender
	block   atomic.Bool
	release <-chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (s *settlementBlockStore) AppendItem(ctx context.Context, id, key string, item session.Item) (int64, error) {
	if !s.block.Load() {
		return 0, &session.TemporaryError{Err: errors.New("queue journal")}
	}
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return s.ItemAppender.AppendItem(ctx, id, key, item)
}

func TestInputAppendRetryRejectsChangedContentAfterLostAcknowledgment(t *testing.T) {
	base := coordinationSQLite(t)
	store := &inputAckStore{Store: base, ItemAppender: base.(session.ItemAppender)}
	r := newDriverTestRuntime(t)
	r.sessionStore = store
	sess := session.New(session.WithID("conflicting-retry"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	d := r.sessionDrivers.Get(sess)
	d.compactReserved = true
	store.fail.Store(true)
	_, err := d.post(t.Context(), QueuedMessage{RequestID: "same", Content: "first"}, true)
	require.Error(t, err)
	_, err = d.post(t.Context(), QueuedMessage{RequestID: "same", Content: "changed"}, true)
	require.ErrorIs(t, err, session.ErrWriteConflict)
	_, err = d.post(t.Context(), QueuedMessage{RequestID: "same", Content: "first"}, true)
	require.NoError(t, err)
	require.Len(t, d.pending, 1)
}

func TestRestoreBatchPublishesDurablePendingFIFOWithoutStorage(t *testing.T) {
	base := coordinationSQLite(t)
	store := &inputAckStore{Store: base, ItemAppender: base.(session.ItemAppender)}
	r := newDriverTestRuntime(t)
	r.sessionStore = store
	sess := session.New(session.WithID("restore-pending"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	for _, id := range []string{"first", "second"} {
		msg := session.UserMessage(id)
		msg.Pending, msg.Accepted, msg.TurnID = true, true, id
		_, err := store.AddMessage(t.Context(), sess.ID, msg)
		require.NoError(t, err)
		sess.AddMessage(msg)
	}
	reservation, err := r.sessionDrivers.PrepareRestore(t.Context(), sess)
	require.NoError(t, err)
	store.fail.Store(true)
	require.NoError(t, r.sessionDrivers.ActivateRestoreBatch([]*restoreDriverReservation{reservation}, nil))
	require.True(t, store.fail.Load(), "atomic registry activation must not call storage")
	d, ok := r.sessionDrivers.Lookup(sess.ID)
	require.True(t, ok)
	require.Len(t, d.pending, 2)
}

func TestSchedulerWorkersAreBoundedAndQueuedSessionsMakeProgress(t *testing.T) {
	base := coordinationSQLite(t)
	store := &boundedSettlementStore{Store: base, ItemAppender: base.(session.ItemAppender), entered: make(chan struct{}, 64), release: make(chan struct{})}
	r := newDriverTestRuntime(t)
	ctx, cancel := context.WithCancel(t.Context())
	r.lifecycleCtx = ctx
	defer cancel()
	observer := newPersistenceObserver(store)
	observer.lifetime = ctx
	r.observers = []EventObserver{observer}
	var drivers []*sessionDriver
	for i := range 40 {
		sess := session.New(session.WithID(fmt.Sprintf("bounded-%02d", i)))
		require.NoError(t, store.AddSession(t.Context(), sess))
		d := r.sessionDrivers.Get(sess)
		observer.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, "output"))
		d.mu.Lock()
		d.phase = sessionSettling
		d.generation = 1
		d.completionErr = &session.TemporaryError{Err: errors.New("retry")}
		d.mu.Unlock()
		drivers = append(drivers, d)
	}
	store.block.Store(true)
	release := sync.OnceFunc(func() { close(store.release) })
	defer release()
	r.sessionDrivers.signalWork()
	for range 32 {
		select {
		case <-store.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("workers did not fill bound")
		}
	}
	select {
	case <-store.entered:
		t.Fatal("scheduler exceeded worker limit")
	case <-time.After(30 * time.Millisecond):
	}
	release()
	require.Eventually(t, func() bool {
		for _, d := range drivers {
			d.mu.Lock()
			done := d.settledGeneration == 1
			d.mu.Unlock()
			if !done {
				return false
			}
		}
		return true
	}, 3*time.Second, time.Millisecond, "queued sessions must progress after occupied workers finish")
	cancel()
	select {
	case <-r.sessionDrivers.workDone:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}
	require.Eventually(t, func() bool { return store.active.Load() == 0 }, time.Second, time.Millisecond, "storage attempts must all exit")
}

type boundedSettlementStore struct {
	session.Store
	session.ItemAppender
	block   atomic.Bool
	active  atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (s *boundedSettlementStore) AppendItem(ctx context.Context, id, key string, item session.Item) (int64, error) {
	if !s.block.Load() {
		return 0, &session.TemporaryError{Err: errors.New("queue journal")}
	}
	s.active.Add(1)
	defer s.active.Add(-1)
	s.entered <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return s.ItemAppender.AppendItem(ctx, id, key, item)
}

func TestCapacityEvictionOrdersSnapshotAndSkipsBusyDriver(t *testing.T) {
	r := newDriverTestRuntime(t)
	r.idleRetention = 0
	var drivers []*sessionDriver
	for _, id := range []string{"busy", "oldest", "newest"} {
		drivers = append(drivers, r.sessionDrivers.Get(session.New(session.WithID(id))))
	}
	for i, d := range drivers {
		d.mu.Lock()
		d.lastActive = time.Unix(int64(i), 0)
		d.mu.Unlock()
	}
	drivers[0].mu.Lock()
	defer drivers[0].mu.Unlock()
	r.sessionDrivers.mu.Lock()
	defer r.sessionDrivers.mu.Unlock()
	require.True(t, r.sessionDrivers.evictSettledForCapacityLocked())
	require.NotContains(t, r.sessionDrivers.drivers, "oldest")
	require.Contains(t, r.sessionDrivers.drivers, "busy")
	require.Contains(t, r.sessionDrivers.drivers, "newest")
}

func TestStartWaitsForOwnMutationWithoutHoldingGlobalAdmission(t *testing.T) {
	r := newDriverTestRuntime(t)
	waiting := r.sessionDrivers.Get(session.New(session.WithID("waiting")))
	independent := r.sessionDrivers.Get(session.New(session.WithID("independent")))
	r.sessionDrivers.closed = true // no execution router in this isolated admission test
	waiting.mu.Lock()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, generation, _, err := waiting.prepareStart(t.Context(), false)
		if err == nil {
			waiting.StopAll()
			waiting.finishRun(generation, "")
			waiting.wg.Done()
		}
		done <- err
	}()
	<-started
	select {
	case err := <-done:
		waiting.mu.Unlock()
		t.Fatalf("same-session contention must wait, not return: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	otherDone := make(chan error, 1)
	go func() {
		_, generation, _, err := independent.prepareStart(t.Context(), false)
		if err == nil {
			independent.StopAll()
			independent.finishRun(generation, "")
			independent.wg.Done()
		}
		otherDone <- err
	}()
	select {
	case err := <-otherDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		waiting.mu.Unlock()
		t.Fatal("waiting start held global admission")
	}
	waiting.mu.Unlock()
	require.NoError(t, <-done)
}

type blockedReportPollStore struct {
	session.CoordinationStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	active  atomic.Bool
}

func (s *blockedReportPollStore) PendingReports(context.Context, string) ([]session.ChildReport, error) {
	s.active.Store(true)
	defer s.active.Store(false)
	s.once.Do(func() { close(s.entered) })
	<-s.release // Deliberately ignore cancellation: drain must join actual I/O.
	return nil, nil
}

func TestSchedulerReportWorkerDrainOwnsStoreUntilExit(t *testing.T) {
	for _, operation := range []string{"close", "release", "delete"} {
		t.Run(operation, func(t *testing.T) {
			r := newDriverTestRuntime(t)
			ctx, cancel := context.WithCancel(t.Context())
			r.lifecycleCtx = ctx
			defer cancel()
			d := r.sessionDrivers.Get(session.New(session.WithID("blocked-report")))
			store := &blockedReportPollStore{CoordinationStore: session.NewInMemorySessionStore().(session.CoordinationStore), entered: make(chan struct{}), release: make(chan struct{})}
			release := sync.OnceFunc(func() { close(store.release) })
			defer release()
			r.subagents = &subagentManager{r: r, coord: store}
			r.sessionDrivers.signalWork()
			select {
			case <-store.entered:
			case <-time.After(time.Second):
				t.Fatal("report poll did not start")
			}
			drain := func(ctx context.Context) error {
				switch operation {
				case "release":
					return r.sessionDrivers.Release(ctx, d.identityID)
				case "delete":
					return r.sessionDrivers.Delete(ctx, d.identityID)
				default:
					return r.sessionDrivers.CloseContext(ctx)
				}
			}
			deadline, stop := context.WithTimeout(t.Context(), 20*time.Millisecond)
			err := drain(deadline)
			stop()
			require.ErrorIs(t, err, context.DeadlineExceeded, "drain must not acknowledge while storage is active")
			require.True(t, store.active.Load())
			retry := make(chan error, 1)
			go func() { retry <- drain(t.Context()) }()
			select {
			case err := <-retry:
				t.Fatalf("retry returned before worker exit: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			release()
			require.NoError(t, <-retry)
			require.False(t, store.active.Load())
			require.NoError(t, r.sessionDrivers.CloseContext(t.Context()))
			select {
			case <-r.sessionDrivers.workDone:
			case <-time.After(time.Second):
				t.Fatal("scheduler join did not finish")
			}
		})
	}
}
