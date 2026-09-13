package scrollview

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestVisualGenerationTracksEffectivePresentationChanges(t *testing.T) {
	t.Parallel()

	m := New()
	generation := m.VisualGeneration()
	require.False(t, m.Changed(generation))

	m.SetSize(10, 4)
	require.True(t, m.Changed(generation))
	generation = m.VisualGeneration()
	m.SetSize(10, 4)
	require.False(t, m.Changed(generation), "identical size changed generation")

	content := []string{"0", "1", "2", "3", "4", "5", "6", "7"}
	m.SetContent(content, len(content))
	require.True(t, m.Changed(generation))
	generation = m.VisualGeneration()
	m.SetContent(append([]string(nil), content...), len(content))
	require.False(t, m.Changed(generation), "equivalent content changed generation")

	m.SetScrollOffset(1)
	require.True(t, m.Changed(generation))
	generation = m.VisualGeneration()
	m.SetScrollOffset(1)
	require.False(t, m.Changed(generation), "identical offset changed generation")

	m.SetScrollOffset(99)
	generation = m.VisualGeneration()
	m.SetSize(10, 6)
	require.True(t, m.Changed(generation), "size and clamp did not change generation")
	assert.Equal(t, 2, m.ScrollOffset())
}

func TestVisualGenerationIgnoresBoundaryInputAndTracksDrag(t *testing.T) {
	t.Parallel()

	m := New()
	m.SetSize(10, 4)
	content := []string{"0", "1", "2", "3", "4", "5", "6", "7"}
	m.SetContent(content, len(content))

	generation := m.VisualGeneration()
	_, _ = m.Update(messages.WheelCoalescedMsg{Delta: -1})
	require.False(t, m.Changed(generation), "top-boundary wheel changed generation")
	_, _ = m.Update(tea.MouseMotionMsg{X: 9, Y: 2})
	require.False(t, m.Changed(generation), "motion without a drag changed generation")

	_, _ = m.Update(tea.MouseClickMsg{X: 9, Y: 0, Button: tea.MouseLeft})
	require.True(t, m.Changed(generation), "starting a drag did not change generation")
	generation = m.VisualGeneration()
	_, _ = m.Update(tea.MouseMotionMsg{X: 9, Y: 0})
	require.False(t, m.Changed(generation), "no-op drag motion changed generation")
	_, _ = m.Update(tea.MouseMotionMsg{X: 9, Y: 2})
	require.True(t, m.Changed(generation), "effective drag motion did not change generation")

	generation = m.VisualGeneration()
	_, _ = m.Update(tea.MouseReleaseMsg{X: 9, Y: 2, Button: tea.MouseLeft})
	require.True(t, m.Changed(generation), "ending a drag did not change generation")
}

func TestVisualGenerationIgnoresOffscreenContentAndIdleViews(t *testing.T) {
	m := New()
	m.SetSize(20, 4)
	content := make([]string, 10000)
	for i := range content {
		content[i] = "line"
	}
	m.SetContent(content, len(content))
	generation := m.VisualGeneration()
	updated := append([]string(nil), content...)
	updated[len(updated)-1] = "offscreen change"
	m.SetContent(updated, len(updated))
	assert.False(t, m.Changed(generation), "offscreen bytes do not change the viewport")
	for range 10 {
		_ = m.View()
		m.SetContent(updated, len(updated))
		m.SetPosition(10, 20)
	}
	assert.False(t, m.Changed(generation), "idle frames and hit-position updates do not invalidate rendering")

	updated = append([]string(nil), updated...)
	updated[0] = "visible change"
	m.SetContent(updated, len(updated))
	assert.True(t, m.Changed(generation))
	generation = m.VisualGeneration()
	updated[1] = "in-place edit"
	m.InvalidateComposeCache()
	assert.True(t, m.Changed(generation))
}
