package tui

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/server"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

type parentNavigationFixture struct {
	root        *appModel
	local       *runtime.LocalRuntime
	sessions    runtime.SessionRuntime
	chain       []*session.Session
	provider    *attachToolProvider
	resolutions []string
}

func newParentNavigationFixture(t *testing.T, remote bool, depth int) *parentNavigationFixture {
	t.Helper()
	const source = "parent-navigation-team"
	store := session.NewInMemorySessionStore()
	dir := t.TempDir()
	chain := []*session.Session{
		session.New(session.WithID("canonical-parent-session"), session.WithAgentName("director")),
		session.New(session.WithID("canonical-child-session"), session.WithAgentName("worker")),
		session.New(session.WithID("canonical-grandchild-session"), session.WithAgentName("worker")),
	}
	for i, sess := range chain {
		sess.WorkingDir = dir
		sess.SetAttribute(runtime.SessionAgentAttribute, sess.AgentName)
		sess.SetAttribute("docker-agent.actor.source", source)
		sess.AddMessage(session.UserMessage("HISTORY " + sess.ID))
		if i > 0 {
			sess.ParentID = chain[i-1].ID
			sess.SetAttribute(runtime.SessionParentAgentAttribute, chain[i-1].AgentName)
		}
	}
	pending := session.UserMessage("accepted FIFO must stay dormant")
	pending.TurnID, pending.Pending, pending.Accepted = "pending-root", true, true
	pending.InputOrigin = session.InputOriginUser
	chain[0].AddMessage(pending)
	require.NoError(t, store.AddSession(t.Context(), chain[0]))
	parentNode := subagent.SessionRootID(chain[0].ID)
	for i, node := range []subagent.NodeID{"child-node-not-session-id", "grandchild-node-not-session-id"} {
		child := chain[i+1]
		require.NoError(t, store.(session.CoordinationStore).AdmitChild(t.Context(), session.ChildAdmission{Child: child, Record: session.ChildRecord{
			RootSessionID: chain[0].ID, ParentSessionID: child.ParentID,
			Node: subagent.Node{ID: node, SessionID: child.ID, Parent: parentNode, Agent: child.AgentName, State: subagent.NodeIdle},
		}}))
		parentNode = node
	}
	provider := &attachToolProvider{packets: make(chan attachToolPacket)}
	local, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(
		agent.New("director", "director", agent.WithModel(provider), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "worker", agent.WithModel(provider), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
	)), runtime.WithSessionStore(store), runtime.WithWorkingDir(dir))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(local)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sessions := owner.Runtime()
	if remote {
		ctx, stop := context.WithCancel(t.Context())
		listener, listenErr := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
		require.NoError(t, listenErr)
		api := server.NewWithManager(server.NewSessionManager(ctx, nil, store, 0, nil, server.WithSessionRuntimes(map[string]runtime.SessionRuntime{source: sessions, "foreign-source": sessions})), "")
		done := make(chan error, 1)
		go func() { done <- api.Serve(ctx, listener) }()
		t.Cleanup(func() { stop(); require.NoError(t, <-done) })
		client, clientErr := runtime.NewClient("http://" + listener.Addr().String())
		require.NoError(t, clientErr)
		sessions, err = runtime.NewSessionTransport(client, runtime.WithSessionTransportSource(source))
		require.NoError(t, err)
	}
	selected, err := runtime.RestoreSessionView(t.Context(), sessions, chain[depth].ID)
	require.NoError(t, err)
	// A child-only projection contains no ancestor, as with a remote tree watch.
	selected.Info.Session.SetSubagentTree(nil)
	services := storeRuntime{store: store}
	application, err := app.NewResolved(t.Context(), sessions, selected, app.WithRuntimeServices(services), app.WithSubagentAttach(*selected.Info.Attach))
	require.NoError(t, err)
	f := &parentNavigationFixture{local: local, sessions: sessions, chain: chain, provider: provider}
	host := supervisor.New(nil)
	scope := supervisor.NewViewOwnerScope()
	identity := supervisor.ViewOwnerIdentity{Scope: scope, Source: source, RootSessionID: chain[0].ID, RootWorkingDir: dir, RootBinding: runtime.SessionBinding{AgentName: "director"}}
	require.NoError(t, host.ConfigureSessionViews(t.Context(), supervisor.HostViewConfig{
		Resolve: func(ctx context.Context, id string) (supervisor.ViewOwnerIdentity, error) {
			f.resolutions = append(f.resolutions, id)
			info, readErr := sessions.(runtime.SessionViewInfoReader).ConfirmedSessionViewInfo(ctx, id)
			if readErr != nil {
				return supervisor.ViewOwnerIdentity{}, readErr
			}
			resolved := identity
			resolved.RootSessionID = info.RootSessionID
			return resolved, nil
		},
		MaxRetainedViewOwners: supervisor.DefaultMaxRetainedViewOwners,
	}))
	require.NoError(t, host.RegisterSessionOwner(identity, supervisor.ViewOwnerResources{Services: services, Sessions: sessions, NewApp: func(ctx context.Context, view runtime.CommittedSessionView) (*app.App, error) {
		return app.NewResolvedFromTemplate(ctx, sessions, view, application)
	}}, true))
	t.Setenv("HOME", t.TempDir())
	f.root = New(t.Context(), nil, application, dir, nil, WithSupervisor(host)).(*appModel)
	t.Cleanup(f.root.cleanupManagedResources)
	f.root.handleWindowResize(120, 40)
	return f
}

