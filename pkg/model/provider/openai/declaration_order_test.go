package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestOptionalAndNestedArgumentsKeepDeclarationOrder(t *testing.T) {
	t.Parallel()
	type child struct {
		Zulu  string `json:"zulu,omitempty"`
		Alpha string `json:"alpha"`
	}
	type args struct {
		Zulu  string  `json:"zulu,omitempty"`
		Alpha []child `json:"alpha"`
	}
	schema, strict, err := ConvertParametersToSchema(tools.MustSchemaFor[args]())
	require.NoError(t, err)
	require.True(t, strict)
	assert.Equal(t, []any{"zulu", "alpha"}, schema["required"])
	nested := schema["properties"].(map[string]any)["alpha"].(map[string]any)["items"].(map[string]any)
	assert.Equal(t, []any{"zulu", "alpha"}, nested["required"])
	tools.OrderedSchemaProperties(schema)
	data, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(data), `"properties":{"zulu":`))
	assert.NotContains(t, string(data), "x-docker-agent-property-order")
}
