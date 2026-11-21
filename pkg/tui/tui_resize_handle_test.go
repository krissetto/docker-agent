package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/internal/resizegrip"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// TestRenderResizeHandle_TinyWidths guards against a panic (negative
// strings.Repeat count) when the terminal reports degenerate sizes such as
// 0x0 or 1x1: the padded inner width goes negative for widths smaller than
// the app's horizontal padding.
func TestRenderResizeHandle_TinyWidths(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t)

	for _, width := range []int{-1, 0, 1, appPaddingHorizontal, appPaddingHorizontal + 1, 80} {
		out := m.renderResizeHandle(width)
		if width <= appPaddingHorizontal {
			assert.Empty(t, out, "width %d", width)
		} else {
			assert.Equal(t, strings.Repeat(" ", width), out, "idle divider at width %d", width)
		}
	}
}

// TestRenderResizeHandle_SuffixNeverOverflows pins that a status suffix wider
// than the handle line is truncated instead of overflowing the row on narrow
// terminals.
func TestRenderResizeHandle_SuffixNeverOverflows(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t)
	m.activeTab.sessionState = &service.SessionState{}
	m.activeTab.sessionState.SetPauseState(service.PausePaused)

	for _, width := range []int{5, 10, 20, 80, 200} {
		out := m.renderResizeHandle(width)
		assert.LessOrEqual(t, lipgloss.Width(out), width, "width %d", width)
	}
}

func TestLineWithSuffix(t *testing.T) {
	t.Parallel()

	// The suffix fits: the line is truncated to make room.
	out := lineWithSuffix("──────────", " ok", 8)
	assert.Equal(t, "───── ok", out)
	assert.Equal(t, 8, lipgloss.Width(out))

	// The suffix alone is too wide: it is truncated and the line dropped.
	out = lineWithSuffix("──────────", " a very long status", 8)
	assert.Equal(t, 8, lipgloss.Width(out))
}

