package gemini

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/docker/portcullis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/internal/portcullistest"
	"github.com/docker/docker-agent/pkg/modelerrors"
)

// TestWrapGeminiError_ValueTypeAPIError is the regression for the pointer-vs-
// value bug: google.golang.org/genai returns APIError by value (its Error()
// method has a value receiver), so a type assertion for *genai.APIError never
// matches and every Gemini API error previously passed through unwrapped,
// with no *modelerrors.StatusError and no category. This reproduces the
// exact shape observed for a Gemini 400 with an empty body (empty Message,
// empty Details) and verifies it is now correctly classified.
func TestWrapGeminiError_ValueTypeAPIError(t *testing.T) {
	t.Parallel()

	apiErr := genai.APIError{Code: http.StatusBadRequest, Status: "400 Bad Request"}

	wrapped := wrapGeminiError(apiErr)

	var statusErr *modelerrors.StatusError
	require.ErrorAs(t, wrapped, &statusErr, "a Gemini APIError must be classified as a *modelerrors.StatusError")
	assert.Equal(t, http.StatusBadRequest, statusErr.StatusCode)
	assert.Equal(t, 0, int(statusErr.RetryAfter), "Gemini exposes no *http.Response, so Retry-After must stay zero")

	var rejErr *APIRejectionError
	require.ErrorAs(t, wrapped, &rejErr, "a 400 must be decorated with *APIRejectionError")
	assert.Equal(t, RejectionOther, rejErr.Category, "an empty message has no keyword to classify against")
}

// TestWrapGeminiError_NonBadRequestPassesThroughRawMessage verifies only 400s
// get the action-oriented override; other status codes keep the SDK's own
// Error() text so existing retry/rate-limit classification (which inspects
// the message for patterns like "429") keeps working unchanged.
func TestWrapGeminiError_NonBadRequestPassesThroughRawMessage(t *testing.T) {
	t.Parallel()

	apiErr := genai.APIError{Code: http.StatusTooManyRequests, Status: "429 Too Many Requests", Message: "rate limit exceeded"}

	wrapped := wrapGeminiError(apiErr)

	var statusErr *modelerrors.StatusError
	require.ErrorAs(t, wrapped, &statusErr)
	assert.Equal(t, http.StatusTooManyRequests, statusErr.StatusCode)

	var rejErr *APIRejectionError
	assert.NotErrorAs(t, wrapped, &rejErr, "only 400s are decorated with APIRejectionError")

	_, rateLimited, _ := modelerrors.ClassifyModelError(wrapped)
	assert.True(t, rateLimited, "429 classification must keep working after the fix")
}

// TestWrapGeminiError_NonGeminiErrorPassesThrough verifies errors unrelated
// to the Gemini SDK (network errors, io.EOF, other providers) are returned
// unchanged.
func TestWrapGeminiError_NonGeminiErrorPassesThrough(t *testing.T) {
	t.Parallel()

	original := errors.New("boom")
	assert.Same(t, original, wrapGeminiError(original))

	assert.NoError(t, wrapGeminiError(nil))
}

// TestAPIRejectionError_RedactsSecretsAndNeverLeaksDetails is the core safety
// regression for the preserved-message design: APIRejectionError.Error() now
// deliberately echoes a bounded, sanitized rendering of the SDK's own
// Message (see [sanitizeAPIErrorMessage]) so downstream classification keeps
// working, but it must still redact any real credential/secret span
// portcullis recognizes within that Message, and it must never surface
// Details (the raw response body) at all, however sensitive-looking its
// contents.
func TestAPIRejectionError_RedactsSecretsAndNeverLeaksDetails(t *testing.T) {
	t.Parallel()

	secret := portcullistest.FakeGitHubPAT("1234567890abcdefghijklmnopqrst")

	apiErr := genai.APIError{
		Code:    http.StatusBadRequest,
		Status:  "400 Bad Request",
		Message: "invalid argument: token " + secret + " is not authorized",
		Details: []map[string]any{{"reason": secret, "path": "/some/local/path"}},
	}

	wrapped := wrapGeminiError(apiErr)
	visible := wrapped.Error()

	assert.NotContains(t, visible, secret, "a real credential in Message must be redacted")
	assert.Contains(t, visible, portcullis.Marker, "the redaction marker must take its place")
	assert.Contains(t, visible, "invalid argument", "surrounding message text must be preserved for classification")
	assert.NotContains(t, visible, "/some/local/path", "Details must never be surfaced")
	assert.NotContains(t, visible, "Details:")
}

