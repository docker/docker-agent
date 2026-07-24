package base

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelinfo"
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
