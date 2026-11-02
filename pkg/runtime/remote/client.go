package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config/latest"
	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// Client is an HTTP client for the docker agent server API
type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
	authToken  string
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

// WithTimeout sets the non-streaming HTTP timeout (deprecated: prefer per-request timeouts).
func WithTimeout(timeout time.Duration) ClientOption {
	return func(c *Client) {
		if c.httpClient == nil {
			c.httpClient = &http.Client{} //rubocop:disable Lint/HTTPClientTransport // remote runtime client; transport set via Clone in callers
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
		httpClient: &http.Client{ //rubocop:disable Lint/HTTPClientTransport // remote runtime base client; callers may add OTel transport via WithOTelTransport
			Timeout: 30 * time.Second,
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

// RunAgent executes an agent and returns a channel of streaming events. The
// optional model override is persisted on the session's current agent before
// the user messages are appended; pass an empty string to leave the existing
// override (if any) untouched. EOF without a root StreamStoppedEvent is reported
// as incomplete; a run is never automatically resubmitted.
func (c *Client) RunAgent(ctx context.Context, sessionID, agent string, messages []api.Message, model string) (<-chan agentruntime.Event, error) {
	return c.runAgentWithAgentName(ctx, sessionID, agent, "", messages, model)
}

// RunAgentWithAgentName executes an agent with a specific agent name and
// returns a channel of streaming events. See [Client.RunAgent] for the
// semantics of model.
func (c *Client) RunAgentWithAgentName(ctx context.Context, sessionID, agent, agentName string, messages []api.Message, model string) (<-chan agentruntime.Event, error) {
	return c.runAgentWithAgentName(ctx, sessionID, agent, agentName, messages, model)
}

func (c *Client) runAgentWithAgentName(ctx context.Context, sessionID, agent, agentName string, messages []api.Message, model string) (<-chan agentruntime.Event, error) {
	endpoint := "/api/sessions/" + sessionID + "/agent/" + agent
	if agentName != "" {
		endpoint += "/" + agentName
	}

	jsonBody, err := json.Marshal(api.RunAgentRequest{Messages: messages, Model: model})
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

	resp, err := c.streamingHTTPClient().Do(req) //nolint:bodyclose // body is closed in the goroutine below
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

	eventChan := make(chan agentruntime.Event, defaultEventChannelCapacity)

	go func() {
		defer close(eventChan)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		// A single SSE line can carry a large tool response; raise the cap
		// above bufio's 64 KiB default so an oversized line does not silently
		// truncate the stream (bufio.ErrTooLong).
		scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxSSELineBytes)
		var sawRootStop, sawError bool
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
				sendClientEvent(ctx, eventChan, agentruntime.Error(fmt.Sprintf("decoding remote agent event: %v", err)))
				return
			}

			// Then unmarshal the full event
			e, found := agentruntime.NewEvent(baseEvent.Type)
			if !found {
				slog.DebugContext(ctx, "event", "invalid_type", baseEvent.Type)
				continue
			}

			if err := json.Unmarshal(after, &e); err != nil {
				sendClientEvent(ctx, eventChan, agentruntime.Error(fmt.Sprintf("decoding remote agent event: %v", err)))
				return
			}

			switch event := e.(type) {
			case *agentruntime.StreamStoppedEvent:
				sawRootStop = sawRootStop || event.SessionID == "" || event.SessionID == sessionID
			case *agentruntime.ErrorEvent:
				sawError = true
			}
			if !sendClientEvent(ctx, eventChan, e) {
				return
			}
		}

		// Surface a read failure (e.g. an over-long line) instead of ending
		// the stream silently — otherwise the run appears to stop with no
		// error after the last event that fit.
		if err := scanner.Err(); err != nil {
			slog.DebugContext(ctx, "event", "scanner_error", err)
			if ctx.Err() == nil {
				sendClientEvent(ctx, eventChan, agentruntime.Error(fmt.Sprintf("reading event stream: %v", err)))
			}
			return
		}
		if ctx.Err() == nil && !sawRootStop && !sawError {
			sendClientEvent(ctx, eventChan, agentruntime.Error("remote agent stream ended before completion; the response may be incomplete"))
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

func (c *Client) ResumeElicitation(ctx context.Context, sessionID string, action tools.ElicitationAction, content map[string]any, elicitationID ...string) error {
	req := api.ResumeElicitationRequest{Action: string(action), Content: content, ElicitationID: firstElicitationID(elicitationID)}
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

// GetSessionSnapshot retrieves the recovery state and event-stream cursor.
func (c *Client) GetSessionSnapshot(ctx context.Context, sessionID string) (*api.SessionSnapshotResponse, error) {
	var snapshot api.SessionSnapshotResponse
	err := c.doRequest(ctx, http.MethodGet, "/api/sessions/"+sessionID+"/snapshot", nil, &snapshot)
	return &snapshot, err
}

const sessionEventGapError = "session event stream has a gap; reload the session snapshot before reconnecting"

// StreamSessionEvents replays buffered events, then tails the session. Sequenced
// streams reconnect after transport drops. A gap produces an ErrorEvent and
// closes the channel: callers must reload a snapshot before subscribing again.
// Unsequenced legacy streams cannot safely reconnect and close at EOF.
func (c *Client) StreamSessionEvents(ctx context.Context, sessionID string) (<-chan agentruntime.Event, error) {
	return c.streamSessionEvents(ctx, sessionID, nil)
}

// StreamSessionEventsSince tails events newer than a snapshot's LastEventSeq.
// It never replays older history; see StreamSessionEvents for gap handling.
func (c *Client) StreamSessionEventsSince(ctx context.Context, sessionID string, since uint64) (<-chan agentruntime.Event, error) {
	return c.streamSessionEvents(ctx, sessionID, &since)
}

// HTTP total timeouts also cover body reads; SSE lifetime belongs to its context.
func (c *Client) streamingHTTPClient() *http.Client {
	client := *c.httpClient
	client.Timeout = 0
	return &client
}

func sendClientEvent(ctx context.Context, events chan<- agentruntime.Event, event agentruntime.Event) bool {
	select {
	case events <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

func waitEventStreamRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

type sessionEventHTTPError struct {
	status int
	body   string
}

func (e *sessionEventHTTPError) Error() string {
	var response ErrorResponse
	if err := json.Unmarshal([]byte(e.body), &response); err == nil && response.Error != "" {
		return fmt.Sprintf("API error (%d): %s", e.status, response.Error)
	}
	return fmt.Sprintf("HTTP error %d: %s", e.status, e.body)
}

func (c *Client) openSessionEventStream(ctx context.Context, sessionID string, since *uint64) (*http.Response, error) {
	u := *c.baseURL
	u.Path = path.Join(u.Path, "/api/sessions/"+sessionID+"/events")
	// The explicit cursor must win over a query inherited from the base URL.
	query := u.Query()
	query.Del("since")
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if since != nil {
		req.Header.Set("Last-Event-ID", strconv.FormatUint(*since, 10))
	}
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	resp, err := c.streamingHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("performing request: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("reading error response body: %w", err)
		}
		return nil, &sessionEventHTTPError{status: resp.StatusCode, body: string(body)}
	}
	return resp, nil
}

func (c *Client) streamSessionEvents(ctx context.Context, sessionID string, since *uint64) (<-chan agentruntime.Event, error) {
	resp, err := c.openSessionEventStream(ctx, sessionID, since) //nolint:bodyclose // consumed and closed by readSessionEventStream
	if err != nil {
		return nil, err
	}
	events := make(chan agentruntime.Event, defaultEventChannelCapacity)
	go func() {
		defer close(events)
		delay := 250 * time.Millisecond
		for {
			terminal, readErr := c.readSessionEventStream(ctx, resp, events, &since)
			if terminal || ctx.Err() != nil {
				return
			}
			if errors.Is(readErr, bufio.ErrTooLong) {
				sendClientEvent(ctx, events, agentruntime.Error(fmt.Sprintf("reading event stream: %v", readErr)))
				return
			}
			// Replaying without a cursor can duplicate answers on older servers.
			if since == nil {
				if readErr != nil {
					sendClientEvent(ctx, events, agentruntime.Error(fmt.Sprintf("reading event stream: %v", readErr)))
				}
				return
			}
			for {
				if !waitEventStreamRetry(ctx, delay) {
					return
				}
				resp, err = c.openSessionEventStream(ctx, sessionID, since) //nolint:bodyclose // consumed and closed by readSessionEventStream
				if err == nil {
					break
				}
				var httpErr *sessionEventHTTPError
				if errors.As(err, &httpErr) && httpErr.status >= 400 && httpErr.status < 500 && httpErr.status != http.StatusTooManyRequests {
					sendClientEvent(ctx, events, agentruntime.Error(fmt.Sprintf("reconnecting session event stream: %v", err)))
					return
				}
				delay = min(2*delay, 5*time.Second)
			}
		}
	}()
	return events, nil
}

func (c *Client) readSessionEventStream(ctx context.Context, resp *http.Response, events chan<- agentruntime.Event, since **uint64) (terminal bool, err error) {
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxSSELineBytes)
	var id *uint64
	sawData := false
	for scanner.Scan() {
		line := scanner.Text()
		if raw, ok := strings.CutPrefix(line, "id:"); ok {
			seq, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
			if err != nil {
				sendClientEvent(ctx, events, agentruntime.Error("invalid session event cursor; reload the session snapshot"))
				return true, nil
			}
			id = &seq
			continue
		}
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		sawData = true
		var base struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(data), &base); err != nil {
			sendClientEvent(ctx, events, agentruntime.Error(fmt.Sprintf("decoding session event: %v", err)))
			return true, nil
		}
		if base.Type == "gap" {
			sendClientEvent(ctx, events, agentruntime.Error(sessionEventGapError))
			return true, nil
		}
		if base.Type == "session_exited" {
			return true, nil
		}
		if id == nil && *since != nil {
			sendClientEvent(ctx, events, agentruntime.Error("session event stream has no cursor; cannot safely resume"))
			return true, nil
		}
		if id != nil && *since != nil && *id <= **since {
			id = nil
			continue
		}
		if event, ok := agentruntime.NewEvent(base.Type); ok {
			if err := json.Unmarshal([]byte(data), event); err != nil {
				sendClientEvent(ctx, events, agentruntime.Error(fmt.Sprintf("decoding session event: %v", err)))
				return true, nil
			}
			if !sendClientEvent(ctx, events, event) {
				return true, nil
			}
		}
		if id != nil {
			*since = id
			id = nil
		}
	}
	if *since == nil && !sawData {
		// No event was delivered, so resuming from zero cannot duplicate it.
		*since = new(uint64)
	}
	return false, scanner.Err()
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

// AddSummary adds a summary item to a session.
func (c *Client) AddSummary(ctx context.Context, sessionID string, item session.Item) error {
	endpoint := fmt.Sprintf("/api/sessions/%s/summaries", sessionID)
	// Tokens mirrors FirstKeptEntry under its legacy wire name so older
	// servers keep understanding the request.
	req := api.AddSummaryRequest{
		Summary:        item.Summary,
		Tokens:         item.FirstKeptEntry,
		FirstKeptEntry: item.FirstKeptEntry,
		Cost:           item.Cost,
		Model:          item.Model,
		Usage:          item.Usage,
	}
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
