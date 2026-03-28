package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

type ResumeType string

// ElicitationResult represents the result of an elicitation request
type ElicitationResult struct {
	Action  tools.ElicitationAction
	Content map[string]any // The submitted form data (only present when action is "accept")
}

// ElicitationError represents an error from a declined/cancelled elicitation
type ElicitationError struct {
	Action  string
	Message string
}

func (e *ElicitationError) Error() string {
	return fmt.Sprintf("elicitation %s: %s", e.Action, e.Message)
}

const (
	ResumeTypeApprove        ResumeType = "approve"
	ResumeTypeApproveSession ResumeType = "approve-session"
	ResumeTypeApproveTool    ResumeType = "approve-tool"
	ResumeTypeReject         ResumeType = "reject"
)

// ResumeRequest carries the user's confirmation decision along with an optional
// reason (used when rejecting a tool call to help the model understand why).
type ResumeRequest struct {
	Type     ResumeType
	Reason   string // Optional; primarily used with ResumeTypeReject
	ToolName string // Optional; used with ResumeTypeApproveTool to specify which tool to always allow
}

// ResumeApprove creates a ResumeRequest to approve a single tool call.
func ResumeApprove() ResumeRequest {
	return ResumeRequest{Type: ResumeTypeApprove}
}

// ResumeApproveSession creates a ResumeRequest to approve all tool calls for the session.
func ResumeApproveSession() ResumeRequest {
	return ResumeRequest{Type: ResumeTypeApproveSession}
}

// ResumeApproveTool creates a ResumeRequest to always approve a specific tool for the session.
func ResumeApproveTool(toolName string) ResumeRequest {
	return ResumeRequest{Type: ResumeTypeApproveTool, ToolName: toolName}
}

// ResumeReject creates a ResumeRequest to reject a tool call with an optional reason.
func ResumeReject(reason string) ResumeRequest {
	return ResumeRequest{Type: ResumeTypeReject, Reason: reason}
}

// ToolHandlerFunc is a function type for handling tool calls
type ToolHandlerFunc func(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, events chan Event) (*tools.ToolCallResult, error)

// ElicitationRequestHandler is a function type for handling elicitation requests
type ElicitationRequestHandler func(ctx context.Context, message string, schema map[string]any) (map[string]any, error)

// Runtime defines the contract for runtime execution
type Runtime interface {
	// CurrentAgentInfo returns information about the currently active agent
	CurrentAgentInfo(ctx context.Context) CurrentAgentInfo
	// CurrentAgentName returns the name of the currently active agent
	CurrentAgentName() string
	// SetCurrentAgent sets the currently active agent for subsequent user messages
	SetCurrentAgent(agentName string) error
	// CurrentAgentTools returns the tools for the active agent
	CurrentAgentTools(ctx context.Context) ([]tools.Tool, error)
	// EmitStartupInfo emits initial agent, team, and toolset information for immediate display.
	// When sess is non-nil and contains token data, a TokenUsageEvent is also emitted
	// so the UI can display context usage percentage on session restore.
	EmitStartupInfo(ctx context.Context, sess *session.Session, events chan Event)
	// ResetStartupInfo resets the startup info emission flag, allowing re-emission
	ResetStartupInfo()
	// RunStream starts the agent's interaction loop and returns a channel of events
	RunStream(ctx context.Context, sess *session.Session) <-chan Event
	// Run starts the agent's interaction loop and returns the final messages
	Run(ctx context.Context, sess *session.Session) ([]session.Message, error)
	// Resume allows resuming execution after user confirmation.
	// The ResumeRequest carries the decision type and an optional reason (for rejections).
	Resume(ctx context.Context, req ResumeRequest)
	// ResumeElicitation sends an elicitation response back to a waiting elicitation request
	ResumeElicitation(_ context.Context, action tools.ElicitationAction, content map[string]any) error
	// SessionStore returns the session store for browsing/loading past sessions.
	// Returns nil if no persistent session store is configured.
	SessionStore() session.Store

	// Summarize generates a summary for the session
	Summarize(ctx context.Context, sess *session.Session, additionalPrompt string, events chan Event)

	// PermissionsInfo returns the team-level permission patterns (allow/ask/deny).
	// Returns nil if no permissions are configured.
	PermissionsInfo() *PermissionsInfo

	// CurrentAgentSkillsToolset returns the skills toolset for the current agent, or nil if skills are not enabled.
	CurrentAgentSkillsToolset() *builtin.SkillsToolset

	// CurrentMCPPrompts returns MCP prompts available from the current agent's toolsets.
	// Returns an empty map if no MCP prompts are available.
	CurrentMCPPrompts(ctx context.Context) map[string]mcptools.PromptInfo

	// ExecuteMCPPrompt executes a named MCP prompt with the given arguments.
	ExecuteMCPPrompt(ctx context.Context, promptName string, arguments map[string]string) (string, error)

	// UpdateSessionTitle persists a new title for the current session.
	UpdateSessionTitle(ctx context.Context, sess *session.Session, title string) error

	// TitleGenerator returns a generator for automatic session titles, or nil
	// if the runtime does not support local title generation (e.g. remote runtimes).
	TitleGenerator() *sessiontitle.Generator

	// Close releases resources held by the runtime (e.g., session store connections).
	Close() error
}

