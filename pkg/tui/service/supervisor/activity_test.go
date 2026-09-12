package supervisor

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func activitySnapshot(childState, grandchildState subagent.NodeState) subagent.Snapshot {
	rootA := subagent.NodeSnapshot{Node: subagent.Node{ID: "root:A", Agent: "root-a", State: subagent.NodeIdle}}
	child := subagent.NodeSnapshot{Node: subagent.Node{ID: "child", Parent: "root:A", Agent: "child", State: childState}}
	if grandchildState != "" {
		child.Children = []subagent.NodeSnapshot{{Node: subagent.Node{ID: "grand", Parent: "child", Agent: "grand", State: grandchildState}}}
	}
	rootA.Children = []subagent.NodeSnapshot{child}
	rootB := subagent.NodeSnapshot{Node: subagent.Node{ID: "root:B", Agent: "root-b", State: subagent.NodeIdle}}
	return subagent.Snapshot{Nodes: []subagent.NodeSnapshot{rootA, rootB}}
}

func TestDeriveTabActivityNestedPropagationAndIsolation(t *testing.T) {
	t.Parallel()
	snapshot := activitySnapshot(subagent.NodeIdle, subagent.NodeRunning)

	assert.Equal(t, messages.TabActivityRunning,
		deriveTabActivity(activitySnapshot(subagent.NodeRunning, subagent.NodeStarting), "child", messages.TabActivityRunning),
		"own running wins over a descendant that has not begun")
	assert.Equal(t, messages.TabActivityDescendantRunning, deriveTabActivity(snapshot, "root:A", messages.TabActivityNone))
	assert.Equal(t, messages.TabActivityDescendantRunning, deriveTabActivity(snapshot, "child", messages.TabActivityNone))
	assert.Equal(t, messages.TabActivityNone, deriveTabActivity(snapshot, "grand", messages.TabActivityNone))
	assert.Equal(t, messages.TabActivityNone, deriveTabActivity(snapshot, "root:B", messages.TabActivityNone))
}

func TestDeriveTabActivityPrecedenceAndTerminalClear(t *testing.T) {
	t.Parallel()
	assert.Equal(t, messages.TabActivityPending,
		deriveTabActivity(activitySnapshot(subagent.NodeStarting, ""), "child", messages.TabActivityPending))
	assert.Equal(t, messages.TabActivityDescendantRunning,
		deriveTabActivity(activitySnapshot(subagent.NodeRunning, subagent.NodeRunning), "child", messages.TabActivityNone),
		"descendant activity wins without stacking ambiguous spinners")

	for _, state := range []subagent.NodeState{subagent.NodeIdle, subagent.NodeCompleted, subagent.NodeFailed, subagent.NodeStopped} {
		assert.Equal(t, messages.TabActivityRunning,
			deriveTabActivity(activitySnapshot(state, ""), "child", messages.TabActivityRunning),
			"canonical session activity is independent of topology state %s", state)
	}
}

func TestDeriveTabActivitySyntheticRootIgnoresOwnPermanentRunningState(t *testing.T) {
	t.Parallel()
	for _, childState := range []subagent.NodeState{subagent.NodeIdle, subagent.NodeCompleted, subagent.NodeFailed, subagent.NodeStopped} {
		snapshot := activitySnapshot(childState, "")
		snapshot.Nodes[0].Node.State = subagent.NodeRunning
		assert.Equal(t, messages.TabActivityNone,
			deriveTabActivity(snapshot, "root:A", messages.TabActivityNone),
			"topology root state must not imply session activity with %s child", childState)
		assert.Equal(t, messages.TabActivityRunning,
			deriveTabActivity(snapshot, "root:A", messages.TabActivityRunning),
			"runner stream lifecycle remains authoritative for root own work")
	}
}

func TestTabNotificationsPreserveTerminalFinalState(t *testing.T) {
	t.Parallel()
	s := newTestSupervisor([]string{"A"}, "A")
	started := make(chan struct{})
	release := make(chan struct{})
	delivered := make(chan messages.TabsUpdatedMsg, 2)
	s.tabSender = func(msg messages.TabsUpdatedMsg) {
		if msg.Tabs[0].Activity == messages.TabActivityRunning {
			close(started)
			<-release
		}
		delivered <- msg
	}

	s.mu.Lock()
	s.runners["A"].sessionState = runtime.SessionStateRunning
	s.notifyTabsUpdated()
	s.mu.Unlock()
	<-started

	s.mu.Lock()
	s.runners["A"].sessionState = runtime.SessionStateSettled
	s.notifyTabsUpdated()
	s.mu.Unlock()
	close(release)

	assert.Equal(t, messages.TabActivityRunning, (<-delivered).Tabs[0].Activity)
	assert.Equal(t, messages.TabActivityNone, (<-delivered).Tabs[0].Activity,
		"a terminal update cannot be overtaken by an older async send")
	close(s.tabNotifyDone)
}

func TestAttachedFallbackInvalidationOwnerIsSingularAndDeterministic(t *testing.T) {
	t.Parallel()
	// The first attached runner in tab order owns fallback invalidation after
	// its root closes; later viewers of the same runtime are duplicates.
	s := newTestSupervisor([]string{"first", "second"}, "first")
	for _, id := range s.order {
		s.runners[id].App = app.New(t.Context(), nil, session.New(session.WithID(id)), runtime.SessionBinding{},
			app.WithSubagentAttach(runtime.SubagentAttachInfo{NodeID: subagent.NodeID("node-" + id)}))
	}
	assert.Equal(t, "first", s.runtimeTreeInvalidationOwnerLocked(nil))
}

func TestDeriveTabActivityRepeatedSnapshotIsIdempotent(t *testing.T) {
	t.Parallel()
	snapshot := activitySnapshot(subagent.NodeRunning, "")
	first := deriveTabActivity(snapshot, "child", messages.TabActivityNone)
	assert.Equal(t, first, deriveTabActivity(snapshot, "child", messages.TabActivityNone))
}
