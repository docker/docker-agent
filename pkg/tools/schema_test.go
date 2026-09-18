package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSchemaToMap_Nil(t *testing.T) {
	t.Parallel()
	m, err := SchemaToMap(nil)
	require.NoError(t, err)

	assert.Equal(t, map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}, m)
}

func TestSchemaToMap_MissingType(t *testing.T) {
	t.Parallel()
	m, err := SchemaToMap(map[string]any{
		"properties": map[string]any{},
	})
	require.NoError(t, err)

	assert.Equal(t, map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}, m)
}

func TestSchemaToMap_MissingEmptyProperties(t *testing.T) {
	t.Parallel()
	m, err := SchemaToMap(map[string]any{
		"type": "object",
	})
	require.NoError(t, err)

	assert.Equal(t, map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}, m)
}

func TestSchemaToMap_PropertyWithoutType(t *testing.T) {
	t.Parallel()
	m, err := SchemaToMap(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type": "string",
			},
			"metadata": map[string]any{
				"description": "some metadata",
			},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type": "string",
			},
			"metadata": map[string]any{
				"type":        "object",
				"description": "some metadata",
			},
		},
	}, m)
}

func TestSchemaToMap_NestedPropertyWithoutType(t *testing.T) {
	t.Parallel()
	m, err := SchemaToMap(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"config": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"host": map[string]any{
						"type": "string",
					},
					"metadata": map[string]any{
						"description": "nested metadata without type",
					},
				},
			},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"config": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"host": map[string]any{
						"type": "string",
					},
					"metadata": map[string]any{
						"type":        "object",
						"description": "nested metadata without type",
					},
				},
			},
		},
	}, m)
}

func TestSchemaToMap_ArrayItemsPropertyWithoutType(t *testing.T) {
	t.Parallel()
	m, err := SchemaToMap(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"value": map[string]any{
							"description": "value without type",
						},
					},
				},
			},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"value": map[string]any{
							"type":        "object",
							"description": "value without type",
						},
					},
				},
			},
		},
	}, m)
}

func TestSchemaToMap_DeeplyNestedPropertyWithoutType(t *testing.T) {
	t.Parallel()
	m, err := SchemaToMap(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"level1": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"level2": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"level3": map[string]any{
								"description": "deeply nested without type",
							},
						},
					},
				},
			},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"level1": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"level2": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"level3": map[string]any{
								"type":        "object",
								"description": "deeply nested without type",
							},
						},
					},
				},
			},
		},
	}, m)
}

func TestSchemaToMap_StripsNullFromRequiredArrayTypes(t *testing.T) {
	t.Parallel()
	m, err := SchemaToMap(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"paths": map[string]any{
				"type":  []any{"null", "array"},
				"items": map[string]any{"type": "string"},
			},
			"excludePatterns": map[string]any{
				"type":  []any{"null", "array"},
				"items": map[string]any{"type": "string"},
			},
		},
		"required": []any{"paths"},
	})
	require.NoError(t, err)

	assert.Equal(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"paths": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
			"excludePatterns": map[string]any{
				"type":  []any{"null", "array"},
				"items": map[string]any{"type": "string"},
			},
		},
		"required": []any{"paths"},
	}, m)
}

// T14: ensurePropertyTypes must not inject a "type" key into a $ref node.
// Doing so pollutes the node with a sibling keyword, which providers like
// OpenAI reject on $ref nodes under strict mode (see docker/docker-agent#4106).
func TestSchemaToMap_RefPropertyLeftAsLeaf(t *testing.T) {
	t.Parallel()
	m, err := SchemaToMap(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"p": map[string]any{"$ref": "#/$defs/thing"},
		},
		"$defs": map[string]any{
			"thing": map[string]any{"type": "string"},
		},
	})
	require.NoError(t, err)

	p := m["properties"].(map[string]any)["p"].(map[string]any)
	assert.Equal(t, map[string]any{"$ref": "#/$defs/thing"}, p, "$ref node must be left as a leaf, no injected type")
}

func TestSchemaToMap_RefPropertyInNestedObjectLeftAsLeaf(t *testing.T) {
	t.Parallel()
	m, err := SchemaToMap(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"config": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"p": map[string]any{"$ref": "#/$defs/thing"},
				},
			},
		},
		"$defs": map[string]any{
			"thing": map[string]any{"type": "string"},
		},
	})
	require.NoError(t, err)

	config := m["properties"].(map[string]any)["config"].(map[string]any)
	p := config["properties"].(map[string]any)["p"].(map[string]any)
	assert.Equal(t, map[string]any{"$ref": "#/$defs/thing"}, p, "$ref node must be left as a leaf, no injected type")
}
