package runtime

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionErrorCapacityWordingDistinguishesReason(t *testing.T) {
	assert.Equal(t, "session compact_busy: operation is busy", (&SessionError{Kind: SessionErrorCapacity, Operation: SessionOperationCompactBusy, Reason: SessionErrorReasonBusy, Limit: 8}).Error())
	assert.Equal(t, "session compact_pending: operation has pending input", (&SessionError{Kind: SessionErrorCapacity, Operation: SessionOperationCompactPending, Reason: SessionErrorReasonPending, Limit: 8}).Error())
	assert.Equal(t, "session skill_busy: operation is busy", (&SessionError{Kind: SessionErrorCapacity, Operation: SessionOperationSkillBusy, Reason: SessionErrorReasonBusy, Limit: 8}).Error())
	assert.Equal(t, "session post: capacity limit reached (limit 8)", (&SessionError{Kind: SessionErrorCapacity, Operation: SessionOperationPost, Reason: SessionErrorReasonLimit, Limit: 8}).Error())
}

func TestSessionErrorLegacyCapacityReasonInference(t *testing.T) {
	assert.Equal(t, "session compact_pending: operation is busy", (&SessionError{Kind: SessionErrorCapacity, Operation: SessionOperationCompactPending, Limit: 8}).Error())
	assert.Equal(t, "session post: capacity limit reached (limit 8)", (&SessionError{Kind: SessionErrorCapacity, Operation: SessionOperationPost, Limit: 8}).Error())
}

func TestSessionErrorJSONPreservesOperationAndReasonStrings(t *testing.T) {
	encoded, err := json.Marshal(&SessionError{Kind: SessionErrorCapacity, Operation: SessionOperationPost, Reason: SessionErrorReasonLimit})
	require.NoError(t, err)
	assert.JSONEq(t, `{"Kind":"capacity","SessionID":"","RequestID":"","Operation":"post","reason":"limit","Limit":0}`, string(encoded))
}
