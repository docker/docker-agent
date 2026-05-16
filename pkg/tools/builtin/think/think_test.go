package think

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestThinkTool_Handler(t *testing.T) {
	tool := NewThinkTool()

	result, err := tool.callTool(t.Context(), Args{Thought: "This is a test thought"})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "This is a test thought")

	result, err = tool.callTool(t.Context(), Args{Thought: "Another thought"})
	require.NoError(t, err)

	assert.Contains(t, result.Output, "This is a test thought")
	assert.Contains(t, result.Output, "Another thought")
}

func TestThinkTool_OutputSchema(t *testing.T) {
	tool := NewThinkTool()

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, allTools)

	for _, tool := range allTools {
		assert.NotNil(t, tool.OutputSchema)
	}
}

func TestThinkTool_ParametersAreObjects(t *testing.T) {
	tool := NewThinkTool()

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, allTools)

	for _, tool := range allTools {
		m, err := tools.SchemaToMap(tool.Parameters)

		require.NoError(t, err)
		assert.Equal(t, "object", m["type"])
	}
}
