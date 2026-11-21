package tui

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/gitreview"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	view "github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

type gitDiscoveryContinuation interface {
	ContinueDiscovery(ctx context.Context) (gitreview.Snapshot, error)
	CanContinueDiscovery() bool
}

type gitDiscoveryContinuationRequest struct{ Owner string }

type gitSidebarWorktreePosition struct {
	repo, worktree int
}

type gitSidebarPrewarm struct {
	generation      uint64
	ownerGeneration uint64
	repo            int
	worktree        int
	complete        bool
	inflight        map[string]gitSidebarWorktreePosition
}

type gitSidebarPage interface {
	SetGitChanges(changes sidebar.GitChanges) tea.Cmd
	SetGitMode(mode view.Mode) tea.Cmd
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

// maxSidebarMetadataConcurrent limits concurrent status/numstat metadata jobs.
const maxSidebarMetadataConcurrent = 4

// markGitSidebar offers Git on the focused page and warms its bounded metadata
// while the sidebar is visible. It never opens a review surface or loads diffs.
func (m *appModel) markGitSidebar() tea.Cmd {
	focus := m.paneFocus()
	if focus == "" || m.supervisor == nil {
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
	var cmds []tea.Cmd
	if !m.gitSidebarMarked[focus] {
		if m.gitSidebarMarked == nil {
			m.gitSidebarMarked = map[string]bool{}
		}
		m.gitSidebarMarked[focus] = true
		cmds = append(cmds, page.SetGitChanges(sidebar.GitChanges{Owner: focus, Available: true}))
	}
	if m.hideSidebar || m.leanMode {
		return tea.Batch(cmds...)
	}
	// Do not prewarm by taking ownership away from a visible Files review or
	// diff inspector. Once that surface closes, syncGitReview calls us again.
	if active := m.activeReview(); active != nil &&
		(active.anchor != focus || active.mode != view.ModeChanges || active.inspecting) {
		return tea.Batch(cmds...)
	}
	ws, ensure := m.ensureGitData(nil)
	cmds = append(cmds, ensure)
	if ws != nil {
		if _, loading := m.gitReviewPending["provider"]; loading {
			cmds = append(cmds, m.pushGitSidebar(ws))
		}
		cmds = append(cmds, m.prewarmGitSidebarMetadata(ws))
	}
	return tea.Batch(cmds...)
}

// prewarmGitSidebarMetadata batches status/numstat metadata until every
// discovered worktree is resolved. Provider.LoadWorktree never hydrates diffs
// or history; work remains lazy while hidden or another surface owns the page.
func (m *appModel) prewarmGitSidebarMetadata(ws *gitReviewWorkspace) tea.Cmd {
	if ws == nil || ws.provider == nil || ws.snapshot == nil || ws.anchor != m.paneFocus() ||
		m.hideSidebar || m.leanMode || ws.inspecting || ws.mode != view.ModeChanges ||
		ws.sidebarMode != view.ModeChanges || ws.snapshot.Browse {
		return nil
	}
	if active := m.activeReview(); active != ws {
		return nil
	}
	state := &ws.prewarm
	if state.generation != ws.snapshot.Generation || state.ownerGeneration != m.gitReviewGeneration {
		*state = gitSidebarPrewarm{generation: ws.snapshot.Generation, ownerGeneration: m.gitReviewGeneration,
			inflight: make(map[string]gitSidebarWorktreePosition)}
	}
	if state.complete {
		return nil
	}
	pending := make(map[string]bool)
	for key := range m.gitReviewPending {
		if id, ok := strings.CutPrefix(key, "worktree:"); ok {
			pending[id] = true
		}
	}
	// If an in-flight request disappears without updating the snapshot, resume
	// at its saved position so cancellation cannot strand later metadata.
	resumeRepo, resumeWorktree := len(ws.snapshot.Repositories), 0
	for id, position := range state.inflight {
		if pending[id] {
			continue
		}
		if position.repo < len(ws.snapshot.Repositories) && position.worktree < len(ws.snapshot.Repositories[position.repo].Worktrees) &&
			ws.snapshot.Repositories[position.repo].Worktrees[position.worktree].State == view.StateUnscanned {
			if position.repo < resumeRepo || position.repo == resumeRepo && position.worktree < resumeWorktree {
				resumeRepo, resumeWorktree = position.repo, position.worktree
			}
		}
		delete(state.inflight, id)
	}
	if resumeRepo < len(ws.snapshot.Repositories) {
		state.repo, state.worktree = resumeRepo, resumeWorktree
	}
	slots := maxSidebarMetadataConcurrent - len(pending)
	if slots <= 0 {
		return nil
	}
	cmds := make([]tea.Cmd, 0, slots)
	for state.repo < len(ws.snapshot.Repositories) {
		repoIndex, worktreeIndex := state.repo, state.worktree
		repo := ws.snapshot.Repositories[repoIndex]
		if worktreeIndex >= len(repo.Worktrees) {
			state.repo++
			state.worktree = 0
			continue
		}
		wt := repo.Worktrees[worktreeIndex]
		state.worktree++
		position := gitSidebarWorktreePosition{repo: repoIndex, worktree: worktreeIndex}
		if pending[wt.ID] {
			state.inflight[wt.ID] = position
			continue
		}
		if wt.State != view.StateUnscanned {
			continue
		}
		cmd := m.submitGitReviewRequest(view.LoadWorktreeRequestMsg{Owner: ws.anchor, WorktreeID: wt.ID})
		if cmd == nil {
			// Submission can be unavailable during a scope handoff. Retry this
			// exact position on the next safe update.
			state.repo, state.worktree = repoIndex, worktreeIndex
			if len(cmds) > 0 {
				return tea.Batch(append(cmds, m.pushGitSidebar(ws))...)
			}
			return nil
		}
		cmds = append(cmds, cmd)
		pending[wt.ID] = true
		state.inflight[wt.ID] = position
		slots--
		if slots == 0 {
			return tea.Batch(append(cmds, m.pushGitSidebar(ws))...)
		}
	}
	state.complete = len(state.inflight) == 0
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(append(cmds, m.pushGitSidebar(ws))...)
}

// pushGitSidebar shows the workspace's Git data in its page's sidebar.
func (m *appModel) pushGitSidebar(ws *gitReviewWorkspace) tea.Cmd {
	page, ok := m.gitPage(ws.anchor)
	if !ok {
		return nil
	}
	// Root-only data is shown only in Files; Changes waits for its explicit scan.
	snapshot := ws.snapshot
	if snapshot != nil && snapshot.Browse && ws.sidebarMode != view.ModeFiles {
		snapshot = nil
	}
	c := sidebar.GitChanges{Owner: ws.anchor, Available: true, Snapshot: snapshot, Mode: ws.sidebarMode, DirectoryGeneration: ws.directoryGeneration}
	if capability, ok := ws.provider.(gitDiscoveryContinuation); ok && ws.snapshot != nil && ws.snapshot.Scope.LimitReason != "" {
		c.DiscoveryContinuationSupported = true
		c.ContinueDiscovery = capability.CanContinueDiscovery()
	}
	_, c.Loading = m.gitReviewPending["provider"]
	if _, resolving := m.gitReviewPending["identity"]; resolving {
		c.Loading = true
	}
	_, c.ContinuingDiscovery = m.gitReviewPending["snapshot.continue"]
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
	for _, directory := range ws.directories {
		c.Directories = append(c.Directories, directory)
	}
	slices.SortFunc(c.Directories, func(a, b view.Directory) int {
		if a.WorktreeID != b.WorktreeID {
			return strings.Compare(a.WorktreeID, b.WorktreeID)
		}
		return strings.Compare(a.Path, b.Path)
	})
	if ws.inspecting {
		if model := ws.models[ws.mode]; model != nil {
			c.Open, _ = model.OpenKey()
		}
	}
	return tea.Batch(page.SetGitChanges(c), m.handleGitWatch(page.GitWatchState()))
}

// ensureGitData makes the focused session's Git data live without showing any
// surface. It is scoped to the focused pane's own session and folder (not the
// pane workspace's root), so the sidebar, inspector and send all agree.
// intent, when set, is the sidebar mode the request serves; nil keeps the current one.
func (m *appModel) ensureGitData(intent *view.Mode) (*gitReviewWorkspace, tea.Cmd) {
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
		m.detachGitReviewScope(ws)
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
	ws.scopePane = true
	if intent != nil {
		ws.sidebarMode = *intent
	}
	if ws.provider == nil && ws.snapshot == nil {
		_, loading := m.gitReviewPending["provider"]
		_, resolving := m.gitReviewPending["identity"]
		loading = loading || resolving
		// Files never waits behind a full Changes bootstrap; it restarts as a root-only browse.
		restartAsBrowse := loading && !ws.loadBrowse && intent != nil && *intent == view.ModeFiles
		if !loading || restartAsBrowse {
			cmds = append(cmds, m.loadGitReviewProvider(anchor, runner.App))
		}
	}
	if policyCmd, changed := m.revalidateGitReviewScope(ws, runner.App); changed {
		cmds = append(cmds, policyCmd)
	}
	return ws, tea.Batch(cmds...)
}

func (m *appModel) handleGitSidebarRequest(msg sidebar.GitRequestMsg) tea.Cmd {
	if msg.Owner != m.paneFocus() {
		return nil // a late request from a page that is no longer focused
	}
	if msg.Action == sidebar.GitLoad || msg.Action == sidebar.GitRefresh {
		if ws := m.gitReviews[msg.Owner]; ws != nil && ws.provider == nil && ws.snapshot != nil {
			// A user retry may recover after provider construction/discovery failed.
			ws.snapshot = nil
		}
	}
	ws, cmd := m.ensureGitData(gitRequestIntent(msg.Action))
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
			m.invalidateGitDirectories(ws)
			cmds = append(cmds, m.submitGitReviewRequest(view.RefreshRequestMsg{Owner: ws.anchor}))
		}
	case sidebar.GitLoadWorktree:
		cmds = append(cmds, m.submitGitReviewRequest(view.LoadWorktreeRequestMsg{Owner: ws.anchor, WorktreeID: msg.WorktreeID}))
	case sidebar.GitBrowse:
		ws.sidebarMode = view.ModeFiles
		if page, ok := m.gitPage(ws.anchor); ok {
			cmds = append(cmds, page.SetGitMode(view.ModeFiles))
		}
		cmds = append(cmds, m.filesRootRequest(ws))
	case sidebar.GitShowChanges:
		ws.sidebarMode = view.ModeChanges
		if page, ok := m.gitPage(ws.anchor); ok {
			cmds = append(cmds, page.SetGitMode(view.ModeChanges))
			if !page.GitWatchState().Open {
				cmds = append(cmds, page.RevealGit())
			}
		}
		cmds = append(cmds, m.ensureChangesScan(ws))
	case sidebar.GitLoadDirectory:
		cmds = append(cmds, m.submitGitReviewRequest(view.LoadDirectoryRequestMsg{Owner: ws.anchor, WorktreeID: msg.WorktreeID, Path: msg.Path}))
	case sidebar.GitOpenFile:
		ws.scopePane = true
		ws.sidebarMode = view.ModeFiles
		if page, ok := m.gitPage(ws.anchor); ok {
			cmds = append(cmds, page.SetGitMode(view.ModeFiles))
		}
		key := view.DocumentKey{Mode: view.ModeFiles, WorktreeID: msg.WorktreeID, Path: msg.Path}
		cmds = append(cmds, m.openGitFilesInspector(ws, key))
	case sidebar.GitContinueDiscovery:
		cmds = append(cmds, m.submitGitReviewRequest(gitDiscoveryContinuationRequest{Owner: ws.anchor}))
	case sidebar.GitOpen:
		ws.scopePane = true
		ws.sidebarMode = view.ModeChanges
		if page, ok := m.gitPage(ws.anchor); ok {
			cmds = append(cmds, page.SetGitMode(view.ModeChanges))
		}
		cmds = append(cmds, m.openGitInspector(ws, view.DocumentKey{Mode: view.ModeChanges, WorktreeID: msg.WorktreeID, Path: msg.Path, Group: msg.Group}))
		cmds = append(cmds, m.ensureChangesScan(ws))
	}
	cmds = append(cmds, m.pushGitSidebar(ws))
	m.viewCacheValid = false
	return tea.Batch(cmds...)
}

