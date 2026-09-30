package tui

import (
	"context"
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
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type retainedShutdownMsg struct{}

type retainedLifecycleProgram struct{ *shellProgramModel }

func (m *retainedLifecycleProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(retainedShutdownMsg); ok {
		m.root.supervisor.Shutdown()
		m.root.supervisor.Shutdown()
		return m, nil
	}
	_, cmd := m.shellProgramModel.Update(msg)
	return m, cmd
}

type retainedBackgroundHandle struct {
	*lifecycleHandle

	cancelCalls atomic.Int32
}

func (h *retainedBackgroundHandle) Cancel(context.Context, string) (runtime.CancelResult, error) {
	h.cancelCalls.Add(1)
	return runtime.CancelResult{SessionID: h.id, Outcome: runtime.CancelAccepted}, nil
}

type retainedBackgroundSessions struct {
	*openSubagentSessions

	handles map[string]*retainedBackgroundHandle
}

func (s *retainedBackgroundSessions) SessionByID(id string) (runtime.SessionHandle, error) {
	if handle := s.handles[id]; handle != nil {
		return handle, nil
	}
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id}
}

func TestActualProgramCloseParentDetachesAndRetainsNestedBackgroundViews(t *testing.T) {
	parent := session.New(session.WithID("close-parent"), session.WithAgentName("root"))
	parent.Title = "CLOSE-PARENT"
	child := session.New(session.WithID("close-child"), session.WithAgentName("worker"))
	child.Title = "KEEP-CHILD"
	grand := session.New(session.WithID("close-grand"), session.WithAgentName("reviewer"))
	grand.Title = "KEEP-GRAND"
	sessions := &retainedBackgroundSessions{openSubagentSessions: &openSubagentSessions{}, handles: map[string]*retainedBackgroundHandle{}}
	for _, sess := range []*session.Session{parent, child, grand} {
		sessions.handles[sess.ID] = &retainedBackgroundHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}}
	}
	services := newCloseTabRuntime("shared", false)
	parentApp := app.New(t.Context(), sessions, parent, runtime.SessionBinding{}, app.WithRuntimeServices(services))
	root := newSidebarProgramRoot(t, parentApp)
	// Install an explicitly owned tab runtime as the custom-spawner case.
	var cleanup atomic.Int32
	root.supervisor.ReplaceRunnerApp(t.Context(), parent.ID, SpawnedSession{App: parentApp, Session: parent, Ownership: RuntimeOwned, Cleanup: func() { cleanup.Add(1) }}, "")
	for _, item := range []struct {
		sess          *session.Session
		parent, agent string
	}{{child, parent.ID, "worker"}, {grand, child.ID, "reviewer"}} {
		attached := runtime.SubagentAttachInfo{NodeID: subagent.NodeID(item.sess.ID + "-node"), Session: item.sess, Agent: item.agent, ParentSessionID: item.parent, ParentAgent: "root"}
		application := app.New(t.Context(), sessions, item.sess, runtime.SessionBinding{}, app.WithRuntimeServices(services), app.WithSubagentAttach(attached))
		_, err := root.supervisor.AddSession(t.Context(), application, item.sess, "", nil)
		require.NoError(t, err)
	}
	root.editor.Blur()
	program := startTestProgram(t, root, &retainedLifecycleProgram{shellProgramModel: &shellProgramModel{root: root}}, tea.WithOutput(&cacheProgramWriter{}))
	root.supervisor.SetProgram(program)
	program.Send(messages.CloseTabMsg{SessionID: parent.ID})
	require.Eventually(t, func() bool { return sidebarProgramSnapshot(t, program).open }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		return strings.Contains(ansi.Strip(sidebarProgramSnapshot(t, program).content), "Subagents keep running")
	}, time.Second, time.Millisecond, "wait for shared reveal before reading prompt text")
	prompt := sidebarProgramSnapshot(t, program)
	require.Len(t, prompt.tabIDs, 3)
	program.Send(tea.KeyPressMsg{Code: 'n', Text: "n"})
	require.Eventually(t, func() bool { return !sidebarProgramSnapshot(t, program).open }, time.Second, time.Millisecond)
	require.Len(t, sidebarProgramSnapshot(t, program).tabIDs, 3, "cancel closes no view")
	require.Zero(t, cleanup.Load())
	program.Send(messages.CloseTabMsg{SessionID: parent.ID})
	require.Eventually(t, func() bool { return sidebarProgramSnapshot(t, program).open }, time.Second, time.Millisecond)
	program.Send(tea.KeyPressMsg{Code: 'y', Text: "y"})
	require.Eventually(t, func() bool { s := sidebarProgramSnapshot(t, program); return len(s.tabIDs) == 2 && !s.open }, time.Second, time.Millisecond)
	closed := sidebarProgramSnapshot(t, program)
	require.ElementsMatch(t, []string{child.ID, grand.ID}, closed.tabIDs)
	require.Zero(t, cleanup.Load(), "owned runtime is retained until shutdown")
	// Background execution admission remains possible after the owner view detaches.
	_, err := sessions.handles[grand.ID].Submit(t.Context(), runtime.TurnInput{Content: "background continues"})
	require.NoError(t, err)
	require.Equal(t, int32(1), sessions.handles[grand.ID].submits.Load())
	for _, handle := range sessions.handles {
		require.Zero(t, handle.cancelCalls.Load())
		require.Zero(t, handle.releases.Load())
	}
	program.Send(messages.CloseTabMsg{SessionID: child.ID})
	require.Eventually(t, func() bool { return sidebarProgramSnapshot(t, program).open }, time.Second, time.Millisecond, "attached parent with grandchild also confirms")
	program.Send(tea.KeyPressMsg{Code: 'y', Text: "y"})
	require.Eventually(t, func() bool { s := sidebarProgramSnapshot(t, program); return len(s.tabIDs) == 1 && !s.open }, time.Second, time.Millisecond)
	require.Equal(t, []string{grand.ID}, sidebarProgramSnapshot(t, program).tabIDs)
	require.Zero(t, cleanup.Load())
	for _, handle := range sessions.handles {
		require.Zero(t, handle.cancelCalls.Load())
	}
	program.Send(retainedShutdownMsg{})
	require.Eventually(t, func() bool { return cleanup.Load() == 1 }, time.Second, time.Millisecond)
}

