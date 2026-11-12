package codemode

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestRunJavascript(t *testing.T) {
	t.Parallel()
	tool := &codeModeTool{}

	result, err := tool.runJavascript(t.Context(), tools.NopRuntime{}, `return "HELLO"`)
	require.NoError(t, err)

	assert.Equal(t, "HELLO", result.Value)
	assert.Empty(t, result.StdOut)
	assert.Empty(t, result.StdErr)
}

func TestRunJavascript_error(t *testing.T) {
	t.Parallel()
	tool := &codeModeTool{}

	result, err := tool.runJavascript(t.Context(), tools.NopRuntime{}, `==`)
	require.NoError(t, err)

	assert.Equal(t, "SyntaxError: SyntaxError: (anonymous): Line 2:1 Unexpected token == (and 2 more errors)", result.Value)
	assert.Empty(t, result.StdOut)
	assert.Empty(t, result.StdErr)
}

func TestRunJavascript_console(t *testing.T) {
	t.Parallel()
	tool := &codeModeTool{}

	result, err := tool.runJavascript(t.Context(), tools.NopRuntime{}, `console.log("to stdout"); console.error("to stderr"); return "RESULT";`)
	require.NoError(t, err)

	assert.Equal(t, "RESULT", result.Value)
	assert.Equal(t, "to stdout\n", result.StdOut)
	assert.Equal(t, "to stderr\n", result.StdErr)
}

func TestRunJavascript_no_result(t *testing.T) {
	t.Parallel()
	tool := &codeModeTool{}

	result, err := tool.runJavascript(t.Context(), tools.NopRuntime{}, ``)
	require.NoError(t, err)

	assert.Empty(t, result.Value)
	assert.Empty(t, result.StdOut)
	assert.Empty(t, result.StdErr)
}

func TestRunJavascript_ParallelTools(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	firstStarted := make(chan struct{})
	secondFinished := make(chan struct{})
	tool := Wrap(&testToolSet{tools: []tools.Tool{
		{Name: "first", Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
			close(firstStarted)
			select {
			case <-secondFinished:
				return tools.ResultSuccess("first"), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})},
		{Name: "second", Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
			select {
			case <-firstStarted:
				close(secondFinished)
				return tools.ResultSuccess("second"), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})},
	}}).(*codeModeTool)
	result, err := tool.runJavascript(ctx, tools.NopRuntime{}, `
 const a = first();
 const b = Second();
 if (!(a instanceof Promise) || !(b instanceof Promise)) throw new Error("not Promises");
 const results = await Promise.all([a, b]);
 console.log(results.join(","));
 throw new Error("forced failure");
 `)
	require.NoError(t, err)
	assert.Contains(t, result.Value, "forced failure")
	assert.Equal(t, "first,second\n", result.StdOut)
	require.Len(t, result.ToolCalls, 2)
	assert.Equal(t, "first", result.ToolCalls[0].Name)
	assert.Equal(t, "first", result.ToolCalls[0].Result)
	assert.Equal(t, "second", result.ToolCalls[1].Name)
	assert.Equal(t, "second", result.ToolCalls[1].Result)
}

func TestRunJavascript_Promises(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, script, want string
		failed             bool
	}{
		{name: "sequential awaits", script: `const a = await Echo({message: "hello"}); return await echo({message: a + " world"});`, want: "hello world"},
		{name: "then callback", script: `return echo({message: "hello"}).then(value => value + " world");`, want: "hello world"},
		{name: "caught rejection", script: `try { await fail(); } catch (e) { return "caught: " + e.message; }`, want: "caught: assert.AnError"},
		{name: "all settled", script: `const results = await Promise.allSettled([fail(), echo({message: "ok"})]); return results.map(r => r.status).join(",");`, want: "rejected,fulfilled"},
		{name: "all rejection", script: `return await Promise.all([fail(), echo({message: "ok"})]);`, want: "assert.AnError", failed: true},
		{name: "unhandled rejection", script: `fail(); return "ignored";`, want: "unhandled Promise rejection", failed: true},
		{name: "unavailable handler", script: `return await unavailable();`, want: `tool "unavailable" is not available in code mode`, failed: true},
		{name: "unsettleable promise", script: `return new Promise(() => {});`, want: "no pending tool calls", failed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tool := Wrap(&testToolSet{tools: []tools.Tool{
				{Name: "echo", Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
					return tools.ResultSuccess(args["message"].(string)), nil
				})},
				{Name: "fail", Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
					return nil, assert.AnError
				})},
				{Name: "unavailable"},
			}}).(*codeModeTool)
			result, err := tool.runJavascript(t.Context(), tools.NopRuntime{}, tt.script)
			require.NoError(t, err)
			assert.Contains(t, result.Value, tt.want)
			if !tt.failed {
				assert.Empty(t, result.ToolCalls)
			}
			if tt.name == "all rejection" {
				require.Len(t, result.ToolCalls, 2)
				assert.Equal(t, "ok", result.ToolCalls[1].Result)
			}
		})
	}
}

func TestRunJavascript_Cancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan struct{})
	tool := Wrap(&testToolSet{tools: []tools.Tool{{Name: "wait", Handler: tools.NewHandler(func(ctx context.Context, args map[string]any) (*tools.ToolCallResult, error) {
		cancel()
		<-ctx.Done()
		close(finished)
		return nil, ctx.Err()
	})}}}).(*codeModeTool)
	result, err := tool.runJavascript(ctx, tools.NopRuntime{}, `return await wait();`)
	require.NoError(t, err)
	assert.Contains(t, result.Value, "context canceled")
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("tool handler did not stop")
	}
}
