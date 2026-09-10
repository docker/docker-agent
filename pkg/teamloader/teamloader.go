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
	"slices"
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/gateway"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/dmr/dmrmodels"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/handoff"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
)

var defaultMaxTokens int64 = 32000

type loadOptions struct {
	workingDir       string
	modelOverrides   []string
	promptFiles      []string
	toolsetRegistry  ToolsetRegistry
	providerRegistry *provider.Registry
	sourceResolver   SourceResolver
	newExpander      func(environment.Provider) Expander
	codeMode         func(...tools.ToolSet) tools.ToolSet
	toon             func(tools.ToolSet, string) tools.ToolSet
	newDeferred      func() DeferredToolSet
	modelOpts        []options.Opt
	strict           bool
	features         []config.Feature
}

type Opt func(*loadOptions) error

// WithWorkingDir overrides the working directory toolsets are built with,
// without touching the caller's RuntimeConfig. Callers that share one
// RuntimeConfig across concurrent loads (the API server, one per session)
// need this to keep each session's shell, filesystem and git tools rooted in
// that session's directory.
func WithWorkingDir(dir string) Opt {
	return func(opts *loadOptions) error {
		opts.workingDir = dir
		return nil
	}
}

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

// WithToolsetRegistry allows using a custom toolset registry instead of the default.
func WithToolsetRegistry(registry ToolsetRegistry) Opt {
	return func(opts *loadOptions) error {
		opts.toolsetRegistry = registry
		return nil
	}
}

// WithProviderRegistry allows using a custom model provider registry instead of the default.
func WithProviderRegistry(registry *provider.Registry) Opt {
	return func(opts *loadOptions) error {
		if registry != nil {
			opts.providerRegistry = registry
		}
		return nil
	}
}

// WithModelOptions appends caller-supplied [options.Opt] values to every model
// client teamloader constructs for this load: primary, fallback, title, and
// compaction models, as well as models built while loading external
// (OCI/URL-referenced) sub-agents. Use this to thread cross-cutting model
// configuration — most notably options.WithHTTPTransportWrapper, which lets an
// embedder authenticate every outbound LLM request (regardless of provider)
// without depending on provider-specific environment variables or
// environment.IsTrustedDockerURL. The opts are appended after teamloader's own
// built-in opts (options.WithGateway, options.WithStructuredOutput, etc.), so
// they take precedence for any option that both sides set.
func WithModelOptions(opts ...options.Opt) Opt {
	return func(o *loadOptions) error {
		o.modelOpts = append(o.modelOpts, opts...)
		return nil
	}
}

// SourceResolver turns an external agent reference (OCI reference or URL)
// into a source. pkg/config/sources.Resolve is the full-featured
// implementation.
type SourceResolver func(ref string, env environment.Provider) (config.Source, error)

// WithSourceResolver enables sub_agents, handoffs and force_handoff entries
// that reference agents outside the config (OCI references, URLs). Without
// it such references fail to load: teamloader deliberately has no default so
// embedders only link the source types they use.
func WithSourceResolver(resolver SourceResolver) Opt {
	return func(opts *loadOptions) error {
		opts.sourceResolver = resolver
		return nil
	}
}

// WithCodeMode enables `code_mode_tools` (and RuntimeConfig.GlobalCodeMode):
// wrap is applied to an agent's toolsets so the model calls them from a single
// JavaScript tool. Pass codemode.Wrap from pkg/tools/codemode. Without it,
// configs asking for code mode fail to load.
func WithCodeMode(wrap func(toolSets ...tools.ToolSet) tools.ToolSet) Opt {
	return func(opts *loadOptions) error {
		opts.codeMode = wrap
		return nil
	}
}

// WithToon enables the `toon` toolset field, which re-encodes matching tools'
// JSON output in the compact TOON format. Pass toon.Wrap from pkg/tools/toon.
// Without it, configs using `toon` fail to load.
func WithToon(wrap func(inner tools.ToolSet, spec string) tools.ToolSet) Opt {
	return func(opts *loadOptions) error {
		opts.toon = wrap
		return nil
	}
}

// DeferredToolSet collects tools declared with `defer` so the model discovers
// and activates them on demand; pkg/tools/builtin/deferred implements it.
type DeferredToolSet interface {
	tools.ToolSet
	AddSource(toolset tools.ToolSet, deferAll bool, toolNames []string)
	HasSources() bool
}

// WithDeferredTools enables the `defer` toolset field. Pass deferred.New from
// pkg/tools/builtin/deferred. Without it, configs using `defer` fail to load.
func WithDeferredTools[D DeferredToolSet](newDeferred func() D) Opt {
	return func(opts *loadOptions) error {
		opts.newDeferred = func() DeferredToolSet { return newDeferred() }
		return nil
	}
}

// WithStrict rejects configs that rely on anything the application did not
// enable: a model provider missing from the provider registry, a toolset type
// missing from the toolset registry, or a [config.Feature] not listed here.
// Every unmet requirement is reported in one error before any model or
// toolset is built (see [config.Requires]). Without it, unknown toolset types
// are load-time warnings and unknown providers fail when their model is
// built. External agents loaded through [config.FeatureExternalAgents] are
// checked with the same rules.
func WithStrict(features ...config.Feature) Opt {
	return func(opts *loadOptions) error {
		opts.strict = true
		opts.features = append(opts.features, features...)
		return nil
	}
}

