package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func completeTiming(turnID string) TurnTiming {
	return TurnTiming{Version: TurnTimingVersion, Basis: TurnTimingBasisActiveWall, TurnID: turnID, DurationMs: 1500, WaitingMs: 500, Outcome: TurnTimingOutcomeCompleted}
}

func TestSetTurnTimingRejectsIncompleteRecords(t *testing.T) {
	t.Parallel()
	s := New(WithID("s"))
	valid := completeTiming("turn")
	for name, timing := range map[string]TurnTiming{
		"wrong turn id":   {Version: 1, Basis: TurnTimingBasisActiveWall, TurnID: "other", DurationMs: 1, Outcome: "completed"},
		"unknown version": {Version: 99, Basis: TurnTimingBasisActiveWall, TurnID: "turn", DurationMs: 1, Outcome: "completed"},
		"unknown basis":   {Version: 1, Basis: "sum_of_work", TurnID: "turn", DurationMs: 1, Outcome: "completed"},
		"missing outcome": {Version: 1, Basis: TurnTimingBasisActiveWall, TurnID: "turn", DurationMs: 1},
		"unknown outcome": {Version: 1, Basis: TurnTimingBasisActiveWall, TurnID: "turn", DurationMs: 1, Outcome: "stopped"},
		"negative wait":   {Version: 1, Basis: TurnTimingBasisActiveWall, TurnID: "turn", DurationMs: 1, WaitingMs: -1, Outcome: "completed"},
	} {
		t.Run(name, func(t *testing.T) {
			s.SetTurnTiming("turn", timing)
			_, ok := s.TurnTiming("turn")
			assert.False(t, ok)
		})
	}
	s.SetTurnTiming("turn", valid)
	got, ok := s.TurnTiming("turn")
	require.True(t, ok)
	assert.Equal(t, valid, got)
}

func TestTurnTimingUnknownTurnIsAbsent(t *testing.T) {
	t.Parallel()
	s := New(WithID("s"))
	s.SetTurnTiming("turn", completeTiming("turn"))
	_, ok := s.TurnTiming("missing")
	assert.False(t, ok)
}

// Legacy execution settings predate timing and must decode without inventing any.
func TestLegacyExecutionSettingsDecodeWithoutTiming(t *testing.T) {
	t.Parallel()
	s := New(WithID("s"))
	require.NoError(t, decodeExecutionSettings(s, `{"turn_outcomes":{"turn":"completed"},"turn_outcome_order":["turn"]}`))
	assert.Equal(t, "completed", s.TurnOutcome("turn"))
	_, ok := s.TurnTiming("turn")
	assert.False(t, ok)
}

func TestExecutionSettingsRoundTripKeepsTiming(t *testing.T) {
	t.Parallel()
	s := New(WithID("s"))
	s.SetTurnOutcome("turn", "completed")
	s.SetTurnTiming("turn", completeTiming("turn"))
	encoded, err := encodeExecutionSettings(s)
	require.NoError(t, err)

	reloaded := New(WithID("s"))
	require.NoError(t, decodeExecutionSettings(reloaded, encoded))
	got, ok := reloaded.TurnTiming("turn")
	require.True(t, ok)
	assert.Equal(t, completeTiming("turn"), got)
}

func TestTurnTimingJSONRoundTrip(t *testing.T) {
	t.Parallel()
	timing := completeTiming("turn")
	data, err := json.Marshal(timing)
	require.NoError(t, err)
	var decoded TurnTiming
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, timing, decoded)

	untimedWait, err := json.Marshal(TurnTiming{Version: 1, Basis: TurnTimingBasisActiveWall, TurnID: "turn", DurationMs: 10, Outcome: "failed"})
	require.NoError(t, err)
	assert.NotContains(t, string(untimedWait), "waiting_ms")
}
