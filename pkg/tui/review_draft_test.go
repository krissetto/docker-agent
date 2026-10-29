package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/gitreview"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	view "github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/reviewdraft"
	"github.com/docker/docker-agent/pkg/tui/service"
)

type reviewHandle struct{ runtime.SessionHandle }

func (reviewHandle) ID() string        { return "s1" }
func (reviewHandle) AgentName() string { return "root" }

type fakeSubmitter struct {
	mu       sync.Mutex
	calls    []string // request IDs
	bodies   []string
	targets  []string // session IDs
	failNext error
}

func (f *fakeSubmitter) submit(_ context.Context, _ *app.App, target app.ReviewSubmitTarget, requestID, body string) (app.ReviewSubmitReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, requestID)
	f.bodies = append(f.bodies, body)
	f.targets = append(f.targets, target.SessionID)
	if err := f.failNext; err != nil {
		f.failNext = nil
		return app.ReviewSubmitReceipt{}, err
	}
	return app.ReviewSubmitReceipt{Submission: runtime.Submission{SessionID: target.SessionID, TurnID: "turn-1"}}, nil
}

func newDraftHarness(t *testing.T) (*reviewHarness, *fakeSubmitter) {
	t.Helper()
	h, f := newDraftHarnessWithoutOpen(t)
	h.open(false)
	return h, f
}

func newDraftHarnessWithoutOpen(t *testing.T) (*reviewHarness, *fakeSubmitter) {
	t.Helper()
	h := newReviewHarness(t, 140)
	f := &fakeSubmitter{}
	h.m.gitReviewSubmit = f.submit
	h.m.gitReviewCapture = func(a *app.App) app.ReviewSubmitTarget {
		return app.ReviewSubmitTarget{SessionID: a.Session().ID, Handle: reviewHandle{}}
	}
	return h, f
}

func (h *reviewHarness) comment(text string) {
	h.t.Helper()
	h.key("j", "c")
	require.True(h.t, h.m.reviewModel().CommentInputActive(), "inline comment editor opened")
	_, cmd := h.m.Update(tea.PasteMsg{Content: text})
	h.pump(cmd)
	h.key("enter")
}

func (h *reviewHarness) draft() reviewdraft.Snapshot { return h.m.activeReview().draft.Snapshot() }

// previewTo previews a send from the draft list. Changes (Git) reviews go to
// the current agent without a picker; Files reviews pick the first target.
func (h *reviewHarness) previewTo() {
	h.t.Helper()
	_, cmd := h.m.Update(view.PreviewDraftMsg{Owner: h.m.activeReview().anchor, Revision: h.m.activeReview().draft.Snapshot().Revision})
	h.pump(cmd)
	require.False(h.t, h.m.dialogMgr.Open(), "review sends target the currently active session")
}

// newFilesDraftHarness opens README.md in the Files surface, where reviews
// may be sent to a chosen recipient.
func newFilesDraftHarness(t *testing.T) (*reviewHarness, *fakeSubmitter) {
	t.Helper()
	h := newReviewHarness(t, 140)
	f := &fakeSubmitter{}
	h.m.gitReviewSubmit = f.submit
	h.m.gitReviewCapture = func(a *app.App) app.ReviewSubmitTarget {
		return app.ReviewSubmitTarget{SessionID: a.Session().ID, Handle: reviewHandle{}}
	}
	h.open(true)
	h.key("j", "enter")
	return h, f
}

