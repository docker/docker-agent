package modelerrors

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockTimeoutError implements net.Error with Timeout() = true
type mockTimeoutError struct{}

func (e *mockTimeoutError) Error() string   { return "mock timeout" }
func (e *mockTimeoutError) Timeout() bool   { return true }
func (e *mockTimeoutError) Temporary() bool { return true }

var _ net.Error = (*mockTimeoutError)(nil)

func TestIsRetryableModelError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{name: "nil error", err: nil, expected: false},
		{name: "context canceled", err: context.Canceled, expected: false},
		{name: "context deadline exceeded", err: context.DeadlineExceeded, expected: false},
		{name: "network timeout", err: &mockTimeoutError{}, expected: true},
		{name: "rate limit 429", err: errors.New("API error: status 429 too many requests"), expected: false},
		{name: "rate limit message", err: errors.New("rate limit exceeded"), expected: false},
		{name: "too many requests", err: errors.New("too many requests"), expected: false},
		{name: "throttling", err: errors.New("request throttled"), expected: false},
		{name: "quota exceeded", err: errors.New("quota exceeded"), expected: false},
		{name: "server error 500", err: errors.New("internal server error 500"), expected: true},
		{name: "bad gateway 502", err: errors.New("502 bad gateway"), expected: true},
		{name: "service unavailable 503", err: errors.New("503 service unavailable"), expected: true},
		{name: "gateway timeout 504", err: errors.New("504 gateway timeout"), expected: true},
		{name: "timeout message", err: errors.New("request timeout"), expected: true},
		{name: "connection refused", err: errors.New("connection refused"), expected: true},
		{name: "unauthorized 401", err: errors.New("401 unauthorized"), expected: false},
		{name: "forbidden 403", err: errors.New("403 forbidden"), expected: false},
		{name: "not found 404", err: errors.New("404 not found"), expected: false},
		{name: "bad request 400", err: errors.New("400 bad request"), expected: false},
		{name: "api key error", err: errors.New("invalid api key"), expected: false},
		{name: "authentication error", err: errors.New("authentication failed"), expected: false},
		{name: "anthropic overloaded 529", err: errors.New("529 overloaded"), expected: true},
		{name: "other side closed", err: errors.New("other side closed the connection"), expected: true},
		{name: "fetch failed", err: errors.New("fetch failed"), expected: true},
		{name: "reset before headers", err: errors.New("reset before headers"), expected: true},
		{name: "upstream connect error", err: errors.New("upstream connect error"), expected: true},
		{name: "HTTP/2 INTERNAL_ERROR", err: fmt.Errorf("error receiving from stream: %w", errors.New("stream error: stream ID 1; INTERNAL_ERROR; received from peer")), expected: true},
		{name: "context overflow - prompt too long", err: errors.New("prompt is too long: 226360 tokens > 200000 maximum"), expected: false},
		{name: "context overflow - thinking budget", err: errors.New("max_tokens must be greater than thinking.budget_tokens"), expected: false},
		{name: "context overflow - wrapped", err: &ContextOverflowError{Underlying: errors.New("test")}, expected: false},
		{name: "unknown error", err: errors.New("something weird happened"), expected: false},
		// Vertex AI / Gemini transient 400 (issue #2683)
		{name: "vertex function response parts 400", err: errors.New("400 Bad Request: please ensure that the number of function response parts is equal to the number of function call parts"), expected: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, isRetryableModelError(tt.err), "isRetryableModelError(%v)", tt.err)
		})
	}
}

func TestExtractHTTPStatusCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		expected int
	}{
		{name: "nil error", err: nil, expected: 0},
		{name: "429 in message", err: errors.New("POST /v1/chat/completions: 429 Too Many Requests"), expected: 429},
		{name: "500 in message", err: errors.New("internal server error 500"), expected: 500},
		{name: "502 in message", err: errors.New("502 bad gateway"), expected: 502},
		{name: "401 in message", err: errors.New("401 unauthorized"), expected: 401},
		{name: "no status code", err: errors.New("connection refused"), expected: 0},
		// StatusError structural path
		{name: "StatusError 429", err: &StatusError{StatusCode: 429, Err: errors.New("rate limited")}, expected: 429},
		{name: "StatusError 500", err: &StatusError{StatusCode: 500, Err: errors.New("server error")}, expected: 500},
		{name: "wrapped StatusError", err: fmt.Errorf("outer: %w", &StatusError{StatusCode: 503, Err: errors.New("unavailable")}), expected: 503},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, extractHTTPStatusCode(tt.err), "extractHTTPStatusCode(%v)", tt.err)
		})
	}
}

func TestIsRetryableStatusCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		statusCode int
		expected   bool
	}{
		{500, true}, {502, true}, {503, true}, {504, true}, // Server errors
		{408, true},                                            // Request timeout
		{529, true},                                            // Anthropic overloaded
		{429, false},                                           // Rate limit
		{400, false}, {401, false}, {403, false}, {404, false}, // Client errors
		{200, false}, // Not an error
		{0, false},   // Unknown
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("status_%d", tt.statusCode), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, isRetryableStatusCode(tt.statusCode), "isRetryableStatusCode(%d)", tt.statusCode)
		})
	}
}

func TestIsContextOverflowError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{name: "nil error", err: nil, expected: false},
		{name: "generic error", err: errors.New("something went wrong"), expected: false},
		{name: "anthropic prompt too long", err: errors.New("prompt is too long: 226360 tokens > 200000 maximum"), expected: true},
		{name: "openai context length exceeded", err: errors.New("maximum context length is 128000 tokens"), expected: true},
		{name: "context_length_exceeded code", err: errors.New("error code: context_length_exceeded"), expected: true},
		{name: "thinking budget error", err: errors.New("max_tokens must be greater than thinking.budget_tokens"), expected: true},
		{name: "request too large", err: errors.New("request too large for model"), expected: true},
		{name: "input is too long", err: errors.New("input is too long"), expected: true},
		{name: "reduce your prompt", err: errors.New("please reduce your prompt"), expected: true},
		{name: "reduce the length", err: errors.New("please reduce the length of the messages"), expected: true},
		{name: "token limit", err: errors.New("token limit exceeded"), expected: true},
		{name: "wrapped ContextOverflowError", err: &ContextOverflowError{Underlying: errors.New("test")}, expected: true},
		{name: "errors.As wrapped", err: fmt.Errorf("all models failed: %w", &ContextOverflowError{Underlying: errors.New("test")}), expected: true},
		{name: "500 internal server error", err: errors.New("500 Internal Server Error"), expected: false},
		{name: "429 rate limit", err: errors.New("429 too many requests"), expected: false},
		{name: "network timeout", err: errors.New("connection timeout"), expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, IsContextOverflowError(tt.err), "IsContextOverflowError(%v)", tt.err)
		})
	}
}

func TestContextOverflowError(t *testing.T) {
	t.Parallel()

	t.Run("wraps underlying error", func(t *testing.T) {
		t.Parallel()
		underlying := errors.New("prompt is too long: 226360 tokens > 200000 maximum")
		ctxErr := NewContextOverflowError(underlying)

		assert.Contains(t, ctxErr.Error(), "context window overflow")
		assert.Contains(t, ctxErr.Error(), "prompt is too long")
		assert.ErrorIs(t, ctxErr, underlying)
	})

	t.Run("nil underlying returns fallback message", func(t *testing.T) {
		t.Parallel()
		ctxErr := NewContextOverflowError(nil)
		assert.Equal(t, "context window overflow", ctxErr.Error())
		assert.NoError(t, ctxErr.Unwrap())
	})

	t.Run("errors.As works through wrapping", func(t *testing.T) {
		t.Parallel()
		underlying := errors.New("test error")
		wrapped := fmt.Errorf("all models failed: %w", NewContextOverflowError(underlying))

		var ctxErr *ContextOverflowError
		require.ErrorAs(t, wrapped, &ctxErr)
		assert.Equal(t, underlying, ctxErr.Underlying)
	})
}

func TestIsRetryableModelError_ContextOverflow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{name: "prompt too long", err: errors.New("prompt is too long: 226360 tokens > 200000 maximum")},
		{name: "thinking budget cascade", err: errors.New("max_tokens must be greater than thinking.budget_tokens")},
		{name: "context length exceeded", err: errors.New("maximum context length is 128000 tokens")},
		{name: "wrapped ContextOverflowError", err: &ContextOverflowError{Underlying: errors.New("test")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.False(t, isRetryableModelError(tt.err), "context overflow errors should not be retryable: %v", tt.err)
		})
	}
}

func TestMatchesTransientPattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{name: "nil error", err: nil, expected: false},
		{name: "unrelated error", err: errors.New("connection refused"), expected: false},
		{name: "exact lowercase match", err: errors.New("number of function response parts"), expected: true},
		// Defence-in-depth: the message is lowercased before comparison so
		// mixed-case provider errors still match the lowercase pattern.
		{name: "mixed-case message", err: errors.New("Please ensure that the Number Of Function Response Parts is equal"), expected: true},
		{name: "wrapped error", err: fmt.Errorf("error receiving from stream: %w", errors.New("NUMBER OF FUNCTION RESPONSE PARTS")), expected: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, matchesTransientPattern(tt.err), "matchesTransientPattern(%v)", tt.err)
		})
	}
}

func TestFormatError(t *testing.T) {
	t.Parallel()

	t.Run("nil error", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, FormatError(nil))
	})

	t.Run("context overflow shows user-friendly message", func(t *testing.T) {
		t.Parallel()
		err := NewContextOverflowError(errors.New("prompt is too long"))
		msg := FormatError(err)
		assert.Contains(t, msg, "context window")
		assert.Contains(t, msg, "/compact")
		assert.NotContains(t, msg, "prompt is too long")
	})

	t.Run("wrapped context overflow shows user-friendly message", func(t *testing.T) {
		t.Parallel()
		err := fmt.Errorf("outer: %w", NewContextOverflowError(errors.New("prompt is too long")))
		msg := FormatError(err)
		assert.Contains(t, msg, "context window")
		assert.Contains(t, msg, "/compact")
	})

	t.Run("generic error preserves message", func(t *testing.T) {
		t.Parallel()
		err := errors.New("authentication failed")
		assert.Equal(t, "authentication failed", FormatError(err))
	})

	t.Run("context overflow takes precedence over status formatting", func(t *testing.T) {
		t.Parallel()
		underlying := errors.New("prompt is too long: 226360 tokens > 200000 maximum")
		wrapped := NewContextOverflowError(&StatusError{StatusCode: 400, Err: underlying})
		msg := FormatError(wrapped)
		assert.Contains(t, msg, "context window")
		assert.Contains(t, msg, "/compact")
	})
}

