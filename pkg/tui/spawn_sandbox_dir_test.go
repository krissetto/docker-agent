package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

func TestSandboxNewTabRoutesThroughWorkingDirectoryPicker(t *testing.T) {
	for _, trigger := range []string{"keyboard", "plus", "new"} {
		t.Run(trigger, func(t *testing.T) {
			t.Setenv("SANDBOX_VM_ID", "test-sandbox")
			workspace := t.TempDir()
			child := filepath.Join(workspace, "child")
			require.NoError(t, os.Mkdir(child, 0o755))
			cwd, err := os.Getwd()
			require.NoError(t, err)

			m, _, _ := wallClockRoot(t, 120, 40)
			spy := &spySpawner{}
			m.supervisor = supervisor.New(spy.spawn)
			m.application.Session().WorkingDir = workspace
			initialID, err := m.supervisor.AddSession(t.Context(), m.application, m.application.Session(), workspace, nil)
			require.NoError(t, err)
			WithDefaultWorkingDir(workspace)(m)
			m.editor.SetValue("keep this draft")

			var cmd tea.Cmd
			switch trigger {
			case "keyboard":
				_, cmd = m.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
			case "plus":
				tabs := ansi.Strip(m.tabBar.View())
				index := strings.LastIndex(tabs, "+")
				require.GreaterOrEqual(t, index, 0)
				x := ansi.StringWidth(tabs[:index]) + tabFrameOrigin()
				_, cmd = m.Update(tea.MouseClickMsg{X: x, Y: m.contentHeight + 1, Button: tea.MouseLeft})
			case "new":
				_, cmd = m.Update(messages.NewSessionMsg{})
			}
			if trigger != "new" {
				spawn, ok := firstOfType[messages.SpawnSessionMsg](collectMsgs(cmd))
				require.True(t, ok, "keyboard and mouse share the spawn route")
				require.Empty(t, spawn.WorkingDir)
				_, cmd = m.Update(spawn)
			}
			open, ok := firstOfType[dialog.OpenDialogMsg](collectMsgs(cmd))
			require.True(t, ok, "sandbox launcher's explicit directory must not bypass picker")
			require.Empty(t, spy.dirs)
			require.Equal(t, 1, m.supervisor.Count())
			open.Model.SetSize(120, 40)

			// The first row selects the active workspace, not the process cwd.
			_, cmd = open.Model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			selected, ok := firstOfType[messages.SpawnSessionMsg](collectMsgs(cmd))
			require.True(t, ok)
			require.Equal(t, workspace, selected.WorkingDir)

			// Browse past "use this directory" and "..", then select the child.
			_, _ = open.Model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
			_, _ = open.Model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
			_, cmd = open.Model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			require.Nil(t, cmd, "entering a directory does not spawn")
			_, cmd = open.Model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			selected, ok = firstOfType[messages.SpawnSessionMsg](collectMsgs(cmd))
			require.True(t, ok)
			require.Equal(t, child, selected.WorkingDir)
			_, _ = m.Update(selected)
			require.Equal(t, []string{child}, spy.dirs)
			require.Equal(t, child, m.supervisor.ActiveRunner().WorkingDir)
			require.Equal(t, child, m.application.Session().WorkingDir)
			require.Equal(t, 2, m.supervisor.Count())
			require.Equal(t, "keep this draft", m.editors[initialID].Value())

			// Subsequent tabs start browsing from the active child, not the launch default.
			_, cmd = m.Update(messages.SpawnSessionMsg{})
			open, ok = firstOfType[dialog.OpenDialogMsg](collectMsgs(cmd))
			require.True(t, ok)
			_, cmd = open.Model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			selected, ok = firstOfType[messages.SpawnSessionMsg](collectMsgs(cmd))
			require.True(t, ok)
			require.Equal(t, child, selected.WorkingDir)
			after, err := os.Getwd()
			require.NoError(t, err)
			require.Equal(t, cwd, after, "new tabs never chdir the process")
		})
	}
}

func TestSandboxWorkingDirectoryPickerCancelAndParent(t *testing.T) {
	t.Setenv("SANDBOX_VM_ID", "test-sandbox")
	workspace := t.TempDir()
	spy := &spySpawner{}
	m := newSpawnTestModel(t, spy, WithDefaultWorkingDir(workspace))
	m.supervisor.ActiveRunner().WorkingDir = workspace

	_, cmd := m.Update(messages.SpawnSessionMsg{})
	open, ok := firstOfType[dialog.OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok)
	open.Model.SetSize(120, 40)
	_, cmd = open.Model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	msgs := collectMsgs(cmd)
	require.True(t, hasMsg[dialog.CloseDialogMsg](msgs))
	require.False(t, hasMsg[messages.SpawnSessionMsg](msgs))
	require.Empty(t, spy.dirs)
	require.Equal(t, 1, m.supervisor.Count())

	// Sandbox mounts define visibility; the picker must not add a workspace fence.
	_, _ = open.Model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	_, cmd = open.Model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd)
	_, cmd = open.Model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	selected, ok := firstOfType[messages.SpawnSessionMsg](collectMsgs(cmd))
	require.True(t, ok)
	require.Equal(t, filepath.Dir(workspace), selected.WorkingDir)
}

func TestSandboxNewSessionInvalidDirectoryDoesNotSpawn(t *testing.T) {
	t.Setenv("SANDBOX_VM_ID", "test-sandbox")
	workspace := t.TempDir()
	file := filepath.Join(workspace, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	for _, dir := range []string{file, filepath.Join(workspace, "missing")} {
		spy := &spySpawner{}
		m := newSpawnTestModel(t, spy, WithDefaultWorkingDir(workspace))
		_, cmd := m.Update(messages.NewSessionMsg{WorkingDir: dir})
		note, ok := firstOfType[notification.ShowMsg](collectMsgs(cmd))
		require.True(t, ok)
		require.Equal(t, notification.TypeError, note.Type)
		require.Empty(t, spy.dirs)
		require.Equal(t, 1, m.supervisor.Count())
	}
}

func TestHostExplicitWorkingDirectoryStillBypassesPicker(t *testing.T) {
	t.Setenv("SANDBOX_VM_ID", "")
	workspace := t.TempDir()
	spy := &spySpawner{}
	m := newSpawnTestModel(t, spy, WithDefaultWorkingDir(workspace))
	_, _ = m.Update(messages.SpawnSessionMsg{})
	require.Equal(t, []string{workspace}, spy.dirs)
	require.Equal(t, 2, m.supervisor.Count())
}
