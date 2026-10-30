package runtime

import (
	"context"
	"errors"
	"fmt"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// scriptedStop is a resource owner whose stop calls follow a script. Call failCall
// returns cause. Calls from blockFrom onward block until release, ignoring their context.
// It records every call and the maximum number of stops in flight at once.
type scriptedStop struct {
	calls       atomic.Int32
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
	failCall    int32
	cause       error
	blockFrom   int32
	block       chan struct{}
	entered     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func newScriptedStop() *scriptedStop {
	return &scriptedStop{block: make(chan struct{}), entered: make(chan struct{})}
}

func (s *scriptedStop) release() { s.releaseOnce.Do(func() { close(s.block) }) }

func (*scriptedStop) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

func (s *scriptedStop) StopResourceOwner(context.Context) error {
	n := s.calls.Add(1)
	cur := s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	for {
		m := s.maxInFlight.Load()
		if cur <= m || s.maxInFlight.CompareAndSwap(m, cur) {
			break
		}
	}
	if s.failCall != 0 && n == s.failCall {
		return s.cause
	}
	if s.blockFrom != 0 && n >= s.blockFrom {
		s.enteredOnce.Do(func() { close(s.entered) })
		<-s.block
	}
	return nil
}

func attachToolset(t *testing.T, d *sessionDriver, ts tools.ToolSet) {
	t.Helper()
	require.NoError(t, d.ownerCall(t.Context(), func() error {
		d.sess.ExtraToolSets = append(d.sess.ExtraToolSets, ts)
		return nil
	}))
}

// namedSettledPair creates the oldest settled session and a younger one under IDs derived
// from the test name, so package-global process claims never collide across tests.
func namedSettledPair(t *testing.T, rt *LocalRuntime) (oldID, newerID string, oldDriver, newerDriver *sessionDriver) {
	t.Helper()
	oldID, newerID = t.Name()+"/old", t.Name()+"/newer"
	oldDriver = createIdleSession(t, rt, oldID)
	require.NoError(t, oldDriver.ownerCall(t.Context(), func() error {
		oldDriver.lastActive = time.Time{}
		return nil
	}))
	newerDriver = createIdleSession(t, rt, newerID)
	require.Eventually(t, func() bool {
		return driverDrained(oldDriver) && driverDrained(newerDriver)
	}, 5*time.Second, time.Millisecond)
	return oldID, newerID, oldDriver, newerDriver
}

// insertProtectorChild registers a resident child of parentID. A resident descendant
// protects its ancestor from removal, which is how these tests keep a removal refused.
func insertProtectorChild(t *testing.T, rt *LocalRuntime, parentID string) string {
	t.Helper()
	childID := parentID + "/child"
	child := newSessionDriver(rt, session.New(session.WithID(childID), session.WithParentID(parentID)))
	t.Cleanup(child.closeOwner)
	rt.sessionDrivers.mu.Lock()
	defer rt.sessionDrivers.mu.Unlock()
	rt.sessionDrivers.drivers[childID] = child
	return childID
}

// awaitReclaimIdle waits for d's reclaim retirement, if one is in flight. Once the token
// is cleared under g.mu, the retirement's outcome, including any removal, is already final.
func awaitReclaimIdle(g *sessionDriverRegistry, d *sessionDriver) {
	g.mu.Lock()
	token := d.retirement
	g.mu.Unlock()
	if token != nil {
		<-token.done
	}
}

// After a first retirement fails, the next retirement runs the stop again and completes.
func TestCleanupRunsAgainAfterFirstRetireFailure(t *testing.T) {
	rt := newPressureRuntime(t, 2)
	rt.sessionDrivers.capacityWaitBudget = 10 * time.Second
	oldID, _, oldDriver, _ := namedSettledPair(t, rt)
	fault := &flakyStopToolset{}
	fault.failures.Store(1)
	attachToolset(t, oldDriver, fault)
	t.Cleanup(func() { fault.failures.Store(0) })
	_, err := rt.CreateSession(t.Context(), session.New(session.WithID(t.Name()+"/pressure")), SessionBinding{})
	require.NoError(t, err)
	assert.Equal(t, int32(2), fault.calls.Load(), "the failed stop is retried once and then completes")
	_, oldResident := rt.sessionDrivers.Lookup(oldID)
	assert.False(t, oldResident)
}

// A child cannot pin a reclaiming parent. A removal that a resident descendant refuses
// leaves the entry hidden and registered with its owner closed. The entry is removed only
// after the descendant leaves.
func TestPinRefusedDuringParentReclaimAndRemovalAbortResumes(t *testing.T) {
	rt := newPressureRuntime(t, 2)
	g := rt.sessionDrivers
	oldID, _, oldDriver, _ := namedSettledPair(t, rt)
	beginOldestReclaim(t, g, oldDriver)
	pinned, err := g.pinResidentParent(oldID)
	g.mu.Lock()
	pendingAfterRefusal := g.pendingClaims[oldID]
	g.mu.Unlock()
	require.False(t, pinned)
	require.ErrorIs(t, err, ErrSessionStopped)
	assert.Zero(t, pendingAfterRefusal)

	childID := insertProtectorChild(t, rt, oldID)
	g.mu.Lock()
	r := g.startRetirementLocked(oldID, oldDriver)
	g.mu.Unlock()
	require.NotNil(t, r)
	<-r.done
	g.mu.Lock()
	_, present := g.drivers[oldID]
	token := oldDriver.retirement
	g.mu.Unlock()
	assert.True(t, present, "a protected parent stays registered")
	assert.Nil(t, token, "the retirement token is cleared")
	assert.True(t, driverOwnerClosed(oldDriver), "the completed retirement closed the owner")

	_, ok := g.Lookup(oldID)
	require.False(t, ok, "an abandoned removal is never handed out")
	awaitReclaimIdle(g, oldDriver)
	g.mu.Lock()
	_, present = g.drivers[oldID]
	g.mu.Unlock()
	assert.True(t, present, "the descendant still protects the parent")

	g.mu.Lock()
	delete(g.drivers, childID)
	g.mu.Unlock()
	_, ok = g.Lookup(oldID)
	require.False(t, ok)
	awaitReclaimIdle(g, oldDriver)
	g.mu.Lock()
	_, present = g.drivers[oldID]
	g.mu.Unlock()
	assert.False(t, present, "the resumed removal completes once the descendant leaves")
}

// Abandoned removal is hidden from every lookup, and no same-ID path adopts it. While the
// protector remains, a same-ID creation fails with a typed error and returns no handle.
// Once the protector leaves, the same ID is created fresh.
func TestAbandonedRemovalIsResumedAndNeverHandedOut(t *testing.T) {
	rt := newPressureRuntime(t, 3)
	g := rt.sessionDrivers
	oldID, _, oldDriver, _ := namedSettledPair(t, rt)
	beginOldestReclaim(t, g, oldDriver)
	childID := insertProtectorChild(t, rt, oldID)
	g.mu.Lock()
	r := g.startRetirementLocked(oldID, oldDriver)
	g.mu.Unlock()
	require.NotNil(t, r)
	<-r.done

	_, ok := g.Lookup(oldID)
	require.False(t, ok)
	awaitReclaimIdle(g, oldDriver)
	handle, err := rt.CreateSession(t.Context(), session.New(session.WithID(oldID)), SessionBinding{})
	require.Error(t, err, "the protected generation is not adopted")
	assert.Nil(t, handle)

	g.mu.Lock()
	delete(g.drivers, childID)
	g.mu.Unlock()
	handle, err = rt.CreateSession(t.Context(), session.New(session.WithID(oldID)), SessionBinding{})
	require.NoError(t, err)
	require.NotNil(t, handle)
	assert.NotSame(t, oldDriver, handle.(*sessionHandle).driver, "the same ID is created fresh")
}

// A reclaiming generation whose retirement failed keeps its owner and resources live. A
// restore may not replace it until an explicit retirement has succeeded.
func TestRestoreWaitsForRetirementBeforeReplacingReclaimingGeneration(t *testing.T) {
	rt := newPressureRuntime(t, 2)
	rt.sessionDrivers.capacityWaitBudget = 10 * time.Second
	g := rt.sessionDrivers
	oldID, _, oldDriver, _ := namedSettledPair(t, rt)
	fault := &flakyStopToolset{}
	fault.failures.Store(1 << 20)
	attachToolset(t, oldDriver, fault)
	t.Cleanup(func() { fault.failures.Store(0) })
	beginOldestReclaim(t, g, oldDriver)

	g.mu.Lock()
	wait, err := g.generationWaitLocked(oldID, oldDriver)
	g.mu.Unlock()
	require.NoError(t, err)
	require.NotNil(t, wait)
	<-wait.busy.done
	require.False(t, driverOwnerClosed(oldDriver), "a failed retirement keeps the owner open")
	_, resident := g.Lookup(oldID)
	require.True(t, resident)

	_, err = g.prepareRestore(t.Context(), session.New(session.WithID(oldID)), oldDriver)
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionOperation("restore_collision"), sessionErr.Operation)

	fault.failures.Store(0)
	g.mu.Lock()
	wait, err = g.generationWaitLocked(oldID, oldDriver)
	g.mu.Unlock()
	require.NoError(t, err)
	require.NotNil(t, wait)
	<-wait.busy.done
	require.True(t, driverOwnerClosed(oldDriver))
	_, resident = g.Lookup(oldID)
	assert.False(t, resident, "the generation is removed only after its resources stopped")
}

// Repeated admissions while one stop stays stuck. The invariants hold at every observation:
// a retirement token is replaced only after the previous one completed; the stop runs in
// at most one goroutine at a time; the stop is invoked exactly once; each admission returns
// within its bound; and the younger driver is never evicted. The check does not depend on
// the drain timeout: a stuck cleanup joins its in-flight attempt, so no second stop starts.
func TestRepeatedAdmissionsWithStuckCleanupInvokeStopOnce(t *testing.T) {
	rt := newPressureRuntime(t, 2)
	g := rt.sessionDrivers
	g.capacityWaitBudget = 50 * time.Millisecond
	oldID, newerID, oldDriver, _ := namedSettledPair(t, rt)
	stuck := newScriptedStop()
	stuck.blockFrom = 1
	t.Cleanup(stuck.release)
	attachToolset(t, oldDriver, stuck)

	var observed *driverRetirement
	for i := range 3 {
		start := time.Now()
		_, err := rt.CreateSession(t.Context(), session.New(session.WithID(fmt.Sprintf("%s/pressure-%d", t.Name(), i))), SessionBinding{})
		require.ErrorIs(t, err, ErrSessionCapacity)
		assert.Less(t, time.Since(start), supersededRetireWaitBudget, "each admission returns within its bound")
		g.mu.Lock()
		current := oldDriver.retirement
		previousFinished := observed == nil || retirementFinished(observed)
		g.mu.Unlock()
		if current != nil {
			if observed != nil && current != observed {
				require.True(t, previousFinished, "a retirement token is replaced only after the previous one completed")
			}
			observed = current
		}
		assert.LessOrEqual(t, stuck.maxInFlight.Load(), int32(1), "the stop runs in at most one goroutine")
	}
	require.NotNil(t, observed)
	waitClosed(t, stuck.entered, "the retirement reached the stuck stop")
	assert.Equal(t, int32(1), stuck.calls.Load(), "the stop is invoked exactly once")
	_, newerResident := g.Lookup(newerID)
	assert.True(t, newerResident, "the younger driver is never evicted")

	stuck.release()
	g.capacityWaitBudget = 10 * time.Second
	_, err := rt.CreateSession(t.Context(), session.New(session.WithID(t.Name()+"/finish")), SessionBinding{})
	require.NoError(t, err)
	_, oldResident := g.Lookup(oldID)
	assert.False(t, oldResident, "the released retirement removes the oldest driver")
	_, newerResident = g.Lookup(newerID)
	assert.True(t, newerResident)
}

// CloseContext with a stuck cleanup returns promptly with the context error. The stuck
// driver is retained: still registered, claimed, and owner open. The other driver is
// retired, removed, and released. A background retirement of the stuck driver is in flight,
// so the close also starts a waiter for it. Once the stop is released, that retirement
// removes the stuck driver, and no waiter goroutine started by the close remains.
func TestCloseContextWithStuckCleanupReturnsPromptlyAndRetainsFailedDriver(t *testing.T) {
	rt := newPressureRuntime(t, 2)
	g := rt.sessionDrivers
	oldID, newerID, oldDriver, newerDriver := namedSettledPair(t, rt)
	stuck := newScriptedStop()
	stuck.blockFrom = 1
	t.Cleanup(stuck.release)
	attachToolset(t, oldDriver, stuck)
	beginOldestReclaim(t, g, oldDriver)
	g.mu.Lock()
	g.startRetirementLocked(oldID, oldDriver)
	g.mu.Unlock()
	waitClosed(t, stuck.entered, "the background retirement reached the stuck stop")

	label := newRegistryWaiterLabel(t, g)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	var err error
	start := time.Now()
	pprof.Do(ctx, pprof.Labels(registryWaiterLabel, label), func(ctx context.Context) {
		err = g.CloseContext(ctx)
	})
	assert.Less(t, time.Since(start), time.Second, "CloseContext returns promptly")
	require.ErrorIs(t, err, context.DeadlineExceeded)

	assert.False(t, driverOwnerClosed(oldDriver), "a failed retirement keeps the owner open")
	assert.True(t, driverOwnerClosed(newerDriver), "the other driver is retired and its owner closed")
	g.mu.Lock()
	assert.Same(t, oldDriver, g.drivers[oldID], "the failed driver stays registered")
	assert.NotContains(t, g.drivers, newerID, "the retired driver is removed")
	g.mu.Unlock()
	resident, found := g.Lookup(oldID)
	require.True(t, found, "the failed driver stays routable")
	assert.Same(t, oldDriver, resident)
	processSessions.Lock()
	assert.Same(t, rt, processSessions.owners[oldID], "the failed driver keeps its claim")
	assert.NotSame(t, rt, processSessions.owners[newerID], "the retired driver's claim is released")
	processSessions.Unlock()

	stuck.release()
	require.Eventually(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.drivers[oldID] == nil
	}, 5*time.Second, time.Millisecond, "the released retirement removes the failed driver")
	assert.True(t, driverOwnerClosed(oldDriver))
	require.NoError(t, g.CloseContext(t.Context()), "a retry on the emptied registry closes cleanly")
	processSessions.Lock()
	assert.NotSame(t, rt, processSessions.owners[oldID], "the claim is released once retirement succeeds")
	processSessions.Unlock()
	assert.Equal(t, int32(1), stuck.calls.Load(), "the stop runs once")
	assert.Eventually(t, func() bool { return registryWaiterCount(t, label) == 0 }, 5*time.Second, time.Millisecond, "no waiter goroutine started by the close remains")
}

