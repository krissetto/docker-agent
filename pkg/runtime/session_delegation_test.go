package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

type delegationFailStore struct {
	session.Store

	fail bool
}

func (s *delegationFailStore) UpdateSession(ctx context.Context, sess *session.Session) error {
	if s.fail {
		return errors.New("delegation metadata write failed")
	}
	return s.Store.UpdateSession(ctx, sess)
}

func TestSessionDelegationRootIsolationInheritanceAndRestart(t *testing.T) {
	service := NewSessionService()
	store := session.NewInMemorySessionStore()
	r := serviceRuntime(t, service, store)
	r.SetUseSubagents(false)
	first, err := r.CreateSession(t.Context(), session.New(session.WithID(t.Name()+"/first")), SessionBinding{})
	require.NoError(t, err)
	second, err := r.CreateSession(t.Context(), session.New(session.WithID(t.Name()+"/second")), SessionBinding{})
	require.NoError(t, err)
	require.NoError(t, first.(SessionDelegationController).SetDelegationPolicy(t.Context(), true))
	enabled, err := second.(SessionDelegationController).DelegationPolicy(t.Context())
	require.NoError(t, err)
	require.False(t, enabled)
	id, err := r.subagents.Spawn(first.(*sessionHandle).driver.session(), "root", subagent.AllowedSubagent{Agent: "root"}, "accepted")
	require.NoError(t, err)
	rec, ok := r.subagents.Read(id)
	require.True(t, ok)
	child, err := r.SessionByID(rec.sessionID)
	require.NoError(t, err)
	enabled, err = child.(SessionDelegationController).DelegationPolicy(t.Context())
	require.NoError(t, err)
	require.True(t, enabled)
	require.NotEmpty(t, r.filterSessionDelegationTools(first.(*sessionHandle).driver.session(), subagent.Definitions()))
	require.NoError(t, child.(SessionDelegationController).SetDelegationPolicy(t.Context(), false))
	_, err = r.subagents.Spawn(child.(*sessionHandle).driver.session(), "root", subagent.AllowedSubagent{Agent: "root"}, "new nested work")
	require.ErrorIs(t, err, errSubagentsDisabled)
	r.subagents.mu.Lock()
	state := r.subagents.children[id].durable.Node.State
	r.subagents.mu.Unlock()
	require.NotEqual(t, subagent.NodeStopped, state)
	require.NoError(t, r.Close())
	next := serviceRuntime(t, service, store)
	restored, _, err := service.LoadSession(t.Context(), first.ID())
	require.NoError(t, err)
	enabled, err = restored.(SessionDelegationController).DelegationPolicy(t.Context())
	require.NoError(t, err)
	require.False(t, enabled)
	require.True(t, next.UseSubagents(), "persisted override must beat default true")
}

func TestSessionDelegationFailedRevocationDoesNotPublish(t *testing.T) {
	service := NewSessionService()
	store := &delegationFailStore{Store: session.NewInMemorySessionStore()}
	r := serviceRuntime(t, service, store)
	h, err := r.CreateSession(t.Context(), session.New(session.WithID(t.Name())), SessionBinding{})
	require.NoError(t, err)
	require.NoError(t, h.(SessionDelegationController).SetDelegationPolicy(t.Context(), true))
	store.fail = true
	require.Error(t, h.(SessionDelegationController).SetDelegationPolicy(t.Context(), false))
	enabled, err := h.(SessionDelegationController).DelegationPolicy(t.Context())
	require.NoError(t, err)
	require.True(t, enabled)
	stored, err := store.GetSession(t.Context(), h.ID())
	require.NoError(t, err)
	require.Equal(t, "true", stored.AttributesSnapshot()[SessionDelegationAttribute])
	store.fail = false
}

func TestSessionDelegationConcurrentChangeFencesFutureAdmission(t *testing.T) {
	service := NewSessionService()
	r := serviceRuntime(t, service, session.NewInMemorySessionStore())
	h, err := r.CreateSession(t.Context(), session.New(session.WithID(t.Name())), SessionBinding{})
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, _ = r.subagents.Spawn(h.(*sessionHandle).driver.session(), "root", subagent.AllowedSubagent{Agent: "root"}, "accepted before revoke")
		})
	}
	require.NoError(t, h.(SessionDelegationController).SetDelegationPolicy(t.Context(), false))
	wg.Wait()
	for range 8 {
		_, err = r.subagents.Spawn(h.(*sessionHandle).driver.session(), "root", subagent.AllowedSubagent{Agent: "root"}, "must reject")
		require.ErrorIs(t, err, errSubagentsDisabled)
	}
}

func TestSessionDelegationUnavailableAncestorFailsClosed(t *testing.T) {
	service := NewSessionService()
	r := serviceRuntime(t, service, session.NewInMemorySessionStore())
	child := session.New(session.WithID(t.Name()+"/child"), session.WithParentID(t.Name()+"/missing-root"))
	require.True(t, r.UseSubagents())
	require.False(t, r.sessionDelegationEnabled(child))
	_, err := r.subagents.Spawn(child, "root", subagent.AllowedSubagent{Agent: "root"}, "must not bypass root revocation")
	require.ErrorIs(t, err, errSubagentsDisabled)
	require.False(t, r.acceptLegacyDelegation(t.Context(), child))
}
