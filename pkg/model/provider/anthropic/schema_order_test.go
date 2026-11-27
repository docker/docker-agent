package anthropic

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
)

func TestWriteFileWireSchemaPathFirst(t *testing.T) {
	t.Parallel()
	schema, err := ConvertParametersToSchema(tools.MustSchemaFor[filesystem.WriteFileArgs]())
	require.NoError(t, err)
	wire, err := json.Marshal(ToolParam{Name: "write_file", InputSchema: schema})
	require.NoError(t, err)
	assert.Less(t, bytes.Index(wire, []byte(`"path":`)), bytes.Index(wire, []byte(`"content":`)))
}

func TestBetaToolsRetainNestedDeclarationOrder(t *testing.T) {
	t.Parallel()
	type child struct {
		Zulu  string `json:"zulu,omitempty"`
		Alpha string `json:"alpha"`
	}
	type args struct {
		Zulu  string  `json:"zulu,omitempty"`
		Alpha []child `json:"alpha"`
	}
	converted, err := convertTools([]tools.Tool{{Name: "ordered", Parameters: tools.MustSchemaFor[args]()}})
	require.NoError(t, err)
	data, err := json.Marshal(converted)
	require.NoError(t, err)
	assert.Equal(t, 2, bytes.Count(data, []byte(`"properties":{"zulu":`)))
	assert.NotContains(t, string(data), "x-docker-agent-property-order")
}

func TestConvertTools_PreservesRootDefsNonStrict(t *testing.T) {
	t.Parallel()

	params := map[string]any{
		"type": "object",
		"$defs": map[string]any{
			"json_value": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string"},
					map[string]any{"type": "integer"},
					map[string]any{
						"type":                 "object",
						"additionalProperties": map[string]any{"$ref": "#/$defs/json_value"},
					},
					map[string]any{
						"type":  "array",
						"items": map[string]any{"$ref": "#/$defs/json_value"},
					},
				},
			},
		},
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
			"arguments": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"$ref": "#/$defs/json_value"},
			},
		},
		"required": []string{"name"},
	}

	reqTools := []tools.Tool{{Name: "mcp_events_subscribe", Parameters: params}}

	t.Run("standard", func(t *testing.T) {
		t.Parallel()
		converted, err := convertTools(reqTools)
		require.NoError(t, err)
		wire, err := json.Marshal(converted)
		require.NoError(t, err)

		var decoded []map[string]any
		require.NoError(t, json.Unmarshal(wire, &decoded))
		require.Len(t, decoded, 1)
		inputSchema := decoded[0]["input_schema"].(map[string]any)
		defs, ok := inputSchema["$defs"].(map[string]any)
		require.True(t, ok, "standard wire schema must preserve root $defs")
		assert.Contains(t, defs, "json_value")
	})

	t.Run("beta", func(t *testing.T) {
		t.Parallel()
		converted, err := convertTools(reqTools)
		require.NoError(t, err)
		wire, err := json.Marshal(converted)
		require.NoError(t, err)

		var decoded []map[string]any
		require.NoError(t, json.Unmarshal(wire, &decoded))
		require.Len(t, decoded, 1)
		inputSchema := decoded[0]["input_schema"].(map[string]any)
		defs, ok := inputSchema["$defs"].(map[string]any)
		require.True(t, ok, "beta wire schema must preserve root $defs")
		assert.Contains(t, defs, "json_value")
	})
}
