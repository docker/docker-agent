package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker-agent/pkg/cache"
	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/structuredoutput"
)

// Agent represents an AI agent
type Agent struct {
	name                    string
	description             string
	welcomeMessage          string
	instruction             string
	toolsets                []*tools.StartableToolSet
	models                  []provider.Provider
	fallbackModels          []provider.Provider                 // Fallback models to try if primary fails
	fallbackRetries         int                                 // Number of retries per fallback model with exponential backoff
	fallbackCooldown        time.Duration                       // Duration to stick with fallback after non-retryable error
	titleModel              provider.Provider                   // Optional dedicated model for session-title generation
	compactionModel         provider.Provider                   // Optional dedicated model for session compaction (summary generation)
	compactionThreshold     float64                             // Custom proactive-compaction trigger fraction; 0 means "use the default"
	sessionCompactionOff    bool                                // True when the agent opted out of automatic session compaction
	modelOverrides          atomic.Pointer[[]provider.Provider] // Optional model override(s) set at runtime (supports alloy)
	subAgents               []*Agent
	handoffs                []*Agent
	forceHandoff            *Agent
	parents                 []*Agent
	addDate                 bool
	addEnvironmentInfo      bool
	addDescriptionParameter bool
	redactSecrets           bool
	safety                  latest.SafetyMode // Author-declared safety-mode default for new sessions; empty when unset
	maxIterations           int
	maxConsecutiveToolCalls int
	maxOldToolCallTokens    int
	maxToolResultTokens     int
	numHistoryItems         int
	addPromptFiles          []string
	tools                   []tools.Tool
	commands                types.Commands
	harness                 *latest.HarnessConfig
	hooks                   *latest.HooksConfig
	cache                   *cache.Cache
	structuredOutput        *latest.StructuredOutput
	// structuredOutputTool lazily compiles the tool-mode output tool once
	// per agent (sync.OnceValues over structuredoutput.New); nil when the
	// agent has no tool-mode structured output. WithStructuredOutput
	// replaces the closure, which resets the cache.
	structuredOutputTool func() (*structuredoutput.OutputTool, error)

	// warningsMu guards pendingWarnings. AddToolWarning and DrainWarnings
	// may be called concurrently from the runtime loop, the MCP server,
	// the TUI and session manager.
	warningsMu      sync.Mutex
	pendingWarnings []string

	// collisionsMu guards reportedCollisions, the once-per-streak guard for
	// duplicate-tool-name warnings. collectTools may run concurrently from
	// the runtime loop and MCP notification handlers.
	collisionsMu       sync.Mutex
	reportedCollisions map[string]bool
}

// New creates a new agent
func New(name, prompt string, opts ...Opt) *Agent {
	agent := &Agent{
		name:        name,
		instruction: prompt,
	}

	for _, opt := range opts {
		opt(agent)
	}

	return agent
}

func (a *Agent) Name() string {
	return a.name
}

// Instruction returns the agent's instructions
func (a *Agent) Instruction() string {
	return a.instruction
}

func (a *Agent) AddDate() bool {
	return a.addDate
}

func (a *Agent) AddEnvironmentInfo() bool {
	return a.addEnvironmentInfo
}

// RedactSecrets reports whether the agent has opted into the
// redact_secrets feature. When true, the runtime auto-injects the
// redact_secrets pre_tool_use builtin (scrubs tool arguments),
// enables the runtime's before_llm_call message transform (scrubs
// outgoing chat content), AND wires the dispatcher's tool-output
// scrub (redacts tool output at the source so it never reaches event
// consumers, the persisted session file, the post_tool_use hook
// input, or the next LLM call).
func (a *Agent) RedactSecrets() bool {
	return a.redactSecrets
}

func (a *Agent) MaxIterations() int {
	return a.maxIterations
}

// Safety returns the safety-mode default the agent's author declared in
// its config (agents.<name>.safety), or empty when unset. It is a
// default only: any user-owned choice (CLI flags, alias options, user
// settings) takes precedence when a session is created, and it never
// replaces the mode stored on a resumed session.
func (a *Agent) Safety() latest.SafetyMode {
	return a.safety
}

func (a *Agent) MaxConsecutiveToolCalls() int {
	return a.maxConsecutiveToolCalls
}

