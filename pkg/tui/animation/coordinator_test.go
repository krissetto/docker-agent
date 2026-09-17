package animation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runTick(t *testing.T, cmd func() any) TickMsg {
	t.Helper()
	msg, ok := cmd().(TickMsg)
	require.True(t, ok)
	return msg
}

func TestExplicitSubscriptionOwnerAcceptsDeliveredTicks(t *testing.T) {
	ar := NewRuntime()
	t.Cleanup(ar.Stop)
	sub := ar.Subscribe()
	require.Nil(t, sub.Start(), "subscription registers without scheduling")
	require.True(t, sub.IsActive())
	require.EqualValues(t, 1, ar.ActiveCount())
	require.False(t, ar.tickScheduled)

	first := ar.Continue()
	require.NotNil(t, first)
	firstTick := runTick(t, func() any { return first() })
	require.Zero(t, ar.Now(), "delivery alone cannot advance the owner clock")
	require.Nil(t, ar.Continue(), "delivery alone cannot release the owner lease")
	accepted, ok := ar.Accept(firstTick)
	require.True(t, ok)
	assert.Equal(t, 1, accepted.Frame)

	second := ar.Continue()
	require.NotNil(t, second, "owner acceptance permits the next frame")
	accepted, ok = ar.Accept(runTick(t, func() any { return second() }))
	require.True(t, ok)
	assert.Equal(t, 2, accepted.Frame)

	queued := ar.Continue()
	require.NotNil(t, queued)
	sub.Stop()
	assert.False(t, ar.HasActive())
	assert.Nil(t, ar.Continue())
	_, ok = ar.Accept(runTick(t, func() any { return queued() }))
	assert.False(t, ok, "stopped owner rejects its queued tick")
}

func TestUnboundSubscriptionRejectsStartAndCanBindAfterward(t *testing.T) {
	ar := NewRuntime()
	t.Cleanup(ar.Stop)
	var sub Subscription
	require.NotPanics(t, sub.Stop, "idle cleanup needs no runtime")
	require.False(t, sub.IsActive())
	require.Panics(t, func() { sub.Start() }, "starting requires an explicit owner")
	require.False(t, sub.IsActive(), "failed start must not mutate subscription state")
	require.Zero(t, ar.ActiveCount(), "unbound start cannot register with another runtime")
	require.NotPanics(t, sub.Stop, "cleanup after rejected start remains safe")

	sub.SetRuntime(ar)
	require.Nil(t, sub.Start())
	require.True(t, sub.IsActive())
	require.EqualValues(t, 1, ar.ActiveCount())
	require.Nil(t, sub.Start(), "repeated start does not duplicate registration")
	require.EqualValues(t, 1, ar.ActiveCount())
	cmd := ar.Continue()
	require.NotNil(t, cmd, "binding after rejection produces a functioning owner lease")
	_, accepted := ar.Accept(runTick(t, func() any { return cmd() }))
	require.True(t, accepted)
	require.Positive(t, ar.Now())
	sub.Stop()
	sub.Stop()
	require.False(t, sub.IsActive())
	require.Zero(t, ar.ActiveCount())
	require.Nil(t, ar.Continue())
}

func TestAcceptedTickCopiesShareDirtyMarker(t *testing.T) {
	ar := NewRuntime()
	sub := ar.Subscribe()
	require.Nil(t, sub.Start())
	tick := runTick(t, func() any { return ar.Continue()() })
	accepted, ok := ar.Accept(tick)
	require.True(t, ok)
	acceptedCopy := accepted
	assert.False(t, accepted.Dirty())
	acceptedCopy.MarkDirty()
	assert.True(t, accepted.Dirty())
	elapsedBeforeTick, elapsedAfterTick := accepted.ElapsedBounds()
	assert.Less(t, elapsedBeforeTick, elapsedAfterTick)
	sub.Stop()
}

func TestRejectedTickCannotBecomeDirty(t *testing.T) {
	first, second := NewRuntime(), NewRuntime()
	sub := first.Subscribe()
	require.Nil(t, sub.Start())
	tick := runTick(t, func() any { return first.Continue()() })
	rejected, ok := second.Accept(tick)
	assert.False(t, ok)
	rejected.MarkDirty()
	assert.False(t, rejected.Dirty())
	sub.Stop()
}

func TestRuntimeIsolationAndOwnerToken(t *testing.T) {
	first, second := NewRuntime(), NewRuntime()
	firstSub, secondSub := first.Subscribe(), second.Subscribe()
	require.Nil(t, firstSub.Start())
	firstCmd := first.Continue()
	require.Nil(t, secondSub.Start())
	secondCmd := second.Continue()
	require.NotNil(t, firstCmd)
	require.NotNil(t, secondCmd)

	firstTick := runTick(t, func() any { return firstCmd() })
	secondTick := runTick(t, func() any { return secondCmd() })
	_, ok := second.Accept(firstTick)
	assert.False(t, ok, "runtime identity rejects another runtime's tick")
	assert.Zero(t, second.Now())
	_, ok = first.Accept(firstTick)
	require.True(t, ok)
	assert.Positive(t, first.Now())
	assert.Zero(t, second.Now())
	_, ok = second.Accept(secondTick)
	require.True(t, ok)
}

