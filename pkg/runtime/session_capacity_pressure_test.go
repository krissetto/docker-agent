package runtime

import (
	"context"
	"errors"
	"fmt"
	"hash/maphash"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func newPressureRuntime(t *testing.T, maxSessions int) *LocalRuntime {
	t.Helper()
	return newPressureRuntimeWith(t, maxSessions)
}

func newPressureRuntimeWith(t *testing.T, maxSessions int, opts ...Opt) *LocalRuntime {
	t.Helper()
	policy := DefaultSessionResourcePolicy()
	policy.MaxSessions = maxSessions
	policy.IdleRetention = 0
	prov := &mockProvider{id: "test/pressure", stream: newStreamBuilder().AddStopWithUsage(1, 1).Build()}
	opts = append([]Opt{WithSessionResourcePolicy(policy)}, opts...)
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(prov)))), opts...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	return rt
}

func TestSessionCapacityPressureReclaimsOldestEligibleSettled(t *testing.T) {
	rt := newPressureRuntime(t, 2)
	old, err := rt.CreateSession(t.Context(), session.New(session.WithID("old")), SessionBinding{})
	require.NoError(t, err)
	oldDriver := old.(*sessionHandle).driver
	require.NoError(t, oldDriver.ownerCall(t.Context(), func() error {
		oldDriver.lastActive = time.Time{}
		return nil
	}))
	_, err = rt.CreateSession(t.Context(), session.New(session.WithID("newer")), SessionBinding{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, err = rt.CreateSession(t.Context(), session.New(session.WithID("pressure")), SessionBinding{})
		return err == nil
	}, time.Second, time.Millisecond)
	_, exists := rt.sessionDrivers.Lookup("old")
	assert.False(t, exists)
	_, err = old.Observe(t.Context(), ObserveOptions{})
	require.Error(t, err)
}

func TestSessionCapacityPressureProtectsObserverPendingSteeringInteractionAndRunning(t *testing.T) {
	tests := []struct {
		name    string
		protect func(*sessionDriver) func()
	}{
		{"observer", func(d *sessionDriver) func() {
			_, _, cancel, _ := d.events.SubscribeSequenced(d.sessionID(), nil, 1)
			return cancel
		}},
		{"pending", func(d *sessionDriver) func() {
			d.mu.Lock()
			d.pending = append(d.pending, QueuedMessage{Content: "p"})
			d.mu.Unlock()
			return func() {}
		}},
		{"guidance", func(d *sessionDriver) func() {
			d.mu.Lock()
			d.pending = append(d.pending, QueuedMessage{Content: "s", InputMode: "steer"})
			d.mu.Unlock()
			return func() {}
		}},
		{"interaction", func(d *sessionDriver) func() {
			d.mu.Lock()
			d.interactions["i"] = sessionInteraction{}
			d.mu.Unlock()
			return func() {}
		}},
		{"running", func(d *sessionDriver) func() {
			d.mu.Lock()
			d.phase = sessionRunning
			d.mu.Unlock()
			return func() {}
		}},
		{"retrying", func(d *sessionDriver) func() {
			d.mu.Lock()
			d.retryRunning = true
			d.mu.Unlock()
			return func() {}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := newPressureRuntime(t, 1)
			h, err := rt.CreateSession(t.Context(), session.New(session.WithID("protected")), SessionBinding{})
			require.NoError(t, err)
			cleanup := tt.protect(h.(*sessionHandle).driver)
			defer cleanup()
			_, err = rt.CreateSession(t.Context(), session.New(session.WithID("pressure")), SessionBinding{})
			var sessionErr *SessionError
			require.ErrorAs(t, err, &sessionErr)
			assert.Equal(t, SessionErrorCapacity, sessionErr.Kind)
		})
	}
}

func TestSessionCapacityPressureStaleSubmitObserveRaceRejected(t *testing.T) {
	for i := range 50 {
		rt := newPressureRuntime(t, 1)
		stale, err := rt.CreateSession(t.Context(), session.New(session.WithID(fmt.Sprintf("%s/stale/%d", t.Name(), i))), SessionBinding{})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			_, err = rt.CreateSession(t.Context(), session.New(session.WithID(fmt.Sprintf("%s/replacement/%d", t.Name(), i))), SessionBinding{})
			return err == nil
		}, time.Second, time.Millisecond)
		_, exists := rt.sessionDrivers.Lookup(stale.ID())
		require.False(t, exists)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var submitErr, observeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, submitErr = stale.Submit(t.Context(), TurnInput{Content: "late"})
		}()
		go func() { defer wg.Done(); <-start; _, observeErr = stale.Observe(t.Context(), ObserveOptions{}) }()
		close(start)
		wg.Wait()
		require.Error(t, submitErr)
		require.Error(t, observeErr)
	}
}