// TestWrapGeminiError_FormatErrorIsActionableAndSafe is the end-to-end
// regression for the runtime/TUI-visible error seam: it drives a wrapped
// 400 through the same modelerrors.FormatError call the runtime loop uses
// to build ErrorEvent.Error (pkg/runtime/loop_steps.go), which the TUI
// renders verbatim (pkg/tui/page/chat/runtime_events.go). Before this
// classification, an empty-body Gemini 400 produced just "HTTP 400: " with
// no actionable content; this proves the visible message now names a
// bounded category and hint, keeps the sanitized provider message visible,
// and still redacts a real credential found within it.
func TestWrapGeminiError_FormatErrorIsActionableAndSafe(t *testing.T) {
	t.Parallel()

	secret := portcullistest.FakeGitHubPAT("abcdefghijklmnopqrstuvwxyz0123")

	apiErr := genai.APIError{
		Code:    http.StatusBadRequest,
		Status:  "400 Bad Request",
		Message: "response_modalities: token " + secret + " rejected",
	}

	visible := modelerrors.FormatError(wrapGeminiError(apiErr))

	assert.Contains(t, visible, string(RejectionMissingResponseModalities))
	assert.Contains(t, visible, "output modality")
	assert.Contains(t, visible, "response_modalities", "the sanitized provider message must stay visible for classification/diagnosis")
	assert.NotContains(t, visible, secret, "a real credential must be redacted even when the surrounding message is preserved")
	assert.NotContains(t, visible, "\n", "the visible message must stay a single line")
}

// TestWrapGeminiError_ContextOverflowClassificationSurvivesWrapping is the
// blocking regression for context-overflow classification: Gemini reports
// token-window overflow as an HTTP 400 whose message contains "input token
// count" (see modelerrors' tokenOverflowPatterns). Replacing that message
// with fixed category text made every Gemini overflow invisible to
// modelerrors.IsContextOverflowError; preserving a sanitized rendering of it
// restores auto-compaction triggering.
func TestWrapGeminiError_ContextOverflowClassificationSurvivesWrapping(t *testing.T) {
	t.Parallel()

	apiErr := genai.APIError{
		Code:    http.StatusBadRequest,
		Status:  "400 Bad Request",
		Message: "The input token count (123456) exceeds the maximum number of tokens allowed (100000) for this model.",
	}

	wrapped := wrapGeminiError(apiErr)

	require.True(t, modelerrors.IsContextOverflowError(wrapped),
		"a preserved, sanitized provider message must still let modelerrors recognize a Gemini context overflow")
	assert.Equal(t, modelerrors.OverflowKindTokens, modelerrors.OverflowKindOf(wrapped))
}

// TestWrapGeminiError_TransientFunctionResponseIsRetryable is the blocking
// regression for issue #2683: Vertex/Gemini sometimes returns a transient
// HTTP 400 whose message says the number of function response parts must
// equal the number of function call parts, and modelerrors treats that
// specific message as retryable (matchesTransientPattern) even though 400s
// are otherwise non-retryable. Replacing the message broke this override for
// Gemini; preserving it restores same-model retry instead of an immediate,
// unnecessary fallback/failure.
func TestWrapGeminiError_TransientFunctionResponseIsRetryable(t *testing.T) {
	t.Parallel()

	apiErr := genai.APIError{
		Code:   http.StatusBadRequest,
		Status: "400 Bad Request",
		Message: "Please ensure that the number of function response parts is equal to the number of " +
			"function call parts of the function call turn.",
	}

	wrapped := wrapGeminiError(apiErr)

	retryable, rateLimited, _ := modelerrors.ClassifyModelError(wrapped)
	assert.True(t, retryable, "issue #2683's transient function-response 400 must stay retryable after classification")
	assert.False(t, rateLimited)
}

// TestWrapGeminiError_AuthMessageStaysInformative is the blocking regression
// for auth-shaped 400s: Gemini reports an invalid/missing API key as an HTTP
// 400 with an informative message rather than a 401. No documented request
// field keyword matches it, so it classifies as [RejectionOther]. The
// regression here is loss of visible context, not a broken classifier —
// modelerrors never runs auth-phrase classification on this error at all,
// since its *StatusError short-circuits that fallback — so the requirement
// is simply that the visible error keeps surfacing the provider's own "API
// key not valid" text instead of a generic, uninformative "request shape"
// hint.
func TestWrapGeminiError_AuthMessageStaysInformative(t *testing.T) {
	t.Parallel()

	apiErr := genai.APIError{
		Code:    http.StatusBadRequest,
		Status:  "400 Bad Request",
		Message: "API key not valid. Please pass a valid API key.",
	}

	wrapped := wrapGeminiError(apiErr)
	visible := modelerrors.FormatError(wrapped)

	assert.Contains(t, visible, "API key not valid",
		"an auth-shaped 400 must stay informative instead of being replaced by a generic request-shape hint")
	assert.Contains(t, visible, string(RejectionOther), "no documented field keyword matches an auth message")
}

