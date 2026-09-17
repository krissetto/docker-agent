package spinner

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
)

func TestSpinnerCopyDoesNotLeakAnimationSubscription(t *testing.T) {
	t.Parallel()
	ar := animation.NewRuntime()
	s1 := New(ar, ModeSpinnerOnly, lipgloss.NewStyle())
	cmd := s1.Init()
	require.Nil(t, cmd)
	require.True(t, ar.HasActive())
	tickCmd := ar.Continue()
	require.NotNil(t, tickCmd)
	tick, accepted := ar.Accept(tickCmd().(animation.TickMsg))
	require.True(t, accepted)
	require.Positive(t, ar.Now())
	_, _ = s1.Update(tick)
	queued := ar.Continue()
	require.NotNil(t, queued)

	// Copy the spinner value and stop via the copy; should still stop the shared subscription.
	s2 := s1
	s2.Stop()
	require.False(t, ar.HasActive())
	require.Nil(t, ar.Continue())
	_, accepted = ar.Accept(queued().(animation.TickMsg))
	require.False(t, accepted, "teardown invalidates a queued continuation")
}

func BenchmarkSpinner_ModeSpinnerOnly(b *testing.B) {
	s := New(animation.NewRuntime(), ModeSpinnerOnly, lipgloss.NewStyle())
	for b.Loop() {
		_ = s.View()
	}
}

func BenchmarkSpinner_ModeBoth(b *testing.B) {
	s := New(animation.NewRuntime(), ModeBoth, lipgloss.NewStyle())
	for b.Loop() {
		_ = s.View()
	}
}