// PermissionsInfo contains the allow, ask, and deny patterns for tool permissions.
type PermissionsInfo struct {
	Allow []string
	Ask   []string
	Deny  []string
}

type CurrentAgentInfo struct {
	Name        string
	Description string
	Commands    types.Commands
}

type ModelStore interface {
	GetModel(ctx context.Context, modelID string) (*modelsdev.Model, error)
	GetDatabase(ctx context.Context) (*modelsdev.Database, error)
}

// ToolsChangeSubscriber is implemented by runtimes that can notify when
// toolsets report a change in their tool list (e.g. after an MCP
// ToolListChanged notification). The provided callback is invoked
// outside of any RunStream, so the UI can update the tool count
// immediately.
type ToolsChangeSubscriber interface {
	OnToolsChanged(handler func(Event))
}

// LocalRuntime manages the execution of agents
type LocalRuntime struct {
	toolMap                     map[string]ToolHandlerFunc
	team                        *team.Team
	currentAgent                string
	resumeChan                  chan ResumeRequest
	tracer                      trace.Tracer
	modelsStore                 ModelStore
	sessionCompaction           bool
	managedOAuth                bool
	startupInfoEmitted          bool                   // Track if startup info has been emitted to avoid unnecessary duplication
	elicitationRequestCh        chan ElicitationResult // Channel for receiving elicitation responses
	elicitationEventsChannel    chan Event             // Current events channel for sending elicitation requests
	elicitationEventsChannelMux sync.RWMutex           // Protects elicitationEventsChannel
	sessionStore                session.Store
	workingDir                  string   // Working directory for hooks execution
	env                         []string // Environment variables for hooks execution
	modelSwitcherCfg            *ModelSwitcherConfig

	// retryOnRateLimit enables retry-with-backoff for HTTP 429 (rate limit) errors
	// when no fallback models are configured. When false (default), 429 errors are
	// treated as non-retryable and immediately fail or skip to the next model.
	// Library consumers can enable this via WithRetryOnRateLimit().
	retryOnRateLimit bool

	// fallbackCooldowns tracks per-agent cooldown state for sticky fallback behavior
	fallbackCooldowns    map[string]*fallbackCooldownState
	fallbackCooldownsMux sync.RWMutex

	currentAgentMu sync.RWMutex

	// onToolsChanged is called when an MCP toolset reports a tool list change.
	onToolsChanged func(Event)

	bgAgents *agenttool.Handler
}

type Opt func(*LocalRuntime)

func WithCurrentAgent(agentName string) Opt {
	return func(r *LocalRuntime) {
		r.currentAgent = agentName
	}
}

func WithManagedOAuth(managed bool) Opt {
	return func(r *LocalRuntime) {
		r.managedOAuth = managed
	}
}

// WithTracer sets a custom OpenTelemetry tracer; if not provided, tracing is disabled (no-op).
func WithTracer(t trace.Tracer) Opt {
	return func(r *LocalRuntime) {
		r.tracer = t
	}
}

func WithSessionCompaction(sessionCompaction bool) Opt {
	return func(r *LocalRuntime) {
		r.sessionCompaction = sessionCompaction
	}
}

func WithModelStore(store ModelStore) Opt {
	return func(r *LocalRuntime) {
		r.modelsStore = store
	}
}

func WithSessionStore(store session.Store) Opt {
	return func(r *LocalRuntime) {
		r.sessionStore = store
	}
}

// WithWorkingDir sets the working directory for hooks execution
func WithWorkingDir(dir string) Opt {
	return func(r *LocalRuntime) {
		r.workingDir = dir
	}
}

// WithEnv sets the environment variables for hooks execution
func WithEnv(env []string) Opt {
	return func(r *LocalRuntime) {
		r.env = env
	}
}

// WithRetryOnRateLimit enables automatic retry with backoff for HTTP 429 (rate limit)
// errors when no fallback models are available. When enabled, the runtime will honor
// the Retry-After header from the provider's response to determine wait time before
// retrying, falling back to exponential backoff if the header is absent.
//
// This is off by default. It is intended for library consumers that run agents
// programmatically and prefer to wait for rate limits to clear rather than fail
// immediately.
//
// When fallback models are configured, 429 errors always skip to the next model
// regardless of this setting.
func WithRetryOnRateLimit() Opt {
	return func(r *LocalRuntime) {
		r.retryOnRateLimit = true
	}
}

