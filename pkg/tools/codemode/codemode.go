package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/tools"
)

const prompt = `Run a Javascript script to call MCP tools.

Instead of calling individual MCP tools directly, use this to run a Javascript script that calls as many tools as needed.
This allows you to combine multiple MCP tool calls in a single request, perform conditional logic,
and manipulate the results before returning them.

Instructions:
 - The script has access to the tools/functions listed below as plain javascript functions.
 - Every tool call returns a Promise. Use "await" to get its result; top-level "await" is supported.
 - Run independent tool calls in parallel with "await Promise.all([ToolA(args), ToolB(args)])".
 - Await dependent tool calls sequentially. Tool failures reject their Promises and can be caught with try/catch.
 - The script must return a string result.
 - "console.*" functions can be used to print debug information.
 - It's often encouraged to group multiple tool calls in a single script to reduce the number of LLM interactions.
   And it allows to do conditional logic based on tool calls.

Available tools/functions:

`

func Wrap(toolsets ...tools.ToolSet) tools.ToolSet {
	managed := make([]*tools.StartableToolSet, len(toolsets))
	for i, toolset := range toolsets {
		managed[i] = tools.NewStartable(toolset)
	}
	return &codeModeTool{managed: managed}
}

type codeModeTool struct {
	managed []*tools.StartableToolSet

	// lifecycleMu serializes composite Start and Stop calls. Each child wrapper
	// remains the sole owner of its child's lifecycle state.
	lifecycleMu sync.Mutex
}

// Verify interface compliance
var (
	_ tools.ToolSet       = (*codeModeTool)(nil)
	_ tools.Composite     = (*codeModeTool)(nil)
	_ tools.Startable     = (*codeModeTool)(nil)
	_ tools.StartReporter = (*codeModeTool)(nil)
	_ tools.Named         = (*codeModeTool)(nil)
)

// Name implements tools.Named; loader-created, so no registry WithName wrapper.
func (c *codeModeTool) Name() string {
	return "code_mode"
}

// Children exposes the canonical lifecycle owners for graph traversal.
func (c *codeModeTool) Children() []tools.ToolSet {
	var children []tools.ToolSet
	for _, managed := range c.managed {
		children = append(children, managed)
	}
	return children
}

type RunToolsWithJavascriptArgs struct {
	Script string `json:"script" jsonschema:"Script to execute"`
}

func isExcludedTool(tool tools.Tool) bool {
	// Tools executed by a host-owned RuntimeHandler (transfer_task, handoff,
	// run_skill, the background-agent tools, ...) have no in-process Handler,
	// so a call from the script would fail with "not available in code mode".
	// Keep them direct so the runtime dispatches them as before.
	if tool.RuntimeHandler != "" {
		return true
	}
	// Repeatable calls need their own dispatch results for loop detection.
	return tool.Category == "todo" || tool.AllowRepeatedCalls
}

// availableToolsets returns children whose canonical lifecycle wrapper says
// they can currently contribute tools.
func (c *codeModeTool) availableToolsets() []*tools.StartableToolSet {
	var available []*tools.StartableToolSet
	for _, managed := range c.managed {
		if managed.TryIsAvailable() {
			available = append(available, managed)
		}
	}
	return available
}

func (c *codeModeTool) Tools(ctx context.Context) ([]tools.Tool, error) {
	var (
		functionsDoc  []string
		excludedTools []tools.Tool
	)

	for _, toolset := range c.availableToolsets() {
		allTools, err := toolset.Tools(ctx)
		if err != nil {
			return nil, err
		}

		for _, tool := range allTools {
			if isExcludedTool(tool) {
				excludedTools = append(excludedTools, tool)
			} else {
				functionsDoc = append(functionsDoc, toolToTypeScript(tool))
			}
		}
	}

	allTools := []tools.Tool{{
		Name:        "run_tools_with_javascript",
		Category:    "code mode",
		Description: prompt + strings.Join(functionsDoc, "\n"),
		Parameters:  tools.MustSchemaFor[RunToolsWithJavascriptArgs](),
		Handler: tools.NewRuntimeHandler(func(ctx context.Context, args RunToolsWithJavascriptArgs, rt tools.Runtime) (*tools.ToolCallResult, error) {
			result, err := c.runJavascript(ctx, rt, args.Script)
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

// Start brings every child up through its canonical lifecycle wrapper. A
// failing child does not stop healthy peers; partial and total failures retain
// the composite's existing degraded-mode contract.
func (c *codeModeTool) Start(ctx context.Context) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()

	var (
		errs []error
		lost bool
	)
	for _, managed := range c.managed {
		wasStarted := managed.TryIsStarted()
		if err := managed.Start(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", tools.DescribeToolSet(managed.ToolSet), err))
			if wasStarted || managed.ShouldReportRecoveryFailure() {
				lost = true
			}
		}
	}
	switch {
	case len(errs) == 0:
		return nil
	case len(errs) == len(c.managed):
		return tools.NewTotalStartError(errs...)
	default:
		err := tools.NewPartialStartError(errs...)
		err.LostAfterStart = lost
		return err
	}
}

// IsStarted reports whether every child wrapper is healthy.
func (c *codeModeTool) IsStarted() bool {
	for _, managed := range c.managed {
		if !managed.TryIsHealthy() {
			return false
		}
	}
	return true
}

func (c *codeModeTool) Stop(ctx context.Context) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()

	var errs []error
	for _, managed := range c.managed {
		if err := managed.Stop(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
