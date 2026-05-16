package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/mattn/go-isatty"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/input"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry"
)

// RuntimeError wraps runtime errors to distinguish them from usage errors
type RuntimeError struct {
	Err error
}

func (e RuntimeError) Error() string {
	return e.Err.Error()
}

func (e RuntimeError) Unwrap() error {
	return e.Err
}

// maxAutoExtensions is the maximum number of times --yolo mode will
// auto-continue when max iterations is reached, to prevent infinite loops.
const maxAutoExtensions = 5

// maxIterAction describes what the caller should do after a MaxIterationsReachedEvent.
type maxIterAction int

const (
	maxIterContinue maxIterAction = iota // auto-approved, keep running
	maxIterStop                          // safety cap reached, caller should stop
	maxIterPrompt                        // not in yolo mode, caller should prompt the user
)

// handleMaxIterationsAutoApprove decides whether to auto-extend iterations in
// --yolo mode. Returns maxIterContinue (approved), maxIterStop (cap reached),
// or maxIterPrompt (not in auto-approve mode, caller should ask the user).
func handleMaxIterationsAutoApprove(autoApprove bool, autoExtensions *int, maxIter int) maxIterAction {
	if !autoApprove {
		return maxIterPrompt
	}
	*autoExtensions++
	if *autoExtensions <= maxAutoExtensions {
		slog.Info("Auto-extending iterations in yolo mode",
			"extension", *autoExtensions,
			"max_extensions", maxAutoExtensions,
			"current_max", maxIter)
		return maxIterContinue
	}
	slog.Warn("Max auto-extensions reached in yolo mode, stopping",
		"total_extensions", *autoExtensions)
	return maxIterStop
}

// Config holds configuration for running an agent in CLI mode
type Config struct {
	AppName        string
	AttachmentPath string
	AutoApprove    bool
	HideToolCalls  bool
	OutputJSON     bool
}

