package codemode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/dop251/goja"

	"github.com/docker/docker-agent/pkg/tools"
)

type ScriptResult struct {
	Value     string         `json:"value" jsonschema:"The value returned by the script"`
	StdOut    string         `json:"stdout" jsonschema:"The standard output of the console"`
	StdErr    string         `json:"stderr" jsonschema:"The standard error of the console"`
	ToolCalls []ToolCallInfo `json:"tool_calls,omitempty" jsonschema:"The list of tool calls made during script execution, only included on failure"`
}

// ToolCallInfo contains information about a tool call made during script execution.
type ToolCallInfo struct {
	Name      string `json:"name" jsonschema:"The name of the tool that was called"`
	Arguments any    `json:"arguments" jsonschema:"The arguments passed to the tool"`
	Result    string `json:"result,omitempty" jsonschema:"The raw response returned by the tool"`
	Error     string `json:"error,omitempty" jsonschema:"The error message, if the tool call failed"`
}

// toolCallTracker tracks tool calls made during script execution.
type toolCallTracker struct {
	calls []ToolCallInfo
}

func (t *toolCallTracker) record(info ToolCallInfo) {
	t.calls = append(t.calls, info)
}

func (c *codeModeTool) runJavascript(ctx context.Context, script string) (ScriptResult, error) {
	vm := goja.New()
	tracker := &toolCallTracker{}

	// Inject console object to the help the LLM debug its own code.
	var (
		stdOut bytes.Buffer
		stdErr bytes.Buffer
	)
	_ = vm.Set("console", console(&stdOut, &stdErr))

	// Inject every tool as a javascript function.
	for _, toolset := range c.toolsets {
		allTools, err := toolset.Tools(ctx)
		if err != nil {
			return ScriptResult{}, err
		}

		for _, tool := range allTools {
			_ = vm.Set(tool.Name, callTool(ctx, tool, tracker))
		}
	}

	// Wrap the user script in an IIFE to allow top-level returns.
	script = "(() => {\n" + script + "\n})()"

	// Run the script.
	v, err := vm.RunString(script)
	if err != nil {
		// Script execution failed - include tool call history to help LLM understand what went wrong
		return ScriptResult{
			StdOut:    stdOut.String(),
			StdErr:    stdErr.String(),
			Value:     err.Error(),
			ToolCalls: tracker.calls,
		}, nil
	}

	value := ""
	if result := v.Export(); result != nil {
		value = fmt.Sprintf("%v", result)
	}

	// Success case - don't include tool calls to avoid unnecessary overhead
	return ScriptResult{
		StdOut: stdOut.String(),
		StdErr: stdErr.String(),
		Value:  value,
	}, nil
}

func callTool(ctx context.Context, tool tools.Tool, tracker *toolCallTracker) func(args map[string]any) (string, error) {
	return func(args map[string]any) (string, error) {
		output, filtered, err := invokeTool(ctx, tool, args)

		info := ToolCallInfo{
			Name:      tool.Name,
			Arguments: filtered,
		}
		if err != nil {
			info.Error = err.Error()
		} else {
			info.Result = output
		}
		tracker.record(info)

		return output, err
	}
}

// invokeTool calls a single tool handler, filtering out nil optional arguments.
// It returns the output, the filtered arguments actually sent, and any error.
func invokeTool(ctx context.Context, tool tools.Tool, args map[string]any) (string, map[string]any, error) {
	if tool.Handler == nil {
		return "", args, fmt.Errorf("tool %q is not available in code mode", tool.Name)
	}

	var schema struct {
		Required []string `json:"required"`
	}
	if err := tools.ConvertSchema(tool.Parameters, &schema); err != nil {
		return "", args, err
	}

	// Strip nil optional arguments that goja passes for omitted parameters.
	filtered := make(map[string]any)
	for k, v := range args {
		if slices.Contains(schema.Required, k) || v != nil {
			filtered[k] = v
		}
	}

	arguments, err := json.Marshal(filtered)
	if err != nil {
		return "", filtered, err
	}

	result, err := tool.Handler(ctx, tools.ToolCall{
		Function: tools.FunctionCall{
			Name:      tool.Name,
			Arguments: string(arguments),
		},
	})
	if err != nil {
		return "", filtered, err
	}

	if result == nil {
		return "", filtered, nil
	}
	return result.Output, filtered, nil
}
