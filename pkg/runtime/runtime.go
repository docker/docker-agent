package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/cagent/pkg/agent"
	"github.com/docker/cagent/pkg/chat"
	"github.com/docker/cagent/pkg/config/latest"
	"github.com/docker/cagent/pkg/config/types"
	"github.com/docker/cagent/pkg/hooks"
	"github.com/docker/cagent/pkg/model/provider"
	"github.com/docker/cagent/pkg/model/provider/options"
	"github.com/docker/cagent/pkg/modelsdev"
	"github.com/docker/cagent/pkg/permissions"
	"github.com/docker/cagent/pkg/rag"
	ragtypes "github.com/docker/cagent/pkg/rag/types"
	"github.com/docker/cagent/pkg/session"
	"github.com/docker/cagent/pkg/sessiontitle"
	"github.com/docker/cagent/pkg/team"
	"github.com/docker/cagent/pkg/telemetry"
	"github.com/docker/cagent/pkg/tools"
	"github.com/docker/cagent/pkg/tools/builtin"
	mcptools "github.com/docker/cagent/pkg/tools/mcp"
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

type ToolHandler struct {
	handler ToolHandlerFunc
	tool    tools.Tool
}

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
	// EmitStartupInfo emits initial agent, team, and toolset information for immediate display
	EmitStartupInfo(ctx context.Context, events chan Event)
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

	// PermissionsInfo returns the team-level permission patterns (allow/deny).
	// Returns nil if no permissions are configured.
	PermissionsInfo() *PermissionsInfo

	// CurrentAgentSkillsEnabled returns whether skills are enabled for the current agent.
	CurrentAgentSkillsEnabled() bool

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
}

// PermissionsInfo contains the allow and deny patterns for tool permissions.
type PermissionsInfo struct {
	Allow []string
	Deny  []string
}

type CurrentAgentInfo struct {
	Name        string
	Description string
	Commands    types.Commands
}

type ModelStore interface {
	GetModel(ctx context.Context, modelID string) (*modelsdev.Model, error)
}

// RAGInitializer is implemented by runtimes that support background RAG initialization.
// Local runtimes use this to start indexing early; remote runtimes typically do not.
type RAGInitializer interface {
	StartBackgroundRAGInit(ctx context.Context, sendEvent func(Event))
}

// LocalRuntime manages the execution of agents
type LocalRuntime struct {
	toolMap                     map[string]ToolHandler
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
	ragInitialized              atomic.Bool
	sessionCompactor            *sessionCompactor
	sessionStore                session.Store
	workingDir                  string   // Working directory for hooks execution
	env                         []string // Environment variables for hooks execution
	modelSwitcherCfg            *ModelSwitcherConfig

	// fallbackCooldowns tracks per-agent cooldown state for sticky fallback behavior
	fallbackCooldowns    map[string]*fallbackCooldownState
	fallbackCooldownsMux sync.RWMutex
}

