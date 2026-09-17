package tui

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/tabbar"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

// Keep the real owner's returned command chain. Timer allocation alone is not
// delivery: collectMsgs executes Continue's command, and only root.Update may
// Accept its resulting TickMsg. No fabricated TickMsg or direct Accept is used.
type sidebarTransitionDriver struct {
	root    *appModel
	pending []tea.Msg
}

func (d *sidebarTransitionDriver) update(msg tea.Msg) {
	_, cmd := d.root.Update(msg)
	d.pending = append(d.pending, collectMsgs(cmd)...)
}

func (d *sidebarTransitionDriver) step(t *testing.T) {
	t.Helper()
	before := d.root.ar.Now()
	for range 100 {
		require.NotEmpty(t, d.pending, "active sidebar must have an executed owner Continue command")
		msg := d.pending[0]
		d.pending = d.pending[1:]
		d.update(msg)
		d.root.View()
		if d.root.ar.Now() > before {
			return
		}
	}
	t.Fatal("owner command chain did not deliver an accepted frame")
}

func (d *sidebarTransitionDriver) settle(t *testing.T) {
	t.Helper()
	for range 120 {
		if d.root.ar.ActiveCount() == 0 {
			return
		}
		d.step(t)
	}
	t.Fatal("finite sidebar transition did not release its leases")
}

func sidebarTransitionRoot(t *testing.T) *sidebarTransitionDriver {
	t.Helper()
	root, _, _ := wallClockRoot(t, 160, 48)
	root.ar.Stop()
	root.ar = animation.NewRuntimeWithScheduler(&rootImmediateScheduler{now: time.Unix(1, 0)})
	root.tabBar = tabbar.New(root.ar, 0)
	root.hideSidebar = false
	d := &sidebarTransitionDriver{root: root}
	for i, id := range []string{"profile", "second", "third"} {
		var application *app.App
		if id == "profile" {
			application = root.application
		} else {
			sess := session.New(session.WithID(id), session.WithAgentName("root"))
			sess.Title = id
			application = app.New(t.Context(), nil, sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(stubRuntime{}))
			_, err := root.supervisor.AddSession(t.Context(), application, sess, "", nil)
			require.NoError(t, err)
		}
		sess := application.Session()
		history, _, _ := mixedHistorySession(40)
		sess.Messages = history.Messages
		children := make([]subagent.NodeSnapshot, 3+i)
		createdAt := time.Now().Add(-10*time.Minute - 30*time.Second)
		for j := range children {
			children[j] = subagent.NodeSnapshot{Node: subagent.Node{
				ID: subagent.NodeID(fmt.Sprintf("%s-canonical-%d", id, j)), SessionID: fmt.Sprintf("%s-child-%d", id, j),
				Agent: "worker", Name: fmt.Sprintf("%s-CHILD-%d", id, j), State: subagent.NodeCompleted,
				CreatedAt: createdAt, UpdatedAt: createdAt.Add(time.Minute),
			}}
		}
		sess.SetSubagentTree(&subagent.Snapshot{Root: subagent.SessionRootID(id), Nodes: []subagent.NodeSnapshot{{
			Node: subagent.Node{ID: subagent.SessionRootID(id), SessionID: id, Agent: "root", State: subagent.NodeCompleted}, Children: children,
		}}})
		// Session hydration is fixture startup only. Cached switches below must
		// retain this page and its loaded transcript, not call Init again.
		root.createSessionComponents(id, application, sess)
		d.pending = append(d.pending, collectMsgs(root.chatPages[id].Init())...)
	}
	root.chatPage, root.editor, root.sessionState = root.chatPages["profile"], root.editors["profile"], root.sessionStates["profile"]
	tabs, active := root.supervisor.GetTabs()
	root.setTabs(tabs, active)
	d.update(tea.WindowSizeMsg{Width: 160, Height: 48})
	d.settle(t)
	for _, id := range []string{"second", "third", "profile"} {
		d.update(messages.SwitchTabMsg{SessionID: id})
		d.settle(t)
		root.View()
	}
	t.Cleanup(func() {
		for _, page := range root.chatPages {
			chat.Cleanup(page)
		}
		root.ar.Stop()
	})
	return d
}

func sidebarTransitionStats(root *appModel) map[string][3]uint64 {
	stats := make(map[string][3]uint64, len(root.chatPages))
	for id, page := range root.chatPages {
		a, b, c := page.(resizeCacheReporter).ResizeCacheStats()
		stats[id] = [3]uint64{a, b, c}
	}
	return stats
}

