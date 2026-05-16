package acp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/version"
)

// Agent implements the ACP Agent interface for docker agent.
type Agent struct {
	agentSource  config.Source
	runConfig    *config.RuntimeConfig
	sessionStore session.Store
	sessions     map[string]*Session

	conn *acp.AgentSideConnection
	team *team.Team
	mu   sync.Mutex
}

var _ acp.Agent = (*Agent)(nil)

// Session represents an ACP session.
type Session struct {
	id         string
	sess       *session.Session
	rt         runtime.Runtime
	cancel     context.CancelFunc
	workingDir string
}

// NewAgent creates a new ACP agent.
func NewAgent(agentSource config.Source, runConfig *config.RuntimeConfig, sessionStore session.Store) *Agent {
	return &Agent{
		agentSource:  agentSource,
		runConfig:    runConfig,
		sessionStore: sessionStore,
		sessions:     make(map[string]*Session),
	}
}

// Stop stops the agent and its toolsets.
func (a *Agent) Stop(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.team != nil {
		if err := a.team.StopToolSets(ctx); err != nil {
			slog.ErrorContext(ctx, "Failed to stop tool sets", "error", err)
		}
	}
}

// SetAgentConnection sets the ACP connection.
func (a *Agent) SetAgentConnection(conn *acp.AgentSideConnection) {
	a.conn = conn
}

// Initialize implements [acp.Agent].
func (a *Agent) Initialize(ctx context.Context, params acp.InitializeRequest) (acp.InitializeResponse, error) {
	slog.DebugContext(ctx, "ACP Initialize called", "client_version", params.ProtocolVersion)

	a.mu.Lock()
	defer a.mu.Unlock()
	t, err := teamloader.Load(ctx, a.agentSource, a.runConfig, teamloader.WithToolsetRegistry(createToolsetRegistry(a)))
	if err != nil {
		return acp.InitializeResponse{}, fmt.Errorf("failed to load teams: %w", err)
	}
	a.team = t
	slog.DebugContext(ctx, "Teams loaded successfully", "source", a.agentSource.Name(), "agent_count", t.Size())

	agentTitle := "docker agent"
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentInfo: &acp.Implementation{
			Name:    "docker agent",
			Version: version.Version,
			Title:   &agentTitle,
		},
		AgentCapabilities: acp.AgentCapabilities{
			LoadSession: false,
			SessionCapabilities: acp.SessionCapabilities{
				Close:  &acp.SessionCloseCapabilities{},
				List:   &acp.SessionListCapabilities{},
				Resume: &acp.SessionResumeCapabilities{},
			},
			PromptCapabilities: acp.PromptCapabilities{
				EmbeddedContext: true,
				Image:           false, // Not yet supported
				Audio:           false, // Not yet supported
			},
			McpCapabilities: acp.McpCapabilities{
				Http: false, // MCP servers from client not yet supported
				Sse:  false, // MCP servers from client not yet supported
			},
		},
	}, nil
}

// newRuntime creates a new runtime using the default agent.
func (a *Agent) newRuntime() (runtime.Runtime, *agent.Agent, error) {
	if a.team == nil {
		return nil, nil, errors.New("agent not initialized")
	}

	defaultAgent, err := a.team.DefaultAgent()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to resolve default agent: %w", err)
	}

	rt, err := runtime.New(a.team,
		runtime.WithCurrentAgent(defaultAgent.Name()),
		runtime.WithSessionStore(a.sessionStore),
	)
	if err != nil {
		return nil, nil, err
	}
	return rt, defaultAgent, nil
}

// registerSession stores a session in the active sessions map.
func (a *Agent) registerSession(acpSess *Session) {
	a.mu.Lock()
	a.sessions[acpSess.id] = acpSess
	a.mu.Unlock()
}

