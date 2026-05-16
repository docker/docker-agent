package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// RemoteClient is the interface that both HTTP and Connect-RPC clients implement
// for communicating with a remote docker agent server. It provides methods for
// creating sessions, running agents, and streaming events.
type RemoteClient interface {
	// GetAgent retrieves an agent configuration by ID
	GetAgent(ctx context.Context, id string) (*latest.Config, error)

	// CreateSession creates a new session
	CreateSession(ctx context.Context, sessTemplate *session.Session) (*session.Session, error)

	// ResumeSession resumes a paused session with optional rejection reason or tool name
	ResumeSession(ctx context.Context, id, confirmation, reason, toolName string) error

	// ResumeElicitation sends an elicitation response
	ResumeElicitation(ctx context.Context, sessionID string, action tools.ElicitationAction, content map[string]any) error

	// RunAgent executes an agent and returns a channel of streaming events
	RunAgent(ctx context.Context, sessionID, agent string, messages []api.Message) (<-chan Event, error)

	// RunAgentWithAgentName executes an agent with a specific agent name
	RunAgentWithAgentName(ctx context.Context, sessionID, agent, agentName string, messages []api.Message) (<-chan Event, error)

	// SteerSession injects user messages into a running session mid-turn
	SteerSession(ctx context.Context, sessionID string, messages []api.Message) error

	// FollowUpSession queues messages for end-of-turn processing
	FollowUpSession(ctx context.Context, sessionID string, messages []api.Message) error

	// UpdateSessionTitle updates the title of a session
	UpdateSessionTitle(ctx context.Context, sessionID, title string) error

	// GetAgentToolCount returns the number of tools available for an agent
	GetAgentToolCount(ctx context.Context, agentFilename, agentName string) (int, error)

	// StreamSessionEvents streams runtime events for a session as they occur
	StreamSessionEvents(ctx context.Context, sessionID string) (<-chan Event, error)

	// GetAllSessions retrieves all sessions from the remote store
	GetAllSessions(ctx context.Context) ([]session.Session, error)

	// DeleteRemoteSession deletes a session from the remote store
	DeleteRemoteSession(ctx context.Context, sessionID string) error

	// GetSessionTools retrieves tools available in a session
	GetSessionTools(ctx context.Context, sessionID string) ([]tools.Tool, error)

	// GetAvailableModels returns available models for the agent
	GetAvailableModels(ctx context.Context) ([]string, error)

	// SetAgentModel sets the model for the agent in a session
	SetAgentModel(ctx context.Context, sessionID, model string) error

	// GetSessionMCPPrompts returns available MCP prompts for a session
	GetSessionMCPPrompts(ctx context.Context, sessionID string) (map[string]any, error)

	// ExecuteSessionMCPPrompt executes an MCP prompt in a session
	ExecuteSessionMCPPrompt(ctx context.Context, sessionID, promptName string, args map[string]string) (string, error)

	// GetSessionSkills returns available skills for a session
	GetSessionSkills(ctx context.Context, sessionID string) (map[string]any, error)

	// CompactSession triggers session compaction on the server
	CompactSession(ctx context.Context, sessionID string) error

	// GetSessionToolsets returns toolset statuses for a session
	GetSessionToolsets(ctx context.Context, sessionID string) ([]map[string]any, error)

	// RestartSessionToolset restarts a toolset in a session
	RestartSessionToolset(ctx context.Context, sessionID, toolsetName string) error

	// PauseSession pauses a session
	PauseSession(ctx context.Context, sessionID string) error

	// GetSessionSnapshots retrieves snapshots for a session
	GetSessionSnapshots(ctx context.Context, sessionID string) ([]map[string]any, error)

	// UndoSession reverts a session to the previous snapshot
	UndoSession(ctx context.Context, sessionID string) error

	// ResetSession resets a session to initial state
	ResetSession(ctx context.Context, sessionID string) error

	// AddMessage adds a message to a session
	AddMessage(ctx context.Context, sessionID string, msg *session.Message) error

	// UpdateMessage updates a message in a session
	UpdateMessage(ctx context.Context, sessionID, msgID string, msg *session.Message) error

	// AddSummary adds a summary to a session
	AddSummary(ctx context.Context, sessionID, summary string, tokens int) error

	// UpdateSessionTokens updates token counts for a session
	UpdateSessionTokens(ctx context.Context, sessionID string, inputTokens, outputTokens int64, cost float64) error

	// SetSessionStarred sets the starred status for a session
	SetSessionStarred(ctx context.Context, sessionID string, starred bool) error
}

var _ RemoteClient = (*Client)(nil)