type topologyRuntime struct {
	*closeTabRuntime

	tree *subagent.Tree
}

func (r *topologyRuntime) SubagentTree() *subagent.Tree { return r.tree }

func TestActualProgramResetHydratesSameSessionCanonicalTree(t *testing.T) {
	for _, source := range []string{"merged-session", "live-runtime"} {
		t.Run(source, func(t *testing.T) {
			sess := session.New(session.WithID("reload-root"), session.WithAgentName("root"))
			sess.Title = "RESTORED-TREE"
			tree := subagent.NewTree()
			require.NoError(t, tree.AddSubtree([]subagent.Node{
				{ID: subagent.SessionRootID(sess.ID), SessionID: sess.ID, Agent: "root", State: subagent.NodeIdle},
				{ID: "reload-child-full-node", Parent: subagent.SessionRootID(sess.ID), SessionID: "reload-child", Agent: "worker", Name: "RestoredChild", State: subagent.NodeIdle},
				{ID: "reload-grand-full-node", Parent: "reload-child-full-node", SessionID: "reload-grand", Agent: "reviewer", Name: "RestoredGrandchild", State: subagent.NodeCompleted},
			}))
			snapshot := tree.Snapshot()
			var services app.Services = newCloseTabRuntime("root", false)
			if source == "merged-session" {
				sess.SetSubagentTree(&snapshot)
			} else {
				services = &topologyRuntime{closeTabRuntime: newCloseTabRuntime("root", false), tree: tree}
			}
			application := app.New(t.Context(), nil, sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(services))
			root := newSidebarProgramRoot(t, application)
			_, _ = root.updateWithLifecycle(runtime.TeamInfo([]runtime.AgentDetails{{Name: "root"}, {Name: "worker"}, {Name: "reviewer"}}, "root"))
			program := startTestProgram(t, root, &shellProgramModel{root: root}, tea.WithOutput(&cacheProgramWriter{}))
			require.Contains(t, ansi.Strip(expandProgramSubagents(t, program, "RestoredGrandchild").content), "RestoredGrandchild")
			reset := session.New(session.WithID(sess.ID), session.WithAgentName("root"))
			reset.AddMessage(session.UserMessage("RESET-TRANSCRIPT"))
			program.Send(&app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: reset, Status: runtime.SessionStatus{SessionID: sess.ID, AgentName: "root", State: runtime.SessionStateSettled}}})
			restored := sidebarProgramSnapshot(t, program)
			require.Contains(t, ansi.Strip(restored.content), "RESET-TRANSCRIPT")
			require.Contains(t, ansi.Strip(restored.content), "RestoredChild")
			require.Contains(t, ansi.Strip(restored.content), "RestoredGrandchild")
			other := session.New(session.WithID("different-reset-session"), session.WithAgentName("root"))
			other.SetSubagentTree(&snapshot)
			program.Send(&app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: other, Status: runtime.SessionStatus{SessionID: other.ID, AgentName: "root", State: runtime.SessionStateSettled}}})
			foreign := sidebarProgramSnapshot(t, program)
			require.NotContains(t, ansi.Strip(foreign.content), "RestoredChild", "foreign-session topology is never borrowed")
			require.NotContains(t, ansi.Strip(foreign.content), "RestoredGrandchild")
		})
	}
}
