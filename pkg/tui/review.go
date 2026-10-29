package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/gitreview"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/messagebar"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	view "github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/internal/editorname"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/reviewdraft"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	// gitReviewDocumentDelay coalesces live previews while the selection moves.
	gitReviewDocumentDelay = 60 * time.Millisecond
	gitAutoRefreshDelay    = 15 * time.Second
	gitReviewDockMinWidth  = 110
	gitReviewDockChatMin   = 50
	gitReviewDockReviewMin = 60
	gitReviewHintOwner     = "git-review"
)

// gitReviewFocus is the focus target to restore when review closes.
type gitReviewFocus struct {
	panel      FocusedPanel
	element    messages.PanelElement
	contextBar bool
}

// Each anchor keeps its presentation and drafts independent of shared workspace data.
type gitReviewWorkspace struct {
	anchor      string
	models      map[view.Mode]*view.Model
	mode        view.Mode
	sidebarMode view.Mode
	// loadBrowse is the intent of the provider bootstrap currently in flight.
	loadBrowse bool
	// snapshotBrowse is the intent of the snapshot refresh currently in flight.
	snapshotBrowse           bool
	draft                    *reviewdraft.Draft
	preview                  *gitReviewPreview
	sent                     *gitReviewPreview // recipient of the unresolved attempt, for exact retry
	snapshot                 *view.Snapshot
	provider                 gitreview.Provider
	sessionID                string
	workingDir               string
	route                    uint64
	open                     bool
	inspecting               bool // Changes: the diff inspector is shown over the transcript
	docked                   bool
	restore                  gitReviewFocus
	session                  view.Session
	hint                     messagebar.Token
	hintText                 string
	splitPref                bool
	refreshAt                time.Time
	prewarm                  gitSidebarPrewarm
	directories              map[string]view.Directory
	directoryGeneration      uint64
	externalEditorGeneration uint64
	scopePane                bool
	// scopeKey names the shared Git scope this workspace is attached to.
	scopeKey string
	// policyFingerprint is the cheap metadata captured at attach; a change revalidates identity.
	policyFingerprint string
}

func newGitReviewWorkspace(anchor string) *gitReviewWorkspace {
	return &gitReviewWorkspace{anchor: anchor, models: map[view.Mode]*view.Model{}, draft: reviewdraft.New(anchor), directories: map[string]view.Directory{}}
}

// current is the surface on screen: /changes and /files keep separate
// navigation state but share the workspace's snapshot, provider and draft.
func (ws *gitReviewWorkspace) current() *view.Model {
	model := ws.models[ws.mode]
	if model == nil {
		model = view.New(ws.anchor)
		model.SetMode(ws.mode)
		// Changes is the diff inspector; the sidebar's Git section navigates.
		model.SetInspector(ws.mode == view.ModeChanges)
		model.SetSplitPreference(ws.splitPref)
		ws.models[ws.mode] = model
	}
	return model
}

func (ws *gitReviewWorkspace) each(fn func(*view.Model)) {
	for _, model := range ws.models {
		fn(model)
	}
}

type gitReviewRequest struct {
	token  uint64
	cancel context.CancelFunc
	key    view.DocumentKey
}

type gitAutoRefreshMsg struct {
	owner string
	token uint64
}

type gitReviewTaskResult struct {
	request tea.Msg
	// browse is the request's captured Files intent, independent of the snapshot it returns.
	browse             bool
	owner, key         string
	token, generation  uint64
	snapshotGeneration uint64
	provider           gitreview.Provider
	snapshot           *view.Snapshot
	worktree           *view.Worktree
	directory          *view.Directory
	document           *view.Document
	refreshDocument    *view.DocumentKey
	err                error
}

// gitReviewProviderFactory returns a provider, or an unavailable snapshot when
// the session must not be read (for example a remote session without the API).
type gitReviewProviderFactory func(a *app.App, sessionID, workingDir string) (gitreview.Provider, *view.Snapshot, error)

// selectGitReviewProvider never falls back to local IO for remote sessions:
// a remote path names another machine's filesystem.
func selectGitReviewProvider(a *app.App, sessionID, workingDir string) (gitreview.Provider, *view.Snapshot, error) {
	if a == nil {
		return nil, nil, errors.New("session unavailable")
	}
	return chooseGitReviewProvider(a.SessionRuntime(), a.SessionHandle(), sessionID, workingDir, func(dir string) (gitreview.Provider, error) {
		return gitreview.NewLocal(dir)
	})
}

func chooseGitReviewProvider(rt runtime.SessionRuntime, handle runtime.SessionHandle, sessionID, workingDir string, local func(string) (gitreview.Provider, error)) (gitreview.Provider, *view.Snapshot, error) {
	if remote, ok := rt.(runtime.SessionGitReview); ok {
		p, err := runtime.NewGitReviewRemote(remote, sessionID)
		return p, nil, err
	}
	_, transport := rt.(*runtime.SessionTransport)
	if transport || (handle != nil && !runtime.IsLocalSessionHandle(handle)) {
		return nil, &view.Snapshot{Generation: 1, Scope: view.Scope{Label: workingDir, Remote: true, RemoteLabel: "remote session", Unavailable: "Review isn't available for this remote session. Local files are never read in its place."}}, nil
	}
	p, err := local(workingDir)
	return p, nil, err
}

// gitScopeKey identifies what the shown review belongs to: the focused pane's
// session for Git (sidebar + inspector), the pane workspace for /files.
func (m *appModel) gitScopeKey() string {
	if ws := m.activeReview(); ws != nil && (ws.scopePane || ws.inspecting) {
		return m.paneFocus()
	}
	return m.reviewAnchor()
}

func (m *appModel) reviewAnchor() string {
	if m.paneWorkspaces.active != nil && m.paneWorkspaces.active.anchor != "" {
		return m.paneWorkspaces.active.anchor
	}
	return m.paneFocus()
}

// activeReview returns the review shown in the content area, if any.
func (m *appModel) activeReview() *gitReviewWorkspace {
	if m.gitReviewOwner == "" {
		return nil
	}
	if ws := m.gitReviews[m.gitReviewOwner]; ws != nil && ws.open {
		return ws
	}
	return nil
}

// reviewModel is the review surface taking keys: the Files surface, or the
// Changes inspector while it is shown. Git data alone takes no input.
func (m *appModel) reviewModel() *view.Model {
	ws := m.activeReview()
	if ws == nil || (ws.mode == view.ModeChanges && !ws.inspecting) {
		return nil
	}
	return ws.current()
}

// reviewSurface is the full-content Files surface, if shown.
func (m *appModel) reviewSurface() *view.Model {
	if ws := m.activeReview(); ws != nil && ws.mode == view.ModeFiles && !ws.inspecting {
		return ws.current()
	}
	return nil
}

func (m *appModel) reviewFocused() bool {
	model := m.reviewModel()
	return model != nil && model.Focused()
}

func (m *appModel) reviewCapturing() bool { return m.reviewFocused() && m.reviewModel().Capturing() }

