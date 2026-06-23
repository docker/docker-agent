package dmr

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListModelsAt(t *testing.T) {
	t.Parallel()

	t.Run("parses and sorts model ids", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/models", r.URL.Path)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[
				{"id":"ai/qwen3:latest"},
				{"id":"ai/gemma3:latest"},
				{"id":"ai/embeddinggemma"}
			]}`))
		}))
		defer server.Close()

		models, err := listModelsAt(t.Context(), server.Client(), server.URL+"/")
		require.NoError(t, err)
		// Sorted, embedding models are NOT filtered here (callers do that).
		assert.Equal(t, []string{"ai/embeddinggemma", "ai/gemma3:latest", "ai/qwen3:latest"}, models)
	})

	t.Run("empty list is not an error", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		defer server.Close()

		models, err := listModelsAt(t.Context(), server.Client(), server.URL)
		require.NoError(t, err)
		assert.Empty(t, models)
	})

	t.Run("blank ids are skipped and duplicates compacted", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[
				{"id":"ai/qwen3:latest"},
				{"id":"   "},
				{"id":""},
				{"id":"ai/qwen3:latest"}
			]}`))
		}))
		defer server.Close()

		models, err := listModelsAt(t.Context(), server.Client(), server.URL)
		require.NoError(t, err)
		assert.Equal(t, []string{"ai/qwen3:latest"}, models)
	})

	t.Run("non-200 status is an error", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()

		_, err := listModelsAt(t.Context(), server.Client(), server.URL)
		require.Error(t, err)
	})

	t.Run("malformed body is an error", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`not json`))
		}))
		defer server.Close()

		_, err := listModelsAt(t.Context(), server.Client(), server.URL)
		require.Error(t, err)
	})

	t.Run("unreachable endpoint is an error", func(t *testing.T) {
		t.Parallel()

		_, err := listModelsAt(t.Context(), &http.Client{}, "http://127.0.0.1:59998/")
		require.Error(t, err)
	})
}

// TestListModels exercises the exported entry point through MODEL_RUNNER_HOST,
// which makes resolveDMRBaseURL bypass the `docker model` CLI and return a
// nil http client (so ListModels falls back to its default client). It is not
// parallel because it mutates the environment.
func TestListModels(t *testing.T) {
	t.Run("resolves via MODEL_RUNNER_HOST", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// MODEL_RUNNER_HOST + /engines/v1/ + models
			assert.Equal(t, "/engines/v1/models", r.URL.Path)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"ai/qwen3:latest"},{"id":"ai/gemma3:latest"}]}`))
		}))
		defer server.Close()

		t.Setenv("MODEL_RUNNER_HOST", server.URL)

		models, err := ListModels(t.Context())
		require.NoError(t, err)
		assert.Equal(t, []string{"ai/gemma3:latest", "ai/qwen3:latest"}, models)
	})

	t.Run("unreachable MODEL_RUNNER_HOST returns an error, not a panic", func(t *testing.T) {
		t.Setenv("MODEL_RUNNER_HOST", "http://127.0.0.1:59997")

		_, err := ListModels(t.Context())
		require.Error(t, err)
	})
}
