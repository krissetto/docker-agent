package mcp

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/host/invocation"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type drainProvider struct{ started, cancelled, release chan struct{} }

func (p *drainProvider) ID() modelsdev.ID        { return modelsdev.ParseIDOrZero("test/drain") }
func (p *drainProvider) BaseConfig() base.Config { return base.Config{} }
func (p *drainProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	close(p.started)
	<-ctx.Done()
	close(p.cancelled)
	<-p.release
	return nil, ctx.Err()
}

func TestToolInvocationDrainRetainsTeam(t *testing.T) {
	for _, borrowed := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned", true: "borrowed"}[borrowed], func(t *testing.T) {
			p := &drainProvider{started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
			tm := team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(p))))
			var stops atomic.Int32
			lifetime := invocation.New(func() error { stops.Add(1); return nil })
			var registry runtime.SessionRuntime
			if borrowed {
				rt, err := runtime.NewLocalRuntime(t.Context(), tm, runtime.WithSessionStore(session.NewInMemorySessionStore()))
				require.NoError(t, err)
				owner := runtime.NewSessionRuntimeSupervisor(rt)
				registry = owner.Runtime()
				t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
			}
			handler := createToolHandlerWithLifetime(tm, "root", session.SafetyPolicyRestricted, t.TempDir(), lifetime, registry)
			workerDone := make(chan error, 1)
			go func() { _, _, err := handler(t.Context(), nil, ToolInput{Message: "hi"}); workerDone <- err }()
			<-p.started
			lifetime.Fence()
			<-p.cancelled
			expired, cancel := context.WithCancel(t.Context())
			cancel()
			var drainErr *invocation.DrainError
			require.ErrorAs(t, lifetime.Close(expired), &drainErr)
			assert.Zero(t, stops.Load())
			close(p.release)
			require.NoError(t, drainErr.Retry(t.Context()))
			require.Error(t, <-workerDone)
			require.NoError(t, lifetime.Close(t.Context()))
			assert.EqualValues(t, 1, stops.Load())
			_, _, err := handler(t.Context(), nil, ToolInput{Message: "late"})
			require.ErrorIs(t, err, invocation.ErrClosing)
			if borrowed {
				_, err := registry.CreateSession(t.Context(), session.New(), runtime.SessionBinding{AgentName: "root"})
				require.NoError(t, err, "borrowed registry must remain open")
			}
		})
	}
}