func TestUnopenedSubagentParentUsesCanonicalSessionView(t *testing.T) {
	for _, remote := range []bool{false, true} {
		for _, depth := range []int{1, 2} {
			name := map[bool]string{false: "local", true: "remote"}[remote] + map[int]string{1: "/child", 2: "/nested-child"}[depth]
			t.Run(name, func(t *testing.T) {
				f := newParentNavigationFixture(t, remote, depth)
				root, parent := f.root, f.chain[depth-1]
				_, found := root.application.ResolveSubagentTarget(parent.ID)
				require.False(t, found, "parent is outside the selected view's topology")
				handle, err := f.sessions.SessionByID(parent.ID)
				require.NoError(t, err)
				before, err := handle.Status(t.Context())
				require.NoError(t, err)
				topology := f.local.SubagentTree().Snapshot()
				root.editor.SetValue("CHILD DRAFT")
				_, cmd := root.Update(messages.SwitchTabMsg{SessionID: parent.ID})
				require.NotNil(t, root.opening)
				require.Equal(t, parent.ID, root.opening.target)
				result, ok := firstOfType[subagentOpenedMsg](collectMsgs(cmd))
				require.True(t, ok)
				require.NoError(t, result.err)
				require.Equal(t, []string{parent.ID}, f.resolutions)
				root.finishSubagentOpening(result)
				require.Equal(t, parent.ID, root.paneFocus())
				require.Equal(t, 2, root.supervisor.Count())
				require.Equal(t, parent.ID, root.application.SessionHandle().ID())
				if depth == 2 {
					require.Equal(t, subagent.NodeID("child-node-not-session-id"), root.application.AttachedSubagent().NodeID)
				} else {
					require.Nil(t, root.application.AttachedSubagent())
				}
				after, err := handle.Status(t.Context())
				require.NoError(t, err)
				require.Equal(t, before, after, "opening a parent never resumes accepted FIFO")
				require.Equal(t, topology, f.local.SubagentTree().Snapshot())
				require.Zero(t, f.provider.calls.Load())
				root.cancelSessionOpening()
				root.handleSwitchTab(f.chain[depth].ID)
				require.Equal(t, "CHILD DRAFT", root.editor.Value())
				root.handleSwitchTab(parent.ID)
				require.Equal(t, 2, root.supervisor.Count(), "existing parent focuses without duplicate acquisition")
				require.Len(t, f.resolutions, 1)
			})
		}
	}
}

func TestSubagentParentExistingAliasedAndLazyTabsAreReused(t *testing.T) {
	for _, lazy := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing", true: "lazy"}[lazy], func(t *testing.T) {
			f := newParentNavigationFixture(t, false, 1)
			root, parent := f.root, f.chain[0]
			placeholder := session.New(session.WithID("parent-tab-key"), session.WithAgentName("director"), session.WithWorkingDir(parent.WorkingDir))
			placeholderApp := app.New(t.Context(), nil, placeholder, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
			id, err := root.supervisor.AddSession(t.Context(), placeholderApp, placeholder, parent.WorkingDir, nil)
			require.NoError(t, err)
			root.createSessionComponents(id, placeholderApp, placeholder)
			if lazy {
				root.pendingRestores[id] = parent.ID
			} else {
				data, err := loadHostedSession(t.Context(), root.supervisor, parent.ID)
				require.NoError(t, err)
				defer data.prepared.Abort()
				application, err := data.newApp(t.Context(), data.committed)
				require.NoError(t, err)
				_, _, err = root.adoptSessionView(application, application.Session(), parent.WorkingDir, sessionViewPolicy{replaceRoute: id})
				require.NoError(t, err)
				f.resolutions = nil
			}
			_, cmd := root.Update(messages.SwitchTabMsg{SessionID: parent.ID})
			if lazy {
				result, ok := firstOfType[hostedLoadResult](collectMsgs(cmd))
				require.True(t, ok)
				require.NoError(t, result.err)
				root.finishHostedLoad(result)
				require.Equal(t, []string{parent.ID}, f.resolutions)
			} else {
				require.Empty(t, f.resolutions, "existing canonical session does not acquire again")
			}
			require.Equal(t, id, root.paneFocus())
			require.Equal(t, 2, root.supervisor.Count())
			require.Nil(t, root.opening)
		})
	}
}

