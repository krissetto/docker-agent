package tui

import (
	"context"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

type closeTabRuntime struct {
	runningSubagents atomic.Bool
}

func newCloseTabRuntime(_ string, runningSubagents bool) *closeTabRuntime {
	r := &closeTabRuntime{}
	r.runningSubagents.Store(runningSubagents)
	return r
}

func (*closeTabRuntime) CurrentAgentInfo(context.Context) runtime.CurrentAgentInfo {
	return runtime.CurrentAgentInfo{}
}

func (*closeTabRuntime) CurrentAgentTools(context.Context) ([]tools.Tool, error) { return nil, nil }

func (*closeTabRuntime) CurrentAgentToolsetStatuses() []tools.ToolsetStatus { return nil }

func (*closeTabRuntime) RestartToolset(context.Context, string) error                         { return nil }
func (*closeTabRuntime) EmitStartupInfo(context.Context, *session.Session, runtime.EventSink) {}
func (*closeTabRuntime) EmitAgentInfo(context.Context, runtime.EventSink)                     {}
func (*closeTabRuntime) ResetStartupInfo()                                                    {}
func (*closeTabRuntime) SessionStore() session.Store                                          { return nil }

func (*closeTabRuntime) PermissionsInfo() *runtime.PermissionsInfo { return nil }

func (*closeTabRuntime) CurrentAgentSkillsToolset() *skillstool.ToolSet { return nil }

func (*closeTabRuntime) CurrentMCPPrompts(context.Context) map[string]tools.PromptInfo { return nil }

func (*closeTabRuntime) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", nil
}

func (*closeTabRuntime) UpdateSessionTitle(context.Context, *session.Session, string) error {
	return nil
}
func (*closeTabRuntime) OnToolsChanged(func(runtime.Event))    {}
func (*closeTabRuntime) OnBackgroundEvent(func(runtime.Event)) {}
func (r *closeTabRuntime) HasRunningSubagents(string) bool     { return r.runningSubagents.Load() }

type openSubagentRuntime struct {
	*closeTabRuntime

	info runtime.SubagentAttachInfo
}

func (r *openSubagentRuntime) SubagentAttachInfo(id subagent.NodeID) (runtime.SubagentAttachInfo, bool) {
	return r.info, id == r.info.NodeID
}

func (r *openSubagentRuntime) SubagentNodeForSession(sessionID string) (subagent.NodeID, bool) {
	return r.info.NodeID, r.info.Session != nil && sessionID == r.info.Session.ID
}

type openSubagentSessions struct{}

func (*openSubagentSessions) CreateSession(context.Context, *session.Session, runtime.SessionBinding) (runtime.SessionHandle, error) {
	return nil, nil
}

func (*openSubagentSessions) SessionByID(string) (runtime.SessionHandle, error) {
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, Operation: "lookup"}
}
func (*openSubagentSessions) DeleteSession(context.Context, string) error { return nil }

var _ runtime.SessionRuntime = (*openSubagentSessions)(nil)

func addCloseTabTestSession(t *testing.T, m *appModel, id string, rt app.Services, cleanup func(), opts ...app.Opt) {
	t.Helper()
	sess := session.New(session.WithID(id))
	a := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, append([]app.Opt{app.WithRuntimeServices(rt)}, opts...)...)
	_, err := m.supervisor.AddSession(t.Context(), a, sess, t.TempDir(), cleanup)
	require.NoError(t, err)
	m.chatPages[id] = &mockChatPage{}
	m.editors[id] = &mockEditor{}
	m.sessionStates[id] = service.NewSessionState(sess)
}

func newCloseTabTestModel(t *testing.T) *appModel {
	t.Helper()
	m, _ := newTestModel(t)
	m.supervisor = supervisor.New(nil)
	m.chatPages = map[string]chat.Page{}
	m.editors = map[string]editor.Editor{}
	m.sessionStates = map[string]*service.SessionState{}
	m.pendingRestores = map[string]string{}
	m.pendingSidebarCollapsed = map[string]bool{}
	m.stashedDialogs = map[string]stashedDialog{}
	return m
}

