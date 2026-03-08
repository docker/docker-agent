package runtime

import (
	"cmp"
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
	agenttool "github.com/docker/cagent/pkg/tools/builtin/agent"
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

// RAGInitializer is implemented by runtimes that support background RAG initialization.
// Local runtimes use this to start indexing early; remote runtimes typically do not.
type RAGInitializer interface {
	StartBackgroundRAGInit(ctx context.Context, sendEvent func(Event))
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
	ragInitialized              atomic.Bool
	sessionCompactor            *sessionCompactor
	sessionStore                session.Store
	workingDir                  string   // Working directory for hooks execution
	env                         []string // Environment variables for hooks execution
	modelSwitcherCfg            *ModelSwitcherConfig

	// fallbackCooldowns tracks per-agent cooldown state for sticky fallback behavior
	fallbackCooldowns    map[string]*fallbackCooldownState
	fallbackCooldownsMux sync.RWMutex

	currentAgentMu sync.RWMutex

	// onToolsChanged is called when an MCP toolset reports a tool list change.
	onToolsChanged func(Event)

	bgAgents *agenttool.Handler
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

					agentName := r.CurrentAgentName()
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
						sendEvent(NewTokenUsageEvent("", agentName, &Usage{
							InputTokens:   ragEvent.TotalTokens,
							ContextLength: ragEvent.TotalTokens,
							Cost:          ragEvent.Cost,
						}))
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
		if !send(ToolsetInfo(totalTools, !isLast, r.CurrentAgentName())) {
			return
		}
	}

	// Emit final state (not loading)
	send(ToolsetInfo(totalTools, false, r.CurrentAgentName()))
}

// registerDefaultTools registers the runtime-managed tool handlers.
// The tool definitions themselves come from the agent's toolsets; this only
// maps tool names to the runtime handler functions that implement them.
func (r *LocalRuntime) registerDefaultTools() {
	r.toolMap[builtin.ToolNameTransferTask] = r.handleTaskTransfer
	r.toolMap[builtin.ToolNameHandoff] = r.handleHandoff
	r.toolMap[builtin.ToolNameChangeModel] = r.handleChangeModel
	r.toolMap[builtin.ToolNameRevertModel] = r.handleRevertModel
	r.toolMap[agenttool.ToolNameRunBackgroundAgent] = r.handleRunBackgroundAgent
	r.toolMap[agenttool.ToolNameListBackgroundAgents] = r.handleListBackgroundAgents
	r.toolMap[agenttool.ToolNameViewBackgroundAgent] = r.handleViewBackgroundAgent
	r.toolMap[agenttool.ToolNameStopBackgroundAgent] = r.handleStopBackgroundAgent
}

func (r *LocalRuntime) handleRunBackgroundAgent(ctx context.Context, sess *session.Session, tc tools.ToolCall, _ chan Event) (*tools.ToolCallResult, error) {
	return r.bgAgents.HandleRun(ctx, sess, tc)
}

func (r *LocalRuntime) handleListBackgroundAgents(ctx context.Context, sess *session.Session, tc tools.ToolCall, _ chan Event) (*tools.ToolCallResult, error) {
	return r.bgAgents.HandleList(ctx, sess, tc)
}

func (r *LocalRuntime) handleViewBackgroundAgent(ctx context.Context, sess *session.Session, tc tools.ToolCall, _ chan Event) (*tools.ToolCallResult, error) {
	return r.bgAgents.HandleView(ctx, sess, tc)
}

func (r *LocalRuntime) handleStopBackgroundAgent(ctx context.Context, sess *session.Session, tc tools.ToolCall, _ chan Event) (*tools.ToolCallResult, error) {
	return r.bgAgents.HandleStop(ctx, sess, tc)
}

func (r *LocalRuntime) finalizeEventChannel(ctx context.Context, sess *session.Session, events chan Event) {
	// Clear the elicitation events channel before closing the events channel
	// to prevent a send-on-closed-channel panic in elicitationHandler.
	// Skip for background sessions (ToolsApproved=true) — they never set the
	// channel, so clearing it would null out the parent session's channel.
	if !sess.ToolsApproved {
		r.clearElicitationEventsChannel()
	}

	defer close(events)

	events <- StreamStopped(sess.ID, r.CurrentAgentName())

	r.executeOnUserInputHooks(ctx, sess.ID, "stream stopped")

	telemetry.RecordSessionEnd(ctx)
}

