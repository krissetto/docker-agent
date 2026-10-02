package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestInterruptedReleaseCanReplaceStoppedGenerationAtCapacity(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	p := coordinationReply("ok")
	p.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
		close(entered)
		<-release
		return newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build(), nil
	}
	policy := DefaultSessionResourcePolicy()
	policy.MaxSessions = 1
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(p)))), WithSessionResourcePolicy(policy))
	require.NoError(t, err)
	t.Cleanup(func() { unblock(); require.NoError(t, rt.Close()) })
	sess := session.New(session.WithID("same-id"))
	stale, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	_, err = stale.Submit(t.Context(), TurnInput{Content: "run"})
	require.NoError(t, err)
	coordinationWait(t, entered)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, rt.ReleaseSession(canceled, stale.ID()), context.Canceled)
	unblock()
	old := stale.(*sessionHandle).driver
	old.Wait()
	retained, ok := rt.sessionDrivers.Lookup(stale.ID())
	require.True(t, ok)
	require.Same(t, old, retained)
	require.True(t, old.stoppedAndSettled())
	current, err := rt.CreateSession(t.Context(), sess.Clone(), SessionBinding{})
	require.NoError(t, err, "replacement needs no additional resident slot")
	require.NotSame(t, old, current.(*sessionHandle).driver)
	_, err = stale.Submit(t.Context(), TurnInput{Content: "stale"})
	require.ErrorIs(t, err, ErrSessionStopped)
}

func TestStoppedGenerationAdmissionFailurePreservesObservation(t *testing.T) {
	rt := newPressureRuntime(t, 1)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	sess := session.New(session.WithID("stopped"))
	h, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	d := h.(*sessionHandle).driver
	d.StopAll()
	d.Wait()
	require.True(t, d.stoppedAndSettled())
	_, _, cancel := rt.sessionEvents.Subscribe(sess.ID, 8)
	defer cancel()
	// A detached restore reservation consumes the only available capacity.
	rt.sessionDrivers.mu.Lock()
	rt.sessionDrivers.reservations["reserved"] = &restoreDriverReservation{}
	rt.sessionDrivers.mu.Unlock()
	defer func() {
		rt.sessionDrivers.mu.Lock()
		delete(rt.sessionDrivers.reservations, "reserved")
		rt.sessionDrivers.mu.Unlock()
	}()
	_, err = rt.CreateSession(t.Context(), sess.Clone(), SessionBinding{})
	var capacity *SessionError
	require.ErrorAs(t, err, &capacity)
	assert.Equal(t, SessionErrorCapacity, capacity.Kind)
	assert.True(t, rt.sessionEvents.HasSubscribers(sess.ID))
	retained, ok := rt.sessionDrivers.Lookup(sess.ID)
	require.True(t, ok)
	require.Same(t, d, retained)
}

func TestStoppedGenerationReplacementDoesNotEvictAnotherSession(t *testing.T) {
	rt := newPressureRuntime(t, 2)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	sess := session.New(session.WithID("replaced"))
	old, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	other, err := rt.CreateSession(t.Context(), session.New(session.WithID("other")), SessionBinding{})
	require.NoError(t, err)
	old.(*sessionHandle).driver.StopAll()
	old.(*sessionHandle).driver.Wait()
	_, err = rt.CreateSession(t.Context(), sess.Clone(), SessionBinding{})
	require.NoError(t, err)
	retained, ok := rt.sessionDrivers.Lookup(other.ID())
	require.True(t, ok, "net-zero replacement must not evict an unrelated resident")
	require.Same(t, other.(*sessionHandle).driver, retained)
}
