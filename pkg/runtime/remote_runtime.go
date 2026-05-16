package runtime

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/skills"
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

	// resolvedDefault caches the team's default agent name fetched from the
	// server, so [CurrentAgentName] stays an O(1) field read after the first
	// call when no specific agent has been selected.
	resolvedDefault     string
	resolvedDefaultOnce sync.Once
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
		agentFilename: "agent.yaml",
		team:          team.New(),
	}

	for _, opt := range opts {
		opt(r)
	}

	return r, nil
}

// resolvedAgent returns the active agent's name and config from the remote
// team. When no specific agent has been selected, both come from the team's
// first agent — the server owns the notion of "default agent" instead of the
// client hard-coding the historical "root" name.
func (r *RemoteRuntime) resolvedAgent(ctx context.Context) (string, latest.AgentConfig) {
	cfg := r.readCurrentAgentConfig(ctx)
	return cmp.Or(r.currentAgent, cfg.Name), cfg
}

// CurrentAgentName returns the name of the currently active agent.
// When no specific agent has been selected, it falls back to the first agent
// declared by the remote team config. The remote lookup happens at most once;
// the result is cached so subsequent calls are O(1).
func (r *RemoteRuntime) CurrentAgentName() string {
	if r.currentAgent != "" {
		return r.currentAgent
	}
	r.resolvedDefaultOnce.Do(func() {
		r.resolvedDefault, _ = r.resolvedAgent(context.Background())
	})
	return r.resolvedDefault
}

func (r *RemoteRuntime) CurrentAgentInfo(ctx context.Context) CurrentAgentInfo {
	name, cfg := r.resolvedAgent(ctx)
	return CurrentAgentInfo{
		Name:        name,
		Description: cfg.Description,
		Commands:    cfg.Commands,
	}
}