func TestStatusErrorParsesProviderBody(t *testing.T) {
	t.Parallel()

	t.Run("opaque proxy body strips URL noise but keeps the message", func(t *testing.T) {
		t.Parallel()
		// This is the case the user reported: the Docker AI gateway returns
		// only {"message":"Bad Request"}, no structured details.
		inner := errors.New(`POST "https://ai-backend-service-stage.docker.com/proxy/v1/messages?beta=true": 400 Bad Request {"message":"Bad Request"}`)
		se := &StatusError{StatusCode: 400, Err: inner}
		assert.Equal(t, "HTTP 400: Bad Request", se.Error())
	})

	t.Run("anthropic-style body surfaces error.type and error.message", func(t *testing.T) {
		t.Parallel()
		inner := errors.New(`POST "https://api.anthropic.com/v1/messages": 400 Bad Request {"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 226360 tokens > 200000 maximum"}}`)
		se := &StatusError{StatusCode: 400, Err: inner}
		assert.Equal(t,
			"HTTP 400: invalid_request_error: prompt is too long: 226360 tokens > 200000 maximum",
			se.Error())
	})

	t.Run("anthropic-style body keeps the request id", func(t *testing.T) {
		t.Parallel()
		inner := errors.New(`POST "https://api.anthropic.com/v1/messages": 400 Bad Request (Request-ID: req_abc123) {"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: Field required"}}`)
		se := &StatusError{StatusCode: 400, Err: inner}
		assert.Equal(t,
			"HTTP 400: invalid_request_error: max_tokens: Field required (Request-ID: req_abc123)",
			se.Error())
	})

	t.Run("openai-style body surfaces type, message, code and param", func(t *testing.T) {
		t.Parallel()
		inner := errors.New(`POST "https://api.openai.com/v1/chat/completions": 400 Bad Request {"error":{"message":"Invalid model 'foo-bar'","type":"invalid_request_error","param":"model","code":"model_not_found"}}`)
		se := &StatusError{StatusCode: 400, Err: inner}
		assert.Equal(t,
			"HTTP 400: invalid_request_error: Invalid model 'foo-bar' (code=model_not_found, param=model)",
			se.Error())
	})

	t.Run("gemini-style body surfaces numeric code and status", func(t *testing.T) {
		t.Parallel()
		inner := errors.New(`{"error":{"code":400,"message":"Invalid value at 'contents[0].parts[0]'","status":"INVALID_ARGUMENT"}}`)
		se := &StatusError{StatusCode: 400, Err: inner}
		assert.Equal(t,
			"HTTP 400: Invalid value at 'contents[0].parts[0]' (code=400, status=INVALID_ARGUMENT)",
			se.Error())
	})

	t.Run("openai-style body with null param omits param meta", func(t *testing.T) {
		t.Parallel()
		inner := errors.New(`POST "/v1/chat": 401 Unauthorized {"error":{"message":"Incorrect API key provided","type":"invalid_request_error","param":null,"code":"invalid_api_key"}}`)
		se := &StatusError{StatusCode: 401, Err: inner}
		assert.Equal(t,
			"HTTP 401: invalid_request_error: Incorrect API key provided (code=invalid_api_key)",
			se.Error())
	})

	t.Run("falls back to underlying message when no JSON body", func(t *testing.T) {
		t.Parallel()
		// No JSON, no URL — keep the existing simple format.
		se := &StatusError{StatusCode: 429, Err: errors.New("rate limit exceeded")}
		assert.Equal(t, "HTTP 429: rate limit exceeded", se.Error())
	})

	t.Run("falls back to underlying message when JSON has no useful fields", func(t *testing.T) {
		t.Parallel()
		se := &StatusError{StatusCode: 500, Err: errors.New(`POST "/v1/x": 500 Internal Server Error {"foo":"bar"}`)}
		assert.Equal(t, `HTTP 500: POST "/v1/x": 500 Internal Server Error {"foo":"bar"}`, se.Error())
	})

	t.Run("handles braces inside string values", func(t *testing.T) {
		t.Parallel()
		inner := errors.New(`POST "/v1/x": 400 Bad Request {"error":{"type":"invalid_request_error","message":"unexpected token '}' in payload"}}`)
		se := &StatusError{StatusCode: 400, Err: inner}
		assert.Equal(t,
			"HTTP 400: invalid_request_error: unexpected token '}' in payload",
			se.Error())
	})

	t.Run("context overflow detection still works on cleaned message", func(t *testing.T) {
		t.Parallel()
		inner := errors.New(`POST "/v1/messages": 400 Bad Request {"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 226360 tokens > 200000 maximum"}}`)
		se := &StatusError{StatusCode: 400, Err: inner}
		assert.True(t, IsContextOverflowError(se),
			"context-overflow phrasing must remain detectable in StatusError.Error()")
	})
}

func TestParseRetryAfterHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		value    string
		expected time.Duration
	}{
		{name: "empty", value: "", expected: 0},
		{name: "zero seconds", value: "0", expected: 0},
		{name: "negative seconds", value: "-1", expected: 0},
		{name: "invalid string", value: "foo", expected: 0},
		{name: "5 seconds", value: "5", expected: 5 * time.Second},
		{name: "30 seconds", value: "30", expected: 30 * time.Second},
		{name: "120 seconds", value: "120", expected: 120 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseRetryAfterHeader(tt.value)
			assert.Equal(t, tt.expected, got, "parseRetryAfterHeader(%q)", tt.value)
		})
	}

	t.Run("HTTP-date in the future", func(t *testing.T) {
		t.Parallel()
		future := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
		got := parseRetryAfterHeader(future)
		assert.Greater(t, got, 0*time.Second, "should return positive duration for future HTTP-date")
		assert.LessOrEqual(t, got, 11*time.Second, "should not exceed ~10s for near-future date")
	})

	t.Run("HTTP-date in the past", func(t *testing.T) {
		t.Parallel()
		past := time.Now().Add(-10 * time.Second).UTC().Format(http.TimeFormat)
		got := parseRetryAfterHeader(past)
		assert.Equal(t, 0*time.Second, got, "should return 0 for past HTTP-date")
	})
}

