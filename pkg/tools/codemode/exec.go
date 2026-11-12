package codemode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"github.com/dop251/goja"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/telemetry/genai"
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

type toolCompletion struct {
	index  int
	info   ToolCallInfo
	settle func() error
}

type toolEventLoop struct {
	vm          *goja.Runtime
	tracker     *toolCallTracker
	completions chan toolCompletion
	pending     int
}

func (c *codeModeTool) runJavascript(ctx context.Context, rt tools.Runtime, script string) (ScriptResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	vm := goja.New()
	stopInterrupt := context.AfterFunc(ctx, func() { vm.Interrupt(ctx.Err()) })
	defer stopInterrupt()
	tracker := &toolCallTracker{}
	loop := &toolEventLoop{vm: vm, tracker: tracker, completions: make(chan toolCompletion)}
	unhandled := make(map[*goja.Promise]bool)
	vm.SetPromiseRejectionTracker(func(p *goja.Promise, operation goja.PromiseRejectionOperation) {
		if operation == goja.PromiseRejectionReject {
			unhandled[p] = true
		} else {
			delete(unhandled, p)
		}
	})

	// Always stamp a hash + length so dashboards can correlate
	// identical scripts ("model ran the same script 200 times this
	// hour") without ever shipping the body. Codemode scripts are
	// kilobyte-scale arbitrary JS — embedded auth tokens, pasted
	// user data, and inline secrets are common — so the body itself
	// is gated behind the GenAI content-capture opt-in.
	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		sum := sha256.Sum256([]byte(script))
		span.SetAttributes(
			attribute.String("cagent.tool.codemode.script_hash", hex.EncodeToString(sum[:])),
			attribute.Int("cagent.tool.codemode.script_length", len(script)),
		)
		if genai.IsContentCaptureEnabled() {
			span.SetAttributes(attribute.String("cagent.tool.codemode.script", script))
		}
	}
	defer func() {
		if span.IsRecording() {
			span.SetAttributes(attribute.Int("cagent.tool.codemode.tool_call_count", len(tracker.calls)))
		}
	}()

	// Inject console object to the help the LLM debug its own code.
	var (
		stdOut bytes.Buffer
		stdErr bytes.Buffer
	)
	_ = vm.Set("console", console(&stdOut, &stdErr))

	// Inject every available tool as a javascript function. Toolsets whose
	// start failed or that died since are omitted, matching the declarations
	// listed by Tools().
	for _, toolset := range c.availableToolsets() {
		allTools, err := toolset.Tools(ctx)
		if err != nil {
			return ScriptResult{}, err
		}

		for _, tool := range allTools {
			if tool.AllowRepeatedCalls {
				continue
			}
			call := loop.callTool(ctx, rt, tool)
			_ = vm.Set(tool.Name, call)
			if name := typeName(tool.Name); name != tool.Name {
				_ = vm.Set(name, call)
			}
		}
	}

	// Wrap the script to support top-level await and return.
	script = "(async () => {\n" + script + "\n})()"

	// Run the script.
	v, err := vm.RunString(script)
	if err == nil {
		promise := v.Export().(*goja.Promise)
		for loop.pending > 0 && err == nil {
			select {
			case completion := <-loop.completions:
				loop.pending--
				tracker.calls[completion.index] = completion.info
				err = completion.settle()
			case <-ctx.Done():
				err = ctx.Err()
			}
		}
		if err == nil {
			switch promise.State() {
			case goja.PromiseStateFulfilled:
				v = promise.Result()
			case goja.PromiseStateRejected:
				err = fmt.Errorf("%s", promise.Result().String())
			case goja.PromiseStatePending:
				err = errors.New("script returned a Promise that cannot settle: no pending tool calls")
			}
		}
		if err == nil {
			for p := range unhandled {
				err = fmt.Errorf("unhandled Promise rejection: %s", p.Result().String())
				break
			}
		}
	}
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

// Tool handlers run concurrently, but only the event loop touches the VM and tracker.
func (l *toolEventLoop) callTool(ctx context.Context, rt tools.Runtime, tool tools.Tool) func(args map[string]any) *goja.Promise {
	return func(args map[string]any) *goja.Promise {
		promise, resolve, reject := l.vm.NewPromise()
		index := len(l.tracker.calls)
		l.tracker.calls = append(l.tracker.calls, ToolCallInfo{Name: tool.Name, Arguments: args})
		l.pending++

		go func() {
			output, filtered, err := invokeTool(ctx, rt, tool, args)
			info := ToolCallInfo{Name: tool.Name, Arguments: filtered, Result: output}
			settle := func() error { return resolve(output) }
			if err != nil {
				info.Error = err.Error()
				settle = func() error { return reject(l.vm.NewGoError(err)) }
			}
			select {
			case l.completions <- toolCompletion{index: index, info: info, settle: settle}:
			case <-ctx.Done():
			}
		}()

		return promise
	}
}

// invokeTool calls a single tool handler, filtering out nil optional arguments.
// It returns the output, the filtered arguments actually sent, and any error.
func invokeTool(ctx context.Context, rt tools.Runtime, tool tools.Tool, args map[string]any) (string, map[string]any, error) {
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

	toolCall := tools.ToolCall{
		ID:   "codemode_" + uuid.NewV4().String(),
		Type: "function",
		Function: tools.FunctionCall{
			Name:      tool.Name,
			Arguments: string(arguments),
		},
	}
	var result *tools.ToolCallResult
	if runner, ok := rt.(tools.ToolRunner); ok {
		result, err = runner.RunTool(ctx, toolCall, tool)
	} else {
		result, err = tool.Handler(ctx, toolCall, rt)
	}
	if err != nil {
		return "", filtered, err
	}

	if result == nil {
		return "", filtered, nil
	}
	return result.Output, filtered, nil
}