func TestReviewDraftSendsOnlyAfterPickerPreviewAndConfirm(t *testing.T) {
	h, f := newDraftHarness(t)
	h.comment("Why bump Go?")
	require.Len(t, h.draft().Comments, 1)
	assert.Equal(t, "work", h.draft().Comments[0].Anchor.RepositoryName)
	assert.Contains(t, h.content(), "┊ Why bump Go?", "comments show under the anchored line")
	assert.Zero(t, h.editor.keys, "the composer draft is untouched")
	h.previewTo()
	content := h.content()
	assert.Contains(t, content, "Send review to current agent · ◆ main session")
	assert.Contains(t, content, "Why bump Go?", "the preview shows the exact payload")
	assert.Empty(t, f.calls, "preview never sends")
	previewPayload := h.m.activeReview().preview.payload

	h.key("enter")
	require.Len(t, f.calls, 1)
	parsed, ok := reviewdraft.ParseReferences(f.bodies[0])
	require.True(t, ok)
	require.Equal(t, "Why bump Go?", parsed.Comments[0].Comment)
	require.Equal(t, previewPayload, f.bodies[0], "preview and provider receive the same canonical string")
	assert.Empty(t, h.draft().Comments, "an accepted send clears the sent comments")
	assert.NotContains(t, h.content(), "Send review to", "preview closes after sending")
}

func TestReviewDraftFailureKeepsExactRetry(t *testing.T) {
	h, f := newDraftHarness(t)
	h.comment("first")
	h.previewTo()
	f.failNext = errors.New("connection lost")
	h.key("enter")
	require.Len(t, f.calls, 1)
	require.Len(t, h.draft().Comments, 1, "failed sends keep the draft")
	assert.Contains(t, h.content(), "connection lost")

	h.key("enter")
	require.Len(t, f.calls, 2)
	assert.Equal(t, f.calls[0], f.calls[1], "a retry reuses the same request ID")
	assert.Equal(t, f.bodies[0], f.bodies[1], "and the same payload")
	assert.Empty(t, h.draft().Comments)
}

func TestReviewDraftRefusesReplacedTargetAndDuplicateConfirm(t *testing.T) {
	h, f := newDraftHarness(t)
	h.comment("note")
	h.previewTo()
	ws := h.m.activeReview()
	ws.preview.target.RouteGeneration++
	h.key("enter")
	assert.Empty(t, f.calls, "a replaced route never receives the review")
	assert.Len(t, h.draft().Comments, 1)

	h2, f2 := newDraftHarness(t)
	h2.comment("once")
	h2.previewTo()
	token := h2.m.activeReview().preview.token
	cmd1 := h2.m.handleReviewDraftMessage(viewSend(token))
	cmd2 := h2.m.handleReviewDraftMessage(viewSend(token))
	assert.Nil(t, cmd2, "a duplicate confirm while sending is ignored")
	h2.pump(cmd1)
	assert.Len(t, f2.calls, 1)
}

func TestReviewDraftChangedAfterPreviewNeedsNewPreview(t *testing.T) {
	h, f := newDraftHarness(t)
	h.comment("one")
	h.previewTo()
	_, err := h.m.activeReview().draft.Add(h.draft().Comments[0].Anchor, "added later")
	require.NoError(t, err)
	h.key("enter")
	assert.Empty(t, f.calls, "an edited draft is never sent from a stale preview")
}

func TestReviewChangesAndFilesShareDraftButNotNavigation(t *testing.T) {
	h, _ := newDraftHarness(t)
	h.comment("shared")
	changes := h.m.reviewModel()
	_, cmd := h.m.Update(messages.OpenGitReviewMsg{Files: true})
	h.pump(cmd)
	files := h.m.reviewModel()
	require.NotSame(t, changes, files, "/files is its own surface")
	content := h.content()
	assert.Contains(t, content, "Files")
	assert.Contains(t, content, "Draft 1", "the draft is shared across surfaces")
	assert.NotContains(t, content, "Changes  ", "no mode tabs")

	h.openDockerfile()
	assert.Same(t, changes, h.m.reviewModel(), "the inspector returns to its own state")
	assert.Contains(t, h.content(), "Dockerfile.dev")
	assert.Contains(t, h.content(), "┊ shared", "the shared draft shows in the inspector")
}

func viewSend(token string) view.SendDraftMsg {
	return view.SendDraftMsg{Owner: "s1", Revision: 1, Token: token}
}

