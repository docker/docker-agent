package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

type cloneTestEnvProvider struct {
	values map[string]string
}

func (m *cloneTestEnvProvider) Get(_ context.Context, name string) (string, bool) {
	v, ok := m.values[name]
	return v, ok
}

func newCloneTestEnv(values map[string]string) environment.Provider {
	return &cloneTestEnvProvider{values: values}
}

func TestCloneWithOptions_RouterWithModelReferences(t *testing.T) {
	t.Parallel()

	// This test verifies that cloning a router with model references works correctly.
	// Previously, CloneWithOptions would fail silently because it called New() instead
	// of NewWithModels(), which meant the models map was nil and model references
	// like "fast" couldn't be resolved.

	// Create a mock server that returns a minimal valid response
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	models := map[string]latest.ModelConfig{
		"fast": {
			Provider: "openai",
			Model:    "gpt-4o-mini",
			BaseURL:  server.URL,
		},
		"capable": {
			Provider: "openai",
			Model:    "gpt-4o",
			BaseURL:  server.URL,
		},
	}

	routerCfg := &latest.ModelConfig{
		Provider: "openai",
		Model:    "gpt-4o-mini", // fallback
		BaseURL:  server.URL,
		Routing: []latest.RoutingRule{
			{
				Model:    "fast",
				Examples: []string{"hello", "hi"},
			},
			{
				Model:    "capable",
				Examples: []string{"explain", "analyze"},
			},
		},
	}

	env := newCloneTestEnv(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})

	// Create the router with the models map
	router, err := NewWithModels(t.Context(), routerCfg, models, env)
	require.NoError(t, err)

	// Verify the original router has the models map stored
	baseConfig := router.BaseConfig()
	require.NotNil(t, baseConfig.Models, "Router should store models map in base config")

	// Clone with max tokens option - this should succeed and not fall back to original
	newMaxTokens := int64(4096)
	cloned := CloneWithOptions(t.Context(), router, options.WithMaxTokens(newMaxTokens))

	// The clone should have the option applied
	clonedConfig := cloned.BaseConfig()
	require.NotNil(t, clonedConfig.ModelConfig.MaxTokens)
	assert.Equal(t, newMaxTokens, *clonedConfig.ModelConfig.MaxTokens)

	// Also verify the models map is preserved in the clone
	assert.NotNil(t, clonedConfig.Models, "Cloned router should preserve models map")
	assert.Equal(t, models, clonedConfig.Models, "Models map should be identical after cloning")
}

func TestCloneWithOptions_DirectProvider(t *testing.T) {
	t.Parallel()

	// Create a mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	// Test that cloning a non-router provider works correctly
	cfg := &latest.ModelConfig{
		Provider: "openai",
		Model:    "gpt-4o",
		BaseURL:  server.URL,
	}

	env := newCloneTestEnv(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})

	provider, err := New(t.Context(), cfg, env)
	require.NoError(t, err)

	// Clone with max tokens
	newMaxTokens := int64(2048)
	cloned := CloneWithOptions(t.Context(), provider, options.WithMaxTokens(newMaxTokens))

	clonedConfig := cloned.BaseConfig()
	require.NotNil(t, clonedConfig.ModelConfig.MaxTokens)
	assert.Equal(t, newMaxTokens, *clonedConfig.ModelConfig.MaxTokens)
}

func TestCloneWithOptions_PreservesMaxTokens(t *testing.T) {
	t.Parallel()

	// This test verifies that max_tokens is preserved when cloning a provider
	// with options that don't explicitly set max_tokens.

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	maxTokens := int64(8192)
	cfg := &latest.ModelConfig{
		Provider:  "openai",
		Model:     "gpt-4o",
		BaseURL:   server.URL,
		MaxTokens: &maxTokens,
	}

	env := newCloneTestEnv(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})

	provider, err := New(t.Context(), cfg, env, options.WithMaxTokens(maxTokens))
	require.NoError(t, err)

	// Clone with an option that doesn't affect max_tokens (e.g., WithGeneratingTitle)
	cloned := CloneWithOptions(t.Context(), provider, options.WithGeneratingTitle())

	clonedConfig := cloned.BaseConfig()

	// MaxTokens should be preserved, not cleared to 0 or nil
	require.NotNil(t, clonedConfig.ModelConfig.MaxTokens,
		"MaxTokens should be preserved after cloning with unrelated options")
	assert.Equal(t, maxTokens, *clonedConfig.ModelConfig.MaxTokens,
		"MaxTokens value should be unchanged after cloning")
}

func TestCloneWithOptions_OverridesMaxTokens(t *testing.T) {
	t.Parallel()

	// This test verifies that max_tokens can be explicitly overridden when cloning.

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	originalMaxTokens := int64(8192)
	newMaxTokens := int64(4096)

	cfg := &latest.ModelConfig{
		Provider:  "openai",
		Model:     "gpt-4o",
		BaseURL:   server.URL,
		MaxTokens: &originalMaxTokens,
	}

	env := newCloneTestEnv(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})

	provider, err := New(t.Context(), cfg, env, options.WithMaxTokens(originalMaxTokens))
	require.NoError(t, err)

	// Clone with an explicit max_tokens override
	cloned := CloneWithOptions(t.Context(), provider, options.WithMaxTokens(newMaxTokens))

	clonedConfig := cloned.BaseConfig()

	// MaxTokens should be updated to the new value
	require.NotNil(t, clonedConfig.ModelConfig.MaxTokens,
		"MaxTokens should not be nil after cloning with explicit override")
	assert.Equal(t, newMaxTokens, *clonedConfig.ModelConfig.MaxTokens,
		"MaxTokens should be updated to the new value")
}