func (a *Agent) MaxOldToolCallTokens() int {
	return a.maxOldToolCallTokens
}

func (a *Agent) MaxToolResultTokens() int {
	return a.maxToolResultTokens
}

func (a *Agent) NumHistoryItems() int {
	return a.numHistoryItems
}

func (a *Agent) AddPromptFiles() []string {
	return a.addPromptFiles
}

// Description returns the agent's description
func (a *Agent) Description() string {
	return a.description
}

// WelcomeMessage returns the agent's welcome message
func (a *Agent) WelcomeMessage() string {
	return a.welcomeMessage
}

// SubAgents returns the list of sub-agents
func (a *Agent) SubAgents() []*Agent {
	return a.subAgents
}

// Handoffs returns the list of handoff agents
func (a *Agent) Handoffs() []*Agent {
	return a.handoffs
}

// ForceHandoff returns the agent that unconditionally receives the
// conversation when this agent produces a final response, or nil when
// no forced handoff is configured.
func (a *Agent) ForceHandoff() *Agent {
	return a.forceHandoff
}

// Parents returns the list of parent agent names
func (a *Agent) Parents() []*Agent {
	return a.parents
}

// HasSubAgents checks if the agent has sub-agents
func (a *Agent) HasSubAgents() bool {
	return len(a.subAgents) > 0
}

// Model returns the model to use for this agent.
// If model override(s) are set, it returns one of the overrides (randomly for alloy).
// Otherwise, it returns a random model from the available models.
//
// ctx is used for log correlation only — the selection itself is local.
// Pass [context.TODO] from callers that don't have a request context
// (configuration validation, debug commands).
func (a *Agent) Model(ctx context.Context) provider.Provider {
	var selected provider.Provider
	var poolSize int
	// Check for model override first (set via TUI model switching)
	if overrides := a.modelOverrides.Load(); overrides != nil && len(*overrides) > 0 {
		selected = (*overrides)[rand.Intn(len(*overrides))]
		poolSize = len(*overrides)
	} else {
		if len(a.models) == 0 {
			return nil
		}
		selected = a.models[rand.Intn(len(a.models))]
		poolSize = len(a.models)
	}
	slog.InfoContext(ctx, "Model selected", "agent", a.name, "model", selected.ID(), "pool_size", poolSize)
	return selected
}

// SetModelOverride sets runtime model override(s) for this agent.
// The override(s) take precedence over the configured models.
// For alloy models, multiple providers can be passed and one will be randomly selected.
// Pass no arguments or nil providers to clear the override.
//
// SetModelOverride returns a snapshot of the value that was just stored.
// Callers performing a scoped override (apply now, restore later) should
// keep this snapshot and pass it as `current` to RestoreModelOverride so
// the deferred restore can detect concurrent changes via CAS. Callers
// that only need the side-effect can ignore the return value.
func (a *Agent) SetModelOverride(models ...provider.Provider) ModelOverrideSnapshot {
	// Filter out nil providers
	var validModels []provider.Provider
	for _, m := range models {
		if m != nil {
			validModels = append(validModels, m)
		}
	}

	var ptr *[]provider.Provider
	if len(validModels) == 0 {
		a.modelOverrides.Store(nil)
		slog.Debug("Cleared model override", "agent", a.name)
	} else {
		ptr = &validModels
		a.modelOverrides.Store(ptr)
		ids := make([]string, len(validModels))
		for i, m := range validModels {
			ids[i] = m.ID().String()
		}
		slog.Debug("Set model override", "agent", a.name, "models", ids)
	}
	return ModelOverrideSnapshot{ptr: ptr}
}

// HasModelOverride returns true if a model override is currently set.
func (a *Agent) HasModelOverride() bool {
	overrides := a.modelOverrides.Load()
	return overrides != nil && len(*overrides) > 0
}

// ModelOverrideSnapshot is an opaque token that captures the agent's model
// override at a point in time. Pass it to RestoreModelOverride to undo a
// scoped override safely.
type ModelOverrideSnapshot struct {
	// ptr is the raw atomic pointer value at snapshot time. It is used for
	// pointer-identity compare-and-swap, never dereferenced by callers.
	ptr *[]provider.Provider
}

