package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/evaluator"
	"github.com/docker/docker-agent/pkg/harness"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/dmr/dmrmodels"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime/artifacts"
	"github.com/docker/docker-agent/pkg/runtime/inspection"
	"github.com/docker/docker-agent/pkg/runtime/modelcatalog"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	"github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

// ToolHandlerFunc handles a runtime-managed tool call. rt is the dispatcher's
// per-call handle, used by handlers that need to talk back to the in-flight
// call (streaming output, asking the user to approve an action).
type ToolHandlerFunc func(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, events EventSink, rt tools.Runtime) (*tools.ToolCallResult, error)

// Runtime defines the contract for runtime execution
type Runtime interface {
	// CurrentAgentInfo returns information about the currently active agent
	CurrentAgentInfo(ctx context.Context) CurrentAgentInfo
	// CurrentAgentName returns the name of the currently active agent
	CurrentAgentName(ctx context.Context) string
	// SetCurrentAgent sets the currently active agent for subsequent user messages
	SetCurrentAgent(ctx context.Context, agentName string) error
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
	// EmitAgentInfo emits up-to-date agent and team info (model, thinking
	// level, description) without re-running heavier startup work such as
	// tool discovery. Used to refresh the UI after a lightweight change like
	// cycling the thinking level, avoiding the tool-count flicker that
	// re-emitting full startup info would cause.
	EmitAgentInfo(ctx context.Context, events EventSink)
	// ResetStartupInfo resets the startup info emission flag, allowing re-emission
	ResetStartupInfo()
	// RunStream starts the agent's interaction loop and returns a channel of events.
	// Consumers must drain it to closure before releasing turn ownership, even
	// after cancellation. Cancel the supplied context before draining on errors.
	RunStream(ctx context.Context, sess *session.Session) <-chan Event
	// Run starts the agent's interaction loop and returns the final messages
	Run(ctx context.Context, sess *session.Session) ([]session.Message, error)
	// Resume allows resuming execution after user confirmation.
	// The ResumeRequest carries the decision type and an optional reason (for rejections).
	Resume(ctx context.Context, req ResumeRequest)
	// ResumeElicitation sends an elicitation response back to a waiting
	// elicitation request. elicitationID correlates the response with a
	// specific concurrent request (see ElicitationRequestEvent.ElicitationID);
	// omit it (or pass "") to fall back to resolving the sole pending
	// request, for backward compatibility with older clients. The parameter
	// is variadic — rather than a required 4th positional argument — purely
	// so pre-#3584 callers of this interface method keep compiling unchanged;
	// implementations should treat more than one value as a caller error and
	// use only the first.
	ResumeElicitation(_ context.Context, action tools.ElicitationAction, content map[string]any, elicitationID ...string) error
	// SessionStore returns the session store for browsing/loading past sessions.
	// Returns nil if no persistent session store is configured.
	SessionStore() session.Store

	// Summarize generates a summary for the session
	Summarize(ctx context.Context, sess *session.Session, additionalPrompt string, events EventSink)

	// PermissionsInfo returns the team-level permission patterns (allow/ask/deny).
	// Returns nil if no permissions are configured.
	PermissionsInfo() *PermissionsInfo

	// ReadSkillContent loads a skill for sess without executing embedded commands.
	ReadSkillContent(ctx context.Context, sess *session.Session, name string) (string, error)

	// CurrentAgentSkillsToolset returns the skills toolset for the current agent, or nil if skills are not enabled.
	CurrentAgentSkillsToolset() *skills.ToolSet

	// RunSkillFork executes a `context: fork` skill as an isolated
	// sub-session of sess. Used by the run_skill tool and the App's
	// slash-command path. Returns a ToolCallResult carrying the sub-agent's
	// last assistant message, or a user-facing error reason; err is non-nil
	// only for unexpected runtime failures.
	RunSkillFork(ctx context.Context, sess *session.Session, args skills.RunSkillArgs, events EventSink) (*tools.ToolCallResult, error)

	// CurrentMCPPrompts returns MCP prompts available from the current agent's toolsets.
	// Returns an empty map if no MCP prompts are available.
	CurrentMCPPrompts(ctx context.Context) map[string]tools.PromptInfo

	// ExecuteMCPPrompt executes a named MCP prompt with the given arguments.
	ExecuteMCPPrompt(ctx context.Context, promptName string, arguments map[string]string) (string, error)

	// UpdateSessionTitle persists a new title for the current session.
	UpdateSessionTitle(ctx context.Context, sess *session.Session, title string) error

	// TitleGenerator returns a generator for automatic session titles, or nil
	// if the runtime does not support local title generation (e.g. remote
	// runtimes) or no configured model is a usable title candidate.
	TitleGenerator(ctx context.Context) *sessiontitle.Generator

	// Steer enqueues a user message for urgent mid-turn injection into the
	// running agent loop. Returns an error if the queue is full or steering
	// is not available.
	Steer(ctx context.Context, msg QueuedMessage) error
	// FollowUp enqueues a message for end-of-turn processing. Each follow-up
	// gets a full undivided agent turn. Returns an error if the queue is full.
	FollowUp(ctx context.Context, msg QueuedMessage) error

	// SetAgentModel sets a model override for the named agent.
	// modelRef can be:
	//   - "" (empty) to clear the override and use the agent's default model
	//   - A model name from the config (e.g., "my_fast_model")
	//   - An inline spec (e.g., "openai/gpt-4o")
	// Returns [ErrUnsupported] for runtimes that don't expose model
	// switching (e.g. remote runtimes, where the server owns the choice).
	SetAgentModel(ctx context.Context, agentName, modelRef string) error

	// CycleAgentThinkingLevel advances the named agent's thinking-effort
	// level to the next value in its provider's cycle (wrapping around),
	// applies it as a runtime model override, and returns the newly
	// selected level. Returns [ErrUnsupported] for runtimes that can't
	// switch models (e.g. remote runtimes) or for models that don't
	// support thinking-effort selection.
	CycleAgentThinkingLevel(ctx context.Context, agentName string) (effort.Level, error)

	// SetAgentThinkingLevel sets the named agent's thinking-effort level to
	// the requested value, applies it as a runtime model override, and
	// returns the applied level. The level must be one the current model
	// supports. Returns [ErrUnsupported] for runtimes that can't switch
	// models (e.g. remote runtimes) or for models that don't support
	// thinking-effort selection.
	SetAgentThinkingLevel(ctx context.Context, agentName string, level effort.Level) (effort.Level, error)

	// AvailableModels returns the models the user can pick from in the
	// /model picker. Returns nil for runtimes that don't expose model
	// switching; see SupportsModelSwitching for a cheap pre-check.
	AvailableModels(ctx context.Context) []modelcatalog.ModelChoice

	// SupportsModelSwitching reports whether SetAgentModel and
	// AvailableModels are wired for this runtime. Use it to gate UI
	// affordances (e.g. show /model in the menu) without paying the
	// cost of AvailableModels.
	SupportsModelSwitching() bool

	// OnToolsChanged registers a handler invoked outside of any RunStream
	// when a toolset reports a tool list change (e.g. after an MCP
	// ToolListChanged notification). A nil handler unregisters. Runtimes
	// that don't emit such events can implement this as a no-op.
	OnToolsChanged(handler func(Event))

	// OnBackgroundEvent registers a handler invoked outside of any RunStream
	// for events surfaced from detached background work (e.g. token usage
	// from background agent tasks started via run_background_agent), which
	// has no live event channel of its own. Runtimes that don't run local
	// background work can implement this as a no-op.
	OnBackgroundEvent(handler func(Event))

	// OnElicitationRequest registers a handler invoked whenever an MCP
	// toolset raises an elicitation request, regardless of which stream
	// (foreground or a detached background job) is currently "active" for
	// the runtime's internal elicitation bridge. This is the reliable
	// delivery route for background-job elicitations (#3584); runtimes
	// that don't run local background work can implement this as a no-op.
	OnElicitationRequest(handler func(Event))

	// QueueStatus returns the current depth and capacity of message queues
	QueueStatus() QueueStatus

	// TogglePause toggles whether the run loop is paused at iteration
	// boundaries. Returns the new state (true if now paused). Returns
	// [ErrUnsupported] for runtimes that don't expose pause control.
	TogglePause(ctx context.Context) (paused bool, err error)

	// Close releases resources held by the runtime (e.g., background
	// agents and pending lifecycles). The session store is *not* closed
	// here: it is supplied by the embedder via WithSessionStore and may
	// be shared with other runtimes (e.g. the TUI's spawner reuses the
	// same store across spawned sessions). Embedders that own the store
	// must close it themselves.
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
	isolatedWorktree  bool
	worktreePrompt    worktreePrompt
	worktreeRoot      string
	liveSessionParent *LocalRuntime
	harnessFactory    *harness.Factory
	commandEvaluator  *CommandEvaluatorFactory

	ctx                       func() context.Context
	toolMap                   map[string]ToolHandlerFunc
	toolDeferrals             tools.DeferralTracker
	team                      *team.Team
	agents                    *agentRouter
	resumeChan                chan ResumeRequest // Queues a decision delivered before the prompt emitter parks.
	tracer                    trace.Tracer
	modelsStore               ModelStore
	sessionCompaction         bool
	managedOAuth              bool
	unmanagedOAuthRedirectURI string
	nonInteractive            bool
	directElicitation         tools.ElicitationHandler
	startupInfoEmitted        atomic.Bool        // Track if startup info has been emitted to avoid unnecessary duplication
	elicitation               elicitationBridge  // Owns the per-stream events channel for outbound elicitation requests
	elicitationWaiters        elicitationWaiters // Routes elicitation responses to the request awaiting them, keyed by ID (#3584)
	elicitationDeclines       elicitationDeclineNotes

	// elicitationSinkMu guards onElicitationRequest; elicitation requests can
	// arrive from background-job goroutines (runCollecting) concurrently with
	// the sink being (re)registered.
	elicitationSinkMu    sync.RWMutex
	onElicitationRequest func(Event)
	onElicitationContext func(context.Context, Event)
	sessionStore         session.Store
	// generatedFiles caches per-owner-session workspace roots and manifest
	// records for [LocalRuntime.ResolveGeneratedFile]. Seeded by
	// materialization, filled lazily for restored sessions.
	artifactsOnce    sync.Once
	artifactService  *artifacts.Service
	workingDir       string   // Working directory for hooks execution
	env              []string // Environment variables for hooks execution
	modelSwitcherCfg *modelcatalog.Config
	providerRegistry *provider.Registry
	catalogOnce      sync.Once
	modelCatalog     *modelcatalog.Catalog
	modelContexts    modelContextCache

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
	observers []func() EventObserver

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

	// budgetCfg is the run-wide budget (nil when unset), budgetsCfg the
	// named budget definitions, and agentBudgets the names each agent
	// declared. All three are immutable templates; budget below is the
	// live accumulator built from them.
	budgetCfg    *latest.BudgetConfig
	budgetsCfg   map[string]latest.BudgetConfig
	agentBudgets map[string][]string

	// budgetMu guards budget and budgetStarted. The first root stream or
	// manual compaction installs the trackers; subsequent runs reuse them.
	// Delegated and compaction work share the same wallets.
	budgetMu sync.Mutex
	budget   *budgetSet
	// budgetStarted distinguishes "not built yet" from "built, and there
	// was nothing to budget" — without it a nil budget would be rebuilt on
	// every message, and the reset bug would come back for unbudgeted runs.
	budgetStarted bool

	// activeRootStreams tracks top-level RunStream loops. Recalls use this to
	// preserve mid-turn steering while still waking the embedder once the parent
	// stream has gone idle.
	activeRootStreams atomic.Int32

	// liveSessions is the registry of sessions with an active RunStream,
	// keyed by session ID. It backs the /context team view (LiveSessions)
	// and targeted manual compaction (CompactLiveSession). Guarded by
	// liveSessionsMu; see live_sessions.go for the enqueue/drain contract.
	liveSessionsMu sync.Mutex
	liveSessions   map[string]*liveSessionEntry

	activeCompactions sync.Map // session ID -> struct{}

	// recallHandler wakes embedders (TUI/App) when a tool recall arrives after
	// the active RunStream has gone idle. Protected by recallMu because tool
	// callbacks can fire from background goroutines.
	recallMu      sync.RWMutex
	recallHandler RecallHandler

	// onToolsChanged is called when an MCP toolset reports a tool list
	// change. Protected by toolsChangedMu because MCP change-notification
	// goroutines call emitToolsChanged concurrently, mirroring
	// onBackgroundEvent/backgroundEventMu below. toolsChangedUnsubs holds
	// this runtime's subscriptions on ChangeSubscriber toolsets, released
	// on re-registration and at Close.
	toolsChangedMu     sync.RWMutex
	onToolsChanged     func(Event)
	toolsChangedUnsubs []func()

	// onBackgroundEvent is called for events surfaced from detached
	// background work (e.g. background agent tasks). Protected by
	// backgroundEventMu because background tasks read it from their own
	// goroutines.
	backgroundEventMu   sync.RWMutex
	onBackgroundEvent   func(Event)
	onBackgroundContext func(context.Context, Event)

	bgAgents *agenttool.Handler

	extraToolsetRuns extraToolsetRuns

	// dmrModelLister lists the models pulled locally in Docker Model Runner,
	// used to populate DMR entries in the model picker. Defaults to
	// dmrmodels.ListModelsWithMetadata in NewLocalRuntime; left nil by runtimes built directly
	// (e.g. tests) so DMR discovery stays opt-in. Tests inject a stub here.
	dmrModelLister func(ctx context.Context) ([]dmrmodels.Model, error)

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

	// toolListTimeout bounds how long EmitStartupInfo waits for a single
	// toolset to enumerate its tools before skipping it, so one hung toolset
	// cannot stall the sidebar's "Loading tools…" state forever. Defaults to
	// defaultToolListTimeout; overridden via WithToolListTimeout.
	toolListTimeout time.Duration

	// toolStartTimeout bounds how long EmitStartupInfo waits for a single
	// toolset to start before skipping it for the startup sidebar pass.
	// Defaults to defaultToolStartTimeout; overridden via WithToolStartTimeout.
	toolStartTimeout time.Duration

	// streamStoppedDeliveryTimeout bounds how long finalizeEventChannel
	// blocks trying to deliver StreamStopped when the event buffer is full.
	// Defaults to defaultStreamStoppedDeliveryTimeout; tests shrink it to
	// exercise the drop-when-abandoned path without a real-time wait. Zero
	// falls back to the default (see streamStoppedTimeout), so runtimes built
	// directly via a struct literal in tests still get bounded delivery.
	streamStoppedDeliveryTimeout time.Duration

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

// WithUnmanagedOAuthRedirectURI configures the redirect_uri the runtime
// advertises when running MCP server OAuth flows in unmanaged mode (i.e.
// when WithManagedOAuth(false) is set). When set, docker-agent generates
// state + PKCE + DCR in-process and emits an elicitation carrying the
// `authorize_url` + `state`; the client returns `{code, state}` via
// ResumeElicitation and docker-agent does the token exchange itself.
// When empty, the runtime falls back to the legacy unmanaged contract
// where the client performs the OAuth flow and returns an access token.
func WithUnmanagedOAuthRedirectURI(uri string) Opt {
	return func(r *LocalRuntime) {
		r.unmanagedOAuthRedirectURI = uri
	}
}

// WithElicitationHandler handles requests synchronously instead of emitting elicitation events.
// Non-interactive runtimes still decline; the handler must honor the request context.
func WithElicitationHandler(handler tools.ElicitationHandler) Opt {
	return func(r *LocalRuntime) { r.directElicitation = handler }
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

func WithProviderRegistry(registry *provider.Registry) Opt {
	return func(r *LocalRuntime) {
		if registry != nil {
			r.providerRegistry = registry
		}
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

// WithBudget sets the run-wide budget: the cost, token and wall-clock
// ceilings that stop a run once crossed, regardless of which agent spends.
// A nil or all-zero config leaves runs unbudgeted, which is the default.
func WithBudget(cfg *latest.BudgetConfig) Opt {
	return func(r *LocalRuntime) {
		if cfg.IsZero() {
			return
		}
		r.budgetCfg = cfg
	}
}

// WithNamedBudgets registers the manifest's named budget definitions and
// the budget names each agent declared. A named budget referenced by
// several agents is one shared pot, not a copy per agent.
func WithNamedBudgets(defs map[string]latest.BudgetConfig, agentBudgets map[string][]string) Opt {
	return func(r *LocalRuntime) {
		if len(defs) == 0 || len(agentBudgets) == 0 {
			return
		}
		r.budgetsCfg = defs
		r.agentBudgets = agentBudgets
	}
}

// WithToolListTimeout overrides how long EmitStartupInfo waits for a single
// toolset to enumerate its tools before skipping it. Defaults to
// defaultToolListTimeout. A non-positive value is ignored so the default
// stands. Tests pass a short timeout to exercise the skip path (a toolset
// whose Tools() blocks) without a real-time wait.
func WithToolListTimeout(d time.Duration) Opt {
	return func(r *LocalRuntime) {
		if d > 0 {
			r.toolListTimeout = d
		}
	}
}

// WithToolStartTimeout overrides how long EmitStartupInfo waits for a single
// toolset to start before skipping it. Defaults to defaultToolStartTimeout.
// A non-positive value is ignored so the default stands. Tests pass a short
// timeout to exercise the skip path (a toolset whose Start() blocks) without
// a real-time wait.
func WithToolStartTimeout(d time.Duration) Opt {
	return func(r *LocalRuntime) {
		if d > 0 {
			r.toolStartTimeout = d
		}
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
func New(ctx context.Context, agents *team.Team, opts ...Opt) (Runtime, error) {
	return NewLocalRuntime(ctx, agents, opts...)
}

// NewLocalRuntime creates a new LocalRuntime without the persistence wrapper.
// This is useful for testing or when persistence is handled externally.
func NewLocalRuntime(ctx context.Context, agents *team.Team, opts ...Opt) (*LocalRuntime, error) {
	defaultAgent, err := agents.DefaultAgent()
	if err != nil {
		return nil, err
	}

	r := &LocalRuntime{
		ctx:                          func() context.Context { return context.WithoutCancel(ctx) },
		toolMap:                      make(map[string]ToolHandlerFunc),
		liveSessions:                 make(map[string]*liveSessionEntry),
		team:                         agents,
		agents:                       newAgentRouter(agents, defaultAgent.Name()),
		resumeChan:                   make(chan ResumeRequest, 1),
		steerQueue:                   NewInMemoryMessageQueue(defaultSteerQueueCapacity),
		followUpQueue:                NewInMemoryMessageQueue(defaultFollowUpQueueCapacity),
		sessionCompaction:            true,
		managedOAuth:                 true,
		sessionStore:                 session.NewInMemorySessionStore(),
		fallback:                     newFallbackExecutor(),
		now:                          time.Now,
		telemetry:                    defaultTelemetry{},
		providerRegistry:             provider.EmptyRegistry(),
		maxOverflowCompactions:       defaultMaxOverflowCompactions,
		toolListTimeout:              defaultToolListTimeout,
		toolStartTimeout:             defaultToolStartTimeout,
		streamStoppedDeliveryTimeout: defaultStreamStoppedDeliveryTimeout,
		dmrModelLister:               dmrmodels.ListModelsWithMetadata,
	}
	r.bgAgents = agenttool.NewHandler(r)
	r.fallback.prepareMessages = r.prepareMessagesForModel

	// stripUnsupportedModalitiesTransform captures the runtime closure to
	// resolve the agent from Input.AgentName, so it lives here rather
	// than as a stateless builtin in pkg/hooks/builtins. It drops image
	// content for text-only models on every model call.
	//
	// redact_secrets used to live here as a sibling [MessageTransform];
	// it now ships entirely as a [hooks.BuiltinFunc] in
	// pkg/hooks/builtins/redact_secrets.go and is wired into all three
	// of tool_input_transform, before_llm_call, and tool_response_transform via
	// [builtins.ApplyAgentDefaults] (or a user's hooks YAML directly),
	// so the rewrite path is the same for every leak vector and there
	// is no flag-only code path to keep in sync.
	//
	// Ordering matters: strip_generated_media MUST run before
	// strip_unsupported_modalities. The latter strips any image/audio/
	// video-kind document part the resolved model can't accept,
	// regardless of whether that part is a runtime-materialized generated
	// artifact or a user attachment — it has no placeholder logic. If it
	// ran first, a capability-less or unknown model would have a
	// media-only generated-media assistant message stripped down to
	// nothing right there, and strip_generated_media would then see no
	// generated-media part left to react to: its placeholder would never
	// fire, and the turn would silently vanish from outgoing history.
	// Running strip_generated_media first guarantees its placeholder text
	// is already in place — as ordinary text, not a media part — by the
	// time strip_unsupported_modalities runs, so there is nothing left for
	// it to strip from that message.
	r.transforms = append(r.transforms,
		// strip_generated_media has no runtime state to capture (the policy
		// is unconditional), so it registers the free function directly
		// rather than a method value like the transform below.
		registeredTransform{
			name: BuiltinStripGeneratedMedia,
			fn:   stripGeneratedMediaTransform,
		},
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
	RegisterModelHook(r.hooksRegistry, r.providerRegistry)
	r.hooksRegistry.Register(hooks.HookTypeEvaluator, hooks.NewEvaluatorFactory(func(agentName, name string) (evaluator.Evaluator, bool) {
		a, err := r.team.Agent(agentName)
		if err != nil {
			return nil, false
		}
		var client evaluator.Evaluator
		var ok bool
		if a.HasEvaluatorScope() {
			client, ok = a.Evaluator(name)
		} else {
			client, ok = r.team.Evaluator(name)
		}
		if !ok || client == nil {
			return nil, false
		}
		return &accountedEvaluator{client: client, name: name}, true
	}, hooks.WithRoutingDefaults(r.routingDefaultAgent)))

	// cache_response is registered here (not in pkg/hooks/builtins)
	// because it needs to capture the runtime to resolve the agent
	// referenced by Input.AgentName. The other builtins are stateless
	// and can stay as package-level functions registered via
	// [builtins.Register] above.
	if err := r.hooksRegistry.RegisterBuiltin(BuiltinCacheResponse, r.cacheResponseBuiltin); err != nil {
		return nil, fmt.Errorf("register %q builtin: %w", BuiltinCacheResponse, err)
	}

	// Build cooldowns after opts so they pick up [WithClock].
	r.fallback.cooldowns = newCooldownManager(r.now)

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
		// Precedence: an explicit WithModelStore (already set above) wins; then a
		// store carried on the ModelSwitcherConfig (the team loader shares the
		// one it warmed so the first /model open skips the cold catalog parse);
		// otherwise a lazy store constructed on first use.
		if r.modelSwitcherCfg != nil && r.modelSwitcherCfg.ModelsStore != nil {
			r.modelsStore = r.modelSwitcherCfg.ModelsStore
		} else {
			r.modelsStore = &lazyModelStore{}
		}
	}

	// Validate that the current agent exists and has a model
	// (the router's current name might have been changed by WithCurrentAgent)
	defaultAgent, err = r.team.Agent(r.agents.Name())
	if err != nil {
		return nil, err
	}

	if defaultAgent.Model(ctx) == nil && !defaultAgent.HasHarness() {
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
	if r.sessionStore != nil {
		factory := func() EventObserver { return newPersistenceObserver(r.sessionStore) }
		r.observers = append([]func() EventObserver{factory}, r.observers...)
	}

	slog.DebugContext(ctx, "Creating new runtime", "agent", r.agents.Name(), "available_agents", agents.Size())

	return r, nil
}

func (r *LocalRuntime) CurrentAgentName(context.Context) string {
	return r.currentAgentName()
}

// currentAgentName is the context-free internal accessor. LocalRuntime
// resolves the active agent purely from in-memory state, so its internal
// callers (event callbacks, logging) use this directly; the exported
// context-taking method exists to satisfy the Runtime interface, whose
// remote implementation needs a context for its one-time lookup.
func (r *LocalRuntime) currentAgentName() string {
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

func (r *LocalRuntime) SetCurrentAgent(_ context.Context, agentName string) error {
	return r.agents.SetValidated(agentName)
}

func (r *LocalRuntime) CurrentAgentCommands(context.Context) types.Commands {
	return r.CurrentAgent().Commands()
}

// CurrentAgentTools returns the tools available to the current agent.
// This starts toolsets if needed; returned handlers enforce this agent's skill
// policy without permitting embedded commands outside a tool call.
func (r *LocalRuntime) CurrentAgentTools(ctx context.Context) ([]tools.Tool, error) {
	a := r.CurrentAgent()
	agentTools, err := a.Tools(ctx)
	if err != nil {
		return agentTools, err
	}
	return bindSkillRuntime(agentTools, skillRuntime{runtime: r, agent: a}), nil
}

// AgentConfigInfo inspects an agent without starting tools or drivers.
func (r *LocalRuntime) AgentConfigInfo(ctx context.Context, name string) inspection.AgentConfigInfo {
	current := ""
	if r.agents != nil {
		current = r.agents.Name()
	}
	return (inspection.Service{Team: r.team, CurrentAgent: current}).AgentConfigInfo(ctx, name)
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
		statuses = append(statuses, inspection.ToolsetStatus(ts))
	}
	return statuses
}

// AgentToolsetStatuses returns one ToolsetStatus per toolset of the named
// agent, in declaration order. It mirrors CurrentAgentToolsetStatuses but for
// an arbitrary agent and is side-effect-free: it reads each toolset's live
// lifecycle (state, kind, last error, restart count) without starting it.
// Unknown agents yield nil, so callers degrade gracefully.
func (r *LocalRuntime) AgentToolsetStatuses(name string) []tools.ToolsetStatus {
	a, err := r.team.Agent(name)
	if err != nil || a == nil {
		return nil
	}
	toolSets := a.ToolSets()
	statuses := make([]tools.ToolsetStatus, 0, len(toolSets))
	for _, ts := range toolSets {
		statuses = append(statuses, inspection.ToolsetStatus(ts))
	}
	return statuses
}

// RestartToolset locates the named toolset on the active agent and
// asks its canonical lifecycle wrapper to restart it in place. The wrapper
// serializes the operation with start/stop and keeps its lifecycle state in
// sync with the underlying supervisor.
//
// Returns an error when:
//   - no toolset matches name (matching uses the same logic as the
//     /tools dialog: the toolset's Name() if any, otherwise its
//     description),
//   - no matching toolset supports restart,
//   - the supervisor itself returned an error (timeout, classified
//     transport failure, etc.).
//
// Display names are not guaranteed unique (a YAML name: can shadow a
// built-in), so non-restartable matches are skipped rather than aborting:
// the first restartable toolset with the given name wins.
func (r *LocalRuntime) RestartToolset(ctx context.Context, name string) error {
	a := r.CurrentAgent()
	if a == nil {
		return errors.New("no active agent")
	}
	found := false
	for _, ts := range a.ToolSets() {
		if inspection.ToolsetName(ts, tools.DescribeToolSet(ts)) != name {
			continue
		}
		found = true
		startable, ok := ts.(*tools.StartableToolSet)
		if ok && startable.CanRestart() {
			return startable.RestartIfSupported(ctx)
		}
	}
	if found {
		return fmt.Errorf("toolset %q does not support restart", name)
	}
	return fmt.Errorf("toolset %q not found", name)
}

// mcpPromptToolset is the subset of the MCP toolset's API the runtime needs
// for prompt discovery and execution. It is an interface (satisfied by
// *mcp.Toolset) so pkg/runtime does not import the MCP toolset package,
// which would link its whole dependency tree into every embedder.
type mcpPromptToolset interface {
	ListPrompts(ctx context.Context) ([]tools.PromptInfo, error)
	GetPrompt(ctx context.Context, name string, arguments map[string]string) (*mcp.GetPromptResult, error)
}

// CurrentMCPPrompts returns the available MCP prompts from all active MCP toolsets
// for the current agent. It discovers prompts by calling ListPrompts on each MCP toolset
// and aggregates the results into a map keyed by prompt name.
func (r *LocalRuntime) CurrentMCPPrompts(ctx context.Context) map[string]tools.PromptInfo {
	prompts := make(map[string]tools.PromptInfo)

	// Get the current agent to access its toolsets
	currentAgent := r.CurrentAgent()
	if currentAgent == nil {
		slog.WarnContext(ctx, "No current agent available for MCP prompt discovery")
		return prompts
	}

	// Iterate through all toolsets of the current agent
	for _, toolset := range currentAgent.ToolSets() {
		mcpToolsets := tools.FindAll[mcpPromptToolset](toolset)
		if len(mcpToolsets) == 0 {
			slog.DebugContext(ctx, "Toolset contains no MCP prompt provider", "type", fmt.Sprintf("%T", toolset))
			continue
		}
		for _, mcpToolset := range mcpToolsets {
			slog.DebugContext(ctx, "Found MCP toolset", "toolset", mcpToolset)
			maps.Copy(prompts, r.discoverMCPPrompts(ctx, mcpToolset))
		}
	}

	slog.DebugContext(ctx, "Discovered MCP prompts", "agent", currentAgent.Name(), "prompt_count", len(prompts))
	return prompts
}

// discoverMCPPrompts queries an MCP toolset for available prompts and converts them
// to PromptInfo structures. This method handles the MCP protocol communication
// and gracefully handles any errors during prompt discovery.
func (r *LocalRuntime) discoverMCPPrompts(ctx context.Context, toolset mcpPromptToolset) map[string]tools.PromptInfo {
	mcpPrompts, err := toolset.ListPrompts(ctx)
	if err != nil {
		slog.WarnContext(ctx, "Failed to list MCP prompts from toolset", "error", err)
		return nil
	}

	prompts := make(map[string]tools.PromptInfo, len(mcpPrompts))
	for _, mcpPrompt := range mcpPrompts {
		promptInfo := tools.PromptInfo{
			Name:        mcpPrompt.Name,
			Description: mcpPrompt.Description,
			Arguments:   make([]tools.PromptArgument, 0, len(mcpPrompt.Arguments)),
		}

		for _, arg := range mcpPrompt.Arguments {
			promptInfo.Arguments = append(promptInfo.Arguments, tools.PromptArgument{
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
func (r *LocalRuntime) CurrentAgentSkillsToolset() *skills.ToolSet {
	return agentSkillsToolset(r.CurrentAgent())
}

// agentSkillsToolset returns the skills toolset configured on a, or nil if
// a is nil or has none. Session-aware callers (e.g. RunSkillFork) use it
// directly so a pinned session's agent is honoured instead of the shared
// current agent.
func agentSkillsToolset(a *agent.Agent) *skills.ToolSet {
	if a == nil {
		return nil
	}
	for _, ts := range a.ToolSets() {
		if st, ok := tools.Find[*skills.ToolSet](ts); ok {
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
		for _, mcpToolset := range tools.FindAll[mcpPromptToolset](toolset) {
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
	}

	return "", fmt.Errorf("MCP prompt '%s' not found in any active toolset", promptName)
}

// TitleGenerator returns a title generator for automatic session title
// generation, or nil when no configured model is a usable title candidate
// (see [sessiontitle.New]). A harness agent has no models: its driver titles
// the session when it implements [harness.Completer].
func (r *LocalRuntime) TitleGenerator(ctx context.Context) *sessiontitle.Generator {
	a := r.CurrentAgent()
	if a == nil {
		return nil
	}
	if a.HasHarness() {
		return r.harnessTitleGenerator(a)
	}
	return sessiontitle.New(a.TitleModels(ctx)...)
}

// getAgentModelID returns the model ID for an agent. The zero ID is
// returned when no model is configured.
func getAgentModelID(ctx context.Context, a *agent.Agent) modelsdev.ID {
	if a == nil {
		return modelsdev.ID{}
	}
	if model := a.Model(ctx); model != nil {
		return model.ID()
	}
	return modelsdev.ID{}
}

// getEffectiveModelID returns the currently active model ID for an agent, accounting
// for any active fallback cooldown. During a cooldown period, this returns the fallback
// model ID instead of the configured primary model, so the UI reflects the actual model in use.
func (r *LocalRuntime) getEffectiveModelID(ctx context.Context, a *agent.Agent) modelsdev.ID {
	cooldownState := r.fallback.cooldowns.Get(a.Name())
	if cooldownState != nil {
		fallbacks := a.FallbackModels()
		if cooldownState.fallbackIndex >= 0 && cooldownState.fallbackIndex < len(fallbacks) {
			return fallbacks[cooldownState.fallbackIndex].ID()
		}
	}
	return getAgentModelID(ctx, a)
}

// agentDetailsFromTeam converts team agent info to AgentDetails for events.
// It accounts for active fallback cooldowns, returning the effective model
// instead of the configured model when a fallback is in effect.
func (r *LocalRuntime) agentDetailsFromTeam(ctx context.Context) []AgentDetails {
	agentsInfo := r.team.AgentsInfo(ctx)
	details := make([]AgentDetails, len(agentsInfo))
	for i, info := range agentsInfo {
		providerName := info.Provider
		modelName := info.Model
		var thinking string
		var capabilities *inspection.ExecutionCapabilities

		// Get the agent to access fallbacks and the effective thinking level.
		if a, err := r.team.Agent(info.Name); err == nil && a != nil {
			capabilities = inspection.Capabilities(a)
			// Check if this agent has an active fallback cooldown
			cooldownState := r.fallback.cooldowns.Get(info.Name)
			if cooldownState != nil {
				fallbacks := a.FallbackModels()
				if cooldownState.fallbackIndex >= 0 && cooldownState.fallbackIndex < len(fallbacks) {
					fb := fallbacks[cooldownState.fallbackIndex].ID()
					providerName = fb.Provider
					modelName = fb.Model
				}
			}
			thinking = r.agentThinkingLabel(ctx, a)
		}

		details[i] = AgentDetails{
			Name:                  info.Name,
			Description:           info.Description,
			Provider:              providerName,
			Model:                 modelName,
			Thinking:              thinking,
			Commands:              info.Commands,
			ExecutionCapabilities: capabilities,
		}
	}
	return details
}

// agentThinkingLabel returns a short, user-facing label for the effective
// thinking-effort level of the agent's current model: the effort level (e.g.
// "high"), "adaptive" for adaptive budgets, the decimal token count for
// token-based budgets, "off" when thinking is disabled on a reasoning-capable
// model, or "" when the model has no selectable thinking configuration to
// display. A harness agent shows its effort, "adaptive" while its CLI picks it.
func (r *LocalRuntime) agentThinkingLabel(ctx context.Context, a *agent.Agent) string {
	if a.HasHarness() {
		return harnessThinkingLabel(a.Harness())
	}
	models := a.EffectiveModels()
	if len(models) == 0 {
		return ""
	}
	cfg := models[0].BaseConfig().ModelConfig
	// Only models that can actually reason get a thinking line.
	if !r.modelSupportsThinking(ctx, &cfg) {
		return ""
	}
	budget := cfg.ThinkingBudget
	if budget == nil || budget.IsDisabled() {
		return "off"
	}
	if l, ok := budget.EffortLevel(); ok {
		return l.String()
	}
	if budget.IsAdaptive() {
		return "adaptive"
	}
	return strconv.Itoa(budget.Tokens) // token-based budget
}

// SessionStore returns the session store for browsing/loading past sessions.
func (r *LocalRuntime) SessionStore() session.Store {
	return r.sessionStore
}

// Close releases resources held by the runtime. The session store is
// *not* closed here: it is provided by the embedder via
// WithSessionStore and may be shared with other runtimes (e.g. the
// TUI's session spawner reuses the same store across spawned
// sessions, so closing it here would break later session lookups).
// Embedders that own the store are responsible for closing it once
// when their process is shutting down.
func (r *LocalRuntime) Close() error {
	r.bgAgents.StopAll()
	r.OnToolsChanged(nil)
	return r.extraToolsetRuns.close(context.WithoutCancel(r.ctx()))
}

// UpdateSessionTitle persists the session title via the session store.
func (r *LocalRuntime) UpdateSessionTitle(ctx context.Context, sess *session.Session, title string) error {
	sess.SetTitle(title)
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
	r.startupInfoEmitted.Store(false)
}

// OnToolsChanged registers a handler that is called when an MCP toolset
// reports a tool list change outside of a RunStream. This allows the UI
// to update the tool count immediately. The runtime subscribes to the
// team's toolsets on behalf of the handler, replacing only its own previous
// subscriptions so runtimes sharing a toolset do not displace each other;
// a nil handler unsubscribes.
func (r *LocalRuntime) OnToolsChanged(handler func(Event)) {
	r.toolsChangedMu.Lock()
	r.onToolsChanged = handler
	previous := r.toolsChangedUnsubs
	r.toolsChangedUnsubs = nil
	if handler != nil {
		r.toolsChangedUnsubs = r.subscribeToolsChanged()
	}
	r.toolsChangedMu.Unlock()

	for _, unsub := range previous {
		unsub()
	}
}

// emitToolsChanged is the callback registered on MCP toolsets. It re-reads
// the current agent's full tool list and pushes a ToolsetInfo event.
func (r *LocalRuntime) emitToolsChanged() {
	r.toolsChangedMu.RLock()
	handler := r.onToolsChanged
	r.toolsChangedMu.RUnlock()
	if handler == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.ctx(), toolsChangedTimeout)
	defer cancel()
	a := r.CurrentAgent()
	agentTools, err := a.StartedTools(ctx)
	if err != nil {
		return
	}
	handler(ToolsetInfo(len(agentTools), false, r.currentAgentName()))
}

// OnBackgroundEvent receives lifecycle and usage events from detached agent tasks.
func (r *LocalRuntime) OnBackgroundEvent(handler func(Event)) {
	r.backgroundEventMu.Lock()
	defer r.backgroundEventMu.Unlock()
	r.onBackgroundEvent = handler
	r.onBackgroundContext = nil
}

// OnBackgroundEventWithContext preserves detached producer ownership.
func (r *LocalRuntime) OnBackgroundEventWithContext(handler func(context.Context, Event)) {
	r.backgroundEventMu.Lock()
	defer r.backgroundEventMu.Unlock()
	r.onBackgroundContext = handler
	r.onBackgroundEvent = nil
}

// emitBackgroundEvent forwards an event from detached background work to the
// registered handler, if any.
func (r *LocalRuntime) emitBackgroundEvent(ctx context.Context, event Event) {
	r.backgroundEventMu.RLock()
	handler := r.onBackgroundEvent
	contextual := r.onBackgroundContext
	r.backgroundEventMu.RUnlock()
	if contextual != nil {
		contextual(ctx, event)
		return
	}
	if handler != nil {
		handler(event)
	}
}

// emitAgentAndTeamInfo sends the AgentInfo and TeamInfo events that drive the
// sidebar's agent/model/thinking display. It returns false when sending was
// aborted (e.g. the context was cancelled). Shared by EmitStartupInfo and
// EmitAgentInfo so both render identical model labels.
func (r *LocalRuntime) armResume() {
	select {
	case <-r.resumeChan:
	default:
	}
}

func (r *LocalRuntime) Resume(_ context.Context, req ResumeRequest) {
	slog.Debug("Resuming runtime", "agent", r.currentAgentName(), "type", req.Type, "reason", req.Reason)

	// Defensive validation:
	//
	// The runtime may be resumed by multiple entry points (API, CLI, TUI, tests).
	// Even if upstream layers perform validation, the runtime must never assume
	// the ResumeType is valid. Accepting invalid values here leads to confusing
	// downstream behavior where tool execution fails without a clear cause.
	if !IsValidResumeType(req.Type) {
		slog.Warn(
			"Invalid resume type received; ignoring resume request",
			"agent", r.currentAgentName(),
			"confirmation_type", req.Type,
			"valid_types", ValidResumeTypes(),
		)
		return
	}
	// Normalize legacy verbs (approve-session, approve-safe,
	// approve-safer) so every downstream consumer sees the current set.
	req.Type = NormalizeResumeType(req.Type)

	// Attempt to deliver the resume signal to the execution loop.
	//
	// The channel is non-blocking by design to avoid deadlocks if the runtime
	// is not currently waiting for a confirmation (e.g. already resumed,
	// canceled, or shutting down).
	select {
	case r.resumeChan <- req:
		slog.Debug("Resume signal sent", "agent", r.currentAgentName())
	default:
		slog.Debug(
			"Resume channel not ready; resume signal dropped",
			"agent", r.currentAgentName(),
			"confirmation_type", req.Type,
		)
	}
}

var (
	// ErrSteerQueueFull indicates that no more steering messages can be queued.
	ErrSteerQueueFull = errors.New("steer queue full")
	// ErrFollowUpQueueFull indicates that no more follow-up messages can be queued.
	ErrFollowUpQueueFull = errors.New("follow-up queue full")
)

// Steer enqueues a user message for urgent mid-turn injection into the
// running agent loop. The message will be picked up after the current batch
// of tool calls finishes but before the loop checks whether to stop.
func (r *LocalRuntime) Steer(ctx context.Context, msg QueuedMessage) error {
	if !r.steerQueue.Enqueue(ctx, msg) {
		return ErrSteerQueueFull
	}
	return nil
}

// FollowUp enqueues a message to be processed after the current agent turn
// finishes. Unlike Steer, follow-ups are popped one at a time and each gets
// a full undivided agent turn.
func (r *LocalRuntime) FollowUp(ctx context.Context, msg QueuedMessage) error {
	if !r.followUpQueue.Enqueue(ctx, msg) {
		return ErrFollowUpQueueFull
	}
	return nil
}

func (r *LocalRuntime) CancelSteer(_ context.Context, id string) bool {
	q, ok := r.steerQueue.(cancelableMessageQueue)
	return ok && q.Cancel(id)
}

func (r *LocalRuntime) CancelFollowUp(_ context.Context, id string) bool {
	q, ok := r.followUpQueue.(cancelableMessageQueue)
	return ok && q.Cancel(id)
}

// SetRecallHandler registers an embedder-owned wake-up path for tool recalls.
// When unset, recalls fall back to the steer queue and are consumed by the next
// active RunStream.
func (r *LocalRuntime) SetRecallHandler(handler RecallHandler) {
	r.recallMu.Lock()
	defer r.recallMu.Unlock()
	r.recallHandler = handler
}

func (r *LocalRuntime) recall(ctx context.Context, msg QueuedMessage) error {
	if r.activeRootStreams.Load() > 0 {
		return r.Steer(ctx, msg)
	}

	r.recallMu.RLock()
	handler := r.recallHandler
	r.recallMu.RUnlock()
	if handler == nil {
		return r.Steer(ctx, msg)
	}
	if !handler(ctx, msg) {
		return errors.New("recall handler rejected message")
	}
	return nil
}

func (r *LocalRuntime) QueueStatus() QueueStatus {
	status := QueueStatus{}
	if steerQ, ok := r.steerQueue.(*inMemoryMessageQueue); ok {
		status.SteerDepth, status.SteerCapacity = steerQ.status()
	}
	if followupQ, ok := r.followUpQueue.(*inMemoryMessageQueue); ok {
		status.FollowupDepth, status.FollowupCapacity = followupQ.status()
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
	// Manual compaction bypasses RunStream, but native compaction may start tools.
	ctx = genai.WithConversationID(ctx, sess.ID)
	ctx = tools.WithHandlerScope(ctx, tools.HandlerScope{
		Elicitation: r.elicitationHandler,
		OAuthSuccess: func() {
			events.Emit(Authorization(tools.ElicitationActionAccept, r.resolveSessionAgent(sess).Name()))
		},
	})
	r.configureToolsetHandlers(r.resolveSessionAgent(sess))
	if err := r.compactWithReason(ctx, sess, additionalPrompt, compactionReasonManual, events); err != nil {
		events.Emit(ErrorForSession(sess.ID, err.Error()))
	}
}

// compactWithReason runs a session compaction with the supplied reason and
// emits a TokenUsageEvent so the UI immediately reflects the new context
// pressure.
//
// reason is reported to BeforeCompaction / AfterCompaction hooks as
// CompactionReason. Use [compactionReasonThreshold] for proactive
// threshold-of-context triggers (90% by default, tunable via
// compaction_threshold), [compactionReasonOverflow] for post-overflow
// auto-recovery, or [compactionReasonManual] for user-invoked compactions.
//
// PreCompact hooks fire first via the legacy [hooks.Input.Source] field
// ("auto" / "tool_overflow" / "overflow" / "manual"); they may cancel the
// compaction or contribute additional steering text. BeforeCompaction
// hooks then fire inside [LocalRuntime.doCompact] with [Input.CompactionReason]
// set to the canonical reason; they may veto or supply a custom summary.
func (r *LocalRuntime) compactWithReason(ctx context.Context, sess *session.Session, additionalPrompt, reason string, events EventSink) error {
	if !r.acquireCompaction(sess.ID) {
		return ErrCompactionInProgress
	}
	defer r.activeCompactions.Delete(sess.ID)
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
		return nil
	}
	additionalPrompt = joinPrompts(additionalPrompt, extraPrompt)

	r.doCompact(ctx, sess, a, additionalPrompt, reason, events)

	// Emit a TokenUsageEvent so the sidebar immediately reflects the
	// compaction: tokens drop to the summary size, context % drops, and
	// cost increases by the summary generation cost.
	modelID := r.getEffectiveModelID(ctx, a)
	contextLimit := r.effectiveContextLimit(ctx, a, r.resolveContextLimit(ctx, a.Model(ctx), modelID))
	events.Emit(NewTokenUsageEvent(sess.ID, a.Name(), SessionUsage(sess, contextLimit, a.CompactionThreshold())))
	return nil
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
