package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
)

func serviceRuntime(t *testing.T, service *SessionService, store session.Store) *LocalRuntime {
	t.Helper()
	tm := team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(&multiRunProvider{mockProvider: &mockProvider{id: "test/service"}, build: func() chat.MessageStream { return newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build() }}))))
	r, err := service.NewRuntime(t.Context(), tm, WithSessionStore(store))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	return r
}

func TestSessionServiceExclusiveProcessOwner(t *testing.T) {
	first := NewSessionService()
	second := NewSessionService()
	store := session.NewInMemorySessionStore()
	r1 := serviceRuntime(t, first, store)
	r2 := serviceRuntime(t, second, store)
	id := t.Name()
	h, err := r1.CreateSession(t.Context(), session.New(session.WithID(id)), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	_, err = r2.CreateSession(t.Context(), session.New(session.WithID(id)), SessionBinding{AgentName: "root"})
	var conflict *SessionError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, SessionErrorConflict, conflict.Kind)
	routed, err := first.Runtime().SessionByID(id)
	require.NoError(t, err)
	require.Equal(t, h.ID(), routed.ID())
	_, hasShutdown := first.Runtime().(interface {
		Shutdown(ctx context.Context) error
	})
	require.False(t, hasShutdown)
	require.NoError(t, r1.Close())
	loaded, _, err := second.LoadSession(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, id, loaded.ID())
}

func TestSessionServiceAggregateAdmissionAndRelease(t *testing.T) {
	service := NewSessionService(SessionServiceOptions{MaxSessions: 1})
	r1 := serviceRuntime(t, service, session.NewInMemorySessionStore())
	r2 := serviceRuntime(t, service, session.NewInMemorySessionStore())
	firstID, secondID := t.Name()+"/first", t.Name()+"/second"
	h, err := r1.CreateSession(t.Context(), session.New(session.WithID(firstID)), SessionBinding{})
	require.NoError(t, err)
	observation, err := h.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	_, err = r2.CreateSession(t.Context(), session.New(session.WithID(secondID)), SessionBinding{})
	require.ErrorIs(t, err, ErrSessionCapacity)
	require.NoError(t, h.Release(t.Context()))
	_, err = r2.CreateSession(t.Context(), session.New(session.WithID(secondID)), SessionBinding{})
	require.NoError(t, err)
}

