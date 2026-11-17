package tui

import (
	"context"
	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/server"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestAgentPickerCreatesEmptySessionWithoutRetargetingSource(t *testing.T) {
	for _, remote := range []bool{false, true} {
		for _, target := range []string{"director", "worker"} {
			t.Run(map[bool]string{false: "local", true: "remote"}[remote]+"/"+target, func(t *testing.T) {
				f := newAgentPickerFixture(t, remote)
				root := f.root
				root.sessionState.SetAvailableAgents([]runtime.AgentDetails{{Name: "director"}, {Name: "worker"}})
				source := root.application.Session()
				originalID := source.ID
				originalMessages := source.MessagesSnapshot()
				root.editor.SetValue("keep my draft")
				_, cmd := root.handleNewAgentSession(messages.NewAgentSessionMsg{AgentName: target, WorkingDir: source.WorkingDir})
				result, ok := firstOfType[hostedLoadResult](collectMsgs(cmd))
				require.True(t, ok)
				require.NoError(t, result.err)
				require.Equal(t, originalID, root.application.Session().ID)
				require.NotEqual(t, originalID, result.data.application.Session().ID)
				next := result.data.application.Session()
				require.Equal(t, target, next.AgentName)
				require.Equal(t, source.WorkingDir, next.WorkingDir)
				require.Empty(t, next.MessagesSnapshot())
				root.finishHostedLoad(result)
				require.Equal(t, 2, root.supervisor.Count())
				require.Equal(t, next.ID, root.application.Session().ID)
				root.handleSwitchTab(originalID)
				require.Equal(t, originalMessages, root.application.Session().MessagesSnapshot())
				require.Equal(t, "keep my draft", root.editor.Value())
			})
		}
	}
}

func TestAgentPickerOpensFromCompleteTeamAndKeepsDirectory(t *testing.T) {
	f := newAgentPickerFixture(t, false)
	root := f.root
	root.sessionState.SetAvailableAgents([]runtime.AgentDetails{{Name: "director"}, {Name: "worker"}})
	_, cmd := root.handleOpenAgentPicker()
	require.NotNil(t, cmd)
	opened, ok := cmd().(dialog.OpenDialogMsg)
	require.True(t, ok)
	opened.Model.SetSize(120, 40)
	require.Contains(t, opened.Model.View(), "director")
	require.Contains(t, opened.Model.View(), "worker")
}

func newAgentPickerFixture(t *testing.T, remote bool) *parentNavigationFixture {
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
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, owner.Shutdown(ctx))
	})
	sessions := owner.Runtime()
	if remote {
		ctx, stop := context.WithCancel(t.Context())
		listener, listenErr := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
		require.NoError(t, listenErr)
		api := server.NewWithManager(server.NewSessionManager(ctx, nil, store, 0, nil, server.WithSessionRuntimes(map[string]runtime.SessionRuntime{source: pickerSessionRegistry{sessions}, "foreign-source": pickerSessionRegistry{sessions}})), "")
		done := make(chan error, 1)
		go func() { done <- api.Serve(ctx, listener) }()
		t.Cleanup(func() {
			stop()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(3 * time.Second):
				t.Error("test server shutdown timed out")
			}
		})
		client, clientErr := runtime.NewClient("http://" + listener.Addr().String())
		require.NoError(t, clientErr)
		sessions, err = runtime.NewSessionTransport(client, runtime.WithSessionTransportSource(source))
		require.NoError(t, err)
	}
	selected, err := runtime.RestoreSessionView(t.Context(), sessions, chain[1].ID)
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

// Hide optional cache warming: its owner-lock reentry deadlocks normal remote SSE fixtures.
// Creation, lookup, preparation, and observation still use the real runtime.
type pickerSessionRegistry struct{ runtime.SessionRuntime }
type pickerSessionHandle struct{ runtime.SessionHandle }

func (r pickerSessionRegistry) CreateSession(ctx context.Context, s *session.Session, b runtime.SessionBinding) (runtime.SessionHandle, error) {
	h, err := r.SessionRuntime.CreateSession(ctx, s, b)
	if err != nil {
		return nil, err
	}
	return pickerSessionHandle{h}, nil
}
func (r pickerSessionRegistry) SessionByID(id string) (runtime.SessionHandle, error) {
	h, err := r.SessionRuntime.SessionByID(id)
	if err != nil {
		return nil, err
	}
	return pickerSessionHandle{h}, nil
}
func (r pickerSessionRegistry) PrepareSessionView(ctx context.Context, id string) (runtime.PreparedSessionView, error) {
	return r.SessionRuntime.(runtime.SessionViewPreparer).PrepareSessionView(ctx, id)
}
func (r pickerSessionRegistry) ConfirmedSessionViewInfo(ctx context.Context, id string) (runtime.PreparedSessionViewInfo, error) {
	return r.SessionRuntime.(runtime.SessionViewInfoReader).ConfirmedSessionViewInfo(ctx, id)
}
