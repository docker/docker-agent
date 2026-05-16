package modelpicker

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestNewModelPickerTool(t *testing.T) {
	tool := NewModelPickerTool([]string{"openai/gpt-4o", "anthropic/claude-sonnet-4-0"})
	assert.NotNil(t, tool)
}

func TestModelPickerTool_AllowedModels(t *testing.T) {
	models := []string{"openai/gpt-4o", "anthropic/claude-sonnet-4-0", "my_fast_model"}
	tool := NewModelPickerTool(models)

	assert.Equal(t, models, tool.AllowedModels())
}

func TestModelPickerTool_Tools(t *testing.T) {
	models := []string{"openai/gpt-4o", "anthropic/claude-sonnet-4-0"}
	tool := NewModelPickerTool(models)

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	assert.Len(t, allTools, 2)

	// Verify change_model tool
	changeTool := allTools[0]
	assert.Equal(t, ToolNameChangeModel, changeTool.Name)
	assert.Equal(t, "model", changeTool.Category)
	assert.Contains(t, changeTool.Description, "openai/gpt-4o")
	assert.Contains(t, changeTool.Description, "anthropic/claude-sonnet-4-0")
	assert.True(t, changeTool.Annotations.ReadOnlyHint)
	assert.Equal(t, "Change Model", changeTool.Annotations.Title)
	assert.Nil(t, changeTool.Handler)

	// Verify change_model schema
	schema, err := json.Marshal(changeTool.Parameters)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"type": "object",
		"properties": {
			"model": {
				"description": "The model to switch to. Must be one of the available models.",
				"type": "string"
			}
		},
		"additionalProperties": false,
		"required": ["model"]
	}`, string(schema))

	// Verify revert_model tool
	revertTool := allTools[1]
	assert.Equal(t, ToolNameRevertModel, revertTool.Name)
	assert.Equal(t, "model", revertTool.Category)
	assert.Contains(t, revertTool.Description, "Revert")
	assert.True(t, revertTool.Annotations.ReadOnlyHint)
	assert.Equal(t, "Revert Model", revertTool.Annotations.Title)
	assert.Nil(t, revertTool.Handler)
}

func TestModelPickerTool_ToolsDescriptionListsModels(t *testing.T) {
	models := []string{"fast_model", "smart_model", "openai/gpt-4o"}
	tool := NewModelPickerTool(models)

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)

	changeTool := allTools[0]
	for _, model := range models {
		assert.Contains(t, changeTool.Description, model,
			"change_model description should list all available models")
	}
}

func TestModelPickerTool_DisplayNames(t *testing.T) {
	tool := NewModelPickerTool([]string{"openai/gpt-4o"})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)

	for _, tool := range allTools {
		assert.NotEmpty(t, tool.DisplayName())
		assert.NotEqual(t, tool.Name, tool.DisplayName())
		assert.Equal(t, "model", tool.Category)
	}
}

func TestModelPickerTool_ParametersAreObjects(t *testing.T) {
	tool := NewModelPickerTool([]string{"openai/gpt-4o"})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, allTools)

	// change_model has parameters
	m, err := tools.SchemaToMap(allTools[0].Parameters)
	require.NoError(t, err)
	assert.Equal(t, "object", m["type"])

	// revert_model has no parameters (nil)
	assert.Nil(t, allTools[1].Parameters)
}

func TestModelPickerTool_ReadOnlyHint(t *testing.T) {
	tool := NewModelPickerTool([]string{"openai/gpt-4o"})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)

	for _, tool := range allTools {
		assert.True(t, tool.Annotations.ReadOnlyHint,
			"tool %s should have ReadOnlyHint set to true", tool.Name)
	}
}

func TestModelPickerTool_NotStartable(t *testing.T) {
	tool := NewModelPickerTool([]string{"openai/gpt-4o"})

	_, ok := any(tool).(tools.Startable)
	assert.False(t, ok, "Tool should not implement Startable")
}

func TestModelPickerTool_Instructions(t *testing.T) {
	models := []string{"openai/gpt-4o", "anthropic/claude-sonnet-4-0"}
	tool := NewModelPickerTool(models)

	_, ok := any(tool).(tools.Instructable)
	assert.True(t, ok, "Tool should implement Instructable")

	instructions := tool.Instructions()
	assert.NotEmpty(t, instructions)
	assert.Contains(t, instructions, "change_model")
	assert.Contains(t, instructions, "revert_model")
	for _, model := range models {
		assert.Contains(t, instructions, model)
	}
}

func TestModelPickerTool_SingleModel(t *testing.T) {
	tool := NewModelPickerTool([]string{"openai/gpt-4o"})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	assert.Len(t, allTools, 2)
	assert.Contains(t, allTools[0].Description, "openai/gpt-4o")
}

func TestModelPickerTool_ManyModels(t *testing.T) {
	models := []string{
		"openai/gpt-4o",
		"anthropic/claude-sonnet-4-0",
		"google/gemini-2.0-flash",
		"my_custom_model",
	}
	tool := NewModelPickerTool(models)

	assert.Equal(t, models, tool.AllowedModels())

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	for _, model := range models {
		assert.Contains(t, allTools[0].Description, model)
	}
}