// NewLocalRuntime creates a new LocalRuntime without the persistence wrapper.
// This is useful for testing or when persistence is handled externally.
func NewLocalRuntime(agents *team.Team, opts ...Opt) (*LocalRuntime, error) {
	defaultAgent, err := agents.DefaultAgent()
	if err != nil {
		return nil, err
	}

	r := &LocalRuntime{
		toolMap:              make(map[string]ToolHandlerFunc),
		team:                 agents,
		currentAgent:         defaultAgent.Name(),
		resumeChan:           make(chan ResumeRequest),
		elicitationRequestCh: make(chan ElicitationResult),
		sessionCompaction:    true,
		managedOAuth:         true,
		sessionStore:         session.NewInMemorySessionStore(),
		fallbackCooldowns:    make(map[string]*fallbackCooldownState),
	}
	r.bgAgents = agenttool.NewHandler(r)

	for _, opt := range opts {
		opt(r)
	}

	if r.modelsStore == nil {
		modelsStore, err := modelsdev.NewStore()
		if err != nil {
			return nil, err
		}
		r.modelsStore = modelsStore
	}

	// Validate that the current agent exists and has a model
	// (currentAgent might have been changed by options)
	defaultAgent, err = r.team.Agent(r.currentAgent)
	if err != nil {
		return nil, err
	}

	if defaultAgent.Model() == nil {
		return nil, fmt.Errorf("agent %s has no valid model", defaultAgent.Name())
	}

	// Register runtime-managed tool handlers once during construction.
	// This avoids concurrent map writes when multiple goroutines call
	// RunStream on the same runtime (e.g. background agent sessions).
	r.registerDefaultTools()

	slog.Debug("Creating new runtime", "agent", r.currentAgent, "available_agents", agents.Size())

	return r, nil
}

func (r *LocalRuntime) CurrentAgentName() string {
	r.currentAgentMu.RLock()
	defer r.currentAgentMu.RUnlock()
	return r.currentAgent
}

func (r *LocalRuntime) setCurrentAgent(name string) {
	r.currentAgentMu.Lock()
	defer r.currentAgentMu.Unlock()
	r.currentAgent = name
}

func (r *LocalRuntime) CurrentAgentInfo(context.Context) CurrentAgentInfo {
	currentAgent := r.CurrentAgent()

	return CurrentAgentInfo{
		Name:        currentAgent.Name(),
		Description: currentAgent.Description(),
		Commands:    currentAgent.Commands(),
	}
}

func (r *LocalRuntime) SetCurrentAgent(agentName string) error {
	// Validate that the agent exists in the team
	if _, err := r.team.Agent(agentName); err != nil {
		return err
	}
	r.setCurrentAgent(agentName)
	slog.Debug("Switched current agent", "agent", agentName)
	return nil
}

func (r *LocalRuntime) CurrentAgentCommands(context.Context) types.Commands {
	return r.CurrentAgent().Commands()
}

// CurrentAgentTools returns the tools available to the current agent.
// This starts the toolsets if needed and returns all available tools.
func (r *LocalRuntime) CurrentAgentTools(ctx context.Context) ([]tools.Tool, error) {
	a := r.CurrentAgent()
	return a.Tools(ctx)
}

// CurrentMCPPrompts returns the available MCP prompts from all active MCP toolsets
// for the current agent. It discovers prompts by calling ListPrompts on each MCP toolset
// and aggregates the results into a map keyed by prompt name.
func (r *LocalRuntime) CurrentMCPPrompts(ctx context.Context) map[string]mcptools.PromptInfo {
	prompts := make(map[string]mcptools.PromptInfo)

	// Get the current agent to access its toolsets
	currentAgent := r.CurrentAgent()
	if currentAgent == nil {
		slog.Warn("No current agent available for MCP prompt discovery")
		return prompts
	}

	// Iterate through all toolsets of the current agent
	for _, toolset := range currentAgent.ToolSets() {
		if mcpToolset, ok := tools.As[*mcptools.Toolset](toolset); ok {
			slog.Debug("Found MCP toolset", "toolset", mcpToolset)
			// Discover prompts from this MCP toolset
			mcpPrompts := r.discoverMCPPrompts(ctx, mcpToolset)

			// Merge prompts into the result map
			// If there are name conflicts, the later toolset's prompt will override
			maps.Copy(prompts, mcpPrompts)
		} else {
			slog.Debug("Toolset is not an MCP toolset", "type", fmt.Sprintf("%T", toolset))
		}
	}

	slog.Debug("Discovered MCP prompts", "agent", currentAgent.Name(), "prompt_count", len(prompts))
	return prompts
}

