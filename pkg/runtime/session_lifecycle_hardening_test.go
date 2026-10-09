package runtime

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestDurableReservationRegisteredBeforeOwnerAcknowledgement(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := r.sessionDrivers.Get(session.New(session.WithID(t.Name())))
	entered, release := make(chan struct{}), make(chan struct{})
	reserved := make(chan error, 1)
	go func() {
		reserved <- d.ownerCall(t.Context(), func() error {
			_, err := d.reserveDurableIO(t.Context(), func() (sessionIOReservation, error) { return sessionIOReservation{}, nil })
			close(entered)
			<-release
			return err
		})
	}()
	<-entered
	tracked := true
	select {
	case <-d.Done():
		tracked = false
	default:
	}
	close(release)
	require.NoError(t, <-reserved)
	// Join the synthetic effect without a worker; production durableIO joins after commit.
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.ioReservations--; return nil }))
	d.wg.Done()
	require.True(t, tracked, "reserved effect must be tracked before owner acknowledgement")
}

type retirementTitleStore struct {
	session.Store

	write func()
}

func (s *retirementTitleStore) UpdateSessionTitle(context.Context, string, string) error {
	s.write()
	return nil
}

func TestPublicUpdateTitleReleaseNeverWritesAfterOwnerRetirement(t *testing.T) {
	for i := range 300 {
		r := &LocalRuntime{}
		g := newSessionDriverRegistry(r)
		r.sessionDrivers = g
		d := newSessionDriver(r, session.New(session.WithID(fmt.Sprintf("title-%d", i))))
		g.drivers[d.identityID] = d
		var late atomic.Bool
		r.sessionStore = &retirementTitleStore{write: func() {
			select {
			case <-d.ownerDone:
				late.Store(true)
			default:
			}
		}}
		h := &sessionHandle{runtime: r, driver: d, sessionID: d.identityID}
		start := make(chan struct{})
		title, released := make(chan error, 1), make(chan error, 1)
		go func() { <-start; title <- h.UpdateTitle(t.Context(), "new title") }()
		go func() { <-start; released <- h.Release(t.Context()) }()
		close(start)
		<-title
		require.NoError(t, <-released)
		require.False(t, late.Load())
	}
}

func TestSettledReplacementRejectsAcceptedSteeringAndBindingChanges(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := r.sessionDrivers.Get(session.New(session.WithID(t.Name())))
	h := &sessionHandle{runtime: r, driver: d, sessionID: d.identityID}
	before, err := d.ownerSnapshot(t.Context())
	require.NoError(t, err)
	for _, change := range []func(*session.Session){
		func(s *session.Session) { s.ParentID = "other" },
		func(s *session.Session) { s.AsyncSubagent = true },
		func(s *session.Session) { s.AgentName = "other" },
	} {
		next := before.Clone()
		change(next)
		require.False(t, ReplaceSettledSession(h, next))
	}
	require.True(t, ReplaceSettledSession(h, before))
	accepted, err := h.Steer(t.Context(), TurnInput{RequestID: "guidance", Content: "keep this"})
	require.NoError(t, err)
	require.False(t, ReplaceSettledSession(h, before))
	after, err := d.ownerSnapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, after.Messages, 1)
	require.Equal(t, accepted.TurnID, after.Messages[0].Message.TurnID)
	require.NoError(t, d.promoteInput(t.Context(), accepted.TurnID, nil))
	d.StopAll()
	d.Wait()
	d.closeOwner()
	require.False(t, ReplaceSettledSession(h, before), "owner failure must propagate")
	r.sessionDrivers.mu.Lock()
	defer r.sessionDrivers.mu.Unlock()
	delete(r.sessionDrivers.drivers, d.identityID)
}