type streamResult struct {
	Calls             []tools.ToolCall
	Content           string
	ReasoningContent  string
	ThinkingSignature string // Used with Anthropic's extended thinking feature
	ThoughtSignature  []byte
	Stopped           bool
	ActualModel       string      // The actual model used (may differ from configured model with routing)
	Usage             *chat.Usage // Token usage for this stream
	RateLimit         *chat.RateLimit
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

// NewLocalRuntime creates a new LocalRuntime without the persistence wrapper.
// This is useful for testing or when persistence is handled externally.
func NewLocalRuntime(agents *team.Team, opts ...Opt) (*LocalRuntime, error) {
	defaultAgent, err := agents.DefaultAgent()
	if err != nil {
		return nil, err
	}

	r := &LocalRuntime{
		toolMap:              make(map[string]ToolHandler),
		team:                 agents,
		currentAgent:         defaultAgent.Name(),
		resumeChan:           make(chan ResumeRequest),
		elicitationRequestCh: make(chan ElicitationResult),
		sessionCompaction:    true,
		managedOAuth:         true,
		sessionStore:         session.NewInMemorySessionStore(),
		fallbackCooldowns:    make(map[string]*fallbackCooldownState),
	}

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

	model := defaultAgent.Model()
	if model == nil {
		return nil, fmt.Errorf("agent %s has no valid model", defaultAgent.Name())
	}

	r.sessionCompactor = newSessionCompactor(model, r.sessionStore)

	slog.Debug("Creating new runtime", "agent", r.currentAgent, "available_agents", agents.Size())

	return r, nil
}

// StartBackgroundRAGInit initializes RAG in background and forwards events
// Should be called early (e.g., by App) to start indexing before RunStream
func (r *LocalRuntime) StartBackgroundRAGInit(ctx context.Context, sendEvent func(Event)) {
	if r.ragInitialized.Swap(true) {
		return
	}

	ragManagers := r.team.RAGManagers()
	if len(ragManagers) == 0 {
		return
	}

	slog.Debug("Starting background RAG initialization with event forwarding", "manager_count", len(ragManagers))

	// Set up event forwarding BEFORE starting initialization
	// This ensures all events are captured
	r.forwardRAGEvents(ctx, ragManagers, sendEvent)

	// Now start initialization (events will be forwarded)
	r.team.InitializeRAG(ctx)
	r.team.StartRAGFileWatchers(ctx)
}

// forwardRAGEvents forwards RAG manager events to the given callback
// Consolidates duplicated event forwarding logic
func (r *LocalRuntime) forwardRAGEvents(ctx context.Context, ragManagers map[string]*rag.Manager, sendEvent func(Event)) {
	for _, mgr := range ragManagers {
		go func(mgr *rag.Manager) {
			ragName := mgr.Name()
			slog.Debug("Starting RAG event forwarder goroutine", "rag", ragName)
			for {
				select {
				case <-ctx.Done():
					slog.Debug("RAG event forwarder stopped", "rag", ragName)
					return
				case ragEvent, ok := <-mgr.Events():
					if !ok {
						slog.Debug("RAG events channel closed", "rag", ragName)
						return
					}

					agentName := r.currentAgent
					slog.Debug("Forwarding RAG event", "type", ragEvent.Type, "rag", ragName, "agent", agentName)

					switch ragEvent.Type {
					case ragtypes.EventTypeIndexingStarted:
						sendEvent(RAGIndexingStarted(ragName, ragEvent.StrategyName, agentName))
					case ragtypes.EventTypeIndexingProgress:
						if ragEvent.Progress != nil {
							sendEvent(RAGIndexingProgress(ragName, ragEvent.StrategyName, ragEvent.Progress.Current, ragEvent.Progress.Total, agentName))
						}
					case ragtypes.EventTypeIndexingComplete:
						sendEvent(RAGIndexingCompleted(ragName, ragEvent.StrategyName, agentName))
					case ragtypes.EventTypeUsage:
						// Convert RAG usage to TokenUsageEvent so TUI displays it
						sendEvent(TokenUsage(
							"",
							agentName,
							ragEvent.TotalTokens, // input tokens (embeddings)
							0,                    // output tokens (0 for embeddings)
							ragEvent.TotalTokens, // context length
							0,                    // context limit (not applicable)
							ragEvent.Cost,
						))
					case ragtypes.EventTypeError:
						if ragEvent.Error != nil {
							sendEvent(Error(fmt.Sprintf("RAG %s error: %v", ragName, ragEvent.Error)))
						}
					default:
						// Log unhandled events for debugging
						slog.Debug("Unhandled RAG event type", "type", ragEvent.Type, "rag", ragName)
					}
				}
			}
		}(mgr)
	}
}

// InitializeRAG is called within RunStream as a fallback when background init wasn't used
// (e.g., for exec command or API mode where there's no App)
func (r *LocalRuntime) InitializeRAG(ctx context.Context, events chan Event) {
	// If already initialized via StartBackgroundRAGInit, skip entirely
	// Event forwarding was already set up there
	if r.ragInitialized.Swap(true) {
		slog.Debug("RAG already initialized, event forwarding already active", "manager_count", len(r.team.RAGManagers()))
		return
	}

	ragManagers := r.team.RAGManagers()
	if len(ragManagers) == 0 {
		return
	}

	slog.Debug("Setting up RAG initialization (fallback path for non-TUI)", "manager_count", len(ragManagers))

	// Set up event forwarding BEFORE starting initialization
	r.forwardRAGEvents(ctx, ragManagers, func(event Event) {
		events <- event
	})

	// Start initialization and file watchers
	r.team.InitializeRAG(ctx)
	r.team.StartRAGFileWatchers(ctx)
}

func (r *LocalRuntime) CurrentAgentName() string {
	return r.currentAgent
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
	r.currentAgent = agentName
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
	current, _ := r.team.Agent(r.currentAgent)
	return current
}

// CurrentAgentSkillsEnabled returns whether skills are enabled for the current agent.
func (r *LocalRuntime) CurrentAgentSkillsEnabled() bool {
	a := r.CurrentAgent()
	return a != nil && a.SkillsEnabled()
}

// ExecuteMCPPrompt executes an MCP prompt with provided arguments and returns the content.
func (r *LocalRuntime) ExecuteMCPPrompt(ctx context.Context, promptName string, arguments map[string]string) (string, error) {
	currentAgent := r.CurrentAgent()
	if currentAgent == nil {
		return "", fmt.Errorf("no current agent available")
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
				content.WriteString(fmt.Sprintf("[Non-text content: %T]", message.Content))
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
		Deny:  permChecker.DenyPatterns(),
	}
}

// ResetStartupInfo resets the startup info emission flag.
// This should be called when replacing a session to allow re-emission of
// agent, team, and toolset info to the UI.
func (r *LocalRuntime) ResetStartupInfo() {
	r.startupInfoEmitted = false
}

// EmitStartupInfo emits initial agent, team, and toolset information for immediate sidebar display
func (r *LocalRuntime) EmitStartupInfo(ctx context.Context, events chan Event) {
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
	if !send(AgentInfo(a.Name(), r.getEffectiveModelID(a), a.Description(), a.WelcomeMessage())) {
		return
	}
	if !send(TeamInfo(r.agentDetailsFromTeam(), r.currentAgent)) {
		return
	}

	// Emit agent warnings (if any) - these are quick
	r.emitAgentWarningsWithSend(a, send)

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
		send(ToolsetInfo(0, false, r.currentAgent))
		return
	}

	// Emit initial loading state
	if !send(ToolsetInfo(0, true, r.currentAgent)) {
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

		// Start the toolset if needed
		if startable, ok := toolset.(*tools.StartableToolSet); ok {
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
		if !send(ToolsetInfo(totalTools, !isLast, r.currentAgent)) {
			return
		}
	}

	// Emit final state (not loading)
	send(ToolsetInfo(totalTools, false, r.currentAgent))
}

// registerDefaultTools registers the default tool handlers
func (r *LocalRuntime) registerDefaultTools() {
	slog.Debug("Registering default tools")

	tt := builtin.NewTransferTaskTool()
	ht := builtin.NewHandoffTool()
	ttTools, _ := tt.Tools(context.TODO())
	htTools, _ := ht.Tools(context.TODO())
	allTools := append(ttTools, htTools...)

	handlers := map[string]ToolHandlerFunc{
		builtin.ToolNameTransferTask: r.handleTaskTransfer,
		builtin.ToolNameHandoff:      r.handleHandoff,
	}

	for _, t := range allTools {
		if h, exists := handlers[t.Name]; exists {
			r.toolMap[t.Name] = ToolHandler{handler: h, tool: t}
		} else {
			slog.Warn("No handler found for default tool", "tool", t.Name)
		}
	}

	slog.Debug("Registered default tools", "count", len(r.toolMap))
}

func (r *LocalRuntime) finalizeEventChannel(ctx context.Context, sess *session.Session, events chan Event) {
	defer close(events)

	events <- StreamStopped(sess.ID, r.currentAgent)

	telemetry.RecordSessionEnd(ctx)
}

// RunStream starts the agent's interaction loop and returns a channel of events
func (r *LocalRuntime) RunStream(ctx context.Context, sess *session.Session) <-chan Event {
	slog.Debug("Starting runtime stream", "agent", r.currentAgent, "session_id", sess.ID)
	events := make(chan Event, 128)

	go func() {
		telemetry.RecordSessionStart(ctx, r.currentAgent, sess.ID)

		ctx, sessionSpan := r.startSpan(ctx, "runtime.session", trace.WithAttributes(
			attribute.String("agent", r.currentAgent),
			attribute.String("session.id", sess.ID),
		))
		defer sessionSpan.End()

		// Set the events channel for elicitation requests
		r.setElicitationEventsChannel(events)
		defer r.clearElicitationEventsChannel()

		// Set elicitation handler on all MCP toolsets before getting tools
		a := r.CurrentAgent()

		// Emit agent information for sidebar display
		// Use getEffectiveModelID to account for active fallback cooldowns
		events <- AgentInfo(a.Name(), r.getEffectiveModelID(a), a.Description(), a.WelcomeMessage())

		// Emit team information
		events <- TeamInfo(r.agentDetailsFromTeam(), r.currentAgent)

		// Initialize RAG and forward events
		r.InitializeRAG(ctx, events)

		r.emitAgentWarnings(a, events)
		r.configureToolsetHandlers(a, events)

		agentTools, err := r.getTools(ctx, a, sessionSpan, events)
		if err != nil {
			events <- Error(fmt.Sprintf("failed to get tools: %v", err))
			return
		}

		events <- ToolsetInfo(len(agentTools), false, r.currentAgent)

		messages := sess.GetMessages(a)
		if sess.SendUserMessage {
			events <- UserMessage(messages[len(messages)-1].Content, sess.ID, len(sess.Messages)-1)
		}

		events <- StreamStarted(sess.ID, a.Name())

		defer r.finalizeEventChannel(ctx, sess, events)

		r.registerDefaultTools()

		iteration := 0
		// Use a runtime copy of maxIterations so we don't modify the session's persistent config
		runtimeMaxIterations := sess.MaxIterations

		for {
			// Set elicitation handler on all MCP toolsets before getting tools
			a := r.CurrentAgent()

			r.emitAgentWarnings(a, events)
			r.configureToolsetHandlers(a, events)

			agentTools, err := r.getTools(ctx, a, sessionSpan, events)
			if err != nil {
				events <- Error(fmt.Sprintf("failed to get tools: %v", err))
				return
			}

			// Check iteration limit
			if runtimeMaxIterations > 0 && iteration >= runtimeMaxIterations {
				slog.Debug(
					"Maximum iterations reached",
					"agent", a.Name(),
					"iterations", iteration,
					"max", runtimeMaxIterations,
				)

				events <- MaxIterationsReached(runtimeMaxIterations)

				// Wait for user decision (resume / reject)
				select {
				case req := <-r.resumeChan:
					if req.Type == ResumeTypeApprove {
						slog.Debug("User chose to continue after max iterations", "agent", a.Name())
						runtimeMaxIterations = iteration + 10
					} else {
						slog.Debug("User rejected continuation", "agent", a.Name())

						assistantMessage := chat.Message{
							Role: chat.MessageRoleAssistant,
							Content: fmt.Sprintf(
								"Execution stopped after reaching the configured max_iterations limit (%d).",
								runtimeMaxIterations,
							),
							CreatedAt: time.Now().Format(time.RFC3339),
						}

						addAgentMessage(sess, a, &assistantMessage, events)
						return
					}

				case <-ctx.Done():
					slog.Debug(
						"Context cancelled while waiting for resume confirmation",
						"agent", a.Name(),
						"session_id", sess.ID,
					)
					return
				}
			}

			iteration++

			// Exit immediately if the stream context has been cancelled (e.g., Ctrl+C)
			if err := ctx.Err(); err != nil {
				slog.Debug("Runtime stream context cancelled, stopping loop", "agent", a.Name(), "session_id", sess.ID)
				return
			}
			slog.Debug("Starting conversation loop iteration", "agent", a.Name())

			streamCtx, streamSpan := r.startSpan(ctx, "runtime.stream", trace.WithAttributes(
				attribute.String("agent", a.Name()),
				attribute.String("session.id", sess.ID),
			))

			model := a.Model()

			// Apply thinking setting based on session state.
			// When thinking is disabled: clone with thinking=false to clear any thinking config.
			// When thinking is enabled: clone with thinking=true to ensure defaults are applied
			// (this handles models with no thinking config, explicitly disabled thinking, or
			// models that already have thinking configured).
			if !sess.Thinking {
				model = provider.CloneWithOptions(ctx, model, options.WithThinking(false))
				slog.Debug("Cloned provider with thinking disabled", "agent", a.Name(), "model", model.ID())
			} else {
				// Always clone with thinking=true when session has thinking enabled.
				// applyOverrides will apply provider defaults if ThinkingBudget is nil or disabled.
				model = provider.CloneWithOptions(ctx, model, options.WithThinking(true))
				slog.Debug("Cloned provider with thinking enabled", "agent", a.Name(), "model", model.ID())
			}

			modelID := model.ID()
			slog.Debug("Using agent", "agent", a.Name(), "model", modelID)
			slog.Debug("Getting model definition", "model_id", modelID)
			m, err := r.modelsStore.GetModel(ctx, modelID)
			if err != nil {
				slog.Debug("Failed to get model definition", "error", err)
			}

			var contextLimit int64
			if m != nil {
				contextLimit = int64(m.Limit.Context)
			}

			if m != nil && r.sessionCompaction {
				if sess.InputTokens+sess.OutputTokens > int64(float64(contextLimit)*0.9) {
					r.Summarize(ctx, sess, "", events)
					events <- TokenUsage(sess.ID, r.currentAgent, sess.InputTokens, sess.OutputTokens, sess.InputTokens+sess.OutputTokens, contextLimit, sess.Cost)
				}
			}

			messages := sess.GetMessages(a)
			slog.Debug("Retrieved messages for processing", "agent", a.Name(), "message_count", len(messages))

			// Try primary model with fallback chain if configured
			res, usedModel, err := r.tryModelWithFallback(streamCtx, a, model, messages, agentTools, sess, m, events)
			if err != nil {
				// Treat context cancellation as a graceful stop
				if errors.Is(err, context.Canceled) {
					slog.Debug("Model stream canceled by context", "agent", a.Name(), "session_id", sess.ID)
					streamSpan.End()
					return
				}
				streamSpan.RecordError(err)
				streamSpan.SetStatus(codes.Error, "error handling stream")
				slog.Error("All models failed", "agent", a.Name(), "error", err)
				// Track error in telemetry
				telemetry.RecordError(ctx, err.Error())
				events <- Error(err.Error())
				streamSpan.End()
				return
			}

			// Update model info if we used a fallback
			if usedModel != nil && usedModel.ID() != model.ID() {
				slog.Info("Used fallback model", "agent", a.Name(), "primary", model.ID(), "used", usedModel.ID())
				events <- AgentInfo(a.Name(), usedModel.ID(), a.Description(), a.WelcomeMessage())
			}
			streamSpan.SetAttributes(
				attribute.Int("tool.calls", len(res.Calls)),
				attribute.Int("content.length", len(res.Content)),
				attribute.Bool("stopped", res.Stopped),
			)
			streamSpan.End()
			slog.Debug("Stream processed", "agent", a.Name(), "tool_calls", len(res.Calls), "content_length", len(res.Content), "stopped", res.Stopped)

			// Add assistant message to conversation history, but skip empty assistant messages
			// Providers reject assistant messages that have neither content nor tool calls.
			var msgUsage *MessageUsage
			if strings.TrimSpace(res.Content) != "" || len(res.Calls) > 0 {
				// Build tool definitions for the tool calls
				var toolDefs []tools.Tool
				if len(res.Calls) > 0 {
					toolMap := make(map[string]tools.Tool, len(agentTools))
					for _, t := range agentTools {
						toolMap[t.Name] = t
					}
					for _, call := range res.Calls {
						if def, ok := toolMap[call.Function.Name]; ok {
							toolDefs = append(toolDefs, def)
						}
					}
				}

				// Calculate per-message cost if usage and pricing info available
				var messageCost float64
				if res.Usage != nil && m != nil && m.Cost != nil {
					messageCost = (float64(res.Usage.InputTokens)*m.Cost.Input +
						float64(res.Usage.OutputTokens)*m.Cost.Output +
						float64(res.Usage.CachedInputTokens)*m.Cost.CacheRead +
						float64(res.Usage.CacheWriteTokens)*m.Cost.CacheWrite) / 1e6
				}

				// Determine the model name to store
				messageModel := modelID
				if res.ActualModel != "" {
					messageModel = res.ActualModel
				}

				assistantMessage := chat.Message{
					Role:              chat.MessageRoleAssistant,
					Content:           res.Content,
					ReasoningContent:  res.ReasoningContent,
					ThinkingSignature: res.ThinkingSignature,
					ThoughtSignature:  res.ThoughtSignature,
					ToolCalls:         res.Calls,
					ToolDefinitions:   toolDefs,
					CreatedAt:         time.Now().Format(time.RFC3339),
					Usage:             res.Usage,
					Model:             messageModel,
					Cost:              messageCost,
				}

				// Build per-message usage for the event
				if res.Usage != nil {
					msgUsage = &MessageUsage{
						Usage: *res.Usage,
						Cost:  messageCost,
						Model: messageModel,
					}
				}
				if res.RateLimit != nil {
					msgUsage.RateLimit = *res.RateLimit
				}

				addAgentMessage(sess, a, &assistantMessage, events)
				slog.Debug("Added assistant message to session", "agent", a.Name(), "total_messages", len(sess.GetAllMessages()))
			} else {
				slog.Debug("Skipping empty assistant message (no content and no tool calls)", "agent", a.Name())
			}

			events <- TokenUsageWithMessage(sess.ID, r.currentAgent, sess.InputTokens, sess.OutputTokens, sess.InputTokens+sess.OutputTokens, contextLimit, sess.Cost, msgUsage)

			r.processToolCalls(ctx, sess, res.Calls, agentTools, events)

			if res.Stopped {
				slog.Debug("Conversation stopped", "agent", a.Name())
				break
			}
		}
	}()

	return events
}

// getTools executes tool retrieval with automatic OAuth handling
func (r *LocalRuntime) getTools(ctx context.Context, a *agent.Agent, sessionSpan trace.Span, events chan Event) ([]tools.Tool, error) {
	shouldEmitMCPInit := len(a.ToolSets()) > 0
	if shouldEmitMCPInit {
		events <- MCPInitStarted(a.Name())
	}
	defer func() {
		if shouldEmitMCPInit {
			events <- MCPInitFinished(a.Name())
		}
	}()

	agentTools, err := a.Tools(ctx)
	if err != nil {
		slog.Error("Failed to get agent tools", "agent", a.Name(), "error", err)
		sessionSpan.RecordError(err)
		sessionSpan.SetStatus(codes.Error, "failed to get tools")
		telemetry.RecordError(ctx, err.Error())
		return nil, err
	}

	slog.Debug("Retrieved agent tools", "agent", a.Name(), "tool_count", len(agentTools))
	return agentTools, nil
}

// configureToolsetHandlers sets up elicitation and OAuth handlers for all toolsets of an agent.
func (r *LocalRuntime) configureToolsetHandlers(a *agent.Agent, events chan Event) {
	for _, toolset := range a.ToolSets() {
		tools.ConfigureHandlers(toolset,
			r.elicitationHandler,
			func() { events <- Authorization(tools.ElicitationActionAccept, r.currentAgent) },
			r.managedOAuth,
		)
	}
}

// emitAgentWarningsWithSend emits agent warnings using the provided send function for context-aware sending.
func (r *LocalRuntime) emitAgentWarningsWithSend(a *agent.Agent, send func(Event) bool) {
	warnings := a.DrainWarnings()
	if len(warnings) == 0 {
		return
	}

	slog.Warn("Tool setup partially failed; continuing", "agent", a.Name(), "warnings", warnings)
	send(Warning(formatToolWarning(a, warnings), r.currentAgent))
}

func (r *LocalRuntime) emitAgentWarnings(a *agent.Agent, events chan Event) {
	warnings := a.DrainWarnings()
	if len(warnings) == 0 {
		return
	}

	slog.Warn("Tool setup partially failed; continuing", "agent", a.Name(), "warnings", warnings)

	if events != nil {
		events <- Warning(formatToolWarning(a, warnings), r.currentAgent)
	}
}

func formatToolWarning(a *agent.Agent, warnings []string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Some toolsets failed to initialize for agent '%s'.\n\nDetails:\n\n", a.Name())
	for _, warning := range warnings {
		fmt.Fprintf(&builder, "- %s\n", warning)
	}
	return strings.TrimSuffix(builder.String(), "\n")
}

func (r *LocalRuntime) Resume(_ context.Context, req ResumeRequest) {
	slog.Debug("Resuming runtime", "agent", r.currentAgent, "type", req.Type, "reason", req.Reason)

	// Defensive validation:
	//
	// The runtime may be resumed by multiple entry points (API, CLI, TUI, tests).
	// Even if upstream layers perform validation, the runtime must never assume
	// the ResumeType is valid. Accepting invalid values here leads to confusing
	// downstream behavior where tool execution fails without a clear cause.
	if !IsValidResumeType(req.Type) {
		slog.Warn(
			"Invalid resume type received; ignoring resume request",
			"agent", r.currentAgent,
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
		slog.Debug("Resume signal sent", "agent", r.currentAgent)
	default:
		slog.Debug(
			"Resume channel not ready; resume signal dropped",
			"agent", r.currentAgent,
			"confirmation_type", req.Type,
		)
	}
}

// ResumeElicitation sends an elicitation response back to a waiting elicitation request
func (r *LocalRuntime) ResumeElicitation(ctx context.Context, action tools.ElicitationAction, content map[string]any) error {
	slog.Debug("Resuming runtime with elicitation response", "agent", r.currentAgent, "action", action)

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
		return fmt.Errorf("no elicitation request in progress")
	}
}

// Run starts the agent's interaction loop
func (r *LocalRuntime) Run(ctx context.Context, sess *session.Session) ([]session.Message, error) {
	eventsChan := r.RunStream(ctx, sess)

	for event := range eventsChan {
		if errEvent, ok := event.(*ErrorEvent); ok {
			return nil, fmt.Errorf("%s", errEvent.Error)
		}
	}

	return sess.GetAllMessages(), nil
}

func (r *LocalRuntime) handleStream(ctx context.Context, stream chat.MessageStream, a *agent.Agent, agentTools []tools.Tool, sess *session.Session, m *modelsdev.Model, events chan Event) (streamResult, error) {
	defer stream.Close()

	var fullContent strings.Builder
	var fullReasoningContent strings.Builder
	var thinkingSignature string
	var thoughtSignature []byte
	var toolCalls []tools.ToolCall
	var actualModel string
	var actualModelEventEmitted bool
	var messageUsage *chat.Usage
	var messageRateLimit *chat.RateLimit

	modelID := getAgentModelID(a)
	toolCallIndex := make(map[string]int)   // toolCallID -> index in toolCalls slice
	emittedPartial := make(map[string]bool) // toolCallID -> whether we've emitted a partial event
	toolDefMap := make(map[string]tools.Tool, len(agentTools))
	for _, t := range agentTools {
		toolDefMap[t.Name] = t
	}

	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return streamResult{Stopped: true}, fmt.Errorf("error receiving from stream: %w", err)
		}

		if response.Usage != nil {
			messageUsage = response.Usage

			if m != nil && m.Cost != nil {
				cost := float64(response.Usage.InputTokens)*m.Cost.Input +
					float64(response.Usage.OutputTokens)*m.Cost.Output +
					float64(response.Usage.CachedInputTokens)*m.Cost.CacheRead +
					float64(response.Usage.CacheWriteTokens)*m.Cost.CacheWrite
				sess.Cost += cost / 1e6
			}

			sess.InputTokens = response.Usage.InputTokens + response.Usage.CachedInputTokens + response.Usage.CacheWriteTokens
			sess.OutputTokens = response.Usage.OutputTokens

			modelName := "unknown"
			if m != nil {
				modelName = m.Name
			}
			telemetry.RecordTokenUsage(ctx, modelName, sess.InputTokens, sess.OutputTokens, sess.Cost)
		}

		if response.RateLimit != nil {
			messageRateLimit = response.RateLimit
		}

		if len(response.Choices) == 0 {
			continue
		}
		choice := response.Choices[0]

		if len(choice.Delta.ThoughtSignature) > 0 {
			thoughtSignature = choice.Delta.ThoughtSignature
		}

		// Capture the actual model from the stream response (useful for model routing)
		// Emit AgentInfo immediately when we discover the actual model differs from configured
		if actualModel == "" && response.Model != "" {
			actualModel = response.Model
			if !actualModelEventEmitted && actualModel != modelID {
				// NOTE(krissetto):Prepend the provider from the configured modelID to maintain consistent format
				// every other invocation in the code uses the provider/model format
				formattedModel := actualModel
				if idx := strings.Index(modelID, "/"); idx != -1 {
					formattedModel = modelID[:idx+1] + actualModel
				}
				slog.Debug("Detected actual model differs from configured model (streaming)", "configured", modelID, "actual", formattedModel)
				events <- AgentInfo(a.Name(), formattedModel, a.Description(), a.WelcomeMessage())
				actualModelEventEmitted = true
			}
		}

		if choice.FinishReason == chat.FinishReasonStop || choice.FinishReason == chat.FinishReasonLength {
			return streamResult{
				Calls:             toolCalls,
				Content:           fullContent.String(),
				ReasoningContent:  fullReasoningContent.String(),
				ThinkingSignature: thinkingSignature,
				ThoughtSignature:  thoughtSignature,
				Stopped:           true,
				ActualModel:       actualModel,
				Usage:             messageUsage,
				RateLimit:         messageRateLimit,
			}, nil
		}

		// Handle tool calls
		if len(choice.Delta.ToolCalls) > 0 {
			// Process each tool call delta
			for _, delta := range choice.Delta.ToolCalls {
				idx, exists := toolCallIndex[delta.ID]
				if !exists {
					idx = len(toolCalls)
					toolCallIndex[delta.ID] = idx
					toolCalls = append(toolCalls, tools.ToolCall{
						ID:   delta.ID,
						Type: delta.Type,
					})
				}

				tc := &toolCalls[idx]

				// Track if we're learning the name for the first time
				learningName := delta.Function.Name != "" && tc.Function.Name == ""

				// Update fields from delta
				if delta.Type != "" {
					tc.Type = delta.Type
				}
				if delta.Function.Name != "" {
					tc.Function.Name = delta.Function.Name
				}
				if delta.Function.Arguments != "" {
					tc.Function.Arguments += delta.Function.Arguments
				}

				// Emit PartialToolCall once we have a name, and on subsequent argument deltas
				if tc.Function.Name != "" && (learningName || delta.Function.Arguments != "") {
					if !emittedPartial[delta.ID] || delta.Function.Arguments != "" {
						events <- PartialToolCall(*tc, toolDefMap[tc.Function.Name], a.Name())
						emittedPartial[delta.ID] = true
					}
				}
			}
			continue
		}

		if choice.Delta.ReasoningContent != "" {
			events <- AgentChoiceReasoning(a.Name(), choice.Delta.ReasoningContent)
			fullReasoningContent.WriteString(choice.Delta.ReasoningContent)
		}

		// Capture thinking signature for Anthropic extended thinking
		if choice.Delta.ThinkingSignature != "" {
			thinkingSignature = choice.Delta.ThinkingSignature
		}

		if choice.Delta.Content != "" {
			events <- AgentChoice(a.Name(), choice.Delta.Content)
			fullContent.WriteString(choice.Delta.Content)
		}
	}

	// If the stream completed without producing any content or tool calls, likely because of a token limit, stop to avoid breaking the request loop
	// NOTE(krissetto): this can likely be removed once compaction works properly with all providers (aka dmr)
	stoppedDueToNoOutput := fullContent.Len() == 0 && len(toolCalls) == 0
	return streamResult{
		Calls:             toolCalls,
		Content:           fullContent.String(),
		ReasoningContent:  fullReasoningContent.String(),
		ThinkingSignature: thinkingSignature,
		ThoughtSignature:  thoughtSignature,
		Stopped:           stoppedDueToNoOutput,
		ActualModel:       actualModel,
		Usage:             messageUsage,
		RateLimit:         messageRateLimit,
	}, nil
}

