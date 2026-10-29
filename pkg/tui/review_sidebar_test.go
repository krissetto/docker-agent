package tui

import (
	"context"
	"errors"
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
	tuiinput "github.com/docker/docker-agent/pkg/tui/input"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
)

type failingGitMetadataProvider struct{ *fakeReviewProvider }

func (p failingGitMetadataProvider) LoadWorktree(ctx context.Context, id string) (gitreview.Worktree, error) {
	_, err := p.fakeReviewProvider.LoadWorktree(ctx, id)
	if err != nil {
		return gitreview.Worktree{}, err
	}
	return gitreview.Worktree{}, errors.New("metadata unavailable")
}

type unscannedSidebarReviewProvider struct{ *fakeReviewProvider }

func (p unscannedSidebarReviewProvider) Snapshot(ctx context.Context) (gitreview.Snapshot, error) {
	snapshot, err := p.fakeReviewProvider.Snapshot(ctx)
	if err != nil {
		return snapshot, err
	}
	for ri := range snapshot.Repositories {
		for wi := range snapshot.Repositories[ri].Worktrees {
			snapshot.Repositories[ri].Worktrees[wi].State = "unscanned"
			snapshot.Repositories[ri].Worktrees[wi].Files = nil
		}
	}
	return snapshot, nil
}

func (p unscannedSidebarReviewProvider) LoadWorktree(ctx context.Context, id string) (gitreview.Worktree, error) {
	_, err := p.fakeReviewProvider.LoadWorktree(ctx, id)
	if err != nil {
		return gitreview.Worktree{}, err
	}
	switch id {
	case "w1":
		return gitreview.Worktree{ID: id, Path: "/work", Branch: "main", Current: true, State: "changed", CountsState: gitreview.StateReady, Files: []gitreview.FileChange{
			{Path: "Dockerfile.dev", Group: gitreview.GroupUnstaged, Status: "M", Added: 1, Removed: 1},
			{Path: "a.go", Group: gitreview.GroupUnstaged, Status: "M", Added: 1},
		}}, nil
	case "w2":
		return gitreview.Worktree{ID: id, Path: "/work/wt", Branch: "feature", Linked: true, State: "changed", CountsState: gitreview.StateReady, Files: []gitreview.FileChange{{Path: "b.go", Group: gitreview.GroupStaged, Status: "A", Added: 2}}}, nil
	default:
		return gitreview.Worktree{ID: id, Path: "/work/det", State: "clean", CountsState: gitreview.StateReady}, nil
	}
}

func useUnscannedSidebarProvider(h *reviewHarness) {
	h.m.gitReviewFactory = func(*app.App, string, string) (gitreview.Provider, *view.Snapshot, error) {
		return unscannedSidebarReviewProvider{h.provider}, nil, nil
	}
}

type interruptedSidebarReviewProvider struct {
	*fakeReviewProvider
	lateResult bool
}

func (p *interruptedSidebarReviewProvider) LoadWorktree(ctx context.Context, id string) (gitreview.Worktree, error) {
	if p.lateResult && id == "w1" {
		return gitreview.Worktree{ID: id, Path: "/late", Branch: "late", State: "changed", CountsState: gitreview.StateReady}, nil
	}
	return p.fakeReviewProvider.LoadWorktree(ctx, id)
}

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

func TestCollapsedGitSidebarPrewarmsBoundedMetadataWithoutHydratingDocuments(t *testing.T) {
	h := newReviewHarness(t, 140)
	useUnscannedSidebarProvider(h)
	h.pump(h.m.syncGitReview())
	require.True(t, h.page.git.Available)
	require.Nil(t, h.page.overlay, "metadata prewarm does not open an inspector")
	require.NotNil(t, h.page.git.Snapshot)
	assert.Equal(t, 1, h.provider.snapshots, "collapsed sidebar performs discovery once")
	assert.Equal(t, []string{"w1", "w2", "w3"}, h.provider.worktrees, "all discovered statuses are loaded")
	assert.Empty(t, h.provider.documents, "prewarm never asks for patches")
	assert.Empty(t, h.provider.dirs, "prewarm never asks for file history/listings")
	require.NotNil(t, h.m.activeReview(), "review data is live for sidebar without a visible review surface")
	assert.False(t, h.m.activeReview().inspecting)
	assert.Nil(t, h.page.overlay, "the transcript is not covered")
}

