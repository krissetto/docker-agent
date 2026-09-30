package chat

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func screenShell(g SplitShellGeometry, x, y int) SplitShellGeometry {
	for _, r := range []*PresentationRect{&g.TranscriptArea, &g.Sidebar, &g.SidebarHandle} {
		r.X += x
		r.Y += y
	}
	return g
}

func TestMeasureSplitShellMatchesSingleAndDoesNotMutate(t *testing.T) {
	for _, position := range []msgtypes.SidebarPosition{msgtypes.SidebarRight, msgtypes.SidebarLeft, msgtypes.SidebarTop, msgtypes.SidebarBottom} {
		for _, collapsed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", position, collapsed), func(t *testing.T) {
				p := newLayoutTestPage(t, position)
				p.sidebar.SetCollapsed(collapsed)
				p.SetSize(160, 40)
				sl, settings := p.appliedLayout, p.GetSidebarSettings()
				generation := p.VisualGeneration()
				g := p.MeasureSplitShell(160, 40)
				require.Equal(t, sl.chatWidth, g.TranscriptArea.Width)
				require.Equal(t, sl.chatHeight, g.TranscriptArea.Height)
				require.Equal(t, styles.AppPadding+sl.chatStartX, g.TranscriptArea.X)
				if position == msgtypes.SidebarBottom {
					require.Equal(t, sl.chatHeight, g.Sidebar.Y)
				}
				_ = p.MeasureSplitShell(80, 17)
				require.Equal(t, sl, p.appliedLayout)
				require.Equal(t, settings, p.GetSidebarSettings())
				require.Equal(t, generation, p.VisualGeneration())
				require.Equal(t, 160, p.width)
				require.Equal(t, 40, p.height)
				for _, size := range [][2]int{{0, 0}, {1, 1}, {80, 2}} {
					clipped := p.MeasureSplitShell(size[0], size[1])
					for _, r := range []PresentationRect{clipped.TranscriptArea, clipped.Sidebar, clipped.SidebarHandle} {
						require.GreaterOrEqual(t, r.Width, 0)
						require.GreaterOrEqual(t, r.Height, 0)
						require.LessOrEqual(t, r.X+r.Width, size[0])
						require.LessOrEqual(t, r.Y+r.Height, size[1])
					}
				}
			})
		}
	}
}

type positionedMessages struct {
	messages.Model

	x, y int
}

func (m *positionedMessages) SetPosition(x, y int) tea.Cmd {
	m.x, m.y = x, y
	return m.Model.SetPosition(x, y)
}

func TestSplitPresentationScreenGeometryAndRestoration(t *testing.T) {
	for _, position := range []msgtypes.SidebarPosition{msgtypes.SidebarRight, msgtypes.SidebarLeft, msgtypes.SidebarTop, msgtypes.SidebarBottom} {
		t.Run(string(position), func(t *testing.T) {
			p := newLayoutTestPage(t, position)
			p.SetSize(160, 40)
			original := p.appliedLayout
			tracked := &positionedMessages{Model: p.messages}
			p.messages = tracked
			p.messages.AddUserMessage("λ界 first pane")
			g := SplitPresentationGeometry{
				Transcript:  PresentationRect{70, 19, 37, 9},
				Shell:       screenShell(p.MeasureSplitShell(160, 40), 7, 5),
				ShowSidebar: true,
			}
			p.SetSplitPresentation(&g)
			require.Equal(t, 70, tracked.x)
			require.Equal(t, 19, tracked.y)
			frame := p.TranscriptView()
			require.Contains(t, ansi.Strip(frame), "λ界 first pane")
			require.NotContains(t, ansi.Strip(frame), "New session")
			require.Len(t, strings.Split(frame, "\n"), 9)
			for line := range strings.SplitSeq(frame, "\n") {
				require.Equal(t, 37, ansi.StringWidth(line))
			}
			require.Equal(t, TargetMessages, NewHitTest(p).At(70, 19))
			require.Equal(t, TargetNone, NewHitTest(p).At(69, 19))
			require.Equal(t, TargetNone, NewHitTest(p).At(70, 28))
			require.Equal(t, wheelTargetNone, p.wheelTarget(69, 19))
			require.Equal(t, wheelTargetMessages, p.wheelTarget(70, 19))
			if g.Shell.SidebarHandle.Width > 0 {
				require.Equal(t, TargetSidebarToggle, NewHitTest(p).At(g.Shell.SidebarHandle.X, g.Shell.SidebarHandle.Y))
				require.NotEmpty(t, p.SidebarHandleView())
			}
			// Geometry is copied rather than retaining the caller's mutable pointer.
			g.Transcript.X++
			require.Equal(t, TargetMessages, NewHitTest(p).At(70, 19))
			g.ShowSidebar = false
			p.SetSplitPresentation(&g)
			require.Contains(t, ansi.Strip(p.SidebarView()), "New session", "unfocused owning sidebar remains inspectable")
			require.Empty(t, p.SidebarHandleView())
			require.NotEmpty(t, p.TranscriptView())
			require.Equal(t, TargetNone, NewHitTest(p).At(g.Shell.Sidebar.X, g.Shell.Sidebar.Y))
			p.SetSplitPresentation(nil)
			require.Equal(t, original, p.appliedLayout)
			require.Equal(t, styles.AppPadding+original.chatStartX, tracked.x)
			require.Contains(t, ansi.Strip(p.View()), "New session")
		})
	}
}