// SnapshotModelOverride captures the agent's current model override. The
// returned snapshot is opaque; pass it to RestoreModelOverride later to
// restore the captured value.
func (a *Agent) SnapshotModelOverride() ModelOverrideSnapshot {
	return ModelOverrideSnapshot{ptr: a.modelOverrides.Load()}
}

// RestoreModelOverride atomically restores the override to the value
// captured by `prev`, but only if the current override is still the one
// captured by `current` (pointer identity). If another caller has changed
// the override since `current` was captured, the restore is a no-op so
// that the concurrent change wins.
//
// This is the safe primitive for applying a temporary override around a
// scope (e.g. a skill sub-session) without clobbering changes made by
// concurrent callers such as the TUI model picker.
func (a *Agent) RestoreModelOverride(prev, current ModelOverrideSnapshot) {
	if a.modelOverrides.CompareAndSwap(current.ptr, prev.ptr) {
		slog.Debug("Restored model override", "agent", a.name)
	} else {
		slog.Debug("Model override changed concurrently; skipping restore", "agent", a.name)
	}
}

// ConfiguredModels returns the originally configured models for this agent.
// This is useful for listing available models in the TUI picker.
func (a *Agent) ConfiguredModels() []provider.Provider {
	return a.models
}

// EffectiveModels returns the providers currently in effect for this agent:
// the runtime override(s) when set, otherwise the configured models. The
// returned slice is a copy and safe for the caller to retain or mutate.
func (a *Agent) EffectiveModels() []provider.Provider {
	if overrides := a.modelOverrides.Load(); overrides != nil && len(*overrides) > 0 {
		return slices.Clone(*overrides)
	}
	return slices.Clone(a.models)
}

// FallbackModels returns the fallback models to try if the primary model fails.
func (a *Agent) FallbackModels() []provider.Provider {
	return a.fallbackModels
}

// FallbackRetries returns the number of retries per fallback model.
func (a *Agent) FallbackRetries() int {
	return a.fallbackRetries
}

// FallbackCooldown returns the duration to stick with a successful fallback
// model before retrying the primary. Returns 0 if not configured.
func (a *Agent) FallbackCooldown() time.Duration {
	return a.fallbackCooldown
}

// TitleModel returns the dedicated model configured for session-title
// generation, or nil when none was configured (in which case title
// generation reuses the agent's own model).
func (a *Agent) TitleModel() provider.Provider {
	return a.titleModel
}

// TitleModels returns the ordered list of providers to use for session-title
// generation. The dedicated title model (when configured) comes first,
// followed by the agent's current model and its fallbacks so title
// generation still succeeds if the dedicated model is unavailable. The
// result never contains nil entries.
func (a *Agent) TitleModels(ctx context.Context) []provider.Provider {
	var models []provider.Provider
	if a.titleModel != nil {
		models = append(models, a.titleModel)
	}
	if m := a.Model(ctx); m != nil {
		models = append(models, m)
	}
	return append(models, a.fallbackModels...)
}

// CompactionModel returns the dedicated model configured for session
// compaction (summary generation), or nil when none was configured (in which
// case compaction reuses the agent's own model). Unlike the primary model,
// it is not affected by runtime model switching: a /compact still runs on the
// configured compaction model after the conversation model has been changed.
func (a *Agent) CompactionModel() provider.Provider {
	return a.compactionModel
}

// CompactionThreshold returns the configured fraction of the context window
// at which proactive auto-compaction triggers, or 0 when the agent uses the
// default. Callers pass the value verbatim to [compaction.ShouldCompact],
// which maps 0 (and any out-of-range value) to the package default. Like
// CompactionModel, it is resolved at load time and not affected by runtime
// model switching.
func (a *Agent) CompactionThreshold() float64 {
	return a.compactionThreshold
}

// SessionCompaction reports whether automatic session compaction (the
// proactive threshold trigger and post-overflow auto-recovery) is enabled
// for this agent. Defaults to true; disabled only by an explicit
// `session_compaction: false` in the agent's config. Manual /compact is
// not affected by this flag.
func (a *Agent) SessionCompaction() bool {
	return !a.sessionCompactionOff
}

// Commands returns the named commands configured for this agent.
func (a *Agent) Commands() types.Commands {
	return a.commands
}

// Harness returns the external coding harness configuration for this agent.
func (a *Agent) Harness() *latest.HarnessConfig {
	return a.harness
}

