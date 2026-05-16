package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	"github.com/docker/docker-agent/pkg/tools/builtin/skills"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

// ToolHandlerFunc is a function type for handling tool calls
type ToolHandlerFunc func(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, events EventSink) (*tools.ToolCallResult, error)

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
	// CurrentAgentToolsetStatuses returns lifecycle status for each toolset of
	// the active agent (name, kind, state, last error, restart count).
	// Used by the /tools dialog. Best-effort: toolsets that don't expose
	// state appear with State == StateStopped/Ready as appropriate.
	CurrentAgentToolsetStatuses() []tools.ToolsetStatus

	// RestartToolset finds the named toolset on the active agent and asks
	// its supervisor to drop the current session and reconnect. Returns
	// an error if no toolset matches name or the toolset does not
	// implement tools.Restartable.
	RestartToolset(ctx context.Context, name string) error
	// EmitStartupInfo emits initial agent, team, and toolset information for immediate display.
	// When sess is non-nil and contains token data, a TokenUsageEvent is also emitted
	// so the UI can display context usage percentage on session restore.
	EmitStartupInfo(ctx context.Context, sess *session.Session, events EventSink)
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
	Summarize(ctx context.Context, sess *session.Session, additionalPrompt string, events EventSink)

	// PermissionsInfo returns the team-level permission patterns (allow/ask/deny).
	// Returns nil if no permissions are configured.
	PermissionsInfo() *PermissionsInfo

	// CurrentAgentSkillsToolset returns the skills toolset for the current agent, or nil if skills are not enabled.
	CurrentAgentSkillsToolset() *skills.Toolset

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

	// Steer enqueues a user message for urgent mid-turn injection into the
	// running agent loop. Returns an error if the queue is full or steering
	// is not available.
	Steer(msg QueuedMessage) error
	// FollowUp enqueues a message for end-of-turn processing. Each follow-up
	// gets a full undivided agent turn. Returns an error if the queue is full.
	FollowUp(msg QueuedMessage) error

	// SetAgentModel sets a model override for the named agent.
	// modelRef can be:
	//   - "" (empty) to clear the override and use the agent's default model
	//   - A model name from the config (e.g., "my_fast_model")
	//   - An inline spec (e.g., "openai/gpt-4o")
	// Returns [ErrUnsupported] for runtimes that don't expose model
	// switching (e.g. remote runtimes, where the server owns the choice).
	SetAgentModel(ctx context.Context, agentName, modelRef string) error

	// AvailableModels returns the models the user can pick from in the
	// /model picker. Returns nil for runtimes that don't expose model
	// switching; see SupportsModelSwitching for a cheap pre-check.
	AvailableModels(ctx context.Context) []ModelChoice

	// SupportsModelSwitching reports whether SetAgentModel and
	// AvailableModels are wired for this runtime. Use it to gate UI
	// affordances (e.g. show /model in the menu) without paying the
	// cost of AvailableModels.
	SupportsModelSwitching() bool

	// OnToolsChanged registers a handler invoked outside of any RunStream
	// when a toolset reports a tool list change (e.g. after an MCP
	// ToolListChanged notification). Runtimes that don't emit such events
	// can implement this as a no-op.
	OnToolsChanged(handler func(Event))

	// QueueStatus returns the current depth and capacity of message queues
	QueueStatus() QueueStatus

	// TogglePause toggles whether the run loop is paused at iteration
	// boundaries. Returns the new state (true if now paused). Returns
	// [ErrUnsupported] for runtimes that don't expose pause control.
	TogglePause(ctx context.Context) (paused bool, err error)

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
	GetModel(ctx context.Context, id modelsdev.ID) (*modelsdev.Model, error)
	GetDatabase(ctx context.Context) (*modelsdev.Database, error)
}

