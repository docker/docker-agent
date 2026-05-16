package codemode

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestCodeModeTool_Tools(t *testing.T) {
	tool := &codeModeTool{}

	toolSet, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, toolSet, 1)

	fetchTool := toolSet[0]
	assert.Equal(t, "run_tools_with_javascript", fetchTool.Name)
	assert.Equal(t, "code mode", fetchTool.Category)
	assert.NotNil(t, fetchTool.Handler)

	inputSchema, err := json.Marshal(fetchTool.Parameters)
	require.NoError(t, err)
	assert.JSONEq(t, `{
	"type": "object",
	"required": [
		"script"
	],
	"properties": {
		"script": {
			"type": "string",
			"description": "Script to execute"
		}
	},
	"additionalProperties": false
}`, string(inputSchema))

	outputSchema, err := json.Marshal(fetchTool.OutputSchema)
	require.NoError(t, err)
	assert.JSONEq(t, `{
	"type": "object",
	"required": [
		"value",
		"stdout",
		"stderr"
	],
	"properties": {
		"stderr": {
			"type": "string",
			"description": "The standard error of the console"
		},
		"stdout": {
			"type": "string",
			"description": "The standard output of the console"
		},
		"value": {
			"type": "string",
			"description": "The value returned by the script"
		},
		"tool_calls": {
			"type": ["null", "array"],
			"description": "The list of tool calls made during script execution, only included on failure",
			"items": {
				"type": "object",
				"additionalProperties": false,
				"required": ["name", "arguments"],
				"properties": {
					"name": {
						"type": "string",
						"description": "The name of the tool that was called"
					},
					"arguments": {
						"description": "The arguments passed to the tool"
					},
					"result": {
						"type": "string",
						"description": "The raw response returned by the tool"
					},
					"error": {
						"type": "string",
						"description": "The error message, if the tool call failed"
					}
				}
			}
		}
	},
	"additionalProperties": false
}`, string(outputSchema))
}

func TestCodeModeTool_Instructions(t *testing.T) {
	tool := &codeModeTool{}

	instructions := tools.GetInstructions(tool)

	assert.Empty(t, instructions)
}

func TestCodeModeTool_StartStop(t *testing.T) {
	inner := &testToolSet{}

	tool := Wrap(inner)

	assert.Equal(t, 0, inner.start)
	assert.Equal(t, 0, inner.stop)

	startable := tool.(tools.Startable)
	err := startable.Start(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, inner.start)
	assert.Equal(t, 0, inner.stop)

	err = startable.Stop(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, inner.start)
	assert.Equal(t, 1, inner.stop)
}

func TestCodeModeTool_CallHello(t *testing.T) {
	tool := Wrap(&testToolSet{
		tools: []tools.Tool{
			{
				Name: "hello_world",
				Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
					return tools.ResultSuccess("Hello, World!"), nil
				}),
			},
		},
	})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)

	result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Arguments: `{"script":"return hello_world();"}`,
		},
	})
	require.NoError(t, err)

	var scriptResult ScriptResult
	err = json.Unmarshal([]byte(result.Output), &scriptResult)
	require.NoError(t, err)

	require.Equal(t, "Hello, World!", scriptResult.Value)
	require.Empty(t, scriptResult.StdErr)
	require.Empty(t, scriptResult.StdOut)
}

func TestCodeModeTool_CallEcho(t *testing.T) {
	type EchoArgs struct {
		Message string `json:"message" jsonschema:"Message to echo"`
	}

	tool := Wrap(&testToolSet{
		tools: []tools.Tool{{
			Name: "echo",
			Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
				return tools.ResultSuccess("ECHO"), nil
			}),
			Parameters: tools.MustSchemaFor[EchoArgs](),
		}},
	})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)

	result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Arguments: `{"script":"return echo({'message':'ECHO'});"}`,
		},
	})
	require.NoError(t, err)

	var scriptResult ScriptResult
	err = json.Unmarshal([]byte(result.Output), &scriptResult)
	require.NoError(t, err)

	require.Equal(t, "ECHO", scriptResult.Value)
	require.Empty(t, scriptResult.StdErr)
	require.Empty(t, scriptResult.StdOut)
}

// TestCodeModeTool_StartRollsBackOnError verifies that when one toolset fails
// to start, all successfully-started toolsets are stopped (rolled back).
func TestCodeModeTool_StartRollsBackOnError(t *testing.T) {
	failing := &testToolSet{startErr: assert.AnError}
	healthy := &testToolSet{}

	tool := Wrap(healthy, failing).(tools.Startable)

	err := tool.Start(t.Context())
	require.ErrorIs(t, err, assert.AnError)
	assert.Equal(t, 1, failing.start, "failing toolset should have attempted start")
	assert.Equal(t, 1, healthy.start, "healthy toolset should have attempted start")
	assert.Equal(t, 1, healthy.stop, "healthy toolset should be rolled back after failure")
}

// TestCodeModeTool_StartStopWrappedToolSet verifies that Start/Stop find
// Startable through a StartableToolSet wrapper via tools.As.
func TestCodeModeTool_StartStopWrappedToolSet(t *testing.T) {
	inner := &testToolSet{}
	wrapped := tools.NewStartable(inner)

	tool := Wrap(wrapped).(tools.Startable)

	err := tool.Start(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, inner.start)

	err = tool.Stop(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, inner.stop)
}

