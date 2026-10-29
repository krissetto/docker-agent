package tui

import (
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
)

type workspaceCloseRoute struct {
	id         string
	generation uint64
}

type workspaceCloseRequest struct {
	owner    *paneWorkspace
	routes   []workspaceCloseRoute
	consumed bool
}

type workspaceCloseConfirmedMsg struct{ request *workspaceCloseRequest }

func (m *appModel) handleCloseWorkspace(sessionID string) (tea.Model, tea.Cmd) {
	if cmd, ok := m.prepareReviewInputTransition(); !ok {
		return m, cmd
	}
	if m.dialogMgr.Open() || m.dialogMgr.Closing() {
		return m, nil
	}
	owner := m.paneWorkspaces.byRoute[sessionID]
	if owner == nil {
		return m, nil
	}
	ids := m.paneWorkspaces.members(owner)
	if len(ids) <= 1 {
		return m.handleCloseTab(sessionID)
	}
	request := &workspaceCloseRequest{owner: owner}
	for _, id := range ids {
		generation, ok := m.supervisor.RouteGeneration(id)
		if !ok {
			return m, nil
		}
		request.routes = append(request.routes, workspaceCloseRoute{id: id, generation: generation})
	}
	return m.forwardDialog(dialog.OpenDialogMsg{Model: dialog.NewCloseWorkspaceDialog(len(ids), core.CmdHandler(workspaceCloseConfirmedMsg{request: request}))})
}

func (m *appModel) confirmCloseWorkspace(request *workspaceCloseRequest) (tea.Model, tea.Cmd) {
	if cmd, ok := m.prepareReviewInputTransition(); !ok {
		return m, cmd
	}
	if request == nil || request.consumed || m.contextClosed {
		return m, nil
	}
	request.consumed = true
	ids := m.paneWorkspaces.members(request.owner)
	valid := len(ids) == len(request.routes)
	for _, route := range request.routes {
		generation, ok := m.supervisor.RouteGeneration(route.id)
		valid = valid && ok && generation == route.generation && m.paneWorkspaces.byRoute[route.id] == request.owner && slices.Contains(ids, route.id)
	}
	if !valid {
		return m, notification.InfoCmd("The workspace changed; no panes were closed.")
	}
	m.cancelSessionOpening()
	// Never partially detach the last workspace or bypass the global exit guard.
	if len(ids) == m.supervisor.Count() {
		return m.forwardDialog(dialog.OpenDialogMsg{Model: dialog.NewExitConfirmationDialog()})
	}

	wasActive := request.owner == m.paneWorkspaces.active
	nextFocus := ""
	if wasActive {
		index := slices.Index(m.paneWorkspaces.order, request.owner)
		next := max(0, index-1)
		if next == index {
			next++
		}
		nextFocus = m.paneWorkspaces.order[next].focus
	}
	// Retain every runtime owner before detaching any root or descendant view.
	for _, id := range ids {
		m.supervisor.RetainCleanupUntilShutdown(id)
	}
	if index := slices.Index(ids, m.supervisor.ActiveID()); index >= 0 {
		activeID := ids[index]
		ids = append(slices.Delete(ids, index, index+1), activeID)
	}
	var cmds []tea.Cmd
	for _, id := range ids {
		_, cmd := m.closeTabView(id, false)
		cmds = append(cmds, cmd)
	}
	tabs, active := m.supervisor.GetTabs()
	cmds = append(cmds, m.setTabs(tabs, active))
	if wasActive {
		_, cmd := m.handleSwitchTab(nextFocus)
		cmds = append(cmds, cmd)
	} else {
		cmds = append(cmds, m.resizeAll())
	}
	return m, tea.Batch(cmds...)
}
