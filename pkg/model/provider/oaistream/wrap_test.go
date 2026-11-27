package oaistream

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/modelerrors"
)

func makeTestOpenAIError(t *testing.T, statusCode int, retryAfterValue string) *APIError {
	t.Helper()
	header := http.Header{}
	if retryAfterValue != "" {
		header.Set("Retry-After", retryAfterValue)
	}
	resp := httptest.NewRecorder().Result()
	resp.StatusCode = statusCode
	resp.Header = header
	return &APIError{
		Method:     http.MethodPost,
		URL:        "https://api.openai.com/v1/chat/completions",
		StatusCode: statusCode,
		Response:   resp,
	}
}

func TestWrapOpenAIError(t *testing.T) {
	t.Parallel()

	t.Run("nil returns nil", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, WrapOpenAIError(nil))
	})

	t.Run("non-openai error passes through unchanged", func(t *testing.T) {
		t.Parallel()
		orig := errors.New("some network error")
		result := WrapOpenAIError(orig)
		assert.Equal(t, orig, result)
		var se *modelerrors.StatusError
		assert.NotErrorAs(t, result, &se)
	})

	t.Run("429 without Retry-After wraps with zero RetryAfter", func(t *testing.T) {
		t.Parallel()
		apiErr := makeTestOpenAIError(t, 429, "")
		result := WrapOpenAIError(apiErr)
		var se *modelerrors.StatusError
		require.ErrorAs(t, result, &se)
		assert.Equal(t, 429, se.StatusCode)
		assert.Equal(t, time.Duration(0), se.RetryAfter)
		// Original error still accessible
		assert.ErrorIs(t, result, apiErr)
	})

	t.Run("429 with Retry-After header sets RetryAfter", func(t *testing.T) {
		t.Parallel()
		apiErr := makeTestOpenAIError(t, 429, "30")
		result := WrapOpenAIError(apiErr)
		var se *modelerrors.StatusError
		require.ErrorAs(t, result, &se)
		assert.Equal(t, 429, se.StatusCode)
		assert.Equal(t, 30*time.Second, se.RetryAfter)
	})

	t.Run("500 wraps with correct status code", func(t *testing.T) {
		t.Parallel()
		apiErr := makeTestOpenAIError(t, 500, "")
		result := WrapOpenAIError(apiErr)
		var se *modelerrors.StatusError
		require.ErrorAs(t, result, &se)
		assert.Equal(t, 500, se.StatusCode)
	})

	t.Run("wrapped error is classified correctly by ClassifyModelError", func(t *testing.T) {
		t.Parallel()
		apiErr := makeTestOpenAIError(t, 429, "10")
		result := WrapOpenAIError(apiErr)
		retryable, rateLimited, retryAfter := modelerrors.ClassifyModelError(result)
		assert.False(t, retryable)
		assert.True(t, rateLimited)
		assert.Equal(t, 10*time.Second, retryAfter)
	})

	t.Run("wrapped in fmt.Errorf still classified correctly", func(t *testing.T) {
		t.Parallel()
		apiErr := makeTestOpenAIError(t, 500, "")
		wrapped := fmt.Errorf("stream error: %w", WrapOpenAIError(apiErr))
		retryable, rateLimited, _ := modelerrors.ClassifyModelError(wrapped)
		assert.True(t, retryable)
		assert.False(t, rateLimited)
	})
}
