package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/fatih/color"
	"github.com/mattn/go-isatty"
	orderedmap "github.com/wk8/go-ordered-map/v2"
	"golang.org/x/term"

	"github.com/docker/docker-agent/pkg/input"
	"github.com/docker/docker-agent/pkg/tools"
)

// ConfirmationResult represents the result of a user confirmation prompt
type ConfirmationResult string

const (
	ConfirmationApprove        ConfirmationResult = "approve"
	ConfirmationApproveSession ConfirmationResult = "approve_session"
	ConfirmationReject         ConfirmationResult = "reject"
	ConfirmationAbort          ConfirmationResult = "abort"
)

var bold = color.New(color.Bold).SprintfFunc()

type Printer struct {
	out      io.Writer
	isTTYOut bool
}

func NewPrinter(out io.Writer) *Printer {
	isTTY := false
	if f, ok := out.(*os.File); ok {
		isTTY = isatty.IsTerminal(f.Fd())
	}
	return &Printer{
		out:      out,
		isTTYOut: isTTY,
	}
}

func (p *Printer) Println(a ...any) {
	fmt.Fprintln(p.out, a...)
}

func (p *Printer) Print(a ...any) {
	fmt.Fprint(p.out, a...)
}

func (p *Printer) Printf(format string, a ...any) {
	fmt.Fprintf(p.out, format, a...)
}

// PrintWelcomeMessage prints the welcome message
func (p *Printer) PrintWelcomeMessage(appName string) {
	p.Printf("\n------- Welcome to %s! -------\n(Ctrl+C to stop the agent and exit)\n\n", bold(appName))
}

// PrintError prints an error message
func (p *Printer) PrintError(err error) {
	p.Printf("❌ %s", err)
}

// PrintAgentName prints the agent name header
func (p *Printer) PrintAgentName(agentName string) {
	if !p.isTTYOut {
		return
	}
	p.Printf("\n--- Agent: %s ---\n", bold(agentName))
}

// PrintToolCall prints a tool call
func (p *Printer) PrintToolCall(toolCall tools.ToolCall) {
	p.Printf("\nCalling %s%s\n", bold(toolCall.Function.Name), formatToolCallArguments(toolCall.Function.Arguments))
}

// PrintToolCallWithConfirmation prints a tool call and prompts for confirmation
func (p *Printer) PrintToolCallWithConfirmation(ctx context.Context, toolCall tools.ToolCall, rd io.Reader) ConfirmationResult {
	p.Printf("\n%s\n", bold("🛠️ Tool call requires confirmation 🛠️"))
	p.PrintToolCall(toolCall)
	p.Printf("\n%s", bold("Can I run this tool? ([y]es/[a]ll/[n]o): "))

	if !isatty.IsTerminal(os.Stdout.Fd()) {
		return ConfirmationReject
	}

	// Try single-character input from stdin in raw mode (no Enter required)
	fd := int(os.Stdin.Fd())
	if oldState, err := term.MakeRaw(fd); err == nil {
		defer func() {
			if err := term.Restore(fd, oldState); err != nil {
				p.Printf("\nFailed to restore terminal state: %v\n", err)
			}
		}()
		buf := make([]byte, 1)
		for {
			if _, err := os.Stdin.Read(buf); err != nil {
				break
			}
			switch buf[0] {
			case 'y', 'Y':
				p.Print(bold("Yes 👍"))
				return ConfirmationApprove
			case 'a', 'A':
				p.Print(bold("Yes to all 👍"))
				return ConfirmationApproveSession
			case 'n', 'N':
				p.Print(bold("No 👎"))
				return ConfirmationReject
			case 3: // Ctrl+C
				return ConfirmationAbort
			case '\r', '\n':
				// ignore
			default:
				// ignore other keys
			}
		}
	}

	// Fallback: line-based scanner (requires Enter)
	text, err := input.ReadLine(ctx, rd)
	if err != nil {
		return ConfirmationReject
	}

	switch text {
	case "y":
		return ConfirmationApprove
	case "a":
		return ConfirmationApproveSession
	case "n":
		return ConfirmationReject
	default:
		// Default to reject for invalid input
		return ConfirmationReject
	}
}

// PrintToolCallResponse prints a tool call response
func (p *Printer) PrintToolCallResponse(name, response string) {
	p.Printf("\n%s response%s\n", bold(name), formatToolCallResponse(response))
}

// PromptMaxIterationsContinue prompts the user to continue after max iterations
func (p *Printer) PromptMaxIterationsContinue(ctx context.Context, maxIterations int) ConfirmationResult {
	p.Printf("\n⚠️  Maximum iterations (%d) reached. The agent may be stuck in a loop.\n", maxIterations)
	p.Println("This can happen with smaller or less capable models.")
	p.Println("\nDo you want to continue for 10 more iterations? (y/n):")

	response, err := input.ReadLine(ctx, os.Stdin)
	if err != nil {
		p.Println("\nFailed to read input, exiting...")
		return ConfirmationAbort
	}

	response = strings.TrimSpace(strings.ToLower(response))
	if response == "y" || response == "yes" {
		p.Print("✓ Continuing...\n\n")
		return ConfirmationApprove
	} else {
		p.Print("Exiting...\n\n")
		return ConfirmationReject
	}
}