// A busy oldest settled driver must stop capacity eviction; it must not let a
// younger settled driver be reclaimed in its place.
func TestSessionCapacityBusyOldestSettledDoesNotEvictYounger(t *testing.T) {
	rt := newPressureRuntime(t, 2)
	old, err := rt.CreateSession(t.Context(), session.New(session.WithID("old")), SessionBinding{})
	require.NoError(t, err)
	oldDriver := old.(*sessionHandle).driver
	require.NoError(t, oldDriver.ownerCall(t.Context(), func() error {
		oldDriver.lastActive = time.Time{}
		return nil
	}))
	_, err = rt.CreateSession(t.Context(), session.New(session.WithID("newer")), SessionBinding{})
	require.NoError(t, err)
	newerDriver, ok := rt.sessionDrivers.Lookup("newer")
	require.True(t, ok)
	require.Eventually(t, func() bool {
		return driverDrained(oldDriver) && driverDrained(newerDriver)
	}, 5*time.Second, time.Millisecond)

	g := rt.sessionDrivers
	// Holding the oldest driver's lock makes its reclaim attempt transiently busy.
	oldDriver.mu.Lock()
	g.mu.Lock()
	busy := g.evictSettledForCapacityLocked()
	g.mu.Unlock()
	oldDriver.mu.Unlock()
	require.NotNil(t, busy)
	require.Same(t, oldDriver, busy.driver)
	_, oldResident := g.Lookup("old")
	_, newerResident := g.Lookup("newer")
	require.True(t, oldResident, "old driver remains while busy")
	require.True(t, newerResident, "younger settled driver must not be evicted past a busy older one")

	// Once released, the oldest is claimed and retired in the background. Eviction
	// keeps targeting it while it is registered, so newer is never claimed.
	require.Eventually(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		if _, resident := g.drivers["old"]; !resident {
			return true
		}
		g.evictSettledForCapacityLocked()
		return false
	}, 5*time.Second, time.Millisecond)
	_, oldResident = g.Lookup("old")
	require.False(t, oldResident, "oldest eligible driver is reclaimed once it is idle")
	_, newerResident = g.Lookup("newer")
	require.True(t, newerResident)
}

// newPressurePair creates the oldest settled session "old" and a younger settled
// session "newer" at capacity two, waiting until both maintenance passes drain.
func newPressurePair(t *testing.T, rt *LocalRuntime) (*sessionDriver, *sessionDriver) {
	t.Helper()
	old, err := rt.CreateSession(t.Context(), session.New(session.WithID("old")), SessionBinding{})
	require.NoError(t, err)
	oldDriver := old.(*sessionHandle).driver
	require.NoError(t, oldDriver.ownerCall(t.Context(), func() error {
		oldDriver.lastActive = time.Time{}
		return nil
	}))
	_, err = rt.CreateSession(t.Context(), session.New(session.WithID("newer")), SessionBinding{})
	require.NoError(t, err)
	newerDriver, ok := rt.sessionDrivers.Lookup("newer")
	require.True(t, ok)
	require.Eventually(t, func() bool {
		return driverDrained(oldDriver) && driverDrained(newerDriver)
	}, 5*time.Second, time.Millisecond)
	return oldDriver, newerDriver
}

// observeCapacityWait returns a channel closed when a capacity admission first
// enters its bounded wait.
func observeCapacityWait(rt *LocalRuntime) <-chan struct{} {
	entered := make(chan struct{})
	var once sync.Once
	hook := func() { once.Do(func() { close(entered) }) }
	rt.sessionDrivers.capacityWaitHook.Store(&hook)
	return entered
}

func TestSessionCapacityWaitsForBusyOldestThenReclaimsIt(t *testing.T) {
	rt := newPressureRuntime(t, 2)
	rt.sessionDrivers.capacityWaitBudget = 10 * time.Second
	oldDriver, _ := newPressurePair(t, rt)
	entered := observeCapacityWait(rt)
	oldDriver.mu.Lock()
	result := make(chan error, 1)
	go func() {
		_, err := rt.CreateSession(t.Context(), session.New(session.WithID("pressure")), SessionBinding{})
		result <- err
	}()
	waitClosed(t, entered, "capacity admission waiting on the busy oldest driver")
	oldDriver.mu.Unlock()
	require.NoError(t, <-result)
	_, oldResident := rt.sessionDrivers.Lookup("old")
	_, newerResident := rt.sessionDrivers.Lookup("newer")
	assert.False(t, oldResident, "oldest eligible driver is reclaimed once released")
	assert.True(t, newerResident, "younger settled driver is never evicted past the oldest")
}

