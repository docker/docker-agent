package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSchemaToMap_Nil(t *testing.T) {
	m, err := SchemaToMap(nil)
	require.NoError(t, err)

	assert.Equal(t, map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}, m)
}

func TestSchemaToMap_MissingType(t *testing.T) {
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
