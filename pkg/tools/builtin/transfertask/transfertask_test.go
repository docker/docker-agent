package transfertask

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestNewTaskTool(t *testing.T) {
	tool := NewTransferTaskTool()
	assert.NotNil(t, tool)
}

func TestTaskTool_Instructions(t *testing.T) {
	tool := NewTransferTaskTool()

	// Tool doesn't implement Instructable
	_, ok := any(tool).(tools.Instructable)
	assert.False(t, ok, "Tool should not implement Instructable")
}

func TestTaskTool_Tools(t *testing.T) {
	tool := NewTransferTaskTool()

	allTools, err := tool.Tools(t.Context())

	require.NoError(t, err)
	assert.Len(t, allTools, 1)

	assert.Equal(t, "transfer_task", allTools[0].Name)
	assert.Equal(t, "transfer", allTools[0].Category)
	assert.Contains(t, allTools[0].Description, "transfer a task to the selected team member")

	assert.Nil(t, allTools[0].Handler)

	schema, err := json.Marshal(allTools[0].Parameters)
	require.NoError(t, err)
	assert.JSONEq(t, `{
	"type": "object",
	"properties": {
		"agent": {
			"description": "The name of the agent to transfer the task to.",
			"type": "string"
		},
		"expected_output": {
			"description": "The expected output from the member (optional).",
			"type": "string"
		},
		"task": {
			"description": "A clear and concise description of the task the member should achieve.",
			"type": "string"
		}
	},
	"additionalProperties": false,
	"required": [
		"agent",
		"task",
		"expected_output"
	]
}`, string(schema))
}

func TestTaskTool_DisplayNames(t *testing.T) {
	tool := NewTransferTaskTool()

	all, err := tool.Tools(t.Context())
	require.NoError(t, err)

	for _, tool := range all {
		assert.NotEmpty(t, tool.DisplayName())
		assert.NotEqual(t, tool.Name, tool.DisplayName())
		assert.Equal(t, "transfer", tool.Category)
	}
}

func TestTaskTool_StartStop(t *testing.T) {
	tool := NewTransferTaskTool()

	// Tool doesn't need to implement Startable -
	// it has no initialization or cleanup requirements
	_, ok := any(tool).(tools.Startable)
	assert.False(t, ok, "Tool should not implement Startable")
}
