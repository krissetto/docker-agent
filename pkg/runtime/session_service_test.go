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
