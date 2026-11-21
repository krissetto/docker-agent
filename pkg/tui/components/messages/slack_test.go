package messages

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// shrinkingView is a layout.Model whose rendered height can be changed
// between renders, simulating content shrinkage (e.g. tools fading out
// of a reasoning block).
type shrinkingView struct {
	height int
}

func (s *shrinkingView) Init() tea.Cmd                          { return nil }
func (s *shrinkingView) Update(tea.Msg) (layout.Model, tea.Cmd) { return s, nil }
func (s *shrinkingView) View() string {
	if s.height <= 0 {
		return ""
	}
	return strings.Repeat("x\n", s.height-1) + "x"
}
func (s *shrinkingView) SetSize(_, _ int) tea.Cmd { return nil }

// addShrinkingView adds a shrinkingView with a placeholder message. It is
// intentionally not cached (MessageTypeAssistantReasoningBlock is not cached)
// so that height changes propagate through ensureAllItemsRendered.
func addShrinkingView(m *model, height int) *shrinkingView {
	view := &shrinkingView{height: height}
	msg := &types.Message{Type: types.MessageTypeAssistantReasoningBlock, Sender: "root"}
	m.messages = append(m.messages, msg)
	m.views = append(m.views, view)
	m.renderDirty = true
	return view
}

func TestBottomSlackCappedOnLargeShrinkage(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(animation.NewRuntime(), 80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	view := addShrinkingView(m, 30)

	// Initial render: content fits the auto-scroll path; no slack.
	m.View()
	require.Equal(t, 30, m.totalHeight)
	require.Equal(t, 0, m.bottomSlack)

	// Simulate a large shrinkage (e.g. multiple tools fading out at once)
	// and re-render. Without the cap, slack would absorb all 25 lines and
	// leave the viewport mostly empty.
	view.height = 5
	m.invalidateAllItems()
	m.View()

	require.Equal(t, 5, m.totalHeight)
	maxSlack := m.maxBottomSlack()
	assert.LessOrEqual(t, m.bottomSlack, maxSlack,
		"slack must be capped to keep the viewport from being mostly empty")
}

type slackScheduler struct {
	now time.Time
}

func (s *slackScheduler) Now() time.Time { return s.now }

func (s *slackScheduler) Tick(d time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		s.now = s.now.Add(d)
		return f(s.now)
	}
}

func tickSlack(t *testing.T, m *model, ar *animation.Runtime, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	require.NotNil(t, cmd)
	tick, ok := ar.Accept(cmd().(animation.TickMsg))
	require.True(t, ok)
	_, _ = m.Update(tick)
	return ar.Continue()
}

func TestBottomSlackDecaysOnAnimationTick(t *testing.T) {
	t.Parallel()

	ar := animation.NewRuntimeWithScheduler(&slackScheduler{now: time.Unix(1, 0)})
	m := NewScrollableView(ar, 80, 24, &service.SessionState{}).(*model)
	addShrinkingView(m, 10)
	m.View()

	// An in-flight fade supplied the first tick before it left some slack.
	require.False(t, m.slackAnimationSub.IsActive())
	fade := ar.Subscribe()
	cmd := fade.Start()
	m.bottomSlack = 3
	cmd = tickSlack(t, m, ar, cmd)
	assert.Equal(t, 3, m.bottomSlack, "a single 60 FPS tick does not discard a line")
	require.True(t, m.slackAnimationSub.IsActive())
	fade.Stop()

	for ar.Now() < 3*slackDecayLineDuration {
		cmd = tickSlack(t, m, ar, cmd)
		assert.Equal(t, 3-int(ar.Now()/slackDecayLineDuration), m.bottomSlack)
	}
	assert.Zero(t, m.bottomSlack)
	assert.False(t, m.slackAnimationSub.IsActive())
	assert.Nil(t, cmd, "settled slack releases the shared clock")
}

func TestBottomSlackDoesNotLeaveEmptyViewportAfterShrinkage(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(animation.NewRuntime(), 80, 10, sessionState).(*model)
	m.SetSize(80, 10)

	// Start with content that fills the viewport while auto-scrolled.
	view := addShrinkingView(m, 30)
	m.View()
	require.Equal(t, 30, m.totalHeight)

	// Shrink the content drastically (simulates several tools fading out
	// at the same time).
	view.height = 2
	m.invalidateAllItems()
	out := m.View()

	// The visible viewport must still contain real content; not be filled
	// with empty slack lines.
	contentLines := 0
	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) != "" {
			contentLines++
		}
	}
	assert.Positive(t, contentLines,
		"viewport should not be entirely empty after content shrinks")
}

func TestMaxBottomSlackScalesWithViewportHeight(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}

	cases := []struct {
		height int
		want   int
	}{
		{height: 3, want: 1},   // tiny viewport → at least 1
		{height: 12, want: 4},  // height/3
		{height: 24, want: 5},  // capped at 5
		{height: 100, want: 5}, // still capped at 5
	}
	for _, c := range cases {
		m := NewScrollableView(animation.NewRuntime(), 80, c.height, sessionState).(*model)
		m.SetSize(80, c.height)
		assert.Equal(t, c.want, m.maxBottomSlack(),
			"maxBottomSlack(height=%d)", c.height)
	}
}

func TestBottomSlackIsZeroWhenUserHasScrolledAway(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(animation.NewRuntime(), 80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	view := addShrinkingView(m, 30)
	m.View()

	// User scrolls away from the bottom, then content shrinks. We don't
	// want slack to be added in that case, since the user already chose
	// their scroll position.
	m.userHasScrolled = true
	view.height = 5
	m.invalidateAllItems()
	m.View()

	assert.Equal(t, 0, m.bottomSlack,
		"slack must remain zero when the user scrolled away from the bottom")
}

func TestBottomSlackDecayPausesWhenUserScrollsAway(t *testing.T) {
	t.Parallel()

	ar := animation.NewRuntimeWithScheduler(&slackScheduler{now: time.Unix(1, 0)})
	m := NewScrollableView(ar, 80, 24, &service.SessionState{}).(*model)
	addShrinkingView(m, 10)
	m.View()
	m.bottomSlack = 3

	cmd := m.slackAnimationSub.Start()
	for ar.Now() < slackDecayLineDuration {
		cmd = tickSlack(t, m, ar, cmd)
	}
	require.Equal(t, 2, m.bottomSlack)

	m.userHasScrolled = true
	cmd = tickSlack(t, m, ar, cmd)
	assert.Zero(t, m.bottomSlack, "scrolling away drops slack without further decay")
	assert.Nil(t, cmd)
}

func TestAdjustBottomSlackRespectsCapAndFloor(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := NewScrollableView(animation.NewRuntime(), 80, 24, sessionState).(*model)
	m.SetSize(80, 24)

	maxSlack := m.maxBottomSlack()

	// Adding more than the cap must clamp to the cap.
	m.AdjustBottomSlack(100)
	assert.Equal(t, maxSlack, m.bottomSlack,
		"AdjustBottomSlack must clamp positive deltas to maxBottomSlack")

	// Subtracting below zero must clamp to zero.
	m.AdjustBottomSlack(-100)
	assert.Equal(t, 0, m.bottomSlack,
		"AdjustBottomSlack must clamp negative deltas at zero")

	// Zero delta is a no-op.
	m.bottomSlack = 2
	m.AdjustBottomSlack(0)
	assert.Equal(t, 2, m.bottomSlack,
		"AdjustBottomSlack(0) must leave slack unchanged")
}