// SetCurrentAgent sets the currently active agent for subsequent user messages.
// It validates the name against the remote team config; an unknown agent is
// rejected so callers see the same behaviour as LocalRuntime. A failure to
// fetch the team config (network error, auth failure, missing remote) is
// propagated rather than silently accepted — the whole point of this check
// is closing that silent-breakage gap.
func (r *RemoteRuntime) SetCurrentAgent(agentName string) error {
	cfg, err := r.client.GetAgent(context.Background(), r.agentFilename)
	if err != nil {
		return fmt.Errorf("validate agent %q against remote team: %w", agentName, err)
	}
	found := false
	for _, a := range cfg.Agents {
		if a.Name == agentName {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("agent %q not found in remote team", agentName)
	}
	r.currentAgent = agentName
	slog.Debug("Switched current agent (remote)", "agent", agentName)
	return nil
}

// CurrentAgentTools returns the tools for the current agent from the session.
func (r *RemoteRuntime) CurrentAgentTools(ctx context.Context) ([]tools.Tool, error) {
	if r.sessionID == "" {
		return nil, nil
	}
	return r.client.GetSessionTools(ctx, r.sessionID)
}

// CurrentAgentToolsetStatuses is not implemented for remote runtimes; the
// remote server owns the toolset lifecycle. Returns an empty slice so
// callers (TUI) can show an explanatory empty state without erroring.
func (r *RemoteRuntime) CurrentAgentToolsetStatuses() []tools.ToolsetStatus {
	return nil
}

// RestartToolset restarts a toolset on the remote server.
func (r *RemoteRuntime) RestartToolset(ctx context.Context, toolsetName string) error {
	if r.sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.RestartSessionToolset(ctx, r.sessionID, toolsetName)
}

// EmitStartupInfo emits initial agent, team, and toolset information
func (r *RemoteRuntime) EmitStartupInfo(ctx context.Context, _ *session.Session, events EventSink) {
	agentName, cfg := r.resolvedAgent(ctx)

	events.Emit(AgentInfo(agentName, cfg.Model, cfg.Description, cfg.WelcomeMessage))
	events.Emit(TeamInfo(r.agentDetailsFromConfig(ctx), agentName))

	// Emit a loading indicator while we fetch the real tool count from the server.
	if len(cfg.Toolsets) > 0 {
		events.Emit(ToolsetInfo(0, true, agentName))
	}

	toolCount, err := r.client.GetAgentToolCount(ctx, r.agentFilename, agentName)
	if err != nil {
		slog.WarnContext(ctx, "Failed to get agent tool count", "error", err)
		return
	}

	events.Emit(ToolsetInfo(toolCount, false, agentName))
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

// readCurrentAgentConfig fetches the active agent's config from the server.
// When no specific agent has been selected, it falls back to the first agent
// in the team — letting the server own the notion of "default agent" instead
// of hard-coding the historical "root" name.
func (r *RemoteRuntime) readCurrentAgentConfig(ctx context.Context) latest.AgentConfig {
	cfg, err := r.client.GetAgent(ctx, r.agentFilename)
	if err != nil || len(cfg.Agents) == 0 {
		return latest.AgentConfig{}
	}

	if r.currentAgent == "" {
		return cfg.Agents[0]
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
	slog.DebugContext(ctx, "Starting remote runtime stream", "agent", r.currentAgent, "session_id", r.sessionID)
	events := make(chan Event, defaultEventChannelCapacity)

	go func() {
		defer close(events)

		messages := r.convertSessionMessages(sess)
		r.sessionID = sess.ID

		var streamChan <-chan Event
		var err error

		if r.currentAgent != "" {
			streamChan, err = r.client.RunAgentWithAgentName(ctx, r.sessionID, r.agentFilename, r.currentAgent, messages)
		} else {
			streamChan, err = r.client.RunAgent(ctx, r.sessionID, r.agentFilename, messages)
		}

		if err != nil {
			events <- Error(fmt.Sprintf("failed to start remote agent: %v", err))
			return
		}

		// Consume events from the agent stream
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

// Steer enqueues a user message for mid-turn injection into the running
// agent loop on the remote server.
func (r *RemoteRuntime) Steer(msg QueuedMessage) error {
	if r.sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.SteerSession(context.Background(), r.sessionID, []api.Message{
		{Content: msg.Content, MultiContent: msg.MultiContent},
	})
}

// FollowUp enqueues a message for end-of-turn processing on the remote server.
func (r *RemoteRuntime) FollowUp(msg QueuedMessage) error {
	if r.sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.FollowUpSession(context.Background(), r.sessionID, []api.Message{
		{Content: msg.Content, MultiContent: msg.MultiContent},
	})
}

func (r *RemoteRuntime) QueueStatus() QueueStatus {
	return QueueStatus{}
}

// Resume allows resuming execution after user confirmation
func (r *RemoteRuntime) Resume(ctx context.Context, req ResumeRequest) {
	slog.DebugContext(ctx, "Resuming remote runtime", "agent", r.currentAgent, "type", req.Type, "reason", req.Reason, "tool_name", req.ToolName, "session_id", r.sessionID)

	if r.sessionID == "" {
		slog.ErrorContext(ctx, "Cannot resume: no session ID available")
		return
	}

	if err := r.client.ResumeSession(ctx, r.sessionID, string(req.Type), req.Reason, req.ToolName); err != nil {
		slog.ErrorContext(ctx, "Failed to resume remote session", "error", err, "session_id", r.sessionID)
	}
}

// Summarize generates a summary for the session by compacting it server-side.
func (r *RemoteRuntime) Summarize(ctx context.Context, sess *session.Session, _ string, sink EventSink) {
	if r.sessionID == "" {
		sink.Emit(SessionSummary(sess.ID, "No active session to summarize", r.currentAgent, 0))
		return
	}
	if err := r.client.CompactSession(ctx, r.sessionID); err != nil {
		slog.WarnContext(ctx, "Failed to compact session", "error", err)
		sink.Emit(SessionSummary(sess.ID, fmt.Sprintf("Compaction failed: %v", err), r.currentAgent, 0))
		return
	}
	sink.Emit(SessionSummary(sess.ID, "Session compacted successfully", r.currentAgent, 0))
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
	slog.DebugContext(ctx, "Resuming remote runtime with elicitation response", "agent", r.currentAgent, "action", action, "session_id", r.sessionID)

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

	slog.DebugContext(ctx, "Handling OAuth elicitation request", "server_url", req.Meta["cagent/server_url"])

	serverURL, ok := req.Meta["cagent/server_url"].(string)
	if !ok {
		err := errors.New("server_url missing from elicitation metadata")
		slog.ErrorContext(ctx, "Failed to extract server_url", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return err
	}

	authServerMetadata, ok := req.Meta["auth_server_metadata"].(map[string]any)
	if !ok {
		err := errors.New("auth_server_metadata missing from elicitation metadata")
		slog.ErrorContext(ctx, "Failed to extract auth_server_metadata", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return err
	}

	var authMetadata mcp.AuthorizationServerMetadata
	metadataBytes, err := json.Marshal(authServerMetadata)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to marshal auth_server_metadata", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return fmt.Errorf("failed to marshal auth_server_metadata: %w", err)
	}
	if err := json.Unmarshal(metadataBytes, &authMetadata); err != nil {
		slog.ErrorContext(ctx, "Failed to unmarshal auth_server_metadata", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return fmt.Errorf("failed to unmarshal auth_server_metadata: %w", err)
	}

	slog.DebugContext(ctx, "Authorization server metadata extracted", "issuer", authMetadata.Issuer)

	oauthCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	slog.DebugContext(ctx, "Creating OAuth callback server")
	callbackServer, err := mcp.NewCallbackServer()
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create callback server", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return fmt.Errorf("failed to create callback server: %w", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := callbackServer.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(ctx, "Failed to shutdown callback server", "error", err)
		}
	}()

	if err := callbackServer.Start(); err != nil {
		slog.ErrorContext(ctx, "Failed to start callback server", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return fmt.Errorf("failed to start callback server: %w", err)
	}

	redirectURI := callbackServer.GetRedirectURI()
	slog.DebugContext(ctx, "Callback server started", "redirect_uri", redirectURI)

	var clientID, clientSecret string
	if authMetadata.RegistrationEndpoint != "" {
		slog.DebugContext(ctx, "Attempting dynamic client registration")
		clientID, clientSecret, err = mcp.RegisterClient(oauthCtx, &authMetadata, redirectURI, nil)
		if err != nil {
			slog.ErrorContext(ctx, "Dynamic client registration failed", "error", err)
			_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
			return fmt.Errorf("failed to register client: %w", err)
		}
		slog.DebugContext(ctx, "Client registered successfully", "client_id", clientID)
	} else {
		err := errors.New("authorization server does not support dynamic client registration")
		slog.ErrorContext(ctx, "Client registration not supported", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return err
	}

	state, err := mcp.GenerateState()
	if err != nil {
		slog.ErrorContext(ctx, "Failed to generate state", "error", err)
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
		nil,
	)

	slog.DebugContext(ctx, "Authorization URL built", "url", authURL)

	slog.DebugContext(ctx, "Requesting authorization code")
	code, receivedState, err := mcp.RequestAuthorizationCode(oauthCtx, authURL, callbackServer, state)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to get authorization code", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return fmt.Errorf("failed to get authorization code: %w", err)
	}

	if receivedState != state {
		err := fmt.Errorf("state mismatch: expected %s, got %s", state, receivedState)
		slog.ErrorContext(ctx, "State mismatch in authorization response", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return err
	}

	slog.DebugContext(ctx, "Authorization code received, exchanging for token")

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
		slog.ErrorContext(ctx, "Failed to exchange code for token", "error", err)
		_ = r.client.ResumeElicitation(ctx, r.sessionID, "decline", nil)
		return fmt.Errorf("failed to exchange code for token: %w", err)
	}

	slog.DebugContext(ctx, "Token obtained successfully", "token_type", token.TokenType)

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

	slog.DebugContext(ctx, "Sending token to server")
	if err := r.client.ResumeElicitation(ctx, r.sessionID, tools.ElicitationActionAccept, tokenData); err != nil {
		slog.ErrorContext(ctx, "Failed to send token to server", "error", err)
		return fmt.Errorf("failed to send token to server: %w", err)
	}

	slog.DebugContext(ctx, "OAuth flow completed successfully")
	return nil
}

// SessionStore returns a RemoteSessionStore that wraps the remote client.
func (r *RemoteRuntime) SessionStore() session.Store {
	return NewRemoteSessionStore(r.client)
}

// AvailableModels returns available models for the agent.
func (r *RemoteRuntime) AvailableModels(ctx context.Context) []ModelChoice {
	models, err := r.client.GetAvailableModels(ctx)
	if err != nil {
		slog.WarnContext(ctx, "Failed to get available models", "error", err)
		return nil
	}
	choices := make([]ModelChoice, len(models))
	for i, m := range models {
		choices[i] = ModelChoice{Name: m, Ref: m}
	}
	return choices
}

// SetAgentModel sets the model for the agent.
func (r *RemoteRuntime) SetAgentModel(ctx context.Context, _, modelRef string) error {
	if r.sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.SetAgentModel(ctx, r.sessionID, modelRef)
}

// SupportsModelSwitching returns true for remote runtimes (model switching is handled server-side).
func (r *RemoteRuntime) SupportsModelSwitching() bool {
	return true
}

// PermissionsInfo returns nil for remote runtime since permissions are handled server-side.
func (r *RemoteRuntime) PermissionsInfo() *PermissionsInfo {
	return nil
}

// ResetStartupInfo is a no-op for remote runtime.
func (r *RemoteRuntime) ResetStartupInfo() {
}

// CurrentAgentSkillsToolset returns nil for remote runtimes since skills are managed server-side.
func (r *RemoteRuntime) CurrentAgentSkillsToolset() *skills.Toolset {
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

// CurrentMCPPrompts returns available MCP prompts from the server.
func (r *RemoteRuntime) CurrentMCPPrompts(ctx context.Context) map[string]mcp.PromptInfo {
	if r.sessionID == "" {
		return make(map[string]mcp.PromptInfo)
	}
	prompts, err := r.client.GetSessionMCPPrompts(ctx, r.sessionID)
	if err != nil {
		slog.WarnContext(ctx, "Failed to get MCP prompts", "error", err)
		return make(map[string]mcp.PromptInfo)
	}
	// Convert map[string]any to map[string]mcp.PromptInfo
	result := make(map[string]mcp.PromptInfo)
	for k, v := range prompts {
		if info, ok := v.(mcp.PromptInfo); ok {
			result[k] = info
		}
	}
	return result
}

// ExecuteMCPPrompt executes an MCP prompt on the server.
func (r *RemoteRuntime) ExecuteMCPPrompt(ctx context.Context, promptName string, args map[string]string) (string, error) {
	if r.sessionID == "" {
		return "", errors.New("no active session")
	}
	return r.client.ExecuteSessionMCPPrompt(ctx, r.sessionID, promptName, args)
}

// TitleGenerator is not supported on remote runtimes (titles are generated server-side).
func (r *RemoteRuntime) TitleGenerator() *sessiontitle.Generator {
	return nil
}

// TogglePause pauses/resumes a session on the server.
func (r *RemoteRuntime) TogglePause(ctx context.Context) (bool, error) {
	if r.sessionID == "" {
		return false, errors.New("no active session")
	}
	return false, r.client.PauseSession(ctx, r.sessionID)
}

// OnToolsChanged is a no-op for remote runtimes; tool-list changes are
// observed server-side and surface through the run-stream events rather
// than via an out-of-band callback.
func (r *RemoteRuntime) OnToolsChanged(func(Event)) {}

// Close is a no-op for remote runtimes.
func (r *RemoteRuntime) Close() error {
	return nil
}

// GetSnapshots retrieves available snapshots for the current session.
func (r *RemoteRuntime) GetSnapshots(ctx context.Context) ([]map[string]any, error) {
	if r.sessionID == "" {
		return nil, errors.New("no active session")
	}
	return r.client.GetSessionSnapshots(ctx, r.sessionID)
}

// Undo reverts to the previous snapshot on the remote server.
func (r *RemoteRuntime) Undo(ctx context.Context) error {
	if r.sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.UndoSession(ctx, r.sessionID)
}

// Reset resets the session to its initial state on the remote server.
func (r *RemoteRuntime) Reset(ctx context.Context) error {
	if r.sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.ResetSession(ctx, r.sessionID)
}

// AddMessageToSession adds a message to the current session on the remote server.
func (r *RemoteRuntime) AddMessageToSession(ctx context.Context, msg *session.Message) error {
	if r.sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.AddMessage(ctx, r.sessionID, msg)
}

// UpdateSessionMessage updates a message in the current session on the remote server.
func (r *RemoteRuntime) UpdateSessionMessage(ctx context.Context, msgID string, msg *session.Message) error {
	if r.sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.UpdateMessage(ctx, r.sessionID, msgID, msg)
}

// AddSessionSummary adds a summary to the current session on the remote server.
func (r *RemoteRuntime) AddSessionSummary(ctx context.Context, summary string, tokens int) error {
	if r.sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.AddSummary(ctx, r.sessionID, summary, tokens)
}

// UpdateSessionTokens updates token counts for the current session on the remote server.
func (r *RemoteRuntime) UpdateSessionTokens(ctx context.Context, inputTokens, outputTokens int64, cost float64) error {
	if r.sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.UpdateSessionTokens(ctx, r.sessionID, inputTokens, outputTokens, cost)
}

// SetSessionStarred sets the starred status for the current session on the remote server.
func (r *RemoteRuntime) SetSessionStarred(ctx context.Context, starred bool) error {
	if r.sessionID == "" {
		return errors.New("no active session")
	}
	return r.client.SetSessionStarred(ctx, r.sessionID, starred)
}

var _ Runtime = (*RemoteRuntime)(nil)

// RemoteSessionStore wraps a RemoteClient to implement the session.Store interface.
type RemoteSessionStore struct {
	client RemoteClient
}

// NewRemoteSessionStore creates a new RemoteSessionStore.
func NewRemoteSessionStore(client RemoteClient) *RemoteSessionStore {
	return &RemoteSessionStore{client: client}
}

func (s *RemoteSessionStore) AddSession(context.Context, *session.Session) error {
	return fmt.Errorf("add session: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) GetSession(context.Context, string) (*session.Session, error) {
	return nil, fmt.Errorf("get session: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) GetSessions(ctx context.Context) ([]*session.Session, error) {
	sessions, err := s.client.GetAllSessions(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]*session.Session, len(sessions))
	for i := range sessions {
		result[i] = &sessions[i]
	}
	return result, nil
}

func (s *RemoteSessionStore) GetSessionSummaries(context.Context) ([]session.Summary, error) {
	return nil, fmt.Errorf("get session summaries: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) DeleteSession(ctx context.Context, id string) error {
	return s.client.DeleteRemoteSession(ctx, id)
}

func (s *RemoteSessionStore) UpdateSession(context.Context, *session.Session) error {
	return fmt.Errorf("update session: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) SetSessionStarred(context.Context, string, bool) error {
	return fmt.Errorf("set session starred: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) AddMessage(context.Context, string, *session.Message) (int64, error) {
	return 0, fmt.Errorf("add message: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) UpdateMessage(context.Context, int64, *session.Message) error {
	return fmt.Errorf("update message: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) AddSubSession(context.Context, string, *session.Session) error {
	return fmt.Errorf("add sub session: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) AddSummary(context.Context, string, string, int) error {
	return fmt.Errorf("add summary: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) UpdateSessionTokens(context.Context, string, int64, int64, float64) error {
	return fmt.Errorf("update session tokens: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) UpdateSessionTitle(context.Context, string, string) error {
	return fmt.Errorf("update session title: %w", ErrUnsupported)
}

func (s *RemoteSessionStore) Close() error {
	return nil
}

var _ session.Store = (*RemoteSessionStore)(nil)
