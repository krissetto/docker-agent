package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestPaneBottomBarGeometryAndPointerOwnership(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {64, 24}, {24, 12}, {1, 1}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			root := splitTestRoot(t)
			root.splitPane("second", "profile", splitRight)
			root.splitPane("third", "second", splitBottom)
			root.handleWindowResize(size[0], size[1])
			tree := root.panes.root
			rows := strings.Split(ansi.Strip(root.composePanes()), "\n")
			for id, r := range root.paneGeometry.Panes {
				page := &splitRecordingPage{Page: root.chatPages[id]}
				root.chatPages[id] = page
				if id == root.paneFocus() {
					root.chatPage = page
				}
				bar := root.paneHeaderHeight()
				areas := root.paneAreas(r)
				require.Equal(t, areas.transcript.H, lipgloss.Height(page.TranscriptView()), "viewport excludes the footer and its gap")
				if bar > 0 {
					require.GreaterOrEqual(t, areas.transcript.H, paneMinHeight-2)
					require.Equal(t, 1, areas.gap.H)
					require.Empty(t, strings.TrimSpace(ansi.Cut(rows[areas.gap.Y], r.X, r.X+r.W)))
					bottom := ansi.Cut(rows[r.Y+r.H-1], r.X, r.X+r.W)
					require.Equal(t, ansi.Strip(root.paneTitle(id, r.W)), bottom)
					require.NotContains(t, bottom, "Send to")
					require.NotContains(t, bottom, "children")
				}
				page.updates = nil
				click := tea.MouseClickMsg{X: r.X, Y: r.Y, Button: tea.MouseLeft}
				root.forwardPanePointer(click, click.X, click.Y, false)
				require.Contains(t, page.updates, click, "top cell belongs to transcript even in a split")
				page.updates = nil
				click.Y = r.Y + r.H - 1
				root.forwardPanePointer(click, click.X, click.Y, false)
				if bar > 0 {
					require.Empty(t, page.updates, "bar click cannot select a transcript message or thumb")
				} else {
					require.Contains(t, page.updates, click, "compact single pane does not reserve a bar")
				}
			}
			require.Same(t, tree, root.panes.root)
		})
	}
}

func TestPaneBottomBarFocusDoubleClickWheelAndDragStayChromeOnly(t *testing.T) {
	root := splitTestRoot(t)
	first := &splitRecordingPage{Page: root.chatPage}
	root.chatPages["profile"], root.chatPage = first, first
	root.splitPane("second", "profile", splitRight)
	root.editors["profile"].SetValue("first draft")
	root.editor.SetValue("second draft")
	r := root.paneGeometry.Panes["profile"]
	x, y := r.X+2, r.Y+r.H-1
	first.updates = nil
	root.Update(messages.WheelCoalescedMsg{X: x, Y: y, Delta: -1})
	require.Equal(t, "second", root.paneFocus(), "bar wheel never steals the composer")
	require.Empty(t, first.updates)
	for range 2 {
		root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		root.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	}
	require.Equal(t, "profile", root.paneFocus())
	require.Equal(t, "first draft", root.editor.Value())
	require.Equal(t, "second draft", root.editors["second"].Value())
	require.False(t, root.dialogMgr.Open(), "bar double-click does not rename sidebar or tab titles")
	root.Update(tea.MouseMotionMsg{X: x + 4, Y: y, Button: tea.MouseLeft})
	require.Nil(t, root.paneGesture, "pane moves still originate from tabs, not the bar")
	require.False(t, root.chatPage.IsSelecting())
	for _, msg := range first.updates {
		switch msg.(type) {
		case tea.MouseClickMsg, tea.MouseReleaseMsg, tea.MouseMotionMsg, messages.WheelCoalescedMsg:
			t.Fatalf("bar pointer leaked into transcript: %#v", msg)
		}
	}
	require.Zero(t, root.ar.ActiveCount(), "idle bar interactions acquire no animation leases")
}

type paneBarTreeRuntime struct {
	stubRuntime

	tree *subagent.Tree
}

func (r *paneBarTreeRuntime) SubagentTree() *subagent.Tree { return r.tree }

