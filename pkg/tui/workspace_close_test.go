package tui

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func workspaceClosePoint(t *testing.T, root *appModel, index int) (int, int) {
	t.Helper()
	plain := ansi.Strip(root.tabBar.View())
	start := 0
	for i := 0; i < index; i++ {
		at := strings.Index(plain[start:], "×")
		require.NotEqual(t, -1, at)
		start += at + len("×")
	}
	at := strings.Index(plain[start:], "×")
	require.NotEqual(t, -1, at)
	return tabFrameOrigin() + ansi.StringWidth(plain[:start+at]), root.composerLayout().tabsTop
}

func workspaceCloseAnswer(t *testing.T, root *appModel, key tea.KeyPressMsg) []tea.Msg {
	t.Helper()
	require.True(t, root.dialogMgr.Open())
	_, cmd := root.dialogMgr.TopDialog().Update(key)
	return collectMsgs(cmd)
}

func TestWorkspaceCloseMouseCancelAlwaysAsksWithoutChangingPanes(t *testing.T) {
	for _, choice := range []messages.CloseTabChoice{messages.CloseTabAsk, messages.CloseTabYes, messages.CloseTabNo} {
		for _, key := range []tea.KeyPressMsg{{Code: tea.KeyEnter}, {Code: tea.KeyEscape}, {Code: 'n', Text: "n"}} {
			root := splitTestRoot(t)
			root.closeTabChoice = choice
			root.splitPane("second", "profile", splitRight)
			root.editor.SetValue("keep exact draft")
			before, owner := root.panes, root.paneWorkspaces.active
			x, y := workspaceClosePoint(t, root, 0)
			_, cmd := root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			intent, ok := firstOfType[messages.CloseWorkspaceMsg](collectMsgs(cmd))
			require.True(t, ok, "group x requests workspace close")
			root.Update(intent)
			require.Contains(t, ansi.Strip(root.dialogMgr.TopDialog().View()), "all 2 panes")
			root.Update(intent)
			msgs := workspaceCloseAnswer(t, root, key)
			require.False(t, hasMsg[workspaceCloseConfirmedMsg](msgs))
			for _, msg := range msgs {
				root.Update(msg)
			}
			require.Equal(t, 3, root.supervisor.Count())
			require.Equal(t, before, root.panes)
			require.Same(t, owner, root.paneWorkspaces.active)
			require.Equal(t, "second", root.paneFocus())
			require.Equal(t, "keep exact draft", root.editor.Value())
			require.Equal(t, choice, root.closeTabChoice)
			requireWorkspaceMembership(t, root)
		}
	}
}

func TestWorkspaceCloseConfirmAllViewsAndCleanupSelection(t *testing.T) {
	for _, active := range []bool{false, true} {
		for _, hidden := range []bool{false, true} {
			root := splitTestRoot(t)
			root.splitPane("second", "profile", splitRight)
			if hidden {
				root.singlePane()
				require.Len(t, root.panes.Sessions(), 1)
			}
			root.editors["third"].SetValue("surviving draft")
			if !active {
				root.handleSwitchTab("third")
			}
			root.Update(messages.CloseWorkspaceMsg{SessionID: "profile"})
			msgs := workspaceCloseAnswer(t, root, tea.KeyPressMsg{Code: 'y', Text: "y"})
			confirmed, ok := firstOfType[workspaceCloseConfirmedMsg](msgs)
			require.True(t, ok)
			for _, msg := range msgs {
				root.Update(msg)
			}
			require.Equal(t, 1, root.supervisor.Count())
			require.Equal(t, "third", root.paneFocus())
			require.Equal(t, []string{"third"}, root.panes.Sessions())
			require.Equal(t, []string{"third"}, workspaceIDs(root))
			require.Equal(t, "surviving draft", root.editor.Value())
			require.Len(t, root.chatPages, 1)
			require.Len(t, root.editors, 1)
			require.Len(t, root.sessionStates, 1)
			require.Equal(t, 1, root.tabBar.Count())
			require.NotContains(t, ansi.Strip(root.tabBar.View()), "[2]")
			requireWorkspaceMembership(t, root)
			root.Update(confirmed)
			require.Equal(t, 1, root.supervisor.Count(), "duplicate response cannot close destination")
		}
	}
}

