package dmr

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/model/provider/dmr/dmrmodels"
)

func TestErrIndicatesNotInstalled(t *testing.T) {
	t.Parallel()

	assert.False(t, dmrmodels.IsNotInstalledError(nil))
	assert.False(t, dmrmodels.IsNotInstalledError(errors.New("dial tcp: connection refused")))

	// The exact message the docker CLI printed when the test suite was
	// written, and a variant with different usage text around it: detection
	// must not depend on the full message staying byte-identical.
	assert.True(t, dmrmodels.IsNotInstalledError(errors.New("unknown flag: --json\n\nUsage:  docker [OPTIONS] COMMAND [ARG...]\n\nRun 'docker --help' for more information")))
	assert.True(t, dmrmodels.IsNotInstalledError(errors.New("some prefix\nunknown flag: --json\nsome other usage text")))
}

func TestModelAvailable(t *testing.T) {
	t.Parallel()

	available := []string{"ai/embeddinggemma:latest", "ai/qwen3:latest", "registry:5000/ai/foo:latest"}

	tests := []struct {
		model string
		want  bool
	}{
		{"ai/qwen3:latest", true},
		{"ai/qwen3", true}, // untagged matches any tag of the repository
		{"ai/qwen3:Q4_K_M", false},
		{"ai/other", false},
		{"registry:5000/ai/foo", true}, // host:port colon is not a tag separator
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, modelAvailable(available, tt.model))
		})
	}

	assert.False(t, modelAvailable(nil, "ai/qwen3"))
}

func TestCheckModelAvailable(t *testing.T) {
	t.Parallel()

	newServer := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/models", r.URL.Path)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
		}))
	}

	t.Run("model pulled", func(t *testing.T) {
		t.Parallel()
		server := newServer(`{"data":[{"id":"ai/qwen3:latest"}]}`)
		defer server.Close()

		err := checkModelAvailable(t.Context(), server.Client(), server.URL+"/", "ai/qwen3")
		require.NoError(t, err)
	})

	t.Run("model not pulled", func(t *testing.T) {
		t.Parallel()
		server := newServer(`{"data":[{"id":"ai/gemma3:latest"}]}`)
		defer server.Close()

		err := checkModelAvailable(t.Context(), server.Client(), server.URL+"/", "ai/qwen3")
		require.Error(t, err)

		var notAvailable *ModelNotAvailableError
		require.ErrorAs(t, err, &notAvailable)
		assert.Equal(t, "ai/qwen3", notAvailable.Model)
		assert.Contains(t, err.Error(), "not pulled in Docker Model Runner")
		assert.Contains(t, err.Error(), "docker model pull ai/qwen3")
	})

	t.Run("endpoint unreachable", func(t *testing.T) {
		t.Parallel()
		server := newServer(`{"data":[]}`)
		server.Close() // unreachable from the start

		err := checkModelAvailable(t.Context(), server.Client(), server.URL+"/", "ai/qwen3")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot query Docker Model Runner")
		assert.Contains(t, err.Error(), "https://docs.docker.com/ai/model-runner/get-started/")
	})
}
