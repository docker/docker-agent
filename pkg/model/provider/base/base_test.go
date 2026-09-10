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
