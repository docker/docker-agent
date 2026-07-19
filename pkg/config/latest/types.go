package latest

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/effort"
)

const Version = "12"

// Config represents the entire configuration file
type Config struct {
	Version   string                    `json:"version,omitempty"`
	Agents    Agents                    `json:"agents,omitempty"`
	Providers map[string]ProviderConfig `json:"providers,omitempty"`
	Models    map[string]ModelConfig    `json:"models,omitempty"`
	MCPs      map[string]MCPToolset     `json:"mcps,omitempty"`
	RAG       map[string]RAGToolset     `json:"rag,omitempty"`
	// Commands and Skills are reusable, named groups shared across agents.
	// An agent opts into a group by listing its name in AgentConfig.UseCommands
	// or AgentConfig.UseSkills; the group is merged into the agent during config
	// resolution (see resolveCommandDefinitions / resolveSkillDefinitions). This
	// mirrors the top-level MCPs/RAG reference-by-name convention.
	Commands map[string]types.Commands `json:"commands,omitempty"`
	Skills   map[string]SkillsConfig   `json:"skills,omitempty"`
	// Toolsets is a map of reusable, named toolset definitions shared across
	// agents. An agent opts in by listing a name in AgentConfig.UseToolsets;
	// the named toolset is appended to the agent during config resolution
	// (see resolveToolsetDefinitions). This mirrors the top-level MCPs/RAG
	// reference-by-name convention but works for any toolset type.
	Toolsets    map[string]Toolset `json:"toolsets,omitempty"`
	Metadata    Metadata           `json:"metadata"`
	Permissions *PermissionsConfig `json:"permissions,omitempty"`
	Runtime     *RuntimeDefaults   `json:"runtime,omitempty"`
	// Budget caps the whole run, regardless of which agent spends. It is
	// the simple case: one ceiling for everything.
	Budget *BudgetConfig `json:"budget,omitempty"`
	// Budgets are reusable, named budget definitions. An agent opts into
	// one by listing its name in AgentConfig.Budgets, mirroring the
	// top-level MCPs/RAG/Toolsets reference-by-name convention.
	//
	// A named budget is a single shared pot: when several agents reference
	// the same name they draw from the *same* ceiling rather than each
	// getting a copy of it. That is what keeps a budget meaningful when an
	// agent fans out — ten sub-agents on one "tight" budget still cannot
	// spend more than "tight" allows. Give agents distinct budget names
	// when you want independent pots.
	Budgets map[string]BudgetConfig `json:"budgets,omitempty"`
}

// BudgetConfig caps what a single run may consume before the agent is
// stopped. Every limit is optional; a zero/unset limit is unlimited, and
// an absent budget block disables the whole mechanism.
//
// The budget is scoped to a run — one root RunStream — and is shared by
// every sub-session spawned inside it (task transfers, sub-agents,
// skills). Delegated work therefore spends against the same wallet as
// its parent, rather than each child receiving a fresh allowance. Per-run
// (not per-session) is the only accounting that makes a cost ceiling
// meaningful when an agent can fan out.
type BudgetConfig struct {
	// MaxCost is the maximum spend for the run, in USD.
	//
	// Only responses the runtime can price count towards it: a model with
	// no pricing data (an unknown ID, or a custom endpoint without a
	// model-level `cost:` block) contributes nothing, because there is no
	// honest number to add. Such a run emits a warning rather than
	// silently behaving as if it were free — see [CostConfig] to price a
	// custom endpoint explicitly.
	MaxCost float64 `json:"max_cost,omitempty" yaml:"max_cost,omitempty"`

	// MaxTokens is the maximum cumulative input+output tokens for the run.
	//
	// This counts every token the run sends and receives over its whole
	// lifetime. It is deliberately not the session's token counters, which
	// compaction resets — those measure current context length, not spend.
	MaxTokens int64 `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`

	// MaxTime is the maximum working time for the run — the cumulative
	// sum of turn durations, not wall-clock since the session opened.
	// Idle time while the user reads and types does not count. In Go
	// duration format ("10m", "30s", "1h30m").
	//
	// It is checked at turn boundaries, so a run stops at the first
	// boundary after the deadline rather than interrupting an in-flight
	// model call or tool. Expect overshoot up to the length of one turn.
	MaxTime Duration `json:"max_time,omitzero" yaml:"max_time,omitempty"`
}

// IsZero reports whether no limit is set, i.e. the block is inert.
func (b *BudgetConfig) IsZero() bool {
	return b == nil || (b.MaxCost == 0 && b.MaxTokens == 0 && b.MaxTime.Duration == 0)
}

// validate rejects negative limits. All-zero is valid and simply means
// "no limit", which keeps `budget: {}` from being an error.
func (b *BudgetConfig) validate() error {
	if b == nil {
		return nil
	}
	if b.MaxCost < 0 {
		return fmt.Errorf("max_cost must not be negative, got %v", b.MaxCost)
	}
	if b.MaxTokens < 0 {
		return fmt.Errorf("max_tokens must not be negative, got %d", b.MaxTokens)
	}
	if b.MaxTime.Duration < 0 {
		return fmt.Errorf("max_time must not be negative, got %s", b.MaxTime.Duration)
	}
	return nil
}

// RuntimeDefaults captures execution-time defaults the agent author
// wants applied when this config is run. The values act as defaults
// only: an explicit CLI flag or user-config setting always wins.
type RuntimeDefaults struct {
	// Sandbox, when true, runs the agent inside a Docker sandbox by
	// default — equivalent to passing --sandbox on the command line.
	// Useful for agents that always need filesystem/network isolation.
	Sandbox bool `json:"sandbox,omitempty" yaml:"sandbox,omitempty"`

	// NetworkAllowlist is the list of hosts that should be added to
	// the sandbox's default-deny network proxy when this agent runs in
	// a sandbox. Each entry is a hostname with an optional ":port"
	// suffix (e.g. "api.example.com", "registry.npmjs.org:443"). The
	// list is unioned with the gateway and tool-install hosts the
	// runner already opens automatically; commas and whitespace are
	// rejected to keep a single entry from smuggling several rules
	// into the policy engine.
	//
	// Use this when an agent's tools call hosts the auto-installer
	// can't infer (custom MCP endpoints, third-party APIs, registries
	// not covered by the aqua resolver, etc.) instead of relying on
	// the wider fallback host set the kit uses on resolution failures.
	NetworkAllowlist []string `json:"network_allowlist,omitempty" yaml:"network_allowlist,omitempty"`
}

// MCPToolset is a reusable MCP server definition stored in the top-level
// "mcps" section. It is identical to a Toolset but skips the normal
// Toolset.validate() call during YAML unmarshaling because the "type"
// field is implicit (always "mcp") and the source (command/remote/ref)
// is validated later during config resolution.
type MCPToolset struct {
	Toolset `json:",inline" yaml:",inline"`
}

func (m *MCPToolset) UnmarshalYAML(unmarshal func(any) error) error {
	// Use a plain alias to avoid triggering Toolset.UnmarshalYAML
	// (which calls validate and requires "type" to be set).
	type alias Toolset
	var tmp alias
	if err := unmarshal(&tmp); err != nil {
		return err
	}
	m.Toolset = Toolset(tmp)
	m.Type = "mcp"
	return m.validate()
}

// RAGToolset is a reusable RAG source definition stored in the top-level
// "rag" section. It is identical to a Toolset but skips the normal
// Toolset.validate() call during YAML unmarshaling because the "type"
// field is implicit (always "rag") and the RAG config is validated
// during config resolution.
type RAGToolset struct {
	Toolset `json:",inline" yaml:",inline"`
}

func (r RAGToolset) MarshalYAML() (any, error) {
	// Flatten RAGConfig fields alongside toolset fields into a single map.
	result := make(map[string]any)

	if r.Instruction != "" {
		result["instruction"] = r.Instruction
	}
	if len(r.Tools) > 0 {
		result["tools"] = r.Tools
	}
	if r.Name != "" {
		result["name"] = r.Name
	}
	if !r.Defer.IsEmpty() {
		result["defer"] = r.Defer
	}

	if r.RAGConfig != nil {
		cfg := r.RAGConfig
		result["tool"] = cfg.Tool
		if len(cfg.Docs) > 0 {
			result["docs"] = cfg.Docs
		}
		if cfg.RespectVCS != nil {
			result["respect_vcs"] = *cfg.RespectVCS
		}
		if len(cfg.Strategies) > 0 {
			result["strategies"] = cfg.Strategies
		}
		result["results"] = cfg.Results
	}

	return result, nil
}

func (r *RAGToolset) UnmarshalYAML(unmarshal func(any) error) error {
	// RAGToolset flattens RAGConfig fields directly at the top level,
	// so users write tool/docs/strategies alongside toolset fields
	// (instruction, tools, name, defer) without a rag_config wrapper.
	//
	// We unmarshal into a raw map first to avoid strict-mode errors
	// from fields that belong to RAGConfig but not Toolset.
	var raw map[string]any
	if err := unmarshal(&raw); err != nil {
		return err
	}

	// Extract toolset-level fields
	var tf Toolset
	tf.Type = "rag"
	if v, ok := raw["instruction"].(string); ok {
		tf.Instruction = v
	}
	if v, ok := raw["name"].(string); ok {
		tf.Name = v
	}
	if v, ok := raw["tools"]; ok {
		if arr, ok := v.([]any); ok {
			for _, item := range arr {
				if s, ok := item.(string); ok {
					tf.Tools = append(tf.Tools, s)
				}
			}
		}
	}
	if v, ok := raw["defer"]; ok {
		data, _ := yaml.Marshal(v)
		_ = yaml.Unmarshal(data, &tf.Defer)
	}

	// Unmarshal RAGConfig from the same map (it has its own UnmarshalYAML)
	var ragCfg RAGConfig
	if err := unmarshal(&ragCfg); err != nil {
		return err
	}

	tf.RAGConfig = &ragCfg
	r.Toolset = tf
	return nil
}

type Agents []AgentConfig

func (c *Agents) UnmarshalYAML(unmarshal func(any) error) error {
	var items yaml.MapSlice
	if err := unmarshal(&items); err != nil {
		return err
	}

	agents := make([]AgentConfig, 0, len(items))
	for _, item := range items {
		name, ok := item.Key.(string)
		if !ok {
			return errors.New("agent name must be a string")
		}

		valueBytes, err := yaml.Marshal(item.Value)
		if err != nil {
			return fmt.Errorf("failed to marshal agent config for %s: %w", name, err)
		}

		var agent AgentConfig
		if err := yaml.UnmarshalWithOptions(valueBytes, &agent, yaml.DisallowUnknownField()); err != nil {
			return fmt.Errorf("failed to unmarshal agent config for %s: %w", name, err)
		}

		agent.Name = name
		agents = append(agents, agent)
	}

	*c = agents
	return nil
}

func (c Agents) MarshalYAML() (any, error) {
	mapSlice := make(yaml.MapSlice, 0, len(c))

	for _, agent := range c {
		mapSlice = append(mapSlice, yaml.MapItem{
			Key:   agent.Name,
			Value: agent,
		})
	}

	return mapSlice, nil
}

func (c *Agents) First() AgentConfig {
	if len(*c) > 0 {
		return (*c)[0]
	}
	panic("no agents configured")
}

func (c *Agents) Lookup(name string) (AgentConfig, bool) {
	for _, agent := range *c {
		if agent.Name == name {
			return agent, true
		}
	}
	return AgentConfig{}, false
}

func (c *Agents) Update(name string, update func(a *AgentConfig)) bool {
	for i := range *c {
		if (*c)[i].Name == name {
			update(&(*c)[i])
			return true
		}
	}
	return false
}

// ProviderConfig represents a reusable provider definition.
// It allows users to define providers with default settings that models can inherit.
// Models referencing a provider by name will inherit any settings not explicitly overridden.
//
// The Provider field specifies the underlying provider type (e.g., "openai", "anthropic",
// "google", "amazon-bedrock"). When not set, it defaults to "openai" for backward compatibility.
type ProviderConfig struct {
	// Provider specifies the underlying provider type. Supported values include:
	// "openai", "anthropic", "google", "amazon-bedrock", "dmr", and any built-in alias.
	// Defaults to "openai" when not set, preserving backward compatibility.
	Provider string `json:"provider,omitempty"`
	// APIType specifies which API schema to use. Only applicable for OpenAI-compatible providers.
	// Supported values:
	// - "openai_chatcompletions" (default for openai): Use the OpenAI Chat Completions API
	// - "openai_responses": Use the OpenAI Responses API
	APIType string `json:"api_type,omitempty"`
	// BaseURL is the base URL for the provider's API endpoint
	BaseURL string `json:"base_url,omitempty"`
	// UnloadAPI is the path (or absolute URL) to the provider's
	// model-unload endpoint. When the agent wires the [unload] builtin
	// into its `on_agent_switch` hook chain, the previous agent's
	// models are POSTed `{"model": "<id>"}` here at every switch.
	// Cloud providers should leave this unset.
	//
	// [unload]: https://pkg.go.dev/github.com/docker/docker-agent/pkg/hooks/builtins#Unload
	UnloadAPI string `json:"unload_api,omitempty"`
	// TokenKey is the environment variable name containing the API token
	TokenKey string `json:"token_key,omitempty"`
	// Temperature is the default sampling temperature for models using this provider
	Temperature *float64 `json:"temperature,omitempty"`
	// MaxTokens is the default maximum number of tokens for models using this provider
	MaxTokens *int64 `json:"max_tokens,omitempty"`
	// TopP is the default top-p sampling parameter
	TopP *float64 `json:"top_p,omitempty"`
	// FrequencyPenalty is the default frequency penalty
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	// PresencePenalty is the default presence penalty
	PresencePenalty *float64 `json:"presence_penalty,omitempty"`
	// ParallelToolCalls controls whether parallel tool calls are enabled by default
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
	// ProviderOpts allows provider-specific options
	ProviderOpts map[string]any `json:"provider_opts,omitempty"`
	// TrackUsage controls whether token usage tracking is enabled by default
	TrackUsage *bool `json:"track_usage,omitempty"`
	// ThinkingBudget controls reasoning effort/budget for models using this provider
	ThinkingBudget *ThinkingBudget `json:"thinking_budget,omitempty"`
	// TaskBudget caps the total tokens a model can spend across an agentic task.
	// Forwarded to Anthropic as `output_config.task_budget` for every Claude
	// model — docker-agent does not gate by model name. At the time of writing,
	// only Claude Opus 4.7 actually honors it; other models will reject the
	// field. Accepts an integer token count or a {type: tokens, total: N} object.
	TaskBudget *TaskBudget `json:"task_budget,omitempty"`
	// Auth selects a non-API-key authentication scheme for this provider
	// (currently: Anthropic Workload Identity Federation). When set, the
	// provider's regular API-key path is bypassed.
	Auth *AuthConfig `json:"auth,omitempty"`
}