func (a *Agent) HasHarness() bool {
	return a.harness != nil
}

// HarnessType returns the external harness provider type (e.g. "claude-code"),
// or an empty string when the agent is not harness-backed. It exposes the
// harness type without leaking the config struct to callers.
func (a *Agent) HarnessType() string {
	if a.harness == nil {
		return ""
	}
	return a.harness.Type
}

// Hooks returns the hooks configuration for this agent.
func (a *Agent) Hooks() *latest.HooksConfig {
	return a.hooks
}

// StructuredOutput returns the agent's structured-output configuration, or
// nil when none was configured. The runtime uses it to enforce tool-mode
// structured output; native mode is handled by the providers themselves.
func (a *Agent) StructuredOutput() *latest.StructuredOutput {
	return a.structuredOutput
}

// StructuredOutputTool returns the compiled tool-mode structured-output
// tool, building it on first use and caching both the tool and the error
// (schema compilation is not free, and the runtime asks on every turn).
// It returns (nil, nil) when the agent has no tool-mode structured output
// configured; native mode never compiles anything here.
func (a *Agent) StructuredOutputTool() (*structuredoutput.OutputTool, error) {
	if a.structuredOutputTool == nil {
		return nil, nil
	}
	return a.structuredOutputTool()
}

// Cache returns the response cache configured for this agent, or nil when
// caching is disabled.
func (a *Agent) Cache() *cache.Cache {
	return a.cache
}

// Tools returns the tools available to this agent
func (a *Agent) Tools(ctx context.Context) ([]tools.Tool, error) {
	a.ensureToolSetsAreStarted(ctx)
	return a.collectTools(ctx)
}

// StartedTools returns tools only from toolsets that have already been started,
// without triggering initialization of unstarted toolsets. This is useful for
// notifications (e.g. MCP tool list changes) that should not block on slow
// toolset startup such as RAG file indexing.
func (a *Agent) StartedTools(ctx context.Context) ([]tools.Tool, error) {
	return a.collectTools(ctx)
}

// collectTools gathers tools from all started toolsets plus static tools.
// Tool names must be unique across the whole set (providers such as
// Anthropic reject requests with duplicate tool names, and the dispatcher
// resolves calls by name), so duplicates are dropped as they are collected:
// the first toolset in configuration order wins, as documented in the MCP
// toolset docs. Each collision is surfaced to the user once per streak via
// reportCollisions. See #2251.
func (a *Agent) collectTools(ctx context.Context) ([]tools.Tool, error) {
	var agentTools []tools.Tool
	origins := make(map[string]string)
	collisions := make(map[string]string)

	collect := func(candidates []tools.Tool, origin string) {
		for _, tool := range candidates {
			if firstOrigin, exists := origins[tool.Name]; exists {
				collisions[collisionKey(tool.Name, firstOrigin, origin)] = fmt.Sprintf(
					"duplicate tool %q: kept from %s, ignored from %s (first toolset in config wins) — set a unique 'name:' on the MCP toolset or use its 'tools:' filter to disambiguate",
					tool.Name, firstOrigin, origin,
				)
				continue
			}
			origins[tool.Name] = origin
			agentTools = append(agentTools, tool)
		}
	}

	// List every started toolset concurrently — a remote MCP tools/list is a
	// network round-trip — then merge in configuration order so dedup
	// ("first toolset in config wins") and warning semantics are unchanged.
	type listResult struct {
		tools   []tools.Tool
		err     error
		started bool
	}
	results := concurrent.MapSlice(a.toolsets, func(toolSet *tools.StartableToolSet) listResult {
		// TryIsStarted: a toolset whose lifecycle operation is still in
		// flight (e.g. a wedged Start abandoned by tryStartToolSet) is
		// skipped for this turn instead of blocking the listing (#4001).
		if !toolSet.TryIsStarted() {
			return listResult{}
		}
		ts, err := toolSet.Tools(ctx)
		return listResult{tools: ts, err: err, started: true}
	})
	for i, toolSet := range a.toolsets {
		if !results[i].started {
			// Toolset not started; skip it
			continue
		}
		ta, err := results[i].tools, results[i].err
		if err != nil {
			desc := tools.DescribeToolSet(toolSet)
			// Route through the once-per-streak guard so a toolset stuck
			// returning an error (e.g. a remote MCP server replying
			// "toolset not started") surfaces a single warning per streak
			// instead of one on every conversation turn.
			if toolSet.ShouldReportListFailure() {
				slog.WarnContext(ctx, "Toolset listing failed; skipping", "agent", a.Name(), "toolset", desc, "error", err)
				a.AddToolWarning(fmt.Sprintf("%s list failed: %v", desc, err))
			} else {
				slog.DebugContext(ctx, "Toolset listing still failing; retrying next turn", "agent", a.Name(), "toolset", desc, "error", err)
			}
			continue
		}
		collect(ta, tools.DescribeToolSet(toolSet))
	}

	collect(a.tools, "agent static tools")

	a.reportCollisions(ctx, collisions)

	if a.addDescriptionParameter {
		agentTools = tools.AddDescriptionParameter(agentTools)
	}

	return agentTools, nil
}