func (m *appModel) reviewTargetLabel(owner string) string {
	if m.supervisor == nil {
		return ""
	}
	focus := m.paneFocus()
	if focus == "" {
		focus = owner
	}
	runner := m.supervisor.GetRunner(focus)
	if runner == nil || runner.App == nil || runner.App.Session() == nil {
		return ""
	}
	label := fallbackString(runner.App.Session().TitleSnapshot(), "current session")
	if attached := runner.App.AttachedSubagent(); attached != nil {
		label += " › " + string(attached.NodeID)
	}
	return label
}

func (m *appModel) reviewCanDock() bool {
	return !m.leanMode && !m.panePresentationEnabled() && m.width >= gitReviewDockMinWidth
}

func (m *appModel) reviewDocked() bool {
	ws := m.activeReview()
	return ws != nil && ws.mode == view.ModeFiles && !ws.inspecting && ws.docked && m.reviewCanDock()
}

func (m *appModel) reviewDockChatWidth() int {
	return min(max(gitReviewDockChatMin, m.width*45/100), m.width-gitReviewDockReviewMin-1)
}

// reviewOwnsPointer reports whether a content-area pointer event belongs to review.
// Undocked review owns the whole content area so nothing reaches hidden chat.
func (m *appModel) reviewOwnsPointer(x, y int) bool {
	if m.reviewSurface() == nil || y < m.mainContentInset() || y >= m.contentHeight {
		return false
	}
	return !m.reviewDocked() || x >= m.reviewDockChatWidth()
}

func (m *appModel) reviewHandleMouse(msg tea.MouseMsg) tea.Cmd {
	_, cmd := m.reviewModel().HandleMouse(msg)
	m.viewCacheValid = false
	return cmd
}

func (m *appModel) reviewHandleWheelDelta(x, y, delta int) {
	model := m.reviewModel()
	if model == nil {
		return
	}
	model.HandleWheelDelta(x, y, delta)
	m.viewCacheValid = false
}

// blurReviewForChat hands keyboard focus to the visible docked chat after a click there.
func (m *appModel) blurReviewForChat(msg tea.MouseMsg) {
	if _, click := msg.(tea.MouseClickMsg); click && m.reviewDocked() && msg.Mouse().Y >= m.mainContentInset() && msg.Mouse().Y < m.contentHeight {
		m.reviewModel().Blur()
		m.viewCacheValid = false
	}
}

// layoutGitReview sizes review (and the docked chat) after the content area changes.
func (m *appModel) layoutGitReview() tea.Cmd {
	ws := m.activeReview()
	if ws == nil || ws.mode != view.ModeFiles || ws.inspecting {
		return nil // inspectors are sized by the chat page with the transcript
	}
	if m.reviewDocked() {
		chatWidth := m.reviewDockChatWidth()
		ws.current().SetSize(m.width-chatWidth-1, m.mainContentHeight())
		ws.current().SetPosition(chatWidth+1, m.mainContentInset())
		if origin, ok := m.chatPage.(interface{ SetMainContentInset(inset int) }); ok {
			origin.SetMainContentInset(m.mainContentInset())
		}
		if owner, ok := m.chatPage.(interface{ SetSidebarPresentationExtent(height int) }); ok {
			owner.SetSidebarPresentationExtent(m.sidebarContinuationHeight())
		}
		return m.chatPage.SetSize(chatWidth, m.mainContentHeight())
	}
	ws.current().SetSize(m.width, m.mainContentHeight())
	ws.current().SetPosition(0, m.mainContentInset())
	return nil
}

func (m *appModel) reviewContentView() string {
	model := m.reviewModel()
	if !m.reviewDocked() {
		return model.View()
	}
	chatWidth := m.reviewDockChatWidth()
	sep := strings.TrimSuffix(strings.Repeat(styles.MutedStyle.Render("│")+"\n", m.mainContentHeight()), "\n")
	return lipgloss.JoinHorizontal(lipgloss.Top, paneClipped(m.chatPage.View(), chatWidth, m.mainContentHeight()), sep, model.View())
}

func (m *appModel) captureReviewFocus() gitReviewFocus {
	return gitReviewFocus{panel: m.focusedPanel, element: m.panelFocused, contextBar: m.editor.IsContextBarFocused()}
}

func (m *appModel) focusReview() {
	model := m.reviewModel()
	if model == nil {
		return
	}
	m.editor.SetContextBarFocused(false)
	m.editor.Blur()
	if m.messageBar != nil {
		m.messageBar.SetFocused(false)
	}
	m.panelFocused = ""
	m.focusedPanel = PanelContent
	m.chatPage.BlurMessages()
	model.Focus()
	m.viewCacheValid = false
}

// focusContent gives content focus to review while it is open, else to the transcript.
// With chat beside review, the visible transcript comes first in the Tab ring.
func (m *appModel) focusContent() tea.Cmd {
	if m.reviewModel() != nil {
		if m.reviewDocked() {
			return m.focusDockedChat()
		}
		m.focusReview()
		return nil
	}
	return m.chatPage.FocusMessages()
}

func (m *appModel) focusDockedChat() tea.Cmd {
	m.reviewModel().Blur()
	m.focusedPanel = PanelContent
	m.viewCacheValid = false
	return m.chatPage.FocusMessages()
}

// focusBeforeReview moves Shift+Tab focus back to the target before review:
// the docked chat when visible, else the context bar or composer.
func (m *appModel) focusBeforeReview() tea.Cmd {
	if m.reviewDocked() && m.reviewFocused() {
		return m.focusDockedChat()
	}
	return m.focusComposerFromContent()
}

func (m *appModel) focusComposerFromContent() tea.Cmd {
	if model := m.reviewModel(); model != nil {
		model.Blur()
	}
	if ws := m.activeReview(); ws != nil && m.reviewModel() != nil {
		if m.editor.HasContextBar() {
			m.chatPage.BlurMessages()
			m.focusedPanel = PanelEditor
			m.editor.SetContextBarFocused(true)
			m.editor.Blur()
			return nil
		}
		return m.startReviewGeneralInput(ws.anchor)
	}
	m.chatPage.BlurMessages()
	m.focusedPanel = PanelEditor
	if m.editor.HasContextBar() {
		m.editor.SetContextBarFocused(true)
		m.editor.Blur()
		return nil
	}
	return m.editor.Focus()
}

func (m *appModel) restoreReviewFocus(f gitReviewFocus) tea.Cmd {
	switch f.panel {
	case PanelContent:
		m.focusedPanel = PanelContent
		return m.chatPage.FocusMessages()
	case PanelMessageBar:
		if m.messageBar != nil && m.messageBar.HasActions() {
			m.messageBar.SetFocused(true)
			m.focusedPanel = PanelMessageBar
			return nil
		}
	case PanelStatus:
		for _, zone := range m.panelFrame().Zones {
			if zone.ID == f.element {
				m.panelFocused = f.element
				m.focusedPanel = PanelStatus
				return nil
			}
		}
	case PanelEditor:
		if f.contextBar && m.editor.HasContextBar() {
			m.focusedPanel = PanelEditor
			m.editor.SetContextBarFocused(true)
			return nil
		}
	}
	m.focusedPanel = PanelEditor
	return m.editor.Focus()
}

