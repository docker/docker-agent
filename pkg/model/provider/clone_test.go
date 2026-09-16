//go:build !js && !docker_agent_no_openai

package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
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
	router, err := fullTestRegistry().NewWithModels(t.Context(), routerCfg, models, env)
	require.NoError(t, err)

	// The clone must retain the captured model map so named routing targets
	// remain resolvable.
	newMaxTokens := int64(4096)
	cloned := CloneWithOptions(t.Context(), router, options.WithMaxTokens(newMaxTokens))

	// The clone should have the option applied
	clonedConfig := cloned.BaseConfig()
	require.NotNil(t, clonedConfig.ModelConfig.MaxTokens)
	assert.Equal(t, newMaxTokens, *clonedConfig.ModelConfig.MaxTokens)

	// A matched named target proves that the clone retained the router's model map.
	stream, err := cloned.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "analyze this"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainStream(t, stream)
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

	provider, err := fullTestRegistry().New(t.Context(), cfg, env)
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

	provider, err := fullTestRegistry().New(t.Context(), cfg, env, options.WithMaxTokens(maxTokens))
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

func TestCloneWithOptions_PreservesOutputCapabilities(t *testing.T) {
	t.Parallel()

	image := true
	cfg := base.Config{ModelConfig: latest.ModelConfig{
		OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: &image},
	}}

	cloned, _ := mergeCloneOptions(cfg, []options.Opt{options.WithGeneratingTitle()})

	require.NotNil(t, cloned.OutputCapabilities)
	require.NotNil(t, cloned.OutputCapabilities.Image)
	assert.True(t, *cloned.OutputCapabilities.Image)
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

	provider, err := fullTestRegistry().New(t.Context(), cfg, env, options.WithMaxTokens(originalMaxTokens))
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

// TestCloneWithOptions_PreservesOpenAIVendorBit_NamedCustomProvider verifies
// that the genuine-OpenAI-vendor bit resolved for a named custom OpenAI
// provider (providers: section, no explicit `provider:` override) survives
// CloneWithOptions(WithNoThinking()), the path used for e.g. title generation
// and sampling clones. The clone re-enters createDirectProvider, which
// recomputes isOpenAIVendor from the base config (preserved via
// options.WithProviders through options.FromModelOptions) rather than
// trusting a stale copy, so the wire behavior (reasoning_effort="none") must
// hold on the clone too, not just the original provider.
func TestCloneWithOptions_PreservesOpenAIVendorBit_NamedCustomProvider(t *testing.T) {
	t.Parallel()

	server, body := captureNamedCustomProviderRequestBody(t, "openai_chatcompletions")

	customProviders := map[string]latest.ProviderConfig{
		"my_openai": {
			BaseURL:  server.URL,
			TokenKey: "MY_OPENAI_TOKEN",
			APIType:  "openai_chatcompletions",
		},
	}
	modelCfg := &latest.ModelConfig{Provider: "my_openai", Model: "gpt-5.6"}
	env := environment.NewMapEnvProvider(map[string]string{"MY_OPENAI_TOKEN": "secret"})

	baseProvider, err := fullTestRegistry().New(t.Context(), modelCfg, env, options.WithProviders(customProviders))
	require.NoError(t, err)
	baseOpts := baseProvider.BaseConfig().ModelOptions
	require.True(t, baseOpts.OpenAIVendor(), "base provider should resolve the OpenAI vendor bit")

	cloned := CloneWithOptions(t.Context(), baseProvider, options.WithNoThinking())
	clonedOpts := cloned.BaseConfig().ModelOptions
	require.True(t, clonedOpts.OpenAIVendor(), "clone must preserve the OpenAI vendor bit")

	stream, err := cloned.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainStream(t, stream)

	var req struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	require.NoError(t, json.Unmarshal(body(), &req))
	assert.Equal(t, "none", req.ReasoningEffort, "cloned NoThinking() path must still send gpt-5.6's real none effort")
}

// TestCloneWithOptions_ThinkingSwitch_LocalModel verifies the title-generation
// and compaction shape on a user-supplied OpenAI-compatible endpoint: a
// NoThinking() clone re-enters the factory with the already-resolved config
// (base_url filled, a none budget injected by mergeCloneOptions) and must
// send chat_template_kwargs only when the user's own YAML disabled thinking.
func TestCloneWithOptions_ThinkingSwitch_LocalModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		budget   *latest.ThinkingBudget
		wantSent bool
	}{
		{name: "thinking_budget: none in YAML", budget: &latest.ThinkingBudget{Effort: "none"}, wantSent: true},
		{name: "thinking_budget unset in YAML", budget: nil, wantSent: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server, body := captureNamedCustomProviderRequestBody(t, "openai_chatcompletions")
			modelCfg := &latest.ModelConfig{Provider: "openai", Model: "mlx-community/Qwen3.6-35B-A3B-8bit", BaseURL: server.URL, ThinkingBudget: tt.budget}
			env := environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "secret"})

			baseProvider, err := fullTestRegistry().New(t.Context(), modelCfg, env)
			require.NoError(t, err)
			baseOpts := baseProvider.BaseConfig().ModelOptions
			require.Equal(t, tt.wantSent, baseOpts.ChatTemplateThinkingOff())

			cloned := CloneWithOptions(t.Context(), baseProvider, options.WithNoThinking())
			clonedOpts := cloned.BaseConfig().ModelOptions
			require.Equal(t, tt.wantSent, clonedOpts.ChatTemplateThinkingOff(), "clone must keep the user's bit, not derive it from NoThinking")

			stream, err := cloned.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hi"}}, nil)
			require.NoError(t, err)
			defer stream.Close()
			drainStream(t, stream)

			var req map[string]any
			require.NoError(t, json.Unmarshal(body(), &req))
			if tt.wantSent {
				assert.Equal(t, map[string]any{"enable_thinking": false}, req["chat_template_kwargs"])
			} else {
				assert.NotContains(t, req, "chat_template_kwargs")
			}
			assert.NotContains(t, req, "reasoning_effort")
		})
	}
}

// TestCloneWithOptions_KeepsOpenAIVendorFalse_UnrelatedAlias is the negative
// counterpart: cloning a provider built on a known non-OpenAI alias (xai,
// mistral) must not somehow gain the OpenAI vendor bit, so the clone's
// NoThinking() path keeps sending "low".
func TestCloneWithOptions_KeepsOpenAIVendorFalse_UnrelatedAlias(t *testing.T) {
	t.Parallel()

	server, body := captureNamedCustomProviderRequestBody(t, "openai_chatcompletions")

	modelCfg := &latest.ModelConfig{
		Provider: "xai",
		Model:    "gpt-5.6",
		BaseURL:  server.URL,
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
		},
	}
	env := environment.NewMapEnvProvider(map[string]string{"XAI_API_KEY": "secret"})

	baseProvider, err := fullTestRegistry().New(t.Context(), modelCfg, env)
	require.NoError(t, err)
	baseOpts := baseProvider.BaseConfig().ModelOptions
	require.False(t, baseOpts.OpenAIVendor())

	cloned := CloneWithOptions(t.Context(), baseProvider, options.WithNoThinking())
	clonedOpts := cloned.BaseConfig().ModelOptions
	require.False(t, clonedOpts.OpenAIVendor())

	stream, err := cloned.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainStream(t, stream)

	var req struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	require.NoError(t, json.Unmarshal(body(), &req))
	assert.Equal(t, "low", req.ReasoningEffort)
}
