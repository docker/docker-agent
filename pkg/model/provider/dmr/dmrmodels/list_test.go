package dmrmodels

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

		models, err := ListModelsAt(t.Context(), server.Client(), server.URL+"/")
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

		models, err := ListModelsAt(t.Context(), server.Client(), server.URL)
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

		models, err := ListModelsAt(t.Context(), server.Client(), server.URL)
		require.NoError(t, err)
		assert.Equal(t, []string{"ai/qwen3:latest"}, models)
	})

	t.Run("non-200 status is an error", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()

		_, err := ListModelsAt(t.Context(), server.Client(), server.URL)
		require.Error(t, err)
	})

	t.Run("malformed body is an error", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`not json`))
		}))
		defer server.Close()

		_, err := ListModelsAt(t.Context(), server.Client(), server.URL)
		require.Error(t, err)
	})

	t.Run("unreachable endpoint is an error", func(t *testing.T) {
		t.Parallel()

		_, err := ListModelsAt(t.Context(), &http.Client{}, "http://127.0.0.1:59998/")
		require.Error(t, err)
	})
}

// TestListModels exercises the exported entry point through MODEL_RUNNER_HOST,
// which makes ResolveBaseURL bypass the `docker model` CLI and return a
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

func TestListModelsWithMetadataAt(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/engines/v1/models", r.URL.Path)
		_, _ = w.Write([]byte(`{"data":[
   {"id":" ai/qwen3:latest ","dmr":{"context_window":32768,"architecture":"qwen3","parameters":"8B","quantization":"Q4_K_M","size":"4.9 GiB"}},
   {"id":"ai/qwen3:latest"}, {"id":""}, {"id":"ai/legacy"}, {"id":"ai/null","dmr":null}
  ]}`))
	}))
	defer server.Close()
	models, err := ListModelsWithMetadataAt(t.Context(), server.Client(), server.URL+"/engines/v1/")
	require.NoError(t, err)
	require.Len(t, models, 3)
	assert.Equal(t, "ai/legacy", models[0].ID)
	assert.Nil(t, models[0].Metadata)
	assert.Nil(t, models[1].Metadata)
	assert.Equal(t, Model{ID: "ai/qwen3:latest", Metadata: &Metadata{
		ContextWindow: 32768, Architecture: "qwen3", Parameters: "8B", Quantization: "Q4_K_M", Size: "4.9 GiB",
	}}, models[2])
}

func TestGetModel(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /engines/v1/models/{name...}", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "hf.co/org/model:Q4_K_M", r.PathValue("name"))
		_, _ = w.Write([]byte(`{"id":"hf.co/org/model:Q4_K_M","dmr":{"context_window":8192}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	model, err := GetModel(t.Context(), server.Client(), server.URL+"/engines/v1/", "hf.co/org/model:Q4_K_M")
	require.NoError(t, err)
	require.NotNil(t, model.Metadata)
	assert.Equal(t, int32(8192), model.Metadata.ContextWindow)
}
