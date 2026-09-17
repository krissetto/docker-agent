package tabbar

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func feedbackMessages(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, child := range batch {
			out = append(out, feedbackMessages(child)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

func feedbackOwnerCommand(tb *TabBar, cmd tea.Cmd) tea.Cmd {
	return tea.Batch(cmd, tb.ar.Continue())
}

func finishFeedback(t *testing.T, tb *TabBar, cmd tea.Cmd) {
	t.Helper()
	cmd = feedbackOwnerCommand(tb, cmd)
	for cmd != nil {
		for _, msg := range feedbackMessages(cmd) {
			if tick, ok := msg.(animation.TickMsg); ok {
				_, accepted := tb.ar.Accept(tick)
				require.True(t, accepted)
				require.Empty(t, feedbackMessages(tb.Tick()))
			}
		}
		cmd = tb.ar.Continue()
	}
}

func TestPlusFeedbackFinishesFromOwnerCommittedClickCommand(t *testing.T) {
	t.Parallel()
	for _, release := range []bool{false, true} {
		tb := New(newMotionRuntime(), 8)
		tb.SetWidth(60)
		tb.SetTabs(motionTabs(1, 0), 0)
		x := plusZone(tb).startX + 1
		finishFeedback(t, tb, tb.Update(tea.MouseMotionMsg{X: x}))
		hover := tb.View()
		cmd := feedbackOwnerCommand(tb, tb.Update(tea.MouseClickMsg{X: x, Button: tea.MouseLeft}))
		clicked := tb.View()
		require.NotEqual(t, hover, clicked, "feedback starts before executing spawn")
		require.True(t, tb.IsAnimating())
		if release {
			require.Empty(t, feedbackMessages(tb.Update(tea.MouseReleaseMsg{X: x, Button: tea.MouseLeft})))
			require.False(t, tb.HasPointerCapture())
			require.True(t, tb.IsAnimating(), "release does not cancel visual feedback")
		}
		frames := map[string]bool{clicked: true}
		spawned := 0
		for cmd != nil {
			for _, msg := range feedbackMessages(cmd) {
				switch msg := msg.(type) {
				case messages.SpawnSessionMsg:
					spawned++
				case animation.TickMsg:
					_, accepted := tb.ar.Accept(msg)
					require.True(t, accepted, "owner command must retain its lease")
					require.Empty(t, feedbackMessages(tb.Tick()))
					frames[tb.View()] = true
				}
			}
			cmd = tb.ar.Continue()
		}
		require.Equal(t, 1, spawned)
		require.Greater(t, len(frames), 2, "elapsed clock produces intermediate frames")
		assert.Equal(t, hover, tb.View())
		assert.False(t, tb.IsAnimating())
		assert.False(t, tb.HasPointerCapture(), "finite feedback clears capture if release was swallowed by modal")
		assert.Zero(t, tb.ar.ActiveCount())
		assert.Nil(t, tb.ar.Continue())
		tb.StopAnimations()
	}
}

func TestPlusFeedbackRetargetLeaveAndCancellation(t *testing.T) {
	t.Parallel()
	tb := New(newMotionRuntime(), 8)
	tb.SetWidth(60)
	tb.SetTabs(motionTabs(1, 0), 0)
	normal := tb.View()
	x := plusZone(tb).startX + 1
	first := tb.Update(tea.MouseClickMsg{X: x, Button: tea.MouseLeft})
	tb.Update(tea.MouseReleaseMsg{X: x, Button: tea.MouseLeft})
	repeated := feedbackMessages(tb.Update(tea.MouseClickMsg{X: x, Button: tea.MouseLeft}))
	require.Equal(t, []tea.Msg{messages.SpawnSessionMsg{}}, repeated, "retarget keeps original runtime lease")
	require.Equal(t, int32(2), tb.ar.ActiveCount(), "independent finite hover and click transitions")
	tb.Update(tea.MouseMotionMsg{X: x, Y: -1})
	tb.Update(tea.MouseReleaseMsg{X: x, Y: -1, Button: tea.MouseLeft})
	require.NotEqual(t, normal, tb.View())
	for cmd := first; cmd != nil; cmd = tb.ar.Continue() {
		for _, msg := range feedbackMessages(cmd) {
			if tick, ok := msg.(animation.TickMsg); ok {
				_, accepted := tb.ar.Accept(tick)
				require.True(t, accepted)
				require.Empty(t, feedbackMessages(tb.Tick()))
			}
		}
	}
	require.Equal(t, normal, tb.View())
	for _, cancel := range []func(){
		func() { tb.Update(tea.BlurMsg{}) },
		func() { tb.SetWidth(0) },
		func() { tb.SetVisible(false) },
	} {
		tb.SetVisible(true)
		tb.SetWidth(60)
		tb.Update(tea.MouseClickMsg{X: x, Button: tea.MouseLeft})
		require.Positive(t, tb.ar.ActiveCount())
		cancel()
		require.Zero(t, tb.ar.ActiveCount())
		require.False(t, tb.HasPointerCapture())
	}
	tb.StopAnimations()
}

func TestPlusSoftHoverAndPressAcrossBundledThemes(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	for _, ref := range []string{"default", "default-light", "calm-roots", "catppuccin-latte", "catppuccin-mocha", "dracula", "gruvbox-dark", "gruvbox-light", "neon-pink", "nord", "one-dark", "solarized-dark", "tokyo-night"} {
		theme, err := styles.LoadTheme(ref)
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		tb := New(newMotionRuntime(), 8)
		tb.SetWidth(60)
		tb.SetTabs(motionTabs(1, 0), 0)
		x := plusZone(tb).startX + 1
		normal := tb.View()
		finishFeedback(t, tb, tb.Update(tea.MouseMotionMsg{X: x}))
		hover := tb.View()
		require.NotEqual(t, normal, hover, ref)
		cells := themeCells(hover)
		requireCellColor(t, blendColors(styles.Background, styles.TabHoverBg, 0.3), cells[x].bg)
		tb.Update(tea.MouseClickMsg{X: x, Button: tea.MouseLeft})
		require.NotEqual(t, hover, tb.View(), ref)
		tb.StopAnimations()
	}
}

func TestPlusHoverEnterReverseExitAndPulseUseCurrentTarget(t *testing.T) {
	t.Parallel()
	tb := New(newMotionRuntime(), 8)
	defer tb.StopAnimations()
	tb.SetWidth(60)
	tb.SetTabs(motionTabs(1, 0), 0)
	normal := tb.View()
	x := plusZone(tb).startX + 1
	cmd := feedbackOwnerCommand(tb, tb.Update(tea.MouseMotionMsg{X: x}))
	require.Equal(t, normal, tb.View(), "entry starts from original fraction")
	msgs := feedbackMessages(cmd)
	require.Len(t, msgs, 1)
	tick, ok := msgs[0].(animation.TickMsg)
	require.True(t, ok)
	_, accepted := tb.ar.Accept(tick)
	require.True(t, accepted)
	tb.Tick()
	fraction := tb.hoverFraction()
	require.Greater(t, fraction, 0.0)
	require.Less(t, fraction, 1.0)
	intermediate := tb.View()
	require.NotEqual(t, normal, intermediate)
	require.Nil(t, tb.Update(tea.MouseMotionMsg{X: -1, Y: -1}), "reversal retains existing lease")
	assert.InDelta(t, fraction, tb.hoverFraction(), 1e-9)
	assert.Equal(t, intermediate, tb.View())
	require.False(t, tb.plusHovered)
	finishFeedback(t, tb, tb.ar.Continue())
	assert.Equal(t, normal, tb.View())
	assert.Zero(t, tb.ar.ActiveCount())
	finishFeedback(t, tb, tb.Update(tea.MouseMotionMsg{X: x}))
	hover := tb.View()
	pulseCmd := tb.Update(tea.MouseClickMsg{X: x, Button: tea.MouseLeft})
	require.NotEqual(t, hover, tb.View())
	require.Empty(t, feedbackMessages(tb.Update(tea.MouseMotionMsg{X: -1, Y: -1})), "modal occlusion/outside sets exit while pulse already owns lease")
	require.False(t, tb.plusHovered)
	require.InDelta(t, 0.0, tb.plusHoverTo, 0)
	tb.Update(tea.MouseReleaseMsg{X: -1, Y: -1, Button: tea.MouseLeft})
	require.False(t, tb.HasPointerCapture())
	finishFeedback(t, tb, pulseCmd)
	assert.Equal(t, normal, tb.View(), "pulse finishes toward current exit target without more pointer events")
	assert.Zero(t, tb.ar.ActiveCount())
	assert.False(t, tb.IsAnimating())
	assert.Nil(t, tb.ar.Continue())
}

func TestPlusHoverThemeSwitchRebasesCurrentFraction(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	tb := New(newMotionRuntime(), 8)
	defer tb.StopAnimations()
	tb.SetWidth(60)
	tb.SetTabs(motionTabs(1, 0), 0)
	x := plusZone(tb).startX + 1
	msgs := feedbackMessages(feedbackOwnerCommand(tb, tb.Update(tea.MouseMotionMsg{X: x})))
	require.Len(t, msgs, 1)
	tick, ok := msgs[0].(animation.TickMsg)
	require.True(t, ok)
	_, accepted := tb.ar.Accept(tick)
	require.True(t, accepted)
	tb.Tick()
	fraction := tb.hoverFraction()
	require.Greater(t, fraction, 0.0)
	require.Less(t, fraction, 1.0)
	for _, ref := range []string{"default-light", "nord", "default"} {
		theme, err := styles.LoadTheme(ref)
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		view := tb.View()
		requireCellColor(t, blendColors(styles.Background, styles.TabHoverBg, .3*fraction), themeCells(view)[x].bg)
		assert.InDelta(t, fraction, tb.hoverFraction(), 0)
	}
	finishFeedback(t, tb, tb.ar.Continue())
	assert.Zero(t, tb.ar.ActiveCount())
	finishFeedback(t, tb, tb.Update(tea.MouseMotionMsg{X: -1, Y: -1}))
	assert.Zero(t, tb.ar.ActiveCount())
	assert.InDelta(t, 0.0, tb.hoverFraction(), 0)
}
