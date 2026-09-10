package v15

import (
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelConfigCapabilitiesYAMLRoundTrip(t *testing.T) {
	t.Parallel()

	const in = `provider: ollama
model: llava
capabilities:
  image: true
  pdf: false
`
	var f FlexibleModelConfig
	require.NoError(t, yaml.Unmarshal([]byte(in), &f))

	require.NotNil(t, f.Capabilities, "capabilities should be parsed")
	assert.True(t, f.Capabilities.Image)
	assert.False(t, f.Capabilities.PDF)

	// A model carrying a capabilities override must not collapse to the
	// "provider/model" shorthand on marshal, or the override would be lost.
	assert.False(t, f.isShorthandOnly(), "capabilities override must defeat shorthand marshalling")

	out, err := yaml.Marshal(f)
	require.NoError(t, err)

	var rt FlexibleModelConfig
	require.NoError(t, yaml.Unmarshal(out, &rt))
	require.NotNil(t, rt.Capabilities, "capabilities should survive a marshal round-trip; got:\n%s", out)
	assert.True(t, rt.Capabilities.Image)
	assert.False(t, rt.Capabilities.PDF)
}

func TestModelConfigShorthandOnlyWithoutCapabilities(t *testing.T) {
	t.Parallel()

	const in = `provider: openai
model: gpt-4o
`
	var f FlexibleModelConfig
	require.NoError(t, yaml.Unmarshal([]byte(in), &f))

	assert.Nil(t, f.Capabilities)
	assert.True(t, f.isShorthandOnly(), "a bare provider/model must still marshal as shorthand")
}

func TestModelConfigBypassModelsGatewayYAMLRoundTrip(t *testing.T) {
	t.Parallel()

	const in = `provider: anthropic
model: claude-sonnet-4-5
bypass_models_gateway: true
`
	var f FlexibleModelConfig
	require.NoError(t, yaml.Unmarshal([]byte(in), &f))
	assert.True(t, f.BypassModelsGateway, "bypass_models_gateway should be parsed")

	// A model carrying only the bypass flag (plus provider/model) must not
	// collapse to the shorthand on marshal, or the flag would be lost.
	assert.False(t, f.isShorthandOnly(), "bypass_models_gateway must defeat shorthand marshalling")

	out, err := yaml.Marshal(f)
	require.NoError(t, err)

	var rt FlexibleModelConfig
	require.NoError(t, yaml.Unmarshal(out, &rt))
	assert.True(t, rt.BypassModelsGateway, "bypass_models_gateway should survive a marshal round-trip; got:\n%s", out)
}

func TestModelConfigCloneCopiesCapabilities(t *testing.T) {
	t.Parallel()

	orig := &ModelConfig{
		Provider:     "my-proxy",
		Model:        "gpt-4o",
		Capabilities: &CapabilitiesConfig{Image: true, PDF: true},
	}

	clone := orig.Clone()
	require.NotNil(t, clone.Capabilities)
	assert.True(t, clone.Capabilities.Image)
	assert.True(t, clone.Capabilities.PDF)

	// Mutating the clone must not affect the original (deep copy).
	clone.Capabilities.Image = false
	assert.True(t, orig.Capabilities.Image, "clone must not share the Capabilities pointer with the original")
}