// sameIDWithStoppedGeneration creates a stopped, settled generation for an ID, with a
// scripted stop attached. It returns the ID and the generation's driver.
func sameIDWithStoppedGeneration(t *testing.T, rt *LocalRuntime, stop *scriptedStop) (string, *sessionDriver) {
	t.Helper()
	id := t.Name() + "/same"
	d := createIdleSession(t, rt, id)
	attachToolset(t, d, stop)
	d.StopAll()
	require.Eventually(t, func() bool { return rt.sessionDrivers.Settled(id) }, 5*time.Second, time.Millisecond)
	return id, d
}

// A slow but successful retirement within the bound lets a same-ID recreation proceed.
func TestSlowSuccessfulRetirementWithinBudgetRecreatesSameID(t *testing.T) {
	rt := newPressureRuntime(t, 3)
	stop := newScriptedStop()
	stop.blockFrom = 1
	t.Cleanup(stop.release)
	id, old := sameIDWithStoppedGeneration(t, rt, stop)

	result := make(chan error, 1)
	var fresh *sessionDriver
	go func() {
		handle, err := rt.CreateSession(t.Context(), session.New(session.WithID(id)), SessionBinding{})
		if err == nil {
			fresh = handle.(*sessionHandle).driver
		}
		result <- err
	}()
	waitClosed(t, stop.entered, "the superseded retirement reached the slow stop")
	stop.release()
	require.NoError(t, <-result)
	require.NotNil(t, fresh)
	assert.NotSame(t, old, fresh, "the recreated generation replaces the retired one")
	require.True(t, driverOwnerClosed(old))
}