// registerSessionIfAbsent stores acpSess only if no session with the same id
// is already registered. It returns the session that ended up in the map
// (either the existing one or acpSess) and a boolean indicating whether
// acpSess was the one stored. This avoids a TOCTOU race between checking
// a.sessions and registering a new session.
func (a *Agent) registerSessionIfAbsent(acpSess *Session) (*Session, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if existing, ok := a.sessions[acpSess.id]; ok {
		return existing, false
	}
	a.sessions[acpSess.id] = acpSess
	return acpSess, true
}

// NewSession implements [acp.Agent].
func (a *Agent) NewSession(ctx context.Context, params acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	slog.DebugContext(ctx, "ACP NewSession called", "cwd", params.Cwd)

	if len(params.McpServers) > 0 {
		slog.WarnContext(ctx, "MCP servers provided by client are not yet supported", "count", len(params.McpServers))
	}

	workingDir, err := resolveWorkingDir(params.Cwd)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}

	// An empty cwd is allowed: clients (e.g. zed) may not always supply a
	// working directory at session creation. We persist it as empty and
	// later prompts/tools fall back to the agent's default working dir.
	if workingDir != "" {
		info, err := os.Stat(workingDir)
		if err != nil {
			return acp.NewSessionResponse{}, fmt.Errorf("working directory does not exist: %w", err)
		}
		if !info.IsDir() {
			return acp.NewSessionResponse{}, errors.New("working directory must be a directory")
		}
	}

	rt, defaultAgent, err := a.newRuntime()
	if err != nil {
		return acp.NewSessionResponse{}, err
	}

	sess := session.New(
		session.WithMaxIterations(defaultAgent.MaxIterations()),
		session.WithMaxConsecutiveToolCalls(defaultAgent.MaxConsecutiveToolCalls()),
		session.WithMaxOldToolCallTokens(defaultAgent.MaxOldToolCallTokens()),
		session.WithWorkingDir(workingDir),
	)
	sess.Title = "ACP Session " + sess.ID

	if err := a.sessionStore.AddSession(ctx, sess); err != nil {
		return acp.NewSessionResponse{}, fmt.Errorf("failed to persist session: %w", err)
	}

	slog.DebugContext(ctx, "ACP session created", "session_id", sess.ID)

	a.registerSession(&Session{
		id:         sess.ID,
		sess:       sess,
		rt:         rt,
		workingDir: workingDir,
	})

	return acp.NewSessionResponse{SessionId: acp.SessionId(sess.ID)}, nil
}

// Authenticate implements [acp.Agent].
func (a *Agent) Authenticate(ctx context.Context, _ acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	slog.DebugContext(ctx, "ACP Authenticate called")
	return acp.AuthenticateResponse{}, nil
}

// LoadSession implements [acp.AgentLoader] (optional, not supported).
func (a *Agent) LoadSession(ctx context.Context, _ acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	slog.DebugContext(ctx, "ACP LoadSession called (not supported)")
	return acp.LoadSessionResponse{}, errors.New("load session not supported")
}

// CloseSession implements [acp.Agent].
func (a *Agent) CloseSession(_ context.Context, params acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	sid := string(params.SessionId)
	slog.Debug("ACP CloseSession called", "session_id", sid)

	a.mu.Lock()
	acpSess, ok := a.sessions[sid]
	if ok {
		delete(a.sessions, sid)
	}
	a.mu.Unlock()

	if ok && acpSess != nil && acpSess.cancel != nil {
		acpSess.cancel()
	}

	return acp.CloseSessionResponse{}, nil
}

// ListSessions implements [acp.Agent].
func (a *Agent) ListSessions(ctx context.Context, _ acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	slog.DebugContext(ctx, "ACP ListSessions called")

	summaries, err := a.sessionStore.GetSessionSummaries(ctx)
	if err != nil {
		return acp.ListSessionsResponse{}, fmt.Errorf("failed to list sessions: %w", err)
	}

	sessions := make([]acp.SessionInfo, 0, len(summaries))
	for _, s := range summaries {
		info := acp.SessionInfo{
			SessionId: acp.SessionId(s.ID),
			Title:     &s.Title,
		}
		if !s.CreatedAt.IsZero() {
			// We don't track session updates yet, so report CreatedAt in
			// the ACP UpdatedAt field as our best-effort timestamp.
			createdAt := s.CreatedAt.UTC().Format(time.RFC3339)
			info.UpdatedAt = &createdAt
		}
		sessions = append(sessions, info)
	}

	return acp.ListSessionsResponse{Sessions: sessions}, nil
}