func TestSplitQueuedClickUsesSidebarScreenOffset(t *testing.T) {
	p := newLayoutTestPage(t, msgtypes.SidebarLeft)
	p.sidebar.SetQueuedMessages([]sidebar.QueuedMessage{{ID: "exact-turn", Text: "λ界 queued content"}})
	p.SetSize(160, 40)
	g := SplitPresentationGeometry{
		Transcript:  PresentationRect{70, 12, 50, 20},
		Shell:       screenShell(p.MeasureSplitShell(160, 40), 17, 8),
		ShowSidebar: true,
	}
	p.SetSplitPresentation(&g)
	frame := p.SidebarView()
	for y, line := range strings.Split(ansi.Strip(frame), "\n") {
		before, _, found := strings.Cut(line, "λ界 queued content")
		if !found {
			continue
		}
		hit := NewHitTest(p)
		x := g.Shell.Sidebar.X + ansi.StringWidth(before)
		require.Equal(t, TargetSidebarQueuedMessage, hit.At(x, g.Shell.Sidebar.Y+y))
		require.Equal(t, "exact-turn", hit.QueueTurnID)
		return
	}
	t.Fatalf("queue text not rendered: %s", ansi.Strip(frame))
}

func TestSplitHeightAndFocusChangesReuseTranscript(t *testing.T) {
	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	p.SetSize(160, 40)
	for i := range 200 {
		p.messages.AddUserMessage(fmt.Sprintf("history λ界 %d", i))
	}
	g := SplitPresentationGeometry{Transcript: PresentationRect{12, 8, 60, 15}, Shell: p.MeasureSplitShell(160, 40), ShowSidebar: true}
	p.SetSplitPresentation(&g)
	p.TranscriptView()
	r, m, rendered := p.ResizeCacheStats()
	for _, height := range []int{14, 18, 12, 15} {
		g.Transcript.Height = height
		g.Transcript.X++
		g.ShowSidebar = !g.ShowSidebar
		p.SetSplitPresentation(&g)
		p.TranscriptView()
		r2, m2, rendered2 := p.ResizeCacheStats()
		require.Equal(t, r, r2)
		require.Equal(t, m, m2)
		require.Equal(t, rendered, rendered2)
	}
}

func TestSplitVisibilityKeepsUnfocusedTranscriptLeases(t *testing.T) {
	p := newTestChatPage(t)
	t.Cleanup(p.ar.Stop)
	p.SetSize(160, 40)
	g := SplitPresentationGeometry{Transcript: PresentationRect{3, 6, 60, 20}, Shell: p.MeasureSplitShell(160, 40), ShowSidebar: true}
	p.SetSplitPresentation(&g)
	p.messages.AddAssistantMessage("root", "")
	p.SetTitleRegenerating(true)
	require.Equal(t, int32(2), p.ar.ActiveCount())
	g.ShowSidebar = false
	p.SetSplitPresentation(&g)
	require.Equal(t, int32(1), p.ar.ActiveCount(), "unfocused transcript still owns its spinner")
	require.NotEmpty(t, p.TranscriptView())
	p.SetPresentationVisible(false)
	require.False(t, p.ar.HasActive())
	p.Update(&runtime.ToolsetInfoEvent{Loading: true})
	require.False(t, p.ar.HasActive(), "hidden ingestion cannot retain a sidebar lease")
	p.SetPresentationVisible(true)
	require.Equal(t, int32(1), p.ar.ActiveCount())
	require.Nil(t, p.SetPresentationVisible(true), "repeat visibility is idempotent")
	g.ShowSidebar = true
	p.SetSplitPresentation(&g)
	require.Equal(t, int32(2), p.ar.ActiveCount())
	p.Update(msgtypes.StreamCancelledMsg{})
	CancelSidebarPresentation(p)
	require.False(t, p.ar.HasActive())
}

type inspectionMessages struct {
	messages.Model

	initializations int
}

func (m *inspectionMessages) Init() tea.Cmd {
	m.initializations++
	return m.Model.Init()
}