// A slow but successful retirement within the bound lets a view restore replace a
// reclaiming generation of the same ID.
func TestSlowSuccessfulRetirementWithinBudgetRestoresSameIDInView(t *testing.T) {
	rt := newPressureRuntime(t, 3)
	g := rt.sessionDrivers
	oldID, _, oldDriver, _ := namedSettledPair(t, rt)
	stop := newScriptedStop()
	stop.blockFrom = 1
	t.Cleanup(stop.release)
	attachToolset(t, oldDriver, stop)
	beginOldestReclaim(t, g, oldDriver)

	p := &preparedSessionView{ctx: func() context.Context { return t.Context() }, existing: map[string]*sessionDriver{}}
	candidate := session.New(session.WithID(oldID))
	candidate.AgentName = "root"
	stopped := map[string]bool{oldID: true}
	_, err := p.tryReserveCandidate(g, candidate, stopped)
	var busy *capacityWaitError
	require.ErrorAs(t, err, &busy)
	require.True(t, busy.superseded, "a same-ID wait uses the same-ID budget")
	waitClosed(t, stop.entered, "the reclaim retirement reached the slow stop")
	stop.release()
	<-busy.busy.done

	reservation, err := p.tryReserveCandidate(g, candidate, stopped)
	require.NoError(t, err)
	require.Nil(t, reservation, "the restore reserves through the normal path once the generation is gone")
	_, resident := g.Lookup(oldID)
	assert.False(t, resident)
}

