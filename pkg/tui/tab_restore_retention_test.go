package tui

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func retentionStore(t *testing.T) (*tuistate.Store, []tuistate.TabEntry, tuistate.PaneWorkspaceState) {
	t.Helper()
	paths.SetDataDir(t.TempDir())
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetDataDir(""); paths.SetConfigDir("") })
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{RestoreTabs: new(true), ShowBanner: new(false)}
		return nil
	}))
	store, err := tuistate.New(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	for _, id := range []string{"first", "current", "last"} {
		require.NoError(t, store.AddTab(t.Context(), id, "/saved/"+id))
	}
	require.NoError(t, store.ToggleSidebarCollapsed(t.Context(), "first"))
	require.NoError(t, store.ToggleSidebarCollapsed(t.Context(), "last"))
	require.NoError(t, store.SetActiveTab(t.Context(), "last"))
	state := tuistate.PaneWorkspaceState{Version: 1, ActiveAnchorSessionID: "first", Workspaces: []tuistate.PaneWorkspace{{
		AnchorSessionID: "first", FocusSessionID: "last", HasSidebar: true, SidebarCollapsed: true, SidebarPreferredWidth: 37,
		Layout:        &tuistate.PaneLayoutNode{SessionID: "last"},
		RestoreLayout: &tuistate.PaneLayoutNode{Ratio: 0.63, First: &tuistate.PaneLayoutNode{SessionID: "first"}, Second: &tuistate.PaneLayoutNode{Ratio: 0.41, Axis: 1, First: &tuistate.PaneLayoutNode{SessionID: "current"}, Second: &tuistate.PaneLayoutNode{SessionID: "last"}}},
	}}}
	require.NoError(t, store.SetPaneWorkspaceState(t.Context(), state))
	tabs, _, err := store.GetTabs(t.Context())
	require.NoError(t, err)
	return store, tabs, state
}

func retentionApp(ctx context.Context, id string, store session.Store) *app.App {
	sess := session.New(session.WithID(id), session.WithAgentName("root"), session.WithWorkingDir("/saved/"+id))
	return app.New(ctx, nil, sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(storeRuntime{store: store}))
}

func assertRetained(t *testing.T, store *tuistate.Store, tabs []tuistate.TabEntry, state tuistate.PaneWorkspaceState) {
	t.Helper()
	got, active, err := store.GetTabs(t.Context())
	require.NoError(t, err)
	require.Equal(t, tabs, got)
	require.Equal(t, "last", active)
	panes, err := store.GetPaneWorkspaceState(t.Context())
	require.NoError(t, err)
	require.Equal(t, state, panes)
}

func TestTabRestoreRetainsUnavailableAcrossRepeatedStartupAndRetry(t *testing.T) {
	for _, mode := range []string{"remote-failure", "missing-local", "unavailable-local", "spawner-failure", "no-spawner"} {
		t.Run(mode, func(t *testing.T) {
			ts, tabs, panes := retentionStore(t)
			for attempt := range 2 {
				initialID := "blank"
				var store session.Store
				var spawn SessionSpawner
				var opts []Option
				if mode == "remote-failure" {
					opts = append(opts, WithSessionRestorer(func(context.Context, string, string) (SpawnedSession, error) {
						return SpawnedSession{}, errors.New("offline")
					}))
				} else if mode != "unavailable-local" {
					store = session.NewInMemorySessionStore()
					if mode == "spawner-failure" || mode == "no-spawner" {
						initialID = "current"
						for _, id := range []string{"first", "last"} {
							require.NoError(t, store.AddSession(t.Context(), retentionApp(t.Context(), id, store).Session()))
						}
					}
				}
				if mode == "spawner-failure" {
					spawn = func(context.Context, string) (SpawnedSession, error) {
						return SpawnedSession{}, errors.New("directory unavailable")
					}
				}
				root := New(t.Context(), spawn, retentionApp(t.Context(), initialID, store), "/initial", nil, opts...).(*appModel)
				_ = root.init()
				root.persistPaneWorkspaceState()
				require.Equal(t, 1, root.supervisor.Count(), "attempt %d must not accumulate blank tabs", attempt)
				require.Empty(t, root.pendingRestores)
				if initialID == "current" {
					require.Equal(t, "current", root.persistedSessionID("current"))
				}
				assertRetained(t, ts, tabs, panes)
				root.Shutdown()
			}
			retry := New(t.Context(), nil, retentionApp(t.Context(), "current", nil), "/saved/current", nil,
				WithSessionRestorer(func(ctx context.Context, id, _ string) (SpawnedSession, error) {
					a := retentionApp(ctx, id, nil)
					return SpawnedSession{App: a, Session: a.Session(), Ownership: RuntimeBorrowed}, nil
				}),
			).(*appModel)
			t.Cleanup(retry.cleanupManagedResources)
			_ = retry.init()
			require.Equal(t, 3, retry.supervisor.Count())
			require.Equal(t, "last", retry.application.Session().ID)
			require.Equal(t, []string{"first", "current", "last"}, retry.paneWorkspaces.active.restore.Sessions())
			retry.persistPaneWorkspaceState()
			assertRetained(t, ts, tabs, panes)
		})
	}
}

