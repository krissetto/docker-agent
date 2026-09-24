package tui

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/gitroot"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/sessionbrowser"
)

type sessionQueryReady struct{ generation uint64 }
type sessionPageResult struct {
	generation  uint64
	application *app.App
	query       string
	more        bool
	rows        []sessionbrowser.Row
	cursor      string
	err         error
}

func (m *appModel) toggleSessionsBrowser() tea.Cmd {
	if m.workspaceUI.browser == nil {
		m.workspaceUI.browser = sessionbrowser.New()
	}
	m.workspaceUI.visible = !m.workspaceUI.visible
	if m.workspaceEmpty() {
		m.workspaceUI.visible = true
	}
	m.viewCacheValid = false
	if m.workspaceUI.visible {
		m.editor.Blur()
		m.chatPage.BlurMessages()
		return tea.Batch(m.workspaceUI.browser.Focus(), m.resizeAll(), m.requestSessionPage(false))
	}
	m.workspaceUI.browser.Blur()
	return tea.Batch(m.editor.Focus(), m.resizeAll())
}
func (m *appModel) resizeSessionsBrowser() tea.Cmd {
	ui := &m.workspaceUI
	if ui.browser == nil {
		return nil
	}
	ui.browserWidth = 0
	ui.fullscreen = false
	if ui.visible {
		ui.browserWidth = min(34, max(24, m.width/4))
		ui.fullscreen = m.width-ui.browserWidth < 2*paneMinWidth+1 || m.contentHeight < paneMinHeight
		if ui.fullscreen {
			ui.browserWidth = m.width
		}
	}
	ui.browser.SetPosition(0, 0)
	return ui.browser.SetSize(ui.browserWidth, m.contentHeight)
}
func (m *appModel) composeSessionsBrowser(content string) string {
	ui := &m.workspaceUI
	if !ui.visible || ui.browser == nil {
		return content
	}
	if ui.fullscreen {
		return paneClipped(ui.browser.View(), m.width, m.contentHeight)
	}
	return composeRootLayers([]*lipgloss.Layer{lipgloss.NewLayer(content), lipgloss.NewLayer(paneClipped(ui.browser.View(), ui.browserWidth, m.contentHeight))}, m.width, m.contentHeight)
}
func (m *appModel) workspacePointer(msg tea.Msg, x, y int) (bool, tea.Cmd) {
	ui := &m.workspaceUI
	if m.dialogMgr.Open() || !ui.visible || ui.browser == nil || y < 0 || y >= m.contentHeight {
		return false, nil
	}
	if x >= 0 && x < ui.browserWidth {
		var focus tea.Cmd
		if _, click := msg.(tea.MouseClickMsg); click {
			m.editor.Blur()
			m.chatPage.BlurMessages()
			focus = ui.browser.Focus()
		}
		m.viewCacheValid = false
		return true, tea.Batch(focus, ui.browser.Update(msg))
	}
	if _, click := msg.(tea.MouseClickMsg); click {
		ui.browser.Blur()
	}
	return ui.fullscreen, nil
}
func (m *appModel) debounceSessionQuery(query string) tea.Cmd {
	ui := &m.workspaceUI
	ui.query = strings.TrimSpace(query)
	ui.generation++
	if ui.cancel != nil {
		ui.cancel()
		ui.cancel = nil
	}
	generation := ui.generation
	ui.browser.SetLoading(true)
	return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg { return sessionQueryReady{generation: generation} })
}
func (m *appModel) requestSessionPage(more bool) tea.Cmd {
	ui := &m.workspaceUI
	if ui.browser == nil || m.application == nil {
		return nil
	}
	if more && ui.cursor == "" {
		return nil
	}
	if ui.cancel != nil {
		ui.cancel()
	}
	ui.generation++
	generation := ui.generation
	pager, ok := m.application.SessionRuntime().(runtime.SessionSummaryPager)
	if !ok {
		ui.browser.SetLoading(false)
		ui.browser.SetError("Stored-session browsing is unsupported by this runtime")
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx())
	ui.cancel = cancel
	query, cursor := ui.query, ""
	if more {
		cursor = ui.cursor
	}
	ui.browser.SetLoading(true)
	ui.browser.SetError("")
	application := m.application
	return func() tea.Msg {
		page, err := pager.ListSessionSummaryPage(ctx, runtime.SessionSummaryPageOptions{IncludeChildren: true, Limit: 50, Cursor: cursor, Query: query})
		result := sessionPageResult{generation: generation, application: application, query: query, more: more, cursor: page.NextCursor, err: err}
		projects := make(map[string]string)
		for _, entry := range page.Entries {
			if ctx.Err() != nil {
				result.err = ctx.Err()
				break
			}
			dir := entry.WorkingDir
			normalized := filepath.Clean(dir)
			if path, err := filepath.EvalSymlinks(normalized); err == nil {
				normalized = path
			}
			project, exists := projects[normalized]
			if !exists {
				project = gitroot.Root(normalized)
				if project == "" {
					project = normalized
				}
				projects[normalized] = project
			}
			result.rows = append(result.rows, sessionbrowser.Row{SessionID: entry.SessionID, Title: entry.Title, WorkingDir: normalized, ProjectID: project, ProjectName: filepath.Base(project), UnavailableReason: entry.RouteError})
		}
		return result
	}
}
func (m *appModel) acceptSessionPage(msg sessionPageResult) tea.Cmd {
	ui := &m.workspaceUI
	if msg.generation != ui.generation || msg.application != m.application || msg.query != ui.query {
		return nil
	}
	ui.browser.SetLoading(false)
	if msg.err != nil {
		ui.browser.SetError(msg.err.Error())
		return nil
	}
	if !msg.more {
		ui.rows = nil
	}
	ui.rows = append(ui.rows, msg.rows...)
	ui.cursor = msg.cursor
	// Keep browsing memory bounded even after many explicit older-page requests.
	if len(ui.rows) > 500 {
		ui.rows = slices.Clone(ui.rows[len(ui.rows)-500:])
	}
	m.refreshBrowserRows()
	return nil
}
func (m *appModel) refreshBrowserRows() {
	ui := &m.workspaceUI
	if ui.browser == nil {
		return
	}
	rows := slices.Clone(ui.rows)
	byID := make(map[string]int)
	for i, row := range rows {
		byID[row.SessionID] = i
	}
	tabs, _ := m.supervisor.GetTabs()
	for _, tab := range tabs {
		runner := m.supervisor.GetRunner(tab.SessionID)
		if runner == nil || runner.App == nil || runner.App.Session() == nil {
			continue
		}
		sess := runner.App.Session()
		id := m.persistedSessionID(tab.SessionID)
		title := sess.TitleSnapshot()
		dir := sess.WorkingDir
		if ui.query != "" && !strings.Contains(strings.ToLower(title+" "+dir), strings.ToLower(ui.query)) {
			continue
		}
		index, exists := byID[id]
		if !exists {
			index = len(rows)
			byID[id] = index
			rows = append(rows, sessionbrowser.Row{SessionID: id, Title: title, WorkingDir: dir, ProjectID: m.sessionProject(dir), ProjectName: filepath.Base(m.sessionProject(dir))})
		}
		rows[index].Title = title
		if project, ok := ui.projects[dir]; ok {
			rows[index].ProjectID = project
			rows[index].ProjectName = filepath.Base(project)
		}
		rows[index].Activity = ansi.Strip(m.paneActivity(tab.SessionID))
		rows[index].NeedsAttention = tab.NeedsAttention
		rows[index].Active = m.chatPages[tab.SessionID] != nil && m.chatPages[tab.SessionID].IsWorking()
		if snapshot := sess.GetSubagentTree(); snapshot != nil {
			for _, node := range snapshot.Nodes {
				rows[index].NeedsAttention = rows[index].NeedsAttention || node.Node.NeedsAttention
			}
		}
	}
	if w := m.paneWorkspaces.active; w != nil {
		for id, reason := range w.unavailable {
			if _, ok := byID[id]; !ok {
				rows = append(rows, sessionbrowser.Row{SessionID: id, Title: id, UnavailableReason: reason, ProjectID: "unavailable", ProjectName: "Unavailable sessions"})
			}
		}
	}
	slices.SortStableFunc(rows, func(a, b sessionbrowser.Row) int {
		if a.Active != b.Active {
			if a.Active {
				return -1
			}
			return 1
		}
		if a.NeedsAttention != b.NeedsAttention {
			if a.NeedsAttention {
				return -1
			}
			return 1
		}
		return 0
	})
	ui.browser.SetRows(rows, ui.cursor != "")
	var workspaces []sessionbrowser.WorkspaceItem
	for _, w := range m.paneWorkspaces.ordered {
		workspaces = append(workspaces, sessionbrowser.WorkspaceItem{ID: w.id, Name: w.name, Active: w == m.paneWorkspaces.active})
	}
	ui.browser.SetWorkspaces(workspaces)
}