func TestRetirementCleanupDeadlineRetainsSingleJoinTrackedAttempt(t *testing.T) {
	entered, release := make(chan context.Context, 1), make(chan struct{})
	var calls atomic.Int32
	resource := &retirementToolset{cleanup: func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			entered <- ctx
			<-release
		}
		return nil
	}}
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "", agent.WithToolSets(resource), agent.WithModel(&mockProvider{id: "test/cleanup"})))))
	require.NoError(t, err)
	h, err := r.CreateSession(t.Context(), session.New(session.WithID(t.Name())), SessionBinding{})
	require.NoError(t, err)
	d := h.(*sessionHandle).driver
	defer func() { require.NoError(t, r.Close()) }()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.Release(ctx) }()
	cleanupCtx := <-entered
	_, bounded := cleanupCtx.Deadline()
	require.True(t, bounded)
	require.ErrorIs(t, <-done, context.DeadlineExceeded)
	require.ErrorIs(t, cleanupCtx.Err(), context.DeadlineExceeded)
	select {
	case <-d.Done():
		t.Fatal("uncooperative cleanup lost join tracking")
	default:
	}
	select {
	case <-d.ownerDone:
		t.Fatal("uncooperative cleanup lost owner authority")
	default:
	}
	for range 2 {
		retryCtx, retryCancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		require.ErrorIs(t, h.Release(retryCtx), context.DeadlineExceeded)
		retryCancel()
	}
	_, err = r.CreateSession(t.Context(), session.New(session.WithID(h.ID())), SessionBinding{})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.EqualValues(t, 1, calls.Load())
	close(release)
	require.NoError(t, h.Release(t.Context()))
	require.EqualValues(t, 1, calls.Load())
}

func TestRetirementCleanupCooperatesWithShutdownDeadlineAndRetries(t *testing.T) {
	var calls atomic.Int32
	resource := &retirementToolset{cleanup: func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}}
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "", agent.WithToolSets(resource), agent.WithModel(&mockProvider{id: "test/cleanup"})))))
	require.NoError(t, err)
	_, err = r.CreateSession(t.Context(), session.New(session.WithID(t.Name())), SessionBinding{})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, NewSessionRuntimeSupervisor(r).Shutdown(ctx), context.DeadlineExceeded)
	require.NoError(t, NewSessionRuntimeSupervisor(r).Shutdown(t.Context()))
	require.EqualValues(t, 2, calls.Load())
}

func TestSettledReplacementRejectsReservedDurableEffect(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := r.sessionDrivers.Get(session.New(session.WithID(t.Name())))
	before, err := d.ownerSnapshot(t.Context())
	require.NoError(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- d.durableIO(t.Context(), func() (sessionIOReservation, error) {
			return sessionIOReservation{write: func(context.Context) error {
				close(entered)
				<-release
				return nil
			}, commit: func(err error) error { d.sess.SetTitle("accepted effect"); return err }}, nil
		})
	}()
	<-entered
	require.False(t, r.sessionDrivers.ReplaceSettledSession(d.identityID, d, before))
	close(release)
	require.NoError(t, <-done)
	after, err := d.ownerSnapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, "accepted effect", after.TitleSnapshot())
}

func TestConcurrentRetirementJoinsSameOwnerAndCleanupOnce(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	resource := &retirementToolset{cleanup: func(context.Context) error {
		calls.Add(1)
		close(entered)
		<-release
		return nil
	}}
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "", agent.WithToolSets(resource), agent.WithModel(&mockProvider{id: "test/retirement"})))))
	require.NoError(t, err)
	h, err := r.CreateSession(t.Context(), session.New(session.WithID(t.Name())), SessionBinding{})
	require.NoError(t, err)
	d := h.(*sessionHandle).driver
	first := make(chan error, 1)
	go func() { first <- d.retire(t.Context()) }()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, d.retire(ctx), context.Canceled)
	require.EqualValues(t, 1, calls.Load())
	joined := make(chan error, 8)
	for range 8 {
		go func() { joined <- d.retire(t.Context()) }()
	}
	close(release)
	require.NoError(t, <-first)
	for range 8 {
		require.NoError(t, <-joined)
	}
	require.EqualValues(t, 1, calls.Load())
	require.NoError(t, r.Close())
}
