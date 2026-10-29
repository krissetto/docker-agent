package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// nodrowStore is native (CoordinationStore) and exposes hooks around AddSession.
type nodrowStore struct {
	*session.InMemorySessionStore
	mu        sync.Mutex
	beforeAdd func(context.Context, *session.Session) error
	afterAdd  func(context.Context, *session.Session)
	onUpdate  func(context.Context, *session.Session)
	updates   int
}

func newNodrowStore() *nodrowStore {
	return &nodrowStore{InMemorySessionStore: session.NewInMemorySessionStore().(*session.InMemorySessionStore)}
}

func (s *nodrowStore) setBeforeAdd(hook func(context.Context, *session.Session) error) {
	s.mu.Lock()
	s.beforeAdd = hook
	s.mu.Unlock()
}

func (s *nodrowStore) setAfterAdd(hook func(context.Context, *session.Session)) {
	s.mu.Lock()
	s.afterAdd = hook
	s.mu.Unlock()
}

func (s *nodrowStore) setOnUpdate(hook func(context.Context, *session.Session)) {
	s.mu.Lock()
	s.onUpdate = hook
	s.mu.Unlock()
}

func (s *nodrowStore) updateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updates
}

func (s *nodrowStore) AddSession(ctx context.Context, sess *session.Session) error {
	s.mu.Lock()
	before, after := s.beforeAdd, s.afterAdd
	s.mu.Unlock()
	if before != nil {
		if err := before(ctx, sess); err != nil {
			return err
		}
	}
	if err := s.InMemorySessionStore.AddSession(ctx, sess); err != nil {
		return err
	}
	if after != nil {
		after(ctx, sess)
	}
	return nil
}

func (s *nodrowStore) UpdateSession(ctx context.Context, sess *session.Session) error {
	s.mu.Lock()
	s.updates++
	hook := s.onUpdate
	s.mu.Unlock()
	if hook != nil {
		hook(ctx, sess)
	}
	return s.InMemorySessionStore.UpdateSession(ctx, sess)
}

// nodrowPlainStore is not native: membership lives only in the volatile coordination store.
type nodrowPlainStore struct {
	session.Store
	inner *session.InMemorySessionStore
}

func (s *nodrowPlainStore) GetSessionMetadata(ctx context.Context, id string) (*session.Session, error) {
	return s.inner.GetSessionMetadata(ctx, id)
}

func (s *nodrowPlainStore) LoadChild(ctx context.Context, id string) (session.ChildRecord, error) {
	return s.inner.LoadChild(ctx, id)
}

// nodrowMinimalStore exposes only session.Store: no metadata or child-record reader.
type nodrowMinimalStore struct{ session.Store }

// nodrowNativeStore is native and can hide child records after admission.
type nodrowNativeStore struct {
	*session.InMemorySessionStore
	hideRecords atomic.Bool
}

func (s *nodrowNativeStore) LoadChild(ctx context.Context, id string) (session.ChildRecord, error) {
	if s.hideRecords.Load() {
		return session.ChildRecord{}, session.ErrNotFound
	}
	return s.InMemorySessionStore.LoadChild(ctx, id)
}

func nodrowRuntime(t *testing.T, store session.Store) (*SessionService, *LocalRuntime) {
	t.Helper()
	prov := &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()}
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	librarian := agent.New("librarian", "Library agent", agent.WithModel(prov))
	service := NewSessionService()
	rt, err := service.NewRuntime(t.Context(), team.New(team.WithAgents(root, librarian)),
		WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	return service, rt
}

