package tui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// paneCatalogRow is metadata only. RoutingID is set only when this canonical
// session already has a supervised view; SessionID is never a display label.
type paneCatalogRow struct {
	runtime.SessionSummaryEntry

	RoutingID string
}

type paneCatalogRequest struct {
	application *app.App
	owner       string
	generation  uint64
	cancel      context.CancelFunc
	guard       paneChoiceGuard
	action      messages.PaneActionMsg
	selector    string
	picker      dialog.Dialog
}

type paneCatalogRefreshMsg struct{}

type paneCatalogResult struct {
	request *paneCatalogRequest
	entries []runtime.SessionSummaryEntry
	err     error
}

func (m *appModel) cancelPaneCatalog() {
	if m.paneCatalogRequest != nil {
		m.paneCatalogRequest.cancel()
		m.paneCatalogRequest = nil
	}
}

// refreshPaneCatalog performs one metadata-only request off the event loop.
// No session loading, tree materialization, page Init or execution occurs here.
func (m *appModel) refreshPaneCatalog() tea.Cmd {
	m.cancelPaneCatalog()
	if m.application == nil || m.supervisor == nil {
		return nil
	}
	catalog, ok := m.application.SessionRuntime().(runtime.SessionSummaryCatalog)
	if !ok {
		m.paneCatalogError = errors.New("this runtime does not support metadata-only session browsing")
		return notification.InfoCmd(m.paneCatalogError.Error())
	}
	owner := m.paneFocus()
	generation, _ := m.supervisor.RouteGeneration(owner)
	ctx, cancel := context.WithCancel(m.ctx())
	request := &paneCatalogRequest{application: m.application, owner: owner, generation: generation, cancel: cancel, guard: m.capturePaneChoiceGuard()}
	if m.dialogMgr.Open() && m.dialogMgr.Closing() {
		request.picker = m.dialogMgr.TopDialog()
	}
	m.paneCatalogRequest = request
	return func() tea.Msg {
		entries, err := catalog.ListSessionSummaries(ctx, runtime.SessionSummaryOptions{IncludeChildren: true})
		return paneCatalogResult{request: request, entries: entries, err: err}
	}
}

func (m *appModel) finishPaneCatalog(result paneCatalogResult) tea.Cmd {
	request := result.request
	if request == nil || request != m.paneCatalogRequest {
		return nil
	}
	m.paneCatalogRequest = nil
	request.cancel()
	generation, exists := m.supervisor.RouteGeneration(request.owner)
	if !exists || generation != request.generation || m.application != request.application || m.paneFocus() != request.owner || !m.validPaneChoiceGuard(request.guard) || (m.dialogMgr.Open() && (m.dialogMgr.TopDialog() != request.picker || !m.dialogMgr.Closing())) {
		return nil
	}
	m.paneCatalogError = result.err
	if result.err != nil {
		return notification.ErrorCmd("Cannot browse session metadata: " + result.err.Error())
	}
	m.paneCatalog = append(m.paneCatalog[:0], result.entries...)
	m.paneCatalogOwner = request.application
	if request.action.Action != "" {
		if request.selector != "" {
			row, err := m.resolveCatalogSource(request.selector)
			if err != nil {
				return notification.ErrorCmd(err.Error())
			}
			return m.splitCatalogSource(row, request.action.Target, request.action.Edge)
		}
		return m.openCatalogChoices(request.action.Target, request.action.Edge)
	}
	return nil
}

func (m *appModel) paneCatalogRows() []paneCatalogRow {
	var rows []paneCatalogRow
	bySession := make(map[string]int)
	if m.paneCatalogOwner == m.application {
		for _, entry := range m.paneCatalog {
			if entry.SessionID == "" {
				continue
			}
			if _, duplicate := bySession[entry.SessionID]; duplicate {
				continue
			}
			bySession[entry.SessionID] = len(rows)
			rows = append(rows, paneCatalogRow{SessionSummaryEntry: entry})
		}
	}
	if m.supervisor == nil {
		return rows
	}
	tabs, _ := m.supervisor.GetTabs()
	for _, tab := range tabs {
		runner := m.supervisor.GetRunner(tab.SessionID)
		if runner == nil || runner.App == nil || runner.App.Session() == nil {
			continue
		}
		sessionID := runner.App.Session().ID
		if restored := m.pendingRestores[tab.SessionID]; restored != "" {
			sessionID = restored
		}
		name, _ := m.tabAgentIdentity(tab)
		if index, exists := bySession[sessionID]; exists {
			rows[index].RoutingID = tab.SessionID
			continue
		}
		bySession[sessionID] = len(rows)
		rows = append(rows, paneCatalogRow{RoutingID: tab.SessionID, SessionSummaryEntry: runtime.SessionSummaryEntry{
			SessionID: sessionID, Title: tab.Title, AgentName: name, Loaded: m.chatPages[tab.SessionID] != nil, Loadable: true,
		}})
	}
	return rows
}

