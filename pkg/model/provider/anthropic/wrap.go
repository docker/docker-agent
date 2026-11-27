package anthropic

import (
	"errors"
	"net/http"

	"github.com/docker/docker-agent/pkg/modelerrors"
)

// wrapAnthropicError wraps an Anthropic APIError in a *modelerrors.StatusError
// to carry HTTP status code and Retry-After metadata for the retry loop.
// Non-Anthropic errors (e.g. io.EOF, network errors) pass through unchanged.
//
// Anthropic streams reply with HTTP 200 even when an error occurs mid-stream:
// the SSE stream contains a `type: error` event whose body looks like
//
//	{"type":"error","error":{"type":"api_error","message":"Internal server error"}}
//
// We map the in-band error type to its closest HTTP equivalent so the
// generic retry/format pipeline (modelerrors.ClassifyModelError, StatusError)
// behaves the same as for transport-level errors.
func wrapAnthropicError(err error) error {
	if err == nil {
		return nil
	}
	apiErr, ok := errors.AsType[*APIError](err)
	if !ok {
		return err
	}
	statusCode := apiErr.StatusCode
	if statusCode < 400 {
		statusCode = statusCodeForAnthropicErrorType(apiErr.Type())
	}
	return modelerrors.WrapHTTPError(statusCode, apiErr.Response, err)
}

func statusCodeForAnthropicErrorType(t ErrorType) int {
	switch t {
	case ErrorTypeOverloadedError:
		return 529 // Anthropic's documented overloaded code; retryable.
	case ErrorTypeRateLimitError:
		return http.StatusTooManyRequests
	case ErrorTypeTimeoutError:
		return http.StatusGatewayTimeout
	case ErrorTypeAuthenticationError:
		return http.StatusUnauthorized
	case ErrorTypePermissionError:
		return http.StatusForbidden
	case ErrorTypeNotFoundError:
		return http.StatusNotFound
	case ErrorTypeBillingError:
		return http.StatusPaymentRequired
	case ErrorTypeInvalidRequestError:
		return http.StatusBadRequest
	case ErrorTypeAPIError:
		return http.StatusInternalServerError
	}
	return http.StatusInternalServerError
}