func TestSessionCapacityWaitsForMaintenanceThenReclaimsOldest(t *testing.T) {
	rt := newPressureRuntime(t, 2)
	rt.sessionDrivers.capacityWaitBudget = 10 * time.Second
	oldDriver, _ := newPressurePair(t, rt)
	oldDriver.wg.Add(1)
	entered := observeCapacityWait(rt)
	result := make(chan error, 1)
	go func() {
		_, err := rt.CreateSession(t.Context(), session.New(session.WithID("pressure")), SessionBinding{})
		result <- err
	}()
	waitClosed(t, entered, "capacity admission waiting on in-flight maintenance")
	oldDriver.wg.Done()
	require.NoError(t, <-result)
	_, oldResident := rt.sessionDrivers.Lookup("old")
	_, newerResident := rt.sessionDrivers.Lookup("newer")
	assert.False(t, oldResident, "oldest eligible driver is reclaimed after maintenance drains")
	assert.True(t, newerResident, "younger settled driver is never evicted past the oldest")
}

func TestSessionCapacityPersistentContentionReturnsTypedDenial(t *testing.T) {
	rt := newPressureRuntime(t, 2)
	rt.sessionDrivers.capacityWaitBudget = 50 * time.Millisecond
	oldDriver, _ := newPressurePair(t, rt)
	oldDriver.mu.Lock()
	_, err := rt.CreateSession(t.Context(), session.New(session.WithID("pressure")), SessionBinding{})
	oldDriver.mu.Unlock()
	require.ErrorIs(t, err, ErrSessionCapacity)
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionOperationCreateSession, sessionErr.Operation)
	assert.Equal(t, 2, sessionErr.Limit)
	_, oldResident := rt.sessionDrivers.Lookup("old")
	_, newerResident := rt.sessionDrivers.Lookup("newer")
	assert.True(t, oldResident, "contended oldest driver is not evicted")
	assert.True(t, newerResident, "younger settled driver is not evicted as a fallback")
}

func TestSessionCapacityWaitHonorsCancellation(t *testing.T) {
	rt := newPressureRuntime(t, 2)
	rt.sessionDrivers.capacityWaitBudget = 10 * time.Second
	oldDriver, _ := newPressurePair(t, rt)
	entered := observeCapacityWait(rt)
	oldDriver.mu.Lock()
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := rt.CreateSession(ctx, session.New(session.WithID("pressure")), SessionBinding{})
		result <- err
	}()
	waitClosed(t, entered, "capacity admission waiting on the busy oldest driver")
	cancel()
	err := <-result
	oldDriver.mu.Unlock()
	require.ErrorIs(t, err, context.Canceled)
	_, oldResident := rt.sessionDrivers.Lookup("old")
	_, newerResident := rt.sessionDrivers.Lookup("newer")
	assert.True(t, oldResident, "canceled admission evicts nothing")
	assert.True(t, newerResident, "canceled admission evicts nothing")
}

func TestSessionCapacityWaitLeavesRegistryAndOtherSessionsResponsive(t *testing.T) {
	rt := newPressureRuntime(t, 2)
	rt.sessionDrivers.capacityWaitBudget = 10 * time.Second
	oldDriver, newerDriver := newPressurePair(t, rt)
	entered := observeCapacityWait(rt)
	oldDriver.mu.Lock()
	result := make(chan error, 1)
	go func() {
		_, err := rt.CreateSession(t.Context(), session.New(session.WithID("pressure")), SessionBinding{})
		result <- err
	}()
	waitClosed(t, entered, "capacity admission waiting on the busy oldest driver")
	_, newerResident := rt.sessionDrivers.Lookup("newer")
	assert.True(t, newerResident, "registry answers while the admission waits")
	require.NoError(t, newerDriver.ownerCall(t.Context(), func() error { return nil }), "younger session owner stays responsive while the admission waits")
	other := newPressureRuntime(t, 2)
	_, err := other.CreateSession(t.Context(), session.New(session.WithID("elsewhere")), SessionBinding{})
	require.NoError(t, err)
	oldDriver.mu.Unlock()
	require.NoError(t, <-result)
	_, newerResident = rt.sessionDrivers.Lookup("newer")
	assert.True(t, newerResident, "younger settled driver is never evicted past the oldest")
}