// discoverMCPPrompts queries an MCP toolset for available prompts and converts them
// to PromptInfo structures. This method handles the MCP protocol communication
// and gracefully handles any errors during prompt discovery.
func (r *LocalRuntime) discoverMCPPrompts(ctx context.Context, toolset *mcptools.Toolset) map[string]mcptools.PromptInfo {
	mcpPrompts, err := toolset.ListPrompts(ctx)
	if err != nil {
		slog.Warn("Failed to list MCP prompts from toolset", "error", err)
		return nil
	}

	prompts := make(map[string]mcptools.PromptInfo, len(mcpPrompts))
	for _, mcpPrompt := range mcpPrompts {
		promptInfo := mcptools.PromptInfo{
			Name:        mcpPrompt.Name,
			Description: mcpPrompt.Description,
			Arguments:   make([]mcptools.PromptArgument, 0, len(mcpPrompt.Arguments)),
		}

		for _, arg := range mcpPrompt.Arguments {
			promptInfo.Arguments = append(promptInfo.Arguments, mcptools.PromptArgument{
				Name:        arg.Name,
				Description: arg.Description,
				Required:    arg.Required,
			})
		}

		prompts[mcpPrompt.Name] = promptInfo
		slog.Debug("Discovered MCP prompt", "name", mcpPrompt.Name, "args_count", len(promptInfo.Arguments))
	}

	return prompts
}

// CurrentAgent returns the current agent
func (r *LocalRuntime) CurrentAgent() *agent.Agent {
	// We validated already that the agent exists
	current, _ := r.team.Agent(r.CurrentAgentName())
	return current
}

// resolveSessionAgent returns the agent for the given session. When the session
// is pinned to a specific agent (e.g. background agent tasks), it returns that
// agent directly instead of reading the shared currentAgent field, which may
// point to a different agent.
func (r *LocalRuntime) resolveSessionAgent(sess *session.Session) *agent.Agent {
	if sess.AgentName != "" {
		if a, err := r.team.Agent(sess.AgentName); err == nil {
			return a
		}
	}
	return r.CurrentAgent()
}

// CurrentAgentSkillsToolset returns the skills toolset for the current agent, or nil if not enabled.
func (r *LocalRuntime) CurrentAgentSkillsToolset() *builtin.SkillsToolset {
	a := r.CurrentAgent()
	if a == nil {
		return nil
	}
	for _, ts := range a.ToolSets() {
		if st, ok := tools.As[*builtin.SkillsToolset](ts); ok {
			return st
		}
	}
	return nil
}

// ExecuteMCPPrompt executes an MCP prompt with provided arguments and returns the content.
func (r *LocalRuntime) ExecuteMCPPrompt(ctx context.Context, promptName string, arguments map[string]string) (string, error) {
	currentAgent := r.CurrentAgent()
	if currentAgent == nil {
		return "", errors.New("no current agent available")
	}

	for _, toolset := range currentAgent.ToolSets() {
		mcpToolset, ok := tools.As[*mcptools.Toolset](toolset)
		if !ok {
			continue
		}

		result, err := mcpToolset.GetPrompt(ctx, promptName, arguments)
		if err != nil {
			// If error is "prompt not found", continue to next toolset
			if err.Error() == "prompt not found" {
				continue
			}
			return "", fmt.Errorf("error executing prompt '%s': %w", promptName, err)
		}

		// Convert the MCP result to a string format
		if len(result.Messages) == 0 {
			return "No content returned from MCP prompt", nil
		}

		var content strings.Builder
		for i, message := range result.Messages {
			if i > 0 {
				content.WriteString("\n\n")
			}
			if textContent, ok := message.Content.(*mcp.TextContent); ok {
				content.WriteString(textContent.Text)
			} else {
				fmt.Fprintf(&content, "[Non-text content: %T]", message.Content)
			}
		}
		return content.String(), nil
	}

	return "", fmt.Errorf("MCP prompt '%s' not found in any active toolset", promptName)
}

// TitleGenerator returns a title generator for automatic session title generation.
func (r *LocalRuntime) TitleGenerator() *sessiontitle.Generator {
	a := r.CurrentAgent()
	if a == nil {
		return nil
	}
	model := a.Model()
	if model == nil {
		return nil
	}
	return sessiontitle.New(model, a.FallbackModels()...)
}

// getHooksExecutor creates a hooks executor for the given agent
func (r *LocalRuntime) getHooksExecutor(a *agent.Agent) *hooks.Executor {
	hooksCfg := hooks.FromConfig(a.Hooks())
	if hooksCfg == nil || hooksCfg.IsEmpty() {
		return nil
	}
	return hooks.NewExecutor(hooksCfg, r.workingDir, r.env)
}