// processToolCalls handles the execution of tool calls for an agent
func (r *LocalRuntime) processToolCalls(ctx context.Context, sess *session.Session, calls []tools.ToolCall, agentTools []tools.Tool, events chan Event) {
	a := r.CurrentAgent()
	slog.Debug("Processing tool calls", "agent", a.Name(), "call_count", len(calls))

	// Build a map of agent tools for quick lookup
	agentToolMap := make(map[string]tools.Tool, len(agentTools))
	for _, t := range agentTools {
		agentToolMap[t.Name] = t
	}

	for i, toolCall := range calls {
		callCtx, callSpan := r.startSpan(ctx, "runtime.tool.call", trace.WithAttributes(
			attribute.String("tool.name", toolCall.Function.Name),
			attribute.String("tool.type", string(toolCall.Type)),
			attribute.String("agent", a.Name()),
			attribute.String("session.id", sess.ID),
			attribute.String("tool.call_id", toolCall.ID),
		))

		slog.Debug("Processing tool call", "agent", a.Name(), "tool", toolCall.Function.Name, "session_id", sess.ID)

		// Find the tool - first check runtime tools, then agent tools
		var tool tools.Tool
		var runTool func()

		if def, exists := r.toolMap[toolCall.Function.Name]; exists {
			// Validate that the tool is actually available to this agent
			if _, available := agentToolMap[toolCall.Function.Name]; !available {
				slog.Warn("Tool call rejected: tool not available to agent", "agent", a.Name(), "tool", toolCall.Function.Name, "session_id", sess.ID)
				r.addToolErrorResponse(ctx, sess, toolCall, def.tool, events, a, fmt.Sprintf("Tool '%s' is not available to this agent (%s).", toolCall.Function.Name, a.Name()))
				callSpan.SetStatus(codes.Error, "tool not available to agent")
				callSpan.End()
				continue
			}
			tool = def.tool
			runTool = func() { r.runAgentTool(callCtx, def.handler, sess, toolCall, def.tool, events, a) }
		} else if t, exists := agentToolMap[toolCall.Function.Name]; exists {
			tool = t
			runTool = func() { r.runTool(callCtx, t, toolCall, events, sess, a) }
		} else {
			// Tool not found - skip
			callSpan.SetStatus(codes.Ok, "tool not found")
			callSpan.End()
			continue
		}

		// Execute tool with approval check
		canceled := r.executeWithApproval(callCtx, sess, toolCall, tool, events, a, runTool, calls[i+1:])
		if canceled {
			callSpan.SetStatus(codes.Ok, "tool call canceled by user")
			callSpan.End()
			return
		}

		callSpan.SetStatus(codes.Ok, "tool call processed")
		callSpan.End()
	}
}