func TestReviewRootSplitHighlightAndNarrowFallback(t *testing.T) {
	h := newReviewHarness(t, 180)
	h.open(false)
	h.key("t")
	frame := ansi.Strip(h.m.composeView().Content)
	var row string
	for line := range strings.SplitSeq(frame, "\n") {
		if strings.Contains(line, "FROM golang:1.22") {
			row = line
		}
	}
	require.NotEmpty(t, row, "\n"+frame)
	assert.Contains(t, row, "FROM golang:1.27", "split shows old and new on one row in the real frame")

	model := h.m.reviewModel()
	h.m.width = 90
	h.pump(h.m.layoutGitReview())
	frame = ansi.Strip(h.m.composeView().Content)
	for line := range strings.SplitSeq(frame, "\n") {
		if strings.Contains(line, "FROM golang:1.22") {
			assert.NotContains(t, line, "FROM golang:1.27", "narrow frames fall back to unified")
		}
	}
	assert.Same(t, model, h.m.reviewModel())
}

func TestReviewDraftRetryAfterCancelKeepsOriginalRecipient(t *testing.T) {
	h, f := newDraftHarness(t)
	h.comment("keep target")
	h.previewTo()
	f.failNext = errors.New("timeout")
	h.key("enter")
	require.Len(t, f.calls, 1)
	h.key("esc", "R", "s") // close preview, reopen draft, and select retry
	assert.False(t, h.m.dialogMgr.Open(), "no picker while an attempt is unresolved")
	assert.Contains(t, h.content(), "Send review to current agent · ◆ main session", "the original recipient is restored")
	h.key("enter")
	require.Len(t, f.calls, 2)
	assert.Equal(t, f.calls[0], f.calls[1])
	assert.Empty(t, h.draft().Comments)
}

func TestReviewAggregatesGeneralAndInlineCommentsIntoOneActiveSessionSend(t *testing.T) {
	h, f := newDraftHarness(t)
	h.comment("inline note")
	ws := h.m.activeReview()
	require.NoError(t, ws.draft.SetGeneral("overall context"))
	ws.pushDraft()
	cmd := h.m.sendReviewFromComposer()
	require.NotNil(t, cmd)
	h.pump(cmd)
	require.Len(t, f.calls, 1)
	require.Equal(t, []string{"s1"}, f.targets, "review sends to the currently active session")
	parsed, ok := reviewdraft.ParseReferences(f.bodies[0])
	require.True(t, ok)
	require.Equal(t, "inline note", parsed.Comments[0].Comment)
	require.Equal(t, "overall context", parsed.General)
	require.Empty(t, ws.draft.Snapshot().Comments)
	require.Empty(t, ws.draft.Snapshot().General)
}

func TestReviewSendFailurePreservesGeneralAndInlineDraft(t *testing.T) {
	h, f := newDraftHarness(t)
	h.comment("inline")
	ws := h.m.activeReview()
	require.NoError(t, ws.draft.SetGeneral("general"))
	f.failNext = errors.New("offline")
	h.pump(h.m.sendReviewFromComposer())
	require.Len(t, ws.draft.Snapshot().Comments, 1)
	require.Equal(t, "general", ws.draft.Snapshot().General)
	require.NotNil(t, ws.draft.Snapshot().Attempt)
}

func TestReviewComposerMouseBorrowSendsInlineOnlyAndRestoresChatDraft(t *testing.T) {
	h, f := newDraftHarness(t)
	h.comment("inline only")
	h.editor.value = "ordinary chat draft"
	_, cmd := h.m.Update(tea.MouseClickMsg{X: 3, Y: h.m.editorTop(), Button: tea.MouseLeft})
	h.pump(cmd)
	require.True(t, h.editor.CommentInputActive(), "clicking the bottom composer while reviewing starts the general review composer")
	_, cmd = h.m.Update(tea.PasteMsg{Content: "review context"})
	h.pump(cmd)
	h.key("enter")
	require.Len(t, f.calls, 1, "inline comments can send with an empty general comment")
	parsed, ok := reviewdraft.ParseReferences(f.bodies[0])
	require.True(t, ok)
	require.Equal(t, "inline only", parsed.Comments[0].Comment)
	require.Equal(t, "review context", parsed.General)
	assert.Equal(t, "ordinary chat draft", h.editor.Value(), "the normal chat draft is restored after accepted send")
}

