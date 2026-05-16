package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewHandler_WithArguments(t *testing.T) {
	type Args struct {
		Name string `json:"name"`
	}

	handler := NewHandler(func(_ context.Context, args Args) (*ToolCallResult, error) {
		return ResultSuccess("hello " + args.Name), nil
	})

	result, err := handler(t.Context(), ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: FunctionCall{
			Name:      "greet",
			Arguments: `{"name":"world"}`,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "hello world", result.Output)
}

func TestNewHandler_EmptyArguments(t *testing.T) {
	handler := NewHandler(func(_ context.Context, _ map[string]any) (*ToolCallResult, error) {
		return ResultSuccess("ok"), nil
	})

	result, err := handler(t.Context(), ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: FunctionCall{
			Name:      "get_memories",
			Arguments: "",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "ok", result.Output)
}

func TestNewHandler_EmptyObjectArguments(t *testing.T) {
	handler := NewHandler(func(_ context.Context, _ map[string]any) (*ToolCallResult, error) {
		return ResultSuccess("ok"), nil
	})

	result, err := handler(t.Context(), ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: FunctionCall{
			Name:      "list_things",
			Arguments: "{}",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "ok", result.Output)
}

func TestNewHandler_InvalidArguments(t *testing.T) {
	handler := NewHandler(func(_ context.Context, _ map[string]any) (*ToolCallResult, error) {
		return ResultSuccess("ok"), nil
	})

	_, err := handler(t.Context(), ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: FunctionCall{
			Name:      "broken",
			Arguments: `{"unterminated`,
		},
	})
	require.Error(t, err)
}
