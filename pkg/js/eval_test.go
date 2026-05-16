package js

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestEvaluate(t *testing.T) {
	t.Parallel()

	mockTools := []tools.Tool{
		{
			Name: "echo",
			Handler: func(_ context.Context, tc tools.ToolCall) (*tools.ToolCallResult, error) {
				return tools.ResultSuccess("echoed: " + tc.Function.Arguments), nil
			},
		},
		{
			Name: "shell",
			Handler: func(_ context.Context, tc tools.ToolCall) (*tools.ToolCallResult, error) {
				return tools.ResultSuccess("output: " + tc.Function.Arguments), nil
			},
		},
	}

	tests := []struct {
		name     string
		input    string
		tools    []tools.Tool
		expected string
	}{
		{
			name:     "simple string literal",
			input:    `${"hello"}`,
			tools:    nil,
			expected: "hello",
		},
		{
			name:     "simple number",
			input:    `${42}`,
			tools:    nil,
			expected: "42",
		},
		{
			name:     "arithmetic",
			input:    `${1 + 2}`,
			tools:    nil,
			expected: "3",
		},
		{
			name:     "tool call",
			input:    `${echo({msg: "test"})}`,
			tools:    mockTools,
			expected: `echoed: {"msg":"test"}`,
		},
		{
			name:     "tool call with string concatenation",
			input:    `${"result: " + echo({msg: "test"})}`,
			tools:    mockTools,
			expected: `result: echoed: {"msg":"test"}`,
		},
		{
			name:     "invalid syntax",
			input:    `${{invalid}`,
			tools:    nil,
			expected: "${{invalid}",
		},
		{
			name:     "no expressions",
			input:    "Just plain text",
			tools:    mockTools,
			expected: "Just plain text",
		},
		{
			name:     "expression with arguments",
			input:    `Status: ${shell({cmd: "git status"})}`,
			tools:    mockTools,
			expected: `Status: output: {"cmd":"git status"}`,
		},
		{
			name:     "multiple expressions",
			input:    `${echo({val:1})}, ${echo({val:2})}`,
			tools:    mockTools,
			expected: `echoed: {"val":1}, echoed: {"val":2}`,
		},
		{
			name:     "expression with text around",
			input:    `Before ${echo({msg:"hi"})} after`,
			tools:    mockTools,
			expected: `Before echoed: {"msg":"hi"} after`,
		},
		{
			name:     "unclosed brace is ignored",
			input:    "Unclosed ${foo",
			tools:    nil,
			expected: "Unclosed ${foo",
		},
		{
			name:     "mixed known and unknown expressions",
			input:    `Known: ${echo({msg: "hi"})}, Unknown: ${undefined_tool()}`,
			tools:    mockTools,
			expected: `Known: echoed: {"msg":"hi"}, Unknown: ${undefined_tool()}`,
		},
		{
			name:     "unknown expression preserves shell calls",
			input:    "Lint: ${shell({cmd: \"task lint\"})}\n${unknown_tool()}",
			tools:    mockTools,
			expected: "Lint: output: {\"cmd\":\"task lint\"}\n${unknown_tool()}",
		},
		{
			name:     "nested braces in tool args",
			input:    `${shell({cmd: "echo }"})}`,
			tools:    mockTools,
			expected: `output: {"cmd":"echo }"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			evaluator := NewEvaluator(tt.tools)
			result := evaluator.Evaluate(t.Context(), tt.input, nil)
			assert.Equal(t, tt.expected, result)
		})
	}
}
