package telemetry

import (
	"net/http"
	"sync"
	"time"
)

// EVENTS BASE

// StructuredEvent represents a type-safe telemetry event with structured properties
type StructuredEvent interface {
	GetEventType() EventType
	ToStructuredProperties() any
}

// Simplified event system - event types are now just strings in the Event.Event field

// EventType represents the type of telemetry event
type EventType string

const (
	EventTypeCommand EventType = "command"
	EventTypeSession EventType = "session"
	EventTypeToken   EventType = "token"
	EventTypeTool    EventType = "tool"
)

// EventPayload represents a structured telemetry event
type EventPayload struct {
	Event          EventType `json:"event"`
	EventTimestamp int64     `json:"event_timestamp"`
	Source         string    `json:"source"`

	Properties map[string]any `json:"properties,omitempty"`
}

// COMMAND EVENTS

// CommandEvent represents command execution events
type CommandEvent struct {
	Action  string   `json:"action"`
	Args    []string `json:"args,omitempty"`
	Success bool     `json:"success"`
	Error   string   `json:"error,omitempty"`
}

// CommandPayload represents the HTTP payload structure for command events
type CommandPayload struct {
	Action    string   `json:"action"`
	Args      []string `json:"args,omitempty"`
	IsSuccess bool     `json:"is_success"`
	Error     string   `json:"error,omitempty"`
}

func (e *CommandEvent) GetEventType() EventType {
	return EventTypeCommand
}

func (e *CommandEvent) ToStructuredProperties() any {
	return CommandPayload{
		Action:    e.Action,
		Args:      e.Args,
		IsSuccess: e.Success,
		Error:     e.Error,
	}
}

// TOOL EVENTS

// ToolEvent represents tool call events
type ToolEvent struct {
	Action    string `json:"action"`
	ToolName  string `json:"tool_name"`
	SessionID string `json:"session_id"`
	AgentName string `json:"agent_name"`
	Duration  int64  `json:"duration_ms"`
	Success   bool   `json:"success"`
	Error     string `json:"error,omitempty"`
}

// ToolPayload represents the HTTP payload structure for tool events
type ToolPayload struct {
	Action     string `json:"action"`
	SessionID  string `json:"session_id"`
	AgentName  string `json:"agent_name"`
	ToolName   string `json:"tool_name"`
	DurationMs int64  `json:"duration_ms"`
	IsSuccess  bool   `json:"is_success"`
	Error      string `json:"error,omitempty"`
}

func (e *ToolEvent) GetEventType() EventType {
	return EventTypeTool
}

func (e *ToolEvent) ToStructuredProperties() any {
	return ToolPayload{
		Action:     e.Action,
		SessionID:  e.SessionID,
		AgentName:  e.AgentName,
		ToolName:   e.ToolName,
		DurationMs: e.Duration,
		IsSuccess:  e.Success,
		Error:      e.Error,
	}
}

// TOKEN EVENTS

// TokenEvent represents token usage events
type TokenEvent struct {
	Action       string  `json:"action"`
	ModelName    string  `json:"model_name"`
	SessionID    string  `json:"session_id"`
	AgentName    string  `json:"agent_name"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
	Cost         float64 `json:"cost"`
}

// TokenPayload represents the HTTP payload structure for token events
type TokenPayload struct {
	Action       string  `json:"action"`
	SessionID    string  `json:"session_id"`
	AgentName    string  `json:"agent_name"`
	ModelName    string  `json:"model_name"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
	Cost         float64 `json:"cost"`
}

func (e *TokenEvent) GetEventType() EventType {
	return EventTypeToken
}

func (e *TokenEvent) ToStructuredProperties() any {
	return TokenPayload{
		Action:       e.Action,
		SessionID:    e.SessionID,
		AgentName:    e.AgentName,
		ModelName:    e.ModelName,
		InputTokens:  e.InputTokens,
		OutputTokens: e.OutputTokens,
		TotalTokens:  e.TotalTokens,
		Cost:         e.Cost,
	}
}

