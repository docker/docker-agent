package config

import (
	"os"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xeipuuv/gojsonschema"
)

func TestDatetimeToolsetConfig(t *testing.T) {
	t.Parallel()

	schemaBytes, err := os.ReadFile(schemaFile)
	require.NoError(t, err)
	schema, err := gojsonschema.NewSchema(gojsonschema.NewBytesLoader(schemaBytes))
	require.NoError(t, err)

	configs := map[string]string{
		"inline": `agents:
  root:
    model: openai/gpt-5-mini
    toolsets:
      - type: datetime
`,
		"named": `toolsets:
  clock:
    type: datetime
agents:
  root:
    model: openai/gpt-5-mini
    use_toolsets: [clock]
`,
	}
	for name, data := range configs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var raw any
			require.NoError(t, yaml.Unmarshal([]byte(data), &raw))
			result, err := schema.Validate(gojsonschema.NewRawLoader(raw))
			require.NoError(t, err)
			assert.True(t, result.Valid(), "%v", result.Errors())

			cfg, err := Load(t.Context(), NewBytesSource("datetime.yaml", []byte(data)))
			require.NoError(t, err)
			require.Len(t, cfg.Agents, 1)
			require.Len(t, cfg.Agents[0].Toolsets, 1)
			assert.Equal(t, "datetime", cfg.Agents[0].Toolsets[0].Type)
		})
	}
}