func TestCollapsedGitSidebarDefersPrewarmUntilVisibleAndNoCompetingSurface(t *testing.T) {
	t.Run("hidden sidebar", func(t *testing.T) {
		h := newReviewHarness(t, 140)
		useUnscannedSidebarProvider(h)
		h.m.hideSidebar = true
		h.pump(h.m.syncGitReview())
		assert.Zero(t, h.provider.snapshots)
		h.m.hideSidebar = false
		h.pump(h.m.syncGitReview())
		assert.Equal(t, 1, h.provider.snapshots)
		assert.Len(t, h.provider.worktrees, 3)
	})
	t.Run("files surface", func(t *testing.T) {
		h := newReviewHarness(t, 140)
		useUnscannedSidebarProvider(h)
		h.open(true)
		require.Equal(t, view.ModeFiles, h.m.activeReview().mode)
		assert.Empty(t, h.provider.worktrees, "visible Files review is not displaced by sidebar prewarm")
		_, cmd := h.m.Update(view.CloseRequestMsg{Owner: "s1"})
		h.pump(cmd)
		h.pump(h.m.syncGitReview())
		assert.Zero(t, h.provider.snapshots, "the Files intent keeps the sidebar from scanning Git")
		h.pump(browseRequest(h, "s1", sidebar.GitShowChanges))
		assert.Equal(t, 1, h.provider.snapshots, "selecting Changes starts the deferred scan")
		assert.NotEmpty(t, h.provider.worktrees)
	})
	t.Run("inspector", func(t *testing.T) {
		h := newReviewHarness(t, 140)
		useUnscannedSidebarProvider(h)
		_, cmd := h.m.Update(messages.RevealGitChangesMsg{})
		h.pump(cmd)
		loadedBeforeInspector := len(h.provider.worktrees)
		h.openDockerfile()
		require.True(t, h.m.activeReview().inspecting)
		assert.Len(t, h.provider.worktrees, loadedBeforeInspector, "opening the inspector does not scan additional worktrees")
		_, closeCmd := h.m.Update(view.CloseRequestMsg{Owner: "s1"})
		h.pump(closeCmd)
		h.pump(h.m.syncGitReview())
		assert.NotEmpty(t, h.provider.worktrees)
	})
}

func countPrefix(values map[string]gitReviewRequest, prefix string) int {
	n := 0
	for key := range values {
		if strings.HasPrefix(key, prefix) {
			n++
		}
	}
	return n
}

func batchCmd(cmds ...tea.Cmd) tea.Cmd {
	return func() tea.Msg { return tea.BatchMsg(cmds) }
}

func TestCollapsedGitSidebarUnavailableProviderDoesNotRetryUntilRequested(t *testing.T) {
	h := newReviewHarness(t, 140)
	factoryCalls := 0
	h.m.gitReviewFactory = func(*app.App, string, string) (gitreview.Provider, *view.Snapshot, error) {
		factoryCalls++
		return nil, nil, errors.New("provider unavailable")
	}
	h.pump(h.m.syncGitReview())
	require.Equal(t, 1, factoryCalls)
	for range 5 {
		h.pump(h.m.syncGitReview())
	}
	assert.Equal(t, 1, factoryCalls, "a completed unavailable result is cached rather than auto-retried every update")
	_, cmd := h.m.Update(sidebar.GitRequestMsg{Owner: "s1", Action: sidebar.GitLoad})
	h.pump(cmd)
	assert.Equal(t, 2, factoryCalls, "an explicit Git request may retry provider creation")
}