// ResumeSession implements [acp.Agent].
func (a *Agent) ResumeSession(ctx context.Context, params acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	sid := string(params.SessionId)
	slog.DebugContext(ctx, "ACP ResumeSession called", "session_id", sid)

	a.mu.Lock()
	_, alreadyRegistered := a.sessions[sid]
	a.mu.Unlock()
	if alreadyRegistered {
		return acp.ResumeSessionResponse{}, nil
	}

	sess, err := a.sessionStore.GetSession(ctx, sid)
	if err != nil {
		return acp.ResumeSessionResponse{}, fmt.Errorf("failed to load session %s: %w", sid, err)
	}

	workingDir, err := resolveWorkingDir(params.Cwd)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	if workingDir != "" {
		sess.WorkingDir = workingDir
	}

	rt, _, err := a.newRuntime()
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}

	// Register atomically: if another goroutine raced us and registered
	// the same session id between our initial check and now, drop the
	// runtime we just built and reuse the existing registration.
	_, stored := a.registerSessionIfAbsent(&Session{
		id:         sid,
		sess:       sess,
		rt:         rt,
		workingDir: workingDir,
	})
	if !stored {
		slog.DebugContext(ctx, "ACP session already registered, reusing existing", "session_id", sid)
		return acp.ResumeSessionResponse{}, nil
	}

	slog.DebugContext(ctx, "ACP session resumed", "session_id", sid)

	return acp.ResumeSessionResponse{}, nil
}

// SetSessionConfigOption implements [acp.Agent] (optional, not advertised in capabilities).
func (a *Agent) SetSessionConfigOption(ctx context.Context, _ acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	slog.DebugContext(ctx, "ACP SetSessionConfigOption called (not supported)")
	return acp.SetSessionConfigOptionResponse{}, nil
}

// Cancel implements [acp.Agent].
func (a *Agent) Cancel(_ context.Context, params acp.CancelNotification) error {
	sid := string(params.SessionId)
	slog.Debug("ACP Cancel called", "session_id", sid)

	a.mu.Lock()
	acpSess, ok := a.sessions[sid]
	a.mu.Unlock()

	if ok && acpSess != nil && acpSess.cancel != nil {
		acpSess.cancel()
	}

	return nil
}

// Prompt implements [acp.Agent].
func (a *Agent) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	sid := string(params.SessionId)
	slog.DebugContext(ctx, "ACP Prompt called", "session_id", sid)

	a.mu.Lock()
	acpSess, ok := a.sessions[sid]
	a.mu.Unlock()

	if !ok {
		return acp.PromptResponse{}, fmt.Errorf("session %s not found", sid)
	}

	// Cancel any previous turn
	a.mu.Lock()
	if acpSess.cancel != nil {
		prev := acpSess.cancel
		a.mu.Unlock()
		prev()
	} else {
		a.mu.Unlock()
	}

	turnCtx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	acpSess.cancel = cancel
	a.mu.Unlock()

	userContent := a.buildUserContent(ctx, sid, params.Prompt)
	if userContent != "" {
		acpSess.sess.AddMessage(session.UserMessage(userContent))
	}

	if err := a.runAgent(turnCtx, acpSess); err != nil {
		if turnCtx.Err() != nil {
			return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
		}
		return acp.PromptResponse{}, err
	}

	a.mu.Lock()
	acpSess.cancel = nil
	a.mu.Unlock()

	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

