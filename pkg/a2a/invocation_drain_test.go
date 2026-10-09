package a2a

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/host/invocation"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/servesafety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

type drainProvider struct {
	started, cancelled, release chan struct{}
}

func (p *drainProvider) ID() modelsdev.ID        { return modelsdev.ParseIDOrZero("test/drain") }
func (p *drainProvider) BaseConfig() base.Config { return base.Config{} }
func (p *drainProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	close(p.started)
	<-ctx.Done()
	close(p.cancelled)
	<-p.release
	return nil, ctx.Err()
}

type drainStore struct {
	session.Store

	closed           atomic.Int32
	writesAfterClose atomic.Int32
}

func (s *drainStore) UpdateSession(ctx context.Context, sess *session.Session) error {
	if s.closed.Load() != 0 {
		s.writesAfterClose.Add(1)
	}
	return s.Store.UpdateSession(ctx, sess)
}

func TestInvocationDrainTracksDetachedADKWorker(t *testing.T) {
	for _, borrowed := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned", true: "borrowed"}[borrowed], func(t *testing.T) {
			p := &drainProvider{started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
			tm, _ := newTeamWithProvider(p)
			store := &drainStore{Store: session.NewInMemorySessionStore()}
			var stops atomic.Int32
			lifetime := invocation.New(func() error { store.closed.Add(1); stops.Add(1); return nil })
			var registry runtime.SessionRuntime
			if borrowed {
				rt, err := runtime.NewLocalRuntime(t.Context(), tm, runtime.WithSessionStore(store))
				require.NoError(t, err)
				owner := runtime.NewSessionRuntimeSupervisor(rt)
				registry = owner.Runtime()
				t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
			}
			adapter, err := newDockerAgentAdapterWithLifetime(tm, "root", store, servesafety.Resolved{Policy: session.SafetyPolicyRestricted}, t.TempDir(), lifetime, registry)
			require.NoError(t, err)
			workerDone := make(chan struct{})
			go func() {
				defer close(workerDone)
				// A2A's task manager detaches execution from HTTP request cancellation.
				for range adapter.Run(newFakeInvocationContext(context.WithoutCancel(t.Context()), "drain-a2a", "hi")) {
				}
			}()
			<-p.started
			lifetime.Fence()
			<-p.cancelled
			expired, cancel := context.WithCancel(t.Context())
			cancel()
			err = lifetime.Close(expired)
			var drainErr *invocation.DrainError
			require.ErrorAs(t, err, &drainErr)
			assert.Zero(t, store.closed.Load())
			assert.Zero(t, stops.Load())
			close(p.release)
			require.NoError(t, drainErr.Retry(t.Context()))
			<-workerDone
			require.NoError(t, lifetime.Close(t.Context()))
			assert.EqualValues(t, 1, store.closed.Load())
			assert.EqualValues(t, 1, stops.Load())
			assert.Zero(t, store.writesAfterClose.Load())
			if borrowed {
				_, err = registry.CreateSession(t.Context(), session.New(), runtime.SessionBinding{AgentName: "root"})
				require.NoError(t, err, "borrowed registry must remain open")
			}
			_, _, err = lifetime.Begin(t.Context())
			assert.ErrorIs(t, err, invocation.ErrClosing)
		})
	}
}