// nodrowTransfer runs one transfer to a librarian child and returns its ID.
func nodrowTransfer(t *testing.T, store session.Store) (*SessionService, *LocalRuntime, string) {
	t.Helper()
	prov := &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("transferred").AddStopWithUsage(10, 5).Build()}
	librarian := agent.New("librarian", "Library agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov), agent.WithSubAgents(librarian))

	service := NewSessionService()
	rt, err := service.NewRuntime(t.Context(), team.New(team.WithAgents(root, librarian)),
		WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })

	parentID := t.Name()
	h, err := rt.CreateSession(t.Context(), session.New(session.WithID(parentID), session.WithUserMessage("Test"), session.WithToolsApproved(true)), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	parent := h.(*sessionHandle).driver.session()
	result, err := rt.handleTaskTransfer(t.Context(), parent, transferToolCall("librarian"), NewChannelSink(make(chan Event, 128)), tools.NopRuntime{})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var childID string
	rt.subagents.mu.Lock()
	for _, rec := range rt.subagents.children {
		if rec.parentSession == parentID {
			childID = rec.sessionID
		}
	}
	rt.subagents.mu.Unlock()
	require.NotEmpty(t, childID)
	return service, rt, childID
}

// nodrowStoredRow is an existing canonical row created by the given creation agent.
func nodrowStoredRow(id, title, creation string) *session.Session {
	row := session.New(session.WithID(id), session.WithTitle(title), session.WithAgentName("root"))
	row.SetAttribute(SessionAgentAttribute, creation)
	return row
}

func nodrowSession(id string) *session.Session {
	return session.New(session.WithID(id), session.WithAgentName("root"), session.WithUserMessage("hi"))
}

func nodrowDrain(ch <-chan Event) []Event {
	var events []Event
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

func nodrowErrorEvents(events []Event) []*ErrorEvent {
	var out []*ErrorEvent
	for _, ev := range events {
		if e, ok := ev.(*ErrorEvent); ok {
			out = append(out, e)
		}
	}
	return out
}

// Public Drive writes the first row before the driver becomes resident.
func TestDriveSavesRowBeforePublishingResident(t *testing.T) {
	t.Parallel()

	store := newNodrowStore()
	_, rt := nodrowRuntime(t, store)
	id := t.Name()

	var residentAtSave bool
	store.setBeforeAdd(func(_ context.Context, sess *session.Session) error {
		if sess.ID == id {
			_, residentAtSave = rt.sessionDrivers.Lookup(id)
		}
		return nil
	})
	type runStart struct{ resident, row bool }
	started := make(chan runStart, 1)
	var once sync.Once
	store.setOnUpdate(func(ctx context.Context, sess *session.Session) {
		if sess.ID != id {
			return
		}
		once.Do(func() {
			_, resident := rt.sessionDrivers.Lookup(id)
			_, err := store.InMemorySessionStore.GetSession(ctx, id)
			started <- runStart{resident: resident, row: err == nil}
		})
	})

	require.Empty(t, nodrowErrorEvents(nodrowDrain(rt.Drive(t.Context(), nodrowSession(id)))))
	require.False(t, residentAtSave)
	select {
	case w := <-started:
		require.True(t, w.resident)
		require.True(t, w.row)
	case <-time.After(5 * time.Second):
		t.Fatal("run-start write never happened")
	}
}

// A failed first save publishes nothing and reports a typed persistence error.
func TestDriveFailedFirstSaveLeavesNonResident(t *testing.T) {
	t.Parallel()

	store := newNodrowStore()
	_, rt := nodrowRuntime(t, store)
	id := t.Name()
	store.setBeforeAdd(func(context.Context, *session.Session) error {
		return errors.New("injected first save failure")
	})

	_, err := rt.sessionDrivers.GetInitialized(t.Context(), nodrowSession(id))
	sessErr, ok := errors.AsType[*SessionError](err)
	require.True(t, ok)
	require.Equal(t, SessionErrorPersistence, sessErr.Kind)
	require.Equal(t, SessionOperationRegister, sessErr.Operation)

	errEvents := nodrowErrorEvents(nodrowDrain(rt.Drive(t.Context(), nodrowSession(id))))
	require.Len(t, errEvents, 1)
	require.Contains(t, errEvents[0].Error, "session register: persistence")

	_, resident := rt.sessionDrivers.Lookup(id)
	require.False(t, resident)
	_, err = store.InMemorySessionStore.GetSession(t.Context(), id)
	require.ErrorIs(t, err, session.ErrNotFound)
	require.Zero(t, store.updateCount())

	store.setBeforeAdd(nil)
	require.Empty(t, nodrowErrorEvents(nodrowDrain(rt.Drive(t.Context(), nodrowSession(id)))))
	_, resident = rt.sessionDrivers.Lookup(id)
	require.True(t, resident)
}

// A successful concurrent CreateSession adopts the row this call wrote. When this
// publication then fails, the row and the adopted driver both remain.
func TestConcurrentCreateSessionKeepsAdoptedRowWhenFirstPublishFails(t *testing.T) {
	t.Parallel()

	store := newNodrowStore()
	_, rt := nodrowRuntime(t, store)
	id := t.Name()
	var created SessionHandle
	var createErr error
	store.setAfterAdd(func(context.Context, *session.Session) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			created, createErr = rt.CreateSession(t.Context(), session.New(session.WithID(id)), SessionBinding{AgentName: "root"})
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("concurrent CreateSession did not finish")
		}
		rt.sessionDrivers.mu.Lock()
		if rt.sessionDrivers.stoppedTrees == nil {
			rt.sessionDrivers.stoppedTrees = make(map[string]struct{})
		}
		rt.sessionDrivers.stoppedTrees[id] = struct{}{}
		rt.sessionDrivers.mu.Unlock()
	})

	_, err := rt.sessionDrivers.GetInitialized(t.Context(), nodrowSession(id))
	sessErr, ok := errors.AsType[*SessionError](err)
	require.True(t, ok)
	require.Equal(t, SessionErrorStopped, sessErr.Kind)
	require.NoError(t, createErr)
	require.NotNil(t, created)

	adopted, resident := rt.sessionDrivers.Lookup(id)
	require.True(t, resident)
	require.Same(t, created.(*sessionHandle).driver, adopted)
	_, err = store.InMemorySessionStore.GetSession(t.Context(), id)
	require.NoError(t, err)
}

