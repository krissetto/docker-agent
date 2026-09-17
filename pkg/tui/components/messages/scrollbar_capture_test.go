package messages

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestScrollbarCaptureClampsOutsideViewportAndIgnoresOtherRelease(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 30, 5, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	addShrinkingView(m, 30)
	_ = m.View()
	m.scrollOffset, m.userHasScrolled = 0, true
	m.scrollview.SetScrollOffset(0)
	_ = m.View()
	_, _ = m.Update(tea.MouseClickMsg{X: m.scrollview.ScrollbarX(), Y: 0, Button: tea.MouseLeft})
	require.True(t, m.IsScrollbarDragging())
	_, _ = m.Update(tea.MouseMotionMsg{X: -1000, Y: 1000, Button: tea.MouseLeft})
	require.Equal(t, m.totalHeight-m.height, m.scrollOffset)
	_, _ = m.Update(tea.MouseReleaseMsg{X: -1000, Y: 1000, Button: tea.MouseRight})
	require.True(t, m.IsScrollbarDragging(), "right release cannot end left capture")
	_, _ = m.Update(tea.MouseMotionMsg{X: 1000, Y: -1000, Button: tea.MouseLeft})
	require.Zero(t, m.scrollOffset)
	_, _ = m.Update(tea.MouseReleaseMsg{X: 1000, Y: -1000, Button: tea.MouseLeft})
	require.False(t, m.IsScrollbarDragging())
	require.Zero(t, m.scrollOffset)
}

func TestScrollbarOwnedReleaseAndCancelPreserveSelection(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		m := NewScrollableView(animation.NewRuntime(), 30, 5, &service.SessionState{}).(*model)
		t.Cleanup(m.StopAnimations)
		addShrinkingView(m, 30)
		_ = m.View()
		m.scrollOffset, m.userHasScrolled = 0, true
		m.scrollview.SetScrollOffset(0)
		_ = m.View()
		_, _ = m.Update(tea.MouseClickMsg{X: m.scrollview.ScrollbarX(), Y: 0, Button: tea.MouseLeft})
		require.True(t, m.IsScrollbarDragging())
		// An independent pending selection must not be completed by the
		// scrollbar's release or by a cancellation fence.
		m.selection.start(1, 2)
		m.selection.update(3, 4)
		m.selection.pendingCopyID = 42
		selection := m.selection
		renderDirty, offset := m.renderDirty, m.scrollOffset
		if cancel {
			m.CancelScrollbarDrag()
			m.CancelScrollbarDrag()
		} else {
			_, cmd := m.Update(tea.MouseReleaseMsg{X: -100, Y: -100, Button: tea.MouseLeft})
			require.Nil(t, cmd, "no copy/open URL command after scrollbar release")
		}
		require.False(t, m.IsScrollbarDragging())
		require.Equal(t, selection, m.selection)
		require.Equal(t, renderDirty, m.renderDirty)
		require.Equal(t, offset, m.scrollOffset)
	}
}