// RunStream starts the agent's interaction loop and returns a channel of events
func (r *LocalRuntime) RunStream(ctx context.Context, sess *session.Session) <-chan Event {
	slog.Debug("Starting runtime stream", "agent", r.CurrentAgentName(), "session_id", sess.ID)
	events := make(chan Event, 128)

	go func() {
		telemetry.RecordSessionStart(ctx, r.CurrentAgentName(), sess.ID)

		ctx, sessionSpan := r.startSpan(ctx, "runtime.session", trace.WithAttributes(
			attribute.String("agent", r.CurrentAgentName()),
			attribute.String("session.id", sess.ID),
		))
		defer sessionSpan.End()

		// Set the events channel for elicitation requests.
		// Skip for background sessions (ToolsApproved=true): they have all tools
		// pre-approved and will never trigger elicitation prompts. Setting the
		// channel would overwrite the parent session's channel; clearing it at
		// teardown would break any pending MCP auth flow in the parent.
		if !sess.ToolsApproved {
			r.setElicitationEventsChannel(events)
		}

		// Resolve the agent for this session. When AgentName is set on the
		// session (e.g., background agent tasks), use it directly to avoid
		// racing on the shared currentAgent field.
		var a *agent.Agent
		if sess.AgentName != "" {
			a, _ = r.team.Agent(sess.AgentName)
		}
		if a == nil {
			a = r.CurrentAgent()
		}

		// Emit agent information for sidebar display
		// Use getEffectiveModelID to account for active fallback cooldowns
		events <- AgentInfo(a.Name(), r.getEffectiveModelID(a), a.Description(), a.WelcomeMessage())

		// Emit team information
		events <- TeamInfo(r.agentDetailsFromTeam(), r.CurrentAgentName())

		// Initialize RAG and forward events
		r.InitializeRAG(ctx, events)

		r.emitAgentWarnings(a, events)
		r.configureToolsetHandlers(a, events)

		agentTools, err := r.getTools(ctx, a, sessionSpan, events)
		if err != nil {
			events <- Error(fmt.Sprintf("failed to get tools: %v", err))
			return
		}

		events <- ToolsetInfo(len(agentTools), false, r.CurrentAgentName())

		messages := sess.GetMessages(a)
		if sess.SendUserMessage {
			lastMsg := messages[len(messages)-1]
			events <- UserMessage(lastMsg.Content, sess.ID, lastMsg.MultiContent, len(sess.Messages)-1)
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

			// Emit updated tool count. After a ToolListChanged MCP notification
			// the cache is invalidated, so getTools above re-fetches from the
			// server and may return a different count.
			events <- ToolsetInfo(len(agentTools), false, r.CurrentAgentName())

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
				contextLength := sess.InputTokens + sess.OutputTokens
				if contextLength > int64(float64(contextLimit)*0.9) {
					r.Summarize(ctx, sess, "", events)
				}
			}

			messages := sess.GetMessages(a)
			slog.Debug("Retrieved messages for processing", "agent", a.Name(), "message_count", len(messages))

			// Strip image content from messages if the model doesn't support image input.
			// This prevents API errors when conversation history contains images (e.g. from
			// tool results or user attachments) but the current model is text-only.
			if m != nil && len(m.Modalities.Input) > 0 && !slices.Contains(m.Modalities.Input, "image") {
				messages = stripImageContent(messages)
			}

			// Try primary model with fallback chain if configured
			res, usedModel, err := r.tryModelWithFallback(streamCtx, a, model, messages, agentTools, sess, m, events)
			if err != nil {
				// Treat context cancellation as a graceful stop
				if errors.Is(err, context.Canceled) {
					slog.Debug("Model stream canceled by context", "agent", a.Name(), "session_id", sess.ID)
					streamSpan.End()
					return
				}

				// Auto-recovery: if the error is a context overflow and
				// session compaction is enabled, compact the conversation
				// and retry the request instead of surfacing raw errors.
				var ctxOverflow *ContextOverflowError
				if errors.As(err, &ctxOverflow) && r.sessionCompaction {
					slog.Warn("Context window overflow detected, attempting auto-compaction",
						"agent", a.Name(),
						"session_id", sess.ID,
						"input_tokens", sess.InputTokens,
						"output_tokens", sess.OutputTokens,
						"context_limit", contextLimit,
					)
					events <- Warning(
						"The conversation has exceeded the model's context window. Automatically compacting the conversation history...",
						r.CurrentAgentName(),
					)
					r.Summarize(ctx, sess, "", events)

					// After compaction, loop back to retry with the
					// compacted context. The next iteration will re-fetch
					// messages from the (now compacted) session.
					streamSpan.End()
					continue
				}

				streamSpan.RecordError(err)
				streamSpan.SetStatus(codes.Error, "error handling stream")
				slog.Error("All models failed", "agent", a.Name(), "error", err)
				// Track error in telemetry
				telemetry.RecordError(ctx, err.Error())
				events <- Error(formatModelError(err))
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
				messageModel := cmp.Or(res.ActualModel, modelID)

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
					if res.RateLimit != nil {
						msgUsage.RateLimit = *res.RateLimit
					}
				}

				addAgentMessage(sess, a, &assistantMessage, events)
				slog.Debug("Added assistant message to session", "agent", a.Name(), "total_messages", len(sess.GetAllMessages()))
			} else {
				slog.Debug("Skipping empty assistant message (no content and no tool calls)", "agent", a.Name())
			}

			usage := SessionUsage(sess, contextLimit)
			usage.LastMessage = msgUsage
			events <- NewTokenUsageEvent(sess.ID, r.CurrentAgentName(), usage)

			// Record the message count before tool calls so we can
			// measure how much content was added by tool results.
			messageCountBeforeTools := len(sess.GetAllMessages())

			r.processToolCalls(ctx, sess, res.Calls, agentTools, events)

			if res.Stopped {
				slog.Debug("Conversation stopped", "agent", a.Name())
				break
			}

			// Root-cause fix for stale token counts (issue #1750):
			// After tool calls, sess.InputTokens still reflects the
			// *previous* API response and doesn't account for the
			// (potentially large) tool results just added. Estimate
			// the additional tokens and compact proactively to prevent
			// the oversized request from ever being sent.
			if m != nil && r.sessionCompaction && contextLimit > 0 {
				newMessages := sess.GetAllMessages()[messageCountBeforeTools:]
				var addedTokens int64
				for _, msg := range newMessages {
					addedTokens += estimateMessageTokens(&msg.Message)
				}

				estimatedTotal := sess.InputTokens + sess.OutputTokens + addedTokens
				if estimatedTotal > int64(float64(contextLimit)*0.9) {
					slog.Info("Proactive compaction: tool results pushed estimated context past 90%% threshold",
						"agent", a.Name(),
						"input_tokens", sess.InputTokens,
						"output_tokens", sess.OutputTokens,
						"added_estimated_tokens", addedTokens,
						"estimated_total", estimatedTotal,
						"context_limit", contextLimit,
					)
					r.Summarize(ctx, sess, "", events)
				}
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
			func() { events <- Authorization(tools.ElicitationActionAccept, r.CurrentAgentName()) },
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
	send(Warning(formatToolWarning(a, warnings), r.CurrentAgentName()))
}

func (r *LocalRuntime) emitAgentWarnings(a *agent.Agent, events chan Event) {
	warnings := a.DrainWarnings()
	if len(warnings) == 0 {
		return
	}

	slog.Warn("Tool setup partially failed; continuing", "agent", a.Name(), "warnings", warnings)

	if events != nil {
		events <- Warning(formatToolWarning(a, warnings), r.CurrentAgentName())
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
	var messageUsage *chat.Usage
	var messageRateLimit *chat.RateLimit

	toolCallIndex := make(map[string]int)   // toolCallID -> index in toolCalls slice
	emittedPartial := make(map[string]bool) // toolCallID -> whether we've emitted a partial event
	toolDefMap := make(map[string]tools.Tool, len(agentTools))
	for _, t := range agentTools {
		toolDefMap[t.Name] = t
	}

	// recordUsage persists the final token counts and emits telemetry exactly
	// once per stream, after we have the most accurate usage snapshot.
	usageRecorded := false
	recordUsage := func() {
		if usageRecorded || messageUsage == nil {
			return
		}
		usageRecorded = true

		sess.InputTokens = messageUsage.InputTokens + messageUsage.CachedInputTokens + messageUsage.CacheWriteTokens
		sess.OutputTokens = messageUsage.OutputTokens

		modelName := "unknown"
		if m != nil {
			modelName = m.Name
		}
		telemetry.RecordTokenUsage(ctx, modelName, sess.InputTokens, sess.OutputTokens, sess.TotalCost())
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
			// Always keep the latest usage snapshot; some providers (e.g.
			// Gemini) emit updated usage on every chunk with cumulative
			// token counts, so the last value is the most accurate.
			messageUsage = response.Usage
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
		if actualModel == "" && response.Model != "" {
			actualModel = response.Model
		}

		if choice.FinishReason == chat.FinishReasonStop || choice.FinishReason == chat.FinishReasonLength {
			recordUsage()
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

	recordUsage()

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

	for _, toolCall := range calls {
		callCtx, callSpan := r.startSpan(ctx, "runtime.tool.call", trace.WithAttributes(
			attribute.String("tool.name", toolCall.Function.Name),
			attribute.String("tool.type", string(toolCall.Type)),
			attribute.String("agent", a.Name()),
			attribute.String("session.id", sess.ID),
			attribute.String("tool.call_id", toolCall.ID),
		))

		slog.Debug("Processing tool call", "agent", a.Name(), "tool", toolCall.Function.Name, "session_id", sess.ID)

		// Resolve the tool: it must be in the agent's tool set to be callable.
		// After a handoff the model may hallucinate tools it saw in the
		// conversation history from a previous agent; rejecting unknown
		// tools with an error response lets it self-correct.
		tool, available := agentToolMap[toolCall.Function.Name]
		if !available {
			slog.Warn("Tool call for unavailable tool", "agent", a.Name(), "tool", toolCall.Function.Name, "session_id", sess.ID)
			errTool := tools.Tool{Name: toolCall.Function.Name}
			r.addToolErrorResponse(ctx, sess, toolCall, errTool, events, a, fmt.Sprintf("Tool '%s' is not available. You can only use the tools provided to you.", toolCall.Function.Name))
			callSpan.SetStatus(codes.Error, "tool not available")
			callSpan.End()
			continue
		}

		// Pick the handler: runtime-managed tools (transfer_task, handoff)
		// have dedicated handlers; everything else goes through the toolset.
		var runTool func()
		if handler, exists := r.toolMap[toolCall.Function.Name]; exists {
			runTool = func() { r.runAgentTool(callCtx, handler, sess, toolCall, tool, events, a) }
		} else {
			runTool = func() { r.runTool(callCtx, tool, toolCall, events, sess, a) }
		}

		// Execute tool with approval check
		canceled := r.executeWithApproval(callCtx, sess, toolCall, tool, events, a, runTool)
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
//  1. sess.ToolsApproved (--yolo flag) - auto-approve everything, takes precedence
//  2. Session-level permissions (if configured) - pattern-based Allow/Ask/Deny rules
//  3. Team-level permissions config - checked second
//  4. Read-only hint - auto-approve
//  5. Default: ask for user confirmation
func (r *LocalRuntime) executeWithApproval(
	ctx context.Context,
	sess *session.Session,
	toolCall tools.ToolCall,
	tool tools.Tool,
	events chan Event,
	a *agent.Agent,
	runTool func(),
) (canceled bool) {
	toolName := toolCall.Function.Name

	// --yolo flag takes absolute precedence: auto-approve everything.
	if sess.ToolsApproved {
		slog.Debug("Tool auto-approved by --yolo flag", "tool", toolName, "session_id", sess.ID)
		runTool()
		return false
	}

	// Parse tool arguments once for permission matching
	var toolArgs map[string]any
	if toolCall.Function.Arguments != "" {
		if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &toolArgs); err != nil {
			slog.Debug("Failed to parse tool arguments for permission check", "tool", toolName, "error", err)
			// Continue with nil args - will only match tool name patterns
		}
	}

	// Collect permission checkers in priority order (session first, then team)
	checkers := r.permissionCheckers(sess)

	for _, pc := range checkers {
		switch pc.checker.CheckWithArgs(toolName, toolArgs) {
		case permissions.Deny:
			slog.Debug("Tool denied by permissions", "tool", toolName, "source", pc.source, "session_id", sess.ID)
			r.addToolErrorResponse(ctx, sess, toolCall, tool, events, a, fmt.Sprintf("Tool '%s' is denied by %s.", toolName, pc.source))
			return false
		case permissions.Allow:
			slog.Debug("Tool auto-approved by permissions", "tool", toolName, "source", pc.source, "session_id", sess.ID)
			runTool()
			return false
		case permissions.ForceAsk:
			slog.Debug("Tool requires confirmation (ask pattern)", "tool", toolName, "source", pc.source, "session_id", sess.ID)
			return r.askUserForConfirmation(ctx, sess, toolCall, tool, events, a, runTool)
		case permissions.Ask:
			// No explicit match at this level; fall through to next checker
		}
	}

	// No permission rule matched. Auto-approve if the tool is read-only.
	if tool.Annotations.ReadOnlyHint {
		runTool()
		return false
	}

	// Default: ask the user for confirmation
	return r.askUserForConfirmation(ctx, sess, toolCall, tool, events, a, runTool)
}

// permissionChecker pairs a checker with a human-readable source label.
type permissionChecker struct {
	checker *permissions.Checker
	source  string
}

// permissionCheckers returns the ordered list of permission checkers to evaluate.
func (r *LocalRuntime) permissionCheckers(sess *session.Session) []permissionChecker {
	var checkers []permissionChecker
	if sess.Permissions != nil {
		checkers = append(checkers, permissionChecker{
			checker: permissions.NewChecker(&latest.PermissionsConfig{
				Allow: sess.Permissions.Allow,
				Ask:   sess.Permissions.Ask,
				Deny:  sess.Permissions.Deny,
			}),
			source: "session permissions",
		})
	}
	if tc := r.team.Permissions(); tc != nil {
		checkers = append(checkers, permissionChecker{
			checker: tc,
			source:  "permissions configuration",
		})
	}
	return checkers
}

// askUserForConfirmation sends a confirmation event and waits for user response.
// This is only called when --yolo is not active and no permission rule auto-approved the tool.
func (r *LocalRuntime) askUserForConfirmation(
	ctx context.Context,
	sess *session.Session,
	toolCall tools.ToolCall,
	tool tools.Tool,
	events chan Event,
	a *agent.Agent,
	runTool func(),
) (canceled bool) {
	toolName := toolCall.Function.Name
	slog.Debug("Tools not approved, waiting for resume", "tool", toolName, "session_id", sess.ID)
	events <- ToolCallConfirmation(toolCall, tool, a.Name())

	r.executeOnUserInputHooks(ctx, sess.ID, "tool confirmation")

	select {
	case req := <-r.resumeChan:
		switch req.Type {
		case ResumeTypeApprove:
			slog.Debug("Resume signal received, approving tool", "tool", toolName, "session_id", sess.ID)
			runTool()
		case ResumeTypeApproveSession:
			slog.Debug("Resume signal received, approving session", "tool", toolName, "session_id", sess.ID)
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
			slog.Debug("Resume signal received, rejecting tool", "tool", toolName, "session_id", sess.ID, "reason", req.Reason)
			rejectMsg := "The user rejected the tool call."
			if strings.TrimSpace(req.Reason) != "" {
				rejectMsg += " Reason: " + strings.TrimSpace(req.Reason)
			}
			r.addToolErrorResponse(ctx, sess, toolCall, tool, events, a, rejectMsg)
		}
		return false
	case <-ctx.Done():
		slog.Debug("Context cancelled while waiting for resume", "tool", toolName, "session_id", sess.ID)
		r.addToolErrorResponse(ctx, sess, toolCall, tool, events, a, "The tool call was canceled by the user.")
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
) {
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
		IsError:    res.IsError,
		CreatedAt:  time.Now().Format(time.RFC3339),
	}

	// If the tool result contains images, attach them as MultiContent
	if len(res.Images) > 0 {
		multiContent := []chat.MessagePart{
			{
				Type: chat.MessagePartTypeText,
				Text: content,
			},
		}
		for _, img := range res.Images {
			multiContent = append(multiContent, chat.MessagePart{
				Type: chat.MessagePartTypeImageURL,
				ImageURL: &chat.MessageImageURL{
					URL:    "data:" + img.MimeType + ";base64," + img.Data,
					Detail: chat.ImageURLDetailAuto,
				},
			})
		}
		toolResponseMsg.MultiContent = multiContent
	}

	addAgentMessage(sess, a, &toolResponseMsg, events)
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
		default:
			if result.SystemMessage != "" {
				events <- Warning(result.SystemMessage, a.Name())
			}
			if result.ModifiedInput != nil {
				updated, merr := json.Marshal(result.ModifiedInput)
				if merr != nil {
					slog.Warn("Failed to marshal modified tool input from hook", "tool", toolCall.Function.Name, "error", merr)
				} else {
					slog.Debug("Pre-tool hook modified tool input", "tool", toolCall.Function.Name)
					toolCall.Function.Arguments = string(updated)
				}
			}
		}
	}

	r.executeToolWithHandler(ctx, toolCall, tool, events, sess, a, "runtime.tool.handler",
		func(ctx context.Context) (*tools.ToolCallResult, time.Duration, error) {
			res, err := tool.Handler(ctx, toolCall)
			return res, 0, err
		})

	// Execute post-tool hooks if configured
	if hooksExec != nil && hooksExec.HasPostToolUseHooks() {
		toolInput := parseToolInput(toolCall.Function.Arguments)
		input := &hooks.Input{
			SessionID:    sess.ID,
			Cwd:          r.workingDir,
			ToolName:     toolCall.Function.Name,
			ToolUseID:    toolCall.ID,
			ToolInput:    toolInput,
			ToolResponse: nil, // TODO: pass actual tool response if needed
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
		IsError:    true,
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

// CurrentAgentSubAgentNames implements agenttool.Runner.
func (r *LocalRuntime) CurrentAgentSubAgentNames() []string {
	a := r.CurrentAgent()
	if a == nil {
		return nil
	}
	var names []string
	for _, sa := range a.SubAgents() {
		names = append(names, sa.Name())
	}
	return names
}

// RunAgent implements agenttool.Runner. It starts a sub-agent synchronously and
// blocks until completion or cancellation.
func (r *LocalRuntime) RunAgent(ctx context.Context, params agenttool.RunParams) *agenttool.RunResult {
	child, err := r.team.Agent(params.AgentName)
	if err != nil {
		return &agenttool.RunResult{ErrMsg: fmt.Sprintf("agent %q not found: %s", params.AgentName, err)}
	}

	systemMsg := "You are a member of a team of agents. Your goal is to complete the following task:"
	systemMsg += fmt.Sprintf("\n\n<task>\n%s\n</task>", params.Task)
	if params.ExpectedOutput != "" {
		systemMsg += fmt.Sprintf("\n\n<expected_output>\n%s\n</expected_output>", params.ExpectedOutput)
	}

	sess := params.ParentSession

	// Background tasks run with tools pre-approved because there is no user present
	// to respond to interactive approval prompts during async execution. This is a
	// deliberate design trade-off: the user implicitly authorises all tool calls made
	// by the sub-agent when they approve run_background_agent. Callers should be aware
	// that prompt injection in the sub-agent's context could exploit this gate-bypass.
	//
	// TODO: propagate the parent session's per-tool permission rules once the runtime
	// supports per-session permission scoping rather than a single shared ToolsApproved flag.
	s := session.New(
		session.WithSystemMessage(systemMsg),
		session.WithImplicitUserMessage("Please proceed."),
		session.WithMaxIterations(child.MaxIterations()),
		session.WithTitle("Background agent task"),
		session.WithToolsApproved(true),
		session.WithThinking(sess.Thinking),
		session.WithSendUserMessage(false),
		session.WithParentID(sess.ID),
		session.WithAgentName(params.AgentName),
	)

	var errMsg string
	events := r.RunStream(ctx, s)
	for event := range events {
		if ctx.Err() != nil {
			break
		}
		if choice, ok := event.(*AgentChoiceEvent); ok && choice.Content != "" {
			if params.OnContent != nil {
				params.OnContent(choice.Content)
			}
		}
		if errEvt, ok := event.(*ErrorEvent); ok {
			errMsg = errEvt.Error
			break
		}
	}
	// Drain remaining events so the RunStream goroutine can complete
	// and close the channel without blocking on a full buffer.
	for range events {
	}

	if errMsg != "" {
		return &agenttool.RunResult{ErrMsg: errMsg}
	}

	result := s.GetLastAssistantMessageContent()
	sess.AddSubSession(s)
	return &agenttool.RunResult{Result: result}
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

	ca := r.CurrentAgentName()

	// Emit agent switching start event
	evts <- AgentSwitching(true, ca, params.Agent)

	r.setCurrentAgent(params.Agent)
	defer func() {
		r.setCurrentAgent(ca)

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

	return r.runSubSession(ctx, sess, s, span, evts, a.Name())
}

// runSubSession runs a child session within the parent, forwarding events and
// propagating state (tool approvals, thinking) back to the parent when done.
func (r *LocalRuntime) runSubSession(ctx context.Context, parent, child *session.Session, span trace.Span, evts chan Event, agentName string) (*tools.ToolCallResult, error) {
	for event := range r.RunStream(ctx, child) {
		evts <- event
		if errEvent, ok := event.(*ErrorEvent); ok {
			span.RecordError(fmt.Errorf("%s", errEvent.Error))
			span.SetStatus(codes.Error, "sub-session error")
			return nil, fmt.Errorf("%s", errEvent.Error)
		}
	}

	parent.ToolsApproved = child.ToolsApproved
	parent.Thinking = child.Thinking

	parent.AddSubSession(child)
	evts <- SubSessionCompleted(parent.ID, child, agentName)

	span.SetStatus(codes.Ok, "sub-session completed")
	return tools.ResultSuccess(child.GetLastAssistantMessageContent()), nil
}

func (r *LocalRuntime) handleHandoff(_ context.Context, _ *session.Session, toolCall tools.ToolCall, _ chan Event) (*tools.ToolCallResult, error) {
	var params builtin.HandoffArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	ca := r.CurrentAgentName()
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

	r.setCurrentAgent(next.Name())
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

// findModelPickerTool returns the ModelPickerTool from the current agent's
// toolsets, or nil if the agent has no model_picker configured.
func (r *LocalRuntime) findModelPickerTool() *builtin.ModelPickerTool {
	currentName := r.CurrentAgentName()
	a, err := r.team.Agent(currentName)
	if err != nil {
		return nil
	}
	for _, ts := range a.ToolSets() {
		if mpt, ok := tools.As[*builtin.ModelPickerTool](ts); ok {
			return mpt
		}
	}
	return nil
}

// handleChangeModel handles the change_model tool call by switching the current agent's model.
func (r *LocalRuntime) handleChangeModel(ctx context.Context, _ *session.Session, toolCall tools.ToolCall, events chan Event) (*tools.ToolCallResult, error) {
	var params builtin.ChangeModelArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if params.Model == "" {
		return tools.ResultError("model parameter is required"), nil
	}

	// Validate the requested model against the allowed list
	mpt := r.findModelPickerTool()
	if mpt == nil {
		return tools.ResultError("model_picker is not configured for this agent"), nil
	}
	allowed := mpt.AllowedModels()
	if !slices.Contains(allowed, params.Model) {
		return tools.ResultError(fmt.Sprintf(
			"model %q is not in the allowed list. Available models: %s",
			params.Model, strings.Join(allowed, ", "),
		)), nil
	}

	return r.setModelAndEmitInfo(ctx, params.Model, events)
}

// handleRevertModel handles the revert_model tool call by reverting the current agent to its default model.
func (r *LocalRuntime) handleRevertModel(ctx context.Context, _ *session.Session, _ tools.ToolCall, events chan Event) (*tools.ToolCallResult, error) {
	return r.setModelAndEmitInfo(ctx, "", events)
}

// setModelAndEmitInfo sets the model for the current agent and emits an updated
// AgentInfo event so the UI reflects the change. An empty modelRef reverts to
// the agent's default model.
func (r *LocalRuntime) setModelAndEmitInfo(ctx context.Context, modelRef string, events chan Event) (*tools.ToolCallResult, error) {
	currentName := r.CurrentAgentName()
	if err := r.SetAgentModel(ctx, currentName, modelRef); err != nil {
		return tools.ResultError(fmt.Sprintf("failed to set model: %v", err)), nil
	}

	if a, err := r.team.Agent(currentName); err == nil {
		events <- AgentInfo(a.Name(), r.getEffectiveModelID(a), a.Description(), a.WelcomeMessage())
	} else {
		slog.Warn("Failed to retrieve agent after model change; UI may not reflect the update", "agent", currentName, "error", err)
	}

	if modelRef == "" {
		slog.Info("Model reverted via model_picker tool", "agent", currentName)
		return tools.ResultSuccess("Model reverted to the agent's default model"), nil
	}
	slog.Info("Model changed via model_picker tool", "agent", currentName, "model", modelRef)
	return tools.ResultSuccess(fmt.Sprintf("Model changed to %s", modelRef)), nil
}

// Summarize generates a summary for the session based on the conversation history.
// The additionalPrompt parameter allows users to provide additional instructions
// for the summarization (e.g., "focus on code changes" or "include action items").
func (r *LocalRuntime) Summarize(ctx context.Context, sess *session.Session, additionalPrompt string, events chan Event) {
	r.sessionCompactor.Compact(ctx, sess, additionalPrompt, events, r.CurrentAgentName())

	// Emit a TokenUsageEvent so the sidebar immediately reflects the
	// compaction: tokens drop to the summary size, context % drops, and
	// cost increases by the summary generation cost.
	a := r.CurrentAgent()
	modelID := r.getEffectiveModelID(a)
	var contextLimit int64
	if m, err := r.modelsStore.GetModel(ctx, modelID); err == nil && m != nil {
		contextLimit = int64(m.Limit.Context)
	}
	events <- NewTokenUsageEvent(sess.ID, r.CurrentAgentName(), SessionUsage(sess, contextLimit))
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

	// Hold the read lock while sending to the channel to prevent a race
	// with clearElicitationEventsChannel / close(events).
	r.elicitationEventsChannelMux.RLock()
	eventsChannel := r.elicitationEventsChannel
	if eventsChannel == nil {
		r.elicitationEventsChannelMux.RUnlock()
		return tools.ElicitationResult{}, fmt.Errorf("no events channel available for elicitation")
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

// stripImageContent returns a copy of messages with all image-related content
// removed. This is used when the target model doesn't support image input to
// prevent API errors. Text content is preserved; image parts in MultiContent
// are filtered out, and file attachments with image MIME types are dropped.
func stripImageContent(messages []chat.Message) []chat.Message {
	result := make([]chat.Message, len(messages))
	for i, msg := range messages {
		result[i] = msg

		if len(msg.MultiContent) == 0 {
			continue
		}

		var filtered []chat.MessagePart
		for _, part := range msg.MultiContent {
			switch part.Type {
			case chat.MessagePartTypeImageURL:
				// Drop image URL parts entirely.
				continue
			case chat.MessagePartTypeFile:
				// Drop file parts that are images.
				if part.File != nil && chat.IsImageMimeType(part.File.MimeType) {
					continue
				}
			}
			filtered = append(filtered, part)
		}

		if len(filtered) != len(msg.MultiContent) {
			result[i].MultiContent = filtered
			slog.Debug("Stripped image content from message", "role", msg.Role, "original_parts", len(msg.MultiContent), "remaining_parts", len(filtered))
		}
	}
	return result
}

// charsPerToken is the average number of characters per token used for
// estimation. A value of 4 is a widely-used heuristic for English text;
// it slightly overestimates token counts for code/JSON (which is ~3.5),
// making compaction trigger earlier — the safe direction.
const charsPerToken = 4

// estimateMessageTokens returns a rough token-count estimate for a single
// chat message based on its text length. This is intentionally conservative
// (overestimates) so that proactive compaction fires before we hit the limit.
// The estimate includes the message content, multi-content text parts, and
// a small overhead per message for role/metadata tokens.
func estimateMessageTokens(msg *chat.Message) int64 {
	var chars int

	// Primary text content.
	chars += len(msg.Content)

	// Multi-content parts (e.g., tool results with image descriptions).
	for _, part := range msg.MultiContent {
		chars += len(part.Text)
	}

	// Reasoning / thinking content.
	chars += len(msg.ReasoningContent)

	// Tool call arguments (they count toward input tokens on the next turn).
	for _, tc := range msg.ToolCalls {
		chars += len(tc.Function.Arguments)
		chars += len(tc.Function.Name)
	}

	// Per-message overhead: role, ToolCallID, delimiters, etc.
	// Models typically use 3-7 tokens for message framing.
	const perMessageOverhead = 5

	if chars == 0 {
		return perMessageOverhead
	}

	return int64(chars/charsPerToken) + perMessageOverhead
}

// formatModelError produces a user-friendly error message from a model error.
// Raw HTTP errors with request IDs, JSON payloads, and API URLs are replaced
// with actionable guidance. Context overflow errors receive a dedicated
// message; other errors are cleaned up to remove noise while preserving the
// essential failure reason.
func formatModelError(err error) string {
	if err == nil {
		return ""
	}

	// Context overflow gets a dedicated, actionable message.
	var ctxOverflow *ContextOverflowError
	if errors.As(err, &ctxOverflow) {
		return "The conversation has exceeded the model's context window and automatic compaction is not enabled. " +
			"Try running /compact to reduce the conversation size, or start a new session."
	}

	return err.Error()
}
