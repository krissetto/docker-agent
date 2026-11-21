package tui

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestTestMainIsolatesPublicNewFromCallerState(t *testing.T) {
	if os.Getenv("DOCKER_AGENT_TUI_ISOLATION_CHILD") == "1" {
		testPublicNewIsolatedState(t)
		return
	}

	caller := t.TempDir()
	paths.SetDataDir(filepath.Join(caller, ".cagent"))
	paths.SetConfigDir(filepath.Join(caller, ".config", "cagent"))
	t.Cleanup(func() { paths.SetRoot("") })
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{Layout: &userconfig.LayoutSettings{SidebarPosition: "left"}, RestoreTabs: new(true)}
		return nil
	}))
	store, err := tuistate.New(t.Context())
	require.NoError(t, err)
	require.NoError(t, store.AddTab(t.Context(), "caller-tab", caller))
	require.NoError(t, store.SetActiveTab(t.Context(), "caller-tab"))
	require.NoError(t, store.SetPaneWorkspaceState(t.Context(), tuistate.PaneWorkspaceState{
		Version: 1, ActiveAnchorSessionID: "caller-tab",
		Workspaces: []tuistate.PaneWorkspace{{AnchorSessionID: "caller-tab", FocusSessionID: "caller-tab", Layout: &tuistate.PaneLayoutNode{SessionID: "caller-tab"}}},
	}))
	require.NoError(t, store.Close())
	require.NoError(t, os.WriteFile(filepath.Join(caller, ".cagent", "history"), []byte("caller history\n"), 0o600))
	before := isolationFiles(t, caller)

	executable, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestTestMainIsolatesPublicNewFromCallerState$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"DOCKER_AGENT_TUI_ISOLATION_CHILD=1",
		"HOME="+caller, "USERPROFILE="+caller,
		"XDG_CONFIG_HOME="+caller, "XDG_DATA_HOME="+caller,
		"XDG_STATE_HOME="+caller, "XDG_CACHE_HOME="+caller, "XDG_RUNTIME_DIR="+caller,
		"APPDATA="+caller, "LOCALAPPDATA="+caller,
		"DOCKER_CONFIG="+caller, "DOCKER_CERT_PATH="+caller,
		"DOCKER_AGENT_CONFIG_DIR="+paths.GetConfigDir(), "CAGENT_CONFIG_DIR="+paths.GetConfigDir(),
	)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	require.Equal(t, before, isolationFiles(t, caller), "direct New must not read or mutate caller settings, layout, history or tab store")
}

func testPublicNewIsolatedState(t *testing.T) {
	t.Helper()
	defaultData, defaultConfig := paths.GetDataDir(), paths.GetConfigDir()
	require.Empty(t, userconfig.Get().GetLayout().SidebarPosition)

	// Explicit per-test fixtures still win, and clearing them returns to safety.
	fixture := t.TempDir()
	paths.SetDataDir(fixture)
	paths.SetConfigDir(fixture)
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{Layout: &userconfig.LayoutSettings{SidebarPosition: "bottom"}}
		return nil
	}))
	checkPublicNewState(t, messages.SidebarBottom)
	require.FileExists(t, filepath.Join(fixture, "tui_state.db"))
	paths.SetRoot("")
	require.Equal(t, defaultData, paths.GetDataDir())
	require.Equal(t, defaultConfig, paths.GetConfigDir())
	require.Empty(t, userconfig.Get().GetLayout().SidebarPosition)
	checkPublicNewState(t, messages.SidebarRight)
	require.FileExists(t, filepath.Join(defaultData, "tui_state.db"))
	require.NoError(t, saveTestSettings(messages.LayoutSettings{SidebarPosition: messages.SidebarTop}, messages.SendModeQueue))
	require.Equal(t, "top", userconfig.Get().GetLayout().SidebarPosition)
}

func checkPublicNewState(t *testing.T, sidebarPosition messages.SidebarPosition) {
	t.Helper()
	sess := session.New(session.WithAgentName("root"), session.WithWorkingDir(t.TempDir()))
	application := app.New(t.Context(), nil, sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(stubRuntime{}))
	root := New(t.Context(), nil, application, sess.WorkingDir, nil, WithHideSidebar()).(*appModel)
	defer root.cleanupManagedResources()
	require.Equal(t, sidebarPosition, root.layoutSettings.SidebarPosition)
	require.NotNil(t, root.tuiStore)
	tabs, active, err := root.tuiStore.GetTabs(t.Context())
	require.NoError(t, err)
	require.Len(t, tabs, 1)
	require.Equal(t, sess.ID, tabs[0].SessionID)
	require.Equal(t, sess.ID, active)
	state, err := root.tuiStore.GetPaneWorkspaceState(t.Context())
	require.NoError(t, err)
	require.Equal(t, sess.ID, state.ActiveAnchorSessionID)
	require.NoError(t, root.history.Add("isolated test history"))
}

func isolationFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[relative] = string(data)
		return nil
	}))
	return files
}
