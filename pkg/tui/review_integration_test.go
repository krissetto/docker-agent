package tui

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/gitreview"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/commands"
	view "github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

type reviewChatPage struct {
	mockChatPage
	keys         int
	working      bool
	focused      bool
	width        int
	git          sidebar.GitChanges
	watch        sidebar.GitWatchMsg
	gitFocused   bool
	gitKeys      int
	revealed     int
	overlay      chat.ContentOverlay
	overlayWidth int
}

func (p *reviewChatPage) SetGitChanges(c sidebar.GitChanges) tea.Cmd { p.git = c; return nil }
func (p *reviewChatPage) SetGitMode(mode view.Mode) tea.Cmd          { p.git.Mode = mode; return nil }
func (p *reviewChatPage) GitWatchState() sidebar.GitWatchMsg {
	if p.watch.Owner != "" {
		return p.watch
	}
	return sidebar.GitWatchMsg{Owner: p.git.Owner}
}
func (p *reviewChatPage) RevealGit() tea.Cmd {
	p.revealed++
	owner := p.git.Owner
	return func() tea.Msg { return sidebar.GitRequestMsg{Owner: owner, Action: sidebar.GitLoad} }
}
func (p *reviewChatPage) GitFocused() bool             { return p.gitFocused }
func (p *reviewChatPage) SetGitFocused(f bool) tea.Cmd { p.gitFocused = f; return nil }
func (p *reviewChatPage) GitKey(tea.KeyPressMsg) (bool, tea.Cmd) {
	p.gitKeys++
	return true, nil
}
func (p *reviewChatPage) SetContentOverlay(o chat.ContentOverlay) {
	p.overlay = o
	if o != nil {
		o.SetSize(p.overlayWidth, 20)
		o.SetPosition(0, 0)
	}
}

func (p *reviewChatPage) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if _, ok := msg.(tea.KeyPressMsg); ok {
		p.keys++
	}
	return p, nil
}
func (p *reviewChatPage) View() string {
	if p.overlay != nil {
		return p.overlay.View()
	}
	return "visible chat transcript"
}
func (p *reviewChatPage) IsWorking() bool                 { return p.working }
func (p *reviewChatPage) FocusMessages() tea.Cmd          { p.focused = true; return nil }
func (p *reviewChatPage) BlurMessages()                   { p.focused = false }
func (p *reviewChatPage) SetSize(width, _ int) tea.Cmd    { p.width = width; return nil }
func (p *reviewChatPage) FocusMessageAt(_, _ int) tea.Cmd { p.focused = true; return nil }

type reviewEditor struct {
	mockEditor
	value         string
	savedValue    string
	keys          int
	focused       bool
	hasContext    bool
	ctxFocused    bool
	commentActive bool
	commentBody   string
}

func (e *reviewEditor) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		e.keys++
		if e.commentActive && key.Text != "" {
			e.commentBody += key.Text
		}
	}
	if paste, ok := msg.(tea.PasteMsg); ok && e.commentActive {
		e.commentBody += paste.Content
	}
	return e, nil
}
func (e *reviewEditor) Value() string {
	if e.commentActive {
		return e.commentBody
	}
	return e.value
}
func (e *reviewEditor) SetValue(v string) {
	if e.commentActive {
		e.commentBody = v
	} else {
		e.value = v
	}
}
func (e *reviewEditor) GetSize() (int, int)                 { return 140, 3 }
func (e *reviewEditor) Focus() tea.Cmd                      { e.focused = true; return nil }
func (e *reviewEditor) Blur() tea.Cmd                       { e.focused = false; return nil }
func (e *reviewEditor) HasContextBar() bool                 { return e.hasContext }
func (e *reviewEditor) IsContextBarFocused() bool           { return e.ctxFocused }
func (e *reviewEditor) SetContextBarFocused(f bool)         { e.ctxFocused = f }
func (e *reviewEditor) TryStartArgumentCompletion() tea.Cmd { return nil }
func (e *reviewEditor) AcceptSuggestion() tea.Cmd           { return nil }
func (e *reviewEditor) BeginCommentInput() bool {
	e.savedValue = e.value
	e.commentActive = true
	e.commentBody = ""
	return true
}
func (e *reviewEditor) CommentInputActive() bool  { return e.commentActive }
func (e *reviewEditor) CommentInputValue() string { return e.commentBody }
func (e *reviewEditor) SetCommentInputValue(v string) {
	if e.commentActive {
		e.commentBody = v
	}
}
func (e *reviewEditor) FinishCommentInput() (string, bool) {
	if !e.commentActive {
		return "", false
	}
	body := e.commentBody
	e.commentActive, e.commentBody = false, ""
	e.value = e.savedValue
	e.savedValue = ""
	return body, true
}

func countString(values []string, want string) int {
	n := 0
	for _, value := range values {
		if value == want {
			n++
		}
	}
	return n
}

