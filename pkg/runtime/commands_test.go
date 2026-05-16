package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

// mockRuntime implements Runtime interface for testing
type mockRuntime struct {
	commands types.Commands
	tools    []tools.Tool
}

func (m *mockRuntime) CurrentAgentTools(context.Context) ([]tools.Tool, error) {
	return m.tools, nil
}
func (m *mockRuntime) CurrentAgentName() string                           { return "test" }
func (m *mockRuntime) CurrentAgentToolsetStatuses() []tools.ToolsetStatus { return nil }
func (m *mockRuntime) RestartToolset(context.Context, string) error       { return nil }
func (m *mockRuntime) CurrentAgentInfo(context.Context) CurrentAgentInfo {
	return CurrentAgentInfo{
		Name:        "test",
		Description: "test description",
		Commands:    m.commands,
	}
}

func (m *mockRuntime) SetCurrentAgent(string) error {
	return nil
}
func (m *mockRuntime) EmitStartupInfo(context.Context, *session.Session, EventSink) {}
func (m *mockRuntime) ResetStartupInfo()                                            {}
func (m *mockRuntime) RunStream(context.Context, *session.Session) <-chan Event {
	return nil
}

func (m *mockRuntime) Run(context.Context, *session.Session) ([]session.Message, error) {
	return nil, nil
}
func (m *mockRuntime) Resume(context.Context, ResumeRequest) {}
func (m *mockRuntime) ResumeElicitation(context.Context, tools.ElicitationAction, map[string]any) error {
	return nil
}
func (m *mockRuntime) SessionStore() session.Store { return nil }
func (m *mockRuntime) Summarize(context.Context, *session.Session, string, EventSink) {
}
func (m *mockRuntime) PermissionsInfo() *PermissionsInfo { return nil }
func (m *mockRuntime) CurrentAgentSkillsToolset() *skillstool.Toolset {
	return nil
}

func (m *mockRuntime) CurrentMCPPrompts(context.Context) map[string]mcptools.PromptInfo {
	return make(map[string]mcptools.PromptInfo)
}

func (m *mockRuntime) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", nil
}

func (m *mockRuntime) UpdateSessionTitle(context.Context, *session.Session, string) error {
	return nil
}
func (m *mockRuntime) TitleGenerator() *sessiontitle.Generator             { return nil }
func (m *mockRuntime) Close() error                                        { return nil }
func (m *mockRuntime) Steer(QueuedMessage) error                           { return nil }
func (m *mockRuntime) FollowUp(QueuedMessage) error                        { return nil }
func (m *mockRuntime) QueueStatus() QueueStatus                            { return QueueStatus{} }
func (m *mockRuntime) TogglePause(context.Context) (bool, error)           { return false, nil }
func (m *mockRuntime) SetAgentModel(context.Context, string, string) error { return nil }
func (m *mockRuntime) AvailableModels(context.Context) []ModelChoice       { return nil }
func (m *mockRuntime) SupportsModelSwitching() bool                        { return false }
func (m *mockRuntime) OnToolsChanged(func(Event))                          {}

func (m *mockRuntime) RegenerateTitle(context.Context, *session.Session, chan Event) {
}

func TestResolveCommand_SimpleCommand(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{
			"test": types.Command{Instruction: "This is the test instruction"},
		},
	}

	result := ResolveCommand(t.Context(), rt, "/test")
	assert.Equal(t, "This is the test instruction", result)
}

func TestResolveCommand_CommandNotFound(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{},
	}

	result := ResolveCommand(t.Context(), rt, "/unknown")
	assert.Equal(t, "/unknown", result)
}

func TestResolveCommand_NotACommand(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{
			"test": types.Command{Instruction: "instruction"},
		},
	}

	result := ResolveCommand(t.Context(), rt, "regular message")
	assert.Equal(t, "regular message", result)
}

func TestResolveCommand_PositionalArgs(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{
			"fix": types.Command{Instruction: "Fix the file ${args[0]} with options ${args[1]}"},
		},
	}

	result := ResolveCommand(t.Context(), rt, "/fix main.go --verbose")
	assert.Equal(t, "Fix the file main.go with options --verbose", result)
}