// LocalRuntime manages the execution of agents
type LocalRuntime struct {
	toolMap              map[string]ToolHandlerFunc
	team                 *team.Team
	agents               *agentRouter
	resumeChan           chan ResumeRequest
	tracer               trace.Tracer
	modelsStore          ModelStore
	sessionCompaction    bool
	managedOAuth         bool
	nonInteractive       bool
	startupInfoEmitted   bool                   // Track if startup info has been emitted to avoid unnecessary duplication
	elicitationRequestCh chan ElicitationResult // Channel for receiving elicitation responses
	elicitation          elicitationBridge      // Owns the per-stream events channel for outbound elicitation requests
	sessionStore         session.Store
	workingDir           string   // Working directory for hooks execution
	env                  []string // Environment variables for hooks execution
	modelSwitcherCfg     *ModelSwitcherConfig

	// hooksRegistry is the runtime-private hooks.Registry used to build
	// every Executor. It carries the runtime-owned builtin hooks
	// (add_date, add_environment_info) registered once during
	// NewLocalRuntime, so they're available to every agent without
	// touching any process-wide state.
	hooksRegistry *hooks.Registry

	// autoInjectors run on every per-agent hook config during
	// [buildHooksExecutors] so embedders can plug in builtins (today
	// snapshot via [builtins.SnapshotController]) without the runtime
	// hard-coding their wiring. Set via [WithAutoInjector].
	autoInjectors []builtins.AutoInjector

	// hooksExecByAgent holds the per-agent [hooks.Executor], keyed by
	// agent name. Built once in [NewLocalRuntime.buildHooksExecutors]
	// after team and runtime config are finalized; agents with no hooks
	// have no entry, so [hooksExec] returns nil for them. Read-only after
	// construction, so no locking is needed.
	hooksExecByAgent map[string]*hooks.Executor

	// transforms is the runtime's [MessageTransform] chain, applied to
	// every LLM call in registration order. Populated by
	// [NewLocalRuntime] (for the runtime-shipped strip transform) and by
	// [WithMessageTransform] (for embedder-supplied transforms).
	// Read-only after construction.
	transforms []registeredTransform

	fallback *fallbackExecutor

	// observers receive every event the runtime produces, in
	// registration order. Built up via [WithEventObserver] during
	// construction; read-only afterwards. Always contains at least one
	// entry: the auto-registered [PersistenceObserver] for the
	// configured session store. See [EventObserver] for the contract.
	observers []EventObserver

	// fallback owns the model-fallback chain (primary + configured
	// fallbacks), per-attempt retry/backoff for transient errors, and
	// the per-agent "sticky" cooldown after a fallback succeeds. It
	// holds the cooldownManager and rate-limit retry flag so that state
	// stays out of LocalRuntime. See [fallbackExecutor].

	// steerQueue stores urgent mid-turn messages. The agent loop drains
	// ALL pending messages after tool execution, before the stop check.
	steerQueue MessageQueue

	// followUpQueue stores end-of-turn messages. The agent loop pops
	// exactly ONE message after the model stops and stop-hooks have run.
	followUpQueue MessageQueue

	// onToolsChanged is called when an MCP toolset reports a tool list change.
	onToolsChanged func(Event)

	bgAgents *agenttool.Handler

	// now is the runtime's clock. Defaults to time.Now and can be replaced
	// in tests via WithClock to make timestamps and cooldown windows
	// deterministic. Every time-dependent call inside the runtime (message
	// CreatedAt, fallback cooldown windows, tool-call latency) goes through
	// this hook so a single fake clock controls them all.
	now func() time.Time

	// telemetry receives the runtime's observability events (session
	// start/end, tool calls, token usage, errors). Defaults to
	// defaultTelemetry which forwards to pkg/telemetry. Tests can inject
	// a recorder via WithTelemetry to assert the lifecycle without
	// standing up an OTel pipeline.
	telemetry Telemetry

	// maxOverflowCompactions caps the number of consecutive context-
	// overflow auto-compactions the run loop attempts before surfacing the
	// error. Defaults to defaultMaxOverflowCompactions; tests use
	// WithMaxOverflowCompactions to exercise both the "compaction
	// succeeded" and "compaction exhausted" branches.
	maxOverflowCompactions int

	// pauseMu guards pauseCh.
	pauseMu sync.Mutex
	// pauseCh is non-nil and open while /pause has paused the run loop;
	// nil otherwise. See TogglePause and waitIfPaused.
	pauseCh chan struct{}
}

type Opt func(*LocalRuntime)

func WithCurrentAgent(agentName string) Opt {
	return func(r *LocalRuntime) {
		r.agents.Set(agentName)
	}
}

func WithManagedOAuth(managed bool) Opt {
	return func(r *LocalRuntime) {
		r.managedOAuth = managed
	}
}

// WithNonInteractive marks the runtime as headless (e.g., MCP serve mode).
// When set, blocking operations like elicitation requests are automatically
// declined instead of waiting for user interaction that will never come.
//
// Note: this complements session.WithNonInteractive, which controls per-session
// loop behavior (e.g., auto-stop on max iterations). Both should be set for
// fully headless operation - the runtime flag prevents elicitation hangs, while
// the session flag adjusts iteration behavior. In MCP serve mode, both are set
// by the server code in pkg/mcp/server.go.
func WithNonInteractive(nonInteractive bool) Opt {
	return func(r *LocalRuntime) {
		r.nonInteractive = nonInteractive
	}
}