// Run executes an agent in non-TUI mode, handling user input and runtime events.
// userMessages contains the user messages to send. If a single message is "-",
// input is read from stdin. If empty, an interactive prompt loop is started.
func Run(ctx context.Context, out *Printer, cfg Config, rt runtime.Runtime, sess *session.Session, userMessages []string) error {
	// Create a cancellable context for this agentic loop and wire Ctrl+C to cancel it
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Ensure telemetry is initialized and add to context so runtime can access it
	telemetry.EnsureGlobalTelemetryInitialized(ctx)
	if telemetryClient := telemetry.GetGlobalTelemetryClient(ctx); telemetryClient != nil {
		ctx = telemetry.WithClient(ctx, telemetryClient)
	}

	sess.Title = "Running agent"
	// If the last received event was an error, return it. That way the exit code
	// will be non-zero if the agent failed.
	var lastErr error

	oneLoop := func(text string, rd io.Reader) error {
		autoExtensions := 0

		userInput := strings.TrimSpace(text)
		if userInput == "" {
			return nil
		}

		userMsg, attachedPath := PrepareUserMessage(ctx, rt, userInput, cfg.AttachmentPath)
		sess.AddMessage(userMsg)
		sess.AddAttachedFile(attachedPath)

		if cfg.OutputJSON {
			for event := range rt.RunStream(ctx, sess) {
				switch e := event.(type) {
				case *runtime.ToolCallConfirmationEvent:
					if !cfg.AutoApprove {
						rt.Resume(ctx, runtime.ResumeReject(""))
					}
				case *runtime.ElicitationRequestEvent:
					_ = rt.ResumeElicitation(ctx, "decline", nil)
				case *runtime.MaxIterationsReachedEvent:
					switch handleMaxIterationsAutoApprove(cfg.AutoApprove, &autoExtensions, e.MaxIterations) {
					case maxIterContinue:
						rt.Resume(ctx, runtime.ResumeApprove())
					default: // maxIterStop or maxIterPrompt (no interactive prompt in JSON mode)
						rt.Resume(ctx, runtime.ResumeReject(""))
						return nil
					}
				case *runtime.ErrorEvent:
					return fmt.Errorf("%s", e.Error)
				}

				buf, err := json.Marshal(event)
				if err != nil {
					return err
				}
				out.Println(string(buf))
			}

			return nil
		}

		firstLoop := true
		lastAgent := rt.CurrentAgentName()
		var lastConfirmedToolCallID string
		for event := range rt.RunStream(ctx, sess) {
			agentName := event.GetAgentName()
			if agentName != "" && (firstLoop || lastAgent != agentName) {
				if !firstLoop {
					out.Println()
				}
				out.PrintAgentName(agentName)
				firstLoop = false
				lastAgent = agentName
			}
			switch e := event.(type) {
			case *runtime.AgentChoiceEvent:
				out.Print(e.Content)
			case *runtime.AgentChoiceReasoningEvent:
				out.Print(e.Content)
			case *runtime.ToolCallConfirmationEvent:
				result := out.PrintToolCallWithConfirmation(ctx, e.ToolCall, rd)
				// If interrupted, skip resuming; the runtime will notice context cancellation and stop
				if ctx.Err() != nil {
					continue
				}
				lastConfirmedToolCallID = e.ToolCall.ID // Store the ID to avoid duplicate printing
				switch result {
				case ConfirmationApprove:
					rt.Resume(ctx, runtime.ResumeApprove())
				case ConfirmationApproveSession:
					sess.ToolsApproved = true
					rt.Resume(ctx, runtime.ResumeApproveSession())
				case ConfirmationReject:
					rt.Resume(ctx, runtime.ResumeReject(""))
					lastConfirmedToolCallID = "" // Clear on reject since tool won't execute
				case ConfirmationAbort:
					// Stop the agent loop immediately
					cancel()
					continue
				}
			case *runtime.ToolCallEvent:
				if cfg.HideToolCalls {
					continue
				}
				// Only print if this wasn't already shown during confirmation
				if e.ToolCall.ID != lastConfirmedToolCallID {
					out.PrintToolCall(e.ToolCall)
				}
			case *runtime.ToolCallResponseEvent:
				if cfg.HideToolCalls {
					continue
				}
				out.PrintToolCallResponse(e.ToolDefinition.Name, e.Response)
				// Clear the confirmed ID after the tool completes
				if e.ToolCallID == lastConfirmedToolCallID {
					lastConfirmedToolCallID = ""
				}
			case *runtime.ErrorEvent:
				lowerErr := strings.ToLower(e.Error)
				if strings.Contains(lowerErr, "context cancel") && ctx.Err() != nil { // treat Ctrl+C cancellations as non-errors
					lastErr = nil
				} else {
					lastErr = fmt.Errorf("%s", e.Error)
					out.PrintError(lastErr)
				}
			case *runtime.MaxIterationsReachedEvent:
				switch handleMaxIterationsAutoApprove(cfg.AutoApprove, &autoExtensions, e.MaxIterations) {
				case maxIterContinue:
					rt.Resume(ctx, runtime.ResumeApprove())
				case maxIterStop:
					rt.Resume(ctx, runtime.ResumeReject(""))
					return nil
				case maxIterPrompt:
					result := out.PromptMaxIterationsContinue(ctx, e.MaxIterations)
					switch result {
					case ConfirmationApprove:
						rt.Resume(ctx, runtime.ResumeApprove())
					case ConfirmationReject:
						rt.Resume(ctx, runtime.ResumeReject(""))
						return nil
					case ConfirmationAbort:
						rt.Resume(ctx, runtime.ResumeReject(""))
						return nil
					}
				}
			case *runtime.ElicitationRequestEvent:
				serverURL, ok := e.Meta["cagent/server_url"].(string)
				if !ok || serverURL == "" {
					slog.WarnContext(ctx, "Skipping elicitation: missing or invalid server_url (non-interactive session?)")
					_ = rt.ResumeElicitation(ctx, "decline", nil)
					return nil
				}

				result := out.PromptOAuthAuthorization(ctx, serverURL)

				if ctx.Err() != nil {
					return ctx.Err()
				}

				switch result {
				case ConfirmationApprove:
					_ = rt.ResumeElicitation(ctx, "accept", nil)
				case ConfirmationReject:
					_ = rt.ResumeElicitation(ctx, "decline", nil)
					return errors.New("OAuth authorization rejected by user")
				}
			}
		}

		// Wrap runtime errors to prevent duplicate error messages and usage display
		if lastErr != nil {
			return RuntimeError{Err: lastErr}
		}
		return nil
	}

	switch {
	case len(userMessages) == 1 && userMessages[0] == "-":
		// Single "-" argument: read from stdin
		buf, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("failed to read from stdin: %w", err)
		}

		if err := oneLoop(string(buf), os.Stdin); err != nil {
			return err
		}
	case len(userMessages) > 0:
		// One or more messages: multi-turn conversation
		for _, msg := range userMessages {
			if err := oneLoop(msg, os.Stdin); err != nil {
				return err
			}
		}
	case !isatty.IsTerminal(os.Stdin.Fd()):
		// Stdin is not a terminal: read all input from stdin
		buf, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("failed to read from stdin: %w", err)
		}

		if err := oneLoop(string(buf), os.Stdin); err != nil {
			return err
		}
	default:
		// No messages: interactive prompt loop
		out.PrintWelcomeMessage(cfg.AppName)
		firstQuestion := true
		for {
			if !firstQuestion {
				out.Println()
				out.Println()
			}
			out.Print("> ")
			firstQuestion = false

			line, err := input.ReadLine(ctx, os.Stdin)
			if err != nil {
				return err
			}

			if err := oneLoop(line, os.Stdin); err != nil {
				return err
			}
		}
	}

	// Wrap runtime errors to prevent duplicate error messages and usage display
	if lastErr != nil {
		return RuntimeError{Err: lastErr}
	}
	return nil
}

