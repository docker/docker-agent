package rag

import (
	"cmp"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRAGTool_ToolName(t *testing.T) {
	tests := []struct {
		name         string
		toolName     string
		expectedName string
	}{
		{
			name:         "Uses custom tool name",
			toolName:     "custom_search",
			expectedName: "custom_search",
		},
		{
			name:         "Uses provided name",
			toolName:     "my_docs",
			expectedName: "my_docs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := &Tool{
				toolName: tt.toolName,
				manager:  nil,
			}

			tools, err := tool.Tools(t.Context())
			require.NoError(t, err)
			require.Len(t, tools, 1)
			assert.Equal(t, tt.expectedName, tools[0].Name)
			assert.Equal(t, "knowledge", tools[0].Category)
		})
	}
}

func TestRAGTool_DefaultDescription(t *testing.T) {
	tool := &Tool{
		toolName: "test_docs",
		manager:  nil,
	}

	tools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, tools, 1)
	assert.Contains(t, tools[0].Description, "test_docs")
}

func TestRAGTool_SortResults(t *testing.T) {
	results := []queryResult{
		{SourcePath: "a.txt", Similarity: 0.5},
		{SourcePath: "b.txt", Similarity: 0.9},
		{SourcePath: "c.txt", Similarity: 0.3},
		{SourcePath: "d.txt", Similarity: 0.7},
	}

	slices.SortFunc(results, func(a, b queryResult) int {
		return cmp.Compare(b.Similarity, a.Similarity)
	})

	assert.Equal(t, "b.txt", results[0].SourcePath)
	assert.Equal(t, "d.txt", results[1].SourcePath)
	assert.Equal(t, "a.txt", results[2].SourcePath)
	assert.Equal(t, "c.txt", results[3].SourcePath)
}
