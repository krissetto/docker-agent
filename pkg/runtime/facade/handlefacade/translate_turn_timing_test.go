package handlefacade

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/facade"
	"github.com/docker/docker-agent/pkg/session"
)

// Settlement timing is copied from the runtime event and never synthesized.
func TestTranslateCarriesTurnSettledTiming(t *testing.T) {
	t.Parallel()
	timing := session.TurnTiming{Version: session.TurnTimingVersion, Basis: session.TurnTimingBasisActiveWall, TurnID: "t", DurationMs: 1200, WaitingMs: 300, Outcome: session.TurnTimingOutcomeCompleted}
	tr := newTranslator("s")
	out := tr.translate(runtime.SessionEvent{SessionID: "s", TurnID: "t", Sequence: 1, Event: &runtime.TurnSettledEvent{Type: "turn_settled", SessionID: "s", TurnID: "t", Outcome: runtime.TurnCompleted, Timing: &timing}})
	require.Len(t, out, 1)
	settled, ok := out[0].Payload.(facade.TurnSettled)
	require.True(t, ok)
	require.NotNil(t, settled.Timing)
	assert.Equal(t, timing, *settled.Timing)

	untimed := tr.translate(runtime.SessionEvent{SessionID: "s", TurnID: "u", Sequence: 2, Event: &runtime.TurnSettledEvent{Type: "turn_settled", SessionID: "s", TurnID: "u", Outcome: runtime.TurnCompleted}})
	require.Len(t, untimed, 1)
	assert.Nil(t, untimed[0].Payload.(facade.TurnSettled).Timing)
}
