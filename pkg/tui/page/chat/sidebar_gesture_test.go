package chat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func newSidebarGesturePage(t *testing.T, position msgtypes.SidebarPosition) (*chatPage, *time.Time) {
	t.Helper()
	sess := session.New(session.WithID("sidebar-view-session"))
	tree := subagent.Snapshot{Root: "root:sidebar-view-session", Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "root:sidebar-view-session", SessionID: sess.ID, Agent: "root"}, Children: []subagent.NodeSnapshot{
		{Node: subagent.Node{ID: "abcde-branch-full", SessionID: "branch-session", Parent: "root:sidebar-view-session", Name: "作業planner", Agent: "planner", State: subagent.NodeIdle}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "abcde-nested-full", SessionID: "nested-session", Parent: "abcde-branch-full", Agent: "reviewer", State: subagent.NodeIdle}}}},
		{Node: subagent.Node{ID: "abcde-leaf-full", SessionID: "leaf-session", Parent: "root:sidebar-view-session", Agent: "leaf", State: subagent.NodeCompleted}},
	}}}}
	sess.SetSubagentTree(&tree)
	a, _ := newSessionTestApp(t, sess, nil, nil)
	ar := animation.NewRuntimeWithScheduler(&referenceHoverScheduler{now: time.Unix(1, 0)})
	p := New(ar, t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	t.Cleanup(func() { Cleanup(p); ar.Stop() })
	p.layoutSettings.SidebarPosition = position
	p.SetRoutingID("sidebar-tab-routing")
	p.SetSize(160, 40)
	p.resetProjection(runtime.SessionSnapshot{Session: sess})
	p.handleRuntimeEvent(&runtime.SubagentTreeEvent{Snapshot: tree})
	p.sidebar.ReconcileLayout()
	settleSidebarGesture(t, p)
	now := time.Unix(10, 0)
	p.sidebarClickNow = func() time.Time { return now }
	x := styles.AppPadding + p.computeSidebarLayout().sidebarStartX + sidebar.DefaultLayoutConfig().PaddingLeft + 2
	y := renderedLineContaining(t, p.sidebar.View(), "subagents")
	p.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	settleSidebarGesture(t, p)
	require.Contains(t, ansi.Strip(p.sidebar.View()), "reviewer")
	return p, &now
}

func settleSidebarGesture(t *testing.T, p *chatPage) {
	t.Helper()
	for steps := 0; p.ar.HasActive(); steps++ {
		require.Less(t, steps, 100, "finite sidebar animations must settle")
		tick, ok := p.ar.Accept(p.ar.Continue()().(animation.TickMsg))
		require.True(t, ok)
		p.Update(tick)
	}
}

func sidebarGesturePoint(t *testing.T, p *chatPage, label string) (int, int, string) {
	t.Helper()
	y := renderedLineContaining(t, p.sidebar.View(), label)
	line := strings.Split(ansi.Strip(p.sidebar.View()), "\n")[y]
	before, _, found := strings.Cut(line, label)
	require.True(t, found)
	return styles.AppPadding + p.computeSidebarLayout().sidebarStartX + ansi.StringWidth(before), y, line
}

func requireNoSidebarNavigation(t *testing.T, events []tea.Msg) {
	t.Helper()
	for _, event := range events {
		switch event.(type) {
		case msgtypes.OpenSubagentMsg, msgtypes.SwitchTabMsg, msgtypes.SwitchAgentMsg, msgtypes.StopSubagentSubtreeMsg:
			t.Fatalf("single click dispatched navigation/action: %#v", event)
		}
	}
}