func TestWorkspaceCloseRetainsRuntimesAndNeverCancelsTurns(t *testing.T) {
	root := splitTestRoot(t)
	var cleanups atomic.Int32
	var handles []*responseHandle
	shared := newCloseTabRuntime("root", true)
	for _, id := range []string{"profile", "second"} {
		sess := session.New(session.WithID(id))
		handle := &responseHandle{lifecycleHandle: &lifecycleHandle{id: id}}
		handles = append(handles, handle)
		opts := []app.Opt{app.WithRuntimeServices(shared)}
		if id == "second" {
			opts = append(opts, app.WithSubagentAttach(runtime.SubagentAttachInfo{NodeID: "child-node", ParentSessionID: "profile"}))
		}
		a := app.New(t.Context(), &responseSessions{openSubagentSessions: &openSubagentSessions{}, handle: handle}, sess, runtime.SessionBinding{}, opts...)
		root.supervisor.ReplaceRunnerApp(t.Context(), id, SpawnedSession{App: a, Session: sess, Ownership: RuntimeOwned, Cleanup: func() { cleanups.Add(1) }}, "")
	}
	root.handleSwitchTab("profile")
	root.splitPane("second", "profile", splitRight)
	root.Update(messages.CloseWorkspaceMsg{SessionID: "profile"})
	for _, msg := range workspaceCloseAnswer(t, root, tea.KeyPressMsg{Code: 'y', Text: "y"}) {
		root.Update(msg)
	}
	require.Nil(t, root.supervisor.GetRunner("profile"))
	require.Nil(t, root.supervisor.GetRunner("second"))
	require.Never(t, func() bool { return cleanups.Load() != 0 }, 20*time.Millisecond, time.Millisecond, "closing views must not stop their owned runtimes")
	for _, handle := range handles {
		require.Zero(t, handle.cancels.Load())
	}
	require.True(t, shared.HasRunningSubagents("profile"))
	root.supervisor.Shutdown()
	require.Equal(t, int32(2), cleanups.Load())
}

func TestWorkspaceCloseRejectsChangedMembershipOrRouteWithoutPartialClosure(t *testing.T) {
	for _, change := range []string{"membership", "generation"} {
		t.Run(change, func(t *testing.T) {
			root := splitTestRoot(t)
			root.splitPane("second", "profile", splitRight)
			root.Update(messages.CloseWorkspaceMsg{SessionID: "profile"})
			confirmed, ok := firstOfType[workspaceCloseConfirmedMsg](workspaceCloseAnswer(t, root, tea.KeyPressMsg{Code: 'y', Text: "y"}))
			require.True(t, ok)
			if change == "membership" {
				root.detachPaneToWorkspace("second", 0)
			} else {
				sess := session.New(session.WithID("second"))
				a := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
				root.supervisor.ReplaceRunnerApp(t.Context(), "second", SpawnedSession{App: a, Session: sess, Ownership: RuntimeBorrowed}, "")
			}
			root.Update(confirmed)
			require.Equal(t, 3, root.supervisor.Count())
			require.NotNil(t, root.supervisor.GetRunner("profile"))
			require.NotNil(t, root.supervisor.GetRunner("second"))
			requireWorkspaceMembership(t, root)
		})
	}
}

func TestActualProgramLastWorkspaceClosePreservesGlobalExitSafeguard(t *testing.T) {
	root := splitTestRoot(t)
	root.ar = animation.NewRuntime()
	root.dialogMgr = dialog.New(root.ar)
	root.splitPane("second", "profile", splitRight)
	root.splitPane("third", "profile", splitBottom)
	root.editor.SetValue("LAST-WORKSPACE-DRAFT")
	program := startPaneProgram(t, root)
	program.Send(paneProgramAction{apply: func(m *appModel) tea.Cmd {
		x, y := workspaceClosePoint(t, m, 0)
		_, cmd := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		return cmd
	}})
	require.Eventually(t, func() bool {
		s := queryPaneProgram(t, program)
		return s.modal && strings.Contains(ansi.Strip(s.content), "Close workspace?")
	}, time.Second, time.Millisecond)
	program.Send(tea.KeyPressMsg{Code: 'y', Text: "y"})
	require.Eventually(t, func() bool {
		s := queryPaneProgram(t, program)
		return s.modal && strings.Contains(ansi.Strip(s.content), "Do you want to exit?")
	}, time.Second, time.Millisecond)
	s := queryPaneProgram(t, program)
	require.Len(t, s.panes, 3, "all panes remain until the global exit decision")
	require.Equal(t, "LAST-WORKSPACE-DRAFT", s.draft)
	program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Eventually(t, func() bool { return !queryPaneProgram(t, program).modal }, time.Second, time.Millisecond)
	s = queryPaneProgram(t, program)
	require.Len(t, s.panes, 3)
	require.Equal(t, "LAST-WORKSPACE-DRAFT", s.draft)
}
