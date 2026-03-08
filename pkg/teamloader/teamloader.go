package teamloader

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"github.com/docker/cagent/pkg/agent"
	"github.com/docker/cagent/pkg/config"
	"github.com/docker/cagent/pkg/config/latest"
	"github.com/docker/cagent/pkg/js"
	"github.com/docker/cagent/pkg/model/provider"
	"github.com/docker/cagent/pkg/model/provider/dmr"
	"github.com/docker/cagent/pkg/model/provider/options"
	"github.com/docker/cagent/pkg/modelsdev"
	"github.com/docker/cagent/pkg/permissions"
	"github.com/docker/cagent/pkg/rag"
	"github.com/docker/cagent/pkg/skills"
	"github.com/docker/cagent/pkg/team"
	"github.com/docker/cagent/pkg/tools"
	"github.com/docker/cagent/pkg/tools/builtin"
	agenttool "github.com/docker/cagent/pkg/tools/builtin/agent"
	"github.com/docker/cagent/pkg/tools/codemode"
)

var defaultMaxTokens int64 = 32000

// isThinkingBudgetDisabled returns true if the thinking budget is explicitly set to disable thinking
// (e.g., thinking_budget: 0 or thinking_budget: none).
func isThinkingBudgetDisabled(tb *latest.ThinkingBudget) bool {
	if tb == nil {
		return false
	}
	// Disabled if tokens is explicitly 0
	if tb.Tokens == 0 && tb.Effort == "" {
		return true
	}
	// Disabled if effort is "none"
	return tb.Effort == "none"
}

type loadOptions struct {
	modelOverrides  []string
	promptFiles     []string
	toolsetRegistry *ToolsetRegistry
}

type Opt func(*loadOptions) error

func WithModelOverrides(overrides []string) Opt {
	return func(opts *loadOptions) error {
		opts.modelOverrides = overrides
		return nil
	}
}

// WithPromptFiles adds additional prompt files to all agents.
// These are merged with any prompt files defined in the agent config.
func WithPromptFiles(files []string) Opt {
	return func(opts *loadOptions) error {
		opts.promptFiles = files
		return nil
	}
}

// WithToolsetRegistry allows using a custom toolset registry instead of the default
func WithToolsetRegistry(registry *ToolsetRegistry) Opt {
	return func(opts *loadOptions) error {
		opts.toolsetRegistry = registry
		return nil
	}
}

// LoadResult contains the result of loading an agent team, including
// the team and configuration needed for runtime model switching.
type LoadResult struct {
	Team      *team.Team
	Models    map[string]latest.ModelConfig
	Providers map[string]latest.ProviderConfig
	// AgentDefaultModels maps agent names to their configured default model references
	AgentDefaultModels map[string]string
}

// Load loads an agent team from the given source
func Load(ctx context.Context, agentSource config.Source, runConfig *config.RuntimeConfig, opts ...Opt) (*team.Team, error) {
	result, err := LoadWithConfig(ctx, agentSource, runConfig, opts...)
	if err != nil {
		return nil, err
	}
	return result.Team, nil
}

