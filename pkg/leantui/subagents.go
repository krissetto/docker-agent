package leantui

import (
	"context"

	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/subagent"
)

func (m *model) openSubagentPicker() {
	snapshot := m.subagentSnapshot
	if snapshot == nil {
		snapshot = m.app.Session().GetSubagentTree()
	}
	if snapshot == nil {
		snapshot = &subagent.Snapshot{}
	}
	var attached subagent.NodeID
	if info := m.app.AttachedSubagent(); info != nil {
		attached = info.NodeID
	}
	m.screen.Autocomplete.Dismiss()
	m.screen.Subagents = ui.NewSubagentPicker(*snapshot, m.app.Session().ID, attached)
}

func (m *model) handleSubagentPickerKey(ctx context.Context, key ui.Key) {
	switch key.Typ {
	case ui.KeyEsc:
		m.screen.Subagents = nil
	case ui.KeyUp, ui.KeyDown, ui.KeyLeft, ui.KeyRight, ui.KeyHome, ui.KeyEnd:
		m.screen.Subagents.Navigate(key.Typ)
	case ui.KeyEnter:
		node, ok := m.screen.Subagents.Current()
		if !ok {
			return
		}
		m.screen.Subagents = nil
		if node.SessionID == m.app.Session().ID {
			return
		}
		// The tree root is context too: return to an already-open ancestor
		// without asking the runtime to treat it as a child attachment.
		if m.viewers != nil && node.SessionID != "" {
			for application, target := range m.viewers.views {
				if application.Session().ID == node.SessionID {
					m.focusViewer(target, true)
					return
				}
			}
		}
		m.attachSubagentViewer(ctx, string(node.ID))
	}
}