// An explicit delete during a first-time save leaves no row behind.
func TestDeleteDuringFirstSaveLeavesNoRow(t *testing.T) {
	t.Parallel()

	store := newNodrowStore()
	_, rt := nodrowRuntime(t, store)
	id := t.Name()
	var deleteErr error
	done := make(chan struct{})
	store.setBeforeAdd(func(context.Context, *session.Session) error {
		go func() {
			defer close(done)
			deleteErr = rt.DeleteSession(t.Context(), id)
		}()
		// Unfenced delete finishes here; a fenced delete waits for this save.
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
		}
		return nil
	})

	_, _ = rt.sessionDrivers.GetInitialized(t.Context(), nodrowSession(id))
	<-done
	require.NoError(t, deleteErr)
	_, resident := rt.sessionDrivers.Lookup(id)
	require.False(t, resident)
	_, err := store.InMemorySessionStore.GetSession(t.Context(), id)
	require.ErrorIs(t, err, session.ErrNotFound)
}

// A template naming another creation agent conflicts with the stored row; the
// row is untouched and nothing becomes resident.
func TestDriveExistingRowCreationMismatchRejected(t *testing.T) {
	t.Parallel()

	store := newNodrowStore()
	_, rt := nodrowRuntime(t, store)
	id := t.Name()
	require.NoError(t, store.InMemorySessionStore.AddSession(t.Context(), nodrowStoredRow(id, "stored", "root")))

	sess := nodrowSession(id)
	sess.SetAttribute(SessionAgentAttribute, "librarian")
	errEvents := nodrowErrorEvents(nodrowDrain(rt.Drive(t.Context(), sess)))
	require.Len(t, errEvents, 1)
	require.Contains(t, errEvents[0].Error, "session register: conflict")

	_, resident := rt.sessionDrivers.Lookup(id)
	require.False(t, resident)
	require.Zero(t, store.updateCount())
	row, err := store.InMemorySessionStore.GetSession(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "stored", row.Title)
	require.Equal(t, "root", row.AttributesSnapshot()[SessionAgentAttribute])
}

const nodrowActorSource = "docker-agent.actor.source"

