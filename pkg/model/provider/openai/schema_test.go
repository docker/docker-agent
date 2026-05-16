package openai

import (
	"encoding/json"
	"testing"

	"github.com/openai/openai-go/v3/shared"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestMakeAllRequired(t *testing.T) {
	type DirectoryTreeArgs struct {
		Path     string `json:"path" jsonschema:"The directory path to traverse"`
		MaxDepth int    `json:"max_depth,omitempty" jsonschema:"Maximum depth to traverse (optional)"`
	}
	schema := tools.MustSchemaFor[DirectoryTreeArgs]()

	schemaMap, err := tools.SchemaToMap(schema)
	require.NoError(t, err)
	required := schemaMap["required"].([]any)
	assert.Len(t, required, 1)
	assert.Contains(t, required, "path")

	updatedSchema := makeAllRequired(schemaMap)
	required = updatedSchema["required"].([]any)
	assert.Len(t, required, 2)
	assert.Contains(t, required, "max_depth")
	assert.Contains(t, required, "path")
}

func TestMakeAllRequired_NoParameter(t *testing.T) {
	type NoArgs struct{}
	schema := tools.MustSchemaFor[NoArgs]()

	schemaMap, err := tools.SchemaToMap(schema)
	require.NoError(t, err)

	buf, err := json.Marshal(schemaMap)
	require.NoError(t, err)
	assert.JSONEq(t, `{"additionalProperties":false,"properties":{},"type":"object"}`, string(buf))

	updatedSchema := makeAllRequired(schemaMap)
	buf, err = json.Marshal(updatedSchema)
	require.NoError(t, err)
	assert.JSONEq(t, `{"additionalProperties":false,"properties":{},"type":"object","required":[]}`, string(buf))
}

func TestMakeAllRequired_NilSchema(t *testing.T) {
	updatedSchema := makeAllRequired(nil)
	buf, err := json.Marshal(updatedSchema)
	require.NoError(t, err)
	assert.JSONEq(t, `{"additionalProperties":false,"properties":{},"type":"object","required":[]}`, string(buf))
}

func TestMakeAllRequired_AnyOf(t *testing.T) {
	// Reproduces the chrome-devtools-mcp "emulate" tool schema where
	// viewport has an anyOf with an object variant whose properties
	// are not all listed in required. OpenAI rejects this.
	schema := shared.FunctionParameters{
		"type": "object",
		"properties": map[string]any{
			"viewport": map[string]any{
				"anyOf": []any{
					map[string]any{
						"type": "object",
						"properties": map[string]any{
							"width":             map[string]any{"type": "number"},
							"height":            map[string]any{"type": "number"},
							"deviceScaleFactor": map[string]any{"type": "number"},
						},
						"required": []any{"width", "height"},
					},
					map[string]any{
						"type": "null",
					},
				},
			},
		},
		"required": []any{"viewport"},
	}

	updated := makeAllRequired(schema)

	// Top-level: viewport must be required
	required := updated["required"].([]any)
	assert.Contains(t, required, "viewport")

	// anyOf[0]: all properties must be required, including deviceScaleFactor
	viewport := updated["properties"].(map[string]any)["viewport"].(map[string]any)
	anyOf := viewport["anyOf"].([]any)
	variant := anyOf[0].(map[string]any)
	variantRequired := variant["required"].([]any)
	assert.Len(t, variantRequired, 3)
	assert.Contains(t, variantRequired, "width")
	assert.Contains(t, variantRequired, "height")
	assert.Contains(t, variantRequired, "deviceScaleFactor")

	// deviceScaleFactor was not originally required, so its type should be nullable
	dsf := variant["properties"].(map[string]any)["deviceScaleFactor"].(map[string]any)
	assert.Equal(t, []string{"number", "null"}, dsf["type"])

	// width was originally required, so its type should be unchanged
	w := variant["properties"].(map[string]any)["width"].(map[string]any)
	assert.Equal(t, "number", w["type"])
}

func TestMakeAllRequired_NestedProperties(t *testing.T) {
	schema := shared.FunctionParameters{
		"type": "object",
		"properties": map[string]any{
			"config": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":  map[string]any{"type": "string"},
					"value": map[string]any{"type": "string"},
				},
				"required": []any{"name"},
			},
		},
		"required": []any{"config"},
	}

	updated := makeAllRequired(schema)

	// Nested object: all properties must be required
	config := updated["properties"].(map[string]any)["config"].(map[string]any)
	configRequired := config["required"].([]any)
	assert.Len(t, configRequired, 2)
	assert.Contains(t, configRequired, "name")
	assert.Contains(t, configRequired, "value")

	// value was not originally required, so its type should be nullable
	value := config["properties"].(map[string]any)["value"].(map[string]any)
	assert.Equal(t, []string{"string", "null"}, value["type"])
}

