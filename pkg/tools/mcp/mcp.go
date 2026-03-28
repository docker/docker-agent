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

	"github.com/docker/docker-agent/pkg/tools"
)

// RemoteOAuthConfig represents explicit OAuth configuration for remote MCP servers.
// This allows using pre-registered OAuth clients with servers that do not support
// Dynamic Client Registration (RFC 7591), such as the Slack MCP server.
type RemoteOAuthConfig struct {
	// ClientID is the OAuth client ID (required for explicit OAuth)
	ClientID string
	// ClientSecret is the OAuth client secret (optional, for confidential clients)
	ClientSecret string
	// CallbackPort is the fixed port for the OAuth callback server (optional)
	// When not specified, a random available port is used
	CallbackPort int
	// Scopes is the list of OAuth scopes to request (optional)
	// When not specified, default scopes from the server are used
	Scopes []string
}

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

// Toolset represents a set of MCP tools
type Toolset struct {
	name         string
	mcpClient    mcpClient
	logID        string
	description  string // user-visible description, set by constructors
	instructions string
	mu           sync.Mutex
	started      bool
	stopping     bool // true when Stop() has been called

	// Cached tools and prompts, invalidated via MCP notifications.
	// cacheGen is bumped on each invalidation so that a concurrent
	// Tools()/ListPrompts() call can detect that its result is stale.
	cachedTools   []tools.Tool
	cachedPrompts []PromptInfo
	cacheGen      uint64

	// toolsChangedHandler is called after the tool cache is refreshed
	// following a ToolListChanged notification from the server.
	toolsChangedHandler func()
}

// invalidateCache clears the cached tools and prompts and bumps the
// generation counter. The caller must hold ts.mu.
func (ts *Toolset) invalidateCache() {
	ts.cachedTools = nil
	ts.cachedPrompts = nil
	ts.cacheGen++
}

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
func NewToolsetCommand(name, command string, args, env []string, cwd string) *Toolset {
	slog.Debug("Creating Stdio MCP toolset", "command", command, "args", args)

	desc := buildStdioDescription(command, args)
	return &Toolset{
		name:        name,
		mcpClient:   newStdioCmdClient(command, args, env, cwd),
		logID:       command,
		description: desc,
	}
}

// NewRemoteToolset creates a new MCP toolset from a remote MCP Server.
func NewRemoteToolset(name, urlString, transport string, headers map[string]string) *Toolset {
	slog.Debug("Creating Remote MCP toolset", "url", urlString, "transport", transport, "headers", headers)

	desc := buildRemoteDescription(urlString, transport)
	return &Toolset{
		name:        name,
		mcpClient:   newRemoteClient(urlString, transport, headers, NewInMemoryTokenStore()),
		logID:       urlString,
		description: desc,
	}
}

// NewRemoteToolsetWithOAuth creates a new MCP toolset from a remote MCP Server with explicit OAuth configuration.
func NewRemoteToolsetWithOAuth(name, urlString, transport string, headers map[string]string, oauthConfig *RemoteOAuthConfig) *Toolset {
	slog.Debug("Creating Remote MCP toolset with OAuth", "url", urlString, "transport", transport, "headers", headers)

	desc := buildRemoteDescription(urlString, transport)
	client := newRemoteClient(urlString, transport, headers, NewInMemoryTokenStore()).WithOAuthConfig(oauthConfig)
	return &Toolset{
		name:        name,
		mcpClient:   client,
		logID:       urlString,
		description: desc,
	}
}

// errServerUnavailable is returned by doStart when the MCP server could not be
// reached but the error is non-fatal (e.g. EOF). The toolset is considered
// "started" so the agent can proceed, but watchConnection must not be spawned
// because there is no live connection to monitor.
var errServerUnavailable = errors.New("MCP server unavailable")

// Describe returns a short, user-visible description of this toolset instance.
// It never includes secrets.
func (ts *Toolset) Describe() string {
	return ts.description
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

func (ts *Toolset) Start(ctx context.Context) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.started {
		return nil
	}

	if err := ts.doStart(ctx); err != nil {
		if errors.Is(err, errServerUnavailable) {
			// The server is unreachable but the error is non-fatal.
			// Mark as started so the agent can proceed; tools will simply
			// be empty. Don't spawn a watcher — there's nothing to watch.
			ts.started = true
			return nil
		}
		return err
	}

	ts.started = true

	// Spawn the connection watcher only on the initial Start.
	// Restarts from within watchConnection call doStart directly
	// and must NOT spawn an additional watcher goroutine.
	// Use WithoutCancel so the watcher outlives the caller's context;
	// the only way to stop it is via Stop() setting ts.stopping.
	go ts.watchConnection(context.WithoutCancel(ctx))

	return nil
}