// WithTracer sets a custom OpenTelemetry tracer; if not provided, tracing is disabled (no-op).
func WithTracer(t trace.Tracer) Opt {
	return func(r *LocalRuntime) {
		r.tracer = t
	}
}

// WithSteerQueue sets a custom MessageQueue for mid-turn message injection.
// If not provided, an in-memory buffered queue is used.
func WithSteerQueue(q MessageQueue) Opt {
	return func(r *LocalRuntime) {
		r.steerQueue = q
	}
}

// WithFollowUpQueue sets a custom MessageQueue for end-of-turn follow-up
// messages. If not provided, an in-memory buffered queue is used.
func WithFollowUpQueue(q MessageQueue) Opt {
	return func(r *LocalRuntime) {
		r.followUpQueue = q
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

// WithClock replaces the runtime's clock. Defaults to time.Now. Tests that
// need deterministic timestamps (assistant message CreatedAt, fallback
// cooldown windows, tool-call latency) can pass a fake clock so assertions
// don't depend on wall-clock advancement.
func WithClock(now func() time.Time) Opt {
	return func(r *LocalRuntime) {
		if now != nil {
			r.now = now
		}
	}
}

// WithTelemetry replaces the runtime's Telemetry sink. Defaults to a
// pass-through to the package-level pkg/telemetry helpers. Tests pass a
// recorder to assert that the runtime emitted the expected lifecycle
// events without setting up an OTel client.
func WithTelemetry(t Telemetry) Opt {
	return func(r *LocalRuntime) {
		if t != nil {
			r.telemetry = t
		}
	}
}

// WithMaxOverflowCompactions overrides how many consecutive context-overflow
// auto-compactions the run loop is allowed to attempt before surfacing the
// error. Defaults to defaultMaxOverflowCompactions (1).
//
// Tests use this to exercise both branches of the overflow-recovery code
// path: pass 0 to verify the failure surface immediately; pass a higher
// number to verify the loop bounds compaction attempts. Negative values
// are clamped to 0.
func WithMaxOverflowCompactions(n int) Opt {
	return func(r *LocalRuntime) {
		if n < 0 {
			n = 0
		}
		r.maxOverflowCompactions = n
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
		r.fallback.retryOnRateLimit = true
	}
}

// WithAutoInjector adds an [builtins.AutoInjector] that augments every
// per-agent hook configuration during executor build. The canonical
// use case is the snapshot controller returned by
// [builtins.RegisterSnapshot]: pass the same controller to the App via
// app.WithSnapshotController so /undo and friends drive the same
// instance that captures the checkpoints.
//
// Multiple calls accumulate; injectors run in registration order.
func WithAutoInjector(inj builtins.AutoInjector) Opt {
	return func(r *LocalRuntime) {
		if inj != nil {
			r.autoInjectors = append(r.autoInjectors, inj)
		}
	}
}

// WithHooksRegistry plugs a pre-populated [hooks.Registry] into the
// runtime instead of letting it allocate a fresh one. Embedders use
// this to pre-register builtins they own (today snapshot, tomorrow
// any custom builtin) so the auto-injection chain set up by
// [WithAutoInjector] resolves against the same registry.
//
// The runtime continues to register its own stateless and
// closure-bound builtins (add_date, max_iterations, cache_response,
// unload, ...) on top of the supplied registry, so the embedder only
// needs to install entries that the runtime can't construct itself.
func WithHooksRegistry(reg *hooks.Registry) Opt {
	return func(r *LocalRuntime) {
		if reg != nil {
			r.hooksRegistry = reg
		}
	}
}

// New creates a runtime ready to drive an agent loop. It is a thin
// alias for [NewLocalRuntime] returning the [Runtime] interface, kept
// for source compatibility with callers written before persistence
// became an [EventObserver]. Persistence is auto-registered against
// the configured (or default in-memory) session store; pass
// [WithSessionStore] to override and [WithEventObserver] to layer
// additional observers (telemetry, audit, ...).
func New(agents *team.Team, opts ...Opt) (Runtime, error) {
	return NewLocalRuntime(agents, opts...)
}

// NewLocalRuntime creates a new LocalRuntime without the persistence wrapper.
// This is useful for testing or when persistence is handled externally.
func NewLocalRuntime(agents *team.Team, opts ...Opt) (*LocalRuntime, error) {
	defaultAgent, err := agents.DefaultAgent()
	if err != nil {
		return nil, err
	}

	r := &LocalRuntime{
		toolMap:                make(map[string]ToolHandlerFunc),
		team:                   agents,
		agents:                 newAgentRouter(agents, defaultAgent.Name()),
		resumeChan:             make(chan ResumeRequest),
		elicitationRequestCh:   make(chan ElicitationResult),
		steerQueue:             NewInMemoryMessageQueue(defaultSteerQueueCapacity),
		followUpQueue:          NewInMemoryMessageQueue(defaultFollowUpQueueCapacity),
		sessionCompaction:      true,
		managedOAuth:           true,
		sessionStore:           session.NewInMemorySessionStore(),
		fallback:               newFallbackExecutor(),
		now:                    time.Now,
		telemetry:              defaultTelemetry{},
		maxOverflowCompactions: defaultMaxOverflowCompactions,
	}
	r.bgAgents = agenttool.NewHandler(r)

	// stripUnsupportedModalitiesTransform captures the runtime closure to
	// resolve the agent from Input.AgentName, so it lives here rather
	// than as a stateless builtin in pkg/hooks/builtins. It drops image
	// content for text-only models on every model call.
	//
	// redact_secrets used to live here as a sibling [MessageTransform];
	// it now ships entirely as a [hooks.BuiltinFunc] in
	// pkg/hooks/builtins/redact_secrets.go and is wired into all three
	// of pre_tool_use, before_llm_call, and tool_response_transform via
	// [builtins.ApplyAgentDefaults] (or a user's hooks YAML directly),
	// so the rewrite path is the same for every leak vector and there
	// is no flag-only code path to keep in sync.
	r.transforms = append(r.transforms,
		registeredTransform{
			name: BuiltinStripUnsupportedModalities,
			fn:   r.stripUnsupportedModalitiesTransform,
		},
	)

	for _, opt := range opts {
		opt(r)
	}

	// Set up the hooks registry. Use the embedder-supplied registry
	// (via [WithHooksRegistry]) when present so any builtins the
	// embedder pre-registered — typically the snapshot builtin from
	// [builtins.RegisterSnapshot] — are visible to the runtime, then
	// register the runtime-owned builtins on top.
	if r.hooksRegistry == nil {
		r.hooksRegistry = hooks.NewRegistry()
	}
	if err := builtins.Register(r.hooksRegistry); err != nil {
		return nil, fmt.Errorf("register builtin hooks: %w", err)
	}
	registerModelHook(r.hooksRegistry)

	// cache_response is registered here (not in pkg/hooks/builtins)
	// because it needs to capture the runtime to resolve the agent
	// referenced by Input.AgentName. The other builtins are stateless
	// and can stay as package-level functions registered via
	// [builtins.Register] above.
	if err := r.hooksRegistry.RegisterBuiltin(BuiltinCacheResponse, r.cacheResponseBuiltin); err != nil {
		return nil, fmt.Errorf("register %q builtin: %w", BuiltinCacheResponse, err)
	}

	// Build the cooldown manager and wire the fallback executor's
	// runtime-bound dependencies after opts so they pick up the final
	// clock and telemetry sink ([WithClock] / [WithTelemetry]).
	r.fallback.cooldowns = newCooldownManager(r.now)
	r.fallback.telemetry = r.telemetry

	// Default the runtime's working directory to the process CWD when no
	// caller supplied one. This matches the session's default and ensures
	// builtin hooks that look up files (add_prompt_files) can find them
	// without the embedder having to remember to call WithWorkingDir.
	if r.workingDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			r.workingDir = cwd
		}
	}

	if r.modelsStore == nil {
		r.modelsStore = &lazyModelStore{}
	}

	// Validate that the current agent exists and has a model
	// (the router's current name might have been changed by WithCurrentAgent)
	defaultAgent, err = r.team.Agent(r.agents.Name())
	if err != nil {
		return nil, err
	}

	if defaultAgent.Model(context.TODO()) == nil {
		return nil, fmt.Errorf("agent %s has no valid model", defaultAgent.Name())
	}

	// Register runtime-managed tool handlers once during construction.
	// This avoids concurrent map writes when multiple goroutines call
	// RunStream on the same runtime (e.g. background agent sessions).
	r.registerDefaultTools()

	// Pre-build per-agent hook executors now that workingDir, env and
	// the team are finalized. Read-only afterwards.
	r.buildHooksExecutors()

	// Auto-register the stock persistence observer against the
	// (possibly user-supplied) session store. It runs first in the
	// observer chain so any user-supplied observers see the same view
	// of the session that future RunStream calls and store reads will.
	if obs := newPersistenceObserver(r.sessionStore); obs != nil {
		r.observers = append([]EventObserver{obs}, r.observers...)
	}

	slog.Debug("Creating new runtime", "agent", r.agents.Name(), "available_agents", agents.Size())

	return r, nil
}

