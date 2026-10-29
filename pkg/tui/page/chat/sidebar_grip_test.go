package chat

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/grip"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// gripScheduler delivers animation ticks synchronously on a manual clock.
type gripScheduler struct{ now time.Time }

func (s *gripScheduler) Now() time.Time { return s.now }
func (s *gripScheduler) Tick(d time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		s.now = s.now.Add(d)
		return f(s.now)
	}
}

// decoratorCounter records scrollbar decorator changes per neighbour so a
// test can prove only the bar beside the edge is ever restyled.
type decoratorCounter struct{ set, cleared int }

func (c *decoratorCounter) record(d scrollview.ScrollbarDecorator) {
	if d == nil {
		c.cleared++
	} else {
		c.set++
	}
}

type gripRecordingMessages struct {
	messages.Model
	calls *decoratorCounter
}

func (m *gripRecordingMessages) SetScrollbarDecorator(d scrollview.ScrollbarDecorator) {
	m.calls.record(d)
	m.Model.SetScrollbarDecorator(d)
}

// Update keeps the wrapper installed: the page stores Update's returned model.
func (m *gripRecordingMessages) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	inner, cmd := m.Model.Update(msg)
	m.Model = inner.(messages.Model)
	return m, cmd
}

type gripRecordingSidebar struct {
	sidebar.Model
	calls *decoratorCounter
}

func (m *gripRecordingSidebar) SetScrollbarDecorator(d scrollview.ScrollbarDecorator) {
	m.calls.record(d)
	m.Model.SetScrollbarDecorator(d)
}

func (m *gripRecordingSidebar) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	inner, cmd := m.Model.Update(msg)
	m.Model = inner.(sidebar.Model)
	return m, cmd
}

// styledLastCell matches the final cell of a row together with the SGR that
// paints it, ignoring state left behind by earlier cells.
var styledLastCell = regexp.MustCompile(`\x1b\[[0-9;:]+m[^\x1b]+(\x1b\[[0-9;:]*m)*$`)

// scrollbarCells returns the styled last cell of every transcript row: the
// reserved scrollbar column.
func scrollbarCells(view string) []string {
	lines := strings.Split(view, "\n")
	for i, line := range lines {
		lines[i] = styledLastCell.FindString(line)
	}
	return lines
}

// newGripTestPage builds a page on both docks whose transcript and sidebar
// both overflow, so each neighbour paints a scrollbar beside the edge.
func newGripTestPage(t *testing.T, position msgtypes.SidebarPosition) (*chatPage, *animation.Runtime) {
	p, ar, _, _ := newRecordingGripTestPage(t, position)
	return p, ar
}

func newRecordingGripTestPage(t *testing.T, position msgtypes.SidebarPosition) (*chatPage, *animation.Runtime, *decoratorCounter, *decoratorCounter) {
	t.Helper()
	p := newLayoutTestPage(t, position)
	messagesCalls, sidebarCalls := &decoratorCounter{}, &decoratorCounter{}
	p.messages = &gripRecordingMessages{Model: p.messages, calls: messagesCalls}
	p.sidebar = &gripRecordingSidebar{Model: p.sidebar, calls: sidebarCalls}
	ar := animation.NewRuntimeWithScheduler(&gripScheduler{now: time.Unix(0, 0)})
	t.Cleanup(ar.Stop)
	p.ar = ar
	p.sidebarGrip = newSidebarGripState(ar)
	p.height = 12
	queued := make([]sidebar.QueuedMessage, 30)
	for i := range queued {
		queued[i] = sidebar.QueuedMessage{ID: strconv.Itoa(i), Text: "queued message"}
	}
	p.sidebar.SetQueuedMessages(queued)
	for range 30 {
		p.messages.AddUserMessage("chat line")
	}
	p.SetSize(p.width, p.height)
	p.sidebar.View()
	p.messages.View()
	return p, ar, messagesCalls, sidebarCalls
}

