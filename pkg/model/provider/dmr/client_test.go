package dmr

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func TestNewClientWithExplicitBaseURL(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		Provider: "dmr",
		Model:    "ai/qwen3",
		BaseURL:  "https://custom.example.com:8080/api/v1",
	}

	client, err := NewClient(t.Context(), cfg)
	require.NoError(t, err)
	assert.Equal(t, "https://custom.example.com:8080/api/v1", client.BaseURL)
}

func TestNewClientReturnsErrNotInstalledWhenDockerModelUnsupported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping docker CLI shim test on Windows")
	}

	tempDir := t.TempDir()
	dockerPath := filepath.Join(tempDir, "docker")
	script := "#!/bin/sh\n" +
		"printf 'unknown flag: --json\\n\\nUsage:  docker [OPTIONS] COMMAND [ARG...]\\n\\nRun '\\''docker --help'\\'' for more information\\n' >&2\n" +
		"exit 1\n"
	require.NoError(t, os.WriteFile(dockerPath, []byte(script), 0o755))

	t.Setenv("PATH", tempDir)
	t.Setenv("MODEL_RUNNER_HOST", "")

	cfg := &latest.ModelConfig{
		Provider: "dmr",
		Model:    "ai/qwen3",
	}

	_, err := NewClient(t.Context(), cfg)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNotInstalled)
}

func TestGetDMRFallbackURLs(t *testing.T) {
	t.Parallel()

	t.Run("inside container", func(t *testing.T) {
		t.Parallel()

		urls := getDMRFallbackURLs(true)

		// Should return 3 container-specific fallback URLs
		require.Len(t, urls, 3)

		// Verify the expected URLs in order (container-specific endpoints)
		assert.Equal(t, "http://model-runner.docker.internal/engines/v1/", urls[0])
		assert.Equal(t, "http://host.docker.internal:12434/engines/v1/", urls[1])
		assert.Equal(t, "http://172.17.0.1:12434/engines/v1/", urls[2])
	})

	t.Run("on host", func(t *testing.T) {
		t.Parallel()

		urls := getDMRFallbackURLs(false)

		// Should return 1 host-specific fallback URL
		require.Len(t, urls, 1)

		// Verify localhost is the only fallback on host
		assert.Equal(t, "http://127.0.0.1:12434/engines/v1/", urls[0])
	})
}

func TestDMRConnectivity(t *testing.T) {
	t.Parallel()

	t.Run("reachable endpoint", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/models", r.URL.Path)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		defer server.Close()

		result := testDMRConnectivity(t.Context(), server.Client(), server.URL+"/")
		assert.True(t, result)
	})

	t.Run("reachable endpoint with error response", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		// Should still return true because server is reachable
		result := testDMRConnectivity(t.Context(), server.Client(), server.URL+"/")
		assert.True(t, result)
	})

	t.Run("unreachable endpoint", func(t *testing.T) {
		t.Parallel()

		// Use a port that's unlikely to have anything listening
		result := testDMRConnectivity(t.Context(), &http.Client{}, "http://127.0.0.1:59999/")
		assert.False(t, result)
	})
}

func TestNewClientWithWrongType(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		Provider: "openai",
		Model:    "gpt-4",
	}

	_, err := NewClient(t.Context(), cfg)
	require.Error(t, err)
}

func TestBuildConfigureURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		baseURL  string
		expected string
	}{
		{
			name:     "standard engines path",
			baseURL:  "http://127.0.0.1:12434/engines/v1/",
			expected: "http://127.0.0.1:12434/engines/_configure",
		},
		{
			name:     "standard engines path without trailing slash",
			baseURL:  "http://127.0.0.1:12434/engines/v1",
			expected: "http://127.0.0.1:12434/engines/_configure",
		},
		{
			name:     "Docker Desktop experimental prefix",
			baseURL:  "http://_/exp/vDD4.40/engines/v1",
			expected: "http://_/exp/vDD4.40/engines/_configure",
		},
		{
			name:     "Docker Desktop experimental prefix with trailing slash",
			baseURL:  "http://_/exp/vDD4.40/engines/v1/",
			expected: "http://_/exp/vDD4.40/engines/_configure",
		},
		{
			name:     "backend-scoped path",
			baseURL:  "http://127.0.0.1:12434/engines/llama.cpp/v1/",
			expected: "http://127.0.0.1:12434/engines/llama.cpp/_configure",
		},
		{
			name:     "container internal host",
			baseURL:  "http://model-runner.docker.internal/engines/v1/",
			expected: "http://model-runner.docker.internal/engines/_configure",
		},
		{
			name:     "custom port",
			baseURL:  "http://localhost:8080/engines/v1/",
			expected: "http://localhost:8080/engines/_configure",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := buildConfigureURL(tt.baseURL)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestBuildConfigureRequest(t *testing.T) {
	t.Parallel()

	t.Run("with all options", func(t *testing.T) {
		t.Parallel()
		specOpts := &speculativeDecodingOpts{
			draftModel:     "ai/qwen3:1B",
			numTokens:      5,
			acceptanceRate: 0.8,
		}
		contextSize := int64(8192)
		backendCfg := buildConfigureBackendConfig(&contextSize, []string{"--threads", "8"}, specOpts, nil, nil, nil)

		req := buildConfigureRequest("ai/qwen3:14B-Q6_K", backendCfg, nil, "")

		assert.Equal(t, "ai/qwen3:14B-Q6_K", req.Model)
		require.NotNil(t, req.ContextSize)
		assert.Equal(t, int32(8192), *req.ContextSize)
		assert.Equal(t, []string{"--threads", "8"}, req.RuntimeFlags)
		require.NotNil(t, req.Speculative)
		assert.Equal(t, "ai/qwen3:1B", req.Speculative.DraftModel)
		assert.Equal(t, 5, req.Speculative.NumTokens)
		assert.InEpsilon(t, 0.8, req.Speculative.MinAcceptanceRate, 0.001)
		assert.Nil(t, req.Mode)
		assert.Empty(t, req.RawRuntimeFlags)
		assert.Nil(t, req.KeepAlive)
		assert.Nil(t, req.VLLM)
	})

	t.Run("without speculative options", func(t *testing.T) {
		t.Parallel()
		contextSize := int64(4096)
		backendCfg := buildConfigureBackendConfig(&contextSize, []string{"--threads", "8"}, nil, nil, nil, nil)

		req := buildConfigureRequest("ai/qwen3:14B-Q6_K", backendCfg, nil, "")

		assert.Equal(t, "ai/qwen3:14B-Q6_K", req.Model)
		require.NotNil(t, req.ContextSize)
		assert.Equal(t, int32(4096), *req.ContextSize)
		assert.Equal(t, []string{"--threads", "8"}, req.RuntimeFlags)
		assert.Nil(t, req.Speculative)
		assert.Nil(t, req.Mode)
		assert.Empty(t, req.RawRuntimeFlags)
		assert.Nil(t, req.KeepAlive)
		assert.Nil(t, req.VLLM)
	})

	t.Run("without context size", func(t *testing.T) {
		t.Parallel()
		specOpts := &speculativeDecodingOpts{
			draftModel: "ai/qwen3:1B",
			numTokens:  5,
		}
		backendCfg := buildConfigureBackendConfig(nil, nil, specOpts, nil, nil, nil)

		req := buildConfigureRequest("ai/qwen3:14B-Q6_K", backendCfg, nil, "")

		assert.Equal(t, "ai/qwen3:14B-Q6_K", req.Model)
		assert.Nil(t, req.ContextSize)
		assert.Nil(t, req.RuntimeFlags)
		require.NotNil(t, req.Speculative)
		assert.Equal(t, "ai/qwen3:1B", req.Speculative.DraftModel)
		assert.Equal(t, 5, req.Speculative.NumTokens)
		assert.Nil(t, req.Mode)
		assert.Empty(t, req.RawRuntimeFlags)
		assert.Nil(t, req.KeepAlive)
		assert.Nil(t, req.VLLM)
	})

	t.Run("minimal config", func(t *testing.T) {
		t.Parallel()
		backendCfg := buildConfigureBackendConfig(nil, nil, nil, nil, nil, nil)
		req := buildConfigureRequest("ai/qwen3:14B-Q6_K", backendCfg, nil, "")

		assert.Equal(t, "ai/qwen3:14B-Q6_K", req.Model)
		assert.Nil(t, req.ContextSize)
		assert.Nil(t, req.RuntimeFlags)
		assert.Nil(t, req.Speculative)
		assert.Nil(t, req.LlamaCpp)
		assert.Nil(t, req.Mode)
		assert.Empty(t, req.RawRuntimeFlags)
		assert.Nil(t, req.KeepAlive)
		assert.Nil(t, req.VLLM)
	})

	t.Run("with llama.cpp reasoning budget", func(t *testing.T) {
		t.Parallel()
		rb := int32(16384)
		llama := &llamaCppConfig{ReasoningBudget: &rb}
		backendCfg := buildConfigureBackendConfig(nil, nil, nil, llama, nil, nil)
		req := buildConfigureRequest("ai/qwen3:14B-Q6_K", backendCfg, nil, "")
		require.NotNil(t, req.LlamaCpp)
		require.NotNil(t, req.LlamaCpp.ReasoningBudget)
		assert.Equal(t, int32(16384), *req.LlamaCpp.ReasoningBudget)
		assert.Nil(t, req.Mode)
		assert.Empty(t, req.RawRuntimeFlags)
		assert.Nil(t, req.KeepAlive)
		assert.Nil(t, req.VLLM)
	})
}

func TestConfigureModelViaAPI(t *testing.T) {
	t.Parallel()

	t.Run("successful configuration", func(t *testing.T) {
		t.Parallel()

		var receivedRequest configureRequest
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/engines/_configure", r.URL.Path)
			assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

			body, err := io.ReadAll(r.Body)
			if !assert.NoError(t, err) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			err = json.Unmarshal(body, &receivedRequest)
			if !assert.NoError(t, err) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			w.WriteHeader(http.StatusAccepted)
		}))
		defer server.Close()

		baseURL := server.URL + "/engines/v1/"
		contextSize := int64(8192)
		specOpts := &speculativeDecodingOpts{
			draftModel:     "ai/qwen3:1B",
			numTokens:      5,
			acceptanceRate: 0.8,
		}
		backendCfg := buildConfigureBackendConfig(&contextSize, []string{"--threads", "8"}, specOpts, nil, nil, nil)

		err := configureModel(t.Context(), server.Client(), baseURL, "ai/qwen3:14B", backendCfg, nil, "")
		require.NoError(t, err)

		// Verify request body
		assert.Equal(t, "ai/qwen3:14B", receivedRequest.Model)
		require.NotNil(t, receivedRequest.ContextSize)
		assert.Equal(t, int32(8192), *receivedRequest.ContextSize)
		assert.Equal(t, []string{"--threads", "8"}, receivedRequest.RuntimeFlags)
		require.NotNil(t, receivedRequest.Speculative)
		assert.Equal(t, "ai/qwen3:1B", receivedRequest.Speculative.DraftModel)
		assert.Equal(t, 5, receivedRequest.Speculative.NumTokens)
		assert.InEpsilon(t, 0.8, receivedRequest.Speculative.MinAcceptanceRate, 0.001)
	})

	t.Run("server returns error", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("internal error"))
		}))
		defer server.Close()

		baseURL := server.URL + "/engines/v1/"
		err := configureModel(t.Context(), server.Client(), baseURL, "ai/qwen3:14B", buildConfigureBackendConfig(nil, nil, nil, nil, nil, nil), nil, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "500")
		assert.Contains(t, err.Error(), "internal error")
	})

	t.Run("server returns conflict", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte("runner already active"))
		}))
		defer server.Close()

		baseURL := server.URL + "/engines/v1/"
		err := configureModel(t.Context(), server.Client(), baseURL, "ai/qwen3:14B", buildConfigureBackendConfig(nil, nil, nil, nil, nil, nil), nil, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "409")
		assert.Contains(t, err.Error(), "runner already active")
	})
}

