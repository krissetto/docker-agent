package anthropic

import (
	"errors"
	"net/http"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/shared"

	"github.com/docker/docker-agent/pkg/modelerrors"
)

// wrapAnthropicError wraps an Anthropic SDK error in a *modelerrors.StatusError
// to carry HTTP status code and Retry-After metadata for the retry loop.
// Non-Anthropic errors (e.g. io.EOF, network errors) pass through unchanged.
//
// Anthropic streams reply with HTTP 200 even when an error occurs mid-stream:
// the SSE stream contains a `type: error` event whose body looks like
//
//	{"type":"error","error":{"type":"api_error","message":"Internal server error"}}
//
// In that case the SDK builds an *anthropic.Error with StatusCode == 200, which
// would short-circuit WrapHTTPError and surface the raw SDK message to the
// user. We map the in-band error type to its closest HTTP equivalent so the
// generic retry/format pipeline (modelerrors.ClassifyModelError, StatusError)
// behaves the same as for transport-level errors.
func wrapAnthropicError(err error) error {
	if err == nil {
		return nil
	}
	apiErr, ok := errors.AsType[*anthropic.Error](err)
	if !ok {
		return err
	}
	statusCode := apiErr.StatusCode
	if statusCode < 400 {
		statusCode = statusCodeForAnthropicErrorType(apiErr.Type())
	}
	return modelerrors.WrapHTTPError(statusCode, apiErr.Response, err)
}

// statusCodeForAnthropicErrorType maps an Anthropic in-band SSE error type
// (see shared.ErrorType) to the HTTP status code with the same retry/fallback
// semantics. Unknown or empty types fall back to 500 so the error is treated
// as a transient server error and retried.
func statusCodeForAnthropicErrorType(t shared.ErrorType) int {
	switch t {
	case shared.ErrorTypeOverloadedError:
		return 529 // Anthropic's documented overloaded code; retryable.
	case shared.ErrorTypeRateLimitError:
		return http.StatusTooManyRequests
	case shared.ErrorTypeTimeoutError:
		return http.StatusGatewayTimeout
	case shared.ErrorTypeAuthenticationError:
		return http.StatusUnauthorized
	case shared.ErrorTypePermissionError:
		return http.StatusForbidden
	case shared.ErrorTypeNotFoundError:
		return http.StatusNotFound
	case shared.ErrorTypeBillingError:
		return http.StatusPaymentRequired
	case shared.ErrorTypeInvalidRequestError:
		return http.StatusBadRequest
	case shared.ErrorTypeAPIError:
		return http.StatusInternalServerError
	}
	return http.StatusInternalServerError
}