// collisionKey identifies one (tool, kept origin, dropped origin) collision so
// the same persistent collision is reported once per streak, while a new
// collision (e.g. introduced by an MCP ToolListChanged notification) is
// reported as fresh.
func collisionKey(toolName, keptOrigin, droppedOrigin string) string {
	return toolName + "\x00" + keptOrigin + "\x00" + droppedOrigin
}

// reportCollisions surfaces duplicate-tool-name collisions to the user once
// per streak: a collision present on consecutive collectTools runs is only
// logged at debug level after the first report, and a collision that
// disappears then reappears is reported again. This mirrors the
// once-per-streak semantics of toolset start/list failure warnings so the
// user is alerted without being spammed on every conversation turn.
func (a *Agent) reportCollisions(ctx context.Context, collisions map[string]string) {
	a.collisionsMu.Lock()
	defer a.collisionsMu.Unlock()

	if len(collisions) == 0 {
		a.reportedCollisions = nil
		return
	}

	reported := make(map[string]bool, len(collisions))
	for key, msg := range collisions {
		if a.reportedCollisions[key] {
			slog.DebugContext(ctx, "Duplicate tool name still present", "agent", a.Name(), "detail", msg)
		} else {
			slog.WarnContext(ctx, "Duplicate tool name; keeping first occurrence", "agent", a.Name(), "detail", msg)
			a.AddToolWarning(msg)
		}
		reported[key] = true
	}
	a.reportedCollisions = reported
}

func (a *Agent) ToolSets() []tools.ToolSet {
	var toolSets []tools.ToolSet

	for _, ts := range a.toolsets {
		toolSets = append(toolSets, ts)
	}

	return toolSets
}

// tryStartToolSet starts one toolset without ever letting it stall the turn
// (#4001). It runs the toolset's bounded, non-blocking TryStartWithTimeout
// with the shared tools.DefaultStartTimeout budget — the same grace period
// as the runtime's startup probe — and translates the outcome:
//
//   - A Start already in flight (e.g. a startup probe that timed out upstream
//     but kept going) is skipped silently — TryStart never joins an in-flight
//     attempt — so the turn proceeds with the toolsets that are ready.
//   - A start initiated here that outlives the budget (a wedged toolset can
//     ignore cancellation) is abandoned to finish in the background and the
//     toolset is skipped for this turn — silently, since the failure
//     reporters share the toolset's single-flight lock and consulting them
//     would block on the very start we just abandoned. Its outcome is picked
//     up on a later turn.
func (a *Agent) tryStartToolSet(ctx context.Context, toolSet *tools.StartableToolSet) error {
	started, err := toolSet.TryStartWithTimeout(ctx, tools.DefaultStartTimeout)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			slog.DebugContext(ctx, "Toolset start still running; skipping for this turn", "agent", a.Name(), "toolset", tools.DescribeToolSet(toolSet), "cause", err)
			return nil
		}
		return err
	}
	if !started {
		slog.DebugContext(ctx, "Toolset start already in flight; skipping for this turn", "agent", a.Name(), "toolset", tools.DescribeToolSet(toolSet))
	}
	return nil
}

