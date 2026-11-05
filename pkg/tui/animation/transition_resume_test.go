package animation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTransitionResumeKeepsProgressAndClock(t *testing.T) {
	runTransitionSynctest(t, func(t *testing.T) {
		t.Helper()
		ar := NewRuntime()
		tr := ar.Transition()
		require.Nil(t, tr.Start(10*TickRate, Linear))
		cmd := ar.Continue()
		for range 4 {
			tickTransition(t, &cmd, &tr)
		}
		require.InDelta(t, 0.4, tr.Progress(), 1e-9)

		tr.Cancel()
		require.Equal(t, int32(0), ar.ActiveCount())

		require.Nil(t, tr.Resume())
		cmd = ar.Continue()
		require.NotNil(t, cmd)
		assert.True(t, tr.Running())
		assert.Equal(t, int32(1), ar.ActiveCount())
		assert.InDelta(t, 0.4, tr.Progress(), 1e-9, "resume must not restart the clock")

		for range 6 {
			tickTransition(t, &cmd, &tr)
		}
		assert.False(t, tr.Running())
		assert.InDelta(t, 1.0, tr.Value(), 1e-9)
		assert.Equal(t, int32(0), ar.ActiveCount())
	})
}

func TestTransitionResumeAfterClockPassedEndCompletesWithoutLease(t *testing.T) {
	runTransitionSynctest(t, func(t *testing.T) {
		t.Helper()
		ar := NewRuntime()
		tr := ar.Transition()
		other := ar.Transition()
		require.Nil(t, tr.Start(3*TickRate, Linear))
		cmd := ar.Continue()
		tickTransition(t, &cmd, &tr)
		tr.Cancel()

		require.Nil(t, other.Start(10*TickRate, Linear))
		otherCmd := ar.Continue()
		for range 5 {
			tickTransition(t, &otherCmd, &other)
		}
		require.Equal(t, int32(1), ar.ActiveCount())

		assert.Nil(t, tr.Resume(), "a transition whose duration elapsed while cancelled needs no tick")
		assert.False(t, tr.Running())
		assert.InDelta(t, 1.0, tr.Value(), 1e-9)
		assert.Equal(t, int32(1), ar.ActiveCount(), "completed resume must not register")
	})
}

func TestTransitionResumeWhileRunningIsNoop(t *testing.T) {
	runTransitionSynctest(t, func(t *testing.T) {
		t.Helper()
		ar := NewRuntime()
		tr := ar.Transition()
		require.Nil(t, tr.Start(10*TickRate, Linear))
		cmd := ar.Continue()
		tickTransition(t, &cmd, &tr)

		assert.Nil(t, tr.Resume())
		assert.Equal(t, int32(1), ar.ActiveCount())
		tickTransition(t, &cmd, &tr)
		assert.Equal(t, int32(1), ar.ActiveCount())
	})
}

func TestTransitionResumeNeverStartedIsNoop(t *testing.T) {
	ar := NewRuntime()
	tr := ar.Transition()
	assert.Nil(t, tr.Resume())
	assert.False(t, tr.Running())
	assert.Equal(t, int32(0), ar.ActiveCount())
}

func TestTransitionResumeSharesInFlightChain(t *testing.T) {
	runTransitionSynctest(t, func(t *testing.T) {
		t.Helper()
		ar := NewRuntime()
		first := ar.Transition()
		second := ar.Transition()
		require.Nil(t, first.Start(10*TickRate, Linear))
		cmd := ar.Continue()
		require.Nil(t, second.Start(10*TickRate, Linear))
		second.Cancel()
		require.Equal(t, int32(1), ar.ActiveCount())

		assert.Nil(t, second.Resume(), "the runtime already holds the tick lease")
		assert.True(t, second.Running())
		assert.Equal(t, int32(2), ar.ActiveCount())

		tickTransition(t, &cmd, &first)
		assert.Equal(t, int32(2), ar.ActiveCount())
		assert.NotNil(t, cmd, "exactly one continuation is scheduled per accepted tick")
	})
}

func TestTransitionResumeWhilePausedDefersToRuntimeResume(t *testing.T) {
	runTransitionSynctest(t, func(t *testing.T) {
		t.Helper()
		ar := NewRuntime()
		tr := ar.Transition()
		require.Nil(t, tr.Start(10*TickRate, Linear))
		cmd := ar.Continue()
		for range 2 {
			tickTransition(t, &cmd, &tr)
		}
		ar.Pause()
		tr.Cancel()
		require.Equal(t, int32(0), ar.ActiveCount())

		assert.Nil(t, tr.Resume(), "a paused runtime schedules nothing")
		assert.Equal(t, int32(1), ar.ActiveCount())
		assert.InDelta(t, 0.2, tr.Progress(), 1e-9, "paused time is not elapsed")

		cmd = ar.Resume()
		require.NotNil(t, cmd)
		assert.Nil(t, ar.Resume(), "duplicate resume cannot fork the chain")
		for range 8 {
			tickTransition(t, &cmd, &tr)
		}
		assert.False(t, tr.Running())
		assert.Equal(t, int32(0), ar.ActiveCount())
	})
}

func TestTransitionStartAfterResumeRetargetsWithoutRegistering(t *testing.T) {
	runTransitionSynctest(t, func(t *testing.T) {
		t.Helper()
		ar := NewRuntime()
		tr := ar.Transition()
		require.Nil(t, tr.Start(10*TickRate, Linear))
		cmd := ar.Continue()
		tickTransition(t, &cmd, &tr)
		tr.Cancel()
		require.Nil(t, tr.Resume())
		cmd = ar.Continue()
		require.NotNil(t, cmd)

		assert.Nil(t, tr.Start(20*TickRate, EaseOutCubic))
		assert.True(t, tr.Running())
		assert.Equal(t, int32(1), ar.ActiveCount())
		assert.InDelta(t, 0.0, tr.Progress(), 1e-9)
		assert.Equal(t, 20*TickRate, tr.Duration())
	})
}
