package api

import (
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
)

type Message struct {
	Role         chat.MessageRole   `json:"role"`
	Content      string             `json:"content"`
	MultiContent []chat.MessagePart `json:"multi_content,omitempty"`
}

// Agent represents an agent in the API
type Agent struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Multi       bool   `json:"multi"`
}

// CreateAgentRequest represents a request to create an agent
type CreateAgentRequest struct {
	Prompt string `json:"prompt"`
}

// CreateAgentResponse represents the response from creating an agent
type CreateAgentResponse struct {
	Path string `json:"path"`
	Out  string `json:"out"`
}

// CreateAgentConfigRequest represents a request to create an agent manually
type CreateAgentConfigRequest struct {
	Filename    string `json:"filename"`
	Model       string `json:"model"`
	Description string `json:"description"`
	Instruction string `json:"instruction"`
}

// CreateAgentConfigResponse represents the response from creating an agent config
type CreateAgentConfigResponse struct {
	Filepath string `json:"filepath"`
}

// EditAgentConfigRequest represents a request to edit an agent config
type EditAgentConfigRequest struct {
	AgentConfig latest.Config `json:"agent_config"`
	Filename    string        `json:"filename"`
}

// EditAgentConfigResponse represents the response from editing an agent config
type EditAgentConfigResponse struct {
	Message string `json:"message"`
	Path    string `json:"path"`
	Config  any    `json:"config"`
}

// ImportAgentRequest represents a request to import an agent
type ImportAgentRequest struct {
	FilePath string `json:"file_path"`
}

// ImportAgentResponse represents the response from importing an agent
type ImportAgentResponse struct {
	OriginalPath string `json:"originalPath"`
	TargetPath   string `json:"targetPath"`
	Description  string `json:"description"`
}

// ExportAgentsResponse represents the response from exporting agents
type ExportAgentsResponse struct {
	ZipPath      string `json:"zipPath"`
	ZipFile      string `json:"zipFile"`
	ZipDirectory string `json:"zipDirectory"`
	AgentsDir    string `json:"agentsDir"`
	CreatedAt    string `json:"createdAt"`
}

// PullAgentRequest represents a request to pull an agent
type PullAgentRequest struct {
	Name string `json:"name"`
}

// PullAgentResponse represents the response from pulling an agent
type PullAgentResponse struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// PushAgentRequest represents a request to push an agent
type PushAgentRequest struct {
	Filepath string `json:"filepath"`
	Tag      string `json:"tag"`
}

// PushAgentResponse represents the response from pushing an agent
type PushAgentResponse struct {
	Filepath string `json:"filepath"`
	Tag      string `json:"tag"`
	Digest   string `json:"digest"`
}

// DeleteAgentRequest represents a request to delete an agent
type DeleteAgentRequest struct {
	FilePath string `json:"file_path"`
}

// DeleteAgentResponse represents the response from deleting an agent
type DeleteAgentResponse struct {
	FilePath string `json:"filePath"`
}

