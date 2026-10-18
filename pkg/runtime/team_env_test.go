package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/team"
)

func environmentForTest(t *testing.T, revision, source, baseline string, dispose func(context.Context) error, models ModelSwitcherConfig) *TeamEnvironment {
	t.Helper()
	if dispose == nil {
		dispose = func(context.Context) error { return nil }
	}
	e, err := NewTeamEnvironment(revision, source, baseline, "team", team.New(), models, dispose)
	require.NoError(t, err)
	return e
}

func TestTeamEnvironmentPublishRetainsOldRevisionUntilLeaseRelease(t *testing.T) {
	t.Parallel()
	var first, second atomic.Int32
	initial := environmentForTest(t, "r1", "source", "baseline", func(context.Context) error { first.Add(1); return nil }, ModelSwitcherConfig{})
	store, err := NewTeamEnvironmentStore(initial)
	require.NoError(t, err)
	lease, err := store.AcquireCurrent()
	require.NoError(t, err)
	next := environmentForTest(t, "r2", "source", "baseline", func(context.Context) error { second.Add(1); return nil }, ModelSwitcherConfig{})
	require.NoError(t, store.Publish("source", "r1", next))
	assert.Zero(t, first.Load())
	oldLease, err := store.Acquire("r1")
	require.NoError(t, err)
	require.NoError(t, oldLease.Release(t.Context()))
	require.Zero(t, first.Load())
	require.NoError(t, lease.Release(t.Context()))
	require.Eventually(t, func() bool { return first.Load() == 1 }, time.Second, 5*time.Millisecond)
	current, err := store.AcquireCurrent()
	require.NoError(t, err)
	assert.Equal(t, "r2", current.Environment().RevisionID())
	require.NoError(t, current.Release(t.Context()))
	require.NoError(t, store.Close(t.Context()))
	require.EqualValues(t, 1, first.Load())
	require.EqualValues(t, 1, second.Load())
}

func TestTeamEnvironmentFailedPublishLeavesCandidateOwnedByCaller(t *testing.T) {
	t.Parallel()
	var currentClosed, candidateClosed atomic.Int32
	initial := environmentForTest(t, "r1", "source", "base", func(context.Context) error { currentClosed.Add(1); return nil }, ModelSwitcherConfig{})
	store, err := NewTeamEnvironmentStore(initial)
	require.NoError(t, err)
	candidate := environmentForTest(t, "r2", "source", "other-base", func(context.Context) error { candidateClosed.Add(1); return nil }, ModelSwitcherConfig{})
	require.ErrorContains(t, store.Publish("source", "r1", candidate), "crossed source or baseline")
	require.ErrorContains(t, store.Publish("source", "stale", candidate), "conflict")
	require.ErrorContains(t, store.Publish("source", "r1", initial), "current team environment")
	assert.Zero(t, candidateClosed.Load())
	require.NoError(t, candidate.dispose(t.Context()))
	require.EqualValues(t, 1, candidateClosed.Load())
	require.NoError(t, store.Close(t.Context()))
	require.EqualValues(t, 1, currentClosed.Load())
}

func TestTeamEnvironmentCloseReportsLeaseThenFinalReleaseCleans(t *testing.T) {
	t.Parallel()
	var closed atomic.Int32
	env := environmentForTest(t, "r1", "source", "base", func(context.Context) error { closed.Add(1); return nil }, ModelSwitcherConfig{})
	store, err := NewTeamEnvironmentStore(env)
	require.NoError(t, err)
	lease, err := store.AcquireCurrent()
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = store.Close(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, ErrTeamEnvironmentLeasesOutstanding)
	assert.Zero(t, closed.Load())
	releaseCtx, cancelRelease := context.WithCancel(t.Context())
	cancelRelease()
	err = lease.Release(releaseCtx)
	require.ErrorIs(t, err, context.Canceled)
	require.Eventually(t, func() bool { return closed.Load() == 1 }, time.Second, 5*time.Millisecond)
	require.NoError(t, store.Close(t.Context()))
}