func TestParseDMRProviderOptsWithSpeculativeDecoding(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		MaxTokens: new(int64(4096)),
		ProviderOpts: map[string]any{
			"context_size":                int64(16384),
			"speculative_draft_model":     "ai/qwen3:1B",
			"speculative_num_tokens":      "5",
			"speculative_acceptance_rate": "0.75",
			"runtime_flags":               []string{"--threads", "8"},
		},
	}

	res, err := parseDMRProviderOpts("llama.cpp", cfg)
	require.NoError(t, err)
	contextSize, runtimeFlags, specOpts, llamaCpp := res.contextSize, res.runtimeFlags, res.specOpts, res.llamaCpp

	require.NotNil(t, contextSize)
	assert.Equal(t, int64(16384), *contextSize)
	assert.Equal(t, []string{"--threads", "8"}, runtimeFlags)
	require.NotNil(t, specOpts)
	assert.Equal(t, "ai/qwen3:1B", specOpts.draftModel)
	assert.Equal(t, 5, specOpts.numTokens)
	assert.InEpsilon(t, 0.75, specOpts.acceptanceRate, 0.001)
	assert.Nil(t, llamaCpp)
}

func TestParseDMRProviderOptsWithoutSpeculativeDecoding(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		MaxTokens: new(int64(4096)),
		ProviderOpts: map[string]any{
			"runtime_flags": []string{"--threads", "8"},
		},
	}

	res, err := parseDMRProviderOpts("llama.cpp", cfg)
	require.NoError(t, err)
	contextSize, runtimeFlags, specOpts, llamaCpp := res.contextSize, res.runtimeFlags, res.specOpts, res.llamaCpp

	assert.Nil(t, contextSize, "context_size not in provider_opts, should be nil regardless of max_tokens")
	assert.Equal(t, []string{"--threads", "8"}, runtimeFlags)
	assert.Nil(t, specOpts)
	assert.Nil(t, llamaCpp)
}

func TestParseDMRProviderOptsContextSizeFromProviderOpts(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		MaxTokens: new(int64(4096)),
		ProviderOpts: map[string]any{
			"context_size": int64(32768),
		},
	}

	res, err := parseDMRProviderOpts("llama.cpp", cfg)
	require.NoError(t, err)
	contextSize, rf, spec, ll := res.contextSize, res.runtimeFlags, res.specOpts, res.llamaCpp
	require.NotNil(t, contextSize)
	assert.Equal(t, int64(32768), *contextSize)
	assert.Nil(t, rf)
	assert.Nil(t, spec)
	assert.Nil(t, ll)
}

func TestParseDMRProviderOptsContextSizeNeitherSet(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		Provider: "dmr",
		Model:    "ai/qwen3",
	}

	res, err := parseDMRProviderOpts("llama.cpp", cfg)
	require.NoError(t, err)
	contextSize, rf, spec, ll := res.contextSize, res.runtimeFlags, res.specOpts, res.llamaCpp
	assert.Nil(t, contextSize)
	assert.Nil(t, rf)
	assert.Nil(t, spec)
	assert.Nil(t, ll)
}