// TestSanitizeAPIErrorMessage covers the bounds this diagnostics feature
// relies on for safety: control characters and newlines collapse to a single
// line, recognizable secrets are redacted, and the output never exceeds
// [maxSanitizedAPIErrorMessageBytes].
func TestSanitizeAPIErrorMessage(t *testing.T) {
	t.Parallel()

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, sanitizeAPIErrorMessage(""))
	})

	t.Run("collapses control characters and newlines to a single line", func(t *testing.T) {
		t.Parallel()
		got := sanitizeAPIErrorMessage("line one\r\nline\ttwo\x00\x1f")
		assert.NotContains(t, got, "\n")
		assert.NotContains(t, got, "\r")
		assert.NotContains(t, got, "\t")
		assert.Equal(t, "line one line two", got)
	})

	t.Run("redacts a recognizable secret", func(t *testing.T) {
		t.Parallel()
		secret := portcullistest.FakeGitHubPAT("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzz")
		got := sanitizeAPIErrorMessage("token " + secret + " rejected")
		assert.NotContains(t, got, secret)
		assert.Contains(t, got, portcullis.Marker)
		assert.Contains(t, got, "rejected")
	})

	t.Run("bounds output length", func(t *testing.T) {
		t.Parallel()
		got := sanitizeAPIErrorMessage(strings.Repeat("a", maxSanitizedAPIErrorMessageBytes*2))
		assert.LessOrEqual(t, len(got), maxSanitizedAPIErrorMessageBytes)
	})

	t.Run("neutralizes JSON-object braces so a downstream reparse can't hijack the message", func(t *testing.T) {
		t.Parallel()
		got := sanitizeAPIErrorMessage(`prefix {"error":{"message":"x","status":"UNAUTHENTICATED"}} suffix`)
		assert.NotContains(t, got, "{")
		assert.NotContains(t, got, "}")
		assert.Contains(t, got, `("error":("message":"x","status":"UNAUTHENTICATED"))`)
	})
}

// TestWrapGeminiError_JSONShapedMessageEnvelopeStaysInertAndSafe is the
// regression for modelerrors.StatusError re-parsing a JSON-object-shaped
// envelope embedded in the SDK's own Message (e.g. a proxy forwarding an
// upstream body verbatim): parseProviderError
// (pkg/modelerrors/modelerrors.go) scans an error's full Error() string for
// the first "{...}" object and, if it looks like a provider error body,
// replaces the ENTIRE message with just that body's fields — silently
// dropping this package's category/hint. It drives both
// [*APIRejectionError.Error] directly and the full
// wrapGeminiError -> StatusError.Error -> modelerrors.FormatError seam, and
// requires the category/hint survive, no literal brace reach the visible
// text, a real credential embedded inside the envelope still be redacted,
// and Details never surface — JSON-shaped or not.
func TestWrapGeminiError_JSONShapedMessageEnvelopeStaysInertAndSafe(t *testing.T) {
	t.Parallel()

	secret := portcullistest.FakeGitHubPAT("0123456789abcdefghijklmnopqrst")
	apiErr := genai.APIError{
		Code:   http.StatusBadRequest,
		Status: "400 Bad Request",
		Message: `Upstream gateway returned {"error":{"message":"token ` + secret +
			` rejected","status":"UNAUTHENTICATED"}} while forwarding the request`,
		Details: []map[string]any{{"secret": secret, "path": "/etc/passwd"}},
	}

	wrapped := wrapGeminiError(apiErr)

	for _, visible := range []string{wrapped.Error(), modelerrors.FormatError(wrapped)} {
		assert.Contains(t, visible, string(RejectionOther),
			"the category must survive an embedded JSON-shaped envelope in Message")
		assert.Contains(t, visible, "request shape may be incompatible",
			"the hint must survive, not be replaced by modelerrors reparsing the embedded JSON body")
		assert.NotContains(t, visible, "{",
			"a JSON-object brace from the provider message must be neutralized before modelerrors.StatusError can reparse it")
		assert.NotContains(t, visible, "}")
		assert.NotContains(t, visible, secret,
			"a credential embedded inside the JSON-shaped envelope must still be redacted")
		assert.Contains(t, visible, portcullis.Marker)
		assert.NotContains(t, visible, "/etc/passwd", "Details must never be surfaced, JSON-shaped or not")
		assert.NotContains(t, visible, "\n")
	}
}

// TestClassifyByMessage covers the bounded keyword classification for each
// category, plus the no-match and empty-message fallbacks. Keywords are
// Google's own public, documented API field names — not derived from any
// captured request.
func TestClassifyByMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		message      string
		wantCategory RequestRejectionCategory
		wantMatched  bool
	}{
		{"empty message", "", RejectionOther, false},
		{"no keyword match", "something went wrong", RejectionOther, false},
		{"response_modalities", "Invalid value for response_modalities: must include IMAGE", RejectionMissingResponseModalities, true},
		{"response_schema", "response_schema is not supported with response_mime_type text/plain", RejectionStructuredOutputConflict, true},
		{"tool_config", "tool_config.function_calling_config.mode is invalid", RejectionIncompatibleToolConfig, true},
		{"function_declarations", "function_declarations are not supported for this model", RejectionIncompatibleFunctionOrBuiltinTools, true},
		{"google_search built-in", "google_search tool is not supported with function calling", RejectionIncompatibleFunctionOrBuiltinTools, true},
		{"thinking_config", "thinking_config is not supported for this model", RejectionModelOrAPICapabilityMismatch, true},
		{"generic not supported", "this feature is not supported by this model", RejectionModelOrAPICapabilityMismatch, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			category, matched := classifyByMessage(tt.message)
			assert.Equal(t, tt.wantCategory, category)
			assert.Equal(t, tt.wantMatched, matched)
		})
	}
}