// ensureToolSetsAreStarted starts every toolset, surfacing the first
// failure of each streak as a user-visible warning and silently retrying
// on every subsequent turn. A successful Start() automatically resets the
// streak inside StartableToolSet, so a future failure is again reported
// as fresh — no recovery callback is needed here, and we deliberately do
// not surface a "now available" notice (the OAuth dialog completing or
// the model just using the tool already makes a successful start
// obvious; a follow-up notification just reads as a spurious warning).
//
// Starts run concurrently so one slow toolset (e.g. an MCP server
// handshake) doesn't delay the others; each start is non-blocking and
// bounded via tryStartToolSet, so neither a start already in flight nor a
// wedged start initiated here can stall the turn, and warnings are
// recorded in configuration order afterwards. Peer-dependent toolsets
// (e.g. the deferred aggregator, whose Start lists its source toolsets'
// tools) start in a second wave, after the toolsets they depend on have
// settled.
func (a *Agent) ensureToolSetsAreStarted(ctx context.Context) {
	var independent, dependent []int
	for i, toolSet := range a.toolsets {
		if _, ok := tools.As[tools.PeerDependent](toolSet); ok {
			dependent = append(dependent, i)
		} else {
			independent = append(independent, i)
		}
	}

	errs := make([]error, len(a.toolsets))
	for _, wave := range [][]int{independent, dependent} {
		concurrent.ForEach(wave, func(i int) {
			errs[i] = a.tryStartToolSet(ctx, a.toolsets[i])
		})
	}

	for i, toolSet := range a.toolsets {
		err := errs[i]
		if err == nil {
			continue
		}
		desc := tools.DescribeToolSet(toolSet)
		if tools.IsAuthorizationRequired(err) {
			// Recovery: previously-working toolset lost its OAuth token in the
			// background. Emit the targeted re-auth notice once per streak so the
			// user knows a dialog will appear on their next message.
			// Initial-startup auth deferral (ShouldReportRecoveryFailure==false)
			// stays silent — the dialog appears naturally on the first turn.
			if toolSet.ShouldReportRecoveryFailure() {
				slog.WarnContext(ctx, "Toolset needs re-authentication after background token rejection", "agent", a.Name(), "toolset", desc)
				a.AddToolWarning(desc + " needs re-authentication — it will prompt on your next message, or use /toolset-restart")
			}
			continue
		}
		if toolSet.ShouldReportFailure() {
			slog.WarnContext(ctx, "Toolset start failed; will retry on next turn", "agent", a.Name(), "toolset", desc, "error", err)
			a.AddToolWarning(fmt.Sprintf("%s start failed: %v", desc, err))
		} else {
			slog.DebugContext(ctx, "Toolset still unavailable; retrying next turn", "agent", a.Name(), "toolset", desc, "error", err)
		}
	}
}

// AddToolWarning records a warning generated while loading or starting toolsets.
// Warnings represent real failures the user should know about (a remote MCP
// server returning 4xx, an MCP binary missing, ...). Recoveries from a
// previous failure are intentionally not surfaced: the OAuth dialog and
// subsequent tool use already make a successful start obvious, so emitting
// a "now available" notification only adds noise.
func (a *Agent) AddToolWarning(msg string) {
	if msg == "" {
		return
	}
	a.warningsMu.Lock()
	defer a.warningsMu.Unlock()
	a.pendingWarnings = append(a.pendingWarnings, msg)
}

// DrainWarnings returns pending warnings and clears them.
func (a *Agent) DrainWarnings() []string {
	a.warningsMu.Lock()
	defer a.warningsMu.Unlock()
	warnings := a.pendingWarnings
	a.pendingWarnings = nil
	return warnings
}

func (a *Agent) StopToolSets(ctx context.Context) error {
	var errs []error
	for _, toolSet := range a.toolsets {
		// StopIfStarted checks-and-stops atomically under the toolset's
		// lifecycle lock, so a toolset that was never started is left
		// untouched, an in-flight Start that settles in time is stopped
		// rather than leaked, and a Start wedged past ctx's deadline
		// surfaces ctx.Err() instead of blocking shutdown forever (#4001).
		// One failed stop must not abandon the rest: later toolsets are
		// still stopped (an expired ctx still stops responsive ones) and
		// the failures surface together.
		if err := toolSet.StopIfStarted(ctx); err != nil {
			errs = append(errs, fmt.Errorf("failed to stop toolset %s: %w", tools.DescribeToolSet(toolSet), err))
		}
	}

	return errors.Join(errs...)
}
