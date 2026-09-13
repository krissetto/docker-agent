package tui

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestWorkingDirectoryURLPreservesLiteralPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "space ; $(no-shell) #?.dir")
	encoded, err := workingDirectoryURL(path)
	require.NoError(t, err)
	parsed, err := url.Parse(encoded)
	require.NoError(t, err)
	require.Equal(t, "file", parsed.Scheme)
	require.Empty(t, parsed.RawQuery)
	require.Empty(t, parsed.Fragment)
	require.True(t, strings.HasSuffix(parsed.Path, "/space ; $(no-shell) #?.dir"))
	_, err = workingDirectoryURL("relative/path")
	require.Error(t, err)
}

func TestRootOpenWorkingDirectoryRunsInjectedOpenerInCommand(t *testing.T) {
	root, _ := newTestModel(t)
	path := filepath.Join(t.TempDir(), "spaces ; $literal")
	calls := 0
	root.openWorkingDirectory = func(_ context.Context, got string) error { calls++; require.Equal(t, path, got); return nil }
	_, cmd := root.Update(messages.OpenWorkingDirMsg{Path: path})
	require.Zero(t, calls, "platform invocation never blocks Update")
	require.Nil(t, cmd())
	require.Equal(t, 1, calls)
	root.openWorkingDirectory = func(context.Context, string) error { return errors.New("launch denied") }
	_, cmd = root.Update(messages.OpenWorkingDirMsg{Path: path})
	note, ok := cmd().(notification.ShowMsg)
	require.True(t, ok)
	require.Equal(t, notification.TypeError, note.Type)
	require.Contains(t, note.Text, "launch denied")
}

func TestRootFolderIconRoutesExactPathWithoutClipboardOrComposer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local folder ; $literal")
	sess := session.New(session.WithID("folder-click"), session.WithWorkingDir(path))
	application := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	root := newSidebarProgramRoot(t, application)
	root.editor.SetValue("KEEP-DRAFT")
	var calls atomic.Int32
	root.openWorkingDirectory = func(_ context.Context, got string) error {
		if got != path {
			return errors.New("unexpected directory")
		}
		calls.Add(1)
		return nil
	}
	program := startTestProgram(t, root, &shellProgramModel{root: root}, tea.WithOutput(&cacheProgramWriter{}))
	initial := sidebarProgramSnapshot(t, program)
	x, y := sidebarProgramPoint(t, initial.content, "local folder")
	program.Send(tea.MouseMotionMsg{X: x, Y: y})
	require.Eventually(t, func() bool {
		s := sidebarProgramSnapshot(t, program)
		return s.active == 0 && strings.Contains(s.content, "↗")
	}, time.Second, time.Millisecond)
	hovered := sidebarProgramSnapshot(t, program)
	x, y = sidebarProgramPoint(t, hovered.content, "↗")
	program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, time.Millisecond)
	require.Equal(t, "KEEP-DRAFT", sidebarProgramSnapshot(t, program).editor)
}