func TestHiddenOwningViewRemainsReadableWithoutAnimationLeases(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprintf("split=%v", split), func(t *testing.T) {
			p := newTestChatPage(t)
			t.Cleanup(p.ar.Stop)
			p.SetSize(160, 40)
			if split {
				p.SetSplitPresentation(&SplitPresentationGeometry{
					Transcript:  PresentationRect{17, 9, 60, 20},
					Shell:       p.MeasureSplitShell(160, 40),
					ShowSidebar: true,
				})
			}
			p.messages.AddUserMessage("λ界 owning transcript remains readable")
			p.messages.AddAssistantMessage("root", "")
			p.sidebar.SetQueuedMessages([]sidebar.QueuedMessage{{ID: "hidden-turn", Text: "hidden pending"}})
			p.SetTitleRegenerating(true)
			require.True(t, p.ar.HasActive())
			tracked := &inspectionMessages{Model: p.messages}
			p.messages = tracked
			p.SetPresentationVisible(false)
			require.Zero(t, p.ar.ActiveCount())
			for range 3 {
				require.Contains(t, ansi.Strip(p.View()), "λ界 owning transcript remains readable")
				require.Contains(t, ansi.Strip(p.TranscriptView()), "λ界 owning transcript remains readable")
				require.Zero(t, p.ar.ActiveCount(), "explicit hidden rendering must not resume animation")
				require.Zero(t, tracked.initializations, "rendering must not replay Init or its media effects")
				require.Contains(t, ansi.Strip(p.SidebarView()), "hidden pending")
				require.Zero(t, p.ar.ActiveCount(), "hidden sidebar reads must not resume animation")
				require.Empty(t, p.SidebarHandleView())
			}
		})
	}
}

func TestPresentationViewClipsANSIAndWideCells(t *testing.T) {
	view := presentationView("\x1b[31mλ界界\x1b[0m\nsecond\nclipped", 4, 2)
	require.Len(t, strings.Split(view, "\n"), 2)
	for line := range strings.SplitSeq(view, "\n") {
		require.Equal(t, 4, ansi.StringWidth(line))
	}
	require.Contains(t, ansi.Strip(view), "λ界")
	require.NotContains(t, ansi.Strip(view), "clipped")
	require.Empty(t, presentationView(view, 0, 4))
	require.Empty(t, presentationView(view, 4, 0))
}

func TestQueuedActionAdapterEditAndConfirmRemoval(t *testing.T) {
	p := newLayoutTestPage(t, msgtypes.SidebarLeft)
	p.app = newTestChatPage(t).app
	p.ctx = t.Context
	p.messageQueue = []queuedMessage{{turnID: "exact-turn", content: "editable content"}}
	p.sidebar.SetQueuedMessages([]sidebar.QueuedMessage{{ID: "exact-turn", Text: "editable content"}})
	p.SetSize(160, 40)
	g := SplitPresentationGeometry{Transcript: PresentationRect{70, 12, 50, 20}, Shell: screenShell(p.MeasureSplitShell(160, 40), 17, 8), ShowSidebar: true}
	p.SetSplitPresentation(&g)
	point := func(target MouseTarget) tea.MouseClickMsg {
		t.Helper()
		for y := g.Shell.Sidebar.Y; y < g.Shell.Sidebar.Y+g.Shell.Sidebar.Height; y++ {
			for x := g.Shell.Sidebar.X; x < g.Shell.Sidebar.X+g.Shell.Sidebar.Width; x++ {
				hit := NewHitTest(p)
				if hit.At(x, y) == target && hit.QueueTurnID == "exact-turn" {
					return tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}
				}
			}
		}
		t.Fatalf("missing target %v", target)
		return tea.MouseClickMsg{}
	}
	remove := point(TargetSidebarRemoveQueuedMessage)
	_, cmd := p.handleMouseClick(remove)
	require.Nil(t, cmd, "first click only arms removal")
	edit := point(TargetSidebarEditQueuedMessage)
	_, cmd = p.handleMouseClick(edit)
	require.NotNil(t, cmd, "edit glyph opens on single click")
	var opened []msgtypes.OpenPendingEditMsg
	var collect func(tea.Cmd)
	collect = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		switch msg := cmd().(type) {
		case tea.BatchMsg:
			for _, child := range msg {
				collect(child)
			}
		case msgtypes.OpenPendingEditMsg:
			opened = append(opened, msg)
		}
	}
	collect(cmd)
	require.Equal(t, []msgtypes.OpenPendingEditMsg{{SessionID: p.app.Session().ID, TurnID: "exact-turn", Content: "editable content"}}, opened)
	_, cmd = p.handleMouseClick(remove)
	require.Nil(t, cmd, "edit click disarms earlier removal")
	_, cmd = p.handleMouseClick(remove)
	require.NotNil(t, cmd, "second removal click alone dispatches cancel")
}