// FallbackConfig represents fallback model configuration for an agent.
// Controls which models to try when the primary fails and how retries/cooldowns work.
// Most users only need to specify Models — the defaults handle common scenarios automatically.
type FallbackConfig struct {
	// Models is a list of fallback models to try in order if the primary fails.
	// Each entry can be a model name from the models section or an inline provider/model format.
	Models []string `json:"models,omitempty"`
	// Retries is the number of retries per model with exponential backoff.
	// Default is 2 (giving 3 total attempts per model). Use -1 to disable retries entirely.
	// Retries only apply to retryable errors (5xx, timeouts); non-retryable errors (429, 4xx)
	// skip immediately to the next model.
	Retries int `json:"retries,omitempty"`
	// Cooldown is the duration to stick with a successful fallback model before
	// retrying the primary. Only applies after a non-retryable error (e.g., 429).
	// Default is 1 minute. Use Go duration format (e.g., "1m", "30s", "2m30s").
	Cooldown Duration `json:"cooldown"`
}

// Duration is a wrapper around time.Duration that supports YAML/JSON unmarshaling
// from string format (e.g., "1m", "30s", "2h30m").
type Duration struct {
	time.Duration
}

// UnmarshalYAML implements custom unmarshaling for Duration from string format
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	if d == nil {
		return errors.New("cannot unmarshal into nil Duration")
	}

	var s string
	if err := unmarshal(&s); err != nil {
		// Try as integer (seconds)
		var secs int
		if err2 := unmarshal(&secs); err2 == nil {
			d.Duration = time.Duration(secs) * time.Second
			return nil
		}
		return err
	}
	if s == "" {
		d.Duration = 0
		return nil
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration format %q: %w", s, err)
	}
	d.Duration = dur
	return nil
}

// MarshalYAML implements custom marshaling for Duration to string format
func (d Duration) MarshalYAML() (any, error) {
	if d.Duration == 0 {
		return "", nil
	}
	return d.String(), nil
}

// UnmarshalJSON implements custom unmarshaling for Duration from string format
func (d *Duration) UnmarshalJSON(data []byte) error {
	if d == nil {
		return errors.New("cannot unmarshal into nil Duration")
	}

	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		// Try as integer (seconds)
		var secs int
		if err2 := json.Unmarshal(data, &secs); err2 == nil {
			d.Duration = time.Duration(secs) * time.Second
			return nil
		}
		return err
	}
	if s == "" {
		d.Duration = 0
		return nil
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration format %q: %w", s, err)
	}
	d.Duration = dur
	return nil
}

// MarshalJSON implements custom marshaling for Duration to string format
func (d Duration) MarshalJSON() ([]byte, error) {
	if d.Duration == 0 {
		return json.Marshal("")
	}
	return json.Marshal(d.String())
}

// HarnessConfig configures an agent that delegates execution to an external
// coding-agent CLI through github.com/rumpl/harness instead of using a
// docker-agent model provider.
type HarnessConfig struct {
	// Type identifies the external harness provider: claude-code, codex, pi, or opencode.
	Type string `json:"type,omitempty"`
	// Model is passed to harnesses that accept a model flag. When omitted,
	// docker-agent lets the external CLI use its own default model.
	Model string `json:"model,omitempty"`
	// Effort is forwarded to Claude Code's --effort flag.
	Effort string `json:"effort,omitempty"`
	// Agent is forwarded to opencode's --agent flag.
	Agent string `json:"agent,omitempty"`
	// Thinking enables opencode's --thinking flag.
	Thinking bool `json:"thinking,omitempty"`
}

// InstructionFiles holds one or more instruction file paths. It accepts both
// a single string (`instruction_file: prompt.md`) and a list of strings
// (`instruction_file: [intro.md, rules.md]`) in YAML and JSON. Empty strings
// are dropped on decode, so `instruction_file: ""` and `instruction_file: [""]`
// both decode to nil (treated as absent). A single path is marshalled back as
// a scalar, and an empty value is omitted entirely, so configs round-trip
// unchanged.
type InstructionFiles []string

func (f *InstructionFiles) unmarshal(scalar, list func(any) error) error {
	var one string
	if err := scalar(&one); err == nil {
		*f = nonEmptyInstructionFiles(one)
		return nil
	}
	var many []string
	if err := list(&many); err != nil {
		return errors.New("instruction_file must be a string or a list of strings")
	}
	*f = nonEmptyInstructionFiles(many...)
	return nil
}