// buildUserContent constructs user message text from ACP content blocks.
func (a *Agent) buildUserContent(ctx context.Context, sessionID string, prompt []acp.ContentBlock) string {
	var parts []string

	for _, content := range prompt {
		switch {
		case content.Text != nil:
			parts = append(parts, content.Text.Text)

		case content.ResourceLink != nil:
			rl := content.ResourceLink
			slog.DebugContext(ctx, "Processing resource link", "uri", rl.Uri, "name", rl.Name)

			if fileContent := a.readResourceLink(ctx, sessionID, rl); fileContent != "" {
				parts = append(parts, fmt.Sprintf("\n\n--- File: %s ---\n%s\n--- End File ---\n", rl.Name, fileContent))
			} else {
				parts = append(parts, fmt.Sprintf("\n[Referenced file: %s (URI: %s)]\n", rl.Name, rl.Uri))
			}

		case content.Resource != nil:
			res := content.Resource.Resource
			if res.TextResourceContents != nil {
				slog.DebugContext(ctx, "Processing embedded text resource", "uri", res.TextResourceContents.Uri)
				parts = append(parts, fmt.Sprintf("\n\n--- Resource: %s ---\n%s\n--- End Resource ---\n",
					res.TextResourceContents.Uri, res.TextResourceContents.Text))
			} else if res.BlobResourceContents != nil {
				slog.DebugContext(ctx, "Processing embedded blob resource", "uri", res.BlobResourceContents.Uri)
				parts = append(parts, fmt.Sprintf("\n[Binary resource: %s (type: %s)]\n",
					res.BlobResourceContents.Uri, stringOrDefault(res.BlobResourceContents.MimeType, "unknown")))
			}

		case content.Image != nil:
			slog.DebugContext(ctx, "Image content received but not yet fully supported")
			parts = append(parts, "[Image content provided]")

		case content.Audio != nil:
			slog.DebugContext(ctx, "Audio content received but not yet supported")
			parts = append(parts, "[Audio content provided]")
		}
	}

	return strings.Join(parts, "")
}

// readResourceLink attempts to read a text file referenced by an ACP resource link.
//
// For security reasons, this function applies basic path hardening:
//   - Only relative paths are allowed
//   - Path traversal (e.g. "../") is blocked
//
// If the path is unsafe or the file cannot be read, an empty string is returned.
func (a *Agent) readResourceLink(ctx context.Context, sessionID string, rl *acp.ContentBlockResourceLink) string {
	path := strings.TrimPrefix(rl.Uri, "file://")
	clean := filepath.Clean(path)

	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		slog.WarnContext(ctx, "Blocked unsafe file resource link", "path", path)
		return ""
	}

	resp, err := a.conn.ReadTextFile(ctx, acp.ReadTextFileRequest{
		SessionId: acp.SessionId(sessionID),
		Path:      clean,
	})
	if err != nil {
		slog.DebugContext(ctx, "Failed to read resource link", "path", clean, "error", err)
		return ""
	}

	return resp.Content
}

func stringOrDefault(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

// SetSessionMode implements acp.Agent (optional).
func (a *Agent) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}

// sendUpdate sends a session update notification to the ACP client.
func (a *Agent) sendUpdate(ctx context.Context, sessionID string, update acp.SessionUpdate) error {
	return a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: acp.SessionId(sessionID),
		Update:    update,
	})
}