// PromptOAuthAuthorization prompts the user for OAuth authorization
func (p *Printer) PromptOAuthAuthorization(ctx context.Context, serverURL string) ConfirmationResult {
	p.Println("\n🔐 OAuth Authorization Required")
	p.Println("Server:", serverURL, "(remote)")
	p.Println("This server requires OAuth authentication to access its tools.")
	p.Println("Your browser will open automatically to complete the authorization.")
	p.Printf("\n%s (y/n): ", "Do you want to authorize access?")

	response, err := input.ReadLine(ctx, os.Stdin)
	if err != nil {
		p.Println("\nFailed to read input, aborting authorization...")
		return ConfirmationAbort
	}

	response = strings.TrimSpace(strings.ToLower(response))
	if response == "y" || response == "yes" {
		p.Println("✓ Starting OAuth authorization...")
		p.Println("Please complete the authorization in your browser.")
		p.Print("Once completed, the agent will continue automatically.\n\n")
		return ConfirmationApprove
	} else {
		p.Print("Authorization declined. Exiting...\n\n")
		return ConfirmationReject
	}
}

func formatToolCallArguments(arguments string) string {
	if arguments == "" {
		return "()"
	}

	// Is is a map?
	kv := orderedmap.New[string, any]()
	if err := json.Unmarshal([]byte(arguments), &kv); err == nil {
		if kv.Len() == 0 {
			return "()"
		}

		var (
			parts     []string
			multiline bool
		)

		for key, value := range kv.FromOldest() {
			formatted := formatJSONValue(key, value)
			parts = append(parts, formatted)

			multiline = multiline || strings.Contains(formatted, "\n")
		}

		if len(parts) == 1 && !multiline {
			return fmt.Sprintf("(%s)", parts[0])
		}

		return fmt.Sprintf("(\n  %s\n)", strings.Join(parts, "\n  "))
	}

	// Maybe some other JSON type?
	var parsed any
	if err := json.Unmarshal([]byte(arguments), &parsed); err == nil {
		formatted, _ := json.MarshalIndent(parsed, "", "  ")
		return fmt.Sprintf("(%s)", string(formatted))
	}

	// JSON parsing failed
	return fmt.Sprintf("(%s)", arguments)
}

func formatToolCallResponse(response string) string {
	if response == "" {
		return " → ()"
	}

	// For responses, we want to show them as readable text, not JSON
	// Check if it looks like JSON first
	var parsed any
	if err := json.Unmarshal([]byte(response), &parsed); err == nil {
		// It's valid JSON, format it nicely
		return " → " + formatParsedJSON(parsed)
	}

	// It's plain text, handle multiline content
	if strings.Contains(response, "\n") {
		// Trim whitespace and split into lines
		trimmed := strings.TrimSpace(response)
		lines := strings.Split(trimmed, "\n")

		if len(lines) <= 3 {
			// Short multiline, show inline
			return fmt.Sprintf(" → %q", response)
		}

		// Long multiline, format with line breaks
		// Process each line individually and collapse consecutive empty lines
		var formatted []string
		lastWasEmpty := false

		for _, line := range lines {
			trimmedLine := strings.TrimSpace(line)
			if trimmedLine == "" {
				// Empty line - only add one if the last line wasn't empty
				if !lastWasEmpty {
					formatted = append(formatted, "")
					lastWasEmpty = true
				}
			} else {
				formatted = append(formatted, line)
				lastWasEmpty = false
			}
		}
		return fmt.Sprintf(" → (\n%s\n)", strings.Join(formatted, "\n"))
	}

	// Single line text response
	return fmt.Sprintf(" → %q", response)
}

func formatParsedJSON(data any) string {
	switch v := data.(type) {
	case map[string]any:
		if len(v) == 0 {
			return "()"
		}

		parts := make([]string, 0, len(v))
		hasMultilineContent := false

		for key, value := range v {
			formatted := formatJSONValue(key, value)
			parts = append(parts, formatted)
			if strings.Contains(formatted, "\n") {
				hasMultilineContent = true
			}
		}

		if len(parts) == 1 && !hasMultilineContent {
			return fmt.Sprintf("(%s)", parts[0])
		}

		return fmt.Sprintf("(\n  %s\n)", strings.Join(parts, "\n  "))

	default:
		// For non-object types, use standard JSON formatting
		formatted, _ := json.MarshalIndent(data, "", "  ")
		return fmt.Sprintf("(%s)", string(formatted))
	}
}

func formatJSONValue(key string, value any) string {
	switch v := value.(type) {
	case string:
		// Handle multiline strings by displaying with actual newlines
		if strings.Contains(v, "\n") {
			// Format as: key: "content with
			// actual line breaks"
			return fmt.Sprintf("%s: %q", bold(key), v)
		}
		// Regular string with proper escaping
		return fmt.Sprintf("%s: %q", bold(key), v)

	case []any:
		if len(v) == 0 {
			return bold(key) + ": []"
		}
		// Single item arrays are rendered on a single line
		if len(v) == 1 {
			jsonBytes, _ := json.Marshal(v)
			return fmt.Sprintf("%s: %s", bold(key), string(jsonBytes))
		}
		// Show full array contents
		jsonBytes, _ := json.MarshalIndent(v, "", "  ")
		return fmt.Sprintf("%s: %s", bold(key), string(jsonBytes))

	case map[string]any:
		jsonBytes, _ := json.MarshalIndent(v, "", "  ")
		return fmt.Sprintf("%s: %s", bold(key), string(jsonBytes))

	default:
		jsonBytes, _ := json.Marshal(v)
		return fmt.Sprintf("%s: %s", bold(key), string(jsonBytes))
	}
}
