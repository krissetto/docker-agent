package messagebar

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type noticeScheduler struct{ now time.Time }

func (s *noticeScheduler) Now() time.Time { return s.now }

func (s *noticeScheduler) Tick(_ time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { return create(s.now) }
}

func acceptNoticeTick(t *testing.T, ar *animation.Runtime, scheduler *noticeScheduler, cmd tea.Cmd, elapsed time.Duration) animation.TickMsg {
	t.Helper()
	require.NotNil(t, cmd)
	scheduler.now = scheduler.now.Add(elapsed)
	msg, ok := cmd().(animation.TickMsg)
	require.True(t, ok)
	accepted, ok := ar.Accept(msg)
	require.True(t, ok)
	return accepted
}

func TestNoticeEntryExitAndIdleLease(t *testing.T) {
	scheduler := &noticeScheduler{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	m := NewWithRuntime(ar)
	m.SetSize(80, 1)
	assert.Equal(t, strings.Repeat(" ", 80), ansi.Strip(m.View()))
	assert.Zero(t, ar.ActiveCount())
	cmd := m.SetMessage(Message{Text: "Press Esc again to cancel the response.", Severity: Warning})
	require.NotNil(t, cmd)
	assert.EqualValues(t, 1, ar.ActiveCount())
	assert.Zero(t, m.alpha)
	initial := m.View()
	m.TakeVisualDirty()
	tick := acceptNoticeTick(t, ar, scheduler, cmd, fadeDuration/2)
	assert.Nil(t, m.Update(tick))
	assert.True(t, tick.Dirty())
	assert.InDelta(t, 0.5, m.alpha, 0)
	assert.Equal(t, ansi.Strip(initial), ansi.Strip(m.View()), "fade must not move any cells")
	assert.Equal(t, 1, m.Height())
	assert.Equal(t, 80, ansi.StringWidth(m.View()))
	tick = acceptNoticeTick(t, ar, scheduler, ar.Continue(), fadeDuration/2)
	assert.Nil(t, m.Update(tick))
	assert.InDelta(t, 1.0, m.alpha, 0)
	assert.Equal(t, rgba(styles.Warning), rgba(foregroundAt(t, m.View(), "P")))
	assert.Zero(t, ar.ActiveCount(), "displayed notices settle to zero leases")
	assert.Nil(t, ar.Continue())
	m.TakeVisualDirty()
	assert.Nil(t, m.Update(tick))
	assert.False(t, m.TakeVisualDirty(), "settled tick must not repaint")
	cmd = m.ClearMessage()
	require.NotNil(t, cmd)
	assert.EqualValues(t, 1, ar.ActiveCount())
	assert.Nil(t, m.ClearMessage(), "repeated clearing must not restart exit")
	tick = acceptNoticeTick(t, ar, scheduler, cmd, fadeDuration/2)
	assert.Nil(t, m.Update(tick))
	assert.InDelta(t, 0.5, m.alpha, 0)
	tick = acceptNoticeTick(t, ar, scheduler, ar.Continue(), fadeDuration/2)
	assert.Nil(t, m.Update(tick))
	assert.Zero(t, ar.ActiveCount())
	assert.Empty(t, m.message.Text)
	assert.Equal(t, strings.Repeat(" ", 80), ansi.Strip(m.View()))
	assert.Equal(t, 1, m.Height())
}

func TestReplacementReusesLeaseAndClearingDisablesActions(t *testing.T) {
	scheduler := &noticeScheduler{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	m := NewWithRuntime(ar)
	m.SetSize(80, 1)
	cmd := m.SetMessage(Message{Text: "Old"})
	assert.Nil(t, m.SetMessage(notice()))
	assert.EqualValues(t, 1, ar.ActiveCount(), "replacement reuses the existing lease")
	tick := acceptNoticeTick(t, ar, scheduler, cmd, fadeDuration)
	assert.Nil(t, m.Update(tick))
	require.True(t, m.HasActions())
	bounds := m.bounds[0]
	m.SetFocused(true)
	cmd = m.ClearMessage()
	assert.False(t, m.HasActions())
	assert.False(t, m.Focused())
	_, hit := m.HitTest(bounds.start, 0)
	assert.False(t, hit)
	assert.Nil(t, m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: bounds.start, Y: 0}))
	assert.Nil(t, m.SetMessage(Message{Text: "Replacement", Severity: Success}))
	tick = acceptNoticeTick(t, ar, scheduler, cmd, fadeDuration)
	assert.Nil(t, m.Update(tick))
	assert.Equal(t, "Replacement", m.message.Text, "old exit cannot clear a replacement")
	assert.Zero(t, ar.ActiveCount())
}

func TestNoticeHideAndTeardownReleaseImmediately(t *testing.T) {
	scheduler := &noticeScheduler{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	m := NewWithRuntime(ar)
	m.SetSize(80, 1)
	cmd := m.SetMessage(Message{Text: "Working"})
	m.SetSize(80, 0)
	assert.Zero(t, ar.ActiveCount())
	assert.Empty(t, m.message.Text)
	queued, ok := cmd().(animation.TickMsg)
	require.True(t, ok)
	_, accepted := ar.Accept(queued)
	assert.False(t, accepted)
	m.SetSize(80, 1)
	m.SetMessage(Message{Text: "Working"})
	m.StopAnimation()
	m.StopAnimations()
	assert.Zero(t, ar.ActiveCount())
	assert.Empty(t, m.message.Text)
	assert.Nil(t, ar.Continue())
}

func TestSeverityUsesSemanticForeground(t *testing.T) {
	for _, severity := range []Severity{Info, Warning, Error, Success} {
		m := New()
		m.SetSize(80, 1)
		assert.Nil(t, m.SetMessage(Message{Text: "Notice", Severity: severity}))
		assert.Equal(t, rgba(m.severityColor()), rgba(foregroundAt(t, m.View(), "N")))
	}
}