// executeWithApproval handles the tool approval flow and executes the tool.
// Returns true if the operation was canceled and processing should stop.
//
// The approval flow considers (in order):
//
//  1. Session-level permissions (if configured) - pattern-based Allow/Deny rules
//  2. Team-level permissions config - checked second
//  3. sess.ToolsApproved (--yolo flag) - auto-approve all
//  4. tool.Annotations.ReadOnlyHint - auto-approve read-only tools
//  5. Default: ask for user confirmation
//
// Example session permissions configuration:
//
//	sess.Permissions = &session.PermissionsConfig{
//	    Allow: []string{"read_*", "think"},  // auto-approve matching tools
//	    Deny:  []string{"shell", "exec_*"},  // block matching tools
//	}
func (r *LocalRuntime) executeWithApproval(
	ctx context.Context,
	sess *session.Session,
	toolCall tools.ToolCall,
	tool tools.Tool,
	events chan Event,
	a *agent.Agent,
	runTool func(),
	remainingCalls []tools.ToolCall,
) (canceled bool) {
	toolName := toolCall.Function.Name

	// Parse tool arguments once for permission matching
	var toolArgs map[string]any
	if toolCall.Function.Arguments != "" {
		if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &toolArgs); err != nil {
			slog.Debug("Failed to parse tool arguments for permission check", "tool", toolName, "error", err)
			// Continue with nil args - will only match tool name patterns
		}
	}

	// 1. Check session-level permissions first (if configured)
	if sess.Permissions != nil {
		sessionChecker := permissions.NewChecker(&latest.PermissionsConfig{
			Allow: sess.Permissions.Allow,
			Deny:  sess.Permissions.Deny,
		})
		decision := sessionChecker.CheckWithArgs(toolName, toolArgs)
		switch decision {
		case permissions.Deny:
			slog.Debug("Tool denied by session permissions", "tool", toolName, "session_id", sess.ID)
			r.addToolErrorResponse(ctx, sess, toolCall, tool, events, a, fmt.Sprintf("Tool '%s' is denied by session permissions.", toolName))
			return false
		case permissions.Allow:
			slog.Debug("Tool auto-approved by session permissions", "tool", toolName, "session_id", sess.ID)
			runTool()
			return false
		case permissions.Ask:
			// Fall through to team permissions
		}
	}

	// 2. Check team-level permissions config
	if permChecker := r.team.Permissions(); permChecker != nil {
		decision := permChecker.CheckWithArgs(toolName, toolArgs)
		switch decision {
		case permissions.Deny:
			slog.Debug("Tool denied by team permissions config", "tool", toolName, "session_id", sess.ID)
			r.addToolErrorResponse(ctx, sess, toolCall, tool, events, a, fmt.Sprintf("Tool '%s' is denied by permissions configuration.", toolName))
			return false
		case permissions.Allow:
			slog.Debug("Tool auto-approved by team permissions config", "tool", toolName, "session_id", sess.ID)
			runTool()
			return false
		case permissions.Ask:
			// Fall through to normal approval flow
		}
	}

	// 3. Check --yolo flag or read-only hint
	if sess.ToolsApproved || tool.Annotations.ReadOnlyHint {
		runTool()
		return false
	}

	// Ask user for confirmation
	slog.Debug("Tools not approved, waiting for resume", "tool", toolCall.Function.Name, "session_id", sess.ID)
	events <- ToolCallConfirmation(toolCall, tool, a.Name())

	select {
	case req := <-r.resumeChan:
		switch req.Type {
		case ResumeTypeApprove:
			slog.Debug("Resume signal received, approving tool", "tool", toolCall.Function.Name, "session_id", sess.ID)
			runTool()
		case ResumeTypeApproveSession:
			slog.Debug("Resume signal received, approving session", "tool", toolCall.Function.Name, "session_id", sess.ID)
			sess.ToolsApproved = true
			runTool()
		case ResumeTypeApproveTool:
			// Add the tool to session's allow list for future auto-approval
			approvedTool := req.ToolName
			if approvedTool == "" {
				approvedTool = toolName
			}
			if sess.Permissions == nil {
				sess.Permissions = &session.PermissionsConfig{}
			}
			if !slices.Contains(sess.Permissions.Allow, approvedTool) {
				sess.Permissions.Allow = append(sess.Permissions.Allow, approvedTool)
			}
			slog.Debug("Resume signal received, approving tool permanently", "tool", approvedTool, "session_id", sess.ID)
			runTool()
		case ResumeTypeReject:
			slog.Debug("Resume signal received, rejecting tool", "tool", toolCall.Function.Name, "session_id", sess.ID, "reason", req.Reason)
			rejectMsg := "The user rejected the tool call."
			if strings.TrimSpace(req.Reason) != "" {
				rejectMsg += " Reason: " + strings.TrimSpace(req.Reason)
			}
			r.addToolErrorResponse(ctx, sess, toolCall, tool, events, a, rejectMsg)
		}
		return false
	case <-ctx.Done():
		slog.Debug("Context cancelled while waiting for resume", "tool", toolCall.Function.Name, "session_id", sess.ID)
		r.addToolErrorResponse(ctx, sess, toolCall, tool, events, a, "The tool call was canceled by the user.")
		for _, remainingCall := range remainingCalls {
			r.addToolErrorResponse(ctx, sess, remainingCall, tool, events, a, "The tool call was canceled by the user.")
		}
		return true
	}
}

