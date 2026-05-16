package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/tools"
)

const prompt = `Run a Javascript script to call MCP tools.

Instead of calling individual MCP tools directly, use this to run a Javascript script that calls as many tools as needed.
This allows you to combine multiple MCP tool calls in a single request, perform conditional logic,
and manipulate the results before returning them.

Instructions:
 - The script has access to all the tools as plain javascript functions.
 - "await"/"async" are never needed. All the tool calls are synchronous.
 - The script must return a string result.
 - "console.*" functions can be used to print debug information.
 - It's often encouraged to group multiple tool calls in a single script to reduce the number of LLM interactions.
   And it allows to do conditional logic based on tool calls.

Available tools/functions:

`

func Wrap(toolsets ...tools.ToolSet) tools.ToolSet {
	return &codeModeTool{
		toolsets: toolsets,
	}
}

type codeModeTool struct {
	toolsets []tools.ToolSet
}

// Verify interface compliance
var (
	_ tools.ToolSet   = (*codeModeTool)(nil)
	_ tools.Startable = (*codeModeTool)(nil)
)

type RunToolsWithJavascriptArgs struct {
	Script string `json:"script" jsonschema:"Script to execute"`
}

func isExcludedTool(tool tools.Tool) bool {
	return tool.Category == "todo"
}

func (c *codeModeTool) Tools(ctx context.Context) ([]tools.Tool, error) {
	var (
		functionsDoc  []string
		excludedTools []tools.Tool
	)

	for _, toolset := range c.toolsets {
		allTools, err := toolset.Tools(ctx)
		if err != nil {
			return nil, err
		}

		for _, tool := range allTools {
			if isExcludedTool(tool) {
				excludedTools = append(excludedTools, tool)
			} else {
				functionsDoc = append(functionsDoc, toolToJsDoc(tool))
			}
		}
	}

	allTools := []tools.Tool{{
		Name:        "run_tools_with_javascript",
		Category:    "code mode",
		Description: prompt + strings.Join(functionsDoc, "\n"),
		Parameters:  tools.MustSchemaFor[RunToolsWithJavascriptArgs](),
		Handler: tools.NewHandler(func(ctx context.Context, args RunToolsWithJavascriptArgs) (*tools.ToolCallResult, error) {
			result, err := c.runJavascript(ctx, args.Script)
			if err != nil {
				return nil, err
			}

			buf, err := json.Marshal(result)
			if err != nil {
				return nil, fmt.Errorf("marshaling script's result: %w", err)
			}

			return tools.ResultSuccess(string(buf)), nil
		}),
		OutputSchema: tools.MustSchemaFor[ScriptResult](),
		Annotations: tools.ToolAnnotations{
			Title: "Run tools with Javascript",
		},
	}}

	allTools = append(allTools, excludedTools...)

	return allTools, nil
}

func (c *codeModeTool) Start(ctx context.Context) error {
	var started []tools.Startable
	var errs []error
	for _, t := range c.toolsets {
		if s, ok := tools.As[tools.Startable](t); ok {
			if err := s.Start(ctx); err != nil {
				errs = append(errs, err)
			} else {
				started = append(started, s)
			}
		}
	}
	if len(errs) > 0 {
		// Roll back successfully-started toolsets so we don't leave
		// the system in a partially-started state.
		for _, s := range started {
			errs = append(errs, s.Stop(ctx))
		}
		return errors.Join(errs...)
	}
	return nil
}

func (c *codeModeTool) Stop(ctx context.Context) error {
	var errs []error
	for _, t := range c.toolsets {
		if s, ok := tools.As[tools.Startable](t); ok {
			if err := s.Stop(ctx); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
