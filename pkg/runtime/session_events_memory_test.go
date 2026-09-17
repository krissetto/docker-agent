package runtime

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReplayEvictionClearsRetiredCorrelationOwnership(t *testing.T) {
	h := newSessionEventHubWithLimits(1, 1<<20)
	first := retainedSessionEvent{sequence: 1, requestID: "retired-request", interactionID: "retired-interaction", generation: 3, event: AgentChoice("root", "s", "first"), bytes: 10}
	backing := make([]retainedSessionEvent, 1, 2)
	backing[0] = first
	h.replay["s"], h.bytes["s"] = backing, first.bytes
	second := retainedSessionEvent{sequence: 2, requestID: "live-request", interactionID: "live-interaction", generation: 4, event: AgentChoice("root", "s", "second"), bytes: 20}
	h.appendReplayLocked("s", second)
	require.Equal(t, retainedSessionEvent{}, backing[0], "retired backing must retain neither payload nor correlation strings")
	require.Equal(t, []retainedSessionEvent{second}, h.replay["s"])
	require.Equal(t, second.bytes, h.bytes["s"])
	replay := h.replayLocked("s", 0)
	require.Len(t, replay, 2)
	require.True(t, replay[0].Gap)
	require.EqualValues(t, 2, replay[0].FirstAvailable)
	require.Equal(t, SequencedSessionEvent{Sequence: 2, RequestID: second.requestID, InteractionID: second.interactionID, Event: second.event}, replay[1])
}