type fakeReviewProvider struct {
	fail       bool
	mu         sync.Mutex
	snapshots  int
	browses    int
	documents  []gitreview.DocumentKey
	dirs       []string
	worktrees  []string
	refreshed  []string
	refreshErr bool
}

// BrowseRoot is the root-only bootstrap: it never increments the full-scan count.
func (p *fakeReviewProvider) BrowseRoot(context.Context) (gitreview.Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.browses++
	// Mirrors Local.BrowseRoot: an existing full inventory is reused unchanged.
	if p.snapshots > 0 {
		return p.inventory(uint64(p.snapshots)), nil
	}
	generation := uint64(1)
	return gitreview.Snapshot{Generation: generation, Browse: true, Scope: gitreview.Scope{Label: "/work"}, Repositories: []gitreview.Repository{
		{ID: "r1", Name: "work", Kind: gitreview.KindNone, Worktrees: []gitreview.Worktree{
			{ID: "w1", Path: "/work", Branch: "main", Current: true, State: "unscanned", CountsState: gitreview.StateUnscanned},
		}},
	}}, nil
}

func (p *fakeReviewProvider) Snapshot(context.Context) (gitreview.Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapshots++
	return p.inventory(uint64(p.snapshots)), nil
}

// inventory is the full repository inventory; callers hold p.mu.
func (p *fakeReviewProvider) inventory(generation uint64) gitreview.Snapshot {
	return gitreview.Snapshot{Generation: generation, Scope: gitreview.Scope{Label: "/work"}, Repositories: []gitreview.Repository{
		{ID: "r1", Name: "work", Kind: gitreview.KindPrimary, Worktrees: []gitreview.Worktree{
			{ID: "w1", Path: "/work", Branch: "main", Current: true, State: "unscanned", CountsState: gitreview.StateUnscanned, Files: []gitreview.FileChange{{Path: "Dockerfile.dev", Group: gitreview.GroupUnstaged, Status: "M", Added: 1, Removed: 1}, {Path: "a.go", Group: gitreview.GroupUnstaged, Status: "M", Added: 1}}},
			{ID: "w2", Path: "/work/wt", Branch: "feature", Linked: true, State: "unscanned", CountsState: gitreview.StateUnscanned, Files: []gitreview.FileChange{{Path: "b.go", Group: gitreview.GroupStaged, Status: "A", Added: 2}}},
			{ID: "w3", Path: "/work/det", Detached: true, DetachedHead: "3e336a8", State: "unscanned", CountsState: gitreview.StateUnscanned},
		}},
	}}
}

func (p *fakeReviewProvider) LoadWorktree(_ context.Context, id string) (gitreview.Worktree, error) {
	p.mu.Lock()
	p.worktrees = append(p.worktrees, id)
	p.mu.Unlock()
	path, branch := map[string]string{"w1": "/work", "w2": "/work/wt", "w3": "/work/det"}[id], map[string]string{"w1": "main", "w2": "feature"}[id]
	return gitreview.Worktree{ID: id, Path: path, Branch: branch, Current: id == "w1", State: "clean", CountsState: gitreview.StateReady}, nil
}
func (p *fakeReviewProvider) RefreshWorktree(ctx context.Context, id string) (gitreview.Worktree, error) {
	p.mu.Lock()
	p.refreshed = append(p.refreshed, id)
	fail := p.refreshErr
	p.mu.Unlock()
	if fail {
		return gitreview.Worktree{}, errors.New("temporary status failure")
	}
	return p.LoadWorktree(ctx, id)
}

func (p *fakeReviewProvider) Directory(_ context.Context, wt, path string) (gitreview.Directory, error) {
	p.mu.Lock()
	p.dirs = append(p.dirs, path)
	p.mu.Unlock()
	return gitreview.Directory{WorktreeID: wt, Path: path, State: gitreview.StateReady, Entries: []gitreview.DirectoryEntry{{Name: "README.md"}}}, nil
}
func (p *fakeReviewProvider) Document(_ context.Context, key gitreview.DocumentKey) (gitreview.Document, error) {
	p.mu.Lock()
	p.documents = append(p.documents, key)
	p.mu.Unlock()
	if p.fail {
		return gitreview.Document{}, errors.New("document unavailable")
	}
	doc := gitreview.Document{Key: key, State: gitreview.StateReady, PatchHeader: "diff --git a/" + key.Path + " b/" + key.Path, Lines: []gitreview.DocumentLine{
		{Kind: gitreview.DiffHunk, Text: "@@ -1,2 +1,2 @@"},
		{Kind: gitreview.DiffRemove, Old: 1, Text: "FROM golang:1.22"},
		{Kind: gitreview.DiffAdd, New: 1, Text: "FROM golang:1.27"},
		{Kind: gitreview.DiffContext, Old: 2, New: 2, Text: "WORKDIR /src"},
	}}
	for i := 0; i < 48; i++ {
		n := i + 3
		doc.Lines = append(doc.Lines, gitreview.DocumentLine{Kind: gitreview.DiffContext, Old: n, New: n, Text: fmt.Sprintf("content line %d", n)})
	}
	return doc, nil
}

