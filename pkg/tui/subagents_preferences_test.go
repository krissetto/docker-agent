package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func preferenceApp(t *testing.T) (*app.App, *runtime.LocalRuntime) {
	t.Helper()
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "fixture", agent.WithModel(tuiModelProvider{})))))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	return app.New(t.Context(), nil, session.New(), runtime.SessionBinding{}, app.WithRuntimeServices(rt)), rt
}

func TestSubagentsPreferenceSaveApplyFailureAndNewRuntime(t *testing.T) {
	setupSettingsConfigTest(t)
	t.Setenv("HOME", t.TempDir())
	a, rt := preferenceApp(t)
	d := dialog.NewSubagentsDialog(nil, nil)
	d.SetSize(100, 25)
	m := &appModel{application: a, panelData: map[string]*panelSessionData{"root": {treeDialog: d}}}
	require.True(t, subagentsPreference(a))
	notice, ok := firstOfType[notification.ShowMsg](collectMsgs(m.setUseSubagents(false)))
	require.True(t, ok)
	require.Equal(t, notification.TypeSuccess, notice.Type)
	require.False(t, rt.UseSubagents())
	require.False(t, userconfig.Get().GetUseSubagents())
	require.Contains(t, ansi.Strip(d.View()), "Use subagents (local default): OFF")
	next, nextRuntime := preferenceApp(t)
	applySavedSubagentsPreference(next)
	require.False(t, nextRuntime.UseSubagents(), "new/restored apps reconcile saved false")
	// A concurrent settings draft preserves the dedicated policy field.
	require.NoError(t, saveChangedPreferences(messages.Preferences{}, messages.Preferences{ShowBanner: true}))
	require.False(t, userconfig.Get().GetUseSubagents())
	blocked := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocked, []byte("not a directory"), 0o600))
	paths.SetConfigDir(filepath.Join(blocked, "config"))
	notice = m.setUseSubagents(true)().(notification.ShowMsg)
	require.Equal(t, notification.TypeError, notice.Type)
	require.False(t, rt.UseSubagents())
	require.Contains(t, ansi.Strip(d.View()), "Use subagents (local default): OFF")
}

func TestSubagentsPreferenceUnsupportedDoesNotSave(t *testing.T) {
	setupSettingsConfigTest(t)
	t.Setenv("HOME", t.TempDir())
	m := &appModel{application: app.New(t.Context(), nil, session.New(), runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))}
	notice, ok := firstOfType[notification.ShowMsg](collectMsgs(m.setUseSubagents(false)))
	require.True(t, ok)
	require.Equal(t, notification.TypeError, notice.Type)
	require.Contains(t, notice.Text, "preference not saved")
	require.True(t, userconfig.Get().GetUseSubagents())
}

func TestSubagentsPreferenceRefreshesVisibleInspectorWithoutClearingTree(t *testing.T) {
	setupSettingsConfigTest(t)
	root, _, _ := harnessRoot(t, 120, 40, animation.NewRuntimeWithScheduler(&rootImmediateScheduler{now: time.Unix(1, 0)}))
	root.dialogMgr = dialog.New(root.ar)
	root.dialogMgr.SetSize(120, 40)
	a, rt := preferenceApp(t)
	a.Session().ID = "profile"
	a.Session().SetSubagentTree(&panelTree("profile", "idle").Snapshot)
	root.application = a
	root.supervisor.GetRunner("profile").App = a
	opened, ok := firstOfType[dialog.OpenDialogMsg](collectMsgs(root.showSubagentSessions()))
	require.True(t, ok)
	root.updateDialogCmd(opened)
	for range 100 {
		if cmd := root.ar.Continue(); cmd != nil {
			for _, msg := range collectMsgs(cmd) {
				if tick, ok := msg.(animation.TickMsg); ok {
					if accepted, ok := root.ar.Accept(tick); ok {
						root.dialogMgr.Update(accepted)
					}
				}
			}
		}
	}
	require.Contains(t, ansi.Strip(root.dialogMgr.View()), "Use subagents (local default): ON")
	// Prime the manager's intrinsic view cache before changing policy.
	_ = root.dialogMgr.View()
	root.Update(messages.SetUseSubagentsMsg{Enabled: false})
	require.False(t, rt.UseSubagents())
	require.Contains(t, ansi.Strip(root.dialogMgr.View()), "Use subagents (local default): OFF")
	require.Contains(t, ansi.Strip(opened.Model.View()), "worker")
}
