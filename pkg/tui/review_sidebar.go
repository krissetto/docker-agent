package tui

import (
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/components/notification"
	view "github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

type gitSidebarPage interface {
	SetGitChanges(changes sidebar.GitChanges) tea.Cmd
	GitWatchState() sidebar.GitWatchMsg
	RevealGit() tea.Cmd
	GitFocused() bool
	SetGitFocused(focused bool) tea.Cmd
	GitKey(msg tea.KeyPressMsg) (bool, tea.Cmd)
	SetContentOverlay(overlay chat.ContentOverlay)
}

func (m *appModel) gitPage(anchor string) (gitSidebarPage, bool) {
	page, ok := m.chatPages[anchor].(gitSidebarPage)
	return page, ok
}

// markGitSidebar offers the Git section on the focused page once its session
// has a workspace folder. Nothing is scanned until the section is expanded.
func (m *appModel) markGitSidebar() tea.Cmd {
	focus := m.paneFocus()
	if focus == "" || m.gitSidebarMarked[focus] || m.supervisor == nil {
		return nil
	}
	runner := m.supervisor.GetRunner(focus)
	if runner == nil || runner.App == nil || runner.App.Session() == nil || runner.App.Session().WorkingDir == "" {
		return nil
	}
	page, ok := m.gitPage(focus)
	if !ok {
		return nil
	}
	if m.gitSidebarMarked == nil {
		m.gitSidebarMarked = map[string]bool{}
	}
	m.gitSidebarMarked[focus] = true
	return page.SetGitChanges(sidebar.GitChanges{Owner: focus, Available: true})
}

// pushGitSidebar shows the workspace's Git data in its page's sidebar.
func (m *appModel) pushGitSidebar(ws *gitReviewWorkspace) tea.Cmd {
	page, ok := m.gitPage(ws.anchor)
	if !ok {
		return nil
	}
	c := sidebar.GitChanges{Owner: ws.anchor, Available: true, Snapshot: ws.snapshot}
	_, c.Loading = m.gitReviewPending["provider"]
	if _, refreshing := m.gitReviewPending["snapshot"]; refreshing {
		c.Loading = true
	}
	for key := range m.gitReviewPending {
		if id, ok := strings.CutPrefix(key, "worktree:"); ok {
			if c.Scanning == nil {
				c.Scanning = map[string]bool{}
			}
			c.Scanning[id] = true
		}
	}
	if ws.inspecting {
		if model := ws.models[view.ModeChanges]; model != nil {
			c.Open, _ = model.OpenKey()
		}
	}
	return tea.Batch(page.SetGitChanges(c), m.handleGitWatch(page.GitWatchState()))
}

// ensureGitData makes the focused session's Git data live without showing any
// surface. It is scoped to the focused pane's own session and folder (not the
// pane workspace's root), so the sidebar, inspector and send all agree.
func (m *appModel) ensureGitData() (*gitReviewWorkspace, tea.Cmd) {
	anchor := m.paneFocus()
	if anchor == "" || m.supervisor == nil {
		return nil, nil
	}
	runner := m.supervisor.GetRunner(anchor)
	if runner == nil || runner.App == nil || runner.App.Session() == nil || runner.App.Session().WorkingDir == "" {
		return nil, nil
	}
	sess := runner.App.Session()
	if m.gitReviews == nil {
		m.gitReviews = map[string]*gitReviewWorkspace{}
	}
	ws := m.gitReviews[anchor]
	if ws == nil {
		ws = newGitReviewWorkspace(anchor)
		ws.splitPref = m.gitDiffSplit
		m.gitReviews[anchor] = ws
	}
	route, _ := m.supervisor.RouteGeneration(anchor)
	if ws.sessionID != sess.ID || filepath.Clean(ws.workingDir) != filepath.Clean(sess.WorkingDir) || ws.route != route {
		draft, preview, sent, splitPref := ws.draft, ws.preview, ws.sent, ws.splitPref
		*ws = *newGitReviewWorkspace(anchor)
		ws.draft, ws.preview, ws.sent, ws.splitPref = draft, preview, sent, splitPref
		ws.sessionID, ws.workingDir, ws.route = sess.ID, sess.WorkingDir, route
	}
	var cmds []tea.Cmd
	if m.gitReviewOwner != anchor {
		cmds = append(cmds, m.hideGitReview())
		m.gitReviewOwner = anchor
		m.gitReviewGeneration++
	}
	if !ws.open {
		ws.open, ws.mode = true, view.ModeChanges
	}
	if ws.provider == nil {
		if _, loading := m.gitReviewPending["provider"]; !loading {
			cmds = append(cmds, m.loadGitReviewProvider(anchor, runner.App))
		}
	}
	return ws, tea.Batch(cmds...)
}

func (m *appModel) handleGitSidebarRequest(msg sidebar.GitRequestMsg) tea.Cmd {
	if msg.Owner != m.paneFocus() {
		return nil // a late request from a page that is no longer focused
	}
	ws, cmd := m.ensureGitData()
	if ws == nil {
		return notification.InfoCmd("This session has no workspace folder to review")
	}
	cmds := []tea.Cmd{cmd}
	switch msg.Action {
	case sidebar.GitLoad:
		if ws.provider != nil && ws.snapshot == nil {
			cmds = append(cmds, m.submitGitReviewRequest(view.RefreshRequestMsg{Owner: ws.anchor}))
		}
	case sidebar.GitRefresh:
		if ws.provider != nil {
			cmds = append(cmds, m.submitGitReviewRequest(view.RefreshRequestMsg{Owner: ws.anchor}))
		}
	case sidebar.GitLoadWorktree:
		cmds = append(cmds, m.submitGitReviewRequest(view.LoadWorktreeRequestMsg{Owner: ws.anchor, WorktreeID: msg.WorktreeID}))
	case sidebar.GitOpen:
		cmds = append(cmds, m.openGitInspector(ws, view.DocumentKey{Mode: view.ModeChanges, WorktreeID: msg.WorktreeID, Path: msg.Path, Group: msg.Group}))
	}
	cmds = append(cmds, m.pushGitSidebar(ws))
	m.viewCacheValid = false
	return tea.Batch(cmds...)
}

// openGitInspector shows a diff over the transcript; the sidebar stays.
func (m *appModel) openGitInspector(ws *gitReviewWorkspace, key view.DocumentKey) tea.Cmd {
	page, ok := m.gitPage(ws.anchor)
	if !ok {
		return nil
	}
	if ws.mode == view.ModeFiles {
		ws.current().Blur()
		ws.mode = view.ModeChanges
	}
	if !ws.inspecting {
		if m.focusedPanel == PanelSidebarGit {
			ws.restore = gitReviewFocus{panel: PanelContent}
		} else {
			ws.restore = m.captureReviewFocus()
		}
	}
	ws.inspecting = true
	model := ws.current()
	if ws.snapshot != nil {
		model.SetSnapshot(*ws.snapshot)
	}
	model.SetSession(ws.session)
	model.SetDraft(ws.draftView())
	page.SetContentOverlay(model)
	cmd := model.OpenDocument(key)
	_ = page.SetGitFocused(false)
	m.focusReview()
	return cmd
}

func (m *appModel) hideGitInspector(ws *gitReviewWorkspace) tea.Cmd {
	if ws == nil || !ws.inspecting {
		return nil
	}
	ws.inspecting = false
	if model := ws.models[view.ModeChanges]; model != nil {
		model.Blur()
	}
	if page, ok := m.gitPage(ws.anchor); ok {
		page.SetContentOverlay(nil)
	}
	m.viewCacheValid = false
	return m.pushGitSidebar(ws)
}

// revealGitChanges is /changes: open the sidebar section and focus it.
func (m *appModel) revealGitChanges() tea.Cmd {
	page, ok := m.gitPage(m.paneFocus())
	if !ok || m.hideSidebar || m.leanMode {
		return notification.InfoCmd("Show the sidebar to review Git changes")
	}
	mark := m.markGitSidebar()
	cmd := page.RevealGit()
	m.focusGitSidebar(page)
	return tea.Batch(mark, cmd)
}

func (m *appModel) focusGitSidebar(page gitSidebarPage) {
	if model := m.reviewModel(); model != nil {
		model.Blur()
	}
	m.editor.SetContextBarFocused(false)
	m.editor.Blur()
	if m.messageBar != nil {
		m.messageBar.SetFocused(false)
	}
	m.panelFocused = ""
	m.chatPage.BlurMessages()
	m.focusedPanel = PanelSidebarGit
	_ = page.SetGitFocused(true)
	m.viewCacheValid = false
}

// gitSidebarFocusable reports whether Tab may stop at the Git section.
func (m *appModel) gitSidebarFocusable() bool {
	if m.hideSidebar || m.leanMode || !m.gitSidebarMarked[m.paneFocus()] {
		return false
	}
	_, ok := m.gitPage(m.paneFocus())
	return ok
}

// handleGitSidebarKey routes keys while the Git section has focus. Tab moves
// on through the usual ring, Shift+Tab back to content, Esc to the composer.
func (m *appModel) handleGitSidebarKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	if m.focusedPanel != PanelSidebarGit {
		return false, nil
	}
	page, ok := m.gitPage(m.paneFocus())
	if !ok {
		m.focusedPanel = PanelEditor
		return false, nil
	}
	switch msg.String() {
	case "tab":
		_ = page.SetGitFocused(false)
		m.focusedPanel = PanelContent
		return true, m.focusAfterContent()
	case "shift+tab":
		_ = page.SetGitFocused(false)
		m.focusedPanel = PanelContent
		return true, m.focusContent()
	case "esc":
		_ = page.SetGitFocused(false)
		return true, m.focusComposerFromContent()
	}
	handled, cmd := page.GitKey(msg)
	m.viewCacheValid = false
	if handled || (msg.Text != "" && msg.Mod == 0) {
		return true, cmd
	}
	return false, nil
}