// executeToolWithHandler is a common helper that handles tool execution, error handling,
// event emission, and session updates. It reduces duplication between runTool and runAgentTool.
func (r *LocalRuntime) executeToolWithHandler(
	ctx context.Context,
	toolCall tools.ToolCall,
	tool tools.Tool,
	events chan Event,
	sess *session.Session,
	a *agent.Agent,
	spanName string,
	execute func(ctx context.Context) (*tools.ToolCallResult, time.Duration, error),
) *tools.ToolCallResult {
	ctx, span := r.startSpan(ctx, spanName, trace.WithAttributes(
		attribute.String("tool.name", toolCall.Function.Name),
		attribute.String("agent", a.Name()),
		attribute.String("session.id", sess.ID),
		attribute.String("tool.call_id", toolCall.ID),
	))
	defer span.End()

	events <- ToolCall(toolCall, tool, a.Name())

	res, duration, err := execute(ctx)

	telemetry.RecordToolCall(ctx, toolCall.Function.Name, sess.ID, a.Name(), duration, err)

	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			slog.Debug("Tool handler canceled by context", "tool", toolCall.Function.Name, "agent", a.Name(), "session_id", sess.ID)
			res = tools.ResultError("The tool call was canceled by the user.")
			span.SetStatus(codes.Ok, "tool handler canceled by user")
		} else {
			span.RecordError(err)
			span.SetStatus(codes.Error, "tool handler error")
			slog.Error("Error calling tool", "tool", toolCall.Function.Name, "error", err)
			res = tools.ResultError(fmt.Sprintf("Error calling tool: %v", err))
		}
	} else {
		span.SetStatus(codes.Ok, "tool handler completed")
		slog.Debug("Tool call completed", "tool", toolCall.Function.Name, "output_length", len(res.Output))
	}

	events <- ToolCallResponse(toolCall, tool, res, res.Output, a.Name())

	// Ensure tool response content is not empty for API compatibility
	content := res.Output
	if strings.TrimSpace(content) == "" {
		content = "(no output)"
	}

	toolResponseMsg := chat.Message{
		Role:       chat.MessageRoleTool,
		Content:    content,
		ToolCallID: toolCall.ID,
		CreatedAt:  time.Now().Format(time.RFC3339),
	}
	addAgentMessage(sess, a, &toolResponseMsg, events)
	return res
}