// LoadResult contains the result of loading an agent team, including
// the team and configuration needed for runtime model switching.
type LoadResult struct {
	Team      *team.Team
	Models    map[string]latest.ModelConfig
	Providers map[string]latest.ProviderConfig
	// ProviderRegistry is the registry used to instantiate model providers for this load.
	ProviderRegistry *provider.Registry
	// AgentDefaultModels maps agent names to their configured default model references
	AgentDefaultModels map[string]string
	// Budget is the manifest's run-wide budget, or nil when the manifest
	// sets no run-wide ceiling. It is per-run rather than per-agent, so it
	// lives on the load result next to the team rather than on any
	// individual agent.
	Budget *latest.BudgetConfig
	// Budgets are the manifest's named budget definitions, and
	// AgentBudgets maps each agent to the budget names it declared. A name
	// referenced by several agents is one shared pot.
	Budgets      map[string]latest.BudgetConfig
	AgentBudgets map[string][]string

	// EncryptedConfig is the encrypted agent config in effect for this load:
	// either the explicit --encrypted-config / env value, or the one captured
	// from the X-Cagent-Encrypted-Config response header when the agent YAML
	// was fetched from a trusted Docker URL. Empty when neither applies.
	EncryptedConfig string
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
func LoadWithConfig(ctx context.Context, agentSource config.Source, runConfig *config.RuntimeConfig, opts ...Opt) (result *LoadResult, err error) {
	// Cold-start path: parses config, resolves model aliases, may pull
	// referenced sub-agents over the network, and starts every toolset.
	// All synchronous from the caller's perspective. The span makes the
	// breakdown attributable when first-use latency is high.
	ctx, span := otel.Tracer("github.com/docker/docker-agent/pkg/teamloader").Start(
		ctx, "teamloader.load",
		trace.WithSpanKind(trace.SpanKindInternal),
	)
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	var loadOpts loadOptions
	loadOpts.toolsetRegistry = NewDefaultToolsetRegistry()
	loadOpts.providerRegistry = provider.DefaultRegistry()
	loadOpts.newExpander = newEnvExpander

	for _, o := range opts {
		if err := o(&loadOpts); err != nil {
			return nil, err
		}
	}

	// Toolsets read runConfig.WorkingDir, and the load below writes the
	// resolved models, providers and provider registry back onto runConfig.
	// Callers that load several agents from one RuntimeConfig (the API server
	// shares a single one across concurrent sessions) must not see those
	// writes, so take a copy when an explicit working directory is supplied.
	if loadOpts.workingDir != "" && loadOpts.workingDir != runConfig.WorkingDir {
		runConfig = runConfig.Clone()
		runConfig.WorkingDir = loadOpts.workingDir
	}

	// Load the agent's configuration
	cfg, err := config.Load(ctx, agentSource, config.WithFlavors(runConfig.Flavors...))
	if err != nil {
		return nil, err
	}

	// When the agent YAML was fetched from a trusted Docker URL that returned
	// the encrypted config in a response header, adopt it so it is forwarded to
	// the Docker models gateway on model requests. An explicit --encrypted-config
	// (or its env var) always wins.
	if runConfig.EncryptedConfig == "" {
		if ecs, ok := agentSource.(config.EncryptedConfigSource); ok {
			if enc := ecs.EncryptedConfig(); enc != "" {
				runConfig.EncryptedConfig = enc
			}
		}
	}
	if cfg != nil {
		span.SetAttributes(
			attribute.Int("cagent.teamloader.agent_count", len(cfg.Agents)),
			attribute.Int("cagent.teamloader.model_count", len(cfg.Models)),
		)
	}

	// Merge user-level provider definitions (seeded into the runtime config
	// from the user config file) so custom providers registered via
	// `docker agent setup` resolve in every run, including inline
	// `--model myprovider/mymodel` overrides. Agent-file definitions win.
	config.MergeGlobalProviders(cfg, runConfig.Providers)

	// Resolve model aliases (e.g., "claude-sonnet-4-5" -> "claude-sonnet-4-5-20250929")
	// This ensures the API uses the pinned model version. The original name is preserved
	// in DisplayModel so the sidebar and other UI elements show the user-configured name.
	modelsStore, err := runConfig.ModelsDevStore()
	if err != nil {
		slog.DebugContext(ctx, "Failed to create modelsdev store for alias resolution", "error", err)
	}

	// Apply model overrides from CLI flags before checking required env vars
	if err := config.ApplyModelOverrides(cfg, loadOpts.modelOverrides); err != nil {
		return nil, err
	}

	// Strict mode audits the config before anything reaches the network or
	// the environment, so an unsupported provider is reported as such rather
	// than as a missing credential. Models whose provider depends on the
	// environment (first_available, auto) are checked once resolved below.
	if loadOpts.strict {
		reqs := config.Requires(cfg)
		if err := reqs.Check(loadOpts.providerRegistry.Has, loadOpts.toolsetRegistry.Has, loadOpts.features); err != nil {
			return nil, err
		}
	}

	// Toolsets referencing an MCP catalog server (ref: docker:...) need the
	// catalog to be built. Kick the fetch off now so its network round-trip
	// overlaps model and environment resolution instead of stalling toolset
	// creation later in this load.
	if configUsesCatalogRefs(cfg) {
		gateway.Prefetch(ctx)
	}

	// Early check for required env vars before loading models and tools.
	env := runConfig.EnvProvider()

	// Snapshot which models are `first_available` selectors before resolution
	// rewrites them in place, so we can prefer locally-available DMR models for
	// any selector that falls back to Docker Model Runner.
	firstAvailableSelectors := map[string]bool{}
	for name, m := range cfg.Models {
		if m.IsFirstAvailable() {
			firstAvailableSelectors[name] = true
		}
	}

	// Resolve `first_available` model selectors into concrete provider/model
	// definitions now that the environment is available, so the rest of the
	// pipeline sees regular model definitions.
	if err := config.ResolveFirstAvailableModels(ctx, cfg, runConfig.ModelsGateway, env); err != nil {
		return nil, err
	}

	// For selectors that fell back to Docker Model Runner, prefer a model the
	// user already pulled over forcing an on-demand pull of the default. The
	// returned set names selectors with no usable local model, so an
	// initialization failure surfaces a "no model available" fallback rather
	// than an opaque pull error.
	dmrFallbackSelectors := config.PreferLocalDMRModels(ctx, cfg, firstAvailableSelectors, dmrmodels.ListModels)

	if modelsStore != nil {
		config.ResolveModelAliases(ctx, cfg, modelsStore)
	}

	if err := config.CheckRequiredEnvVars(ctx, cfg, runConfig.ModelsGateway, env); err != nil {
		return nil, err
	}

	autoModel := sync.OnceValue(func() latest.ModelConfig {
		return config.AutoModelConfig(ctx, runConfig.ModelsGateway, env, runConfig.DefaultModel, dmrmodels.ListModels)
	})

	if loadOpts.strict {
		if err := checkResolvedModels(cfg, firstAvailableSelectors, &loadOpts, autoModel); err != nil {
			return nil, err
		}
	}

	// Make model definitions available to toolset creators (e.g., RAG reranking)
	runConfig.Models = cfg.Models
	runConfig.Providers = cfg.Providers
	// Share the resolved provider registry so toolsets that build providers at
	// load time (e.g. RAG embeddings/reranking) use the same one as agent models.
	runConfig.ProviderRegistry = loadOpts.providerRegistry

	// Load agents
	workingDir := runConfig.WorkingDir
	parentDir := cmp.Or(agentSource.ParentDir(), workingDir)
	configName := configNameFromSource(agentSource.Name())
	var agents []*agent.Agent
	agentsByName := make(map[string]*agent.Agent)

	expander := loadOpts.newExpander(env)

	globalHooks := runConfig.GlobalHooks
	cliHooks := runConfig.CLIHooks()

	for _, agentConfig := range cfg.Agents {
		// Merge CLI prompt files with agent config prompt files, deduplicating
		promptFiles := slices.Concat(agentConfig.AddPromptFiles, loadOpts.promptFiles)

		seen := make(map[string]bool)
		unique := make([]string, 0, len(promptFiles))
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
			agent.WithRedactSecrets(agentConfig.RedactSecretsEnabled()),
			agent.WithSafety(agentConfig.Safety),
			agent.WithAddPromptFiles(promptFiles),
			agent.WithAddPromptFilesDepth(agentConfig.AddPromptFilesDepth),
			agent.WithMaxIterations(agentConfig.MaxIterations),
			agent.WithMaxConsecutiveToolCalls(agentConfig.MaxConsecutiveToolCalls),
			agent.WithMaxOldToolCallTokens(agentConfig.MaxOldToolCallTokens),
			agent.WithMaxToolResultTokens(agentConfig.MaxToolResultTokens),
			agent.WithNumHistoryItems(agentConfig.NumHistoryItems),
			agent.WithSessionCompaction(agentConfig.SessionCompactionEnabled()),
			agent.WithCommands(expander.ExpandCommands(ctx, agentConfig.Commands)),
			agent.WithHooks(config.MergeHooks(config.MergeHooks(agentConfig.Hooks, globalHooks), cliHooks)),
			agent.WithStructuredOutput(agentConfig.StructuredOutput),
		}

		if agentConfig.Cache != nil && agentConfig.Cache.Enabled {
			c, err := buildAgentCache(agentConfig.Name, agentConfig.Cache, parentDir)
			if err != nil {
				return nil, err
			}
			opts = append(opts, agent.WithCache(c))
		}

		if agentConfig.Harness != nil {
			harnessCfg := *agentConfig.Harness
			if harnessCfg.Model == "" {
				harnessCfg.Model = agentConfig.Model
			}
			opts = append(opts, agent.WithHarness(&harnessCfg))
		} else {
			models, err := getModelsForAgent(ctx, cfg, &agentConfig, autoModel, dmrFallbackSelectors, runConfig, loadOpts.providerRegistry, loadOpts.modelOpts)
			if err != nil {
				// Return auto model fallback errors, DMR not installed errors and
				// model pull/availability errors (which carry their own guidance
				// via ModelPullErrorSummary) directly without wrapping to provide
				// cleaner, actionable messages.
				_, isAuto := errors.AsType[*config.AutoModelFallbackError](err)
				_, hasSummary := errors.AsType[interface {
					error
					ModelPullErrorSummary() string
				}](err)
				if isAuto || hasSummary || errors.Is(err, dmrmodels.ErrNotInstalled) {
					return nil, err
				}
				return nil, fmt.Errorf("failed to get models: %w", err)
			}
			for _, model := range models {
				opts = append(opts, agent.WithModel(model))
			}

			// Load fallback models if configured
			fallbackModelRefs := agentConfig.GetFallbackModels()
			if len(fallbackModelRefs) > 0 {
				fallbackModels, err := getFallbackModelsForAgent(ctx, cfg, &agentConfig, runConfig, loadOpts.providerRegistry, loadOpts.modelOpts)
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

			// A model may delegate session-title generation to another model.
			titleModel, err := getTitleModelForAgent(ctx, cfg, &agentConfig, runConfig, loadOpts.providerRegistry, loadOpts.modelOpts)
			if err != nil {
				return nil, fmt.Errorf("failed to get title model: %w", err)
			}
			if titleModel != nil {
				opts = append(opts, agent.WithTitleModel(titleModel))
			}

			// A model may delegate session compaction (summary generation) to
			// another, cheaper/faster model.
			compactionModel, err := getCompactionModelForAgent(ctx, cfg, &agentConfig, runConfig, loadOpts.providerRegistry, loadOpts.modelOpts)
			if err != nil {
				return nil, fmt.Errorf("failed to get compaction model: %w", err)
			}
			if compactionModel != nil {
				opts = append(opts, agent.WithCompactionModel(compactionModel))
			}

			if threshold := compactionThresholdForAgent(cfg, &agentConfig); threshold != nil {
				opts = append(opts, agent.WithCompactionThreshold(*threshold))
			}
		}

		agentTools, warnings, err := getToolsForAgent(ctx, &agentConfig, parentDir, runConfig, configName, &loadOpts, expander)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", agentConfig.Name, err)
		}
		if len(warnings) > 0 {
			opts = append(opts, agent.WithLoadTimeWarnings(warnings))
		}

		// Add skills toolset if skills are enabled
		if agentConfig.Skills.Enabled() {
			loadedSkills := skills.Load(ctx, agentConfig.Skills.Sources)
			loadedSkills = filterSkillsByName(loadedSkills, agentConfig.Skills.Include)
			// Inline skills are defined in the agent config itself; they are
			// always exposed and never subject to the include filter.
			loadedSkills = overrideWithInlineSkills(loadedSkills, inlineSkills(agentConfig.Skills.Inline))
			if len(loadedSkills) > 0 {
				skillSet := skillstool.New(loadedSkills, workingDir)
				// Resolve the additional toolsets each fork skill exposes in
				// its sub-session from the top-level toolsets section.
				forkToolSets, forkWarnings, err := forkSkillToolSets(ctx, cfg, &agentConfig, loadedSkills, parentDir, runConfig, configName, &loadOpts, expander)
				if err != nil {
					return nil, fmt.Errorf("agent %s: %w", agentConfig.Name, err)
				}
				if len(forkToolSets) > 0 {
					skillSet.SetForkToolSets(forkToolSets)
				}
				if len(forkWarnings) > 0 {
					opts = append(opts, agent.WithLoadTimeWarnings(forkWarnings))
				}
				agentTools = append(agentTools, skillSet)
			}
		}

		opts = append(opts, agent.WithToolSets(agentTools...))

		ag := agent.New(agentConfig.Name, expander.Expand(ctx, agentConfig.Instruction, nil), opts...)

		// Tool-mode structured output needs a compilable JSON schema; catch a
		// broken one at load time instead of on the first model turn. Going
		// through the agent's lazy cache also pre-compiles the tool the
		// runtime will reuse on every turn.
		if _, err := ag.StructuredOutputTool(); err != nil {
			return nil, fmt.Errorf("agent %s: structured_output: %w", agentConfig.Name, err)
		}

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

		if agentConfig.ForceHandoff != "" {
			targets, err := resolveAgentRefs(ctx, []string{agentConfig.ForceHandoff}, agentsByName, externalAgents, &agents, runConfig, &loadOpts)
			if err != nil {
				return nil, fmt.Errorf("agent '%s': resolving force_handoff: %w", agentConfig.Name, err)
			}
			if len(targets) == 0 {
				return nil, fmt.Errorf("agent '%s': force_handoff '%s' did not resolve to an agent", agentConfig.Name, agentConfig.ForceHandoff)
			}
			agent.WithForceHandoff(targets[0])(a)
		}
	}

	// Create permissions checker from config
	permChecker := permissions.NewChecker(cfg.Permissions)

	// Build agent default models map
	agentDefaultModels := make(map[string]string)
	for _, agent := range cfg.Agents {
		if agent.Harness == nil && agent.Model != "" {
			agentDefaultModels[agent.Name] = agent.Model
		}
	}

	// Retain the resolved per-agent configs so inspection surfaces (the agent
	// inspector modal) can show declared toolset allow-lists, limits and flags.
	agentConfigs := make(map[string]latest.AgentConfig, len(cfg.Agents))
	agentBudgets := make(map[string][]string, len(cfg.Agents))
	for i := range cfg.Agents {
		agentConfigs[cfg.Agents[i].Name] = cfg.Agents[i]
		if len(cfg.Agents[i].Budgets) > 0 {
			agentBudgets[cfg.Agents[i].Name] = cfg.Agents[i].Budgets
		}
	}

	// runtime.safety is a config-wide session default; it travels on the
	// team so session constructors can consult it without the raw config.
	var runtimeSafety latest.SafetyMode
	if cfg.Runtime != nil {
		runtimeSafety = cfg.Runtime.Safety
	}

	return &LoadResult{
		Team: team.New(
			team.WithAgents(agents...),
			team.WithPermissions(permChecker),
			team.WithAgentConfigs(agentConfigs),
			team.WithRuntimeSafety(runtimeSafety),
		),
		Models:             cfg.Models,
		Providers:          cfg.Providers,
		ProviderRegistry:   loadOpts.providerRegistry,
		AgentDefaultModels: agentDefaultModels,
		Budget:             cfg.Budget,
		Budgets:            cfg.Budgets,
		AgentBudgets:       agentBudgets,
		EncryptedConfig:    runConfig.EncryptedConfig,
	}, nil
}

