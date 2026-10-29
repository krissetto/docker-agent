package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/gitreview"
	view "github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
)

// browseBootstrapProvider counts each bootstrap entry point. Its full scan
// honours cancellation, so a superseded Changes bootstrap returns without
// touching the workspace.
type browseBootstrapProvider struct {
	*fakeReviewProvider

	browseErr error
	// scan, when set, holds full scans in flight until their context ends.
	scan *blockingScan
}

// blockingScan reports when a full scan has entered Snapshot.
type blockingScan struct {
	entered chan struct{}
	block   bool
}

func newBlockingScan() *blockingScan { return &blockingScan{entered: make(chan struct{}, 1)} }

func (p browseBootstrapProvider) Snapshot(ctx context.Context) (gitreview.Snapshot, error) {
	if p.scan != nil && p.scan.block {
		p.scan.entered <- struct{}{}
		<-ctx.Done()
		return gitreview.Snapshot{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return gitreview.Snapshot{}, err
	}
	return p.fakeReviewProvider.Snapshot(ctx)
}

func (p browseBootstrapProvider) BrowseRoot(ctx context.Context) (gitreview.Snapshot, error) {
	if p.browseErr != nil {
		return gitreview.Snapshot{}, p.browseErr
	}
	if err := ctx.Err(); err != nil {
		return gitreview.Snapshot{}, err
	}
	return p.fakeReviewProvider.BrowseRoot(ctx)
}

func newBrowseHarness(t *testing.T) (*reviewHarness, browseBootstrapProvider) {
	t.Helper()
	h := newReviewHarness(t, 140)
	provider := browseBootstrapProvider{fakeReviewProvider: h.provider, scan: newBlockingScan()}
	h.m.gitReviewFactory = func(*app.App, string, string) (gitreview.Provider, *view.Snapshot, error) { return provider, nil, nil }
	return h, provider
}

// browseRequest returns the command a sidebar request produces without running it,
// so tests can order superseded and current work deterministically.
func browseRequest(h *reviewHarness, owner string, action sidebar.GitAction) tea.Cmd {
	_, cmd := h.m.Update(sidebar.GitRequestMsg{Owner: owner, Action: action})
	return cmd
}

func TestFilesSidebarRequestBootstrapsRootWithoutScan(t *testing.T) {
	h, provider := newBrowseHarness(t)
	owner := h.m.paneFocus()
	h.pump(browseRequest(h, owner, sidebar.GitBrowse))
	require.Equal(t, 1, provider.browses)
	require.Equal(t, 0, provider.snapshots, "Files opens without a repository scan")
	require.Empty(t, provider.dirs, "the sidebar, not the bootstrap, requests the root listing")
	ws := h.m.gitReviews[owner]
	require.NotNil(t, ws.snapshot)
	require.True(t, ws.snapshot.Browse)
	require.Equal(t, view.ModeFiles, ws.sidebarMode)

	generation := ws.snapshot.Generation
	h.pump(browseRequest(h, owner, sidebar.GitBrowse))
	require.Equal(t, 1, provider.browses, "a repeated Files request reuses the root")
	require.Equal(t, 0, provider.snapshots)
	require.Equal(t, generation, ws.snapshot.Generation)
}

func TestStandaloneFilesUsesRootBootstrap(t *testing.T) {
	h, provider := newBrowseHarness(t)
	h.open(true)
	require.Equal(t, 1, provider.browses)
	require.Equal(t, 0, provider.snapshots)
}

func TestFilesRefreshReadsRootAndKeepsGeneration(t *testing.T) {
	h, provider := newBrowseHarness(t)
	owner := h.m.paneFocus()
	h.pump(browseRequest(h, owner, sidebar.GitBrowse))
	ws := h.m.gitReviews[owner]
	generation := ws.snapshot.Generation
	h.pump(browseRequest(h, owner, sidebar.GitRefresh))
	require.Equal(t, 2, provider.browses, "refresh in Files re-reads the root")
	require.Equal(t, 0, provider.snapshots, "refresh in Files never runs a repository scan")
	require.Equal(t, generation, ws.snapshot.Generation, "the root keeps its generation")
}

func TestSelectingChangesAfterFilesRunsOneExplicitScan(t *testing.T) {
	h, provider := newBrowseHarness(t)
	owner := h.m.paneFocus()
	h.pump(browseRequest(h, owner, sidebar.GitBrowse))
	h.pump(browseRequest(h, owner, sidebar.GitShowChanges))
	require.Equal(t, 1, provider.snapshots, "Changes performs exactly one explicit scan")
	require.Equal(t, 1, provider.browses)
	ws := h.m.gitReviews[owner]
	require.False(t, ws.snapshot.Browse, "root-only data is replaced, not shown as complete")
	require.Equal(t, view.ModeChanges, ws.sidebarMode)
}

func TestFilesDuringChangesBootstrapSupersedesFullScan(t *testing.T) {
	h, provider := newBrowseHarness(t)
	owner := h.m.paneFocus()
	changes := browseRequest(h, owner, sidebar.GitShowChanges)
	browse := browseRequest(h, owner, sidebar.GitBrowse)
	h.pump(browse)
	h.pump(changes) // superseded: its context is cancelled and its result is dropped
	require.Equal(t, 1, provider.browses)
	require.Equal(t, 0, provider.snapshots, "the superseded Changes bootstrap never applies a full scan")
	ws := h.m.gitReviews[owner]
	require.True(t, ws.snapshot.Browse)
	require.Equal(t, view.ModeFiles, ws.sidebarMode)
}

func TestChangesSelectedDuringPendingBrowseScansOnceOnResult(t *testing.T) {
	h, provider := newBrowseHarness(t)
	owner := h.m.paneFocus()
	browse := browseRequest(h, owner, sidebar.GitBrowse)
	changes := browseRequest(h, owner, sidebar.GitShowChanges)
	h.pump(browse)
	h.pump(changes)
	require.Equal(t, 1, provider.browses)
	require.Equal(t, 1, provider.snapshots, "the browse result triggers exactly one explicit Git scan")
	require.False(t, h.m.gitReviews[owner].snapshot.Browse)
}

func TestFilesOwnerIsolationIgnoresUnfocusedSessions(t *testing.T) {
	h, provider := newBrowseHarness(t)
	h.pump(browseRequest(h, "not-focused", sidebar.GitBrowse))
	require.Equal(t, 0, provider.browses)
	require.NotContains(t, h.m.gitReviews, "not-focused", "an unfocused request creates no workspace")
}

func TestFilesBrowseFailureNeverFallsBackToScan(t *testing.T) {
	h, provider := newBrowseHarness(t)
	provider.browseErr = errors.New("Git review folder browsing is not supported by this server")
	h.m.gitReviewFactory = func(*app.App, string, string) (gitreview.Provider, *view.Snapshot, error) { return provider, nil, nil }
	owner := h.m.paneFocus()
	h.pump(browseRequest(h, owner, sidebar.GitBrowse))
	require.Equal(t, 0, provider.snapshots, "an unsupported browse never runs a repository scan")
	ws := h.m.gitReviews[owner]
	require.NotNil(t, ws.snapshot)
	require.Contains(t, ws.snapshot.Scope.Unavailable, "not supported by this server")
}

// withBlockingScan makes full scans of the provider block inside Snapshot until
// their context ends, so the test controls exactly when superseding happens.
func withBlockingScan(provider browseBootstrapProvider) *blockingScan {
	provider.scan.block = true
	return provider.scan
}

func awaitEntered(t *testing.T, scan *blockingScan) {
	t.Helper()
	select {
	case <-scan.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the full scan never entered Snapshot")
	}
}

// runInFlight runs cmd on its own goroutine and returns the task results it
// produces, so a test can supersede the work while it is still blocked.
func runInFlight(cmd tea.Cmd) <-chan []tea.Msg {
	out := make(chan []tea.Msg, 1)
	go func() {
		var results []tea.Msg
		var collect func(tea.Cmd)
		collect = func(c tea.Cmd) {
			if c == nil {
				return
			}
			switch msg := c().(type) {
			case tea.BatchMsg:
				for _, next := range msg {
					collect(next)
				}
			case gitReviewTaskResult:
				results = append(results, msg)
			}
		}
		collect(cmd)
		out <- results
	}()
	return out
}

// runInFlightWith is runInFlight for flows that start with identity resolution
// (no provider yet). Identity is applied on the caller's goroutine, so the model is
// never touched from the background; only the bootstrap that follows runs in flight.
// Use runInFlight directly when the provider is already bootstrapped: identityFirst
// would execute a blocking full scan on the caller's goroutine.
func runInFlightWith(h *reviewHarness, cmd tea.Cmd) <-chan []tea.Msg {
	return runInFlight(identityFirst(h, cmd))
}

// identityFirst executes cmd here, applies identity results to the model, and
// returns the follow-up commands unexecuted.
func identityFirst(h *reviewHarness, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		follow := make([]tea.Cmd, 0, len(msg))
		for _, child := range msg {
			follow = append(follow, identityFirst(h, child))
		}
		return tea.Batch(follow...)
	case gitReviewIdentityMsg:
		_, next := h.m.Update(msg)
		return next
	default:
		return func() tea.Msg { return msg }
	}
}

