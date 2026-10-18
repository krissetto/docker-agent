package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"fmt"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/gitreview"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	view "github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// withRealPage swaps the mock for a real chat page with the normal sidebar.
func withRealPage(t *testing.T, h *reviewHarness) chat.Page {
	t.Helper()
	// A virtual clock lets the sidebar's row animations settle deterministically.
	h.pageAR = animation.NewRuntimeWithScheduler(&rootImmediateScheduler{now: time.Unix(1, 0)})
	page := chat.New(h.pageAR, t.Context(), h.m.application, service.NewSessionState(h.sess))
	t.Cleanup(func() { chat.Cleanup(page); h.pageAR.Stop() })
	h.m.chatPage, h.m.chatPages["s1"] = page, page
	page.SetSize(h.m.width, h.m.contentHeight)
	h.pump(h.m.resizePanes()) // the root lays the page out like a running program
	h.m.viewCacheValid = false
	return page
}

// settle drives finite sidebar animations so the painted rows are final.
func (h *reviewHarness) settle() {
	h.t.Helper()
	ar := h.pageAR
	if ar == nil {
		return
	}
	for steps := 0; ar.HasActive(); steps++ {
		require.Less(h.t, steps, 200, "animations must settle")
		raw, isTick := ar.Continue()().(animation.TickMsg)
		if !isTick {
			return
		}
		tick, ok := ar.Accept(raw)
		if !ok {
			continue
		}
		_, cmd := h.m.chatPage.Update(tick)
		h.pump(cmd)
	}
}

// clickText physically clicks the first rendered cell of label in the root frame.
func (h *reviewHarness) clickText(label string) {
	h.t.Helper()
	h.settle()
	h.m.viewCacheValid = false
	frame := ansi.Strip(h.m.composeView().Content)
	for y, line := range strings.Split(frame, "\n") {
		i := strings.Index(line, label)
		if i < 0 {
			continue
		}
		x := ansi.StringWidth(line[:i])
		_, cmd := h.m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		h.pump(cmd)
		_, cmd = h.m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
		h.pump(cmd)
		return
	}
	h.t.Fatalf("%q not rendered:\n%s", label, frame)
}