func TestReviewPendingAttemptRejectsSessionSwitchAndKeepsDraft(t *testing.T) {
	h, f := newDraftHarness(t)
	h.comment("target-bound")
	h.previewTo()
	child := &session.Session{ID: "s2", Title: "other", WorkingDir: "/other"}
	_, err := h.m.supervisor.AddSession(t.Context(), app.New(t.Context(), nil, child, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{})), child, "/other", nil)
	require.NoError(t, err)
	h.m.supervisor.SwitchTo("s2")
	cmd := h.m.sendGitReviewDraft(view.SendDraftMsg{Owner: "s1", Revision: 1, Token: h.m.gitReviews["s1"].preview.token})
	require.NotNil(t, cmd)
	h.pump(cmd)
	assert.Empty(t, f.calls)
	assert.Len(t, h.m.gitReviews["s1"].draft.Snapshot().Comments, 1)
}

func TestReviewAcceptancePreservesGeneralCommentEditedInFlight(t *testing.T) {
	h, f := newDraftHarness(t)
	h.comment("inline")
	ws := h.m.activeReview()
	require.NoError(t, ws.draft.SetGeneral("captured"))
	cmd := h.m.sendReviewFromComposer()
	require.NotNil(t, cmd)
	result, ok := cmd().(gitReviewSendResult)
	require.True(t, ok)
	require.NoError(t, ws.draft.SetGeneral("newer while pending"))
	h.m.applyGitReviewSend(result)
	require.Equal(t, "newer while pending", ws.draft.Snapshot().General)
	require.Empty(t, ws.draft.Snapshot().Comments)
	parsed, ok := reviewdraft.ParseReferences(f.bodies[0])
	require.True(t, ok)
	require.Equal(t, "captured", parsed.General)
}

func TestReviewComposerPasteEscRetainsGeneralAndChatDraft(t *testing.T) {
	h, _ := newDraftHarness(t)
	h.editor.value = "ordinary"
	_, cmd := h.m.Update(view.GeneralCommentRequestMsg{Owner: "s1"})
	h.pump(cmd)
	_, cmd = h.m.Update(tea.PasteMsg{Content: "paste general"})
	h.pump(cmd)
	h.key("esc")
	require.Equal(t, "paste general", h.m.activeReview().draft.Snapshot().General)
	assert.Equal(t, "ordinary", h.editor.Value())
}

func TestReviewCounterpartSwitchesInspectorAndKeepsSidebar(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	ws := h.m.activeReview()
	key := view.DocumentKey{Mode: view.ModeFiles, WorktreeID: "w1", Path: "Dockerfile.dev", Group: view.GroupNone}
	_, cmd := h.m.Update(view.CounterpartRequestMsg{Owner: "s1", Key: key, OldLine: 1, NewLine: 1, Top: 0})
	h.pump(cmd)
	require.Equal(t, view.ModeFiles, ws.mode)
	require.True(t, ws.inspecting)
	require.NotNil(t, h.page.overlay)
	require.Equal(t, view.ModeFiles, h.page.git.Mode)
	require.Contains(t, h.provider.documents, gitreview.DocumentKey{Mode: "files", WorktreeID: "w1", Path: "Dockerfile.dev", Group: gitreview.GroupNone})
}

func TestReviewProviderWithoutEditCapabilityIsRejected(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(true)
	model := h.m.reviewModel()
	key := view.DocumentKey{Mode: view.ModeFiles, WorktreeID: "w1", Path: "Dockerfile.dev", Group: view.GroupNone}
	model.OpenDocument(key)
	cmd := h.m.openReviewExternalEditor(view.OpenExternalEditorRequestMsg{Owner: "s1", Key: key})
	require.NotNil(t, cmd)
}

func TestReviewBlankComposerSendUsesInlineComments(t *testing.T) {
	h, f := newDraftHarness(t)
	h.comment("inline-only")
	h.m.reviewModel().Blur()
	h.m.focusedPanel = PanelEditor
	_, cmd := h.m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	h.pump(cmd)
	require.Len(t, f.calls, 1)
	parsed, ok := reviewdraft.ParseReferences(f.bodies[0])
	require.True(t, ok)
	require.Equal(t, "inline-only", parsed.Comments[0].Comment)
	require.Empty(t, parsed.General)
}

