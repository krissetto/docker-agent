package tui

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func (m *appModel) paneCommandCategory() commands.Category {
	return commands.Category{Name: "Panes", Commands: []commands.Item{{
		ID: "layout.panes", Label: "Panes", SlashCommand: "/panes", Category: "Panes", Immediate: true,
		Description:         "Split sessions left/right/up/down, focus or remove a pane, resize dividers, or return to single view (does not close tabs)",
		Execute:             func(arg string) tea.Cmd { return core.CmdHandler(messages.OpenPanesMsg{Arguments: arg}) },
		CompleteArgument:    m.paneArgumentCandidates,
		CompleteArgumentFor: m.paneArgumentCandidatesFor,
	}}}
}

func paneAction(label, description string, action messages.PaneActionMsg) commands.Item {
	return commands.Item{ID: "pane." + action.Action + "." + action.Source + action.Edge + action.DividerID, Label: label, Description: description, Category: "Panes", Execute: func(string) tea.Cmd { return core.CmdHandler(action) }}
}

func (m *appModel) openPanePicker(items []commands.Item) tea.Cmd {
	return m.openPaneChoices("Panes", items)
}

func (m *appModel) openPaneChoices(title string, items []commands.Item) tea.Cmd {
	m.cancelPaneGesture()
	guard := m.capturePaneChoiceGuard()
	var picker dialog.Dialog
	request := &panePickerRequest{}
	guarded := append([]commands.Item(nil), items...)
	for i := range guarded {
		execute := guarded[i].Execute
		guarded[i].Execute = func(arg string) tea.Cmd {
			if m.panePicker != request || m.dialogMgr.TopDialog() != picker {
				return nil
			}
			request.confirmed = true
			return func() tea.Msg { return paneChosenMsg{guard: guard, action: execute(arg)(), request: request} }
		}
	}
	picker = dialog.NewPanesDialog(title, guarded)
	request.dialog = picker
	m.panePicker = request
	return core.CmdHandler(dialog.OpenDialogMsg{Model: picker})
}