func TestMakeAllRequired_ArrayItems(t *testing.T) {
	schema := shared.FunctionParameters{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":   map[string]any{"type": "string"},
						"name": map[string]any{"type": "string"},
					},
					"required": []any{"id"},
				},
			},
		},
		"required": []any{"items"},
	}

	updated := makeAllRequired(schema)

	// Array items object: all properties must be required
	itemsSchema := updated["properties"].(map[string]any)["items"].(map[string]any)
	itemObj := itemsSchema["items"].(map[string]any)
	itemRequired := itemObj["required"].([]any)
	assert.Len(t, itemRequired, 2)
	assert.Contains(t, itemRequired, "id")
	assert.Contains(t, itemRequired, "name")
}

func TestMakeAllRequired_AdditionalProperties(t *testing.T) {
	// Reproduces the Notion MCP tool schema where additionalProperties
	// contains an object schema with its own properties (like bulleted_list_item).
	// OpenAI requires all properties in additionalProperties schemas to also
	// be listed in the required array.
	schema := shared.FunctionParameters{
		"type": "object",
		"properties": map[string]any{
			"children": map[string]any{
				"type": "object",
				"additionalProperties": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"bulleted_list_item": map[string]any{"type": "string"},
						"numbered_list_item": map[string]any{"type": "string"},
					},
					"required": []any{"bulleted_list_item"},
				},
			},
		},
		"required": []any{"children"},
	}

	updated := makeAllRequired(schema)

	// additionalProperties object: all properties must be required
	children := updated["properties"].(map[string]any)["children"].(map[string]any)
	additionalProps := children["additionalProperties"].(map[string]any)
	additionalRequired := additionalProps["required"].([]any)
	assert.Len(t, additionalRequired, 2)
	assert.Contains(t, additionalRequired, "bulleted_list_item")
	assert.Contains(t, additionalRequired, "numbered_list_item")

	// numbered_list_item was not originally required, so its type should be nullable
	numberedListItem := additionalProps["properties"].(map[string]any)["numbered_list_item"].(map[string]any)
	assert.Equal(t, []string{"string", "null"}, numberedListItem["type"])

	// bulleted_list_item was originally required, so its type should be unchanged
	bulletedListItem := additionalProps["properties"].(map[string]any)["bulleted_list_item"].(map[string]any)
	assert.Equal(t, "string", bulletedListItem["type"])
}

func TestRemoveFormatFields(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"format":      "uri",
				"description": "The URL",
			},
			"email": map[string]any{
				"type":   "string",
				"format": "email",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "The name",
			},
		},
	}

	updated := removeFormatFields(schema)

	url := updated["properties"].(map[string]any)["url"].(map[string]any)
	assert.Equal(t, "string", url["type"])
	assert.Equal(t, "The URL", url["description"])
	assert.NotContains(t, url, "format")

	email := updated["properties"].(map[string]any)["email"].(map[string]any)
	assert.Equal(t, "string", email["type"])
	assert.NotContains(t, email, "format")

	name := updated["properties"].(map[string]any)["name"].(map[string]any)
	assert.Equal(t, "string", name["type"])
	assert.Equal(t, "The name", name["description"])
}

func TestRemoveFormatFields_NestedObjects(t *testing.T) {
	schema := shared.FunctionParameters{
		"type": "object",
		"properties": map[string]any{
			"user": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"email": map[string]any{
						"type":   "string",
						"format": "email",
					},
					"website": map[string]any{
						"type":   "string",
						"format": "uri",
					},
				},
			},
			"name": map[string]any{
				"type":   "string",
				"format": "hostname",
			},
		},
	}

	updated := removeFormatFields(schema)

	user := updated["properties"].(map[string]any)["user"].(map[string]any)
	email := user["properties"].(map[string]any)["email"].(map[string]any)
	assert.NotContains(t, email, "format")
	assert.Equal(t, "string", email["type"])

	website := user["properties"].(map[string]any)["website"].(map[string]any)
	assert.NotContains(t, website, "format")

	name := updated["properties"].(map[string]any)["name"].(map[string]any)
	assert.NotContains(t, name, "format")
}

func TestRemoveFormatFields_ArrayItems(t *testing.T) {
	schema := shared.FunctionParameters{
		"type": "object",
		"properties": map[string]any{
			"urls": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":   "string",
					"format": "uri",
				},
			},
			"contacts": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"email": map[string]any{
							"type":   "string",
							"format": "email",
						},
					},
				},
			},
		},
	}

	updated := removeFormatFields(schema)

	urls := updated["properties"].(map[string]any)["urls"].(map[string]any)
	urlItems := urls["items"].(map[string]any)
	assert.NotContains(t, urlItems, "format")
	assert.Equal(t, "string", urlItems["type"])

	contacts := updated["properties"].(map[string]any)["contacts"].(map[string]any)
	contactItems := contacts["items"].(map[string]any)
	email := contactItems["properties"].(map[string]any)["email"].(map[string]any)
	assert.NotContains(t, email, "format")
	assert.Equal(t, "string", email["type"])
}