// executeSessionStartHooks executes session start hooks for the given agent.
// It logs the hook output as additional context and emits warnings for system messages.
func (r *LocalRuntime) executeSessionStartHooks(ctx context.Context, sess *session.Session, a *agent.Agent, events chan Event) {
	hooksExec := r.getHooksExecutor(a)
	if hooksExec == nil || !hooksExec.HasSessionStartHooks() {
		return
	}

	slog.Debug("Executing session start hooks", "agent", a.Name(), "session_id", sess.ID)
	input := &hooks.Input{
		SessionID: sess.ID,
		Cwd:       r.workingDir,
		Source:    "startup",
	}

	result, err := hooksExec.ExecuteSessionStart(ctx, input)
	if err != nil {
		slog.Warn("Session start hook execution failed", "agent", a.Name(), "error", err)
		return
	}

	if result.SystemMessage != "" {
		events <- Warning(result.SystemMessage, a.Name())
	}
	if result.AdditionalContext != "" {
		slog.Debug("Session start hook provided additional context", "context", result.AdditionalContext)
		sess.AddMessage(session.SystemMessage(result.AdditionalContext))
	}
}

// executeSessionEndHooks executes session end hooks for the given agent.
func (r *LocalRuntime) executeSessionEndHooks(ctx context.Context, sess *session.Session, a *agent.Agent) {
	hooksExec := r.getHooksExecutor(a)
	if hooksExec == nil || !hooksExec.HasSessionEndHooks() {
		return
	}

	slog.Debug("Executing session end hooks", "agent", a.Name(), "session_id", sess.ID)
	input := &hooks.Input{
		SessionID: sess.ID,
		Cwd:       r.workingDir,
		Reason:    "stream_ended",
	}

	_, err := hooksExec.ExecuteSessionEnd(ctx, input)
	if err != nil {
		slog.Error("Session end hook execution failed", "agent", a.Name(), "error", err)
	}
}

// executeStopHooks executes stop hooks when the model finishes responding.
// The stop hook receives the model's final response content.
func (r *LocalRuntime) executeStopHooks(ctx context.Context, sess *session.Session, a *agent.Agent, responseContent string, events chan Event) {
	hooksExec := r.getHooksExecutor(a)
	if hooksExec == nil || !hooksExec.HasStopHooks() {
		return
	}

	slog.Debug("Executing stop hooks", "agent", a.Name(), "session_id", sess.ID)
	input := &hooks.Input{
		SessionID:    sess.ID,
		Cwd:          r.workingDir,
		StopResponse: responseContent,
	}

	result, err := hooksExec.ExecuteStop(ctx, input)
	if err != nil {
		slog.Warn("Stop hook execution failed", "agent", a.Name(), "error", err)
		return
	}

	if result.SystemMessage != "" {
		events <- Warning(result.SystemMessage, a.Name())
	}
}

// executeNotificationHooks executes notification hooks when the agent emits a user-facing
// notification (e.g., errors or warnings). Hook output is logged but does not affect the
// notification itself. Individual hooks are subject to their configured timeout.
func (r *LocalRuntime) executeNotificationHooks(ctx context.Context, a *agent.Agent, sessionID, level, message string) {
	if a == nil {
		return
	}

	if level != "error" && level != "warning" {
		slog.Error("Invalid notification level", "level", level, "expected", "error|warning")
		return
	}

	hooksExec := r.getHooksExecutor(a)
	if hooksExec == nil || !hooksExec.HasNotificationHooks() {
		return
	}

	slog.Debug("Executing notification hooks", "level", level, "session_id", sessionID)
	input := &hooks.Input{
		SessionID:           sessionID,
		Cwd:                 r.workingDir,
		NotificationLevel:   level,
		NotificationMessage: message,
	}

	_, err := hooksExec.ExecuteNotification(ctx, input)
	if err != nil {
		slog.Warn("Notification hook execution failed", "error", err)
	}
}

// executeOnUserInputHooks executes on-user-input hooks for the current agent
func (r *LocalRuntime) executeOnUserInputHooks(ctx context.Context, sessionID, logContext string) {
	a, _ := r.team.Agent(r.CurrentAgentName())
	if a == nil {
		return
	}

	hooksExec := r.getHooksExecutor(a)
	if hooksExec == nil || !hooksExec.HasOnUserInputHooks() {
		return
	}

	slog.Debug("Executing on-user-input hooks", "context", logContext)
	input := &hooks.Input{
		SessionID: sessionID,
		Cwd:       r.workingDir,
	}

	result, err := hooksExec.ExecuteOnUserInput(ctx, input)
	if err != nil {
		slog.Warn("On-user-input hook execution failed", "error", err)
	} else {
		slog.Debug("On-user-input hooks executed successfully")
	}
	_ = result // Hook result not used
}

// getAgentModelID returns the model ID for an agent, or empty string if no model is set.
func getAgentModelID(a *agent.Agent) string {
	if model := a.Model(); model != nil {
		return model.ID()
	}
	return ""
}