func TestCloseRootWithRunningSubagentsRequiresConfirmation(t *testing.T) {
	t.Parallel()

	m := newCloseTabTestModel(t)
	addCloseTabTestSession(t, m, "root", newCloseTabRuntime("root", true), nil)
	addCloseTabTestSession(t, m, "other", newCloseTabRuntime("other", false), nil)

	_, cmd := m.handleCloseTab("root")
	require.NotNil(t, cmd)
	open, ok := cmd().(dialog.OpenDialogMsg)
	require.True(t, ok)
	_, _ = open.Model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	view := open.Model.View()
	assert.Contains(t, view, "Subagents")
	assert.Contains(t, view, "stay open")
	assert.NotContains(t, view, "interrupt")
	assert.NotNil(t, m.supervisor.GetRunner("root"), "root stays open until confirmed")
	assert.NotNil(t, m.supervisor.GetRunner("other"))
}

func TestCloseRootWithIdleAttachedViewsRequiresConfirmation(t *testing.T) {
	t.Parallel()

	m := newCloseTabTestModel(t)
	shared := newCloseTabRuntime("shared", false)
	addCloseTabTestSession(t, m, "root", shared, nil)
	info := runtime.SubagentAttachInfo{NodeID: "node1", ParentSessionID: "root", ParentAgent: "root"}
	addCloseTabTestSession(t, m, "child", shared, nil, app.WithSubagentAttach(info))
	addCloseTabTestSession(t, m, "other", newCloseTabRuntime("other", false), nil)
	require.NotNil(t, m.supervisor.SwitchTo("other"))

	_, cmd := m.handleCloseTab("root")
	require.NotNil(t, cmd)
	assert.NotNil(t, m.supervisor.GetRunner("root"))
	_, cmd = m.Update(dialog.CloseRootWithSubagentsConfirmedMsg{SessionID: "root"})
	require.Nil(t, cmd)
	assert.Nil(t, m.supervisor.GetRunner("root"))
	assert.NotNil(t, m.supervisor.GetRunner("child"), "attached idle view stays open")
	assert.NotNil(t, m.supervisor.GetRunner("other"))
}

func TestCloseRootWithRunningSubagentsConfirmedRetainsAttachedTabs(t *testing.T) {
	t.Parallel()

	m := newCloseTabTestModel(t)
	shared := newCloseTabRuntime("shared", true)
	other := newCloseTabRuntime("other", false)
	var cleanupCalls atomic.Int32

	addCloseTabTestSession(t, m, "root", shared, func() { cleanupCalls.Add(1) })
	info := runtime.SubagentAttachInfo{NodeID: "node1", ParentSessionID: "root", ParentAgent: "root"}
	addCloseTabTestSession(t, m, "child", shared, nil, app.WithSubagentAttach(info))
	addCloseTabTestSession(t, m, "other", other, nil)
	require.NotNil(t, m.supervisor.SwitchTo("other"))

	_, cmd := m.Update(dialog.CloseRootWithSubagentsConfirmedMsg{SessionID: "root"})
	require.Nil(t, cmd)

	assert.Nil(t, m.supervisor.GetRunner("root"))
	assert.NotNil(t, m.supervisor.GetRunner("child"), "confirming root close retains attached subagent tabs")
	assert.NotNil(t, m.supervisor.GetRunner("other"))
	require.Zero(t, cleanupCalls.Load(), "detached owner cleanup is retained for supervisor shutdown")
	m.supervisor.Shutdown()
	assert.Equal(t, int32(1), cleanupCalls.Load())
}

func TestOpenSubagentMarksAttachedLifetime(t *testing.T) {
	t.Parallel()

	child := session.New(session.WithID("opened-child"))
	info := runtime.SubagentAttachInfo{NodeID: "node-open", Session: child, Agent: "worker", ParentSessionID: "root", ParentAgent: "root"}
	services := &openSubagentRuntime{closeTabRuntime: newCloseTabRuntime("root", false), info: info}
	childApp := newAttachedSubagentApp(t.Context(), &openSubagentSessions{}, services, info, runtime.SessionBinding{AgentName: info.Agent})
	require.NotNil(t, childApp.AttachedSubagent(), "opened child must retain attached-view lifetime metadata")
	assert.Equal(t, info.NodeID, childApp.AttachedSubagent().NodeID)

	replacement := session.New(session.WithID("replacement"))
	childApp.ReplaceSession(t.Context(), replacement)
	assert.Equal(t, child.ID, childApp.Session().ID, "attached child views cannot detach by replacing their session")
}

