package runtime

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestRequiredApprovalDurableRecoveryRetryAndSuccessor(t *testing.T) {
	store := coordinationSQLite(t)
	original := session.New(session.WithID("approval-recovery"), session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
	require.NoError(t, store.AddSession(t.Context(), original))
	pending := QueuedMessage{Content: "constrained accepted request", RequestID: "original", RequireApproval: true, InputOrigin: session.InputOriginUser}.sessionMessage()
	pending.Pending, pending.Accepted, pending.TurnID = true, true, "original"
	_, err := store.AddMessage(t.Context(), original.ID, pending)
	require.NoError(t, err)
	// Read through SQLite rather than sharing the first runtime's in-memory input.
	restored, err := store.GetSession(t.Context(), original.ID)
	require.NoError(t, err)
	require.True(t, restored.MessagesSnapshot()[0].Message.RequireApproval)
	// Pending editing must preserve the durable marker as well.
	require.NoError(t, store.(session.PendingMessageEditor).EditPendingUserMessage(t.Context(), original.ID, "original", "edited request"))
	restored, err = store.GetSession(t.Context(), original.ID)
	require.NoError(t, err)
	require.True(t, restored.MessagesSnapshot()[0].Message.RequireApproval)
	r := newDriverTestRuntime(t)
	r.sessionStore = store
	r.sessionDrivers.closed = true // no scheduler; all transitions are deterministic below
	d := newSessionDriver(r, restored)
	defer d.closeOwner()
	require.Len(t, d.pending, 1)
	require.True(t, d.pending[0].RequireApproval)
	ctx, generation, _, err := d.prepareStart(t.Context(), true)
	require.NoError(t, err)
	require.True(t, d.activeRequireApproval)
	execution, _, err := d.executionContext(ctx, generation)
	require.NoError(t, err)
	require.NotNil(t, execution)
	_, _, again := d.finishRun(generation, "")
	require.False(t, again)
	d.wg.Done()
	require.False(t, d.activeRequireApproval, "settlement retires the constraint")
	require.Equal(t, session.SafetyPolicyAutonomous, d.session().GetSafetyPolicy())
	// An explicit retry with no flag still derives the constraint from its input.
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.compactReserved = true; return nil }))
	_, err = d.admitInputReceipt(t.Context(), QueuedMessage{RequestID: "retry", Retry: true}, SessionOperationPost, true)
	require.NoError(t, err)
	require.True(t, d.pending[0].RequireApproval)
	// New ordinary UI input does not inherit the preceding request's constraint.
	_, err = d.admitInputReceipt(t.Context(), QueuedMessage{RequestID: "ui-next", Content: "new UI request"}, SessionOperationPost, true)
	require.NoError(t, err)
	require.False(t, d.pending[1].RequireApproval)
	data, err := json.Marshal(pending)
	require.NoError(t, err)
	var wire session.Message
	require.NoError(t, json.Unmarshal(data, &wire))
	require.True(t, wire.RequireApproval)
}

func TestRequireIdleIsAtomicAndNeverQueuesBehindOwner(t *testing.T) {
	for _, state := range []string{"running", "starting", "settling", "pending", "interaction", "compaction", "pause", "skill"} {
		t.Run(state, func(t *testing.T) {
			r := newDriverTestRuntime(t)
			r.sessionDrivers.closed = true
			d := newSessionDriver(r, session.New(session.WithID("busy")))
			defer d.closeOwner()
			require.NoError(t, d.ownerCall(t.Context(), func() error {
				switch state {
				case "running":
					d.phase = sessionRunning
				case "starting":
					d.phase = sessionStarting
				case "settling":
					d.phase = sessionSettling
				case "pending":
					d.pending = []QueuedMessage{{RequestID: "unrelated"}}
				case "interaction":
					d.interactions["unrelated"] = sessionInteraction{}
				case "compaction":
					d.compactReserved = true
				case "pause":
					d.pauseCh = make(chan struct{})
				case "skill":
					d.skillOperationID = "unrelated"
				}
				return nil
			}))
			before := len(d.pending)
			_, err := d.admitInputReceipt(t.Context(), QueuedMessage{Content: "must not queue", RequestID: "ACP", RequireIdle: true}, SessionOperationPost, true)
			require.ErrorIs(t, err, ErrSessionCapacity)
			require.Len(t, d.pending, before)
			require.Empty(t, d.sess.GetAllMessages())
		})
	}
}
