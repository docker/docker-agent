package tools_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

// fakeToolSet is a minimal ToolSet used to assert wrapper behaviour.
type fakeToolSet struct{}

func (fakeToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (fakeToolSet) Instructions() string                        { return "" }

// namedFake is a fakeToolSet that already advertises a Name(); WithName
// must respect that and return the inner toolset unchanged.
type namedFake struct {
	fakeToolSet

	name string
}

func (n namedFake) Name() string { return n.name }

func TestWithName_AddsNameToBareToolset(t *testing.T) {
	t.Parallel()
	wrapped := tools.WithName(fakeToolSet{}, "shell")
	require.NotNil(t, wrapped)
	assert.Equal(t, "shell", tools.GetName(wrapped))
}

func TestWithName_NoOpWhenNameAlreadySet(t *testing.T) {
	t.Parallel()
	original := namedFake{name: "github-mcp"}
	wrapped := tools.WithName(original, "mcp")
	// Wrapping must NOT shadow the existing name.
	assert.Equal(t, "github-mcp", tools.GetName(wrapped))
	// And must return the original toolset (no extra wrapping). The
	// returned ToolSet must still be a namedFake — if a wrapper had
	// been inserted it would now be a *namedToolSet, which doesn't
	// match this type assertion.
	_, isNamedFake := wrapped.(namedFake)
	assert.True(t, isNamedFake, "WithName must not wrap a toolset that already has a Name")
}

func TestWithName_EmptyNameIsNoOp(t *testing.T) {
	t.Parallel()
	original := fakeToolSet{}
	wrapped := tools.WithName(original, "")
	assert.Empty(t, tools.GetName(wrapped))
}

// TestWithName_UnwrapReachesInnerCapabilities guards against the wrapper
// hiding non-Named capabilities (Statable, Restartable, Kinder, …) from
// callers that walk the chain via tools.As.
func TestWithName_UnwrapReachesInnerCapabilities(t *testing.T) {
	t.Parallel()
	// Wrap a Startable+Stoppable Toolset: the existing Startable wrapper
	// (StartableToolSet) is the canonical decorator, so it makes a good
	// stand-in for "any inner capability the wrapper must not hide".
	inner := tools.NewStartable(fakeToolSet{})
	wrapped := tools.WithName(inner, "shell")

	// The Named interface is satisfied by the outer wrapper.
	assert.Equal(t, "shell", tools.GetName(wrapped))
	// And the inner Startable is still reachable via tools.As.
	got, ok := tools.As[*tools.StartableToolSet](wrapped)
	require.True(t, ok, "tools.As must walk through the WithName wrapper")
	assert.Same(t, inner, got)
}