func TestTeamEnvironmentConcurrentCASHasOneWinner(t *testing.T) {
	t.Parallel()
	initial, err := testTeamEnvironmentNoClose(t, "r1", "base")
	require.NoError(t, err)
	store, err := NewTeamEnvironmentStore(initial)
	require.NoError(t, err)
	candidates := make([]*TeamEnvironment, 16)
	for i := range candidates {
		candidates[i], err = testTeamEnvironmentNoClose(t, "r-next-"+string(rune('a'+i)), "base")
		require.NoError(t, err)
	}
	var winners, losers atomic.Int32
	var wg sync.WaitGroup
	for _, candidate := range candidates {
		wg.Add(1)
		go func(next *TeamEnvironment) {
			defer wg.Done()
			if err := store.Publish("source", "r1", next); err == nil {
				winners.Add(1)
			} else {
				losers.Add(1)
			}
		}(candidate)
	}
	wg.Wait()
	require.EqualValues(t, 1, winners.Load())
	require.EqualValues(t, 15, losers.Load())
	require.NoError(t, store.Close(t.Context()))
}

func TestTeamEnvironmentModelSnapshotsDeepCopyMutableMaps(t *testing.T) {
	t.Parallel()
	models := map[string]latest.ModelConfig{"m": {Provider: "openai", Model: "one"}}
	providers := map[string]latest.ProviderConfig{"p": {Provider: "openai", BaseURL: "https://example.invalid"}}
	env := environmentForTest(t, "r", "s", "b", nil, ModelSwitcherConfig{Models: models, Providers: providers, AgentDefaultModels: map[string]string{"root": "m"}})
	models["m"] = latest.ModelConfig{Model: "caller mutation"}
	delete(providers, "p")
	snapshot, err := env.ModelSwitcherConfig()
	require.NoError(t, err)
	assert.Equal(t, "one", snapshot.Models["m"].Model)
	assert.Contains(t, snapshot.Providers, "p")
	snapshot.Models["m"] = latest.ModelConfig{Model: "returned mutation"}
	again, err := env.ModelSwitcherConfig()
	require.NoError(t, err)
	assert.Equal(t, "one", again.Models["m"].Model)
}

func TestTeamEnvironmentDisposalErrorRecordedExactlyOnce(t *testing.T) {
	t.Parallel()
	boom := errors.New("stop team")
	var calls atomic.Int32
	env := environmentForTest(t, "r", "s", "b", func(context.Context) error { calls.Add(1); return boom }, ModelSwitcherConfig{})
	store, err := NewTeamEnvironmentStore(env)
	require.NoError(t, err)
	lease, err := store.AcquireCurrent()
	require.NoError(t, err)
	next := environmentForTest(t, "r2", "s", "b", nil, ModelSwitcherConfig{})
	require.NoError(t, store.Publish("s", "r", next))
	require.Zero(t, calls.Load())
	require.ErrorIs(t, lease.Release(t.Context()), boom)
	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, 5*time.Millisecond)
	require.ErrorIs(t, store.Close(t.Context()), boom)
}

func TestTeamEnvironmentRevisionIDsCannotBeReusedAndPanicsAreContained(t *testing.T) {
	t.Parallel()
	initial, err := testTeamEnvironmentNoClose(t, "r1", "base")
	require.NoError(t, err)
	store, err := NewTeamEnvironmentStore(initial)
	require.NoError(t, err)
	second, err := testTeamEnvironmentNoClose(t, "r2", "base")
	require.NoError(t, err)
	require.NoError(t, store.Publish("source", "r1", second))
	reused, err := testTeamEnvironmentNoClose(t, "r1", "base")
	require.NoError(t, err)
	require.ErrorContains(t, store.Publish("source", "r2", reused), "already used")
	require.NoError(t, reused.dispose(t.Context()))
	panicEnv, err := NewTeamEnvironment("r3", "source", "base", "team", team.New(), ModelSwitcherConfig{}, func(context.Context) error { panic("dispose panic") })
	require.NoError(t, err)
	require.NoError(t, store.Publish("source", "r2", panicEnv))
	require.ErrorContains(t, store.Close(t.Context()), "disposing team environment panicked")
}

