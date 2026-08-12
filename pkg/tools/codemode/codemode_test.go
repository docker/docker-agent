package codemode

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestCodeModeTool_Tools(t *testing.T) {
	t.Parallel()
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

func TestCodeModeTool_TypeScriptDeclarationsInDescription(t *testing.T) {
	t.Parallel()

	tool := Wrap(&testToolSet{tools: []tools.Tool{{
		Name:         "find_item",
		Description:  "Find an item",
		Parameters:   map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}, "required": []any{"id"}},
		OutputSchema: map[string]any{"type": "boolean"},
	}}})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)
	assert.Contains(t, allTools[0].Description, "interface FindItemInput")
	assert.Contains(t, allTools[0].Description, "id: string;")
	assert.Contains(t, allTools[0].Description, "type FindItemOutput = boolean;")
	assert.Contains(t, allTools[0].Description, "declare function FindItem(args: FindItemInput): FindItemOutput;")
	assert.NotContains(t, allTools[0].Description, "Where Input follows the following JSON schema")
}

func TestCodeModeTool_Instructions(t *testing.T) {
	t.Parallel()
	tool := &codeModeTool{}

	instructions := tools.GetInstructions(tool)

	assert.Empty(t, instructions)
}

func TestCodeModeTool_StartStop(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
			Arguments: `{"script":"return HelloWorld();"}`,
		},
	}, tools.NopRuntime{})
	require.NoError(t, err)

	var scriptResult ScriptResult
	err = json.Unmarshal([]byte(result.Output), &scriptResult)
	require.NoError(t, err)

	require.Equal(t, "Hello, World!", scriptResult.Value)
	require.Empty(t, scriptResult.StdErr)
	require.Empty(t, scriptResult.StdOut)
}

func TestCodeModeTool_CallToolWithNonIdentifierName(t *testing.T) {
	t.Parallel()
	tool := Wrap(&testToolSet{
		tools: []tools.Tool{
			{
				Name: "hello-world",
				Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
					return tools.ResultSuccess("Hello, World!"), nil
				}),
			},
		},
	})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)
	assert.Contains(t, allTools[0].Description, "declare function HelloWorld(args: HelloWorldInput): HelloWorldOutput;")

	result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Arguments: `{"script":"return HelloWorld();"}`,
		},
	}, tools.NopRuntime{})
	require.NoError(t, err)

	var scriptResult ScriptResult
	err = json.Unmarshal([]byte(result.Output), &scriptResult)
	require.NoError(t, err)

	require.Equal(t, "Hello, World!", scriptResult.Value)
}

func TestCodeModeTool_CallEcho(t *testing.T) {
	t.Parallel()
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
	}, tools.NopRuntime{})
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
	t.Parallel()
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
	t.Parallel()
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

// capableToolSet is a testToolSet that also implements the capability
// interfaces codeModeTool must forward (Elicitable, Sampleable,
// SampleableWithTools, OAuthCapable, ChangeNotifier) so tests can assert
// that codeModeTool actually forwards to inner toolsets instead of
// silently dropping them (the regression this file guards against: an
// MCP toolset wrapped by code_mode_tools never got its OAuth elicitation
// handler wired up, so its authorization dialog never surfaced).
type capableToolSet struct {
	testToolSet

	elicitationHandler        tools.ElicitationHandler
	samplingHandler           tools.SamplingHandler
	samplingWithToolsHandler  tools.SamplingWithToolsHandler
	oauthSuccessHandler       func()
	managedOAuth              bool
	unmanagedOAuthRedirectURI string
	toolsChangedHandler       func()
}

var (
	_ tools.Elicitable          = (*capableToolSet)(nil)
	_ tools.Sampleable          = (*capableToolSet)(nil)
	_ tools.SampleableWithTools = (*capableToolSet)(nil)
	_ tools.OAuthCapable        = (*capableToolSet)(nil)
	_ tools.ChangeNotifier      = (*capableToolSet)(nil)
)

func (c *capableToolSet) SetElicitationHandler(handler tools.ElicitationHandler) {
	c.elicitationHandler = handler
}

func (c *capableToolSet) SetSamplingHandler(handler tools.SamplingHandler) {
	c.samplingHandler = handler
}

func (c *capableToolSet) SetSamplingWithToolsHandler(handler tools.SamplingWithToolsHandler) {
	c.samplingWithToolsHandler = handler
}