func TestResolveCommand_PositionalArgsWithQuotes(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{
			"search": types.Command{Instruction: `Search for ${args[0]} in ${args[1]}`},
		},
	}

	result := ResolveCommand(t.Context(), rt, `/search "hello world" ./src`)
	assert.Equal(t, "Search for hello world in ./src", result)
}

func TestResolveCommand_AllArgs(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{
			"run": types.Command{Instruction: `Run command with args: ${args.join(" ")}`},
		},
	}

	result := ResolveCommand(t.Context(), rt, "/run arg1 arg2 arg3")
	assert.Equal(t, "Run command with args: arg1 arg2 arg3", result)
}

func TestResolveCommand_ArgsPlaceholder(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{
			"run": types.Command{Instruction: `Run command with args: ${args.join(" ")}`},
		},
	}

	result := ResolveCommand(t.Context(), rt, "/run arg1 arg2 arg3")
	assert.Equal(t, "Run command with args: arg1 arg2 arg3", result)
}

func TestResolveCommand_ArgsPlaceholderEmpty(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{
			"run": types.Command{Instruction: `Run command with args: ${args.join(" ")}`},
		},
	}

	result := ResolveCommand(t.Context(), rt, "/run")
	assert.Equal(t, "Run command with args: ", result)
}

func TestResolveCommand_ArgsPlaceholderWithPositional(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{
			"run": types.Command{Instruction: `First: ${args[0]}, All: ${args.join(" ")}`},
		},
	}

	result := ResolveCommand(t.Context(), rt, "/run arg1 arg2 arg3")
	assert.Equal(t, "First: arg1, All: arg1 arg2 arg3", result)
}

func TestResolveCommand_MissingArgs(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{
			"fix": types.Command{Instruction: `Fix ${args[0] || ""} and ${args[1] || ""} and ${args[2] || ""}`},
		},
	}

	result := ResolveCommand(t.Context(), rt, "/fix file1")
	// args[1] and args[2] should be replaced with empty strings via || operator
	assert.Equal(t, "Fix file1 and  and ", result)
}

func TestResolveCommand_ToolCommand(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{
			"info": types.Command{Instruction: "Result: !echo_tool()"},
		},
		tools: []tools.Tool{
			{
				Name: "echo_tool",
				Handler: func(_ context.Context, _ tools.ToolCall) (*tools.ToolCallResult, error) {
					return tools.ResultSuccess("hello from tool"), nil
				},
			},
		},
	}

	result := ResolveCommand(t.Context(), rt, "/info")
	assert.Equal(t, "Result: hello from tool", result)
}

func TestResolveCommand_ToolCommandWithArgs(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{
			"greet": types.Command{Instruction: "Greeting: !greet_tool(name=World)"},
		},
		tools: []tools.Tool{
			{
				Name: "greet_tool",
				Handler: func(_ context.Context, tc tools.ToolCall) (*tools.ToolCallResult, error) {
					// Parse the args to verify they're passed correctly
					return tools.ResultSuccess("Hello, " + tc.Function.Arguments), nil
				},
			},
		},
	}

	result := ResolveCommand(t.Context(), rt, "/greet")
	assert.Contains(t, result, "Greeting:")
	assert.Contains(t, result, "World")
}

func TestResolveCommand_ToolCommandWithQuotedArgs(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{
			"search": types.Command{Instruction: `Files: !search_tool(query="hello world" path=/tmp)`},
		},
		tools: []tools.Tool{
			{
				Name: "search_tool",
				Handler: func(_ context.Context, tc tools.ToolCall) (*tools.ToolCallResult, error) {
					return tools.ResultSuccess("searched: " + tc.Function.Arguments), nil
				},
			},
		},
	}

	result := ResolveCommand(t.Context(), rt, "/search")
	assert.Contains(t, result, "Files:")
	assert.Contains(t, result, "hello world")
}

func TestResolveCommand_ToolCommandNotFound(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{
			"fail": types.Command{Instruction: "Result: !nonexistent_tool()"},
		},
		tools: []tools.Tool{},
	}

	result := ResolveCommand(t.Context(), rt, "/fail")
	assert.Contains(t, result, "Error")
	assert.Contains(t, result, "not found")
}

