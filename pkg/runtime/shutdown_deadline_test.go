package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestRuntimeShutdownConcurrentDeadlinesRetainOwnedStartupWork(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	toolset := &coordinatedStartToolSet{entered: entered, release: release}
	root := agent.New("root", "prompt", agent.WithModel(&mockProvider{id: "test/shutdown"}), agent.WithToolSets(toolset))
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithToolStartTimeout(time.Hour))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	t.Cleanup(func() { close(release) })
	startupDone := make(chan struct{})
	go func() { r.EmitStartupInfo(t.Context(), nil, EventSinkFunc(func(Event) {})); close(startupDone) }()
	receiveWithin(t, entered)
	// Model an owned startup publisher that cannot complete yet. Shutdown may
	// cancel discovery, but cannot pretend this runtime work has drained.
	r.startupToolsWG.Add(1)
	defer r.startupToolsWG.Done()
	drain := r.startupToolsWG.DoneChan()
	firstCtx, cancelFirst := context.WithCancel(t.Context())
	defer cancelFirst()
	firstDone := make(chan error, 1)
	go func() { firstDone <- r.shutdownSessions(firstCtx) }()
	require.Eventually(t, func() bool { return r.lifetime().Err() != nil }, time.Second, time.Millisecond)
	for range 8 {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		result := make(chan error, 1)
		go func() { result <- r.shutdownSessions(ctx) }()
		select {
		case err := <-result:
			require.ErrorIs(t, err, context.DeadlineExceeded)
		case <-time.After(time.Second):
			t.Fatal("shutdown caller waited behind another caller's drain")
		}
		cancel()
		require.Equal(t, drain, r.startupToolsWG.DoneChan())
	}
	_, err = r.CreateSession(t.Context(), session.New(session.WithID(t.Name())), SessionBinding{})
	require.ErrorIs(t, err, ErrSessionClosed)
	cancelFirst()
	require.ErrorIs(t, receiveWithin(t, firstDone), context.Canceled)
	receiveWithin(t, startupDone)
}

func TestStartupAndSubagentDrainReuseOwnedCompletionSignal(t *testing.T) {
	r := serviceRuntime(t, NewSessionService(), session.NewInMemorySessionStore())
	r.startupToolsWG.Add(1)
	r.subagents.wg.Add(1)
	defer r.startupToolsWG.Done()
	defer r.subagents.wg.Done()
	startupDone, childDone := r.startupToolsWG.DoneChan(), r.subagents.wg.DoneChan()
	for range 8 {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		require.ErrorIs(t, r.shutdownStartupTools(ctx), context.Canceled)
		require.ErrorIs(t, r.subagents.CloseContext(ctx), context.Canceled)
		require.Equal(t, startupDone, r.startupToolsWG.DoneChan())
		require.Equal(t, childDone, r.subagents.wg.DoneChan())
	}
}
