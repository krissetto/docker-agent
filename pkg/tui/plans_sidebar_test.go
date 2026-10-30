package tui

import (
	"errors"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

func TestPlanSidebar_DisabledDoesNotLoad(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		show, lean, hidden bool
	}{
		{name: "default"},
		{name: "lean", show: true, lean: true},
		{name: "hidden", show: true, hidden: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, _ := newPlansTestModel(t)
			m.layoutSettings.ShowPlans, m.leanMode, m.hideSidebar = tc.show, tc.lean, tc.hidden
			assert.Nil(t, m.refreshPlanSidebarCmd())
			assert.False(t, m.planRefreshInFlight)
			_, cmd := m.handleEditSidebarPlan(messages.EditSidebarPlanMsg{Ref: plans.SharedRef("p"), ExpectedVersion: 1})
			assert.Nil(t, cmd)
		})
	}
}

func TestPlanSidebar_EditIntentFromAnotherTabIsIgnored(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	m.layoutSettings.ShowPlans = true
	p := mustCreatePlan(t, svc, "release", "content")
	m.supervisor = supervisor.New(nil)
	activeID := addPlanTestSession(t, m.supervisor)
	otherID := addPlanTestSession(t, m.supervisor)
	require.Equal(t, activeID, m.supervisor.ActiveID())
	_, cmd := m.Update(messages.EditSidebarPlanMsg{
		TabID: otherID, Ref: plans.SharedRef(p.Name), ExpectedVersion: *p.Version,
	})
	_, started := findPlanEditReady(collectMsgs(cmd))
	assert.False(t, started, "a click routed to a background tab starts no edit")
	assert.False(t, m.sidebarPlanEditInFlight)
}

func TestPlanSidebar_RefreshWithoutDialogs(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	m.layoutSettings.ShowPlans = true
	p := mustCreatePlan(t, svc, "release", "first")
	cmd := m.refreshPlanSidebarCmd()
	assert.True(t, m.planSidebarData.Loading)
	drainPlanFlow(t, m, cmd)
	assert.False(t, m.planSidebarData.Loading)
	require.Len(t, m.planSidebarData.Result.Plans, 1)
	assert.Equal(t, p.Name, m.planSidebarData.Result.Plans[0].Name)
	assert.False(t, m.planDialogOpen(), "loading a sidebar must not open the browser")

	_, err := svc.SetStatus(t.Context(), plans.SetStatusRequest{Ref: plans.SharedRef(p.Name), Status: "awaiting-special-review", ExpectedVersion: p.Version})
	require.NoError(t, err)
	runPlanFlow(t, m, runtime.PlanChanged("shared", p.Name, "status", 2, ""))
	assert.Equal(t, "awaiting-special-review", m.planSidebarData.Result.Plans[0].Status)
}

func TestPlanSidebar_RefreshFailureRetainsRowsAndRecovers(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	m.layoutSettings.ShowPlans = true
	mustCreatePlan(t, svc, "release", "first")
	drainPlanFlow(t, m, m.refreshPlanSidebarCmd())
	_, cmd := m.handlePlanRefreshed(planRefreshedMsg{listErr: errors.New("offline")})
	assert.Empty(t, notificationTexts(collectMsgs(cmd)), "background errors live in the section, not repeated toasts")
	require.Len(t, m.planSidebarData.Result.Plans, 1)
	require.Error(t, m.planSidebarData.Err)
	_, cmd = m.handlePlanRefreshed(planRefreshedMsg{listErr: errors.New("offline"), notifyWarnings: true})
	assert.NotEmpty(t, notificationTexts(collectMsgs(cmd)), "explicit refresh failures notify")
	drainPlanFlow(t, m, m.refreshPlanSidebarCmd())
	assert.NoError(t, m.planSidebarData.Err)
}

