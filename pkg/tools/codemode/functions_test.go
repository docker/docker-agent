package codemode

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestToolToJsDoc(t *testing.T) {
	type CreateTodoArgs struct {
		Description string `json:"description" jsonschema:"Description of the todo item"`
	}

	tool := tools.Tool{
		Name:         "create_todo",
		Description:  "Create new todo\n each of them with a description",
		Parameters:   tools.MustSchemaFor[CreateTodoArgs](),
		OutputSchema: tools.MustSchemaFor[string](),
	}

	jsDoc := toolToJsDoc(tool)

	assert.Equal(t, `
/**
 * Create new todo
 * each of them with a description
 * 
 * @param args - Input object containing the parameters.
 * @returns Output - The result of the function execution.
 *
 * Where Input follows the following JSON schema:
 * {
 *   "type": "object",
 *   "properties": {
 *     "description": {
 *       "type": "string",
 *       "description": "Description of the todo item"
 *     }
 *   },
 *   "required": [
 *     "description"
 *   ],
 *   "additionalProperties": false
 * }
 *
 * And Output follows the following JSON schema:
 * {
 *   "type": "string"
 * }
 */
function create_todo(args: Input): Output { ... }
`, jsDoc)
}
