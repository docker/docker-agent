package mcp

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

type mcpClient interface {
	Initialize(ctx context.Context, request *mcp.InitializeRequest) (*mcp.InitializeResult, error)
	ListTools(ctx context.Context, request *mcp.ListToolsParams) iter.Seq2[*mcp.Tool, error]
	CallTool(ctx context.Context, request *mcp.CallToolParams) (*mcp.CallToolResult, error)
	ListPrompts(ctx context.Context, request *mcp.ListPromptsParams) iter.Seq2[*mcp.Prompt, error]
	GetPrompt(ctx context.Context, request *mcp.GetPromptParams) (*mcp.GetPromptResult, error)
	SetElicitationHandler(handler tools.ElicitationHandler)
	SetOAuthSuccessHandler(handler func())
	SetManagedOAuth(managed bool)
	SetToolListChangedHandler(handler func())
	SetPromptListChangedHandler(handler func())
	// Wait blocks until the underlying connection is closed by the server.
	// It returns nil if the connection was closed gracefully.
	Wait() error
	Close(ctx context.Context) error
}

// Toolset represents a set of MCP tools.
//
// Connection lifecycle (initial connect, watcher goroutine, restart with
// backoff, graceful Stop) is delegated to a *lifecycle.Supervisor; the
// historical watchConnection / tryRestart / forceReconnectAndWait helpers
// have been replaced by the supervisor's Start / RestartAndWait / Stop.
type Toolset struct {
	name         string
	mcpClient    mcpClient
	logID        string
	description  string // user-visible description, set by constructors
	instructions string

	supervisor *lifecycle.Supervisor

	mu sync.Mutex

	// Cached tools and prompts, invalidated via MCP notifications and
	// supervisor disconnect callbacks. cacheGen is bumped on each
	// invalidation so that a concurrent Tools()/ListPrompts() call can
	// detect that its result is stale and drop it.
	cachedTools   []tools.Tool
	cachedPrompts []PromptInfo
	cacheGen      uint64

	// toolsChangedHandler is called after the tool cache is refreshed
	// following a ToolListChanged notification from the server, or after
	// a successful supervisor reconnect.
	toolsChangedHandler func()
}

// invalidateCache clears the cached tools and prompts and bumps the
// generation counter. The caller must hold ts.mu.
func (ts *Toolset) invalidateCache() {
	ts.cachedTools = nil
	ts.cachedPrompts = nil
	ts.cacheGen++
}

// sessionMissingRetryTimeout is the maximum time to wait for the supervisor
// to restart the MCP server after an ErrSessionMissing error.
const sessionMissingRetryTimeout = 35 * time.Second

var (
	_ tools.ToolSet   = (*Toolset)(nil)
	_ tools.Describer = (*Toolset)(nil)
)

// Verify that Toolset implements optional capability interfaces
var (
	_ tools.Instructable   = (*Toolset)(nil)
	_ tools.Elicitable     = (*Toolset)(nil)
	_ tools.OAuthCapable   = (*Toolset)(nil)
	_ tools.ChangeNotifier = (*Toolset)(nil)
)

// NewToolsetCommand creates a new MCP toolset from a command.
//
// The optional policy lets callers tune restart/backoff behaviour. When
// the zero value is passed the supervisor uses its built-in defaults
// (RestartOnFailure, 5 attempts, 1s..32s backoff). Internal callbacks
// (OnDisconnect, OnRestart, Logger) are always set by the constructor
// and any values passed in for those fields are ignored.
func NewToolsetCommand(name, command string, args, env []string, cwd string, policy ...lifecycle.Policy) *Toolset {
	slog.Debug("Creating Stdio MCP toolset", "command", command, "args", args)

	desc := buildStdioDescription(command, args)
	ts := &Toolset{
		name:        name,
		mcpClient:   newStdioCmdClient(command, args, env, cwd),
		logID:       command,
		description: desc,
	}
	ts.supervisor = newSupervisor(ts, firstOrZero(policy))
	return ts
}

