package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

type creationBlockingStore struct {
	session.Store

	entered, release chan struct{}
}

func (s *creationBlockingStore) GetSession(ctx context.Context, id string) (*session.Session, error) {
	if id == "blocked" {
		close(s.entered)
		<-s.release
	}
	return s.Store.GetSession(ctx, id)
}

func TestSessionOwnerRegressionCreateCancellationHOL(t *testing.T) {
	rt := newPressureRuntime(t, 3)
	st := &creationBlockingStore{Store: session.NewInMemorySessionStore(), entered: make(chan struct{}), release: make(chan struct{})}
	rt.sessionStore = st
	first := make(chan error, 1)
	go func() {
		_, err := rt.CreateSession(t.Context(), session.New(session.WithID("blocked")), SessionBinding{})
		first <- err
	}()
	<-st.entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	second := make(chan error, 1)
	go func() {
		_, err := rt.CreateSession(ctx, session.New(session.WithID("unrelated")), SessionBinding{})
		second <- err
	}()
	unblock := sync.OnceFunc(func() { close(st.release) })
	defer func() { unblock(); require.NoError(t, <-first) }()
	select {
	case err := <-second:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("canceled unrelated creation blocked")
	}
	active := make(chan error, 1)
	go func() {
		_, err := rt.CreateSession(t.Context(), session.New(session.WithID("independent")), SessionBinding{})
		active <- err
	}()
	select {
	case err := <-active:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("independent creation blocked")
	}
	sameCtx, cancelSame := context.WithCancel(t.Context())
	same := make(chan error, 1)
	go func() {
		_, err := rt.CreateSession(sameCtx, session.New(session.WithID("blocked")), SessionBinding{})
		same <- err
	}()
	cancelSame()
	select {
	case err := <-same:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("same ID cancellation blocked")
	}
}

func TestCreationReservationsBoundAndReleaseTransientIDs(t *testing.T) {
	r := newPressureRuntime(t, 2)
	g := r.sessionDrivers
	one, err := g.reserveCreation(t.Context(), "one")
	require.NoError(t, err)
	two, err := g.reserveCreation(t.Context(), "two")
	require.NoError(t, err)
	_, err = g.reserveCreation(t.Context(), "three")
	require.ErrorIs(t, err, ErrSessionCapacity)
	one()
	three, err := g.reserveCreation(t.Context(), "three")
	require.NoError(t, err)
	two()
	three()
	g.mu.Lock()
	defer g.mu.Unlock()
	require.Empty(t, g.creating)
}
