package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// leaseRefs sums the outstanding leases the store holds across all revisions.
func leaseRefs(store *TeamEnvironmentStore) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	total := 0
	for _, env := range store.entries {
		total += env.refs
	}
	return total
}

// runLeaseGateProvider blocks its first model call until release is closed, so
// a test can admit more input while the first run is provably in flight.
type runLeaseGateProvider struct {
	*mockProvider

	mu      sync.Mutex
	calls   int
	inputs  []string
	entered chan struct{}
	release chan struct{}
}

func (p *runLeaseGateProvider) CreateChatCompletionStream(ctx context.Context, msgs []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	last := ""
	for _, msg := range msgs {
		if msg.Role == chat.MessageRoleUser {
			last = msg.Content
		}
	}
	p.inputs = append(p.inputs, last)
	p.mu.Unlock()
	if call == 1 {
		close(p.entered)
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build(), nil
}

func (p *runLeaseGateProvider) recorded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.inputs...)
}

// newRunLeaseRuntime builds a runtime whose session is bound to a one-revision
// team environment store, so every run acquires and releases a real lease.
func newRunLeaseRuntime(t *testing.T, prov provider.Provider) (*sessionDriver, *TeamEnvironmentStore, *LocalRuntime) {
	t.Helper()
	tm := team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(prov))))
	env, err := NewTeamEnvironment("rev-1", "source-1", "baseline-1", "team-1", tm, ModelSwitcherConfig{}, func(context.Context) error { return nil })
	require.NoError(t, err)
	envs, err := NewTeamEnvironmentStore(env)
	require.NoError(t, err)
	store := session.NewInMemorySessionStore()
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithTeamEnvironmentStore(envs))
	require.NoError(t, err)
	sess := session.New(session.WithID(t.Name() + "/session"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	return rt.sessionDrivers.Get(sess), envs, rt
}

// Input admitted while the first run is in flight is consumed by a successor
// run. The successor replaces the run lease, so the predecessor's lease must be
// released: after everything settles only the pinned lease remains.
func TestSuccessorRunReleasesPredecessorEnvironmentLease(t *testing.T) {
	prov := &runLeaseGateProvider{
		mockProvider: &mockProvider{id: "test/mock-model"},
		entered:      make(chan struct{}),
		release:      make(chan struct{}),
	}
	d, envs, rt := newRunLeaseRuntime(t, prov)

	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "first"}, true))
	select {
	case <-prov.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first model call never entered")
	}
	require.Equal(t, 2, leaseRefs(envs), "pinned lease plus the in-flight run lease")
	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "second"}, true))
	close(prov.release)
	d.Wait()

	require.Equal(t, []string{"first", "second"}, prov.recorded())
	require.Equal(t, 1, leaseRefs(envs), "the predecessor's run lease must not leak into the successor")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, NewSessionRuntimeSupervisor(rt).Shutdown(ctx))
	require.Zero(t, leaseRefs(envs), "shutdown releases every lease")
}

// A submit made from a settled callback starts a new run before the settling
// run releases its lease. The overwrite must not leak the settling run's lease.
func TestSettledCallbackSubmitDoesNotLeakRunLease(t *testing.T) {
	prov := &runLeaseGateProvider{
		mockProvider: &mockProvider{id: "test/mock-model"},
		entered:      make(chan struct{}),
		release:      make(chan struct{}),
	}
	close(prov.release)
	d, envs, rt := newRunLeaseRuntime(t, prov)

	var once sync.Once
	var admitted bool
	unwatch := d.OnSettled(func() {
		once.Do(func() {
			admitted = d.Post(t.Context(), QueuedMessage{Content: "second"}, true)
		})
	})
	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "first"}, true))
	d.Wait()
	unwatch()

	require.True(t, admitted, "the settled callback must admit the second submit")
	require.Equal(t, []string{"first", "second"}, prov.recorded())
	require.Equal(t, 1, leaseRefs(envs), "a callback-started run must not leak the settling run's lease")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, NewSessionRuntimeSupervisor(rt).Shutdown(ctx))
	require.Zero(t, leaseRefs(envs), "shutdown releases every lease")
}