// getEffectiveModelID returns the currently active model ID for an agent, accounting
// for any active fallback cooldown. During a cooldown period, this returns the fallback
// model ID instead of the configured primary model, so the UI reflects the actual model in use.
func (r *LocalRuntime) getEffectiveModelID(a *agent.Agent) string {
	cooldownState := r.getCooldownState(a.Name())
	if cooldownState != nil {
		fallbacks := a.FallbackModels()
		if cooldownState.fallbackIndex >= 0 && cooldownState.fallbackIndex < len(fallbacks) {
			return fallbacks[cooldownState.fallbackIndex].ID()
		}
	}
	return getAgentModelID(a)
}

// agentDetailsFromTeam converts team agent info to AgentDetails for events.
// It accounts for active fallback cooldowns, returning the effective model
// instead of the configured model when a fallback is in effect.
func (r *LocalRuntime) agentDetailsFromTeam() []AgentDetails {
	agentsInfo := r.team.AgentsInfo()
	details := make([]AgentDetails, len(agentsInfo))
	for i, info := range agentsInfo {
		providerName := info.Provider
		modelName := info.Model

		// Check if this agent has an active fallback cooldown
		cooldownState := r.getCooldownState(info.Name)
		if cooldownState != nil {
			// Get the agent to access fallback models
			if a, err := r.team.Agent(info.Name); err == nil && a != nil {
				fallbacks := a.FallbackModels()
				if cooldownState.fallbackIndex >= 0 && cooldownState.fallbackIndex < len(fallbacks) {
					fb := fallbacks[cooldownState.fallbackIndex]
					// Parse provider/model from the fallback model ID
					modelID := fb.ID()
					if p, m, found := strings.Cut(modelID, "/"); found {
						providerName = p
						modelName = m
					} else {
						modelName = modelID
					}
				}
			}
		}

		details[i] = AgentDetails{
			Name:        info.Name,
			Description: info.Description,
			Provider:    providerName,
			Model:       modelName,
			Commands:    info.Commands,
		}
	}
	return details
}

// SessionStore returns the session store for browsing/loading past sessions.
func (r *LocalRuntime) SessionStore() session.Store {
	return r.sessionStore
}

// Close releases resources held by the runtime, including the session store.
func (r *LocalRuntime) Close() error {
	r.bgAgents.StopAll()
	if r.sessionStore != nil {
		return r.sessionStore.Close()
	}
	return nil
}

// UpdateSessionTitle persists the session title via the session store.
func (r *LocalRuntime) UpdateSessionTitle(ctx context.Context, sess *session.Session, title string) error {
	sess.Title = title
	if r.sessionStore != nil {
		return r.sessionStore.UpdateSession(ctx, sess)
	}
	return nil
}

// PermissionsInfo returns the team-level permission patterns.
// Returns nil if no permissions are configured.
func (r *LocalRuntime) PermissionsInfo() *PermissionsInfo {
	permChecker := r.team.Permissions()
	if permChecker == nil || permChecker.IsEmpty() {
		return nil
	}
	return &PermissionsInfo{
		Allow: permChecker.AllowPatterns(),
		Ask:   permChecker.AskPatterns(),
		Deny:  permChecker.DenyPatterns(),
	}
}

// ResetStartupInfo resets the startup info emission flag.
// This should be called when replacing a session to allow re-emission of
// agent, team, and toolset info to the UI.
func (r *LocalRuntime) ResetStartupInfo() {
	r.startupInfoEmitted = false
}

// OnToolsChanged registers a handler that is called when an MCP toolset
// reports a tool list change outside of a RunStream. This allows the UI
// to update the tool count immediately.
func (r *LocalRuntime) OnToolsChanged(handler func(Event)) {
	r.onToolsChanged = handler

	for _, name := range r.team.AgentNames() {
		a, err := r.team.Agent(name)
		if err != nil {
			continue
		}
		for _, ts := range a.ToolSets() {
			if n, ok := tools.As[tools.ChangeNotifier](ts); ok {
				n.SetToolsChangedHandler(r.emitToolsChanged)
			}
		}
	}
}

// emitToolsChanged is the callback registered on MCP toolsets. It re-reads
// the current agent's full tool list and pushes a ToolsetInfo event.
func (r *LocalRuntime) emitToolsChanged() {
	if r.onToolsChanged == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	agentTools, err := r.CurrentAgentTools(ctx)
	if err != nil {
		return
	}
	r.onToolsChanged(ToolsetInfo(len(agentTools), false, r.CurrentAgentName()))
}

