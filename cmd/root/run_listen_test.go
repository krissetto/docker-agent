package root

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type coordinatorSessionRuntime struct {
	created runtime.SessionBinding
}

func (r *coordinatorSessionRuntime) CreateSession(_ context.Context, _ *session.Session, binding runtime.SessionBinding) (runtime.SessionHandle, error) {
	r.created = binding
	return nil, nil
}

func (*coordinatorSessionRuntime) SessionByID(string) (runtime.SessionHandle, error) {
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, Operation: "lookup"}
}
func (*coordinatorSessionRuntime) DeleteSession(context.Context, string) error { return nil }

func TestStartSessionCoordinatorWithoutListenDoesNotCreateClassicWiring(t *testing.T) {
	sessions := &coordinatorSessionRuntime{}
	flags := &runExecFlags{}
	require.NoError(t, flags.startSessionCoordinator(t.Context(), nil, sessions, session.NewInMemorySessionStore(), session.New()))
	require.Nil(t, flags.listenSM)
}

type namedSession struct {
	runtime.SessionHandle

	id string
}

func (a namedSession) ID() string { return a.id }

type sessionSet map[string]namedSession

func (s sessionSet) CreateSession(_ context.Context, sess *session.Session, binding runtime.SessionBinding) (runtime.SessionHandle, error) {
	if binding.ParentSessionID != "" {
		if _, ok := s[binding.ParentSessionID]; !ok {
			return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: binding.ParentSessionID}
		}
	}
	a := namedSession{id: sess.ID}
	s[sess.ID] = a
	return a, nil
}

func (s sessionSet) SessionByID(id string) (runtime.SessionHandle, error) {
	if a, ok := s[id]; ok {
		return a, nil
	}
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id, Operation: "lookup"}
}

func (s sessionSet) DeleteSession(_ context.Context, id string) error {
	delete(s, id)
	return nil
}

// TestControlPlaneSessionsReachTabsOnOtherRuntimes pins what --listen exposes:
// sessions are created on the shared runtime, while sessions of tabs that
// run on their own runtime remain reachable (and deletable where they live).
func TestControlPlaneSessionsReachTabsOnOtherRuntimes(t *testing.T) {
	shared, other := sessionSet{}, sessionSet{}
	registry := newControlPlaneSessions(shared)
	registry.Add(other)

	created, err := registry.CreateSession(t.Context(), session.New(session.WithID("shared-1")), runtime.SessionBinding{})
	require.NoError(t, err)
	require.Equal(t, "shared-1", created.ID())
	_, ok := shared["shared-1"]
	require.True(t, ok, "new sessions land on the shared runtime")

	_, err = other.CreateSession(t.Context(), session.New(session.WithID("tab-2")), runtime.SessionBinding{})
	require.NoError(t, err)
	found, err := registry.SessionByID("tab-2")
	require.NoError(t, err)
	require.Equal(t, "tab-2", found.ID())

	child, err := registry.CreateSession(t.Context(), session.New(session.WithID("child")), runtime.SessionBinding{ParentSessionID: "tab-2"})
	require.NoError(t, err)
	require.Equal(t, "child", child.ID())
	_, inOther := other["child"]
	require.True(t, inOther, "parent-bound creation follows the parent runtime")
	_, inShared := shared["child"]
	require.False(t, inShared)

	require.NoError(t, registry.DeleteSession(t.Context(), "tab-2"))
	_, ok = other["tab-2"]
	require.False(t, ok, "deletes reach the runtime that owns the session")
	_, err = registry.SessionByID("missing")
	require.Error(t, err)
}
