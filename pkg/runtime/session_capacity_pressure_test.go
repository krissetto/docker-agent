package runtime

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func newPressureRuntime(t *testing.T, maxSessions int) *LocalRuntime {
	t.Helper()
	policy := DefaultSessionResourcePolicy()
	policy.MaxSessions = maxSessions
	policy.IdleRetention = 0
	prov := &mockProvider{id: "test/pressure", stream: newStreamBuilder().AddStopWithUsage(1, 1).Build()}
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(prov)))), WithSessionResourcePolicy(policy))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	return rt
}

func TestSessionCapacityPressureReclaimsOldestEligibleSettled(t *testing.T) {
	rt := newPressureRuntime(t, 2)
	old, err := rt.CreateSession(t.Context(), session.New(session.WithID("old")), SessionBinding{})
	require.NoError(t, err)
	_, err = rt.CreateSession(t.Context(), session.New(session.WithID("newer")), SessionBinding{})
	require.NoError(t, err)
	_, err = rt.CreateSession(t.Context(), session.New(session.WithID("pressure")), SessionBinding{})
	require.NoError(t, err)
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
		{"steering", func(d *sessionDriver) func() {
			d.mu.Lock()
			d.steering = append(d.steering, QueuedMessage{Content: "s"})
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