func TestSidebarAgentIdentitySingleAttachWithoutDisclosure(t *testing.T) {
	for _, position := range []msgtypes.SidebarPosition{msgtypes.SidebarLeft, msgtypes.SidebarRight} {
		for _, expanded := range []bool{false, true} {
			for _, width := range []int{22, 70} {
				t.Run(fmt.Sprintf("%s/expanded=%t/width=%d", position, expanded, width), func(t *testing.T) {
					p, _ := newSidebarGesturePage(t, position)
					p.sidebar.SetPreferredWidth(width)
					p.SetSize(160, 40)
					settleSidebarGesture(t, p)
					x, y, _ := sidebarGesturePoint(t, p, "作")
					if !expanded {
						_, cmd := p.handleMouseClick(tea.MouseClickMsg{X: styles.AppPadding + p.computeSidebarLayout().sidebarStartX + sidebar.DefaultLayoutConfig().PaddingLeft, Y: y, Button: tea.MouseLeft})
						requireNoSidebarNavigation(t, runTimerCmd(t, cmd))
						settleSidebarGesture(t, p)
					}
					before := ansi.Strip(p.sidebar.View())
					_, cmd := p.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
					events := runTimerCmd(t, cmd)
					require.Contains(t, events, msgtypes.OpenSubagentMsg{NodeID: "abcde-branch-full"})
					require.Contains(t, events, msgtypes.ShowInteractionHintMsg{SessionID: "sidebar-tab-routing"})
					require.NotContains(t, events, msgtypes.ShowInteractionHintMsg{SessionID: "sidebar-tab-routing", Text: "Double-click to attach"})
					settleSidebarGesture(t, p)
					require.Equal(t, before, ansi.Strip(p.sidebar.View()), "name attaches without hover or a disclosure change")
				})
			}
		}
	}
}

func TestSidebarAgentRowRepeatedClicksOnlyToggle(t *testing.T) {
	for _, position := range []msgtypes.SidebarPosition{msgtypes.SidebarLeft, msgtypes.SidebarRight} {
		for _, expanded := range []bool{false, true} {
			for _, part := range []string{"indent", "count", "whitespace", "status"} {
				t.Run(fmt.Sprintf("%s/expanded=%t/%s", position, expanded, part), func(t *testing.T) {
					p, now := newSidebarGesturePage(t, position)
					x, y, _ := sidebarGesturePoint(t, p, "作業planner")
					if !expanded {
						p.handleMouseClick(tea.MouseClickMsg{X: x + 25, Y: y, Button: tea.MouseLeft})
						settleSidebarGesture(t, p)
					}
					p.lastSidebarClick = sidebarClick{}
					base := styles.AppPadding + p.computeSidebarLayout().sidebarStartX
					_, _, line := sidebarGesturePoint(t, p, "作業planner")
					switch part {
					case "indent":
						x = base + sidebar.DefaultLayoutConfig().PaddingLeft
					case "count":
						x += ansi.StringWidth("作業planner ")
					case "whitespace":
						x += 25
					case "status":
						x = base + ansi.StringWidth(strings.TrimRight(line, " ")) - ansi.StringWidth("idle")
					}
					click := tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}
					_, cmd := p.handleMouseClick(click)
					events := runTimerCmd(t, cmd)
					requireNoSidebarNavigation(t, events)
					require.NotContains(t, events, msgtypes.ShowInteractionHintMsg{SessionID: "sidebar-tab-routing", Text: "Double-click to attach"})
					settleSidebarGesture(t, p)
					require.Equal(t, !expanded, strings.Contains(ansi.Strip(p.sidebar.View()), "reviewer"))
					*now = now.Add(100 * time.Millisecond)
					_, cmd = p.handleMouseClick(click)
					events = runTimerCmd(t, cmd)
					requireNoSidebarNavigation(t, events)
					settleSidebarGesture(t, p)
					require.Equal(t, expanded, strings.Contains(ansi.Strip(p.sidebar.View()), "reviewer"), "second row click toggles again, never attaches")
				})
			}
		}
	}
}

func TestSidebarAgentChevronRepeatedClicksOnlyToggle(t *testing.T) {
	p, now := newSidebarGesturePage(t, msgtypes.SidebarRight)
	x, y, _ := sidebarGesturePoint(t, p, "作業planner")
	p.handleMouseMotion(tea.MouseMotionMsg{X: x, Y: y})
	settleSidebarGesture(t, p)
	x, y, _ = sidebarGesturePoint(t, p, "⌄")
	for i := range 2 {
		_, cmd := p.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		events := runTimerCmd(t, cmd)
		requireNoSidebarNavigation(t, events)
		require.NotContains(t, events, msgtypes.ShowInteractionHintMsg{SessionID: "sidebar-tab-routing", Text: "Double-click to attach"})
		settleSidebarGesture(t, p)
		require.Equal(t, i == 1, strings.Contains(ansi.Strip(p.sidebar.View()), "reviewer"))
		*now = now.Add(100 * time.Millisecond)
	}
}