type reviewHarness struct {
	pageAR   *animation.Runtime
	t        *testing.T
	m        *appModel
	editor   *reviewEditor
	page     *reviewChatPage
	provider *fakeReviewProvider
	sess     *session.Session
	other    []tea.Msg
}

func newReviewHarness(t *testing.T, width int) *reviewHarness {
	t.Helper()
	m, _ := newTestModel(t)
	h := &reviewHarness{t: t, m: m, editor: &reviewEditor{focused: true}, page: &reviewChatPage{overlayWidth: width}, provider: &fakeReviewProvider{}}
	m.gitRefreshTimer = func(_ time.Duration, _ func(time.Time) tea.Msg) tea.Cmd { return func() tea.Msg { return nil } }
	h.sess = &session.Session{ID: "s1", Title: "main session", WorkingDir: "/work"}
	a := app.New(t.Context(), nil, h.sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	sv := supervisor.New(nil)
	_, err := sv.AddSession(t.Context(), a, h.sess, "/work", nil)
	require.NoError(t, err)
	m.supervisor, m.application = sv, a
	m.sessionState = service.NewSessionState(h.sess)
	m.chatPage, m.chatPages["s1"] = h.page, h.page
	m.editor = h.editor
	m.focusedPanel = PanelEditor
	m.ready = true
	m.width, m.height, m.contentHeight = width, 30, 20
	m.ensureMessageBar()
	m.messageBar.SetSize(width, 1)
	m.gitReviewFactory = func(*app.App, string, string) (gitreview.Provider, *view.Snapshot, error) {
		return h.provider, nil, nil
	}
	return h
}

// pump runs commands and feeds review traffic back through the root Update.
func (h *reviewHarness) pump(cmd tea.Cmd) {
	h.t.Helper()
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg := c()
		if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice && v.Type().Elem() == reflect.TypeFor[tea.Cmd]() {
			for i := range v.Len() {
				queue = append(queue, v.Index(i).Interface().(tea.Cmd))
			}
			continue
		}
		switch msg := msg.(type) {
		case nil:
		case gitReviewTaskResult, reviewExternalEditorDone, view.CommentInputRequestMsg, view.CommentResultMsg, view.CounterpartRequestMsg, view.GeneralCommentRequestMsg, view.OpenExternalEditorRequestMsg, view.RefreshRequestMsg, view.LoadWorktreeRequestMsg, view.LoadDirectoryRequestMsg,
			view.OpenDocumentRequestMsg, view.CloseRequestMsg, view.DockToggleRequestMsg, view.FocusRequestMsg,
			view.SelectionChangedMsg, view.HighlightedMsg, view.AddCommentMsg, view.EditCommentMsg, view.DeleteCommentMsg,
			view.PreviewDraftMsg, view.SendDraftMsg, view.CancelPreviewMsg, gitReviewSendResult,
			dialog.OpenDialogMsg, dialog.CloseDialogMsg, sidebar.GitRequestMsg, messages.RevealGitChangesMsg, dialog.ReviewTargetResultMsg, dialog.ReviewTargetMoreMsg, sidebar.GitWatchMsg, gitAutoRefreshMsg, gitReviewCatalogResult, gitReviewResolved, gitReviewAgents, gitReviewIdentityMsg, gitReviewPeerContent:
			_, next := h.m.Update(msg)
			queue = append(queue, next)
		default:
			h.other = append(h.other, msg)
		}
	}
}

// open shows the Files surface, or for Changes reveals the sidebar section
// and opens Dockerfile.dev in the diff inspector.
func (h *reviewHarness) open(files bool) {
	h.t.Helper()
	if files {
		_, cmd := h.m.Update(messages.OpenGitReviewMsg{Files: true})
		h.pump(cmd)
	} else {
		_, cmd := h.m.Update(messages.RevealGitChangesMsg{})
		h.pump(cmd)
		h.openDockerfile()
	}
	require.NotNil(h.t, h.m.reviewModel())
}

// openDockerfile is what a click on the sidebar's Dockerfile.dev row sends.
func (h *reviewHarness) openDockerfile() {
	h.t.Helper()
	_, cmd := h.m.Update(sidebar.GitRequestMsg{Owner: h.m.paneFocus(), Action: sidebar.GitOpen, WorktreeID: "w1", Path: "Dockerfile.dev", Group: view.GroupUnstaged})
	h.pump(cmd)
}

func (h *reviewHarness) key(keys ...string) {
	h.t.Helper()
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "tab":
			msg = tea.KeyPressMsg{Code: tea.KeyTab}
		case "shift+tab":
			msg = tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "end":
			msg = tea.KeyPressMsg{Code: tea.KeyEnd}
		case "ctrl+w":
			msg = tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl}
		default:
			r, _ := utf8.DecodeRuneInString(k)
			msg = tea.KeyPressMsg{Code: r, Text: k}
		}
		_, cmd := h.m.Update(msg)
		h.pump(cmd)
	}
}