func TestReviewRootClickSidebarFileShowsDiffBesideNormalSidebar(t *testing.T) {
	h, f := newDraftHarnessWithoutOpen(t)
	withRealPage(t, h)
	h.pump(h.m.syncGitReview())
	h.settle()
	assert.Zero(t, h.provider.snapshots, "nothing is scanned before the section is opened")

	h.clickText("Git changes")
	assert.Equal(t, 1, h.provider.snapshots, "opening the section runs discovery once")
	assert.Empty(t, h.provider.worktrees)
	h.settle()
	h.settle()
	h.m.viewCacheValid = false
	frame := ansi.Strip(h.m.composeView().Content)
	assert.Contains(t, frame, "└ work")
	assert.NotContains(t, frame, "● main", "repositories start collapsed")

	h.clickText("└ work")
	assert.Empty(t, h.provider.worktrees, "expanding a repository lists worktrees without scanning them")
	h.clickText("● main")
	assert.NotContains(t, h.provider.worktrees, "w2", "other worktrees are never scanned")
	assert.NotContains(t, h.provider.worktrees, "w3")
	h.clickText("Unstaged")
	h.clickText("Dockerfile.dev")
	h.settle()
	h.m.viewCacheValid = false

	frame = ansi.Strip(h.m.composeView().Content)
	t.Log("\n" + frame)
	assert.Contains(t, frame, "- FROM golang:1.22", "the inspector shows the diff")
	assert.Contains(t, frame, "+ FROM golang:1.27")
	assert.Contains(t, frame, "Dockerfile.dev")
	assert.Contains(t, frame, "● main", "the normal sidebar stays beside it")
	assert.Contains(t, frame, "Unstaged")
	assert.Contains(t, frame, "New session", "with its usual session section")
	assert.NotContains(t, frame, "Loading…")

	model := h.m.reviewModel()
	key := view.DocumentKey{Mode: view.ModeChanges, WorktreeID: "w1", Path: "Dockerfile.dev", Group: view.GroupUnstaged}
	doc := view.Document{Key: key, State: view.StateReady, Lines: []view.Line{{Kind: view.LineHunk, Text: "@@ -1,50 +1,50 @@"}}}
	for n := 0; n < 50; n++ {
		doc.Lines = append(doc.Lines, view.Line{Kind: view.LineContext, Old: n + 1, New: n + 1, Text: fmt.Sprintf("content line %d", n)})
	}
	model.SetDocument(doc)
	page := h.m.chatPage.(interface{ ContentOverlayBounds() (int, int, int, int) })
	ox, oy, _, _ := page.ContentOverlayBounds()
	beforeWheel := ansi.Strip(model.View())
	var wheelCmd tea.Cmd
	for range 12 {
		_, wheelCmd = h.m.Update(messages.WheelCoalescedMsg{X: ox + 10, Y: oy + 4, Delta: 12})
		h.pump(wheelCmd)
	}
	afterWheel := ansi.Strip(model.View())
	assert.NotEqual(t, beforeWheel, afterWheel, "the root coalesced wheel scrolls the sidebar-opened diff")
	frame = ansi.Strip(h.m.composeView().Content)
	var sidebarY, sidebarX = -1, -1
	for y, line := range strings.Split(frame, "\n") {
		if strings.Contains(line, "3 changes") {
			sidebarY = y
			sidebarX = ansi.StringWidth(line[:strings.Index(line, "3 changes")]) + 1
			break
		}
	}
	require.GreaterOrEqual(t, sidebarY, 0, "the real Git sidebar remains visible beside the inspector")
	_, wheelCmd = h.m.Update(messages.WheelCoalescedMsg{X: sidebarX, Y: sidebarY, Delta: 12})
	h.pump(wheelCmd)
	assert.Equal(t, afterWheel, ansi.Strip(model.View()), "a wheel over the independent sidebar does not scroll the open diff")
	h.key("down")
	assert.True(t, model.Focused(), "keyboard navigation remains intact after mouse wheel routing")

	h.comment("why bump?")
	h.key("R", "s")
	assert.False(t, h.m.dialogMgr.Open(), "Git reviews never show a picker")
	assert.Contains(t, ansi.Strip(h.m.composeView().Content), "Send review to current agent")
	h.key("enter")
	require.Len(t, f.calls, 1, "sent to the current agent after confirmation")
	assert.Empty(t, h.draft().Comments)
}

