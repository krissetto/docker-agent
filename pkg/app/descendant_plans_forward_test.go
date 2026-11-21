package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

func TestDescendantPlanChangesReachIdleParent(t *testing.T) {
	t.Parallel()
	for _, topology := range []bool{false, true} {
		name := "local"
		if topology {
			name = "remote-tree"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := &App{events: make(chan any, 8)}
			a.bridgeEpoch.Store(7)
			created := runtime.PlanChanged("shared", "release", "write", 1, "")
			revised := runtime.PlanChanged("shared", "release", "write", 2, "")
			updates := make(chan runtime.TreeUpdate, 2)
			updates <- runtime.TreeUpdate{Event: &runtime.SessionEvent{SessionID: "child", Event: created}}
			updates <- runtime.TreeUpdate{Event: &runtime.SessionEvent{SessionID: "child", Event: revised}}
			close(updates)
			observation := runtime.Observation{TreeUpdates: updates}
			watch := newSeedWatch()
			if topology {
				a.consumeTreeObservation(t.Context(), watch, observation, 7, false)
			} else {
				a.consumeDescendantUsage(t.Context(), watch, observation, 7)
			}
			require.Len(t, a.events, 2)
			assert.Same(t, created, <-a.events, "first child write wakes the parent's shared Plans view")
			assert.Same(t, revised, <-a.events, "subsequent revisions keep refreshing the view")
		})
	}
}

func TestDescendantPlanChangesIgnoreRootGapsAndStaleAttachments(t *testing.T) {
	t.Parallel()
	a := &App{events: make(chan any, 8)}
	a.bridgeEpoch.Store(7)
	watch := newSeedWatch()
	change := runtime.PlanChanged("shared", "release", "write", 1, "")
	for _, envelope := range []runtime.SessionEvent{
		{SessionID: "root", Event: change},
		{SessionID: "child", Gap: true, Event: change},
		{Event: change},
		{SessionID: "child", Event: runtime.StreamStarted("child", "worker")},
	} {
		a.forwardDescendantPlanChange(t.Context(), watch, envelope, 7)
	}
	a.forwardDescendantPlanChange(t.Context(), watch, runtime.SessionEvent{SessionID: "child", Event: change}, 6)
	assert.Empty(t, a.events)
}
