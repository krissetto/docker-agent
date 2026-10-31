package anthropic

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/modelerrors"
)

// makeTestAnthropicError creates an *anthropic.Error with the given status code and
// optional Retry-After header value for testing.
func makeTestAnthropicError(t *testing.T, statusCode int, retryAfterValue string) *anthropic.Error {
	t.Helper()
	header := http.Header{}
	if retryAfterValue != "" {
		header.Set("Retry-After", retryAfterValue)
	}
	resp := httptest.NewRecorder().Result()
	resp.StatusCode = statusCode
	resp.Header = header
	// anthropic.Error.Error() dereferences Request, so we must provide a non-nil one.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.anthropic.com/v1/messages", http.NoBody)
	return &anthropic.Error{
		StatusCode: statusCode,
		Response:   resp,
		Request:    req,
	}
}

// makeTestSSEAnthropicError simulates the in-band SSE error path: the HTTP
// response was 200 OK but a `type:error` event arrived in the stream, so the
// SDK populated an *anthropic.Error with StatusCode == 200 and a body whose
// `error.type` indicates the actual failure (e.g. "api_error",
// "overloaded_error"). See https://github.com/docker/docker-agent/issues/2870.
func makeTestSSEAnthropicError(t *testing.T, errorType, message string) *anthropic.Error {
	t.Helper()
	resp := httptest.NewRecorder().Result()
	resp.StatusCode = http.StatusOK
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.anthropic.com/v1/messages", http.NoBody)
	body := fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":%q},"request_id":"req_test"}`, errorType, message)
	apiErr := &anthropic.Error{
		StatusCode: http.StatusOK,
		Response:   resp,
		Request:    req,
		RequestID:  "req_test",
	}
	require.NoError(t, apiErr.UnmarshalJSON([]byte(body)))
	return apiErr
}

