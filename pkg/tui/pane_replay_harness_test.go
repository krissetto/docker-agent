package tui

import (
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

// This file is injected byte-for-byte into baseline and candidate sources.
// Histories and replay length are bounded, clocks deterministic, and commands are not
// executed: the measured work is the production Update/View presentation path.
type paneReplayPage struct {
	*splitRecordingPage

	sizes, presentations, transcriptViews int
}

func (p *paneReplayPage) SetSize(w, h int) tea.Cmd {
	p.sizes++
	return p.Page.SetSize(w, h)
}

func (p *paneReplayPage) SetSplitPresentation(g *chat.SplitPresentationGeometry) tea.Cmd {
	p.presentations++
	return p.splitRecordingPage.SetSplitPresentation(g)
}

func (p *paneReplayPage) TranscriptView() string {
	p.transcriptViews++
	return p.splitRecordingPage.TranscriptView()
}

// Structural forwarding preserves optional candidate presentation seams while
// compiling against the pristine baseline, which has no sidebar override API.
func (p *paneReplayPage) SetSplitSidebarSettings(settings *chat.SidebarSettings) tea.Cmd {
	if owner, ok := p.Page.(interface {
		SetSplitSidebarSettings(settings *chat.SidebarSettings) tea.Cmd
	}); ok {
		return owner.SetSplitSidebarSettings(settings)
	}
	return nil
}

func (p *paneReplayPage) SplitSidebarSettings() (chat.SidebarSettings, bool) {
	if owner, ok := p.Page.(interface {
		SplitSidebarSettings() (chat.SidebarSettings, bool)
	}); ok {
		return owner.SplitSidebarSettings()
	}
	return chat.SidebarSettings{}, false
}

func (p *paneReplayPage) SidebarCacheStats() (uint64, uint64) {
	if owner, ok := p.Page.(interface{ SidebarCacheStats() (uint64, uint64) }); ok {
		return owner.SidebarCacheStats()
	}
	return 0, 0
}

func paneReplayRoot(tb testing.TB) (*appModel, []*paneReplayPage, *paneReplayScheduler) {
	tb.Helper()
	scheduler := &paneReplayScheduler{now: time.Unix(1, 0)}
	root, _, _ := harnessRoot(tb, 156, 48, animation.NewRuntimeWithScheduler(scheduler))
	root.hideSidebar = false
	root.chatPage = chat.New(root.ar, tb.Context(), root.application, root.sessionState)
	root.chatPage.SetRoutingID("profile")
	root.chatPages["profile"] = root.chatPage
	root.editors["profile"] = root.editor
	for _, id := range []string{"second", "third"} {
		sess, _, _ := mixedHistorySession(24)
		sess.ID, sess.Title = id, "Bounded replay "+id
		a := app.New(tb.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
		if _, err := root.supervisor.AddSession(tb.Context(), a, sess, "", nil); err != nil {
			tb.Fatal(err)
		}
		page, state, ed, application := root.chatPage, root.sessionState, root.editor, root.application
		root.initSessionComponents(id, a, sess)
		root.chatPage.Init()
		root.chatPage, root.sessionState, root.editor, root.application = page, state, ed, application
	}
	sess, _, _ := mixedHistorySession(24)
	root.application.Session().Messages = sess.Messages
	root.chatPage.Init()
	root.splitPane("second", "profile", splitRight)
	root.splitPane("third", "second", splitBottom)
	root.handleSwitchTab("profile")
	// The fixture has no running supervisor watcher command; explicitly deliver
	// the real canonical tab snapshot that the normal program receives.
	tabs, active := root.supervisor.GetTabs()
	root.Update(messages.TabsUpdatedMsg{Tabs: tabs, ActiveIdx: active})
	root.Update(tea.WindowSizeMsg{Width: 156, Height: 48})
	var pages []*paneReplayPage
	for _, id := range []string{"profile", "second", "third"} {
		p := &paneReplayPage{splitRecordingPage: &splitRecordingPage{Page: root.chatPages[id]}}
		root.chatPages[id] = p
		pages = append(pages, p)
	}
	root.chatPage = root.chatPages["profile"]
	root.View()
	settlePaneReplay(root, scheduler)
	tb.Cleanup(func() {
		for _, page := range root.chatPages {
			chat.Cleanup(page)
		}
		root.ar.Stop()
	})
	return root, pages, scheduler
}

// The scheduler captures only real Tick requests made by production owners.
// It never replaces a pending lease or synthesizes registrations to rescue a chain.
type paneReplayScheduler struct {
	now     time.Time
	pending []tea.Cmd
	step    time.Duration // optional deterministic benchmark cadence
}

func (s *paneReplayScheduler) Now() time.Time { return s.now }
func (s *paneReplayScheduler) Tick(delay time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	cmd := func() tea.Msg {
		elapsed := delay
		if s.step > 0 {
			elapsed = s.step
		}
		s.now = s.now.Add(elapsed)
		return create(s.now)
	}
	s.pending = append(s.pending, cmd)
	return cmd
}

func settlePaneReplay(root *appModel, scheduler *paneReplayScheduler) {
	for step := 0; step < 240 && len(scheduler.pending) > 0; step++ {
		cmd := scheduler.pending[0]
		scheduler.pending = scheduler.pending[1:]
		root.Update(cmd())
	}
}

func replayPaneFrame(root *appModel, step int) {
	ids := []string{"profile", "second", "third"}
	id := ids[step%len(ids)]
	generation, _ := root.supervisor.RouteGeneration(id)
	// Canonical tab activity drives headers without starting fake page leases.
	// The same DTO/animation workload exists on the pristine baseline tabbar.
	tabs, active := root.supervisor.GetTabs()
	for i := range tabs {
		if tabs[i].SessionID == id {
			tabs[i].Activity = messages.TabActivityRunning
		} else {
			tabs[i].Activity = messages.TabActivityNone
		}
	}
	root.Update(messages.TabsUpdatedMsg{Tabs: tabs, ActiveIdx: active})
	root.Update(messages.RoutedMsg{SessionID: id, RouteGeneration: generation, Inner: runtime.AgentChoice("root", id, " bounded stream λ界 `code` ")})
	// Include real composer input, scrolling, focus and height-only geometry.
	if step%8 == 0 {
		root.updateEditorCmd(tea.KeyPressMsg{Code: 'x', Text: "x"})
	}
	if step%12 == 0 {
		root.handleSwitchTab(ids[(step/12)%3])
	}
	if step%16 == 0 {
		root.handleWindowResize(156, 48+(step/16)%2)
	}
	if step%6 == 0 {
		r := root.paneGeometry.Panes[id]
		root.Update(messages.WheelCoalescedMsg{X: r.X + 2, Y: r.Y + 2, Delta: -1})
	}
	root.View()
}

func TestPaneBoundedReplayCounters(t *testing.T) {
	root, pages, scheduler := paneReplayRoot(t)
	if leases := root.ar.ActiveCount(); leases != 0 {
		t.Fatalf("settled initial idle leases=%d", leases)
	}
	for step := range 96 {
		replayPaneFrame(root, step)
		settlePaneReplay(root, scheduler)
	}
	t.Logf("workload end leases=%d (before settling)", root.ar.ActiveCount())
	settlePaneReplay(root, scheduler)
	if leases := root.ar.ActiveCount(); leases != 0 {
		t.Fatalf("settled final idle leases=%d", leases)
	}
	for i, p := range pages {
		if stats, ok := any(p).(interface{ SidebarCacheStats() (uint64, uint64) }); ok {
			invalidations, renders := stats.SidebarCacheStats()
			t.Logf("pane=%d sidebar invalidations=%d renders=%d", i, invalidations, renders)
		}
		rebuilds, misses, renders := p.Page.(resizeCacheReporter).ResizeCacheStats()
		t.Logf("pane=%d history=24 frames=96 setsize=%d presentation=%d rebuilds=%d misses=%d renders=%d", i, p.sizes, p.presentations, rebuilds, misses, renders)
	}
	if len(root.panes.Sessions()) != 3 {
		t.Fatal("replay lost a canonical pane")
	}
}

func BenchmarkPaneBoundedReplay(b *testing.B) {
	// Exactly one bounded replay per invocation; validator repeats processes.
	if b.N != 1 {
		b.Fatal("bounded replay requires -benchtime=1x")
	}
	for range b.N {
		b.StopTimer()
		root, pages, scheduler := paneReplayRoot(b)
		b.StartTimer()
		for step := range 96 {
			replayPaneFrame(root, step)
			settlePaneReplay(root, scheduler)
		}
		b.StopTimer()
		for i, p := range pages {
			b.ReportMetric(float64(p.sizes), fmt.Sprintf("pane%d-resizes/replay", i))
		}
		// Cleanup each bounded batch rather than retain histories across b.N.
		root.cleanupManagedResources()
		b.StartTimer()
	}
}

// The baseline and candidate share this pointer workload byte-for-byte. Locate
// tab rows by the production region mapper, not a separator-era fixed offset.
func replayPaneDrag(tb testing.TB, root *appModel) {
	tb.Helper()
	root.View()
	x, y := -1, -1
	for row := range root.height {
		if root.hitTestRegion(row) != regionTabBar {
			continue
		}
		for column := tabFrameOrigin(); column < root.width; column++ {
			if id, hit := root.tabBar.TabBodyAt(column-tabFrameOrigin(), 0); hit && id == "second" {
				x, y = column, row
				break
			}
		}
		if x >= 0 {
			break
		}
	}
	if x < 0 {
		tb.Fatal("drag replay cannot locate source tab")
	}
	root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	_, bounds, _ := root.measurePanes()
	for step := range 96 {
		motion := tea.MouseMotionMsg{X: bounds.X + bounds.W/2 + (step % 7), Y: bounds.Y + bounds.H/2 + (step % 3), Button: tea.MouseLeft}
		root.Update(messages.PointerUpdateMsg{X: motion.X, Y: motion.Y, Motion: &motion})
		root.View()
		root.Update(messages.PointerUpdateMsg{X: motion.X, Y: motion.Y, Motion: &motion})
		root.View()
	}
	root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	root.View()
}

func TestPaneDragBoundedReplayCounters(t *testing.T) {
	root, pages, scheduler := paneReplayRoot(t)
	before := make([][4]uint64, len(pages))
	for i, page := range pages {
		a, b, c := page.Page.(resizeCacheReporter).ResizeCacheStats()
		before[i] = [4]uint64{a, b, c, uint64(page.transcriptViews)}
	}
	replayPaneDrag(t, root)
	settlePaneReplay(root, scheduler)
	for i, page := range pages {
		a, b, c := page.Page.(resizeCacheReporter).ResizeCacheStats()
		if [3]uint64{a, b, c} != [3]uint64{before[i][0], before[i][1], before[i][2]} {
			t.Fatalf("pane%d drag reflowed transcript", i)
		}
		t.Logf("pane=%d drag96 repeated96 transcriptViews=%d setsize=%d presentation=%d", i, uint64(page.transcriptViews)-before[i][3], page.sizes, page.presentations)
	}
	if root.ar.ActiveCount() != 0 {
		t.Fatal("stationary/canceled drag retained animation leases")
	}
}

func BenchmarkPaneDragBoundedReplay(b *testing.B) {
	if b.N != 1 {
		b.Fatal("bounded drag replay requires -benchtime=1x")
	}
	b.StopTimer()
	root, pages, _ := paneReplayRoot(b)
	before := make([]int, len(pages))
	for i, page := range pages {
		before[i] = page.transcriptViews
	}
	b.StartTimer()
	replayPaneDrag(b, root)
	b.StopTimer()
	for i, page := range pages {
		b.ReportMetric(float64(page.transcriptViews-before[i]), fmt.Sprintf("pane%d-views/drag", i))
	}
}