func TestReviewGeneralComposerDoesNotSendOrdinaryChatAfterSessionSwitch(t *testing.T) {
	h, f := newDraftHarness(t)
	h.editor.value = "normal draft"
	h.comment("inline")
	ws := h.m.activeReview()
	require.NoError(t, ws.draft.SetGeneral("review note"))
	_, err := h.m.supervisor.AddSession(t.Context(), app.New(t.Context(), nil, &session.Session{ID: "s2", Title: "other", WorkingDir: "/other"}, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{})), &session.Session{ID: "s2", Title: "other", WorkingDir: "/other"}, "/other", nil)
	require.NoError(t, err)
	cmd := h.m.previewCurrentAgent(ws, ws.draft.Snapshot().Revision)
	h.pump(cmd)
	token := ws.preview.token
	h.m.finishReviewComment(false)
	h.m.supervisor.SwitchTo("s2")
	cmd = h.m.sendGitReviewDraft(view.SendDraftMsg{Owner: "s1", Revision: ws.preview.revision, Token: token})
	h.pump(cmd)
	assert.Empty(t, f.calls, "a preview cannot send to the prior active session")
	assert.Equal(t, "normal draft", h.editor.value)
	assert.Len(t, ws.draft.Snapshot().Comments, 1)
}

type editableReviewProvider struct {
	*fakeReviewProvider
	path string
}

func (p editableReviewProvider) EditablePath(string, string) (string, error) { return p.path, nil }

func TestReviewExternalEditorRefreshesWholeFileAfterScopedStatus(t *testing.T) {
	h := newReviewHarness(t, 140)
	provider := editableReviewProvider{fakeReviewProvider: h.provider, path: "/work/Dockerfile.dev"}
	h.m.gitReviewFactory = func(*app.App, string, string) (gitreview.Provider, *view.Snapshot, error) { return provider, nil, nil }
	h.open(true)
	key := view.DocumentKey{Mode: view.ModeFiles, WorktreeID: "w1", Path: "Dockerfile.dev"}
	h.pump(h.m.reviewModel().OpenDocument(key))
	documents := len(h.provider.documents)
	result := reviewExternalEditorDone{owner: "s1", sessionID: h.m.activeReview().sessionID, route: h.m.activeReview().route, key: key}
	result.generation = 1
	result.reviewGeneration = h.m.gitReviewGeneration
	h.m.activeReview().externalEditorGeneration = result.generation
	cmd := h.m.finishReviewExternalEditor(result)
	require.NotNil(t, cmd)
	msg, ok := cmd().(gitReviewTaskResult)
	require.True(t, ok)
	require.NotNil(t, msg.refreshDocument, "status refresh carries the guarded document to reload after invalidation")
	_, next := h.m.Update(msg)
	h.pump(next)
	require.Greater(t, len(h.provider.documents), documents, "the full file is reread after worktree refresh")
	last := h.provider.documents[len(h.provider.documents)-1]
	require.Equal(t, "files", last.Mode)
	require.Equal(t, key.WorktreeID, last.WorktreeID)
	require.Equal(t, key.Path, last.Path)
}

func TestReviewExternalEditorStaleExitDoesNotRefreshSwitchedDocument(t *testing.T) {
	h := newReviewHarness(t, 140)
	provider := editableReviewProvider{fakeReviewProvider: h.provider, path: "/work/Dockerfile.dev"}
	h.m.gitReviewFactory = func(*app.App, string, string) (gitreview.Provider, *view.Snapshot, error) { return provider, nil, nil }
	h.open(true)
	key := view.DocumentKey{Mode: view.ModeFiles, WorktreeID: "w1", Path: "Dockerfile.dev"}
	h.pump(h.m.reviewModel().OpenDocument(key))
	ws := h.m.activeReview()
	msg := reviewExternalEditorDone{owner: "s1", sessionID: ws.sessionID, route: ws.route, generation: 1, reviewGeneration: h.m.gitReviewGeneration, key: key}
	ws.externalEditorGeneration = 2
	require.Nil(t, h.m.finishReviewExternalEditor(msg))
	assert.Empty(t, h.m.gitReviewPending)
}

