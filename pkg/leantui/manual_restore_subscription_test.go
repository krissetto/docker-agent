package leantui

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/stretchr/testify/require"
)

func TestManualRestoreReconnectsViewerSubscription(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	store := session.NewInMemorySessionStore()
	local, err := runtime.NewLocalRuntime(ctx, team.New(team.WithAgents(agent.New("root", "fixture", agent.WithModel(&viewerLifecycleProvider{})))), runtime.WithSessionStore(store), runtime.WithWorkingDir(dir))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(local)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(ctx))) })
	sessions := owner.Runtime()
	services := &viewerLifecycleLocalServices{local: local}
	initial := session.New(session.WithAgentName("root"), session.WithWorkingDir(dir))
	original := app.New(ctx, sessions, initial, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(services))
	past := session.New(session.WithID(t.Name()), session.WithAgentName("root"), session.WithWorkingDir(dir), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root"}))
	require.NoError(t, store.AddSession(ctx, past))
	m := bareModel(80)
	m.app = original
	m.sessionState = service.NewSessionState(original.Session())
	m.viewers = &viewerHost{ctx: func() context.Context { return ctx }, events: make(chan any, 256), views: make(map[*app.App]*model)}
	t.Cleanup(m.viewers.close)
	m.subscribeViewer(ctx, original)
	m.resumeSession(ctx, past.ID)
	require.NotSame(t, original, m.app)
	require.Equal(t, past.ID, m.app.Session().ID)
	require.True(t, m.viewers.subscribed[m.app])
	require.Eventually(t, func() bool { return m.app.Presentation() != nil }, 2*time.Second, time.Millisecond)
	require.NoError(t, m.app.SessionHandle().UpdateTitle(ctx, "restored viewer event"))
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-m.viewers.events:
			routed, ok := event.(viewerEvent)
			if !ok || routed.origin != m.app {
				continue
			}
			msg, ok := routed.event.(app.SessionEventMsg)
			if !ok {
				continue
			}
			title, ok := msg.Event.(*runtime.SessionTitleEvent)
			if ok && title.Title == "restored viewer event" {
				return
			}
		case <-deadline:
			t.Fatal("restored App did not deliver its canonical event to the viewer subscription")
		}
	}
}