func TestTabRestoreRetainsCurrentAlongsideInaccessibleTabs(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "remote"}[remote], func(t *testing.T) {
			ts, tabs, panes := retentionStore(t)
			store := session.NewInMemorySessionStore()
			var opts []Option
			if remote {
				opts = append(opts, WithSessionRestorer(func(context.Context, string, string) (SpawnedSession, error) {
					return SpawnedSession{}, errors.New("offline")
				}))
			}
			root := New(t.Context(), nil, retentionApp(t.Context(), "current", store), "/saved/current", nil, opts...).(*appModel)
			t.Cleanup(root.cleanupManagedResources)
			_ = root.init()
			root.persistPaneWorkspaceState()
			require.Equal(t, "current", root.application.Session().ID)
			require.Equal(t, "current", root.persistedSessionID("current"))
			require.Empty(t, root.pendingRestores)
			assertRetained(t, ts, tabs, panes)
			// Explicit navigation is allowed to replace the saved active selection.
			root.handleSwitchTab("current")
			_, active, err := ts.GetTabs(t.Context())
			require.NoError(t, err)
			require.Equal(t, "current", active)
		})
	}
}

func TestTabRestoreUsedFallbackAndExplicitChoice(t *testing.T) {
	for _, choice := range []string{"send", "session-choice", "existing-conversation"} {
		t.Run(choice, func(t *testing.T) {
			ts, tabs, panes := retentionStore(t)
			initial := retentionApp(t.Context(), "fallback", nil)
			if choice == "existing-conversation" {
				initial.Session().AddMessage(session.UserMessage("keep this conversation"))
			}
			root := New(t.Context(), nil, initial, "/fallback", nil, WithSessionRestorer(func(context.Context, string, string) (SpawnedSession, error) {
				return SpawnedSession{}, errors.New("offline")
			})).(*appModel)
			t.Cleanup(root.cleanupManagedResources)
			_ = root.init()
			if choice != "existing-conversation" {
				assertRetained(t, ts, tabs, panes)
			}
			expected := "fallback"
			switch choice {
			case "send":
				root.update(messages.SendMsg{Content: "keep my fallback"})
			case "session-choice":
				cmd := root.beginHostedLoad("chosen", root.paneFocus(), nil)
				require.NotNil(t, cmd)
				request := root.hostedLoad
				chosen := retentionApp(t.Context(), "chosen", nil)
				root.finishHostedLoad(hostedLoadResult{request: request, data: &hostedSessionData{application: chosen}})
				expected = "chosen"
			}
			got, active, err := ts.GetTabs(t.Context())
			require.NoError(t, err)
			require.Len(t, got, 4)
			require.Equal(t, tabs, got[:3])
			require.Equal(t, expected, got[3].SessionID)
			if choice != "existing-conversation" {
				require.Equal(t, expected, active)
			}
			root.persistPaneWorkspaceState()
			state, err := ts.GetPaneWorkspaceState(t.Context())
			require.NoError(t, err)
			require.Equal(t, panes.Workspaces[0], state.Workspaces[0])
		})
	}
}

func TestTabRestoreExplicitCloseAndDisabledStart(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit-close", true: "restore-disabled"}[disabled], func(t *testing.T) {
			ts, _, _ := retentionStore(t)
			if disabled {
				require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error { cfg.Settings.RestoreTabs = new(false); return nil }))
			}
			root := New(t.Context(), nil, retentionApp(t.Context(), "current", nil), "/saved/current", nil, WithSupervisor(supervisor.New(nil))).(*appModel)
			t.Cleanup(root.cleanupManagedResources)
			_ = root.init()
			if !disabled {
				root.closeTabView("current", false)
			}
			tabs, _, err := ts.GetTabs(t.Context())
			require.NoError(t, err)
			if disabled {
				require.Equal(t, []tuistate.TabEntry{{SessionID: "current", WorkingDir: "/saved/current"}}, tabs)
			} else {
				require.Equal(t, []tuistate.TabEntry{{SessionID: "first", WorkingDir: "/saved/first", SidebarCollapsed: true}, {SessionID: "last", WorkingDir: "/saved/last", SidebarCollapsed: true}}, tabs)
				state, err := ts.GetPaneWorkspaceState(t.Context())
				require.NoError(t, err)
				require.False(t, paneLayoutContains(state.Workspaces[0].RestoreLayout, "current"))
			}
		})
	}
}