// runAgent runs a single agent loop and streams updates to the ACP client.
func (a *Agent) runAgent(ctx context.Context, acpSess *Session) error {
	slog.DebugContext(ctx, "Running agent turn", "session_id", acpSess.id)

	ctx = withSessionID(ctx, acpSess.id)

	if err := a.emitAvailableCommands(ctx, acpSess); err != nil {
		slog.DebugContext(ctx, "Failed to emit available commands", "error", err)
	}

	eventsChan := acpSess.rt.RunStream(ctx, acpSess.sess)
	toolCallArgs := map[string]string{}

	for event := range eventsChan {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		switch e := event.(type) {
		case *runtime.AgentChoiceEvent:
			if err := a.sendUpdate(ctx, acpSess.id, acp.UpdateAgentMessageText(e.Content)); err != nil {
				return err
			}

		case *runtime.AgentChoiceReasoningEvent:
			if err := a.sendUpdate(ctx, acpSess.id, acp.UpdateAgentThoughtText(e.Content)); err != nil {
				return err
			}

		case *runtime.ToolCallConfirmationEvent:
			if err := a.handleToolCallConfirmation(ctx, acpSess, e); err != nil {
				return err
			}

		case *runtime.ToolCallEvent:
			toolCallArgs[e.ToolCall.ID] = e.ToolCall.Function.Arguments
			if err := a.sendUpdate(ctx, acpSess.id, buildToolCallStart(e.ToolCall, e.ToolDefinition)); err != nil {
				return err
			}

		case *runtime.ToolCallResponseEvent:
			args, ok := toolCallArgs[e.ToolCallID]
			if !ok {
				return fmt.Errorf("missing tool call arguments for tool call ID %s", e.ToolCallID)
			}
			delete(toolCallArgs, e.ToolCallID)

			if err := a.sendUpdate(ctx, acpSess.id, buildToolCallComplete(args, e)); err != nil {
				return err
			}

			if isTodoTool(e.ToolDefinition.Name) && e.Result != nil && e.Result.Meta != nil {
				if planUpdate := buildPlanUpdateFromTodos(e.Result.Meta); planUpdate != nil {
					if err := a.sendUpdate(ctx, acpSess.id, *planUpdate); err != nil {
						return err
					}
				}
			}

		case *runtime.ErrorEvent:
			if err := a.sendUpdate(ctx, acpSess.id, acp.UpdateAgentMessageText(fmt.Sprintf("\n\nError: %s\n", e.Error))); err != nil {
				return err
			}

		case *runtime.WarningEvent:
			if err := a.sendUpdate(ctx, acpSess.id, acp.UpdateAgentMessageText(fmt.Sprintf("\nWarning: %s\n", e.Message))); err != nil {
				return err
			}

		case *runtime.SessionTitleEvent:
			if err := a.sendUpdate(ctx, acpSess.id, acp.SessionUpdate{
				SessionInfoUpdate: &acp.SessionSessionInfoUpdate{
					SessionUpdate: "session_info_update",
					Title:         &e.Title,
				},
			}); err != nil {
				return err
			}

		case *runtime.TokenUsageEvent:
			if e.Usage != nil {
				usageUpdate := acp.SessionUsageUpdate{
					SessionUpdate: "usage_update",
					Size:          int(e.Usage.ContextLimit),
					Used:          int(e.Usage.ContextLength),
				}
				if e.Usage.Cost > 0 {
					usageUpdate.Cost = &acp.Cost{
						Amount:   e.Usage.Cost,
						Currency: "USD",
					}
				}
				if err := a.sendUpdate(ctx, acpSess.id, acp.SessionUpdate{UsageUpdate: &usageUpdate}); err != nil {
					return err
				}
			}

		case *runtime.ModelFallbackEvent:
			if err := a.sendUpdate(ctx, acpSess.id, acp.UpdateAgentMessageText(
				fmt.Sprintf("\nModel %s failed, falling back to %s (%s)\n", e.FailedModel, e.FallbackModel, e.Reason),
			)); err != nil {
				return err
			}

		case *runtime.MaxIterationsReachedEvent:
			if err := a.handleMaxIterationsReached(ctx, acpSess, e); err != nil {
				return err
			}
		}
	}

	return nil
}

