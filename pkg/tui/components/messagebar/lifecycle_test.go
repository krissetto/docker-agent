package messagebar

import (
	"math"
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
	require.Nil(t, cmd)
	cmd = ar.Continue()
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
	require.Nil(t, cmd)
	cmd = ar.Continue()
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
	require.Nil(t, cmd)
	cmd = ar.Continue()
	assert.Nil(t, m.SetMessage(notice()))
	assert.EqualValues(t, 1, ar.ActiveCount(), "replacement reuses the existing lease")
	tick := acceptNoticeTick(t, ar, scheduler, cmd, fadeDuration)
	assert.Nil(t, m.Update(tick))
	require.True(t, m.HasActions())
	bounds := m.bounds[0]
	m.SetFocused(true)
	cmd = m.ClearMessage()
	require.Nil(t, cmd)
	cmd = ar.Continue()
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
	require.Nil(t, cmd)
	cmd = ar.Continue()
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

func TestSettledPresentationObservesAppliedLocalState(t *testing.T) {
	scheduler := &noticeScheduler{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	m := NewWithRuntime(ar)
	require.True(t, m.SettledPresentation(), "initial hidden row is settled")
	m.SetSize(80, 1)
	require.True(t, m.SettledPresentation(), "empty visible row is settled")
	cmd := m.SetMessage(Message{Text: "Notice"})
	require.Nil(t, cmd)
	cmd = ar.Continue()
	require.NotNil(t, cmd)
	m.TakeVisualDirty()
	view := m.View()
	require.False(t, m.SettledPresentation(), "entry is active before command execution")
	require.Equal(t, math.Float64bits(0), math.Float64bits(m.alpha))
	assert.Equal(t, view, m.View())
	assert.False(t, m.TakeVisualDirty(), "observation must not mutate presentation")
	tick := acceptNoticeTick(t, ar, scheduler, cmd, fadeDuration)
	require.False(t, m.SettledPresentation(), "elapsed entry needs its component Tick")
	require.Equal(t, math.Float64bits(0), math.Float64bits(m.alpha), "observation must not apply elapsed opacity")
	m.Update(tick)
	require.True(t, m.SettledPresentation(), "displayed hold is settled")
	other := ar.Transition()
	other.Start(time.Second, animation.Linear)
	require.True(t, m.SettledPresentation(), "other runtime leases are irrelevant")
	other.Cancel()
	cmd = m.ClearMessage()
	require.Nil(t, cmd)
	cmd = ar.Continue()
	require.NotNil(t, cmd)
	require.False(t, m.SettledPresentation(), "exit is unsettled immediately")
	tick = acceptNoticeTick(t, ar, scheduler, cmd, fadeDuration)
	require.False(t, m.SettledPresentation(), "elapsed exit needs its component Tick")
	m.Update(tick)
	require.True(t, m.SettledPresentation(), "final empty row is settled")
	assert.Empty(t, m.message.Text)
	m.SetMessage(Message{Text: "Hidden during entry"})
	m.SetSize(80, 0)
	require.True(t, m.SettledPresentation(), "hiding clears and settles the row")
}

func TestSeverityUsesSemanticForeground(t *testing.T) {
	for _, severity := range []Severity{Info, Warning, Error, Success} {
		m := New()
		m.SetSize(80, 1)
		assert.Nil(t, m.SetMessage(Message{Text: "Notice", Severity: severity}))
		assert.Equal(t, rgba(m.severityColor()), rgba(foregroundAt(t, m.View(), "N")))
	}
}

func TestFiveSecondNoticeKeepsDefaultBackgroundThroughExpiry(t *testing.T) {
	scheduler := &noticeScheduler{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	m := NewWithRuntime(ar)
	m.SetSize(80, 1)
	deadline := scheduler.now.Add(5 * time.Second)
	token, cmd, ok := m.SetNotice(notice(), deadline)
	require.True(t, ok)
	require.Nil(t, cmd)
	assertDefaultBackground(t, m.View(), 80)
	for range 2 {
		tick := acceptNoticeTick(t, ar, scheduler, ar.Continue(), fadeDuration/2)
		m.Update(tick)
		assertDefaultBackground(t, m.View(), 80)
	}
	require.True(t, m.SettledPresentation())
	require.Zero(t, ar.ActiveCount())
	require.Nil(t, ar.Continue(), "five-second hold must not schedule frame ticks")
	require.Nil(t, m.Expire(token, deadline.Add(-time.Nanosecond)))
	require.True(t, m.HasActions())
	require.Nil(t, m.Expire(Token{Owner: token.Owner, Generation: token.Generation + 1}, deadline))
	require.False(t, m.closing, "stale expiry cannot dismiss the notice")
	scheduler.now = deadline
	require.Nil(t, m.Expire(token, deadline))
	require.False(t, m.HasActions())
	assertDefaultBackground(t, m.View(), 80)
	for range 2 {
		tick := acceptNoticeTick(t, ar, scheduler, ar.Continue(), fadeDuration/2)
		m.Update(tick)
		assertDefaultBackground(t, m.View(), 80)
	}
	require.Empty(t, m.message.Text)
	require.Zero(t, ar.ActiveCount())
	require.Nil(t, ar.Continue())
}