// runTool executes agent tools from toolsets (MCP, filesystem, etc.).
func (r *LocalRuntime) runTool(ctx context.Context, tool tools.Tool, toolCall tools.ToolCall, events chan Event, sess *session.Session, a *agent.Agent) {
	// Get hooks executor for this agent
	hooksExec := r.getHooksExecutor(a)

	// Execute pre-tool hooks if configured
	if hooksExec != nil && hooksExec.HasPreToolUseHooks() {
		toolInput := parseToolInput(toolCall.Function.Arguments)
		input := &hooks.Input{
			SessionID: sess.ID,
			Cwd:       r.workingDir,
			ToolName:  toolCall.Function.Name,
			ToolUseID: toolCall.ID,
			ToolInput: toolInput,
		}

		result, err := hooksExec.ExecutePreToolUse(ctx, input)
		switch {
		case err != nil:
			slog.Warn("Pre-tool hook execution failed", "tool", toolCall.Function.Name, "error", err)
		case !result.Allowed:
			// Hook blocked the tool call
			slog.Debug("Pre-tool hook blocked tool call", "tool", toolCall.Function.Name, "message", result.Message)
			events <- HookBlocked(toolCall, tool, result.Message, a.Name())
			r.addToolErrorResponse(ctx, sess, toolCall, tool, events, a, "Tool call blocked by hook: "+result.Message)
			return
		case result.SystemMessage != "":
			events <- Warning(result.SystemMessage, a.Name())
		}
	}

	toolResult := r.executeToolWithHandler(ctx, toolCall, tool, events, sess, a, "runtime.tool.handler",
		func(ctx context.Context) (*tools.ToolCallResult, time.Duration, error) {
			res, err := tool.Handler(ctx, toolCall)
			return res, 0, err
		})

	// Execute post-tool hooks if configured
	if hooksExec != nil && hooksExec.HasPostToolUseHooks() {
		toolInput := parseToolInput(toolCall.Function.Arguments)
		var toolResponse any
		if toolResult != nil {
			toolResponse = toolResult.Output
		}
		input := &hooks.Input{
			SessionID:    sess.ID,
			Cwd:          r.workingDir,
			ToolName:     toolCall.Function.Name,
			ToolUseID:    toolCall.ID,
			ToolInput:    toolInput,
			ToolResponse: toolResponse,
		}

		result, err := hooksExec.ExecutePostToolUse(ctx, input)
		if err != nil {
			slog.Warn("Post-tool hook execution failed", "tool", toolCall.Function.Name, "error", err)
		} else if result.SystemMessage != "" {
			events <- Warning(result.SystemMessage, a.Name())
		}
	}
}

