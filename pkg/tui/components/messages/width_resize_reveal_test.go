package messages

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// settleWidthRewrapReveal drains the finite scrollbar reveal/fade lease.
func settleWidthRewrapReveal(t *testing.T, ar *animation.Runtime, m *model) {
	t.Helper()
	for range 60 {
		if ar.ActiveCount() == 0 {
			break
		}
		tick, ok := ar.Accept(ar.Continue()().(animation.TickMsg))
		require.True(t, ok)
		_, _ = m.Update(tick)
	}
	require.Zero(t, ar.ActiveCount())
}

// A rewrap that shortens a user-scrolled transcript clamps the bottom offset;
// the clamp must not reveal, but the next real scroll must.
func TestWidthResizeRewrapClampStaysSilentAndRealScrollReveals(t *testing.T) {
	ar := animation.NewRuntimeWithScheduler(&referenceScheduler{now: time.Unix(1, 0), step: 50 * time.Millisecond})
	m := newModel(ar, 40, 12, &service.SessionState{})
	t.Cleanup(m.StopAnimations)
	m.SetScrollbarsActive(true)
	for i := range 40 {
		position := i
		msg := &types.Message{
			Type:            types.MessageTypeUser,
			Content:         fmt.Sprintf("history %d %s", i, strings.Repeat("wrapping words ", 10)),
			SessionPosition: &position,
		}
		m.messages = append(m.messages, msg)
		m.views = append(m.views, m.createMessageView(msg))
	}
	m.View()
	settleWidthRewrapReveal(t, ar, m)

	m.userHasScrolled = true
	_ = m.View()
	settleWidthRewrapReveal(t, ar, m)
	require.Equal(t, m.totalScrollableHeight()-m.height, m.scrollOffset, "fixture must sit at the bottom")

	before := m.totalScrollableHeight()
	m.SetSize(80, 12)
	require.Zero(t, ar.ActiveCount(), "resize itself must not reveal")
	_ = m.View()
	require.Less(t, m.totalScrollableHeight(), before, "wider rewrap must shrink the transcript")
	require.Greater(t, m.totalScrollableHeight(), m.height, "transcript still overflows the viewport")
	require.Equal(t, m.totalScrollableHeight()-m.height, m.scrollOffset, "rewrap re-anchors to the new bottom")
	require.Zero(t, ar.ActiveCount(), "rewrap clamp must not reveal or schedule ticks")
	require.False(t, m.scrollview.AnimationActive())

	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	require.Zero(t, m.scrollOffset)
	require.EqualValues(t, 1, ar.ActiveCount(), "real scroll after the rewrap reveals")
}
