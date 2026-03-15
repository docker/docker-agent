package teamloader

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

type toolSet struct {
	tools.ToolSet
	instruction string
}

func (t toolSet) Instructions() string {
	return t.instruction
}

func TestWithEmptyInstructions(t *testing.T) {
	inner := &toolSet{}

	wrapped := WithInstructions(inner, "")

	assert.Same(t, wrapped, inner)
}

func TestWithInstructions_replace(t *testing.T) {
	inner := &toolSet{
		instruction: "Existing instructions",
	}

	wrapped := WithInstructions(inner, "New instructions")

	assert.Equal(t, "New instructions", tools.GetInstructions(wrapped))
}

func TestWithInstructions_add(t *testing.T) {
	inner := &toolSet{
		instruction: "Existing instructions",
	}

	wrapped := WithInstructions(inner, "{ORIGINAL_INSTRUCTIONS}\nMore instructions")

	assert.Equal(t, "Existing instructions\nMore instructions", tools.GetInstructions(wrapped))
}

type startableInstructableToolSet struct {
	toolSet
	started bool
}

func (s *startableInstructableToolSet) Start(_ context.Context) error {
	s.started = true
	return nil
}

func (s *startableInstructableToolSet) Stop(_ context.Context) error {
	s.started = false
	return nil
}

func TestWithInstructions_ForwardsStartToStartableInner(t *testing.T) {
	t.Parallel()

	inner := &startableInstructableToolSet{
		toolSet: toolSet{instruction: "test"},
	}

	wrapped := WithInstructions(inner, "New instructions")

	startable, ok := wrapped.(tools.Startable)
	require.True(t, ok, "replaceInstruction should implement tools.Startable")

	err := startable.Start(t.Context())
	require.NoError(t, err)
	assert.True(t, inner.started, "Start() should have been forwarded to inner toolset")

	err = startable.Stop(t.Context())
	require.NoError(t, err)
	assert.False(t, inner.started, "Stop() should have been forwarded to inner toolset")
}

func TestWithInstructions_StartNoOpForNonStartableInner(t *testing.T) {
	t.Parallel()

	inner := &toolSet{instruction: "test"}
	wrapped := WithInstructions(inner, "New instructions")

	startable, ok := wrapped.(tools.Startable)
	require.True(t, ok, "replaceInstruction should implement tools.Startable")

	err := startable.Start(t.Context())
	require.NoError(t, err)

	err = startable.Stop(t.Context())
	require.NoError(t, err)
}