// EmitStartupInfo emits initial agent, team, and toolset information for immediate sidebar display.
// When sess is non-nil and contains token data, a TokenUsageEvent is also emitted so that the
// sidebar can display context usage percentage on session restore.
func (r *LocalRuntime) EmitStartupInfo(ctx context.Context, sess *session.Session, events chan Event) {
	// Prevent duplicate emissions
	if r.startupInfoEmitted {
		return
	}
	r.startupInfoEmitted = true

	a := r.CurrentAgent()

	// Helper to send events with context check
	send := func(event Event) bool {
		select {
		case events <- event:
			return true
		case <-ctx.Done():
			return false
		}
	}

	// Emit agent and team information immediately for fast sidebar display
	// Use getEffectiveModelID to account for active fallback cooldowns
	modelID := r.getEffectiveModelID(a)
	if !send(AgentInfo(a.Name(), modelID, a.Description(), a.WelcomeMessage())) {
		return
	}
	if !send(TeamInfo(r.agentDetailsFromTeam(), r.CurrentAgentName())) {
		return
	}

	// When restoring a session that already has token data, emit a
	// TokenUsageEvent so the sidebar can show the context usage percentage.
	// The context limit comes from the model definition (models.dev), which
	// is a model property — not persisted in the session.
	//
	// Use TotalCost (not OwnCost) because this is a restore/branch context:
	// sub-sessions won't emit their own events, so the parent must include
	// their costs.
	if sess != nil && (sess.InputTokens > 0 || sess.OutputTokens > 0) {
		var contextLimit int64
		if m, err := r.modelsStore.GetModel(ctx, modelID); err == nil && m != nil {
			contextLimit = int64(m.Limit.Context)
		}
		usage := SessionUsage(sess, contextLimit)
		usage.Cost = sess.TotalCost()
		send(NewTokenUsageEvent(sess.ID, r.CurrentAgentName(), usage))
	}

	// Emit agent warnings (if any) - these are quick
	r.emitAgentWarnings(a, func(e Event) { send(e) })

	// Tool loading can be slow (MCP servers need to start)
	// Emit progressive updates as each toolset loads
	r.emitToolsProgressively(ctx, a, send)
}

// emitToolsProgressively loads tools from each toolset and emits progress updates.
// This allows the UI to show the tool count incrementally as each toolset loads,
// with a spinner indicating that more tools may be coming.
func (r *LocalRuntime) emitToolsProgressively(ctx context.Context, a *agent.Agent, send func(Event) bool) {
	toolsets := a.ToolSets()
	totalToolsets := len(toolsets)

	// If no toolsets, emit final state immediately
	if totalToolsets == 0 {
		send(ToolsetInfo(0, false, r.CurrentAgentName()))
		return
	}

	// Emit initial loading state
	if !send(ToolsetInfo(0, true, r.CurrentAgentName())) {
		return
	}

	// Load tools from each toolset and emit progress
	var totalTools int
	for i, toolset := range toolsets {
		// Check context before potentially slow operations
		if ctx.Err() != nil {
			return
		}

		isLast := i == totalToolsets-1

		// Start the toolset if needed.
		// OAuth-protected toolsets are deferred — they require a live elicitation
		// handler and events channel, which are only available once RunStream is
		// active. They will start lazily on the first tool call.
		if startable, ok := toolset.(*tools.StartableToolSet); ok {
			if mcpTS, isMCP := tools.As[*mcptools.Toolset](startable); isMCP && mcpTS.RequiresOAuth() {
				slog.Debug("Deferring OAuth toolset start until first use", "toolset", tools.DescribeToolSet(startable))
				continue
			}

			if !startable.IsStarted() {
				if err := startable.Start(ctx); err != nil {
					slog.Warn("Toolset start failed; skipping", "agent", a.Name(), "toolset", fmt.Sprintf("%T", startable.ToolSet), "error", err)
					continue
				}
			}
		}

		// Get tools from this toolset
		ts, err := toolset.Tools(ctx)
		if err != nil {
			slog.Warn("Failed to get tools from toolset", "agent", a.Name(), "error", err)
			continue
		}

		totalTools += len(ts)

		// Emit progress update - still loading unless this is the last toolset
		if !send(ToolsetInfo(totalTools, !isLast, r.CurrentAgentName())) {
			return
		}
	}

	// Emit final state (not loading)
	send(ToolsetInfo(totalTools, false, r.CurrentAgentName()))
}