func (ts *Toolset) doStart(ctx context.Context) error {
	// The MCP toolset connection needs to persist beyond the initial HTTP request that triggered its creation.
	// When OAuth succeeds, subsequent agent requests should reuse the already-authenticated MCP connection.
	// But if the connection's underlying context is tied to the first HTTP request, it gets cancelled when that request
	// completes, killing the connection even though OAuth succeeded.
	// This is critical for OAuth flows where the toolset connection needs to remain alive after the initial HTTP request completes.
	ctx = context.WithoutCancel(ctx)

	slog.Debug("Starting MCP toolset", "server", ts.logID)

	// Register notification handlers to invalidate caches when the server
	// notifies us that its tools or prompts have changed.
	// We invalidate the cache and then eagerly re-fetch the list so that
	// subsequent Tools()/ListPrompts() calls return the up-to-date data
	// without racing with the server.
	ts.mcpClient.SetToolListChangedHandler(func() {
		ts.mu.Lock()
		ts.invalidateCache()
		ts.mu.Unlock()

		slog.Debug("MCP server notified tool list changed, refreshing", "server", ts.logID)
		ts.refreshToolCache(ctx)
	})
	ts.mcpClient.SetPromptListChangedHandler(func() {
		ts.mu.Lock()
		ts.invalidateCache()
		ts.mu.Unlock()

		slog.Debug("MCP server notified prompt list changed, refreshing", "server", ts.logID)
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
			if errors.Is(err, io.EOF) {
				slog.Debug(
					"MCP client unavailable (EOF), skipping MCP toolset",
					"server", ts.logID,
				)
				return errServerUnavailable
			}

			slog.Error("Failed to initialize MCP client", "error", err)
			return fmt.Errorf("failed to initialize MCP client: %w", err)
		}
		if attempt >= maxRetries {
			slog.Error("Failed to initialize MCP client after retries", "error", err)
			return fmt.Errorf("failed to initialize MCP client after retries: %w", err)
		}
		backoff := time.Duration(200*(attempt+1)) * time.Millisecond
		slog.Debug("MCP initialize failed to send initialized notification; retrying", "id", ts.logID, "attempt", attempt+1, "backoff_ms", backoff.Milliseconds())
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return fmt.Errorf("failed to initialize MCP client: %w", ctx.Err())
		}
	}

	slog.Debug("Started MCP toolset successfully", "server", ts.logID)
	ts.instructions = result.Instructions

	return nil
}

// watchConnection monitors the MCP server connection and auto-restarts it
// if the server dies unexpectedly (i.e. we didn't call Stop()).
// Only one watchConnection goroutine exists per Toolset; it is spawned by
// Start() and loops across restarts without spawning additional goroutines.
func (ts *Toolset) watchConnection(ctx context.Context) {
	for {
		err := ts.mcpClient.Wait()

		ts.mu.Lock()
		if ts.stopping {
			ts.mu.Unlock()
			return
		}
		ts.started = false
		ts.invalidateCache()
		ts.mu.Unlock()

		slog.Warn("MCP server connection lost, attempting restart", "server", ts.logID, "error", err)

		if !ts.tryRestart(ctx) {
			return
		}
	}
}

// tryRestart attempts to restart the MCP server with exponential backoff.
// Returns true if the server was restarted, false if all attempts failed or
// Stop() was called.
func (ts *Toolset) tryRestart(ctx context.Context) bool {
	const maxAttempts = 5

	for attempt := range maxAttempts {
		backoff := time.Duration(1<<uint(attempt)) * time.Second
		slog.Debug("Restarting MCP server", "server", ts.logID, "attempt", attempt+1, "backoff", backoff)
		time.Sleep(backoff)

		ts.mu.Lock()
		if ts.stopping {
			ts.mu.Unlock()
			return false
		}

		if err := ts.doStart(ctx); err != nil {
			ts.mu.Unlock()
			slog.Warn("MCP server restart failed", "server", ts.logID, "attempt", attempt+1, "error", err)
			continue
		}

		ts.started = true
		ts.mu.Unlock()

		slog.Info("MCP server restarted successfully", "server", ts.logID)
		return true
	}

	slog.Error("MCP server restart failed after all attempts", "server", ts.logID)
	return false
}

func (ts *Toolset) Instructions() string {
	ts.mu.Lock()
	started := ts.started
	ts.mu.Unlock()
	if !started {
		// TODO: this should never happen...
		return ""
	}
	return ts.instructions
}

