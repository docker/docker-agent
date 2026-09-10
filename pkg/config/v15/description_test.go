package v15

import (
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelConfigDescriptionYAMLRoundTrip(t *testing.T) {
	t.Parallel()

	const in = `provider: anthropic
model: claude-haiku-4-5
description: Fast and cheap; good for summaries.
`
	var f FlexibleModelConfig
	require.NoError(t, yaml.Unmarshal([]byte(in), &f))
	assert.Equal(t, "Fast and cheap; good for summaries.", f.Description)

	// A model carrying a description must not collapse to the
	// "provider/model" shorthand on marshal, or the description would be lost.
	assert.False(t, f.isShorthandOnly(), "description must defeat shorthand marshalling")

	out, err := yaml.Marshal(f)
	require.NoError(t, err)

	var rt FlexibleModelConfig
	require.NoError(t, yaml.Unmarshal(out, &rt))
	assert.Equal(t, "Fast and cheap; good for summaries.", rt.Description, "description should survive a marshal round-trip; got:\n%s", out)
}
