package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"time"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// Client is an HTTP client for the docker agent server API
type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
	authToken  string
	registry   map[string]func() Event
}

// ClientOption is a function for configuring the Client
type ClientOption func(*Client)

// WithHTTPClient sets a custom HTTP client
func WithHTTPClient(client *http.Client) ClientOption {
	return func(c *Client) {
		c.httpClient = client
	}
}

// WithAuthToken sets the bearer token for authentication
func WithAuthToken(token string) ClientOption {
	return func(c *Client) {
		c.authToken = token
	}
}

// WithTimeout sets the HTTP client timeout (deprecated: prefer per-request timeouts)
func WithTimeout(timeout time.Duration) ClientOption {
	return func(c *Client) {
		if c.httpClient == nil {
			c.httpClient = &http.Client{}
		}
		c.httpClient.Timeout = timeout
	}
}

// timeoutFor returns the appropriate timeout for a request category
func (c *Client) timeoutFor(category string) time.Duration {
	// Short timeout for metadata/CRUD operations
	if category == "metadata" || category == "crud" {
		return 30 * time.Second
	}
	// Long timeout for streaming/SSE operations
	return 5 * time.Minute
}

// NewClient creates a new HTTP client for the docker agent server
func NewClient(baseURL string, opts ...ClientOption) (*Client, error) {
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}

	client := &Client{
		baseURL: parsedURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		registry: map[string]func() Event{
			"user_message":           func() Event { return &UserMessageEvent{} },
			"tool_call":              func() Event { return &ToolCallEvent{} },
			"tool_call_response":     func() Event { return &ToolCallResponseEvent{} },
			"tool_call_confirmation": func() Event { return &ToolCallConfirmationEvent{} },
			"token_usage":            func() Event { return &TokenUsageEvent{} },
			"stream_stopped":         func() Event { return &StreamStoppedEvent{} },
			"stream_started":         func() Event { return &StreamStartedEvent{} },
			"shell":                  func() Event { return &ShellOutputEvent{} },
			"session_title":          func() Event { return &SessionTitleEvent{} },
			"session_summary":        func() Event { return &SessionSummaryEvent{} },
			"session_compaction":     func() Event { return &SessionCompactionEvent{} },
			"partial_tool_call":      func() Event { return &PartialToolCallEvent{} },
			"max_iterations_reached": func() Event { return &MaxIterationsReachedEvent{} },
			"error":                  func() Event { return &ErrorEvent{} },
			"elicitation_request":    func() Event { return &ElicitationRequestEvent{} },
			"authorization_event":    func() Event { return &AuthorizationEvent{} },
			"agent_choice":           func() Event { return &AgentChoiceEvent{} },
			"agent_choice_reasoning": func() Event { return &AgentChoiceReasoningEvent{} },
			"mcp_init_started":       func() Event { return &MCPInitStartedEvent{} },
			"mcp_init_finished":      func() Event { return &MCPInitFinishedEvent{} },
			"agent_info":             func() Event { return &AgentInfoEvent{} },
			"team_info":              func() Event { return &TeamInfoEvent{} },
			"toolset_info":           func() Event { return &ToolsetInfoEvent{} },
			"agent_switching":        func() Event { return &AgentSwitchingEvent{} },
			"warning":                func() Event { return &WarningEvent{} },
			"hook_blocked":           func() Event { return &HookBlockedEvent{} },
			"hook_started":           func() Event { return &HookStartedEvent{} },
			"hook_finished":          func() Event { return &HookFinishedEvent{} },
			"rag_indexing_started":   func() Event { return &RAGIndexingStartedEvent{} },
			"rag_indexing_progress":  func() Event { return &RAGIndexingProgressEvent{} },
			"rag_indexing_completed": func() Event { return &RAGIndexingCompletedEvent{} },
			"message_added":          func() Event { return &MessageAddedEvent{} },
			"model_fallback":         func() Event { return &ModelFallbackEvent{} },
			"sub_session_completed":  func() Event { return &SubSessionCompletedEvent{} },
			"connection_lost":        func() Event { return &ConnectionLostEvent{} },
			"connection_restored":    func() Event { return &ConnectionRestoredEvent{} },
		},
	}

	for _, opt := range opts {
		opt(client)
	}

	return client, nil
}