// nonEmptyInstructionFiles returns the given paths with empty strings dropped,
// or nil when nothing remains. This keeps the list form consistent with the
// scalar form, where `instruction_file: ""` is treated as absent rather than a
// path to resolve.
func nonEmptyInstructionFiles(paths ...string) InstructionFiles {
	var out InstructionFiles
	for _, p := range paths {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (f *InstructionFiles) UnmarshalYAML(unmarshal func(any) error) error {
	return f.unmarshal(unmarshal, unmarshal)
}

func (f InstructionFiles) MarshalYAML() (any, error) {
	if len(f) == 0 {
		return nil, nil
	}
	if len(f) == 1 {
		return f[0], nil
	}
	return []string(f), nil
}

func (f *InstructionFiles) UnmarshalJSON(data []byte) error {
	return f.unmarshal(
		func(v any) error { return json.Unmarshal(data, v) },
		func(v any) error { return json.Unmarshal(data, v) },
	)
}

func (f InstructionFiles) MarshalJSON() ([]byte, error) {
	if len(f) == 0 {
		return json.Marshal(nil)
	}
	if len(f) == 1 {
		return json.Marshal(f[0])
	}
	return json.Marshal([]string(f))
}

// AgentConfig represents a single agent configuration
type AgentConfig struct {
	Name           string
	Model          string          `json:"model,omitempty"`
	Fallback       *FallbackConfig `json:"fallback,omitempty"`
	Description    string          `json:"description,omitempty"`
	WelcomeMessage string          `json:"welcome_message,omitempty"`
	Toolsets       []Toolset       `json:"toolsets,omitempty"`
	Instruction    string          `json:"instruction,omitempty"`
	// InstructionFile names one or more files, relative to the config file's
	// directory, whose contents are loaded into Instruction when the config is
	// loaded. It accepts either a single path (`instruction_file: prompt.md`)
	// or a list (`instruction_file: [intro.md, rules.md]`); when several files
	// are given their contents are concatenated in order, separated by a blank
	// line. It keeps long behavioral prompts out of the YAML, letting
	// infrastructure configuration and instruction content evolve in separate
	// files. Mutually exclusive with Instruction. Only file-based config
	// sources are supported (not OCI/URL/bytes sources, which have no directory
	// to resolve against). The field is cleared once resolved so the in-memory
	// config stays self-contained (see config.Load).
	InstructionFile InstructionFiles `json:"instruction_file,omitempty" yaml:"instruction_file,omitempty"`
	Harness         *HarnessConfig   `json:"harness,omitempty"`
	SubAgents       []string         `json:"sub_agents,omitempty"`
	Handoffs        []string         `json:"handoffs,omitempty"`
	// ForceHandoff names an agent that unconditionally receives the
	// conversation whenever this agent produces a final response,
	// bypassing the LLM's tool-calling entirely. Unlike Handoffs (which
	// rely on the model choosing to call the handoff tool), the runtime
	// intercepts the natural stop and routes deterministically, making
	// strict pipelines reliable. The full conversation context carries
	// over to the target agent.
	ForceHandoff string `json:"force_handoff,omitempty" yaml:"force_handoff,omitempty"`

	AddDate            bool `json:"add_date,omitempty"`
	AddEnvironmentInfo bool `json:"add_environment_info,omitempty"`
	// ReadOnly makes every one of the agent's toolsets read-only: only
	// tools whose annotations carry a read-only hint are listed and
	// callable. Equivalent to setting `readonly: true` on each toolset.
	ReadOnly bool `json:"readonly,omitempty" yaml:"readonly,omitempty"`
	// RedactSecrets enables every leg of the redact_secrets feature:
	// the pre_tool_use builtin (scrubs tool arguments), the
	// before_llm_call hook (scrubs outgoing chat content), and the
	// tool_response_transform hook (scrubs tool output before it
	// reaches event consumers, the persisted session, the post_tool_use
	// hook, or the next LLM call). Equivalent to writing all three
	// hook entries by hand — the runtime auto-injects them when this
	// flag is true. See pkg/hooks/builtins/redact_secrets.go for the
	// hook-side implementation.
	//
	// Pointer (tri-state) so we can distinguish "unset" (nil → default
	// on) from "explicitly disabled" (false). Use
	// [AgentConfig.RedactSecretsEnabled] to read the effective value.
	RedactSecrets           *bool `json:"redact_secrets,omitempty"`
	CodeModeTools           bool  `json:"code_mode_tools,omitempty"`
	AddDescriptionParameter bool  `json:"add_description_parameter,omitempty"`
	MaxIterations           int   `json:"max_iterations,omitempty"`
	// Budgets names the top-level `budgets` entries this agent spends
	// against. Every listed budget must exist. An agent may list several,
	// in which case all of them apply and the first to be exhausted stops
	// the run. Agents sharing a name share one pot — see [Config.Budgets].
	Budgets                 []string `json:"budgets,omitempty" yaml:"budgets,omitempty"`
	MaxConsecutiveToolCalls int      `json:"max_consecutive_tool_calls,omitempty"`
	MaxOldToolCallTokens    int      `json:"max_old_tool_call_tokens,omitempty"`
	// MaxToolResultTokens caps each textual tool result when it enters the
	// session. Oversized results are middle-out truncated (head and tail kept,
	// middle replaced with a marker); textual documents attached to the result
	// share the same budget. Tokens are approximated as len/4.
	// Disabled by default; only positive values enable the cap.
	MaxToolResultTokens int `json:"max_tool_result_tokens,omitempty"`
	NumHistoryItems     int `json:"num_history_items,omitempty"`
	// SessionCompaction toggles automatic session compaction for this agent:
	// the proactive threshold trigger and the post-overflow auto-recovery.
	// Manual /compact stays available regardless. Pointer (tri-state) so
	// "unset" (nil → default on) is distinguishable from an explicit
	// `session_compaction: false`. Use [AgentConfig.SessionCompactionEnabled]
	// to read the effective value.
	SessionCompaction *bool `json:"session_compaction,omitempty"`
	// CompactionThreshold is the fraction of the context window at which
	// proactive auto-compaction triggers for this agent. Must be greater than
	// 0 and at most 1; defaults to 0.9 when unset. A `compaction_threshold`
	// set on the agent's model takes precedence.
	CompactionThreshold *float64          `json:"compaction_threshold,omitempty"`
	AddPromptFiles      []string          `json:"add_prompt_files,omitempty" yaml:"add_prompt_files,omitempty"`
	Commands            types.Commands    `json:"commands,omitempty"`
	StructuredOutput    *StructuredOutput `json:"structured_output,omitempty"`
	Skills              SkillsConfig      `json:"skills,omitzero"`
	// UseCommands and UseSkills reference reusable groups defined in the
	// top-level Config.Commands / Config.Skills sections. The referenced
	// groups are merged into Commands / Skills during config resolution;
	// inline entries on the agent take precedence on name conflicts.
	UseCommands []string `json:"use_commands,omitempty"`
	UseSkills   []string `json:"use_skills,omitempty"`
	// UseToolsets references reusable toolset definitions defined in the
	// top-level Config.Toolsets section. The referenced toolsets are appended
	// to the agent's own Toolsets during config resolution (see
	// resolveToolsetDefinitions). Inline toolsets on the agent come first.
	UseToolsets []string     `json:"use_toolsets,omitempty"`
	Hooks       *HooksConfig `json:"hooks,omitempty"`
	Cache       *CacheConfig `json:"cache,omitempty"`
}

// CacheConfig configures the agent's response cache. When set and Enabled
// is true, the agent stores the assistant response produced for a given
// user question and replays it when the same question is asked again,
// skipping the model entirely.
//
// Two normalization options control what "same question" means:
//   - CaseSensitive: when false (the default), question matching is
//     case-insensitive ("Hello" == "hello").
//   - TrimSpaces: when true, leading and trailing whitespace is stripped
//     before comparison ("  hello  " == "hello").
//
// Storage is in-memory by default. Set Path to persist entries to a JSON
// file that is reloaded on startup.
type CacheConfig struct {
	Enabled       bool   `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	CaseSensitive bool   `json:"case_sensitive,omitempty" yaml:"case_sensitive,omitempty"`
	TrimSpaces    bool   `json:"trim_spaces,omitempty" yaml:"trim_spaces,omitempty"`
	Path          string `json:"path,omitempty" yaml:"path,omitempty"`
}

const SkillSourceLocal = "local"

// errSkillsFormat is returned when the `skills` value is neither a boolean nor
// a list of strings and/or inline skill definitions.
var errSkillsFormat = errors.New("skills must be a boolean or a list of skill sources, names, and/or inline skill definitions")

// skillsFormatError maps a list-decode failure to a user-facing error. When the
// failure carries a specific inline-skill diagnostic (an unknown/misspelled
// field surfaced by skillListItem), that detail is preserved; otherwise the
// generic shape hint is returned.
func skillsFormatError(err error) error {
	if err != nil && strings.Contains(err.Error(), "invalid inline skill") {
		return err
	}
	return errSkillsFormat
}

// InlineSkill is a skill defined directly in the agent config rather than
// loaded from the filesystem or a remote URL. It supports the subset of the
// SKILL.md format that can be expressed in YAML; the skill body lives in
// Instructions instead of a Markdown file.
type InlineSkill struct {
	// Name is the skill identifier used by read_skill / run_skill and the
	// /<name> slash command. Required.
	Name string `json:"name" yaml:"name"`
	// Description is injected into the system prompt so the model knows when
	// the skill applies. Required.
	Description string `json:"description" yaml:"description"`
	// Instructions is the skill body (equivalent to the Markdown content
	// below the SKILL.md frontmatter). Required.
	Instructions string `json:"instructions" yaml:"instructions"`
	// Context, when set to "fork", runs the skill as an isolated sub-agent
	// (exposed via run_skill) instead of inlining it in the conversation.
	Context string `json:"context,omitempty" yaml:"context,omitempty"`
	// Model optionally overrides the model used while a fork-mode skill runs.
	// Ignored for non-fork skills.
	Model string `json:"model,omitempty" yaml:"model,omitempty"`
	// AllowedTools optionally records the tools the skill expects. It mirrors
	// the SKILL.md `allowed-tools` field. For a fork-mode skill it is enforced
	// as an allow-list over the parent agent's inherited tools while the skill
	// runs in its sub-session: only tools whose names match an entry are kept.
	// Entries are matched with filepath.Match-style globs (e.g. "read_*"),
	// falling back to an exact match. Ignored for non-fork skills.
	AllowedTools []string `json:"allowed_tools,omitempty" yaml:"allowed_tools,omitempty"`
	// Toolsets lists names of reusable toolset definitions (from the top-level
	// `toolsets` section) to expose to the skill while it runs in its fork
	// sub-session, in addition to the parent agent's tools. Ignored for
	// non-fork skills.
	Toolsets []string `json:"toolsets,omitempty" yaml:"toolsets,omitempty"`
}

// SkillsConfig controls skill discovery sources, filtering, and inline
// definitions for an agent. Supports these YAML formats:
//   - Boolean: `skills: true` (equivalent to ["local"]) or `skills: false` (disabled)
//   - List:    `skills: ["local", "http://example.com"]` — sources to load from
//   - List:    `skills: ["git", "docker"]`               — names of skills to include
//   - List:    `skills: ["local", "git"]`                — mix of sources and names
//   - List:    a list may also contain mapping items, each an inline skill
//     definition (see InlineSkill), freely mixed with the string items above.
//
// String items in the list are classified automatically:
//   - "local" or any HTTP/HTTPS URL → a skill source (added to Sources)
//   - any other string             → a skill name filter (added to Include)
//
// Mapping items are decoded as inline skills (added to Inline).
//
// When Include is non-empty but no explicit sources are provided, Sources defaults
// to ["local"] so that `skills: ["git"]` loads local skills and keeps only "git".
// Inline skills on their own do not pull in any sources: a list containing only
// inline definitions enables skills without loading local or remote ones.
//
// The special source "local" loads skills from the filesystem (standard locations).
// HTTP/HTTPS URLs load skills from remote servers per the well-known skills discovery spec.
type SkillsConfig struct { //nolint:recvcheck // MarshalYAML/MarshalJSON must use value receiver, UnmarshalYAML/UnmarshalJSON must use pointer
	// Sources lists where to load skills from: "local" and/or HTTP/HTTPS URLs.
	Sources []string
	// Include optionally filters loaded skills by name. When non-empty, only
	// skills whose Name matches an entry in this list are exposed to the agent.
	Include []string
	// Inline holds skills defined directly in the agent config. They are
	// always exposed (never subject to the Include filter) and are not
	// affected by Sources.
	Inline []InlineSkill
}

func (s SkillsConfig) Enabled() bool {
	return len(s.Sources) > 0 || len(s.Inline) > 0
}

func (s SkillsConfig) HasLocal() bool {
	return slices.Contains(s.Sources, SkillSourceLocal)
}

func (s SkillsConfig) RemoteURLs() []string {
	var urls []string
	for _, src := range s.Sources {
		if isRemoteURL(src) {
			urls = append(urls, src)
		}
	}
	return urls
}

// isRemoteURL reports whether s looks like an HTTP or HTTPS URL.
func isRemoteURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// isSkillSource reports whether a list item should be treated as a skill source
// (the special value "local" or an HTTP/HTTPS URL) rather than a skill name.
func isSkillSource(item string) bool {
	return item == SkillSourceLocal || isRemoteURL(item)
}

// skillListItem is one entry of the `skills` list, which may be either a
// plain string (a source or a name filter) or a mapping (an inline skill
// definition). Exactly one of str/inline is populated after unmarshaling.
type skillListItem struct {
	str    string
	inline *InlineSkill
}

func (i *skillListItem) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err == nil {
		i.str = s
		return nil
	}
	// Not a scalar: the item must be an inline skill definition. Surface the
	// decode error (e.g. an unknown/misspelled field under strict mode) rather
	// than the generic errSkillsFormat so the user can see what's wrong.
	var inline InlineSkill
	if err := unmarshal(&inline); err != nil {
		return fmt.Errorf("invalid inline skill: %w", err)
	}
	i.inline = &inline
	return nil
}

func (i *skillListItem) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		i.str = s
		return nil
	}
	var inline InlineSkill
	if err := json.Unmarshal(data, &inline); err != nil {
		return fmt.Errorf("invalid inline skill: %w", err)
	}
	i.inline = &inline
	return nil
}

// setFromBool is the shared "boolean shorthand" logic for YAML and JSON
// unmarshaling: `true` means load local skills, `false` disables skills.
func (s *SkillsConfig) setFromBool(b bool) {
	s.Include = nil
	s.Inline = nil
	if b {
		s.Sources = []string{SkillSourceLocal}
	} else {
		s.Sources = nil
	}
}

// setFromList splits items into Sources ("local" + URLs), Include (skill name
// filters), and Inline (mapping items). When Include is non-empty and Sources
// is empty, Sources defaults to ["local"] so that `skills: ["git"]` filters
// local skills without requiring the user to spell out the source. Inline
// skills do not, on their own, pull in any source.
func (s *SkillsConfig) setFromList(items []skillListItem) {
	s.Sources = nil
	s.Include = nil
	s.Inline = nil
	for _, item := range items {
		switch {
		case item.inline != nil:
			s.Inline = append(s.Inline, *item.inline)
		case isSkillSource(item.str):
			s.Sources = append(s.Sources, item.str)
		default:
			s.Include = append(s.Include, item.str)
		}
	}
	if len(s.Sources) == 0 && len(s.Include) > 0 {
		s.Sources = []string{SkillSourceLocal}
	}
}

// marshalValue returns the canonical encoded representation: `false` when
// disabled, `true` when only the default local source is set, otherwise a
// list combining Sources, Include, and Inline. The default local source is
// omitted from the list when Include is non-empty so the output round-trips
// back through setFromList.
func (s SkillsConfig) marshalValue() any {
	switch {
	case len(s.Sources) == 0 && len(s.Include) == 0 && len(s.Inline) == 0:
		return false
	case len(s.Include) == 0 && len(s.Inline) == 0 && len(s.Sources) == 1 && s.Sources[0] == SkillSourceLocal:
		return true
	}

	sources := s.Sources
	if len(s.Include) > 0 && len(sources) == 1 && sources[0] == SkillSourceLocal {
		sources = nil
	}
	out := make([]any, 0, len(sources)+len(s.Include)+len(s.Inline))
	for _, src := range sources {
		out = append(out, src)
	}
	for _, name := range s.Include {
		out = append(out, name)
	}
	for _, inline := range s.Inline {
		out = append(out, inline)
	}
	return out
}

func (s *SkillsConfig) UnmarshalYAML(unmarshal func(any) error) error {
	var b bool
	if err := unmarshal(&b); err == nil {
		s.setFromBool(b)
		return nil
	}
	var items []skillListItem
	if err := unmarshal(&items); err != nil {
		return skillsFormatError(err)
	}
	s.setFromList(items)
	return nil
}

func (s SkillsConfig) MarshalYAML() (any, error) {
	return s.marshalValue(), nil
}

func (s *SkillsConfig) UnmarshalJSON(data []byte) error {
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		s.setFromBool(b)
		return nil
	}
	var items []skillListItem
	if err := json.Unmarshal(data, &items); err != nil {
		return skillsFormatError(err)
	}
	s.setFromList(items)
	return nil
}

func (s SkillsConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.marshalValue())
}

// GetFallbackModels returns the fallback models from the config.
func (a *AgentConfig) GetFallbackModels() []string {
	if a.Fallback != nil {
		return a.Fallback.Models
	}
	return nil
}

// RedactSecretsEnabled reports the effective value of the agent's
// redact_secrets flag. The feature is on by default: a nil pointer
// (the field omitted from YAML) means enabled, an explicit
// `redact_secrets: false` is the only way to disable it.
func (a *AgentConfig) RedactSecretsEnabled() bool {
	if a == nil || a.RedactSecrets == nil {
		return true
	}
	return *a.RedactSecrets
}

// SessionCompactionEnabled reports the effective value of the agent's
// session_compaction flag. Automatic compaction is on by default: a nil
// pointer (the field omitted from YAML) means enabled, an explicit
// `session_compaction: false` is the only way to disable it.
func (a *AgentConfig) SessionCompactionEnabled() bool {
	if a == nil || a.SessionCompaction == nil {
		return true
	}
	return *a.SessionCompaction
}

// SaferShellEnabled reports whether any of the agent's shell toolsets
// has opted into destructive-command detection. The flag lives on the
// toolset (not the agent), so this aggregates across all of an agent's
// toolsets — one match anywhere flips the agent-level flag the runtime
// uses to register the safer_shell builtin.
//
// Off by default: a missing pointer or explicit `safer: false` does
// not enable the feature. Only an explicit `safer: true` on at least
// one shell toolset switches it on.
//
// Multi-toolset note: when an agent declares more than one shell
// toolset and only some opt in, the safer_shell builtin still runs
// for every shell call on this agent (it filters by ToolName, not
// by toolset identity). Author one shell toolset per agent when you
// need toolset-scoped granularity.
func (a *AgentConfig) SaferShellEnabled() bool {
	if a == nil {
		return false
	}
	for i := range a.Toolsets {
		ts := &a.Toolsets[i]
		if ts.Type == "shell" && ts.Safer != nil && *ts.Safer {
			return true
		}
	}
	return false
}

// GetFallbackRetries returns the fallback retries from the config.
func (a *AgentConfig) GetFallbackRetries() int {
	if a.Fallback != nil {
		return a.Fallback.Retries
	}
	return 0
}

// GetFallbackCooldown returns the fallback cooldown duration from the config.
// Returns the configured cooldown, or 0 if not set (caller should apply default).
func (a *AgentConfig) GetFallbackCooldown() time.Duration {
	if a.Fallback != nil {
		return a.Fallback.Cooldown.Duration
	}
	return 0
}

// ModelConfig represents the configuration for a model
type ModelConfig struct {
	// Name is the manifest model name (map key), populated at runtime.
	// Not serialized — set by teamloader/model_switcher when resolving models.
	Name     string `json:"-"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// DisplayModel holds the original model name from the YAML config, before alias resolution.
	// When set, provider.ID() returns Provider + "/" + DisplayModel instead of the resolved name.
	// This ensures the UI shows the user-configured name (e.g., "claude-haiku-4-5")
	// while the API uses the resolved name (e.g., "claude-haiku-4-5-20251001").
	DisplayModel      string   `json:"-"`
	Temperature       *float64 `json:"temperature,omitempty"`
	MaxTokens         *int64   `json:"max_tokens,omitempty"`
	TopP              *float64 `json:"top_p,omitempty"`
	FrequencyPenalty  *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty   *float64 `json:"presence_penalty,omitempty"`
	BaseURL           string   `json:"base_url,omitempty"`
	ParallelToolCalls *bool    `json:"parallel_tool_calls,omitempty"`
	TokenKey          string   `json:"token_key,omitempty"`
	// BypassModelsGateway, when true, forces this model to connect directly to
	// its provider, ignoring any configured models gateway (the --models-gateway
	// flag / CAGENT_MODELS_GATEWAY env var). The model then authenticates with
	// the provider's own credentials (e.g. OPENAI_API_KEY, ANTHROPIC_API_KEY, or
	// token_key) instead of routing through the gateway.
	//
	// A custom base_url (set on the model or inherited from a custom provider
	// definition) implies the bypass even when this flag is false: user-chosen
	// endpoints are never routed through the models gateway.
	//
	// Security note: a model that bypasses the gateway sends the provider's
	// ambient credentials directly to its endpoint. A malicious base_url thus
	// lets a config exfiltrate those credentials to an arbitrary host, so only
	// run configs you trust. For a router model the bypass propagates to its
	// fallback and every routed target.
	BypassModelsGateway bool `json:"bypass_models_gateway,omitempty"`
	// ProviderOpts allows provider-specific options.
	ProviderOpts map[string]any `json:"provider_opts,omitempty"`
	TrackUsage   *bool          `json:"track_usage,omitempty"`
	// ThinkingBudget controls reasoning effort/budget.
	// Accepts an integer token count or a string effort level.
	// See [effort.ValidNames] for the full list of accepted strings.
	// Provider-specific mappings are in the effort package.
	ThinkingBudget *ThinkingBudget `json:"thinking_budget,omitempty"`
	// TaskBudget caps the total tokens a model can spend across an agentic task.
	// Forwarded to Anthropic as `output_config.task_budget` for every Claude
	// model — docker-agent does not gate by model name. At the time of writing,
	// only Claude Opus 4.7 actually honors it; other models will reject the
	// field. Accepts an integer token count or a {type: tokens, total: N} object.
	TaskBudget *TaskBudget `json:"task_budget,omitempty"`
	// Auth selects a non-API-key authentication scheme for this model
	// (currently: Anthropic Workload Identity Federation). When set, it
	// takes precedence over both the provider's API-key path and any
	// auth defined on the referenced ProviderConfig.
	Auth *AuthConfig `json:"auth,omitempty"`
	// Routing defines rules for routing requests to different models.
	// When routing is configured, this model becomes a rule-based router:
	// - The provider/model fields define the fallback model
	// - Each routing rule maps to a different model based on examples
	Routing []RoutingRule `json:"routing,omitempty"`
	// FirstAvailable lists candidate model references, in priority order.
	// At load time docker-agent selects the first candidate whose credentials
	// are configured. Candidates may be named models or inline "provider/model"
	// specs. Local providers (dmr, ollama) need no credentials and make reliable
	// final fallbacks. When set, other model configuration fields must be empty.
	FirstAvailable []string `json:"first_available,omitempty"`
	// TitleModel names the model used to generate session titles when an agent
	// runs with this model. It lets a heavyweight primary model delegate the
	// cheap title-generation call to a smaller/faster model. The value can be a
	// model name from the models section or an inline "provider/model" spec.
	// When empty, title generation reuses the agent's own model.
	TitleModel string `json:"title_model,omitempty"`
	// CompactionModel names the model used to summarize the conversation when
	// this model's session is compacted (manual /compact, the proactive
	// threshold trigger, or post-overflow recovery). The summary call ingests
	// the whole conversation, so it is the slowest and most expensive call in a
	// session; pointing this at a smaller/faster model makes compaction cheaper
	// and more reliable without changing the model that runs the conversation.
	// The value can be a model name from the models section or an inline
	// "provider/model" spec. When empty, compaction reuses the agent's own
	// model. If the compaction model has a smaller context window than the
	// primary, compaction is triggered against the smaller window so the
	// summary call can always ingest the conversation it must compact.
	CompactionModel string `json:"compaction_model,omitempty"`
	// CompactionThreshold overrides, for agents running this model, the
	// fraction of the context window at which proactive auto-compaction
	// triggers. Must be greater than 0 and at most 1. It takes precedence over
	// the agent-level `compaction_threshold`; when both are unset the default
	// of 0.9 applies. Useful next to CompactionModel: a model that compacts
	// with a slower/smaller summarizer may want to trigger earlier.
	CompactionThreshold *float64 `json:"compaction_threshold,omitempty"`
	// Capabilities optionally declares the model's attachment capabilities,
	// overriding the automatic models.dev-based detection. See [CapabilitiesConfig].
	Capabilities *CapabilitiesConfig `json:"capabilities,omitempty"`
	// Cost optionally declares the model's token pricing explicitly,
	// overriding the models.dev catalogue. See [CostConfig].
	Cost *CostConfig `json:"cost,omitempty"`
}

// CostConfig declares a model's token pricing explicitly, in USD per one
// million tokens. When set it takes precedence over the models.dev catalogue
// for per-turn cost computation, session cost tracking, and the after_llm_call
// hook's cost field. It also makes an uncatalogued model "priced", so custom
// endpoints (base_url providers, local models, private deployments) can carry
// accurate spend accounting instead of billing $0.
type CostConfig struct {
	// Input is the USD price per 1M input tokens.
	Input float64 `json:"input,omitempty"`
	// Output is the USD price per 1M output tokens.
	Output float64 `json:"output,omitempty"`
	// CacheRead is the USD price per 1M cached input tokens.
	CacheRead float64 `json:"cache_read,omitempty"`
	// CacheWrite is the USD price per 1M cache-write tokens.
	CacheWrite float64 `json:"cache_write,omitempty"`
}

// validate rejects negative prices. All-zero is valid and means "priced,
// free" — distinct from an unset Cost (unpriced).
func (c *CostConfig) validate() error {
	if c == nil {
		return nil
	}
	for _, f := range []struct {
		name  string
		value float64
	}{
		{"input", c.Input},
		{"output", c.Output},
		{"cache_read", c.CacheRead},
		{"cache_write", c.CacheWrite},
	} {
		if f.value < 0 {
			return fmt.Errorf("cost.%s must not be negative, got %v", f.name, f.value)
		}
	}
	return nil
}

// CapabilitiesConfig declares a model's attachment capabilities explicitly,
// overriding docker-agent's automatic detection (which queries the models.dev
// catalogue). It exists for models the catalogue does not describe correctly:
// custom OpenAI-compatible providers, local models (e.g. Ollama), and model
// versions that have been dropped from the catalogue. Without it, such models
// fall back to text-only and their image/PDF attachments are silently dropped.
//
// When set, the declared flags are authoritative and no models.dev lookup is
// performed. When nil (the default), capabilities are detected from models.dev.
// The declared flags must match what the underlying endpoint actually accepts;
// claiming an unsupported modality results in a provider-side API error.
type CapabilitiesConfig struct {
	// Image reports whether the model accepts image attachments.
	Image bool `json:"image,omitempty"`
	// PDF reports whether the model accepts PDF (application/pdf) attachments.
	PDF bool `json:"pdf,omitempty"`
}

// IsFirstAvailable reports whether this model is a first-available selector
// (i.e. it picks the first candidate with configured credentials).
func (m *ModelConfig) IsFirstAvailable() bool {
	return m != nil && m.FirstAvailable != nil
}

// Clone returns a deep copy of the ModelConfig.
func (m *ModelConfig) Clone() *ModelConfig {
	if m == nil {
		return nil
	}
	var c ModelConfig
	types.CloneThroughJSON(m, &c)
	// Preserve fields excluded from JSON serialization
	c.Name = m.Name
	c.DisplayModel = m.DisplayModel
	return &c
}

// DisplayOrModel returns DisplayModel if set (i.e., alias resolution preserved the original name),
// otherwise falls back to Model.
func (m *ModelConfig) DisplayOrModel() string {
	return cmp.Or(m.DisplayModel, m.Model)
}

// UnloadAPI returns the unload endpoint inherited from the model's
// provider config, or "" when no `unload_api` was set. Populated by
// the provider-config merge step from [ProviderConfig.UnloadAPI].
func (m *ModelConfig) UnloadAPI() string {
	v, _ := m.ProviderOpts["unload_api"].(string)
	return v
}

// ContextSizeFromProviderOpts reads a positive `context_size` from a
// provider_opts map, returning 0 when the key is absent, non-positive, or not
// parseable as an integer. Accepted shapes mirror what the YAML/JSON decoders
// produce: goccy/go-yaml decodes a positive integer as uint64 (and a negative
// one as int64), JSON decodes numbers as float64, and env expansion can leave a
// decimal string. Shared by the runtime (context-limit resolution) and the
// provider clients (max_tokens clamping) so the two never diverge on how
// context_size is interpreted.
func ContextSizeFromProviderOpts(opts map[string]any) int64 {
	v, ok := opts["context_size"]
	if !ok {
		return 0
	}
	var n int64
	switch t := v.(type) {
	case int64:
		n = t
	case int:
		n = int64(t)
	case int32:
		n = int64(t)
	case uint64:
		if t > math.MaxInt64 {
			return 0
		}
		n = int64(t)
	case uint:
		if uint64(t) > math.MaxInt64 {
			return 0
		}
		n = int64(t)
	case uint32:
		n = int64(t)
	case float64:
		n = int64(t)
	case float32:
		n = int64(t)
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		if err != nil {
			return 0
		}
		n = parsed
	default:
		return 0
	}
	if n <= 0 {
		return 0
	}
	return n
}

// ExpandEnv rewrites the model config's value-bearing string fields in place by
// passing each through expand, which substitutes ${env.X} / ${X} references
// against the runtime environment. Only fields that carry a literal value the
// provider sends or dials are expanded: model (the model identifier) and
// base_url (the endpoint). token_key is intentionally excluded because it names
// an environment variable rather than holding a value, and reference-only
// fields (provider, routing/fallback specs) are resolved before a provider is
// built.
//
// expand is injected so this package keeps no dependency on pkg/environment,
// which already depends on the config packages. A nil expand or an empty field
// is a no-op; the first expansion error is returned and stops processing.
// Addresses issue #2261, where ${env.X} in a model's model field reached the
// provider verbatim instead of being substituted.
func (m *ModelConfig) ExpandEnv(expand func(string) (string, error)) error {
	if m == nil || expand == nil {
		return nil
	}
	for _, field := range []*string{&m.Model, &m.BaseURL} {
		if *field == "" {
			continue
		}
		expanded, err := expand(*field)
		if err != nil {
			return err
		}
		*field = expanded
	}
	return nil
}

// FlexibleModelConfig wraps ModelConfig to support both shorthand and full syntax.
// It can be unmarshaled from either:
//   - A shorthand string: "provider/model" (e.g., "anthropic/claude-sonnet-4-5")
//   - A full model definition with all options
type FlexibleModelConfig struct {
	ModelConfig
}

// UnmarshalYAML implements custom unmarshaling for flexible model config
func (f *FlexibleModelConfig) UnmarshalYAML(unmarshal func(any) error) error {
	// Try string shorthand first
	var shorthand string
	if err := unmarshal(&shorthand); err == nil && shorthand != "" {
		parsed, parseErr := ParseModelRef(shorthand)
		if parseErr != nil {
			return fmt.Errorf("invalid model shorthand %q: expected format 'provider/model'", shorthand)
		}
		f.Provider = parsed.Provider
		f.Model = parsed.Model
		return nil
	}

	// Try full model config
	var cfg ModelConfig
	if err := unmarshal(&cfg); err != nil {
		return err
	}
	f.ModelConfig = cfg
	return nil
}

// MarshalYAML outputs shorthand format if only provider/model are set
func (f FlexibleModelConfig) MarshalYAML() (any, error) {
	if f.isShorthandOnly() {
		return f.Provider + "/" + f.Model, nil
	}
	return f.ModelConfig, nil
}

// isShorthandOnly returns true if only provider and model are set
func (f *FlexibleModelConfig) isShorthandOnly() bool {
	return f.Temperature == nil &&
		f.MaxTokens == nil &&
		f.TopP == nil &&
		f.FrequencyPenalty == nil &&
		f.PresencePenalty == nil &&
		f.BaseURL == "" &&
		f.ParallelToolCalls == nil &&
		f.TokenKey == "" &&
		!f.BypassModelsGateway &&
		len(f.ProviderOpts) == 0 &&
		f.TrackUsage == nil &&
		f.ThinkingBudget == nil &&
		f.TaskBudget == nil &&
		len(f.Routing) == 0 &&
		f.FirstAvailable == nil &&
		f.TitleModel == "" &&
		f.CompactionModel == "" &&
		f.CompactionThreshold == nil &&
		f.Capabilities == nil &&
		f.Cost == nil
}

// RoutingRule defines a single routing rule for model selection.
// Each rule maps example phrases to a target model.
type RoutingRule struct {
	// Model is a reference to another model in the models section or an inline model spec (e.g., "openai/gpt-4o")
	Model string `json:"model"`
	// Examples are phrases that should trigger routing to this model
	Examples []string `json:"examples"`
}

type Metadata struct {
	Author      string   `json:"author,omitempty"`
	License     string   `json:"license,omitempty"`
	Description string   `json:"description,omitempty"`
	Readme      string   `json:"readme,omitempty"`
	Version     string   `json:"version,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// Commands represents a set of named prompts for quick-starting conversations.
// It supports two YAML formats:
//
// commands:
//
//	df: "check disk space"
//	ls: "list files"
//
// or
//
// commands:
//   - df: "check disk space"
//   - ls: "list files"
// Commands YAML unmarshalling is implemented in pkg/config/types/commands.go

// ScriptShellToolConfig represents a custom shell tool configuration
type ScriptShellToolConfig struct {
	Cmd         string `json:"cmd"`
	Description string `json:"description"`

	// Args is directly passed as "properties" in the JSON schema
	Args map[string]any `json:"args,omitempty"`

	// Required is directly passed as "required" in the JSON schema
	Required []string `json:"required"`

	Env        map[string]string `json:"env,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
}

type APIToolConfig struct {
	Instruction string            `json:"instruction,omitempty"`
	Name        string            `json:"name,omitempty"`
	Required    []string          `json:"required,omitempty"`
	Args        map[string]any    `json:"args,omitempty"`
	Endpoint    string            `json:"endpoint,omitempty"`
	Method      string            `json:"method,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	// OutputSchema optionally describes the API response as JSON Schema for MCP/Code Mode consumers; runtime still returns the raw string body.
	OutputSchema map[string]any `json:"output_schema,omitempty"`
}

// PostEditConfig represents a post-edit command configuration
type PostEditConfig struct {
	Path string `json:"path"`
	Cmd  string `json:"cmd"`
}

// Toolset represents a tool configuration
type Toolset struct {
	Type        string   `json:"type,omitempty"`
	Tools       []string `json:"tools,omitempty"`
	Instruction string   `json:"instruction,omitempty"`
	Toon        string   `json:"toon,omitempty"`

	// ReadOnly restricts the toolset to tools whose annotations carry a
	// read-only hint. Every other tool is filtered out, so the agent can
	// list and call only the non-mutating subset of the toolset.
	ReadOnly bool `json:"readonly,omitempty" yaml:"readonly,omitempty"`

	// Model overrides the LLM used for the turn that processes tool results
	// from this toolset, enabling per-toolset model routing. Value can be a
	// model name from the models section or "provider/model" (e.g. "openai/gpt-4o-mini").
	Model string `json:"model,omitempty"`

	Defer DeferConfig `json:"defer,omitzero" yaml:"defer,omitempty"`

	// For the `mcp` tool
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Ref     string   `json:"ref,omitempty"`
	Remote  Remote   `json:"remote"`
	Config  any      `json:"config,omitempty"`

	// For `mcp` and `lsp` tools - version/package reference for auto-installation.
	// Format: "owner/repo" or "owner/repo@version"
	// When empty and auto-install is enabled, docker agent auto-detects from the command name.
	// Set to "false" or "off" to disable auto-install for this toolset.
	Version string `json:"version,omitempty"`

	// For the `a2a`, `api`, `openapi` and `fetch` tools
	Name    string            `json:"name,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// For `shell`, `background_jobs`, `script`, `mcp` or `lsp` tools
	Env map[string]string `json:"env,omitempty"`

	// For the `todo` tool
	Shared bool `json:"shared,omitempty"`

	// For the `memory` and `tasks` tools
	Path string `json:"path,omitempty"`

	// For the `script` tool
	Shell map[string]ScriptShellToolConfig `json:"shell,omitempty"`

	// For the `filesystem` tool - post-edit commands
	PostEdit []PostEditConfig `json:"post_edit,omitempty"`

	APIConfig APIToolConfig `json:"api_config"`

	// For the `filesystem` tool - VCS integration
	IgnoreVCS *bool `json:"ignore_vcs,omitempty"`

	// For the `filesystem` tool - allow-list of directories the tools are
	// permitted to access. Each entry may be "." (the agent's working
	// directory), "~" or "~/..." (the user's home directory), an absolute
	// path, or a relative path (anchored at the working directory). When
	// non-empty, every read/write operation is rejected unless its target
	// resolves under one of the listed roots. Symlinks are followed before
	// the containment check so they cannot be used to escape the allow-list.
	// An empty or omitted list preserves the default behaviour (any path
	// reachable by the process is allowed).
	AllowList []string `json:"allow_list,omitempty" yaml:"allow_list,omitempty"`

	// For the `filesystem` tool - deny-list of directories the tools are
	// forbidden to access. Same expansion and matching rules as `allow_list`.
	// The deny-list takes precedence over `allow_list`: a path that matches
	// both is rejected. An empty or omitted list disables the deny-list.
	DenyList []string `json:"deny_list,omitempty" yaml:"deny_list,omitempty"`

	// For the `mcp_catalog` tool - allow-list of catalog server ids that
	// are offered by default. When non-empty, only these servers are
	// searchable and enableable; every other catalog entry is hidden. An
	// empty or omitted list offers the full catalog. Combine with
	// `blocked_servers` to subtract individual ids from the allowed set
	// (block takes precedence).
	AllowedServers []string `json:"allowed_servers,omitempty" yaml:"allowed_servers,omitempty"`

	// For the `mcp_catalog` tool - block-list of catalog server ids that
	// are removed from the offered set. Applied after `allowed_servers`,
	// so a server listed in both is blocked. An empty or omitted list
	// disables the block-list.
	BlockedServers []string `json:"blocked_servers,omitempty" yaml:"blocked_servers,omitempty"`

	// For the `lsp` tool
	FileTypes []string `json:"file_types,omitempty"`

	// HTTP timeout in seconds for `fetch`, `api`, `openapi`, and `a2a` toolsets.
	// Defaults to 30 seconds when omitted.
	Timeout int `json:"timeout,omitempty"`

	// For the `fetch` tool - allow-list of domains the tool is permitted to fetch.
	// A pattern matches the host exactly (case-insensitive) and any of its subdomains;
	// e.g. "example.com" matches "example.com" and "docs.example.com" but not
	// "badexample.com". A leading dot (".example.com") restricts the match to
	// strict subdomains. Mutually exclusive with `blocked_domains`.
	AllowedDomains []string `json:"allowed_domains,omitempty" yaml:"allowed_domains,omitempty"`

	// For the `fetch` tool - deny-list of domains the tool is forbidden to fetch.
	// Uses the same matching rules as `allowed_domains`. Mutually exclusive with
	// `allowed_domains`.
	BlockedDomains []string `json:"blocked_domains,omitempty" yaml:"blocked_domains,omitempty"`

	// For the `fetch`, `api`, `openapi`, `a2a` and remote `mcp` toolsets — opt in
	// to dialling non-public IP addresses.
	//
	// By default, protected HTTP clients refuse connections (after DNS
	// resolution, so DNS rebinding is also blocked) to loopback (127/8,
	// ::1), RFC1918 private ranges, link-local — including the cloud
	// metadata endpoint at 169.254.169.254 — multicast and the unspecified
	// address. Set this to true to permit those addresses, which is required
	// when an agent legitimately needs to call internal services.
	//
	// For `fetch`, `allowed_domains` and `blocked_domains` are evaluated
	// independently of this flag: even with `allow_private_ips: true`, an
	// entry in `blocked_domains` (or absence from `allowed_domains`) still
	// rejects the request before any network call.
	// nil means the field was omitted and may inherit from a referenced definition.
	AllowPrivateIPs *bool `json:"allow_private_ips,omitempty" yaml:"allow_private_ips,omitempty"`

	// For the `shell` toolset — opt in to a sudo privilege escalation flow.
	// When enabled, sudo commands prompt the user for their password (masked)
	// through the host UI via SUDO_ASKPASS; in non-interactive runs the prompt
	// is declined automatically and sudo fails as before. No effect on Windows.
	// nil/false keeps the default behaviour (sudo has no TTY and fails fast).
	SudoAskpass *bool `json:"sudo_askpass,omitempty" yaml:"sudo_askpass,omitempty"`

	// For the `background_jobs` toolset — opt in to background-job recall. When
	// enabled, run_background_job exposes a recall boolean parameter. If the
	// agent sets recall:true on a background job, the runtime injects a steering
	// message with a short completion sentence and the job output when the job
	// finishes.
	Recall *bool `json:"recall,omitempty" yaml:"recall,omitempty"`

	// For the `shell` toolset — opt in to destructive-command detection.
	// When enabled, the agent auto-registers the safer_shell builtin under
	// pre_tool_use with preempt_yolo:true. Destructive commands (rm -rf, docker
	// volume rm, mkfs, …) get an Ask verdict carrying a blast-radius
	// classification; known-safe reads (ls, git status, docker ps, …)
	// flow through silently; everything else asks with blast_radius=unknown
	// so the user sees the prompt before --yolo or permission allow-rules
	// can auto-approve it. nil/false leaves the agent's shell calls subject
	// only to the regular approval pipeline.
	Safer *bool `json:"safer,omitempty" yaml:"safer,omitempty"`

	// For the `rag` tool
	RAGConfig *RAGConfig `json:"rag_config,omitempty" yaml:"rag_config,omitempty"`

	// For the `model_picker` tool
	Models []string `json:"models,omitempty"`

	// For `mcp` and `lsp` tools - optional working directory override.
	// When set, the toolset process is started from this directory.
	// Relative paths are resolved relative to the agent's working directory.
	WorkingDir string `json:"working_dir,omitempty"`

	// For `mcp` and `lsp` tools — lifecycle policy controlling startup,
	// restart, and backoff behaviour. nil means "use the resilient defaults"
	// (auto-restart on failure, 5 attempts, 1s..32s exponential backoff).
	Lifecycle *LifecycleConfig `json:"lifecycle,omitempty"`
}

func (t *Toolset) UnmarshalYAML(unmarshal func(any) error) error {
	type alias Toolset
	var tmp alias
	if err := unmarshal(&tmp); err != nil {
		return err
	}
	*t = Toolset(tmp)
	return t.validate()
}

// AllowPrivateIPsEnabled reports the effective boolean value for allow_private_ips.
func (t *Toolset) AllowPrivateIPsEnabled() bool {
	return t != nil && t.AllowPrivateIPs != nil && *t.AllowPrivateIPs
}

type Remote struct {
	URL           string             `json:"url"`
	TransportType string             `json:"transport_type,omitempty"`
	Headers       map[string]string  `json:"headers,omitempty"`
	OAuth         *RemoteOAuthConfig `json:"oauth,omitempty"`
}

// RemoteOAuthConfig holds explicit OAuth credentials for remote MCP servers
// that do not support Dynamic Client Registration (RFC 7591).
type RemoteOAuthConfig struct {
	ClientID     string   `json:"clientId"`
	ClientSecret string   `json:"clientSecret,omitempty"`
	CallbackPort int      `json:"callbackPort,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	// CallbackRedirectURL, when set, is used as the OAuth redirect URI
	// instead of the default http://127.0.0.1:{callbackPort}/callback.
	// This allows inserting a public-facing proxy (e.g. a URL shortener or
	// a pre-registered static redirect) in front of the local callback
	// server — useful for authorization servers that require the redirect
	// URI to be HTTPS or pre-registered.
	//
	// The literal placeholder ${callbackPort} is replaced with the actual
	// port the local callback server is listening on (either CallbackPort
	// when set, or a random free port otherwise). The external URL is
	// expected to redirect the browser back to
	// http://127.0.0.1:{callbackPort}/callback preserving the OAuth query
	// parameters.
	CallbackRedirectURL string `json:"callbackRedirectURL,omitempty"`
}

// DeferConfig represents the deferred loading configuration for a toolset.
// It can be either a boolean (true to defer all tools) or a slice of strings
// (list of tool names to defer).
type DeferConfig struct { //nolint:recvcheck // MarshalYAML must use value receiver for YAML slice encoding, UnmarshalYAML must use pointer
	// DeferAll is true when all tools should be deferred
	DeferAll bool `json:"-"`
	// Tools is the list of specific tool names to defer (empty if DeferAll is true)
	Tools []string `json:"-"`
}

func (d DeferConfig) IsEmpty() bool {
	return !d.DeferAll && len(d.Tools) == 0
}

func (d *DeferConfig) UnmarshalYAML(unmarshal func(any) error) error {
	var b bool
	if err := unmarshal(&b); err == nil {
		d.DeferAll = b
		d.Tools = nil
		return nil
	}

	var tools []string
	if err := unmarshal(&tools); err == nil {
		d.DeferAll = false
		d.Tools = tools
		return nil
	}

	return nil
}

func (d DeferConfig) MarshalYAML() (any, error) {
	if d.DeferAll {
		return true, nil
	}
	if len(d.Tools) == 0 {
		// Return false for empty config - this will be omitted by yaml encoder
		return false, nil
	}
	return d.Tools, nil
}

// ThinkingBudget represents reasoning budget configuration.
// It accepts either a string effort level (see [effort.ValidNames]) or an
// integer token budget.
type ThinkingBudget struct {
	// Effort stores string-based reasoning effort levels
	Effort string `json:"effort,omitempty"`
	// Tokens stores integer-based token budgets
	Tokens int `json:"tokens,omitempty"`
}

func (t *ThinkingBudget) UnmarshalYAML(unmarshal func(any) error) error {
	// Try integer tokens first
	var n int
	if err := unmarshal(&n); err == nil {
		*t = ThinkingBudget{Tokens: n}
		return nil
	}

	// Try string level
	var s string
	if err := unmarshal(&s); err == nil {
		if !effort.IsValid(s) {
			return fmt.Errorf("invalid thinking_budget effort %q: must be one of %s", s, effort.ValidNames())
		}
		*t = ThinkingBudget{Effort: s}
		return nil
	}

	return nil
}

// MarshalYAML implements custom marshaling to output simple string or int format
func (t ThinkingBudget) MarshalYAML() (any, error) {
	// If Effort string is set (non-empty), marshal as string
	if t.Effort != "" {
		return t.Effort, nil
	}

	// Otherwise marshal as integer (includes 0, -1, and positive values)
	return t.Tokens, nil
}

// IsDisabled returns true if the thinking budget is explicitly disabled.
// A nil receiver is treated as "not configured" (not disabled).
//
// Disabled when:
//   - Tokens == 0 with no Effort (thinking_budget: 0)
//   - Effort == "none" (thinking_budget: none)
//
// NOT disabled when:
//   - Tokens > 0 or Tokens == -1 (explicit token budget)
//   - Effort is a real level like "medium" or "high"
//   - Effort is "adaptive"
func (t *ThinkingBudget) IsDisabled() bool {
	if t == nil {
		return false
	}
	if t.Tokens == 0 && t.Effort == "" {
		return true
	}
	return strings.EqualFold(t.Effort, "none")
}

// IsAdaptive returns true if the thinking budget is set to adaptive mode.
// Adaptive thinking lets the model decide how much thinking to do.
// Matches both "adaptive" and "adaptive/<effort>" formats.
func (t *ThinkingBudget) IsAdaptive() bool {
	if t == nil {
		return false
	}
	norm := strings.ToLower(strings.TrimSpace(t.Effort))
	return norm == "adaptive" || strings.HasPrefix(norm, "adaptive/")
}

// EffortLevel parses the Effort field into an [effort.Level].
// Returns ("", false) when the budget is nil, uses token counts, or has an
// unrecognised effort string.
func (t *ThinkingBudget) EffortLevel() (effort.Level, bool) {
	if t == nil {
		return "", false
	}
	return effort.Parse(t.Effort)
}

// AdaptiveEffort returns the effort level for adaptive thinking.
// For "adaptive" it returns the default ("high").
// For "adaptive/<effort>" it returns the specified effort.
// Returns ("", false) if the budget is not adaptive.
func (t *ThinkingBudget) AdaptiveEffort() (string, bool) {
	if !t.IsAdaptive() {
		return "", false
	}
	norm := strings.ToLower(strings.TrimSpace(t.Effort))
	if after, ok := strings.CutPrefix(norm, "adaptive/"); ok && after != "" {
		return after, true
	}
	return "high", true
}

// EffortTokens maps a string effort level to a token budget for providers
// that only support token-based thinking (e.g. Bedrock Claude).
// Delegates to [effort.BedrockTokens].
//
// Returns (tokens, true) when a mapping exists, or (0, false) when
// the budget uses an explicit token count or an unrecognised effort string.
func (t *ThinkingBudget) EffortTokens() (int, bool) {
	l, ok := t.EffortLevel()
	if !ok {
		return 0, false
	}
	return effort.BedrockTokens(l)
}

// MarshalJSON implements custom marshaling to output simple string or int format
// This ensures JSON and YAML have the same flattened format for consistency
func (t ThinkingBudget) MarshalJSON() ([]byte, error) {
	// If Effort string is set (non-empty), marshal as string
	if t.Effort != "" {
		return fmt.Appendf(nil, "%q", t.Effort), nil
	}

	// Otherwise marshal as integer (includes 0, -1, and positive values)
	return fmt.Appendf(nil, "%d", t.Tokens), nil
}

// UnmarshalJSON implements custom unmarshaling to accept simple string or int format
// This ensures JSON and YAML have the same flattened format for consistency
func (t *ThinkingBudget) UnmarshalJSON(data []byte) error {
	// Try integer tokens first
	var n int
	if err := json.Unmarshal(data, &n); err == nil {
		*t = ThinkingBudget{Tokens: n}
		return nil
	}

	// Try string level
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		if !effort.IsValid(s) {
			return fmt.Errorf("invalid thinking_budget effort %q: must be one of %s", s, effort.ValidNames())
		}
		*t = ThinkingBudget{Effort: s}
		return nil
	}

	return nil
}

// TaskBudget caps the total tokens a model can spend across an agentic task
// (combined thinking, tool calls, and final output). It is forwarded to
// Anthropic as `output_config.task_budget` and docker-agent automatically
// attaches the required `task-budgets-2026-03-13` beta header when set.
//
// docker-agent does not gate by model name — any Claude model accepts the
// configuration, though at the time of writing only Claude Opus 4.7 actually
// honors it; other models will reject requests containing the field. See:
// https://platform.claude.com/docs/en/about-claude/models/whats-new-claude-4-7
//
// Accepted YAML/JSON forms:
//   - Integer shorthand ("tokens" budget):  task_budget: 128000
//   - Full object:                          task_budget: {type: tokens, total: 128000}
//
// A value of 0 (or an empty object) disables the feature.
type TaskBudget struct {
	// Type is the budget kind. Only "tokens" is supported today; defaults to
	// "tokens" when Total is set via the integer shorthand.
	Type string `json:"type,omitempty"`
	// Total is the total budget value (token count for Type == "tokens").
	Total int `json:"total,omitempty"`
}

// IsZero reports whether the task budget is effectively unset.
//
// A budget is considered unset when Total <= 0 (there is no meaningful
// "zero-token" budget, and validate() already rejects negative totals for
// explicit object forms). This is what lets users disable the feature with
// the shorthand `task_budget: 0`, which otherwise unmarshals to a non-empty
// {Type: "tokens", Total: 0} struct.
func (t *TaskBudget) IsZero() bool {
	return t == nil || t.Total <= 0
}

// AsMap returns the API representation, or nil when the budget is zero.
func (t *TaskBudget) AsMap() map[string]any {
	if t.IsZero() {
		return nil
	}
	typ := t.Type
	if typ == "" {
		typ = "tokens"
	}
	return map[string]any{"type": typ, "total": t.Total}
}

// validate checks the invariants shared by both YAML and JSON decoding.
func (t *TaskBudget) validate() error {
	if t.Total < 0 {
		return fmt.Errorf("task_budget.total must be non-negative, got %d", t.Total)
	}
	if t.Type != "" && t.Type != "tokens" {
		return fmt.Errorf("task_budget.type %q is not supported (only %q)", t.Type, "tokens")
	}
	return nil
}

// UnmarshalYAML accepts either an integer shorthand (tokens) or a full object.
func (t *TaskBudget) UnmarshalYAML(unmarshal func(any) error) error {
	var n int
	if err := unmarshal(&n); err == nil {
		*t = TaskBudget{Type: "tokens", Total: n}
		return t.validate()
	}
	type alias TaskBudget
	var raw alias
	if err := unmarshal(&raw); err != nil {
		return errors.New("task_budget must be an integer or a {type,total} object")
	}
	*t = TaskBudget(raw)
	return t.validate()
}

// MarshalYAML emits the integer shorthand for a plain token budget, otherwise
// the full {type, total} object.
func (t TaskBudget) MarshalYAML() (any, error) {
	if t.Type == "" || t.Type == "tokens" {
		return t.Total, nil
	}
	return map[string]any{"type": t.Type, "total": t.Total}, nil
}

// UnmarshalJSON mirrors UnmarshalYAML: accepts int shorthand or full object.
func (t *TaskBudget) UnmarshalJSON(data []byte) error {
	var n int
	if err := json.Unmarshal(data, &n); err == nil {
		*t = TaskBudget{Type: "tokens", Total: n}
		return t.validate()
	}
	type alias TaskBudget
	var raw alias
	if err := json.Unmarshal(data, &raw); err != nil {
		return errors.New("task_budget must be an integer or a {type,total} object")
	}
	*t = TaskBudget(raw)
	return t.validate()
}

// MarshalJSON emits the integer shorthand for a plain token budget.
func (t TaskBudget) MarshalJSON() ([]byte, error) {
	if t.Type == "" || t.Type == "tokens" {
		return json.Marshal(t.Total)
	}
	return json.Marshal(map[string]any{"type": t.Type, "total": t.Total})
}

// StructuredOutput defines a JSON schema for structured output
type StructuredOutput struct {
	// Name is the name of the response format
	Name string `json:"name"`
	// Description is optional description of the response format
	Description string `json:"description,omitempty"`
	// Schema is a JSON schema object defining the structure
	Schema map[string]any `json:"schema"`
	// Strict enables strict schema adherence (OpenAI only)
	Strict bool `json:"strict,omitempty"`
}

// RAGToolConfig represents tool-specific configuration for a RAG source
type RAGToolConfig struct {
	Name        string `json:"name,omitempty"`        // Custom name for the tool (defaults to RAG source name if empty)
	Description string `json:"description,omitempty"` // Tool description (what the tool does)
	Instruction string `json:"instruction,omitempty"` // Tool instruction (how to use the tool effectively)
}

// RAGConfig represents a RAG (Retrieval-Augmented Generation) configuration
// Uses a unified strategies array for flexible, extensible configuration
type RAGConfig struct {
	Tool       RAGToolConfig       `json:"tool"`                  // Tool configuration
	Docs       []string            `json:"docs,omitempty"`        // Shared documents across all strategies
	RespectVCS *bool               `json:"respect_vcs,omitempty"` // Whether to respect VCS ignore files like .gitignore (default: true)
	Strategies []RAGStrategyConfig `json:"strategies,omitempty"`  // Array of strategy configurations
	Results    RAGResultsConfig    `json:"results"`
}

// GetRespectVCS returns whether VCS ignore files should be respected, defaulting to true
func (c *RAGConfig) GetRespectVCS() bool {
	if c.RespectVCS == nil {
		return true
	}
	return *c.RespectVCS
}

// RAGStrategyConfig represents a single retrieval strategy configuration
// Strategy-specific fields are stored in Params (validated by strategy implementation)
type RAGStrategyConfig struct { //nolint:recvcheck // Marshal methods must use value receiver for YAML/JSON slice encoding, Unmarshal must use pointer
	Type     string            `json:"type"`            // Strategy type: "chunked-embeddings", "bm25", etc.
	Docs     []string          `json:"docs,omitempty"`  // Strategy-specific documents (augments shared docs)
	Database RAGDatabaseConfig `json:"database"`        // Database configuration
	Chunking RAGChunkingConfig `json:"chunking"`        // Chunking configuration
	Limit    int               `json:"limit,omitempty"` // Max results from this strategy (for fusion input)

	// Strategy-specific parameters (arbitrary key-value pairs)
	// Examples:
	// - chunked-embeddings: embedding_model, similarity_metric, threshold, vector_dimensions
	// - bm25: k1, b, threshold
	Params map[string]any // Flattened into parent JSON
}

// UnmarshalYAML implements custom unmarshaling to capture all extra fields into Params
// This allows strategies to have flexible, strategy-specific configuration parameters
// without requiring changes to the core config schema
func (s *RAGStrategyConfig) UnmarshalYAML(unmarshal func(any) error) error {
	// First unmarshal into a map to capture everything
	var raw map[string]any
	if err := unmarshal(&raw); err != nil {
		return err
	}

	// Extract known fields
	if t, ok := raw["type"].(string); ok {
		s.Type = t
		delete(raw, "type")
	}

	if docs, ok := raw["docs"].([]any); ok {
		s.Docs = make([]string, len(docs))
		for i, d := range docs {
			if str, ok := d.(string); ok {
				s.Docs[i] = str
			}
		}
		delete(raw, "docs")
	}

	if dbRaw, ok := raw["database"]; ok {
		// Unmarshal database config using helper
		var db RAGDatabaseConfig
		unmarshalDatabaseConfig(dbRaw, &db)
		s.Database = db
		delete(raw, "database")
	}

	if chunkRaw, ok := raw["chunking"]; ok {
		var chunk RAGChunkingConfig
		unmarshalChunkingConfig(chunkRaw, &chunk)
		s.Chunking = chunk
		delete(raw, "chunking")
	}

	if limit, ok := raw["limit"].(int); ok {
		s.Limit = limit
		delete(raw, "limit")
	}

	// Everything else goes into Params for strategy-specific configuration
	s.Params = raw

	return nil
}

// MarshalYAML implements custom marshaling to flatten Params into parent level
func (s RAGStrategyConfig) MarshalYAML() (any, error) {
	result := s.buildFlattenedMap()
	return result, nil
}

// MarshalJSON implements custom marshaling to flatten Params into parent level
// This ensures JSON and YAML have the same flattened format for consistency
func (s RAGStrategyConfig) MarshalJSON() ([]byte, error) {
	result := s.buildFlattenedMap()
	return json.Marshal(result)
}

// UnmarshalJSON implements custom unmarshaling to capture all extra fields into Params
// This ensures JSON and YAML have the same flattened format for consistency
func (s *RAGStrategyConfig) UnmarshalJSON(data []byte) error {
	// First unmarshal into a map to capture everything
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	// Extract known fields
	if t, ok := raw["type"].(string); ok {
		s.Type = t
		delete(raw, "type")
	}

	if docs, ok := raw["docs"].([]any); ok {
		s.Docs = make([]string, len(docs))
		for i, d := range docs {
			if str, ok := d.(string); ok {
				s.Docs[i] = str
			}
		}
		delete(raw, "docs")
	}

	if dbRaw, ok := raw["database"]; ok {
		if dbStr, ok := dbRaw.(string); ok {
			var db RAGDatabaseConfig
			db.value = dbStr
			s.Database = db
		}
		delete(raw, "database")
	}

	if chunkRaw, ok := raw["chunking"]; ok {
		// Re-marshal and unmarshal chunking config
		chunkBytes, _ := json.Marshal(chunkRaw)
		var chunk RAGChunkingConfig
		if err := json.Unmarshal(chunkBytes, &chunk); err == nil {
			s.Chunking = chunk
		}
		delete(raw, "chunking")
	}

	if limit, ok := raw["limit"].(float64); ok {
		s.Limit = int(limit)
		delete(raw, "limit")
	}

	// Everything else goes into Params for strategy-specific configuration
	s.Params = raw

	return nil
}

// buildFlattenedMap creates a flattened map representation for marshaling
// Used by both MarshalYAML and MarshalJSON to ensure consistent format
func (s RAGStrategyConfig) buildFlattenedMap() map[string]any {
	result := make(map[string]any)

	if s.Type != "" {
		result["type"] = s.Type
	}
	if len(s.Docs) > 0 {
		result["docs"] = s.Docs
	}
	if !s.Database.IsEmpty() {
		dbStr, _ := s.Database.AsString()
		result["database"] = dbStr
	}
	// Only include chunking if any fields are set
	if s.Chunking.Size > 0 || s.Chunking.Overlap > 0 || s.Chunking.RespectWordBoundaries {
		result["chunking"] = s.Chunking
	}
	if s.Limit > 0 {
		result["limit"] = s.Limit
	}

	// Flatten Params into the same level
	maps.Copy(result, s.Params)

	return result
}

// unmarshalDatabaseConfig handles DatabaseConfig unmarshaling from raw YAML data.
// For RAG strategies, the database configuration is intentionally simple:
// a single string value under the `database` key that points to the SQLite
// database file on disk. TODO(krissetto): eventually support more db types
func unmarshalDatabaseConfig(src any, dst *RAGDatabaseConfig) {
	s, ok := src.(string)
	if !ok {
		return
	}

	dst.value = s
}

// unmarshalChunkingConfig handles ChunkingConfig unmarshaling from raw YAML data
func unmarshalChunkingConfig(src any, dst *RAGChunkingConfig) {
	m, ok := src.(map[string]any)
	if !ok {
		return
	}

	// Handle size - try various numeric types that YAML might produce
	if size, ok := m["size"]; ok {
		dst.Size = coerceToInt(size)
	}

	// Handle overlap - try various numeric types that YAML might produce
	if overlap, ok := m["overlap"]; ok {
		dst.Overlap = coerceToInt(overlap)
	}

	// Handle respect_word_boundaries - YAML should give us a bool
	if rwb, ok := m["respect_word_boundaries"]; ok {
		if val, ok := rwb.(bool); ok {
			dst.RespectWordBoundaries = val
		}
	}

	// Handle code_aware - YAML should give us a bool
	if ca, ok := m["code_aware"]; ok {
		if val, ok := ca.(bool); ok {
			dst.CodeAware = val
		}
	}
}

// coerceToInt converts various numeric types to int
func coerceToInt(v any) int {
	switch val := v.(type) {
	case int:
		return val
	case int64:
		return int(val)
	case uint64:
		return int(val) //nolint:gosec // value comes from validated YAML config; bounds enforced by schema
	case float64:
		return int(val)
	default:
		return 0
	}
}

// RAGDatabaseConfig represents database configuration for RAG strategies.
// Currently it only supports a single string value which is interpreted as
// the path to a SQLite database file.
type RAGDatabaseConfig struct {
	value any // nil (unset) or string path
}

// UnmarshalYAML implements custom unmarshaling for DatabaseConfig
func (d *RAGDatabaseConfig) UnmarshalYAML(unmarshal func(any) error) error {
	var str string
	if err := unmarshal(&str); err == nil {
		d.value = str
		return nil
	}

	return errors.New("database must be a string path to a sqlite database")
}

// AsString returns the database config as a connection string
// For simple string configs, returns as-is
// For structured configs, builds connection string based on type
func (d *RAGDatabaseConfig) AsString() (string, error) {
	if d.value == nil {
		return "", nil
	}

	if str, ok := d.value.(string); ok {
		return str, nil
	}

	return "", errors.New("invalid database configuration: expected string path")
}

// IsEmpty returns true if no database is configured
func (d *RAGDatabaseConfig) IsEmpty() bool {
	return d.value == nil
}

// RAGChunkingConfig represents text chunking configuration
type RAGChunkingConfig struct {
	Size                  int  `json:"size,omitempty"`
	Overlap               int  `json:"overlap,omitempty"`
	RespectWordBoundaries bool `json:"respect_word_boundaries,omitempty"`
	// CodeAware enables code-aware chunking for source files. When true, the
	// chunking strategy uses tree-sitter for AST-based chunking, producing
	// semantically aligned chunks (e.g., whole functions). Falls back to
	// plain text chunking for unsupported languages.
	CodeAware bool `json:"code_aware,omitempty"`
}

// UnmarshalYAML implements custom unmarshaling to apply sensible defaults for chunking
func (c *RAGChunkingConfig) UnmarshalYAML(unmarshal func(any) error) error {
	// Use a struct with pointer to distinguish "not set" from "explicitly set to false"
	var raw struct {
		Size                  int   `yaml:"size"`
		Overlap               int   `yaml:"overlap"`
		RespectWordBoundaries *bool `yaml:"respect_word_boundaries"`
	}

	if err := unmarshal(&raw); err != nil {
		return err
	}

	c.Size = raw.Size
	c.Overlap = raw.Overlap

	// Apply default of true for RespectWordBoundaries if not explicitly set
	if raw.RespectWordBoundaries != nil {
		c.RespectWordBoundaries = *raw.RespectWordBoundaries
	} else {
		c.RespectWordBoundaries = true
	}

	return nil
}

// RAGResultsConfig represents result post-processing configuration (common across strategies)
type RAGResultsConfig struct {
	Limit             int                 `json:"limit,omitempty"`               // Maximum number of results to return (top K)
	Fusion            *RAGFusionConfig    `json:"fusion,omitempty"`              // How to combine results from multiple strategies
	Reranking         *RAGRerankingConfig `json:"reranking,omitempty"`           // Optional reranking configuration
	Deduplicate       bool                `json:"deduplicate,omitempty"`         // Remove duplicate documents across strategies
	IncludeScore      bool                `json:"include_score,omitempty"`       // Include relevance scores in results
	ReturnFullContent bool                `json:"return_full_content,omitempty"` // Return full document content instead of just matched chunks
}

// RAGRerankingConfig represents reranking configuration
type RAGRerankingConfig struct {
	Model     string  `json:"model"`               // Model reference for reranking (e.g., "hf.co/ggml-org/Qwen3-Reranker-0.6B-Q8_0-GGUF")
	TopK      int     `json:"top_k,omitempty"`     // Optional: only rerank top K results (0 = rerank all)
	Threshold float64 `json:"threshold,omitempty"` // Optional: minimum score threshold after reranking (default: 0.5)
	Criteria  string  `json:"criteria,omitempty"`  // Optional: domain-specific relevance criteria to guide scoring
}

// UnmarshalYAML implements custom unmarshaling to apply sensible defaults for reranking
func (r *RAGRerankingConfig) UnmarshalYAML(unmarshal func(any) error) error {
	// Use a struct with pointer to distinguish "not set" from "explicitly set to 0"
	var raw struct {
		Model     string   `yaml:"model"`
		TopK      int      `yaml:"top_k"`
		Threshold *float64 `yaml:"threshold"`
		Criteria  string   `yaml:"criteria"`
	}

	if err := unmarshal(&raw); err != nil {
		return err
	}

	r.Model = raw.Model
	r.TopK = raw.TopK
	r.Criteria = raw.Criteria

	// Apply default threshold of 0.5 if not explicitly set
	// This filters documents with negative logits (sigmoid < 0.5 = not relevant)
	if raw.Threshold != nil {
		r.Threshold = *raw.Threshold
	} else {
		r.Threshold = 0.5
	}

	return nil
}

// defaultRAGResultsConfig returns the default results configuration
func defaultRAGResultsConfig() RAGResultsConfig {
	return RAGResultsConfig{
		Limit:             15,
		Deduplicate:       true,
		IncludeScore:      false,
		ReturnFullContent: false,
	}
}

// UnmarshalYAML implements custom unmarshaling so we can apply sensible defaults
func (r *RAGResultsConfig) UnmarshalYAML(unmarshal func(any) error) error {
	var raw struct {
		Limit             int                 `json:"limit,omitempty"`
		Fusion            *RAGFusionConfig    `json:"fusion,omitempty"`
		Reranking         *RAGRerankingConfig `json:"reranking,omitempty"`
		Deduplicate       *bool               `json:"deduplicate,omitempty"`
		IncludeScore      *bool               `json:"include_score,omitempty"`
		ReturnFullContent *bool               `json:"return_full_content,omitempty"`
	}

	if err := unmarshal(&raw); err != nil {
		return err
	}

	// Start from defaults and then overwrite with any provided values.
	def := defaultRAGResultsConfig()
	*r = def

	if raw.Limit != 0 {
		r.Limit = raw.Limit
	}
	r.Fusion = raw.Fusion
	r.Reranking = raw.Reranking

	if raw.Deduplicate != nil {
		r.Deduplicate = *raw.Deduplicate
	}
	if raw.IncludeScore != nil {
		r.IncludeScore = *raw.IncludeScore
	}
	if raw.ReturnFullContent != nil {
		r.ReturnFullContent = *raw.ReturnFullContent
	}

	return nil
}

// UnmarshalYAML for RAGConfig ensures that the Results field is always
// initialized with defaults, even when the `results` block is omitted.
func (c *RAGConfig) UnmarshalYAML(unmarshal func(any) error) error {
	type alias RAGConfig
	tmp := alias{
		Results: defaultRAGResultsConfig(),
	}
	if err := unmarshal(&tmp); err != nil {
		return err
	}
	*c = RAGConfig(tmp)
	return nil
}

// RAGFusionConfig represents configuration for combining multi-strategy results
type RAGFusionConfig struct {
	Strategy string             `json:"strategy,omitempty"` // Fusion strategy: "rrf" (Reciprocal Rank Fusion), "weighted", "max"
	K        int                `json:"k,omitempty"`        // RRF parameter k (default: 60)
	Weights  map[string]float64 `json:"weights,omitempty"`  // Strategy weights for weighted fusion
}

// PermissionsConfig represents tool permission configuration.
// Allow/Ask/Deny model. This controls tool call approval behavior:
// - Allow: Tools matching these patterns are auto-approved (like --yolo for specific tools)
// - Ask: Tools matching these patterns always require user approval, even if the tool is read-only
// - Deny: Tools matching these patterns are always rejected, even with --yolo
//
// Patterns support glob-style matching (e.g., "shell", "read_*", "mcp:github:*")
// The evaluation order is: Deny (checked first), then Allow, then Ask (explicit), then default
// (read-only tools auto-approved, others ask)
type PermissionsConfig struct {
	// Allow lists tool name patterns that are auto-approved without user confirmation
	Allow []string `json:"allow,omitempty"`
	// Ask lists tool name patterns that always require user confirmation,
	// even for tools that are normally auto-approved (e.g. read-only tools)
	Ask []string `json:"ask,omitempty"`
	// Deny lists tool name patterns that are always rejected
	Deny []string `json:"deny,omitempty"`
}

// HooksConfig represents the hooks configuration for an agent.
// Hooks allow running shell commands at various points in the agent lifecycle.
type HooksConfig struct {
	// PreToolUse hooks run before tool execution
	PreToolUse HookMatcherConfigs `json:"pre_tool_use,omitempty" yaml:"pre_tool_use,omitempty"`

	// PostToolUse hooks run after a tool completes — both success and
	// failure: a failed tool call still fires this event, with the
	// failure surfaced in tool_response (notably the is_error flag and
	// any error text). Use post_tool_use to react to either outcome
	// (logging, audits, circuit-breakers); branch on tool_response.is_error
	// in the handler when you only want to act on one of them.
	PostToolUse HookMatcherConfigs `json:"post_tool_use,omitempty" yaml:"post_tool_use,omitempty"`

	// PermissionRequest hooks run just before the runtime would prompt
	// the user to approve a tool call (i.e. when neither --yolo nor a
	// permissions rule short-circuited the decision). Hooks may auto-allow
	// or auto-deny via hook_specific_output.permission_decision so the
	// user is not prompted; otherwise the runtime falls through to the
	// usual interactive confirmation. Tool-matched, like pre_tool_use.
	PermissionRequest HookMatcherConfigs `json:"permission_request,omitempty" yaml:"permission_request,omitempty"`

	// SessionStart hooks run when a session begins
	SessionStart HookDefinitions `json:"session_start,omitempty" yaml:"session_start,omitempty"`

	// UserPromptSubmit hooks run once per user message, after the user
	// has submitted their prompt and before the first model call of the
	// turn. The submitted text is passed in the prompt field. Hooks can
	// block submission (decision="block" / continue=false / exit code 2)
	// or contribute additional_context that is spliced into the
	// conversation as a transient system message for that turn only.
	// Sub-sessions (transferred tasks, background agents) do not fire
	// this event because their kick-off message is synthesised by the
	// runtime, not authored by the user.
	UserPromptSubmit HookDefinitions `json:"user_prompt_submit,omitempty" yaml:"user_prompt_submit,omitempty"`

	// UserSteeringMessagesSubmit hooks run once each time the runtime
	// drains the steering queue and appends the queued user messages to
	// the session — i.e. messages the user submitted while the agent was
	// already working (mid-turn, after the model stopped, or while idle
	// before the first model call). The drained messages are passed in
	// the steering_messages field. Like user_prompt_submit, hooks can
	// block the run (decision="block" / continue=false / exit code 2) or
	// contribute additional_context that is spliced into the conversation
	// as a transient system message for the steered turn only — it is NOT
	// persisted to the session.
	UserSteeringMessagesSubmit HookDefinitions `json:"user_steering_messages_submit,omitempty" yaml:"user_steering_messages_submit,omitempty"`

	// UserFollowupSubmit hooks run once each time the runtime dequeues a
	// follow-up message at the end of a turn and starts a fresh turn for
	// it. Follow-ups are user messages queued for end-of-turn processing
	// (the FollowUp API / queue), as opposed to mid-turn steering. The
	// follow-up text is passed in the prompt field. Like
	// user_prompt_submit, hooks can block the run (decision="block" /
	// continue=false / exit code 2) or contribute additional_context that
	// is spliced into the conversation as a transient system message for
	// the follow-up turn only — it is NOT persisted to the session.
	UserFollowupSubmit HookDefinitions `json:"user_followup_submit,omitempty" yaml:"user_followup_submit,omitempty"`

	// TurnStart hooks run at the start of every agent turn (each model
	// call). Their AdditionalContext is appended as transient system
	// messages for that turn only — it is NOT persisted to the session,
	// so per-turn signals (date, prompt files, ...) are recomputed every
	// turn instead of bloating the message history on every resume.
	TurnStart HookDefinitions `json:"turn_start,omitempty" yaml:"turn_start,omitempty"`

	// TurnEnd hooks run once per agent turn when the turn finishes —
	// the symmetric counterpart of TurnStart. Fires no matter why the
	// turn ended: a normal stop, an error, a hook-driven shutdown, the
	// loop detector, or context cancellation. The reason is reported
	// in the hook input's reason field ("normal", "continue",
	// "steered", "error", "canceled", "hook_blocked",
	// "loop_detected"). Observational; output is ignored.
	TurnEnd HookDefinitions `json:"turn_end,omitempty" yaml:"turn_end,omitempty"`

	// BeforeLLMCall hooks run just before each model call (after
	// turn_start). Use this for observability, cost guardrails, or
	// auditing without contributing system messages — turn_start is the
	// right event for the latter.
	BeforeLLMCall HookDefinitions `json:"before_llm_call,omitempty" yaml:"before_llm_call,omitempty"`

	// AfterLLMCall hooks run just after each successful model call,
	// before the response is recorded into the session and tool calls
	// are dispatched. Receives the assistant text content in
	// stop_response.
	AfterLLMCall HookDefinitions `json:"after_llm_call,omitempty" yaml:"after_llm_call,omitempty"`

	// SessionEnd hooks run when a session ends
	SessionEnd HookDefinitions `json:"session_end,omitempty" yaml:"session_end,omitempty"`

	// PreCompact hooks run just before the runtime compacts the session
	// transcript into a summary. The trigger is reported in the source
	// field: "manual" (user-initiated /compact), "auto" (proactive
	// threshold), "overflow" (context-overflow recovery), or
	// "tool_overflow" (proactive after tool results pushed past the
	// threshold). Hooks may block compaction (decision="block" /
	// continue=false / exit code 2) or contribute additional_context
	// that is appended to the compaction prompt — useful for steering
	// the summary without modifying the agent's instruction.
	PreCompact HookDefinitions `json:"pre_compact,omitempty" yaml:"pre_compact,omitempty"`

	// SubagentStop hooks run when a sub-agent (transferred task,
	// background agent, skill sub-session) finishes. The sub-agent's
	// name is passed in agent_name and its final assistant message in
	// stop_response. Useful for handoff auditing and per-sub-agent
	// metrics, separately from the parent's stop event.
	SubagentStop HookDefinitions `json:"subagent_stop,omitempty" yaml:"subagent_stop,omitempty"`

	// OnUserInput hooks run when the agent needs user input
	OnUserInput HookDefinitions `json:"on_user_input,omitempty" yaml:"on_user_input,omitempty"`

	// Stop hooks run when the model finishes responding and is about to hand control back to the user
	Stop HookDefinitions `json:"stop,omitempty" yaml:"stop,omitempty"`

	// Notification hooks run when the agent sends a notification (error, warning) to the user
	Notification HookDefinitions `json:"notification,omitempty" yaml:"notification,omitempty"`

	// OnError hooks run when the runtime hits an error during a turn
	// (model failures, repetitive tool-call loops). Fires alongside
	// Notification with level="error".
	OnError HookDefinitions `json:"on_error,omitempty" yaml:"on_error,omitempty"`

	// OnMaxIterations hooks run when the runtime reaches its configured
	// max_iterations limit. Fires alongside Notification with
	// level="warning".
	OnMaxIterations HookDefinitions `json:"on_max_iterations,omitempty" yaml:"on_max_iterations,omitempty"`

	// OnAgentSwitch hooks run whenever the runtime moves the active
	// agent to a new one — transfer_task, handoff, force_handoff, or
	// the return after a transferred task completes. Observational;
	// useful for audit, transcript, and metrics pipelines.
	OnAgentSwitch HookDefinitions `json:"on_agent_switch,omitempty" yaml:"on_agent_switch,omitempty"`

	// OnSessionResume hooks run when the user explicitly approves the
	// runtime to continue past its configured max_iterations limit.
	// Observational; useful for alerting on extended-runtime sessions.
	OnSessionResume HookDefinitions `json:"on_session_resume,omitempty" yaml:"on_session_resume,omitempty"`

	// OnToolApprovalDecision hooks run after the runtime's tool
	// approval chain resolves a verdict for a tool call. Observational;
	// gives audit pipelines a structured "who approved what" record
	// without re-implementing the chain.
	OnToolApprovalDecision HookDefinitions `json:"on_tool_approval_decision,omitempty" yaml:"on_tool_approval_decision,omitempty"`

	// BeforeCompaction hooks run immediately before a session compaction.
	// Hooks may veto compaction (Decision: "block") or supply a custom
	// summary via HookSpecificOutput.summary, in which case the runtime
	// applies that summary verbatim and skips the LLM call. Hooks receive
	// the current input/output token counts, the model context limit, and
	// a compaction_reason of "threshold", "overflow", or "manual".
	BeforeCompaction HookDefinitions `json:"before_compaction,omitempty" yaml:"before_compaction,omitempty"`

	// AfterCompaction hooks run after a successful compaction (a summary
	// was applied to the session). The Input.summary field carries the
	// produced summary text. AfterCompaction is purely observational.
	AfterCompaction HookDefinitions `json:"after_compaction,omitempty" yaml:"after_compaction,omitempty"`

	// ToolResponseTransform hooks run between a tool's exec and the
	// runtime's emission/record of the response. Hooks may rewrite the
	// tool's textual output by returning a non-empty
	// HookSpecificOutput.updated_tool_response — the runtime applies
	// the rewrite before the response fans out to event consumers, the
	// recorded chat message, and the post_tool_use hook input. This is
	// the third leg of the redact_secrets feature: pre_tool_use scrubs
	// arguments, before_llm_call scrubs outgoing chat content, and
	// tool_response_transform scrubs tool output. Tool-matched, like
	// pre_tool_use / post_tool_use.
	ToolResponseTransform HookMatcherConfigs `json:"tool_response_transform,omitempty" yaml:"tool_response_transform,omitempty"`

	// WorktreeCreate hooks run once, just after `docker agent run
	// --worktree` creates a git worktree and before the session starts.
	// They execute inside the new worktree (their working directory is
	// the fresh checkout) with the worktree path, branch, and source
	// repository root passed in worktree_path / worktree_branch /
	// worktree_source_dir. Use them to prepare the checkout — copy
	// untracked files like .env from the source dir, install
	// dependencies, warm caches. A hook may abort the run by blocking
	// (decision="block" / continue=false / exit code 2); stdout is added
	// as context.
	WorktreeCreate HookDefinitions `json:"worktree_create,omitempty" yaml:"worktree_create,omitempty"`
}

// IsEmpty returns true if no hooks are configured
func (h *HooksConfig) IsEmpty() bool {
	if h == nil {
		return true
	}
	return len(h.PreToolUse) == 0 &&
		len(h.PostToolUse) == 0 &&
		len(h.PermissionRequest) == 0 &&
		len(h.SessionStart) == 0 &&
		len(h.UserPromptSubmit) == 0 &&
		len(h.UserSteeringMessagesSubmit) == 0 &&
		len(h.UserFollowupSubmit) == 0 &&
		len(h.TurnStart) == 0 &&
		len(h.TurnEnd) == 0 &&
		len(h.BeforeLLMCall) == 0 &&
		len(h.AfterLLMCall) == 0 &&
		len(h.SessionEnd) == 0 &&
		len(h.PreCompact) == 0 &&
		len(h.SubagentStop) == 0 &&
		len(h.OnUserInput) == 0 &&
		len(h.Stop) == 0 &&
		len(h.Notification) == 0 &&
		len(h.OnError) == 0 &&
		len(h.OnMaxIterations) == 0 &&
		len(h.OnAgentSwitch) == 0 &&
		len(h.OnSessionResume) == 0 &&
		len(h.OnToolApprovalDecision) == 0 &&
		len(h.BeforeCompaction) == 0 &&
		len(h.AfterCompaction) == 0 &&
		len(h.ToolResponseTransform) == 0 &&
		len(h.WorktreeCreate) == 0
}

// HookMatcherConfig represents a hook matcher with its hooks.
// Used for tool-related hooks (PreToolUse, PostToolUse).
type HookMatcherConfig struct {
	// Matcher is a regex pattern to match tool names (e.g., "shell|edit_file")
	// Use "*" to match all tools. Case-sensitive.
	Matcher string `json:"matcher,omitempty" yaml:"matcher,omitempty"`

	// Hooks are the hooks to execute when the matcher matches
	Hooks HookDefinitions `json:"hooks" yaml:"hooks"`

	// PreemptYolo opts a pre_tool_use entry into firing BEFORE the
	// deterministic approval pipeline (--yolo, permission patterns).
	// A deny/ask verdict from a preempting hook cannot be bypassed by
	// auto-approval rules; an allow verdict is advisory (the pipeline
	// still runs Decide() and the rest of pre_tool_use). Default
	// pre_tool_use entries fire AFTER Decide(), as before. Only valid
	// on pre_tool_use; ignored on other events.
	//
	// Used by the safer_shell builtin (auto-registered with this flag
	// when a shell toolset has `safer: true`). Custom hooks set it to
	// true when they implement a security-critical check that must
	// not be bypassed by --yolo.
	PreemptYolo *bool `json:"preempt_yolo,omitempty" yaml:"preempt_yolo,omitempty"`
}

// HookDefinitions is a list of hook definitions that also accepts a single
// mapping in YAML and JSON (`stop: {type: command, ...}`) as sugar for a
// one-element list. Hand-written user configs frequently use the mapping
// form; rejecting it made the whole user config unparseable and silently
// dropped aliases (docker/docker-agent#3536). Always marshals back as a
// list, the canonical documented form.
type HookDefinitions []HookDefinition

func (h *HookDefinitions) UnmarshalYAML(unmarshal func(any) error) error {
	return h.unmarshal(unmarshal)
}

func (h *HookDefinitions) UnmarshalJSON(data []byte) error {
	return h.unmarshal(func(v any) error { return json.Unmarshal(data, v) })
}

func (h *HookDefinitions) unmarshal(unmarshal func(any) error) error {
	var many []HookDefinition
	if err := unmarshal(&many); err == nil {
		*h = many
		return nil
	}
	var one HookDefinition
	if err := unmarshal(&one); err != nil {
		return errors.New("hook event must be a hook or a list of hooks")
	}
	*h = HookDefinitions{one}
	return nil
}

// HookMatcherConfigs is a list of hook matchers that also accepts a single
// mapping in YAML and JSON as sugar for a one-element list, mirroring
// HookDefinitions. Always marshals back as a list.
type HookMatcherConfigs []HookMatcherConfig

func (h *HookMatcherConfigs) UnmarshalYAML(unmarshal func(any) error) error {
	return h.unmarshal(unmarshal)
}

func (h *HookMatcherConfigs) UnmarshalJSON(data []byte) error {
	return h.unmarshal(func(v any) error { return json.Unmarshal(data, v) })
}

func (h *HookMatcherConfigs) unmarshal(unmarshal func(any) error) error {
	var many []HookMatcherConfig
	if err := unmarshal(&many); err == nil {
		*h = many
		return nil
	}
	var one HookMatcherConfig
	if err := unmarshal(&one); err != nil {
		return errors.New("hook event must be a matcher or a list of matchers")
	}
	*h = HookMatcherConfigs{one}
	return nil
}

// HookDefinition represents a single hook configuration
type HookDefinition struct {
	// Name gives the hook a friendly label for logs and runtime events.
	Name string `json:"name,omitempty" yaml:"name,omitempty"`

	// Type specifies the hook type. Supported values:
	//   - "command":  run a shell command (default)
	//   - "builtin":  invoke a named, in-process Go function (the name
	//                 lives in Command). The set of registered builtins
	//                 is owned by the runtime; the docker-agent runtime
	//                 ships add_date, add_environment_info,
	//                 add_prompt_files, redact_secrets (see also the
	//                 redact_secrets agent flag), and several others
	//                 documented in pkg/hooks/builtins.
	//   - "model":    ask an LLM and translate its reply into the hook's
	//                 native output. See Model / Prompt / Schema. Used to
	//                 implement "LLM as a judge" pre_tool_use hooks,
	//                 turn-start summarizers, etc., with no Go code.
	Type string `json:"type" yaml:"type"`

	// Command is the shell command (Type==command) or the builtin name
	// (Type==builtin) to invoke.
	Command string `json:"command,omitempty" yaml:"command,omitempty"`

	// Args are arbitrary string arguments passed to the hook handler.
	// Builtin handlers receive them as the args parameter; future handler
	// kinds (http, mcp, ...) can adopt the same field. Empty for command
	// hooks today (the shell command stays self-contained).
	Args []string `json:"args,omitempty" yaml:"args,omitempty"`

	// Timeout is the execution timeout in seconds (default: 60)
	Timeout int `json:"timeout,omitempty" yaml:"timeout,omitempty"`

	// Env adds or overrides environment variables for this hook only.
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`

	// WorkingDir overrides the runtime working directory for this hook.
	WorkingDir string `json:"working_dir,omitempty" yaml:"working_dir,omitempty"`

	// OnError controls non-fail-closed hook failures: warn (default), ignore, or block.
	OnError string `json:"on_error,omitempty" yaml:"on_error,omitempty"`

	// Model is the model spec ("provider/model", e.g. "openai/gpt-4o-mini")
	// invoked by Type==model hooks. Required for that type, ignored
	// otherwise.
	Model string `json:"model,omitempty" yaml:"model,omitempty"`

	// Prompt is the user-message template rendered for each invocation
	// of a Type==model hook. It is parsed as a Go text/template with the
	// hook [Input] as the data context (so {{ .ToolName }},
	// {{ .ToolInput }}, etc. work). Required for Type==model.
	Prompt string `json:"prompt,omitempty" yaml:"prompt,omitempty"`

	// Schema selects a well-known response interpretation for Type==model
	// hooks. The empty value means "return the model's reply as
	// additional_context". Other values (registered by the runtime) ask
	// the provider for strict-JSON output and translate the result into
	// the right Output shape (e.g. "pre_tool_use_decision" produces a
	// permission_decision verdict).
	Schema string `json:"schema,omitempty" yaml:"schema,omitempty"`
}

// GetTimeout returns the per-hook execution timeout, defaulting to 60
// seconds when [HookDefinition.Timeout] is zero or negative.
func (h *HookDefinition) GetTimeout() time.Duration {
	if h.Timeout <= 0 {
		return 60 * time.Second
	}
	return time.Duration(h.Timeout) * time.Second
}

// DisplayName returns a human-friendly identifier for the hook: the
// configured Name when set, otherwise the Command, otherwise the Type.
func (h *HookDefinition) DisplayName() string {
	if h.Name != "" {
		return h.Name
	}
	if h.Command != "" {
		return h.Command
	}
	return h.Type
}

// Validate validates the HooksConfig
func (h *HooksConfig) Validate() error {
	// Validate PreToolUse matchers
	for i, m := range h.PreToolUse {
		if err := m.validate("pre_tool_use", i); err != nil {
			return err
		}
	}

	// Validate PostToolUse matchers
	for i, m := range h.PostToolUse {
		if err := m.validate("post_tool_use", i); err != nil {
			return err
		}
	}

	// Validate PermissionRequest matchers
	for i, m := range h.PermissionRequest {
		if err := m.validate("permission_request", i); err != nil {
			return err
		}
	}

	// Validate SessionStart hooks
	for i, hook := range h.SessionStart {
		if err := hook.validate("session_start", i); err != nil {
			return err
		}
	}

	// Validate UserPromptSubmit hooks
	for i, hook := range h.UserPromptSubmit {
		if err := hook.validate("user_prompt_submit", i); err != nil {
			return err
		}
	}

	// Validate UserSteeringMessagesSubmit hooks
	for i, hook := range h.UserSteeringMessagesSubmit {
		if err := hook.validate("user_steering_messages_submit", i); err != nil {
			return err
		}
	}

	// Validate UserFollowupSubmit hooks
	for i, hook := range h.UserFollowupSubmit {
		if err := hook.validate("user_followup_submit", i); err != nil {
			return err
		}
	}

	// Validate TurnStart hooks
	for i, hook := range h.TurnStart {
		if err := hook.validate("turn_start", i); err != nil {
			return err
		}
	}

	// Validate TurnEnd hooks
	for i, hook := range h.TurnEnd {
		if err := hook.validate("turn_end", i); err != nil {
			return err
		}
	}

	// Validate BeforeLLMCall hooks
	for i, hook := range h.BeforeLLMCall {
		if err := hook.validate("before_llm_call", i); err != nil {
			return err
		}
	}

	// Validate AfterLLMCall hooks
	for i, hook := range h.AfterLLMCall {
		if err := hook.validate("after_llm_call", i); err != nil {
			return err
		}
	}

	// Validate SessionEnd hooks
	for i, hook := range h.SessionEnd {
		if err := hook.validate("session_end", i); err != nil {
			return err
		}
	}

	// Validate PreCompact hooks
	for i, hook := range h.PreCompact {
		if err := hook.validate("pre_compact", i); err != nil {
			return err
		}
	}

	// Validate SubagentStop hooks
	for i, hook := range h.SubagentStop {
		if err := hook.validate("subagent_stop", i); err != nil {
			return err
		}
	}

	// Validate OnUserInput hooks
	for i, hook := range h.OnUserInput {
		if err := hook.validate("on_user_input", i); err != nil {
			return err
		}
	}

	// Validate Stop hooks
	for i, hook := range h.Stop {
		if err := hook.validate("stop", i); err != nil {
			return err
		}
	}

	// Validate Notification hooks
	for i, hook := range h.Notification {
		if err := hook.validate("notification", i); err != nil {
			return err
		}
	}

	// Validate OnError hooks
	for i, hook := range h.OnError {
		if err := hook.validate("on_error", i); err != nil {
			return err
		}
	}

	// Validate OnMaxIterations hooks
	for i, hook := range h.OnMaxIterations {
		if err := hook.validate("on_max_iterations", i); err != nil {
			return err
		}
	}

	// Validate OnAgentSwitch hooks
	for i, hook := range h.OnAgentSwitch {
		if err := hook.validate("on_agent_switch", i); err != nil {
			return err
		}
	}

	// Validate OnSessionResume hooks
	for i, hook := range h.OnSessionResume {
		if err := hook.validate("on_session_resume", i); err != nil {
			return err
		}
	}

	// Validate OnToolApprovalDecision hooks
	for i, hook := range h.OnToolApprovalDecision {
		if err := hook.validate("on_tool_approval_decision", i); err != nil {
			return err
		}
	}

	// Validate BeforeCompaction hooks
	for i, hook := range h.BeforeCompaction {
		if err := hook.validate("before_compaction", i); err != nil {
			return err
		}
	}

	// Validate AfterCompaction hooks
	for i, hook := range h.AfterCompaction {
		if err := hook.validate("after_compaction", i); err != nil {
			return err
		}
	}

	// Validate ToolResponseTransform matchers
	for i, m := range h.ToolResponseTransform {
		if err := m.validate("tool_response_transform", i); err != nil {
			return err
		}
	}

	// Validate WorktreeCreate hooks
	for i, hook := range h.WorktreeCreate {
		if err := hook.validate("worktree_create", i); err != nil {
			return err
		}
	}

	return nil
}

// validate validates a HookMatcherConfig
func (m *HookMatcherConfig) validate(eventType string, index int) error {
	if len(m.Hooks) == 0 {
		return fmt.Errorf("hooks.%s[%d]: at least one hook is required", eventType, index)
	}

	for i, hook := range m.Hooks {
		if err := hook.validate(fmt.Sprintf("%s[%d].hooks", eventType, index), i); err != nil {
			return err
		}
	}

	return nil
}

// validate validates a HookDefinition
func (h *HookDefinition) validate(prefix string, index int) error {
	if h.Type == "" {
		return fmt.Errorf("hooks.%s[%d]: type is required", prefix, index)
	}

	switch h.Type {
	case "command":
		if h.Command == "" {
			return fmt.Errorf("hooks.%s[%d]: command is required for command hooks", prefix, index)
		}
	case "builtin":
		if h.Command == "" {
			return fmt.Errorf("hooks.%s[%d]: command must name the builtin to invoke", prefix, index)
		}
	case "model":
		if h.Model == "" {
			return fmt.Errorf("hooks.%s[%d]: model is required for model hooks (e.g. 'openai/gpt-4o-mini')", prefix, index)
		}
		if h.Prompt == "" {
			return fmt.Errorf("hooks.%s[%d]: prompt is required for model hooks", prefix, index)
		}
	default:
		return fmt.Errorf("hooks.%s[%d]: unsupported hook type '%s' (supported: 'command', 'builtin', 'model')", prefix, index, h.Type)
	}

	return nil
}
