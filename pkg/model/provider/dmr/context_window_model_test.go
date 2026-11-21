package dmr

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// captureConfigureBody starts NewClient against a fake runner and returns the
// body of the _configure request it sends, which is what reconfigures the engine.
func captureConfigureBody(t *testing.T, cfg *latest.ModelConfig) string {
	t.Helper()

	var (
		body string
		mu   sync.Mutex
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/engines/_configure" && r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			body = string(b)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	cfg.BaseURL = server.URL + "/engines/v1/"
	_, err := NewClient(t.Context(), cfg)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, body, "NewClient should have sent the _configure request")
	return body
}

// TestModelLevelContextWindowDoesNotReconfigureEngine pins that the model-level
// context_window never reaches the engine configuration: only
// provider_opts.context_size does.
func TestModelLevelContextWindowDoesNotReconfigureEngine(t *testing.T) {
	t.Parallel()

	size := int64(32_768)
	body := captureConfigureBody(t, &latest.ModelConfig{
		Provider:      "dmr",
		Model:         "ai/qwen3",
		ContextWindow: &size,
	})
	assert.NotContains(t, body, "context-size", "model-level context_window must not configure the engine")
	assert.NotContains(t, body, "32768")
}

// TestProviderOptsContextSizeStillConfiguresEngine keeps the DMR behaviour
// unchanged: provider_opts.context_size is still sent to _configure.
func TestProviderOptsContextSizeStillConfiguresEngine(t *testing.T) {
	t.Parallel()

	body := captureConfigureBody(t, &latest.ModelConfig{
		Provider:     "dmr",
		Model:        "ai/qwen3",
		ProviderOpts: map[string]any{"context_size": 8192},
	})
	assert.Contains(t, body, `"context-size":8192`)
}
