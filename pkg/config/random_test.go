package config

import (
	"os"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xeipuuv/gojsonschema"
)

func TestRandomToolsetSchema(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(schemaFile)
	require.NoError(t, err)
	schema, err := gojsonschema.NewSchema(gojsonschema.NewBytesLoader(data))
	require.NoError(t, err)

	for name, input := range map[string]string{
		"inline": `
agents:
  root:
    model: openai/gpt-5-mini
    toolsets:
      - type: random
`,
		"reusable": `
toolsets:
  dice:
    type: random
agents:
  root:
    model: openai/gpt-5-mini
    use_toolsets: [dice]
`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var raw any
			require.NoError(t, yaml.Unmarshal([]byte(input), &raw))
			result, err := schema.Validate(gojsonschema.NewRawLoader(raw))
			require.NoError(t, err)
			assert.True(t, result.Valid(), "%v", result.Errors())
		})
	}
}