func TestParseDMRProviderOptsThinkingBudget(t *testing.T) {
	t.Parallel()

	t.Run("llama.cpp: effort maps to token budget", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{
			ThinkingBudget: &latest.ThinkingBudget{Effort: "medium"},
		}
		res, err := parseDMRProviderOpts("llama.cpp", cfg)
		require.NoError(t, err)
		llamaCpp := res.llamaCpp
		require.NotNil(t, llamaCpp)
		require.NotNil(t, llamaCpp.ReasoningBudget)
		assert.Equal(t, int32(8192), *llamaCpp.ReasoningBudget)
	})

	t.Run("llama.cpp: explicit tokens", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{
			ThinkingBudget: &latest.ThinkingBudget{Tokens: 2048},
		}
		res, err := parseDMRProviderOpts("llama.cpp", cfg)
		require.NoError(t, err)
		llamaCpp := res.llamaCpp
		require.NotNil(t, llamaCpp)
		require.NotNil(t, llamaCpp.ReasoningBudget)
		assert.Equal(t, int32(2048), *llamaCpp.ReasoningBudget)
	})

	t.Run("llama.cpp: disabled", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{
			ThinkingBudget: &latest.ThinkingBudget{Effort: "none"},
		}
		res, err := parseDMRProviderOpts("llama.cpp", cfg)
		require.NoError(t, err)
		llamaCpp := res.llamaCpp
		require.NotNil(t, llamaCpp)
		require.NotNil(t, llamaCpp.ReasoningBudget)
		assert.Equal(t, int32(0), *llamaCpp.ReasoningBudget)
	})

	t.Run("empty engine defaults to llama.cpp", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{
			ThinkingBudget: &latest.ThinkingBudget{Tokens: 4096},
		}
		res, err := parseDMRProviderOpts("", cfg)
		require.NoError(t, err)
		llamaCpp := res.llamaCpp
		require.NotNil(t, llamaCpp)
		require.NotNil(t, llamaCpp.ReasoningBudget)
		assert.Equal(t, int32(4096), *llamaCpp.ReasoningBudget)
	})

	t.Run("vllm engine: no llamacpp config (thinking handled per-request)", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{
			ThinkingBudget: &latest.ThinkingBudget{Effort: "high"},
		}
		res, err := parseDMRProviderOpts("vllm", cfg)
		require.NoError(t, err)
		llamaCpp := res.llamaCpp
		assert.Nil(t, llamaCpp, "vllm engine should not produce llamacpp config; thinking_budget is sent per-request instead")
	})
}

