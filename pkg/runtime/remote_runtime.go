package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin"
	"github.com/docker/docker-agent/pkg/tools/mcp"
)

// RemoteRuntime implements the Runtime interface using a remote client.
// It works with any client that implements the RemoteClient interface,
// including both HTTP (Client) and Connect-RPC (ConnectRPCClient) clients.
type RemoteRuntime struct {
	client                  RemoteClient
	currentAgent            string
	agentFilename           string
	sessionID               string
	team                    *team.Team
	pendingOAuthElicitation *ElicitationRequestEvent
}

// RemoteRuntimeOption is a function for configuring the RemoteRuntime
type RemoteRuntimeOption func(*RemoteRuntime)

// WithRemoteCurrentAgent sets the current agent name
func WithRemoteCurrentAgent(agentName string) RemoteRuntimeOption {
	return func(r *RemoteRuntime) {
		r.currentAgent = agentName
	}
}

// WithRemoteAgentFilename sets the agent filename to use with the remote API
func WithRemoteAgentFilename(filename string) RemoteRuntimeOption {
	return func(r *RemoteRuntime) {
		r.agentFilename = filename
	}
}

// NewRemoteRuntime creates a new remote runtime that implements the Runtime interface.
// It accepts any client that implements the RemoteClient interface.
func NewRemoteRuntime(client RemoteClient, opts ...RemoteRuntimeOption) (*RemoteRuntime, error) {
	if client == nil {
		return nil, errors.New("client cannot be nil")
	}

	r := &RemoteRuntime{
		client:        client,
		currentAgent:  "root",
		agentFilename: "agent.yaml",
		team:          team.New(),
	}

	for _, opt := range opts {
		opt(r)
	}

	return r, nil
}

// CurrentAgentName returns the name of the currently active agent
func (r *RemoteRuntime) CurrentAgentName() string {
	return r.currentAgent
}

func (r *RemoteRuntime) CurrentAgentInfo(ctx context.Context) CurrentAgentInfo {
	cfg := r.readCurrentAgentConfig(ctx)
	return CurrentAgentInfo{
		Name:        r.currentAgent,
		Description: cfg.Description,
		Commands:    cfg.Commands,
	}
}

// SetCurrentAgent sets the currently active agent for subsequent user messages
func (r *RemoteRuntime) SetCurrentAgent(agentName string) error {
	r.currentAgent = agentName
	slog.Debug("Switched current agent (remote)", "agent", agentName)
	return nil
}

// CurrentAgentTools returns the tools for the current agent.
// For remote runtime, this returns nil as tools are managed server-side.
func (r *RemoteRuntime) CurrentAgentTools(_ context.Context) ([]tools.Tool, error) {
	return nil, nil
}

// EmitStartupInfo emits initial agent, team, and toolset information
func (r *RemoteRuntime) EmitStartupInfo(ctx context.Context, _ *session.Session, events chan Event) {
	cfg := r.readCurrentAgentConfig(ctx)

	events <- AgentInfo(r.currentAgent, cfg.Model, cfg.Description, cfg.WelcomeMessage)
	events <- TeamInfo(r.agentDetailsFromConfig(ctx), r.currentAgent)

	// Emit a loading indicator while we fetch the real tool count from the server.
	if len(cfg.Toolsets) > 0 {
		events <- ToolsetInfo(0, true, r.currentAgent)
	}

	toolCount, err := r.client.GetAgentToolCount(ctx, r.agentFilename, r.currentAgent)
	if err != nil {
		slog.Warn("Failed to get agent tool count", "error", err)
		return
	}

	events <- ToolsetInfo(toolCount, false, r.currentAgent)
}

func (r *RemoteRuntime) agentDetailsFromConfig(ctx context.Context) []AgentDetails {
	cfg, err := r.client.GetAgent(ctx, r.agentFilename)
	if err != nil {
		return nil
	}

	var details []AgentDetails
	for _, agent := range cfg.Agents {
		info := AgentDetails{
			Name:        agent.Name,
			Description: agent.Description,
			Commands:    agent.Commands,
		}

		if provider, model, found := strings.Cut(agent.Model, "/"); found {
			info.Provider = provider
			info.Model = model
		} else {
			info.Model = agent.Model
		}

		details = append(details, info)
	}

	return details
}

func (r *RemoteRuntime) readCurrentAgentConfig(ctx context.Context) latest.AgentConfig {
	cfg, err := r.client.GetAgent(ctx, r.agentFilename)
	if err != nil {
		return latest.AgentConfig{}
	}

	for _, agent := range cfg.Agents {
		if agent.Name == r.currentAgent {
			return agent
		}
	}

	return latest.AgentConfig{}
}

