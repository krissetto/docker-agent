package runtime

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// TestSteerAcceptanceDoesNotPromiseStillPending pins the Steer contract. Steer
// returns once the guidance is durably accepted. Consumption happens at the
// next safe boundary, which can be as soon as the in-flight model attempt is
// interrupted, so callers must not assume the row is still pending on return.
// Once consumed, the id acknowledges AwaitTurn, withdrawal reports false
// without error, and edits fail with a typed stale error.
func TestSteerAcceptanceDoesNotPromiseStillPending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		var restarted []chat.Message
		provider := coordinationReply("done")
		provider.call = func(ctx context.Context, messages []chat.Message) (chat.MessageStream, error) {
			switch calls.Add(1) {
			case 1:
				// Hold the first attempt open until steering interrupts it at the boundary.
				return &steeringBoundaryStream{done: ctx.Done(), err: ctx.Err, chunks: newStreamBuilder().AddContent("partial").responses, release: make(chan struct{})}, nil
			case 2:
				restarted = messages
			}
			return newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build(), nil
		}
		store := session.NewInMemorySessionStore()
		_, owner := coordinationRuntime(t, store, provider, coordinationReply("unused"))
		h := coordinationCreate(t, owner.Runtime(), "steer-contract", "")
		active, err := h.Submit(t.Context(), TurnInput{Content: "active", RequestID: "active"})
		require.NoError(t, err)
		synctest.Wait()

		guide, err := h.Steer(t.Context(), TurnInput{Content: "user guidance", RequestID: "guide"})
		require.NoError(t, err)
		assert.NotEmpty(t, guide.TurnID, "Steer returns a server-assigned turn id for the accepted guidance")
		assert.Contains(t, []SubmissionDisposition{"", SubmissionDispositionQueued}, guide.Disposition,
			"queued means still pending at return; empty means already delivered or not queued")

		coordinationAwait(t, h, guide.TurnID)
		coordinationAwait(t, h, active.TurnID)
		coordinationSettled(t, h)
		assert.Equal(t, int32(2), calls.Load(), "consumed guidance restarts the interrupted attempt once")
		var delivered []string
		for _, message := range restarted {
			delivered = append(delivered, message.Content)
		}
		assert.Contains(t, delivered, "user guidance", "guidance consumed at the boundary is in the restarted request")

		withdrawn, err := h.(*sessionHandle).CancelPendingMessage(t.Context(), guide.TurnID)
		require.NoError(t, err, "withdrawing a consumed row is a no-op, not an error")
		assert.False(t, withdrawn)

		_, err = h.Edit(t.Context(), SessionEdit{Kind: SessionEditPendingMessage, PendingMessage: &PendingMessageEdit{TurnID: guide.TurnID, Content: "edited after delivery"}})
		assertSessionContractError(t, err, SessionErrorStale, "edit_pending_message")

		snapshot, err := h.Snapshot(t.Context())
		require.NoError(t, err)
		var found bool
		for _, item := range snapshot.MessagesSnapshot() {
			if item.Message != nil && item.Message.TurnID == guide.TurnID {
				found = true
				assert.False(t, item.Message.Pending, "consumed guidance is no longer pending")
				assert.Equal(t, "user guidance", item.Message.Message.Content)
			}
		}
		assert.True(t, found, "consumed guidance stays in the transcript")
	})
}