// gitRequestIntent is the sidebar mode a request is for; nil leaves it unchanged.
func gitRequestIntent(action sidebar.GitAction) *view.Mode {
	switch action {
	case sidebar.GitBrowse, sidebar.GitLoadDirectory, sidebar.GitOpenFile:
		mode := view.ModeFiles
		return &mode
	case sidebar.GitShowChanges, sidebar.GitOpen:
		mode := view.ModeChanges
		return &mode
	}
	return nil
}

// filesRootRequest makes Files read the root, superseding a full Git scan that is
// still running. A root read already in flight, or data already loaded, is reused.
func (m *appModel) filesRootRequest(ws *gitReviewWorkspace) tea.Cmd {
	if ws == nil || ws.provider == nil {
		return nil
	}
	m.setGitReviewLease(ws, false)
	if req, pending := m.gitReviewPending["snapshot"]; pending {
		if ws.snapshotBrowse {
			return nil
		}
		req.cancel()
		delete(m.gitReviewPending, "snapshot")
	} else if ws.snapshot != nil {
		return nil
	}
	return m.submitGitReviewRequest(view.RefreshRequestMsg{Owner: ws.anchor})
}

// ensureChangesScan turns a root-only Files snapshot into an explicit Git scan
// once Changes is selected, so root-only data is never shown as complete.
func (m *appModel) ensureChangesScan(ws *gitReviewWorkspace) tea.Cmd {
	if ws == nil || ws.provider == nil || ws.snapshot == nil || !ws.snapshot.Browse || ws.sidebarMode != view.ModeChanges {
		return nil
	}
	return m.submitGitReviewRequest(view.RefreshRequestMsg{Owner: ws.anchor})
}