func TestPlanSidebar_EditOpensWithoutBrowserAndDeduplicates(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	m.layoutSettings.ShowPlans = true
	p := mustCreatePlan(t, svc, "release", "draft content")
	blocking := newBlockingReadPlansService(svc)
	WithPlansService(blocking)(m)
	msg := messages.EditSidebarPlanMsg{Ref: plans.SharedRef(p.Name), ExpectedVersion: *p.Version}
	_, cmd := m.Update(msg)
	require.NotNil(t, cmd)
	assert.Zero(t, blocking.readsStarted.Load())
	_, duplicate := m.Update(msg)
	assert.Nil(t, duplicate)
	close(blocking.release)
	ready, ok := findPlanEditReady(collectMsgs(cmd))
	require.True(t, ok)
	require.NoError(t, ready.err)
	require.NoError(t, ready.draftErr)
	t.Cleanup(func() { _ = os.Remove(ready.draftPath) })
	content, err := os.ReadFile(ready.draftPath)
	require.NoError(t, err)
	assert.Equal(t, "draft content", string(content))
	_, editorCmd := m.Update(ready)
	require.NotNil(t, editorCmd, "a valid sidebar request must launch the existing editor")
	assert.Empty(t, notificationTexts(collectMsgs(editorCmd)))
	assert.False(t, m.planDialogOpen())
	_, duplicate = m.Update(msg)
	assert.Nil(t, duplicate, "keep duplicate guard until editor closes")
	runPlanFlow(t, m, planEditorClosedMsg{ref: ready.ref, expectedVersion: ready.expectedVersion, path: ready.draftPath, sidebarRequest: ready.sidebarRequest})
	assert.False(t, m.sidebarPlanEditInFlight)
}

func TestPlanSidebar_CancelledEditDoesNotTakeOverTerminal(t *testing.T) {
	t.Parallel()
	for _, cause := range []string{"escape", "disabled", "modal", "generation"} {
		t.Run(cause, func(t *testing.T) {
			t.Parallel()
			m, svc := newPlansTestModel(t)
			m.layoutSettings.ShowPlans = true
			p := mustCreatePlan(t, svc, "release", "content")
			_, cmd := m.Update(messages.EditSidebarPlanMsg{Ref: plans.SharedRef(p.Name), ExpectedVersion: *p.Version})
			ready, ok := findPlanEditReady(collectMsgs(cmd))
			require.True(t, ok)
			require.NotEmpty(t, ready.draftPath)
			t.Cleanup(func() { _ = os.Remove(ready.draftPath) })
			switch cause {
			case "escape":
				m.handleKeyPress(tea.KeyPressMsg{Code: tea.KeyEscape})
			case "disabled":
				m.layoutSettings.ShowPlans = false
				m.cancelSidebarPlanEdit()
			case "modal":
				m.Update(dialog.OpenDialogMsg{Model: dialog.NewPlanBrowserDialog(plans.ListResult{})})
			case "generation":
				m.cancelSidebarPlanEdit()
			}
			_, cmd = m.Update(ready)
			assert.Nil(t, cmd)
			_, err := os.Stat(ready.draftPath)
			assert.True(t, os.IsNotExist(err), "an unused seeded draft must be removed")
		})
	}
}

func TestPlanSidebar_StaleRevisionAndMissingPlan(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	m.layoutSettings.ShowPlans = true
	p := mustCreatePlan(t, svc, "release", "v1")
	_, err := svc.Update(t.Context(), plans.UpdateRequest{Ref: plans.SharedRef(p.Name), ExpectedVersion: p.Version, Content: "v2"})
	require.NoError(t, err)
	msgs := runPlanFlow(t, m, messages.EditSidebarPlanMsg{Ref: plans.SharedRef(p.Name), ExpectedVersion: *p.Version})
	assert.Contains(t, strings.Join(notificationTexts(msgs), "\n"), "click the plan again")
	require.Len(t, m.planSidebarData.Result.Plans, 1)
	assert.Equal(t, 2, *m.planSidebarData.Result.Plans[0].Version)
	assert.False(t, m.sidebarPlanEditInFlight)
	msgs = runPlanFlow(t, m, messages.EditSidebarPlanMsg{Ref: plans.SharedRef("deleted"), ExpectedVersion: 1})
	assert.Contains(t, strings.Join(notificationTexts(msgs), "\n"), "deleted")
	assert.False(t, m.sidebarPlanEditInFlight)
}