// PrepareUserMessage resolves commands, parses /attach directives, and creates
// a user message with optional image attachment. This is the common flow for
// both TUI and CLI modes.
//
// Parameters:
//   - ctx: context for command resolution
//   - rt: runtime for command resolution
//   - userInput: the raw user input (may contain /commands and /attach directives)
//   - globalAttachPath: attachment path from --attach flag (can be empty)
//
// Returns the prepared session.Message ready to be added to the session, plus
// the absolute path of the file that was actually attached (empty when no
// attachment was used). Callers should pass that path to
// session.Session.AddAttachedFile so sub-agents inherit the file context.
func PrepareUserMessage(ctx context.Context, rt runtime.Runtime, userInput, globalAttachPath string) (*session.Message, string) {
	// Resolve any /command to its prompt text
	resolvedContent := runtime.ResolveCommand(ctx, rt, userInput)

	// Parse for /attach commands in the message
	messageText, attachPath := ParseAttachCommand(resolvedContent)

	// Use either the per-message attachment or the global one
	finalAttachPath := cmp.Or(attachPath, globalAttachPath)

	return CreateUserMessageWithAttachment(ctx, messageText, finalAttachPath)
}

// ParseAttachCommand parses user input for /attach commands
// Returns the message text (with /attach commands removed) and the attachment path
func ParseAttachCommand(userInput string) (messageText, attachPath string) {
	lines := strings.Split(userInput, "\n")
	var messageLines []string

	for _, line := range lines {
		// Look for /attach anywhere in the line
		attachIndex := strings.Index(line, "/attach ")
		if attachIndex != -1 {
			// Extract the part before /attach
			beforeAttach := line[:attachIndex]

			// Extract the part after /attach (starting after "/attach ")
			afterAttachStart := attachIndex + 8 // Length of "/attach "
			if afterAttachStart < len(line) {
				afterAttach := line[afterAttachStart:]

				// Split on spaces to get the file path (first token) and any remaining text
				tokens := strings.Fields(afterAttach)
				if len(tokens) > 0 {
					attachPath = tokens[0]

					// Reconstruct the line with /attach and file path removed
					var remainingText string
					if len(tokens) > 1 {
						remainingText = strings.Join(tokens[1:], " ")
					}

					// Combine the text before /attach and any text after the file path
					var parts []string
					if strings.TrimSpace(beforeAttach) != "" {
						parts = append(parts, strings.TrimSpace(beforeAttach))
					}
					if remainingText != "" {
						parts = append(parts, remainingText)
					}
					reconstructedLine := strings.Join(parts, " ")
					if reconstructedLine != "" {
						messageLines = append(messageLines, reconstructedLine)
					}
				}
			}
		} else {
			// Keep lines without /attach commands
			messageLines = append(messageLines, line)
		}
	}

	// Join the message lines back together
	messageText = strings.TrimSpace(strings.Join(messageLines, "\n"))
	return messageText, attachPath
}