// NewRemoteToolset creates a new MCP toolset from a remote MCP Server.
//
// The optional policy lets callers tune restart/backoff behaviour;
// see NewToolsetCommand for the semantics.
func NewRemoteToolset(name, urlString, transport string, headers map[string]string, oauthConfig *latest.RemoteOAuthConfig, policy ...lifecycle.Policy) *Toolset {
	slog.Debug("Creating Remote MCP toolset", "url", urlString, "transport", transport, "headers", headers)

	desc := buildRemoteDescription(urlString, transport)
	ts := &Toolset{
		name:        name,
		mcpClient:   newRemoteClient(urlString, transport, headers, NewKeyringTokenStore(), oauthConfig),
		logID:       urlString,
		description: desc,
	}
	ts.supervisor = newSupervisor(ts, firstOrZero(policy))
	return ts
}

// firstOrZero returns the first element of s or the zero value of T if
// s is empty. Used to give variadic optional arguments a clean default.
func firstOrZero[T any](s []T) T {
	if len(s) > 0 {
		return s[0]
	}
	var zero T
	return zero
}

// newSupervisor constructs a Supervisor wired to the toolset's mcpClient
// using the provided policy as a base. Internal callbacks (OnDisconnect,
// OnRestart, Logger) are always overridden so the supervisor can
// invalidate caches and refresh tools/prompts on reconnect; any values
// passed in for those fields are ignored.
//
// When the policy is the zero value, the supervisor uses its built-in
// defaults that match the historical mcp.Toolset behaviour:
// RestartOnFailure, max 5 attempts, 1s..32s exponential backoff.
func newSupervisor(ts *Toolset, base lifecycle.Policy) *lifecycle.Supervisor {
	connector := &clientConnector{ts: ts}
	policy := base
	policy.Logger = slog.With("component", "mcp.supervisor", "server", ts.logID)
	policy.OnDisconnect = func(error) {
		ts.mu.Lock()
		ts.invalidateCache()
		ts.mu.Unlock()
	}
	policy.OnRestart = func() {
		// Refresh tool and prompt caches eagerly so subsequent
		// Tools()/ListPrompts() calls return the up-to-date data
		// from the new server. The new server may expose a
		// different set of tools/prompts and notifications won't
		// fire for tools that disappeared.
		ctx := context.Background()
		ts.refreshToolCache(ctx)
		ts.refreshPromptCache(ctx)
	}
	return lifecycle.New(ts.logID, connector, policy)
}

// errServerUnavailable is returned by the connector when the MCP server could
// not be reached but the error is non-fatal (e.g. EOF, binary not found).
// Start() propagates this so the toolset stays unstarted, and the agent
// runtime retries via ensureToolSetsAreStarted on the next conversation turn.
//
// It aliases lifecycle.ErrServerUnavailable so that supervisor code can use
// errors.Is(err, lifecycle.ErrServerUnavailable) without importing this
// package.
var errServerUnavailable = lifecycle.ErrServerUnavailable

// WorkingDir returns the working directory of the underlying stdio client,
// or an empty string if this toolset uses a remote transport.
// This is intended for testing only.
func (ts *Toolset) WorkingDir() string {
	if c, ok := ts.mcpClient.(*stdioMCPClient); ok {
		return c.cwd
	}
	return ""
}

// Describe returns a short, user-visible description of this toolset instance.
// It never includes secrets.
func (ts *Toolset) Describe() string {
	return ts.description
}

// Name returns the user-facing identifier for this MCP toolset.
//
// When the YAML provides a `name:` field it always wins (it's also the
// prefix applied to every tool exposed by the server, so a stable user
// choice). Otherwise we fall back to the description — "mcp(stdio
// cmd=docker)", "mcp(remote host=api.github.com)", "mcp(ref=duckduckgo)"
// — because that disambiguates between several unnamed MCP toolsets
// far better than the bare YAML type "mcp" the registry would otherwise
// fall back to.
func (ts *Toolset) Name() string {
	if ts.name != "" {
		return ts.name
	}
	return ts.description
}

