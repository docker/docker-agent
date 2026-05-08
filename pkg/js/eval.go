package js

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/tools"
)

// Evaluator handles JavaScript expression evaluation in strings.
type Evaluator struct {
	tools []tools.Tool
}

// NewEvaluator creates a new Evaluator with the given tools.
func NewEvaluator(agentTools []tools.Tool) *Evaluator {
	return &Evaluator{
		tools: agentTools,
	}
}

// Evaluate finds and evaluates ${...} JavaScript expressions in the input string.
// args are available as the 'args' array in JavaScript.
func (e *Evaluator) Evaluate(ctx context.Context, input string, args []string) string {
	if !strings.Contains(input, "${") {
		return input
	}

	vm := newVM()
	if args == nil {
		args = []string{}
	}
	_ = vm.Set("args", args)

	// Bind tools to VM
	for _, tool := range e.tools {
		_ = vm.Set(tool.Name, e.createToolCaller(ctx, tool))
	}

	slog.Debug("Evaluating JS template", "input", input)

	return runExpansion(vm, input)
}

// createToolCaller creates a JavaScript function that calls the given tool.
func (e *Evaluator) createToolCaller(ctx context.Context, tool tools.Tool) func(args map[string]any) (string, error) {
	return func(args map[string]any) (string, error) {
		var toolArgs struct {
			Required []string `json:"required"`
		}

		if err := tools.ConvertSchema(tool.Parameters, &toolArgs); err != nil {
			return "", err
		}

		// Filter out nil values for non-required arguments
		nonNilArgs := make(map[string]any)
		for k, v := range args {
			if slices.Contains(toolArgs.Required, k) || v != nil {
				nonNilArgs[k] = v
			}
		}

		arguments, err := json.Marshal(nonNilArgs)
		if err != nil {
			return "", err
		}

		toolCall := tools.ToolCall{
			ID:   "jseval_" + tool.Name,
			Type: "function",
			Function: tools.FunctionCall{
				Name:      tool.Name,
				Arguments: string(arguments),
			},
		}

		if tool.Handler == nil {
			return "", fmt.Errorf("tool '%s' has no handler", tool.Name)
		}

		// Use the parent context directly, relying on its cancellation/timeout
		result, err := tool.Handler(ctx, toolCall)
		if err != nil {
			return "", err
		}

		return result.Output, nil
	}
}