type sessionProjectsMsg struct {
	application *app.App
	projects    map[string]string
}

func (m *appModel) sessionProject(dir string) string {
	if project, ok := m.workspaceUI.projects[dir]; ok {
		return project
	}
	return dir
}
func (m *appModel) prepareSessionProjects() tea.Cmd {
	if !m.workspaceUI.visible || m.workspaceUI.grouping {
		return nil
	}
	var dirs []string
	tabs, _ := m.supervisor.GetTabs()
	for _, tab := range tabs {
		if runner := m.supervisor.GetRunner(tab.SessionID); runner != nil && runner.App != nil && runner.App.Session() != nil {
			dir := runner.App.Session().WorkingDir
			if _, known := m.workspaceUI.projects[dir]; !known && !slices.Contains(dirs, dir) {
				dirs = append(dirs, dir)
			}
		}
	}
	if len(dirs) == 0 {
		return nil
	}
	m.workspaceUI.grouping = true
	application := m.application
	ctx := m.ctx()
	return func() tea.Msg {
		result := sessionProjectsMsg{application: application, projects: make(map[string]string)}
		for _, dir := range dirs {
			if ctx.Err() != nil {
				break
			}
			normalized := filepath.Clean(dir)
			if resolved, err := filepath.EvalSymlinks(normalized); err == nil {
				normalized = resolved
			}
			project := gitroot.Root(normalized)
			if project == "" {
				project = normalized
			}
			result.projects[dir] = project
		}
		return result
	}
}