func paneCatalogSelectable(row paneCatalogRow) bool {
	return row.RoutingID != "" || (row.RouteError == "" && (row.Loadable || row.RequiresConfirmation))
}

func (m *appModel) requestPaneSources(target, edge, selector string) tea.Cmd {
	cmd := m.refreshPaneCatalog()
	if m.paneCatalogRequest == nil {
		// Existing open views remain usable, but unsupported metadata browsing
		// is explicit rather than pretending the catalog is complete.
		if selector != "" {
			if id, err := m.resolvePaneSource(selector); err == nil {
				return m.handlePaneAction(messages.PaneActionMsg{Action: "split", Source: id, Target: target, Edge: edge})
			}
			return cmd
		}
		return tea.Batch(cmd, m.openCatalogChoices(target, edge))
	}
	m.paneCatalogRequest.action = messages.PaneActionMsg{Action: "sources", Target: target, Edge: edge}
	m.paneCatalogRequest.selector = selector
	return cmd
}

func (m *appModel) catalogChoiceLabel(row paneCatalogRow) string {
	if row.RoutingID != "" {
		return m.paneChoiceLabel(row.RoutingID)
	}
	name := row.AgentName
	if name == "" {
		name = "agent"
	}
	return styles.AgentIdentityStyle(name, false).Render(ansi.Truncate(name, 20, "…")) + styles.MutedStyle.Render(" · "+ansi.Truncate(row.Title, 28, "…"))
}

func (m *appModel) openCatalogChoices(target, edge string) tea.Cmd {
	var items []commands.Item
	for _, row := range m.paneCatalogRows() {
		if row.RoutingID == target {
			if _, _, ok := m.paneSplitCandidate(m.paneLayout(), target, target, splitRight); !ok {
				continue
			}
		}
		description := "Open canonical session as pane"
		if row.RequiresConfirmation {
			description = "Restore as paused view; Resume or submit to run"
		}
		if !paneCatalogSelectable(row) {
			description = row.RouteError
		}
		items = append(items, commands.Item{ID: "pane.session." + row.SessionID, Label: m.catalogChoiceLabel(row), Description: description, Execute: func(string) tea.Cmd {
			return core.CmdHandler(paneCatalogChosenMsg{row: row, target: target, edge: edge})
		}})
	}
	if len(items) == 0 {
		return notification.InfoCmd("No accessible sessions found")
	}
	return m.openPaneChoices("Panes · choose session", items)
}

type paneCatalogChosenMsg struct {
	row          paneCatalogRow
	target, edge string
}

func (m *appModel) splitCatalogSource(row paneCatalogRow, target, edge string) tea.Cmd {
	if !paneCatalogSelectable(row) {
		return notification.ErrorCmd("Session unavailable: " + row.RouteError)
	}
	if row.RoutingID != "" {
		return m.handlePaneAction(messages.PaneActionMsg{Action: "split", Source: row.RoutingID, Target: target, Edge: edge})
	}
	edges := map[string]splitEdge{"left": splitLeft, "right": splitRight, "up": splitTop, "down": splitBottom}
	direction, ok := edges[edge]
	if !ok {
		return notification.ErrorCmd("Unknown pane direction")
	}
	return m.beginPaneSource(row.SessionID, target, direction)
}

func (m *appModel) resolveCatalogSource(selector string) (paneCatalogRow, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return paneCatalogRow{}, errors.New("choose a session")
	}
	if strings.HasPrefix(selector, "\"") {
		value, err := strconv.Unquote(selector)
		if err != nil {
			return paneCatalogRow{}, err
		}
		selector = value
	} else if strings.HasPrefix(selector, "'") && strings.HasSuffix(selector, "'") && len(selector) > 1 {
		selector = selector[1 : len(selector)-1]
	}
	var matches []paneCatalogRow
	for _, row := range m.paneCatalogRows() {
		if selector == row.SessionID || selector == row.RoutingID || selector == row.Title || selector == row.AgentName {
			matches = append(matches, row)
		}
	}
	if len(matches) != 1 {
		return paneCatalogRow{}, fmt.Errorf("session %q matches %d candidates; omit it to choose", selector, len(matches))
	}
	return matches[0], nil
}