func (r *LocalRuntime) CurrentAgentName() string {
	return r.agents.Name()
}

func (r *LocalRuntime) setCurrentAgent(name string) {
	r.agents.Set(name)
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
	return r.agents.SetValidated(agentName)
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

// CurrentAgentToolsetStatuses returns one ToolsetStatus per toolset of the
// active agent. The list is in declaration order. Toolsets that wrap
// another (StartableToolSet, Multiplexer) are unwrapped so the inner
// supervisor's state is visible.
func (r *LocalRuntime) CurrentAgentToolsetStatuses() []tools.ToolsetStatus {
	a := r.CurrentAgent()
	if a == nil {
		return nil
	}
	toolSets := a.ToolSets()
	statuses := make([]tools.ToolsetStatus, 0, len(toolSets))
	for _, ts := range toolSets {
		statuses = append(statuses, toolsetStatusFor(ts))
	}
	return statuses
}

// RestartToolset locates the named toolset on the active agent and
// asks it to restart in place. The supervisor closes the current
// session and reconnects; this method blocks until the new session
// is Ready, ctx is cancelled, or the underlying supervisor's
// timeout elapses.
//
// Returns an error when:
//   - no toolset matches name (matching uses the same logic as the
//     /tools dialog: the toolset's Name() if any, otherwise its
//     description),
//   - the toolset is not supervisor-backed (no Restartable capability),
//   - the supervisor itself returned an error (timeout, classified
//     transport failure, etc.).
func (r *LocalRuntime) RestartToolset(ctx context.Context, name string) error {
	a := r.CurrentAgent()
	if a == nil {
		return errors.New("no active agent")
	}
	for _, ts := range a.ToolSets() {
		if nameFor(ts, tools.DescribeToolSet(ts)) != name {
			continue
		}
		restartable, ok := tools.As[tools.Restartable](ts)
		if !ok {
			return fmt.Errorf("toolset %q does not support restart", name)
		}
		return restartable.Restart(ctx)
	}
	return fmt.Errorf("toolset %q not found", name)
}

// toolsetStatusFor builds a ToolsetStatus for ts. tools.As walks the
// wrapper chain so Statable/Describer can live anywhere in the stack.
func toolsetStatusFor(ts tools.ToolSet) tools.ToolsetStatus {
	status := tools.ToolsetStatus{
		Description: tools.DescribeToolSet(ts),
	}
	if kinder, ok := tools.As[tools.Kinder](ts); ok {
		status.Kind = kinder.Kind()
	}
	if statable, ok := tools.As[tools.Statable](ts); ok {
		info := statable.State()
		status.State = info.State
		status.LastError = info.LastError
		status.RestartCount = info.RestartCount
	} else {
		// Toolsets without a supervisor are considered ready by default;
		// the StartableToolSet wrapper would have surfaced an error
		// earlier if Start failed.
		status.State = lifecycleStateForUnsupervised(ts)
	}
	status.Name = nameFor(ts, status.Description)
	return status
}

func lifecycleStateForUnsupervised(ts tools.ToolSet) lifecycle.State {
	if s, ok := ts.(*tools.StartableToolSet); ok && !s.IsStarted() {
		return lifecycle.StateStopped
	}
	return lifecycle.StateReady
}

// nameFor picks a stable, user-visible name for a toolset. We look for
// any inner toolset that implements tools.Named (walked via tools.As so
// wrappers like StartableToolSet are transparent). The registry adds a
// WithName wrapper for every built-in toolset so this is reachable for
// almost every toolset; fallback uses the description ("mcp(stdio cmd=...)"),
// which is still better than the Go type name.
func nameFor(ts tools.ToolSet, fallback string) string {
	if name := tools.GetName(ts); name != "" {
		return name
	}
	return fallback
}

// CurrentMCPPrompts returns the available MCP prompts from all active MCP toolsets
// for the current agent. It discovers prompts by calling ListPrompts on each MCP toolset
// and aggregates the results into a map keyed by prompt name.
func (r *LocalRuntime) CurrentMCPPrompts(ctx context.Context) map[string]mcptools.PromptInfo {
	prompts := make(map[string]mcptools.PromptInfo)

	// Get the current agent to access its toolsets
	currentAgent := r.CurrentAgent()
	if currentAgent == nil {
		slog.WarnContext(ctx, "No current agent available for MCP prompt discovery")
		return prompts
	}

	// Iterate through all toolsets of the current agent
	for _, toolset := range currentAgent.ToolSets() {
		if mcpToolset, ok := tools.As[*mcptools.Toolset](toolset); ok {
			slog.DebugContext(ctx, "Found MCP toolset", "toolset", mcpToolset)
			// Discover prompts from this MCP toolset
			mcpPrompts := r.discoverMCPPrompts(ctx, mcpToolset)

			// Merge prompts into the result map
			// If there are name conflicts, the later toolset's prompt will override
			maps.Copy(prompts, mcpPrompts)
		} else {
			slog.DebugContext(ctx, "Toolset is not an MCP toolset", "type", fmt.Sprintf("%T", toolset))
		}
	}

	slog.DebugContext(ctx, "Discovered MCP prompts", "agent", currentAgent.Name(), "prompt_count", len(prompts))
	return prompts
}

// discoverMCPPrompts queries an MCP toolset for available prompts and converts them
// to PromptInfo structures. This method handles the MCP protocol communication
// and gracefully handles any errors during prompt discovery.
func (r *LocalRuntime) discoverMCPPrompts(ctx context.Context, toolset *mcptools.Toolset) map[string]mcptools.PromptInfo {
	mcpPrompts, err := toolset.ListPrompts(ctx)
	if err != nil {
		slog.WarnContext(ctx, "Failed to list MCP prompts from toolset", "error", err)
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
		slog.DebugContext(ctx, "Discovered MCP prompt", "name", mcpPrompt.Name, "args_count", len(promptInfo.Arguments))
	}

	return prompts
}

// CurrentAgent returns the current agent
func (r *LocalRuntime) CurrentAgent() *agent.Agent {
	return r.agents.Current()
}

// resolveSessionAgent returns the agent for the given session. Delegates to
// agentRouter.ResolveSession; kept on LocalRuntime for the existing callsites
// in loop.go and elsewhere.
func (r *LocalRuntime) resolveSessionAgent(sess *session.Session) *agent.Agent {
	return r.agents.ResolveSession(sess)
}

// CurrentAgentSkillsToolset returns the skills toolset for the current agent, or nil if not enabled.
func (r *LocalRuntime) CurrentAgentSkillsToolset() *skills.Toolset {
	a := r.CurrentAgent()
	if a == nil {
		return nil
	}
	for _, ts := range a.ToolSets() {
		if st, ok := tools.As[*skills.Toolset](ts); ok {
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
	// Title-gen setup happens before any session ctx exists; the resulting
	// generator carries its own ctx when actually invoked. context.TODO is
	// the right marker here.
	model := a.Model(context.TODO())
	if model == nil {
		return nil
	}
	return sessiontitle.New(model, a.FallbackModels()...)
}

// getAgentModelID returns the model ID for an agent. The zero ID is
// returned when no model is configured.
func getAgentModelID(a *agent.Agent) modelsdev.ID {
	if model := a.Model(context.TODO()); model != nil {
		return model.ID()
	}
	return modelsdev.ID{}
}

// getEffectiveModelID returns the currently active model ID for an agent, accounting
// for any active fallback cooldown. During a cooldown period, this returns the fallback
// model ID instead of the configured primary model, so the UI reflects the actual model in use.
func (r *LocalRuntime) getEffectiveModelID(a *agent.Agent) modelsdev.ID {
	cooldownState := r.fallback.cooldowns.Get(a.Name())
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
		cooldownState := r.fallback.cooldowns.Get(info.Name)
		if cooldownState != nil {
			// Get the agent to access fallback models
			if a, err := r.team.Agent(info.Name); err == nil && a != nil {
				fallbacks := a.FallbackModels()
				if cooldownState.fallbackIndex >= 0 && cooldownState.fallbackIndex < len(fallbacks) {
					fb := fallbacks[cooldownState.fallbackIndex].ID()
					providerName = fb.Provider
					modelName = fb.Model
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
	ctx, cancel := context.WithTimeout(context.Background(), toolsChangedTimeout)
	defer cancel()
	a := r.CurrentAgent()
	agentTools, err := a.StartedTools(ctx)
	if err != nil {
		return
	}
	r.onToolsChanged(ToolsetInfo(len(agentTools), false, r.CurrentAgentName()))
}

// EmitStartupInfo emits initial agent, team, and toolset information for immediate sidebar display.
// When sess is non-nil and contains token data, a TokenUsageEvent is also emitted so that the
// sidebar can display context usage percentage on session restore.
func (r *LocalRuntime) EmitStartupInfo(ctx context.Context, sess *session.Session, events EventSink) {
	// Prevent duplicate emissions
	if r.startupInfoEmitted {
		return
	}
	r.startupInfoEmitted = true

	a := r.CurrentAgent()

	// Helper to send events with context check
	send := func(event Event) bool {
		if ctx.Err() != nil {
			return false
		}
		events.Emit(event)
		return true
	}

	// Emit agent and team information immediately for fast sidebar display
	// Use getEffectiveModelID to account for active fallback cooldowns
	modelID := r.getEffectiveModelID(a)
	if !send(AgentInfo(a.Name(), modelID.String(), a.Description(), a.WelcomeMessage())) {
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

		// Reconstruct LastMessage from the parent session's last assistant
		// message so that FinishReason (and other per-message fields) are
		// available on session restore.  We intentionally iterate
		// sess.Messages (not GetAllMessages) so the result reflects the
		// parent agent's state: this event carries the parent session_id,
		// and sub-agents emit their own token_usage events with their own
		// session_id during live streaming.
		for i := range slices.Backward(sess.Messages) {
			item := &sess.Messages[i]
			if !item.IsMessage() || item.Message.Message.Role != chat.MessageRoleAssistant {
				continue
			}
			msg := &item.Message.Message
			lm := &MessageUsage{
				Model:        msg.Model,
				Cost:         msg.Cost,
				FinishReason: msg.FinishReason,
			}
			if msg.Usage != nil {
				lm.Usage = *msg.Usage
			}
			usage.LastMessage = lm
			break
		}

		send(NewTokenUsageEvent(sess.ID, r.CurrentAgentName(), usage))
	}

	// Tool loading can be slow (MCP servers need to start). Mark the
	// context as non-interactive so toolsets that require user-driven
	// flows (e.g. an OAuth elicitation for a remote MCP server) fail
	// fast with a recognisable error rather than blocking on a dialog
	// the TUI is not yet ready to render. The actual prompt happens on
	// the first RunStream when the user is interacting with the agent.
	nonInteractiveCtx := mcptools.WithoutInteractivePrompts(ctx)
	r.emitToolsProgressively(nonInteractiveCtx, a, send)

	// Flush any agent warnings: load-time warnings recorded at agent
	// construction (WithLoadTimeWarnings) and per-toolset warnings recorded
	// during startup above (e.g. a remote MCP server returning 4xx during
	// initialize). Surfacing them as WarningEvents lets the TUI show a
	// persistent notice with the actual server-side explanation — otherwise
	// the user only sees the toolset disappear from the sidebar with no clue
	// as to why.
	r.emitAgentWarnings(a, events)
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
					desc := tools.DescribeToolSet(startable.ToolSet)
					// IsAuthorizationRequired must be checked BEFORE
					// ShouldReportFailure: this is the first — expected —
					// failure of a deferred-OAuth toolset, and consuming the
					// failure-reported flag here would suppress the *real*
					// failure (e.g. server 4xx on the eventual interactive
					// retry) that the user actually needs to see.
					if mcptools.IsAuthorizationRequired(err) {
						// The toolset just needs an OAuth approval that we
						// deliberately deferred until the user is interacting
						// with the agent. The dialog will appear naturally on
						// the first RunStream — no need to pre-announce it.
						slog.DebugContext(ctx, "Toolset deferred until first message", "agent", a.Name(), "toolset", desc, "reason", err)
						continue
					}
					// Route real failures through the agent's warning
					// channel so the TUI surfaces a persistent,
					// user-visible notice that includes the actual
					// server-side cause (threaded through by
					// remoteMCPClient.Initialize). Use the same
					// once-per-streak guard as ensureToolSetsAreStarted
					// so a failing toolset doesn't flood the UI with a
					// new warning every time the agent is restarted.
					if !startable.ShouldReportFailure() {
						slog.DebugContext(ctx, "Toolset still unavailable; skipping", "agent", a.Name(), "toolset", desc, "error", err)
						continue
					}
					slog.WarnContext(ctx, "Toolset start failed; skipping", "agent", a.Name(), "toolset", desc, "error", err)
					a.AddToolWarning(fmt.Sprintf("%s start failed: %v", desc, err))
					continue
				}
			}
		}

		// Get tools from this toolset
		ts, err := toolset.Tools(ctx)
		if err != nil {
			slog.WarnContext(ctx, "Failed to get tools from toolset", "agent", a.Name(), "error", err)
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

// Steer enqueues a user message for urgent mid-turn injection into the
// running agent loop. The message will be picked up after the current batch
// of tool calls finishes but before the loop checks whether to stop.
func (r *LocalRuntime) Steer(msg QueuedMessage) error {
	if !r.steerQueue.Enqueue(context.Background(), msg) {
		return errors.New("steer queue full")
	}
	return nil
}

// FollowUp enqueues a message to be processed after the current agent turn
// finishes. Unlike Steer, follow-ups are popped one at a time and each gets
// a full undivided agent turn.
func (r *LocalRuntime) FollowUp(msg QueuedMessage) error {
	if !r.followUpQueue.Enqueue(context.Background(), msg) {
		return errors.New("follow-up queue full")
	}
	return nil
}

func (r *LocalRuntime) QueueStatus() QueueStatus {
	status := QueueStatus{}
	if steerQ, ok := r.steerQueue.(*inMemoryMessageQueue); ok {
		status.SteerDepth = len(steerQ.ch)
		status.SteerCapacity = cap(steerQ.ch)
	}
	if followupQ, ok := r.followUpQueue.(*inMemoryMessageQueue); ok {
		status.FollowupDepth = len(followupQ.ch)
		status.FollowupCapacity = cap(followupQ.ch)
	}
	return status
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
//
// Summarize is the public entry point used by user-driven /compact actions; it
// reports compactionReasonManual to BeforeCompaction / AfterCompaction hooks
// and "manual" to PreCompact hooks.
// Internal callers (proactive threshold, overflow recovery) use
// [LocalRuntime.compactWithReason] directly to forward a more specific reason.
func (r *LocalRuntime) Summarize(ctx context.Context, sess *session.Session, additionalPrompt string, events EventSink) {
	r.compactWithReason(ctx, sess, additionalPrompt, compactionReasonManual, events)
}

// compactWithReason runs a session compaction with the supplied reason and
// emits a TokenUsageEvent so the UI immediately reflects the new context
// pressure.
//
// reason is reported to BeforeCompaction / AfterCompaction hooks as
// CompactionReason. Use [compactionReasonThreshold] for proactive
// 90%-of-context triggers, [compactionReasonOverflow] for post-overflow
// auto-recovery, [compactionReasonToolOverflow] for tool-result-driven
// 90% triggers, or [compactionReasonManual] for user-invoked compactions.
//
// PreCompact hooks fire first via the legacy [hooks.Input.Source] field
// ("auto" / "tool_overflow" / "overflow" / "manual"); they may cancel the
// compaction or contribute additional steering text. BeforeCompaction
// hooks then fire inside [LocalRuntime.doCompact] with [Input.CompactionReason]
// set to the canonical reason; they may veto or supply a custom summary.
func (r *LocalRuntime) compactWithReason(ctx context.Context, sess *session.Session, additionalPrompt, reason string, events EventSink) {
	// Stamp the session ID on ctx so the compaction LLM call carries
	// `X-Cagent-Session-Id` to the gateway. Manual compaction
	// (via `Summarize` from the App) bypasses `runStreamLoop`'s seed;
	// internal callers (proactive threshold, overflow recovery) already
	// run with a stamped ctx, but re-stamping is idempotent.
	ctx = httpclient.ContextWithSessionID(ctx, sess.ID)
	a := r.resolveSessionAgent(sess)

	source := preCompactSourceFor(reason)
	skip, msg, extraPrompt := r.executePreCompactHooks(ctx, sess, a, source, events)
	if skip {
		slog.WarnContext(ctx, "pre_compact hook signalled skip",
			"agent", a.Name(), "session_id", sess.ID, "source", source, "reason", msg)
		if msg != "" {
			events.Emit(Warning(msg, a.Name()))
		}
		return
	}
	additionalPrompt = joinPrompts(additionalPrompt, extraPrompt)

	r.doCompact(ctx, sess, a, additionalPrompt, reason, events)

	// Emit a TokenUsageEvent so the sidebar immediately reflects the
	// compaction: tokens drop to the summary size, context % drops, and
	// cost increases by the summary generation cost.
	modelID := r.getEffectiveModelID(a)
	var contextLimit int64
	if m, err := r.modelsStore.GetModel(ctx, modelID); err == nil && m != nil {
		contextLimit = int64(m.Limit.Context)
	}
	events.Emit(NewTokenUsageEvent(sess.ID, a.Name(), SessionUsage(sess, contextLimit)))
}

// preCompactSourceFor maps the canonical compaction reason
// ([compactionReasonThreshold] / [compactionReasonOverflow] /
// [compactionReasonManual]) onto the [hooks.Input.Source] string
// surfaced by the pre_compact hook ("auto" / "overflow" / "manual").
// Unknown reasons fall through unchanged so future, more specific
// reasons (e.g. "tool_overflow") can be forwarded verbatim without
// touching this map.
func preCompactSourceFor(reason string) string {
	switch reason {
	case compactionReasonThreshold:
		return "auto"
	case compactionReasonOverflow:
		return "overflow"
	case compactionReasonManual:
		return "manual"
	default:
		return reason
	}
}

// joinPrompts concatenates two non-empty prompt fragments with a blank
// line, returning whichever is non-empty when the other isn't. Used by
// compactWithReason to splice pre_compact's additional_context into
// the caller's additionalPrompt without having to special-case empty
// strings at the callsite.
func joinPrompts(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "\n\n" + b
	}
}