func getModelsForAgent(ctx context.Context, cfg *latest.Config, a *latest.AgentConfig, autoModelFn func() latest.ModelConfig, dmrFallbackSelectors map[string]bool, runConfig *config.RuntimeConfig, providerRegistry *provider.Registry, modelOpts []options.Opt) ([]provider.Provider, error) {
	var models []provider.Provider

	// Obtain the singleton store once, outside the loop.
	modelsStore, modelsStoreErr := runConfig.ModelsDevStore()

	for name := range strings.SplitSeq(a.Model, ",") {
		modelCfg, exists := cfg.Models[name]
		isAutoModel := false
		if !exists {
			if name == "auto" {
				modelCfg = autoModelFn()
				isAutoModel = true
			} else {
				return nil, fmt.Errorf("model '%s' not found in configuration", name)
			}
		}
		// A `first_available` selector that fell back to Docker Model Runner with
		// no usable local model is, like `auto`, a best-effort selection: surface
		// init failures as a "no model available" fallback rather than a raw
		// pull error.
		if dmrFallbackSelectors[name] {
			isAutoModel = true
		}
		modelCfg.Name = name

		// Use max_tokens from config if specified, otherwise look up from models.dev
		maxTokens := &defaultMaxTokens
		if modelCfg.MaxTokens != nil {
			maxTokens = modelCfg.MaxTokens
		} else if modelsStoreErr == nil {
			m, err := modelsStore.GetModel(ctx, modelsdev.NewID(modelCfg.Provider, modelCfg.Model))
			if err == nil {
				maxTokens = &m.Limit.Output
			}
		}

		opts := []options.Opt{
			options.WithGateway(runConfig.ModelsGateway),
			options.WithEncryptedConfig(runConfig.EncryptedConfig),
			options.WithStructuredOutput(a.StructuredOutput),
			options.WithProviders(cfg.Providers),
		}
		if maxTokens != nil {
			opts = append(opts, options.WithMaxTokens(*maxTokens))
		}
		if modelsStoreErr == nil {
			opts = append(opts, options.WithModelsDevStore(modelsStore))
		}
		opts = append(opts, modelOpts...)

		// Pass the full models map for routing rules to resolve model references
		model, err := providerRegistry.NewWithModels(ctx,
			&modelCfg,
			cfg.Models,
			runConfig.EnvProvider(),
			opts...,
		)
		if err != nil {
			// Return a cleaner error message for auto model selection failures,
			// keeping the underlying cause (e.g. a declined DMR pull) so the
			// message can explain why selection fell through.
			if isAutoModel {
				return nil, &config.AutoModelFallbackError{Cause: err}
			}
			return nil, err
		}
		models = append(models, model)
	}

	return models, nil
}