// LoadWithConfig loads an agent team and returns both the team and config info
// needed for runtime model switching.
func LoadWithConfig(ctx context.Context, agentSource config.Source, runConfig *config.RuntimeConfig, opts ...Opt) (*LoadResult, error) {
	var loadOpts loadOptions
	loadOpts.toolsetRegistry = NewDefaultToolsetRegistry()

	for _, o := range opts {
		if err := o(&loadOpts); err != nil {
			return nil, err
		}
	}

	// Load the agent's configuration
	cfg, err := config.Load(ctx, agentSource)
	if err != nil {
		return nil, err
	}

	// Resolve model aliases (e.g., "claude-sonnet-4-5" -> "claude-sonnet-4-5-20250929")
	// This ensures the API uses the pinned model version. The original name is preserved
	// in DisplayModel so the sidebar and other UI elements show the user-configured name.
	modelsStore, err := modelsdev.NewStore()
	if err != nil {
		slog.Debug("Failed to create modelsdev store for alias resolution", "error", err)
	} else {
		config.ResolveModelAliases(ctx, cfg, modelsStore)
	}

	// Apply model overrides from CLI flags before checking required env vars
	if err := config.ApplyModelOverrides(cfg, loadOpts.modelOverrides); err != nil {
		return nil, err
	}

	// Early check for required env vars before loading models and tools.
	env := runConfig.EnvProvider()
	if err := config.CheckRequiredEnvVars(ctx, cfg, runConfig.ModelsGateway, env); err != nil {
		return nil, err
	}

	// Create RAG managers
	parentDir := cmp.Or(agentSource.ParentDir(), runConfig.WorkingDir)
	configName := configNameFromSource(agentSource.Name())
	ragManagers, err := rag.NewManagers(ctx, cfg, rag.ManagersBuildConfig{
		ParentDir:     parentDir,
		ModelsGateway: runConfig.ModelsGateway,
		Env:           env,
		Models:        cfg.Models,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create RAG managers: %w", err)
	}

	// Load agents
	var agents []*agent.Agent
	agentsByName := make(map[string]*agent.Agent)

	autoModel := sync.OnceValue(func() latest.ModelConfig {
		return config.AutoModelConfig(ctx, runConfig.ModelsGateway, env, runConfig.DefaultModel)
	})

	expander := js.NewJsExpander(env)

	for _, agentConfig := range cfg.Agents {
		// Merge CLI prompt files with agent config prompt files, deduplicating
		promptFiles := append([]string{}, agentConfig.AddPromptFiles...)
		promptFiles = append(promptFiles, loadOpts.promptFiles...)

		seen := make(map[string]bool)
		unique := promptFiles[:0]
		for _, f := range promptFiles {
			if !seen[f] {
				seen[f] = true
				unique = append(unique, f)
			}
		}
		promptFiles = unique

		opts := []agent.Opt{
			agent.WithName(agentConfig.Name),
			agent.WithDescription(expander.Expand(ctx, agentConfig.Description, nil)),
			agent.WithWelcomeMessage(expander.Expand(ctx, agentConfig.WelcomeMessage, nil)),
			agent.WithAddDate(agentConfig.AddDate),
			agent.WithAddEnvironmentInfo(agentConfig.AddEnvironmentInfo),
			agent.WithAddDescriptionParameter(agentConfig.AddDescriptionParameter),
			agent.WithAddPromptFiles(promptFiles),
			agent.WithMaxIterations(agentConfig.MaxIterations),
			agent.WithNumHistoryItems(agentConfig.NumHistoryItems),
			agent.WithCommands(expander.ExpandCommands(ctx, agentConfig.Commands)),
			agent.WithHooks(agentConfig.Hooks),
		}

		models, thinkingConfigured, err := getModelsForAgent(ctx, cfg, &agentConfig, autoModel, runConfig)
		if err != nil {
			// Return auto model fallback errors and DMR not installed errors directly
			// without wrapping to provide cleaner messages
			if _, ok := errors.AsType[*config.AutoModelFallbackError](err); ok || errors.Is(err, dmr.ErrNotInstalled) {
				return nil, err
			}
			return nil, fmt.Errorf("failed to get models: %w", err)
		}
		for _, model := range models {
			opts = append(opts, agent.WithModel(model))
		}
		opts = append(opts, agent.WithThinkingConfigured(thinkingConfigured))

		// Load fallback models if configured
		fallbackModelRefs := agentConfig.GetFallbackModels()
		if len(fallbackModelRefs) > 0 {
			fallbackModels, err := getFallbackModelsForAgent(ctx, cfg, &agentConfig, runConfig)
			if err != nil {
				return nil, fmt.Errorf("failed to get fallback models: %w", err)
			}
			for _, model := range fallbackModels {
				opts = append(opts, agent.WithFallbackModel(model))
			}
			opts = append(opts,
				agent.WithFallbackRetries(agentConfig.GetFallbackRetries()),
				agent.WithFallbackCooldown(agentConfig.GetFallbackCooldown()),
			)
		}

		agentTools, warnings := getToolsForAgent(ctx, &agentConfig, parentDir, runConfig, loadOpts.toolsetRegistry, configName)
		if len(warnings) > 0 {
			opts = append(opts, agent.WithLoadTimeWarnings(warnings))
		}

		// Add RAG tools if agent has RAG sources
		if len(agentConfig.RAG) > 0 {
			ragTools := createRAGToolsForAgent(&agentConfig, ragManagers)
			agentTools = append(agentTools, ragTools...)
		}

		// Add skills toolset if skills are enabled
		if agentConfig.Skills.Enabled() {
			loadedSkills := skills.Load(agentConfig.Skills.Sources)
			if len(loadedSkills) > 0 {
				agentTools = append(agentTools, builtin.NewSkillsToolset(loadedSkills))
			}
		}

		opts = append(opts, agent.WithToolSets(agentTools...))

		ag := agent.New(agentConfig.Name, agentConfig.Instruction, opts...)
		agents = append(agents, ag)
		agentsByName[agentConfig.Name] = ag
	}

	// Connect sub-agents and handoff agents.
	// externalAgents caches agents loaded from external references (OCI/URL),
	// keyed by the original reference string, to avoid loading the same
	// external agent twice. This is kept separate from agentsByName to
	// prevent external agents from shadowing locally-defined agents.
	externalAgents := make(map[string]*agent.Agent)
	for _, agentConfig := range cfg.Agents {
		a, exists := agentsByName[agentConfig.Name]
		if !exists {
			continue
		}

		subAgents, err := resolveAgentRefs(ctx, agentConfig.SubAgents, agentsByName, externalAgents, &agents, runConfig, &loadOpts)
		if err != nil {
			return nil, fmt.Errorf("agent '%s': resolving sub-agents: %w", agentConfig.Name, err)
		}
		if len(subAgents) > 0 {
			agent.WithSubAgents(subAgents...)(a)
		}

		handoffs, err := resolveAgentRefs(ctx, agentConfig.Handoffs, agentsByName, externalAgents, &agents, runConfig, &loadOpts)
		if err != nil {
			return nil, fmt.Errorf("agent '%s': resolving handoffs: %w", agentConfig.Name, err)
		}
		if len(handoffs) > 0 {
			agent.WithHandoffs(handoffs...)(a)
		}
	}

	// Create permissions checker from config
	permChecker := permissions.NewChecker(cfg.Permissions)

	// Build agent default models map
	agentDefaultModels := make(map[string]string)
	for _, agent := range cfg.Agents {
		if agent.Model != "" {
			agentDefaultModels[agent.Name] = agent.Model
		}
	}

	return &LoadResult{
		Team: team.New(
			team.WithAgents(agents...),
			team.WithRAGManagers(ragManagers),
			team.WithPermissions(permChecker),
		),
		Models:             cfg.Models,
		Providers:          cfg.Providers,
		AgentDefaultModels: agentDefaultModels,
	}, nil
}

func getModelsForAgent(ctx context.Context, cfg *latest.Config, a *latest.AgentConfig, autoModelFn func() latest.ModelConfig, runConfig *config.RuntimeConfig) ([]provider.Provider, bool, error) {
	var models []provider.Provider
	thinkingConfigured := false

	for name := range strings.SplitSeq(a.Model, ",") {
		modelCfg, exists := cfg.Models[name]
		isAutoModel := false
		if !exists {
			if name == "auto" {
				modelCfg = autoModelFn()
				isAutoModel = true
			} else {
				return nil, false, fmt.Errorf("model '%s' not found in configuration", name)
			}
		}
		modelCfg.Name = name

		// Check if thinking_budget was explicitly configured BEFORE provider defaults are applied.
		// This is used to initialize session thinking state - thinking is only enabled by default
		// when the user explicitly configured it in their YAML.
		if modelCfg.ThinkingBudget != nil && !isThinkingBudgetDisabled(modelCfg.ThinkingBudget) {
			thinkingConfigured = true
		}

		// Use max_tokens from config if specified, otherwise look up from models.dev
		maxTokens := &defaultMaxTokens
		if modelCfg.MaxTokens != nil {
			maxTokens = modelCfg.MaxTokens
		} else {
			modelsStore, err := modelsdev.NewStore()
			if err != nil {
				return nil, false, err
			}
			m, err := modelsStore.GetModel(ctx, modelCfg.Provider+"/"+modelCfg.Model)
			if err == nil {
				maxTokens = &m.Limit.Output
			}
		}

		opts := []options.Opt{
			options.WithGateway(runConfig.ModelsGateway),
			options.WithStructuredOutput(a.StructuredOutput),
			options.WithProviders(cfg.Providers),
		}
		if maxTokens != nil {
			opts = append(opts, options.WithMaxTokens(*maxTokens))
		}

		// Pass the full models map for routing rules to resolve model references
		model, err := provider.NewWithModels(ctx,
			&modelCfg,
			cfg.Models,
			runConfig.EnvProvider(),
			opts...,
		)
		if err != nil {
			// Return a cleaner error message for auto model selection failures
			if isAutoModel {
				return nil, false, &config.AutoModelFallbackError{}
			}
			return nil, false, err
		}
		models = append(models, model)
	}

	return models, thinkingConfigured, nil
}

// getFallbackModelsForAgent returns fallback providers for an agent based on its fallback configuration.
// It uses the same resolution logic as primary models (named model, inline provider/model format).
func getFallbackModelsForAgent(ctx context.Context, cfg *latest.Config, a *latest.AgentConfig, runConfig *config.RuntimeConfig) ([]provider.Provider, error) {
	var fallbackModels []provider.Provider

	for _, name := range a.GetFallbackModels() {
		modelCfg, exists := cfg.Models[name]
		if !exists {
			// Try parsing as inline provider/model format (e.g., "openai/gpt-4o")
			providerName, modelName, ok := strings.Cut(name, "/")
			if !ok {
				return nil, fmt.Errorf("fallback model '%s' not found in configuration and is not a valid provider/model format", name)
			}
			modelCfg = latest.ModelConfig{
				Provider: providerName,
				Model:    modelName,
			}
		}
		modelCfg.Name = name

		// Use max_tokens from config if specified, otherwise look up from models.dev
		maxTokens := &defaultMaxTokens
		if modelCfg.MaxTokens != nil {
			maxTokens = modelCfg.MaxTokens
		} else {
			modelsStore, err := modelsdev.NewStore()
			if err != nil {
				return nil, err
			}
			m, err := modelsStore.GetModel(ctx, modelCfg.Provider+"/"+modelCfg.Model)
			if err == nil {
				maxTokens = &m.Limit.Output
			}
		}

		opts := []options.Opt{
			options.WithGateway(runConfig.ModelsGateway),
			options.WithStructuredOutput(a.StructuredOutput),
			options.WithProviders(cfg.Providers),
		}
		if maxTokens != nil {
			opts = append(opts, options.WithMaxTokens(*maxTokens))
		}

		// Pass the full models map for routing rules to resolve model references
		model, err := provider.NewWithModels(ctx,
			&modelCfg,
			cfg.Models,
			runConfig.EnvProvider(),
			opts...,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create fallback model '%s': %w", name, err)
		}
		fallbackModels = append(fallbackModels, model)
	}

	return fallbackModels, nil
}

// getToolsForAgent returns the tool definitions for an agent based on its configuration
func getToolsForAgent(ctx context.Context, a *latest.AgentConfig, parentDir string, runConfig *config.RuntimeConfig, registry *ToolsetRegistry, configName string) ([]tools.ToolSet, []string) {
	var (
		toolSets []tools.ToolSet
		warnings []string
	)

	deferredToolset := builtin.NewDeferredToolset()

	for i := range a.Toolsets {
		toolset := a.Toolsets[i]

		tool, err := registry.CreateTool(ctx, toolset, parentDir, runConfig, configName)
		if err != nil {
			// Collect error but continue loading other toolsets
			slog.Warn("Toolset configuration failed; skipping", "type", toolset.Type, "ref", toolset.Ref, "command", toolset.Command, "error", err)
			warnings = append(warnings, fmt.Sprintf("toolset %s failed: %v", toolset.Type, err))
			continue
		}

		wrapped := WithToolsFilter(tool, toolset.Tools...)
		wrapped = WithInstructions(wrapped, toolset.Instruction)
		wrapped = WithToon(wrapped, toolset.Toon)

		// Handle deferred tools
		if !toolset.Defer.IsEmpty() {
			deferredToolset.AddSource(wrapped, toolset.Defer.DeferAll, toolset.Defer.Tools)
			if toolset.Defer.DeferAll {
				// Don't add the wrapped toolset to toolSets - all its tools are deferred
				// TODO: maybe we _do_ want to add this toolset since it has instructions?
				continue
			} else {
				wrapped = WithToolsExcludeFilter(wrapped, toolset.Defer.Tools...)
			}
		}

		toolSets = append(toolSets, wrapped)
	}

	if deferredToolset.HasSources() {
		toolSets = append(toolSets, deferredToolset)
	}

	if len(a.SubAgents) > 0 {
		toolSets = append(toolSets, builtin.NewTransferTaskTool(), agenttool.NewToolSet())
	}
	if len(a.Handoffs) > 0 {
		toolSets = append(toolSets, builtin.NewHandoffTool())
	}

	// Wrap all tools in a single Code Mode toolset.
	// This allows the agent to call multiple tools in a single response.
	// It also allows to combine the results of multiple tools in a single response.
	if a.CodeModeTools || runConfig.GlobalCodeMode {
		toolSets = []tools.ToolSet{codemode.Wrap(toolSets...)}
	}

	return toolSets, warnings
}

// configNameFromSource extracts a clean config name from a source name.
// The result is "<basename>-<hash>" where basename comes from the file name
// (e.g. "memory_agent" from "/path/to/memory_agent.yaml") and hash is a short
// SHA-256 of the full source name to prevent collisions between identically
// named configs in different directories.
func configNameFromSource(sourceName string) string {
	base := filepath.Base(sourceName)
	ext := filepath.Ext(base)
	if ext != "" {
		base = base[:len(base)-len(ext)]
	}
	if base == "" || base == "." || base == ".." {
		base = "default"
	}
	h := sha256.Sum256([]byte(sourceName))
	return base + "-" + hex.EncodeToString(h[:4])
}

// resolveAgentRefs resolves a list of agent references to agent instances.
// References that match a locally-defined agent name are looked up directly.
// References that are external (OCI or URL) are loaded on-demand and cached
// in externalAgents so the same reference isn't loaded twice.
func resolveAgentRefs(
	ctx context.Context,
	refs []string,
	agentsByName map[string]*agent.Agent,
	externalAgents map[string]*agent.Agent,
	agents *[]*agent.Agent,
	runConfig *config.RuntimeConfig,
	loadOpts *loadOptions,
) ([]*agent.Agent, error) {
	resolved := make([]*agent.Agent, 0, len(refs))
	for _, ref := range refs {
		// First, try local agents by name.
		if a, ok := agentsByName[ref]; ok {
			resolved = append(resolved, a)
			continue
		}

		// Then, check whether this ref was already loaded as an external agent.
		if a, ok := externalAgents[ref]; ok {
			resolved = append(resolved, a)
			continue
		}

		if !config.IsExternalReference(ref) {
			continue
		}

		a, err := loadExternalAgent(ctx, ref, runConfig, loadOpts)
		if err != nil {
			return nil, fmt.Errorf("loading %q: %w", ref, err)
		}
		*agents = append(*agents, a)
		externalAgents[ref] = a
		resolved = append(resolved, a)
	}
	return resolved, nil
}

// maxExternalDepth is the maximum nesting depth for loading external agents.
// This prevents infinite recursion when external agents reference each other.
const maxExternalDepth = 10

// loadExternalAgent loads an agent from an external reference (OCI or URL).
// It resolves the reference, loads its config, and returns the default agent.
func loadExternalAgent(ctx context.Context, ref string, runConfig *config.RuntimeConfig, loadOpts *loadOptions) (*agent.Agent, error) {
	depth := externalDepthFromContext(ctx)
	if depth >= maxExternalDepth {
		return nil, fmt.Errorf("maximum external agent nesting depth (%d) exceeded — check for circular references", maxExternalDepth)
	}

	source, err := config.Resolve(ref, runConfig.EnvProvider())
	if err != nil {
		return nil, err
	}

	var opts []Opt
	if loadOpts.toolsetRegistry != nil {
		opts = append(opts, WithToolsetRegistry(loadOpts.toolsetRegistry))
	}

	result, err := Load(contextWithExternalDepth(ctx, depth+1), source, runConfig, opts...)
	if err != nil {
		return nil, err
	}

	return result.DefaultAgent()
}

// contextKey is an unexported type for context keys defined in this package.
type contextKey int

// externalDepthKey is the context key for tracking external agent loading depth.
var externalDepthKey contextKey

func externalDepthFromContext(ctx context.Context) int {
	if v, ok := ctx.Value(externalDepthKey).(int); ok {
		return v
	}
	return 0
}

func contextWithExternalDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, externalDepthKey, depth)
}

// createRAGToolsForAgent creates RAG tools for an agent, one for each referenced RAG source
func createRAGToolsForAgent(agentConfig *latest.AgentConfig, allManagers map[string]*rag.Manager) []tools.ToolSet {
	if len(agentConfig.RAG) == 0 {
		return nil
	}

	var ragTools []tools.ToolSet

	for _, ragName := range agentConfig.RAG {
		mgr, exists := allManagers[ragName]
		if !exists {
			slog.Error("RAG source not found", "rag_source", ragName)
			continue
		}

		// Use custom tool name if configured, otherwise use the RAG source name
		toolName := cmp.Or(mgr.ToolName(), ragName)

		// Create a separate tool for this RAG source
		ragTool := builtin.NewRAGTool(mgr, toolName)

		ragTools = append(ragTools, ragTool)

		slog.Debug("Created RAG tool for agent",
			"rag_source", ragName,
			"tool_name", toolName,
			"manager_name", mgr.Name(),
			"description", mgr.Description(),
			"instruction", mgr.ToolInstruction())
	}

	return ragTools
}
