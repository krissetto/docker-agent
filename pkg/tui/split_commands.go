package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func paneCommandCategory() commands.Category {
	return commands.Category{Name: "Panes", Commands: []commands.Item{{
		ID: "layout.panes", Label: "Panes", SlashCommand: "/panes", Category: "Panes", Immediate: true,
		Description: "Split sessions left/right/up/down, focus or remove a pane, resize dividers, or return to single view (does not close tabs)",
		Execute:     func(string) tea.Cmd { return core.CmdHandler(messages.OpenPanesMsg{}) },
	}}}
}

func paneAction(label, description string, action messages.PaneActionMsg) commands.Item {
	return commands.Item{ID: "pane." + action.Action + "." + action.Source + action.Edge + action.DividerID, Label: label, Description: description, Category: "Panes", Execute: func(string) tea.Cmd { return core.CmdHandler(action) }}
}

func (m *appModel) openPanePicker(items []commands.Item) tea.Cmd {
	m.cancelPaneGesture()
	return core.CmdHandler(dialog.OpenDialogMsg{Model: dialog.NewCommandPaletteDialog([]commands.Category{{Name: "Panes", Commands: items}})})
}

func (m *appModel) openPanes() tea.Cmd {
	if m.leanMode {
		return nil
	}
	if _, _, ok := m.measurePanes(); !ok {
		return notification.ErrorCmd("Panes unavailable: this session does not support split presentation")
	}
	focus := m.paneFocus()
	var items []commands.Item
	for _, edge := range []string{"left", "right", "up", "down"} {
		items = append(items, paneAction("Split "+edge, "Choose an open session to move beside this pane", messages.PaneActionMsg{Action: "sources", Target: focus, Edge: edge}))
	}
	for _, id := range m.paneLayout().Sessions() {
		label := id
		if ss := m.sessionStates[id]; ss != nil && ss.SessionTitle() != "" {
			label = ss.SessionTitle()
		}
		items = append(items, paneAction("Focus "+label, "Shared composer, draft, attachments and sidebar follow this session", messages.PaneActionMsg{Action: "focus", Source: id}))
		if m.panesEnabled() {
			items = append(items, paneAction("Remove pane: "+label, "Hide this pane only; keep the session tab and its work running", messages.PaneActionMsg{Action: "remove", Source: id}))
		}
	}
	items = append(items, paneAction("Single view", "Show only the focused pane; keep all session tabs and work", messages.PaneActionMsg{Action: "single", Source: focus}))
	for i, d := range m.paneGeometry.Dividers {
		items = append(items, paneAction(fmt.Sprintf("Resize divider %d", i+1), "Arrow keys preview; Enter commits; Esc cancels", messages.PaneActionMsg{Action: "resize", DividerID: d.ID}))
	}
	return m.openPanePicker(items)
}

func (m *appModel) handlePaneAction(msg messages.PaneActionMsg) tea.Cmd {
	if m.leanMode || m.supervisor == nil {
		return nil
	}
	switch msg.Action {
	case "sources":
		if !m.paneLayout().Contains(msg.Target) {
			return nil
		}
		tabs, _ := m.supervisor.GetTabs()
		var items []commands.Item
		for _, tab := range tabs {
			if tab.SessionID == msg.Target {
				continue
			}
			items = append(items, paneAction(tab.Title+" · "+tab.SessionID, "Move this canonical session to the "+msg.Edge+"; existing visible source is moved, never duplicated", messages.PaneActionMsg{Action: "split", Source: tab.SessionID, Target: msg.Target, Edge: msg.Edge}))
		}
		if len(items) == 0 {
			return notification.ErrorCmd("Open another session tab before splitting")
		}
		return m.openPanePicker(items)
	case "split":
		if m.supervisor.GetRunner(msg.Source) == nil {
			return nil
		}
		edges := map[string]splitEdge{"left": splitLeft, "right": splitRight, "up": splitTop, "down": splitBottom}
		edge, ok := edges[msg.Edge]
		if !ok {
			return nil
		}
		return m.splitPane(msg.Source, msg.Target, edge)
	case "focus":
		if !m.paneLayout().Contains(msg.Source) {
			return nil
		}
		_, cmd := m.handleSwitchTab(msg.Source)
		return cmd
	case "remove":
		return m.removePane(msg.Source)
	case "single":
		return m.singlePane()
	case "resize":
		if m.paneGeometry.Compact {
			return nil
		}
		for _, d := range m.paneGeometry.Dividers {
			if d.ID != msg.DividerID {
				continue
			}
			m.cancelPaneGesture()
			m.paneGesture = &panePointerTransaction{layout: m.panes, order: m.paneOrder(), bounds: m.paneBounds, geometry: m.paneGeometry, divider: &d, position: dividerPosition(d), preview: d.Rect, active: true, keyboard: true}
			return nil
		}
	}
	return nil
}