// An existing row's canonical identity wins over the caller's: the active agent
// and provenance come from the row, and the row keeps them after the run and
// remains attachable.
func TestDriveExistingRowAdoptsStoredCreationAndTitle(t *testing.T) {
	t.Parallel()

	store := newNodrowStore()
	service, rt := nodrowRuntime(t, store)
	id := t.Name()
	stored := nodrowStoredRow(id, "stored", "root")
	stored.SetAttribute(nodrowActorSource, "human")
	require.NoError(t, store.InMemorySessionStore.AddSession(t.Context(), stored))

	sess := session.New(session.WithID(id), session.WithAgentName("librarian"), session.WithUserMessage("hi"))
	sess.SetAttribute(nodrowActorSource, "other")
	require.Empty(t, nodrowErrorEvents(nodrowDrain(rt.Drive(t.Context(), sess))))

	row, err := store.InMemorySessionStore.GetSession(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "stored", row.Title)
	require.Equal(t, "root", row.AgentName)
	require.Equal(t, "root", row.AttributesSnapshot()[SessionAgentAttribute])
	require.Equal(t, "human", row.AttributesSnapshot()[nodrowActorSource])
	_, snapshot, err := service.LoadSession(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, id, snapshot.ID)
}

// A row created between the identity read and the first write is adopted under the
// same creation rule. A conflicting template is rejected; nothing is rewritten.
func TestDriveFirstWriteRaceAdoptsCanonicalRow(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		template string
	}{
		{name: "absent-template"},
		{name: "creation-mismatch", template: "librarian"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := newNodrowStore()
			_, rt := nodrowRuntime(t, store)
			id := t.Name()
			store.setBeforeAdd(func(ctx context.Context, _ *session.Session) error {
				stored := nodrowStoredRow(id, "stored", "root")
				stored.SetAttribute(nodrowActorSource, "human")
				return store.InMemorySessionStore.AddSession(ctx, stored)
			})

			sess := session.New(session.WithID(id), session.WithAgentName("librarian"), session.WithUserMessage("hi"))
			if tc.template != "" {
				sess.SetAttribute(SessionAgentAttribute, tc.template)
			}
			events := nodrowErrorEvents(nodrowDrain(rt.Drive(t.Context(), sess)))
			_, resident := rt.sessionDrivers.Lookup(id)
			row, err := store.InMemorySessionStore.GetSession(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, "stored", row.Title)
			require.Equal(t, "root", row.AttributesSnapshot()[SessionAgentAttribute])
			require.Equal(t, "human", row.AttributesSnapshot()[nodrowActorSource])

			if tc.template != "" {
				require.Len(t, events, 1)
				require.Contains(t, events[0].Error, "session register: conflict")
				require.False(t, resident)
				require.Zero(t, store.updateCount())
				return
			}
			require.Empty(t, events)
			require.True(t, resident)
			require.Equal(t, "root", row.AgentName)
		})
	}
}

