package app

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

func TestCriticalLifecycleEventSurvivesActualFanoutOverflow(t *testing.T) {
	ctx := t.Context()
	a := &App{ctx: func() context.Context { return ctx }, events: make(chan any, 2048)}
	slow := make(chan any, 1)
	registerFanoutTestSubscriber(a, ctx, slow)
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