func (m *appModel) openGitFilesInspector(ws *gitReviewWorkspace, key view.DocumentKey) tea.Cmd {
	if ws == nil {
		return nil
	}
	if ws.mode != view.ModeFiles {
		if ws.inspecting {
			m.hideGitInspector(ws)
		}
		ws.mode = view.ModeFiles
	}
	if !ws.inspecting {
		if m.focusedPanel == PanelSidebarGit {
			ws.restore = gitReviewFocus{panel: PanelContent}
		} else {
			ws.restore = m.captureReviewFocus()
		}
	}
	model := ws.current()
	if ws.snapshot != nil {
		model.SetSnapshot(*ws.snapshot)
	}
	model.SetDraft(ws.draftView())
	model.SetInspector(true)
	ws.inspecting = true
	ws.scopePane = true
	ws.sidebarMode = view.ModeFiles
	_ = m.chatPage.SetSize(m.width, m.contentHeight)
	if page, ok := m.gitPage(ws.anchor); ok {
		page.SetContentOverlay(model)
		_ = page.SetGitFocused(false)
	}
	model.Focus()
	m.focusReview()
	m.viewCacheValid = false
	return model.OpenDocument(key)
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
	ws.scopePane = true
	_ = m.chatPage.SetSize(m.width, m.contentHeight)
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
	if model := ws.models[ws.mode]; model != nil {
		model.Blur()
		model.SetInspector(ws.mode == view.ModeChanges)
	}
	if page, ok := m.gitPage(ws.anchor); ok {
		page.SetContentOverlay(nil)
	}
	_ = m.chatPage.SetSize(m.width, m.contentHeight)
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
	return m.focusReviewComposer()
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
	choice := &gitReviewTargetChoice{owner: ws.anchor, revision: revision, activeOnly: true, options: map[string]gitReviewCandidate{
		focus: {label: label, sessionID: focus, app: runner.App, route: route},
	}}
	return m.applyGitReviewTarget(choice, focus)
}
