package anthropic

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
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
}
