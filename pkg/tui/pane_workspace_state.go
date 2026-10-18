package tui

import (
	"context"
	"log/slog"
	"math"
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
)

func (m *appModel) persistPaneWorkspaceState() {
	if m.tuiStore == nil {
		return
	}
	ctx := m.ctx()
	tabs, _, err := m.tuiStore.GetTabs(ctx)
	if err != nil {
		slog.WarnContext(ctx, "Failed to read tabs while persisting pane workspaces", "error", err)
		return
	}
	persisted := make(map[string]bool, len(tabs))
	for _, tab := range tabs {
		persisted[tab.SessionID] = true
	}
	liveTabs, _ := m.supervisor.GetTabs()
	toPersisted := make(map[string]string, len(persisted))
	for _, tab := range liveTabs {
		id := m.persistedSessionID(tab.SessionID)
		if persisted[id] {
			toPersisted[tab.SessionID] = id
		}
	}
	state := tuistate.PaneWorkspaceState{Version: 1}
	if m.paneWorkspaces.active != nil {
		state.ActiveAnchorSessionID = toPersisted[m.paneWorkspaces.active.anchor]
	}
	for _, owner := range m.paneWorkspaces.order {
		layout := persistPaneLayout(owner.layout.root, toPersisted)
		restore := persistPaneLayout(owner.restore.root, toPersisted)
		anchor, focus := toPersisted[owner.anchor], toPersisted[owner.focus]
		if anchor == "" {
			anchor = firstPaneSession(layout, restore)
		}
		if anchor == "" {
			continue
		}
		if focus == "" || !paneLayoutContains(layout, focus) && !paneLayoutContains(restore, focus) {
			focus = anchor
		}
		workspace := tuistate.PaneWorkspace{AnchorSessionID: anchor, FocusSessionID: focus, Layout: layout, RestoreLayout: restore}
		if owner.sidebar != nil {
			workspace.HasSidebar = true
			workspace.SidebarCollapsed = owner.sidebar.Collapsed
			workspace.SidebarPreferredWidth = owner.sidebar.PreferredWidth
		} else if owner == m.paneWorkspaces.active && m.paneSidebarSettings != nil {
			workspace.HasSidebar = true
			workspace.SidebarCollapsed = m.paneSidebarSettings.Collapsed
			workspace.SidebarPreferredWidth = m.paneSidebarSettings.PreferredWidth
		}
		state.Workspaces = append(state.Workspaces, workspace)
	}
	if err := m.tuiStore.SetPaneWorkspaceState(ctx, state); err != nil {
		slog.WarnContext(ctx, "Failed to persist pane workspaces", "error", err)
	}
}

func persistPaneLayout(node *splitNode, sessionIDs map[string]string) *tuistate.PaneLayoutNode {
	if node == nil {
		return nil
	}
	if node.first == nil {
		id := sessionIDs[node.session]
		if id == "" {
			return nil
		}
		return &tuistate.PaneLayoutNode{SessionID: id}
	}
	first := persistPaneLayout(node.first, sessionIDs)
	second := persistPaneLayout(node.second, sessionIDs)
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}
	return &tuistate.PaneLayoutNode{Axis: uint8(node.axis), Ratio: node.ratio, First: first, Second: second}
}

func loadPaneLayout(node *tuistate.PaneLayoutNode, routes map[string]string, used map[string]bool) (*splitNode, bool) {
	if node == nil {
		return nil, true
	}
	if node.SessionID != "" {
		if node.First != nil || node.Second != nil {
			return nil, false
		}
		route := routes[node.SessionID]
		if route == "" {
			return nil, true
		}
		if used[route] {
			return nil, false
		}
		used[route] = true
		return &splitNode{session: route}, true
	}
	if node.First == nil || node.Second == nil || node.Axis > uint8(splitRows) || math.IsNaN(node.Ratio) || math.IsInf(node.Ratio, 0) || node.Ratio <= 0 || node.Ratio >= 1 {
		return nil, false
	}
	first, firstOK := loadPaneLayout(node.First, routes, used)
	second, secondOK := loadPaneLayout(node.Second, routes, used)
	if !firstOK || !secondOK {
		return nil, false
	}
	if first == nil {
		return second, true
	}
	if second == nil {
		return first, true
	}
	return &splitNode{axis: splitAxis(node.Axis), ratio: node.Ratio, first: first, second: second}, true
}

func paneLayoutContains(node *tuistate.PaneLayoutNode, id string) bool {
	if node == nil {
		return false
	}
	return node.SessionID == id || paneLayoutContains(node.First, id) || paneLayoutContains(node.Second, id)
}

func firstPaneSession(layouts ...*tuistate.PaneLayoutNode) string {
	for _, layout := range layouts {
		if layout == nil {
			continue
		}
		if layout.SessionID != "" {
			return layout.SessionID
		}
		if id := firstPaneSession(layout.First, layout.Second); id != "" {
			return id
		}
	}
	return ""
}