func (h *reviewHarness) content() string {
	if h.m.reviewSurface() != nil {
		return ansi.Strip(h.m.reviewContentView())
	}
	return ansi.Strip(h.page.View())
}

func TestReviewCommandsOpenChangesAndFiles(t *testing.T) {
	h := newReviewHarness(t, 140)
	byCommand := map[string]commands.Item{}
	for _, category := range commands.BuildCommandCategories(t.Context(), h.m.application) {
		for _, item := range category.Commands {
			byCommand[item.SlashCommand] = item
		}
	}
	require.Contains(t, byCommand, "/changes")
	require.Contains(t, byCommand, "/files")
	assert.Equal(t, messages.RevealGitChangesMsg{}, byCommand["/changes"].Execute("")())
	assert.Equal(t, messages.OpenGitReviewMsg{Files: true}, byCommand["/files"].Execute("")())

	h.open(true)
	assert.Equal(t, view.ModeFiles, h.m.reviewModel().Mode())
	assert.Contains(t, h.provider.dirs, "", "Files mode requests the root folder through the host")
	assert.Contains(t, h.content(), "README.md")
}

func TestReviewInspectorKeepsSidebarAndOwnsKeys(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	require.NotNil(t, h.page.git.Snapshot, "the sidebar section received the Git snapshot")
	assert.True(t, h.page.git.Available)
	assert.Equal(t, "Dockerfile.dev", h.page.git.Open.Path, "the open file is highlighted in the sidebar")
	require.NotNil(t, h.page.overlay, "the diff inspector covers only the transcript area of the page")
	content := h.content()
	assert.Contains(t, content, "Dockerfile.dev")
	assert.NotContains(t, content, "visible chat transcript")
	assert.NotContains(t, content, "Changes  Draft", "no second Changes navigator")

	h.key("j", "x", "ctrl+w")
	assert.Zero(t, h.editor.keys, "inspector navigation never reaches the composer")
	assert.Zero(t, h.page.keys, "or the hidden transcript")
	assert.True(t, h.m.reviewFocused(), "Ctrl+W never closes the session or inspector")
	h.key("c")
	require.True(t, h.m.reviewModel().CommentInputActive(), "c opens the inline comment editor")
	h.key("esc") // cancel the comment capture
	require.NotNil(t, h.m.reviewModel(), "cancelling a comment keeps the inspector open")
	h.key("esc")
	assert.Nil(t, h.m.reviewModel(), "Esc closes the inspector after comment capture is cancelled")
	assert.Nil(t, h.page.overlay, "the transcript is back")
	assert.True(t, h.page.focused, "and focused")
	assert.NotNil(t, h.page.git.Snapshot, "Git data stays live for the sidebar")
}

func TestReviewChangesCommandRevealsSidebarAndPrewarmsMetadata(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.pump(h.m.syncGitReview())
	assert.True(t, h.page.git.Available, "the section is offered")
	assert.Equal(t, 1, h.provider.snapshots, "the visible collapsed sidebar performs one bounded metadata scan")

	_, cmd := h.m.Update(messages.RevealGitChangesMsg{})
	h.pump(cmd)
	assert.Equal(t, 1, h.page.revealed)
	assert.Equal(t, PanelSidebarGit, h.m.focusedPanel, "/changes focuses the section")
	assert.True(t, h.page.gitFocused)
	assert.Equal(t, 1, h.provider.snapshots, "expanding reuses discovery")
	assert.Equal(t, 3, len(h.provider.worktrees), "discovered worktrees receive bounded metadata-only prewarm")
	assert.Empty(t, h.provider.documents, "prewarm never reads diffs or history")
	assert.Empty(t, h.provider.dirs, "prewarm never loads file contents")
	assert.Nil(t, h.page.overlay, "no surface opens")

	h.key("j", "x")
	assert.Equal(t, 2, h.page.gitKeys, "keys go to the section")
	assert.Zero(t, h.editor.keys)
	h.key("esc")
	assert.Equal(t, PanelEditor, h.m.focusedPanel, "Esc leaves the section")
	assert.False(t, h.page.gitFocused)

	_, cmd = h.m.Update(sidebar.GitRequestMsg{Owner: "s1", Action: sidebar.GitLoadWorktree, WorktreeID: "w2"})
	h.pump(cmd)
	assert.Equal(t, []string{"w1", "w2", "w3", "w2"}, h.provider.worktrees, "explicit expansion can request its metadata again")
	assert.Empty(t, h.provider.documents, "expanding Git status does not load a diff")
}