// gripTick runs one accepted animation frame through the page.
func gripTick(t *testing.T, p *chatPage, ar *animation.Runtime) {
	t.Helper()
	cmd := ar.Continue()
	require.NotNil(t, cmd, "a fading grip keeps exactly one tick scheduled")
	tick, ok := cmd().(animation.TickMsg)
	require.True(t, ok)
	accepted, ok := ar.Accept(tick)
	require.True(t, ok)
	p.Update(accepted)
}

func settleGrip(t *testing.T, p *chatPage, ar *animation.Runtime) {
	t.Helper()
	for i := 0; p.sidebarGrip.reveal.Animating(); i++ {
		require.Less(t, i, 30, "fade must settle in a bounded number of frames")
		gripTick(t, p, ar)
	}
	assert.Nil(t, ar.Continue(), "a settled grip schedules no further ticks")
}

func gripHandleX(p *chatPage) int { return styles.AppPadding + p.computeSidebarLayout().handleX }

func gripDots(t *testing.T, p *chatPage) int {
	t.Helper()
	for _, line := range strings.Split(p.renderSidebarHandle(p.computeSidebarLayout().chatHeight), "\n") {
		require.Equal(t, 1, ansi.StringWidth(line), "edge column stays one cell wide")
		if ansi.Strip(line) == "│" {
			return 1
		}
	}
	return 0
}

func TestSidebarGripRevealsOnHoverAndDimsAdjacentScrollbar(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		position msgtypes.SidebarPosition
		dimmed   gripDim
	}{{msgtypes.SidebarRight, gripDimMessages}, {msgtypes.SidebarLeft, gripDimSidebar}} {
		t.Run(string(tc.position), func(t *testing.T) {
			t.Parallel()
			p, ar, messagesCalls, sidebarCalls := newRecordingGripTestPage(t, tc.position)
			adjacent, other := messagesCalls, sidebarCalls
			if tc.dimmed == gripDimSidebar {
				adjacent, other = sidebarCalls, messagesCalls
			}
			require.Equal(t, sidebarVertical, p.computeSidebarLayout().mode)
			assert.Zero(t, gripDots(t, p), "edge is invisible at rest")
			handleX := gripHandleX(p)
			shellGen := p.shellVisualGeneration
			barBefore := scrollbarCells(p.messages.View())

			p.handleMouseMotion(tea.MouseMotionMsg{X: handleX, Y: 5})
			require.True(t, p.sidebarGrip.reveal.Animating(), "hover starts a bounded fade")
			gripTick(t, p, ar)
			partial := p.sidebarGrip.reveal.Visibility()
			assert.Greater(t, partial, 0.0)
			assert.Less(t, partial, 1.0, "the grip fades in rather than popping")
			assert.Equal(t, 1, gripDots(t, p), "compact glyph is present (faded) during the fade")
			settleGrip(t, p, ar)
			assert.InDelta(t, 1, p.sidebarGrip.reveal.Visibility(), 0)
			assert.Equal(t, 1, gripDots(t, p), "compact centered grip is fully revealed")
			assert.Equal(t, tc.dimmed, p.sidebarGrip.dimmed)
			assert.Greater(t, p.shellVisualGeneration, shellGen, "edge column recomposes")
			assert.Positive(t, adjacent.set, "the scrollbar beside the edge is dimmed")
			assert.Zero(t, other.set+other.cleared, "the far scrollbar is never touched")
			barDimmed := scrollbarCells(p.messages.View())
			require.Len(t, barDimmed, len(barBefore), "no layout change")
			for i := range barBefore {
				assert.Equal(t, ansi.Strip(barBefore[i]), ansi.Strip(barDimmed[i]), "row %d: glyphs unchanged", i)
			}
			if tc.dimmed == gripDimMessages {
				assert.NotEqual(t, barBefore, barDimmed, "right dock: transcript scrollbar paint is dimmed")
			} else {
				assert.Equal(t, barBefore, barDimmed, "left dock: transcript scrollbar untouched")
			}

			// Stationary hover: no ticks, no cache churn.
			shellGen, sets := p.shellVisualGeneration, adjacent.set
			p.handleMouseMotion(tea.MouseMotionMsg{X: handleX, Y: 5})
			assert.False(t, p.sidebarGrip.reveal.Animating())
			assert.Equal(t, shellGen, p.shellVisualGeneration)
			assert.Equal(t, sets, adjacent.set)

			// Leaving fades out and restores the scrollbar.
			sl := p.computeSidebarLayout()
			p.handleMouseMotion(tea.MouseMotionMsg{X: styles.AppPadding + sl.chatStartX + 3, Y: 5})
			require.True(t, p.sidebarGrip.reveal.Animating())
			gripTick(t, p, ar)
			assert.Less(t, p.sidebarGrip.reveal.Visibility(), 1.0)
			settleGrip(t, p, ar)
			assert.Zero(t, p.sidebarGrip.reveal.Visibility())
			assert.Equal(t, gripDimNone, p.sidebarGrip.dimmed)
			assert.Equal(t, 1, adjacent.cleared, "the dimmed scrollbar is restored exactly once")
			assert.Zero(t, gripDots(t, p))
			assert.EqualValues(t, 0, ar.ActiveCount())
			assert.Equal(t, barBefore, scrollbarCells(p.messages.View()), "transcript scrollbar paint is fully restored")
		})
	}
}