func TestWrapAnthropicError(t *testing.T) {
	t.Parallel()

	t.Run("nil returns nil", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, wrapAnthropicError(nil))
	})

	t.Run("non-anthropic error passes through unchanged", func(t *testing.T) {
		t.Parallel()
		orig := errors.New("some network error")
		result := wrapAnthropicError(orig)
		assert.Equal(t, orig, result)
		var se *modelerrors.StatusError
		assert.NotErrorAs(t, result, &se)
	})

	t.Run("429 without Retry-After wraps with zero RetryAfter", func(t *testing.T) {
		t.Parallel()
		apiErr := makeTestAnthropicError(t, 429, "")
		result := wrapAnthropicError(apiErr)
		var se *modelerrors.StatusError
		require.ErrorAs(t, result, &se)
		assert.Equal(t, 429, se.StatusCode)
		assert.Equal(t, time.Duration(0), se.RetryAfter)
		// Original error still accessible
		assert.ErrorIs(t, result, apiErr)
	})

	t.Run("429 with Retry-After header sets RetryAfter", func(t *testing.T) {
		t.Parallel()
		apiErr := makeTestAnthropicError(t, 429, "20")
		result := wrapAnthropicError(apiErr)
		var se *modelerrors.StatusError
		require.ErrorAs(t, result, &se)
		assert.Equal(t, 429, se.StatusCode)
		assert.Equal(t, 20*time.Second, se.RetryAfter)
	})

	t.Run("500 wraps with correct status code", func(t *testing.T) {
		t.Parallel()
		apiErr := makeTestAnthropicError(t, 500, "")
		result := wrapAnthropicError(apiErr)
		var se *modelerrors.StatusError
		require.ErrorAs(t, result, &se)
		assert.Equal(t, 500, se.StatusCode)
		assert.Equal(t, time.Duration(0), se.RetryAfter)
	})

	t.Run("wrapped error is classified correctly by ClassifyModelError", func(t *testing.T) {
		t.Parallel()
		apiErr := makeTestAnthropicError(t, 429, "15")
		result := wrapAnthropicError(apiErr)
		retryable, rateLimited, retryAfter := modelerrors.ClassifyModelError(result)
		assert.False(t, retryable)
		assert.True(t, rateLimited)
		assert.Equal(t, 15*time.Second, retryAfter)
	})

	t.Run("wrapped in fmt.Errorf still classified correctly", func(t *testing.T) {
		t.Parallel()
		apiErr := makeTestAnthropicError(t, 429, "5")
		wrapped := fmt.Errorf("stream error: %w", wrapAnthropicError(apiErr))
		retryable, rateLimited, retryAfter := modelerrors.ClassifyModelError(wrapped)
		assert.False(t, retryable)
		assert.True(t, rateLimited)
		assert.Equal(t, 5*time.Second, retryAfter)
	})

	// Issue #2870: SSE in-band errors arrive as *anthropic.Error with HTTP 200.
	// We must synthesize a sensible HTTP status from the body's error.type so
	// the generic retry/format pipeline kicks in and the user sees a friendly
	// message instead of the raw `200 {"type":"error",...}` blob.
	t.Run("sse in-band api_error becomes retryable HTTP 500", func(t *testing.T) {
		t.Parallel()
		apiErr := makeTestSSEAnthropicError(t, "api_error", "Internal server error")
		result := wrapAnthropicError(apiErr)
		var se *modelerrors.StatusError
		require.ErrorAs(t, result, &se)
		assert.Equal(t, http.StatusInternalServerError, se.StatusCode)
		retryable, rateLimited, _ := modelerrors.ClassifyModelError(result)
		assert.True(t, retryable, "api_error in SSE stream must be retryable")
		assert.False(t, rateLimited)
		// The user-facing message must surface error.type and error.message,
		// not the raw "200 {...}" SDK blob.
		assert.Contains(t, se.Error(), "api_error: Internal server error")
		assert.NotContains(t, se.Error(), ": 200")
	})

	t.Run("sse in-band overloaded_error becomes retryable HTTP 529", func(t *testing.T) {
		t.Parallel()
		apiErr := makeTestSSEAnthropicError(t, "overloaded_error", "Anthropic is overloaded")
		result := wrapAnthropicError(apiErr)
		var se *modelerrors.StatusError
		require.ErrorAs(t, result, &se)
		assert.Equal(t, 529, se.StatusCode)
		retryable, _, _ := modelerrors.ClassifyModelError(result)
		assert.True(t, retryable)
	})

	t.Run("sse in-band rate_limit_error becomes rate-limited HTTP 429", func(t *testing.T) {
		t.Parallel()
		apiErr := makeTestSSEAnthropicError(t, "rate_limit_error", "Slow down")
		result := wrapAnthropicError(apiErr)
		var se *modelerrors.StatusError
		require.ErrorAs(t, result, &se)
		assert.Equal(t, http.StatusTooManyRequests, se.StatusCode)
		retryable, rateLimited, _ := modelerrors.ClassifyModelError(result)
		assert.False(t, retryable)
		assert.True(t, rateLimited)
	})

	t.Run("sse in-band authentication_error is not retryable", func(t *testing.T) {
		t.Parallel()
		apiErr := makeTestSSEAnthropicError(t, "authentication_error", "Invalid API key")
		result := wrapAnthropicError(apiErr)
		var se *modelerrors.StatusError
		require.ErrorAs(t, result, &se)
		assert.Equal(t, http.StatusUnauthorized, se.StatusCode)
		retryable, rateLimited, _ := modelerrors.ClassifyModelError(result)
		assert.False(t, retryable, "auth errors must not be retried")
		assert.False(t, rateLimited)
	})

	t.Run("sse in-band unknown error type defaults to retryable HTTP 500", func(t *testing.T) {
		t.Parallel()
		apiErr := makeTestSSEAnthropicError(t, "some_new_error_type", "unknown")
		result := wrapAnthropicError(apiErr)
		var se *modelerrors.StatusError
		require.ErrorAs(t, result, &se)
		assert.Equal(t, http.StatusInternalServerError, se.StatusCode)
		retryable, _, _ := modelerrors.ClassifyModelError(result)
		assert.True(t, retryable, "unknown SSE errors should be treated as transient")
	})
}