// Kind returns a short, user-friendly classification of this toolset:
// "Remote MCP" for HTTP/SSE/streamable-HTTP transports and "MCP" for
// stdio-spawned servers. Used by status surfaces (e.g. the /tools
// dialog) to label the toolset without leaking Go type names.
func (ts *Toolset) Kind() string {
	if _, ok := ts.mcpClient.(*remoteMCPClient); ok {
		return "Remote MCP"
	}
	return "MCP"
}

// IsStarted reports whether the supervisor currently considers the toolset
// connected and serving requests. Used by tests and TUI status surfaces.
func (ts *Toolset) IsStarted() bool {
	if ts.supervisor == nil {
		return false
	}
	return ts.supervisor.IsReady()
}

// State returns a snapshot of the toolset's lifecycle state, suitable for
// status displays.
func (ts *Toolset) State() lifecycle.StateInfo {
	if ts.supervisor == nil {
		return lifecycle.StateInfo{State: lifecycle.StateStopped}
	}
	return ts.supervisor.State()
}

// Restart brings the toolset back up regardless of state. Failed or
// Stopped supervisors are recovered via Start (RestartAndWait would
// return immediately because `done` is closed). Otherwise the current
// session is dropped and we wait for the supervisor to reconnect, up to
// sessionMissingRetryTimeout.
func (ts *Toolset) Restart(ctx context.Context) error {
	if ts.supervisor == nil {
		return errors.New("toolset has no supervisor: must be created via NewToolsetCommand or NewRemoteToolset")
	}
	if ts.supervisor.State().State.IsTerminal() {
		return ts.supervisor.Start(ctx)
	}
	return ts.supervisor.RestartAndWait(ctx, sessionMissingRetryTimeout)
}

// buildStdioDescription produces a user-visible description for a stdio MCP toolset.
func buildStdioDescription(command string, args []string) string {
	if len(args) == 0 {
		return "mcp(stdio cmd=" + command + ")"
	}
	return fmt.Sprintf("mcp(stdio cmd=%s args_len=%d)", command, len(args))
}

// buildRemoteDescription produces a user-visible description for a remote MCP toolset,
// exposing only the host (and port when present) from the URL.
func buildRemoteDescription(rawURL, transport string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "mcp(remote transport=" + transport + ")"
	}
	return "mcp(remote host=" + u.Host + " transport=" + transport + ")"
}

// Start performs the initial connect via the supervisor. If the connect fails
// (e.g. the server binary is missing), Start returns the underlying error and
// the toolset stays in StateStopped; the caller is expected to retry.
func (ts *Toolset) Start(ctx context.Context) error {
	if ts.supervisor == nil {
		return errors.New("toolset has no supervisor: must be created via NewToolsetCommand or NewRemoteToolset")
	}
	return ts.supervisor.Start(ctx)
}

// Stop tears the supervisor down. Idempotent.
func (ts *Toolset) Stop(ctx context.Context) error {
	slog.DebugContext(ctx, "Stopping MCP toolset", "server", ts.logID)
	if ts.supervisor == nil {
		return nil
	}
	if err := ts.supervisor.Stop(ctx); err != nil && ctx.Err() == nil {
		slog.ErrorContext(ctx, "Failed to stop MCP toolset", "server", ts.logID, "error", err)
		return err
	}
	slog.DebugContext(ctx, "Stopped MCP toolset successfully", "server", ts.logID)
	return nil
}

// clientConnector adapts an mcpClient to the lifecycle.Connector interface.
// It owns the initialize handshake (including the upstream-bug retry for the
// "failed to send initialized notification" case) and exposes the shared
// mcpClient as a Session.
type clientConnector struct {
	ts *Toolset
}