func TestReviewBorrowedComposerFailureKeepsTextAndBothDraftLayers(t *testing.T) {
	h, f := newDraftHarness(t)
	h.comment("inline")
	h.editor.value = "ordinary chat draft"
	_, cmd := h.m.Update(view.GeneralCommentRequestMsg{Owner: "s1"})
	h.pump(cmd)
	h.editor.SetCommentInputValue("general")
	f.failNext = errors.New("offline")
	h.pump(h.m.sendReviewFromComposer())
	require.True(t, h.editor.CommentInputActive())
	assert.Equal(t, "general", h.editor.CommentInputValue())
	assert.Equal(t, "ordinary chat draft", h.editor.value)
	require.Len(t, h.m.activeReview().draft.Snapshot().Comments, 1)
	assert.Equal(t, "general", h.m.activeReview().draft.Snapshot().General)
}

func TestFilesPreviewSendsAggregateToActiveSessionWithoutPicker(t *testing.T) {
	h, f := newFilesDraftHarness(t)
	h.comment("files inline")
	ws := h.m.activeReview()
	require.NoError(t, ws.draft.SetGeneral("files general"))
	ws.pushDraft()
	h.key("R", "s")
	require.False(t, h.m.dialogMgr.Open(), "Files review sends to the currently active session rather than opening a picker")
	h.key("enter")
	require.Len(t, f.calls, 1)
	require.Equal(t, []string{"s1"}, f.targets)
	parsed, ok := reviewdraft.ParseReferences(f.bodies[0])
	require.True(t, ok)
	require.Equal(t, "files inline", parsed.Comments[0].Comment)
	require.Equal(t, "files general", parsed.General)
}

func TestReviewLeadWhitespaceAcceptedGeneralClosesComposer(t *testing.T) {
	h, f := newDraftHarness(t)
	h.comment("inline")
	h.editor.value = "ordinary chat draft"
	h.pump(h.m.startReviewGeneralInput("s1"))
	h.editor.SetCommentInputValue("general comment\n")
	h.pump(h.m.sendReviewFromComposer())
	require.Len(t, f.calls, 1)
	assert.False(t, h.editor.CommentInputActive())
	assert.Equal(t, "ordinary chat draft", h.editor.Value())
	h.key("enter")
	assert.Len(t, f.calls, 1)
}

func TestReviewLeadContextBarTypingStartsGeneralComposer(t *testing.T) {
	h, f := newDraftHarness(t)
	h.comment("inline")
	h.editor.hasContext = true
	h.editor.value = "ordinary chat draft"
	h.key("shift+tab")
	require.True(t, h.editor.ctxFocused)
	h.key("x")
	assert.True(t, h.editor.CommentInputActive())
	h.editor.SetCommentInputValue("typed context")
	h.key("enter")
	require.Len(t, f.calls, 1)
	parsed, ok := reviewdraft.ParseReferences(f.bodies[0])
	require.True(t, ok)
	require.Equal(t, "typed context", parsed.General)
}

