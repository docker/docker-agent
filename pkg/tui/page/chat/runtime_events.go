package chat

import (
	"fmt"
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/sound"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/types"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// Runtime Event Handling
//
// This file maps runtime events to UI updates, following the Elm Architecture
// pattern of explicit event-to-update mappings. Events are organized by category:
//
// Stream Lifecycle:
//   - StreamStartedEvent  → Start spinners, set pending response
//   - StreamStoppedEvent  → Stop spinners, process queue, maybe exit
//
// Content Events:
//   - AgentChoiceEvent         → Append text to message
//   - AgentChoiceReasoningEvent → Append reasoning block
//   - UserMessageEvent         → Replace loading with user message
//
// Tool Events:
//   - PartialToolCallEvent      → Show tool call in progress
//   - ToolCallEvent             → Tool execution started
//   - ToolCallConfirmationEvent → Show confirmation dialog
//   - ToolCallResponseEvent     → Show tool result
//
// Sidebar Updates (forwarded):
//   - TokenUsageEvent, AgentInfoEvent, TeamInfoEvent, etc.
//
// Dialogs:
//   - MaxIterationsReachedEvent → Show max iterations dialog
//   - ElicitationRequestEvent   → Show elicitation/OAuth dialog

// handleRuntimeEvent processes runtime events and returns the appropriate command.
// Returns (handled, cmd) where handled indicates if the event was processed.
//
// The switch is organized by event category for clarity.
func (p *chatPage) handleRuntimeEvent(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	// ===== Error and Warning Events =====
	case *runtime.ErrorEvent:
		if userconfig.Get().GetSound() {
			sound.Play(sound.Failure)
		}
		return true, p.messages.AddErrorMessage(msg.Error)

	case *runtime.WarningEvent:
		return true, notification.WarningCmd(msg.Message)

	case *runtime.ModelFallbackEvent:
		// Update sidebar with the fallback model immediately so it reflects the switch
		sidebarCmd := p.sidebar.SetAgentInfo(msg.AgentName, msg.FallbackModel, "")
		// Notify user when switching to a fallback model, include the reason
		fallbackMsg := fmt.Sprintf("Model %s failed (%s), switching to %s", msg.FailedModel, msg.Reason, msg.FallbackModel)
		return true, tea.Batch(sidebarCmd, notification.WarningCmd(fallbackMsg))

	// ===== Stream Lifecycle Events =====
	case *runtime.StreamStartedEvent:
		return true, p.handleStreamStarted(msg)

	case *runtime.StreamStoppedEvent:
		return true, p.handleStreamStopped(msg)

	// ===== Content Events =====
	case *runtime.UserMessageEvent:
		return true, p.messages.ReplaceLoadingWithUser(msg.Message, msg.SessionPosition)

	case *runtime.AgentChoiceEvent:
		return true, p.handleAgentChoice(msg)

	case *runtime.AgentChoiceReasoningEvent:
		return true, p.handleAgentChoiceReasoning(msg)

	case *runtime.ShellOutputEvent:
		return true, p.messages.AddShellOutputMessage(msg.Output)

	// ===== Tool Events =====
	case *runtime.PartialToolCallEvent:
		return true, p.handlePartialToolCall(msg)

	case *runtime.ToolCallEvent:
		return true, p.handleToolCall(msg)

	case *runtime.ToolCallConfirmationEvent:
		return true, p.handleToolCallConfirmation(msg)

	case *runtime.ToolCallResponseEvent:
		return true, p.handleToolCallResponse(msg)

	// ===== Sidebar Info Events (forwarded) =====
	case *runtime.TokenUsageEvent:
		p.handleTokenUsage(msg)
		return true, nil

	case *runtime.AgentInfoEvent:
		sidebarCmd := p.sidebar.SetAgentInfo(msg.AgentName, msg.Model, msg.Description)
		p.messages.AddWelcomeMessage(msg.WelcomeMessage)
		return true, sidebarCmd

	case *runtime.TeamInfoEvent:
		p.sidebar.SetTeamInfo(msg.AvailableAgents)
		return true, nil

	case *runtime.AgentSwitchingEvent:
		p.sidebar.SetAgentSwitching(msg.Switching)
		return true, nil

	case *runtime.ToolsetInfoEvent:
		p.sidebar.SetSkillsInfo(len(p.app.CurrentAgentSkills()))
		return true, p.forwardToSidebar(msg)

	case *runtime.SessionTitleEvent:
		return true, p.forwardToSidebar(msg)

	case *runtime.SessionCompactionEvent:
		if msg.Status == "completed" {
			return true, tea.Batch(
				p.setWorking(false),
				p.setPendingResponse(false),
				notification.SuccessCmd("Session compacted successfully."),
				p.messages.ScrollToBottom(),
			)
		}
		return true, nil

	// ===== RAG Indexing Events (forwarded to sidebar) =====
	case *runtime.RAGIndexingStartedEvent,
		*runtime.RAGIndexingProgressEvent,
		*runtime.RAGIndexingCompletedEvent:
		return true, p.forwardToSidebar(msg)

	// ===== Dialog Events =====
	case *runtime.MaxIterationsReachedEvent:
		return true, p.handleMaxIterationsReached(msg)

	case *runtime.ElicitationRequestEvent:
		return true, p.handleElicitationRequest(msg)
	}

	return false, nil
}