// openGitReview shows review for the focused workspace; files selects Files mode.
func (m *appModel) openGitReview(files bool) tea.Cmd {
	if m.dialogMgr.Open() || m.opening != nil || m.chatPage.IsInlineEditing() || m.chatPage.IsTitleEditing() {
		return notification.InfoCmd("Close the current dialog or edit before opening review")
	}
	anchor := m.reviewAnchor()
	if anchor == "" || m.supervisor == nil {
		return notification.InfoCmd("No session workspace is available for review")
	}
	runner := m.supervisor.GetRunner(anchor)
	if runner == nil || runner.App == nil || runner.App.Session() == nil {
		return notification.InfoCmd("Session workspace unavailable for review")
	}
	sess := runner.App.Session()
	if sess.WorkingDir == "" {
		return notification.InfoCmd("This session has no workspace folder to review")
	}
	var cmds []tea.Cmd
	if m.gitReviewOwner != "" && m.gitReviewOwner != anchor {
		cmds = append(cmds, m.hideGitReview())
	}
	if m.gitReviews == nil {
		m.gitReviews = make(map[string]*gitReviewWorkspace)
	}
	ws := m.gitReviews[anchor]
	if ws == nil {
		ws = newGitReviewWorkspace(anchor)
		ws.splitPref = m.gitDiffSplit
		m.gitReviews[anchor] = ws
	}
	route, _ := m.supervisor.RouteGeneration(anchor)
	scopeChanged := ws.sessionID != sess.ID || filepath.Clean(ws.workingDir) != filepath.Clean(sess.WorkingDir) || ws.route != route
	if scopeChanged {
		// Review data never outlives its scope; the user's draft does.
		m.detachGitReviewScope(ws)
		draft, preview, sent, splitPref := ws.draft, ws.preview, ws.sent, ws.splitPref
		*ws = *newGitReviewWorkspace(anchor)
		ws.draft, ws.preview, ws.sent, ws.splitPref = draft, preview, sent, splitPref
		ws.sessionID, ws.workingDir, ws.route = sess.ID, sess.WorkingDir, route
	}
	mode := view.ModeChanges
	if files {
		mode = view.ModeFiles
	}
	standaloneFiles := mode == view.ModeFiles
	if ws.open && m.gitReviewOwner == anchor && ws.inspecting && (standaloneFiles || ws.mode != mode) {
		cmds = append(cmds, m.hideGitInspector(ws))
	}
	if ws.open && m.gitReviewOwner == anchor && ws.mode != mode {
		ws.current().Blur()
	}
	_, existed := ws.models[mode]
	ws.mode = mode
	ws.sidebarMode = mode
	ws.scopePane = !standaloneFiles
	if standaloneFiles {
		ws.inspecting = false
		if model := ws.models[view.ModeFiles]; model != nil {
			model.SetInspector(false)
		}
	}
	if !existed {
		model := ws.current()
		if ws.snapshot != nil {
			cmds = append(cmds, model.SetSnapshot(*ws.snapshot))
		}
		model.SetSession(ws.session)
		model.SetDraft(ws.draftView())
	}
	if !ws.open || m.gitReviewOwner != anchor {
		ws.restore = m.captureReviewFocus()
		ws.open = true
		m.gitReviewOwner = anchor
		m.gitReviewGeneration++
		m.cancelGitReviewRequests()
	}
	m.focusReview()
	if page, ok := m.gitPage(anchor); ok {
		cmds = append(cmds, page.SetGitMode(mode))
	}
	cmds = append(cmds, m.layoutGitReview(), m.syncReviewSession())
	if policyCmd, changed := m.revalidateGitReviewScope(ws, runner.App); changed {
		cmds = append(cmds, policyCmd)
		m.viewCacheValid = false
		return tea.Batch(cmds...)
	}
	switch {
	case ws.provider != nil && standaloneFiles:
		cmds = append(cmds, m.filesRootRequest(ws))
	case ws.provider != nil:
		cmds = append(cmds, m.submitGitReviewRequest(view.RefreshRequestMsg{Owner: anchor}))
	default:
		cmds = append(cmds, m.loadGitReviewProvider(anchor, runner.App))
	}
	m.viewCacheValid = false
	return tea.Batch(cmds...)
}

func (m *appModel) gitReviewProviderFactory() gitReviewProviderFactory {
	if m.gitReviewFactory != nil {
		return m.gitReviewFactory
	}
	return selectGitReviewProvider
}

func (m *appModel) loadGitReviewProvider(anchor string, a *app.App) tea.Cmd {
	return m.loadGitReviewIdentity(anchor, a)
}

// startGitReviewBootstrap reads the workspace's first snapshot, through the shared
// scope when one is attached. Files reads only the root; Changes scans.
func (m *appModel) startGitReviewBootstrap(ws *gitReviewWorkspace, scope *gitReviewScope, a *app.App, browse bool) tea.Cmd {
	ws.loadBrowse = browse
	m.setGitReviewLease(ws, !browse)
	ctx, token := m.beginGitReviewRequest("provider")
	factory := m.gitReviewProviderFactory()
	generation, anchor, sessionID, workingDir := m.gitReviewGeneration, ws.anchor, ws.sessionID, ws.workingDir
	return func() tea.Msg {
		result := gitReviewTaskResult{owner: anchor, key: "provider", token: token, generation: generation}
		var provider gitreview.Provider
		var unavailable *view.Snapshot
		var err error
		if scope != nil {
			provider, unavailable, err = scope.open(factory, a, anchor, sessionID, workingDir)
		} else {
			provider, unavailable, err = factory(a, sessionID, workingDir)
		}
		switch {
		case err != nil:
			result.err = err
		case unavailable != nil:
			result.snapshot = unavailable
		default:
			snapshot, err := reviewBootstrapSnapshot(provider, browse)(ctx)
			if err != nil {
				result.err = err
				break
			}
			v := reviewSnapshot(snapshot)
			result.provider, result.snapshot = provider, &v
		}
		return result
	}
}

func (m *appModel) beginGitReviewRequest(key string) (context.Context, uint64) {
	if old, ok := m.gitReviewPending[key]; ok {
		old.cancel()
	}
	if m.gitReviewPending == nil {
		m.gitReviewPending = make(map[string]gitReviewRequest)
	}
	m.gitReviewToken++
	ctx, cancel := context.WithCancel(m.ctx())
	req := gitReviewRequest{token: m.gitReviewToken, cancel: cancel}
	if key == "doc" {
		if ws := m.activeReview(); ws != nil {
			if model := ws.models[view.ModeChanges]; model != nil {
				req.key, _ = model.OpenKey()
			}
		}
	}
	m.gitReviewPending[key] = req
	return ctx, m.gitReviewToken
}

