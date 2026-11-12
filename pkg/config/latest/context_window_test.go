package latest

import (
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelConfigContextWindowYAMLRoundTrip(t *testing.T) {
	t.Parallel()

	const in = "provider: openai\nmodel: gpt-4o\ncontext_window: 64000\n"
	var f FlexibleModelConfig
	require.NoError(t, yaml.Unmarshal([]byte(in), &f))
	require.NotNil(t, f.ContextWindow)
	assert.Equal(t, int64(64000), *f.ContextWindow)
	assert.Empty(t, f.ProviderOpts, "context_window must stay out of provider_opts")
	assert.Equal(t, int64(64000), f.DeclaredContextWindow())
	assert.False(t, f.isShorthandOnly(), "context_window must defeat shorthand marshalling")

	out, err := yaml.Marshal(f)
	require.NoError(t, err)
	var rt FlexibleModelConfig
	require.NoError(t, yaml.Unmarshal(out, &rt))
	assert.Equal(t, f.ModelConfig, rt.ModelConfig)
}