// getFallbackModelsForAgent returns fallback providers for an agent based on its fallback configuration.
// It uses the same resolution logic as primary models (named model, inline provider/model format).
func getFallbackModelsForAgent(ctx context.Context, cfg *latest.Config, a *latest.AgentConfig, runConfig *config.RuntimeConfig, providerRegistry *provider.Registry, modelOpts []options.Opt) ([]provider.Provider, error) {
	var fallbackModels []provider.Provider

	// Obtain the singleton store once, outside the loop.
	modelsStore, modelsStoreErr := runConfig.ModelsDevStore()

	for _, name := range a.GetFallbackModels() {
		modelCfg, exists := cfg.Models[name]
		if !exists {
			// Try parsing as inline provider/model format (e.g., "openai/gpt-4o")
			parsed, err := latest.ParseModelRef(name)
			if err != nil {
				return nil, fmt.Errorf("fallback model '%s' not found in configuration and is not a valid provider/model format", name)
			}
			modelCfg = parsed
		}
		modelCfg.Name = name

		// Use max_tokens from config if specified, otherwise look up from models.dev
		maxTokens := &defaultMaxTokens
		if modelCfg.MaxTokens != nil {
			maxTokens = modelCfg.MaxTokens
		} else if modelsStoreErr == nil {
			m, err := modelsStore.GetModel(ctx, modelsdev.NewID(modelCfg.Provider, modelCfg.Model))
			if err == nil {
				maxTokens = &m.Limit.Output
			}
		}

		opts := []options.Opt{
			options.WithGateway(runConfig.ModelsGateway),
			options.WithEncryptedConfig(runConfig.EncryptedConfig),
			options.WithStructuredOutput(a.StructuredOutput),
			options.WithProviders(cfg.Providers),
		}
		if maxTokens != nil {
			opts = append(opts, options.WithMaxTokens(*maxTokens))
		}
		if modelsStoreErr == nil {
			opts = append(opts, options.WithModelsDevStore(modelsStore))
		}
		opts = append(opts, modelOpts...)

		// Pass the full models map for routing rules to resolve model references
		model, err := providerRegistry.NewWithModels(ctx,
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

// getTitleModelForAgent resolves the dedicated title-generation model for an
// agent, if any. It returns the model named by the `title_model` field of the
// first of the agent's configured models that sets it, or nil when none do.
func getTitleModelForAgent(ctx context.Context, cfg *latest.Config, a *latest.AgentConfig, runConfig *config.RuntimeConfig, providerRegistry *provider.Registry, modelOpts []options.Opt) (provider.Provider, error) {
	var titleRef string
	for name := range strings.SplitSeq(a.Model, ",") {
		if modelCfg, ok := cfg.Models[name]; ok && modelCfg.TitleModel != "" {
			titleRef = modelCfg.TitleModel
			break
		}
	}
	if titleRef == "" {
		return nil, nil
	}

	modelsStore, modelsStoreErr := runConfig.ModelsDevStore()

	modelCfg, exists := cfg.Models[titleRef]
	if !exists {
		parsed, err := latest.ParseModelRef(titleRef)
		if err != nil {
			return nil, fmt.Errorf("title model '%s' not found in configuration and is not a valid provider/model format", titleRef)
		}
		modelCfg = parsed
	}
	modelCfg.Name = titleRef

	maxTokens := &defaultMaxTokens
	if modelCfg.MaxTokens != nil {
		maxTokens = modelCfg.MaxTokens
	} else if modelsStoreErr == nil {
		m, err := modelsStore.GetModel(ctx, modelsdev.NewID(modelCfg.Provider, modelCfg.Model))
		if err == nil {
			maxTokens = &m.Limit.Output
		}
	}

	opts := []options.Opt{
		options.WithGateway(runConfig.ModelsGateway),
		options.WithEncryptedConfig(runConfig.EncryptedConfig),
		options.WithStructuredOutput(a.StructuredOutput),
		options.WithProviders(cfg.Providers),
	}
	if maxTokens != nil {
		opts = append(opts, options.WithMaxTokens(*maxTokens))
	}
	if modelsStoreErr == nil {
		opts = append(opts, options.WithModelsDevStore(modelsStore))
	}
	opts = append(opts, modelOpts...)

	model, err := providerRegistry.NewWithModels(ctx, &modelCfg, cfg.Models, runConfig.EnvProvider(), opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create title model '%s': %w", titleRef, err)
	}
	return model, nil
}

// getCompactionModelForAgent resolves the dedicated compaction (summary
// generation) model for an agent, if any. Precedence is resolved by
// [config.EffectiveCompactionModelRef]: agent-level wins, then model-level,
// then the provider-level default. It returns nil when none set one. The
// value may be a named model from the models section or an inline
// "provider/model" spec.
func getCompactionModelForAgent(ctx context.Context, cfg *latest.Config, a *latest.AgentConfig, runConfig *config.RuntimeConfig, providerRegistry *provider.Registry, modelOpts []options.Opt) (provider.Provider, error) {
	compactionRef := config.EffectiveCompactionModelRef(cfg, a)
	if compactionRef == "" {
		return nil, nil
	}

	modelsStore, modelsStoreErr := runConfig.ModelsDevStore()

	modelCfg, exists := cfg.Models[compactionRef]
	if !exists {
		parsed, err := latest.ParseModelRef(compactionRef)
		if err != nil {
			return nil, fmt.Errorf("compaction model '%s' not found in configuration and is not a valid provider/model format", compactionRef)
		}
		modelCfg = parsed
	}
	modelCfg.Name = compactionRef

	maxTokens := &defaultMaxTokens
	if modelCfg.MaxTokens != nil {
		maxTokens = modelCfg.MaxTokens
	} else if modelsStoreErr == nil {
		m, err := modelsStore.GetModel(ctx, modelsdev.NewID(modelCfg.Provider, modelCfg.Model))
		if err == nil {
			maxTokens = &m.Limit.Output
		}
	}

	opts := []options.Opt{
		options.WithGateway(runConfig.ModelsGateway),
		options.WithEncryptedConfig(runConfig.EncryptedConfig),
		options.WithStructuredOutput(a.StructuredOutput),
		options.WithProviders(cfg.Providers),
	}
	if maxTokens != nil {
		opts = append(opts, options.WithMaxTokens(*maxTokens))
	}
	if modelsStoreErr == nil {
		opts = append(opts, options.WithModelsDevStore(modelsStore))
	}
	opts = append(opts, modelOpts...)

	model, err := providerRegistry.NewWithModels(ctx, &modelCfg, cfg.Models, runConfig.EnvProvider(), opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create compaction model '%s': %w", compactionRef, err)
	}
	return model, nil
}

// compactionThresholdForAgent resolves the proactive-compaction threshold for
// an agent, or nil when neither the agent nor its models set one (the
// compaction package default then applies). The `compaction_threshold` of the
// first of the agent's configured models that sets it wins; the agent-level
// value is the fallback.
func compactionThresholdForAgent(cfg *latest.Config, a *latest.AgentConfig) *float64 {
	for name := range strings.SplitSeq(a.Model, ",") {
		if modelCfg, ok := cfg.Models[name]; ok && modelCfg.CompactionThreshold != nil {
			return modelCfg.CompactionThreshold
		}
	}
	return a.CompactionThreshold
}

// getToolsForAgent returns the tool definitions for an agent based on its
// configuration. Toolset instructions support ${...} JavaScript placeholders
// (e.g. ${env.X}); they are expanded here using the runtime env provider.
func getToolsForAgent(ctx context.Context, a *latest.AgentConfig, parentDir string, runConfig *config.RuntimeConfig, configName string, loadOpts *loadOptions, expander Expander) ([]tools.ToolSet, []string, error) {
	var (
		toolSets []tools.ToolSet
		warnings []string
		// Toolsets implementing tools.Mergeable are grouped by key and
		// combined after the loop, in first-declaration order.
		mergeGroups map[string][]tools.MergeSibling
		mergeOrder  []string
	)
	registry := loadOpts.toolsetRegistry

	var deferredToolset DeferredToolSet

	for i := range a.Toolsets {
		toolset := a.Toolsets[i]

		tool, err := registry.CreateTool(ctx, toolset, parentDir, runConfig, configName)
		if err != nil {
			// Collect error but continue loading other toolsets
			slog.WarnContext(ctx, "Toolset configuration failed; skipping", "type", toolset.Type, "ref", toolset.Ref, "command", toolset.Command, "error", err)
			warnings = append(warnings, fmt.Sprintf("toolset %s failed: %v", toolset.Type, err))
			continue
		}

		wrapped := WithToolsFilter(tool, toolset.Tools...)
		wrapped = WithReadOnlyFilter(wrapped, toolset.ReadOnly || a.ReadOnly)
		wrapped = WithInstructions(wrapped, expander.Expand(ctx, toolset.Instruction, nil))
		wrapped, err = loadOpts.withToon(wrapped, toolset.Toon)
		if err != nil {
			return nil, nil, err
		}
		wrapped = WithModelOverride(wrapped, toolset.Model)

		// Handle deferred tools
		if !toolset.Defer.IsEmpty() {
			if loadOpts.newDeferred == nil {
				return nil, nil, errors.New("toolset defer needs teamloader.WithDeferredTools (e.g. deferred.New from pkg/tools/builtin/deferred)")
			}
			if deferredToolset == nil {
				deferredToolset = loadOpts.newDeferred()
			}
			deferredToolset.AddSource(wrapped, toolset.Defer.DeferAll, toolset.Defer.Tools)
			if toolset.Defer.DeferAll {
				wrapped = WithNoToolsFilter(wrapped)
			} else {
				wrapped = WithToolsExcludeFilter(wrapped, toolset.Defer.Tools...)
			}
		}

		if m, ok := tools.As[tools.Mergeable](tool); ok {
			key := m.MergeKey()
			if mergeGroups == nil {
				mergeGroups = map[string][]tools.MergeSibling{}
			}
			if _, seen := mergeGroups[key]; !seen {
				mergeOrder = append(mergeOrder, key)
			}
			mergeGroups[key] = append(mergeGroups[key], tools.MergeSibling{Raw: m, Wrapped: wrapped})
			continue
		}

		toolSets = append(toolSets, wrapped)
	}

	// A single mergeable toolset is used as-is; several sharing a key are
	// combined so the model sees one set of tools instead of duplicates.
	for _, key := range mergeOrder {
		group := mergeGroups[key]
		if len(group) == 1 {
			toolSets = append(toolSets, group[0].Wrapped)
			continue
		}
		toolSets = append(toolSets, group[0].Raw.(tools.Mergeable).Merge(group))
	}

	if deferredToolset != nil && deferredToolset.HasSources() {
		toolSets = append(toolSets, deferredToolset)
	}

	if len(a.SubAgents) > 0 {
		toolSets = append(toolSets, transfertask.New())
	}
	if len(a.Handoffs) > 0 {
		toolSets = append(toolSets, handoff.New())
	}

	// Wrap all tools in a single Code Mode toolset.
	// This allows the agent to call multiple tools in a single response.
	// It also allows to combine the results of multiple tools in a single response.
	if a.CodeModeTools || runConfig.GlobalCodeMode {
		if loadOpts.codeMode == nil {
			return nil, nil, errors.New("code_mode_tools needs teamloader.WithCodeMode (e.g. codemode.Wrap from pkg/tools/codemode)")
		}
		toolSets = []tools.ToolSet{loadOpts.codeMode(toolSets...)}
	}

	return toolSets, warnings, nil
}

// inlineSkills converts inline skill definitions from the agent config into
// runtime skills. Their body is carried in memory (InlineContent) so the
// toolset serves it without touching the filesystem.
func inlineSkills(defs []latest.InlineSkill) []skills.Skill {
	if len(defs) == 0 {
		return nil
	}
	out := make([]skills.Skill, 0, len(defs))
	for _, d := range defs {
		out = append(out, skills.Skill{
			Name:          d.Name,
			Description:   d.Description,
			InlineContent: d.Instructions,
			Context:       d.Context,
			Model:         d.Model,
			AllowedTools:  d.AllowedTools,
			Toolsets:      d.Toolsets,
		})
	}
	return out
}

// overrideWithInlineSkills folds inline skills into the discovered ones,
// letting an inline definition win over a file- or URL-loaded skill of the
// same name: the agent config is explicit, while local skills are picked up
// from whatever happens to sit in the search paths. Appending blindly would
// leave the shadowed skill advertised to the model — the same name listed
// twice, with lookups resolving to the discovered one.
func overrideWithInlineSkills(loaded, inline []skills.Skill) []skills.Skill {
	if len(inline) == 0 {
		return loaded
	}

	inlined := make(map[string]bool, len(inline))
	for _, s := range inline {
		inlined[s.Name] = true
	}

	merged := make([]skills.Skill, 0, len(loaded)+len(inline))
	for _, s := range loaded {
		if inlined[s.Name] {
			slog.Warn("Inline skill overrides a discovered skill with the same name", "name", s.Name, "path", s.FilePath)
			continue
		}
		merged = append(merged, s)
	}
	return append(merged, inline...)
}

// forkSkillToolSets builds, for each fork skill that declares toolsets, the
// list of toolsets to expose while the skill runs in its sub-session. Toolset
// names are resolved against the top-level `toolsets` section and instantiated
// through the same registry path agents use, so they get the standard
// name/filter/instruction wrappers. Each toolset is wrapped in a
// StartableToolSet so the runtime gets the same lazy, single-flight start and
// failure-dedup semantics as the agent's own toolsets. Non-fork skills and
// skills without declared toolsets are skipped. Creation failures are
// collected as warnings (parity with getToolsForAgent) rather than aborting
// the load.
func forkSkillToolSets(ctx context.Context, cfg *latest.Config, a *latest.AgentConfig, loadedSkills []skills.Skill, parentDir string, runConfig *config.RuntimeConfig, configName string, loadOpts *loadOptions, expander Expander) (map[string][]tools.ToolSet, []string, error) {
	var (
		result   map[string][]tools.ToolSet
		warnings []string
	)
	registry := loadOpts.toolsetRegistry
	for i := range loadedSkills {
		skill := loadedSkills[i]
		if !skill.IsFork() || len(skill.Toolsets) == 0 {
			continue
		}
		var built []tools.ToolSet
		for _, ref := range skill.Toolsets {
			toolset, ok := cfg.Toolsets[ref]
			if !ok {
				// Validated in config.validateSkillToolsetRefs; defensive only.
				warnings = append(warnings, fmt.Sprintf("skill %s references unknown toolset %s", skill.Name, ref))
				continue
			}
			tool, err := registry.CreateTool(ctx, toolset, parentDir, runConfig, configName)
			if err != nil {
				slog.WarnContext(ctx, "Skill toolset configuration failed; skipping", "skill", skill.Name, "toolset", ref, "error", err)
				warnings = append(warnings, fmt.Sprintf("skill %s toolset %s failed: %v", skill.Name, ref, err))
				continue
			}
			wrapped := WithToolsFilter(tool, toolset.Tools...)
			// Honor the agent-level readonly flag, exactly like getToolsForAgent:
			// a readonly agent must not gain mutating tools through a fork skill.
			wrapped = WithReadOnlyFilter(wrapped, toolset.ReadOnly || a.ReadOnly)
			wrapped = WithInstructions(wrapped, expander.Expand(ctx, toolset.Instruction, nil))
			wrapped, err = loadOpts.withToon(wrapped, toolset.Toon)
			if err != nil {
				return nil, nil, err
			}
			wrapped = WithModelOverride(wrapped, toolset.Model)
			// Wrap for lazy, single-flight start + failure-dedup, matching
			// agent.WithToolSets. skillSubSessionTools calls Start() on every
			// run-loop iteration, so the toolset must tolerate repeated starts.
			built = append(built, tools.NewStartable(wrapped))
		}
		if len(built) > 0 {
			if result == nil {
				result = make(map[string][]tools.ToolSet)
			}
			result[skill.Name] = built
		}
	}
	return result, warnings, nil
}

// withToon applies the configured TOON wrapper when a toolset asks for it.
func (o *loadOptions) withToon(inner tools.ToolSet, spec string) (tools.ToolSet, error) {
	if spec == "" {
		return inner, nil
	}
	if o.toon == nil {
		return nil, errors.New("toolset toon needs teamloader.WithToon (e.g. toon.Wrap from pkg/tools/toon)")
	}
	return o.toon(inner, spec), nil
}

// filterSkillsByName returns the subset of skills whose Name matches one of
// the include filters. When include is empty, skills is returned unchanged.
// Skills are not reordered; each matching skill keeps its original position.
// Any include entry that does not match any loaded skill is logged as a warning.
func filterSkillsByName(loaded []skills.Skill, include []string) []skills.Skill {
	if len(include) == 0 {
		return loaded
	}
	wanted := make(map[string]bool, len(include))
	for _, name := range include {
		wanted[name] = true
	}
	matched := make(map[string]bool, len(wanted))
	filtered := make([]skills.Skill, 0, len(loaded))
	for _, s := range loaded {
		if wanted[s.Name] {
			filtered = append(filtered, s)
			matched[s.Name] = true
		}
	}
	for _, name := range include {
		if !matched[name] {
			slog.Warn("Skill filter does not match any loaded skill", "name", name)
		}
	}
	return filtered
}

// configUsesCatalogRefs reports whether any toolset in the config references
// an MCP catalog server (ref: docker:...), i.e. whether loading the team will
// need the MCP catalog.
func configUsesCatalogRefs(cfg *latest.Config) bool {
	if cfg == nil {
		return false
	}
	for i := range cfg.Agents {
		for _, ts := range cfg.Agents[i].Toolsets {
			if ts.Ref != "" {
				return true
			}
		}
	}
	for _, ts := range cfg.Toolsets {
		if ts.Ref != "" {
			return true
		}
	}
	return false
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
// External references may include an explicit name prefix ("name:ref") or
// derive a short name from the reference (e.g. "myorg/review-pr" → "review-pr").
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

		agentName, externalRef := config.ParseExternalAgentRef(ref)

		// Check for name collisions before loading the external agent.
		if existing, ok := agentsByName[agentName]; ok {
			return nil, fmt.Errorf("external agent %q resolves to name %q which conflicts with agent %q", ref, agentName, existing.Name())
		}

		a, err := loadExternalAgent(ctx, externalRef, runConfig, loadOpts)
		if err != nil {
			return nil, fmt.Errorf("loading %q: %w", externalRef, err)
		}

		// Rename the external agent so it doesn't collide with locally-defined
		// agents. External agents resolve to their team's default agent (one
		// explicitly named "root" if it exists, otherwise the first agent
		// declared), which we may want to expose under a different name in
		// the importing team.
		agent.WithName(agentName)(a)

		*agents = append(*agents, a)
		externalAgents[ref] = a
		agentsByName[agentName] = a
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

	// Tag references (including the implicit ":latest") are re-resolved against
	// the registry every time the config is loaded, adding a digest lookup to
	// startup even when the agent is never invoked. Digest-pinned references are
	// served from the local cache with no network call, so nudge users to pin.
	if config.IsOCIReference(ref) && !config.IsDigestReference(ref) {
		slog.WarnContext(ctx, "External agent reference uses a tag, not a digest; it is re-resolved against the registry on every run. Pin it to a digest (ref@sha256:...) to avoid the per-run registry lookup.", "ref", ref)
	}

	if loadOpts.sourceResolver == nil {
		return nil, errors.New("external agent references need a source resolver: pass teamloader.WithSourceResolver (e.g. sources.Resolve from pkg/config/sources)")
	}
	source, err := loadOpts.sourceResolver(ref, runConfig.EnvProvider())
	if err != nil {
		return nil, err
	}

	result, err := Load(contextWithExternalDepth(ctx, depth+1), source, runConfig, inheritOptions(loadOpts))
	if err != nil {
		return nil, err
	}

	return result.DefaultAgent()
}

// inheritOptions carries a parent load's capability options (registries,
// resolver, expander, feature wrappers, strictness, model options) into the
// load of an external agent, so it is built under the same rules. Per-load
// inputs — working directory, model overrides, prompt files — are not
// inherited.
func inheritOptions(parent *loadOptions) Opt {
	return func(opts *loadOptions) error {
		opts.toolsetRegistry = parent.toolsetRegistry
		opts.providerRegistry = parent.providerRegistry
		opts.sourceResolver = parent.sourceResolver
		opts.newExpander = parent.newExpander
		opts.codeMode = parent.codeMode
		opts.toon = parent.toon
		opts.newDeferred = parent.newDeferred
		opts.modelOpts = parent.modelOpts
		opts.strict = parent.strict
		opts.features = parent.features
		return nil
	}
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