// forwardToSidebar forwards a message to the sidebar and returns the resulting command.
func (p *chatPage) forwardToSidebar(msg tea.Msg) tea.Cmd {
	slog.Debug("Forwarding event to sidebar", "event_type", fmt.Sprintf("%T", msg))
	model, cmd := p.sidebar.Update(msg)
	p.sidebar = model.(sidebar.Model)
	return cmd
}

// handleTokenUsage updates sidebar and session with token usage data.
// This handler performs side effects only and returns no command.
func (p *chatPage) handleTokenUsage(msg *runtime.TokenUsageEvent) {
	p.sidebar.SetTokenUsage(msg)
	if msg.Usage != nil {
		if sess := p.app.Session(); sess != nil {
			// Only update the parent session's token counts when the event
			// belongs to this session. Sub-sessions emit their own
			// TokenUsageEvents with a different SessionID; writing those
			// values into the parent would overwrite the parent's own
			// context-tracking counters.
			if msg.SessionID == "" || msg.SessionID == sess.ID {
				sess.InputTokens = msg.Usage.InputTokens
				sess.OutputTokens = msg.Usage.OutputTokens
			}

			// Track per-message usage for /cost dialog
			if msg.Usage.LastMessage != nil {
				sess.AddMessageUsageRecord(
					msg.AgentName,
					msg.Usage.LastMessage.Model,
					msg.Usage.LastMessage.Cost,
					&msg.Usage.LastMessage.Usage,
				)
			}
		}
	}
}

func (p *chatPage) handleStreamStarted(msg *runtime.StreamStartedEvent) tea.Cmd {
	slog.Debug("handleStreamStarted called", "agent", msg.AgentName, "session_id", msg.SessionID)
	p.streamCancelled = false
	p.streamDepth++
	p.streamStartTime = time.Now()
	spinnerCmd := p.setWorking(true)
	pendingCmd := p.setPendingResponse(true)
	sidebarCmd := p.forwardToSidebar(msg)
	return tea.Batch(pendingCmd, spinnerCmd, sidebarCmd)
}

func (p *chatPage) handleAgentChoice(msg *runtime.AgentChoiceEvent) tea.Cmd {
	if p.streamCancelled {
		return nil
	}
	// Track that we've received assistant content
	p.hasReceivedAssistantContent = true
	// Clear pending response indicator - first chunk has arrived
	p.setPendingResponse(false)
	return p.messages.AppendToLastMessage(msg.AgentName, msg.Content)
}

func (p *chatPage) handleAgentChoiceReasoning(msg *runtime.AgentChoiceReasoningEvent) tea.Cmd {
	if p.streamCancelled {
		return nil
	}
	p.setPendingResponse(false)
	return p.messages.AppendReasoning(msg.AgentName, msg.Content)
}

func (p *chatPage) handleStreamStopped(msg *runtime.StreamStoppedEvent) tea.Cmd {
	slog.Debug("handleStreamStopped called",
		"agent", msg.AgentName,
		"session_id", msg.SessionID,
		"reason", msg.Reason,
		"should_exit", p.app.ShouldExitAfterFirstResponse(),
		"has_content", p.hasReceivedAssistantContent,
		"stream_depth", p.streamDepth)

	if p.streamDepth > 0 {
		p.streamDepth--
	}

	sidebarCmd := p.forwardToSidebar(msg)

	// Sub-agent stream stopped — the parent is still running, so only
	// forward to the sidebar and keep the working/cancel state intact.
	// Without this guard, pressing Esc after a sub-agent completes but
	// while the parent continues would have no effect.
	if p.streamDepth > 0 {
		return tea.Batch(p.messages.ScrollToBottom(), sidebarCmd)
	}

	// Outermost stream stopped — fully clean up.
	// Only play the success sound when the stream completed normally.
	// Errors already trigger a failure sound via ErrorEvent, and
	// user-initiated cancels don't warrant a chime.
	if userconfig.Get().GetSound() && isSuccessfulStop(msg.Reason) {
		duration := time.Since(p.streamStartTime)
		threshold := time.Duration(userconfig.Get().GetSoundThreshold()) * time.Second
		if duration >= threshold {
			sound.Play(sound.Success)
		}
	}
	p.msgCancel = nil
	p.streamCancelled = false
	spinnerCmd := p.setWorking(false)
	p.setPendingResponse(false)
	queueCmd := p.processNextQueuedMessage()

	var exitCmd tea.Cmd
	if p.app.ShouldExitAfterFirstResponse() && p.hasReceivedAssistantContent {
		slog.Debug("Exit after first response triggered, scheduling delayed exit")
		exitCmd = tea.Tick(50*time.Millisecond, func(time.Time) tea.Msg {
			return msgtypes.ExitAfterFirstResponseMsg{}
		})
	}

	return tea.Batch(p.messages.ScrollToBottom(), spinnerCmd, sidebarCmd, queueCmd, exitCmd)
}