// ErrorResponse represents an error response from the API
type ErrorResponse struct {
	Error string `json:"error"`
}

// doRequest performs an HTTP request and handles common response patterns
func (c *Client) doRequest(ctx context.Context, method, endpoint string, body, result any) error {
	return c.doRequestWithTimeout(ctx, method, endpoint, body, result, "crud")
}

// doRequestWithTimeout performs an HTTP request with explicit timeout category
func (c *Client) doRequestWithTimeout(ctx context.Context, method, endpoint string, body, result any, timeoutCategory string) error {
	var reqBody io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshaling request body: %w", err)
		}
		reqBody = bytes.NewReader(jsonBody)
	}

	u := *c.baseURL
	u.Path = path.Join(u.Path, endpoint)

	// Apply per-request timeout based on category
	timeout := c.timeoutFor(timeoutCategory)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, u.String(), reqBody)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("performing request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		var errResp ErrorResponse
		if err := json.Unmarshal(respBody, &errResp); err == nil && errResp.Error != "" {
			return fmt.Errorf("API error (%d): %s", resp.StatusCode, errResp.Error)
		}
		return fmt.Errorf("HTTP error %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("unmarshaling response: %w", err)
		}
	}

	return nil
}

// GetAgents retrieves all available agents
func (c *Client) GetAgents(ctx context.Context) ([]api.Agent, error) {
	var agents []api.Agent
	err := c.doRequest(ctx, http.MethodGet, "/api/agents", nil, &agents)
	return agents, err
}

// GetAgent retrieves an agent by ID
func (c *Client) GetAgent(ctx context.Context, id string) (*latest.Config, error) {
	var config latest.Config
	err := c.doRequest(ctx, http.MethodGet, "/api/agents/"+id, nil, &config)
	return &config, err
}

// CreateAgent creates a new agent using a prompt
func (c *Client) CreateAgent(ctx context.Context, prompt string) (*api.CreateAgentResponse, error) {
	req := api.CreateAgentRequest{Prompt: prompt}
	var resp api.CreateAgentResponse
	err := c.doRequest(ctx, http.MethodPost, "/api/agents", req, &resp)
	return &resp, err
}

// CreateAgentConfig creates a new agent manually with YAML configuration
func (c *Client) CreateAgentConfig(ctx context.Context, filename, model, description, instruction string) (*api.CreateAgentConfigResponse, error) {
	req := api.CreateAgentConfigRequest{
		Filename:    filename,
		Model:       model,
		Description: description,
		Instruction: instruction,
	}
	var resp api.CreateAgentConfigResponse
	err := c.doRequest(ctx, http.MethodPost, "/api/agents/config", req, &resp)
	return &resp, err
}

// EditAgentConfig edits an agent configuration
func (c *Client) EditAgentConfig(ctx context.Context, filename string, config latest.Config) (*api.EditAgentConfigResponse, error) {
	req := api.EditAgentConfigRequest{
		AgentConfig: config,
		Filename:    filename,
	}
	var resp api.EditAgentConfigResponse
	err := c.doRequest(ctx, "PUT", "/api/agents/config", req, &resp)
	return &resp, err
}

// ImportAgent imports an agent from a file path
func (c *Client) ImportAgent(ctx context.Context, filePath string) (*api.ImportAgentResponse, error) {
	req := api.ImportAgentRequest{FilePath: filePath}
	var resp api.ImportAgentResponse
	err := c.doRequest(ctx, http.MethodPost, "/api/agents/import", req, &resp)
	return &resp, err
}

// ExportAgents exports multiple agents as a zip file
func (c *Client) ExportAgents(ctx context.Context) (*api.ExportAgentsResponse, error) {
	var resp api.ExportAgentsResponse
	err := c.doRequest(ctx, http.MethodPost, "/api/agents/export", nil, &resp)
	return &resp, err
}

// PullAgent pulls an agent from a remote registry
func (c *Client) PullAgent(ctx context.Context, name string) (*api.PullAgentResponse, error) {
	req := api.PullAgentRequest{Name: name}
	var resp api.PullAgentResponse
	err := c.doRequest(ctx, http.MethodPost, "/api/agents/pull", req, &resp)
	return &resp, err
}

