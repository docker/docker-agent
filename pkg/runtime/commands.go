package runtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/tools"
)

// CommandEvaluator expands ${...} JavaScript expressions in a /command
// instruction, with access to the agent's tools and the command arguments.
type CommandEvaluator interface {
	Evaluate(ctx context.Context, instruction string, args []string) string
}

// commandEvaluatorFactory builds the CommandEvaluator used by
// ResolveCommand. It is empty until a JS engine is registered: calling
// jscommands.Register (done by loaderdefaults.Opts, the CLI and
// embeddedchat/defaults) wires in the goja-backed evaluator from pkg/js.
// The indirection keeps the JS engine out of pkg/runtime's import graph for
// embedders that build teams in code and never use JS command expressions.
var commandEvaluatorFactory atomic.Pointer[CommandEvaluatorFactory]

// RegisterCommandEvaluator installs the factory ResolveCommand uses to
// expand ${...} expressions. See pkg/runtime/jscommands.
// Prefer [WithCommandEvaluatorFactory] to configure independent runtimes.
func RegisterCommandEvaluator(factory CommandEvaluatorFactory) {
	commandEvaluatorFactory.Store(&factory)
}

// CommandEvaluatorFactory builds an evaluator with the current agent's tools.
type CommandEvaluatorFactory = func(agentTools []tools.Tool) CommandEvaluator

// WithCommandEvaluatorFactory selects the slash-command evaluator for this runtime.
// Passing nil leaves JavaScript expressions unexpanded, even when a global
// evaluator is registered. Without this option, the global evaluator is used.
func WithCommandEvaluatorFactory(factory CommandEvaluatorFactory) Opt {
	return func(r *LocalRuntime) {
		r.commandEvaluator = &factory
	}
}

// CommandEvaluatorFactory returns the effective slash-command evaluator factory.
// Runtime decorators passed to ResolveCommand should forward this method to
// preserve instance configuration, including an explicitly disabled evaluator.
func (r *LocalRuntime) CommandEvaluatorFactory() CommandEvaluatorFactory {
	if r.commandEvaluator != nil {
		return *r.commandEvaluator
	}
	return globalCommandFactory()
}

func globalCommandFactory() CommandEvaluatorFactory {
	if factory := commandEvaluatorFactory.Load(); factory != nil {
		return *factory
	}
	return nil
}

// argsPlaceholderRegex matches ${args...} patterns to check if args are used.
// This includes ${args}, ${args[N]}, ${args.join(...)}, ${args.length}, etc.
var argsPlaceholderRegex = regexp.MustCompile(`\$\{args[^}]*\}`)

// LookupCommand parses userInput as a /command invocation and returns the
// matching command along with its trailing arguments. The boolean is false
// when userInput doesn't start with '/' or doesn't match a configured
// command. Callers that need both the resolved instruction and the original
// command metadata (e.g. its target agent) typically call LookupCommand to
// inspect the command before calling ResolveCommand.
func LookupCommand(ctx context.Context, rt Runtime, userInput string) (cmd types.Command, rest string, ok bool) {
	if !strings.HasPrefix(userInput, "/") {
		return types.Command{}, "", false
	}

	head, tail, _ := strings.Cut(userInput, " ")
	commandName := head[1:]

	command, found := rt.CurrentAgentInfo(ctx).Commands[commandName]
	if !found {
		return types.Command{}, "", false
	}
	return command, tail, true
}

