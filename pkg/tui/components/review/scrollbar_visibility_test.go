package review

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
)

type reviewVisibilityClock struct{ now time.Time }

func (c *reviewVisibilityClock) Now() time.Time { return c.now }
func (c *reviewVisibilityClock) Tick(_ time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		c.now = c.now.Add(50 * time.Millisecond)
		return create(c.now)
	}
}

func TestScrollbarVisibilityFollowsVisibleReviewPanel(t *testing.T) {
	m := New("review")
	m.SetSize(120, 24)
	g := m.geometry()
	require.True(t, g.showNav)
	m.navSV.SetPosition(1000, 1000)
	navX, navY := m.navSV.ScrollbarX(), 1001
	clock := &reviewVisibilityClock{now: time.Unix(1, 0)}
	runtime := animation.NewRuntimeWithScheduler(clock)
	m.SetAnimationRuntime(runtime)
	m.navSV.SetContent(nil, 100)
	m.readSV.SetContent(nil, 100)
	m.SetInspector(true)
	g = m.geometry()
	require.True(t, g.showRead)
	m.readSV.SetPosition(m.x+g.readerX, m.y+g.readerBodyY)
	m.readSV.SetSize(g.readerW, g.readerBodyH)
	m.readSV.SetContent(nil, 100)
	require.False(t, m.SetScrollbarPointer(navX, navY), "hidden navigator ignores pointer proximity")
	require.Zero(t, runtime.ActiveCount())
	require.True(t, m.SetScrollbarPointer(m.readSV.ScrollbarX(), m.y+g.readerBodyY), "visible reader accepts pointer proximity")
	require.EqualValues(t, 1, runtime.ActiveCount())

	m.SetScrollbarActive(false)
	require.Zero(t, runtime.ActiveCount())
	require.False(t, m.SetScrollbarPointer(m.readSV.ScrollbarX(), m.y+g.readerBodyY), "occlusion suppresses pointer state")
	require.Zero(t, runtime.ActiveCount())
	m.SetScrollbarActive(true)
	require.True(t, m.SetScrollbarPointer(m.readSV.ScrollbarX(), m.y+g.readerBodyY), "visible reader resumes")
	require.EqualValues(t, 1, runtime.ActiveCount())
	m.StopAnimation()
	require.Zero(t, runtime.ActiveCount())
}