func TestResolveCommand_CombinedPositionalAndTool(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{
			"check": types.Command{
				Instruction: "Check ${args[0]} with result: !check_tool()",
			},
		},
		tools: []tools.Tool{
			{
				Name: "check_tool",
				Handler: func(_ context.Context, _ tools.ToolCall) (*tools.ToolCallResult, error) {
					return tools.ResultSuccess("check_output"), nil
				},
			},
		},
	}

	result := ResolveCommand(t.Context(), rt, "/check myfile.go")
	assert.Equal(t, "Check myfile.go with result: check_output", result)
}

func TestResolveCommand_ExtraArgsAppended(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{
			"simple": types.Command{Instruction: "Do the thing"},
		},
	}

	// Commands without positional args get extra text appended
	result := ResolveCommand(t.Context(), rt, "/simple with extra text")
	assert.Equal(t, "Do the thing with extra text", result)
}

func TestTokenize(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{"empty", "", nil},
		{"single", "arg1", []string{"arg1"}},
		{"multiple", "arg1 arg2 arg3", []string{"arg1", "arg2", "arg3"}},
		{"quoted", `"hello world" arg2`, []string{"hello world", "arg2"}},
		{"single quoted", `'hello world' arg2`, []string{"hello world", "arg2"}},
		{"mixed", `arg1 "quoted arg" arg3`, []string{"arg1", "quoted arg", "arg3"}},
		{"extra spaces", "arg1  arg2   arg3", []string{"arg1", "arg2", "arg3"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := tokenize(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParseToolArgs(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected map[string]any
	}{
		{
			name:     "empty",
			input:    "",
			expected: map[string]any{},
		},
		{
			name:     "single arg",
			input:    "key=value",
			expected: map[string]any{"key": "value"},
		},
		{
			name:     "multiple args",
			input:    "key1=value1 key2=value2",
			expected: map[string]any{"key1": "value1", "key2": "value2"},
		},
		{
			name:     "quoted value",
			input:    `key="hello world"`,
			expected: map[string]any{"key": "hello world"},
		},
		{
			name:     "single quoted value",
			input:    `key='hello world'`,
			expected: map[string]any{"key": "hello world"},
		},
		{
			name:     "mixed quotes and unquoted",
			input:    `name="John Doe" age=30`,
			expected: map[string]any{"name": "John Doe", "age": int64(30)},
		},
		{
			name:     "boolean true",
			input:    "flag=true",
			expected: map[string]any{"flag": true},
		},
		{
			name:     "boolean false",
			input:    "flag=false",
			expected: map[string]any{"flag": false},
		},
		{
			name:     "integer",
			input:    "count=42",
			expected: map[string]any{"count": int64(42)},
		},
		{
			name:     "float",
			input:    "ratio=3.14",
			expected: map[string]any{"ratio": 3.14},
		},
		{
			name:     "path value",
			input:    "path=/tmp/file.txt",
			expected: map[string]any{"path": "/tmp/file.txt"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := parseToolArgs(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParseValue(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected any
	}{
		{"string", "hello", "hello"},
		{"true", "true", true},
		{"false", "false", false},
		{"integer", "42", int64(42)},
		{"negative integer", "-10", int64(-10)},
		{"float", "3.14", 3.14},
		{"negative float", "-2.5", -2.5},
		{"path", "/tmp/file", "/tmp/file"},
		{"url-like", "http://example.com", "http://example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := parseValue(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParseToolCommands(t *testing.T) {
	tests := []struct {
		name        string
		instruction string
		expected    []toolCommand
	}{
		{
			name:        "empty",
			instruction: "",
			expected:    nil,
		},
		{
			name:        "no commands",
			instruction: "Just some text",
			expected:    nil,
		},
		{
			name:        "simple command",
			instruction: "!tool()",
			expected:    []toolCommand{{start: 0, end: 7, toolName: "tool", argsStr: ""}},
		},
		{
			name:        "command with args",
			instruction: "!tool(arg=value)",
			expected:    []toolCommand{{start: 0, end: 16, toolName: "tool", argsStr: "arg=value"}},
		},
		{
			name:        "command in text",
			instruction: "Before !tool(arg=value) After",
			expected:    []toolCommand{{start: 7, end: 23, toolName: "tool", argsStr: "arg=value"}},
		},
		{
			name:        "nested parentheses",
			instruction: `!shell(cmd="echo $((2+2))")`,
			expected:    []toolCommand{{start: 0, end: 27, toolName: "shell", argsStr: `cmd="echo $((2+2))"`}},
		},
		{
			name:        "deeply nested parentheses",
			instruction: `!shell(cmd="sh -c 'echo $((1+2+3))'")`,
			expected:    []toolCommand{{start: 0, end: 37, toolName: "shell", argsStr: `cmd="sh -c 'echo $((1+2+3))'"`}},
		},
		{
			name:        "multiple commands",
			instruction: "!tool1() and !tool2(arg=val)",
			expected: []toolCommand{
				{start: 0, end: 8, toolName: "tool1", argsStr: ""},
				{start: 13, end: 28, toolName: "tool2", argsStr: "arg=val"},
			},
		},
		{
			name:        "quoted string with parens",
			instruction: `!tool(msg="hello (world)")`,
			expected:    []toolCommand{{start: 0, end: 26, toolName: "tool", argsStr: `msg="hello (world)"`}},
		},
		{
			name:        "single quoted string with parens",
			instruction: `!tool(msg='hello (world)')`,
			expected:    []toolCommand{{start: 0, end: 26, toolName: "tool", argsStr: `msg='hello (world)'`}},
		},
		{
			name:        "exclamation without tool",
			instruction: "Hello! World",
			expected:    nil,
		},
		{
			name:        "unbalanced parens",
			instruction: "!tool(arg=value",
			expected:    nil,
		},
		{
			name:        "tool name with underscore",
			instruction: "!my_tool()",
			expected:    []toolCommand{{start: 0, end: 10, toolName: "my_tool", argsStr: ""}},
		},
		{
			name:        "tool name with numbers",
			instruction: "!tool123()",
			expected:    []toolCommand{{start: 0, end: 10, toolName: "tool123", argsStr: ""}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := parseToolCommands(tt.instruction)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestResolveCommand_NestedParentheses(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{
			"calc": types.Command{Instruction: `${shell({cmd: "sh -c 'echo $((" + args.join(" ") + "))'"})}`},
		},
		tools: []tools.Tool{
			{
				Name: "shell",
				Handler: func(_ context.Context, tc tools.ToolCall) (*tools.ToolCallResult, error) {
					// Return the arguments to verify they're parsed correctly
					return tools.ResultSuccess("args: " + tc.Function.Arguments), nil
				},
			},
		},
	}

	result := ResolveCommand(t.Context(), rt, "/calc 2+2")
	assert.Contains(t, result, `sh -c 'echo $((2+2))'`)
}

func TestResolveCommand_MultipleNestedParens(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{
			"test": types.Command{Instruction: `Result: !tool(cmd="func(a(b(c)))")`},
		},
		tools: []tools.Tool{
			{
				Name: "tool",
				Handler: func(_ context.Context, tc tools.ToolCall) (*tools.ToolCallResult, error) {
					return tools.ResultSuccess(tc.Function.Arguments), nil
				},
			},
		},
	}

	result := ResolveCommand(t.Context(), rt, "/test")
	assert.Contains(t, result, `func(a(b(c)))`)
}

func TestResolveCommand_ArgsJoinComma(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{
			"test": types.Command{Instruction: `Args: ${args.join(",")}`},
		},
	}

	result := ResolveCommand(t.Context(), rt, "/test a b c")
	assert.Equal(t, "Args: a,b,c", result)
}

func TestResolveCommand_ArgsLength(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{
			"test": types.Command{Instruction: `Count: ${args.length}`},
		},
	}

	result := ResolveCommand(t.Context(), rt, "/test a b c")
	assert.Equal(t, "Count: 3", result)
}

func TestResolveCommand_ArgsSlice(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		commands: types.Commands{
			"test": types.Command{Instruction: `Rest: ${args.slice(1).join(" ")}`},
		},
	}

	result := ResolveCommand(t.Context(), rt, "/test first second third")
	assert.Equal(t, "Rest: second third", result)
}