func (m *appModel) cancelGitReviewRequests() {
	cancelledDirectories := false
	for key, req := range m.gitReviewPending {
		req.cancel()
		if strings.HasPrefix(key, "dir:") {
			cancelledDirectories = true
		}
	}
	clear(m.gitReviewPending)
	if cancelledDirectories {
		if ws := m.activeReview(); ws != nil {
			ws.directoryGeneration++
		}
	}
}

// validGitReviewScope reports whether results for owner still match what is shown.
func (m *appModel) validGitReviewScope(owner string, generation uint64) bool {
	ws := m.activeReview()
	if ws == nil || m.gitReviewOwner != owner || m.gitReviewGeneration != generation || m.supervisor == nil {
		return false
	}
	runner := m.supervisor.GetRunner(owner)
	if runner == nil || runner.App == nil || runner.App.Session() == nil {
		return false
	}
	sess := runner.App.Session()
	if sess.ID != ws.sessionID || filepath.Clean(sess.WorkingDir) != filepath.Clean(ws.workingDir) {
		return false
	}
	route, ok := m.supervisor.RouteGeneration(owner)
	return ok && route == ws.route
}

func (m *appModel) clearReviewHint(ws *gitReviewWorkspace) tea.Cmd {
	if ws == nil || ws.hint.Generation == 0 || m.messageBar == nil {
		return nil
	}
	cmd := m.messageBar.ClearOwned(ws.hint)
	ws.hint, ws.hintText = messagebar.Token{}, ""
	return cmd
}

// hideGitReview removes review from screen but keeps it open for its workspace.
func (m *appModel) hideGitReview() tea.Cmd {
	ws := m.activeReview()
	if ws == nil {
		return nil
	}
	m.cancelGitReviewRequests()
	ws.current().Blur()
	// Cancelled scans must not leave the sidebar spinning.
	cmds := []tea.Cmd{m.clearReviewHint(ws), m.hideGitInspector(ws), m.pushGitSidebar(ws)}
	docked := m.reviewDocked()
	m.gitReviewOwner = ""
	m.gitReviewGeneration++
	if docked {
		cmds = append(cmds, m.resizePanes())
	}
	m.viewCacheValid = false
	return tea.Batch(cmds...)
}

func (m *appModel) closeGitReview(owner string) tea.Cmd {
	ws := m.activeReview()
	if ws == nil || owner != m.gitReviewOwner {
		return nil
	}
	hadFocus := m.focusedPanel == PanelContent
	if ws.mode == view.ModeChanges {
		// Closing the inspector keeps Git data live for the sidebar.
		cmd := m.hideGitInspector(ws)
		if hadFocus {
			return tea.Batch(cmd, m.restoreReviewFocus(ws.restore))
		}
		return cmd
	}
	cmds := []tea.Cmd{m.hideGitReview()}
	ws.open, ws.docked = false, false
	if hadFocus {
		cmds = append(cmds, m.restoreReviewFocus(ws.restore))
	}
	return tea.Batch(cmds...)
}

func (m *appModel) toggleGitReviewDock(owner string) tea.Cmd {
	ws := m.activeReview()
	if ws == nil || owner != m.gitReviewOwner {
		return nil
	}
	if !ws.docked && !m.reviewCanDock() {
		return notification.InfoCmd(fmt.Sprintf("Chat beside review needs a single-session workspace at least %d columns wide", gitReviewDockMinWidth))
	}
	ws.docked = !ws.docked
	m.viewCacheValid = false
	if !ws.docked {
		return tea.Batch(m.resizePanes(), m.layoutGitReview())
	}
	return m.layoutGitReview()
}

// syncGitReview runs after every update: it follows workspace switches, scope
// changes and session activity without touching hidden workspaces.
func (m *appModel) syncGitReview() tea.Cmd {
	m.sweepGitReviewWorkspaces()
	var policy tea.Cmd
	if ws := m.activeReview(); ws != nil {
		if a := m.gitReviewApp(ws.anchor); a != nil {
			policy, _ = m.revalidateGitReviewScope(ws, a)
		}
	}
	return tea.Batch(policy, m.syncGitReviewState())
}

func (m *appModel) syncGitReviewState() tea.Cmd {
	mark := m.markGitSidebar()
	watchCmd := tea.Cmd(nil)
	if owner := m.paneFocus(); owner != "" {
		if m.gitRefreshOwner != "" && owner != m.gitRefreshOwner {
			m.invalidateGitRefresh(m.gitRefreshOwner)
		}
		if page, ok := m.gitPage(owner); ok {
			watchCmd = m.handleGitWatch(page.GitWatchState())
		} else if owner != m.gitRefreshOwner {
			m.gitRefreshOwner = owner
		}
	}
	if m.gitReviewOwner == "" && len(m.gitReviews) == 0 {
		return tea.Batch(mark, watchCmd)
	}
	if anchor := m.gitScopeKey(); anchor != m.gitReviewOwner {
		return tea.Batch(mark, watchCmd, m.switchGitReviewWorkspace(anchor))
	}
	ws := m.activeReview()
	if ws == nil {
		return tea.Batch(mark, watchCmd)
	}
	if !m.validGitReviewScope(m.gitReviewOwner, m.gitReviewGeneration) {
		// The workspace now shows another session or folder: never reuse its data.
		return m.closeGitReview(m.gitReviewOwner)
	}
	return tea.Batch(mark, watchCmd, m.syncReviewSession())
}

func (m *appModel) switchGitReviewWorkspace(anchor string) tea.Cmd {
	hadFocus := m.focusedPanel == PanelContent
	cmds := []tea.Cmd{m.hideGitReview()}
	ws := m.gitReviews[anchor]
	if anchor == "" || ws == nil || !ws.open {
		if hadFocus {
			cmds = append(cmds, m.chatPage.FocusMessages())
		}
		return tea.Batch(cmds...)
	}
	m.gitReviewOwner = anchor
	m.gitReviewGeneration++
	if !m.validGitReviewScope(anchor, m.gitReviewGeneration) {
		ws.open = false
		m.gitReviewOwner = ""
		return tea.Batch(cmds...)
	}
	if hadFocus {
		m.focusReview()
	} else {
		ws.current().Blur()
	}
	m.viewCacheValid = false
	return tea.Batch(append(cmds, m.layoutGitReview(), m.syncReviewSession())...)
}

// syncReviewSession keeps the review's session label and composer target current.
func (m *appModel) syncReviewSession() tea.Cmd {
	ws := m.activeReview()
	if ws == nil || m.supervisor == nil {
		return nil
	}
	var s view.Session
	if runner := m.supervisor.GetRunner(m.gitReviewOwner); runner != nil && runner.App != nil && runner.App.Session() != nil {
		s.Title = runner.App.Session().TitleSnapshot()
	}
	if page := m.chatPages[m.gitReviewOwner]; page != nil {
		s.Working = page.IsWorking()
	}
	if ws.snapshot != nil {
		// The composer stays with the focused session; mark any other folder as a mismatch.
		s.WorktreeID = "composer-outside-review-scope"
		if m.application != nil {
			if composer := m.application.Session(); composer != nil && filepath.Clean(composer.WorkingDir) == filepath.Clean(ws.workingDir) {
				s.WorktreeID = reviewCurrentWorktree(*ws.snapshot)
			}
		}
	}
	if s != ws.session {
		ws.session = s
		ws.each(func(model *view.Model) { model.SetSession(s) })
		m.viewCacheValid = false
	}
	return m.syncReviewMismatch()
}