// handleToolCallConfirmation handles tool call permission requests.
func (a *Agent) handleToolCallConfirmation(ctx context.Context, acpSess *Session, e *runtime.ToolCallConfirmationEvent) error {
	toolCallUpdate := buildToolCallUpdate(e.ToolCall, e.ToolDefinition, acp.ToolCallStatusPending)

	permResp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: acp.SessionId(acpSess.id),
		ToolCall:  toolCallUpdate,
		Options: []acp.PermissionOption{
			{
				Kind:     acp.PermissionOptionKindAllowOnce,
				Name:     "Allow this action",
				OptionId: "allow",
			},
			{
				Kind:     acp.PermissionOptionKindAllowAlways,
				Name:     "Allow and remember my choice",
				OptionId: "allow-always",
			},
			{
				Kind:     acp.PermissionOptionKindRejectOnce,
				Name:     "Skip this action",
				OptionId: "reject",
			},
		},
	})
	if err != nil {
		return err
	}

	if permResp.Outcome.Cancelled != nil {
		acpSess.rt.Resume(ctx, runtime.ResumeRequest{Type: runtime.ResumeTypeReject})
		return nil
	}

	if permResp.Outcome.Selected == nil {
		return errors.New("unexpected permission outcome")
	}

	switch string(permResp.Outcome.Selected.OptionId) {
	case "allow":
		acpSess.rt.Resume(ctx, runtime.ResumeRequest{Type: runtime.ResumeTypeApprove})
	case "allow-always":
		acpSess.rt.Resume(ctx, runtime.ResumeRequest{Type: runtime.ResumeTypeApproveSession})
	case "reject":
		acpSess.rt.Resume(ctx, runtime.ResumeRequest{Type: runtime.ResumeTypeReject})
	default:
		return fmt.Errorf("unexpected permission option: %s", permResp.Outcome.Selected.OptionId)
	}

	return nil
}

// handleMaxIterationsReached handles max iterations events.
func (a *Agent) handleMaxIterationsReached(ctx context.Context, acpSess *Session, e *runtime.MaxIterationsReachedEvent) error {
	title := fmt.Sprintf("Maximum iterations (%d) reached", e.MaxIterations)
	permResp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: acp.SessionId(acpSess.id),
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: "max_iterations",
			Title:      &title,
			Kind:       acp.Ptr(acp.ToolKindExecute),
			Status:     acp.Ptr(acp.ToolCallStatusPending),
		},
		Options: []acp.PermissionOption{
			{
				Kind:     acp.PermissionOptionKindAllowOnce,
				Name:     "Continue",
				OptionId: "continue",
			},
			{
				Kind:     acp.PermissionOptionKindRejectOnce,
				Name:     "Stop",
				OptionId: "stop",
			},
		},
	})
	if err != nil {
		return err
	}

	if permResp.Outcome.Cancelled != nil || permResp.Outcome.Selected == nil ||
		string(permResp.Outcome.Selected.OptionId) == "stop" {
		acpSess.rt.Resume(ctx, runtime.ResumeRequest{Type: runtime.ResumeTypeReject})
	} else {
		acpSess.rt.Resume(ctx, runtime.ResumeRequest{Type: runtime.ResumeTypeApprove})
	}

	return nil
}

// emitAvailableCommands sends the list of available slash commands to the client.
func (a *Agent) emitAvailableCommands(ctx context.Context, acpSess *Session) error {
	return a.sendUpdate(ctx, acpSess.id, acp.SessionUpdate{
		AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{
			SessionUpdate: "available_commands_update",
			AvailableCommands: []acp.AvailableCommand{
				{Name: "new", Description: "Clear session history and start fresh"},
				{Name: "compact", Description: "Generate summary and compact session history"},
				{Name: "usage", Description: "Display token usage statistics"},
			},
		},
	})
}

// resolveWorkingDir validates and normalizes a working directory path.
func resolveWorkingDir(cwd string) (string, error) {
	wd := strings.TrimSpace(cwd)
	if wd == "" {
		return "", nil
	}
	absWd, err := filepath.Abs(wd)
	if err != nil {
		return "", fmt.Errorf("invalid working directory: %w", err)
	}
	return absWd, nil
}
