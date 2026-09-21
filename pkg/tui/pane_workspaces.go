package tui

import "github.com/docker/docker-agent/pkg/tui/page/chat"

// Workspaces retain presentation only. Canonical pages, drafts and runners stay
// in their existing route-keyed owners, and no layout is persisted to disk.
type paneWorkspace struct {
	layout  splitLayout
	sidebar *chat.SidebarSettings
}

type paneWorkspaces struct {
	active  *paneWorkspace
	byRoute map[string]*paneWorkspace
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
			w.active = &paneWorkspace{}
			for _, id := range m.panes.Sessions() {
				w.byRoute[id] = w.active
			}
		}
	}
	if w.active != nil {
		w.active.layout = m.panes
		w.active.sidebar = copyPaneSidebar(m.paneSidebarSettings)
	}
}

func (w *paneWorkspaces) detach(id string) {
	if owner := w.byRoute[id]; owner != nil {
		owner.layout = splitLayout{root: splitRemove(splitClone(owner.layout.root), id)}
		delete(w.byRoute, id)
	}
}

// Explicit edits move their leaves into the active workspace. Any detached
// companions retain their own remaining tree, including its preferred ratios.
func (m *appModel) commitPaneWorkspace(layout splitLayout) {
	m.syncPaneSidebarSettings()
	m.savePaneWorkspace(m.paneFocus())
	w := &m.paneWorkspaces
	if w.active == nil {
		w.active = &paneWorkspace{}
	}
	remainder := w.active.layout
	for _, id := range layout.Sessions() {
		remainder = splitLayout{root: splitRemove(splitClone(remainder.root), id)}
		if w.byRoute[id] != w.active {
			w.detach(id)
		}
	}
	if remainder.root != nil {
		companions := &paneWorkspace{layout: remainder, sidebar: copyPaneSidebar(w.active.sidebar)}
		for _, id := range remainder.Sessions() {
			w.byRoute[id] = companions
		}
	}
	m.panes = layout
	w.active.layout = layout
	for _, id := range layout.Sessions() {
		w.byRoute[id] = w.active
	}
	m.viewCacheValid = false
}

func (m *appModel) switchPaneWorkspace(old, next string) {
	m.savePaneWorkspace(old)
	w := &m.paneWorkspaces
	if w.active != nil && w.active.layout.Contains(next) {
		return
	}
	m.cancelPaneGesture()
	owner := w.byRoute[next]
	if owner == nil {
		owner = &paneWorkspace{layout: newSplitLayout(next)}
		w.byRoute[next] = owner
	}
	w.active = owner
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
		m.panes = w.active.layout
		if m.panes.root == nil {
			w.active = nil
		}
	}
	m.viewCacheValid = false
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
	if w.active != nil {
		m.panes = w.active.layout
		if m.panes.root == nil {
			w.active = nil
		}
	}
	m.viewCacheValid = false
}