// SESSION EVENTS

// SessionTokenUsage tracks token consumption
type SessionTokenUsage struct {
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	Cost         float64 `json:"cost"`
}

// SessionState consolidates all session-related tracking
type SessionState struct {
	ID           string
	AgentName    string
	StartTime    time.Time
	ToolCalls    int
	TokenUsage   SessionTokenUsage
	ErrorCount   int
	Error        []string
	SessionEnded bool
}

// SessionStartEvent represents session events
type SessionStartEvent struct {
	Action    string `json:"action"`
	SessionID string `json:"session_id"`
	AgentName string `json:"agent_name"`
}

// SessionStartPayload represents the HTTP payload structure for session events
type SessionStartPayload struct {
	Action    string `json:"action"`
	SessionID string `json:"session_id"`
	AgentName string `json:"agent_name"`
}

func (e *SessionStartEvent) GetEventType() EventType {
	return EventTypeSession
}

func (e *SessionStartEvent) ToStructuredProperties() any {
	return SessionStartPayload{
		Action:    "start",
		SessionID: e.SessionID,
		AgentName: e.AgentName,
	}
}

// SessionEndEvent represents session events
type SessionEndEvent struct {
	Action       string   `json:"action"`
	SessionID    string   `json:"session_id"`
	AgentName    string   `json:"agent_name"`
	Duration     int64    `json:"duration_ms"`
	ToolCalls    int      `json:"tool_calls"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	TotalTokens  int64    `json:"total_tokens"`
	ErrorCount   int      `json:"error_count"`
	Cost         float64  `json:"cost"`
	IsSuccess    bool     `json:"is_success"`
	Error        []string `json:"error"`
}

// SessionEndPayload represents the HTTP payload structure for session events
type SessionEndPayload struct {
	Action       string   `json:"action"`
	SessionID    string   `json:"session_id"`
	AgentName    string   `json:"agent_name"`
	DurationMs   int64    `json:"duration_ms"`
	ToolCalls    int      `json:"tool_calls"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	TotalTokens  int64    `json:"total_tokens"`
	Cost         float64  `json:"cost"`
	ErrorCount   int      `json:"error_count"`
	IsSuccess    bool     `json:"is_success"`
	Error        []string `json:"error"`
}

func (e *SessionEndEvent) GetEventType() EventType {
	return EventTypeSession
}

func (e *SessionEndEvent) ToStructuredProperties() any {
	return SessionEndPayload{
		Action:       "end",
		SessionID:    e.SessionID,
		AgentName:    e.AgentName,
		DurationMs:   e.Duration,
		ToolCalls:    e.ToolCalls,
		InputTokens:  e.InputTokens,
		OutputTokens: e.OutputTokens,
		TotalTokens:  e.TotalTokens,
		Cost:         e.Cost,
		ErrorCount:   e.ErrorCount,
		IsSuccess:    e.IsSuccess,
		Error:        e.Error,
	}
}

// TELEMETRY CLIENT

// HTTPClient interface for making HTTP requests (allows mocking in tests)
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client provides simplified telemetry functionality for docker agent
type Client struct {
	logger      *telemetryLogger
	userUUID    string
	desktopUUID string
	enabled     bool
	debugMode   bool // Print to stdout instead of sending
	httpClient  HTTPClient
	endpoint    string // Docker events API endpoint
	apiKey      string // Docker events API key for authentication
	header      string // Authorization header for remote telemetry
	version     string // App version for User-Agent and events
	mu          sync.RWMutex

	// Session tracking
	session SessionState
}

// setVersion safely sets the version with proper locking
func (tc *Client) setVersion(version string) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.version = version
}

// getVersion safely gets the version with proper locking
func (tc *Client) getVersion() string {
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	return tc.version
}
