package runtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/tools"
)

// argsPlaceholderRegex matches ${args...} patterns to check if args are used.
// This includes ${args}, ${args[N]}, ${args.join(...)}, ${args.length}, etc.
var argsPlaceholderRegex = regexp.MustCompile(`\$\{args[^}]*\}`)

// ResolveCommand transforms a /command into its expanded instruction text.
// It processes:
// 1. Command lookup from agent commands
// 2. Tool command execution (!tool_name(arg=value)) - tools executed and output inserted
// 3. JavaScript expressions (${...}) - evaluated with access to all agent tools and args array
//   - ${args[0]}, ${args[1]}, etc. for positional arguments
//   - ${args} or ${args.join(" ")} for all arguments
//   - ${tool({...})} for tool calls
func ResolveCommand(ctx context.Context, rt Runtime, userInput string) string {
	if !strings.HasPrefix(userInput, "/") {
		return userInput
	}

	cmd, rest, _ := strings.Cut(userInput, " ")
	commandName := cmd[1:] // Remove leading "/"

	command, found := rt.CurrentAgentInfo(ctx).Commands[commandName]
	if !found {
		return userInput
	}

	instruction := command.Instruction
	args := tokenize(rest)

	// Execute JavaScript expressions (${...} syntax) with args array
	// We execute JS first to prevent tool output (from !tool commands) from being evaluated as JS,
	// which would be a security vulnerability (injection).
	agentTools, err := rt.CurrentAgentTools(ctx)
	if err != nil {
		slog.WarnContext(ctx, "Failed to get agent tools for JS expression execution", "error", err)
	} else {
		evaluator := js.NewEvaluator(agentTools)
		instruction = evaluator.Evaluate(ctx, instruction, args)
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

	result, err := tool.Handler(toolCtx, toolCall)
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