func TestCollapsedGitSidebarMetadataPrewarmBatchesAllWorktrees(t *testing.T) {
	for _, total := range []int{20, 40} {
		t.Run(fmt.Sprintf("%d worktrees", total), func(t *testing.T) {
			h := newReviewHarness(t, 140)
			owner := h.m.paneFocus()
			route, _ := h.m.supervisor.RouteGeneration(owner)
			ws := newGitReviewWorkspace(owner)
			ws.open = true
			ws.mode = view.ModeChanges
			ws.provider = h.provider
			ws.sessionID, ws.workingDir, ws.route = h.sess.ID, h.sess.WorkingDir, route
			ws.snapshot = &view.Snapshot{Generation: 1, Repositories: []view.Repository{{ID: "r", Worktrees: make([]view.Worktree, total)}}}
			ids := make([]string, total)
			for i := range ws.snapshot.Repositories[0].Worktrees {
				id := fmt.Sprintf("w%d", i)
				ids[i] = id
				ws.snapshot.Repositories[0].Worktrees[i] = view.Worktree{ID: id, State: view.StateUnscanned}
			}
			h.m.gitReviews = map[string]*gitReviewWorkspace{owner: ws}
			h.m.gitReviewOwner = owner
			h.m.gitReviewGeneration++
			h.m.gitSidebarMarked = map[string]bool{owner: true}
			cmd := h.m.prewarmGitSidebarMetadata(ws)
			require.NotNil(t, cmd)
			initial := cmd().(tea.BatchMsg)
			require.Equal(t, maxSidebarMetadataConcurrent, countPrefix(h.m.gitReviewPending, "worktree:"), "the first batch fills, but never exceeds, the concurrency budget")
			assert.Nil(t, h.m.prewarmGitSidebarMetadata(ws), "a repeated sync does not duplicate or exceed the batch")
			h.pump(batchCmd(initial...))
			require.Len(t, h.provider.worktrees, total, "completion refills slots until all discovered worktrees are handled")
			assert.ElementsMatch(t, ids, h.provider.worktrees)
			assert.Empty(t, h.provider.documents)
			assert.Empty(t, h.provider.dirs)
			assert.True(t, ws.prewarm.complete)
		})
	}
}

func TestCollapsedGitSidebarPrewarmStopsWhenHiddenAndResumes(t *testing.T) {
	h := newReviewHarness(t, 140)
	owner := h.m.paneFocus()
	route, _ := h.m.supervisor.RouteGeneration(owner)
	ws := newGitReviewWorkspace(owner)
	ws.open, ws.mode, ws.provider = true, view.ModeChanges, h.provider
	ws.sessionID, ws.workingDir, ws.route = h.sess.ID, h.sess.WorkingDir, route
	ws.snapshot = &view.Snapshot{Generation: 1, Repositories: []view.Repository{{ID: "r", Worktrees: make([]view.Worktree, 20)}}}
	for i := range ws.snapshot.Repositories[0].Worktrees {
		ws.snapshot.Repositories[0].Worktrees[i] = view.Worktree{ID: fmt.Sprintf("w%d", i), State: view.StateUnscanned}
	}
	h.m.gitReviews = map[string]*gitReviewWorkspace{owner: ws}
	h.m.gitReviewOwner = owner
	h.m.gitReviewGeneration++
	h.m.gitSidebarMarked = map[string]bool{owner: true}
	cmd := h.m.prewarmGitSidebarMetadata(ws)
	initial := cmd().(tea.BatchMsg)
	require.Equal(t, maxSidebarMetadataConcurrent, countPrefix(h.m.gitReviewPending, "worktree:"))
	h.m.hideSidebar = true
	h.pump(batchCmd(initial...))
	require.Len(t, h.provider.worktrees, maxSidebarMetadataConcurrent, "hidden sidebar stops scheduling remaining work")
	h.m.hideSidebar = false
	h.pump(h.m.prewarmGitSidebarMetadata(ws))
	require.Len(t, h.provider.worktrees, 20)
	assert.True(t, ws.prewarm.complete)
}