func TestPaneBarCountsDirectSubagentsAcrossStatesAndRestoration(t *testing.T) {
	root := splitTestRoot(t)
	root.sessionStates["profile"].SetCurrentAgentName("director")
	runner := root.supervisor.GetRunner("profile")
	sess := runner.App.Session()
	nodeID := subagent.SessionRootID(sess.ID)
	snapshot := subagent.Snapshot{Root: nodeID, Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: nodeID},
		Children: []subagent.NodeSnapshot{
			{Node: subagent.Node{ID: "11111", State: subagent.NodeRunning}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "aaaaa"}}}},
			{Node: subagent.Node{ID: "22222", State: subagent.NodeCompleted}},
			{Node: subagent.Node{ID: "33333", State: subagent.NodeFailed}},
		},
	}}}
	sess.SetSubagentTree(&snapshot)
	root.splitPane("second", "profile", splitRight)
	for i := range root.tabInfos {
		if root.tabInfos[i].SessionID == "profile" {
			root.tabInfos[i].Activity = messages.TabActivityDescendantRunning
		}
	}
	require.Equal(t, 3, root.paneSubagentCount("profile", nodeID), "all direct children, excluding grandchild and unrelated views")
	require.Equal(t, 1, root.paneSubagentCount("profile", "11111"), "an attached subtree counts only its own children")
	for _, width := range []int{24, 60, 120} {
		bar := root.paneTitle("profile", width)
		require.Equal(t, width, ansi.StringWidth(bar))
		require.Contains(t, ansi.Strip(bar), "director (3)")
		require.NotContains(t, ansi.Strip(bar), "children")
		require.NotContains(t, ansi.Strip(bar), "Send to")
	}
	require.NotContains(t, ansi.Strip(root.paneTitle("second", 60)), "(0)")
	for _, width := range []int{1, 2, 3, 4, 8} {
		require.Equal(t, width, ansi.StringWidth(root.paneTitle("profile", width)))
	}
	services := &paneBarTreeRuntime{tree: subagent.NewTree()}
	runner.App = app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(services))
	require.Equal(t, 3, root.paneSubagentCount("profile", nodeID), "empty live tree preserves restored metadata")
	require.NoError(t, services.tree.Add(subagent.Node{ID: nodeID, Agent: "director"}))
	require.Zero(t, root.paneSubagentCount("profile", nodeID), "present live root with no children overrides restored tree")
	require.NoError(t, services.tree.Add(subagent.Node{ID: "44444", Parent: nodeID, Agent: "worker"}))
	require.Equal(t, 1, root.paneSubagentCount("profile", nodeID))
	require.Contains(t, ansi.Strip(root.paneTitle("profile", 60)), "director (1)")
	require.Zero(t, root.ar.ActiveCount(), "passive descendant count owns no spinner")
}

type filledPanePage struct{ *splitRecordingPage }

func (p *filledPanePage) TranscriptView() string {
	return strings.Repeat("\x1b[48;2;10;20;30mCONTENT\x1b[m\n", 200)
}

func TestPaneFooterGapRenderingAcrossLayoutsAndResize(t *testing.T) {
	for _, shape := range []string{"columns", "rows", "nested"} {
		t.Run(shape, func(t *testing.T) {
			root := splitTestRoot(t)
			edge := splitRight
			if shape == "rows" {
				edge = splitBottom
			}
			root.splitPane("second", "profile", edge)
			if shape == "nested" {
				root.splitPane("third", "second", splitBottom)
			}
			for id, page := range root.chatPages {
				filled := &filledPanePage{&splitRecordingPage{Page: page}}
				root.chatPages[id] = filled
				if id == root.paneFocus() {
					root.chatPage = filled
				}
			}
			tree := root.panes.root
			for _, size := range [][2]int{{120, 40}, {160, 50}, {64, 24}, {24, 12}, {1, 1}, {120, 40}} {
				root.handleWindowResize(size[0], size[1])
				for _, dim := range []bool{false, true} {
					root.dimInactivePanes = dim
					rows := strings.Split(root.composePanes(), "\n")
					for id, r := range root.paneGeometry.Panes {
						areas := root.paneAreas(r)
						if len(root.paneGeometry.Panes) == 1 {
							require.Equal(t, r, areas.transcript)
							require.Zero(t, areas.gap.H)
							require.Zero(t, areas.title.H)
							continue
						}
						require.Equal(t, r.H-2, areas.transcript.H)
						require.Equal(t, 1, areas.gap.H)
						require.Equal(t, 1, areas.title.H)
						require.Equal(t, r.Y+r.H-1, areas.title.Y)
						content := ansi.Cut(rows[areas.gap.Y-1], r.X, r.X+r.W)
						gap := ansi.Cut(rows[areas.gap.Y], r.X, r.X+r.W)
						title := ansi.Cut(rows[areas.title.Y], r.X, r.X+r.W)
						require.Contains(t, ansi.Strip(content), "CONTENT", "exactly one reserved blank row")
						require.Empty(t, strings.TrimSpace(ansi.Strip(gap)))
						for _, cell := range chromeCells(gap) {
							require.Nil(t, cell.bg, "gap preserves terminal transparency")
						}
						require.Equal(t, ansi.Strip(root.paneTitle(id, r.W)), ansi.Strip(title))
						if size == [2]int{120, 40} && !dim {
							t.Logf("%s footer rows: %q / %q / %q", id, ansi.Strip(content), ansi.Strip(gap), ansi.Strip(title))
						}
					}
				}
				require.Same(t, tree, root.panes.root)
			}
		})
	}
}

