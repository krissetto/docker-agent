package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

type stoppedAttachFixture struct {
	root     *appModel
	parent   *session.Session
	child    *session.Session
	provider *attachToolProvider
	local    *runtime.LocalRuntime
	store    session.Store
	msgs     chan tea.Msg
}

func newStoppedAttachFixture(t *testing.T) *stoppedAttachFixture {
	t.Helper()
	const source = "stopped-attach-team"
	store := session.NewInMemorySessionStore()
	dir := t.TempDir()
	parent := session.New(session.WithID("stopped-attach-parent"), session.WithAgentName("director"))
	child := session.New(session.WithID("stopped-attach-child"), session.WithAgentName("worker"), session.WithParentID(parent.ID))
	for _, sess := range []*session.Session{parent, child} {
		sess.WorkingDir = dir
		sess.SetAttribute(runtime.SessionAgentAttribute, sess.AgentName)
		sess.SetAttribute("docker-agent.actor.source", source)
		sess.AddMessage(session.UserMessage("HISTORY " + sess.ID))
	}
	child.SetAttribute(runtime.SessionParentAgentAttribute, parent.AgentName)
	require.NoError(t, store.AddSession(t.Context(), parent))
	require.NoError(t, store.(session.CoordinationStore).AdmitChild(t.Context(), session.ChildAdmission{Child: child, Record: session.ChildRecord{
		RootSessionID: parent.ID, ParentSessionID: parent.ID,
		Node: subagent.Node{ID: "stopped-attach-child-node", SessionID: child.ID, Parent: subagent.SessionRootID(parent.ID), Agent: child.AgentName, State: subagent.NodeStopped},
	}}))
	provider := &attachToolProvider{packets: make(chan attachToolPacket)}
	local, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(
		agent.New("director", "director", agent.WithModel(provider), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "worker", agent.WithModel(provider), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
	)), runtime.WithSessionStore(store), runtime.WithWorkingDir(dir))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(local)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sessions := owner.Runtime()
	selected, err := runtime.RestoreSessionView(t.Context(), sessions, parent.ID)
	require.NoError(t, err)
	services := storeRuntime{store: store}
	application, err := app.NewResolved(t.Context(), sessions, selected, app.WithRuntimeServices(services))
	require.NoError(t, err)
	host := supervisor.New(nil)
	identity := supervisor.ViewOwnerIdentity{Scope: supervisor.NewViewOwnerScope(), Source: source, RootSessionID: parent.ID, RootWorkingDir: dir, RootBinding: runtime.SessionBinding{AgentName: "director"}}
	require.NoError(t, host.ConfigureSessionViews(t.Context(), supervisor.HostViewConfig{
		Resolve: func(ctx context.Context, id string) (supervisor.ViewOwnerIdentity, error) {
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
	root := New(t.Context(), nil, application, dir, nil, WithSupervisor(host)).(*appModel)
	t.Cleanup(root.cleanupManagedResources)
	root.handleWindowResize(120, 40)
	return &stoppedAttachFixture{root: root, parent: parent, child: child, provider: provider, local: local, store: store, msgs: make(chan tea.Msg, 256)}
}

// beginAttach starts opening the stopped child and returns the acquisition result.
func (f *stoppedAttachFixture) beginAttach(t *testing.T) subagentOpenedMsg {
	t.Helper()
	_, cmd := f.root.update(messages.SwitchTabMsg{SessionID: f.child.ID})
	result, ok := firstOfType[subagentOpenedMsg](collectMsgs(cmd))
	require.True(t, ok)
	require.NoError(t, result.err)
	return result
}

// start runs cmd asynchronously, as bubbletea does; its message returns via pump.
func (f *stoppedAttachFixture) start(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		if msg := cmd(); msg != nil {
			f.msgs <- msg
		}
	}()
}

// pump delivers async results and animation ticks to the model until done holds,
// then drains results already in flight so completion-time replays are delivered.
func (f *stoppedAttachFixture) pump(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	f.start(f.root.ar.Continue())
	for !done() {
		select {
		case msg := <-f.msgs:
			f.deliver(msg)
		case <-deadline:
			require.FailNow(t, "session opening did not complete")
		}
	}
	settle := time.After(200 * time.Millisecond)
	for {
		select {
		case msg := <-f.msgs:
			f.deliver(msg)
		case <-settle:
			return
		}
	}
}

func (f *stoppedAttachFixture) deliver(msg tea.Msg) {
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, cmd := range batch {
			f.start(cmd)
		}
		return
	}
	_, next := f.root.Update(msg)
	f.start(next)
}

// attach opens the child and runs the opening's async work to completion.
func (f *stoppedAttachFixture) attach(t *testing.T) {
	t.Helper()
	_, cmd := f.root.update(messages.SwitchTabMsg{SessionID: f.child.ID})
	result, ok := firstOfType[subagentOpenedMsg](collectMsgs(cmd))
	require.True(t, ok)
	require.NoError(t, result.err)
	f.start(f.root.finishSubagentOpening(result))
	f.pump(t, func() bool { return f.root.opening == nil })
}

func (f *stoppedAttachFixture) typeText(t *testing.T, text string) {
	t.Helper()
	for _, r := range text {
		_, _ = f.root.update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func (f *stoppedAttachFixture) childStopped(t *testing.T) bool {
	t.Helper()
	handle, err := f.local.SessionByID(f.child.ID)
	require.NoError(t, err)
	status, err := handle.Status(t.Context())
	require.NoError(t, err)
	return status.Stopped
}

func TestAttachedStoppedChildStartsTurnOnNormalSend(t *testing.T) {
	f := newStoppedAttachFixture(t)
	result := f.beginAttach(t)
	f.root.finishSubagentOpening(result)
	require.Equal(t, f.child.ID, f.root.paneFocus())
	require.False(t, f.root.application.IsReadOnly())
	require.NotNil(t, f.root.application.AttachedSubagent())
	f.root.cancelSessionOpening()
	_ = f.root.editor.Focus()
	require.True(t, f.childStopped(t))
	_, _ = f.root.update(messages.SendMsg{Content: "wake up"})
	require.Eventually(t, func() bool { return f.provider.calls.Load() >= 1 }, 3*time.Second, 10*time.Millisecond, "normal send did not start a turn")
	assert.False(t, f.childStopped(t))
}

// Typed input and Enter during the overlay are delivered once it completes, never earlier.
func TestAttachedStoppedChildReplaysTypedInputAfterOpening(t *testing.T) {
	f := newStoppedAttachFixture(t)
	_, cmd := f.root.update(messages.SwitchTabMsg{SessionID: f.child.ID})
	result, ok := firstOfType[subagentOpenedMsg](collectMsgs(cmd))
	require.True(t, ok)
	require.NoError(t, result.err)
	f.start(f.root.finishSubagentOpening(result))
	require.NotNil(t, f.root.opening)
	f.typeText(t, "wake up")
	_, _ = f.root.update(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.Zero(t, f.provider.calls.Load(), "no dispatch before the opening completes")
	require.NotNil(t, f.root.opening)
	_, _ = f.root.update(messages.SendMsg{Content: "origin command"})
	f.pump(t, func() bool { return f.root.opening == nil })
	require.Eventually(t, func() bool { return f.provider.calls.Load() >= 1 }, 3*time.Second, 10*time.Millisecond, "buffered input did not start a turn")
	assert.EqualValues(t, 1, f.provider.calls.Load(), "origin send is dropped, never replayed into the child")
	assert.False(t, f.childStopped(t))
}

// Cancelling an installed attach keeps the text as the child's unsent draft and
// leaves the origin draft and the provider untouched.
func TestAttachedStoppedChildCancelKeepsInputForTargetOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel func(f *stoppedAttachFixture)
	}{
		{name: "escape", cancel: func(f *stoppedAttachFixture) {
			_, _ = f.root.update(tea.KeyPressMsg{Code: tea.KeyEscape})
		}},
		{name: "switch tab", cancel: func(f *stoppedAttachFixture) {
			_, _ = f.root.update(messages.SwitchTabMsg{SessionID: f.parent.ID})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStoppedAttachFixture(t)
			f.root.editors[f.parent.ID].SetValue("ORIGIN DRAFT")
			result := f.beginAttach(t)
			f.root.finishSubagentOpening(result)
			require.NotNil(t, f.root.opening)
			f.typeText(t, "wake up")
			_, _ = f.root.update(tea.KeyPressMsg{Code: tea.KeyEnter})
			tc.cancel(f)
			assert.Nil(t, f.root.opening)
			assert.Equal(t, "ORIGIN DRAFT", f.root.editors[f.parent.ID].Value())
			assert.Contains(t, f.root.editors[f.child.ID].Value(), "wake up")
			assert.Zero(t, f.provider.calls.Load(), "cancelled input is never dispatched")
			assert.True(t, f.childStopped(t))
		})
	}
}

// Cancelling before install holds the text for the child until it is next opened.
func TestAttachedStoppedChildCancelBeforeInstallRetainsDraftForNextOpen(t *testing.T) {
	f := newStoppedAttachFixture(t)
	f.root.editors[f.parent.ID].SetValue("ORIGIN DRAFT")
	result := f.beginAttach(t)
	f.typeText(t, "wake up")
	_, _ = f.root.update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Nil(t, f.root.opening)
	assert.Equal(t, "ORIGIN DRAFT", f.root.editors[f.parent.ID].Value())
	if result.prepared != nil {
		result.prepared.Abort()
	}
	assert.Zero(t, f.provider.calls.Load())

	f.attach(t)
	assert.Equal(t, "wake up", f.root.editors[f.child.ID].Value())
	assert.Equal(t, "ORIGIN DRAFT", f.root.editors[f.parent.ID].Value())
	assert.Zero(t, f.provider.calls.Load(), "retained draft is not sent on reopen")
	assert.True(t, f.childStopped(t))
}

// Repeated cancels of one target never retain more than the per-target budget.
func TestAttachedStoppedChildRepeatedCancelsStayWithinRetainedBudget(t *testing.T) {
	f := newStoppedAttachFixture(t)
	for range 4 {
		result := f.beginAttach(t)
		f.typeText(t, strings.Repeat("a", 1500))
		_, _ = f.root.update(tea.KeyPressMsg{Code: tea.KeyEscape})
		require.Nil(t, f.root.opening)
		if result.prepared != nil {
			result.prepared.Abort()
		}
	}
	f.attach(t)
	assert.Len(t, f.root.editors[f.child.ID].Value(), openingInputLimit)
	_, held := f.root.retainedOpeningInput[f.child.ID]
	assert.False(t, held, "consumed held text releases its budget")
	assert.Zero(t, f.provider.calls.Load(), "retained input is never dispatched")
	assert.True(t, f.childStopped(t))
}

// Replays addressed to a stale generation or another active session are retained, never sent.
func TestAttachedStoppedChildStaleReplayNeverReachesOtherTarget(t *testing.T) {
	f := newStoppedAttachFixture(t)
	f.root.editors[f.parent.ID].SetValue("ORIGIN DRAFT")
	f.attach(t)
	replay := []tea.Msg{tea.KeyPressMsg{Code: 'w', Text: "w"}, tea.KeyPressMsg{Code: tea.KeyEnter}}

	_, _ = f.root.update(messages.SwitchTabMsg{SessionID: f.parent.ID})
	_, _ = f.root.update(openingInputReplayMsg{id: f.root.openingSeq, target: f.child.ID, title: "child", msgs: replay})
	assert.Equal(t, "ORIGIN DRAFT", f.root.editors[f.parent.ID].Value())
	assert.Contains(t, f.root.editors[f.child.ID].Value(), "w\n")

	_, _ = f.root.update(messages.SwitchTabMsg{SessionID: f.child.ID})
	_, _ = f.root.update(openingInputReplayMsg{id: f.root.openingSeq - 1, target: f.child.ID, title: "child", msgs: replay})
	assert.Equal(t, "ORIGIN DRAFT", f.root.editors[f.parent.ID].Value())
	assert.Zero(t, f.provider.calls.Load(), "stale replay is never dispatched")
}

// A composer submission during opening is restored to its origin draft, never
// dispatched during opening and never replayed into the child.
func TestOpeningOriginRoutedSendStaysWithOrigin(t *testing.T) {
	f := newStoppedAttachFixture(t)
	result := f.beginAttach(t)
	require.NotNil(t, f.root.opening)
	routed := f.root.stampEditorSend(core.CmdHandler(messages.SendMsg{Content: "origin routed"}))()
	require.IsType(t, messages.RoutedMsg{}, routed)
	require.Equal(t, f.parent.ID, routed.(messages.RoutedMsg).SessionID)
	_, _ = f.root.update(routed)
	assert.Equal(t, "origin routed", f.root.editors[f.parent.ID].Value(), "origin send is restored to its origin draft")
	assert.Zero(t, f.provider.calls.Load(), "no dispatch during opening")

	f.start(f.root.finishSubagentOpening(result))
	f.pump(t, func() bool { return f.root.opening == nil })
	assert.Empty(t, f.root.editors[f.child.ID].Value(), "origin send is never replayed into the child")
	assert.Equal(t, "origin routed", f.root.editors[f.parent.ID].Value())
	assert.Zero(t, f.provider.calls.Load(), "origin send is not dispatched to any session")
}