func TestCollapsedGitSidebarPrewarmStopsSchedulingAfterPaneSwitch(t *testing.T) {
	h := newReviewHarness(t, 140)
	owner := h.m.paneFocus()
	route, _ := h.m.supervisor.RouteGeneration(owner)
	ws := newGitReviewWorkspace(owner)
	ws.open, ws.mode, ws.provider = true, view.ModeChanges, h.provider
	ws.sessionID, ws.workingDir, ws.route = h.sess.ID, h.sess.WorkingDir, route
	ws.snapshot = &view.Snapshot{Generation: 1, Repositories: []view.Repository{{ID: "r", Worktrees: make([]view.Worktree, 20)}}}
	for i := range ws.snapshot.Repositories[0].Worktrees {
		ws.snapshot.Repositories[0].Worktrees[i] = view.Worktree{ID: fmt.Sprintf("w%d", i), State: view.StateUnscanned}
	}
	h.m.gitReviews = map[string]*gitReviewWorkspace{owner: ws}
	h.m.gitReviewOwner = owner
	h.m.gitReviewGeneration++
	h.m.gitSidebarMarked = map[string]bool{owner: true}
	cmd := h.m.prewarmGitSidebarMetadata(ws)
	require.NotNil(t, cmd)
	assert.Equal(t, maxSidebarMetadataConcurrent, countPrefix(h.m.gitReviewPending, "worktree:"))
	child := &session.Session{ID: "s2", Title: "child", WorkingDir: "/child"}
	_, err := h.m.supervisor.AddSession(t.Context(), app.New(t.Context(), nil, child, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{})), child, "/child", nil)
	require.NoError(t, err)
	h.m.supervisor.SwitchTo("s2")
	assert.Nil(t, h.m.prewarmGitSidebarMetadata(ws), "a background pane never queues more metadata for the old owner")
	assert.Equal(t, maxSidebarMetadataConcurrent, countPrefix(h.m.gitReviewPending, "worktree:"))
}

func TestCollapsedGitSidebarPrewarmErrorDoesNotTightLoop(t *testing.T) {
	h := newReviewHarness(t, 140)
	owner := h.m.paneFocus()
	route, _ := h.m.supervisor.RouteGeneration(owner)
	ws := newGitReviewWorkspace(owner)
	ws.open, ws.mode, ws.provider = true, view.ModeChanges, failingGitMetadataProvider{h.provider}
	ws.sessionID, ws.workingDir, ws.route = h.sess.ID, h.sess.WorkingDir, route
	ws.snapshot = &view.Snapshot{Generation: 1, Repositories: []view.Repository{{ID: "r", Worktrees: make([]view.Worktree, 8)}}}
	for i := range ws.snapshot.Repositories[0].Worktrees {
		ws.snapshot.Repositories[0].Worktrees[i] = view.Worktree{ID: fmt.Sprintf("w%d", i), State: view.StateUnscanned}
	}
	h.m.gitReviews = map[string]*gitReviewWorkspace{owner: ws}
	h.m.gitReviewOwner = owner
	h.m.gitReviewGeneration++
	h.m.gitSidebarMarked = map[string]bool{owner: true}
	h.pump(h.m.prewarmGitSidebarMetadata(ws))
	require.Eventually(t, func() bool { return ws.prewarm.complete }, time.Second, time.Millisecond, "failed worktrees are marked unavailable and batch proceeds")
	require.Len(t, h.provider.worktrees, 8)
	for range 5 {
		h.pump(h.m.syncGitReview())
	}
	assert.Len(t, h.provider.worktrees, 8, "failed worktrees become error and are not retried on every sync")
	assert.True(t, ws.prewarm.complete)
}

