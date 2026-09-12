package runtime

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeSessionHTTPErrorPreservesStructuredSessionKind(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusConflict, Body: io.NopCloser(strings.NewReader(`{"error":"wrong_session","operation":"respond","session_id":"s"}`))}
	err := decodeSessionHTTPError(resp)
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionErrorWrongSession, sessionErr.Kind)
	assert.Equal(t, SessionOperationRespond, sessionErr.Operation)
	assert.Equal(t, "s", sessionErr.SessionID)
}

func TestDecodeSessionHTTPErrorPreservesArbitraryOperationAndReason(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(`{"error":"capacity","operation":"future_operation","reason":"pending","session_id":"s"}`))}
	err := decodeSessionHTTPError(resp)
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionOperation("future_operation"), sessionErr.Operation)
	assert.Equal(t, SessionErrorReasonPending, sessionErr.Reason)
	assert.Equal(t, "session future_operation: operation has pending input", sessionErr.Error())
}

func TestDecodeSessionHTTPErrorInfersLegacyMissingReason(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(`{"error":"capacity","operation":"compact_busy","session_id":"s"}`))}
	err := decodeSessionHTTPError(resp)
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Empty(t, sessionErr.Reason)
	assert.Equal(t, "session compact_busy: operation is busy", sessionErr.Error())
}