func awaitResults(t *testing.T, results <-chan []tea.Msg) []tea.Msg {
	t.Helper()
	select {
	case msgs := <-results:
		return msgs
	case <-time.After(5 * time.Second):
		t.Fatal("the superseded work never returned")
		return nil
	}
}

func feedResults(h *reviewHarness, msgs []tea.Msg) {
	for _, msg := range msgs {
		h.pump(func() tea.Msg { return msg })
	}
}

func TestFilesSupersedesRunningFullScanAndReadsRoot(t *testing.T) {
	h, provider := newBrowseHarness(t)
	owner := h.m.paneFocus()
	h.pump(browseRequest(h, owner, sidebar.GitBrowse))
	scan := withBlockingScan(provider)
	// The provider is already bootstrapped, so this request has no identity step to
	// apply first; the whole command, including the blocking scan, runs in flight.
	results := runInFlight(browseRequest(h, owner, sidebar.GitShowChanges))
	awaitEntered(t, scan) // the full scan is inside Snapshot right now

	h.pump(browseRequest(h, owner, sidebar.GitBrowse))
	feedResults(h, awaitResults(t, results))

	require.Equal(t, 0, provider.snapshots, "the superseded full scan never applies")
	require.Equal(t, 2, provider.browses, "Files re-reads only the root")
	ws := h.m.gitReviews[owner]
	require.True(t, ws.snapshot.Browse)
	require.Equal(t, view.ModeFiles, ws.sidebarMode)
	require.NotContains(t, h.m.gitReviewPending, "snapshot", "no full scan remains pending")
}

