package latest

import (
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestModelConfigOutputCapabilitiesYAMLRoundTrip pins that an explicit
// output_capabilities.image declaration survives parse and re-marshal, and
// defeats the provider/model shorthand collapse the same way capabilities does.
func TestModelConfigOutputCapabilitiesYAMLRoundTrip(t *testing.T) {
	t.Parallel()

	const in = `provider: google
model: gemini-2.5-flash-image
output_capabilities:
  image: true
`
	var f FlexibleModelConfig
	require.NoError(t, yaml.Unmarshal([]byte(in), &f))

	require.NotNil(t, f.OutputCapabilities, "output_capabilities should be parsed")
	require.NotNil(t, f.OutputCapabilities.Image)
	assert.True(t, *f.OutputCapabilities.Image)

	assert.False(t, f.isShorthandOnly(), "output_capabilities override must defeat shorthand marshalling")

	out, err := yaml.Marshal(f)
	require.NoError(t, err)

	var rt FlexibleModelConfig
	require.NoError(t, yaml.Unmarshal(out, &rt))
	require.NotNil(t, rt.OutputCapabilities, "output_capabilities should survive a marshal round-trip; got:\n%s", out)
	require.NotNil(t, rt.OutputCapabilities.Image)
	assert.True(t, *rt.OutputCapabilities.Image)
}

// TestModelConfigOutputCapabilitiesFalseYAMLRoundTrip pins that an explicit
// `image: false` is distinguishable from an omitted block: OutputCapabilities
// itself is non-nil (the owner declared the model, and declared it
// image-output-incapable), even though the Image flag is false.
func TestModelConfigOutputCapabilitiesFalseYAMLRoundTrip(t *testing.T) {
	t.Parallel()

	const in = `provider: google
model: gemini-2.5-flash
output_capabilities:
  image: false
`
	var f FlexibleModelConfig
	require.NoError(t, yaml.Unmarshal([]byte(in), &f))

	require.NotNil(t, f.OutputCapabilities, "an explicit false block should still be parsed as present")
	require.NotNil(t, f.OutputCapabilities.Image)
	assert.False(t, *f.OutputCapabilities.Image)
}

// TestModelConfigShorthandOnlyWithoutOutputCapabilities pins that a bare
// provider/model with no output_capabilities block still collapses to the
// shorthand form on marshal.
func TestModelConfigShorthandOnlyWithoutOutputCapabilities(t *testing.T) {
	t.Parallel()

	const in = `provider: openai
model: gpt-4o
`
	var f FlexibleModelConfig
	require.NoError(t, yaml.Unmarshal([]byte(in), &f))

	assert.Nil(t, f.OutputCapabilities)
	assert.True(t, f.isShorthandOnly(), "a bare provider/model must still marshal as shorthand")
}

// TestModelConfigOutputCapabilitiesOmittedStaysNil pins the default,
// missing-declaration case: OutputCapabilities stays nil, distinct from an
// explicit false block.
func TestModelConfigOutputCapabilitiesOmittedStaysNil(t *testing.T) {
	t.Parallel()

	const in = `provider: google
model: gemini-2.5-flash
`
	var f FlexibleModelConfig
	require.NoError(t, yaml.Unmarshal([]byte(in), &f))

	assert.Nil(t, f.OutputCapabilities)
}

func TestModelConfigCloneCopiesOutputCapabilities(t *testing.T) {
	t.Parallel()

	orig := &ModelConfig{
		Provider:           "google",
		Model:              "gemini-2.5-flash-image",
		OutputCapabilities: &OutputCapabilitiesConfig{Image: new(true)},
	}

	clone := orig.Clone()
	require.NotNil(t, clone.OutputCapabilities)
	require.NotNil(t, clone.OutputCapabilities.Image)
	assert.True(t, *clone.OutputCapabilities.Image)

	// Mutating the clone must not affect the original (deep copy).
	*clone.OutputCapabilities.Image = false
	assert.True(t, *orig.OutputCapabilities.Image, "clone must not share the OutputCapabilities pointer with the original")
}

// TestModelConfigCloneNilOutputCapabilities pins that a model with no
// declaration clones to nil, not a zero-value struct — the "unknown" state
// must not be silently upgraded to an authoritative false on clone.
func TestModelConfigCloneNilOutputCapabilities(t *testing.T) {
	t.Parallel()

	orig := &ModelConfig{Provider: "google", Model: "gemini-2.5-flash"}

	clone := orig.Clone()
	assert.Nil(t, clone.OutputCapabilities)
}