func TestPlanSidebar_DisabledDuringRefreshDropsResult(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	m.layoutSettings.ShowPlans = true
	mustCreatePlan(t, svc, "release", "content")
	cmd := m.refreshPlanSidebarCmd()
	assert.Nil(t, m.refreshPlanSidebarCmd(), "coalesce repeated requests")
	m.layoutSettings.ShowPlans = false
	msgs := drainPlanFlow(t, m, cmd)
	assert.Empty(t, msgs)
	assert.False(t, m.planRefreshInFlight)
	assert.False(t, m.planRefreshQueued)
	assert.False(t, m.planSidebarData.Loading)
	assert.Empty(t, m.planSidebarData.Result.Plans)
}

// addPlanTestSession registers a bare session with the supervisor and returns
// its tab ID.
func addPlanTestSession(t *testing.T, sv *supervisor.Supervisor) string {
	t.Helper()
	id, err := sv.AddSession(t.Context(), nil, session.New(), "", nil)
	require.NoError(t, err)
	return id
}

type planSidebarTestPage struct {
	mockChatPage

	data messages.PlanSidebarDataMsg
}

func (p *planSidebarTestPage) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if data, ok := msg.(messages.PlanSidebarDataMsg); ok {
		p.data = data
	}
	return p, nil
}

func TestPlanSidebar_BackgroundEventsUpdateEveryPage(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	m.layoutSettings.ShowPlans = true
	p := mustCreatePlan(t, svc, "release", "content")
	sv := supervisor.New(nil)
	activeID := addPlanTestSession(t, sv)
	backgroundID := addPlanTestSession(t, sv)
	m.supervisor = sv
	active, background := &planSidebarTestPage{}, &planSidebarTestPage{}
	m.chatPages[activeID], m.chatPages[backgroundID] = active, background
	m.chatPage = active
	runPlanFlow(t, m, messages.RoutedMsg{
		SessionID: backgroundID,
		Inner:     runtime.PlanChanged("shared", p.Name, "write", *p.Version, ""),
	})
	for _, page := range []*planSidebarTestPage{active, background} {
		require.Len(t, page.data.Result.Plans, 1)
		assert.Equal(t, p.Name, page.data.Result.Plans[0].Name)
		assert.False(t, page.data.Loading)
	}
	assert.Same(t, active, m.chatPage)
	assert.False(t, m.planDialogOpen())
}

func TestPlanSidebar_CancelledBrowserEditStaysCancelled(t *testing.T) {
	t.Parallel()
	m, svc := newPlansTestModel(t)
	m.layoutSettings.ShowPlans = true
	p := mustCreatePlan(t, svc, "release", "content")
	openPlanBrowser(t, m, plans.ListResult{})
	_, cmd := m.Update(messages.EditPlanMsg{Ref: plans.SharedRef(p.Name), ExpectedVersion: *p.Version})
	ready, ok := findPlanEditReady(collectMsgs(cmd))
	require.True(t, ok)
	require.Zero(t, ready.sidebarRequest)
	t.Cleanup(func() { _ = os.Remove(ready.draftPath) })
	m.Update(dialog.CloseDialogMsg{})
	_, cmd = m.Update(ready)
	assert.Nil(t, cmd, "sidebar visibility must not revive an edit requested by the closed browser")
	_, err := os.Stat(ready.draftPath)
	assert.True(t, os.IsNotExist(err))
	_, cmd = m.Update(planDetailLoadedMsg{ref: plans.SharedRef(p.Name), plan: p})
	assert.Nil(t, cmd, "sidebar visibility must not revive a detail requested by the closed browser")
}

// findPlanEditReady extracts the edit preparation result from a command batch;
// the app wraps the read in batches alongside reconciliation commands.
func findPlanEditReady(msgs []tea.Msg) (planEditReadyMsg, bool) {
	for _, msg := range msgs {
		if ready, ok := msg.(planEditReadyMsg); ok {
			return ready, true
		}
	}
	return planEditReadyMsg{}, false
}