func TestPaneFooterGapIsNotAPointerTarget(t *testing.T) {
	root := splitTestRoot(t)
	root.splitPane("second", "profile", splitRight)
	root.splitPane("third", "second", splitBottom)
	for _, id := range root.panes.Sessions() {
		page := installPaneRecorder(root, id)
		areas := root.paneAreas(root.paneGeometry.Panes[id])
		targets := page.Page.(interface{ PointerTargetsMessages(x, y int) bool })
		for x := areas.gap.X; x < areas.gap.X+areas.gap.W; x++ {
			require.True(t, targets.PointerTargetsMessages(x, areas.gap.Y-1))
			require.False(t, targets.PointerTargetsMessages(x, areas.gap.Y))
			require.False(t, targets.PointerTargetsMessages(x, areas.title.Y))
			page.updates = nil
			focus := root.paneFocus()
			for range 2 {
				root.Update(tea.MouseClickMsg{X: x, Y: areas.gap.Y, Button: tea.MouseLeft})
				root.Update(tea.MouseMotionMsg{X: x, Y: areas.gap.Y, Button: tea.MouseLeft})
				root.Update(tea.MouseReleaseMsg{X: x, Y: areas.gap.Y, Button: tea.MouseLeft})
			}
			root.Update(tea.MouseMotionMsg{X: x, Y: areas.gap.Y})
			root.Update(messages.WheelCoalescedMsg{X: x, Y: areas.gap.Y, Delta: -1})
			require.Equal(t, focus, root.paneFocus(), "gap is not a title focus target")
			require.Empty(t, page.updates, "gap is not transcript content")
			require.False(t, page.IsSelecting())
			require.Nil(t, root.messagesScrollbar)
			require.Nil(t, root.paneGesture)
			require.False(t, root.tabBar.HasPointerCapture())
			require.False(t, root.dialogMgr.Open())
		}
	}
}

func TestPaneFooterAreasTinyHeight(t *testing.T) {
	root := &appModel{paneGeometry: splitGeometry{Panes: map[string]splitRect{"first": {}, "second": {}}}}
	for height := range 7 {
		r := splitRect{X: 3, Y: 5, W: 24, H: height}
		areas := root.paneAreas(r)
		require.Equal(t, height, areas.transcript.H+areas.gap.H+areas.title.H)
		require.Equal(t, max(0, height-2), areas.transcript.H)
		require.Equal(t, min(1, max(0, height-1)), areas.gap.H)
		require.Equal(t, min(1, height), areas.title.H)
		require.Equal(t, r.Y+height, areas.title.Y+areas.title.H)
	}
}

func TestPaneSelectionCanFinishOverFooterGap(t *testing.T) {
	root := splitTestRoot(t)
	root.splitPane("second", "profile", splitBottom)
	root.Update(&runtime.UserMessageEvent{Message: "selectable transcript", SessionPosition: 100000})
	root.chatPage.ScrollToBottom()
	frame := strings.Split(root.composePanes(), "\n")
	r := root.paneGeometry.Panes[root.paneFocus()]
	areas := root.paneAreas(r)
	for y := r.Y; y < areas.gap.Y; y++ {
		line := ansi.Cut(ansi.Strip(frame[y]), r.X, r.X+r.W)
		before, _, found := strings.Cut(line, "selectable transcript")
		if !found {
			continue
		}
		x := r.X + ansi.StringWidth(before)
		root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		require.True(t, root.chatPage.IsSelecting())
		root.Update(tea.MouseMotionMsg{X: x + 8, Y: areas.gap.Y, Button: tea.MouseLeft})
		require.True(t, root.chatPage.IsSelecting(), "existing selection keeps its outside-viewport capture")
		root.Update(tea.MouseReleaseMsg{X: x + 8, Y: areas.gap.Y, Button: tea.MouseLeft})
		require.False(t, root.chatPage.IsSelecting())
		require.Equal(t, "second", root.paneFocus())
		selected := strings.Split(root.composePanes(), "\n")
		require.Equal(t, frame[areas.gap.Y], selected[areas.gap.Y], "gap is never painted as selected text")
		require.Equal(t, frame[areas.title.Y], selected[areas.title.Y], "footer stays pinned outside selection")
		return
	}
	t.Fatal("selection fixture missing")
}