func (c *clientConnector) Connect(ctx context.Context) (lifecycle.Session, error) {
	ts := c.ts

	// The MCP toolset connection needs to persist beyond the initial HTTP
	// request that triggered its creation. When OAuth succeeds, subsequent
	// agent requests should reuse the already-authenticated MCP connection.
	// But if the connection's underlying context is tied to the first HTTP
	// request, it gets cancelled when that request completes, killing the
	// connection even though OAuth succeeded.
	ctx = context.WithoutCancel(ctx)

	slog.DebugContext(ctx, "Starting MCP toolset", "server", ts.logID)

	// Register notification handlers: they invalidate caches and refresh
	// eagerly so subsequent Tools()/ListPrompts() calls see fresh data.
	// They are re-registered on every Connect so that a fresh client
	// session inherits them.
	ts.mcpClient.SetToolListChangedHandler(func() {
		ts.mu.Lock()
		ts.invalidateCache()
		ts.mu.Unlock()
		slog.DebugContext(ctx, "MCP server notified tool list changed, refreshing", "server", ts.logID)
		ts.refreshToolCache(ctx)
	})
	ts.mcpClient.SetPromptListChangedHandler(func() {
		ts.mu.Lock()
		ts.invalidateCache()
		ts.mu.Unlock()
		slog.DebugContext(ctx, "MCP server notified prompt list changed, refreshing", "server", ts.logID)
		ts.refreshPromptCache(ctx)
	})

	initRequest := &mcp.InitializeRequest{
		Params: &mcp.InitializeParams{
			ClientInfo: &mcp.Implementation{
				Name:    "docker agent",
				Version: "1.0.0",
			},
			Capabilities: &mcp.ClientCapabilities{
				Elicitation: &mcp.ElicitationCapabilities{
					Form: &mcp.FormElicitationCapabilities{},
					URL:  &mcp.URLElicitationCapabilities{},
				},
			},
		},
	}

	var result *mcp.InitializeResult
	const maxRetries = 3
	for attempt := 0; ; attempt++ {
		var err error
		result, err = ts.mcpClient.Initialize(ctx, initRequest)
		if err == nil {
			break
		}
		// TODO(krissetto): This is a temporary fix to handle the case where the remote server hasn't finished its async init
		// and we send the notifications/initialized message before the server is ready. Fix upstream in mcp-go if possible.
		//
		// Only retry when initialization fails due to sending the initialized notification.
		if !isInitNotificationSendError(err) {
			classified := lifecycle.Classify(err)
			if errors.Is(classified, lifecycle.ErrServerUnavailable) {
				slog.DebugContext(ctx, "MCP client unavailable, will retry on next conversation turn",
					"server", ts.logID,
					"error", err,
				)
				return nil, errServerUnavailable
			}
			slog.ErrorContext(ctx, "Failed to initialize MCP client", "error", err)
			return nil, fmt.Errorf("failed to initialize MCP client: %w", err)
		}
		if attempt >= maxRetries {
			slog.ErrorContext(ctx, "Failed to initialize MCP client after retries", "error", err)
			return nil, fmt.Errorf("failed to initialize MCP client after retries: %w", err)
		}
		backoff := time.Duration(200*(attempt+1)) * time.Millisecond
		slog.DebugContext(ctx, "MCP initialize failed to send initialized notification; retrying", "id", ts.logID, "attempt", attempt+1, "backoff_ms", backoff.Milliseconds())
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, fmt.Errorf("failed to initialize MCP client: %w", ctx.Err())
		}
	}

	slog.DebugContext(ctx, "Started MCP toolset successfully", "server", ts.logID)
	ts.mu.Lock()
	ts.instructions = result.Instructions
	ts.mu.Unlock()

	return &clientSession{client: ts.mcpClient}, nil
}

// clientSession adapts an mcpClient's Wait/Close to the lifecycle.Session
// interface. The underlying client is shared across reconnects: a fresh
// gomcp.ClientSession is created internally by the client each Initialize.
type clientSession struct {
	client mcpClient
}

func (s *clientSession) Wait() error                     { return s.client.Wait() }
func (s *clientSession) Close(ctx context.Context) error { return s.client.Close(ctx) }

