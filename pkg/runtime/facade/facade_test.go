package facade

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Payload implementations are the closed set the facade event envelope accepts.
var (
	_ Payload = TurnStarted{}
	_ Payload = TurnSettled{}
	_ Payload = InteractionRequested{}
	_ Payload = InteractionResolved{}
	_ Payload = QueueAccepted{}
	_ Payload = QueueWithdrawn{}
	_ Payload = QueueConsumed{}
	_ Payload = SubagentStatus{}
	_ Payload = Gap{}
	_ Payload = Passthrough{}
)

func TestSentinelErrorsAreDistinctAndMatchable(t *testing.T) {
	t.Parallel()

	sentinels := []error{
		ErrUnknownInteraction,
		ErrStaleInteraction,
		ErrAmbiguousInteraction,
		ErrNotActive,
		ErrInvalidResponse,
		ErrUnsupported,
		ErrSessionClosed,
		ErrBusy,
		ErrUnknownAgent,
	}
	seen := make(map[string]struct{}, len(sentinels))
	for _, err := range sentinels {
		require.Error(t, err)
		_, dup := seen[err.Error()]
		assert.False(t, dup, "duplicate sentinel message %q", err.Error())
		seen[err.Error()] = struct{}{}
	}
}

func TestWireVocabularyValues(t *testing.T) {
	t.Parallel()

	// These strings cross the consumer boundary; changing one is a contract change.
	assert.Equal(t, "accepted", string(CancelAccepted))
	assert.Equal(t, "already_cancelling", string(CancelAlreadyCancelling))
	assert.Equal(t, "not_active", string(CancelNotActive))

	assert.Equal(t, "completed", string(OutcomeCompleted))
	assert.Equal(t, "canceled", string(OutcomeCanceled))
	assert.Equal(t, "failed", string(OutcomeFailed))

	assert.Equal(t, "withdrawn", string(WithdrawWithdrawn))
	assert.Equal(t, "not_withdrawn", string(WithdrawNotWithdrawn))
	assert.Equal(t, "consumed", string(WithdrawConsumed))

	assert.Equal(t, "edited", string(EditEdited))
	assert.Equal(t, "stale", string(EditStale))

	assert.Equal(t, "confirmation", string(InteractionConfirmation))
	assert.Equal(t, "max_iterations", string(InteractionMaxIterations))
	assert.Equal(t, "elicitation", string(InteractionElicitation))

	assert.Equal(t, "approve", string(DecisionApprove))
	assert.Equal(t, "approve-balanced", string(DecisionApproveBalanced))
	assert.Equal(t, "approve-autonomous", string(DecisionApproveAutonomous))
	assert.Equal(t, "approve-tool", string(DecisionApproveTool))
	assert.Equal(t, "reject", string(DecisionReject))

	assert.Equal(t, "responded", string(ResolutionResponded))
	assert.Equal(t, "canceled", string(ResolutionCanceled))
	assert.Equal(t, "stopped", string(ResolutionStopped))
}

func TestZeroCapabilitiesAdvertiseNothing(t *testing.T) {
	t.Parallel()

	var info Info
	assert.Equal(t, Capabilities{}, info.Capabilities)
}

func TestTurnSettledCarriesOutcome(t *testing.T) {
	t.Parallel()

	ev := Event{TurnID: "t1", Seq: 7, Payload: TurnSettled{Outcome: Outcome{Kind: OutcomeFailed, Message: "boom"}}}
	settled, ok := ev.Payload.(TurnSettled)
	require.True(t, ok)
	assert.Equal(t, OutcomeFailed, settled.Outcome.Kind)
	assert.Equal(t, "boom", settled.Outcome.Message)
	assert.Empty(t, settled.Outcome.Reason)
}