func TestComposerGripKeepsFullWidthResizeHitArea(t *testing.T) {
	m, _, _ := harnessRoot(t, 160, 40, animation.NewRuntimeWithScheduler(frozenScheduler{}))
	// Snap to hover colors here; timed transitions are covered by resizegrip's tests.
	m.composerGrip = resizegrip.Reveal{}

	rest := m.renderResizeHandle(m.width)
	require.Equal(t, strings.Repeat(" ", m.width), rest)
	start := (m.width - resizeHandleWidth) / 2
	segment := strings.Repeat("━", resizeHandleWidth)
	visible := strings.Repeat(" ", start) + segment + strings.Repeat(" ", m.width-start-resizeHandleWidth)

	row := m.contentHeight
	_, _ = m.Update(tea.MouseMotionMsg{X: start - 1, Y: row})
	edge := m.renderResizeHandle(m.width)
	assert.Equal(t, visible, ansi.Strip(edge), "reveal only the compact segment, not a full-width rule")
	_, _ = m.Update(tea.MouseMotionMsg{X: start, Y: row})
	hover := m.renderResizeHandle(m.width)
	assert.Contains(t, hover, styles.ResizeHandleHoverStyle.Render(strings.Repeat("━", resizeHandleWidth)))
	assert.Equal(t, visible, ansi.Strip(hover))

	_, _ = m.Update(tea.MouseClickMsg{X: 0, Y: row, Button: tea.MouseLeft})
	require.True(t, m.isDragging, "the full row, including its padded edge, remains draggable")
	assert.Contains(t, m.renderResizeHandle(m.width), styles.ResizeHandleActiveStyle.Render(strings.Repeat("━", resizeHandleWidth)))
	_, _ = m.Update(tea.MouseMotionMsg{X: 0, Y: row - 3, Button: tea.MouseLeft})
	assert.Less(t, m.contentHeight, row)
	_, _ = m.Update(tea.MouseReleaseMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	assert.False(t, m.isDragging)
	assert.Equal(t, rest, m.renderResizeHandle(m.width))
}

// Exercise the actual rendered divider, not just the editor's row count.
func TestComposerDragKeepsGripUnderPointer(t *testing.T) {
	for _, tc := range []struct {
		name string
		tabs []messages.TabInfo
	}{
		{"single", []messages.TabInfo{{SessionID: "profile", Title: "One"}}},
		{"tab strip", []messages.TabInfo{{SessionID: "profile", Title: "One"}, {SessionID: "second", Title: "Two"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _ := frozenClockRoot(t, 160, 40)
			m.tabBar.SetTabs(tc.tabs, 0)
			m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
			x, initial := m.width/2, m.contentHeight
			m.Update(tea.MouseClickMsg{X: x, Y: initial, Button: tea.MouseLeft})
			require.True(t, m.isDragging)
			move := func(y int) {
				t.Helper()
				m.Update(tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft})
			}
			at := func(y int) {
				t.Helper()
				require.Equal(t, regionResizeHandle, m.hitTestRegion(y), "pointer row %d", y)
				lines := strings.Split(ansi.Strip(m.View().Content), "\n")
				require.Contains(t, lines[y], strings.Repeat("━", resizeHandleWidth), "painted row %d", y)
			}
			for _, dy := range []int{0, -1, -4, -4, -2, 0} {
				move(initial + dy)
				at(initial + dy)
			}
			move(0) // Clamp, then move back into the available range without drift.
			require.Positive(t, m.contentHeight)
			move(initial - 3)
			m.Update(tea.MouseReleaseMsg{X: x, Y: initial - 3, Button: tea.MouseLeft})
			require.False(t, m.isDragging)
			at(initial - 3)
		})
	}
}

func TestComposerGripDoesNotHighlightClippedStatusSuffix(t *testing.T) {
	t.Parallel()
	m, _ := newTestModel(t)
	m.width = 20
	m.activeTab.sessionState = &service.SessionState{}
	m.activeTab.sessionState.SetPauseState(service.PausePaused)
	assert.Contains(t, ansi.Strip(m.renderResizeHandle(m.width)), "Paused", "status stays visible on a blank divider")
	m.updateComposerGrip(m.width/2, regionResizeHandle)
	assert.Equal(t, styles.ResizeHandleStyle.Render("━"), m.composerGrip.Render("━", 1))
	assert.NotContains(t, ansi.Strip(m.renderResizeHandle(m.width)), "━")
}

func TestSidebarGripFadesWhenPointerLeavesChatAndReleasesOutside(t *testing.T) {
	ar := animation.NewRuntimeWithScheduler(&rootImmediateScheduler{now: time.Unix(1, 0)})
	m, _, _ := harnessRoot(t, 160, 40, ar)
	p := chat.New(ar, t.Context(), m.application, m.activeTab.sessionState)
	m.activeTab.chatPage = p
	t.Cleanup(func() { chat.Cleanup(p) })
	_, _ = m.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})

	rest := m.View().Content
	require.NotContains(t, ansi.Strip(rest), "┃", "no grip glyph at rest")
	// The toggle marks the same divider column even when the grip is invisible.
	first, _, _ := strings.Cut(ansi.Strip(rest), "\n")
	before, _, found := strings.Cut(first, "»")
	require.True(t, found, "right sidebar toggle in first row")
	x, y := lipgloss.Width(before), m.contentHeight/2
	advance := func(cmd tea.Cmd) {
		t.Helper()
		for i := 0; ar.HasActive(); i++ {
			require.Less(t, i, 3)
			var next tea.Cmd
			for _, msg := range collectMsgs(cmd) {
				if tick, ok := msg.(animation.TickMsg); ok {
					_, next = m.Update(tick)
				}
			}
			cmd = next
		}
	}

	_, cmd := m.Update(tea.MouseMotionMsg{X: x, Y: y})
	require.True(t, ar.HasActive())
	advance(cmd)
	assert.Contains(t, ansi.Strip(m.View().Content), "┃", "only revealed while pointer is near")
	_, cmd = m.Update(tea.MouseMotionMsg{X: x, Y: m.height - 1})
	require.True(t, ar.HasActive())
	advance(cmd)
	assert.Equal(t, rest, m.View().Content, "sidebar must fade without another motion inside chat")

	_, _ = m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.True(t, chat.IsResizingSidebar(p))
	_, cmd = m.Update(tea.MouseReleaseMsg{X: x, Y: m.height - 1, Button: tea.MouseLeft})
	assert.False(t, chat.IsResizingSidebar(p))
	advance(cmd)
	assert.Equal(t, rest, m.View().Content)
}
