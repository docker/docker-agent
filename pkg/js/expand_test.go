package js

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestExpand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		commands string
		envVars  map[string]string
		expected string
	}{
		{
			name:     "no placeholder",
			commands: "List all files",
			envVars:  map[string]string{},
			expected: "List all files",
		},
		{
			name:     "single placeholder",
			commands: "Say hello to ${env.USER}",
			envVars:  map[string]string{"USER": "alice"},
			expected: "Say hello to alice",
		},
		{
			name:     "multiple placeholders",
			commands: "Analyze ${env.PROJECT_NAME} in ${env.ENVIRONMENT}",
			envVars:  map[string]string{"PROJECT_NAME": "myproject", "ENVIRONMENT": "production"},
			expected: "Analyze myproject in production",
		},
		{
			name:     "default value",
			commands: "Say hello to ${env.USER || 'Bob'}",
			envVars:  map[string]string{},
			expected: "Say hello to Bob",
		},
		{
			name:     "missing env var expands to empty string",
			commands: "Check ${env.MISSING_VAR} status",
			envVars:  map[string]string{},
			expected: "Check  status",
		},
		{
			name:     "ternary operator",
			commands: "${env.NAME == 'bob' ? 'Yes' : 'No'}",
			envVars:  map[string]string{"NAME": "bob"},
			expected: "Yes",
		},
		{
			name:     "default value (found)",
			commands: "${env.NAME || 'UNKNOWN'}",
			envVars:  map[string]string{"NAME": "bob"},
			expected: "bob",
		},
		{
			name:     "default value (not found)",
			commands: "${env.NAME || 'UNKNOWN'}",
			envVars:  map[string]string{},
			expected: "UNKNOWN",
		},
		{
			name:     "backticks in template (markdown code fence)",
			commands: "Here is code:\n```\n${env.CODE}\n```\nEnd.",
			envVars:  map[string]string{"CODE": "fmt.Println()"},
			expected: "Here is code:\n```\nfmt.Println()\n```\nEnd.",
		},
		{
			name:     "multiple backticks",
			commands: "Use `inline` and ```block``` code",
			envVars:  map[string]string{},
			expected: "Use `inline` and ```block``` code",
		},
		{
			name:     "single backslash",
			commands: "test\\value",
			envVars:  map[string]string{},
			expected: "test\\value",
		},
		{
			name:     "backslash n (not newline)",
			commands: "test\\nvalue",
			envVars:  map[string]string{},
			expected: "test\\nvalue",
		},
		{
			name:     "backslash t (not tab)",
			commands: "test\\tvalue",
			envVars:  map[string]string{},
			expected: "test\\tvalue",
		},
		{
			name:     "windows path",
			commands: "C:\\Users\\Alice\\Documents",
			envVars:  map[string]string{},
			expected: "C:\\Users\\Alice\\Documents",
		},
		{
			name:     "network path",
			commands: "\\\\server\\share\\file",
			envVars:  map[string]string{},
			expected: "\\\\server\\share\\file",
		},
		{
			name:     "multiple backslashes",
			commands: "test\\\\value",
			envVars:  map[string]string{},
			expected: "test\\\\value",
		},
		{
			name:     "regex pattern with backslashes",
			commands: "\\d+\\.\\d+",
			envVars:  map[string]string{},
			expected: "\\d+\\.\\d+",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := testEnvProvider(tt.envVars)

			expander := NewJsExpander(&env)
			result := expander.Expand(t.Context(), tt.commands, nil)

			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestExpandMap(t *testing.T) {
	t.Parallel()

	env := testEnvProvider(map[string]string{
		"USER": "alice",
	})

	expander := NewJsExpander(&env)
	result := expander.ExpandMap(t.Context(), map[string]string{
		"none":   "List all files",
		"simple": "Say hello to ${env.USER}",
	})

	assert.Equal(t, map[string]string{
		"none":   "List all files",
		"simple": "Say hello to alice",
	}, result)
}

func TestExpandMap_Reuse(t *testing.T) {
	t.Parallel()

	env := testEnvProvider(map[string]string{
		"USER": "alice",
	})

	expander := NewJsExpander(&env)

	result := expander.ExpandMap(t.Context(), map[string]string{
		"none": "List all files",
	})
	assert.Equal(t, map[string]string{
		"none": "List all files",
	}, result)

	result = expander.ExpandMap(t.Context(), map[string]string{
		"simple": "Say hello to ${env.USER}",
	})
	assert.Equal(t, map[string]string{
		"simple": "Say hello to alice",
	}, result)
}

func TestExpandMap_Empty(t *testing.T) {
	t.Parallel()

	env := testEnvProvider(map[string]string{})

	expander := NewJsExpander(&env)
	result := expander.ExpandMap(t.Context(), map[string]string{})

	assert.Empty(t, result)
}

func TestExpandString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		template string
		values   map[string]string
		expected string
	}{
		{
			name:     "simple substitution",
			template: "Hello ${name}!",
			values:   map[string]string{"name": "World"},
			expected: "Hello World!",
		},
		{
			name:     "multiple values",
			template: "File: ${path} (chunk ${index})",
			values:   map[string]string{"path": "/foo/bar.go", "index": "0"},
			expected: "File: /foo/bar.go (chunk 0)",
		},
		{
			name:     "backticks in template are preserved",
			template: "Code:\n```\n${content}\n```",
			values:   map[string]string{"content": "func main() {}"},
			expected: "Code:\n```\nfunc main() {}\n```",
		},
		{
			name:     "backticks in value are preserved",
			template: "The code is: ${code}",
			values:   map[string]string{"code": "use `fmt.Println()`"},
			expected: "The code is: use `fmt.Println()`",
		},
		{
			name:     "semantic prompt with code fence",
			template: "Summarize:\n```\n${content}\n```\nBe concise.",
			values:   map[string]string{"content": "package main\n\nfunc main() {\n\tfmt.Println(`hello`)\n}"},
			expected: "Summarize:\n```\npackage main\n\nfunc main() {\n\tfmt.Println(`hello`)\n}\n```\nBe concise.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := testEnvProvider(map[string]string{})
			expander := NewJsExpander(&env)
			result := expander.Expand(t.Context(), tt.template, tt.values)
			assert.Equal(t, tt.expected, result)
		})
	}
}