func TestSubagentParentMissingAndForeignLeaveCurrentViewUnchanged(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreign", true: "missing"}[missing], func(t *testing.T) {
			f := newParentNavigationFixture(t, true, 1)
			root := f.root
			parentID := "missing-parent"
			if !missing {
				foreign := session.New(session.WithID("foreign-parent"), session.WithAgentName("director"), session.WithWorkingDir(f.chain[0].WorkingDir))
				foreign.SetAttribute(runtime.SessionAgentAttribute, "director")
				foreign.SetAttribute("docker-agent.actor.source", "foreign-source")
				require.NoError(t, root.application.Runtime().SessionStore().AddSession(t.Context(), foreign))
				parentID = foreign.ID
			}
			_, arbitrary := root.handleSwitchTab(parentID)
			arbitraryNote, ok := firstOfType[notification.ShowMsg](collectMsgs(arbitrary))
			require.True(t, ok)
			require.Equal(t, "Session not found", arbitraryNote.Text, "unknown targets are not blanket restore requests")
			require.Empty(t, f.resolutions)
			root.application.AttachedSubagent().ParentSessionID = parentID
			application := root.application
			root.editor.SetValue("UNCHANGED DRAFT")
			_, cmd := root.handleSwitchTab(parentID)
			result, ok := firstOfType[subagentOpenedMsg](collectMsgs(cmd))
			require.True(t, ok)
			require.Error(t, result.err)
			note, ok := firstOfType[notification.ShowMsg](collectMsgs(root.finishSubagentOpening(result)))
			require.True(t, ok)
			require.Equal(t, notification.TypeError, note.Type)
			require.Same(t, application, root.application)
			require.Equal(t, f.chain[1].ID, root.paneFocus())
			require.Equal(t, "UNCHANGED DRAFT", root.editor.Value())
			require.Equal(t, 1, root.supervisor.Count())
			require.Nil(t, root.opening)
			require.Zero(t, f.provider.calls.Load())
		})
	}
}

func TestRemoteSidebarParentClickPreservesRunningCanonicalParent(t *testing.T) {
	f := newParentNavigationFixture(t, true, 2)
	root, parent := f.root, f.chain[1]
	handle, err := f.sessions.SessionByID(parent.ID)
	require.NoError(t, err)
	_, err = handle.Submit(t.Context(), runtime.TurnInput{Content: "keep running", RequestID: "active-parent-turn"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return f.provider.calls.Load() == 1 }, time.Second, time.Millisecond)
	before, err := handle.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, runtime.SessionStateRunning, before.State)
	_ = root.init()
	program := startTestProgram(t, root, &shellProgramModel{root: root}, tea.WithOutput(&cacheProgramWriter{}))
	root.supervisor.SetProgram(program)
	var frame shellSnapshot
	require.Eventually(t, func() bool {
		frame = sidebarProgramSnapshot(t, program)
		return frame.active == 0 && strings.Contains(ansi.Strip(frame.content), "parent: worker")
	}, time.Second, time.Millisecond)
	x, y := sidebarProgramPoint(t, frame.content, "parent: worker")
	program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Eventually(t, func() bool {
		frame = sidebarProgramSnapshot(t, program)
		return frame.activeID == parent.ID && strings.Contains(ansi.Strip(frame.content), "HISTORY "+parent.ID)
	}, 4*time.Second, time.Millisecond)
	after, err := handle.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, before.State, after.State)
	require.Equal(t, before.TurnID, after.TurnID)
	require.EqualValues(t, 1, f.provider.calls.Load(), "navigation must not restart the running parent")
	require.NotContains(t, ansi.Strip(frame.content), "Session not found")
}