// Optional validator-owned artifact directory. .ansi is the actual, unmodified
// root View().Content (not terminal-diff output); .txt is separately normalized.
func captureSidebarTransition(t *testing.T, root *appModel, stage string) string {
	t.Helper()
	frame := root.View().Content
	rows := strings.Split(frame, "\n")
	require.Len(t, rows, root.height, "transition preserves terminal height")
	for _, row := range rows {
		require.Equal(t, root.width, ansi.StringWidth(row), "transition preserves terminal width")
	}
	t.Logf("sidebar progression stage=%s elapsed=%s leases=%d raw-ansi=%q", stage, root.ar.Now(), root.ar.ActiveCount(), frame)
	if directory := os.Getenv("TUI_SIDEBAR_TRANSITION_CAPTURE_DIR"); directory != "" {
		require.NoError(t, os.MkdirAll(directory, 0o700))
		name := strings.ReplaceAll(t.Name(), "/", "_") + "-" + stage
		writeSidebarTransitionCapture(t, filepath.Join(directory, name+".ansi"), frame)
		writeSidebarTransitionCapture(t, filepath.Join(directory, name+".txt"), ansi.Strip(frame))
	}
	return frame
}

func writeSidebarTransitionCapture(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	require.NoError(t, err, "capture needs a fresh validator-owned artifact directory")
	_, writeErr := file.WriteString(content)
	closeErr := file.Close()
	require.NoError(t, writeErr)
	require.NoError(t, closeErr)
}

func TestRootCachedSidebarSwitchProgressionAndRapidInterruption(t *testing.T) {
	d := sidebarTransitionRoot(t)
	root := d.root
	owners := map[string]chat.Page{}
	maps.Copy(owners, root.chatPages)
	stats := sidebarTransitionStats(root)
	bounds, shell := root.paneBounds, root.paneShell
	contentHeight, editorTop := root.contentHeight, root.editorTop()
	initial := captureSidebarTransition(t, root, "A-settled")
	source := root.chatPage.(chat.SplitPresentation).SidebarView()
	d.update(messages.SwitchTabMsg{SessionID: "second"})
	require.Same(t, owners["second"], root.chatPage)
	require.Equal(t, source, root.chatPage.(chat.SplitPresentation).SidebarView(), "cached destination starts at the outgoing painted state, not its own old target")
	require.Positive(t, root.ar.ActiveCount())
	start := captureSidebarTransition(t, root, "AB-start")
	d.step(t)
	d.step(t)
	middle := captureSidebarTransition(t, root, "AB-intermediate")
	require.NotEqual(t, start, middle, "accepted root ticks publish an intermediate frame")
	painted := root.chatPage.(chat.SplitPresentation).SidebarView()
	d.update(messages.SwitchTabMsg{SessionID: "third"})
	require.Equal(t, painted, root.chatPage.(chat.SplitPresentation).SidebarView(), "A→B→C captures the currently displayed interpolation, not settled A or B")
	captureSidebarTransition(t, root, "ABC-start")
	d.step(t)
	captureSidebarTransition(t, root, "ABC-intermediate")
	d.settle(t)
	final := captureSidebarTransition(t, root, "C-settled")
	require.NotEqual(t, initial, final)
	require.Contains(t, ansi.Strip(final), "third-CHILD-0")
	require.NotContains(t, ansi.Strip(final), "profile-CHILD-")
	require.NotContains(t, ansi.Strip(final), "second-CHILD-")
	require.Equal(t, bounds, root.paneBounds)
	require.Equal(t, shell, root.paneShell)
	require.Equal(t, contentHeight, root.contentHeight)
	require.Equal(t, editorTop, root.editorTop())
	for id, page := range owners {
		require.Same(t, page, root.chatPages[id])
	}
	require.Equal(t, stats, sidebarTransitionStats(root), "switching never rebuilds or rerenders warmed transcripts")
	require.Zero(t, root.ar.ActiveCount(), "hidden and settled pages own no leases")
	require.True(t, root.SettledPresentation())
}