func TestSidebarGripHighlightsOnlyDirectHandleHover(t *testing.T) {
	t.Parallel()
	p, ar := newGripTestPage(t, msgtypes.SidebarRight)
	handleX := gripHandleX(p)
	sl := p.computeSidebarLayout()
	top, length := grip.Rows(sl.chatHeight)
	centerY := top + length/2
	base := strings.Split(p.renderSidebarHandle(sl.chatHeight), "\n")[centerY]

	// Hovering the edge at a non-grip row reveals it in the subdued resize color.
	p.handleMouseMotion(tea.MouseMotionMsg{X: handleX, Y: centerY + 2})
	settleGrip(t, p, ar)
	revealed := strings.Split(p.renderSidebarHandle(sl.chatHeight), "\n")[centerY]
	assert.Equal(t, styles.ResizeHandleStyle.Render("│"), revealed)
	assert.NotEqual(t, base, revealed)

	// Moving onto the actual glyph adds the sidebar's reversible text highlight.
	p.handleMouseMotion(tea.MouseMotionMsg{X: handleX, Y: centerY})
	require.True(t, p.sidebarGrip.reveal.Animating())
	gripTick(t, p, ar)
	partial := p.sidebarGrip.reveal.HandleHighlight()
	assert.Greater(t, partial, 0.0)
	assert.Less(t, partial, 1.0)
	settleGrip(t, p, ar)
	highlighted := strings.Split(p.renderSidebarHandle(sl.chatHeight), "\n")[centerY]
	assert.Equal(t, styles.HoverText(styles.ResizeHandleStyle.Render("│"), 1, styles.Resize), highlighted)
	assert.NotEqual(t, revealed, highlighted)

	// The toggle is part of the edge but not the grip's direct-hover target.
	p.handleMouseMotion(tea.MouseMotionMsg{X: handleX, Y: 0})
	settleGrip(t, p, ar)
	assert.Zero(t, p.sidebarGrip.reveal.HandleHighlight())
	assert.Equal(t, styles.ResizeHandleStyle.Render("│"), strings.Split(p.renderSidebarHandle(sl.chatHeight), "\n")[centerY])
}