// SessionsResponse represents a session in the sessions list
type SessionsResponse struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	CreatedAt    string `json:"created_at"`
	NumMessages  int    `json:"num_messages"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	WorkingDir   string `json:"working_dir,omitempty"`
}

// SessionResponse represents a detailed session
type SessionResponse struct {
	ID            string                     `json:"id"`
	Title         string                     `json:"title"`
	Messages      []session.Message          `json:"messages,omitempty"`
	CreatedAt     time.Time                  `json:"created_at"`
	ToolsApproved bool                       `json:"tools_approved"`
	InputTokens   int64                      `json:"input_tokens"`
	OutputTokens  int64                      `json:"output_tokens"`
	WorkingDir    string                     `json:"working_dir,omitempty"`
	Permissions   *session.PermissionsConfig `json:"permissions,omitempty"`
}

// UpdateSessionPermissionsRequest represents a request to update session permissions.
type UpdateSessionPermissionsRequest struct {
	Permissions *session.PermissionsConfig `json:"permissions"`
}

// ResumeSessionRequest represents a request to resume a session
type ResumeSessionRequest struct {
	Confirmation string `json:"confirmation"`
	Reason       string `json:"reason,omitempty"`    // e.g reason for tool call rejection
	ToolName     string `json:"tool_name,omitempty"` // tool name for approve-tool confirmation
}

// DesktopTokenResponse represents the response from getting a desktop token
type DesktopTokenResponse struct {
	Token string `json:"token"`
}

// ResumeElicitationRequest represents a request to resume with an elicitation response
type ResumeElicitationRequest struct {
	Action  string         `json:"action"`  // "accept", "decline", or "cancel"
	Content map[string]any `json:"content"` // The submitted form data (only present when action is "accept")
}

// SteerSessionRequest represents a request to inject user messages into a
// running agent session. The messages are picked up by the agent loop between
// tool execution and the next LLM call.
type SteerSessionRequest struct {
	Messages []Message `json:"messages"`
}

// UpdateSessionTitleRequest represents a request to update a session's title
type UpdateSessionTitleRequest struct {
	Title string `json:"title"`
}

// UpdateSessionTitleResponse represents the response from updating a session's title
type UpdateSessionTitleResponse struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// AddMessageRequest represents a request to add a message to a session
type AddMessageRequest struct {
	Message *session.Message `json:"message"`
}

// UpdateMessageRequest represents a request to update a message in a session
type UpdateMessageRequest struct {
	Message *session.Message `json:"message"`
}

// AddSummaryRequest represents a request to add a summary to a session
type AddSummaryRequest struct {
	Summary string `json:"summary"`
	Tokens  int    `json:"tokens,omitempty"`
}

// UpdateSessionTokensRequest represents a request to update session token counts
type UpdateSessionTokensRequest struct {
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	Cost         float64 `json:"cost,omitempty"`
}

// SetSessionStarredRequest represents a request to star or unstar a session
type SetSessionStarredRequest struct {
	Starred bool `json:"starred"`
}

// BatchDeleteSessionsRequest represents a request to delete multiple sessions
type BatchDeleteSessionsRequest struct {
	SessionIDs []string `json:"session_ids"`
}

// BatchDeleteSessionsResponse represents the response from batch delete
type BatchDeleteSessionsResponse struct {
	DeletedCount int      `json:"deleted_count"`
	FailedCount  int      `json:"failed_count"`
	FailedIDs    []string `json:"failed_ids,omitempty"`
}

// BatchExportSessionsRequest represents a request to export multiple sessions
type BatchExportSessionsRequest struct {
	SessionIDs []string `json:"session_ids"`
	Format     string   `json:"format,omitempty"` // "json" (default) or "zip"
}

// BatchExportSessionsResponse represents the response from batch export
type BatchExportSessionsResponse struct {
	ExportFormat string `json:"export_format"`
	DataURL      string `json:"data_url,omitempty"`  // Base64 data URL for small exports
	FilePath     string `json:"file_path,omitempty"` // Temporary file path for large exports
	SessionCount int    `json:"session_count"`
}

// HealthResponse represents the response from the health check endpoint
type HealthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// ReadyResponse represents the response from the readiness check endpoint
type ReadyResponse struct {
	Ready          bool   `json:"ready"`
	ActiveSessions int    `json:"active_sessions"`
	StoreConnected bool   `json:"store_connected"`
	ToolsetHealth  string `json:"toolset_health,omitempty"`
	LatestError    string `json:"latest_error,omitempty"`
}

// QueueDepthResponse represents the queue depth information for a session
type QueueDepthResponse struct {
	Steer struct {
		Depth    int `json:"depth"`
		Capacity int `json:"capacity"`
	} `json:"steer"`
	Followup struct {
		Depth    int `json:"depth"`
		Capacity int `json:"capacity"`
	} `json:"followup"`
}

// SessionStatusResponse represents the current runtime state of a session.
// Designed for late-joining SSE consumers that need a state snapshot on connect.
type SessionStatusResponse struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Streaming    bool   `json:"streaming"`
	Agent        string `json:"agent,omitempty"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	NumMessages  int    `json:"num_messages"`
}