// CreateUserMessageWithAttachment creates a user message with optional file attachment.
// All attachment processing (MIME detection, image resize, text inlining) is delegated
// to [chat.ProcessAttachment], which runs once at message-assembly time.
//
// Returns the prepared session.Message and the absolute path of the file that
// was actually attached. The returned path is empty when no attachment was
// produced (no path supplied, file unreadable, type unsupported, file too
// large to inline, etc.). Callers should record successful attachments via
// session.Session.AddAttachedFile so sub-agents inherit the file context.
func CreateUserMessageWithAttachment(ctx context.Context, userContent, attachmentPath string) (*session.Message, string) {
	// noAttachment returns the message without any attachment.
	noAttachment := func() (*session.Message, string) {
		return session.UserMessage(userContent), ""
	}

	if attachmentPath == "" {
		return noAttachment()
	}

	absPath, err := filepath.Abs(attachmentPath)
	if err != nil {
		slog.WarnContext(ctx, "Failed to get absolute path for attachment", "path", attachmentPath, "error", err)
		return noAttachment()
	}

	fi, err := os.Stat(absPath)
	if err != nil {
		slog.WarnContext(ctx, "Attachment file not accessible", "path", absPath, "error", err)
		return noAttachment()
	}

	// Ensure we have some text content when attaching a file.
	textContent := cmp.Or(strings.TrimSpace(userContent), "Please analyze this attached file.")

	multiContent := []chat.MessagePart{
		{Type: chat.MessagePartTypeText, Text: textContent},
	}

	switch {
	case chat.IsTextFile(absPath):
		// Text files are inlined directly as text content.
		if fi.Size() > chat.MaxInlineFileSize {
			slog.WarnContext(ctx, "Attachment text file too large to inline", "path", absPath, "size", fi.Size())
			return noAttachment()
		}
		content, err := chat.ReadFileForInline(absPath)
		if err != nil {
			slog.WarnContext(ctx, "Failed to read attachment file", "path", absPath, "error", err)
			return noAttachment()
		}
		multiContent = append(multiContent, chat.MessagePart{
			Type: chat.MessagePartTypeText,
			Text: content,
		})

	default:
		// Binary files (images, PDFs, etc.) — delegate to ProcessAttachment.
		if !chat.IsSupportedMimeType(chat.DetectMimeType(absPath)) {
			slog.WarnContext(ctx, "Unsupported attachment file type", "path", absPath)
			return noAttachment()
		}
		doc, _, procErr := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
			Type: chat.MessagePartTypeFile,
			File: &chat.MessageFile{Path: absPath},
		})
		if procErr != nil {
			slog.WarnContext(ctx, "Failed to process attachment", "path", absPath, "error", procErr)
			return noAttachment()
		}
		multiContent = append(multiContent, chat.MessagePart{
			Type:     chat.MessagePartTypeDocument,
			Document: &doc,
		})
	}

	return session.UserMessage(textContent, multiContent...), absPath
}
