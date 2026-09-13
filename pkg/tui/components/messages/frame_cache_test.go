package messages

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestTranscriptFrameCacheRefreshSameBackingArray(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 30, 3, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	view := &countedHistoryView{content: "short"}
	m.messages = append(m.messages, types.User(view.content))
	m.views = append(m.views, view)
	first := m.View()
	key := m.lastFrameKey
	require.Equal(t, first, m.View())
	require.Equal(t, key, m.lastFrameKey)
	require.Equal(t, 1, view.renders)
	before := &m.renderedLines[0]
	view.content = "a much longer replacement"
	require.True(t, m.refreshRenderedItem(0))
	require.Equal(t, before, &m.renderedLines[0], "exercise target's in-place refresh, not fresh-slice donor")
	frame := m.View()
	require.NotEqual(t, first, frame)
	require.Contains(t, ansi.Strip(frame), view.content)
	for line := range strings.SplitSeq(frame, "\n") {
		require.Equal(t, 30, ansi.StringWidth(line))
	}
	require.Equal(t, frame, m.View())
	require.NotEqual(t, key, m.lastFrameKey)
}

func TestTranscriptFrameCacheTracksSelectionResizeAndInvalidation(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 30, 3, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	m.AddUserMessage("first line\nsecond line\nthird line\nfourth line")
	first := m.View()
	key := m.lastFrameKey
	m.selection.active = true
	m.selection.startLine, m.selection.endLine = m.scrollOffset, m.scrollOffset
	m.selection.startCol, m.selection.endCol = 0, 4
	require.NotEqual(t, first, m.View())
	require.NotEqual(t, key, m.lastFrameKey)
	m.selection.active = false
	m.SetSize(20, 4)
	frame := m.View()
	for line := range strings.SplitSeq(frame, "\n") {
		require.Equal(t, 20, ansi.StringWidth(line))
	}
	key = m.lastFrameKey
	m.InvalidateRenderCaches()
	require.Empty(t, m.lastFrameOutput)
	_ = m.View()
	require.NotEqual(t, key, m.lastFrameKey)
	require.Equal(t, m.totalHeight, m.RenderedContentHeight())
}

func TestTranscriptFrameCacheScrollbarSameOffsetPressRelease(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 30, 5, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	addShrinkingView(m, 30)
	_ = m.View()
	m.scrollOffset, m.userHasScrolled = 0, true
	m.scrollview.SetScrollOffset(0)
	before := m.View()
	x := m.scrollview.ScrollbarX()
	_, _ = m.Update(tea.MouseClickMsg{X: x, Y: 0, Button: tea.MouseLeft})
	require.True(t, m.IsScrollbarDragging())
	require.Zero(t, m.scrollOffset)
	pressed := m.View()
	require.NotEqual(t, before, pressed, "thumb active style must bypass identical content/offset frame")
	_, _ = m.Update(tea.MouseReleaseMsg{X: x, Y: 0, Button: tea.MouseLeft})
	require.False(t, m.IsScrollbarDragging())
	require.Zero(t, m.scrollOffset)
	require.Equal(t, before, m.View(), "release restores inactive thumb without scrolling")
}
