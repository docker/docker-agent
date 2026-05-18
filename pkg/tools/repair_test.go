package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These are integration tests for NewHandler's repair plumbing. Unit tests
// for the four repair kinds live in github.com/docker/json-tool-repair.

func TestNewHandler_RepairsBareStringArray(t *testing.T) {
	type fileArgs struct {
		Paths []string `json:"paths"`
	}
	var got fileArgs
	handler := NewHandler(func(_ context.Context, args fileArgs) (*ToolCallResult, error) {
		got = args
		return ResultSuccess("ok"), nil
	})

	result, err := handler(t.Context(), ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: FunctionCall{
			Name:      "read_multiple_files",
			Arguments: `{"paths":"only.txt"}`,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "ok", result.Output)
	assert.Equal(t, []string{"only.txt"}, got.Paths)
}

func TestNewHandler_RepairsStringifiedArray(t *testing.T) {
	type fileArgs struct {
		Paths []string `json:"paths"`
	}
	var got fileArgs
	handler := NewHandler(func(_ context.Context, args fileArgs) (*ToolCallResult, error) {
		got = args
		return ResultSuccess("ok"), nil
	})

	result, err := handler(t.Context(), ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: FunctionCall{
			Name:      "read_multiple_files",
			Arguments: `{"paths":"[\"a.txt\",\"b.txt\"]"}`,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "ok", result.Output)
	assert.Equal(t, []string{"a.txt", "b.txt"}, got.Paths)
}

func TestNewHandler_UnrepairableInputReturnsOriginalError(t *testing.T) {
	type fileArgs struct {
		Paths []string `json:"paths"`
	}
	handler := NewHandler(func(_ context.Context, _ fileArgs) (*ToolCallResult, error) {
		t.Fatal("handler should not be called for unrepairable input")
		return nil, nil
	})

	_, err := handler(t.Context(), ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: FunctionCall{
			Name:      "read_multiple_files",
			Arguments: `{not even json`,
		},
	})
	require.Error(t, err)
}
