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

// startableToolSet is a mock that implements both ToolSet and Startable,
// like the real MCP toolset does.
type startableToolSet struct {
	mockToolSet
	started bool
}

func (s *startableToolSet) Start(context.Context) error {
	s.started = true
	return nil
}

func (s *startableToolSet) Stop(context.Context) error {
	s.started = false
	return nil
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

func TestWithToolsFilter_ForwardsStartToStartableInner(t *testing.T) {
	t.Parallel()

	inner := &startableToolSet{
		mockToolSet: mockToolSet{
			toolsFunc: func(context.Context) ([]tools.Tool, error) {
				return []tools.Tool{{Name: "tool1"}, {Name: "tool2"}}, nil
			},
		},
	}

	wrapped := WithToolsFilter(inner, "tool1")

	// Verify the inner toolset is not started yet
	assert.False(t, inner.started)

	// The wrapped filterTools should satisfy Startable
	startable, ok := wrapped.(tools.Startable)
	require.True(t, ok, "filterTools should implement tools.Startable")

	// Start should forward to the inner toolset
	err := startable.Start(t.Context())
	require.NoError(t, err)
	assert.True(t, inner.started, "Start() should have been forwarded to inner toolset")

	// Stop should also forward
	err = startable.Stop(t.Context())
	require.NoError(t, err)
	assert.False(t, inner.started, "Stop() should have been forwarded to inner toolset")
}

func TestWithToolsFilter_StartNoOpForNonStartableInner(t *testing.T) {
	t.Parallel()

	inner := &mockToolSet{
		toolsFunc: func(context.Context) ([]tools.Tool, error) {
			return []tools.Tool{{Name: "tool1"}}, nil
		},
	}

	wrapped := WithToolsFilter(inner, "tool1")

	// Should still implement Startable
	startable, ok := wrapped.(tools.Startable)
	require.True(t, ok, "filterTools should implement tools.Startable")

	// Start/Stop should be no-ops without error
	err := startable.Start(t.Context())
	require.NoError(t, err)

	err = startable.Stop(t.Context())
	require.NoError(t, err)
}

func TestWithToolsFilter_StartableToolSetIntegration(t *testing.T) {
	t.Parallel()

	// This test simulates the real wrapping: MCP → filterTools → StartableToolSet
	inner := &startableToolSet{
		mockToolSet: mockToolSet{
			toolsFunc: func(context.Context) ([]tools.Tool, error) {
				return []tools.Tool{{Name: "tool1"}, {Name: "tool2"}}, nil
			},
		},
	}

	// Wrap in filterTools (like teamloader does)
	filtered := WithToolsFilter(inner, "tool1")

	// Wrap in StartableToolSet (like agent.WithToolSets does)
	startable := tools.NewStartable(filtered)

	// Start should propagate through: StartableToolSet → filterTools → startableToolSet
	err := startable.Start(t.Context())
	require.NoError(t, err)
	assert.True(t, startable.IsStarted(), "StartableToolSet should be started")
	assert.True(t, inner.started, "Inner startable toolset should have been started")

	// Tools should work through the whole chain
	result, err := startable.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, "tool1", result[0].Name)
}