// A same-ID timeout reports a typed stopped error, joined with the last retirement cause.
// The earlier failure is returned directly. The later timeout still exposes its cause.
func TestSameIDTimeoutReportsStoppedJoinedWithLastCause(t *testing.T) {
	rt := newPressureRuntime(t, 3)
	g := rt.sessionDrivers
	errCause := errors.New("stop cause")
	stop := newScriptedStop()
	stop.failCall = 1
	stop.cause = errCause
	stop.blockFrom = 2
	t.Cleanup(stop.release)
	id, _ := sameIDWithStoppedGeneration(t, rt, stop)
	g.supersededRetireWaitOverride = 50 * time.Millisecond

	_, err := rt.CreateSession(t.Context(), session.New(session.WithID(id)), SessionBinding{})
	require.ErrorIs(t, err, errCause, "the failed retirement's own error is returned directly")

	_, err = rt.CreateSession(t.Context(), session.New(session.WithID(id)), SessionBinding{})
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionErrorStopped, sessionErr.Kind)
	require.ErrorIs(t, err, errCause, "the timeout keeps the cause reachable")
	require.ErrorIs(t, err, context.DeadlineExceeded, "an internal budget expiry is reported as a deadline")
}

// Cancelling a same-ID wait returns the context error without evicting or recreating.
func TestSameIDRetirementWaitHonorsCancellation(t *testing.T) {
	rt := newPressureRuntime(t, 3)
	stop := newScriptedStop()
	stop.blockFrom = 1
	t.Cleanup(stop.release)
	id, old := sameIDWithStoppedGeneration(t, rt, stop)

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := rt.CreateSession(ctx, session.New(session.WithID(id)), SessionBinding{})
		result <- err
	}()
	waitClosed(t, stop.entered, "the superseded retirement reached the stop")
	cancel()
	err := <-result
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, context.DeadlineExceeded, "caller cancellation is not reported as internal expiry")
	var sessionErr *SessionError
	assert.NotErrorAs(t, err, &sessionErr, "caller cancellation returns the caller's error, not a stopped error")
	_, resident := rt.sessionDrivers.Lookup(id)
	assert.True(t, resident)
	assert.False(t, driverOwnerClosed(old), "the cancelled wait leaves the retirement running, not the owner closed")
	stop.release()
}

