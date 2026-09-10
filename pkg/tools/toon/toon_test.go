package toon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func mockHandler(output string) tools.ToolHandler {
	return func(ctx context.Context, toolCall tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
		return tools.ResultSuccess(output), nil
	}
}

func TestToon(t *testing.T) {
	t.Parallel()

	testcases := []struct {
		name       string
		toolResult string
		expected   string
		filter     string
	}{
		{
			name:       "should return a toon representation of a json response",
			toolResult: `{"key": "value", "number": 42}`,
			expected:   "key: value\nnumber: 42",
		},
		{
			name:       "should return originial if not a json",
			toolResult: "plain text output",
			expected:   "plain text output",
		},
		{
			name:       "should return original if not toon-ed",
			toolResult: `{"key": "value", "number": 42}`,
			expected:   `{"key": "value", "number": 42}`,
			filter:     "other_tool",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner := &mockToolSet{
				toolsFunc: func(ctx context.Context) ([]tools.Tool, error) {
					return []tools.Tool{
						{
							Name:    "test_tool",
							Handler: mockHandler(tc.toolResult),
						},
					}, nil
				},
			}
			toolFilter := "test_tool"
			if tc.filter != "" {
				toolFilter = tc.filter
			}
			wrapped := Wrap(inner, toolFilter)

			resultTools, err := wrapped.Tools(t.Context())
			require.NoError(t, err)
			require.Len(t, resultTools, 1)

			result, err := resultTools[0].Handler(t.Context(), tools.ToolCall{}, tools.NopRuntime{})
			require.NoError(t, err)
			assert.Equal(t, tc.expected, result.Output)
		})
	}
}

// The inner toolset's slice must be left untouched: MCP hands out its cached
// slice, and mutating it would re-wrap the handlers on every listing.
func TestToonDoesNotMutateInnerSlice(t *testing.T) {
	t.Parallel()

	var calls int
	cached := []tools.Tool{{
		Name: "test_tool",
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			calls++
			return tools.ResultSuccess(`{"key": "value"}`), nil
		},
	}}
	inner := &mockToolSet{
		toolsFunc: func(context.Context) ([]tools.Tool, error) { return cached, nil },
	}
	wrapped := Wrap(inner, "test_.*,.*_tool")

	for range 3 {
		resultTools, err := wrapped.Tools(t.Context())
		require.NoError(t, err)
		result, err := resultTools[0].Handler(t.Context(), tools.ToolCall{}, tools.NopRuntime{})
		require.NoError(t, err)
		assert.Equal(t, "key: value", result.Output)
	}

	// The original handler is still unwrapped and was invoked exactly once per call.
	res, err := cached[0].Handler(t.Context(), tools.ToolCall{}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"key": "value"}`, res.Output)
	assert.Equal(t, 4, calls)
}

type mockToolSet struct {
	tools.ToolSet

	toolsFunc func(ctx context.Context) ([]tools.Tool, error)
}

func (m *mockToolSet) Tools(ctx context.Context) ([]tools.Tool, error) {
	if m.toolsFunc != nil {
		return m.toolsFunc(ctx)
	}
	return nil, nil
}