// RunStream starts the agent's interaction loop and returns a channel of events
func (r *RemoteRuntime) RunStream(ctx context.Context, sess *session.Session) <-chan Event {
	slog.Debug("Starting remote runtime stream", "agent", r.currentAgent, "session_id", r.sessionID)
	events := make(chan Event, 128)

	go func() {
		defer close(events)

		messages := r.convertSessionMessages(sess)
		r.sessionID = sess.ID

		var streamChan <-chan Event
		var err error

		if r.currentAgent != "" && r.currentAgent != "root" {
			streamChan, err = r.client.RunAgentWithAgentName(ctx, r.sessionID, r.agentFilename, r.currentAgent, messages)
		} else {
			streamChan, err = r.client.RunAgent(ctx, r.sessionID, r.agentFilename, messages)
		}

		if err != nil {
			events <- Error(fmt.Sprintf("failed to start remote agent: %v", err))
			return
		}

		for streamEvent := range streamChan {
			if elicitationRequest, ok := streamEvent.(*ElicitationRequestEvent); ok {
				r.pendingOAuthElicitation = elicitationRequest
			}
			events <- streamEvent
		}
	}()

	return events
}

// Run starts the agent's interaction loop and returns the final messages
func (r *RemoteRuntime) Run(ctx context.Context, sess *session.Session) ([]session.Message, error) {
	eventsChan := r.RunStream(ctx, sess)

	for event := range eventsChan {
		if errEvent, ok := event.(*ErrorEvent); ok {
			return nil, fmt.Errorf("%s", errEvent.Error)
		}
	}

	return sess.GetAllMessages(), nil
}

// Resume allows resuming execution after user confirmation
func (r *RemoteRuntime) Resume(ctx context.Context, req ResumeRequest) {
	slog.Debug("Resuming remote runtime", "agent", r.currentAgent, "type", req.Type, "reason", req.Reason, "tool_name", req.ToolName, "session_id", r.sessionID)

	if r.sessionID == "" {
		slog.Error("Cannot resume: no session ID available")
		return
	}

	if err := r.client.ResumeSession(ctx, r.sessionID, string(req.Type), req.Reason, req.ToolName); err != nil {
		slog.Error("Failed to resume remote session", "error", err, "session_id", r.sessionID)
	}
}

// Summarize generates a summary for the session
func (r *RemoteRuntime) Summarize(_ context.Context, sess *session.Session, _ string, events chan Event) {
	slog.Debug("Summarize not yet implemented for remote runtime", "session_id", r.sessionID)
	events <- SessionSummary(sess.ID, "Summary generation not yet implemented for remote runtime", r.currentAgent)
}

func (r *RemoteRuntime) convertSessionMessages(sess *session.Session) []api.Message {
	sessionMessages := sess.GetAllMessages()
	messages := make([]api.Message, 0, len(sessionMessages))

	for i := range sessionMessages {
		if sessionMessages[i].Message.Role == chat.MessageRoleUser || sessionMessages[i].Message.Role == chat.MessageRoleAssistant {
			messages = append(messages, api.Message{
				Role:    sessionMessages[i].Message.Role,
				Content: sessionMessages[i].Message.Content,
			})
		}
	}

	return messages
}

// ResumeElicitation sends an elicitation response back to a waiting elicitation request
func (r *RemoteRuntime) ResumeElicitation(ctx context.Context, action tools.ElicitationAction, content map[string]any) error {
	slog.Debug("Resuming remote runtime with elicitation response", "agent", r.currentAgent, "action", action, "session_id", r.sessionID)

	err := r.handleOAuthElicitation(ctx, r.pendingOAuthElicitation)
	if err != nil {
		return err
	}

	if err := r.client.ResumeElicitation(ctx, r.sessionID, action, content); err != nil {
		return err
	}

	return nil
}

