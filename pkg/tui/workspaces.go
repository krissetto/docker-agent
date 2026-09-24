package tui

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/messagebar"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/sessionbrowser"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
	"github.com/docker/docker-agent/pkg/userconfig"
)

type workspaceUI struct {
	projects                     map[string]string
	grouping                     bool
	browser                      *sessionbrowser.Model
	visible, fullscreen, compact bool
	browserWidth                 int
	persistenceDisabled          bool
	persisted                    string
	initialError                 error
	rows                         []sessionbrowser.Row
	query                        string
	cursor                       string
	generation                   uint64
	cancel                       context.CancelFunc
	open                         *workspaceOpenIntent
}
type workspaceActionMsg struct {
	action, id string
}

func (m *appModel) initializeWorkspaces() {
	m.workspaceUI.browser = sessionbrowser.New()
	m.workspaceUI.compact = userconfig.Get().WorkspaceCompact
	m.savePaneWorkspace(m.paneFocus())
	if m.tuiStore == nil {
		return
	}
	state, err := m.tuiStore.GetWorkspaces(m.ctx())
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			m.workspaceUI.persistenceDisabled = true
			m.workspaceUI.initialError = err
		}
		return
	}
	if len(state.Workspaces) == 0 {
		return
	}
	m.paneWorkspaces = paneWorkspaces{byRoute: make(map[string]*paneWorkspace)}
	for _, saved := range state.Workspaces {
		w := &paneWorkspace{desiredFocus: saved.FocusedSessionID, id: saved.ID, name: saved.Name, saved: saved.Layout, unavailable: make(map[string]string), sidebar: &chat.SidebarSettings{Collapsed: saved.Sidebar.Collapsed, PreferredWidth: saved.Sidebar.PreferredWidth}}
		m.paneWorkspaces.ordered = append(m.paneWorkspaces.ordered, w)
		if userconfig.Get().GetRestoreTabs() {
			w.layout = m.restoreWorkspaceLayout(saved.Layout, w)
		} else {
			m.markWorkspaceUnmounted(saved.Layout, w)
		}
		for _, route := range w.layout.Sessions() {
			m.paneWorkspaces.byRoute[route] = w
			if m.persistedSessionID(route) == saved.FocusedSessionID {
				w.focus = route
			}
		}
		if w.id == state.ActiveID {
			m.paneWorkspaces.active = w
		}
	}
	if !userconfig.Get().GetRestoreTabs() {
		for _, old := range m.paneWorkspaces.ordered {
			old.layout = splitLayout{}
			old.focus = ""
		}
		m.paneWorkspaces.byRoute = make(map[string]*paneWorkspace)
		for _, old := range m.paneWorkspaces.ordered {
			old.saved = removePersistedLeaf(old.saved, m.persistedSessionID(m.paneFocus()))
			delete(old.unavailable, m.persistedSessionID(m.paneFocus()))
		}
		w := m.paneWorkspaces.newWorkspace()
		w.layout = newSplitLayout(m.paneFocus())
		w.focus = m.paneFocus()
		m.paneWorkspaces.byRoute[w.focus] = w
		m.paneWorkspaces.active = w
	} else if m.paneWorkspaces.active == nil {
		m.paneWorkspaces.active = m.paneWorkspaces.ordered[0]
	}
	m.panes = m.paneWorkspaces.active.layout
	if focus := m.paneWorkspaces.active.focus; focus != "" {
		m.pendingActiveTab = focus
	}
	if m.workspaceEmpty() {
		m.workspaceUI.visible = true
		m.workspaceUI.browser.Focus()
	}
}
func (m *appModel) restoreWorkspaceLayout(node *tuistate.LayoutNode, w *paneWorkspace) splitLayout {
	if node == nil {
		return splitLayout{}
	}
	if node.SessionID != "" {
		tabs, _ := m.supervisor.GetTabs()
		for _, tab := range tabs {
			if m.persistedSessionID(tab.SessionID) == node.SessionID {
				return newSplitLayout(tab.SessionID)
			}
		}
		w.unavailable[node.SessionID] = "Not mounted — Enter to retry"
		return splitLayout{}
	}
	a, b := m.restoreWorkspaceLayout(node.First, w), m.restoreWorkspaceLayout(node.Second, w)
	if a.root == nil {
		return b
	}
	if b.root == nil {
		return a
	}
	axis := splitColumns
	if node.Axis == tuistate.SplitRows {
		axis = splitRows
	}
	return splitLayout{root: &splitNode{axis: axis, ratio: node.Ratio, first: a.root, second: b.root}}
}
func (m *appModel) workspaceEmpty() bool {
	return m.paneWorkspaces.active != nil && m.paneWorkspaces.active.layout.root == nil
}
func (m *appModel) activateWorkspace(id string) tea.Cmd {
	w := m.paneWorkspaces.find(id)
	if w == nil {
		return nil
	}
	m.syncPaneSidebarSettings()
	m.savePaneWorkspace(m.paneFocus())
	m.cancelPaneGesture()
	m.cancelSessionOpening()
	m.cancelWorkspaceOpen()
	m.paneWorkspaces.active = w
	w.revision++
	if m.workspaceUI.cancel != nil {
		m.workspaceUI.cancel()
		m.workspaceUI.cancel = nil
	}
	m.workspaceUI.generation++
	m.panes = splitLayout{root: splitClone(w.layout.root)}
	w.layout = m.panes
	m.paneSidebarSettings = copyPaneSidebar(w.sidebar)
	m.viewCacheValid = false
	if w.focus == "" || !w.layout.Contains(w.focus) {
		w.focus = ""
		ids := w.layout.Sessions()
		if len(ids) > 0 {
			w.focus = ids[0]
		}
	}
	if w.focus != "" {
		_, cmd := m.handleSwitchTab(w.focus)
		return tea.Batch(cmd, m.resizeAll())
	}
	m.workspaceUI.visible = true
	m.editor.Blur()
	cmds := []tea.Cmd{m.workspaceUI.browser.Focus(), m.resizeAll(), m.requestSessionPage(false)}
	if w.saved != nil && len(w.unavailable) > 0 {
		id := w.desiredFocus
		if id == "" {
			id = firstPersistedLeaf(w.saved)
		}
		if id != "" {
			cmds = append(cmds, m.openWorkspaceSession(id, "restore"))
		}
	}
	return tea.Batch(cmds...)
}
func (m *appModel) createWorkspace() tea.Cmd {
	m.savePaneWorkspace(m.paneFocus())
	w := m.paneWorkspaces.newWorkspace()
	return m.activateWorkspace(w.id)
}
func (m *appModel) closeWorkspace(id string) tea.Cmd {
	w := m.paneWorkspaces.find(id)
	if w == nil {
		return nil
	}
	m.savePaneWorkspace(m.paneFocus())
	for _, route := range w.layout.Sessions() {
		delete(m.paneWorkspaces.byRoute, route)
	}
	i := slices.Index(m.paneWorkspaces.ordered, w)
	m.paneWorkspaces.ordered = slices.Delete(m.paneWorkspaces.ordered, i, i+1)
	if w != m.paneWorkspaces.active {
		return nil
	}
	m.paneWorkspaces.active = nil
	if len(m.paneWorkspaces.ordered) == 0 {
		m.paneWorkspaces.newWorkspace()
	}
	return m.activateWorkspace(m.paneWorkspaces.ordered[min(i, len(m.paneWorkspaces.ordered)-1)].id)
}
func (m *appModel) adjacentWorkspace(delta int) *paneWorkspace {
	ws := m.paneWorkspaces.ordered
	if len(ws) == 0 {
		return nil
	}
	i := slices.Index(ws, m.paneWorkspaces.active)
	return ws[(i+delta+len(ws))%len(ws)]
}
func (m *appModel) workspaceKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	if m.dialogMgr.Open() || key.Matches(msg, core.GetKeys().Quit, core.GetKeys().Help) {
		return false, nil
	}
	keys := core.GetKeys()
	switch {
	case key.Matches(msg, keys.SessionsBrowser):
		return true, m.toggleSessionsBrowser()
	case key.Matches(msg, keys.WorkspaceNext):
		if w := m.adjacentWorkspace(1); w != nil {
			return true, m.activateWorkspace(w.id)
		}
	case key.Matches(msg, keys.WorkspacePrevious):
		if w := m.adjacentWorkspace(-1); w != nil {
			return true, m.activateWorkspace(w.id)
		}
	case key.Matches(msg, keys.WorkspaceCreate):
		return true, m.createWorkspace()
	case key.Matches(msg, keys.WorkspaceClose):
		if w := m.paneWorkspaces.active; w != nil {
			return true, m.closeWorkspace(w.id)
		}
	case key.Matches(msg, keys.WorkspaceMove):
		return true, m.movePaneWorkspace()
	}
	for i, binding := range keys.WorkspaceDirect {
		if key.Matches(msg, binding) {
			if i < len(m.paneWorkspaces.ordered) {
				return true, m.activateWorkspace(m.paneWorkspaces.ordered[i].id)
			}
			return true, nil
		}
	}
	if m.workspaceUI.visible && m.workspaceUI.browser != nil && m.workspaceUI.browser.Focused() {
		return true, m.workspaceUI.browser.Update(msg)
	}
	if m.workspaceEmpty() {
		return true, nil
	}
	return false, nil
}
func (m *appModel) movePaneWorkspace() tea.Cmd {
	source := m.paneFocus()
	if m.workspaceEmpty() {
		return nil
	}
	destination := m.adjacentWorkspace(1)
	if destination == m.paneWorkspaces.active {
		destination = m.paneWorkspaces.newWorkspace()
	}
	m.savePaneWorkspace(source)
	m.paneWorkspaces.detach(source)
	if destination.layout.root == nil {
		destination.layout = newSplitLayout(source)
	} else {
		destination.layout, _ = destination.layout.Insert(source, destination.layout.Sessions()[0], splitRight)
	}
	destination.saved = nil
	destination.revision++
	destination.focus = source
	m.paneWorkspaces.byRoute[source] = destination
	// Save the source's detached tree before activation captures its current layout.
	m.panes = m.paneWorkspaces.active.layout
	return m.activateWorkspace(destination.id)
}
func (m *appModel) prepareWorkspaceControls() {
	if m.messageBar == nil {
		return
	}
	var controls []messagebar.Control
	for i, w := range m.paneWorkspaces.ordered {
		label := fmt.Sprint(i + 1)
		if !m.workspaceUI.compact {
			label += " " + w.name
		}
		controls = append(controls, messagebar.Control{ID: w.id, Label: label, Active: w == m.paneWorkspaces.active, Command: core.CmdHandler(workspaceActionMsg{action: "select", id: w.id})})
	}
	controls = append(controls, messagebar.Control{ID: "browser", Label: "Sessions", Command: core.CmdHandler(workspaceActionMsg{action: "browser"})})
	m.messageBar.SetControls(controls)
	m.prepareWorkspaceFallback()
}
func (m *appModel) prepareWorkspaces(msg tea.Msg) tea.Cmd {
	if m.workspaceUI.browser == nil {
		return nil
	}
	if _, tick := msg.(animation.TickMsg); tick {
		return nil
	}
	m.prepareWorkspaceControls()
	m.refreshBrowserRows()
	cmds := []tea.Cmd{m.prepareSessionProjects()}
	if err := m.workspaceUI.initialError; err != nil {
		m.workspaceUI.initialError = nil
		cmds = append(cmds, notification.WarningCmd("Workspace persistence unavailable: "+err.Error()))
	}
	if m.tuiStore != nil && !m.workspaceUI.persistenceDisabled {
		if err := m.persistWorkspaces(); err != nil {
			m.workspaceUI.persistenceDisabled = true
			cmds = append(cmds, notification.WarningCmd("Workspace persistence failed: "+err.Error()))
		}
	}
	return tea.Batch(cmds...)
}
func (m *appModel) workspaceMessage(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case workspaceActionMsg:
		switch msg.action {
		case "select":
			return true, m.activateWorkspace(msg.id)
		case "browser":
			return true, m.toggleSessionsBrowser()
		}
	case sessionbrowser.SelectWorkspace:
		return true, m.activateWorkspace(msg.ID)
	case sessionbrowser.CreateWorkspace:
		return true, m.createWorkspace()
	case sessionbrowser.CloseWorkspace:
		return true, m.closeWorkspace(msg.ID)
	case sessionbrowser.RenameWorkspace:
		if w := m.paneWorkspaces.find(msg.ID); w != nil {
			if name := strings.TrimSpace(msg.Name); name != "" {
				w.name = name
				w.revision++
			}
		}
		return true, nil
	case sessionbrowser.QueryChanged:
		return true, m.debounceSessionQuery(msg.Query)
	case sessionbrowser.LoadMore:
		return true, m.requestSessionPage(true)
	case tea.PasteMsg:
		if m.workspaceUI.visible && m.workspaceUI.browser != nil && m.workspaceUI.browser.Focused() {
			return true, m.workspaceUI.browser.Update(msg)
		}
		if m.workspaceEmpty() {
			return true, nil
		}
	case sessionbrowser.Open:
		return true, m.openWorkspaceSession(msg.SessionID, msg.Mode)
	case sessionbrowser.RetryMissing:
		return true, m.openWorkspaceSession(msg.SessionID, "open")
	case sessionbrowser.RemoveMissing:
		m.removeMissingSession(msg.SessionID)
		return true, nil
	case sessionQueryReady:
		if msg.generation == m.workspaceUI.generation {
			return true, m.requestSessionPage(false)
		}
		return true, nil
	case sessionProjectsMsg:
		m.workspaceUI.grouping = false
		if msg.application == m.application {
			if m.workspaceUI.projects == nil {
				m.workspaceUI.projects = make(map[string]string)
			}
			for path, project := range msg.projects {
				m.workspaceUI.projects[path] = project
			}
			m.refreshBrowserRows()
		}
		return true, nil
	case sessionPageResult:
		return true, m.acceptSessionPage(msg)
	case workspaceOpenDestinationMsg:
		destination := msg.workspace
		if destination == "" {
			destination = m.paneWorkspaces.newWorkspace().id
		}
		return true, tea.Sequence(m.activateWorkspace(destination), m.openWorkspaceSession(msg.sessionID, "open"))
	case workspaceOpenedMsg:
		return true, m.finishWorkspaceOpen(msg)
	}
	return false, nil
}

func (m *appModel) prepareWorkspaceFallback() {
	if m.messageBar == nil {
		return
	}
	fallback := ""
	if !m.workspaceEmpty() && m.paneHeaderHeight() == 0 {
		if m.focusedSessionDormant() || m.sessionState.PauseState() != service.PauseNone {
			fallback = m.paneActivity(m.paneFocus())
		} else if m.leanMode && m.chatPage.IsWorking() {
			fallback = "active"
		}
	}
	m.messageBar.SetFallback(fallback)
}

func (m *appModel) markWorkspaceUnmounted(node *tuistate.LayoutNode, w *paneWorkspace) {
	if node == nil {
		return
	}
	if node.SessionID != "" {
		w.unavailable[node.SessionID] = "Not mounted — Enter to retry"
		return
	}
	m.markWorkspaceUnmounted(node.First, w)
	m.markWorkspaceUnmounted(node.Second, w)
}

func firstPersistedLeaf(node *tuistate.LayoutNode) string {
	if node == nil {
		return ""
	}
	if node.SessionID != "" {
		return node.SessionID
	}
	if id := firstPersistedLeaf(node.First); id != "" {
		return id
	}
	return firstPersistedLeaf(node.Second)
}
