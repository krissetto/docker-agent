package supervisor

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
		"an idle owner shows descendant activity without spinning")
	assert.Equal(t, messages.TabActivityRunning,
		deriveTabActivity(activitySnapshot(subagent.NodeRunning, subagent.NodeRunning), "child", messages.TabActivityRunning),
		"canonical own work must not be hidden by running descendants")

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

type activityTreeServices struct {
	app.Services
	tree  *subagent.Tree
	reads atomic.Int32
}

func (s *activityTreeServices) SubagentTree() *subagent.Tree {
	s.reads.Add(1)
	return s.tree
}

func sharedTreeSupervisor(tb testing.TB, count int) (*Supervisor, *activityTreeServices) {
	tb.Helper()
	s := New(nil)
	services := &activityTreeServices{tree: subagent.NewTree()}
	nodes := make([]subagent.Node, count)
	for i := range count {
		id := fmt.Sprintf("tab-%d", i)
		node := subagent.SessionRootID(id)
		nodes[i] = subagent.Node{ID: node, Agent: "agent", State: subagent.NodeIdle}
		sess := session.New(session.WithID(id))
		a := app.New(tb.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(services))
		s.runners[id] = &SessionTab{ID: id, App: a}
		s.order = append(s.order, id)
	}
	require.NoError(tb, services.tree.AddSubtree(nodes))
	tb.Cleanup(s.Shutdown)
	return s, services
}

func TestTabActivitySharesOneTreeSnapshotPerRebuild(t *testing.T) {
	s, services := sharedTreeSupervisor(t, 2)
	cache := make(map[*subagent.Tree]subagent.Snapshot)
	activity, _, _ := tabActivity(s.runners["tab-0"], cache)
	require.Equal(t, messages.TabActivityNone, activity)
	require.Len(t, cache, 1)
	require.NoError(t, services.tree.AddSubtree([]subagent.Node{{ID: "child", Parent: subagent.SessionRootID("tab-1"), Agent: "agent", State: subagent.NodeRunning}}))
	activity, _, _ = tabActivity(s.runners["tab-1"], cache)
	require.Equal(t, messages.TabActivityNone, activity, "one rebuild sees one coherent tree head")
	tabs, _ := s.GetTabs()
	require.Equal(t, messages.TabActivityDescendantRunning, tabs[1].Activity, "the next rebuild sees new tree activity")
}

func TestTabNotificationsCoalesceBeforeTreeReads(t *testing.T) {
	s, services := sharedTreeSupervisor(t, 20)
	delivered := make(chan messages.TabsUpdatedMsg, 1)
	s.mu.Lock()
	s.tabSender = func(msg messages.TabsUpdatedMsg) { delivered <- msg }
	for range 100 {
		s.notifyTabsUpdated()
	}
	require.Zero(t, services.reads.Load(), "invalidations must not build tab snapshots")
	s.mu.Unlock()
	require.Len(t, (<-delivered).Tabs, 20)
}

func BenchmarkTabsSharedTree(b *testing.B) {
	for _, count := range []int{10, 100, 500} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s, _ := sharedTreeSupervisor(b, count)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				s.GetTabs()
			}
		})
	}
}

// Remote views have no in-process tree: their projected topology and status
// still mark the tab, so descendants and uncertain recovery are never idle.
func TestTabAttentionFromPortableTreeAndRecoveryUncertainty(t *testing.T) {
	s := New(nil)
	t.Cleanup(s.Shutdown)
	sess := session.New(session.WithID("remote-root"))
	tree := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{
		Node:     subagent.Node{ID: subagent.SessionRootID(sess.ID), SessionID: sess.ID, State: subagent.NodeIdle},
		Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "c0ffe", Parent: subagent.SessionRootID(sess.ID), SessionID: "child", State: subagent.NodeRunning, NeedsAttention: true, WaitingOn: "approve tool"}}},
	}}}
	sess.SetSubagentTree(&tree)
	a := app.New(t.Context(), nil, sess, runtime.SessionBinding{})
	s.runners[sess.ID] = &SessionTab{ID: sess.ID, App: a}
	s.order = append(s.order, sess.ID)
	s.activeID = sess.ID

	tabs, _ := s.GetTabs()
	require.Len(t, tabs, 1)
	assert.True(t, tabs[0].NeedsAttention, "a waiting descendant marks even the active tab")
	assert.Equal(t, messages.TabActivityDescendantRunning, tabs[0].Activity)

	tree.Nodes[0].Children[0].Node.NeedsAttention, tree.Nodes[0].Children[0].Node.State = false, subagent.NodeIdle
	sess.SetSubagentTree(&tree)
	s.runners[sess.ID].App = app.New(t.Context(), nil, sess, runtime.SessionBinding{})
	tabs, _ = s.GetTabs()
	assert.False(t, tabs[0].NeedsAttention)

	s.runners[sess.ID].projection = &app.PresentationState{Status: runtime.SessionStatus{SessionID: sess.ID, State: runtime.SessionStateSettled, InterruptedTurns: 1}}
	tabs, _ = s.GetTabs()
	assert.True(t, tabs[0].NeedsAttention, "uncertain recovery is not presented as a normal idle tab")
}

func TestCanonicalOwnTabActivityWithRunningDescendantsLocalAndRemote(t *testing.T) {
	for _, portable := range []bool{false, true} {
		name := "local tree"
		if portable {
			name = "remote portable tree"
		}
		t.Run(name, func(t *testing.T) {
			s, services := sharedTreeSupervisor(t, 2)
			rootID := subagent.SessionRootID("tab-0")
			require.NoError(t, services.tree.AddSubtree([]subagent.Node{
				{ID: "child", Parent: rootID, SessionID: "tab-1", Agent: "agent", State: subagent.NodeRunning},
				{ID: "grandchild", Parent: "child", SessionID: "grandchild-session", Agent: "agent", State: subagent.NodeRunning},
			}))
			for _, id := range s.order {
				runner := s.runners[id]
				sess := runner.App.Session()
				options := []app.Opt{app.WithRuntimeServices(services)}
				if id == "tab-1" {
					options = append(options, app.WithSubagentAttach(runtime.SubagentAttachInfo{NodeID: "child"}))
				}
				if portable {
					snapshot := services.tree.Snapshot()
					sess.SetSubagentTree(&snapshot)
					options[0] = app.WithRuntimeServices(nil)
				}
				runner.App = app.New(t.Context(), nil, sess, runtime.SessionBinding{}, options...)
			}
			for _, active := range s.order {
				s.SwitchTo(active)
				for _, id := range s.order {
					runner := s.GetRunner(id)
					for _, state := range []runtime.SessionState{runtime.SessionStateRunning, runtime.SessionStateSettled} {
						head := &app.PresentationState{Status: runtime.SessionStatus{SessionID: id, State: state}}
						s.applyPresentation(id, runner.App, runner.routeGeneration, head, nil)
						tabs, _ := s.GetTabs()
						for _, tab := range tabs {
							if tab.SessionID != id {
								continue
							}
							expected := messages.TabActivityDescendantRunning
							if state == runtime.SessionStateRunning {
								expected = messages.TabActivityRunning
							}
							require.Equal(t, expected, tab.Activity, "owner=%s active=%s state=%s", id, active, state)
							require.Equal(t, id == "tab-1", tab.IsAttached)
						}
					}
				}
			}
		})
	}
}