func (r *RemoteRuntime) handleOAuthElicitation(ctx context.Context, req *ElicitationRequestEvent) error {
	if req == nil {
		return nil
	}

	slog.Debug("Handling OAuth elicitation request", "server_url", req.Meta["cagent/server_url"])

	serverURL, ok := req.Meta["cagent/server_url"].(string)
	if !ok {
		err := errors.New("server_url missing from elicitation metadata")
		slog.Error("Failed to extract server_url", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return err
	}

	authServerMetadata, ok := req.Meta["auth_server_metadata"].(map[string]any)
	if !ok {
		err := errors.New("auth_server_metadata missing from elicitation metadata")
		slog.Error("Failed to extract auth_server_metadata", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return err
	}

	var authMetadata mcp.AuthorizationServerMetadata
	metadataBytes, err := json.Marshal(authServerMetadata)
	if err != nil {
		slog.Error("Failed to marshal auth_server_metadata", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return fmt.Errorf("failed to marshal auth_server_metadata: %w", err)
	}
	if err := json.Unmarshal(metadataBytes, &authMetadata); err != nil {
		slog.Error("Failed to unmarshal auth_server_metadata", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return fmt.Errorf("failed to unmarshal auth_server_metadata: %w", err)
	}

	slog.Debug("Authorization server metadata extracted", "issuer", authMetadata.Issuer)

	oauthCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	slog.Debug("Creating OAuth callback server")
	callbackServer, err := mcp.NewCallbackServer()
	if err != nil {
		slog.Error("Failed to create callback server", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return fmt.Errorf("failed to create callback server: %w", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := callbackServer.Shutdown(shutdownCtx); err != nil {
			slog.Error("Failed to shutdown callback server", "error", err)
		}
	}()

	if err := callbackServer.Start(); err != nil {
		slog.Error("Failed to start callback server", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return fmt.Errorf("failed to start callback server: %w", err)
	}

	redirectURI := callbackServer.GetRedirectURI()
	slog.Debug("Callback server started", "redirect_uri", redirectURI)

	var clientID, clientSecret string
	if authMetadata.RegistrationEndpoint != "" {
		slog.Debug("Attempting dynamic client registration")
		clientID, clientSecret, err = mcp.RegisterClient(oauthCtx, &authMetadata, redirectURI, nil)
		if err != nil {
			slog.Error("Dynamic client registration failed", "error", err)
			_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
			return fmt.Errorf("failed to register client: %w", err)
		}
		slog.Debug("Client registered successfully", "client_id", clientID)
	} else {
		err := errors.New("authorization server does not support dynamic client registration")
		slog.Error("Client registration not supported", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return err
	}

	state, err := mcp.GenerateState()
	if err != nil {
		slog.Error("Failed to generate state", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return fmt.Errorf("failed to generate state: %w", err)
	}

	callbackServer.SetExpectedState(state)
	verifier := mcp.GeneratePKCEVerifier()

	authURL := mcp.BuildAuthorizationURL(
		authMetadata.AuthorizationEndpoint,
		clientID,
		redirectURI,
		state,
		oauth2.S256ChallengeFromVerifier(verifier),
		serverURL,
		nil, // scopes - use server defaults
	)

	slog.Debug("Authorization URL built", "url", authURL)

	slog.Debug("Requesting authorization code")
	code, receivedState, err := mcp.RequestAuthorizationCode(oauthCtx, authURL, callbackServer, state)
	if err != nil {
		slog.Error("Failed to get authorization code", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return fmt.Errorf("failed to get authorization code: %w", err)
	}

	if receivedState != state {
		err := fmt.Errorf("state mismatch: expected %s, got %s", state, receivedState)
		slog.Error("State mismatch in authorization response", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return err
	}

	slog.Debug("Authorization code received, exchanging for token")

	token, err := mcp.ExchangeCodeForToken(
		oauthCtx,
		authMetadata.TokenEndpoint,
		code,
		verifier,
		clientID,
		clientSecret,
		redirectURI,
	)
	if err != nil {
		slog.Error("Failed to exchange code for token", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return fmt.Errorf("failed to exchange code for token: %w", err)
	}

	slog.Debug("Token obtained successfully", "token_type", token.TokenType)

	tokenData := map[string]any{
		"access_token": token.AccessToken,
		"token_type":   token.TokenType,
	}
	if token.ExpiresIn > 0 {
		tokenData["expires_in"] = token.ExpiresIn
	}
	if token.RefreshToken != "" {
		tokenData["refresh_token"] = token.RefreshToken
	}

	slog.Debug("Sending token to server")
	if err := r.client.ResumeElicitation(ctx, r.sessionID, tools.ElicitationActionAccept, tokenData); err != nil {
		slog.Error("Failed to send token to server", "error", err)
		return fmt.Errorf("failed to send token to server: %w", err)
	}

	slog.Debug("OAuth flow completed successfully")
	return nil
}

// SessionStore returns nil for remote runtime since session storage is handled server-side.
func (r *RemoteRuntime) SessionStore() session.Store {
	return nil
}

// PermissionsInfo returns nil for remote runtime since permissions are handled server-side.
func (r *RemoteRuntime) PermissionsInfo() *PermissionsInfo {
	return nil
}

// ResetStartupInfo is a no-op for remote runtime.
func (r *RemoteRuntime) ResetStartupInfo() {
}

// CurrentAgentSkillsToolset returns nil for remote runtimes since skills are managed server-side.
func (r *RemoteRuntime) CurrentAgentSkillsToolset() *builtin.SkillsToolset {
	return nil
}

// UpdateSessionTitle updates the title of the current session on the remote server.
func (r *RemoteRuntime) UpdateSessionTitle(ctx context.Context, sess *session.Session, title string) error {
	sess.Title = title
	if r.sessionID == "" {
		return errors.New("cannot update session title: no session ID available")
	}
	return r.client.UpdateSessionTitle(ctx, r.sessionID, title)
}

// CurrentMCPPrompts is not supported on remote runtimes.
func (r *RemoteRuntime) CurrentMCPPrompts(context.Context) map[string]mcp.PromptInfo {
	return make(map[string]mcp.PromptInfo)
}

// ExecuteMCPPrompt is not supported on remote runtimes.
func (r *RemoteRuntime) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", errors.New("MCP prompts are not supported by remote runtimes")
}

// TitleGenerator is not supported on remote runtimes (titles are generated server-side).
func (r *RemoteRuntime) TitleGenerator() *sessiontitle.Generator {
	return nil
}

// Close is a no-op for remote runtimes.
func (r *RemoteRuntime) Close() error {
	return nil
}

var _ Runtime = (*RemoteRuntime)(nil)