func TestStatusError(t *testing.T) {
	t.Parallel()

	t.Run("Error() includes status code and wrapped message", func(t *testing.T) {
		t.Parallel()
		underlying := errors.New("rate limit exceeded")
		se := &StatusError{StatusCode: 429, Err: underlying}
		assert.Equal(t, "HTTP 429: rate limit exceeded", se.Error())
	})

	t.Run("Unwrap() allows errors.Is traversal", func(t *testing.T) {
		t.Parallel()
		sentinel := errors.New("sentinel")
		se := &StatusError{StatusCode: 500, Err: sentinel}
		assert.ErrorIs(t, se, sentinel)
	})

	t.Run("errors.As finds StatusError in chain", func(t *testing.T) {
		t.Parallel()
		se := &StatusError{StatusCode: 429, RetryAfter: 10 * time.Second, Err: errors.New("rate limited")}
		wrapped := fmt.Errorf("outer: %w", se)
		var found *StatusError
		require.ErrorAs(t, wrapped, &found)
		assert.Equal(t, 429, found.StatusCode)
		assert.Equal(t, 10*time.Second, found.RetryAfter)
	})
}

func TestWrapHTTPError(t *testing.T) {
	t.Parallel()

	t.Run("nil error returns nil", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, WrapHTTPError(429, nil, nil))
	})

	t.Run("status < 400 passes through unchanged", func(t *testing.T) {
		t.Parallel()
		origErr := errors.New("original")
		result := WrapHTTPError(200, nil, origErr)
		assert.Equal(t, origErr, result)
		var se *StatusError
		assert.NotErrorAs(t, result, &se)
	})

	t.Run("429 without response has zero RetryAfter", func(t *testing.T) {
		t.Parallel()
		origErr := errors.New("rate limited")
		result := WrapHTTPError(429, nil, origErr)
		var se *StatusError
		require.ErrorAs(t, result, &se)
		assert.Equal(t, 429, se.StatusCode)
		assert.Equal(t, time.Duration(0), se.RetryAfter)
		assert.Equal(t, "HTTP 429: rate limited", se.Error())
	})

	t.Run("429 with Retry-After header sets RetryAfter", func(t *testing.T) {
		t.Parallel()
		origErr := errors.New("rate limited")
		respHeader := http.Header{}
		respHeader.Set("Retry-After", "30")
		resp := &http.Response{Header: respHeader}
		result := WrapHTTPError(429, resp, origErr)
		var se *StatusError
		require.ErrorAs(t, result, &se)
		assert.Equal(t, 429, se.StatusCode)
		assert.Equal(t, 30*time.Second, se.RetryAfter)
	})

	t.Run("500 wraps correctly", func(t *testing.T) {
		t.Parallel()
		origErr := errors.New("internal server error")
		result := WrapHTTPError(500, nil, origErr)
		var se *StatusError
		require.ErrorAs(t, result, &se)
		assert.Equal(t, 500, se.StatusCode)
		assert.Equal(t, time.Duration(0), se.RetryAfter)
	})

	t.Run("original error still accessible via Unwrap", func(t *testing.T) {
		t.Parallel()
		sentinel := errors.New("sentinel")
		result := WrapHTTPError(429, nil, sentinel)
		assert.ErrorIs(t, result, sentinel)
	})
}