// PushAgent pushes an agent to a remote registry
func (c *Client) PushAgent(ctx context.Context, filepath, tag string) (*api.PushAgentResponse, error) {
	req := api.PushAgentRequest{Filepath: filepath, Tag: tag}
	var resp api.PushAgentResponse
	err := c.doRequest(ctx, http.MethodPost, "/api/agents/push", req, &resp)
	return &resp, err
}

// DeleteAgent deletes an agent by file path
func (c *Client) DeleteAgent(ctx context.Context, filePath string) (*api.DeleteAgentResponse, error) {
	req := api.DeleteAgentRequest{FilePath: filePath}
	var resp api.DeleteAgentResponse
	err := c.doRequest(ctx, "DELETE", "/api/agents", req, &resp)
	return &resp, err
}

// GetSessions retrieves all sessions
func (c *Client) GetSessions(ctx context.Context) ([]api.SessionsResponse, error) {
	var sessions []api.SessionsResponse
	err := c.doRequest(ctx, http.MethodGet, "/api/sessions", nil, &sessions)
	return sessions, err
}

// GetSession retrieves a session by ID
func (c *Client) GetSession(ctx context.Context, id string) (*api.SessionResponse, error) {
	var sess api.SessionResponse
	err := c.doRequest(ctx, http.MethodGet, "/api/sessions/"+id, nil, &sess)
	return &sess, err
}

// CreateSession creates a new session
func (c *Client) CreateSession(ctx context.Context, sessTemplate *session.Session) (*session.Session, error) {
	var sess session.Session
	err := c.doRequest(ctx, http.MethodPost, "/api/sessions", sessTemplate, &sess)
	return &sess, err
}

// ResumeSession resumes a session by ID with optional rejection reason or tool name
func (c *Client) ResumeSession(ctx context.Context, id, confirmation, reason, toolName string) error {
	req := api.ResumeSessionRequest{Confirmation: confirmation, Reason: reason, ToolName: toolName}
	return c.doRequest(ctx, http.MethodPost, "/api/sessions/"+id+"/resume", req, nil)
}

// SteerSession injects user messages into a running session mid-turn.
func (c *Client) SteerSession(ctx context.Context, sessionID string, messages []api.Message) error {
	req := api.SteerSessionRequest{Messages: messages}
	return c.doRequest(ctx, http.MethodPost, "/api/sessions/"+sessionID+"/steer", req, nil)
}

// FollowUpSession queues messages for end-of-turn processing.
func (c *Client) FollowUpSession(ctx context.Context, sessionID string, messages []api.Message) error {
	req := api.SteerSessionRequest{Messages: messages}
	return c.doRequest(ctx, http.MethodPost, "/api/sessions/"+sessionID+"/followup", req, nil)
}

// DeleteSession deletes a session by ID
func (c *Client) DeleteSession(ctx context.Context, id string) error {
	return c.doRequest(ctx, "DELETE", "/api/sessions/"+id, nil, nil)
}

// GetDesktopToken retrieves a desktop authentication token
func (c *Client) GetDesktopToken(ctx context.Context) (*api.DesktopTokenResponse, error) {
	var resp api.DesktopTokenResponse
	err := c.doRequest(ctx, http.MethodGet, "/api/desktop/token", nil, &resp)
	return &resp, err
}

// RunAgent executes an agent and returns a channel of streaming events
func (c *Client) RunAgent(ctx context.Context, sessionID, agent string, messages []api.Message) (<-chan Event, error) {
	return c.runAgentWithAgentName(ctx, sessionID, agent, "", messages)
}

// RunAgentWithAgentName executes an agent with a specific agent name and returns a channel of streaming events
func (c *Client) RunAgentWithAgentName(ctx context.Context, sessionID, agent, agentName string, messages []api.Message) (<-chan Event, error) {
	return c.runAgentWithAgentName(ctx, sessionID, agent, agentName, messages)
}