func (ts *Toolset) Instructions() string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.instructions
}

func (ts *Toolset) Tools(ctx context.Context) ([]tools.Tool, error) {
	if !ts.IsStarted() {
		return nil, lifecycle.ErrNotStarted
	}
	ts.mu.Lock()
	if ts.cachedTools != nil {
		result := ts.cachedTools
		ts.mu.Unlock()
		return result, nil
	}
	// Snapshot the generation so we can detect invalidation after the unlock.
	gen := ts.cacheGen
	ts.mu.Unlock()

	slog.DebugContext(ctx, "Listing MCP tools (cache miss)", "server", ts.logID)

	resp := ts.mcpClient.ListTools(ctx, &mcp.ListToolsParams{})

	var toolsList []tools.Tool
	for t, err := range resp {
		if err != nil {
			return nil, err
		}

		name := t.Name
		if ts.name != "" {
			name = fmt.Sprintf("%s_%s", ts.name, name)
		}

		tool := tools.Tool{
			Name:         name,
			Category:     "mcp",
			Description:  t.Description,
			Parameters:   t.InputSchema,
			OutputSchema: t.OutputSchema,
			Handler:      ts.callTool,
		}
		if t.Annotations != nil {
			tool.Annotations = tools.ToolAnnotations(*t.Annotations)
		}
		toolsList = append(toolsList, tool)

		slog.DebugContext(ctx, "Added MCP tool", "tool", name)
	}

	slog.DebugContext(ctx, "Listed MCP tools", "count", len(toolsList), "server", ts.logID)

	ts.mu.Lock()
	// Only populate the cache if no invalidation happened while we were
	// fetching from the server. Otherwise drop the result so the next
	// caller re-fetches with the latest data.
	if ts.cacheGen == gen {
		ts.cachedTools = toolsList
	}
	ts.mu.Unlock()

	return toolsList, nil
}

// refreshToolCache fetches the tool list from the server and populates the
// cache. It is called by the ToolListChanged notification handler so that
// the cache is already warm by the time the runtime loop calls Tools().
func (ts *Toolset) refreshToolCache(ctx context.Context) {
	if _, err := ts.Tools(ctx); err != nil {
		slog.WarnContext(ctx, "Failed to refresh tools after notification", "server", ts.logID, "error", err)
		return
	}

	ts.mu.Lock()
	handler := ts.toolsChangedHandler
	ts.mu.Unlock()

	if handler != nil {
		handler()
	}
}

// refreshPromptCache fetches the prompt list from the server and populates
// the cache. It is called by the PromptListChanged notification handler.
func (ts *Toolset) refreshPromptCache(ctx context.Context) {
	if _, err := ts.ListPrompts(ctx); err != nil {
		slog.WarnContext(ctx, "Failed to refresh prompts after notification", "server", ts.logID, "error", err)
	}
}

