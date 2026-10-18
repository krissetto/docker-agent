package tui

import (
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

// Workspaces retain presentation only. Canonical pages, drafts and runners stay
// in their existing route-keyed owners. Disk restoration remains session-based.
type paneWorkspace struct {
	layout        splitLayout
	sidebar       *chat.SidebarSettings
	anchor, focus string
	restore       splitLayout
}

type paneWorkspaces struct {
	active  *paneWorkspace
	byRoute map[string]*paneWorkspace
	order   []*paneWorkspace
}

func copyPaneSidebar(settings *chat.SidebarSettings) *chat.SidebarSettings {
	if settings == nil {
		return nil
	}
	cloned := *settings
	return &cloned
}

func (m *appModel) savePaneWorkspace(fallback string) {
	w := &m.paneWorkspaces
	if w.byRoute == nil {
		w.byRoute = make(map[string]*paneWorkspace)
		if m.panes.root == nil {
			m.panes = newSplitLayout(fallback)
		}
		if m.panes.root != nil {
			w.active = &paneWorkspace{anchor: fallback, focus: fallback}
			w.order = append(w.order, w.active)
			for _, id := range m.panes.Sessions() {
				w.byRoute[id] = w.active
			}
		}
	}
	if w.active != nil {
		w.active.layout = m.panes
		if m.paneSidebarSettings != nil || w.active.sidebar == nil {
			w.active.sidebar = copyPaneSidebar(m.paneSidebarSettings)
		}
		if w.byRoute[m.paneFocus()] == w.active &&
			(m.pendingActiveTab == "" || w.byRoute[m.pendingActiveTab] != w.active) {
			w.active.focus = m.paneFocus()
		}
	}
}

func (w *paneWorkspaces) detach(id string) {
	if owner := w.byRoute[id]; owner != nil {
		owner.layout = splitLayout{root: splitRemove(splitClone(owner.layout.root), id)}
		delete(w.byRoute, id)
		owner.restore = splitLayout{root: splitRemove(splitClone(owner.restore.root), id)}
		if owner.focus == id {
			owner.focus = ""
		}
		if owner.anchor == id {
			owner.anchor = ""
		}
		w.prune()
	}
}

// Explicit edits move their leaves into the active workspace. Any detached
// companions retain their own remaining tree, including its preferred ratios.
func (m *appModel) commitPaneWorkspace(layout splitLayout) {
	m.syncPaneSidebarSettings()
	m.savePaneWorkspace(m.paneFocus())
	w := &m.paneWorkspaces
	if w.active == nil {
		w.active = &paneWorkspace{anchor: m.paneFocus(), focus: m.paneFocus()}
		w.order = append(w.order, w.active)
	}
	for _, id := range layout.Sessions() {
		if w.byRoute[id] != w.active {
			w.detach(id)
		}
	}
	m.panes = layout
	w.active.layout = layout
	if w.active.restore.root != nil {
		restored := layout
		for _, id := range w.active.restore.Sessions() {
			if w.byRoute[id] == w.active && !restored.Contains(id) {
				restored, _ = restored.Insert(id, layout.Sessions()[0], splitRight)
			}
		}
		w.active.restore = restored
	}
	for _, id := range layout.Sessions() {
		w.byRoute[id] = w.active
	}
	w.prune()
	m.refreshWorkspaceTabs()
	m.viewCacheValid = false
	m.persistPaneWorkspaceState()
}

func (m *appModel) switchPaneWorkspace(old, next string) {
	m.savePaneWorkspace(old)
	w := &m.paneWorkspaces
	if w.active != nil && w.byRoute[next] == w.active {
		if !w.active.layout.Contains(next) && w.active.restore.Contains(next) {
			m.panes = splitLayout{root: splitClone(w.active.restore.root)}
			w.active.layout, w.active.restore = m.panes, splitLayout{}
		}
		m.paneSidebarSettings = copyPaneSidebar(w.active.sidebar)
		w.active.focus = next
		return
	}
	m.cancelPaneGesture()
	owner := w.byRoute[next]
	if owner == nil {
		owner = &paneWorkspace{layout: newSplitLayout(next), anchor: next, focus: next}
		w.order = append(w.order, owner)
		w.byRoute[next] = owner
	}
	w.active = owner
	owner.focus = next
	if !owner.layout.Contains(next) && owner.restore.Contains(next) {
		owner.layout, owner.restore = owner.restore, splitLayout{}
	}
	// A round trip must not revive pointer-based guards from an old visit.
	m.panes = splitLayout{root: splitClone(owner.layout.root)}
	owner.layout = m.panes
	m.paneSidebarSettings = copyPaneSidebar(owner.sidebar)
	m.viewCacheValid = false
}

func (m *appModel) closePaneWorkspaceRoute(id string) {
	m.syncPaneSidebarSettings()
	m.savePaneWorkspace(m.paneFocus())
	w := &m.paneWorkspaces
	w.detach(id)
	if w.active != nil {
		if w.active.layout.root == nil {
			w.active.layout, w.active.restore = w.active.restore, splitLayout{}
		}
		m.panes = w.active.layout
		if m.panes.root == nil {
			w.active = nil
		}
	}
	m.viewCacheValid = false
	m.persistPaneWorkspaceState()
}

// Hosted acquisition can replace a placeholder route with an existing route.
// Preserve the placeholder's workspace, moving the canonical route only once.
func (m *appModel) replacePaneWorkspaceRoute(old, next string) {
	if old == "" || next == "" || old == next {
		return
	}
	m.syncPaneSidebarSettings()
	m.savePaneWorkspace(m.paneFocus())
	w := &m.paneWorkspaces
	owner := w.byRoute[old]
	if owner == nil {
		return
	}
	m.cancelPaneGesture()
	w.detach(next)
	owner.layout, _ = owner.layout.Replace(old, next)
	delete(w.byRoute, old)
	w.byRoute[next] = owner
	if owner.anchor == old {
		owner.anchor = next
	}
	if owner.focus == old {
		owner.focus = next
	}
	owner.restore, _ = owner.restore.Replace(old, next)
	if w.active != nil {
		if w.active.layout.root == nil {
			w.active.layout, w.active.restore = w.active.restore, splitLayout{}
		}
		m.panes = w.active.layout
		if m.panes.root == nil {
			w.active = nil
		}
	}
	m.viewCacheValid = false
}

func (w *paneWorkspaces) members(owner *paneWorkspace) []string {
	var ids []string
	for _, id := range owner.layout.Sessions() {
		if w.byRoute[id] == owner {
			ids = append(ids, id)
		}
	}
	for _, id := range owner.restore.Sessions() {
		if w.byRoute[id] == owner && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

func (w *paneWorkspaces) prune() {
	w.order = slices.DeleteFunc(w.order, func(owner *paneWorkspace) bool {
		ids := w.members(owner)
		if len(ids) == 0 {
			return true
		}
		if !slices.Contains(ids, owner.anchor) {
			owner.anchor = ids[0]
		}
		if !slices.Contains(ids, owner.focus) {
			owner.focus = ids[0]
		}
		return false
	})
}

func (m *appModel) refreshWorkspaceTabs() tea.Cmd {
	if m.tabBar == nil {
		return nil
	}
	w := &m.paneWorkspaces
	infos := make(map[string]messages.TabInfo, len(m.tabInfos))
	for _, tab := range m.tabInfos {
		infos[tab.SessionID] = tab
	}
	var tabs []messages.TabInfo
	active := 0
	for _, owner := range w.order {
		info, exists := infos[owner.anchor]
		if !exists {
			continue
		}
		info.FocusSessionID = owner.focus
		info.PaneCount = len(w.members(owner))
		info.IsActive = owner == w.active
		if info.IsActive {
			active = len(tabs)
		}
		for _, id := range w.members(owner) {
			member := infos[id]
			info.NeedsAttention = info.NeedsAttention || member.NeedsAttention
			info.IsRunning = info.IsRunning || member.IsRunning
			if member.Activity == messages.TabActivityRunning || (info.Activity != messages.TabActivityRunning && member.Activity > info.Activity) {
				info.Activity = member.Activity
			}
		}
		tabs = append(tabs, info)
	}
	return m.tabBar.SetTabs(tabs, active)
}

func (m *appModel) syncPaneWorkspaceTabs() bool {
	m.savePaneWorkspace(m.paneFocus())
	w := &m.paneWorkspaces
	changed := false
	for id := range w.byRoute {
		if !slices.ContainsFunc(m.tabInfos, func(tab messages.TabInfo) bool { return tab.SessionID == id }) {
			w.detach(id)
			changed = true
		}
	}
	for _, tab := range m.tabInfos {
		if w.byRoute[tab.SessionID] == nil {
			owner := &paneWorkspace{layout: newSplitLayout(tab.SessionID), anchor: tab.SessionID, focus: tab.SessionID}
			w.byRoute[tab.SessionID] = owner
			w.order = append(w.order, owner)
			changed = true
		}
	}
	return changed
}

func (m *appModel) workspaceTabFocus(id string) string {
	if owner := m.paneWorkspaces.byRoute[id]; owner != nil && owner.anchor == id {
		return owner.focus
	}
	return id
}

func (m *appModel) hidePaneWorkspace(layout splitLayout) {
	m.syncPaneSidebarSettings()
	m.savePaneWorkspace(m.paneFocus())
	owner := m.paneWorkspaces.active
	if owner.restore.root == nil {
		owner.restore = owner.layout
	}
	owner.layout, m.panes = layout, layout
	m.refreshWorkspaceTabs()
	m.viewCacheValid = false
	m.persistPaneWorkspaceState()
}

func (m *appModel) detachPaneToWorkspace(id string, position int) tea.Cmd {
	w := &m.paneWorkspaces
	owner := w.byRoute[id]
	if owner == nil || len(w.members(owner)) < 2 {
		return nil
	}
	m.syncPaneSidebarSettings()
	m.savePaneWorkspace(m.paneFocus())
	w.detach(id)
	if owner.layout.root == nil {
		owner.layout, owner.restore = owner.restore, splitLayout{}
	}
	detached := &paneWorkspace{layout: newSplitLayout(id), anchor: id, focus: id}
	w.byRoute[id] = detached
	position = max(0, min(position, len(w.order)))
	w.order = slices.Insert(w.order, position, detached)
	m.panes = owner.layout
	m.viewCacheValid = false
	_, cmd := m.handleSwitchTab(id)
	return tea.Batch(cmd, m.refreshWorkspaceTabs())
}

func (m *appModel) workspaceSplitCandidate(current splitLayout, source, target string, edge splitEdge) (splitLayout, bool) {
	owner := m.paneWorkspaces.byRoute[source]
	if owner == nil || owner == m.paneWorkspaces.active {
		return current, false
	}
	sourceLayout := owner.layout
	if owner.restore.root != nil {
		sourceLayout = owner.restore
	}
	return current.InsertLayout(sourceLayout, target, edge)
}

func (m *appModel) tileWorkspace(source, target string, edge splitEdge) tea.Cmd {
	owner := m.paneWorkspaces.byRoute[source]
	next, ok := m.workspaceSplitCandidate(m.paneLayout(), source, target, edge)
	if !ok || owner == nil || !m.supportsPaneCandidate(next, source) {
		return nil
	}
	_, bounds, ok := m.measurePanes()
	if !ok || next.Compute(bounds, owner.focus, paneMinWidth, paneMinHeight).Compact {
		return nil
	}
	if m.chatPages[source] == nil || m.pendingRestores[source] != "" {
		return m.beginPaneHydration(source, source, target, edge, next, bounds)
	}
	focus := owner.focus
	m.capturePaneSidebarSettings()
	m.commitPaneWorkspace(next)
	_, cmd := m.handleSwitchTab(focus)
	return cmd
}