func TestRemoveFormatFields_NilSchema(t *testing.T) {
	assert.Nil(t, removeFormatFields(nil))
}

func TestRemoveFormatFields_NoProperties(t *testing.T) {
	schema := shared.FunctionParameters{"type": "object"}
	updated := removeFormatFields(schema)
	assert.Equal(t, schema, updated)
}

func TestMakeAllRequired_TypeArrayWithObject(t *testing.T) {
	// Reproduces the user_prompt tool schema where a property has
	// type: ["object", "null"] with nested properties. OpenAI requires
	// these nested properties to also have additionalProperties: false.
	schema := shared.FunctionParameters{
		"type": "object",
		"properties": map[string]any{
			"schema": map[string]any{
				"type": []string{"object", "null"},
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
					"age":  map[string]any{"type": "number"},
				},
				"required": []any{"name"},
			},
		},
		"required": []any{"schema"},
	}

	updated := makeAllRequired(schema)

	// Top-level should have additionalProperties: false
	assert.Equal(t, false, updated["additionalProperties"])

	// The schema property should also have additionalProperties: false
	schemaProps := updated["properties"].(map[string]any)["schema"].(map[string]any)
	assert.Equal(t, false, schemaProps["additionalProperties"])

	// All properties in schema should be required
	schemaRequired := schemaProps["required"].([]any)
	assert.Len(t, schemaRequired, 2)
	assert.Contains(t, schemaRequired, "name")
	assert.Contains(t, schemaRequired, "age")

	// age was not originally required, so its type should be nullable
	age := schemaProps["properties"].(map[string]any)["age"].(map[string]any)
	assert.Equal(t, []string{"number", "null"}, age["type"])
}

func TestEnsureTypeFields_AdditionalPropertiesMissingType(t *testing.T) {
	// Reproduces the Notion MCP notion-search tool schema where
	// filters.additionalProperties is an object schema without a "type" key.
	// OpenAI Responses API rejects schemas missing "type".
	schema := shared.FunctionParameters{
		"type": "object",
		"properties": map[string]any{
			"filters": map[string]any{
				"type": "object",
				"additionalProperties": map[string]any{
					// No "type" key here — this is the bug
					"properties": map[string]any{
						"property": map[string]any{"type": "string"},
					},
				},
			},
		},
		"required": []any{"filters"},
	}

	updated := ensureTypeFields(schema)

	filters := updated["properties"].(map[string]any)["filters"].(map[string]any)
	additionalProps := filters["additionalProperties"].(map[string]any)
	assert.Equal(t, "object", additionalProps["type"], "additionalProperties should have type added")
}

func TestConvertParametersToSchema_AdditionalPropertiesMissingType(t *testing.T) {
	// End-to-end test: the full pipeline should produce a valid schema
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"filters": map[string]any{
				"type": "object",
				"additionalProperties": map[string]any{
					"properties": map[string]any{
						"property": map[string]any{"type": "string"},
					},
				},
			},
		},
		"required": []any{"filters"},
	}

	result, err := ConvertParametersToSchema(schema)
	require.NoError(t, err)

	filters := result["properties"].(map[string]any)["filters"].(map[string]any)
	additionalProps := filters["additionalProperties"].(map[string]any)
	assert.Equal(t, "object", additionalProps["type"])
}

func TestFixSchemaArrayItems(t *testing.T) {
	schema := `{
  "properties": {
    "arguments": {
      "description": "Arguments to pass to the tool (can be any valid JSON value)",
      "type": [
        "string",
        "number",
        "boolean",
        "object",
        "array",
        "null"
      ]
    },
    "name": {
      "description": "Name of the tool to execute",
      "type": "string"
    }
  },
  "required": [
    "name"
  ],
  "type": "object"
}`

	schemaMap := map[string]any{}
	err := json.Unmarshal([]byte(schema), &schemaMap)
	require.NoError(t, err)

	updatedSchema := fixSchemaArrayItems(schemaMap)
	buf, err := json.Marshal(updatedSchema)
	require.NoError(t, err)

	assert.JSONEq(t, `{
  "properties": {
    "arguments": {
      "description": "Arguments to pass to the tool (can be any valid JSON value)",
      "type": [
        "string",
        "number",
        "boolean",
        "object",
        "array",
        "null"
      ]
    },
    "name": {
      "description": "Name of the tool to execute",
      "type": "string"
    }
  },
  "required": [
    "name"
  ],
  "type": "object"
}`, string(buf))
}
