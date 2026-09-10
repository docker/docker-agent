package openai

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

// TestCreateBatchEmbedding_429SurfacesAsStatusError mirrors the DMR embed test
// for the OpenAI provider path (pkg/model/provider/openai/client.go:1168).
// It proves that a 429 from the embedding endpoint propagates as
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
		Provider:     "openai",
		Model:        "text-embedding-3-small",
		BaseURL:      srv.URL,
		ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"}, // custom provider → no auth required
	}
	client, err := NewClient(t.Context(), cfg, nil)
	require.NoError(t, err)

	_, err = client.CreateBatchEmbedding(t.Context(), []string{"hello"})
	require.Error(t, err, "expected error from 429 response")

	// The critical assertion: the error must contain *modelerrors.StatusError
	// so the StartableToolSet backoff gate can arm.
	var se *modelerrors.StatusError
	require.ErrorAs(t, err, &se,
		"error must wrap *modelerrors.StatusError so the backoff gate can arm")
	assert.Equal(t, http.StatusTooManyRequests, se.StatusCode)

	// The underlying SDK error must also be reachable.
	var apiErr *openaisdk.Error
	assert.ErrorAs(t, err, &apiErr,
		"underlying *openaisdk.Error must be reachable through the chain")
}

// TestCreateBatchEmbedding_5xxSurfacesAsStatusError verifies that 5xx errors
// from the embedding endpoint also surface as *modelerrors.StatusError — they
// are wrapped by WrapOpenAIError the same way 429s are. (The indexing strategy
// classifies them as per-file retryable rather than aborting, but the wrapper
// is still in place for any caller that needs the typed error.)
func TestCreateBatchEmbedding_5xxSurfacesAsStatusError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"service unavailable","type":"server_error"}}`))
	}))
	t.Cleanup(srv.Close)

	cfg := &latest.ModelConfig{
		Provider:     "openai",
		Model:        "text-embedding-3-small",
		BaseURL:      srv.URL,
		ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"}, // custom provider → no auth required
	}
	client, err := NewClient(t.Context(), cfg, nil)
	require.NoError(t, err)

	_, err = client.CreateBatchEmbedding(t.Context(), []string{"hello"})
	require.Error(t, err)

	// 5xx is also wrapped by WrapOpenAIError — it surfaces as *StatusError
	// but is classified retryable-per-file by classifyModelCallError.
	var se *modelerrors.StatusError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, http.StatusServiceUnavailable, se.StatusCode)

	// Verify the chain is intact.
	var apiErr *openaisdk.Error
	assert.ErrorAs(t, err, &apiErr)
}