// parseToolInput parses tool arguments JSON into a map
func parseToolInput(arguments string) map[string]any {
	var result map[string]any
	if err := json.Unmarshal([]byte(arguments), &result); err != nil {
		return nil
	}
	return result
}

func (r *LocalRuntime) runAgentTool(ctx context.Context, handler ToolHandlerFunc, sess *session.Session, toolCall tools.ToolCall, tool tools.Tool, events chan Event, a *agent.Agent) {
	r.executeToolWithHandler(ctx, toolCall, tool, events, sess, a, "runtime.tool.handler.runtime",
		func(ctx context.Context) (*tools.ToolCallResult, time.Duration, error) {
			start := time.Now()
			res, err := handler(ctx, sess, toolCall, events)
			return res, time.Since(start), err
		})
}

func addAgentMessage(sess *session.Session, a *agent.Agent, msg *chat.Message, events chan Event) {
	agentMsg := session.NewAgentMessage(a, msg)
	sess.AddMessage(agentMsg)
	events <- MessageAdded(sess.ID, agentMsg, a.Name())
}

// addToolErrorResponse adds a tool error response to the session and emits the event.
// This consolidates the common pattern used by validation, rejection, and cancellation responses.
func (r *LocalRuntime) addToolErrorResponse(_ context.Context, sess *session.Session, toolCall tools.ToolCall, tool tools.Tool, events chan Event, a *agent.Agent, errorMsg string) {
	events <- ToolCallResponse(toolCall, tool, tools.ResultError(errorMsg), errorMsg, a.Name())

	toolResponseMsg := chat.Message{
		Role:       chat.MessageRoleTool,
		Content:    errorMsg,
		ToolCallID: toolCall.ID,
		CreatedAt:  time.Now().Format(time.RFC3339),
	}
	addAgentMessage(sess, a, &toolResponseMsg, events)
}

// startSpan wraps tracer.Start, returning a no-op span if the tracer is nil.
func (r *LocalRuntime) startSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if r.tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	return r.tracer.Start(ctx, name, opts...)
}