func TestSessionServiceConcurrentLoadSharesDriver(t *testing.T) {
	service := NewSessionService()
	store := session.NewInMemorySessionStore()
	serviceRuntime(t, service, store)
	id := t.Name()
	require.NoError(t, store.AddSession(t.Context(), session.New(session.WithID(id), session.WithAgentName("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))))
	var wg sync.WaitGroup
	handles := make([]SessionHandle, 8)
	errs := make([]error, len(handles))
	for i := range handles {
		wg.Go(func() { handles[i], _, errs[i] = service.LoadSession(t.Context(), id) })
	}
	wg.Wait()
	for i := range handles {
		require.NoError(t, errs[i])
		require.Same(t, handles[0].(*sessionHandle).driver, handles[i].(*sessionHandle).driver)
	}
	require.NoError(t, service.Shutdown(t.Context()))
	_, err := service.Runtime().SessionByID(id)
	require.ErrorIs(t, err, ErrSessionClosed)
}

func TestSessionServiceFailedCreateReleasesClaim(t *testing.T) {
	first, second := NewSessionService(), NewSessionService()
	r1 := serviceRuntime(t, first, session.NewInMemorySessionStore())
	r2 := serviceRuntime(t, second, session.NewInMemorySessionStore())
	id := t.Name()
	_, err := r1.CreateSession(t.Context(), session.New(session.WithID(id)), SessionBinding{AgentName: "missing"})
	require.Error(t, err)
	_, err = r2.CreateSession(t.Context(), session.New(session.WithID(id)), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
}

func TestSessionServiceRootStopFencesSpawnAndPreservesRows(t *testing.T) {
	service := NewSessionService()
	store := session.NewInMemorySessionStore()
	r := serviceRuntime(t, service, store)
	id := t.Name()
	h, err := r.CreateSession(t.Context(), session.New(session.WithID(id), session.WithUserMessage("retain history")), SessionBinding{})
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_, _ = r.subagents.Spawn(h.(*sessionHandle).driver.session(), "root", subagent.AllowedSubagent{Agent: "root"}, "race")
		})
	}
	require.NoError(t, h.(SessionTreeController).StopSubtree(t.Context()))
	wg.Wait()
	_, err = r.subagents.Spawn(h.(*sessionHandle).driver.session(), "root", subagent.AllowedSubagent{Agent: "root"}, "after fence")
	require.Error(t, err)
	_, err = r.CreateSession(t.Context(), session.New(session.WithID(id)), SessionBinding{})
	require.ErrorIs(t, err, ErrSessionStopped)
	persisted, err := store.GetSession(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "retain history", persisted.MessagesSnapshot()[0].Message.Message.Content)
}

func TestSessionServiceRootStopDeadlineRetainsFenceAndRetries(t *testing.T) {
	service := NewSessionService()
	store := session.NewInMemorySessionStore()
	r := serviceRuntime(t, service, store)
	h, err := r.CreateSession(t.Context(), session.New(session.WithID(t.Name())), SessionBinding{})
	require.NoError(t, err)
	d := h.(*sessionHandle).driver
	d.wg.Add(1)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err = h.(SessionTreeController).StopSubtree(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	_, err = r.subagents.Spawn(d.session(), "root", subagent.AllowedSubagent{Agent: "root"}, "must remain fenced")
	require.Error(t, err)
	d.wg.Done()
	require.NoError(t, h.(SessionTreeController).StopSubtree(t.Context()))
	_, err = store.GetSession(t.Context(), h.ID())
	require.NoError(t, err)
}

func TestSessionServiceResidentLoadDoesNotRequireStoredRow(t *testing.T) {
	service := NewSessionService()
	store := session.NewInMemorySessionStore()
	r := serviceRuntime(t, service, store)
	sess := session.New(session.WithID(t.Name()), session.WithAgentName("root"))
	driver, err := r.sessionDrivers.GetInitialized(t.Context(), sess)
	require.NoError(t, err)
	_, err = store.GetSession(t.Context(), sess.ID)
	require.ErrorIs(t, err, session.ErrNotFound)
	handle, snapshot, err := service.LoadSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, sess.ID, snapshot.ID)
	require.Same(t, driver, handle.(*sessionHandle).driver)
}

func TestSessionServiceClosedRuntimeDoesNotRemainRouteCandidate(t *testing.T) {
	service := NewSessionService()
	store := session.NewInMemorySessionStore()
	first := serviceRuntime(t, service, store)
	id := t.Name()
	_, err := first.CreateSession(t.Context(), session.New(session.WithID(id)), SessionBinding{})
	require.NoError(t, err)
	require.NoError(t, first.Close())
	second := serviceRuntime(t, service, store)
	handle, _, err := service.LoadSession(t.Context(), id)
	require.NoError(t, err)
	require.Same(t, second, handle.(*sessionHandle).runtime)
	fresh, err := service.Runtime().CreateSession(t.Context(), session.New(session.WithID(id+"/fresh")), SessionBinding{})
	require.NoError(t, err)
	require.Same(t, second, fresh.(*sessionHandle).runtime)
}

func TestSessionServiceReclaimsForAggregateAdmission(t *testing.T) {
	for _, crossRuntime := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-runtime", true: "other-runtime"}[crossRuntime], func(t *testing.T) {
			service := NewSessionService(SessionServiceOptions{MaxSessions: 1})
			first := serviceRuntime(t, service, session.NewInMemorySessionStore())
			second := first
			if crossRuntime {
				second = serviceRuntime(t, service, session.NewInMemorySessionStore())
			}
			oldID, newID := t.Name()+"/old", t.Name()+"/new"
			old, err := first.CreateSession(t.Context(), session.New(session.WithID(oldID)), SessionBinding{})
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				_, err = second.CreateSession(t.Context(), session.New(session.WithID(newID)), SessionBinding{})
				return err == nil
			}, time.Second, time.Millisecond)
			_, err = service.SessionByID(oldID)
			require.ErrorIs(t, err, &SessionError{Kind: SessionErrorNotFound})
			_, err = old.Observe(t.Context(), ObserveOptions{})
			require.ErrorIs(t, err, ErrSessionStopped)
			_, err = service.SessionByID(newID)
			require.NoError(t, err)
		})
	}
}

