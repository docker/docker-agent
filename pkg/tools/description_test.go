package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddDescriptionParameter_AddsDescriptionParameter(t *testing.T) {
	t.Parallel()

	tools := []Tool{
		{
			Name:        "test_tool",
			Description: "A test tool",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "The file path",
					},
				},
				"required": []string{"path"},
			},
			AddDescriptionParameter: true,
		},
	}

	result := AddDescriptionParameter(tools)
	require.Len(t, result, 1)

	tool := result[0]
	schema, ok := tool.Parameters.(map[string]any)
	require.True(t, ok)

	properties, ok := schema["properties"].(map[string]any)
	require.True(t, ok)

	descProp, ok := properties[DescriptionParam].(map[string]any)
	require.True(t, ok, "description parameter should be added")
	assert.Equal(t, "string", descProp["type"])
	assert.Contains(t, descProp["description"], "description")
}

func TestAddDescriptionParameter_PreservesOriginalParameters(t *testing.T) {
	t.Parallel()

	tools := []Tool{
		{
			Name: "test_tool",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "The file path",
					},
				},
			},
			AddDescriptionParameter: true,
		},
	}

	result := AddDescriptionParameter(tools)

	schema := result[0].Parameters.(map[string]any)
	properties := schema["properties"].(map[string]any)

	pathProp, ok := properties["path"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "string", pathProp["type"])
	assert.Equal(t, "The file path", pathProp["description"])
}

func TestAddDescriptionParameter_SkipsToolsWithoutFlag(t *testing.T) {
	t.Parallel()

	tools := []Tool{
		{
			Name: "test_tool",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			AddDescriptionParameter: false,
		},
	}

	result := AddDescriptionParameter(tools)
	require.Len(t, result, 1)

	schema := result[0].Parameters.(map[string]any)
	properties := schema["properties"].(map[string]any)

	_, hasDesc := properties[DescriptionParam]
	assert.False(t, hasDesc, "description parameter should not be added")
}

func TestExtractDescription(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     string
		expected string
	}{
		{
			name:     "extracts description",
			args:     `{"path": "/tmp/file.txt", "description": "Reading config file"}`,
			expected: "Reading config file",
		},
		{
			name:     "returns empty for missing description",
			args:     `{"path": "/tmp/file.txt"}`,
			expected: "",
		},
		{
			name:     "returns empty for invalid JSON",
			args:     `invalid json`,
			expected: "",
		},
		{
			name:     "returns empty for non-string description",
			args:     `{"description": 123}`,
			expected: "",
		},
		{
			name:     "handles empty args",
			args:     "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := ExtractDescription(tt.args)
			assert.Equal(t, tt.expected, result)
		})
	}
}