type testToolSet struct {
	tools    []tools.Tool
	start    int
	stop     int
	startErr error
}

// Verify interface compliance
var (
	_ tools.ToolSet   = (*testToolSet)(nil)
	_ tools.Startable = (*testToolSet)(nil)
)

func (t *testToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return t.tools, nil
}

func (t *testToolSet) Start(context.Context) error {
	t.start++
	return t.startErr
}

func (t *testToolSet) Stop(context.Context) error {
	t.stop++
	return nil
}

// TestCodeModeTool_SuccessNoToolCalls verifies that successful execution does not include tool calls.
func TestCodeModeTool_SuccessNoToolCalls(t *testing.T) {
	tool := Wrap(&testToolSet{
		tools: []tools.Tool{
			{
				Name: "get_data",
				Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
					return tools.ResultSuccess("data"), nil
				}),
			},
		},
	})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)

	result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Arguments: `{"script":"return get_data();"}`,
		},
	})
	require.NoError(t, err)

	var scriptResult ScriptResult
	err = json.Unmarshal([]byte(result.Output), &scriptResult)
	require.NoError(t, err)

	// Success case should not include tool calls
	assert.Equal(t, "data", scriptResult.Value)
	assert.Empty(t, scriptResult.ToolCalls, "successful execution should not include tool_calls")
}

// TestCodeModeTool_FailureIncludesToolCalls verifies that failed execution includes tool call history.
func TestCodeModeTool_FailureIncludesToolCalls(t *testing.T) {
	tool := Wrap(&testToolSet{
		tools: []tools.Tool{
			{
				Name: "first_tool",
				Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
					return tools.ResultSuccess("first result"), nil
				}),
			},
			{
				Name: "second_tool",
				Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
					return tools.ResultSuccess("second result"), nil
				}),
			},
		},
	})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)

	// Script calls tools successfully but then throws a runtime error
	result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Arguments: `{"script":"var a = first_tool(); var b = second_tool(); throw new Error('runtime error');"}`,
		},
	})
	require.NoError(t, err)

	var scriptResult ScriptResult
	err = json.Unmarshal([]byte(result.Output), &scriptResult)
	require.NoError(t, err)

	// Failure case should include tool calls
	assert.Contains(t, scriptResult.Value, "runtime error")
	require.Len(t, scriptResult.ToolCalls, 2, "failed execution should include tool_calls")

	// Verify first tool call
	assert.Equal(t, "first_tool", scriptResult.ToolCalls[0].Name)
	assert.Equal(t, "first result", scriptResult.ToolCalls[0].Result)
	assert.Empty(t, scriptResult.ToolCalls[0].Error)

	// Verify second tool call
	assert.Equal(t, "second_tool", scriptResult.ToolCalls[1].Name)
	assert.Equal(t, "second result", scriptResult.ToolCalls[1].Result)
	assert.Empty(t, scriptResult.ToolCalls[1].Error)
}

// TestCodeModeTool_FailureIncludesToolError verifies that tool errors are captured in tool call history.
func TestCodeModeTool_FailureIncludesToolError(t *testing.T) {
	tool := Wrap(&testToolSet{
		tools: []tools.Tool{
			{
				Name: "failing_tool",
				Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
					return nil, assert.AnError
				}),
			},
		},
	})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)

	result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Arguments: `{"script":"return failing_tool();"}`,
		},
	})
	require.NoError(t, err)

	var scriptResult ScriptResult
	err = json.Unmarshal([]byte(result.Output), &scriptResult)
	require.NoError(t, err)

	// Script fails due to tool error
	assert.Contains(t, scriptResult.Value, "assert.AnError")
	require.Len(t, scriptResult.ToolCalls, 1, "failed execution should include tool_calls")

	// Verify the tool call recorded the error
	assert.Equal(t, "failing_tool", scriptResult.ToolCalls[0].Name)
	assert.Empty(t, scriptResult.ToolCalls[0].Result)
	assert.Contains(t, scriptResult.ToolCalls[0].Error, "assert.AnError")
}

// TestCodeModeTool_FailureIncludesToolArguments verifies that tool arguments are captured.
func TestCodeModeTool_FailureIncludesToolArguments(t *testing.T) {
	type TestArgs struct {
		Value string `json:"value" jsonschema:"Test value"`
	}

	tool := Wrap(&testToolSet{
		tools: []tools.Tool{
			{
				Name: "tool_with_args",
				Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
					return tools.ResultSuccess("result"), nil
				}),
				Parameters: tools.MustSchemaFor[TestArgs](),
			},
		},
	})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)

	result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Arguments: `{"script":"tool_with_args({'value': 'test123'}); throw new Error('forced error');"}`,
		},
	})
	require.NoError(t, err)

	var scriptResult ScriptResult
	err = json.Unmarshal([]byte(result.Output), &scriptResult)
	require.NoError(t, err)

	// Verify the tool call captured the arguments
	require.Len(t, scriptResult.ToolCalls, 1)
	assert.Equal(t, "tool_with_args", scriptResult.ToolCalls[0].Name)
	assert.Equal(t, map[string]any{"value": "test123"}, scriptResult.ToolCalls[0].Arguments)
	assert.Equal(t, "result", scriptResult.ToolCalls[0].Result)
}
