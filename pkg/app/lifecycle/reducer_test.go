package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

func TestFromSnapshotPreservesExactStatusAndPendingWithoutFakeStream(t *testing.T) {
	state := FromSnapshot(runtime.SessionSnapshot{Status: runtime.SessionStatus{SessionID: "s", State: runtime.SessionStateQueued}, PendingInputs: []runtime.PendingInput{{TurnID: "a"}, {TurnID: "b"}}})
	assert.Equal(t, runtime.SessionStateQueued, state.Status)
	assert.Equal(t, []string{"a", "b"}, state.Pending)
	assert.Empty(t, state.Streams)
}

func TestReducerTransitions(t *testing.T) {
	state := State{SessionID: "s", Status: runtime.SessionStateSettled}
	state, actions := state.Apply(runtime.StreamStarted("s", "root"))
	require.Equal(t, TurnStarted, actions[0].Kind)
	assert.Equal(t, 1, state.Depth())
	assert.Equal(t, runtime.SessionStateRunning, state.Status)
	state, _ = state.Apply(runtime.StreamStarted("s", "worker"))
	state, actions = state.Apply(runtime.StreamStopped("s", "worker", "normal"))
	assert.Equal(t, TurnSettled, actions[0].Kind)
	assert.Equal(t, 1, state.Depth())
	assert.Equal(t, runtime.SessionStateRunning, state.Status)
	state, actions = state.Apply(runtime.StreamStopped("s", "root", "cancelled"))
	assert.Equal(t, CancelSettled, actions[0].Kind)
	assert.Zero(t, state.Depth())
	assert.Equal(t, runtime.SessionStateSettled, state.Status)
	_, actions = state.Apply(runtime.StreamStopped("s", "root", "normal"))
	assert.Equal(t, TurnSettled, actions[0].Kind, "zero-depth stops still produce an action")
}

func TestPendingAndCompactionActions(t *testing.T) {
	state := State{SessionID: "s"}
	accepted := runtime.PendingUserMessageAccepted("s", "turn", "hello", nil, 0)
	state, actions := state.Apply(accepted)
	assert.Equal(t, PendingAccepted, actions[0].Kind)
	state, _ = state.Apply(accepted)
	assert.Equal(t, []string{"turn"}, state.Pending)
	state, actions = state.Apply(runtime.PendingUserMessagePromoted("s", "missing", "", nil, 0))
	assert.Equal(t, PendingPromoted, actions[0].Kind)
	assert.Equal(t, "missing", actions[0].TurnID)

	_, actions = state.Apply(runtime.SessionCompaction("other", "completed", "root"))
	assert.Equal(t, Action{Kind: CompactionBoundary, Foreign: true, Phase: "completed", Boundary: true}, actions[0])
	_, actions = state.Apply(runtime.SessionCompaction("s", "progress", "root"))
	assert.Equal(t, Action{Kind: CompactionBoundary, Own: true, Phase: "progress"}, actions[0])
}
