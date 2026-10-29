package tui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	view "github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/reviewdraft"
)

type fakeCatalog struct {
	mu    sync.Mutex
	calls []string
	total int
}

func (c *fakeCatalog) page(_ context.Context, cursor string, limit int) ([]runtime.SessionSummaryEntry, string, error) {
	c.mu.Lock()
	c.calls = append(c.calls, cursor)
	c.mu.Unlock()
	start, _ := strconv.Atoi(cursor)
	var out []runtime.SessionSummaryEntry
	for i := start; i < min(start+limit, c.total); i++ {
		out = append(out, runtime.SessionSummaryEntry{SessionID: fmt.Sprintf("h%d", i), Title: fmt.Sprintf("hidden %d", i), AgentName: "reviewer", Loadable: true})
	}
	next := ""
	if start+limit < c.total {
		next = strconv.Itoa(start + limit)
	}
	return out, next, nil
}

type fakeResolver struct {
	released int
	err      error
	apps     map[string]*app.App
}

func (r *fakeResolver) resolve(_ context.Context, id string) (*app.App, func(), error) {
	if r.err != nil {
		return nil, nil, r.err
	}
	return r.apps[id], func() { r.released++ }, nil
}

func newHiddenHarness(t *testing.T) (*reviewHarness, *fakeSubmitter, *fakeResolver, *fakeCatalog) {
	t.Helper()
	h, f := newFilesDraftHarness(t)
	catalog := &fakeCatalog{total: 45}
	hidden := &session.Session{ID: "h0", Title: "hidden 0", WorkingDir: "/other"}
	res := &fakeResolver{apps: map[string]*app.App{"h0": app.New(t.Context(), nil, hidden, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))}}
	h.m.gitReviewCatalog = catalog.page
	h.m.gitReviewResolve = res.resolve
	h.m.gitReviewCapture = func(a *app.App) app.ReviewSubmitTarget {
		return app.ReviewSubmitTarget{SessionID: a.Session().ID, Handle: reviewHandle{}}
	}
	return h, f, res, catalog
}

// pickHidden loads one catalog page and chooses the first hidden session.
func (h *reviewHarness) pickHidden() {
	h.t.Helper()
	h.pump(h.m.openGitReviewTargetPicker(view.PreviewDraftMsg{Owner: "s1", Revision: h.m.activeReview().draft.Snapshot().Revision}))
	require.True(h.t, h.m.dialogMgr.Open())
	h.key("end", "enter") // Load more…
	h.key("h", "i", "d", "d", "e", "n", " ", "0", "enter")
}

func TestReviewHiddenSessionSendWithoutTab(t *testing.T) {
	h, f, res, catalog := newHiddenHarness(t)
	h.comment("for the hidden reviewer")
	h.pickHidden()
	assert.Equal(t, []string{""}, catalog.calls, "one bounded page, loaded on request")
	require.Contains(t, h.content(), "Send review to ◇ hidden 0")
	tabs, _ := h.m.supervisor.GetTabs()
	assert.Len(t, tabs, 1, "no tab is opened for the recipient")
	assert.Zero(t, res.released, "the borrowed view stays while previewing")

	h.key("enter")
	require.Len(t, f.calls, 1)
	assert.Empty(t, h.draft().Comments)
	assert.Equal(t, 1, res.released, "the borrowed view is released after acceptance")
}

func TestReviewHiddenCancelReleasesAndFailureRetains(t *testing.T) {
	h, _, res, _ := newHiddenHarness(t)
	h.comment("cancel me")
	h.pickHidden()
	h.key("esc")
	assert.Equal(t, 1, res.released, "cancelling the preview releases the borrowed view")

	h2, f2, res2, _ := newHiddenHarness(t)
	h2.comment("retry me")
	h2.pickHidden()
	f2.failNext = errors.New("timeout")
	h2.key("enter")
	assert.Zero(t, res2.released, "an unresolved send keeps its recipient")
	h2.key("esc", "R", "s")
	h2.key("enter")
	require.Len(t, f2.calls, 2)
	assert.Equal(t, f2.calls[0], f2.calls[1], "retry reuses the request ID")
	assert.Equal(t, 1, res2.released)
}

func TestReviewHiddenMissingTargetKeepsDraft(t *testing.T) {
	h, f, res, _ := newHiddenHarness(t)
	res.err = errors.New("session gone")
	h.comment("keep")
	h.pickHidden()
	assert.NotContains(t, h.content(), "Send review to")
	assert.Empty(t, f.calls)
	assert.Len(t, h.draft().Comments, 1)
}

