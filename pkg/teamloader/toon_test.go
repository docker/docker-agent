package teamloader

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func mockHandler(output string) tools.ToolHandler {
	return func(ctx context.Context, toolCall tools.ToolCall) (*tools.ToolCallResult, error) {
		return tools.ResultSuccess(output), nil
	}
}

func TestToon(t *testing.T) {
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
			wrapped := WithToon(inner, toolFilter)

			resultTools, err := wrapped.Tools(t.Context())
			require.NoError(t, err)
			require.Len(t, resultTools, 1)

			result, err := resultTools[0].Handler(t.Context(), tools.ToolCall{})
			require.NoError(t, err)
			assert.Equal(t, tc.expected, result.Output)
		})
	}
}

func TestWithToon_ForwardsStartToStartableInner(t *testing.T) {
	t.Parallel()

	inner := &startableToolSet{
		mockToolSet: mockToolSet{
			toolsFunc: func(ctx context.Context) ([]tools.Tool, error) {
				return []tools.Tool{{Name: "tool1"}}, nil
			},
		},
	}

	wrapped := WithToon(inner, "tool1")

	startable, ok := wrapped.(tools.Startable)
	require.True(t, ok, "toonTools should implement tools.Startable")

	err := startable.Start(t.Context())
	require.NoError(t, err)
	assert.True(t, inner.started, "Start() should have been forwarded to inner toolset")

	err = startable.Stop(t.Context())
	require.NoError(t, err)
	assert.False(t, inner.started, "Stop() should have been forwarded to inner toolset")
}

func TestWithToon_StartNoOpForNonStartableInner(t *testing.T) {
	t.Parallel()

	inner := &mockToolSet{
		toolsFunc: func(ctx context.Context) ([]tools.Tool, error) {
			return []tools.Tool{{Name: "tool1"}}, nil
		},
	}

	wrapped := WithToon(inner, "tool1")

	startable, ok := wrapped.(tools.Startable)
	require.True(t, ok, "toonTools should implement tools.Startable")

	err := startable.Start(t.Context())
	require.NoError(t, err)

	err = startable.Stop(t.Context())
	require.NoError(t, err)
}