func TestReviewLeadWholeFileInFocusedChildStaysOpen(t *testing.T) {
	h, _ := newDraftHarnessWithoutOpen(t)
	child := &session.Session{ID: "s2", Title: "child", WorkingDir: "/child"}
	childApp := app.New(t.Context(), nil, child, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	_, err := h.m.supervisor.AddSession(t.Context(), childApp, child, "/child", nil)
	require.NoError(t, err)
	h.m.paneWorkspaces.active = &paneWorkspace{anchor: "s1"}
	h.m.supervisor.SwitchTo("s2")
	page := &reviewChatPage{overlayWidth: h.m.width}
	h.m.chatPage, h.m.chatPages["s2"] = page, page
	_, cmd := h.m.Update(sidebar.GitRequestMsg{Owner: "s2", Action: sidebar.GitOpenFile, WorktreeID: "w1", Path: "README.md"})
	h.pump(cmd)
	assert.Equal(t, "s2", h.m.gitReviewOwner)
	assert.NotNil(t, page.overlay)
}

func TestReviewLeadAcceptedGeneralToChildRestoresComposer(t *testing.T) {
	h, f := newDraftHarnessWithoutOpen(t)
	child := &session.Session{ID: "s2", Title: "child", WorkingDir: "/child"}
	childApp := app.New(t.Context(), nil, child, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	_, err := h.m.supervisor.AddSession(t.Context(), childApp, child, "/child", nil)
	require.NoError(t, err)
	h.m.paneWorkspaces.active = &paneWorkspace{anchor: "s1"}
	h.m.supervisor.SwitchTo("s2")
	page := &reviewChatPage{overlayWidth: h.m.width}
	h.m.chatPage, h.m.chatPages["s2"] = page, page
	h.open(true)
	h.editor.value = "ordinary chat"
	h.pump(h.m.startReviewGeneralInput("s1"))
	h.editor.SetCommentInputValue("general")
	h.pump(h.m.sendReviewFromComposer())
	require.Len(t, f.calls, 1)
	require.Equal(t, "s2", f.targets[0])
	assert.False(t, h.editor.CommentInputActive())
	h.key("enter")
	assert.Len(t, f.calls, 1)
}

func TestReviewLeadOversizeGeneralSwitchDoesNotStrandComposer(t *testing.T) {
	h, _ := newDraftHarness(t)
	child := &session.Session{ID: "s2", Title: "child", WorkingDir: "/child"}
	childApp := app.New(t.Context(), nil, child, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	_, err := h.m.supervisor.AddSession(t.Context(), childApp, child, "/child", nil)
	require.NoError(t, err)
	h.m.chatPages["s2"] = &reviewChatPage{overlayWidth: h.m.width}
	h.m.editors["s1"] = h.editor
	h.m.editors["s2"] = &reviewEditor{}
	h.m.sessionStates["s2"] = service.NewSessionState(child)
	h.pump(h.m.startReviewGeneralInput("s1"))
	h.editor.SetCommentInputValue(strings.Repeat("x", reviewdraft.MaxBodyBytes+1))
	_, cmd := h.m.handleSwitchTab("s2")
	require.NotNil(t, cmd)
	assert.Equal(t, "s1", h.m.paneFocus())
	assert.True(t, h.editor.CommentInputActive())
}

func TestReviewLeadStandaloneFilesFromFileInspectorClosesOverlay(t *testing.T) {
	h, _ := newDraftHarness(t)
	ws := h.m.activeReview()
	h.pump(h.m.openGitFilesInspector(ws, view.DocumentKey{Mode: view.ModeFiles, WorktreeID: "w1", Path: "README.md"}))
	require.NotNil(t, h.page.overlay)
	_, cmd := h.m.Update(messages.OpenGitReviewMsg{Files: true})
	h.pump(cmd)
	require.False(t, ws.inspecting)
	h.pump(h.m.closeGitReview("s1"))
	assert.Nil(t, h.page.overlay)
}

func TestReviewLeadMultilineCharacterAnchorIsAcceptedByRoot(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	ws := h.m.activeReview()
	a := view.Anchor{RepositoryID: "repo", RepositoryName: "work", WorktreeID: "w1", CheckoutPath: "/work", Path: "Dockerfile.dev", Mode: view.ModeChanges, Group: view.GroupUnstaged, NewStart: 1, NewEnd: 2, StartColumn: 10, EndColumn: 2, CharacterRange: true, Context: []string{"+0123456789abc", "+xy"}, Version: "valid"}
	cmd := h.m.handleReviewDraftMessage(view.AddCommentMsg{Owner: "s1", Anchor: a, Body: "multiline"})
	h.pump(cmd)
	require.Len(t, ws.draft.Snapshot().Comments, 1)
	require.Equal(t, 10, ws.draft.Snapshot().Comments[0].Anchor.StartColumn)
}

func TestReviewHostValidationFailureReturnsInlineACK(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	ws := h.m.activeReview()
	msg := view.AddCommentMsg{Owner: "s1", RequestID: "s1:0:77", Anchor: view.Anchor{Path: "../bad"}, Body: "bad anchor"}
	cmd := h.m.handleReviewDraftMessage(msg)
	require.NotNil(t, cmd)
	var got *view.CommentResultMsg
	batch, ok := cmd().(tea.BatchMsg)
	if ok {
		for _, child := range batch {
			if child == nil {
				continue
			}
			result := child()
			if ack, ok := result.(view.CommentResultMsg); ok {
				got = &ack
			}
		}
	}
	require.NotNil(t, got)
	require.Equal(t, msg.RequestID, got.RequestID)
	require.NotEmpty(t, got.Err)
	require.Empty(t, ws.draft.Snapshot().Comments)
}

func TestReviewHostMultilineAnchorAndCommentAck(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	ws := h.m.activeReview()
	anchor := view.Anchor{RepositoryID: "repo", RepositoryName: "work", WorktreeID: "w1", CheckoutPath: "/work", Path: "Dockerfile.dev", Mode: view.ModeChanges, Group: view.GroupUnstaged, NewStart: 1, NewEnd: 2, StartColumn: 10, EndColumn: 2, CharacterRange: true, Context: []string{"+0123456789abc", "+xy"}, Version: "valid"}
	add := view.AddCommentMsg{Owner: "s1", RequestID: "r1", Anchor: anchor, Body: "selected lines"}
	cmd := h.m.handleReviewDraftMessage(add)
	h.pump(cmd)
	require.Len(t, ws.draft.Snapshot().Comments, 1)
	require.Equal(t, 10, ws.draft.Snapshot().Comments[0].Anchor.StartColumn)
	require.Equal(t, 2, ws.draft.Snapshot().Comments[0].Anchor.EndColumn)
	rejected := view.AddCommentMsg{Owner: "s1", RequestID: "r2", Anchor: view.Anchor{Path: "../bad"}, Body: "invalid"}
	cmd = h.m.handleReviewDraftMessage(rejected)
	var ack view.CommentResultMsg
	batch, ok := cmd().(tea.BatchMsg)
	require.True(t, ok)
	for _, child := range batch {
		if child == nil {
			continue
		}
		if result, ok := child().(view.CommentResultMsg); ok {
			ack = result
		}
	}
	require.Equal(t, "r2", ack.RequestID)
	require.NotEmpty(t, ack.Err)
}

func TestReviewAcceptanceDoesNotClearAnotherWorkspaceComposer(t *testing.T) {
	h, _ := newDraftHarness(t)
	h.comment("first inline")
	h.pump(h.m.startReviewGeneralInput("s1"))
	h.editor.SetCommentInputValue("same general")
	cmd := h.m.sendReviewFromComposer()
	require.NotNil(t, cmd)
	result, ok := cmd().(gitReviewSendResult)
	require.True(t, ok)
	h.m.finishReviewComment(false)

	child := &session.Session{ID: "s2", Title: "second", WorkingDir: "/second"}
	childApp := app.New(t.Context(), nil, child, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	_, err := h.m.supervisor.AddSession(t.Context(), childApp, child, "/second", nil)
	require.NoError(t, err)
	h.m.supervisor.SwitchTo("s2")
	page := &reviewChatPage{overlayWidth: h.m.width}
	h.m.chatPage, h.m.chatPages["s2"] = page, page
	h.m.application = childApp
	h.editor = &reviewEditor{value: "second ordinary"}
	h.m.editor = h.editor
	h.open(false)
	require.Equal(t, "s2", h.m.activeReview().anchor)
	h.pump(h.m.startReviewGeneralInput("s2"))
	h.editor.SetCommentInputValue("same general")
	require.True(t, h.editor.CommentInputActive())

	h.m.applyGitReviewSend(result)
	assert.True(t, h.editor.CommentInputActive(), "an accepted s1 review must not close the current s2 review composer")
	assert.Equal(t, "same general", h.editor.CommentInputValue(), "another workspace's unsaved input is preserved")
}