func TestRuntimeStopDoesNotAffectAnotherRuntime(t *testing.T) {
	first, second := NewRuntime(), NewRuntime()
	firstSub, secondSub := first.Subscribe(), second.Subscribe()
	require.Nil(t, firstSub.Start())
	require.Nil(t, secondSub.Start())
	firstCmd, secondCmd := first.Continue(), second.Continue()
	firstSub.Stop()
	assert.False(t, first.HasActive())
	assert.True(t, second.HasActive())
	assert.Nil(t, first.Continue())
	secondTick := runTick(t, func() any { return secondCmd() })
	_, ok := second.Accept(secondTick)
	require.True(t, ok)
	assert.NotNil(t, second.Continue())
	secondSub.Stop()
	_ = firstCmd
}

func TestRuntimeAcceptOnlyOnce(t *testing.T) {
	ar := NewRuntime()
	sub := ar.Subscribe()
	require.Nil(t, sub.Start())
	tick := runTick(t, func() any { return ar.Continue()() })
	_, ok := ar.Accept(tick)
	require.True(t, ok)
	elapsed := ar.Now()
	_, ok = ar.Accept(tick)
	assert.False(t, ok)
	assert.Equal(t, elapsed, ar.Now())
	sub.Stop()
}

func TestRuntimeTransitionUsesOwnedClock(t *testing.T) {
	ar := NewRuntime()
	transition := ar.Transition()
	require.Nil(t, transition.Start(TickRate, Linear))
	cmd := ar.Continue()
	tick := runTick(t, func() any { return cmd() })
	_, ok := ar.Accept(tick)
	require.True(t, ok)
	transition.Tick()
	assert.False(t, transition.Running())
	assert.Equal(t, int32(0), ar.ActiveCount())
}

func TestTickOwnerIsImmutableAcrossRestart(t *testing.T) {
	ar := NewRuntime()
	sub := ar.Subscribe()
	require.Nil(t, sub.Start())
	staleCmd := ar.Continue()
	sub.Stop()
	require.Nil(t, sub.Start())
	freshCmd := ar.Continue()
	stale := runTick(t, func() any { return staleCmd() })
	fresh := runTick(t, func() any { return freshCmd() })
	_, ok := ar.Accept(stale)
	assert.False(t, ok)
	_, ok = ar.Accept(fresh)
	assert.True(t, ok)
	sub.Stop()
}

func TestRuntimeStopInvalidatesQueuedTickAndQuiesces(t *testing.T) {
	ar := NewRuntime()
	sub := ar.Subscribe()
	require.Nil(t, sub.Start())
	queued := ar.Continue()
	require.NotNil(t, queued)

	ar.Stop()
	assert.Equal(t, int32(0), ar.ActiveCount())
	assert.Nil(t, ar.Continue(), "stopped animation runtime schedules no successor")

	_, accepted := ar.Accept(runTick(t, func() any { return queued() }))
	assert.False(t, accepted, "queued generation is stale after teardown")
	assert.Zero(t, ar.Now(), "rejected queued tick does not advance the clock")
}

func TestExactlyOneContinuationPerAcceptedTick(t *testing.T) {
	ar := NewRuntime()
	sub := ar.Subscribe()
	require.Nil(t, sub.Start())
	first := ar.Continue()
	_, accepted := ar.Accept(runTick(t, func() any { return first() }))
	require.True(t, accepted)

	successor := ar.Continue()
	require.NotNil(t, successor)
	assert.Nil(t, ar.Continue(), "a live lease deduplicates parallel continuations")
	sub.Stop()
	_, accepted = ar.Accept(runTick(t, func() any { return successor() }))
	assert.False(t, accepted, "last unregister invalidates queued successor")
	assert.Nil(t, ar.Continue())
}

func TestTabBusySpinnerIsOneCellBraille(t *testing.T) {
	for _, frame := range TabBusy.Frames() {
		runes := []rune(frame)
		require.Len(t, runes, 1)
		assert.GreaterOrEqual(t, runes[0], rune(0x2800))
		assert.LessOrEqual(t, runes[0], rune(0x28ff))
	}
}

func TestRuntimeNowAdvancesByDeliveredTime(t *testing.T) {
	ar := NewRuntime()
	ar.Register()
	base := time.Now()
	ar.mu.Lock()
	ar.tickScheduled = true
	ar.generation = 2
	ar.lastDeliveredAt = base
	ar.mu.Unlock()
	_, ok := ar.Accept(TickMsg{runtimeIdentity: ar.runtimeIdentity, generation: 2, deliveredAt: base.Add(125 * time.Millisecond)})
	require.True(t, ok)
	assert.Equal(t, 125*time.Millisecond, ar.Now())
	ar.Unregister()
}

func TestRuntimeRestartExcludesIdleTime(t *testing.T) {
	ar := NewRuntime()
	sub := ar.Subscribe()
	require.Nil(t, sub.Start())
	first := runTick(t, func() any { return ar.Continue()() })
	_, ok := ar.Accept(first)
	require.True(t, ok)
	before := ar.Now()
	sub.Stop()
	// A new lease's first tick must use its own timer baseline, not the
	// previous animation's last delivery before the idle period.
	require.True(t, ar.lastDeliveredAt.IsZero())
	require.Nil(t, sub.Start())
	require.NotNil(t, ar.Continue())
	started := first.deliveredAt.Add(time.Hour)
	next := TickMsg{
		runtimeIdentity: ar.runtimeIdentity, generation: ar.generation,
		timerStartedAt: started, deliveredAt: started.Add(TickRate),
	}
	_, ok = ar.Accept(next)
	require.True(t, ok)
	require.Equal(t, before+TickRate, ar.Now())
	sub.Stop()
	require.Nil(t, ar.Continue())
}
