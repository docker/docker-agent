package todo_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo/types"
)

func TestTodoAlias(t *testing.T) {
	t.Parallel()

	assert.IsType(t, types.Todo{}, todo.Todo{})
	assert.IsType(t, []types.Todo(nil), []todo.Todo(nil), "tool result Meta must retain its shared type identity")
}
