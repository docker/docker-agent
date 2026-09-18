package base

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

func TestConfigCapsOverride(t *testing.T) {
	t.Parallel()

	t.Run("nil when config declares no capabilities", func(t *testing.T) {
		t.Parallel()
		c := &Config{ModelConfig: latest.ModelConfig{Provider: "openai", Model: "gpt-4o"}}
		assert.Nil(t, c.CapsOverride())
	})

	t.Run("mirrors the declared capabilities", func(t *testing.T) {
		t.Parallel()
		c := &Config{ModelConfig: latest.ModelConfig{
			Provider:     "ollama",
			Model:        "llava",
			Capabilities: &latest.CapabilitiesConfig{Image: true, PDF: false},
		}}
		got := c.CapsOverride()
		require.NotNil(t, got)
		assert.Equal(t, &modelinfo.CapsOverride{Image: true, PDF: false}, got)
	})

	t.Run("mirrors declared audio/video capabilities", func(t *testing.T) {
		t.Parallel()
		c := &Config{ModelConfig: latest.ModelConfig{
			Provider:     "vision-proxy",
			Model:        "gemini-2.5-pro",
			Capabilities: &latest.CapabilitiesConfig{Image: true, PDF: true, Audio: true, Video: true},
		}}
		got := c.CapsOverride()
		require.NotNil(t, got)
		assert.Equal(t, &modelinfo.CapsOverride{Image: true, PDF: true, Audio: true, Video: true}, got)
	})

	t.Run("omitted audio/video default to false", func(t *testing.T) {
		t.Parallel()
		c := &Config{ModelConfig: latest.ModelConfig{
			Provider:     "ollama",
			Model:        "llava",
			Capabilities: &latest.CapabilitiesConfig{Image: true, PDF: false},
		}}
		got := c.CapsOverride()
		require.NotNil(t, got)
		assert.False(t, got.Audio)
		assert.False(t, got.Video)
	})
}

func TestConfigToolCallSupport(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"google": {Models: map[string]modelsdev.Model{
			"tool-model": {ToolCall: true},
		}},
	}})
	cfg := Config{
		ModelConfig:  latest.ModelConfig{Provider: "google", Model: "tool-model"},
		ModelOptions: options.Apply(options.WithModelsDevStore(store)),
	}

	assert.Equal(t, modelinfo.ToolCallSupported, cfg.ToolCallSupport(t.Context()))
}

func TestConfigImageOutputEnabled(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"google": {Models: map[string]modelsdev.Model{
			"image-model": {Modalities: modelsdev.Modalities{Output: []string{"text", "image"}}},
		}},
	}})

	tests := []struct {
		name               string
		outputCapabilities *latest.OutputCapabilitiesConfig
		want               bool
	}{
		{name: "catalogue fallback", want: true},
		{name: "explicit true", outputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)}, want: true},
		{name: "explicit false", outputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(false)}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := Config{
				ModelConfig: latest.ModelConfig{
					Provider:           "google",
					Model:              "image-model",
					OutputCapabilities: tt.outputCapabilities,
				},
				ModelOptions: options.Apply(options.WithModelsDevStore(store)),
			}
			assert.Equal(t, tt.want, cfg.ImageOutputEnabled(t.Context()))
		})
	}
}

func TestConfigNativeToolSearchEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  latest.ModelConfig
		want bool
	}{
		{name: "off by default", cfg: latest.ModelConfig{Provider: "openai", Model: "gpt-5.4"}},
		{name: "opted in on a verified model", cfg: latest.ModelConfig{Provider: "openai", Model: "gpt-5.4", ProviderOpts: map[string]any{"native_tool_search": true}}, want: true},
		{name: "explicit false", cfg: latest.ModelConfig{Provider: "openai", Model: "gpt-5.4", ProviderOpts: map[string]any{"native_tool_search": false}}},
		{name: "non-bool ignored", cfg: latest.ModelConfig{Provider: "openai", Model: "gpt-5.4", ProviderOpts: map[string]any{"native_tool_search": "true"}}},
		{name: "unsupported model", cfg: latest.ModelConfig{Provider: "openai", Model: "gpt-5.2", ProviderOpts: map[string]any{"native_tool_search": true}}},
		{name: "chatgpt backend excluded", cfg: latest.ModelConfig{Provider: "chatgpt", Model: "gpt-5.4", ProviderOpts: map[string]any{"native_tool_search": true}}},
		{name: "custom provider excluded", cfg: latest.ModelConfig{Provider: "proxy", Model: "gpt-5.4", ProviderOpts: map[string]any{"native_tool_search": true, "supports_deferred_tools": true}}},
		{name: "custom base_url excluded", cfg: latest.ModelConfig{Provider: "openai", Model: "gpt-5.4", BaseURL: "https://vllm.internal/v1", ProviderOpts: map[string]any{"native_tool_search": true}}},
		{name: "explicit chat completions excluded", cfg: latest.ModelConfig{Provider: "openai", Model: "gpt-5.4", ProviderOpts: map[string]any{"native_tool_search": true, "api_type": "openai_chatcompletions"}}},
		{name: "explicit responses allowed", cfg: latest.ModelConfig{Provider: "openai", Model: "gpt-5.4", ProviderOpts: map[string]any{"native_tool_search": true, "api_type": "openai_responses"}}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{ModelConfig: tt.cfg}
			assert.Equal(t, tt.want, cfg.NativeToolSearchEnabled())
		})
	}
}

// Only the user-supplied base_url excludes hosted tool search: the resolved
// endpoint may legitimately point at a local test server.
func TestConfigNativeToolSearchEnabled_IgnoresResolvedBaseURL(t *testing.T) {
	t.Parallel()

	cfg := Config{
		ModelConfig: latest.ModelConfig{Provider: "openai", Model: "gpt-5.4", ProviderOpts: map[string]any{"native_tool_search": true}},
		BaseURL:     "http://127.0.0.1:1234",
	}
	assert.True(t, cfg.NativeToolSearchEnabled())
}