func (ts *Toolset) callTool(ctx context.Context, toolCall tools.ToolCall) (*tools.ToolCallResult, error) {
	slog.DebugContext(ctx, "Calling MCP tool", "tool", toolCall.Function.Name, "arguments", toolCall.Function.Arguments)

	toolCall.Function.Arguments = cmp.Or(toolCall.Function.Arguments, "{}")
	var args map[string]any
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
		slog.ErrorContext(ctx, "Failed to parse tool arguments", "tool", toolCall.Function.Name, "error", err)
		return nil, fmt.Errorf("failed to parse tool arguments: %w", err)
	}

	// Strip null values from arguments. Some models (e.g. OpenAI) send explicit
	// null for optional parameters, but MCP servers may reject them because
	// null is not a valid value for the declared parameter type (e.g. string).
	// Omitting the key is semantically equivalent to null for optional params.
	for k, v := range args {
		if v == nil {
			delete(args, k)
		}
	}

	request := &mcp.CallToolParams{}
	request.Name = toolCall.Function.Name
	request.Arguments = args

	resp, err := ts.mcpClient.CallTool(ctx, request)

	// If the call failed with a connection or session error (e.g. the
	// server restarted), trigger or wait for a reconnection and retry
	// the call once.
	if err != nil && isConnectionError(err) && ctx.Err() == nil {
		slog.WarnContext(ctx, "MCP call failed, forcing reconnect and retrying", "tool", toolCall.Function.Name, "server", ts.logID, "error", err)
		if waitErr := ts.supervisor.RestartAndWait(ctx, sessionMissingRetryTimeout); waitErr != nil {
			return nil, fmt.Errorf("failed to reconnect after call failure: %w", waitErr)
		}
		resp, err = ts.mcpClient.CallTool(ctx, request)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			slog.DebugContext(ctx, "CallTool canceled by context", "tool", toolCall.Function.Name)
			return nil, err
		}
		slog.ErrorContext(ctx, "Failed to call MCP tool", "tool", toolCall.Function.Name, "error", err)
		return nil, fmt.Errorf("failed to call tool: %w", err)
	}

	result := processMCPContent(resp)
	slog.DebugContext(ctx, "MCP tool call completed", "tool", toolCall.Function.Name, "output_length", len(result.Output))
	slog.DebugContext(ctx, result.Output)
	return result, nil
}

// isInitNotificationSendError returns true if initialization failed while sending the
// notifications/initialized message to the server.
func isInitNotificationSendError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// mcp-go client returns this error
	if strings.Contains(msg, "failed to send initialized notification") {
		return true
	}
	return false
}

func processMCPContent(toolResult *mcp.CallToolResult) *tools.ToolCallResult {
	var text strings.Builder
	var images, audios []tools.MediaContent

	for _, c := range toolResult.Content {
		switch c := c.(type) {
		case *mcp.TextContent:
			text.WriteString(c.Text)
		case *mcp.ImageContent:
			images = append(images, encodeMedia(c.Data, c.MIMEType))
		case *mcp.AudioContent:
			audios = append(audios, encodeMedia(c.Data, c.MIMEType))
		case *mcp.ResourceLink:
			if c.Name != "" {
				// Escape ] in name and ) in URI to prevent broken markdown links.
				name := strings.ReplaceAll(c.Name, "]", "\\]")
				uri := strings.ReplaceAll(c.URI, ")", "%29")
				fmt.Fprintf(&text, "[%s](%s)", name, uri)
			} else {
				text.WriteString(c.URI)
			}
		case *mcp.EmbeddedResource:
			if c.Resource == nil {
				continue
			}
			if c.Resource.Text != "" {
				text.WriteString(c.Resource.Text)
				continue
			}
			if len(c.Resource.Blob) > 0 {
				// Binary blobs can't be inlined as text; surface a marker the model can reason about.
				fmt.Fprintf(&text, "[embedded resource %s (%s, %d bytes)]",
					c.Resource.URI, c.Resource.MIMEType, len(c.Resource.Blob))
			}
		}
	}

	return &tools.ToolCallResult{
		Output:            cmp.Or(text.String(), "no output"),
		IsError:           toolResult.IsError,
		Images:            images,
		Audios:            audios,
		StructuredContent: toolResult.StructuredContent,
	}
}

// encodeMedia re-encodes raw bytes (as decoded by the MCP SDK) back to base64
// for our internal MediaContent representation.
func encodeMedia(data []byte, mimeType string) tools.MediaContent {
	return tools.MediaContent{
		Data:     base64.StdEncoding.EncodeToString(data),
		MimeType: mimeType,
	}
}

func (ts *Toolset) SetElicitationHandler(handler tools.ElicitationHandler) {
	ts.mcpClient.SetElicitationHandler(handler)
}

func (ts *Toolset) SetOAuthSuccessHandler(handler func()) {
	ts.mcpClient.SetOAuthSuccessHandler(handler)
}

func (ts *Toolset) SetManagedOAuth(managed bool) {
	ts.mcpClient.SetManagedOAuth(managed)
}