func TestReviewGitScopeFollowsFocusedPaneSession(t *testing.T) {
	h, f := newDraftHarnessWithoutOpen(t)
	child := &session.Session{ID: "s2", Title: "child", WorkingDir: "/child"}
	childApp := app.New(t.Context(), nil, child, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	_, err := h.m.supervisor.AddSession(t.Context(), childApp, child, "/child", nil)
	require.NoError(t, err)
	childPage := &reviewChatPage{overlayWidth: h.m.width}
	h.m.chatPages["s2"] = childPage
	var dirs []string
	h.m.gitReviewFactory = func(_ *app.App, _ string, dir string) (gitreview.Provider, *view.Snapshot, error) {
		dirs = append(dirs, dir)
		return h.provider, nil, nil
	}
	h.m.paneWorkspaces.active = &paneWorkspace{anchor: "s1"}
	h.m.supervisor.SwitchTo("s2")
	h.m.chatPage = childPage

	_, cmd := h.m.Update(messages.RevealGitChangesMsg{})
	h.pump(cmd)
	assert.Equal(t, []string{"/child"}, dirs, "the focused pane's own folder is scanned, not the workspace root's")
	assert.Equal(t, "s2", childPage.git.Owner)
	assert.Zero(t, h.page.revealed, "the root pane's sidebar is untouched")

	_, cmd = h.m.Update(sidebar.GitRequestMsg{Owner: "s1", Action: sidebar.GitOpen, WorktreeID: "w1", Path: "Dockerfile.dev", Group: view.GroupUnstaged})
	h.pump(cmd)
	assert.Nil(t, childPage.overlay, "a late request from another pane is ignored")
	assert.Nil(t, h.page.overlay)

	_, cmd = h.m.Update(sidebar.GitRequestMsg{Owner: "s2", Action: sidebar.GitOpen, WorktreeID: "w1", Path: "Dockerfile.dev", Group: view.GroupUnstaged})
	h.pump(cmd)
	require.NotNil(t, childPage.overlay, "the inspector covers the focused pane")
	assert.Nil(t, h.page.overlay)

	h.comment("child review")
	h.key("R", "s")
	assert.Contains(t, ansi.Strip(childPage.View()), "Send review to current agent · ◆ child")
	h.key("enter")
	require.Len(t, f.calls, 1)
	assert.Equal(t, "s2", f.targets[0], "sent to the focused child session")
}

func TestReviewInterruptedScanNeverLeavesSidebarLoading(t *testing.T) {
	h, _ := newDraftHarnessWithoutOpen(t)
	other := &session.Session{ID: "s2", Title: "other", WorkingDir: "/other"}
	_, err := h.m.supervisor.AddSession(t.Context(), app.New(t.Context(), nil, other, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{})), other, "/other", nil)
	require.NoError(t, err)
	h.m.chatPages["s2"] = &reviewChatPage{overlayWidth: h.m.width}

	h.pump(h.m.syncGitReview())
	_, load := h.m.Update(sidebar.GitRequestMsg{Owner: "s1", Action: sidebar.GitLoad})
	assert.True(t, h.page.git.Loading, "the section shows a scan in flight")

	// Switch panes before the scan answers; the late answer is dropped.
	h.m.supervisor.SwitchTo("s2")
	h.pump(h.m.syncGitReview())
	assert.False(t, h.page.git.Loading, "an interrupted scan stops showing as loading")
	h.pump(load)
	assert.Nil(t, h.page.git.Snapshot, "the late result is not applied after the switch")

	h.m.supervisor.SwitchTo("s1")
	h.pump(h.m.syncGitReview())
	_, cmd := h.m.Update(sidebar.GitRequestMsg{Owner: "s1", Action: sidebar.GitRefresh})
	h.pump(cmd)
	require.NotNil(t, h.page.git.Snapshot, "refresh recovers")
	assert.False(t, h.page.git.Loading)
}

func TestGitReviewRefreshTouchesOnlyExpandedWorktreeAndReloadsOpenDocument(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	pageIface, ok := h.m.gitPage("s1")
	require.True(t, ok)
	page := pageIface.(*reviewChatPage)
	page.watch = sidebar.GitWatchMsg{Owner: "s1", Open: true, ExpandedWorktrees: []string{"w1"}}
	_, cmd := h.m.Update(sidebar.GitWatchMsg{Owner: "s1", Open: true, ExpandedWorktrees: []string{"w1"}})
	h.pump(cmd)
	assert.True(t, h.m.gitRefreshScheduled)
	refresh := h.m.handleGitAutoRefresh(gitAutoRefreshMsg{owner: "s1", token: h.m.gitRefreshToken})
	h.pump(refresh)
	assert.Equal(t, []string{"w1"}, h.provider.refreshed)
	assert.NotContains(t, h.provider.refreshed, "w2", "collapsed or unrelated worktrees remain idle")
	assert.Contains(t, h.provider.documents, gitreview.DocumentKey{Mode: "changes", WorktreeID: "w1", Path: "Dockerfile.dev", Group: gitreview.GroupUnstaged}, "the active diff reloads after its worktree refresh")
}