func TestRootSidebarSwitchSourceInertTargetCanonicalAndCollapseRetained(t *testing.T) {
	d := sidebarTransitionRoot(t)
	root := d.root
	x, y := sidebarProgramPoint(t, root.View().Content, "profile-CHILD-0")
	d.update(messages.SwitchTabMsg{SessionID: "second"})
	_, cmd := root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	for _, msg := range collectMsgs(cmd) {
		_, opensSubagent := msg.(messages.OpenSubagentMsg)
		require.False(t, opensSubagent, "painted source-only child cannot navigate")
		d.pending = append(d.pending, msg)
	}
	require.Equal(t, "second", root.supervisor.ActiveID())
	d.step(t)
	d.step(t)
	require.Positive(t, root.ar.ActiveCount(), "target hit is exercised during the handoff, not only after settlement")
	// Select the destination's own painted child; inspect the emitted canonical
	// command but do not execute a subagent attach/provider operation.
	x, y = sidebarProgramPoint(t, root.View().Content, "second-CHILD-0")
	_, cmd = root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	found := false
	for _, msg := range collectMsgs(cmd) {
		if open, ok := msg.(messages.OpenSubagentMsg); ok {
			require.Equal(t, "second-canonical-0", open.NodeID)
			found = true
		} else {
			d.pending = append(d.pending, msg)
		}
	}
	require.True(t, found, "destination click routes its exact canonical node ID")
	d.settle(t)
	require.Contains(t, ansi.Strip(root.View().Content), "second-CHILD-3", "source-only row click cannot collapse the destination's tree")
	// Whole-tree collapse is a live destination preference, not snapshot data.
	x, y = sidebarProgramPoint(t, root.View().Content, "4 subagents")
	d.update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	d.settle(t)
	require.NotContains(t, ansi.Strip(root.View().Content), "second-CHILD-")
	settings := root.chatPage.GetSidebarSettings()
	for _, id := range []string{"third", "second"} {
		d.update(messages.SwitchTabMsg{SessionID: id})
		d.settle(t)
	}
	require.Equal(t, settings, root.chatPage.GetSidebarSettings())
	require.Contains(t, ansi.Strip(root.View().Content), "4 subagents")
	require.NotContains(t, ansi.Strip(root.View().Content), "second-CHILD-", "cached destination retains its own tree collapse")
	require.Zero(t, root.ar.ActiveCount())
}

func TestRootSidebarHoverIdleSplitTranscriptCounters(t *testing.T) {
	d := sidebarTransitionRoot(t)
	root := d.root
	d.pending = append(d.pending, collectMsgs(root.splitPane("second", "profile", splitRight))...)
	d.update(tea.WindowSizeMsg{Width: 160, Height: 48})
	d.settle(t)
	root.View()
	stats := sidebarTransitionStats(root)
	bounds, shell := root.paneBounds, root.paneShell
	base := root.View().Content
	x, y := sidebarProgramPoint(t, base, "second-CHILD-0")
	d.update(tea.MouseMotionMsg{X: x, Y: y})
	require.Positive(t, root.ar.ActiveCount())
	d.step(t)
	intermediate := root.View().Content
	d.settle(t)
	hovered := root.View().Content
	require.NotEqual(t, base, intermediate)
	require.NotEqual(t, intermediate, hovered)
	baseRows := strings.Split(ansi.Strip(base), "\n")
	hoverRows := strings.Split(ansi.Strip(hovered), "\n")
	require.Len(t, hoverRows, len(baseRows))
	for row := range baseRows {
		require.Equal(t, ansi.StringWidth(baseRows[row]), ansi.StringWidth(hoverRows[row]))
		if row == y {
			require.Contains(t, baseRows[row], "second-CHILD-0")
			require.Contains(t, baseRows[row], "completed")
			require.Contains(t, hoverRows[row], "second-CHILD-0")
			require.Contains(t, hoverRows[row], "10m ago", "hover truthfully reveals the node's creation age")
			require.Equal(t, ansi.Cut(baseRows[row], 0, x), ansi.Cut(hoverRows[row], 0, x), "hover cannot move the name or change neighboring transcripts")
		} else {
			require.Equal(t, baseRows[row], hoverRows[row], "hover leaves every other row unchanged")
		}
	}
	d.update(tea.MouseMotionMsg{X: 0, Y: 0})
	d.settle(t)
	require.Equal(t, base, root.View().Content)
	for range 5 {
		d.update(tea.MouseMotionMsg{X: 0, Y: 0})
		root.View()
	}
	require.Equal(t, stats, sidebarTransitionStats(root), "all visible idle histories retain zero rebuild/item-miss/render deltas throughout root hover ticks")
	require.Equal(t, bounds, root.paneBounds)
	require.Equal(t, shell, root.paneShell)
	require.Zero(t, root.ar.ActiveCount())
	require.True(t, root.SettledPresentation())
}

func TestRootSidebarFailedSwitchDoesNotAdoptPresentation(t *testing.T) {
	d := sidebarTransitionRoot(t)
	root := d.root
	page := root.chatPage
	before := page.(chat.SplitPresentation).SidebarView()
	_, _ = root.Update(messages.SwitchTabMsg{SessionID: "missing-sidebar-session"})
	// Error notification command deliberately remains unexecuted: this guard
	// observes only the failed switch's synchronous presentation side effects.
	require.Same(t, page, root.chatPage)
	require.Equal(t, "profile", root.supervisor.ActiveID())
	require.Equal(t, before, page.(chat.SplitPresentation).SidebarView())
	require.Zero(t, root.ar.ActiveCount())
}