func (c *Client) runAgentWithAgentName(ctx context.Context, sessionID, agent, agentName string, messages []api.Message) (<-chan Event, error) {
	endpoint := "/api/sessions/" + sessionID + "/agent/" + agent
	if agentName != "" {
		endpoint += "/" + agentName
	}

	jsonBody, err := json.Marshal(messages)
	if err != nil {
		return nil, fmt.Errorf("marshaling messages: %w", err)
	}

	u := *c.baseURL
	u.Path = path.Join(u.Path, endpoint)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}

	resp, err := c.httpClient.Do(req) //nolint:bodyclose // body is closed in the goroutine below
	if err != nil {
		return nil, fmt.Errorf("performing request: %w", err)
	}

	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("reading error response body: %w", err)
		}

		var errResp ErrorResponse
		if err := json.Unmarshal(respBody, &errResp); err == nil && errResp.Error != "" {
			return nil, fmt.Errorf("API error (%d): %s", resp.StatusCode, errResp.Error)
		}
		return nil, fmt.Errorf("HTTP error %d: %s", resp.StatusCode, string(respBody))
	}

	eventChan := make(chan Event, defaultEventChannelCapacity)

	go func() {
		defer close(eventChan)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 || line[0] == ':' {
				continue
			}

			after, ok := bytes.CutPrefix(line, []byte("data: "))
			if !ok {
				continue
			}

			slog.DebugContext(ctx, "event", "event", string(after))

			// First unmarshal to get the type
			var baseEvent struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(after, &baseEvent); err != nil {
				slog.DebugContext(ctx, "event", "error", err)
				continue
			}

			// Then unmarshal the full event
			createEvent, found := c.registry[baseEvent.Type]
			if !found {
				slog.DebugContext(ctx, "event", "invalid_type", baseEvent.Type)
				continue
			}

			e := createEvent()
			if err := json.Unmarshal(after, &e); err != nil {
				slog.DebugContext(ctx, "event", "error", err)
				continue
			}

			eventChan <- e
		}

		if err := scanner.Err(); err != nil {
			return
		}
	}()

	return eventChan, nil
}

// GetAllSessions retrieves all sessions from the remote store.
func (c *Client) GetAllSessions(ctx context.Context) ([]session.Session, error) {
	var sessions []session.Session
	err := c.doRequest(ctx, http.MethodGet, "/api/sessions", nil, &sessions)
	return sessions, err
}

// DeleteRemoteSession deletes a session from the remote store.
func (c *Client) DeleteRemoteSession(ctx context.Context, sessionID string) error {
	return c.doRequest(ctx, http.MethodDelete, "/api/sessions/"+sessionID, nil, nil)
}

func (c *Client) ResumeElicitation(ctx context.Context, sessionID string, action tools.ElicitationAction, content map[string]any) error {
	req := api.ResumeElicitationRequest{Action: string(action), Content: content}
	return c.doRequest(ctx, http.MethodPost, "/api/sessions/"+sessionID+"/elicitation", req, nil)
}

// UpdateSessionTitle updates the title of a session
func (c *Client) UpdateSessionTitle(ctx context.Context, sessionID, title string) error {
	req := api.UpdateSessionTitleRequest{Title: title}
	return c.doRequest(ctx, http.MethodPatch, "/api/sessions/"+sessionID+"/title", req, nil)
}

// GetAgentToolCount returns the number of tools available for an agent.
func (c *Client) GetAgentToolCount(ctx context.Context, agentFilename, agentName string) (int, error) {
	var resp struct {
		AvailableTools int `json:"available_tools"`
	}
	endpoint := fmt.Sprintf("/api/agents/%s/%s/tools/count", url.PathEscape(agentFilename), url.PathEscape(agentName))
	err := c.doRequest(ctx, http.MethodGet, endpoint, nil, &resp)
	if err != nil {
		return 0, err
	}

	return resp.AvailableTools, nil
}