func TestGitReviewRefreshTimerStopsWhenGitTreeCollapses(t *testing.T) {
	h := newReviewHarness(t, 140)
	page := h.page
	page.watch = sidebar.GitWatchMsg{Owner: "s1", Open: true, ExpandedWorktrees: []string{"w1"}}
	snapshot := reviewSnapshot(gitreview.Snapshot{Generation: 1})
	h.m.gitReviews = map[string]*gitReviewWorkspace{"s1": {anchor: "s1", provider: h.provider, snapshot: &snapshot, open: true}}
	h.m.gitReviewOwner = "s1"
	h.m.gitRefreshTimer = func(_ time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
		return func() tea.Msg { return f(time.Time{}) }
	}
	cmd := h.m.handleGitWatch(page.GitWatchState())
	require.NotNil(t, cmd)
	token := h.m.gitRefreshToken
	h.pump(h.m.handleGitWatch(sidebar.GitWatchMsg{Owner: "s1", Open: false}))
	assert.False(t, h.m.gitRefreshScheduled)
	assert.Nil(t, h.m.handleGitAutoRefresh(gitAutoRefreshMsg{owner: "s1", token: token}), "a stale tick cannot refresh a collapsed section")
	assert.Empty(t, h.provider.refreshed)
}

func TestRefreshWorktreeKeepsUnrelatedDocumentRequestAndCoalescesSameWorktree(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	ws := h.m.activeReview()
	other := view.DocumentKey{Mode: view.ModeChanges, WorktreeID: "w2", Path: "b.go", Group: view.GroupStaged}
	model := ws.models[view.ModeChanges]
	model.OpenDocument(other)
	doc := h.m.submitGitReviewRequest(view.OpenDocumentRequestMsg{Owner: "s1", Key: other})
	require.NotNil(t, doc)
	request := h.m.gitReviewPending["doc"]
	first := h.m.submitGitReviewRequest(view.LoadWorktreeRequestMsg{Owner: "s1", WorktreeID: "w1", Refresh: true})
	require.NotNil(t, first)
	assert.Equal(t, request.token, h.m.gitReviewPending["doc"].token, "refresh in a different worktree does not cancel the open document")
	worktreeRequest := h.m.gitReviewPending["worktree:w1"]
	second := h.m.submitGitReviewRequest(view.LoadWorktreeRequestMsg{Owner: "s1", WorktreeID: "w1", Refresh: true})
	assert.Nil(t, second, "same-worktree refresh requests coalesce")
	assert.Equal(t, worktreeRequest.token, h.m.gitReviewPending["worktree:w1"].token)
}

func TestFailedAutomaticRefreshKeepsLastGoodStatusAndRetries(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	page := h.page
	page.watch = sidebar.GitWatchMsg{Owner: "s1", Open: true, ExpandedWorktrees: []string{"w1"}}
	h.provider.refreshErr = true
	first := h.m.handleGitAutoRefresh(gitAutoRefreshMsg{owner: "s1", token: h.m.gitRefreshToken})
	h.pump(first)
	wt, ok := findReviewWorktree(h.m.activeReview().snapshot, "w1")
	require.True(t, ok)
	assert.Equal(t, view.StateReady, wt.State, "a transient refresh error does not poison cached data")
	assert.Equal(t, 1, len(h.provider.refreshed))
	h.provider.refreshErr = false
	second := h.m.handleGitAutoRefresh(gitAutoRefreshMsg{owner: "s1", token: h.m.gitRefreshToken})
	h.pump(second)
	assert.Equal(t, 2, len(h.provider.refreshed), "the next timer retries the visible status")
}

func TestRefreshWorktreeDoesNotCancelOpenDiffInAnotherWorktree(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	model := h.m.activeReview().models[view.ModeChanges]
	other := view.DocumentKey{Mode: view.ModeChanges, WorktreeID: "w2", Path: "b.go", Group: view.GroupStaged}
	model.ReloadDocument(other)
	doc := h.m.submitGitReviewRequest(view.OpenDocumentRequestMsg{Owner: "s1", Key: other})
	require.NotNil(t, doc)
	request := h.m.gitReviewPending["doc"]
	refresh := h.m.submitGitReviewRequest(view.LoadWorktreeRequestMsg{Owner: "s1", WorktreeID: "w1", Refresh: true})
	require.NotNil(t, refresh)
	assert.Equal(t, request.token, h.m.gitReviewPending["doc"].token)
	assert.NotNil(t, h.m.gitReviewPending["doc"].cancel)
}