func TestReviewTabCycleReturnsToReviewNotHiddenTranscript(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	h.key("tab")
	assert.False(t, h.m.reviewFocused())
	for i := 0; i < 3 && h.m.focusedPanel != PanelEditor; i++ {
		h.key("tab") // footer items or message actions sit between review and the composer
	}
	assert.Equal(t, PanelEditor, h.m.focusedPanel)
	assert.True(t, h.editor.focused)

	h.key("tab")
	assert.True(t, h.m.reviewFocused(), "Tab from the composer returns to review, not the hidden transcript")
	assert.False(t, h.page.focused)

	h.key("shift+tab")
	assert.False(t, h.m.reviewFocused())
	assert.True(t, h.editor.focused, "Shift+Tab goes back to the composer instead of cycling thinking")

	h.editor.hasContext = true
	h.key("tab")
	assert.True(t, h.editor.ctxFocused, "composer → context bar → review")
	h.key("tab")
	assert.True(t, h.m.reviewFocused())
	h.key("shift+tab")
	assert.True(t, h.editor.ctxFocused, "Shift+Tab lands on the context bar when present")
}

func TestReviewPointerOwnsContentArea(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(true) // the Files surface replaces the whole content area
	h.m.reviewModel().Blur()
	for _, msg := range []tea.Msg{
		tea.MouseClickMsg{X: 3, Y: 4, Button: tea.MouseLeft},
		tea.MouseReleaseMsg{X: 3, Y: 4, Button: tea.MouseLeft},
		tea.MouseMotionMsg{X: 10, Y: 5},
		tea.MouseWheelMsg{X: 10, Y: 5, Button: tea.MouseWheelDown},
	} {
		_, cmd := h.m.Update(msg)
		h.pump(cmd)
	}
	assert.Zero(t, h.page.keys)
	assert.False(t, h.page.focused, "clicks never reach the hidden chat")
	assert.True(t, h.m.reviewFocused(), "a click focuses review")
	assert.False(t, h.m.reviewOwnsPointer(0, 0), "the application-owned top inset does not belong to review")
	assert.True(t, h.m.reviewOwnsPointer(0, 1), "review owns its visible content below the inset")
	assert.False(t, h.m.reviewOwnsPointer(0, h.m.contentHeight), "composer rows stay with the composer")
}

func TestReviewFocusRestoresOpener(t *testing.T) {
	t.Run("composer via click", func(t *testing.T) {
		h := newReviewHarness(t, 140)
		h.openDockerfile()
		h.key("q")
		assert.Nil(t, h.m.reviewModel())
		assert.Equal(t, PanelEditor, h.m.focusedPanel)
		assert.True(t, h.editor.focused)
	})
	t.Run("transcript after the sidebar", func(t *testing.T) {
		h := newReviewHarness(t, 140)
		h.open(false)
		h.key("q")
		assert.Equal(t, PanelContent, h.m.focusedPanel, "back from the inspector restores the transcript")
		assert.True(t, h.page.focused)
	})
	t.Run("context bar", func(t *testing.T) {
		h := newReviewHarness(t, 140)
		h.editor.hasContext, h.editor.ctxFocused, h.editor.focused = true, true, false
		h.openDockerfile()
		assert.False(t, h.editor.ctxFocused)
		h.key("q")
		assert.True(t, h.editor.ctxFocused)
	})
}

func TestReviewStaleResultCannotCancelNewerRequest(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	before := len(h.provider.documents)
	first := h.m.submitGitReviewRequest(view.OpenDocumentRequestMsg{Owner: "s1", Key: view.DocumentKey{WorktreeID: "w1", Path: "a.go", Group: view.GroupUnstaged}})
	second := h.m.submitGitReviewRequest(view.OpenDocumentRequestMsg{Owner: "s1", Key: view.DocumentKey{WorktreeID: "w1", Path: "a.go", Group: view.GroupStaged}})
	newest := h.m.gitReviewPending["doc"]

	stale := first().(gitReviewTaskResult)
	require.ErrorIs(t, stale.err, context.Canceled, "the superseded document request stops before any IO")
	assert.Nil(t, h.m.applyGitReviewTask(stale))
	assert.Equal(t, newest.token, h.m.gitReviewPending["doc"].token, "a stale result must not cancel newer work")

	h.pump(func() tea.Msg { return second() })
	assert.Len(t, h.provider.documents, before+1, "only the newest selection reaches the provider")
	assert.Equal(t, gitreview.GroupStaged, h.provider.documents[before].Group)
	assert.Empty(t, h.m.gitReviewPending)
}

func TestReviewRemoteWithoutCapabilityNeverReadsLocal(t *testing.T) {
	local := func(string) (gitreview.Provider, error) {
		t.Fatal("remote sessions must never fall back to local files")
		return nil, nil
	}
	provider, unavailable, err := chooseGitReviewProvider(nil, remoteHandle{}, "s1", "/remote/work", local)
	require.NoError(t, err)
	assert.Nil(t, provider)
	require.NotNil(t, unavailable)
	assert.True(t, unavailable.Scope.Remote)
	assert.NotEmpty(t, unavailable.Scope.Unavailable)

	provider, unavailable, err = chooseGitReviewProvider((*runtime.SessionTransport)(nil), nil, "s1", "/remote/work", local)
	require.NoError(t, err)
	assert.Nil(t, unavailable)
	assert.IsType(t, &runtime.GitReviewRemote{}, provider, "remote sessions use the server-side capability")
}