func (ts *Toolset) Tools(ctx context.Context) ([]tools.Tool, error) {
	ts.mu.Lock()
	if !ts.started {
		ts.mu.Unlock()
		return nil, errors.New("toolset not started")
	}
	if ts.cachedTools != nil {
		result := ts.cachedTools
		ts.mu.Unlock()
		return result, nil
	}
	// Snapshot the generation so we can detect invalidation after the unlock.
	gen := ts.cacheGen
	ts.mu.Unlock()

	slog.Debug("Listing MCP tools (cache miss)", "server", ts.logID)

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
			Description:  t.Description,
			Parameters:   t.InputSchema,
			OutputSchema: t.OutputSchema,
			Handler:      ts.callTool,
		}
		if t.Annotations != nil {
			tool.Annotations = tools.ToolAnnotations(*t.Annotations)
		}
		toolsList = append(toolsList, tool)

		slog.Debug("Added MCP tool", "tool", name)
	}

	slog.Debug("Listed MCP tools", "count", len(toolsList), "server", ts.logID)

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
		slog.Warn("Failed to refresh tools after notification", "server", ts.logID, "error", err)
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
		slog.Warn("Failed to refresh prompts after notification", "server", ts.logID, "error", err)
	}
}

func (ts *Toolset) callTool(ctx context.Context, toolCall tools.ToolCall) (*tools.ToolCallResult, error) {
	slog.Debug("Calling MCP tool", "tool", toolCall.Function.Name, "arguments", toolCall.Function.Arguments)

	toolCall.Function.Arguments = cmp.Or(toolCall.Function.Arguments, "{}")
	var args map[string]any
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
		slog.Error("Failed to parse tool arguments", "tool", toolCall.Function.Name, "error", err)
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
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			slog.Debug("CallTool canceled by context", "tool", toolCall.Function.Name)
			return nil, err
		}
		slog.Error("Failed to call MCP tool", "tool", toolCall.Function.Name, "error", err)
		return nil, fmt.Errorf("failed to call tool: %w", err)
	}

	result := processMCPContent(resp)
	slog.Debug("MCP tool call completed", "tool", toolCall.Function.Name, "output_length", len(result.Output))
	slog.Debug(result.Output)
	return result, nil
}

func (ts *Toolset) Stop(ctx context.Context) error {
	slog.Debug("Stopping MCP toolset", "server", ts.logID)

	ts.mu.Lock()
	ts.stopping = true
	ts.started = false
	ts.mu.Unlock()

	if err := ts.mcpClient.Close(context.WithoutCancel(ctx)); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		slog.Error("Failed to stop MCP toolset", "server", ts.logID, "error", err)
		return err
	}

	slog.Debug("Stopped MCP toolset successfully", "server", ts.logID)
	return nil
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
	ts.mu.Lock()
	if !ts.started {
		ts.mu.Unlock()
		return nil, errors.New("toolset not started")
	}
	if ts.cachedPrompts != nil {
		result := ts.cachedPrompts
		ts.mu.Unlock()
		return result, nil
	}
	gen := ts.cacheGen
	ts.mu.Unlock()

	slog.Debug("Listing MCP prompts (cache miss)", "server", ts.logID)

	// Call the underlying MCP client to list prompts
	resp := ts.mcpClient.ListPrompts(ctx, &mcp.ListPromptsParams{})

	var promptsList []PromptInfo
	for prompt, err := range resp {
		if err != nil {
			slog.Warn("Error listing MCP prompt", "error", err)
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
		slog.Debug("Added MCP prompt", "prompt", prompt.Name, "args_count", len(promptInfo.Arguments))
	}

	slog.Debug("Listed MCP prompts", "count", len(promptsList), "server", ts.logID)

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
	ts.mu.Lock()
	started := ts.started
	ts.mu.Unlock()
	if !started {
		return nil, errors.New("toolset not started")
	}

	slog.Debug("Getting MCP prompt", "prompt", name, "arguments", arguments)

	// Prepare the request parameters
	request := &mcp.GetPromptParams{
		Name:      name,
		Arguments: arguments,
	}

	// Call the underlying MCP client to get the prompt
	result, err := ts.mcpClient.GetPrompt(ctx, request)
	if err != nil {
		slog.Error("Failed to get MCP prompt", "prompt", name, "error", err)
		return nil, fmt.Errorf("failed to get prompt %s: %w", name, err)
	}

	slog.Debug("Retrieved MCP prompt", "prompt", name, "messages_count", len(result.Messages))
	return result, nil
}
