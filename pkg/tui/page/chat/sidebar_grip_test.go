package chat

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestSidebarGripPreservesResizeAndToggleTargets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		position  msgtypes.SidebarPosition
		toggle    string
		direction int
	}{
		{msgtypes.SidebarLeft, "«", 1},
		{msgtypes.SidebarRight, "»", -1},
	} {
		t.Run(string(tc.position), func(t *testing.T) {
			p := newLayoutTestPage(t, tc.position)
			p.SetSize(p.width, p.height)
			sl := p.computeSidebarLayout()
			x := styles.AppPadding + sl.handleX
			tester := NewHitTest(p)
			rest := p.renderSidebarHandle(sl.chatHeight)
			lines := strings.Split(ansi.Strip(rest), "\n")
			require.Len(t, lines, sl.chatHeight)
			assert.Equal(t, tc.toggle, lines[0])
			assert.Equal(t, TargetSidebarToggle, tester.At(x, 0))
			assert.Equal(t, TargetSidebarResizeHandle, tester.At(x, 1))
			mid := sl.chatHeight / 2
			assert.Equal(t, tc.toggle+strings.Repeat("\n ", sl.chatHeight-1), ansi.Strip(rest))

			_, _ = p.handleMouseMotion(tea.MouseMotionMsg{X: x, Y: 0})
			edge := p.renderSidebarHandle(sl.chatHeight)
			assert.NotEqual(t, rest, edge)
			assert.Contains(t, edge, styles.ResizeHandleStyle.Render("┃"))
			_, _ = p.handleMouseMotion(tea.MouseMotionMsg{X: x, Y: mid})
			assert.Contains(t, p.renderSidebarHandle(sl.chatHeight), styles.ResizeHandleHoverStyle.Render("┃"))

			width := p.sidebar.GetPreferredWidth()
			_, _ = p.handleMouseClick(tea.MouseClickMsg{X: x, Y: 1, Button: tea.MouseLeft})
			require.True(t, IsResizingSidebar(p), "resize can start outside the visible grip")
			assert.Contains(t, p.renderSidebarHandle(sl.chatHeight), styles.ResizeHandleActiveStyle.Render("┃"))
			_, _ = p.handleMouseMotion(tea.MouseMotionMsg{X: x + tc.direction, Y: 1, Button: tea.MouseLeft})
			assert.Equal(t, width, p.sidebar.GetPreferredWidth(), "below the resize threshold")
			_, _ = p.handleMouseMotion(tea.MouseMotionMsg{X: x - tc.direction*width, Y: 1, Button: tea.MouseLeft})
			assert.True(t, p.sidebar.IsCollapsed())
			assert.Equal(t, 0, p.sidebar.GetPreferredWidth())
			_, _ = p.handleMouseMotion(tea.MouseMotionMsg{X: x + tc.direction*dragThreshold, Y: 1, Button: tea.MouseLeft})
			assert.False(t, p.sidebar.IsCollapsed())
			assert.Equal(t, p.sidebar.ClampWidth(width+dragThreshold, sl.innerWidth), p.sidebar.GetPreferredWidth())
			assert.GreaterOrEqual(t, p.sidebar.GetPreferredWidth(), sidebar.MinWidth)
			_, _ = p.handleMouseRelease(tea.MouseReleaseMsg{X: 0, Y: p.height, Button: tea.MouseLeft})
			assert.False(t, IsResizingSidebar(p))
			assert.Equal(t, rest, p.renderSidebarHandle(sl.chatHeight))
		})
	}
}

func TestSidebarGripMarginRevealsWithoutHoverOrCapture(t *testing.T) {
	t.Parallel()
	for _, dock := range []msgtypes.SidebarPosition{msgtypes.SidebarLeft, msgtypes.SidebarRight} {
		t.Run(string(dock), func(t *testing.T) {
			p := newLayoutTestPage(t, dock)
			p.SetSize(p.width, p.height)
			sl := p.computeSidebarLayout()
			x, y := styles.AppPadding+sl.handleX, sl.chatHeight/2
			view := func() string { return p.renderSidebarHandle(sl.chatHeight) }
			rest := view()
			p.updateSidebarGrip(x, 0) // Divider, but not directly over the grip glyphs.
			revealed := view()
			require.NotEqual(t, rest, revealed)

			for _, direction := range []int{-1, 1} {
				p.updateSidebarGrip(x, y)
				require.Contains(t, view(), styles.ResizeHandleHoverStyle.Render("┃"))
				_, _ = p.handleMouseMotion(tea.MouseMotionMsg{X: x + direction, Y: y})
				require.Equal(t, revealed, view(), "neighbor reveals, but loses direct hover")
				hit := NewHitTest(p).At(x+direction, y)
				if sl.isInSidebar(sl.handleX + direction) {
					require.Equal(t, TargetSidebarContent, hit)
				} else {
					require.Equal(t, TargetMessages, hit)
				}
				_, _ = p.handleMouseClick(tea.MouseClickMsg{X: x + direction, Y: y, Button: tea.MouseLeft})
				require.False(t, IsResizingSidebar(p))
				_, _ = p.handleMouseRelease(tea.MouseReleaseMsg{X: x + direction, Y: y, Button: tea.MouseLeft})
				p.updateSidebarGrip(x+2*direction, y)
				require.Equal(t, rest, view(), "two cells away is outside the reveal zone")
				p.updateSidebarGrip(x+direction, y)
				require.Equal(t, revealed, view(), "entering from outside reveals the grip")
				p.updateSidebarGrip(x+direction, sl.chatHeight)
				require.Equal(t, rest, view(), "the reveal margin does not extend below chat")
			}
		})
	}
}

func TestSidebarGripLeavesToggleAloneOnShortColumn(t *testing.T) {
	t.Parallel()
	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	assert.Empty(t, p.renderSidebarHandle(0))
	assert.Equal(t, "»", ansi.Strip(p.renderSidebarHandle(1)))
	assert.Equal(t, "»\n ", ansi.Strip(p.renderSidebarHandle(2)))
}

func TestSidebarBandKeepsBlankSeparatorRow(t *testing.T) {
	t.Parallel()
	for _, dock := range []msgtypes.SidebarPosition{msgtypes.SidebarTop, msgtypes.SidebarBottom, msgtypes.SidebarRight} {
		t.Run(string(dock), func(t *testing.T) {
			p := newLayoutTestPage(t, dock)
			p.sidebar.SetCollapsed(true)
			p.SetSize(p.width, p.height)
			sl := p.computeSidebarLayout()
			lines := strings.Split(ansi.Strip(p.renderCollapsedSidebar(sl)), "\n")
			require.Len(t, lines, sl.sidebarHeight)
			y := sl.sidebarHeight - 1
			if sl.bandAtBottom {
				y = 0
			}
			require.Equal(t, strings.Repeat(" ", sl.innerWidth), lines[y])
		})
	}
}