func TestSessionServiceReclaimPreservesPendingClaimsAndReservations(t *testing.T) {
	service := NewSessionService(SessionServiceOptions{MaxSessions: 1})
	r := serviceRuntime(t, service, session.NewInMemorySessionStore())
	id := t.Name() + "/resident"
	_, err := r.CreateSession(t.Context(), session.New(session.WithID(id)), SessionBinding{})
	require.NoError(t, err)
	require.NoError(t, r.sessionDrivers.beginClaim(id))
	_, err = r.CreateSession(t.Context(), session.New(session.WithID(t.Name()+"/blocked")), SessionBinding{})
	require.ErrorIs(t, err, ErrSessionCapacity)
	_, err = service.SessionByID(id)
	require.NoError(t, err)
	r.sessionDrivers.releaseUnpublishedClaim(id)

	var reservation *restoreDriverReservation
	require.Eventually(t, func() bool {
		reservation, err = r.sessionDrivers.PrepareRestore(t.Context(), session.New(session.WithID(t.Name()+"/reserved"), session.WithAgentName("root")))
		return err == nil
	}, time.Second, time.Millisecond)
	defer reservation.Discard()
	_, err = r.CreateSession(t.Context(), session.New(session.WithID(t.Name()+"/blocked-again")), SessionBinding{})
	require.ErrorIs(t, err, ErrSessionCapacity)
	reservation.Discard()
	_, err = r.CreateSession(t.Context(), session.New(session.WithID(t.Name()+"/admitted")), SessionBinding{})
	require.NoError(t, err)
}