func TestCloseAttachedSubagentTabSkipsRunningSubagentConfirmation(t *testing.T) {
	t.Parallel()

	m := newCloseTabTestModel(t)
	shared := newCloseTabRuntime("shared", false)
	info := runtime.SubagentAttachInfo{NodeID: "node1", ParentSessionID: "root", ParentAgent: "root"}
	addCloseTabTestSession(t, m, "child", shared, nil, app.WithSubagentAttach(info))
	addCloseTabTestSession(t, m, "other", newCloseTabRuntime("other", false), nil)
	require.NotNil(t, m.supervisor.SwitchTo("other"))

	_, cmd := m.handleCloseTab("child")
	require.Nil(t, cmd)
	assert.Nil(t, m.supervisor.GetRunner("child"))
	assert.NotNil(t, m.supervisor.GetRunner("other"))
}

func TestCloseRootWithSubagentsDialogYesConfirms(t *testing.T) {
	t.Parallel()

	d := dialog.NewCloseRootWithSubagentsDialog("root")
	_, cmd := d.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	assert.True(t, hasMsg[dialog.CloseDialogMsg](msgs))
	assert.True(t, hasMsg[dialog.CloseRootWithSubagentsConfirmedMsg](msgs))
}

func TestCloseRootWithSubagentsDialogCancelDoesNotConfirm(t *testing.T) {
	t.Parallel()

	d := dialog.NewCloseRootWithSubagentsDialog("root")
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	assert.True(t, hasMsg[dialog.CloseDialogMsg](msgs))
	assert.False(t, hasMsg[dialog.CloseRootWithSubagentsConfirmedMsg](msgs))
}

func TestCloseUnrelatedSharedRuntimeTabDoesNotConfirmOrCloseOtherTree(t *testing.T) {
	t.Parallel()
	m := newCloseTabTestModel(t)
	shared := newCloseTabRuntime("shared", false)
	addCloseTabTestSession(t, m, "unrelated", shared, nil)
	addCloseTabTestSession(t, m, "root", shared, nil)
	addCloseTabTestSession(t, m, "child", shared, nil, app.WithSubagentAttach(runtime.SubagentAttachInfo{NodeID: "child-node", ParentSessionID: "root"}))
	require.NotNil(t, m.supervisor.SwitchTo("root"))
	_, cmd := m.handleCloseTab("unrelated")
	require.Nil(t, cmd)
	assert.Nil(t, m.supervisor.GetRunner("unrelated"))
	assert.NotNil(t, m.supervisor.GetRunner("root"))
	assert.NotNil(t, m.supervisor.GetRunner("child"))
}

func TestRetainedGrandchildCountsWhenIntermediateViewIsClosed(t *testing.T) {
	t.Parallel()
	tree := subagent.NewTree()
	require.NoError(t, tree.AddSubtree([]subagent.Node{
		{ID: subagent.SessionRootID("root"), SessionID: "root", Agent: "root"},
		{ID: "child-node", SessionID: "child", Agent: "worker", Parent: subagent.SessionRootID("root")},
		{ID: "grand-node", SessionID: "grand", Agent: "reviewer", Parent: "child-node"},
	}))
	shared := &topologyRuntime{closeTabRuntime: newCloseTabRuntime("shared", false), tree: tree}
	m := newCloseTabTestModel(t)
	addCloseTabTestSession(t, m, "root", shared, nil)
	addCloseTabTestSession(t, m, "grand", shared, nil, app.WithSubagentAttach(runtime.SubagentAttachInfo{NodeID: "grand-node", ParentSessionID: "child"}))
	require.Equal(t, []string{"grand"}, m.descendantAttachedTabs("root"))
	_, cmd := m.handleCloseTab("root")
	require.NotNil(t, cmd, "live attached descendant still prompts with closed intermediate view")
	require.NotNil(t, m.supervisor.GetRunner("root"))
}