func testTeamEnvironmentNoClose(t *testing.T, revision, baseline string) (*TeamEnvironment, error) {
	t.Helper()
	return NewTeamEnvironment(revision, "source", baseline, "team", team.New(), ModelSwitcherConfig{}, func(context.Context) error { return nil })
}

func TestTeamEnvironmentPublishWithCommitCouplesSourceCAS(t *testing.T) {
	t.Parallel()
	current := environmentForTest(t, "r1", "source", "base", nil, ModelSwitcherConfig{})
	store, err := NewTeamEnvironmentStore(current)
	require.NoError(t, err)
	next := environmentForTest(t, "r2", "source", "base", nil, ModelSwitcherConfig{})
	var sourceHead atomic.Int32
	commitRejected := errors.New("source CAS conflict")
	err = store.PublishWithCommit("source", "r1", next, func() error { return commitRejected })
	require.ErrorIs(t, err, commitRejected)
	assert.Zero(t, sourceHead.Load())
	lease, err := store.AcquireCurrent()
	require.NoError(t, err)
	assert.Equal(t, "r1", lease.Environment().RevisionID(), "failed combined CAS must not publish either head")
	require.NoError(t, lease.Release(t.Context()))

	committedRevision := ""
	err = store.PublishWithCommit("source", "r1", next, func() error { committedRevision = "r2"; sourceHead.Add(1); return nil })
	require.NoError(t, err)
	assert.Equal(t, "r2", committedRevision)
	assert.EqualValues(t, 1, sourceHead.Load())
	currentLease, err := store.AcquireCurrent()
	require.NoError(t, err)
	assert.Equal(t, committedRevision, currentLease.Environment().RevisionID())
	require.NoError(t, currentLease.Release(t.Context()))
	require.NoError(t, store.Close(t.Context()))
}

func TestTeamEnvironmentDiscardCandidateIdempotentAndRejectPublished(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	candidate := environmentForTest(t, "candidate", "source", "base", func(context.Context) error { calls.Add(1); return nil }, ModelSwitcherConfig{})
	require.NoError(t, candidate.Discard(t.Context()))
	require.NoError(t, candidate.Discard(t.Context()))
	require.EqualValues(t, 1, calls.Load())
	current := environmentForTest(t, "current", "source", "base", nil, ModelSwitcherConfig{})
	store, err := NewTeamEnvironmentStore(current)
	require.NoError(t, err)
	require.ErrorContains(t, current.Discard(t.Context()), "owned by its store")
	require.NoError(t, store.Close(t.Context()))
}

func TestTeamEnvironmentDiscardAndPublishRaceHasSingleOwner(t *testing.T) {
	t.Parallel()
	initial := environmentForTest(t, "base", "source", "baseline", nil, ModelSwitcherConfig{})
	store, err := NewTeamEnvironmentStore(initial)
	require.NoError(t, err)
	var disposeCount atomic.Int32
	candidate := environmentForTest(t, "next", "source", "baseline", func(context.Context) error { disposeCount.Add(1); return nil }, ModelSwitcherConfig{})
	start := make(chan struct{})
	publishErr := make(chan error, 1)
	discardErr := make(chan error, 1)
	go func() { <-start; publishErr <- store.Publish("source", "base", candidate) }()
	go func() { <-start; discardErr <- candidate.Discard(t.Context()) }()
	close(start)
	pe, de := <-publishErr, <-discardErr
	if pe == nil {
		require.ErrorContains(t, de, "owned by its store")
		require.NoError(t, store.Close(t.Context()))
	} else {
		require.NoError(t, de)
		require.ErrorContains(t, pe, "discarded")
		require.NoError(t, store.Close(t.Context()))
	}
	require.LessOrEqual(t, disposeCount.Load(), int32(1))
}