func (m *appModel) syncReviewMismatch() tea.Cmd {
	ws := m.activeReview()
	if ws == nil || m.messageBar == nil {
		return nil
	}
	// One concise target line, only while typing, and only when the reviewed
	// checkout is not the session's own folder.
	_, mismatch := ws.current().TargetMismatch()
	visible := ws.mode != view.ModeChanges || ws.inspecting
	if !visible || !mismatch || m.focusedPanel != PanelEditor || m.editor.IsContextBarFocused() {
		return m.clearReviewHint(ws)
	}
	text := "sends to " + fallbackString(ws.session.Title, "this session")
	if text == ws.hintText && ws.hint.Generation != 0 {
		return nil
	}
	token, cmd, ok := m.messageBar.SetNotice(messagebar.Message{Text: text, Severity: messagebar.Info, Category: messagebar.Hint, Owner: gitReviewHintOwner}, time.Time{})
	if ok {
		ws.hint, ws.hintText = token, text
	}
	return cmd
}

func fallbackString(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (m *appModel) invalidateGitDirectories(ws *gitReviewWorkspace) {
	if ws == nil {
		return
	}
	cancelled := false
	for key, req := range m.gitReviewPending {
		if strings.HasPrefix(key, "dir:") {
			req.cancel()
			delete(m.gitReviewPending, key)
			cancelled = true
		}
	}
	if cancelled {
		ws.directoryGeneration++
	}
	clear(ws.directories)
	if model := ws.models[view.ModeFiles]; model != nil {
		model.ClearDirectories()
	}
}

func (m *appModel) submitGitReviewRequest(msg tea.Msg) tea.Cmd {
	ws := m.activeReview()
	if ws == nil || ws.provider == nil {
		return nil
	}
	var owner, key string
	var documentKey view.DocumentKey
	delay := time.Duration(0)
	switch x := msg.(type) {
	case gitDiscoveryContinuationRequest:
		owner, key = x.Owner, "snapshot.continue"
		if _, pending := m.gitReviewPending[key]; pending {
			return nil
		}
	case view.RefreshRequestMsg:
		owner, key = x.Owner, "snapshot"
		if _, pending := m.gitReviewPending[key]; pending {
			return nil
		}
		m.invalidateGitDirectories(ws)
		for pending, req := range m.gitReviewPending {
			if pending != "snapshot" {
				req.cancel()
				delete(m.gitReviewPending, pending)
			}
		}
	case view.LoadWorktreeRequestMsg:
		owner, key = x.Owner, "worktree:"+x.WorktreeID
		if _, pending := m.gitReviewPending[key]; pending {
			return nil
		}
		if x.Refresh {
			if req, ok := m.gitReviewPending["doc"]; ok && req.key.WorktreeID == x.WorktreeID {
				req.cancel()
				delete(m.gitReviewPending, "doc")
			}
		}
	case view.LoadDirectoryRequestMsg:
		owner, key = x.Owner, "dir:"+x.WorktreeID+"\x00"+x.Path
		if _, pending := m.gitReviewPending[key]; pending {
			return nil
		}
	case view.OpenDocumentRequestMsg:
		// One document slot: a newer selection cancels the older request.
		owner, key, delay, documentKey = x.Owner, "doc", gitReviewDocumentDelay, x.Key
	default:
		return nil
	}
	if owner != m.gitReviewOwner || !m.validGitReviewScope(owner, m.gitReviewGeneration) {
		return nil
	}
	if request, ok := msg.(view.LoadDirectoryRequestMsg); ok {
		if ws.directories == nil {
			ws.directories = map[string]view.Directory{}
		}
		directory := view.Directory{WorktreeID: request.WorktreeID, Path: request.Path, State: view.StateLoading}
		ws.directories[request.WorktreeID+"\x00"+request.Path] = directory
		if model := ws.models[view.ModeFiles]; model != nil {
			model.SetDirectory(directory)
		}
	}
	provider := ws.provider
	// A refresh in Files re-reads the root only; Changes keeps the full scan.
	browse := ws.sidebarMode == view.ModeFiles
	if _, refresh := msg.(view.RefreshRequestMsg); refresh {
		ws.snapshotBrowse = browse
	}
	ctx, token := m.beginGitReviewRequest(key)
	if key == "doc" {
		req := m.gitReviewPending[key]
		req.key = documentKey
		m.gitReviewPending[key] = req
	}
	result := gitReviewTaskResult{request: msg, owner: owner, key: key, token: token, generation: m.gitReviewGeneration, browse: browse}
	if ws.snapshot != nil {
		result.snapshotGeneration = ws.snapshot.Generation
	}
	work := func() tea.Msg {
		if delay > 0 {
			select {
			case <-ctx.Done():
				result.err = ctx.Err()
				return result
			case <-time.After(delay):
			}
		}
		switch x := msg.(type) {
		case view.RefreshRequestMsg:
			s, err := reviewScanSnapshot(provider, browse)(ctx)
			if result.err = err; err == nil {
				v := reviewSnapshot(s)
				result.snapshot = &v
			}
		case gitDiscoveryContinuationRequest:
			continuation, ok := provider.(gitDiscoveryContinuation)
			if !ok || !continuation.CanContinueDiscovery() {
				result.err = errors.New("this Git review provider cannot continue discovery")
				break
			}
			s, err := continuation.ContinueDiscovery(ctx)
			if result.err = err; err == nil {
				v := reviewSnapshot(s)
				result.snapshot = &v
			}
		case view.LoadWorktreeRequestMsg:
			var w gitreview.Worktree
			var err error
			if x.Refresh {
				if refresher, ok := provider.(interface {
					RefreshWorktree(ctx context.Context, id string) (gitreview.Worktree, error)
				}); ok {
					w, err = refresher.RefreshWorktree(ctx, x.WorktreeID)
				} else {
					w, err = provider.LoadWorktree(ctx, x.WorktreeID)
				}
			} else {
				w, err = provider.LoadWorktree(ctx, x.WorktreeID)
			}
			if result.err = err; err == nil {
				v := reviewWorktree(w)
				result.worktree = &v
			}
		case view.LoadDirectoryRequestMsg:
			d, err := provider.Directory(ctx, x.WorktreeID, x.Path)
			if result.err = err; err == nil {
				v := reviewDirectory(d)
				// Shared normalises the path; the view keys listings by what was requested.
				v.WorktreeID, v.Path = x.WorktreeID, x.Path
				result.directory = &v
			}
		case view.OpenDocumentRequestMsg:
			d, err := provider.Document(ctx, gitReviewKey(x.Key))
			if result.err = err; err == nil {
				v := reviewDocument(d)
				result.document = &v
			}
		}
		return result
	}
	workCmd := tea.Cmd(work)
	switch msg.(type) {
	case view.LoadDirectoryRequestMsg, view.RefreshRequestMsg:
		return tea.Batch(workCmd, m.pushGitSidebar(ws))
	}
	return workCmd
}

func (m *appModel) applyGitReviewTask(result gitReviewTaskResult) tea.Cmd {
	req, ok := m.gitReviewPending[result.key]
	if !ok || req.token != result.token {
		// Superseded: an older result must never cancel or overwrite newer work.
		return nil
	}
	delete(m.gitReviewPending, result.key)
	req.cancel()
	if !m.validGitReviewScope(result.owner, result.generation) {
		return nil
	}
	ws := m.activeReview()
	if result.snapshot == nil && result.snapshotGeneration != 0 && (ws.snapshot == nil || ws.snapshot.Generation != result.snapshotGeneration) {
		return nil
	}
	if result.err != nil {
		if errors.Is(result.err, context.Canceled) {
			return nil
		}
		cmd := m.applyGitReviewError(ws, result)
		if result.refreshDocument != nil {
			if model := ws.models[result.refreshDocument.Mode]; model != nil {
				if key, open := model.OpenKey(); open && key == *result.refreshDocument {
					cmd = tea.Batch(cmd, model.ReloadDocument(*result.refreshDocument))
				}
			}
		}
		return tea.Batch(cmd, m.pushGitSidebar(ws))
	}
	var cmds []tea.Cmd
	if result.provider != nil {
		ws.provider = result.provider
	}
	if result.snapshot != nil {
		var expanded []string
		if page, ok := m.gitPage(ws.anchor); ok {
			expanded = page.GitWatchState().ExpandedWorktrees
		}
		ws.snapshot = result.snapshot
		cmds = append(cmds, m.prewarmGitSidebarMetadata(ws))
		ws.each(func(model *view.Model) { cmds = append(cmds, model.SetSnapshot(*result.snapshot)) })
		cmds = append(cmds, m.syncReviewSession())
		// Only a full scan re-reads Git status; a root read leaves expanded worktrees alone.
		if _, refresh := result.request.(view.RefreshRequestMsg); refresh && !result.browse {
			for _, id := range expanded {
				cmds = append(cmds, m.submitGitReviewRequest(view.LoadWorktreeRequestMsg{Owner: ws.anchor, WorktreeID: id, Refresh: true}))
			}
		}
	}
	if result.worktree != nil && ws.snapshot != nil {
		next := cloneReviewSnapshot(*ws.snapshot)
		for i := range next.Repositories {
			for j := range next.Repositories[i].Worktrees {
				if next.Repositories[i].Worktrees[j].ID == result.worktree.ID {
					next.Repositories[i].Worktrees[j] = *result.worktree
				}
			}
		}
		ws.snapshot = &next
		ws.each(func(model *view.Model) { cmds = append(cmds, model.SetSnapshot(next)) })
		if req, ok := result.request.(view.LoadWorktreeRequestMsg); ok && req.Refresh && result.refreshDocument == nil {
			if model := ws.models[ws.mode]; model != nil {
				if key, open := model.OpenKey(); open && key.WorktreeID == req.WorktreeID {
					cmds = append(cmds, model.ReloadDocument(key))
				}
			}
		}
	}
	if result.refreshDocument != nil && result.worktree != nil {
		if model := ws.models[result.refreshDocument.Mode]; model != nil {
			if key, open := model.OpenKey(); open && key == *result.refreshDocument {
				cmds = append(cmds, model.ReloadDocument(*result.refreshDocument))
			}
		}
	}
	if result.directory != nil {
		if ws.directories == nil {
			ws.directories = map[string]view.Directory{}
		}
		ws.directories[result.directory.WorktreeID+"\x00"+result.directory.Path] = *result.directory
		if model := ws.models[view.ModeFiles]; model != nil {
			cmds = append(cmds, model.SetDirectory(*result.directory))
		}
	}
	if result.document != nil {
		if model := ws.models[result.document.Key.Mode]; model != nil {
			cmds = append(cmds, model.SetDocument(*result.document))
		}
	}
	if result.snapshot != nil && ws.snapshot != nil {
		// A root-only result that arrives after Changes was selected still scans.
		cmds = append(cmds, m.ensureChangesScan(ws))
	}
	cmds = append(cmds, m.fanoutGitReviewScope(ws))
	m.viewCacheValid = false
	return tea.Batch(append(cmds, m.pushGitSidebar(ws))...)
}

// applyGitReviewError ends the loading state of whatever failed, so nothing
// stays on "Loading…" forever.
func (m *appModel) applyGitReviewError(ws *gitReviewWorkspace, result gitReviewTaskResult) tea.Cmd {
	text := result.err.Error()
	switch req := result.request.(type) {
	case view.OpenDocumentRequestMsg:
		if model := ws.models[req.Key.Mode]; model != nil {
			model.SetDocument(view.Document{Key: req.Key, State: view.StateError, Err: text})
		}
	case view.LoadDirectoryRequestMsg:
		directory := view.Directory{WorktreeID: req.WorktreeID, Path: req.Path, State: view.StateError, Err: text}
		if ws.directories == nil {
			ws.directories = map[string]view.Directory{}
		}
		ws.directories[directory.WorktreeID+"\x00"+directory.Path] = directory
		if model := ws.models[view.ModeFiles]; model != nil {
			model.SetDirectory(directory)
		}
		return nil
	case view.LoadWorktreeRequestMsg:
		if req.Refresh {
			if result.refreshDocument != nil {
				return nil
			}
			if model := ws.models[ws.mode]; model != nil {
				if key, open := model.OpenKey(); open && key.WorktreeID == req.WorktreeID {
					return model.ReloadDocument(key)
				}
			}
			return nil // keep last good status and allow the next visible refresh to retry
		}
		if ws.snapshot == nil {
			break
		}
		next := cloneReviewSnapshot(*ws.snapshot)
		for i := range next.Repositories {
			for j := range next.Repositories[i].Worktrees {
				if wt := &next.Repositories[i].Worktrees[j]; wt.ID == req.WorktreeID {
					wt.State, wt.Err = view.StateError, text
				}
			}
		}
		ws.snapshot = &next
		var cmds []tea.Cmd
		ws.each(func(model *view.Model) { cmds = append(cmds, model.SetSnapshot(next)) })
		return tea.Batch(cmds...)
	default:
		if ws.snapshot == nil {
			unavailable := view.Snapshot{Generation: 1, Scope: view.Scope{Label: ws.workingDir, Unavailable: "Review unavailable: " + text}}
			ws.snapshot = &unavailable
			var cmds []tea.Cmd
			ws.each(func(model *view.Model) { cmds = append(cmds, model.SetSnapshot(unavailable)) })
			return tea.Batch(cmds...)
		}
		return notification.ErrorCmd("Review refresh failed: " + text)
	}
	m.viewCacheValid = false
	return nil
}

func cloneReviewSnapshot(s view.Snapshot) view.Snapshot {
	s.Repositories = append([]view.Repository(nil), s.Repositories...)
	for i := range s.Repositories {
		s.Repositories[i].Worktrees = append([]view.Worktree(nil), s.Repositories[i].Worktrees...)
	}
	return s
}

func reviewCurrentWorktree(snapshot view.Snapshot) string {
	for _, repo := range snapshot.Repositories {
		for _, wt := range repo.Worktrees {
			if wt.Current {
				return wt.ID
			}
		}
	}
	return ""
}

func (m *appModel) handleReviewComponentMessage(msg tea.Msg) tea.Cmd {
	switch x := msg.(type) {
	case view.CommentInputRequestMsg:
		return m.startReviewComment(x)
	case view.GeneralCommentRequestMsg:
		return m.startReviewGeneralInput(x.Owner)
	case view.OpenExternalEditorRequestMsg:
		return m.openReviewExternalEditor(x)
	case view.CounterpartRequestMsg:
		return m.openReviewCounterpart(x)
	case view.CommentResultMsg:
		if ws := m.gitReviews[x.Owner]; ws != nil {
			for _, model := range ws.models {
				model.HandleCommentResult(x)
			}
		}
		return nil
	case view.RefreshRequestMsg:
		ws := m.gitReviews[x.Owner]
		if ws != nil {
			m.invalidateGitDirectories(ws)
			return tea.Batch(m.pushGitSidebar(ws), m.submitGitReviewRequest(x))
		}
		return m.submitGitReviewRequest(x)
	case view.LoadWorktreeRequestMsg, view.LoadDirectoryRequestMsg, view.OpenDocumentRequestMsg:
		return m.submitGitReviewRequest(x)
	case view.CloseRequestMsg:
		return m.closeGitReview(x.Owner)
	case view.DockToggleRequestMsg:
		return m.toggleGitReviewDock(x.Owner)
	case view.FocusRequestMsg:
		if x.Owner == m.gitReviewOwner {
			m.focusReview()
		}
	case view.CopyRequestMsg:
		if x.Owner == m.gitReviewOwner {
			return copyToClipboard(x.Text, x.Summary)
		}
	case view.SelectionChangedMsg:
		if x.Owner == m.gitReviewOwner {
			return m.syncReviewMismatch()
		}
	case gitReviewTaskResult:
		return m.applyGitReviewTask(x)
	case view.HighlightedMsg:
		for _, ws := range m.gitReviews {
			ws.each(func(model *view.Model) { model.ApplyHighlight(x) })
		}
	case view.AddCommentMsg, view.EditCommentMsg, view.DeleteCommentMsg, view.PreviewDraftMsg, view.SendDraftMsg, view.CancelPreviewMsg, gitReviewSendResult, dialog.ReviewTargetResultMsg, dialog.ReviewTargetMoreMsg, gitReviewCatalogResult, gitReviewResolved, gitReviewAgents:
		return m.handleReviewDraftMessage(x)
	}
	return nil
}

// handleReviewKey routes keys while review owns content focus. It reports
// whether the key was consumed.
func (m *appModel) handleReviewKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	if handled, cmd := m.updateReviewCommentKey(msg); handled {
		return true, cmd
	}
	model := m.reviewModel()
	if model == nil {
		return false, nil
	}
	if !model.Focused() {
		if m.focusedPanel != PanelContent {
			return false, nil
		}
		if m.reviewDocked() {
			// The docked transcript keeps its own keys; Tab moves on to review.
			switch msg.String() {
			case "tab":
				m.focusReview()
				return true, nil
			case "shift+tab":
				return true, m.focusComposerFromContent()
			}
			return false, nil
		}
		// Undocked review stands in for the hidden transcript; keys never reach it.
		m.focusReview()
	}
	if isReviewCommentKey(msg) && model.CommentSelectionContext() {
		if m.keyReleaseSupported {
			if msg.IsRepeat {
				return true, nil
			}
			m.reviewHoldC = true
			model.BeginRangeSelection()
			m.viewCacheValid = false
			return true, nil
		}
		return true, model.StartComment()
	}
	if m.reviewHoldC && msg.String() == "esc" {
		m.reviewHoldC = false
	}
	handled, cmd := model.HandleKey(msg)
	m.viewCacheValid = false
	if handled || model.Capturing() {
		return true, cmd
	}
	switch msg.String() {
	case "tab":
		m.reviewHoldC = false
		model.Blur()
	case "shift+tab":
		m.reviewHoldC = false
		return true, m.focusBeforeReview()
	}
	return false, nil
}