// ResolveCommand transforms a /command into its expanded instruction text.
// It processes:
// 1. Command lookup from agent commands
// 2. Tool command execution (!tool_name(arg=value)) - tools executed and output inserted
// 3. JavaScript expressions (${...}) - evaluated with access to all agent tools and args array
//   - ${args[0]}, ${args[1]}, etc. for positional arguments
//   - ${args} or ${args.join(" ")} for all arguments
//   - ${tool({...})} for tool calls
//
// For agent-switching commands (those declaring `agent: <name>` and no
// instruction), ResolveCommand returns the trailing arguments verbatim so the
// caller can forward them to the target sub-agent after switching. When the
// command has no instruction and no arguments, the result is the empty
// string, signalling "no message to send".
func ResolveCommand(ctx context.Context, rt Runtime, userInput string) string {
	command, rest, ok := LookupCommand(ctx, rt, userInput)
	if !ok {
		return userInput
	}

	instruction := command.Instruction

	// Agent-only commands (no instruction): forward the trailing args verbatim
	// so the target sub-agent receives the user's original prompt.
	if instruction == "" {
		return rest
	}

	args := tokenize(rest)

	// Execute JavaScript expressions (${...} syntax) with args array
	// We execute JS first to prevent tool output (from !tool commands) from being evaluated as JS,
	// which would be a security vulnerability (injection).
	factory := globalCommandFactory()
	if local, ok := rt.(interface {
		CommandEvaluatorFactory() CommandEvaluatorFactory
	}); ok {
		factory = local.CommandEvaluatorFactory()
	}
	if factory == nil {
		if strings.Contains(instruction, "${") {
			slog.WarnContext(ctx, "No JavaScript evaluator registered; ${...} expressions left unexpanded (call jscommands.Register to enable them)")
		}
	} else if agentTools, err := rt.CurrentAgentTools(ctx); err != nil {
		slog.WarnContext(ctx, "Failed to get agent tools for JS expression execution", "error", err)
	} else {
		instruction = factory(agentTools).Evaluate(ctx, instruction, args)
	}

	// Execute tool commands and substitute their output (legacy !tool() syntax)
	instruction = executeToolCommands(ctx, rt, instruction)

	// Append remaining text if no placeholders were used
	if rest != "" && !argsPlaceholderRegex.MatchString(command.Instruction) {
		instruction += " " + rest
	}

	return instruction
}

// tokenize splits input into tokens, respecting quoted strings.
// Quotes are stripped from the tokens.
func tokenize(input string) []string {
	if input == "" {
		return nil
	}

	var tokens []string
	var current strings.Builder
	var quoteChar rune

	for _, r := range input {
		switch {
		case quoteChar == 0 && (r == '"' || r == '\''):
			quoteChar = r
		case r == quoteChar:
			quoteChar = 0
		case r == ' ' && quoteChar == 0:
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}

	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}

	return tokens
}

// toolCommand represents a parsed tool command from the instruction.
type toolCommand struct {
	start    int
	end      int
	toolName string
	argsStr  string
}

// parseToolCommands finds all !tool_name(...) patterns in the instruction.
func parseToolCommands(instruction string) []toolCommand {
	var commands []toolCommand

	for i := 0; i < len(instruction); i++ {
		if instruction[i] != '!' {
			continue
		}

		start := i
		i++

		// Parse tool name
		nameStart := i
		for i < len(instruction) && isWordChar(instruction[i]) {
			i++
		}
		if i == nameStart || i >= len(instruction) || instruction[i] != '(' {
			continue
		}
		toolName := instruction[nameStart:i]

		// Find matching ')'
		argsStart := i + 1
		end, ok := findMatchingParen(instruction, i)
		if !ok {
			continue
		}

		commands = append(commands, toolCommand{
			start:    start,
			end:      end,
			toolName: toolName,
			argsStr:  instruction[argsStart : end-1],
		})
		i = end - 1 // -1 because loop will increment
	}

	return commands
}

