package runtime

import (
	"cmp"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

type Event interface {
	GetAgentName() string
}

// SessionScoped is implemented by events that belong to a specific session.
// The PersistentRuntime uses this to filter out sub-session events that
// should not be persisted into the parent session's history.
type SessionScoped interface {
	GetSessionID() string
}

// AgentContext carries optional agent attribution and timestamp for an event.
type AgentContext struct {
	AgentName string    `json:"agent_name,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// GetAgentName returns the agent name for events embedding AgentContext.
func (a AgentContext) GetAgentName() string { return a.AgentName }

// newAgentContext creates a new AgentContext with the current timestamp.
func newAgentContext(agentName string) AgentContext {
	return AgentContext{AgentName: agentName, Timestamp: time.Now()}
}

// UserMessageEvent is sent when a user message is received
type UserMessageEvent struct {
	AgentContext

	Type            string             `json:"type"`
	Message         string             `json:"message"`
	MultiContent    []chat.MessagePart `json:"multi_content,omitempty"`
	SessionID       string             `json:"session_id"`
	SessionPosition int                `json:"session_position"` // Index in session.Messages, -1 if unknown
}

func UserMessage(message, sessionID string, multiContent []chat.MessagePart, sessionPos ...int) Event {
	pos := -1
	if len(sessionPos) > 0 {
		pos = sessionPos[0]
	}
	return &UserMessageEvent{
		Type:            "user_message",
		Message:         message,
		MultiContent:    multiContent,
		SessionID:       sessionID,
		SessionPosition: pos,
		AgentContext:    newAgentContext(""),
	}
}

func (e *UserMessageEvent) GetSessionID() string { return e.SessionID }

// PartialToolCallEvent is sent when a tool call is first received (partial/complete)
type PartialToolCallEvent struct {
	AgentContext

	Type           string         `json:"type"`
	ToolCall       tools.ToolCall `json:"tool_call"`
	ToolDefinition *tools.Tool    `json:"tool_definition,omitempty"`
}

func PartialToolCall(toolCall tools.ToolCall, toolDefinition tools.Tool, agentName string) Event {
	var toolDef *tools.Tool
	if toolDefinition.Name != "" {
		def := toolDefinition
		toolDef = &def
	}
	return &PartialToolCallEvent{
		Type:           "partial_tool_call",
		ToolCall:       toolCall,
		ToolDefinition: toolDef,
		AgentContext:   newAgentContext(agentName),
	}
}

// ToolCallEvent is sent when a tool call is received
type ToolCallEvent struct {
	AgentContext

	Type           string         `json:"type"`
	ToolCall       tools.ToolCall `json:"tool_call"`
	ToolDefinition tools.Tool     `json:"tool_definition"`
}

func ToolCall(toolCall tools.ToolCall, toolDefinition tools.Tool, agentName string) Event {
	return &ToolCallEvent{
		Type:           "tool_call",
		ToolCall:       toolCall,
		ToolDefinition: toolDefinition,
		AgentContext:   newAgentContext(agentName),
	}
}

type ToolCallConfirmationEvent struct {
	AgentContext

	Type           string         `json:"type"`
	ToolCall       tools.ToolCall `json:"tool_call"`
	ToolDefinition tools.Tool     `json:"tool_definition"`
}

func ToolCallConfirmation(toolCall tools.ToolCall, toolDefinition tools.Tool, agentName string) Event {
	return &ToolCallConfirmationEvent{
		Type:           "tool_call_confirmation",
		ToolCall:       toolCall,
		ToolDefinition: toolDefinition,
		AgentContext:   newAgentContext(agentName),
	}
}

type ToolCallResponseEvent struct {
	AgentContext

	Type           string                `json:"type"`
	ToolCallID     string                `json:"tool_call_id"`
	ToolDefinition tools.Tool            `json:"tool_definition"`
	Response       string                `json:"response"`
	Result         *tools.ToolCallResult `json:"result,omitempty"`
}

func ToolCallResponse(toolCallID string, toolDefinition tools.Tool, result *tools.ToolCallResult, response, agentName string) Event {
	return &ToolCallResponseEvent{
		Type:           "tool_call_response",
		Response:       response,
		Result:         result,
		ToolCallID:     toolCallID,
		ToolDefinition: toolDefinition,
		AgentContext:   newAgentContext(agentName),
	}
}

type StreamStartedEvent struct {
	AgentContext

	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
}

func StreamStarted(sessionID, agentName string) Event {
	return &StreamStartedEvent{
		Type:         "stream_started",
		SessionID:    sessionID,
		AgentContext: newAgentContext(agentName),
	}
}

func (e *StreamStartedEvent) GetSessionID() string { return e.SessionID }

type AgentChoiceEvent struct {
	AgentContext

	Type      string `json:"type"`
	Content   string `json:"content"`
	SessionID string `json:"session_id,omitempty"`
}

func (e *AgentChoiceEvent) GetSessionID() string { return e.SessionID }

func AgentChoice(agentName, sessionID, content string) Event {
	return &AgentChoiceEvent{
		Type:         "agent_choice",
		Content:      content,
		SessionID:    sessionID,
		AgentContext: newAgentContext(agentName),
	}
}

type AgentChoiceReasoningEvent struct {
	AgentContext

	Type      string `json:"type"`
	Content   string `json:"content"`
	SessionID string `json:"session_id,omitempty"`
}

func (e *AgentChoiceReasoningEvent) GetSessionID() string { return e.SessionID }

func AgentChoiceReasoning(agentName, sessionID, content string) Event {
	return &AgentChoiceReasoningEvent{
		Type:         "agent_choice_reasoning",
		Content:      content,
		SessionID:    sessionID,
		AgentContext: newAgentContext(agentName),
	}
}

// ErrorCode constants classify errors so external consumers (boards,
// dashboards) can react programmatically without parsing free-form messages.
const (
	ErrorCodeModelError      = "model_error"
	ErrorCodeRateLimited     = "rate_limited"
	ErrorCodeContextExceeded = "context_exceeded"
	ErrorCodeToolFailed      = "tool_failed"
	ErrorCodeHookBlocked     = "hook_blocked"
	ErrorCodeLoopDetected    = "loop_detected"
)

type ErrorEvent struct {
	AgentContext

	Type  string `json:"type"`
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

func Error(msg string) Event {
	return &ErrorEvent{
		Type:  "error",
		Error: msg,
	}
}

func ErrorWithCode(code, msg string) Event {
	return &ErrorEvent{
		Type:  "error",
		Error: msg,
		Code:  code,
	}
}

type ShellOutputEvent struct {
	AgentContext

	Type   string `json:"type"`
	Output string `json:"output"`
}

func ShellOutput(output string) Event {
	return &ShellOutputEvent{
		Type:         "shell",
		Output:       output,
		AgentContext: newAgentContext(""),
	}
}

type WarningEvent struct {
	AgentContext

	Type    string `json:"type"`
	Message string `json:"message"`
}

func Warning(message, agentName string) Event {
	return &WarningEvent{
		Type:         "warning",
		Message:      message,
		AgentContext: newAgentContext(agentName),
	}
}

// ModelFallbackEvent is emitted when the runtime switches to a fallback model
// after the previous model in the chain fails. This can happen due to:
// - Retryable errors (5xx, timeouts) after exhausting retries
// - Non-retryable errors (429, 4xx) which skip retries and move immediately to fallback
type ModelFallbackEvent struct {
	AgentContext

	Type          string `json:"type"`
	FailedModel   string `json:"failed_model"`
	FallbackModel string `json:"fallback_model"`
	Reason        string `json:"reason"`
	Attempt       int    `json:"attempt"`      // Current attempt number (1-indexed)
	MaxAttempts   int    `json:"max_attempts"` // Total attempts allowed for this model
}

// ModelFallback creates a new ModelFallbackEvent.
func ModelFallback(agentName, failedModel, fallbackModel, reason string, attempt, maxAttempts int) Event {
	return &ModelFallbackEvent{
		Type:          "model_fallback",
		FailedModel:   failedModel,
		FallbackModel: fallbackModel,
		Reason:        reason,
		Attempt:       attempt,
		MaxAttempts:   maxAttempts,
		AgentContext:  AgentContext{AgentName: agentName},
	}
}

type TokenUsageEvent struct {
	AgentContext

	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Usage     *Usage `json:"usage"`
}

type Usage struct {
	InputTokens   int64         `json:"input_tokens"`
	OutputTokens  int64         `json:"output_tokens"`
	ContextLength int64         `json:"context_length"`
	ContextLimit  int64         `json:"context_limit"`
	Cost          float64       `json:"cost"`
	LastMessage   *MessageUsage `json:"last_message,omitempty"`
}

// MessageUsage contains per-message usage data to include in TokenUsageEvent.
// It embeds chat.Usage and adds Cost, Model, and FinishReason fields.
type MessageUsage struct {
	chat.Usage

	Cost         float64
	Model        string
	FinishReason chat.FinishReason `json:"finish_reason,omitempty"`
}

// NewTokenUsageEvent creates a TokenUsageEvent with the given usage data.
func NewTokenUsageEvent(sessionID, agentName string, usage *Usage) Event {
	return &TokenUsageEvent{
		Type:         "token_usage",
		SessionID:    sessionID,
		Usage:        usage,
		AgentContext: newAgentContext(agentName),
	}
}

// GetSessionID makes TokenUsageEvent satisfy [SessionScoped] so the
// observer fan-out can drop sub-session events without each observer
// re-implementing the check.
func (e *TokenUsageEvent) GetSessionID() string { return e.SessionID }

// SessionUsage builds a Usage from the session's current token counts, the
// model's context limit, and the session's own cost.
func SessionUsage(sess *session.Session, contextLimit int64) *Usage {
	return &Usage{
		InputTokens:   sess.InputTokens,
		OutputTokens:  sess.OutputTokens,
		ContextLength: sess.InputTokens + sess.OutputTokens,
		ContextLimit:  contextLimit,
		Cost:          sess.OwnCost(),
	}
}

type SessionTitleEvent struct {
	AgentContext

	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
}

func SessionTitle(sessionID, title string) Event {
	return &SessionTitleEvent{
		Type:      "session_title",
		SessionID: sessionID,
		Title:     title,
	}
}

func (e *SessionTitleEvent) GetSessionID() string { return e.SessionID }

type SessionSummaryEvent struct {
	AgentContext

	Type           string `json:"type"`
	SessionID      string `json:"session_id"`
	Summary        string `json:"summary"`
	FirstKeptEntry int    `json:"first_kept_entry,omitempty"`
}

func SessionSummary(sessionID, summary, agentName string, firstKeptEntry int) Event {
	return &SessionSummaryEvent{
		Type:           "session_summary",
		SessionID:      sessionID,
		Summary:        summary,
		FirstKeptEntry: firstKeptEntry,
		AgentContext:   newAgentContext(agentName),
	}
}

func (e *SessionSummaryEvent) GetSessionID() string { return e.SessionID }

type SessionCompactionEvent struct {
	AgentContext

	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
}

func SessionCompaction(sessionID, status, agentName string) Event {
	return &SessionCompactionEvent{
		Type:         "session_compaction",
		SessionID:    sessionID,
		Status:       status,
		AgentContext: newAgentContext(agentName),
	}
}

func (e *SessionCompactionEvent) GetSessionID() string { return e.SessionID }

type StreamStoppedEvent struct {
	AgentContext

	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func StreamStopped(sessionID, agentName, reason string) Event {
	return &StreamStoppedEvent{
		Type:         "stream_stopped",
		SessionID:    sessionID,
		AgentContext: newAgentContext(agentName),
		Reason:       reason,
	}
}

func (e *StreamStoppedEvent) GetSessionID() string { return e.SessionID }

// ElicitationRequestEvent is sent when an elicitation request is received from an MCP server
type ElicitationRequestEvent struct {
	AgentContext

	Type          string         `json:"type"`
	Message       string         `json:"message"`
	Mode          string         `json:"mode,omitempty"` // "form" or "url"
	Schema        any            `json:"schema,omitempty"`
	URL           string         `json:"url,omitempty"`
	ElicitationID string         `json:"elicitation_id,omitempty"`
	Meta          map[string]any `json:"meta,omitempty"`
}

func ElicitationRequest(message, mode string, schema any, url, elicitationID string, meta map[string]any, agentName string) Event {
	return &ElicitationRequestEvent{
		Type:          "elicitation_request",
		Message:       message,
		Mode:          mode,
		Schema:        schema,
		URL:           url,
		ElicitationID: elicitationID,
		Meta:          meta,
		AgentContext:  newAgentContext(agentName),
	}
}

type AuthorizationEvent struct {
	AgentContext

	Type         string                  `json:"type"`
	Confirmation tools.ElicitationAction `json:"confirmation"`
}

func Authorization(confirmation tools.ElicitationAction, agentName string) Event {
	return &AuthorizationEvent{
		Type:         "authorization_event",
		Confirmation: confirmation,
		AgentContext: newAgentContext(agentName),
	}
}

type MaxIterationsReachedEvent struct {
	AgentContext

	Type          string `json:"type"`
	MaxIterations int    `json:"max_iterations"`
}

func MaxIterationsReached(maxIterations int) Event {
	return &MaxIterationsReachedEvent{
		Type:          "max_iterations_reached",
		MaxIterations: maxIterations,
	}
}

// MCPInitStartedEvent is for MCP initialization lifecycle events
type MCPInitStartedEvent struct {
	AgentContext

	Type string `json:"type"`
}

func MCPInitStarted(agentName string) Event {
	return &MCPInitStartedEvent{
		Type:         "mcp_init_started",
		AgentContext: newAgentContext(agentName),
	}
}

type MCPInitFinishedEvent struct {
	AgentContext

	Type string `json:"type"`
}

func MCPInitFinished(agentName string) Event {
	return &MCPInitFinishedEvent{
		Type:         "mcp_init_finished",
		AgentContext: newAgentContext(agentName),
	}
}

// AgentInfoEvent is sent when agent information is available or changes
type AgentInfoEvent struct {
	AgentContext

	Type           string `json:"type"`
	AgentName      string `json:"agent_name"`
	Model          string `json:"model"` // this is in provider/model format (e.g., "openai/gpt-4o")
	Description    string `json:"description"`
	WelcomeMessage string `json:"welcome_message,omitempty"`
}

func AgentInfo(agentName, model, description, welcomeMessage string) Event {
	return &AgentInfoEvent{
		Type:           "agent_info",
		AgentName:      agentName,
		Model:          model,
		Description:    description,
		WelcomeMessage: welcomeMessage,
		AgentContext:   newAgentContext(agentName),
	}
}

// AgentDetails contains information about an agent for display in the sidebar
type AgentDetails struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Provider    string         `json:"provider"`
	Model       string         `json:"model"`
	Commands    types.Commands `json:"commands,omitempty"`
}

// TeamInfoEvent is sent when team information is available
type TeamInfoEvent struct {
	AgentContext

	Type            string         `json:"type"`
	AvailableAgents []AgentDetails `json:"available_agents"`
	CurrentAgent    string         `json:"current_agent"`
}

func TeamInfo(availableAgents []AgentDetails, currentAgent string) Event {
	return &TeamInfoEvent{
		Type:            "team_info",
		AvailableAgents: availableAgents,
		CurrentAgent:    currentAgent,
		AgentContext:    newAgentContext(currentAgent),
	}
}

// AgentSwitchingEvent is sent when agent switching starts or stops
type AgentSwitchingEvent struct {
	AgentContext

	Type      string `json:"type"`
	Switching bool   `json:"switching"`
	FromAgent string `json:"from_agent,omitempty"`
	ToAgent   string `json:"to_agent,omitempty"`
}

func AgentSwitching(switching bool, fromAgent, toAgent string) Event {
	return &AgentSwitchingEvent{
		Type:         "agent_switching",
		Switching:    switching,
		FromAgent:    fromAgent,
		ToAgent:      toAgent,
		AgentContext: newAgentContext(cmp.Or(toAgent, fromAgent)),
	}
}

// ToolsetInfoEvent is sent when toolset information is available
// When Loading is true, more tools may still be loading (e.g., MCP servers starting)
type ToolsetInfoEvent struct {
	AgentContext

	Type           string `json:"type"`
	AvailableTools int    `json:"available_tools"`
	Loading        bool   `json:"loading"`
}

func ToolsetInfo(availableTools int, loading bool, agentName string) Event {
	return &ToolsetInfoEvent{
		Type:           "toolset_info",
		AvailableTools: availableTools,
		Loading:        loading,
		AgentContext:   newAgentContext(agentName),
	}
}

// RAGIndexingStartedEvent is for RAG lifecycle events
type RAGIndexingStartedEvent struct {
	AgentContext

	Type         string `json:"type"`
	RAGName      string `json:"rag_name"`
	StrategyName string `json:"strategy_name"`
}

func RAGIndexingStarted(ragName, strategyName string) Event {
	return &RAGIndexingStartedEvent{
		Type:         "rag_indexing_started",
		RAGName:      ragName,
		StrategyName: strategyName,
		AgentContext: newAgentContext(""),
	}
}

type RAGIndexingProgressEvent struct {
	AgentContext

	Type         string `json:"type"`
	RAGName      string `json:"rag_name"`
	StrategyName string `json:"strategy_name"`
	Current      int    `json:"current"`
	Total        int    `json:"total"`
}

func RAGIndexingProgress(ragName, strategyName string, current, total int, agentName string) Event {
	return &RAGIndexingProgressEvent{
		Type:         "rag_indexing_progress",
		RAGName:      ragName,
		StrategyName: strategyName,
		Current:      current,
		Total:        total,
		AgentContext: newAgentContext(agentName),
	}
}

type RAGIndexingCompletedEvent struct {
	AgentContext

	Type         string `json:"type"`
	RAGName      string `json:"rag_name"`
	StrategyName string `json:"strategy_name"`
}

func RAGIndexingCompleted(ragName, strategyName string) Event {
	return &RAGIndexingCompletedEvent{
		Type:         "rag_indexing_completed",
		RAGName:      ragName,
		StrategyName: strategyName,
		AgentContext: newAgentContext(""),
	}
}

// HookStartedEvent is emitted when a configured hook event begins dispatching.
type HookStartedEvent struct {
	AgentContext

	Type      string          `json:"type"`
	SessionID string          `json:"session_id"`
	HookEvent hooks.EventType `json:"hook_event"`
}

func (e *HookStartedEvent) GetSessionID() string { return e.SessionID }

func HookStarted(event hooks.EventType, sessionID, agentName string) Event {
	return &HookStartedEvent{
		Type:         "hook_started",
		SessionID:    sessionID,
		HookEvent:    event,
		AgentContext: newAgentContext(agentName),
	}
}

// HookFinishedEvent is emitted when a configured hook event completes.
type HookFinishedEvent struct {
	AgentContext

	Type       string          `json:"type"`
	SessionID  string          `json:"session_id"`
	HookEvent  hooks.EventType `json:"hook_event"`
	DurationMs int64           `json:"duration_ms"`
	Allowed    bool            `json:"allowed"`
	Error      string          `json:"error,omitempty"`
	Message    string          `json:"message,omitempty"`
}

func (e *HookFinishedEvent) GetSessionID() string { return e.SessionID }

func HookFinished(event hooks.EventType, sessionID string, result *hooks.Result, dispatchErr error, duration time.Duration, agentName string) Event {
	e := &HookFinishedEvent{
		Type:         "hook_finished",
		SessionID:    sessionID,
		HookEvent:    event,
		DurationMs:   duration.Milliseconds(),
		Allowed:      true,
		AgentContext: newAgentContext(agentName),
	}
	if result != nil {
		e.Allowed = result.Allowed
		e.Message = result.Message
	}
	if dispatchErr != nil {
		e.Allowed = false
		e.Error = dispatchErr.Error()
	}
	return e
}

// HookBlockedEvent is sent when a pre-tool hook blocks a tool call
type HookBlockedEvent struct {
	AgentContext

	Type           string         `json:"type"`
	ToolCall       tools.ToolCall `json:"tool_call"`
	ToolDefinition tools.Tool     `json:"tool_definition"`
	Message        string         `json:"message"`
}

func HookBlocked(toolCall tools.ToolCall, toolDefinition tools.Tool, message, agentName string) Event {
	return &HookBlockedEvent{
		Type:           "hook_blocked",
		ToolCall:       toolCall,
		ToolDefinition: toolDefinition,
		Message:        message,
		AgentContext:   newAgentContext(agentName),
	}
}

// MessageAddedEvent is emitted when a message is added to the session.
// This event is used by the PersistentRuntime wrapper to persist messages.
type MessageAddedEvent struct {
	AgentContext

	Type      string           `json:"type"`
	SessionID string           `json:"session_id"`
	Message   *session.Message `json:"-"`
}

func (e *MessageAddedEvent) GetSessionID() string { return e.SessionID }

func MessageAdded(sessionID string, msg *session.Message, agentName string) Event {
	return &MessageAddedEvent{
		Type:         "message_added",
		SessionID:    sessionID,
		Message:      msg,
		AgentContext: newAgentContext(agentName),
	}
}

// SubSessionCompletedEvent is emitted when a sub-session completes and is added to parent.
// This event is used by the PersistentRuntime wrapper to persist sub-sessions.
type SubSessionCompletedEvent struct {
	AgentContext

	Type            string `json:"type"`
	ParentSessionID string `json:"parent_session_id"`
	SubSession      any    `json:"sub_session"` // *session.Session
}

func SubSessionCompleted(parentSessionID string, subSession any, agentName string) Event {
	return &SubSessionCompletedEvent{
		Type:            "sub_session_completed",
		ParentSessionID: parentSessionID,
		SubSession:      subSession,
		AgentContext:    newAgentContext(agentName),
	}
}

// ConnectionLostEvent is emitted when the connection to the remote server is lost
type ConnectionLostEvent struct {
	AgentContext

	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Attempt int    `json:"attempt"`
}

func ConnectionLost(reason string, attempt int) Event {
	return &ConnectionLostEvent{
		Type:         "connection_lost",
		Reason:       reason,
		Attempt:      attempt,
		AgentContext: newAgentContext(""),
	}
}

// ConnectionRestoredEvent is emitted when the connection to the remote server is restored
type ConnectionRestoredEvent struct {
	AgentContext

	Type    string `json:"type"`
	Attempt int    `json:"attempt"`
}

func ConnectionRestored(attempt int) Event {
	return &ConnectionRestoredEvent{
		Type:         "connection_restored",
		Attempt:      attempt,
		AgentContext: newAgentContext(""),
	}
}