func (m *appModel) openPanes() tea.Cmd {
	if m.leanMode {
		return notification.InfoCmd("Panes are available in the full TUI")
	}
	if _, _, ok := m.measurePanes(); !ok {
		return notification.ErrorCmd("Panes unavailable: this session does not support split presentation")
	}
	focus := m.paneFocus()
	var items []commands.Item
	for _, edge := range []string{"left", "right", "up", "down"} {
		items = append(items, paneAction("Split "+edge, "Choose an accessible session to show beside this pane", messages.PaneActionMsg{Action: "sources", Target: focus, Edge: edge}))
	}
	for _, id := range m.paneLayout().Sessions() {
		label := m.paneChoiceLabel(id)
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
	if m.leanMode {
		return notification.InfoCmd("Panes are available in the full TUI")
	}
	if m.supervisor == nil {
		return nil
	}
	switch msg.Action {
	case "sources":
		if !m.paneLayout().Contains(msg.Target) {
			return nil
		}
		return m.requestPaneSources(msg.Target, msg.Edge, "")
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

func (m *appModel) paneChoiceLabel(id string) string {
	name, node := m.tabAgentIdentity(messages.TabInfo{SessionID: id})
	if name == "" {
		name = "agent"
	}
	title := "New session"
	if state := m.sessionStates[id]; state != nil && state.SessionTitle() != "" {
		title = state.SessionTitle()
	} else if m.supervisor != nil {
		tabs, _ := m.supervisor.GetTabs()
		for _, tab := range tabs {
			if tab.SessionID == id && tab.Title != "" {
				title = tab.Title
			}
		}
	}
	label := styles.AgentIdentityStyle(name, false).Render(ansi.Truncate(name, 20, "…"))
	if display := paneDisplayNodeID(node); display != "" {
		label += styles.MutedStyle.Render(" (" + display + ")")
	}
	return label + styles.MutedStyle.Render(" · "+ansi.Truncate(title, 28, "…"))
}

func (m *appModel) resolvePaneSource(selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	if strings.HasPrefix(selector, "\"") {
		decoded, err := strconv.Unquote(selector)
		if err != nil {
			return "", errors.New("invalid quoted session selector")
		}
		selector = decoded
	} else if strings.HasPrefix(selector, "'") {
		if len(selector) < 2 || !strings.HasSuffix(selector, "'") {
			return "", errors.New("invalid quoted session selector")
		}
		selector = selector[1 : len(selector)-1]
	}
	if selector == "" {
		return "", errors.New("choose an open session")
	}
	tabs, _ := m.supervisor.GetTabs()
	var matches []string
	for _, tab := range tabs {
		name, node := m.tabAgentIdentity(tab)
		title := tab.Title
		if state := m.sessionStates[tab.SessionID]; state != nil && state.SessionTitle() != "" {
			title = state.SessionTitle()
		}
		if selector == tab.SessionID || selector == node || selector == name || selector == title {
			matches = append(matches, tab.SessionID)
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no open session matches %q; omit the session to choose", selector)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("session %q is ambiguous; omit it to choose", selector)
	}
	return matches[0], nil
}

func (m *appModel) executePaneArguments(arguments string) tea.Cmd {
	if m.leanMode {
		return notification.InfoCmd("Panes are available in the full TUI")
	}
	arguments = strings.TrimSpace(arguments)
	if arguments == "" {
		return m.openPanes()
	}
	if m.supervisor == nil {
		return notification.ErrorCmd("No open session")
	}
	action, rest := arguments, ""
	if end := strings.IndexAny(arguments, " \t\r\n"); end >= 0 {
		action, rest = arguments[:end], strings.TrimSpace(arguments[end:])
	}
	switch action {
	case "left", "right", "up", "down":
		if rest == "" {
			return m.handlePaneAction(messages.PaneActionMsg{Action: "sources", Target: m.paneFocus(), Edge: action})
		}
		if _, supports := m.application.SessionRuntime().(runtime.SessionSummaryCatalog); supports {
			return m.requestPaneSources(m.paneFocus(), action, rest)
		}
		source, err := m.resolvePaneSource(rest)
		if err != nil {
			return notification.ErrorCmd(err.Error())
		}
		return m.handlePaneAction(messages.PaneActionMsg{Action: "split", Source: source, Target: m.paneFocus(), Edge: action})
	case "next", "prev", "remove", "single":
		if rest != "" {
			return notification.ErrorCmd("Usage: /panes " + action)
		}
		if action == "remove" {
			return m.removePane(m.paneFocus())
		}
		if action == "single" {
			return m.singlePane()
		}
		ids := m.paneLayout().Sessions()
		for i, id := range ids {
			if id == m.paneFocus() {
				offset := 1
				if action == "prev" {
					offset = len(ids) - 1
				}
				_, cmd := m.handleSwitchTab(ids[(i+offset)%len(ids)])
				return cmd
			}
		}
	case "resize":
		if m.paneGeometry.Compact || len(m.paneGeometry.Dividers) == 0 {
			return notification.ErrorCmd("No visible divider to resize")
		}
		if rest == "" && len(m.paneGeometry.Dividers) > 1 {
			var items []commands.Item
			for i, divider := range m.paneGeometry.Dividers {
				items = append(items, paneAction(fmt.Sprintf("Divider %d", i+1), "Arrows preview; Enter commits; Esc cancels", messages.PaneActionMsg{Action: "resize", DividerID: divider.ID}))
			}
			return m.openPaneChoices("Panes · choose divider", items)
		}
		index := 1
		if rest != "" {
			var err error
			index, err = strconv.Atoi(rest)
			if err != nil || index < 1 || index > len(m.paneGeometry.Dividers) {
				return notification.ErrorCmd("Usage: /panes resize [visible divider number]")
			}
		}
		return m.handlePaneAction(messages.PaneActionMsg{Action: "resize", DividerID: m.paneGeometry.Dividers[index-1].ID})
	default:
		return notification.ErrorCmd("Usage: /panes [left|right|up|down [session]|next|prev|remove|single|resize [divider]]")
	}
	return nil
}

func (m *appModel) paneArgumentCandidates() []commands.ArgumentCandidate {
	var candidates []commands.ArgumentCandidate
	for _, action := range []string{"left", "right", "up", "down", "next", "prev", "remove", "single", "resize"} {
		candidates = append(candidates, commands.ArgumentCandidate{Label: action, Description: "Pane " + action})
	}
	return candidates
}

func (m *appModel) paneArgumentCandidatesFor(argument string) []commands.ArgumentCandidate {
	action, query, hasSpace := strings.Cut(strings.TrimLeft(argument, " "), " ")
	if !hasSpace {
		var filtered []commands.ArgumentCandidate
		for _, candidate := range m.paneArgumentCandidates() {
			if strings.HasPrefix(candidate.Label, action) {
				filtered = append(filtered, candidate)
			}
		}
		return filtered
	}
	query = strings.ToLower(strings.TrimSpace(query))
	if action == "resize" {
		var candidates []commands.ArgumentCandidate
		for i := range m.paneGeometry.Dividers {
			label := fmt.Sprintf("Divider %d", i+1)
			if query != "" && !strings.Contains(strings.ToLower(label), query) {
				continue
			}
			candidates = append(candidates, commands.ArgumentCandidate{Label: label, Value: fmt.Sprintf("resize %d", i+1), Description: "Arrows preview; Enter commits; Esc cancels"})
		}
		return candidates
	}
	switch action {
	case "left", "right", "up", "down":
	default:
		return nil
	}
	if m.supervisor == nil {
		return nil
	}
	tabs, _ := m.supervisor.GetTabs()
	var candidates []commands.ArgumentCandidate
	for _, tab := range tabs {
		if tab.SessionID == m.paneFocus() {
			if _, _, ok := m.paneSplitCandidate(m.paneLayout(), tab.SessionID, tab.SessionID, splitRight); !ok {
				continue
			}
		}
		// Prefer a human-readable unique selector; never put a full routing UUID
		// into the composer just to disambiguate otherwise identical choices.
		name, node := m.tabAgentIdentity(tab)
		title := tab.Title
		if state := m.sessionStates[tab.SessionID]; state != nil && state.SessionTitle() != "" {
			title = state.SessionTitle()
		}
		selector := ""
		for _, choice := range []string{title, name, paneDisplayNodeID(node)} {
			if id, err := m.resolvePaneSource(strconv.Quote(choice)); err == nil && id == tab.SessionID {
				selector = choice
				break
			}
		}
		value, description := action, "Identical labels: open the source chooser"
		if selector != "" {
			value, description = action+" "+strconv.Quote(selector), "Move canonical session; never duplicate or close"
		}
		label := ansi.Strip(m.paneChoiceLabel(tab.SessionID))
		if query != "" && !strings.Contains(strings.ToLower(label), strings.Trim(query, "\"'")) {
			continue
		}
		candidates = append(candidates, commands.ArgumentCandidate{Label: label, Value: value, Description: description})
	}
	for _, row := range m.paneCatalogRows() {
		if row.RoutingID != "" || !paneCatalogSelectable(row) {
			continue
		}
		label := ansi.Strip(m.catalogChoiceLabel(row))
		if query != "" && !strings.Contains(strings.ToLower(label), strings.Trim(query, "\"'")) {
			continue
		}
		value := action
		for _, selector := range []string{row.Title, row.AgentName} {
			if resolved, err := m.resolveCatalogSource(strconv.Quote(selector)); err == nil && resolved.SessionID == row.SessionID {
				value = action + " " + strconv.Quote(selector)
				break
			}
		}
		candidates = append(candidates, commands.ArgumentCandidate{Label: label, Value: value, Description: "Restore as paused canonical view"})
	}
	return candidates
}

type paneChoiceGuard struct {
	root        *splitNode
	order       []string
	focus       string
	bounds      splitRect
	generations map[string]uint64
}

type panePickerRequest struct {
	dialog    dialog.Dialog
	confirmed bool
}

type paneChosenMsg struct {
	guard   paneChoiceGuard
	action  tea.Msg
	request *panePickerRequest
}

func (m *appModel) capturePaneChoiceGuard() paneChoiceGuard {
	layout := m.paneLayout()
	m.panes = layout
	_, bounds, _ := m.measurePanes()
	guard := paneChoiceGuard{root: layout.root, order: m.paneOrder(), focus: m.paneFocus(), bounds: bounds, generations: make(map[string]uint64)}
	for _, id := range guard.order {
		guard.generations[id], _ = m.supervisor.RouteGeneration(id)
	}
	return guard
}

func (m *appModel) validPaneChoiceGuard(guard paneChoiceGuard) bool {
	_, bounds, supported := m.measurePanes()
	if !supported || m.panes.root != guard.root || m.paneFocus() != guard.focus || bounds != guard.bounds || !slices.Equal(m.paneOrder(), guard.order) {
		return false
	}
	for id, expected := range guard.generations {
		if generation, exists := m.supervisor.RouteGeneration(id); !exists || generation != expected {
			return false
		}
	}
	return true
}

func (m *appModel) handlePaneChoice(msg paneChosenMsg) tea.Cmd {
	if msg.request != nil && (m.panePicker != msg.request || !msg.request.confirmed) {
		return nil
	}
	if !m.validPaneChoiceGuard(msg.guard) {
		return notification.InfoCmd("Pane choice expired because the layout or session changed; open /panes again")
	}
	switch action := msg.action.(type) {
	case messages.PaneActionMsg:
		return m.handlePaneAction(action)
	case paneCatalogChosenMsg:
		return m.splitCatalogSource(action.row, action.target, action.edge)
	}
	return nil
}
