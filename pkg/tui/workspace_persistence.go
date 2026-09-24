package tui

import (
	"encoding/json"

	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
)

func (m *appModel) persistedWorkspaceNode(node *splitNode) *tuistate.LayoutNode {
	if node == nil {
		return nil
	}
	if node.first == nil {
		return &tuistate.LayoutNode{SessionID: m.persistedSessionID(node.session)}
	}
	axis := tuistate.SplitColumns
	if node.axis == splitRows {
		axis = tuistate.SplitRows
	}
	return &tuistate.LayoutNode{Axis: axis, Ratio: node.ratio, First: m.persistedWorkspaceNode(node.first), Second: m.persistedWorkspaceNode(node.second)}
}
func (m *appModel) workspaceState() tuistate.WorkspaceState {
	m.savePaneWorkspace(m.paneFocus())
	state := tuistate.WorkspaceState{Version: tuistate.WorkspaceStateVersion}
	for _, w := range m.paneWorkspaces.ordered {
		saved := tuistate.Workspace{ID: w.id, Name: w.name, Layout: m.persistedWorkspaceNode(w.layout.root)}
		if w.saved != nil {
			saved.Layout = w.saved
		}
		if w.desiredFocus != "" && w.saved != nil {
			saved.FocusedSessionID = w.desiredFocus
		} else if w.focus != "" && w.layout.Contains(w.focus) {
			saved.FocusedSessionID = m.persistedSessionID(w.focus)
		}
		if w.sidebar != nil {
			saved.Sidebar = tuistate.SidebarState{Collapsed: w.sidebar.Collapsed, PreferredWidth: w.sidebar.PreferredWidth}
		}
		state.Workspaces = append(state.Workspaces, saved)
	}
	if m.paneWorkspaces.active != nil {
		state.ActiveID = m.paneWorkspaces.active.id
	}
	return state
}

// Writes are serialized on the owner loop, like existing tab preferences.
func (m *appModel) persistWorkspaces() error {
	state := m.workspaceState()
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if string(encoded) == m.workspaceUI.persisted {
		return nil
	}
	if err := m.tuiStore.SaveWorkspaces(m.ctx(), state); err != nil {
		return err
	}
	m.workspaceUI.persisted = string(encoded)
	return nil
}
func removePersistedLeaf(node *tuistate.LayoutNode, id string) *tuistate.LayoutNode {
	if node == nil {
		return nil
	}
	if node.SessionID != "" {
		if node.SessionID == id {
			return nil
		}
		return node
	}
	clone := *node
	clone.First = removePersistedLeaf(node.First, id)
	clone.Second = removePersistedLeaf(node.Second, id)
	if clone.First == nil {
		return clone.Second
	}
	if clone.Second == nil {
		return clone.First
	}
	return &clone
}
func (m *appModel) removeMissingSession(id string) {
	if w := m.paneWorkspaces.active; w != nil {
		delete(w.unavailable, id)
		if w.desiredFocus == id {
			w.desiredFocus = ""
		}
		w.saved = removePersistedLeaf(w.saved, id)
		w.revision++
	}
}
