package openai

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression: an optional property whose shape is expressed only through
// anyOf (Atlassian MCP getConfluenceSpaces.expand, Notion MCP filter values)
// must not gain a sibling "type" during normalization. "type" ANDs with
// anyOf, so {"anyOf":[string,array],"type":["object","null"]} is
// unsatisfiable; OpenAI's Responses API then answers with an instant
// status=incomplete / max_output_tokens and zero tokens instead of a 400.
func TestConvertParametersToSchema_AnyOfPropertyKeepsNoSiblingType(t *testing.T) {
	t.Parallel()

	src := `{
	  "type": "object",
	  "properties": {
	    "cloudId": {"type": "string"},
	    "expand": {
	      "anyOf": [{"type": "string"}, {"type": "array", "items": {"type": "string"}}],
	      "description": "Properties to expand"
	    }
	  },
	  "required": ["cloudId"]
	}`
	var params map[string]any
	require.NoError(t, json.Unmarshal([]byte(src), &params))

	out, _, err := ConvertParametersToSchema(params)
	require.NoError(t, err)

	props := out["properties"].(map[string]any)
	expand := props["expand"].(map[string]any)

	_, hasType := expand["type"]
	assert.False(t, hasType, "anyOf node must not get a sibling type: %v", expand)
	_, hasAddProps := expand["additionalProperties"]
	assert.False(t, hasAddProps, "anyOf node must not get additionalProperties: %v", expand)
	assert.Len(t, expand["anyOf"], 2)

	// The property still becomes required (all-required contract) — as an
	// optional anyOf it must be made nullable via an extra null variant or
	// left as-is, but never via a sibling type.
	assert.ElementsMatch(t, []any{"cloudId", "expand"}, out["required"])
}