func TestAutomaticRefreshCoalescesAndRefreshErrorsRetryWithoutLosingReadyState(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	page := h.page
	page.watch = sidebar.GitWatchMsg{Owner: "s1", Open: true, ExpandedWorktrees: []string{"w1"}}
	_ = h.m.handleGitWatch(page.GitWatchState())
	first := h.m.handleGitAutoRefresh(gitAutoRefreshMsg{owner: "s1", token: h.m.gitRefreshToken})
	require.NotNil(t, first)
	h.provider.refreshErr = true
	h.pump(first)
	wt, ok := findReviewWorktree(h.m.activeReview().snapshot, "w1")
	require.True(t, ok)
	assert.Equal(t, view.StateReady, wt.State, "transient errors keep last good worktree state")
	assert.True(t, h.m.gitRefreshScheduled, "the still-open visible tree has a future retry scheduled")
	assert.Equal(t, 1, len(h.provider.refreshed), "transient failure was attempted once")
}

func TestStaleBackgroundGitWatchDoesNotCancelFocusedRefreshTimer(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	h.m.gitRefreshTimer = func(_ time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
		return func() tea.Msg { return f(time.Time{}) }
	}
	page, ok := h.m.gitPage("s1")
	require.True(t, ok)
	h.m.gitReviews = nil
	_, cmd := h.m.Update(messages.RevealGitChangesMsg{})
	h.pump(cmd)
	require.NotNil(t, h.m.gitReviews["s1"])
	watch := sidebar.GitWatchMsg{Owner: "s1", Open: true, ExpandedWorktrees: []string{"w1"}}
	timer := h.m.handleGitWatch(watch)
	require.NotNil(t, timer)
	token := h.m.gitRefreshToken
	stale := h.m.handleGitWatch(sidebar.GitWatchMsg{Owner: "background", Open: false})
	assert.Nil(t, stale)
	assert.True(t, h.m.gitRefreshScheduled)
	assert.Equal(t, token, h.m.gitRefreshToken)
	assert.NotNil(t, page)
}

func TestInspectorRefreshContinuesWithCollapsedSidebarAndOnlyItsWorktree(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	page, ok := h.m.gitPage("s1")
	require.True(t, ok)
	page.(*reviewChatPage).watch = sidebar.GitWatchMsg{Owner: "s1", Open: true, ExpandedWorktrees: []string{"w1", "w2"}}
	h.m.hideSidebar = true
	h.m.gitRefreshOwner = "s1"
	h.m.gitRefreshTimer = func(_ time.Duration, _ func(time.Time) tea.Msg) tea.Cmd { return func() tea.Msg { return nil } }
	ws := h.m.activeReview()
	require.NotNil(t, ws)
	require.True(t, ws.inspecting)
	require.NotNil(t, ws.snapshot)
	require.NotNil(t, ws.provider)
	h.m.gitRefreshScheduled, h.m.gitRefreshScope = false, ""
	timer := h.m.handleGitWatch(page.GitWatchState())
	require.NotNil(t, timer, "a visible inspector keeps its known target fresh without the sidebar")
	refresh := h.m.handleGitAutoRefresh(gitAutoRefreshMsg{owner: "s1", token: h.m.gitRefreshToken})
	h.pump(refresh)
	assert.Equal(t, []string{"w1"}, h.provider.refreshed)
	assert.NotContains(t, h.provider.refreshed, "w2", "hidden/collapsed unrelated checkouts remain idle")
}

func TestTargetedRefreshWaitsUntilExplicitSnapshotCompletes(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(false)
	page, ok := h.m.gitPage("s1")
	require.True(t, ok)
	page.(*reviewChatPage).watch = sidebar.GitWatchMsg{Owner: "s1", Open: true, ExpandedWorktrees: []string{"w1"}}
	h.m.gitReviewPending["snapshot"] = gitReviewRequest{token: 99, cancel: func() {}}
	assert.Nil(t, h.m.handleGitWatch(page.GitWatchState()), "automatic work does not race explicit discovery")
	assert.False(t, h.m.gitRefreshScheduled)
	assert.Empty(t, h.provider.refreshed)
}
