package app

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
)

type selectedTemplateHandle struct {
	*titleCaptureSession
}

func (*selectedTemplateHandle) AgentName() string             { return "worker" }
func (*selectedTemplateHandle) Hydrate(context.Context) error { panic("unexpected hydrate") }
func (*selectedTemplateHandle) Metadata() runtime.SessionMetadata {
	panic("unexpected metadata lookup")
}

func (*selectedTemplateHandle) SetModel(context.Context, string) error {
	panic("unexpected model change")
}

type targetSnapshotController struct {
	sessions    []string
	workingDirs []string
}

func (*targetSnapshotController) Enabled() bool            { return true }
func (*targetSnapshotController) AutoInject(*hooks.Config) {}
func (c *targetSnapshotController) UndoLast(_ context.Context, id, cwd string) (int, bool, error) {
	c.sessions = append(c.sessions, id)
	c.workingDirs = append(c.workingDirs, cwd)
	return 2, true, nil
}

func (c *targetSnapshotController) List(id string) []builtins.SnapshotInfo {
	c.sessions = append(c.sessions, id)
	return []builtins.SnapshotInfo{{Files: 2}}
}

func (c *targetSnapshotController) Reset(_ context.Context, id, cwd string, _ int) (int, bool, error) {
	c.sessions = append(c.sessions, id)
	c.workingDirs = append(c.workingDirs, cwd)
	return 2, true, nil
}

func TestResolvedTemplatePreservesConfigurationNotSessionOrInputState(t *testing.T) {
	provider := &blockingTitleProvider{entered: make(chan struct{}), release: make(chan struct{})}
	close(provider.release)
	controller := &targetSnapshotController{}
	registry := &projectionSessions{session: &projectionSession{id: "template"}}
	template := New(t.Context(), registry, session.New(session.WithID("template")), runtime.SessionBinding{AgentName: "root"},
		WithRuntimeServices(&mockRuntime{}), WithReadOnly(), WithExitAfterFirstResponse(),
		WithTitleGenerator(sessiontitle.New(provider)), WithSnapshotController(controller), WithQueuedMessages([]string{"must not replay"}))
	first := "must not send"
	template.firstMessage = &first
	template.firstMessageAttach = "must-not-read"
	template.cancelledRequests["old-request"] = struct{}{}
	template.latestRequestID = "old-request"
	handle := &selectedTemplateHandle{titleCaptureSession: &titleCaptureSession{projectionSession: projectionSession{id: "selected"}, titles: make(chan string, 1)}}
	selected := session.New(session.WithID(handle.ID()), session.WithAgentName("worker"))
	selected.WorkingDir = "/synthetic/selected-workspace"
	committed := runtime.CommittedSessionView{SessionHandle: handle, Info: runtime.PreparedSessionViewInfo{SessionID: selected.ID, RootSessionID: selected.ID, Session: selected, Binding: runtime.SessionBinding{AgentName: "worker", Model: "provider/model"}, WorkingDir: selected.WorkingDir}}
	view, err := NewResolvedFromTemplate(t.Context(), registry, committed, template)
	require.NoError(t, err)
	assert.Equal(t, selected.ID, view.Session().ID)
	assert.Equal(t, "worker", view.SessionHandle().AgentName())
	assert.True(t, view.IsReadOnly())
	assert.True(t, view.ShouldExitAfterFirstResponse())
	assert.Empty(t, view.InitialEventCommands())
	assert.Empty(t, view.queuedMessages)
	assert.Empty(t, view.firstMessageAttach)
	assert.Empty(t, view.cancelledRequests)
	assert.Empty(t, view.latestRequestID)
	assert.True(t, view.SnapshotsEnabled())
	assert.Equal(t, []int{2}, view.ListSnapshots())
	_, err = view.UndoLastSnapshot(t.Context())
	require.NoError(t, err)
	_, err = view.ResetSnapshot(t.Context(), 0)
	require.NoError(t, err)
	assert.Equal(t, []string{selected.ID, selected.ID, selected.ID}, controller.sessions)
	assert.Equal(t, []string{selected.WorkingDir, selected.WorkingDir}, controller.workingDirs)
	view.generateTitle(t.Context(), view.state(), []string{"selected conversation"})
	select {
	case title := <-handle.titles:
		assert.Equal(t, "captured title", title)
	case <-time.After(time.Second):
		t.Fatal("selected view did not retain title generation behavior")
	}
	template.Close()
	require.ErrorIs(t, template.busLifetime().Err(), context.Canceled)
	require.NoError(t, view.busLifetime().Err(), "template close cannot cancel the new observer lifetime")
	assert.Empty(t, template.Session().TitleSnapshot(), "title generation targets committed handle, not template")
	view.Close()
}