func TestFilesDuringRunningChangesBootstrapRestartsAsRootOnly(t *testing.T) {
	h, provider := newBrowseHarness(t)
	owner := h.m.paneFocus()
	scan := withBlockingScan(provider)
	results := runInFlightWith(h, browseRequest(h, owner, sidebar.GitShowChanges))
	awaitEntered(t, scan)

	h.pump(browseRequest(h, owner, sidebar.GitBrowse))
	feedResults(h, awaitResults(t, results))

	require.Equal(t, 0, provider.snapshots)
	require.Equal(t, 1, provider.browses)
	ws := h.m.gitReviews[owner]
	require.True(t, ws.snapshot.Browse)
	require.Equal(t, view.ModeFiles, ws.sidebarMode)
}

func TestFilesRefreshDoesNotStatusLoadExpandedGitWorktrees(t *testing.T) {
	h, provider := newBrowseHarness(t)
	owner := h.m.paneFocus()
	h.pump(browseRequest(h, owner, sidebar.GitShowChanges))
	require.Equal(t, 1, provider.snapshots)
	h.page.watch = sidebar.GitWatchMsg{Owner: owner, Open: true, Mode: view.ModeChanges, ExpandedWorktrees: []string{"w1", "w2"}}
	loads := len(provider.worktrees)

	h.pump(browseRequest(h, owner, sidebar.GitBrowse))
	h.pump(browseRequest(h, owner, sidebar.GitRefresh))
	require.Equal(t, 1, provider.browses, "the full scan was the bootstrap; this refresh is the only root read")
	require.Len(t, provider.worktrees, loads, "a root read never re-runs Git status for expanded worktrees")
}

// Files never schedules Git status for expanded checkouts, even when the
// sidebar's cached expansion still names them.
func TestFilesWatchDoesNotScheduleGitStatusForExpandedWorktrees(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false) // the Changes review, exactly as the existing watch test opens it
	page, ok := h.m.gitPage("s1")
	require.True(t, ok)
	page.(*reviewChatPage).watch = sidebar.GitWatchMsg{Owner: "s1", Open: true, ExpandedWorktrees: []string{"w1"}}
	watch := sidebar.GitWatchMsg{Owner: "s1", Open: true, Mode: view.ModeChanges, ExpandedWorktrees: []string{"w1"}}
	_, cmd := h.m.Update(watch)
	h.pump(cmd)
	require.True(t, h.m.gitRefreshScheduled, "Changes keeps refreshing expanded checkouts")

	h.open(true) // standalone Files closes the Changes inspector, whose own checkout is legitimately refreshed
	watch.Mode = view.ModeFiles
	_, cmd = h.m.Update(watch)
	h.pump(cmd)
	require.False(t, h.m.gitRefreshScheduled, "Files schedules no Git status refresh")
}