// findMatchingParen finds the index after the matching closing parenthesis.
// It handles nested parentheses and quoted strings.
func findMatchingParen(s string, openIdx int) (int, bool) {
	depth := 1
	var quoteChar byte

	for i := openIdx + 1; i < len(s) && depth > 0; i++ {
		ch := s[i]
		if quoteChar != 0 {
			if ch == quoteChar {
				quoteChar = 0
			} else if ch == '\\' && i+1 < len(s) {
				i++ // skip escaped char
			}
			continue
		}
		switch ch {
		case '"', '\'':
			quoteChar = ch
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

// isWordChar returns true if the byte is a valid word character.
func isWordChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

// executeToolCommands executes !tool_name(arg=value) patterns and replaces them with output.
func executeToolCommands(ctx context.Context, rt Runtime, instruction string) string {
	commands := parseToolCommands(instruction)
	if len(commands) == 0 {
		return instruction
	}

	agentTools, err := rt.CurrentAgentTools(ctx)
	if err != nil {
		slog.WarnContext(ctx, "Failed to get agent tools for command execution", "error", err)
		return instruction
	}

	toolMap := make(map[string]tools.Tool, len(agentTools))
	for _, t := range agentTools {
		toolMap[t.Name] = t
	}

	// Process in reverse order to maintain correct indices
	result := instruction
	for _, cmd := range slices.Backward(commands) {
		replacement := executeSingleToolCommand(ctx, toolMap, cmd.toolName, cmd.argsStr)
		result = result[:cmd.start] + replacement + result[cmd.end:]
	}

	return result
}

// executeSingleToolCommand executes a single tool command and returns the output.
func executeSingleToolCommand(ctx context.Context, toolMap map[string]tools.Tool, toolName, argsStr string) string {
	slog.DebugContext(ctx, "Executing tool command", "tool", toolName, "args", argsStr)

	tool, exists := toolMap[toolName]
	if !exists {
		slog.WarnContext(ctx, "Tool not found for command execution", "tool", toolName)
		return "Error: tool '" + toolName + "' not found"
	}
	if tool.Handler == nil {
		slog.WarnContext(ctx, "Tool has no handler", "tool", toolName)
		return "Error: tool '" + toolName + "' has no handler"
	}

	argsJSON, err := json.Marshal(parseToolArgs(argsStr))
	if err != nil {
		slog.WarnContext(ctx, "Failed to marshal tool arguments", "tool", toolName, "error", err)
		return "Error: failed to marshal arguments for '" + toolName + "'"
	}

	toolCall := tools.ToolCall{
		ID:   "cmd_" + toolName,
		Type: "function",
		Function: tools.FunctionCall{
			Name:      toolName,
			Arguments: string(argsJSON),
		},
	}

	toolCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	result, err := tool.Handler(toolCtx, toolCall, tools.NopRuntime{})
	if err != nil {
		slog.WarnContext(ctx, "Tool execution failed", "tool", toolName, "error", err)
		return "Error executing '" + toolName + "': " + err.Error()
	}

	output := strings.TrimSpace(result.Output)
	slog.DebugContext(ctx, "Tool command output", "tool", toolName, "output_length", len(output))
	return output
}

// parseToolArgs parses key=value pairs from a tool command argument string.
func parseToolArgs(argsStr string) map[string]any {
	result := make(map[string]any)
	if strings.TrimSpace(argsStr) == "" {
		return result
	}

	var key, value strings.Builder
	var quoteChar rune
	inValue := false

	flush := func() {
		k := strings.TrimSpace(key.String())
		if k != "" {
			result[k] = parseValue(strings.TrimSpace(value.String()))
		}
		key.Reset()
		value.Reset()
		inValue = false
	}

	for _, r := range argsStr {
		switch {
		case quoteChar == 0 && (r == '"' || r == '\''):
			quoteChar = r
		case r == quoteChar:
			quoteChar = 0
		case r == '=' && !inValue && quoteChar == 0:
			inValue = true
		case r == ' ' && quoteChar == 0 && inValue:
			flush()
		case inValue:
			value.WriteRune(r)
		default:
			key.WriteRune(r)
		}
	}
	flush()

	return result
}

// parseValue converts a string to a typed value (bool, int, float, or string).
func parseValue(s string) any {
	if b, err := strconv.ParseBool(s); err == nil {
		return b
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}