// focusAfterContent continues the Tab ring past content: message actions,
// footer items, then the composer.
func (m *appModel) focusAfterContent() tea.Cmd {
	m.chatPage.BlurMessages()
	if m.messageBar != nil && m.messageBar.HasActions() {
		m.messageBar.SetFocused(true)
		m.focusedPanel = PanelMessageBar
		return nil
	}
	if m.focusPanel() {
		return nil
	}
	m.focusedPanel = PanelEditor
	return m.editor.Focus()
}

// previewCurrentAgent captures the focused session's current agent as the
// only recipient for a Git review; no picker is offered.
func (m *appModel) previewCurrentAgent(ws *gitReviewWorkspace, revision uint64) tea.Cmd {
	focus := m.paneFocus()
	runner := m.supervisor.GetRunner(focus)
	if runner == nil || runner.App == nil || runner.App.Session() == nil || runner.App.IsReadOnly() {
		return notification.ErrorCmd("The current session can't receive this review")
	}
	label := "current agent · ◆ " + fallbackString(runner.App.Session().TitleSnapshot(), "untitled session")
	if attached := runner.App.AttachedSubagent(); attached != nil {
		label += " › " + string(attached.NodeID)
	}
	route, _ := m.supervisor.RouteGeneration(focus)
	choice := &gitReviewTargetChoice{owner: ws.anchor, revision: revision, options: map[string]gitReviewCandidate{
		focus: {label: label, sessionID: focus, app: runner.App, route: route},
	}}
	return m.applyGitReviewTarget(choice, focus)
}
