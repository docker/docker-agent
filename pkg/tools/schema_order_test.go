package tools

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOrderedSchemaProperties(t *testing.T) {
	t.Parallel()
	schema := map[string]any{"properties": map[string]any{"content": map[string]any{"type": "string"}, "description": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"}}, schemaPropertyOrder: []string{"path", "content"}, "required": []any{"content", "description", "path"}}
	before, err := json.Marshal(schema)
	require.NoError(t, err)
	OrderedSchemaProperties(schema)
	after, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.Less(t, bytes.Index(after, []byte(`"path":`)), bytes.Index(after, []byte(`"content":`)))
	var original, ordered any
	require.NoError(t, json.Unmarshal(before, &original))
	require.NoError(t, json.Unmarshal(after, &ordered))
	delete(original.(map[string]any), schemaPropertyOrder)
	assert.Equal(t, original, ordered)
}

type orderEmbedded struct {
	Second string `json:"second,omitempty"`
}

type orderedTestArgs struct {
	orderEmbedded

	Zulu    string `json:"zulu"`
	Ignored string `json:"-"`
	Alpha   []struct {
		Zulu  string `json:"zulu"`
		Alpha string `json:"alpha,omitempty"`
	} `json:"alpha"`
}

func TestSchemaDeclarationOrderSurvivesToolRoundTrip(t *testing.T) {
	t.Parallel()
	schema := MustSchemaFor[orderedTestArgs]()
	tool := AddDescriptionParameter([]Tool{{Name: "ordered", Parameters: schema, AddDescriptionParameter: true}})[0]
	for range 2 {
		data, err := json.Marshal(tool)
		require.NoError(t, err)
		assert.NotContains(t, string(data), schemaPropertyOrder)
		assert.Less(t, bytes.Index(data, []byte(`"second":`)), bytes.Index(data, []byte(`"zulu":`)))
		assert.Less(t, bytes.Index(data, []byte(`"second":`)), bytes.Index(data, []byte(`"alpha":`)))
		var decoded Tool
		require.NoError(t, json.Unmarshal(data, &decoded))
		m, err := SchemaToMap(decoded.Parameters)
		require.NoError(t, err)
		assert.Equal(t, []string{"second", "zulu", "alpha", "description"}, SchemaPropertyNames(m))
		child := m["properties"].(map[string]any)["alpha"].(map[string]any)["items"].(map[string]any)
		assert.Equal(t, []string{"zulu", "alpha"}, SchemaPropertyNames(child))
		tool = decoded
	}
}
