package messages

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func newWidthGuardModel(t *testing.T, width, count int) (*animation.Runtime, *model) {
	t.Helper()
	ar := animation.NewRuntimeWithScheduler(&referenceScheduler{now: time.Unix(1, 0), step: 50 * time.Millisecond})
	m := newModel(ar, width, 12, &service.SessionState{})
	t.Cleanup(m.StopAnimations)
	m.SetScrollbarsActive(true)
	for i := range count {
		addWidthGuardMessage(m, i)
	}
	return ar, m
}

func addWidthGuardMessage(m *model, i int) {
	position := i
	msg := &types.Message{
		Type:            types.MessageTypeUser,
		Content:         fmt.Sprintf("history %d %s", i, strings.Repeat("wrapping words ", 10)),
		SessionPosition: &position,
	}
	m.messages = append(m.messages, msg)
	m.views = append(m.views, m.createMessageView(msg))
}

// Auto-follow through a narrower resize grows the transcript; that follow is layout.
func TestAutoFollowWidthShrinkRewrapStaysSilent(t *testing.T) {
	ar, m := newWidthGuardModel(t, 80, 40)
	_ = m.View()
	settleWidthRewrapReveal(t, ar, m)
	require.False(t, m.userHasScrolled)
	before := m.totalScrollableHeight()

	m.SetSize(40, 12)
	require.Zero(t, ar.ActiveCount(), "resize must not reveal")
	_ = m.View()
	require.Greater(t, m.totalScrollableHeight(), before, "narrower rewrap must grow the transcript")
	require.Equal(t, m.totalScrollableHeight()-m.height, m.scrollOffset, "auto-follow stays at the bottom")
	require.Zero(t, ar.ActiveCount(), "auto-follow growth from a resize must not reveal")
	require.False(t, m.scrollview.AnimationActive())
}

// Auto-follow through a wider resize shrinks the transcript; that follow is layout too.
func TestAutoFollowWidthGrowRewrapStaysSilent(t *testing.T) {
	ar, m := newWidthGuardModel(t, 40, 40)
	_ = m.View()
	settleWidthRewrapReveal(t, ar, m)
	require.False(t, m.userHasScrolled)
	before := m.totalScrollableHeight()

	m.SetSize(80, 12)
	require.Zero(t, ar.ActiveCount(), "resize must not reveal")
	_ = m.View()
	require.Less(t, m.totalScrollableHeight(), before, "wider rewrap must shrink the transcript")
	require.Equal(t, m.totalScrollableHeight()-m.height, m.scrollOffset, "auto-follow stays at the bottom")
	require.Zero(t, ar.ActiveCount(), "auto-follow shrink from a resize must not reveal")
	require.False(t, m.scrollview.AnimationActive())
}

// A resize with no transcript must not leave a pending layout sync for later content.
func TestResizeOfEmptyTranscriptDoesNotSilenceLaterAutoFollow(t *testing.T) {
	ar, m := newWidthGuardModel(t, 80, 0)
	m.SetSize(40, 12)
	require.Zero(t, ar.ActiveCount())
	for i := range 40 {
		addWidthGuardMessage(m, i)
	}
	_ = m.View()
	require.EqualValues(t, 1, ar.ActiveCount(), "auto-follow growth after an empty resize reveals")
}

// Content shrinks outside a resize keep revealing through the normal sync.
func TestNonResizeContentShrinkStillReveals(t *testing.T) {
	ar, m := newWidthGuardModel(t, 40, 40)
	_ = m.View()
	settleWidthRewrapReveal(t, ar, m)
	m.userHasScrolled = true
	_ = m.View()
	settleWidthRewrapReveal(t, ar, m)

	m.messages = m.messages[:20]
	m.views = m.views[:20]
	m.invalidateAllItems()
	_ = m.View()
	require.EqualValues(t, 1, ar.ActiveCount(), "content shrink clamp outside a resize still reveals")
}
