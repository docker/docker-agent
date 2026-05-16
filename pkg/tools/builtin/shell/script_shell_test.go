package shell

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestNewScriptShellTool_Empty(t *testing.T) {
	tool, err := NewScriptShellTool(nil, nil)
	require.NoError(t, err)

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	assert.Empty(t, allTools)
}

func TestNewScriptShellTool_ToolNoArg(t *testing.T) {
	shellTools := map[string]latest.ScriptShellToolConfig{
		"get_ip": {
			Description: "Get public IP",
		},
	}

	tool, err := NewScriptShellTool(shellTools, nil)
	require.NoError(t, err)

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	assert.Len(t, allTools, 1)

	schema, err := json.Marshal(allTools[0].Parameters)
	require.NoError(t, err)
	assert.JSONEq(t, `{
	"type": "object",
	"properties": {}
}`, string(schema))
}

func TestNewScriptShellTool_Tool(t *testing.T) {
	shellTools := map[string]latest.ScriptShellToolConfig{
		"github_user_repos": {
			Description: "List GitHub repositories of the provided user",
			Args: map[string]any{
				"username": map[string]any{
					"description": "GitHub username to get the repository list for",
					"type":        "string",
				},
			},
			Required: []string{"username"},
		},
	}

	tool, err := NewScriptShellTool(shellTools, nil)
	require.NoError(t, err)

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	assert.Len(t, allTools, 1)

	schema, err := json.Marshal(allTools[0].Parameters)
	require.NoError(t, err)
	assert.JSONEq(t, `{
	"type": "object",
	"properties": {
		"username": {
			"description": "GitHub username to get the repository list for",
			"type": "string"
		}
	},
	"required": ["username"]
}`, string(schema))
}

func TestNewScriptShellTool_Typo(t *testing.T) {
	shellTools := map[string]latest.ScriptShellToolConfig{
		"docker_images": {
			Description: "List running Docker containers",
			Cmd:         "docker images $image",
			Args: map[string]any{
				"img": map[string]any{
					"description": "Docker image to list",
					"type":        "string",
				},
			},
			Required: []string{"img"},
		},
	}

	tool, err := NewScriptShellTool(shellTools, nil)
	require.Nil(t, tool)
	require.ErrorContains(t, err, "tool 'docker_images' uses undefined args: [image]")
}

func TestNewScriptShellTool_MissingRequired(t *testing.T) {
	shellTools := map[string]latest.ScriptShellToolConfig{
		"docker_images": {
			Description: "List running Docker containers",
			Cmd:         "docker images $image",
			Args: map[string]any{
				"image": map[string]any{
					"description": "Docker image to list",
					"type":        "string",
				},
			},
			Required: []string{"img"},
		},
	}

	tool, err := NewScriptShellTool(shellTools, nil)
	require.Nil(t, tool)
	require.ErrorContains(t, err, "tool 'docker_images' has required arg 'img' which is not defined in args")
}

func TestNewScriptShellTool_NumberArg(t *testing.T) {
	shellTools := map[string]latest.ScriptShellToolConfig{
		"repeat": {
			Description: "Repeat a message N times",
			Cmd:         "for i in $(seq 1 $count); do echo $message; done",
			Args: map[string]any{
				"message": map[string]any{
					"description": "Message to repeat",
					"type":        "string",
				},
				"count": map[string]any{
					"description": "Number of repetitions",
					"type":        "number",
				},
			},
			Required: []string{"message", "count"},
		},
	}

	tool, err := NewScriptShellTool(shellTools, os.Environ())
	require.NoError(t, err)

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)

	// Simulate LLM sending a number argument (JSON numbers are float64)
	result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Arguments: `{"message": "hello", "count": 3}`,
		},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError, "unexpected error: %s", result.Output)
	assert.Equal(t, "hello\nhello\nhello\n", result.Output)
}

func TestScriptShellTool_DropsUndeclaredArgs(t *testing.T) {
	// `env` lists the spawned process's full environment. With base env
	// set to an empty slice, the only entries should be those forwarded
	// from declared args.
	shellTools := map[string]latest.ScriptShellToolConfig{
		"echo_name": {
			Cmd: "env",
			Args: map[string]any{
				"name": map[string]any{
					"description": "who to greet",
					"type":        "string",
				},
			},
			Required: []string{"name"},
		},
	}

	tool, err := NewScriptShellTool(shellTools, []string{})
	require.NoError(t, err)

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)

	// The LLM hallucinates LD_PRELOAD alongside the declared `name`.
	// Only `name` should reach execve; LD_PRELOAD must be dropped.
	result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Arguments: `{"name": "alice", "LD_PRELOAD": "/tmp/evil.so"}`,
		},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError, "unexpected error: %s", result.Output)
	assert.Contains(t, result.Output, "name=alice")
	assert.NotContains(t, result.Output, "LD_PRELOAD")
}

func TestScriptShellTool_RejectsNULInValue(t *testing.T) {
	shellTools := map[string]latest.ScriptShellToolConfig{
		"echo_name": {
			Cmd: "echo $name",
			Args: map[string]any{
				"name": map[string]any{
					"description": "who to greet",
					"type":        "string",
				},
			},
			Required: []string{"name"},
		},
	}

	tool, err := NewScriptShellTool(shellTools, []string{})
	require.NoError(t, err)

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)

	result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Arguments: "{\"name\": \"alice\\u0000extra\"}",
		},
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "NUL byte")
}

func TestNewScriptShellTool_ArgWithoutType(t *testing.T) {
	shellTools := map[string]latest.ScriptShellToolConfig{
		"greet": {
			Description: "Greet someone",
			Cmd:         "echo Hello $name",
			Args: map[string]any{
				"name": map[string]any{
					"description": "Name to greet",
				},
			},
			Required: []string{"name"},
		},
	}

	tool, err := NewScriptShellTool(shellTools, nil)
	require.NoError(t, err)

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	assert.Len(t, allTools, 1)

	schema, err := json.Marshal(allTools[0].Parameters)
	require.NoError(t, err)
	assert.JSONEq(t, `{
	"type": "object",
	"properties": {
		"name": {
			"description": "Name to greet",
			"type": "string"
		}
	},
	"required": ["name"]
}`, string(schema))
}