func TestTabRestoreLazyCompletionPreservesInaccessibleSelectionAndPanes(t *testing.T) {
	for _, outcome := range []string{"loaded", "failed", "used-placeholder", "explicit-session-choice"} {
		t.Run(outcome, func(t *testing.T) {
			ts, tabs, panes := retentionStore(t)
			store := session.NewInMemorySessionStore()
			first := retentionApp(t.Context(), "first", store)
			require.NoError(t, store.AddSession(t.Context(), first.Session()))
			root := New(t.Context(), nil, retentionApp(t.Context(), "blank", store), "/initial", nil).(*appModel)
			t.Cleanup(root.cleanupManagedResources)
			require.Equal(t, "first", root.pendingRestores["blank"])
			_ = root.init()
			request := root.hostedLoad
			require.NotNil(t, request)
			switch outcome {
			case "loaded":
				root.finishHostedLoad(hostedLoadResult{request: request, data: &hostedSessionData{application: first}})
				require.Empty(t, root.pendingRestores)
				require.Equal(t, "first", root.application.Session().ID)
			case "failed", "used-placeholder", "explicit-session-choice":
				root.finishHostedLoad(hostedLoadResult{request: request, err: errors.New("temporarily unavailable")})
				require.Equal(t, "first", root.pendingRestores["blank"])
			}
			root.persistPaneWorkspaceState()
			assertRetained(t, ts, tabs, panes)
			if outcome == "used-placeholder" {
				root.update(messages.SendMsg{Content: "use the fallback after lazy failure"})
				got, active, err := ts.GetTabs(t.Context())
				require.NoError(t, err)
				require.Len(t, got, 4)
				require.Equal(t, tabs, got[:3])
				require.Equal(t, "blank", got[3].SessionID)
				require.Equal(t, "blank", active)
				state, err := ts.GetPaneWorkspaceState(t.Context())
				require.NoError(t, err)
				require.Equal(t, panes.Workspaces[0], state.Workspaces[0])
			}
			if outcome == "explicit-session-choice" {
				_ = root.beginHostedLoad("chosen", "blank", nil)
				chosen := retentionApp(t.Context(), "chosen", nil)
				root.finishHostedLoad(hostedLoadResult{request: root.hostedLoad, data: &hostedSessionData{application: chosen}})
				got, active, err := ts.GetTabs(t.Context())
				require.NoError(t, err)
				require.Len(t, got, 3)
				require.Equal(t, "chosen", got[0].SessionID)
				require.Equal(t, tabs[1:], got[1:])
				require.Equal(t, "chosen", active)
			}
		})
	}
}

func TestTabRestoreReorderKeepsInaccessibleSlots(t *testing.T) {
	ts, _, _ := retentionStore(t)
	root := New(t.Context(), nil, retentionApp(t.Context(), "current", nil), "/saved/current", nil,
		WithSessionRestorer(func(ctx context.Context, id, _ string) (SpawnedSession, error) {
			if id == "last" {
				return SpawnedSession{}, errors.New("offline")
			}
			a := retentionApp(ctx, id, nil)
			return SpawnedSession{App: a, Session: a.Session(), Ownership: RuntimeBorrowed}, nil
		}),
	).(*appModel)
	t.Cleanup(root.cleanupManagedResources)
	require.NoError(t, root.persistTabOrder([]string{"current", "unused-blank", "first"}))
	got, active, err := ts.GetTabs(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"current", "first", "last"}, []string{got[0].SessionID, got[1].SessionID, got[2].SessionID})
	require.Equal(t, "last", active)
}

func TestTabRestoreFirstStartPersistsInitialWorkspace(t *testing.T) {
	ts, _, _ := retentionStore(t)
	require.NoError(t, ts.ClearTabs(t.Context()))
	root := New(t.Context(), nil, retentionApp(t.Context(), "initial", nil), "/initial", nil).(*appModel)
	t.Cleanup(root.cleanupManagedResources)
	_ = root.init()
	got, active, err := ts.GetTabs(t.Context())
	require.NoError(t, err)
	require.Equal(t, []tuistate.TabEntry{{SessionID: "initial", WorkingDir: "/initial"}}, got)
	require.Equal(t, "initial", active)
	state, err := ts.GetPaneWorkspaceState(t.Context())
	require.NoError(t, err)
	require.Equal(t, "initial", state.ActiveAnchorSessionID)
	require.Len(t, state.Workspaces, 1)
	require.Equal(t, &tuistate.PaneLayoutNode{SessionID: "initial"}, state.Workspaces[0].Layout)
}