func (ts *Toolset) SetToolsChangedHandler(handler func()) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.toolsChangedHandler = handler
}

// ListPrompts retrieves available prompts from the MCP server.
// Returns a slice of PromptInfo containing metadata about each available prompt
// including name, description, and argument specifications.
func (ts *Toolset) ListPrompts(ctx context.Context) ([]PromptInfo, error) {
	if !ts.IsStarted() {
		return nil, lifecycle.ErrNotStarted
	}
	ts.mu.Lock()
	if ts.cachedPrompts != nil {
		result := ts.cachedPrompts
		ts.mu.Unlock()
		return result, nil
	}
	gen := ts.cacheGen
	ts.mu.Unlock()

	slog.DebugContext(ctx, "Listing MCP prompts (cache miss)", "server", ts.logID)

	// Call the underlying MCP client to list prompts
	resp := ts.mcpClient.ListPrompts(ctx, &mcp.ListPromptsParams{})

	var promptsList []PromptInfo
	for prompt, err := range resp {
		if err != nil {
			slog.WarnContext(ctx, "Error listing MCP prompt", "error", err)
			return promptsList, err
		}

		// Convert MCP prompt to our internal PromptInfo format
		promptInfo := PromptInfo{
			Name:        prompt.Name,
			Description: prompt.Description,
			Arguments:   make([]PromptArgument, 0),
		}

		// Convert arguments if they exist
		if prompt.Arguments != nil {
			for _, arg := range prompt.Arguments {
				promptArg := PromptArgument{
					Name:        arg.Name,
					Description: arg.Description,
					Required:    arg.Required,
				}
				promptInfo.Arguments = append(promptInfo.Arguments, promptArg)
			}
		}

		promptsList = append(promptsList, promptInfo)
		slog.DebugContext(ctx, "Added MCP prompt", "prompt", prompt.Name, "args_count", len(promptInfo.Arguments))
	}

	slog.DebugContext(ctx, "Listed MCP prompts", "count", len(promptsList), "server", ts.logID)

	ts.mu.Lock()
	if ts.cacheGen == gen {
		ts.cachedPrompts = promptsList
	}
	ts.mu.Unlock()

	return promptsList, nil
}

// GetPrompt retrieves a specific prompt with provided arguments from the MCP server.
// This method executes the prompt and returns the result content.
func (ts *Toolset) GetPrompt(ctx context.Context, name string, arguments map[string]string) (*mcp.GetPromptResult, error) {
	if !ts.IsStarted() {
		return nil, lifecycle.ErrNotStarted
	}

	slog.DebugContext(ctx, "Getting MCP prompt", "prompt", name, "arguments", arguments)

	// Prepare the request parameters
	request := &mcp.GetPromptParams{
		Name:      name,
		Arguments: arguments,
	}

	// Call the underlying MCP client to get the prompt
	result, err := ts.mcpClient.GetPrompt(ctx, request)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to get MCP prompt", "prompt", name, "error", err)
		return nil, fmt.Errorf("failed to get prompt %s: %w", name, err)
	}

	slog.DebugContext(ctx, "Retrieved MCP prompt", "prompt", name, "messages_count", len(result.Messages))
	return result, nil
}

// isConnectionError reports whether err is a connection or session error
// that warrants a reconnect-and-retry (as opposed to an application-level
// error that would fail again even after reconnecting).
//
// It defers to lifecycle.Classify, which understands the same set of
// patterns the MCP SDK emits via ErrSessionMissing, EOF, net.Error, and
// substring-wrapped transport failures.
func isConnectionError(err error) bool {
	if errors.Is(err, mcp.ErrSessionMissing) {
		return true
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	classified := lifecycle.Classify(err)
	return errors.Is(classified, lifecycle.ErrTransport) ||
		errors.Is(classified, lifecycle.ErrSessionMissing) ||
		errors.Is(classified, lifecycle.ErrServerUnavailable)
}
