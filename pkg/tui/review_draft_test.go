package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	view "github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/reviewdraft"
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
	h.key("j", "a")
	require.True(h.t, h.m.reviewModel().Capturing(), "comment editor open")
	for _, r := range text {
		h.key(string(r))
	}
	h.key("enter")
}

func (h *reviewHarness) draft() reviewdraft.Snapshot { return h.m.activeReview().draft.Snapshot() }

// previewTo previews a send from the draft list. Changes (Git) reviews go to
// the current agent without a picker; Files reviews pick the first target.
func (h *reviewHarness) previewTo() {
	h.t.Helper()
	h.key("R", "s")
	if h.m.activeReview().mode == view.ModeFiles {
		require.True(h.t, h.m.dialogMgr.Open(), "Files reviews choose a recipient")
		h.key("enter")
	}
	require.False(h.t, h.m.dialogMgr.Open(), "Git reviews never show a picker")
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

	h.key("enter")
	require.Len(t, f.calls, 1)
	assert.Contains(t, f.bodies[0], "Comment (literal user text):\nWhy bump Go?")
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
	h.key("esc") // close the preview; the attempt stays unresolved
	h.key("s")
	assert.False(t, h.m.dialogMgr.Open(), "no picker while an attempt is unresolved")
	assert.Contains(t, h.content(), "Send review to current agent · ◆ main session", "the original recipient is restored")
	h.key("enter")
	require.Len(t, f.calls, 2)
	assert.Equal(t, f.calls[0], f.calls[1])
	assert.Empty(t, h.draft().Comments)
}
