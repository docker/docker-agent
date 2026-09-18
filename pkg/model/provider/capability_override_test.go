package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/modelinfo"
)

// TestCapabilityOverride_SurvivesProviderConstruction is the end-to-end check
// for issue #2741: a `capabilities` override declared on a model config must
// survive the full construction path (Registry.New → applyProviderDefaults →
// factory → base.Config) and be exposed to the conversion layer via
// base.Config.CapsOverride(). Without this, the override would parse correctly
// yet silently never reach the attachment-capability decision.
func TestCapabilityOverride_SurvivesProviderConstruction(t *testing.T) {
	t.Parallel()

	env := environment.NewMapEnvProvider(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})

	t.Run("override flows through to the built provider", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{
			Provider:     "openai",
			Model:        "gpt-4o",
			BaseURL:      "https://llm.internal.example.com/v1",
			Capabilities: &latest.CapabilitiesConfig{Image: true, PDF: false},
		}

		p, err := fullTestRegistry().New(t.Context(), cfg, env)
		require.NoError(t, err)

		bc := p.BaseConfig()
		got := bc.CapsOverride()
		require.NotNil(t, got, "capabilities override must survive provider construction (issue #2741)")
		assert.Equal(t, &modelinfo.CapsOverride{Image: true, PDF: false}, got)
	})

	t.Run("no override declared yields nil (auto-detection path)", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{
			Provider: "openai",
			Model:    "gpt-4o",
			BaseURL:  "https://llm.internal.example.com/v1",
		}

		p, err := fullTestRegistry().New(t.Context(), cfg, env)
		require.NoError(t, err)
		bc := p.BaseConfig()
		assert.Nil(t, bc.CapsOverride())
	})

	t.Run("audio/video override flows through to the built provider", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.ModelConfig{
			Provider:     "openai",
			Model:        "gpt-4o",
			BaseURL:      "https://llm.internal.example.com/v1",
			Capabilities: &latest.CapabilitiesConfig{Image: true, PDF: true, Audio: true, Video: true},
		}

		p, err := fullTestRegistry().New(t.Context(), cfg, env)
		require.NoError(t, err)

		bc := p.BaseConfig()
		got := bc.CapsOverride()
		require.NotNil(t, got, "audio/video override must survive provider construction")
		assert.Equal(t, &modelinfo.CapsOverride{Image: true, PDF: true, Audio: true, Video: true}, got)
	})
}