func TestParseVLLMConfig(t *testing.T) {
	t.Parallel()

	t.Run("nil opts returns nil", func(t *testing.T) {
		t.Parallel()
		got, err := parseVLLMConfig(nil)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("empty opts returns nil", func(t *testing.T) {
		t.Parallel()
		got, err := parseVLLMConfig(map[string]any{})
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("unrelated opts returns nil", func(t *testing.T) {
		t.Parallel()
		got, err := parseVLLMConfig(map[string]any{"foo": "bar"})
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("gpu_memory_utilization as float", func(t *testing.T) {
		t.Parallel()
		got, err := parseVLLMConfig(map[string]any{"gpu_memory_utilization": 0.9})
		require.NoError(t, err)
		require.NotNil(t, got)
		require.NotNil(t, got.GPUMemoryUtilization)
		assert.InEpsilon(t, 0.9, *got.GPUMemoryUtilization, 0.001)
		assert.Nil(t, got.HFOverrides)
	})

	t.Run("gpu_memory_utilization as string", func(t *testing.T) {
		t.Parallel()
		got, err := parseVLLMConfig(map[string]any{"gpu_memory_utilization": "0.75"})
		require.NoError(t, err)
		require.NotNil(t, got)
		require.NotNil(t, got.GPUMemoryUtilization)
		assert.InEpsilon(t, 0.75, *got.GPUMemoryUtilization, 0.001)
	})

	t.Run("gpu_memory_utilization with invalid type is ignored", func(t *testing.T) {
		t.Parallel()
		got, err := parseVLLMConfig(map[string]any{"gpu_memory_utilization": []int{1, 2}})
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("gpu_memory_utilization out of range returns error", func(t *testing.T) {
		t.Parallel()
		for _, val := range []float64{-0.1, 1.5, 2.0} {
			_, err := parseVLLMConfig(map[string]any{"gpu_memory_utilization": val})
			assert.Error(t, err, "expected error for gpu_memory_utilization=%v", val)
		}
	})

	t.Run("hf_overrides as map", func(t *testing.T) {
		t.Parallel()
		overrides := map[string]any{"max_model_len": 4096, "dtype": "bfloat16"}
		got, err := parseVLLMConfig(map[string]any{"hf_overrides": overrides})
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, overrides, got.HFOverrides)
		assert.Nil(t, got.GPUMemoryUtilization)
	})

	t.Run("hf_overrides with non-map value is ignored", func(t *testing.T) {
		t.Parallel()
		got, err := parseVLLMConfig(map[string]any{"hf_overrides": "not-a-map"})
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("hf_overrides with invalid key returns error", func(t *testing.T) {
		t.Parallel()
		_, err := parseVLLMConfig(map[string]any{
			"hf_overrides": map[string]any{"--malicious": "bad"},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid hf_overrides key")
	})

	t.Run("hf_overrides with invalid nested key returns error", func(t *testing.T) {
		t.Parallel()
		_, err := parseVLLMConfig(map[string]any{
			"hf_overrides": map[string]any{
				"good_key": map[string]any{"--bad": 1},
			},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid hf_overrides nested key")
	})

	t.Run("hf_overrides with valid nested values is accepted", func(t *testing.T) {
		t.Parallel()
		overrides := map[string]any{
			"rope_scaling": map[string]any{
				"type":   "yarn",
				"factor": 2.0,
			},
			"tags": []any{"v1", "v2"},
		}
		got, err := parseVLLMConfig(map[string]any{"hf_overrides": overrides})
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, overrides, got.HFOverrides)
	})

	t.Run("both options together", func(t *testing.T) {
		t.Parallel()
		got, err := parseVLLMConfig(map[string]any{
			"gpu_memory_utilization": 0.85,
			"hf_overrides":           map[string]any{"dtype": "float16"},
		})
		require.NoError(t, err)
		require.NotNil(t, got)
		require.NotNil(t, got.GPUMemoryUtilization)
		assert.InEpsilon(t, 0.85, *got.GPUMemoryUtilization, 0.001)
		assert.Equal(t, "float16", got.HFOverrides["dtype"])
	})
}

func TestParseDMRProviderOptsVLLMEngine(t *testing.T) {
	t.Parallel()

	t.Run("vllm engine populates vllm config", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{
			ProviderOpts: map[string]any{
				"gpu_memory_utilization": 0.9,
				"hf_overrides":           map[string]any{"max_model_len": 8192},
			},
		}
		res, err := parseDMRProviderOpts("vllm", cfg)
		require.NoError(t, err)
		llamaCpp, vllm := res.llamaCpp, res.vllm
		assert.Nil(t, llamaCpp, "llamacpp config should not be set for vllm engine")
		require.NotNil(t, vllm)
		require.NotNil(t, vllm.GPUMemoryUtilization)
		assert.InEpsilon(t, 0.9, *vllm.GPUMemoryUtilization, 0.001)
		assert.Equal(t, 8192, vllm.HFOverrides["max_model_len"])
	})

	t.Run("llama.cpp engine ignores vllm opts", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{
			ProviderOpts: map[string]any{
				"gpu_memory_utilization": 0.9,
				"hf_overrides":           map[string]any{"dtype": "float16"},
			},
		}
		res, err := parseDMRProviderOpts("llama.cpp", cfg)
		require.NoError(t, err)
		vllm := res.vllm
		assert.Nil(t, vllm, "vllm config should not be set for llama.cpp engine")
	})

	t.Run("vllm engine flows end-to-end into configure request JSON", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{
			ProviderOpts: map[string]any{
				"gpu_memory_utilization": 0.85,
				"hf_overrides":           map[string]any{"test_key": "test-value"},
			},
		}
		res, err := parseDMRProviderOpts("vllm", cfg)
		require.NoError(t, err)
		contextSize, runtimeFlags, specOpts, llamaCpp, vllm := res.contextSize, res.runtimeFlags, res.specOpts, res.llamaCpp, res.vllm
		backendCfg := buildConfigureBackendConfig(contextSize, runtimeFlags, specOpts, llamaCpp, vllm, nil)
		req := buildConfigureRequest("ai/vllm-model", backendCfg, nil, "")

		data, err := json.Marshal(req)
		require.NoError(t, err)

		var parsed map[string]any
		err = json.Unmarshal(data, &parsed)
		require.NoError(t, err)

		vllmParsed, ok := parsed["vllm"].(map[string]any)
		require.True(t, ok, "vllm key should be present in JSON")
		assert.InEpsilon(t, 0.85, vllmParsed["gpu-memory-utilization"].(float64), 0.001)
		hfOverrides, ok := vllmParsed["hf-overrides"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "test-value", hfOverrides["test_key"])
	})
}

func TestBuildVLLMRequestFields(t *testing.T) {
	t.Parallel()

	t.Run("nil config returns nil", func(t *testing.T) {
		t.Parallel()
		fields := buildVLLMRequestFields(nil)
		assert.Nil(t, fields)
	})

	t.Run("nil budget returns nil", func(t *testing.T) {
		t.Parallel()
		fields := buildVLLMRequestFields(&latest.ModelConfig{})
		assert.Nil(t, fields)
	})

	t.Run("disabled (effort none) returns 0", func(t *testing.T) {
		t.Parallel()
		fields := buildVLLMRequestFields(&latest.ModelConfig{
			ThinkingBudget: &latest.ThinkingBudget{Effort: "none"},
		})
		require.NotNil(t, fields)
		assert.Equal(t, int64(0), fields["thinking_token_budget"])
	})

	t.Run("explicit token count", func(t *testing.T) {
		t.Parallel()
		fields := buildVLLMRequestFields(&latest.ModelConfig{
			ThinkingBudget: &latest.ThinkingBudget{Tokens: 4096},
		})
		require.NotNil(t, fields)
		assert.Equal(t, int64(4096), fields["thinking_token_budget"])
	})

	t.Run("effort medium maps to 8192", func(t *testing.T) {
		t.Parallel()
		fields := buildVLLMRequestFields(&latest.ModelConfig{
			ThinkingBudget: &latest.ThinkingBudget{Effort: "medium"},
		})
		require.NotNil(t, fields)
		assert.Equal(t, int64(8192), fields["thinking_token_budget"])
	})

	t.Run("effort high maps to 16384", func(t *testing.T) {
		t.Parallel()
		fields := buildVLLMRequestFields(&latest.ModelConfig{
			ThinkingBudget: &latest.ThinkingBudget{Effort: "high"},
		})
		require.NotNil(t, fields)
		assert.Equal(t, int64(16384), fields["thinking_token_budget"])
	})

	t.Run("adaptive returns -1 (unlimited)", func(t *testing.T) {
		t.Parallel()
		fields := buildVLLMRequestFields(&latest.ModelConfig{
			ThinkingBudget: &latest.ThinkingBudget{Effort: "adaptive"},
		})
		require.NotNil(t, fields)
		assert.Equal(t, int64(-1), fields["thinking_token_budget"])
	})
}

func TestResolveReasoningBudget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      *latest.ThinkingBudget
		wantBudget int64
		wantOK     bool
	}{
		{
			name:       "nil → (0, false)",
			input:      nil,
			wantBudget: 0,
			wantOK:     false,
		},
		{
			name:       "disabled via Tokens:0 → (0, true)",
			input:      &latest.ThinkingBudget{Tokens: 0},
			wantBudget: 0,
			wantOK:     true,
		},
		{
			name:       "disabled via Effort:none → (0, true)",
			input:      &latest.ThinkingBudget{Effort: "none"},
			wantBudget: 0,
			wantOK:     true,
		},
		{
			name:       "explicit Tokens:4096 → (4096, true)",
			input:      &latest.ThinkingBudget{Tokens: 4096},
			wantBudget: 4096,
			wantOK:     true,
		},
		{
			name:       "explicit Tokens:-1 (dynamic) → (-1, true)",
			input:      &latest.ThinkingBudget{Tokens: -1},
			wantBudget: -1,
			wantOK:     true,
		},
		{
			name:       "Effort:minimal → (1024, true)",
			input:      &latest.ThinkingBudget{Effort: "minimal"},
			wantBudget: 1024,
			wantOK:     true,
		},
		{
			name:       "Effort:low → (2048, true)",
			input:      &latest.ThinkingBudget{Effort: "low"},
			wantBudget: 2048,
			wantOK:     true,
		},
		{
			name:       "Effort:medium → (8192, true)",
			input:      &latest.ThinkingBudget{Effort: "medium"},
			wantBudget: 8192,
			wantOK:     true,
		},
		{
			name:       "Effort:high → (16384, true)",
			input:      &latest.ThinkingBudget{Effort: "high"},
			wantBudget: 16384,
			wantOK:     true,
		},
		{
			name:       "Effort:adaptive → (-1, true)",
			input:      &latest.ThinkingBudget{Effort: "adaptive"},
			wantBudget: -1,
			wantOK:     true,
		},
		{
			name:       "Effort:adaptive/low → (-1, true)",
			input:      &latest.ThinkingBudget{Effort: "adaptive/low"},
			wantBudget: -1,
			wantOK:     true,
		},
		{
			name:       "Effort:unknown → (-1, true)",
			input:      &latest.ThinkingBudget{Effort: "unknown"},
			wantBudget: -1,
			wantOK:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotBudget, gotOK := resolveReasoningBudget(tt.input)
			assert.Equal(t, tt.wantBudget, gotBudget)
			assert.Equal(t, tt.wantOK, gotOK)
		})
	}
}

func TestConfigureRequestJSONSerialization(t *testing.T) {
	t.Parallel()

	t.Run("full request serializes correctly", func(t *testing.T) {
		t.Parallel()
		contextSize := int32(8192)
		reasoning := int32(-1)
		req := configureRequest{
			Model: "ai/qwen3:14B",
			configureBackendConfig: configureBackendConfig{
				ContextSize:  &contextSize,
				RuntimeFlags: []string{"--keep-alive", "5m"},
				Speculative: &speculativeDecodingRequest{
					DraftModel:        "ai/qwen3:1B",
					NumTokens:         5,
					MinAcceptanceRate: 0.8,
				},
				LlamaCpp: &llamaCppConfig{ReasoningBudget: &reasoning},
			},
		}

		data, err := json.Marshal(req)
		require.NoError(t, err)

		var parsed map[string]any
		err = json.Unmarshal(data, &parsed)
		require.NoError(t, err)

		assert.Equal(t, "ai/qwen3:14B", parsed["model"])
		assert.InEpsilon(t, float64(8192), parsed["context-size"].(float64), 0.001)
		assert.Equal(t, []any{"--keep-alive", "5m"}, parsed["runtime-flags"])

		spec, ok := parsed["speculative"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "ai/qwen3:1B", spec["draft_model"])
		assert.InEpsilon(t, float64(5), spec["num_tokens"].(float64), 0.001)
		assert.InEpsilon(t, 0.8, spec["min_acceptance_rate"].(float64), 0.001)

		llama, ok := parsed["llamacpp"].(map[string]any)
		require.True(t, ok)
		assert.InEpsilon(t, float64(-1), llama["reasoning-budget"].(float64), 0.001)
	})

	t.Run("minimal request omits nil fields", func(t *testing.T) {
		t.Parallel()
		req := configureRequest{
			Model: "ai/qwen3:14B",
		}

		data, err := json.Marshal(req)
		require.NoError(t, err)

		var parsed map[string]any
		err = json.Unmarshal(data, &parsed)
		require.NoError(t, err)

		assert.Equal(t, "ai/qwen3:14B", parsed["model"])
		_, hasContextSize := parsed["context-size"]
		assert.False(t, hasContextSize, "context-size should be omitted when nil")
		_, hasRuntimeFlags := parsed["runtime-flags"]
		assert.False(t, hasRuntimeFlags, "runtime-flags should be omitted when nil")
		_, hasSpeculative := parsed["speculative"]
		assert.False(t, hasSpeculative, "speculative should be omitted when nil")
		_, hasLlamaCpp := parsed["llamacpp"]
		assert.False(t, hasLlamaCpp, "llamacpp should be omitted when nil")
		_, hasMode := parsed["mode"]
		assert.False(t, hasMode, "mode should be omitted when nil")
		_, hasRawRuntimeFlags := parsed["raw-runtime-flags"]
		assert.False(t, hasRawRuntimeFlags, "raw-runtime-flags should be omitted when empty")
		_, hasKeepAlive := parsed["keep_alive"]
		assert.False(t, hasKeepAlive, "keep_alive should be omitted when nil")
		_, hasVLLM := parsed["vllm"]
		assert.False(t, hasVLLM, "vllm should be omitted when nil")
	})

	t.Run("schema parity fields serialize with expected keys", func(t *testing.T) {
		t.Parallel()
		mode := "completion"
		keepAlive := "5m"
		gpu := 0.9
		req := configureRequest{
			Model:           "ai/qwen3:14B",
			Mode:            &mode,
			RawRuntimeFlags: "--foo --bar",
			configureBackendConfig: configureBackendConfig{
				KeepAlive: &keepAlive,
				VLLM: &vllmConfig{
					HFOverrides:          map[string]any{"foo": "bar"},
					GPUMemoryUtilization: &gpu,
				},
			},
		}

		data, err := json.Marshal(req)
		require.NoError(t, err)

		var parsed map[string]any
		err = json.Unmarshal(data, &parsed)
		require.NoError(t, err)

		assert.Equal(t, "completion", parsed["mode"])
		assert.Equal(t, "--foo --bar", parsed["raw-runtime-flags"])
		assert.Equal(t, "5m", parsed["keep_alive"])

		vllm, ok := parsed["vllm"].(map[string]any)
		require.True(t, ok)
		hfOverrides, ok := vllm["hf-overrides"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "bar", hfOverrides["foo"])
		assert.InEpsilon(t, 0.9, vllm["gpu-memory-utilization"].(float64), 0.001)
	})
}

func TestParseKeepAlive(t *testing.T) {
	t.Parallel()

	t.Run("nil opts returns nil", func(t *testing.T) {
		t.Parallel()
		got, err := parseKeepAlive(nil)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("unset returns nil", func(t *testing.T) {
		t.Parallel()
		got, err := parseKeepAlive(map[string]any{"other": 1})
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("valid durations", func(t *testing.T) {
		t.Parallel()
		for _, v := range []string{"5m", "1h", "30s", "2h30m", "0", "-1"} {
			got, err := parseKeepAlive(map[string]any{"keep_alive": v})
			require.NoErrorf(t, err, "value %q should be valid", v)
			require.NotNil(t, got)
			assert.Equal(t, v, *got)
		}
	})

	t.Run("non-string rejected", func(t *testing.T) {
		t.Parallel()
		_, err := parseKeepAlive(map[string]any{"keep_alive": 300})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be a string")
	})

	t.Run("empty string rejected", func(t *testing.T) {
		t.Parallel()
		_, err := parseKeepAlive(map[string]any{"keep_alive": "   "})
		require.Error(t, err)
	})

	t.Run("bad duration rejected", func(t *testing.T) {
		t.Parallel()
		_, err := parseKeepAlive(map[string]any{"keep_alive": "not-a-duration"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid keep_alive")
	})
}

func TestParseMode(t *testing.T) {
	t.Parallel()

	t.Run("nil opts returns nil", func(t *testing.T) {
		t.Parallel()
		got, err := parseMode(nil)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("unset returns nil", func(t *testing.T) {
		t.Parallel()
		got, err := parseMode(map[string]any{"other": 1})
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("valid modes accepted", func(t *testing.T) {
		t.Parallel()
		for _, m := range []string{"completion", "embedding", "reranking", "image-generation"} {
			got, err := parseMode(map[string]any{"mode": m})
			require.NoErrorf(t, err, "mode %q should be valid", m)
			require.NotNil(t, got)
			assert.Equal(t, m, *got)
		}
	})

	t.Run("unknown mode rejected", func(t *testing.T) {
		t.Parallel()
		_, err := parseMode(map[string]any{"mode": "nonsense"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid")
	})

	t.Run("non-string rejected", func(t *testing.T) {
		t.Parallel()
		_, err := parseMode(map[string]any{"mode": 1})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be a string")
	})
}

func TestParseRawRuntimeFlags(t *testing.T) {
	t.Parallel()

	t.Run("unset returns empty", func(t *testing.T) {
		t.Parallel()
		got, err := parseRawRuntimeFlags(nil)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("set returns value", func(t *testing.T) {
		t.Parallel()
		got, err := parseRawRuntimeFlags(map[string]any{"raw_runtime_flags": "--foo bar"})
		require.NoError(t, err)
		assert.Equal(t, "--foo bar", got)
	})

	t.Run("whitespace only returns empty", func(t *testing.T) {
		t.Parallel()
		got, err := parseRawRuntimeFlags(map[string]any{"raw_runtime_flags": "   "})
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("non-string rejected", func(t *testing.T) {
		t.Parallel()
		_, err := parseRawRuntimeFlags(map[string]any{"raw_runtime_flags": 123})
		require.Error(t, err)
	})
}

func TestParseDMRProviderOptsKeepAliveAndMode(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		ProviderOpts: map[string]any{
			"keep_alive": "10m",
			"mode":       "embedding",
		},
	}
	res, err := parseDMRProviderOpts("llama.cpp", cfg)
	require.NoError(t, err)
	require.NotNil(t, res.keepAlive)
	assert.Equal(t, "10m", *res.keepAlive)
	require.NotNil(t, res.mode)
	assert.Equal(t, "embedding", *res.mode)
}

func TestParseDMRProviderOptsRejectsBothRuntimeFlagVariants(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		ProviderOpts: map[string]any{
			"runtime_flags":     []string{"--threads", "8"},
			"raw_runtime_flags": "--threads 8",
		},
	}
	_, err := parseDMRProviderOpts("llama.cpp", cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot set both")
}

func TestParseDMRProviderOptsPropagatesValidationError(t *testing.T) {
	t.Parallel()

	t.Run("bad keep_alive fails parseDMRProviderOpts", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{
			ProviderOpts: map[string]any{"keep_alive": "banana"},
		}
		_, err := parseDMRProviderOpts("llama.cpp", cfg)
		require.Error(t, err)
	})

	t.Run("bad mode fails parseDMRProviderOpts", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{
			ProviderOpts: map[string]any{"mode": "banana"},
		}
		_, err := parseDMRProviderOpts("llama.cpp", cfg)
		require.Error(t, err)
	})

	t.Run("bad hf_overrides fails parseDMRProviderOpts (vllm engine)", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{
			ProviderOpts: map[string]any{
				"hf_overrides": map[string]any{"--bad": 1},
			},
		}
		_, err := parseDMRProviderOpts("vllm", cfg)
		require.Error(t, err)
	})
}

func TestConfigureRequestTopLevelFieldsEndToEnd(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		ProviderOpts: map[string]any{
			"keep_alive":        "-1",
			"mode":              "completion",
			"raw_runtime_flags": "--threads 8 --ctx 4096",
		},
	}
	res, err := parseDMRProviderOpts("llama.cpp", cfg)
	require.NoError(t, err)

	backendCfg := buildConfigureBackendConfig(res.contextSize, res.runtimeFlags, res.specOpts, res.llamaCpp, res.vllm, res.keepAlive)
	req := buildConfigureRequest("ai/qwen3", backendCfg, res.mode, res.rawRuntimeFlags)

	data, err := json.Marshal(req)
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(data, &parsed))

	assert.Equal(t, "ai/qwen3", parsed["model"])
	assert.Equal(t, "-1", parsed["keep_alive"])
	assert.Equal(t, "completion", parsed["mode"])
	assert.Equal(t, "--threads 8 --ctx 4096", parsed["raw-runtime-flags"])
}

func TestNoThinkingSetsChatTemplateKwargsAndBumpsMaxTokens(t *testing.T) {
	t.Parallel()

	var captured []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			body, _ := io.ReadAll(r.Body)
			captured = body
			// Return a minimal streaming response so the SDK is happy.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	maxTokens := int64(20)
	cfg := &latest.ModelConfig{
		Provider:  "dmr",
		Model:     "ai/qwen3",
		BaseURL:   server.URL + "/engines/v1/",
		MaxTokens: &maxTokens,
	}
	client, err := NewClient(t.Context(), cfg, options.WithNoThinking())
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
		{Role: chat.MessageRoleUser, Content: "hi"},
	}, nil)
	require.NoError(t, err)
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
	stream.Close()

	require.NotEmpty(t, captured, "chat/completions should have been called")

	var req map[string]any
	require.NoError(t, json.Unmarshal(captured, &req))

	// max_tokens floor (20 -> 256).
	assert.EqualValues(t, noThinkingMinOutputTokens, req["max_tokens"])

	// chat_template_kwargs.enable_thinking=false on every engine.
	ct, ok := req["chat_template_kwargs"].(map[string]any)
	require.True(t, ok, "chat_template_kwargs must be present")
	assert.Equal(t, false, ct["enable_thinking"])
}