func TestReviewTargetPagesAreBoundedAndStaleResultsIgnored(t *testing.T) {
	h, _, _, catalog := newHiddenHarness(t)
	catalog.total = 1000
	h.comment("pages")
	h.pump(h.m.openGitReviewTargetPicker(view.PreviewDraftMsg{Owner: "s1", Revision: h.m.activeReview().draft.Snapshot().Revision}))
	for range gitReviewTargetMaxPages {
		h.key("end", "enter")
	}
	assert.Len(t, catalog.calls, gitReviewTargetMaxPages, "never more than the page cap")
	assert.Equal(t, gitReviewTargetMaxPages, h.m.gitReviewPicker.pages)
	assert.Nil(t, h.m.loadGitReviewCatalogPage(h.m.gitReviewPicker), "the cap holds even if asked directly")
	choice := h.m.gitReviewPicker
	require.NotNil(t, choice)
	h.key("esc")
	assert.Nil(t, h.m.gitReviewPicker)
	require.Error(t, choice.ctx.Err(), "closing the picker cancels loading")
	assert.Nil(t, h.m.applyGitReviewCatalog(gitReviewCatalogResult{choice: choice, entries: []runtime.SessionSummaryEntry{{SessionID: "late"}}}), "late pages never reach a newer picker")
}

type fakeFactory struct {
	mu       sync.Mutex
	requests []ReviewSessionRequest
	failNext error
	source   string
}

func (f *fakeFactory) agent(name, description string) ReviewSessionAgent {
	return ReviewSessionAgent{Name: name, Description: description, Source: f.source}
}

func (f *fakeFactory) ListAgents(context.Context) ([]ReviewSessionAgent, error) {
	return []ReviewSessionAgent{f.agent("root", "default"), f.agent("reviewer", "code review")}, nil
}

// CreateAndSubmit enforces the production contract: a source is mandatory.
func (f *fakeFactory) CreateAndSubmit(_ context.Context, r ReviewSessionRequest) (ReviewSessionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
	if r.Source == "" || r.Source != f.source {
		return ReviewSessionResult{}, errors.New("source mismatch")
	}
	if err := f.failNext; err != nil {
		f.failNext = nil
		return ReviewSessionResult{}, err
	}
	return ReviewSessionResult{SessionID: r.SessionID, TurnID: "t1", Title: "Review", Accepted: true}, nil
}

func TestReviewNewSessionRefusesUnknownSource(t *testing.T) {
	h, _ := newFilesDraftHarness(t)
	factory := &fakeFactory{}
	h.m.reviewSessionFactory = factory
	h.comment("needs a source")
	h.pump(h.m.openGitReviewTargetPicker(view.PreviewDraftMsg{Owner: "s1", Revision: h.m.activeReview().draft.Snapshot().Revision}))
	h.key("N", "e", "w", "enter")
	h.key("r", "e", "v", "i", "e", "w", "e", "r", "enter")
	assert.NotContains(t, h.content(), "Send review to new", "no preview without an authoritative source")
	assert.Empty(t, factory.requests, "nothing is created")
	assert.Len(t, h.draft().Comments, 1)
}

func TestReviewNewSessionCreatesOnlyAfterConfirm(t *testing.T) {
	h, _ := newFilesDraftHarness(t)
	factory := &fakeFactory{source: "local:hackerspace"}
	h.m.reviewSessionFactory = factory
	h.comment("new reviewer please")
	h.pump(h.m.openGitReviewTargetPicker(view.PreviewDraftMsg{Owner: "s1", Revision: h.m.activeReview().draft.Snapshot().Revision}))
	h.key("N", "e", "w", "enter")
	require.True(t, h.m.dialogMgr.Open(), "the agent chooser opens")
	h.key("r", "e", "v", "i", "e", "w", "e", "r", "enter")
	assert.Contains(t, h.content(), "Send review to new reviewer session in /work · local:hackerspace")
	assert.Empty(t, factory.requests, "nothing is created before confirmation")

	factory.failNext = errors.New("startup failed")
	h.key("enter")
	require.Len(t, factory.requests, 1)
	assert.Len(t, h.draft().Comments, 1, "a failed creation keeps the draft")
	h.key("enter")
	require.Len(t, factory.requests, 2)
	first, second := factory.requests[0], factory.requests[1]
	assert.Equal(t, first.SessionID, second.SessionID, "retry reuses the reserved session")
	assert.Equal(t, first.RequestID, second.RequestID)
	assert.Equal(t, first.Body, second.Body, "the exact payload is resent")
	assert.Equal(t, "reviewer", second.AgentName)
	assert.Equal(t, "/work", second.WorkingDir)
	assert.Equal(t, "local:hackerspace", second.Source, "the chosen agent's source is passed through")
	parsed, ok := reviewdraft.ParseReferences(second.Body)
	require.True(t, ok)
	require.Equal(t, "new reviewer please", parsed.Comments[0].Comment)
	assert.Empty(t, h.draft().Comments)
	tabs, _ := h.m.supervisor.GetTabs()
	assert.Len(t, tabs, 1, "the new session does not take a tab or focus")
	assert.Contains(t, ansi.Strip(h.m.messageBar.View()), "Open", "success offers an explicit Open")
}

