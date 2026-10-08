package tui

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
	"github.com/docker/docker-agent/pkg/tui/tuitest"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func hostedStartupRoot(t *testing.T, active string) *appModel {
	t.Helper()
	dir := t.TempDir()
	paths.SetDataDir(dir)
	paths.SetConfigDir(dir)
	t.Cleanup(func() { paths.SetDataDir(""); paths.SetConfigDir("") })
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{RestoreTabs: new(true), ShowBanner: new(false)}
		return nil
	}))
	owner, rt, store := hostedRootFixture(t)
	for _, id := range []string{"first", "second"} {
		saved := session.New(session.WithID(id), session.WithAgentName("root"), session.WithWorkingDir(dir))
		saved.SetAttribute(runtime.SessionAgentAttribute, "root")
		saved.AddMessage(session.UserMessage("SAVED-HOSTED-" + id))
		require.NoError(t, store.AddSession(t.Context(), saved))
	}
	services := storeRuntime{store: store}
	initial := session.New(session.WithID("blank"), session.WithAgentName("root"), session.WithWorkingDir(dir))
	application := app.New(t.Context(), rt, initial, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(services))
	ts, err := tuistate.New(t.Context())
	require.NoError(t, err)
	require.NoError(t, ts.AddTab(t.Context(), "first", dir))
	require.NoError(t, ts.AddTab(t.Context(), "second", dir))
	require.NoError(t, ts.SetActiveTab(t.Context(), active))
	require.NoError(t, ts.Close())
	spawner := func(ctx context.Context, cwd string) (SpawnedSession, error) {
		sess := session.New(session.WithAgentName("root"), session.WithWorkingDir(cwd))
		a := app.New(ctx, rt, sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(services))
		return SpawnedSession{App: a, Session: sess, Ownership: RuntimeBorrowed}, nil
	}
	root := New(t.Context(), spawner, application, dir, nil, WithSupervisor(owner)).(*appModel)
	t.Cleanup(root.cleanupManagedResources)
	return root
}

type hostedStartupProgram struct {
	*lifecycleProgramModel
	completed chan hostedLoadResult
}

func (m *hostedStartupProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	_, cmd := m.lifecycleProgramModel.Update(msg)
	if result, ok := msg.(hostedLoadResult); ok {
		m.completed <- result
	}
	return m, cmd
}

// The initial WindowSizeMsg must leave restoration alive before any navigation.
func TestHostedStartupResizeRestoresWithoutTabRoundTrip(t *testing.T) {
	for _, active := range []string{"first", "second"} {
		t.Run(active, func(t *testing.T) {
			root := hostedStartupRoot(t, active)
			completed := make(chan hostedLoadResult, 8)
			driver := tuitest.New(t, &hostedStartupProgram{&lifecycleProgramModel{root}, completed}, 120, 40, tuitest.WithTimeout(time.Second))
			select {
			case result := <-completed:
				t.Logf("initial result err=%v, target=%s, persisted=%s", result.err, result.request.target, result.request.persisted)
			case <-time.After(time.Second):
				t.Fatal("initial hosted restore did not finish")
			}
			var original, other string
			driver.Send(lifecycleProgramProbe{inspect: func(m *appModel) {
				require.Nil(t, m.hostedLoad)
				require.Equal(t, active, m.application.Session().ID)
				require.Len(t, m.pendingRestores, 1)
				require.Contains(t, m.View().Content, "SAVED-HOSTED-"+active)
				original = m.findTabByPersistedID(active)
				other = m.findTabByPersistedID(map[string]string{"first": "second", "second": "first"}[active])
			}})
			driver.Send(messages.SwitchTabMsg{SessionID: other})
			driver.WaitFor(tuitest.Contains("SAVED-HOSTED-" + map[string]string{"first": "second", "second": "first"}[active]))
			driver.Send(messages.SwitchTabMsg{SessionID: original})
			driver.WaitFor(tuitest.Contains("SAVED-HOSTED-" + active))
			driver.Send(lifecycleProgramProbe{inspect: func(m *appModel) {
				require.Empty(t, m.pendingRestores)
				require.Same(t, m.chatPages[m.paneFocus()], m.chatPage)
				require.Equal(t, active, m.application.Session().ID)
				require.Contains(t, m.View().Content, "SAVED-HOSTED-"+active)
				require.Zero(t, m.application.SessionHandle().(*hostedRootHandle).submits.Load())
				require.Zero(t, m.application.SessionHandle().(*hostedRootHandle).releases.Load())
			}})
		})
	}
}

// Request ownership is preserved deterministically, without relying on IO timing.
func TestHostedStartupResizeMustPreservePendingRestore(t *testing.T) {
	root := hostedStartupRoot(t, "first")
	_ = root.init()
	request := root.hostedLoad
	require.NotNil(t, request)
	root.handleWindowResize(120, 40)
	require.Same(t, request, root.hostedLoad, "the initial WindowSizeMsg must not retire a geometry-independent hosted restore")
	request.cancel()
}

func TestHostedStartupEscapeAndNavigationCancelPendingRestore(t *testing.T) {
	for _, action := range []string{"escape", "navigation"} {
		t.Run(action, func(t *testing.T) {
			root := hostedStartupRoot(t, "first")
			_ = root.init()
			request := root.hostedLoad
			require.NotNil(t, request)
			cancelled := false
			cancel := request.cancel
			request.cancel = func() { cancelled = true; cancel() }
			switch action {
			case "escape":
				_, handled := root.handlePaneGestureKey(tea.KeyPressMsg{Code: tea.KeyEscape})
				require.True(t, handled)
			case "navigation":
				root.handleSwitchTab(root.findTabByPersistedID("second"))
			}
			require.True(t, cancelled, "non-geometry cancellation keeps retiring hosted loads")
			require.NotSame(t, request, root.hostedLoad)
			if root.hostedLoad != nil {
				root.hostedLoad.cancel()
			}
		})
	}
}