func (r *LocalRuntime) Resume(_ context.Context, req ResumeRequest) {
	slog.Debug("Resuming runtime", "agent", r.CurrentAgentName(), "type", req.Type, "reason", req.Reason)

	// Defensive validation:
	//
	// The runtime may be resumed by multiple entry points (API, CLI, TUI, tests).
	// Even if upstream layers perform validation, the runtime must never assume
	// the ResumeType is valid. Accepting invalid values here leads to confusing
	// downstream behavior where tool execution fails without a clear cause.
	if !IsValidResumeType(req.Type) {
		slog.Warn(
			"Invalid resume type received; ignoring resume request",
			"agent", r.CurrentAgentName(),
			"confirmation_type", req.Type,
			"valid_types", ValidResumeTypes(),
		)
		return
	}

	// Attempt to deliver the resume signal to the execution loop.
	//
	// The channel is non-blocking by design to avoid deadlocks if the runtime
	// is not currently waiting for a confirmation (e.g. already resumed,
	// canceled, or shutting down).
	select {
	case r.resumeChan <- req:
		slog.Debug("Resume signal sent", "agent", r.CurrentAgentName())
	default:
		slog.Debug(
			"Resume channel not ready; resume signal dropped",
			"agent", r.CurrentAgentName(),
			"confirmation_type", req.Type,
		)
	}
}

// ResumeElicitation sends an elicitation response back to a waiting elicitation request
func (r *LocalRuntime) ResumeElicitation(ctx context.Context, action tools.ElicitationAction, content map[string]any) error {
	slog.Debug("Resuming runtime with elicitation response", "agent", r.CurrentAgentName(), "action", action)

	result := ElicitationResult{
		Action:  action,
		Content: content,
	}

	select {
	case <-ctx.Done():
		slog.Debug("Context cancelled while sending elicitation response")
		return ctx.Err()
	case r.elicitationRequestCh <- result:
		slog.Debug("Elicitation response sent successfully", "action", action)
		return nil
	default:
		slog.Debug("Elicitation channel not ready")
		return errors.New("no elicitation request in progress")
	}
}

// Run starts the agent's interaction loop

func (r *LocalRuntime) startSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if r.tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	return r.tracer.Start(ctx, name, opts...)
}

// Summarize generates a summary for the session based on the conversation history.
// The additionalPrompt parameter allows users to provide additional instructions
// for the summarization (e.g., "focus on code changes" or "include action items").
func (r *LocalRuntime) Summarize(ctx context.Context, sess *session.Session, additionalPrompt string, events chan Event) {
	a := r.resolveSessionAgent(sess)
	r.doCompact(ctx, sess, a, additionalPrompt, events)

	// Emit a TokenUsageEvent so the sidebar immediately reflects the
	// compaction: tokens drop to the summary size, context % drops, and
	// cost increases by the summary generation cost.
	modelID := r.getEffectiveModelID(a)
	var contextLimit int64
	if m, err := r.modelsStore.GetModel(ctx, modelID); err == nil && m != nil {
		contextLimit = int64(m.Limit.Context)
	}
	events <- NewTokenUsageEvent(sess.ID, a.Name(), SessionUsage(sess, contextLimit))
}

// swapElicitationEventsChannel atomically replaces the current elicitation
// events channel and returns the previous one. Each RunStream call swaps in
// its own channel on entry and swaps the previous one back on exit, so nested
// streams (sub-sessions, background agents) don't lose the parent's channel.
func (r *LocalRuntime) swapElicitationEventsChannel(ch chan Event) chan Event {
	r.elicitationEventsChannelMux.Lock()
	defer r.elicitationEventsChannelMux.Unlock()
	prev := r.elicitationEventsChannel
	r.elicitationEventsChannel = ch
	return prev
}

// elicitationHandler creates an elicitation handler that can be used by MCP clients
// This handler propagates elicitation requests to the runtime's client via events
func (r *LocalRuntime) elicitationHandler(ctx context.Context, req *mcp.ElicitParams) (tools.ElicitationResult, error) {
	slog.Debug("Elicitation request received from MCP server", "message", req.Message)

	// Hold the read lock while sending to the channel to prevent a race
	// with swapElicitationEventsChannel / close(events).
	r.elicitationEventsChannelMux.RLock()
	eventsChannel := r.elicitationEventsChannel
	if eventsChannel == nil {
		r.elicitationEventsChannelMux.RUnlock()
		return tools.ElicitationResult{}, errors.New("no events channel available for elicitation")
	}

	r.executeOnUserInputHooks(ctx, "", "elicitation")

	slog.Debug("Sending elicitation request event to client", "message", req.Message, "mode", req.Mode, "requested_schema", req.RequestedSchema, "url", req.URL)
	slog.Debug("Elicitation request meta", "meta", req.Meta)

	// Send elicitation request event to the runtime's client
	eventsChannel <- ElicitationRequest(req.Message, req.Mode, req.RequestedSchema, req.URL, req.ElicitationID, req.Meta, r.CurrentAgentName())
	r.elicitationEventsChannelMux.RUnlock()

	// Wait for response from the client
	select {
	case result := <-r.elicitationRequestCh:
		return tools.ElicitationResult{
			Action:  result.Action,
			Content: result.Content,
		}, nil
	case <-ctx.Done():
		slog.Debug("Context cancelled while waiting for elicitation response")
		return tools.ElicitationResult{}, ctx.Err()
	}
}
