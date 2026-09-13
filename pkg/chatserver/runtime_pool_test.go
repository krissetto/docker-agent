package chatserver

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

type testRuntimeSupervisor struct {
	mu        sync.Mutex
	shutdowns int
}

func (*testRuntimeSupervisor) Runtime() runtime.SessionRuntime { return nil }

func (s *testRuntimeSupervisor) Shutdown(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shutdowns++
	return nil
}

func (s *testRuntimeSupervisor) shutdownCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shutdowns
}

func newTestRuntimePool(t *testing.T, maxIdle int) (*runtimePool, *[]*testRuntimeSupervisor) {
	t.Helper()
	p := newRuntimePool(t.Context(), nil, maxIdle)
	created := make([]*testRuntimeSupervisor, 0)
	p.new = func() (runtime.SessionRuntimeSupervisor, error) {
		owner := &testRuntimeSupervisor{}
		created = append(created, owner)
		return owner, nil
	}
	return p, &created
}

func TestRuntimePool_MaxIdleZeroBuildsAndShutsDownEachRequest(t *testing.T) {
	p, created := newTestRuntimePool(t, 0)

	_, release, err := p.Get("root")
	require.NoError(t, err)
	require.NoError(t, release(t.Context(), true))
	_, release, err = p.Get("root")
	require.NoError(t, err)
	require.NoError(t, release(t.Context(), true))

	require.Len(t, *created, 2)
	assert.Equal(t, 1, (*created)[0].shutdownCount())
	assert.Equal(t, 1, (*created)[1].shutdownCount())
}

func TestRuntimePool_MaxIdleOneReusesReturnedRuntime(t *testing.T) {
	p, created := newTestRuntimePool(t, 1)

	_, release, err := p.Get("root")
	require.NoError(t, err)
	require.NoError(t, release(t.Context(), true))
	_, release, err = p.Get("root")
	require.NoError(t, err)

	require.Len(t, *created, 1)
	assert.Zero(t, (*created)[0].shutdownCount())
	require.NoError(t, release(t.Context(), true))
	require.NoError(t, p.Shutdown(t.Context()))
	assert.Equal(t, 1, (*created)[0].shutdownCount())
}

func TestRuntimePool_EvictsLeastRecentlyReturnedRuntime(t *testing.T) {
	p, created := newTestRuntimePool(t, 1)

	_, releaseFirst, err := p.Get("root")
	require.NoError(t, err)
	_, releaseSecond, err := p.Get("root")
	require.NoError(t, err)
	require.NoError(t, releaseFirst(t.Context(), true))
	require.NoError(t, releaseSecond(t.Context(), true))

	require.Len(t, *created, 2)
	assert.Equal(t, 1, (*created)[0].shutdownCount())
	assert.Zero(t, (*created)[1].shutdownCount())

	_, releaseReused, err := p.Get("root")
	require.NoError(t, err)
	require.Len(t, *created, 2)
	require.NoError(t, releaseReused(t.Context(), true))
}

func TestRuntimePool_ShutdownClosesBorrowedRuntimeOnce(t *testing.T) {
	p, created := newTestRuntimePool(t, 1)

	_, release, err := p.Get("root")
	require.NoError(t, err)
	require.Len(t, *created, 1)

	require.NoError(t, p.Shutdown(t.Context()))
	assert.Equal(t, 1, (*created)[0].shutdownCount())
	require.NoError(t, release(t.Context(), true))
	assert.Equal(t, 1, (*created)[0].shutdownCount())
}

func TestRuntimePool_ReleaseRacingShutdownClosesRuntimeOnce(t *testing.T) {
	p, created := newTestRuntimePool(t, 1)
	_, release, err := p.Get("root")
	require.NoError(t, err)

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_ = release(t.Context(), true)
	}()
	go func() {
		defer wg.Done()
		<-start
		_ = p.Shutdown(t.Context())
	}()
	close(start)
	wg.Wait()

	require.Len(t, *created, 1)
	assert.Equal(t, 1, (*created)[0].shutdownCount())
}

func TestRuntimePool_NilReceiver(t *testing.T) {
	var p *runtimePool
	rt, release, err := p.Get("root")
	require.ErrorIs(t, err, errInvalidRuntime)
	assert.Nil(t, rt)
	assert.Nil(t, release)
}

func TestRuntimePool_DiscardsRuntimeAfterFailedDrain(t *testing.T) {
	p, created := newTestRuntimePool(t, 1)
	_, release, err := p.Get("root")
	require.NoError(t, err)
	require.NoError(t, release(t.Context(), false))
	require.NoError(t, release(t.Context(), true), "a failed drain cannot be made reusable later")
	assert.Equal(t, 1, (*created)[0].shutdownCount())
	_, releaseNext, err := p.Get("root")
	require.NoError(t, err)
	require.Len(t, *created, 2)
	require.NoError(t, releaseNext(t.Context(), true))
	require.NoError(t, p.Shutdown(t.Context()))
}
