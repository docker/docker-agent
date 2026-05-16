package bedrock

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	smithy "github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/modelerrors"
)

func makeTestBedrockError(statusCode int, retryAfterValue string) error {
	header := http.Header{}
	if retryAfterValue != "" {
		header.Set("Retry-After", retryAfterValue)
	}

	httpResp := &http.Response{
		StatusCode: statusCode,
		Header:     header,
	}
	resp := &smithyhttp.Response{Response: httpResp}

	return &smithy.OperationError{
		ServiceID:     "BedrockRuntime",
		OperationName: "ConverseStream",
		Err: &smithyhttp.ResponseError{
			Response: resp,
			Err: &smithy.GenericAPIError{
				Code:    "ThrottlingException",
				Message: "Rate exceeded",
			},
		},
	}
}

func TestWrapBedrockError(t *testing.T) {
	t.Parallel()

	t.Run("nil returns nil", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, wrapBedrockError(nil))
	})

	t.Run("non-AWS error passes through unchanged", func(t *testing.T) {
		t.Parallel()
		orig := errors.New("some network error")
		result := wrapBedrockError(orig)
		assert.Equal(t, orig, result)
		var se *modelerrors.StatusError
		assert.NotErrorAs(t, result, &se)
	})

	t.Run("429 without Retry-After wraps with zero RetryAfter", func(t *testing.T) {
		t.Parallel()
		awsErr := makeTestBedrockError(429, "")
		result := wrapBedrockError(awsErr)
		var se *modelerrors.StatusError
		require.ErrorAs(t, result, &se)
		assert.Equal(t, 429, se.StatusCode)
		assert.Equal(t, time.Duration(0), se.RetryAfter)
		// Original error still accessible
		assert.ErrorIs(t, result, awsErr)
	})

	t.Run("429 with Retry-After header sets RetryAfter", func(t *testing.T) {
		t.Parallel()
		awsErr := makeTestBedrockError(429, "20")
		result := wrapBedrockError(awsErr)
		var se *modelerrors.StatusError
		require.ErrorAs(t, result, &se)
		assert.Equal(t, 429, se.StatusCode)
		assert.Equal(t, 20*time.Second, se.RetryAfter)
	})

	t.Run("500 wraps with correct status code", func(t *testing.T) {
		t.Parallel()
		awsErr := makeTestBedrockError(500, "")
		result := wrapBedrockError(awsErr)
		var se *modelerrors.StatusError
		require.ErrorAs(t, result, &se)
		assert.Equal(t, 500, se.StatusCode)
		assert.Equal(t, time.Duration(0), se.RetryAfter)
	})

	t.Run("wrapped error is classified correctly by ClassifyModelError", func(t *testing.T) {
		t.Parallel()
		awsErr := makeTestBedrockError(429, "15")
		result := wrapBedrockError(awsErr)
		retryable, rateLimited, retryAfter := modelerrors.ClassifyModelError(result)
		assert.False(t, retryable)
		assert.True(t, rateLimited)
		assert.Equal(t, 15*time.Second, retryAfter)
	})

	t.Run("wrapped in fmt.Errorf still classified correctly", func(t *testing.T) {
		t.Parallel()
		awsErr := makeTestBedrockError(500, "")
		wrapped := fmt.Errorf("bedrock converse stream failed: %w", wrapBedrockError(awsErr))
		retryable, rateLimited, _ := modelerrors.ClassifyModelError(wrapped)
		assert.True(t, retryable)
		assert.False(t, rateLimited)
	})
}
