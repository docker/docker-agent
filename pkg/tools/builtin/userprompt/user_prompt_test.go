package userprompt

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestUserPromptTool_AcceptResponse(t *testing.T) {
	tool := NewUserPromptTool()

	tool.SetElicitationHandler(func(_ context.Context, req *mcp.ElicitParams) (tools.ElicitationResult, error) {
		assert.Equal(t, "What is your name?", req.Message)
		return tools.ElicitationResult{
			Action:  tools.ElicitationActionAccept,
			Content: map[string]any{"name": "Alice"},
		}, nil
	})

	result, err := tool.userPrompt(t.Context(), Args{Message: "What is your name?"})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var response Response
	err = json.Unmarshal([]byte(result.Output), &response)
	require.NoError(t, err)
	assert.Equal(t, "accept", response.Action)
	assert.Equal(t, "Alice", response.Content["name"])
}

func TestUserPromptTool_DeclineResponse(t *testing.T) {
	tool := NewUserPromptTool()

	tool.SetElicitationHandler(func(_ context.Context, _ *mcp.ElicitParams) (tools.ElicitationResult, error) {
		return tools.ElicitationResult{
			Action: tools.ElicitationActionDecline,
		}, nil
	})

	result, err := tool.userPrompt(t.Context(), Args{Message: "Do you want to proceed?"})
	require.NoError(t, err)
	assert.True(t, result.IsError)

	var response Response
	err = json.Unmarshal([]byte(result.Output), &response)
	require.NoError(t, err)
	assert.Equal(t, "decline", response.Action)
	assert.Nil(t, response.Content)
}

func TestUserPromptTool_CancelResponse(t *testing.T) {
	tool := NewUserPromptTool()

	tool.SetElicitationHandler(func(_ context.Context, _ *mcp.ElicitParams) (tools.ElicitationResult, error) {
		return tools.ElicitationResult{
			Action: tools.ElicitationActionCancel,
		}, nil
	})

	result, err := tool.userPrompt(t.Context(), Args{Message: "Enter your API key"})
	require.NoError(t, err)
	assert.True(t, result.IsError)

	var response Response
	err = json.Unmarshal([]byte(result.Output), &response)
	require.NoError(t, err)
	assert.Equal(t, "cancel", response.Action)
}

func TestUserPromptTool_WithSchema(t *testing.T) {
	tool := NewUserPromptTool()

	var receivedSchema any
	tool.SetElicitationHandler(func(_ context.Context, req *mcp.ElicitParams) (tools.ElicitationResult, error) {
		receivedSchema = req.RequestedSchema
		return tools.ElicitationResult{
			Action:  tools.ElicitationActionAccept,
			Content: map[string]any{"username": "bob", "remember": true},
		}, nil
	})

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"username": map[string]any{"type": "string"},
			"remember": map[string]any{"type": "boolean"},
		},
		"required": []string{"username"},
	}

	result, err := tool.userPrompt(t.Context(), Args{
		Message: "Login",
		Schema:  schema,
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	schemaMap, ok := receivedSchema.(map[string]any)
	require.True(t, ok, "expected schema to be map[string]any")
	assert.Equal(t, "object", schemaMap["type"])

	var response Response
	err = json.Unmarshal([]byte(result.Output), &response)
	require.NoError(t, err)
	assert.Equal(t, "bob", response.Content["username"])
	assert.Equal(t, true, response.Content["remember"])
}