// A delete whose context ends while a first-time save holds the fence fails
// without tombstoning; a later delete still removes the session.
func TestDeleteFenceHonorsContext(t *testing.T) {
	t.Parallel()

	store := newNodrowStore()
	_, rt := nodrowRuntime(t, store)
	id := t.Name()
	entered, release := make(chan struct{}), make(chan struct{})
	store.setBeforeAdd(func(context.Context, *session.Session) error {
		close(entered)
		<-release
		return nil
	})
	driveDone := make(chan []Event, 1)
	go func() { driveDone <- nodrowDrain(rt.Drive(t.Context(), nodrowSession(id))) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first save never started")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, rt.DeleteSession(ctx, id), context.DeadlineExceeded)

	close(release)
	require.Empty(t, nodrowErrorEvents(<-driveDone))
	require.NoError(t, rt.DeleteSession(t.Context(), id))
	_, resident := rt.sessionDrivers.Lookup(id)
	require.False(t, resident)
	_, err := store.InMemorySessionStore.GetSession(t.Context(), id)
	require.ErrorIs(t, err, session.ErrNotFound)
}

// An existing stored row is kept; first use only publishes the driver.
func TestDrivePreservesExistingStoredRow(t *testing.T) {
	t.Parallel()

	store := newNodrowStore()
	_, rt := nodrowRuntime(t, store)
	id := t.Name()
	require.NoError(t, store.InMemorySessionStore.AddSession(t.Context(), session.New(session.WithID(id), session.WithTitle("stored"), session.WithAgentName("root"))))

	_, err := rt.sessionDrivers.GetInitialized(t.Context(), session.New(session.WithID(id), session.WithTitle("caller"), session.WithAgentName("root")))
	require.NoError(t, err)
	row, err := store.InMemorySessionStore.GetSession(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "stored", row.Title)
}

// While the first save is blocked, the ID is neither resident nor attachable.
func TestDriveBlockedFirstSaveIsNotAttachable(t *testing.T) {
	t.Parallel()

	store := newNodrowStore()
	service, rt := nodrowRuntime(t, store)
	id := t.Name()
	entered, release := make(chan struct{}), make(chan struct{})
	store.setBeforeAdd(func(context.Context, *session.Session) error {
		close(entered)
		<-release
		return nil
	})

	done := make(chan []Event, 1)
	go func() { done <- nodrowDrain(rt.Drive(t.Context(), nodrowSession(id))) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first save never started")
	}
	_, resident := rt.sessionDrivers.Lookup(id)
	require.False(t, resident)
	_, _, err := service.LoadSession(t.Context(), id)
	require.Error(t, err)

	close(release)
	require.Empty(t, nodrowErrorEvents(<-done))
	_, resident = rt.sessionDrivers.Lookup(id)
	require.True(t, resident)
}

// A store without CoordinationStore keeps membership only in the volatile
// coordination store, so the live child record supplies it on attach.
func TestCustomStoreTransferChildAttachesFromLiveRecord(t *testing.T) {
	t.Parallel()

	inner := session.NewInMemorySessionStore().(*session.InMemorySessionStore)
	store := &nodrowPlainStore{Store: inner, inner: inner}
	service, rt, childID := nodrowTransfer(t, store)

	resident, ok := rt.sessionDrivers.Lookup(childID)
	require.True(t, ok)
	_, err := inner.LoadChild(t.Context(), childID)
	require.ErrorIs(t, err, session.ErrNotFound)
	handle, snapshot, err := service.LoadSession(t.Context(), childID)
	require.NoError(t, err)
	require.Equal(t, childID, snapshot.ID)
	require.Same(t, resident, handle.(*sessionHandle).driver)
}

// A store with only session.Store attaches a transfer child from the live record.
func TestMinimalStoreTransferChildAttachesFromLiveRecord(t *testing.T) {
	t.Parallel()

	service, rt, childID := nodrowTransfer(t, &nodrowMinimalStore{Store: session.NewInMemorySessionStore()})
	resident, ok := rt.sessionDrivers.Lookup(childID)
	require.True(t, ok)
	handle, snapshot, err := service.LoadSession(t.Context(), childID)
	require.NoError(t, err)
	require.Equal(t, childID, snapshot.ID)
	require.Same(t, resident, handle.(*sessionHandle).driver)
}

// A native store's deleted child row stays rejected.
func TestNativeDeletedChildRowRejected(t *testing.T) {
	t.Parallel()

	store := newNodrowStore()
	service, _, childID := nodrowTransfer(t, store)
	require.NoError(t, store.InMemorySessionStore.DeleteSession(t.Context(), childID))
	_, _, err := service.LoadSession(t.Context(), childID)
	require.Error(t, err)
}

// A native store's missing child record stays rejected; the live fallback is non-native only.
func TestNativeMissingChildRecordRejected(t *testing.T) {
	t.Parallel()

	store := &nodrowNativeStore{InMemorySessionStore: session.NewInMemorySessionStore().(*session.InMemorySessionStore)}
	service, rt, childID := nodrowTransfer(t, store)
	store.hideRecords.Store(true)
	_, _, err := service.LoadSession(t.Context(), childID)
	require.ErrorIs(t, err, session.ErrNotFound)
	_, resident := rt.sessionDrivers.Lookup(childID)
	require.True(t, resident)
}