type remoteHandle struct{ runtime.SessionHandle }

func TestReviewComposerTargetLineOnlyWhileTypingElsewhere(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	ws := h.m.activeReview()
	_, cmd := h.m.Update(sidebar.GitRequestMsg{Owner: "s1", Action: sidebar.GitOpen, WorktreeID: "w2", Path: "b.go", Group: view.GroupStaged})
	h.pump(cmd)
	_, mismatch := ws.current().TargetMismatch()
	require.True(t, mismatch, "another checkout is selected")
	assert.Zero(t, ws.hint.Generation, "no line while review has focus")

	for i := 0; i < 4 && h.m.focusedPanel != PanelEditor; i++ {
		h.key("tab")
	}
	require.Equal(t, PanelEditor, h.m.focusedPanel)
	h.pump(h.m.syncGitReview())
	assert.Equal(t, "sends to main session", ws.hintText)
	assert.NotZero(t, ws.hint.Generation, "typing shows where the composer sends")
	assert.Contains(t, ansi.Strip(h.m.messageBar.View()), "sends to main session")
	assert.Equal(t, "s1", h.m.reviewAnchor(), "selection never changes the session the composer sends to")

	h.key("tab")
	h.pump(h.m.syncGitReview())
	require.True(t, h.m.reviewFocused())
	assert.Zero(t, ws.hint.Generation, "the line leaves with composer focus")
}

func TestReviewDockedChatIsReachableByKeyboard(t *testing.T) {
	h := newReviewHarness(t, 160)
	h.open(true)
	h.key("D")
	require.True(t, h.m.reviewDocked())

	for i := 0; i < 4 && h.m.focusedPanel != PanelEditor; i++ {
		h.key("tab")
	}
	require.Equal(t, PanelEditor, h.m.focusedPanel)
	h.key("tab")
	assert.Equal(t, PanelContent, h.m.focusedPanel)
	assert.True(t, h.page.focused, "Tab from the composer reaches the docked chat")
	assert.False(t, h.m.reviewFocused())

	h.key("j", "k")
	assert.Equal(t, 2, h.page.keys, "the docked transcript receives its own keys")

	h.key("tab")
	assert.True(t, h.m.reviewFocused(), "Tab from the chat moves to review")
	assert.False(t, h.page.focused)
	h.key("shift+tab")
	assert.True(t, h.page.focused, "Shift+Tab from review returns to the chat")
	h.key("shift+tab")
	assert.Equal(t, PanelEditor, h.m.focusedPanel, "Shift+Tab from the chat returns to the composer")
	assert.True(t, h.editor.focused)

	h.key("tab", "tab", "q")
	assert.Nil(t, h.m.reviewModel())
	assert.Equal(t, PanelEditor, h.m.focusedPanel, "closing restores the opener's focus")
	assert.True(t, h.editor.focused)
	assert.Equal(t, h.m.width, h.page.width, "closing restores the full chat width")
}

func TestReviewRootFrameShowsDiffBodyAndTerminatesErrors(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	require.NotEmpty(t, h.provider.documents)
	assert.Equal(t, "Dockerfile.dev", h.provider.documents[len(h.provider.documents)-1].Path)
	frame := ansi.Strip(h.m.composeView().Content)
	t.Log("\n" + frame)
	assert.Contains(t, frame, "- FROM golang:1.22", "the root frame shows removed lines")
	assert.Contains(t, frame, "+ FROM golang:1.27", "the root frame shows added lines")
	assert.Contains(t, frame, "WORKDIR /src")
	assert.NotContains(t, frame, "Loading…", "a ready document never stays loading")
	assert.NotContains(t, frame, "visible chat transcript", "undocked review replaces the transcript")

	h.provider.fail = true
	_, cmd := h.m.Update(view.OpenDocumentRequestMsg{Owner: "s1", Key: view.DocumentKey{Mode: view.ModeChanges, WorktreeID: "w1", Path: "Dockerfile.dev", Group: view.GroupUnstaged}})
	h.pump(cmd)
	frame = ansi.Strip(h.m.composeView().Content)
	assert.NotContains(t, frame, "Loading…", "a failed request never leaves the reader loading")
	assert.Contains(t, frame, "⚠ document unavailable")
}