func (m *appModel) handleGitWatch(msg sidebar.GitWatchMsg) tea.Cmd {
	owner := m.paneFocus()
	if msg.Owner == "" || msg.Owner != owner {
		return nil // a stale/background page cannot cancel the focused page's timer
	}
	if (m.hideSidebar || m.leanMode) && (m.activeReview() == nil || !m.activeReview().inspecting || m.activeReview().anchor != owner) {
		m.invalidateGitRefresh(owner)
		return nil
	}
	ws := m.gitReviews[owner]
	if ws == nil || ws.provider == nil || ws.snapshot == nil {
		m.invalidateGitRefresh(owner)
		return nil
	}
	if _, discovering := m.gitReviewPending["snapshot"]; discovering {
		m.invalidateGitRefresh(owner)
		return nil
	}
	var ids []string
	// Files never runs Git status, so its expanded folders do not schedule it.
	if msg.Open && !m.hideSidebar && !m.leanMode && ws.sidebarMode != view.ModeFiles {
		ids = append(ids, msg.ExpandedWorktrees...)
	}
	// An open inspector is itself visible Git context even when its sidebar
	// section is collapsed or hidden; it refreshes only its known checkout.
	if review := m.activeReview(); review != nil && review.anchor == owner && review.inspecting {
		if model := review.models[view.ModeChanges]; model != nil {
			if key, ok := model.OpenKey(); ok && key.WorktreeID != "" && !slices.Contains(ids, key.WorktreeID) {
				ids = append(ids, key.WorktreeID)
			}
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) == 0 {
		m.invalidateGitRefresh(owner)
		return nil
	}
	scope := owner + "\x00" + strings.Join(ids, "\x00")
	if m.gitRefreshScheduled && scope == m.gitRefreshScope {
		return nil
	}
	m.gitRefreshToken++
	token := m.gitRefreshToken
	m.gitRefreshScheduled, m.gitRefreshScope, m.gitRefreshOwner = true, scope, owner
	tick := m.gitRefreshTimer
	if tick == nil {
		tick = tea.Tick
	}
	return tick(gitAutoRefreshDelay, func(time.Time) tea.Msg { return gitAutoRefreshMsg{owner: owner, token: token} })
}

func (m *appModel) invalidateGitRefresh(owner string) {
	if m.gitRefreshOwner != owner || (!m.gitRefreshScheduled && m.gitRefreshScope == "") {
		return
	}
	m.gitRefreshToken++
	m.gitRefreshScheduled = false
	m.gitRefreshScope = ""
}

func (m *appModel) handleGitAutoRefresh(msg gitAutoRefreshMsg) tea.Cmd {
	if !m.gitRefreshScheduled || msg.token != m.gitRefreshToken || msg.owner != m.paneFocus() {
		return nil
	}
	page, ok := m.gitPage(msg.owner)
	if !ok {
		m.invalidateGitRefresh(msg.owner)
		return nil
	}
	watch := page.GitWatchState()
	if _, discovering := m.gitReviewPending["snapshot"]; discovering {
		m.invalidateGitRefresh(msg.owner)
		return nil
	}
	if m.hideSidebar || m.leanMode || !watch.Open {
		if !(func() bool {
			ws := m.gitReviews[msg.owner]
			return ws != nil && ws.inspecting
		})() {
			m.invalidateGitRefresh(msg.owner)
			return nil
		}
	}
	ws := m.gitReviews[msg.owner]
	if ws == nil || ws.provider == nil || ws.snapshot == nil {
		m.invalidateGitRefresh(msg.owner)
		return nil
	}
	var ids []string
	if watch.Open && !m.hideSidebar && !m.leanMode && ws.sidebarMode != view.ModeFiles {
		ids = append(ids, watch.ExpandedWorktrees...)
	}
	if review := m.activeReview(); review != nil && review.anchor == msg.owner && review.inspecting {
		if model := review.models[view.ModeChanges]; model != nil {
			if key, ok := model.OpenKey(); ok && key.WorktreeID != "" && !slices.Contains(ids, key.WorktreeID) {
				ids = append(ids, key.WorktreeID)
			}
		}
	}
	if len(ids) == 0 {
		m.invalidateGitRefresh(msg.owner)
		return nil
	}
	m.gitRefreshScheduled = false
	var cmds []tea.Cmd
	for _, id := range ids {
		if wt, ok := findReviewWorktree(ws.snapshot, id); ok && (wt.State == view.StateReady || wt.State == view.StateError) {
			cmds = append(cmds, m.submitGitReviewRequest(view.LoadWorktreeRequestMsg{Owner: msg.owner, WorktreeID: id, Refresh: true}))
		}
	}
	cmds = append(cmds, m.handleGitWatch(watch))
	return tea.Batch(cmds...)
}

func findReviewWorktree(snapshot *view.Snapshot, id string) (view.Worktree, bool) {
	if snapshot == nil {
		return view.Worktree{}, false
	}
	for _, repo := range snapshot.Repositories {
		for _, wt := range repo.Worktrees {
			if wt.ID == id {
				return wt, true
			}
		}
	}
	return view.Worktree{}, false
}

func (m *appModel) openReviewCounterpart(msg view.CounterpartRequestMsg) tea.Cmd {
	ws := m.gitReviews[msg.Owner]
	if ws == nil || ws != m.activeReview() || ws.provider == nil {
		return nil
	}
	if msg.Key.Mode != view.ModeChanges && msg.Key.Mode != view.ModeFiles {
		return nil
	}
	paneScoped := ws.scopePane
	if ws.mode != msg.Key.Mode {
		if model := ws.models[ws.mode]; model != nil {
			model.Blur()
		}
		ws.mode = msg.Key.Mode
	}
	ws.scopePane = paneScoped
	ws.sidebarMode = msg.Key.Mode
	model := ws.current()
	if ws.snapshot != nil {
		model.SetSnapshot(*ws.snapshot)
	}
	model.SetDraft(ws.draftView())
	model.SetInspector(true)
	ws.inspecting = true
	ws.scopePane = true
	var pageCmd tea.Cmd
	if page, ok := m.gitPage(ws.anchor); ok {
		pageCmd = page.SetGitMode(msg.Key.Mode)
		page.SetContentOverlay(model)
	}
	model.Focus()
	m.focusReview()
	cmd := model.OpenCounterpart(msg.Key, msg.OldLine, msg.NewLine, msg.Top)
	m.viewCacheValid = false
	return tea.Batch(pageCmd, cmd, m.pushGitSidebar(ws))
}

type reviewExternalEditorDone struct {
	owner, sessionID                    string
	route, generation, reviewGeneration uint64
	key                                 view.DocumentKey
	err                                 error
}

func (m *appModel) openReviewExternalEditor(msg view.OpenExternalEditorRequestMsg) tea.Cmd {
	ws := m.gitReviews[msg.Owner]
	if ws == nil || ws != m.activeReview() || ws.provider == nil || msg.Key.Mode != view.ModeFiles || ws.mode != view.ModeFiles {
		return nil
	}
	model := ws.models[view.ModeFiles]
	if model == nil {
		return nil
	}
	current, open := model.OpenKey()
	if !open || current != msg.Key {
		return nil
	}
	editable, ok := reviewLocalEditor(ws.provider)
	if !ok {
		return notification.InfoCmd("External editing is available only for local workspace files")
	}
	path, err := editable.EditablePath(msg.Key.WorktreeID, msg.Key.Path)
	if err != nil {
		return notification.ErrorCmd("Cannot edit review file: " + err.Error())
	}
	cmd, err := editorname.PrepareCommandFromEnv(os.Getenv("VISUAL"), os.Getenv("EDITOR"), path)
	if err != nil {
		return notification.ErrorCmd("Cannot open external editor: " + err.Error())
	}
	ws.externalEditorGeneration++
	owner, key, sessionID, route, generation, reviewGeneration := msg.Owner, msg.Key, ws.sessionID, ws.route, ws.externalEditorGeneration, m.gitReviewGeneration
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return reviewExternalEditorDone{owner: owner, sessionID: sessionID, route: route, generation: generation, reviewGeneration: reviewGeneration, key: key, err: err}
	})
}

func (m *appModel) finishReviewExternalEditor(msg reviewExternalEditorDone) tea.Cmd {
	ws := m.gitReviews[msg.owner]
	if ws == nil || ws != m.activeReview() || ws.provider == nil || ws.sessionID != msg.sessionID || ws.route != msg.route || ws.externalEditorGeneration != msg.generation || m.gitReviewGeneration != msg.reviewGeneration || ws.mode != view.ModeFiles {
		return nil
	}
	model := ws.models[view.ModeFiles]
	if model == nil {
		return nil
	}
	key, open := model.OpenKey()
	if !open || key != msg.key {
		return nil
	}
	refresh := m.submitGitReviewRequest(view.LoadWorktreeRequestMsg{Owner: msg.owner, WorktreeID: msg.key.WorktreeID, Refresh: true})
	cmd := core.MapCommand(refresh, func(result tea.Msg) tea.Msg {
		if task, ok := result.(gitReviewTaskResult); ok {
			task.refreshDocument = &msg.key
			return task
		}
		return result
	})
	if msg.err != nil {
		return tea.Batch(cmd, notification.ErrorCmd("External editor error: "+msg.err.Error()))
	}
	m.viewCacheValid = false
	return cmd
}