func TestClassifyModelError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		err             error
		wantRetryable   bool
		wantRateLimited bool
		wantRetryAfter  time.Duration
	}{
		{name: "nil", err: nil, wantRetryable: false, wantRateLimited: false},
		{name: "context canceled", err: context.Canceled, wantRetryable: false, wantRateLimited: false},
		{name: "context deadline exceeded", err: context.DeadlineExceeded, wantRetryable: false, wantRateLimited: false},
		{name: "context overflow", err: errors.New("prompt is too long: 200000 tokens > 100000 maximum"), wantRetryable: false, wantRateLimited: false},
		// 429 without StatusError (fallback message-pattern path)
		{name: "429 message fallback, no RetryAfter", err: errors.New("POST /v1/chat: 429 Too Many Requests"), wantRetryable: false, wantRateLimited: true, wantRetryAfter: 0},
		// 429 via StatusError (primary path) — no Retry-After
		{name: "429 StatusError no retry-after", err: &StatusError{StatusCode: 429, RetryAfter: 0, Err: errors.New("rate limited")}, wantRetryable: false, wantRateLimited: true, wantRetryAfter: 0},
		// 429 via StatusError with Retry-After from response header
		{name: "429 StatusError with retry-after", err: &StatusError{StatusCode: 429, RetryAfter: 20 * time.Second, Err: errors.New("rate limited")}, wantRetryable: false, wantRateLimited: true, wantRetryAfter: 20 * time.Second},
		// Retryable status codes via StatusError
		{name: "500 StatusError", err: &StatusError{StatusCode: 500, Err: errors.New("internal server error")}, wantRetryable: true, wantRateLimited: false},
		{name: "529 StatusError", err: &StatusError{StatusCode: 529, Err: errors.New("overloaded")}, wantRetryable: true, wantRateLimited: false},
		{name: "408 StatusError", err: &StatusError{StatusCode: 408, Err: errors.New("timeout")}, wantRetryable: true, wantRateLimited: false},
		// Retryable fallback path (message-based)
		{name: "500 message fallback", err: errors.New("500 internal server error"), wantRetryable: true, wantRateLimited: false},
		{name: "502 message fallback", err: errors.New("502 bad gateway"), wantRetryable: true, wantRateLimited: false},
		// Non-retryable via StatusError
		{name: "401 StatusError", err: &StatusError{StatusCode: 401, Err: errors.New("unauthorized")}, wantRetryable: false, wantRateLimited: false},
		{name: "403 StatusError", err: &StatusError{StatusCode: 403, Err: errors.New("forbidden")}, wantRetryable: false, wantRateLimited: false},
		// Non-retryable fallback
		{name: "401 message fallback", err: errors.New("401 unauthorized"), wantRetryable: false, wantRateLimited: false},
		// 400 with Vertex AI "function response parts" message is treated as transient (issue #2683)
		{name: "vertex transient 400 StatusError", err: &StatusError{StatusCode: 400, Err: errors.New("Error 400, Message: Please ensure that the number of function response parts is equal to the number of function call parts of the function call turn., Status: INVALID_ARGUMENT, Details: []")}, wantRetryable: true, wantRateLimited: false},
		{name: "vertex transient 400 wrapped in stream error", err: fmt.Errorf("error receiving from stream: %w", &StatusError{StatusCode: 400, Err: errors.New("number of function response parts")}), wantRetryable: true, wantRateLimited: false},
		{name: "vertex transient 400 message fallback (no StatusError)", err: errors.New("400 Bad Request: Please ensure that the number of function response parts is equal to the number of function call parts"), wantRetryable: true, wantRateLimited: false},
		// Network errors
		{name: "network timeout", err: &mockTimeoutError{}, wantRetryable: true, wantRateLimited: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			retryable, rateLimited, retryAfterOut := ClassifyModelError(tt.err)
			assert.Equal(t, tt.wantRetryable, retryable, "retryable mismatch")
			assert.Equal(t, tt.wantRateLimited, rateLimited, "rateLimited mismatch")
			assert.Equal(t, tt.wantRetryAfter, retryAfterOut, "retryAfter mismatch")
		})
	}

	t.Run("wrapped StatusError is found by errors.As", func(t *testing.T) {
		t.Parallel()
		statusErr := &StatusError{StatusCode: 429, RetryAfter: 15 * time.Second, Err: errors.New("rate limited")}
		wrapped := fmt.Errorf("model failed: %w", statusErr)
		retryable, rateLimited, retryAfterOut := ClassifyModelError(wrapped)
		assert.False(t, retryable)
		assert.True(t, rateLimited)
		assert.Equal(t, 15*time.Second, retryAfterOut)
	})

	t.Run("ContextOverflowError wrapping a StatusError is not retryable", func(t *testing.T) {
		t.Parallel()
		// A 400 StatusError whose message also triggers context overflow detection
		statusErr := &StatusError{StatusCode: 400, Err: errors.New("prompt is too long")}
		ctxErr := NewContextOverflowError(statusErr)
		retryable, rateLimited, retryAfter := ClassifyModelError(ctxErr)
		assert.False(t, retryable, "context overflow should never be retryable")
		assert.False(t, rateLimited)
		assert.Equal(t, time.Duration(0), retryAfter)
	})
}

