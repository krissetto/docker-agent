package messages

import (
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Keyboard scrolling right after a resize, before any render, must reveal the
// scrollbar, while the resize itself stays silent.
func TestKeyboardScrollAfterResizeBeforeRenderReveals(t *testing.T) {
	ar := animation.NewRuntimeWithScheduler(&referenceScheduler{now: time.Unix(1, 0), step: 50 * time.Millisecond})
	m := newModel(ar, 80, 12, &service.SessionState{})
	t.Cleanup(m.StopAnimations)
	// Bind the scrollbar the way the root does on hover updates; unbound views never reveal.
	m.SetScrollbarsActive(true)
	for i := range 40 {
		position := i
		msg := &types.Message{Type: types.MessageTypeUser, Content: fmt.Sprintf("history %d", i), SessionPosition: &position}
		m.messages = append(m.messages, msg)
		m.views = append(m.views, m.createMessageView(msg))
	}
	m.View()
	// Drain the reveal from the initial bottom follow.
	for range 60 {
		if ar.ActiveCount() == 0 {
			break
		}
		tick, ok := ar.Accept(ar.Continue()().(animation.TickMsg))
		require.True(t, ok)
		_, _ = m.Update(tick)
	}
	require.Zero(t, ar.ActiveCount())
	require.Positive(t, m.scrollOffset, "fixture must start scrolled to the bottom")

	m.SetSize(80, 10)
	require.Zero(t, ar.ActiveCount(), "resize must not reveal")
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	require.Zero(t, m.scrollOffset)
	require.EqualValues(t, 1, ar.ActiveCount(), "keyboard scroll before render reveals")
}
