package gemini

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/docker/portcullis"
	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelerrors"
)

// wrapGeminiError wraps a Gemini SDK error in a *modelerrors.StatusError
// to carry HTTP status code metadata for the retry loop, and — for HTTP 400
// rejections — decorates it with a bounded [RequestRejectionCategory] and an
// action-oriented [APIRejectionError] message that also preserves a bounded,
// sanitized rendering of the SDK's own Message (see [sanitizeAPIErrorMessage]).
// The category is derived only from keyword matching against that same
// Message field; Details (the raw response body) is never inspected or
// surfaced.
//
// Gemini's *genai.APIError does not expose *http.Response, so no Retry-After
// header extraction is possible; the RetryAfter field will be zero.
// Non-Gemini errors (e.g. io.EOF, network errors) pass through unchanged.
func wrapGeminiError(err error) error {
	if err == nil {
		return nil
	}
	// google.golang.org/genai's newAPIError returns APIError by value (see
	// its `func (e APIError) Error() string`), never a pointer, so the type
	// parameter here must match a value type. Asserting *genai.APIError
	// would silently never match, letting every Gemini error through
	// unclassified and unwrapped (no StatusCode, no category).
	apiErr, ok := errors.AsType[genai.APIError](err)
	if !ok {
		return err
	}

	wrapped := err
	if apiErr.Code == http.StatusBadRequest {
		category, _ := classifyByMessage(apiErr.Message)
		wrapped = &APIRejectionError{APIError: apiErr, Category: category}
	}

	// Pass nil for resp — Gemini doesn't expose *http.Response.
	return modelerrors.WrapHTTPError(apiErr.Code, nil, wrapped)
}

// APIRejectionError decorates a Gemini HTTP 400 [genai.APIError] with a
// bounded [RequestRejectionCategory] and an action-oriented hint, then
// appends the SDK's own Message — bounded, single-line, JSON-envelope-
// neutralized, and secret-redacted via [sanitizeAPIErrorMessage] — rather
// than replacing it outright. Details (the raw response body, which can
// carry field paths, schemas, or other request-shaped data) is never
// inspected or surfaced. Preserving a safe rendering of Message matters
// beyond this package: general model-error classification (pkg/modelerrors)
// identifies context overflow and retryable transient rejections by
// matching known phrases against err.Error() — replacing Message entirely
// broke both for Gemini 400s (see wrap_test.go's classification-chain
// tests). An auth-shaped 400 (e.g. "API key not valid") isn't affected by
// either classifier: a *modelerrors.StatusError short-circuits before the
// phrase-based fallback that would inspect it. Dropping Message there still
// cost the user the provider's own informative text, leaving only a
// generic "request shape" hint — a loss of visible context, not a
// classifier failure.
type APIRejectionError struct {
	genai.APIError

	Category RequestRejectionCategory
}

func (e *APIRejectionError) Unwrap() error { return e.APIError }

func (e *APIRejectionError) Error() string {
	msg := fmt.Sprintf("provider rejected the request (%s): %s", e.Category, categoryHint(e.Category))
	if detail := sanitizeAPIErrorMessage(e.Message); detail != "" {
		msg += " — provider message: " + detail
	}
	return msg
}

// maxSanitizedAPIErrorMessageBytes bounds the provider message preserved in
// [APIRejectionError.Error], independent of the unrelated field-length bound
// pkg/chat applies to display-name-shaped fields: this is free-form
// provider text, and downstream substring classification (e.g.
// modelerrors' "input token count" / "number of function response parts"
// patterns) needs enough of it intact to keep matching, while still staying
// well short of a provider's full, unbounded response body.
const maxSanitizedAPIErrorMessageBytes = 300

// sanitizeAPIErrorMessage returns a bounded, single-line, secret-redacted,
// JSON-envelope-neutralized rendering of message — intended to be called
// with a Gemini [genai.APIError]'s own Message field, never Details (the
// raw response body). Control characters and newlines are collapsed to
// single spaces, '{' and '}' are rewritten to '(' and ')' so a
// JSON-object-shaped envelope embedded in Message (e.g. a proxy forwarding
// an upstream body verbatim) can't later be re-parsed by
// modelerrors.StatusError.Error (see parseProviderError in pkg/modelerrors)
// and have its own fields substituted for this package's category/hint,
// [portcullis.Redact] scrubs any recognizable credential/secret span, and
// the result is truncated to [maxSanitizedAPIErrorMessageBytes] via
// [chat.TruncateUTF8Bytes]. An empty message returns "".
func sanitizeAPIErrorMessage(message string) string {
	if message == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(message))
	for _, r := range message {
		switch {
		case r < 0x20 || r == 0x7f:
			b.WriteRune(' ')
		case r == '{':
			b.WriteRune('(')
		case r == '}':
			b.WriteRune(')')
		default:
			b.WriteRune(r)
		}
	}
	collapsed := strings.Join(strings.Fields(b.String()), " ")
	redacted := portcullis.Redact(collapsed)
	return chat.TruncateUTF8Bytes(redacted, maxSanitizedAPIErrorMessageBytes)
}

// categoryHint returns a short, safe, actionable explanation for a
// [RequestRejectionCategory]. It never echoes provider-supplied text.
func categoryHint(c RequestRejectionCategory) string {
	switch c {
	case RejectionMissingResponseModalities:
		return "the response may need an explicit output modality (e.g. image) that this request didn't set"
	case RejectionIncompatibleFunctionOrBuiltinTools:
		return "the combination of tools enabled for this request may not be supported by this model"
	case RejectionIncompatibleToolConfig:
		return "the tool-invocation configuration may not be supported by this model"
	case RejectionStructuredOutputConflict:
		return "structured output may conflict with another option in this request"
	case RejectionModelOrAPICapabilityMismatch:
		return "this model may not support a feature enabled for this request (e.g. thinking/reasoning)"
	default:
		return "the request shape may be incompatible with this model or its current configuration"
	}
}
