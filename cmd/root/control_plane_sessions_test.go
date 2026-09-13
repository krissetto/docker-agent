package root

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type countingSessionRuntime struct {
	sessions map[string]runtime.SessionHandle
	lookups  int
}

func (r *countingSessionRuntime) CreateSession(_ context.Context, sess *session.Session, _ runtime.SessionBinding) (runtime.SessionHandle, error) {
	handle := namedSession{id: sess.ID}
	r.sessions[sess.ID] = handle
	return handle, nil
}

func (r *countingSessionRuntime) SessionByID(id string) (runtime.SessionHandle, error) {
	r.lookups++
	if handle, ok := r.sessions[id]; ok {
		return handle, nil
	}
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id, Operation: "lookup"}
}

func (r *countingSessionRuntime) DeleteSession(_ context.Context, id string) error {
	delete(r.sessions, id)
	return nil
}

type blockingLookupRuntime struct {
	started chan struct{}
	finish  chan struct{}
}

func (*blockingLookupRuntime) CreateSession(context.Context, *session.Session, runtime.SessionBinding) (runtime.SessionHandle, error) {
	return nil, nil
}

func (r *blockingLookupRuntime) SessionByID(string) (runtime.SessionHandle, error) {
	close(r.started)
	<-r.finish
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, Operation: "lookup"}
}

func (*blockingLookupRuntime) DeleteSession(context.Context, string) error { return nil }

func TestControlPlaneUnregisterDrainsBorrowedLookup(t *testing.T) {
	primary := &countingSessionRuntime{sessions: map[string]runtime.SessionHandle{}}
	blocking := &blockingLookupRuntime{started: make(chan struct{}), finish: make(chan struct{})}
	registry := newControlPlaneSessions(primary)
	unregister := registry.Add(blocking)
	lookupDone := make(chan struct{})
	go func() {
		_, _ = registry.SessionByID("missing")
		close(lookupDone)
	}()
	<-blocking.started
	unregisterDone := make(chan struct{})
	go func() { _ = unregister(t.Context()); close(unregisterDone) }()
	select {
	case <-unregisterDone:
		t.Fatal("unregister returned while a copied lookup still borrowed the runtime")
	case <-time.After(20 * time.Millisecond):
	}
	close(blocking.finish)
	select {
	case <-unregisterDone:
	case <-time.After(time.Second):
		t.Fatal("unregister did not drain after lookup completed")
	}
	<-lookupDone
}

func TestControlPlaneUnregisterWedgedLookupHonorsContext(t *testing.T) {
	primary := &countingSessionRuntime{sessions: map[string]runtime.SessionHandle{}}
	blocking := &blockingLookupRuntime{started: make(chan struct{}), finish: make(chan struct{})}
	registry := newControlPlaneSessions(primary)
	unregister := registry.Add(blocking)
	lookupDone := make(chan struct{})
	go func() { _, _ = registry.SessionByID("missing"); close(lookupDone) }()
	<-blocking.started
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, unregister(ctx), context.DeadlineExceeded)
	close(blocking.finish)
	<-lookupDone
	require.NoError(t, unregister(t.Context()), "a later cleanup can observe the completed drain")
}

func TestControlPlaneSessionsDeduplicatesAndCachesOwners(t *testing.T) {
	primary := &countingSessionRuntime{sessions: map[string]runtime.SessionHandle{}}
	extra := &countingSessionRuntime{sessions: map[string]runtime.SessionHandle{"other": namedSession{id: "other"}}}
	registry := newControlPlaneSessions(primary)
	unregister := registry.Add(extra)

	_, err := registry.SessionByID("other")
	require.NoError(t, err)
	firstPrimaryLookups, firstExtraLookups := primary.lookups, extra.lookups
	_, err = registry.SessionByID("other")
	require.NoError(t, err)
	require.Equal(t, firstPrimaryLookups, primary.lookups, "cached ownership avoids rescanning the primary")
	require.Equal(t, firstExtraLookups+1, extra.lookups, "the cached owner is queried directly")

	created, err := registry.CreateSession(t.Context(), session.New(session.WithID("child")), runtime.SessionBinding{ParentSessionID: "other"})
	require.NoError(t, err)
	require.Equal(t, "child", created.ID())
	before := primary.lookups
	_, err = registry.SessionByID("child")
	require.NoError(t, err)
	require.Equal(t, before, primary.lookups, "successful creation seeds the direct owner cache")
	require.NoError(t, registry.DeleteSession(t.Context(), "child"))
	registry.mu.RLock()
	_, cachedAfterDelete := registry.owners["child"]
	registry.mu.RUnlock()
	require.False(t, cachedAfterDelete, "successful deletion clears cached ownership")

	require.NoError(t, unregister(t.Context()))
	require.NoError(t, unregister(t.Context()))
	require.Empty(t, registry.extras, "unregister is idempotent and removes the runtime")
	registry.mu.RLock()
	_, cachedAfterUnregister := registry.owners["other"]
	registry.mu.RUnlock()
	require.False(t, cachedAfterUnregister, "unregister clears owners pointing at the removed runtime")
}