// flakyStopToolset is a resource owner whose first failures stop calls fail. It
// reaches retirement through the existing tools.ResourceOwnerStopper path.
type flakyStopToolset struct {
	failures atomic.Int32
	calls    atomic.Int32
}

func (*flakyStopToolset) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

func (s *flakyStopToolset) StopResourceOwner(context.Context) error {
	s.calls.Add(1)
	if s.failures.Add(-1) >= 0 {
		return errors.New("stop failed")
	}
	return nil
}

// attachStopFault gives one driver a resource owner whose first failures stops fail.
// It is disarmed before runtime shutdown, which is registered earlier, so Close can finish.
func attachStopFault(t *testing.T, d *sessionDriver, failures int32) {
	t.Helper()
	fault := &flakyStopToolset{}
	fault.failures.Store(failures)
	require.NoError(t, d.ownerCall(t.Context(), func() error {
		d.sess.ExtraToolSets = append(d.sess.ExtraToolSets, fault)
		return nil
	}))
	t.Cleanup(func() { fault.failures.Store(0) })
}

// beginOldestReclaim starts a reclaim of the oldest driver as an admitter does, leaving the
// driver registered and reclaiming. Work-group drain can still flicker right after a
// settled check, and a busy claim changes nothing, so the claim is retried until it wins.
func beginOldestReclaim(t *testing.T, g *sessionDriverRegistry, d *sessionDriver) {
	t.Helper()
	require.Eventually(t, func() bool {
		claimed := false
		if err := d.ownerCall(t.Context(), func() error {
			g.mu.Lock()
			defer g.mu.Unlock()
			if d.beginReclaimLocked() != driverReclaimed {
				return nil
			}
			d.maintenanceRetired = true
			claimed = true
			return nil
		}); err != nil {
			return false
		}
		return claimed
	}, 5*time.Second, time.Millisecond)
}

// A reclaim already in progress holds the retirement gate. The waiting admission
// parks on that retirement and succeeds once it completes; newer is never evicted.
func TestSessionCapacityWaitsForInProgressRetirementThenSucceeds(t *testing.T) {
	rt := newPressureRuntime(t, 2)
	rt.sessionDrivers.capacityWaitBudget = 10 * time.Second
	oldDriver, _ := newPressurePair(t, rt)
	beginOldestReclaim(t, rt.sessionDrivers, oldDriver)
	oldDriver.retirementGate <- struct{}{}
	entered := observeCapacityWait(rt)
	result := make(chan error, 1)
	go func() {
		_, err := rt.CreateSession(t.Context(), session.New(session.WithID("pressure")), SessionBinding{})
		result <- err
	}()
	waitClosed(t, entered, "capacity admission waiting on the in-progress retirement")
	<-oldDriver.retirementGate
	require.NoError(t, oldDriver.retire(t.Context()))
	require.NoError(t, <-result)
	_, oldResident := rt.sessionDrivers.Lookup("old")
	_, newerResident := rt.sessionDrivers.Lookup("newer")
	assert.False(t, oldResident, "the retired driver leaves the registry")
	assert.True(t, newerResident, "younger settled driver is never evicted past the oldest")
}

// A failed retirement is retried within the admission budget. The first stop fails,
// the waiter retires the driver again, and the admission succeeds without evicting newer.
func TestSessionCapacityFailedRetirementIsRetriedWithinBudget(t *testing.T) {
	rt := newPressureRuntime(t, 2)
	rt.sessionDrivers.capacityWaitBudget = 10 * time.Second
	oldDriver, _ := newPressurePair(t, rt)
	attachStopFault(t, oldDriver, 1)
	entered := observeCapacityWait(rt)
	_, err := rt.CreateSession(t.Context(), session.New(session.WithID("pressure")), SessionBinding{})
	require.NoError(t, err)
	waitClosed(t, entered, "a failed retirement is retried after a bounded wait")
	_, oldResident := rt.sessionDrivers.Lookup("old")
	_, newerResident := rt.sessionDrivers.Lookup("newer")
	assert.False(t, oldResident, "the oldest driver is reclaimed once its retirement succeeds")
	assert.True(t, newerResident, "younger settled driver is never evicted past the oldest")
}