func TestSidebarGripHitGeometry(t *testing.T) {
	t.Parallel()
	for _, position := range []msgtypes.SidebarPosition{msgtypes.SidebarRight, msgtypes.SidebarLeft} {
		t.Run(string(position), func(t *testing.T) {
			t.Parallel()
			p, _ := newGripTestPage(t, position)
			p.sidebarGrip = newSidebarGripState(nil) // snap, so geometry is observable without ticks
			sl := p.computeSidebarLayout()
			handleX := gripHandleX(p)

			assert.True(t, p.sidebarGripAt(handleX, 0), "toggle glyph shares the edge column")
			assert.True(t, p.sidebarGripAt(handleX, sl.chatHeight-1), "bottom row of the edge")
			assert.False(t, p.sidebarGripAt(handleX, sl.chatHeight), "below the content")
			assert.False(t, p.sidebarGripAt(handleX-1, 5), "one column toward the chat/sidebar")
			assert.False(t, p.sidebarGripAt(handleX+1, 5), "one column toward the sidebar/chat")
			assert.False(t, p.sidebarGripAt(styles.AppPadding+sl.sidebarStartX+2, 3), "sidebar content")
			assert.False(t, p.sidebarGripAt(styles.AppPadding+sl.chatStartX+2, 3), "transcript content")

			assert.Equal(t, TargetSidebarResizeHandle, NewHitTest(p).At(handleX, 5), "hit target unchanged: whole edge still resizes")
			assert.Equal(t, TargetSidebarToggle, NewHitTest(p).At(handleX, 0), "hit target unchanged: row 0 still toggles")

			p.handleMouseMotion(tea.MouseMotionMsg{X: handleX, Y: 0})
			assert.Equal(t, 1, gripDots(t, p))
			top, length := grip.Rows(sl.chatHeight)
			lines := strings.Split(p.renderSidebarHandle(sl.chatHeight), "\n")
			for i, line := range lines {
				switch {
				case i == 0:
					assert.Contains(t, []string{"«", "»"}, ansi.Strip(line), "toggle glyph keeps row 0")
				case i >= top && i < top+length:
					assert.Equal(t, "│", ansi.Strip(line), "compact vertical handle is a contiguous line segment")
				default:
					assert.Equal(t, " ", line)
				}
			}
			p.handleMouseMotion(tea.MouseMotionMsg{X: styles.AppPadding + sl.sidebarStartX + 2, Y: 3})
			assert.Zero(t, gripDots(t, p))
		})
	}
}

func TestSidebarGripDragPinsVisibleUntilRelease(t *testing.T) {
	t.Parallel()
	p, ar := newGripTestPage(t, msgtypes.SidebarRight)
	handleX := gripHandleX(p)
	startWidth := p.presentationSidebarSettings().PreferredWidth
	p.handleMouseMotion(tea.MouseMotionMsg{X: handleX, Y: 5})
	settleGrip(t, p, ar)

	p.handleMouseClick(tea.MouseClickMsg{X: handleX, Y: 5, Button: tea.MouseLeft})
	require.True(t, p.isDraggingSidebar)
	assert.True(t, p.sidebarGrip.reveal.Dragging())
	active := strings.Split(p.renderSidebarHandle(p.computeSidebarLayout().chatHeight), "\n")
	top, length := grip.Rows(p.computeSidebarLayout().chatHeight)
	r, g, b := styles.ColorToRGB(styles.Resize)
	darker := styles.ResizeHandleStyle.Foreground(styles.RGBToColor(r*0.7, g*0.7, b*0.7)).Render("│")
	assert.Equal(t, darker, active[top+length/2], "dragging darkens the compact grip")

	// Drag far off the original column: the sidebar resizes and the grip stays.
	p.handleMouseMotion(tea.MouseMotionMsg{X: handleX - 10, Y: 5, Button: tea.MouseLeft})
	assert.NotEqual(t, startWidth, p.presentationSidebarSettings().PreferredWidth, "drag resizing is preserved")
	assert.InDelta(t, 1, p.sidebarGrip.reveal.Visibility(), 0)
	assert.False(t, p.sidebarGrip.reveal.Animating())
	assert.Equal(t, gripDimMessages, p.sidebarGrip.dimmed)

	// Release on the (moved) edge keeps it visible; release elsewhere fades out.
	p.handleMouseRelease(tea.MouseReleaseMsg{X: gripHandleX(p), Y: 5, Button: tea.MouseLeft})
	assert.False(t, p.sidebarGrip.reveal.Dragging())
	assert.True(t, p.sidebarGrip.reveal.Hovered())
	assert.InDelta(t, 1, p.sidebarGrip.reveal.Visibility(), 0)
	assert.False(t, p.sidebarGrip.reveal.Animating())

	p.handleMouseClick(tea.MouseClickMsg{X: gripHandleX(p), Y: 5, Button: tea.MouseLeft})
	p.handleMouseRelease(tea.MouseReleaseMsg{X: styles.AppPadding + 2, Y: 5, Button: tea.MouseLeft})
	require.True(t, p.sidebarGrip.reveal.Animating())
	settleGrip(t, p, ar)
	assert.Zero(t, p.sidebarGrip.reveal.Visibility())
	assert.Equal(t, gripDimNone, p.sidebarGrip.dimmed)
}