func TestSidebarAgentLeafOnlyNameAttachesAndNeverExpands(t *testing.T) {
	for _, part := range []string{"name", "row", "id"} {
		t.Run(part, func(t *testing.T) {
			p, now := newSidebarGesturePage(t, msgtypes.SidebarRight)
			p.sidebar.SetPreferredWidth(70)
			p.SetSize(160, 40)
			settleSidebarGesture(t, p)
			x, y, _ := sidebarGesturePoint(t, p, "leaf")
			p.handleMouseMotion(tea.MouseMotionMsg{X: x, Y: y})
			settleSidebarGesture(t, p)
			_, _, before := sidebarGesturePoint(t, p, "leaf")
			if part == "row" {
				x = styles.AppPadding + p.computeSidebarLayout().sidebarStartX + sidebar.DefaultLayoutConfig().PaddingLeft
			} else if part == "id" {
				x, y, _ = sidebarGesturePoint(t, p, "(abcde-leaf-full)")
			}
			for range 2 {
				_, cmd := p.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				events := runTimerCmd(t, cmd)
				if part == "name" {
					require.Contains(t, events, msgtypes.OpenSubagentMsg{NodeID: "abcde-leaf-full"})
				} else {
					requireNoSidebarNavigation(t, events)
					require.NotContains(t, events, msgtypes.ShowInteractionHintMsg{SessionID: "sidebar-tab-routing", Text: "Double-click to attach"})
					_, _, after := sidebarGesturePoint(t, p, "leaf")
					require.Equal(t, before, after, "leaf does not invent expansion or a disclosure glyph")
				}
				*now = now.Add(100 * time.Millisecond)
			}
		})
	}
}

func TestSidebarAgentIdentitySplitScreenAndVisibleIDSuffix(t *testing.T) {
	p, _ := newSidebarGesturePage(t, msgtypes.SidebarLeft)
	p.sidebar.SetPreferredWidth(70)
	p.SetSize(160, 40)
	g := SplitPresentationGeometry{
		Transcript:  PresentationRect{70, 12, 50, 20},
		Shell:       screenShell(p.MeasureSplitShell(160, 40), 17, 8),
		ShowSidebar: true,
	}
	p.SetSplitPresentation(&g)
	settleSidebarGesture(t, p)
	y := renderedLineContaining(t, p.sidebar.View(), "作業planner")
	line := strings.Split(ansi.Strip(p.sidebar.View()), "\n")[y]
	before, _, found := strings.Cut(line, "作業planner")
	require.True(t, found)
	x := g.Shell.Sidebar.X + ansi.StringWidth(before)
	y += g.Shell.Sidebar.Y
	_, cmd := p.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Contains(t, runTimerCmd(t, cmd), msgtypes.OpenSubagentMsg{NodeID: "abcde-branch-full"})
	p.handleMouseMotion(tea.MouseMotionMsg{X: x, Y: y})
	settleSidebarGesture(t, p)
	line = strings.Split(ansi.Strip(p.sidebar.View()), "\n")[y-g.Shell.Sidebar.Y]
	before, _, found = strings.Cut(line, "(abcde-branch-full)")
	require.True(t, found)
	x = g.Shell.Sidebar.X + ansi.StringWidth(before)
	hit := NewHitTest(p)
	require.Equal(t, TargetSidebarSubagent, hit.At(x, y))
	require.False(t, hit.OnSubagentName)
	_, cmd = p.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	events := runTimerCmd(t, cmd)
	requireNoSidebarNavigation(t, events)
	settleSidebarGesture(t, p)
	require.NotContains(t, ansi.Strip(p.sidebar.View()), "reviewer", "visible ID toggles without attaching")
	_, cmd = p.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	requireNoSidebarNavigation(t, runTimerCmd(t, cmd))
	settleSidebarGesture(t, p)
	require.Contains(t, ansi.Strip(p.sidebar.View()), "reviewer", "repeated visible ID click only toggles")
}
