package runtime

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// Concurrent initialization of one caller-owned session pointer must not write
// it: the first publication, and later calls while the driver's owner is busy,
// all resolve to the same resident driver without racing on the pointer.
func TestConcurrentGetInitializedSharesDriverWithoutWritingCallerSession(t *testing.T) {
	tm := team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(&mockProvider{id: "test/mock-model"}))))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	sess := session.New(session.WithID(t.Name() + "/session"))

	const callers = 8
	drivers := make([]*sessionDriver, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Go(func() {
			<-start
			drivers[i], errs[i] = rt.sessionDrivers.GetInitialized(t.Context(), sess)
		})
	}
	close(start)
	wg.Wait()

	for i := range callers {
		require.NoError(t, errs[i])
		require.Same(t, drivers[0], drivers[i], "every caller must share the first published driver")
	}
	// The resident owner keeps running while a later initialization races it.
	d := drivers[0]
	for range 4 {
		again, err := rt.sessionDrivers.GetInitialized(t.Context(), sess)
		require.NoError(t, err)
		require.Same(t, d, again)
	}
}