// A permanently failing retirement holds capacity: admission fails with the typed
// capacity error once the budget expires. Nothing younger is evicted. This is the
// pending policy decision recorded in the report.
func TestSessionCapacityPersistentRetirementFailureReportsTypedCapacity(t *testing.T) {
	rt := newPressureRuntime(t, 2)
	rt.sessionDrivers.capacityWaitBudget = 100 * time.Millisecond
	oldDriver, _ := newPressurePair(t, rt)
	attachStopFault(t, oldDriver, 1<<20)
	_, err := rt.CreateSession(t.Context(), session.New(session.WithID("pressure")), SessionBinding{})
	require.ErrorIs(t, err, ErrSessionCapacity)
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionOperationCreateSession, sessionErr.Operation)
	_, oldResident := rt.sessionDrivers.Lookup("old")
	_, newerResident := rt.sessionDrivers.Lookup("newer")
	assert.True(t, oldResident, "the driver whose retirement keeps failing stays registered")
	assert.True(t, newerResident, "younger settled driver is never evicted as a fallback")
}

// A cold restore under capacity pressure waits for the busy oldest driver, then
// succeeds after release, and never evicts the younger driver.
func TestSessionViewRestoreWaitsForBusyOldestThenSucceeds(t *testing.T) {
	store := session.NewInMemorySessionStore()
	rt := newPressureRuntimeWith(t, 2, WithSessionStore(store))
	rt.sessionDrivers.capacityWaitBudget = 10 * time.Second
	oldDriver, _ := newPressurePair(t, rt)
	cold := session.New(session.WithID("cold"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	cold.AgentName = "root"
	require.NoError(t, store.AddSession(t.Context(), cold))
	entered := observeCapacityWait(rt)
	oldDriver.mu.Lock()
	result := make(chan error, 1)
	go func() {
		prepared, err := (&localSessionRuntimeView{runtime: rt}).PrepareSessionView(t.Context(), "cold")
		if err == nil {
			prepared.Abort()
		}
		result <- err
	}()
	waitClosed(t, entered, "cold restore waiting on the busy oldest driver")
	oldDriver.mu.Unlock()
	require.NoError(t, <-result)
	_, oldResident := rt.sessionDrivers.Lookup("old")
	_, newerResident := rt.sessionDrivers.Lookup("newer")
	assert.False(t, oldResident, "the oldest eligible driver is reclaimed for the restore")
	assert.True(t, newerResident, "younger settled driver is never evicted past the oldest")
}

// A subagent spawn under capacity pressure waits for the busy oldest driver, then
// admits the child. The parent is the younger resident and is never evicted.
func TestSubagentSpawnWaitsForBusyOldestThenSucceeds(t *testing.T) {
	policy := DefaultSessionResourcePolicy()
	policy.MaxSessions = 2
	policy.IdleRetention = 0
	rt, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), coordinationReply("root"), coordinationReply("worker"), WithSessionResourcePolicy(policy))
	rt.sessionDrivers.capacityWaitBudget = 10 * time.Second
	parent := coordinationCreate(t, owner.Runtime(), "spawn-parent", "")
	old := coordinationCreate(t, owner.Runtime(), "spawn-old", "")
	parentDriver := parent.(*sessionHandle).driver
	oldDriver := old.(*sessionHandle).driver
	require.NoError(t, oldDriver.ownerCall(t.Context(), func() error {
		oldDriver.lastActive = time.Time{}
		return nil
	}))
	require.Eventually(t, func() bool {
		return driverDrained(oldDriver) && driverDrained(parentDriver)
	}, 5*time.Second, time.Millisecond)
	parentSession := parentDriver.session()
	entered := observeCapacityWait(rt)
	oldDriver.mu.Lock()
	result := make(chan error, 1)
	go func() {
		_, err := rt.subagents.Spawn(parentSession, "root", subagent.AllowedSubagent{Agent: "worker"}, "")
		result <- err
	}()
	waitClosed(t, entered, "spawn waiting on the busy oldest driver")
	oldDriver.mu.Unlock()
	require.NoError(t, <-result)
	_, oldResident := rt.sessionDrivers.Lookup("spawn-old")
	_, parentResident := rt.sessionDrivers.Lookup("spawn-parent")
	assert.False(t, oldResident, "the oldest eligible driver is reclaimed for the child")
	assert.True(t, parentResident, "the younger resident parent is never evicted")
}

// blockingStopToolset is a resource owner whose stop blocks until released. It
// ignores the stop context on purpose: only the retirement's own bounds apply.
type blockingStopToolset struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (*blockingStopToolset) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