func (m *appModel) restorePaneWorkspaceState(ctx context.Context, store *tuistate.Store) {
	state, err := store.GetPaneWorkspaceState(ctx)
	if err != nil {
		slog.WarnContext(ctx, "Failed to read saved pane workspaces", "error", err)
		return
	}
	if state.Version > 1 {
		slog.WarnContext(ctx, "Ignoring pane workspace state from a newer version", "version", state.Version)
		return
	}
	tabs, _ := m.supervisor.GetTabs()
	routes := make(map[string]string, len(tabs))
	for _, tab := range tabs {
		persisted := m.persistedSessionID(tab.SessionID)
		routes[persisted] = tab.SessionID
	}
	workspaces := paneWorkspaces{byRoute: make(map[string]*paneWorkspace)}
	used := make(map[string]bool)
	for _, saved := range state.Workspaces {
		layoutRoutes, restoreRoutes := make(map[string]bool), make(map[string]bool)
		layout, ok := loadPaneLayout(saved.Layout, routes, layoutRoutes)
		if !ok {
			continue
		}
		restore, ok := loadPaneLayout(saved.RestoreLayout, routes, restoreRoutes)
		if !ok {
			continue
		}
		layoutMembers := splitLayout{root: layout}.Sessions()
		restoreMembers := splitLayout{root: restore}.Sessions()
		focus := routes[saved.FocusSessionID]
		if slices.ContainsFunc(restoreMembers, func(id string) bool { return slices.Contains(layoutMembers, id) && id != focus }) {
			continue
		}
		members := slices.Clone(layoutMembers)
		for _, id := range restoreMembers {
			if !slices.Contains(members, id) {
				members = append(members, id)
			}
		}
		if len(members) == 0 || slices.ContainsFunc(members, func(id string) bool { return used[id] }) {
			continue
		}
		for _, id := range members {
			used[id] = true
		}
		anchor := routes[saved.AnchorSessionID]
		if !slices.Contains(members, anchor) {
			anchor = members[0]
		}
		if !slices.Contains(members, focus) {
			focus = anchor
		}
		owner := &paneWorkspace{layout: splitLayout{root: layout}, restore: splitLayout{root: restore}, anchor: anchor, focus: focus}
		if saved.HasSidebar {
			owner.sidebar = &chat.SidebarSettings{Collapsed: saved.SidebarCollapsed, PreferredWidth: saved.SidebarPreferredWidth}
		}
		for _, id := range members {
			workspaces.byRoute[id] = owner
		}
		workspaces.order = append(workspaces.order, owner)
	}
	for _, tab := range tabs {
		if workspaces.byRoute[tab.SessionID] != nil {
			continue
		}
		owner := &paneWorkspace{layout: newSplitLayout(tab.SessionID), anchor: tab.SessionID, focus: tab.SessionID}
		workspaces.byRoute[tab.SessionID] = owner
		workspaces.order = append(workspaces.order, owner)
	}
	if len(workspaces.order) == 0 {
		return
	}
	activeRoute := m.pendingActiveTab
	if activeRoute == "" {
		activeRoute = m.supervisor.ActiveID()
	}
	workspaces.active = workspaces.byRoute[activeRoute]
	if workspaces.active == nil && state.ActiveAnchorSessionID != "" {
		workspaces.active = workspaces.byRoute[routes[state.ActiveAnchorSessionID]]
	}
	if workspaces.active == nil {
		workspaces.active = workspaces.order[0]
	}
	if workspaces.byRoute[activeRoute] == workspaces.active {
		workspaces.active.focus = activeRoute
	}
	m.paneWorkspaces = workspaces
	m.panes = splitLayout{root: splitClone(workspaces.active.layout.root)}
	if m.panes.root == nil {
		m.panes = newSplitLayout(activeRoute)
	}
	m.paneSidebarSettings = copyPaneSidebar(workspaces.active.sidebar)
	if m.paneSidebarSettings == nil && workspaces.active.restore.root != nil {
		settings := chat.SidebarSettings{}
		if m.chatPage != nil {
			settings = m.chatPage.GetSidebarSettings()
		}
		m.paneSidebarSettings = &settings
	}
	m.viewCacheValid = false
}

func (m *appModel) ensurePaneWorkspaceComponents() tea.Cmd {
	owner := m.paneWorkspaces.active
	if owner == nil {
		return nil
	}
	var commands []tea.Cmd
	for _, id := range m.paneWorkspaces.members(owner) {
		if m.chatPages[id] != nil || m.pendingRestores[id] != "" {
			continue
		}
		runner := m.supervisor.GetRunner(id)
		if runner == nil || runner.App == nil || runner.App.Session() == nil {
			continue
		}
		if runner.App.Session().ID != m.persistedSessionID(id) {
			continue
		}
		m.createSessionComponents(id, runner.App, runner.App.Session())
		if collapsed, ok := m.pendingSidebarCollapsed[id]; ok {
			m.chatPages[id].SetSidebarSettings(chat.SidebarSettings{Collapsed: collapsed})
			delete(m.pendingSidebarCollapsed, id)
		}
		page, editor := m.chatPages[id], m.editors[id]
		commands = append(commands, page.Init(), chat.WatchGitBranch(page), editor.Init())
	}
	return tea.Batch(commands...)
}
