package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

const registryWaiterLabel = "runtime-test-registry-waiter"

var registryWaiterLabelSequence atomic.Uint64

func newRegistryWaiterLabel(t *testing.T, g *sessionDriverRegistry) string {
	t.Helper()
	return fmt.Sprintf("%s/%p/%d", t.Name(), g, registryWaiterLabelSequence.Add(1))
}

// Labels scope the assertion to descendants of the operation, not unrelated
// background drivers. Read the complete profile and decode exact label values.
func registryWaiterCount(t *testing.T, label string) int {
	t.Helper()
	var profile bytes.Buffer
	require.NoError(t, pprof.Lookup("goroutine").WriteTo(&profile, 1))
	count := 0
	for record := range strings.SplitSeq(profile.String(), "\n\n") {
		lines := strings.Split(record, "\n")
		for _, line := range lines {
			metadata, ok := strings.CutPrefix(line, "# labels: ")
			if !ok {
				continue
			}
			var labels map[string]string
			require.NoError(t, json.Unmarshal([]byte(metadata), &labels))
			if labels[registryWaiterLabel] != label {
				continue
			}
			found := false
			for _, header := range lines {
				amount, _, ok := strings.Cut(header, " @ ")
				if !ok {
					continue
				}
				n, err := strconv.Atoi(amount)
				require.NoError(t, err)
				require.Positive(t, n)
				count += n
				found = true
				break
			}
			require.True(t, found, "labeled goroutine profile record must have a count")
		}
	}
	return count
}

func TestRegistryWaiterLabelsDetectOwnedGoroutine(t *testing.T) {
	g := newSessionDriverRegistry(nil)
	label := newRegistryWaiterLabel(t, g)
	ready, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer func() {
		unblock()
		waitClosed(t, done, "labeled waiter exit")
	}()
	pprof.Do(t.Context(), pprof.Labels(registryWaiterLabel, label), func(context.Context) {
		go func() {
			defer close(done)
			close(ready)
			<-release
		}()
	})
	waitClosed(t, ready, "labeled waiter ready")
	assert.Equal(t, 1, registryWaiterCount(t, label), "the oracle must detect an inherited operation label")
	assert.Zero(t, registryWaiterCount(t, label+"/other"), "label matching must be exact")
	unblock()
	waitClosed(t, done, "labeled waiter exit")
	assert.Zero(t, registryWaiterCount(t, label))
}

func TestReleaseTimeoutDoesNotLeaveWaiterGoroutine(t *testing.T) {
	g := newSessionDriverRegistry(nil)
	d := newSessionDriver(&LocalRuntime{}, session.New(session.WithID("release-timeout")))
	d.wg.Add(1)
	finishWork := sync.OnceFunc(d.wg.Done)
	defer finishWork()
	g.drivers[d.sessionID()] = d
	label := newRegistryWaiterLabel(t, g)

	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	var err error
	pprof.Do(ctx, pprof.Labels(registryWaiterLabel, label), func(ctx context.Context) {
		err = g.Release(ctx, d.sessionID())
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Zero(t, registryWaiterCount(t, label))

	finishWork()
	select {
	case <-d.Done():
	case <-time.After(time.Second):
		t.Fatal("driver work did not drain after timed-out release")
	}
	closeCtx, closeCancel := context.WithTimeout(t.Context(), time.Second)
	defer closeCancel()
	require.NoError(t, g.CloseContext(closeCtx))
	assert.Empty(t, g.drivers)
}

func TestCloseTimeoutDoesNotLeaveWaiterGoroutinesAndCanFinish(t *testing.T) {
	g := newSessionDriverRegistry(nil)
	const drivers = 32
	all := make([]*sessionDriver, 0, drivers)
	for i := range drivers {
		d := newSessionDriver(&LocalRuntime{}, session.New(session.WithID(string(rune('a'+i)))))
		d.wg.Add(1)
		g.drivers[d.sessionID()] = d
		all = append(all, d)
	}
	finishWork := sync.OnceFunc(func() {
		for _, d := range all {
			d.wg.Done()
		}
	})
	defer finishWork()
	label := newRegistryWaiterLabel(t, g)
	for range 8 {
		ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
		var err error
		pprof.Do(ctx, pprof.Labels(registryWaiterLabel, label), func(ctx context.Context) {
			err = g.CloseContext(ctx)
		})
		cancel()
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Zero(t, registryWaiterCount(t, label))
	}

	finishWork()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, g.CloseContext(ctx))
	assert.Empty(t, g.drivers)
}
