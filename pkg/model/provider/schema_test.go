package provider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/model/provider/anthropic"
	"github.com/docker/docker-agent/pkg/model/provider/dmr"
	"github.com/docker/docker-agent/pkg/model/provider/gemini"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
)

const schemaJSON = `
{
    "type": "object",
    "properties": {
      "direction": {
        "description": "Order",
        "enum": [
          "ASC",
          "DESC"
        ],
        "type": "string"
      },
      "labels": {
        "description": "Filter",
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "perPage": {
        "description": "Results",
        "maximum": 100,
        "minimum": 1,
        "type": "number"
      },
      "repo": {
        "description": "Repository",
        "type": "string"
      }
    },
	"additionalProperties": false,
    "required": ["repo"]
}`

func parseFunctionParameters(t *testing.T, schemaJSON string) map[string]any {
	t.Helper()

	var parameters map[string]any
	err := json.Unmarshal([]byte(schemaJSON), &parameters)
	require.NoError(t, err)

	return parameters
}

func TestEmptyMapSchemaForGemini(t *testing.T) {
	schema, err := gemini.ConvertParametersToSchema(map[string]any{})
	require.NoError(t, err)

	schemaJSON, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type": "object"}`, string(schemaJSON))
}

func TestEmptySchemaForGemini(t *testing.T) {
	parameters := parseFunctionParameters(t, "{}")

	schema, err := gemini.ConvertParametersToSchema(parameters)
	require.NoError(t, err)

	schemaJSON, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type": "object"}`, string(schemaJSON))
}

func TestNilSchemaForGemini(t *testing.T) {
	schema, err := gemini.ConvertParametersToSchema(nil)
	require.NoError(t, err)

	schemaJSON, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type": "object"}`, string(schemaJSON))
}

func TestSchemaForGemini(t *testing.T) {
	parameters := parseFunctionParameters(t, schemaJSON)

	schema, err := gemini.ConvertParametersToSchema(parameters)
	require.NoError(t, err)

	schemaJSON, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.JSONEq(t, `
{
    "type": "object",
    "properties": {
      "direction": {
        "description": "Order",
        "enum": [
          "ASC",
          "DESC"
        ],
        "type": "string"
      },
      "labels": {
        "description": "Filter",
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "perPage": {
        "description": "Results",
        "maximum": 100,
        "minimum": 1,
        "type": "number"
      },
      "repo": {
        "description": "Repository",
        "type": "string"
      }
    },
    "required": ["repo"]
}`, string(schemaJSON))
}

func TestEmptyMapSchemaForAnthropic(t *testing.T) {
	shema, err := anthropic.ConvertParametersToSchema(map[string]any{})
	require.NoError(t, err)

	schemaJSON, err := json.Marshal(shema)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type": "object", "properties": {}}`, string(schemaJSON))
}

func TestNilSchemaForAnthropic(t *testing.T) {
	shema, err := anthropic.ConvertParametersToSchema(nil)
	require.NoError(t, err)

	schemaJSON, err := json.Marshal(shema)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type": "object", "properties": {}}`, string(schemaJSON))
}

func TestSchemaForAnthropic(t *testing.T) {
	parameters := parseFunctionParameters(t, schemaJSON)
	shema, err := anthropic.ConvertParametersToSchema(parameters)
	require.NoError(t, err)

	schemaJSON, err := json.Marshal(shema)
	require.NoError(t, err)
	assert.JSONEq(t, `
{
	"type": "object",
	"properties": {
		"direction": {
			"description": "Order",
			"enum": ["ASC", "DESC"],
			"type": "string"
		},
		"labels": {
			"description": "Filter",
			"items": {
				"type": "string"
			},
			"type": "array"
		},
		"perPage": {
			"description": "Results",
			"maximum": 100,
			"minimum": 1,
			"type": "number"
		},
		"repo": {
			"description": "Repository",
			"type": "string"
		}
	},
	"required": ["repo"]
}`, string(schemaJSON))
}

// TestEmptyMapSchemaForOpenai makes sure we format empty properties in a way that
// OpenAI and LM Studio accept.
// See https://github.com/docker/docker-agent/issues/278
func TestEmptyMapSchemaForOpenai(t *testing.T) {
	schema, err := openai.ConvertParametersToSchema(map[string]any{})
	require.NoError(t, err)

	schemaJSON, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type": "object", "properties": {}, "required": [], "additionalProperties": false}`, string(schemaJSON))
}

func TestNilSchemaForOpenai(t *testing.T) {
	schema, err := openai.ConvertParametersToSchema(nil)
	require.NoError(t, err)

	schemaJSON, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type": "object", "properties": {}, "required": [], "additionalProperties": false}`, string(schemaJSON))
}

func TestSchemaForOpenai(t *testing.T) {
	parameters := parseFunctionParameters(t, schemaJSON)

	schema, err := openai.ConvertParametersToSchema(parameters)
	require.NoError(t, err)

	schemaJSON, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.JSONEq(t, `
{
	"type": "object",
	"properties": {
		"direction": {
			"description": "Order",
			"enum": ["ASC", "DESC"],
			"type": "string"
		},
		"labels": {
			"description": "Filter",
			"items": {
				"type": "string"
			},
			"type": "array"
		},
		"perPage": {
			"description": "Results",
			"maximum": 100,
			"minimum": 1,
			"type": "number"
		},
		"repo": {
			"description": "Repository",
			"type": "string"
		}
	},
	"additionalProperties": false,
	"required": ["direction", "labels", "perPage", "repo"]
}`, string(schemaJSON))
}

func TestEmptyMapSchemaForDMR(t *testing.T) {
	schema, err := dmr.ConvertParametersToSchema(map[string]any{})
	require.NoError(t, err)

	schemaJSON, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type": "object", "properties": {}}`, string(schemaJSON))
}

func TestNilSchemaForDMR(t *testing.T) {
	schema, err := dmr.ConvertParametersToSchema(nil)
	require.NoError(t, err)

	schemaJSON, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type": "object", "properties": {}}`, string(schemaJSON))
}

func TestSchemaForDMR(t *testing.T) {
	parameters := parseFunctionParameters(t, schemaJSON)

	schema, err := dmr.ConvertParametersToSchema(parameters)
	require.NoError(t, err)

	schemaJSON, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.JSONEq(t, `
{
	"type": "object",
	"properties": {
		"direction": {
			"description": "Order",
			"enum": ["ASC", "DESC"],
			"type": "string"
		},
		"labels": {
			"description": "Filter",
			"items": {
				"type": "string"
			},
			"type": "array"
		},
		"perPage": {
			"description": "Results",
			"maximum": 100,
			"minimum": 1,
			"type": "number"
		},
		"repo": {
			"description": "Repository",
			"type": "string"
		}
	},
	"required": ["repo"]
}`, string(schemaJSON))
}