// A failed descendant keeps its ancestor. The ancestor is not retired while the descendant
// still needs it, so both stay registered, routable, and claimed. The close error names the
// pair. A retry after the descendant's stop recovers closes both cleanly.
func TestCloseRetainsAncestorWhileFailedDescendantRetains(t *testing.T) {
	rt := newPressureRuntime(t, 3)
	g := rt.sessionDrivers
	parentID := t.Name() + "/parent"
	parent := createIdleSession(t, rt, parentID)
	require.Eventually(t, func() bool { return driverDrained(parent) }, 5*time.Second, time.Millisecond)
	cause := errors.New("child stop failed")
	stop := newScriptedStop()
	stop.failCall = 1
	stop.cause = cause
	childID := parentID + "/child"
	child := newSessionDriver(rt, session.New(session.WithID(childID), session.WithParentID(parentID)))
	t.Cleanup(child.closeOwner)
	attachToolset(t, child, stop)
	g.mu.Lock()
	g.drivers[childID] = child
	g.mu.Unlock()

	err := g.CloseContext(t.Context())
	require.ErrorIs(t, err, cause, "the descendant's own failure is reported")
	require.ErrorContains(t, err, parentID, "the error names the retained ancestor")
	require.ErrorContains(t, err, childID, "the error names the retained descendant")
	assert.False(t, driverOwnerClosed(parent), "an ancestor is not retired while its descendant still needs it")
	assert.False(t, driverOwnerClosed(child), "the failed descendant keeps its owner open")
	resident, ok := g.Lookup(parentID)
	require.True(t, ok, "the retained ancestor stays routable")
	assert.Same(t, parent, resident)
	resident, ok = g.Lookup(childID)
	require.True(t, ok, "the failed descendant stays routable")
	assert.Same(t, child, resident)
	processSessions.Lock()
	assert.Same(t, rt, processSessions.owners[parentID], "the retained ancestor keeps its claim")
	processSessions.Unlock()

	require.NoError(t, g.CloseContext(t.Context()), "a retry retires the descendant, then the ancestor")
	assert.True(t, driverOwnerClosed(parent))
	assert.True(t, driverOwnerClosed(child))
	g.mu.Lock()
	assert.Empty(t, g.drivers)
	g.mu.Unlock()
	assert.Equal(t, int32(2), stop.calls.Load(), "the failed stop is retried once and then succeeds")
}