func TestSidebarGripFollowsDockSwapAndLeavesNarrowLayout(t *testing.T) {
	t.Parallel()
	p, ar := newGripTestPage(t, msgtypes.SidebarRight)
	p.handleMouseMotion(tea.MouseMotionMsg{X: gripHandleX(p), Y: 5})
	settleGrip(t, p, ar)
	require.Equal(t, gripDimMessages, p.sidebarGrip.dimmed)

	messagesGen := p.messages.VisualGeneration()
	p.layoutSettings.SidebarPosition = msgtypes.SidebarLeft
	p.SetSize(p.width, p.height)
	assert.Equal(t, gripDimSidebar, p.sidebarGrip.dimmed, "dimming moves with the dock")
	assert.Greater(t, p.messages.VisualGeneration(), messagesGen, "the old neighbour's scrollbar is restored")

	sidebarGen := p.sidebar.VisualGeneration()
	p.SetSize(minWindowWidth-1, p.height)
	require.NotEqual(t, sidebarVertical, p.computeSidebarLayout().mode)
	assert.Equal(t, gripDimNone, p.sidebarGrip.dimmed, "no edge column, no dimmed scrollbar")
	assert.Zero(t, p.sidebarGrip.reveal.Visibility())
	assert.False(t, p.sidebarGrip.reveal.Hovered())
	assert.Greater(t, p.sidebar.VisualGeneration(), sidebarGen)
	assert.EqualValues(t, 0, ar.ActiveCount())

	p.SetSize(160, p.height)
	assert.Zero(t, gripDots(t, p), "returning to the wide layout starts hidden")
	assert.False(t, p.sidebarGripAt(styles.AppPadding, 5))
}

func TestSidebarGripResetsWhenHiddenOrLeftViaRoot(t *testing.T) {
	t.Parallel()
	p, ar := newGripTestPage(t, msgtypes.SidebarRight)
	p.handleMouseMotion(tea.MouseMotionMsg{X: gripHandleX(p), Y: 5})
	settleGrip(t, p, ar)

	ClearSidebarEdgeHover(p)
	require.True(t, p.sidebarGrip.reveal.Animating(), "pointer left the content region: fade out")
	settleGrip(t, p, ar)
	assert.Equal(t, gripDimNone, p.sidebarGrip.dimmed)

	p.handleMouseMotion(tea.MouseMotionMsg{X: gripHandleX(p), Y: 5})
	gripTick(t, p, ar)
	require.Greater(t, p.sidebarGrip.reveal.Visibility(), 0.0)
	p.SetPresentationVisible(false)
	assert.Zero(t, p.sidebarGrip.reveal.Visibility(), "hidden presentation snaps the grip away")
	assert.False(t, p.sidebarGrip.reveal.Animating())
	assert.Equal(t, gripDimNone, p.sidebarGrip.dimmed)
	assert.Nil(t, ar.Continue())
}

func TestSidebarGripIgnoresPointerDuringScrollbarCapture(t *testing.T) {
	t.Parallel()
	p, _ := newGripTestPage(t, msgtypes.SidebarRight)
	p.sidebarGrip = newSidebarGripState(nil)
	sl := p.computeSidebarLayout()
	scrollbarX := styles.AppPadding + sl.sidebarStartX + sl.sidebarWidth - toggleColumnWidth - 1
	p.handleMouseClick(tea.MouseClickMsg{X: scrollbarX, Y: 0, Button: tea.MouseLeft})
	require.True(t, p.sidebar.IsScrollbarDragging())
	p.handleMouseMotion(tea.MouseMotionMsg{X: gripHandleX(p), Y: 5, Button: tea.MouseLeft})
	assert.Zero(t, p.sidebarGrip.reveal.Visibility(), "a captured scrollbar drag never reveals the grip")
	assert.True(t, p.sidebar.IsScrollbarDragging())
}
