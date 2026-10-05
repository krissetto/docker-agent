package runtime

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/team"
)

// TestOnToolsChangedConcurrentRegisterAndEmit pins the fix for
// onToolsChanged: it used to be a plain func(Event) field, written by
// OnToolsChanged and read by emitToolsChanged, with no lock — unlike its
// sibling onBackgroundEvent, which is guarded by backgroundEventMu because
// background tasks read it from their own goroutines. MCP change-
// notification goroutines call emitToolsChanged concurrently with the
// handler being (re)registered, so onToolsChanged needs the same guard.
func TestOnToolsChangedConcurrentRegisterAndEmit(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model"}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	var calls atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			rt.OnToolsChanged(func(Event) {
				calls.Add(1)
			})
		})
		wg.Go(func() {
			rt.emitToolsChanged(root)
		})
	}
	wg.Wait()
}
