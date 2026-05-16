package e2e_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDebug_Toolsets_None(t *testing.T) {
	t.Parallel()

	output := runCLI(t, "debug", "toolsets", "testdata/no_tools.yaml")

	require.Equal(t, "No tools for root\n", output)
}

func TestDebug_Toolsets_Todo(t *testing.T) {
	t.Parallel()

	output := runCLI(t, "debug", "toolsets", "testdata/todo_tools.yaml")

	require.Equal(t, "2 tool(s) for root:\n + create_todo - Create a new todo item with a description\n + list_todos - List all current todos with their status\n", output)
}