func TestStatusErrorEdgeCases(t *testing.T) {
	t.Parallel()

	t.Run("empty JSON object falls back to underlying", func(t *testing.T) {
		t.Parallel()
		inner := errors.New(`POST "/v1/x": 400 Bad Request {}`)
		se := &StatusError{StatusCode: 400, Err: inner}
		assert.Equal(t, `HTTP 400: POST "/v1/x": 400 Bad Request {}`, se.Error())
	})

	t.Run("malformed JSON falls back to underlying", func(t *testing.T) {
		t.Parallel()
		inner := errors.New(`POST "/v1/x": 400 Bad Request {"error":{"message":"test"`)
		se := &StatusError{StatusCode: 400, Err: inner}
		assert.Equal(t, `HTTP 400: POST "/v1/x": 400 Bad Request {"error":{"message":"test"`, se.Error())
	})

	t.Run("multiple JSON objects extracts first", func(t *testing.T) {
		t.Parallel()
		inner := errors.New(`POST "/v1/x": 400 Bad Request {"message":"First"} {"message":"Second"}`)
		se := &StatusError{StatusCode: 400, Err: inner}
		assert.Equal(t, "HTTP 400: First", se.Error())
	})

	t.Run("unicode in error message", func(t *testing.T) {
		t.Parallel()
		inner := errors.New(`POST "/v1/x": 400 Bad Request {"message":"Invalid emoji: 😀"}`)
		se := &StatusError{StatusCode: 400, Err: inner}
		assert.Equal(t, "HTTP 400: Invalid emoji: 😀", se.Error())
	})

	t.Run("very large number in code field", func(t *testing.T) {
		t.Parallel()
		inner := errors.New(`{"error":{"code":9007199254740992,"message":"test"}}`)
		se := &StatusError{StatusCode: 400, Err: inner}
		// Large float64 that exceeds int64 range should still format
		assert.Contains(t, se.Error(), "test")
		assert.Contains(t, se.Error(), "code=")
	})

	t.Run("request-id with special characters", func(t *testing.T) {
		t.Parallel()
		inner := errors.New(`POST "/v1/x": 400 Bad Request (Request-ID: req_abc-123_XYZ) {"message":"test"}`)
		se := &StatusError{StatusCode: 400, Err: inner}
		assert.Equal(t, "HTTP 400: test (Request-ID: req_abc-123_XYZ)", se.Error())
	})

	t.Run("brace in URL before JSON", func(t *testing.T) {
		t.Parallel()
		// Ensure we don't mistake a '{' in the URL for the start of JSON
		inner := errors.New(`POST "https://api.example.com/v1/messages?param={value}": 400 Bad Request {"message":"test"}`)
		se := &StatusError{StatusCode: 400, Err: inner}
		assert.Equal(t, "HTTP 400: test", se.Error())
	})

	t.Run("nested JSON in message field", func(t *testing.T) {
		t.Parallel()
		// The message field itself contains JSON-like text
		inner := errors.New(`{"error":{"message":"Expected format: {\"key\":\"value\"}"}}`)
		se := &StatusError{StatusCode: 400, Err: inner}
		assert.Contains(t, se.Error(), `Expected format: {"key":"value"}`)
	})
}

func TestScalarStringEdgeCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    any
		expected string
	}{
		{name: "nil", input: nil, expected: ""},
		{name: "empty string", input: "", expected: ""},
		{name: "normal string", input: "test", expected: "test"},
		{name: "whole number", input: float64(400), expected: "400"},
		{name: "decimal number", input: float64(3.14), expected: "3.14"},
		{name: "negative whole", input: float64(-500), expected: "-500"},
		{name: "zero", input: float64(0), expected: "0"},
		{name: "bool true", input: true, expected: "true"},
		{name: "bool false", input: false, expected: "false"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, scalarString(tt.input))
		})
	}
}
