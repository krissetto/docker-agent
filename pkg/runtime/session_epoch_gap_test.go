package runtime

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSessionEventHubRestartAheadCursorRequiresFreshSnapshot(t *testing.T) {
	// A new hub is the journal epoch after process restart, before any publish.
	hub := newSessionEventHubWithLimits(4, 4096)
	since := uint64(99)
	replay, _, cancel, cursor := hub.SubscribeSequenced("s", &since, 1)
	cancel()
	require.Len(t, replay, 1)
	assert.True(t, replay[0].Gap)
	assert.Equal(t, uint64(0), cursor)
	_, events, cancel, cursor := hub.SubscribeSequenced("s", nil, 1)
	defer cancel()
	assert.Zero(t, cursor)
	hub.Publish("s", AgentChoice("root", "s", "after restart"))
	assert.Equal(t, uint64(1), (<-events).Sequence)
}