func TestSessionServiceConcurrentClaimsRespectAggregateCapacity(t *testing.T) {
	service := NewSessionService(SessionServiceOptions{MaxSessions: 1})
	first := serviceRuntime(t, service, session.NewInMemorySessionStore())
	second := serviceRuntime(t, service, session.NewInMemorySessionStore())
	const count = 16
	errs := make([]error, count)
	ids := make([]string, count)
	runtimes := make([]*LocalRuntime, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range count {
		ids[i] = session.New().ID
		runtimes[i] = first
		if i%2 != 0 {
			runtimes[i] = second
		}
		wg.Go(func() {
			<-start
			errs[i] = runtimes[i].sessionDrivers.beginClaim(ids[i])
		})
	}
	close(start)
	wg.Wait()
	admitted := 0
	for i, err := range errs {
		if err == nil {
			admitted++
			runtimes[i].sessionDrivers.releaseUnpublishedClaim(ids[i])
		} else {
			require.ErrorIs(t, err, ErrSessionCapacity)
		}
	}
	require.Equal(t, 1, admitted)
	_, err := first.CreateSession(t.Context(), session.New(session.WithID(t.Name())), SessionBinding{})
	require.NoError(t, err)
}

func TestSessionServiceConcurrentClaimsPreserveProcessExclusivity(t *testing.T) {
	first := serviceRuntime(t, NewSessionService(), session.NewInMemorySessionStore())
	second := serviceRuntime(t, NewSessionService(), session.NewInMemorySessionStore())
	id := t.Name()
	const count = 16
	errs := make([]error, count)
	runtimes := make([]*LocalRuntime, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range count {
		runtimes[i] = first
		if i%2 != 0 {
			runtimes[i] = second
		}
		wg.Go(func() {
			<-start
			errs[i] = runtimes[i].sessionDrivers.beginClaim(id)
		})
	}
	close(start)
	wg.Wait()
	var owner *LocalRuntime
	for i, err := range errs {
		if err == nil {
			if owner == nil {
				owner = runtimes[i]
			}
			require.Same(t, owner, runtimes[i])
		} else {
			require.ErrorIs(t, err, &SessionError{Kind: SessionErrorConflict})
		}
	}
	require.NotNil(t, owner)
	for i, err := range errs {
		if err == nil {
			runtimes[i].sessionDrivers.releaseUnpublishedClaim(id)
		}
	}
	_, err := second.CreateSession(t.Context(), session.New(session.WithID(id)), SessionBinding{})
	require.NoError(t, err)
}

func TestSessionServiceAggregateReclaimDoesNotWaitForSessionIO(t *testing.T) {
	service := NewSessionService(SessionServiceOptions{MaxSessions: 1})
	first := serviceRuntime(t, service, session.NewInMemorySessionStore())
	second := serviceRuntime(t, service, session.NewInMemorySessionStore())
	handle, err := first.CreateSession(t.Context(), session.New(session.WithID(t.Name()+"/busy")), SessionBinding{})
	require.NoError(t, err)
	driver := handle.(*sessionHandle).driver
	driver.mu.Lock()
	defer driver.mu.Unlock()
	result := make(chan error, 1)
	go func() {
		_, err := second.CreateSession(t.Context(), session.New(session.WithID(t.Name()+"/new")), SessionBinding{})
		result <- err
	}()
	select {
	case err := <-result:
		require.ErrorIs(t, err, ErrSessionCapacity)
	case <-time.After(time.Second):
		t.Fatal("aggregate admission waited for a session lock")
	}
}

func TestSessionRestoreReservationPinsRuntimeCapacity(t *testing.T) {
	r := serviceRuntime(t, NewSessionService(SessionServiceOptions{MaxSessions: -1}), session.NewInMemorySessionStore())
	r.maxSessions = 1
	reservation, err := r.sessionDrivers.PrepareRestore(t.Context(), session.New(session.WithID(t.Name()+"/reserved"), session.WithAgentName("root")))
	require.NoError(t, err)
	defer reservation.Discard()
	_, err = r.CreateSession(t.Context(), session.New(session.WithID(t.Name()+"/blocked")), SessionBinding{})
	require.ErrorIs(t, err, ErrSessionCapacity)
	_, err = r.sessionDrivers.PrepareRestore(t.Context(), session.New(session.WithID(t.Name()+"/also-blocked"), session.WithAgentName("root")))
	require.ErrorIs(t, err, ErrSessionCapacity)
	r.maxSessions = 0
	require.NoError(t, r.sessionDrivers.ActivateRestoreBatch([]*restoreDriverReservation{reservation}, nil))
	_, err = r.SessionByID(reservation.id)
	require.NoError(t, err)
}

func TestSessionRestoreReservationProtectsParentAtAggregateCapacity(t *testing.T) {
	service := NewSessionService(SessionServiceOptions{MaxSessions: 1})
	r := serviceRuntime(t, service, session.NewInMemorySessionStore())
	parentID := t.Name() + "/parent"
	_, err := r.CreateSession(t.Context(), session.New(session.WithID(parentID)), SessionBinding{})
	require.NoError(t, err)
	child := session.New(session.WithID(t.Name()+"/child"), session.WithAgentName("root"), session.WithParentID(parentID))
	_, err = r.sessionDrivers.PrepareRestore(t.Context(), child)
	require.ErrorIs(t, err, ErrSessionCapacity)
	_, err = service.SessionByID(parentID)
	require.NoError(t, err)
}
