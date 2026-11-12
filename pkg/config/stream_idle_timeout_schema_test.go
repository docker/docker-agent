package config

import (
	"fmt"
	"os"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xeipuuv/gojsonschema"
)

func TestStreamIdleTimeoutSchema(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(schemaFile)
	require.NoError(t, err)
	schema, err := gojsonschema.NewSchema(gojsonschema.NewBytesLoader(data))
	require.NoError(t, err)
	for _, field := range []string{"runtime:\n  stream_idle_timeout: %s\n", "models:\n  x:\n    provider: openai\n    model: gpt\n    stream_idle_timeout: %s\n"} {
		for _, tc := range []struct {
			value string
			valid bool
		}{
			{"null", true}, {"1m30s", true}, {"-1", true}, {"0", false}, {"-2", false}, {"0s", false}, {"-1s", false}, {"bad", false}, {"1.5", false}, {"true", false}, {"[]", false}, {"{}", false},
		} {
			doc := fmt.Sprintf(field, tc.value) + "agents:\n  root:\n    model: openai/gpt\n    instruction: test\n"
			encoded, err := yaml.YAMLToJSON([]byte(doc))
			require.NoError(t, err)
			result, err := schema.Validate(gojsonschema.NewBytesLoader(encoded))
			require.NoError(t, err)
			assert.Equal(t, tc.valid, result.Valid(), "%s %s: %v", field, tc.value, result.Errors())
		}
	}
}
