package agent

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/docker/cagent/pkg/config/latest"
	"github.com/docker/cagent/pkg/config/types"
	"github.com/docker/cagent/pkg/model/provider"
	"github.com/docker/cagent/pkg/tools"
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
	noFallbackStatusCodes   []int                               // Status codes that skip fallback when using a gateway
	modelOverrides          atomic.Pointer[[]provider.Provider] // Optional model override(s) set at runtime (supports alloy)
	subAgents               []*Agent
	handoffs                []*Agent
	parents                 []*Agent
	addDate                 bool
	addEnvironmentInfo      bool
	addDescriptionParameter bool
	maxIterations           int
	numHistoryItems         int
	addPromptFiles          []string
	tools                   []tools.Tool
	commands                types.Commands
	pendingWarnings         []string
	hooks                   *latest.HooksConfig
	thinkingConfigured      bool // true if thinking_budget was explicitly set in config
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

func (a *Agent) MaxIterations() int {
	return a.maxIterations
}

func (a *Agent) NumHistoryItems() int {
	return a.numHistoryItems
}

func (a *Agent) AddPromptFiles() []string {
	return a.addPromptFiles
}

// ThinkingConfigured returns true if thinking_budget was explicitly set in the agent's config.
// This is used to initialize session thinking state - thinking is only enabled by default
// when the user explicitly configured it in their YAML.
func (a *Agent) ThinkingConfigured() bool {
	return a.thinkingConfigured
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
func (a *Agent) Model() provider.Provider {
	// Check for model override first (set via TUI model switching)
	if overrides := a.modelOverrides.Load(); overrides != nil && len(*overrides) > 0 {
		return (*overrides)[rand.Intn(len(*overrides))]
	}
	return a.models[rand.Intn(len(a.models))]
}

// SetModelOverride sets runtime model override(s) for this agent.
// The override(s) take precedence over the configured models.
// For alloy models, multiple providers can be passed and one will be randomly selected.
// Pass no arguments or nil providers to clear the override.
func (a *Agent) SetModelOverride(models ...provider.Provider) {
	// Filter out nil providers
	var validModels []provider.Provider
	for _, m := range models {
		if m != nil {
			validModels = append(validModels, m)
		}
	}

	if len(validModels) == 0 {
		a.modelOverrides.Store(nil)
		slog.Debug("Cleared model override", "agent", a.name)
	} else {
		a.modelOverrides.Store(&validModels)
		ids := make([]string, len(validModels))
		for i, m := range validModels {
			ids[i] = m.ID()
		}
		slog.Debug("Set model override", "agent", a.name, "models", ids)
	}
}

// HasModelOverride returns true if a model override is currently set.
func (a *Agent) HasModelOverride() bool {
	overrides := a.modelOverrides.Load()
	return overrides != nil && len(*overrides) > 0
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

// NoFallbackStatusCodes returns the HTTP status codes for which fallback should
// be skipped when using a models gateway.
func (a *Agent) NoFallbackStatusCodes() []int {
	return a.noFallbackStatusCodes
}

// Commands returns the named commands configured for this agent.
func (a *Agent) Commands() types.Commands {
	return a.commands
}

// Hooks returns the hooks configuration for this agent.
func (a *Agent) Hooks() *latest.HooksConfig {
	return a.hooks
}

// Tools returns the tools available to this agent
func (a *Agent) Tools(ctx context.Context) ([]tools.Tool, error) {
	a.ensureToolSetsAreStarted(ctx)

	var agentTools []tools.Tool
	for _, toolSet := range a.toolsets {
		if !toolSet.IsStarted() {
			// Toolset failed to start; skip it
			continue
		}
		ta, err := toolSet.Tools(ctx)
		if err != nil {
			desc := tools.DescribeToolSet(toolSet)
			slog.Warn("Toolset listing failed; skipping", "agent", a.Name(), "toolset", desc, "error", err)
			a.addToolWarning(fmt.Sprintf("%s list failed: %v", desc, err))
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

func (a *Agent) ensureToolSetsAreStarted(ctx context.Context) {
	for _, toolSet := range a.toolsets {
		if err := toolSet.Start(ctx); err != nil {
			desc := tools.DescribeToolSet(toolSet)
			slog.Warn("Toolset start failed; skipping", "agent", a.Name(), "toolset", desc, "error", err)
			a.addToolWarning(fmt.Sprintf("%s start failed: %v", desc, err))
			continue
		}
	}
}

// addToolWarning records a warning generated while loading or starting toolsets.
func (a *Agent) addToolWarning(msg string) {
	if msg == "" {
		return
	}
	a.pendingWarnings = append(a.pendingWarnings, msg)
}

// DrainWarnings returns pending warnings and clears them.
func (a *Agent) DrainWarnings() []string {
	if len(a.pendingWarnings) == 0 {
		return nil
	}
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