// handlePartialToolCall processes partial tool call events by rendering each
// tool call as it streams in. The tool call appears with its name and a static
// "pending" indicator (not animated) to show it's receiving data.
func (p *chatPage) handlePartialToolCall(msg *runtime.PartialToolCallEvent) tea.Cmd {
	p.setPendingResponse(false)
	var toolDef tools.Tool
	if msg.ToolDefinition != nil {
		toolDef = *msg.ToolDefinition
	}
	toolCmd := p.messages.AddOrUpdateToolCall(msg.AgentName, msg.ToolCall, toolDef, types.ToolStatusPending)
	return tea.Batch(toolCmd, p.messages.ScrollToBottom())
}

func (p *chatPage) handleToolCallConfirmation(msg *runtime.ToolCallConfirmationEvent) tea.Cmd {
	spinnerCmd := p.setWorking(false)
	toolCmd := p.messages.AddOrUpdateToolCall(msg.AgentName, msg.ToolCall, msg.ToolDefinition, types.ToolStatusConfirmation)
	dialogCmd := core.CmdHandler(dialog.OpenDialogMsg{
		Model:            dialog.NewToolConfirmationDialog(msg, p.sessionState),
		OriginatingEvent: msg,
	})
	return tea.Batch(toolCmd, p.messages.ScrollToBottom(), spinnerCmd, dialogCmd)
}

func (p *chatPage) handleToolCall(msg *runtime.ToolCallEvent) tea.Cmd {
	p.setPendingResponse(false)
	spinnerCmd := p.setWorking(true)
	sidebarCmd := p.forwardToSidebar(msg)
	toolCmd := p.messages.AddOrUpdateToolCall(msg.AgentName, msg.ToolCall, msg.ToolDefinition, types.ToolStatusRunning)
	return tea.Batch(toolCmd, p.messages.ScrollToBottom(), spinnerCmd, sidebarCmd)
}

func (p *chatPage) handleToolCallResponse(msg *runtime.ToolCallResponseEvent) tea.Cmd {
	spinnerCmd := p.setWorking(true)
	sidebarCmd := p.forwardToSidebar(msg)

	status := types.ToolStatusCompleted
	if msg.Result.IsError {
		status = types.ToolStatusError
	}
	toolCmd := p.messages.AddToolResult(msg, status)

	// Update todo sidebar if this is a todo tool
	if msg.ToolDefinition.Category == "todo" && !msg.Result.IsError {
		_ = p.sidebar.SetTodos(msg.Result)
	}

	return tea.Batch(toolCmd, p.messages.ScrollToBottom(), spinnerCmd, sidebarCmd)
}

func (p *chatPage) handleMaxIterationsReached(msg *runtime.MaxIterationsReachedEvent) tea.Cmd {
	spinnerCmd := p.setWorking(false)
	dialogCmd := core.CmdHandler(dialog.OpenDialogMsg{
		Model:            dialog.NewMaxIterationsDialog(msg.MaxIterations, p.app),
		OriginatingEvent: msg,
	})
	return tea.Batch(spinnerCmd, dialogCmd)
}

func (p *chatPage) handleElicitationRequest(msg *runtime.ElicitationRequestEvent) tea.Cmd {
	spinnerCmd := p.setWorking(false)

	// Check if this is an OAuth flow by looking at the meta type
	// Guard against nil Meta map to prevent panic
	if msg.Meta != nil {
		if elicitationType, ok := msg.Meta["cagent/type"].(string); ok && elicitationType == "oauth_flow" {
			// OAuth flow - show the OAuth authorization dialog
			var serverURL string
			if url, ok := msg.Meta["cagent/server_url"].(string); ok {
				serverURL = url
			}
			dialogCmd := core.CmdHandler(dialog.OpenDialogMsg{
				Model:            dialog.NewOAuthAuthorizationDialog(serverURL, p.app),
				OriginatingEvent: msg,
			})
			return tea.Batch(spinnerCmd, dialogCmd)
		}
	}

	// Check elicitation mode
	switch msg.Mode {
	case "url":
		// URL-based elicitation - show URL dialog
		dialogCmd := core.CmdHandler(dialog.OpenDialogMsg{
			Model:            dialog.NewURLElicitationDialog(msg.Message, msg.URL),
			OriginatingEvent: msg,
		})
		return tea.Batch(spinnerCmd, dialogCmd)

	default:
		// Form-based elicitation (default) - show form dialog
		dialogCmd := core.CmdHandler(dialog.OpenDialogMsg{
			Model:            dialog.NewElicitationDialog(msg.Message, msg.Schema, msg.Meta),
			OriginatingEvent: msg,
		})
		return tea.Batch(spinnerCmd, dialogCmd)
	}
}

// isSuccessfulStop returns true when the stream reason indicates a
// normal completion that warrants the success sound. Empty reason
// (e.g. cache hits, early exits before a turn runs) is treated as
// success to preserve backward compatibility.
func isSuccessfulStop(reason string) bool {
	switch reason {
	case "", "normal", "continue", "steered":
		return true
	default:
		return false
	}
}
