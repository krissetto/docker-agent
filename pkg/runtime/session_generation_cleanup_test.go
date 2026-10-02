package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestSessionGenerationCleanupAtSettlement(t *testing.T) {
	for _, scenario := range []string{"settled", "successor", "promotion failure", "completion retry"} {
		t.Run(scenario, func(t *testing.T) {
			r := newDriverTestRuntime(t)
			r.sessionDrivers.closed = true // Drive settlement explicitly, without scheduler retries.
			lifetime, cancelLifetime := context.WithCancel(t.Context())
			defer cancelLifetime()
			r.lifecycleCtx = lifetime
			d := newSessionDriver(r, session.New(session.WithID("cleanup")))
			runCtx, generation, _, err := d.prepareStart(t.Context(), false)
			require.NoError(t, err)
			defer d.wg.Done()
			originalCancel := d.cancel
			calls := 0
			d.cancel = func() {
				d.mu.Lock() // Cleanup must run outside the driver lock.
				calls++
				d.mu.Unlock()
				originalCancel()
			}
			switch scenario {
			case "successor":
				d.pending = []QueuedMessage{{RequestID: "next", Retry: true}}
			case "promotion failure":
				d.pending = []QueuedMessage{{RequestID: "missing"}}
			case "completion retry":
				observer := newPersistenceObserver(session.NewInMemorySessionStore())
				r.observers = []EventObserver{observer}
				attempts := 0
				observer.journal("cleanup").pending = []persistenceEffect{{write: func(context.Context) error {
					attempts++
					if attempts == 1 {
						return assert.AnError
					}
					return nil
				}}}
				_, _, again := d.finishRun(generation, "")
				require.False(t, again)
				require.Zero(t, calls)
				require.NoError(t, runCtx.Err(), "retry retains the original generation context")
			}
			nextCtx, nextGeneration, again := d.finishRun(generation, "")
			require.Equal(t, 1, calls)
			require.ErrorIs(t, runCtx.Err(), context.Canceled)
			if scenario == "successor" {
				require.True(t, again)
				require.NoError(t, nextCtx.Err(), "cleanup must not cancel the successor")
				_, _, again = d.finishRun(nextGeneration, "")
				require.False(t, again)
				require.ErrorIs(t, nextCtx.Err(), context.Canceled)
			} else {
				require.False(t, again)
			}
			d.finishRun(generation, "")
			require.Equal(t, 1, calls, "repeated settlement does not repeat cleanup")
			if scenario == "promotion failure" {
				require.Len(t, d.pending, 1, "failed promotion preserves accepted work")
			}
		})
	}
}