func TestReviewSidebarRemoteIncompleteDiscoveryStaysCompact(t *testing.T) {
	h := newReviewHarness(t, 220)
	useUnscannedSidebarProvider(h)
	withRealPage(t, h)
	h.pump(h.m.syncGitReview())
	ws := h.m.activeReview()
	require.NotNil(t, ws)
	require.NotNil(t, ws.snapshot)
	ws.snapshot.Scope.Remote = true
	ws.snapshot.Scope.LimitReason = "entries"
	ws.snapshot.Scope.LimitValue = 4096
	ws.snapshot.Scope.Note = "Partial scan: discovery entry limit reached (4096 entries). This remote view cannot continue discovery; some repositories may be missing."
	h.pump(h.m.pushGitSidebar(ws))
	h.clickText("Git")
	h.settle()
	h.m.viewCacheValid = false
	frame := ansi.Strip(h.m.composeView().Content)
	assert.Contains(t, frame, "Files - Git")
	assert.Contains(t, frame, "work")
	assert.NotContains(t, frame, "Partial scan:")
	assert.NotContains(t, frame, "discovery entry limit")
	assert.NotContains(t, frame, "cannot continue discovery")
	assert.NotContains(t, frame, "repositories may be missing")
	assert.NotContains(t, frame, "Changes(")
	assert.NotContains(t, frame, "Browse")
	assert.Regexp(t, `(?i)(incomplete|partial|limited)`, frame, "incomplete results remain visible as a concise status")
	assert.Equal(t, "entries", ws.snapshot.Scope.LimitReason, "presentation does not erase discovery state")
}

func TestReviewSidebarSingleHeaderSwitchesExpandedModes(t *testing.T) {
	h := newReviewHarness(t, 220)
	useUnscannedSidebarProvider(h)
	withRealPage(t, h)
	h.pump(h.m.syncGitReview())
	page, ok := h.m.gitPage("s1")
	require.True(t, ok)

	for _, label := range []string{"Git", "Files", "Git"} {
		h.clickText(label)
		h.settle()
		mode := view.ModeChanges
		if label == "Files" {
			mode = view.ModeFiles
		}
		assert.Equal(t, mode, page.GitWatchState().Mode)
		assert.True(t, page.GitWatchState().Open, "mode choices keep the tree open")
		h.m.viewCacheValid = false
		frame := ansi.Strip(h.m.composeView().Content)
		assert.Equal(t, 1, strings.Count(frame, "Files - Git"))
		assert.NotContains(t, frame, "Changes(")
		assert.NotContains(t, frame, "Browse")
		lines := strings.Split(frame, "\n")
		for y, line := range lines {
			if strings.Contains(line, "Files - Git") {
				require.Less(t, y+1, len(lines))
				firstEntry := "work"
				if mode == view.ModeFiles {
					firstEntry = "README.md"
				}
				assert.Contains(t, lines[y+1], firstEntry, "the tree starts immediately below the header")
				break
			}
		}
	}
}