// StreamSessionEvents streams events for a session as they occur via Server-Sent Events.
// The returned channel is closed when ctx is cancelled, the stream's max
// duration is reached, or the server closes the connection.
func (c *Client) StreamSessionEvents(ctx context.Context, sessionID string) (<-chan Event, error) {
	endpoint := fmt.Sprintf("/api/sessions/%s/events", sessionID)

	u := *c.baseURL
	u.Path = path.Join(u.Path, endpoint)

	// Bound the maximum lifetime of a single SSE connection. The cancel
	// must be tied to the goroutine consuming the stream, not to this
	// function's return: cancelling streamCtx kills the in-flight HTTP
	// request, which would turn the stream into a one-shot read.
	timeout := c.timeoutFor("streaming")
	streamCtx, cancel := context.WithTimeout(ctx, timeout)

	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}

	resp, err := c.httpClient.Do(req) //nolint:bodyclose // body is closed in the goroutine below
	if err != nil {
		cancel()
		return nil, fmt.Errorf("performing request: %w", err)
	}

	if resp.StatusCode >= 400 {
		defer cancel()
		defer resp.Body.Close()
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("reading error response body: %w", err)
		}

		var errResp ErrorResponse
		if err := json.Unmarshal(respBody, &errResp); err == nil && errResp.Error != "" {
			return nil, fmt.Errorf("API error (%d): %s", resp.StatusCode, errResp.Error)
		}
		return nil, fmt.Errorf("HTTP error %d: %s", resp.StatusCode, string(respBody))
	}

	eventChan := make(chan Event, defaultEventChannelCapacity)

	go func() {
		defer cancel()
		defer close(eventChan)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 || line[0] == ':' {
				continue
			}

			after, ok := bytes.CutPrefix(line, []byte("data: "))
			if !ok {
				continue
			}

			slog.DebugContext(ctx, "received event", "data", string(after))

			// First unmarshal to get the type
			var baseEvent struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(after, &baseEvent); err != nil {
				slog.DebugContext(ctx, "failed to unmarshal event type", "error", err)
				continue
			}

			// Then unmarshal the full event
			createEvent, found := c.registry[baseEvent.Type]
			if !found {
				slog.DebugContext(ctx, "unknown event type", "type", baseEvent.Type)
				continue
			}

			e := createEvent()
			if err := json.Unmarshal(after, &e); err != nil {
				slog.DebugContext(ctx, "failed to unmarshal event", "error", err)
				continue
			}

			eventChan <- e
		}

		if err := scanner.Err(); err != nil {
			slog.DebugContext(ctx, "scanner error", "error", err)
		}
	}()

	return eventChan, nil
}

// GetSessionTools retrieves tools available in a session.
func (c *Client) GetSessionTools(ctx context.Context, sessionID string) ([]tools.Tool, error) {
	var toolList []tools.Tool
	endpoint := fmt.Sprintf("/api/sessions/%s/tools", sessionID)
	err := c.doRequest(ctx, http.MethodGet, endpoint, nil, &toolList)
	return toolList, err
}

// GetAvailableModels returns available models for the agent.
func (c *Client) GetAvailableModels(ctx context.Context) ([]string, error) {
	var models []string
	err := c.doRequest(ctx, http.MethodGet, "/api/models", nil, &models)
	return models, err
}

// SetAgentModel sets the model for the agent in a session.
func (c *Client) SetAgentModel(ctx context.Context, sessionID, model string) error {
	req := map[string]string{"model": model}
	return c.doRequest(ctx, http.MethodPost, "/api/sessions/"+sessionID+"/model", req, nil)
}

// GetSessionMCPPrompts returns available MCP prompts for a session.
func (c *Client) GetSessionMCPPrompts(ctx context.Context, sessionID string) (map[string]any, error) {
	var prompts map[string]any
	endpoint := fmt.Sprintf("/api/sessions/%s/mcp/prompts", sessionID)
	err := c.doRequest(ctx, http.MethodGet, endpoint, nil, &prompts)
	return prompts, err
}

// ExecuteSessionMCPPrompt executes an MCP prompt in a session.
func (c *Client) ExecuteSessionMCPPrompt(ctx context.Context, sessionID, promptName string, args map[string]string) (string, error) {
	endpoint := fmt.Sprintf("/api/sessions/%s/mcp/prompts/%s/execute", sessionID, promptName)
	var result struct {
		Result string `json:"result"`
	}
	err := c.doRequest(ctx, http.MethodPost, endpoint, args, &result)
	return result.Result, err
}

// GetSessionSkills returns available skills for a session.
func (c *Client) GetSessionSkills(ctx context.Context, sessionID string) (map[string]any, error) {
	var skills map[string]any
	endpoint := fmt.Sprintf("/api/sessions/%s/skills", sessionID)
	err := c.doRequest(ctx, http.MethodGet, endpoint, nil, &skills)
	return skills, err
}

