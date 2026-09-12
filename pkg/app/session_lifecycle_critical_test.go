package app

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

func TestCriticalLifecycleEventEvictsOnActualFanoutOverflow(t *testing.T) {
	ctx := t.Context()
	a := &App{ctx: func() context.Context { return ctx }, events: make(chan any, 2048)}
	slow := make(chan any, 1)
	a.addSubscriber(slow)
	slow <- runtime.AgentChoice("a", "s", "old")
	a.fanoutOnce.Do(a.startFanOut)
	a.events <- SessionEventMsg{Event: runtime.SkillOperation("s", "a", "op", "skill", "failed", "boom")}
	require.Eventually(t, func() bool {
		select {
		case msg := <-slow:
			bridged, ok := msg.(SessionEventMsg)
			if !ok {
				return false
			}
			_, ok = bridged.Event.(*runtime.SkillOperationEvent)
			return ok
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
}

func TestSessionLifecycleEventsAreCriticalOnFanoutOverflow(t *testing.T) {
	t.Parallel()
	for _, event := range []runtime.Event{
		runtime.PauseChanged("s", "a", true),
		runtime.SkillOperation("s", "a", "op", "skill", "accepted", ""),
		runtime.SkillOperation("s", "a", "op", "skill", "failed", "boom"),
	} {
		assert.True(t, isTurnBoundaryEvent(SessionEventMsg{Event: event}), "%T must evict content rather than be dropped", event)
	}
}