func TestReviewRootClickSidebarFileShowsDiffBesideNormalSidebar(t *testing.T) {
	h, f := newDraftHarnessWithoutOpen(t)
	useUnscannedSidebarProvider(h)
	withRealPage(t, h)
	h.pump(h.m.syncGitReview())
	h.settle()
	assert.Equal(t, 1, h.provider.snapshots, "visible collapsed sidebar starts bounded discovery")
	assert.NotEmpty(t, h.provider.worktrees, "status metadata is prewarmed")
	assert.Empty(t, h.provider.documents, "metadata prewarm does not hydrate diffs")

	h.clickText("Git")
	h.settle()
	h.m.viewCacheValid = false
	frame := ansi.Strip(h.m.composeView().Content)
	assert.Contains(t, frame, "work")
	assert.Nil(t, h.page.overlay, "opening the sidebar tree does not open the inspector")
	h.clickText("work")
	h.clickText("● /work")
	h.clickText("Unstaged")
	h.clickText("Dockerfile.dev")
	h.settle()
	h.m.viewCacheValid = false

	frame = ansi.Strip(h.m.composeView().Content)
	t.Log("\n" + frame)
	assert.Contains(t, frame, "- FROM golang:1.22", "the inspector shows the diff")
	assert.Contains(t, frame, "+ FROM golang:1.27")
	assert.Contains(t, frame, "Dockerfile.dev")
	assert.Contains(t, frame, "● /work  ⎇ main", "checkout path is primary and branch is secondary")
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
	topBeforeWheel := model.Save().Top
	var wheelCmd tea.Cmd
	_, wheelCmd = h.m.Update(messages.WheelCoalescedMsg{X: ox + 10, Y: oy + 4, Delta: 3})
	h.pump(wheelCmd)
	afterWheel := ansi.Strip(model.View())
	assert.Equal(t, topBeforeWheel+6, model.Save().Top, "the root preserves all three coalesced wheel ticks at two visual rows each")
	assert.NotEqual(t, beforeWheel, afterWheel, "the root coalesced wheel scrolls the sidebar-opened diff")
	beforeRaw := ansi.Strip(model.View())
	_, rawCmd := h.m.Update(tea.MouseWheelMsg{X: ox + 10, Y: oy + 4, Button: tea.MouseWheelUp})
	h.pump(rawCmd)
	assert.NotEqual(t, beforeRaw, ansi.Strip(model.View()), "a direct terminal wheel event also scrolls the diff")
	afterWheel = ansi.Strip(model.View())
	frame = ansi.Strip(h.m.composeView().Content)
	var sidebarY, sidebarX = -1, -1
	for y, line := range strings.Split(frame, "\n") {
		if index := strings.Index(line, "Files - Git"); index >= 0 {
			sidebarY = y
			sidebarX = ansi.StringWidth(line[:index]) + 1
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

func TestReviewInspectorWheelUsesProductionCoalescer(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(map[bool]string{false: "unified", true: "split"}[split], func(t *testing.T) {
			h := newReviewHarness(t, 220)
			h.m.gitDiffSplit = split
			useUnscannedSidebarProvider(h)
			withRealPage(t, h)
			h.pump(h.m.syncGitReview())
			h.settle()
			h.clickText("Git")
			h.clickText("work")
			h.clickText("● /work")
			h.clickText("Unstaged")
			h.clickText("Dockerfile.dev")
			h.settle()
			model := h.m.reviewModel()
			require.NotNil(t, model)
			before := ansi.Strip(model.View())
			frameBefore := ansi.Strip(h.m.composeView().Content)
			page := h.m.chatPage.(interface{ ContentOverlayBounds() (int, int, int, int) })
			ox, oy, _, _ := page.ContentOverlayBounds()

			coalescer := tuiinput.NewMouseCoalescer()
			t.Cleanup(coalescer.Stop)
			assert.Nil(t, coalescer.Filter(tea.MouseWheelMsg{X: ox + 8, Y: oy + 4, Button: tea.MouseWheelDown}))
			pointer, ok := coalescer.Filter(tea.MouseWheelMsg{X: ox + 9, Y: oy + 4, Button: tea.MouseWheelDown}).(messages.PointerUpdateMsg)
			require.True(t, ok, "the production coalescer flushes PointerUpdateMsg when wheel position changes")
			_, cmd := h.m.Update(pointer)
			h.pump(cmd)
			afterFrame := ansi.Strip(h.m.composeView().Content)
			assert.NotEqual(t, before, ansi.Strip(model.View()), "PointerUpdateMsg reaches the chat content overlay and scrolls the diff model")
			assert.NotEqual(t, frameBefore, afterFrame, "the root rendered frame reflects the wheel scroll")
			assert.True(t, model.Focused(), "wheel scrolling preserves review keyboard focus")
			assert.Contains(t, afterFrame, "● /work", "the adjacent Git sidebar remains visible")
		})
	}
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

	provider := &interruptedSidebarReviewProvider{fakeReviewProvider: h.provider}
	h.m.gitReviewFactory = func(*app.App, string, string) (gitreview.Provider, *view.Snapshot, error) {
		return provider, nil, nil
	}
	h.pump(h.m.syncGitReview()) // collapsed-sidebar discovery and bounded prewarm finish
	require.NotNil(t, h.page.git.Snapshot, "prewarm has already discovered the sidebar snapshot")
	ws := h.m.activeReview()
	require.NotNil(t, ws)
	provider.lateResult = true
	load := h.m.submitGitReviewRequest(view.LoadWorktreeRequestMsg{Owner: "s1", WorktreeID: "w1"})
	require.NotNil(t, load, "an explicit worktree scan starts")
	h.pump(h.m.pushGitSidebar(ws))
	assert.True(t, h.page.git.Scanning["w1"], "the section shows a worktree scan in flight")

	// Switch panes before the scan answers; the late answer is dropped.
	h.m.supervisor.SwitchTo("s2")
	h.pump(h.m.syncGitReview())
	assert.False(t, h.page.git.Scanning["w1"], "an interrupted scan stops showing as loading")
	assert.NotContains(t, h.m.gitReviewPending, "worktree:w1", "switching panes cancels the outstanding request")
	beforeLateResult := h.page.git.Snapshot
	require.NotNil(t, beforeLateResult, "switching panes preserves the prewarmed sidebar snapshot")
	h.pump(load)
	assert.Equal(t, beforeLateResult, h.page.git.Snapshot, "the late result is rejected without mutating the sidebar snapshot")
	assert.Equal(t, "main", h.page.git.Snapshot.Repositories[0].Worktrees[0].Branch)

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

type toggledDirectoryReviewProvider struct {
	*fakeReviewProvider
	fail bool
}

func (p *toggledDirectoryReviewProvider) Directory(ctx context.Context, worktreeID, path string) (gitreview.Directory, error) {
	if p.fail {
		return gitreview.Directory{}, errors.New("directory unavailable")
	}
	return p.fakeReviewProvider.Directory(ctx, worktreeID, path)
}

func TestGitReviewDirectoryErrorUpdatesSidebarCacheAndCanRetry(t *testing.T) {
	h := newReviewHarness(t, 140)
	provider := &toggledDirectoryReviewProvider{fakeReviewProvider: h.provider, fail: true}
	h.m.gitReviewFactory = func(*app.App, string, string) (gitreview.Provider, *view.Snapshot, error) { return provider, nil, nil }
	h.open(true)
	key := view.LoadDirectoryRequestMsg{Owner: "s1", WorktreeID: "w1", Path: "src"}
	h.pump(h.m.submitGitReviewRequest(key))
	ws := h.m.gitReviews["s1"]
	require.Equal(t, view.StateError, ws.directories["w1\x00src"].State)
	var sidebarDirectory *view.Directory
	for i := range h.page.git.Directories {
		if h.page.git.Directories[i].Path == "src" {
			sidebarDirectory = &h.page.git.Directories[i]
			break
		}
	}
	require.NotNil(t, sidebarDirectory, "sidebar receives the terminal directory state and clears pending")
	require.Equal(t, view.StateError, sidebarDirectory.State)
	provider.fail = false
	h.pump(h.m.submitGitReviewRequest(key))
	require.Equal(t, view.StateReady, ws.directories["w1\x00src"].State)
}

func TestCancelledDirectoryRequestGenerationAllowsSidebarRetry(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.open(true)
	ws := h.m.activeReview()
	require.NotNil(t, ws)
	key := view.LoadDirectoryRequestMsg{Owner: "s1", WorktreeID: "w1", Path: "src"}
	load := h.m.submitGitReviewRequest(key)
	require.NotNil(t, load)
	initialGeneration := ws.directoryGeneration
	_, pending := h.m.gitReviewPending["dir:w1\x00src"]
	require.True(t, pending)

	h.pump(h.m.pushGitSidebar(ws))
	h.m.invalidateGitDirectories(ws)
	h.pump(h.m.pushGitSidebar(ws))
	assert.Greater(t, ws.directoryGeneration, initialGeneration)
	assert.Equal(t, ws.directoryGeneration, h.page.git.DirectoryGeneration)
	assert.Empty(t, ws.directories)
	_, pending = h.m.gitReviewPending["dir:w1\x00src"]
	assert.False(t, pending, "cancelled request is removed so the next explicit browse can retry")

	// The root accepts a new explicit request after cancellation; no stale
	// canceled result is needed to release its request guard.
	retry := h.m.submitGitReviewRequest(key)
	require.NotNil(t, retry)
	_, pending = h.m.gitReviewPending["dir:w1\x00src"]
	assert.True(t, pending)
}