// CompactSession triggers session compaction on the server.
func (c *Client) CompactSession(ctx context.Context, sessionID string) error {
	endpoint := fmt.Sprintf("/api/sessions/%s/compact", sessionID)
	return c.doRequest(ctx, http.MethodPost, endpoint, nil, nil)
}

// GetSessionToolsets returns toolset statuses for a session.
func (c *Client) GetSessionToolsets(ctx context.Context, sessionID string) ([]map[string]any, error) {
	var toolsets []map[string]any
	endpoint := fmt.Sprintf("/api/sessions/%s/toolsets", sessionID)
	err := c.doRequest(ctx, http.MethodGet, endpoint, nil, &toolsets)
	return toolsets, err
}

// RestartSessionToolset restarts a toolset in a session.
func (c *Client) RestartSessionToolset(ctx context.Context, sessionID, toolsetName string) error {
	endpoint := fmt.Sprintf("/api/sessions/%s/toolsets/%s/restart", sessionID, toolsetName)
	return c.doRequest(ctx, http.MethodPost, endpoint, nil, nil)
}

// PauseSession pauses a session.
func (c *Client) PauseSession(ctx context.Context, sessionID string) error {
	endpoint := fmt.Sprintf("/api/sessions/%s/pause", sessionID)
	return c.doRequest(ctx, http.MethodPost, endpoint, nil, nil)
}

// GetSessionSnapshots retrieves snapshots for a session.
func (c *Client) GetSessionSnapshots(ctx context.Context, sessionID string) ([]map[string]any, error) {
	var snapshots []map[string]any
	endpoint := fmt.Sprintf("/api/sessions/%s/snapshots", sessionID)
	err := c.doRequest(ctx, http.MethodGet, endpoint, nil, &snapshots)
	return snapshots, err
}

// UndoSession reverts a session to the previous snapshot.
func (c *Client) UndoSession(ctx context.Context, sessionID string) error {
	endpoint := fmt.Sprintf("/api/sessions/%s/undo", sessionID)
	return c.doRequest(ctx, http.MethodPost, endpoint, nil, nil)
}

// ResetSession resets a session to initial state.
func (c *Client) ResetSession(ctx context.Context, sessionID string) error {
	endpoint := fmt.Sprintf("/api/sessions/%s/reset", sessionID)
	return c.doRequest(ctx, http.MethodPost, endpoint, nil, nil)
}

// AddMessage adds a message to a session.
func (c *Client) AddMessage(ctx context.Context, sessionID string, msg *session.Message) error {
	endpoint := fmt.Sprintf("/api/sessions/%s/messages", sessionID)
	req := api.AddMessageRequest{Message: msg}
	return c.doRequest(ctx, http.MethodPost, endpoint, req, nil)
}

// UpdateMessage updates a message in a session.
func (c *Client) UpdateMessage(ctx context.Context, sessionID, msgID string, msg *session.Message) error {
	endpoint := fmt.Sprintf("/api/sessions/%s/messages/%s", sessionID, msgID)
	req := api.UpdateMessageRequest{Message: msg}
	return c.doRequest(ctx, http.MethodPatch, endpoint, req, nil)
}

// AddSummary adds a summary to a session.
func (c *Client) AddSummary(ctx context.Context, sessionID, summary string, tokens int) error {
	endpoint := fmt.Sprintf("/api/sessions/%s/summaries", sessionID)
	req := api.AddSummaryRequest{Summary: summary, Tokens: tokens}
	return c.doRequest(ctx, http.MethodPost, endpoint, req, nil)
}

// UpdateSessionTokens updates token counts for a session.
func (c *Client) UpdateSessionTokens(ctx context.Context, sessionID string, inputTokens, outputTokens int64, cost float64) error {
	endpoint := fmt.Sprintf("/api/sessions/%s/tokens", sessionID)
	req := api.UpdateSessionTokensRequest{InputTokens: inputTokens, OutputTokens: outputTokens, Cost: cost}
	return c.doRequest(ctx, http.MethodPatch, endpoint, req, nil)
}

