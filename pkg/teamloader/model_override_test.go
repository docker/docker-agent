package teamloader

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestWithModelOverride_Empty(t *testing.T) {
	inner := &mockToolSet{
		toolsFunc: func(_ context.Context) ([]tools.Tool, error) {
			return []tools.Tool{{Name: "read_file"}}, nil
		},
	}

	// Empty model string should return the inner toolset as-is.
	wrapped := WithModelOverride(inner, "")
	assert.Same(t, inner, wrapped)
}

func TestWithModelOverride_SetsModelOnTools(t *testing.T) {
	inner := &mockToolSet{
		toolsFunc: func(_ context.Context) ([]tools.Tool, error) {
			return []tools.Tool{
				{Name: "read_file"},
				{Name: "write_file"},
			}, nil
		},
	}

	wrapped := WithModelOverride(inner, "openai/gpt-4o-mini")
	result, err := wrapped.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, result, 2)
	assert.Equal(t, "openai/gpt-4o-mini", result[0].ModelOverride)
	assert.Equal(t, "openai/gpt-4o-mini", result[1].ModelOverride)
}

func TestWithModelOverride_DoesNotMutateOriginal(t *testing.T) {
	inner := &mockToolSet{
		toolsFunc: func(_ context.Context) ([]tools.Tool, error) {
			return []tools.Tool{
				{Name: "read_file"},
			}, nil
		},
	}

	wrapped := WithModelOverride(inner, "openai/gpt-4o-mini")
	result, err := wrapped.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, "openai/gpt-4o-mini", result[0].ModelOverride)

	// Original tools should be unaffected since we copy.
	originalTools, err := inner.Tools(t.Context())
	require.NoError(t, err)
	assert.Empty(t, originalTools[0].ModelOverride)
}

func TestWithModelOverride_Unwrap(t *testing.T) {
	inner := &mockToolSet{
		toolsFunc: func(_ context.Context) ([]tools.Tool, error) {
			return []tools.Tool{{Name: "read_file"}}, nil
		},
	}

	wrapped := WithModelOverride(inner, "openai/gpt-4o-mini")

	unwrapper, ok := wrapped.(tools.Unwrapper)
	require.True(t, ok)
	assert.Same(t, inner, unwrapper.Unwrap())
}

func TestWithModelOverride_Instructions(t *testing.T) {
	inner := &instructableToolSet{
		mockToolSet: mockToolSet{
			toolsFunc: func(_ context.Context) ([]tools.Tool, error) {
				return []tools.Tool{{Name: "read_file"}}, nil
			},
		},
		instructions: "Use this for file operations",
	}

	wrapped := WithModelOverride(inner, "openai/gpt-4o-mini")

	inst, ok := wrapped.(tools.Instructable)
	require.True(t, ok)
	assert.Equal(t, "Use this for file operations", inst.Instructions())
}