func (s *blockingStopToolset) StopResourceOwner(context.Context) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return nil
}

// sameShardIDs returns n distinct IDs that hash to the same publication and
// initialization shard as id, computed from this registry's seed.
func sameShardIDs(g *sessionDriverRegistry, id string, n int) []string {
	publicationShard := func(s string) uint64 { return maphash.String(g.publicationSeed, s) % uint64(len(g.publishing)) }
	initShard := func(s string) uint64 { return maphash.String(g.publicationSeed, s) % uint64(len(g.initializing)) }
	wantPublication, wantInit := publicationShard(id), initShard(id)
	ids := make([]string, 0, n)
	for i := 0; len(ids) < n; i++ {
		candidate := fmt.Sprintf("%s-shard-%d", id, i)
		if publicationShard(candidate) == wantPublication && initShard(candidate) == wantInit {
			ids = append(ids, candidate)
		}
	}
	return ids
}

func createIdleSession(t *testing.T, rt *LocalRuntime, id string) *sessionDriver {
	t.Helper()
	h, err := rt.CreateSession(t.Context(), session.New(session.WithID(id)), SessionBinding{})
	require.NoError(t, err)
	return h.(*sessionHandle).driver
}

// A blocked resource cleanup on the oldest driver never stalls admission past its
// budget, and never blocks sessions that share its publication or initialization
// shard. Once released, a retry admission reclaims the oldest.
func TestSessionCapacityBlockedCleanupBoundsAdmissionAndSparesSharedShards(t *testing.T) {
	rt := newPressureRuntime(t, 3)
	g := rt.sessionDrivers
	g.capacityWaitBudget = 50 * time.Millisecond
	oldDriver := createIdleSession(t, rt, "old")
	require.NoError(t, oldDriver.ownerCall(t.Context(), func() error {
		oldDriver.lastActive = time.Time{}
		return nil
	}))
	ids := sameShardIDs(g, "old", 2)
	collide := createIdleSession(t, rt, ids[0])
	newerDriver := createIdleSession(t, rt, "newer")
	require.Eventually(t, func() bool {
		return driverDrained(oldDriver) && driverDrained(collide) && driverDrained(newerDriver)
	}, 5*time.Second, time.Millisecond)

	blocking := &blockingStopToolset{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	releaseStop := func() { releaseOnce.Do(func() { close(blocking.release) }) }
	t.Cleanup(releaseStop)
	require.NoError(t, oldDriver.ownerCall(t.Context(), func() error {
		oldDriver.sess.ExtraToolSets = append(oldDriver.sess.ExtraToolSets, blocking)
		return nil
	}))

	start := time.Now()
	_, err := rt.CreateSession(t.Context(), session.New(session.WithID("pressure")), SessionBinding{})
	require.ErrorIs(t, err, ErrSessionCapacity)
	assert.Less(t, time.Since(start), defaultSubagentPersistenceTimeout, "admission returns within its budget, not the retirement bound")
	waitClosed(t, blocking.entered, "background retirement reached the blocked stop")

	// Unrelated sessions stay responsive while the cleanup is blocked, including
	// the one that shares the oldest session's publication and initialization shard.
	require.NoError(t, collide.ownerCall(t.Context(), func() error { return nil }))
	_, newerResident := g.Lookup("newer")
	require.True(t, newerResident)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	resolved, err := g.GetInitialized(ctx, session.New(session.WithID(ids[0])))
	require.NoError(t, err)
	require.Same(t, collide, resolved)
	start = time.Now()
	_, err = rt.CreateSession(t.Context(), session.New(session.WithID(ids[1])), SessionBinding{})
	require.ErrorIs(t, err, ErrSessionCapacity, "a same-shard admission is bounded, not blocked behind the cleanup")
	assert.Less(t, time.Since(start), defaultSubagentPersistenceTimeout)

	// Released, the retirement completes and a retry admission reclaims the oldest.
	releaseStop()
	g.capacityWaitBudget = 10 * time.Second
	_, err = rt.CreateSession(t.Context(), session.New(session.WithID("retry")), SessionBinding{})
	require.NoError(t, err)
	_, oldResident := g.Lookup("old")
	_, newerResident = g.Lookup("newer")
	_, collideResident := g.Lookup(ids[0])
	assert.False(t, oldResident, "the oldest driver is reclaimed once its cleanup completes")
	assert.True(t, newerResident, "younger settled driver is never evicted past the oldest")
	assert.True(t, collideResident, "same-shard session is never evicted")
}