func TestReviewAdapterMarksReturnedDocumentsReady(t *testing.T) {
	for _, state := range []gitreview.LoadState{gitreview.StateReady, "", "unstaged", "none"} {
		doc := reviewDocument(gitreview.Document{State: state, Lines: []gitreview.DocumentLine{{Kind: gitreview.DiffAdd, New: 1, Text: "x"}}})
		assert.Equal(t, view.StateReady, doc.State, "state %q", state)
	}
	assert.Equal(t, view.StateError, reviewDocument(gitreview.Document{State: gitreview.StateError}).State)
	assert.Empty(t, reviewDocument(gitreview.Document{PatchHeader: "@@ -1 +1 @@"}).PatchHeader, "incomplete headers disable patch copy")
	assert.NotEmpty(t, reviewDocument(gitreview.Document{PatchHeader: "diff --git a/x b/x\n--- a/x\n+++ b/x"}).PatchHeader)
	file := reviewDocument(gitreview.Document{Key: gitreview.DocumentKey{Mode: "files"}, Lines: []gitreview.DocumentLine{{Kind: gitreview.DiffAdd, New: 1, Text: "plain"}}})
	assert.Equal(t, view.LineContext, file.Lines[0].Kind, "file contents are not shown as additions")
	partial := reviewSnapshot(gitreview.Snapshot{Scope: gitreview.Scope{Note: "Discovery limited; some repositories were not scanned"}})
	assert.Equal(t, "Discovery limited; some repositories were not scanned", partial.Scope.Note)
	assert.False(t, partial.MaybeOutdated, "a partial result is not stale")
	wt := reviewWorktree(gitreview.Worktree{Detached: true, DetachedHead: "abc1234"})
	assert.Equal(t, "abc1234", wt.Detached)
	assert.Equal(t, view.RepoNone, reviewRepoKind(gitreview.KindNone))
}

func TestReviewWorkingFollowsAnchorChatActivity(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	assert.False(t, h.m.activeReview().session.Working)
	h.page.working = true
	h.pump(h.m.syncGitReview())
	assert.True(t, h.m.activeReview().session.Working, "activity comes from the anchor chat page, not handle presence")
}

func TestReviewStatePerWorkspaceAndScopeChanges(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	h.key("j", "j")
	first := h.m.activeReview()
	selection := first.current().Selection()

	other := &session.Session{ID: "s2", Title: "other", WorkingDir: "/elsewhere"}
	_, err := h.m.supervisor.AddSession(t.Context(), app.New(t.Context(), nil, other, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{})), other, "/elsewhere", nil)
	require.NoError(t, err)
	h.m.paneWorkspaces.active = &paneWorkspace{anchor: "s2"}
	h.m.supervisor.SwitchTo("s2")
	h.pump(h.m.syncGitReview())
	assert.Nil(t, h.m.reviewModel(), "a workspace without review shows its chat")
	assert.True(t, first.open, "the first workspace keeps its review open")

	h.m.paneWorkspaces.active = &paneWorkspace{anchor: "s1"}
	h.m.supervisor.SwitchTo("s1")
	h.pump(h.m.syncGitReview())
	require.Same(t, first, h.m.activeReview())
	assert.Equal(t, selection, first.current().Selection(), "returning restores the exact selection")
	assert.Equal(t, 1, h.provider.snapshots, "returning does not rescan")

	h.sess.WorkingDir = "/moved"
	h.pump(h.m.syncGitReview())
	assert.Nil(t, h.m.reviewModel(), "a workspace now showing another folder never reuses old review data")
}

func TestReviewDockShowsChatBesideReview(t *testing.T) {
	h := newReviewHarness(t, 160)
	h.open(true)
	h.key("D")
	require.True(t, h.m.reviewDocked())
	chatWidth := h.m.reviewDockChatWidth()
	assert.Equal(t, chatWidth, h.page.width, "the chat is laid out at its docked width")
	content := h.content()
	assert.Contains(t, content, "visible chat transcript")
	assert.Contains(t, content, "Files")
	for line := range strings.SplitSeq(h.m.reviewContentView(), "\n") {
		assert.Equal(t, h.m.width, ansi.StringWidth(line))
	}

	assert.False(t, h.m.reviewOwnsPointer(chatWidth-1, 3), "the docked chat keeps its own pointer events")
	assert.True(t, h.m.reviewOwnsPointer(chatWidth+2, 3))
	_, cmd := h.m.Update(tea.MouseClickMsg{X: 2, Y: 3, Button: tea.MouseLeft})
	h.pump(cmd)
	assert.False(t, h.m.reviewFocused(), "clicking the docked chat hands it the keyboard")

	h.m.focusReview()
	h.key("D")
	assert.False(t, h.m.reviewDocked())
	assert.Equal(t, h.m.width, h.page.width, "undocking restores the full chat width")

	narrow := newReviewHarness(t, 100)
	narrow.open(false)
	narrow.key("D")
	assert.False(t, narrow.m.reviewDocked(), "docking is refused when too narrow")
}

func TestReviewContextHelp(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	doc := h.m.helpDocument()
	assert.Equal(t, "Changes review — reader", doc.Context, "the inspector's own context help")
	h.key("/")
	assert.Equal(t, "Changes review — typing search", h.m.helpDocument().Context)
}