// SetSessionStarred sets the starred status for a session.
func (c *Client) SetSessionStarred(ctx context.Context, sessionID string, starred bool) error {
	endpoint := fmt.Sprintf("/api/sessions/%s/starred", sessionID)
	req := api.SetSessionStarredRequest{Starred: starred}
	return c.doRequest(ctx, http.MethodPatch, endpoint, req, nil)
}

// Health checks the health of the remote server.
func (c *Client) Health(ctx context.Context) error {
	var resp api.HealthResponse
	return c.doRequest(ctx, http.MethodGet, "/health", nil, &resp)
}

// Ready checks if the remote server is ready to handle requests.
func (c *Client) Ready(ctx context.Context) (*api.ReadyResponse, error) {
	var resp api.ReadyResponse
	if err := c.doRequest(ctx, http.MethodGet, "/ready", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// StreamSessionEventsWithRetry streams events for a session with exponential backoff reconnection.
// It emits ConnectionLostEvent and ConnectionRestoredEvent on connection changes.
// Backs off exponentially between retries: 100ms, 200ms, 400ms, 800ms, 1600ms (max).
func (c *Client) StreamSessionEventsWithRetry(ctx context.Context, sessionID string) (<-chan Event, error) {
	output := make(chan Event, defaultEventChannelCapacity)

	go func() {
		defer close(output)
		attempt := 0
		const maxBackoff = 1600 * time.Millisecond
		const initialBackoff = 100 * time.Millisecond

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			attempt++
			stream, err := c.StreamSessionEvents(ctx, sessionID)
			if err != nil {
				if attempt == 1 {
					// First attempt failed; report and exit
					return
				}
				// Emit connection lost event
				select {
				case output <- ConnectionLost(err.Error(), attempt):
				case <-ctx.Done():
					return
				}

				// Calculate backoff: 100ms, 200ms, 400ms, 800ms, 1600ms (max)
				backoff := initialBackoff
				if attempt > 1 {
					backoff = initialBackoff * time.Duration(1<<uint(attempt-2))
				}
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				select {
				case <-time.After(backoff):
					continue
				case <-ctx.Done():
					return
				}
			}

			// Connection successful; emit restored event (if not first attempt)
			if attempt > 1 {
				select {
				case output <- ConnectionRestored(attempt):
				case <-ctx.Done():
					return
				}
			}

			// Stream events until error
			for event := range stream {
				select {
				case output <- event:
				case <-ctx.Done():
					return
				}
			}

			// Stream ended; will retry
		}
	}()

	return output, nil
}

// GetSessionRecoveryData retrieves recovery data for a session in case of store failure
func (c *Client) GetSessionRecoveryData(ctx context.Context, sessionID string) (map[string]any, error) {
	var data map[string]any
	endpoint := fmt.Sprintf("/api/sessions/%s/recovery", sessionID)
	err := c.doRequest(ctx, http.MethodGet, endpoint, nil, &data)
	return data, err
}

// BatchDeleteSessions deletes multiple sessions in a single operation
func (c *Client) BatchDeleteSessions(ctx context.Context, sessionIDs []string) (map[string]any, error) {
	var resp map[string]any
	req := api.BatchDeleteSessionsRequest{SessionIDs: sessionIDs}
	err := c.doRequest(ctx, http.MethodPost, "/api/sessions/batch/delete", req, &resp)
	return resp, err
}

// BatchExportSessions exports multiple sessions
func (c *Client) BatchExportSessions(ctx context.Context, sessionIDs []string, format string) (map[string]any, error) {
	var resp map[string]any
	req := api.BatchExportSessionsRequest{SessionIDs: sessionIDs, Format: format}
	err := c.doRequest(ctx, http.MethodPost, "/api/sessions/batch/export", req, &resp)
	return resp, err
}

// GetSessionQueueStatus retrieves the queue depth and capacity for a session
func (c *Client) GetSessionQueueStatus(ctx context.Context, sessionID string) (*api.QueueDepthResponse, error) {
	var resp api.QueueDepthResponse
	endpoint := fmt.Sprintf("/api/sessions/%s/queue", sessionID)
	err := c.doRequest(ctx, http.MethodGet, endpoint, nil, &resp)
	return &resp, err
}
