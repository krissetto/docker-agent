package tui

import (
	"context"
	"maps"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/subagentview"
)

type panelTitlesMsg struct {
	data             *panelSessionData
	application      *app.App
	owner, sessionID string
	generation       uint64
	dialog           dialog.Dialog
	titles           map[string]string
}

func (m *appModel) panelTitles(nodes []subagent.NodeSnapshot) map[string]string {
	titles := map[string]string{}
	if m.paneCatalogOwner == m.application {
		for _, entry := range m.paneCatalog {
			titles[entry.SessionID] = entry.Title
		}
	}
	result := map[string]string{}
	for _, row := range subagentview.Rows(nodes, nil) {
		id := row.Node.SessionID
		result[id] = titles[id]
		if m.supervisor != nil {
			if runner := m.supervisor.FindBySession(id); runner != nil && runner.App.Session() != nil {
				result[id] = runner.App.Session().TitleSnapshot()
			}
		}
	}
	return result
}
func (m *appModel) loadPanelTitles(d dialog.Dialog, data *panelSessionData) tea.Cmd {
	catalog, ok := m.application.SessionRuntime().(runtime.SessionSummaryCatalog)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(m.ctx(), 3*time.Second)
	d.(interface{ SetCancel(func()) }).SetCancel(cancel)
	result := panelTitlesMsg{data: data, application: m.application, owner: m.paneFocus(), sessionID: data.sessionID, generation: data.generation, dialog: d, titles: m.panelTitles(data.nodes)}
	return func() tea.Msg {
		defer cancel()
		entries, err := catalog.ListSessionSummaries(ctx, runtime.SessionSummaryOptions{IncludeChildren: true})
		if err == nil {
			for _, entry := range entries {
				if _, ok := result.titles[entry.SessionID]; ok {
					result.titles[entry.SessionID] = entry.Title
				}
			}
		}
		return result
	}
}
func (m *appModel) acceptPanelTitles(msg panelTitlesMsg) tea.Cmd {
	if m.application != msg.application || m.paneFocus() != msg.owner || m.dialogMgr.TopDialog() != msg.dialog || m.dialogMgr.Closing() {
		return nil
	}
	data := m.panelData[msg.owner]
	if data == nil || data != msg.data || data.sessionID != msg.sessionID || data.generation != msg.generation {
		return nil
	}
	generation, ok := m.supervisor.RouteGeneration(msg.owner)
	if !ok || generation != msg.generation {
		return nil
	}
	titles := maps.Clone(msg.titles)
	for id := range titles {
		if runner := m.supervisor.FindBySession(id); runner != nil && runner.App.Session() != nil {
			titles[id] = runner.App.Session().TitleSnapshot()
		}
	}
	return m.updateDialogCmd(dialog.SubagentsRefreshMsg{Dialog: msg.dialog, Titles: titles})
}