func TestReviewHoldCPreservesExistingKeyboardRange(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	model := h.m.reviewModel()
	_, cmd := h.m.Update(tea.KeyboardEnhancementsMsg{Flags: ansi.KittyReportEventTypes})
	h.pump(cmd)
	_, cmd = h.m.Update(tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModShift})
	h.pump(cmd)
	selected := model.Save().SelectFrom
	require.GreaterOrEqual(t, selected, 0)
	h.key("c")
	assert.Equal(t, selected, model.Save().SelectFrom, "tap-c anchors, not replaces, an existing range")
	h.key("down")
	_, cmd = h.m.Update(tea.KeyReleaseMsg{Code: 'c'})
	h.pump(cmd)
	assert.True(t, h.m.reviewModel().CommentInputActive())
	assert.Equal(t, selected, model.Save().SelectFrom, "the comment editor keeps the original range anchor")
	h.key("esc")
}

func TestReviewSearchAndUppercaseCRemainReviewCommands(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	model := h.m.reviewModel()
	h.key("/")
	_, cmd := h.m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	h.pump(cmd)
	assert.False(t, model.CommentInputActive(), "search text c is not stolen as a comment shortcut")
	assert.Contains(t, ansi.Strip(model.View()), "c", "search field receives the character")
}

func TestReviewHoldCSelectsRangeAndUnsupportedTerminalUsesImmediateFallback(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	_, cmd := h.m.Update(tea.KeyboardEnhancementsMsg{Flags: ansi.KittyReportEventTypes})
	h.pump(cmd)
	h.key("c")
	assert.True(t, h.m.reviewHoldC, "supported terminals start a hold gesture")
	assert.False(t, h.m.reviewModel().CommentInputActive(), "press alone never starts the inline editor")
	h.key("j", "j")
	_, cmd = h.m.Update(tea.KeyReleaseMsg{Code: 'c'})
	h.pump(cmd)
	require.True(t, h.m.reviewModel().CommentInputActive(), "releasing c opens the inline editor")
	h.key("esc")

	_, cmd = h.m.Update(tea.KeyboardEnhancementsMsg{})
	h.pump(cmd)
	h.key("c")
	require.True(t, h.m.reviewModel().CommentInputActive(), "terminals without key-up support use c as an immediate comment action")
	h.key("esc")
}

func TestReviewHoldCReleaseAfterContextChangesDoesNotOpenComment(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	_, cmd := h.m.Update(tea.KeyboardEnhancementsMsg{Flags: ansi.KittyReportEventTypes})
	h.pump(cmd)
	h.key("c", "/")
	assert.True(t, h.m.reviewHoldC, "hold state waits for the actual c release")
	_, cmd = h.m.Update(tea.KeyReleaseMsg{Code: 'c'})
	h.pump(cmd)
	assert.False(t, h.m.reviewModel().CommentInputActive(), "release outside reader context cannot start a comment")
	assert.False(t, h.m.reviewHoldC, "the stale hold is cleared")
	_, cmd = h.m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	h.pump(cmd)
	assert.False(t, h.m.reviewModel().CommentInputActive(), "c types in search after the hold release")
}

func TestReviewShiftCDoesNotStartCommentShortcut(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	_, cmd := h.m.Update(tea.KeyboardEnhancementsMsg{Flags: ansi.KittyReportEventTypes})
	h.pump(cmd)
	msg := tea.KeyPressMsg{Code: 'c', ShiftedCode: 'C', Mod: tea.ModShift, Text: "C"}
	_, cmd = h.m.Update(msg)
	h.pump(cmd)
	assert.False(t, h.m.reviewModel().CommentInputActive(), "physical Shift+C does not activate lowercase c comment")
	assert.False(t, h.m.reviewHoldC, "Shift+C is not a hold gesture")
}

func TestClosingDiffInspectorDoesNotRestoreReviewTargetHint(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.openDockerfile() // opened from the composer, so close restores composer focus
	ws := h.m.activeReview()
	require.NotNil(t, ws)

	_, cmd := h.m.Update(sidebar.GitRequestMsg{Owner: "s1", Action: sidebar.GitOpen, WorktreeID: "w2", Path: "b.go", Group: view.GroupStaged})
	h.pump(cmd)
	_, mismatch := ws.current().TargetMismatch()
	require.True(t, mismatch)
	for i := 0; i < 4 && h.m.focusedPanel != PanelEditor; i++ {
		h.key("tab")
	}
	require.Equal(t, PanelEditor, h.m.focusedPanel)
	h.pump(h.m.syncGitReview())
	require.Equal(t, gitReviewHintOwner, h.m.messageBar.CurrentMessage().Owner)

	h.key("tab", "q") // return focus to the diff, then close it
	require.Nil(t, h.m.reviewModel(), "closing the diff inspector hides the review surface")
	require.Equal(t, PanelEditor, h.m.focusedPanel, "the original composer focus is restored")
	assert.Zero(t, ws.hint.Generation, "a hidden review relinquishes its mismatch notice")
	assert.Empty(t, h.m.messageBar.CurrentMessage().Text, "the hint is cleared instead of remaining as a persistent message")
	h.pump(h.m.syncGitReview())
	assert.Zero(t, ws.hint.Generation, "hidden review updates cannot re-arm the notice")
}