type testEnvProvider map[string]string

func (p *testEnvProvider) Get(_ context.Context, name string) (string, bool) {
	val, found := (*p)[name]
	return val, found
}

// TestExpandCommandsThenEvaluate verifies the two-phase flow that slash commands go through:
// 1. ExpandCommands at config load time (env-only, no tools)
// 2. Evaluate at runtime (tools available)
// This catches regressions where one phase corrupts expressions needed by the other.
func TestExpandCommandsThenEvaluate(t *testing.T) {
	t.Parallel()

	env := testEnvProvider(map[string]string{"USER": "alice"})

	mockTools := []tools.Tool{
		{
			Name: "shell",
			Handler: func(_ context.Context, tc tools.ToolCall) (*tools.ToolCallResult, error) {
				return tools.ResultSuccess("lint output"), nil
			},
		},
	}

	cmds := types.Commands{
		"fix-lint": {
			Description: "Fix lint",
			Instruction: "User: ${env.USER}\nLint: ${shell({cmd: \"task lint\"})}\n${unknown_mcp_tool()}",
		},
	}

	// Phase 1: ExpandCommands with env only (no tools)
	expander := NewJsExpander(&env)
	expanded := expander.ExpandCommands(t.Context(), cmds)

	// env.USER should be expanded, tool calls should be preserved
	assert.Contains(t, expanded["fix-lint"].Instruction, "User: alice")
	assert.Contains(t, expanded["fix-lint"].Instruction, "${shell({cmd: \"task lint\"})}") // preserved
	assert.Contains(t, expanded["fix-lint"].Instruction, "${unknown_mcp_tool()}")          // preserved

	// Phase 2: Evaluate with tools (no env)
	evaluator := NewEvaluator(mockTools)
	result := evaluator.Evaluate(t.Context(), expanded["fix-lint"].Instruction, nil)

	// shell should now be expanded, unknown tool should be preserved
	assert.Contains(t, result, "User: alice")
	assert.Contains(t, result, "lint output")
	assert.Contains(t, result, "${unknown_mcp_tool()}")
	assert.NotContains(t, result, "${shell")
}

func TestFindClosingBrace(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		text     string
		pos      int
		expected int
	}{
		{
			name:     "simple",
			text:     "${foo}",
			pos:      2,
			expected: 5,
		},
		{
			name:     "nested braces",
			text:     "${shell({cmd: \"ls\"})}",
			pos:      2,
			expected: len("${shell({cmd: \"ls\"})}") - 1,
		},
		{
			name:     "closing brace inside quotes",
			text:     `${shell({cmd: "echo }"})}`,
			pos:      2,
			expected: len(`${shell({cmd: "echo }"})}`) - 1,
		},
		{
			name:     "escaped quote inside string",
			text:     `${shell({cmd: "echo \"}"})}`,
			pos:      2,
			expected: len(`${shell({cmd: "echo \"}"})}`) - 1,
		},
		{
			name:     "unclosed",
			text:     "${foo",
			pos:      2,
			expected: -1,
		},
		{
			name:     "unclosed nested",
			text:     "${foo({bar: 1}",
			pos:      2,
			expected: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := findClosingBrace(tt.text, tt.pos)
			assert.Equal(t, tt.expected, result)
		})
	}
}
