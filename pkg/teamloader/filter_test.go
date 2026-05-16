package teamloader

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

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

func TestWithToolsFilter_NilToolNames(t *testing.T) {
	inner := &mockToolSet{}

	wrapped := WithToolsFilter(inner)

	assert.Same(t, inner, wrapped)
}

func TestWithToolsFilter_EmptyNames(t *testing.T) {
	inner := &mockToolSet{}

	wrapped := WithToolsFilter(inner, []string{}...)

	assert.Same(t, inner, wrapped)
}

func TestWithToolsFilter_PickOne(t *testing.T) {
	inner := &mockToolSet{
		toolsFunc: func(context.Context) ([]tools.Tool, error) {
			return []tools.Tool{{Name: "tool1"}, {Name: "tool2"}, {Name: "tool3"}}, nil
		},
	}

	wrapped := WithToolsFilter(inner, "tool2")

	result, err := wrapped.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, "tool2", result[0].Name)
}

func TestWithToolsFilter_PickAll(t *testing.T) {
	inner := &mockToolSet{
		toolsFunc: func(context.Context) ([]tools.Tool, error) {
			return []tools.Tool{{Name: "tool1"}, {Name: "tool2"}, {Name: "tool3"}}, nil
		},
	}

	wrapped := WithToolsFilter(inner, "tool1", "tool2", "tool3")

	result, err := wrapped.Tools(t.Context())
	require.NoError(t, err)

	require.Len(t, result, 3)
	assert.Equal(t, "tool1", result[0].Name)
	assert.Equal(t, "tool2", result[1].Name)
	assert.Equal(t, "tool3", result[2].Name)
}

func TestWithToolsFilter_NoMatch(t *testing.T) {
	inner := &mockToolSet{
		toolsFunc: func(context.Context) ([]tools.Tool, error) {
			return []tools.Tool{{Name: "tool1"}, {Name: "tool2"}}, nil
		},
	}

	wrapped := WithToolsFilter(inner, "tool3", "tool4")

	result, err := wrapped.Tools(t.Context())
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestWithToolsFilter_ErrorFromInner(t *testing.T) {
	expectedErr := errors.New("mock error")
	inner := &mockToolSet{
		toolsFunc: func(context.Context) ([]tools.Tool, error) {
			return nil, expectedErr
		},
	}

	wrapped := WithToolsFilter(inner, "tool1")

	result, err := wrapped.Tools(t.Context())
	assert.Nil(t, result)
	assert.ErrorIs(t, err, expectedErr)
}

func TestWithToolsFilter_CaseSensitive(t *testing.T) {
	inner := &mockToolSet{
		toolsFunc: func(ctx context.Context) ([]tools.Tool, error) {
			return []tools.Tool{
				{Name: "Tool1"},
				{Name: "tool1"},
				{Name: "TOOL1"},
			}, nil
		},
	}

	wrapped := WithToolsFilter(inner, "tool1")

	result, err := wrapped.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, "tool1", result[0].Name)
}

type instructableToolSet struct {
	mockToolSet

	instructions string
}

func (i *instructableToolSet) Instructions() string {
	return i.instructions
}

func TestWithToolsFilter_InstructablePassthrough(t *testing.T) {
	// Test that filtering preserves instructions from inner toolset
	inner := &instructableToolSet{
		mockToolSet: mockToolSet{
			toolsFunc: func(context.Context) ([]tools.Tool, error) {
				return []tools.Tool{{Name: "tool1"}, {Name: "tool2"}}, nil
			},
		},
		instructions: "Test instructions for the toolset",
	}

	wrapped := WithToolsFilter(inner, "tool1")

	// Verify instructions are preserved through the filter wrapper
	instructions := tools.GetInstructions(wrapped)
	assert.Equal(t, "Test instructions for the toolset", instructions)

	// Verify filtering still works
	result, err := wrapped.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, "tool1", result[0].Name)
}

func TestWithToolsFilter_NonInstructableInner(t *testing.T) {
	// Test that filter works with toolsets that don't implement Instructable
	inner := &mockToolSet{
		toolsFunc: func(context.Context) ([]tools.Tool, error) {
			return []tools.Tool{{Name: "tool1"}}, nil
		},
	}

	wrapped := WithToolsFilter(inner, "tool1")

	// Verify instructions are empty for non-instructable inner toolset
	instructions := tools.GetInstructions(wrapped)
	assert.Empty(t, instructions)
}