func TestReviewPickerDefaultsToFocusedSession(t *testing.T) {
	h, _ := newFilesDraftHarness(t)
	focused := &session.Session{ID: "s2", Title: "focused child", WorkingDir: "/work"}
	_, err := h.m.supervisor.AddSession(t.Context(), app.New(t.Context(), nil, focused, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{})), focused, "/work", nil)
	require.NoError(t, err)
	h.m.gitReviewCapture = func(a *app.App) app.ReviewSubmitTarget {
		return app.ReviewSubmitTarget{SessionID: a.Session().ID, Handle: reviewHandle{}}
	}
	h.comment("to the focused one")
	h.m.paneWorkspaces.active = &paneWorkspace{anchor: "s1"}
	h.m.supervisor.SwitchTo("s2")
	h.pump(h.m.openGitReviewTargetPicker(view.PreviewDraftMsg{Owner: "s1", Revision: h.m.activeReview().draft.Snapshot().Revision}))
	h.key("enter")
	assert.Contains(t, h.content(), "Send review to ◆ focused child", "the focused session is the default recipient")
}

func TestReviewStaleResolveLeavesNewPickerOpen(t *testing.T) {
	h, _, res, _ := newHiddenHarness(t)
	h.comment("stale")
	h.pump(h.m.openGitReviewTargetPicker(view.PreviewDraftMsg{Owner: "s1", Revision: h.m.activeReview().draft.Snapshot().Revision}))
	old := h.m.gitReviewPicker
	h.key("esc")
	h.key("s")
	current := h.m.gitReviewPicker
	require.NotSame(t, old, current)
	h.pump(h.m.handleGitReviewTargetMessage(gitReviewResolved{choice: old, target: catalogTarget(runtime.SessionSummaryEntry{SessionID: "h0", Loadable: true}), app: res.apps["h0"], lease: &gitReviewLease{release: func() { res.released++ }}}))
	assert.Same(t, current, h.m.gitReviewPicker, "a late resolve never closes the newer picker")
	assert.Equal(t, 1, res.released, "and its borrowed view is released")
}

func TestReviewNewSessionHiddenWithoutFactory(t *testing.T) {
	h, _ := newFilesDraftHarness(t)
	h.comment("x")
	h.pump(h.m.openGitReviewTargetPicker(view.PreviewDraftMsg{Owner: "s1", Revision: h.m.activeReview().draft.Snapshot().Revision}))
	require.True(t, h.m.dialogMgr.Open())
	h.key("N", "e", "w", "enter")
	assert.True(t, h.m.dialogMgr.Open(), "without a factory there is no New session row to choose")
	require.NotNil(t, h.m.gitReviewPicker)
	assert.Nil(t, h.m.gitReviewPicker.agents, "no agent chooser")
}

type pagerRuntime struct {
	runtime.SessionRuntime
	requests []runtime.SessionSummaryPageOptions
}

func (p *pagerRuntime) ListSessionSummaryPage(_ context.Context, o runtime.SessionSummaryPageOptions) (runtime.SessionSummaryPage, error) {
	p.requests = append(p.requests, o)
	return runtime.SessionSummaryPage{Entries: []runtime.SessionSummaryEntry{{SessionID: "x", Loadable: true}}, NextCursor: "c2"}, nil
}

func TestReviewProductionPagerRequestsOneBoundedPage(t *testing.T) {
	h, _ := newDraftHarness(t)
	rt := &pagerRuntime{}
	pager := reviewPagerFor(rt)
	require.NotNil(t, pager, "the session runtime's bounded pager is used")
	entries, next, err := pager(t.Context(), "c1", gitReviewTargetPage)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	assert.Equal(t, "c2", next)
	assert.Equal(t, []runtime.SessionSummaryPageOptions{{IncludeChildren: true, Limit: 20, Cursor: "c1"}}, rt.requests)

	assert.Nil(t, h.m.gitReviewCatalogPager(), "no unpaged fallback when the runtime has no pager")
}