func TestResolvedTemplateRejectsForeignRuntimeBeforeConstruction(t *testing.T) {
	registry := &projectionSessions{session: &projectionSession{id: "template"}}
	template := New(t.Context(), registry, nil, runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{}))
	other := &projectionSessions{session: &projectionSession{id: "selected"}}
	view, err := NewResolvedFromTemplate(t.Context(), other, runtime.CommittedSessionView{}, template)
	require.ErrorIs(t, err, &runtime.SessionError{Kind: runtime.SessionErrorWrongSession})
	assert.Nil(t, view)
	view, err = NewResolvedFromTemplate(t.Context(), registry, runtime.CommittedSessionView{}, nil)
	require.ErrorIs(t, err, &runtime.SessionError{Kind: runtime.SessionErrorWrongSession})
	assert.Nil(t, view)
}

func TestResolvedTemplateUsesCommittedAttachNeverTemplateAttach(t *testing.T) {
	registry := &projectionSessions{session: &projectionSession{id: "template"}}
	templateSession := session.New(session.WithID("template"))
	template := New(t.Context(), registry, templateSession, runtime.SessionBinding{AgentName: "root"}, WithRuntimeServices(&mockRuntime{}), WithSubagentAttach(runtime.SubagentAttachInfo{NodeID: "template-node", Session: templateSession, ParentSessionID: "template-parent"}))
	handle := &selectedTemplateHandle{titleCaptureSession: &titleCaptureSession{projectionSession: projectionSession{id: "selected"}, titles: make(chan string, 1)}}
	selected := session.New(session.WithID("selected"), session.WithAgentName("worker"), session.WithParentID("selected-parent"))
	selected.WorkingDir = "/synthetic/selected"
	committed := runtime.CommittedSessionView{SessionHandle: handle, Info: runtime.PreparedSessionViewInfo{SessionID: selected.ID, RootSessionID: "selected-parent", Session: selected, Binding: runtime.SessionBinding{AgentName: "worker", ParentSessionID: selected.ParentID}, WorkingDir: selected.WorkingDir, Attach: &runtime.SubagentAttachInfo{NodeID: "selected-node", Agent: "worker", Session: selected, ParentSessionID: selected.ParentID, ParentAgent: "root"}}}
	view, err := NewResolvedFromTemplate(t.Context(), registry, committed, template)
	require.NoError(t, err)
	defer view.Close()
	attach := view.AttachedSubagent()
	require.NotNil(t, attach)
	assert.Equal(t, "selected-node", string(attach.NodeID))
	assert.Equal(t, selected.ID, attach.Session.ID)
	assert.Equal(t, selected.ParentID, attach.ParentSessionID)
	assert.Equal(t, "worker", attach.Agent)
	assert.Equal(t, "root", attach.ParentAgent)
	selected.SetTitle("mutated after construction")
	assert.NotEqual(t, "mutated after construction", attach.Session.TitleSnapshot())
	committed.Info.Attach = nil
	committed.Info.Session = selected.Clone()
	committed.Info.Session.ParentID = ""
	committed.Info.RootSessionID = selected.ID
	committed.Info.Binding.ParentSessionID = ""
	rootView, err := NewResolvedFromTemplate(t.Context(), registry, committed, template)
	require.NoError(t, err)
	defer rootView.Close()
	assert.Nil(t, rootView.AttachedSubagent(), "a committed root never inherits template child attachment")
}