func (c *capableToolSet) SetOAuthSuccessHandler(handler func()) {
	c.oauthSuccessHandler = handler
}

func (c *capableToolSet) SetManagedOAuth(managed bool) {
	c.managedOAuth = managed
}

func (c *capableToolSet) SetUnmanagedOAuthRedirectURI(uri string) {
	c.unmanagedOAuthRedirectURI = uri
}

func (c *capableToolSet) SetToolsChangedHandler(handler func()) {
	c.toolsChangedHandler = handler
}

// TestCodeModeTool_ForwardsCapabilityHandlers verifies that codeModeTool
// forwards elicitation, sampling, OAuth, and tool-list-changed handlers to
// every inner toolset that supports them. Before this fix, codeModeTool
// implemented none of these capability interfaces, so
// tools.ConfigureHandlers (called by the runtime once per turn) could never
// reach an MCP toolset hidden behind code_mode_tools — its OAuth
// elicitation handler stayed nil forever and the authorization dialog
// never surfaced.
func TestCodeModeTool_ForwardsCapabilityHandlers(t *testing.T) {
	t.Parallel()
	capable := &capableToolSet{}
	// A plain toolset without any capability must be tolerated (As returns
	// ok=false) rather than panicking.
	plain := &testToolSet{}

	tool := Wrap(capable, plain)

	elicitHandler := func(context.Context, *mcp.ElicitParams) (tools.ElicitationResult, error) {
		return tools.ElicitationResult{}, nil
	}
	samplingHandler := func(context.Context, *mcp.CreateMessageParams) (*mcp.CreateMessageResult, error) {
		return nil, nil
	}
	samplingWithToolsHandler := func(context.Context, *mcp.CreateMessageWithToolsParams) (*mcp.CreateMessageWithToolsResult, error) {
		return nil, nil
	}
	oauthCalled := false

	// tools.ConfigureHandlers is the exact call the runtime makes once per
	// turn (see configureToolsetHandlers in pkg/runtime/loop.go); routing
	// through it here instead of casting to each capability interface
	// directly keeps this test aligned with the real call site.
	tools.ConfigureHandlers(tool, elicitHandler, samplingHandler, samplingWithToolsHandler,
		func() { oauthCalled = true }, true, "http://127.0.0.1:1234/callback")

	require.NotNil(t, capable.elicitationHandler)
	require.NotNil(t, capable.samplingHandler)
	require.NotNil(t, capable.samplingWithToolsHandler)
	require.NotNil(t, capable.oauthSuccessHandler)
	capable.oauthSuccessHandler()
	assert.True(t, oauthCalled)
	assert.True(t, capable.managedOAuth)
	assert.Equal(t, "http://127.0.0.1:1234/callback", capable.unmanagedOAuthRedirectURI)

	changedCalled := false
	tool.(tools.ChangeNotifier).SetToolsChangedHandler(func() { changedCalled = true })
	require.NotNil(t, capable.toolsChangedHandler)
	capable.toolsChangedHandler()
	assert.True(t, changedCalled)
}

// TestCodeModeTool_ForwardsCapabilityHandlersThroughStartableWrapper verifies
// that the capability forwarding also finds an inner toolset wrapped in a
// tools.StartableToolSet, matching how real MCP toolsets are wired
// (tools.NewStartable(mcpToolset)) before being handed to codemode.Wrap.
func TestCodeModeTool_ForwardsCapabilityHandlersThroughStartableWrapper(t *testing.T) {
	t.Parallel()
	capable := &capableToolSet{}
	wrapped := tools.NewStartable(capable)

	tool := Wrap(wrapped)

	handler := func(context.Context, *mcp.ElicitParams) (tools.ElicitationResult, error) {
		return tools.ElicitationResult{}, nil
	}
	tool.(tools.Elicitable).SetElicitationHandler(handler)

	assert.NotNil(t, capable.elicitationHandler)
}

// TestCodeModeTool_SuccessNoToolCalls verifies that successful execution does not include tool calls.
func TestCodeModeTool_SuccessNoToolCalls(t *testing.T) {
	t.Parallel()
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
	}, tools.NopRuntime{})
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
	t.Parallel()
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
	}, tools.NopRuntime{})
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
	t.Parallel()
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
	}, tools.NopRuntime{})
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
	t.Parallel()
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
	}, tools.NopRuntime{})
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
