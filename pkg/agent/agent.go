package agent

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker-agent/pkg/cache"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/tools"
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
	modelOverrides          atomic.Pointer[[]provider.Provider] // Optional model override(s) set at runtime (supports alloy)
	subAgents               []*Agent
	handoffs                []*Agent
	parents                 []*Agent
	addDate                 bool
	addEnvironmentInfo      bool
	addDescriptionParameter bool
	redactSecrets           bool
	maxIterations           int
	maxConsecutiveToolCalls int
	maxOldToolCallTokens    int
	numHistoryItems         int
	addPromptFiles          []string
	tools                   []tools.Tool
	commands                types.Commands
	hooks                   *latest.HooksConfig
	cache                   *cache.Cache

	// warningsMu guards pendingWarnings. AddToolWarning and DrainWarnings
	// may be called concurrently from the runtime loop, the MCP server,
	// the TUI and session manager.
	warningsMu      sync.Mutex
	pendingWarnings []string
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

func (a *Agent) MaxConsecutiveToolCalls() int {
	return a.maxConsecutiveToolCalls
}

func (a *Agent) MaxOldToolCallTokens() int {
	return a.maxOldToolCallTokens
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

// Commands returns the named commands configured for this agent.
func (a *Agent) Commands() types.Commands {
	return a.commands
}

// Hooks returns the hooks configuration for this agent.
func (a *Agent) Hooks() *latest.HooksConfig {
	return a.hooks
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
func (a *Agent) collectTools(ctx context.Context) ([]tools.Tool, error) {
	var agentTools []tools.Tool
	for _, toolSet := range a.toolsets {
		if !toolSet.IsStarted() {
			// Toolset not started; skip it
			continue
		}
		ta, err := toolSet.Tools(ctx)
		if err != nil {
			desc := tools.DescribeToolSet(toolSet)
			slog.WarnContext(ctx, "Toolset listing failed; skipping", "agent", a.Name(), "toolset", desc, "error", err)
			a.AddToolWarning(fmt.Sprintf("%s list failed: %v", desc, err))
			continue
		}
		agentTools = append(agentTools, ta...)
	}

	agentTools = append(agentTools, a.tools...)

	if a.addDescriptionParameter {
		agentTools = tools.AddDescriptionParameter(agentTools)
	}

	return agentTools, nil
}

func (a *Agent) ToolSets() []tools.ToolSet {
	var toolSets []tools.ToolSet

	for _, ts := range a.toolsets {
		toolSets = append(toolSets, ts)
	}

	return toolSets
}

// ensureToolSetsAreStarted starts every toolset, surfacing the first
// failure of each streak as a user-visible warning and silently retrying
// on every subsequent turn. A successful Start() automatically resets the
// streak inside StartableToolSet, so a future failure is again reported
// as fresh — no recovery callback is needed here, and we deliberately do
// not surface a "now available" notice (the OAuth dialog completing or
// the model just using the tool already makes a successful start
// obvious; a follow-up notification just reads as a spurious warning).
func (a *Agent) ensureToolSetsAreStarted(ctx context.Context) {
	for _, toolSet := range a.toolsets {
		err := toolSet.Start(ctx)
		if err == nil {
			continue
		}
		desc := tools.DescribeToolSet(toolSet)
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
	a.pendingWarnings = append(a.pendingWarnings, msg)
	a.warningsMu.Unlock()
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
	for _, toolSet := range a.toolsets {
		// Only stop toolsets that were successfully started
		if !toolSet.IsStarted() {
			continue
		}

		if err := toolSet.Stop(ctx); err != nil {
			return fmt.Errorf("failed to stop toolset: %w", err)
		}
	}

	return nil
}