func (r *LocalRuntime) handleTaskTransfer(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, evts chan Event) (*tools.ToolCallResult, error) {
	var params struct {
		Agent          string `json:"agent"`
		Task           string `json:"task"`
		ExpectedOutput string `json:"expected_output"`
	}

	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	a := r.CurrentAgent()

	// Validate that the target agent is in the current agent's sub-agents list
	subAgents := a.SubAgents()
	if !slices.ContainsFunc(subAgents, func(sa *agent.Agent) bool { return sa.Name() == params.Agent }) {
		var subAgentNames []string
		for _, sa := range subAgents {
			subAgentNames = append(subAgentNames, sa.Name())
		}
		var errorMsg string
		if len(subAgentNames) > 0 {
			errorMsg = fmt.Sprintf("Agent %s cannot transfer task to %s: target agent not in sub-agents list. Available sub-agent IDs are: %s", a.Name(), params.Agent, strings.Join(subAgentNames, ", "))
		} else {
			errorMsg = fmt.Sprintf("Agent %s cannot transfer task to %s: target agent not in sub-agents list. This agent has no sub-agents configured.", a.Name(), params.Agent)
		}
		return tools.ResultError(errorMsg), nil
	}

	// Span for task transfer (optional)
	ctx, span := r.startSpan(ctx, "runtime.task_transfer", trace.WithAttributes(
		attribute.String("from.agent", a.Name()),
		attribute.String("to.agent", params.Agent),
		attribute.String("session.id", sess.ID),
	))
	defer span.End()

	slog.Debug("Transferring task to agent", "from_agent", a.Name(), "to_agent", params.Agent, "task", params.Task)

	ca := r.currentAgent

	// Emit agent switching start event
	evts <- AgentSwitching(true, ca, params.Agent)

	r.currentAgent = params.Agent
	defer func() {
		r.currentAgent = ca

		// Emit agent switching end event
		evts <- AgentSwitching(false, params.Agent, ca)

		// Restore original agent info in sidebar
		if originalAgent, err := r.team.Agent(ca); err == nil {
			evts <- AgentInfo(originalAgent.Name(), getAgentModelID(originalAgent), originalAgent.Description(), originalAgent.WelcomeMessage())
		}
	}()

	// Emit agent info for the new agent
	if newAgent, err := r.team.Agent(params.Agent); err == nil {
		evts <- AgentInfo(newAgent.Name(), getAgentModelID(newAgent), newAgent.Description(), newAgent.WelcomeMessage())
	}

	memberAgentTask := "You are a member of a team of agents. Your goal is to complete the following task:"
	memberAgentTask += fmt.Sprintf("\n\n<task>\n%s\n</task>", params.Task)
	if params.ExpectedOutput != "" {
		memberAgentTask += fmt.Sprintf("\n\n<expected_output>\n%s\n</expected_output>", params.ExpectedOutput)
	}

	slog.Debug("Creating new session with parent session", "parent_session_id", sess.ID, "tools_approved", sess.ToolsApproved, "thinking", sess.Thinking)

	child, err := r.team.Agent(params.Agent)
	if err != nil {
		return nil, err
	}

	s := session.New(
		session.WithSystemMessage(memberAgentTask),
		session.WithImplicitUserMessage("Please proceed."),
		session.WithMaxIterations(child.MaxIterations()),
		session.WithTitle("Transferred task"),
		session.WithToolsApproved(sess.ToolsApproved),
		session.WithThinking(sess.Thinking),
		session.WithSendUserMessage(false),
		session.WithParentID(sess.ID),
	)

	for event := range r.RunStream(ctx, s) {
		evts <- event
		if errEvent, ok := event.(*ErrorEvent); ok {
			span.RecordError(fmt.Errorf("%s", errEvent.Error))
			span.SetStatus(codes.Error, "error in transferred session")
			return nil, fmt.Errorf("%s", errEvent.Error)
		}
	}

	sess.ToolsApproved = s.ToolsApproved
	sess.Thinking = s.Thinking

	sess.AddSubSession(s)
	evts <- SubSessionCompleted(sess.ID, s, a.Name())

	slog.Debug("Task transfer completed", "agent", params.Agent, "task", params.Task)

	span.SetStatus(codes.Ok, "task transfer completed")
	return tools.ResultSuccess(s.GetLastAssistantMessageContent()), nil
}

func (r *LocalRuntime) handleHandoff(_ context.Context, _ *session.Session, toolCall tools.ToolCall, _ chan Event) (*tools.ToolCallResult, error) {
	var params builtin.HandoffArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	ca := r.currentAgent
	currentAgent, err := r.team.Agent(ca)
	if err != nil {
		return nil, fmt.Errorf("current agent not found: %w", err)
	}

	// Validate that the target agent is in the current agent's handoffs list
	handoffs := currentAgent.Handoffs()
	if !slices.ContainsFunc(handoffs, func(a *agent.Agent) bool { return a.Name() == params.Agent }) {
		var handoffNames []string
		for _, h := range handoffs {
			handoffNames = append(handoffNames, h.Name())
		}
		var errorMsg string
		if len(handoffNames) > 0 {
			errorMsg = fmt.Sprintf("Agent %s cannot hand off to %s: target agent not in handoffs list. Available handoff agent IDs are: %s", ca, params.Agent, strings.Join(handoffNames, ", "))
		} else {
			errorMsg = fmt.Sprintf("Agent %s cannot hand off to %s: target agent not in handoffs list. This agent has no handoff agents configured.", ca, params.Agent)
		}
		return tools.ResultError(errorMsg), nil
	}

	next, err := r.team.Agent(params.Agent)
	if err != nil {
		return nil, err
	}

	r.currentAgent = next.Name()
	handoffMessage := "The agent " + ca + " handed off the conversation to you. " +
		"Your available handoff agents and tools are specified in the system messages that follow. " +
		"Only use those capabilities - do not attempt to use tools or hand off to agents that you see " +
		"in the conversation history from previous agents, as those were available to different agents " +
		"with different capabilities. Look at the conversation history for context, but only use the " +
		"handoff agents and tools that are listed in your system messages below. " +
		"Complete your part of the task and hand off to the next appropriate agent in your workflow " +
		"(if any are available to you), or respond directly to the user if you are the final agent."
	return tools.ResultSuccess(handoffMessage), nil
}

// Summarize generates a summary for the session based on the conversation history.
// The additionalPrompt parameter allows users to provide additional instructions
// for the summarization (e.g., "focus on code changes" or "include action items").
func (r *LocalRuntime) Summarize(ctx context.Context, sess *session.Session, additionalPrompt string, events chan Event) {
	r.sessionCompactor.Compact(ctx, sess, additionalPrompt, events, r.currentAgent)
}

// setElicitationEventsChannel sets the current events channel for elicitation requests
func (r *LocalRuntime) setElicitationEventsChannel(events chan Event) {
	r.elicitationEventsChannelMux.Lock()
	defer r.elicitationEventsChannelMux.Unlock()
	r.elicitationEventsChannel = events
}

// clearElicitationEventsChannel clears the current events channel
func (r *LocalRuntime) clearElicitationEventsChannel() {
	r.elicitationEventsChannelMux.Lock()
	defer r.elicitationEventsChannelMux.Unlock()
	r.elicitationEventsChannel = nil
}

// elicitationHandler creates an elicitation handler that can be used by MCP clients
// This handler propagates elicitation requests to the runtime's client via events
func (r *LocalRuntime) elicitationHandler(ctx context.Context, req *mcp.ElicitParams) (tools.ElicitationResult, error) {
	slog.Debug("Elicitation request received from MCP server", "message", req.Message)

	// Get the current events channel
	r.elicitationEventsChannelMux.RLock()
	eventsChannel := r.elicitationEventsChannel
	r.elicitationEventsChannelMux.RUnlock()

	if eventsChannel == nil {
		return tools.ElicitationResult{}, fmt.Errorf("no events channel available for elicitation")
	}

	slog.Debug("Sending elicitation request event to client", "message", req.Message, "mode", req.Mode, "requested_schema", req.RequestedSchema, "url", req.URL)
	slog.Debug("Elicitation request meta", "meta", req.Meta)

	// Send elicitation request event to the runtime's client
	eventsChannel <- ElicitationRequest(req.Message, req.Mode, req.RequestedSchema, req.URL, req.ElicitationID, req.Meta, r.currentAgent)

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
