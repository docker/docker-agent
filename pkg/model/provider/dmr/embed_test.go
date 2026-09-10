package dmr

import (
	"net/http"
	"net/http/httptest"
	"testing"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelerrors"
)

// TestCreateBatchEmbedding_429SurfacesAsStatusError proves that when the
// embedding endpoint returns HTTP 429 the error propagates as
// *modelerrors.StatusError so the StartableToolSet backoff gate can arm.
func TestCreateBatchEmbedding_429SurfacesAsStatusError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded","type":"requests","code":"rate_limit_exceeded"}}`))
	}))
	t.Cleanup(srv.Close)

	cfg := &latest.ModelConfig{
		Provider: "dmr",
		Model:    "ai/nomic-embed-text",
		BaseURL:  srv.URL + "/v1",
	}
	client, err := NewClient(t.Context(), cfg, nil)
	require.NoError(t, err)

	_, err = client.CreateBatchEmbedding(t.Context(), []string{"hello"})
	require.Error(t, err, "expected error from 429 response")

	// The critical assertion: the OpenAI SDK error must be wrapped in
	// *modelerrors.StatusError by WrapOpenAIError so the backoff gate can arm.
	var se *modelerrors.StatusError
	require.ErrorAs(t, err, &se,
		"error must contain *modelerrors.StatusError so the backoff gate can arm")
	assert.Equal(t, http.StatusTooManyRequests, se.StatusCode)

	// Cross-check: the underlying SDK error is also reachable.
	var apiErr *openaisdk.Error
	assert.ErrorAs(t, err, &apiErr,
		"underlying *openaisdk.Error must be reachable through the chain")
}
