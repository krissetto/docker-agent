package sidebar

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestSubagentStartingIsStaticPending(t *testing.T) {
	t.Parallel()
	m := New(animation.NewRuntime(), t.Context(), &service.SessionState{}).(*model)

	assert.False(t, isActiveSubagentState(subagent.NodeStarting))
	assert.True(t, isActiveSubagentState(subagent.NodeRunning))
	assert.Contains(t, m.subagentGlyph(subagent.Node{Agent: "worker", State: subagent.NodeStarting}), "·")
}

func TestCanonicalRootSpinnerLeaseAndIdleUnregister(t *testing.T) {
	t.Parallel()
	for _, attached := range []bool{false, true} {
		t.Run(map[bool]string{false: "session root", true: "attached root"}[attached], func(t *testing.T) {
			scheduler := &immediateScheduler{now: time.Unix(1, 0)}
			ar := animation.NewRuntimeWithScheduler(scheduler)
			t.Cleanup(ar.Stop)
			m := New(ar, t.Context(), &service.SessionState{}).(*model)
			m.rootSessionID = "sess"
			root := subagent.Node{ID: "root:sess", Agent: "worker", State: subagent.NodeRunning}
			if attached {
				root.ID = "attached-node"
				m.SetSubagentContext(root.ID, "parent", "parent-session")
			}
			snap := subagent.Snapshot{Root: root.ID, Nodes: []subagent.NodeSnapshot{{Node: root}}}
			cmd := m.SetSubagentTree(snap)
			require.NotNil(t, cmd)
			require.Empty(t, m.workingAgent)
			require.Empty(t, m.subagentNodes)
			require.True(t, m.subagentSpinnerOn)
			require.EqualValues(t, 1, ar.ActiveCount())
			before := m.View()
			frame := m.subagentSpinner.RawFrame()
			for range 20 {
				tick, ok := ar.Accept(cmd().(animation.TickMsg))
				require.True(t, ok)
				m.Update(tick)
				cmd = ar.Continue()
				if m.subagentSpinner.RawFrame() != frame {
					break
				}
				require.NotNil(t, cmd)
			}
			require.NotEqual(t, frame, m.subagentSpinner.RawFrame())
			assert.NotEqual(t, before, m.View(), "the sole running canonical row must animate")
			snap.Nodes[0].Node.State = subagent.NodeIdle
			m.SetSubagentTree(snap)
			assert.False(t, m.subagentSpinnerOn)
			assert.Zero(t, ar.ActiveCount(), "idle canonical root releases the last animation lease")
			assert.Nil(t, ar.Continue())
		})
	}
}
