package v4

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/docker/cagent/pkg/config/types"
)

const Version = "4"

// Config represents the entire configuration file
type Config struct {
	Version     string                    `json:"version,omitempty"`
	Agents      Agents                    `json:"agents,omitempty"`
	Providers   map[string]ProviderConfig `json:"providers,omitempty"`
	Models      map[string]ModelConfig    `json:"models,omitempty"`
	RAG         map[string]RAGConfig      `json:"rag,omitempty"`
	Metadata    Metadata                  `json:"metadata"`
	Permissions *PermissionsConfig        `json:"permissions,omitempty"`
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
			return fmt.Errorf("agent name must be a string")
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
// It allows users to define custom providers with default base URLs and token keys.
// Models can reference these providers by name, inheriting the defaults.
type ProviderConfig struct {
	// APIType specifies which API schema to use. Supported values:
	// - "openai_chatcompletions" (default): Use the OpenAI Chat Completions API
	// - "openai_responses": Use the OpenAI Responses API
	APIType string `json:"api_type,omitempty"`
	// BaseURL is the base URL for the provider's API endpoint
	BaseURL string `json:"base_url"`
	// TokenKey is the environment variable name containing the API token
	TokenKey string `json:"token_key,omitempty"`
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
	// NoFallbackStatusCodes is a list of HTTP status codes for which the fallback
	// chain should be skipped entirely when using a models gateway.
	NoFallbackStatusCodes []int `json:"no_fallback_status_codes,omitempty"`
}

// Duration is a wrapper around time.Duration that supports YAML/JSON unmarshaling
// from string format (e.g., "1m", "30s", "2h30m").
type Duration struct {
	time.Duration
}

// UnmarshalYAML implements custom unmarshaling for Duration from string format
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	if d == nil {
		return fmt.Errorf("cannot unmarshal into nil Duration")
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
		return fmt.Errorf("cannot unmarshal into nil Duration")
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

// AgentConfig represents a single agent configuration
type AgentConfig struct {
	Name                    string
	Model                   string            `json:"model,omitempty"`
	Fallback                *FallbackConfig   `json:"fallback,omitempty"`
	Description             string            `json:"description,omitempty"`
	WelcomeMessage          string            `json:"welcome_message,omitempty"`
	Toolsets                []Toolset         `json:"toolsets,omitempty"`
	Instruction             string            `json:"instruction,omitempty"`
	SubAgents               []string          `json:"sub_agents,omitempty"`
	Handoffs                []string          `json:"handoffs,omitempty"`
	RAG                     []string          `json:"rag,omitempty"`
	AddDate                 bool              `json:"add_date,omitempty"`
	AddEnvironmentInfo      bool              `json:"add_environment_info,omitempty"`
	CodeModeTools           bool              `json:"code_mode_tools,omitempty"`
	AddDescriptionParameter bool              `json:"add_description_parameter,omitempty"`
	MaxIterations           int               `json:"max_iterations,omitempty"`
	NumHistoryItems         int               `json:"num_history_items,omitempty"`
	AddPromptFiles          []string          `json:"add_prompt_files,omitempty" yaml:"add_prompt_files,omitempty"`
	Commands                types.Commands    `json:"commands,omitempty"`
	StructuredOutput        *StructuredOutput `json:"structured_output,omitempty"`
	Skills                  *bool             `json:"skills,omitempty"`
	Hooks                   *HooksConfig      `json:"hooks,omitempty"`
}

// GetFallbackModels returns the fallback models from the config.
func (a *AgentConfig) GetFallbackModels() []string {
	if a.Fallback != nil {
		return a.Fallback.Models
	}
	return nil
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
	Provider          string   `json:"provider,omitempty"`
	Model             string   `json:"model,omitempty"`
	Temperature       *float64 `json:"temperature,omitempty"`
	MaxTokens         *int64   `json:"max_tokens,omitempty"`
	TopP              *float64 `json:"top_p,omitempty"`
	FrequencyPenalty  *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty   *float64 `json:"presence_penalty,omitempty"`
	BaseURL           string   `json:"base_url,omitempty"`
	ParallelToolCalls *bool    `json:"parallel_tool_calls,omitempty"`
	TokenKey          string   `json:"token_key,omitempty"`
	// ProviderOpts allows provider-specific options.
	ProviderOpts map[string]any `json:"provider_opts,omitempty"`
	TrackUsage   *bool          `json:"track_usage,omitempty"`
	// ThinkingBudget controls reasoning effort/budget:
	// - For OpenAI: accepts string levels "minimal", "low", "medium", "high"
	// - For Anthropic: accepts integer token budget (1024-32000)
	// - For other providers: may be ignored
	ThinkingBudget *ThinkingBudget `json:"thinking_budget,omitempty"`
	// Routing defines rules for routing requests to different models.
	// When routing is configured, this model becomes a rule-based router:
	// - The provider/model fields define the fallback model
	// - Each routing rule maps to a different model based on examples
	Routing []RoutingRule `json:"routing,omitempty"`
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
		provider, model, ok := strings.Cut(shorthand, "/")
		if !ok || provider == "" || model == "" {
			return fmt.Errorf("invalid model shorthand %q: expected format 'provider/model'", shorthand)
		}
		f.Provider = provider
		f.Model = model
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
		len(f.ProviderOpts) == 0 &&
		f.TrackUsage == nil &&
		f.ThinkingBudget == nil &&
		len(f.Routing) == 0
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
	Author      string `json:"author,omitempty"`
	License     string `json:"license,omitempty"`
	Description string `json:"description,omitempty"`
	Readme      string `json:"readme,omitempty"`
	Version     string `json:"version,omitempty"`
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

	Defer DeferConfig `json:"defer" yaml:"defer,omitempty"`

	// For the `mcp` tool
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Ref     string   `json:"ref,omitempty"`
	Remote  Remote   `json:"remote"`
	Config  any      `json:"config,omitempty"`

	// For the `a2a` and `openapi` tools
	Name    string            `json:"name,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// For `shell`, `script`, `mcp` or `lsp` tools
	Env map[string]string `json:"env,omitempty"`

	// For the `shell` tool - sandbox mode
	Sandbox *SandboxConfig `json:"sandbox,omitempty"`

	// For the `todo` tool
	Shared bool `json:"shared,omitempty"`

	// For the `memory` tool
	Path string `json:"path,omitempty"`

	// For the `script` tool
	Shell map[string]ScriptShellToolConfig `json:"shell,omitempty"`

	// For the `filesystem` tool - post-edit commands
	PostEdit []PostEditConfig `json:"post_edit,omitempty"`

	APIConfig APIToolConfig `json:"api_config"`

	// For the `filesystem` tool - VCS integration
	IgnoreVCS *bool `json:"ignore_vcs,omitempty"`

	// For the `lsp` tool
	FileTypes []string `json:"file_types,omitempty"`

	// For the `fetch` tool
	Timeout int `json:"timeout,omitempty"`
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

type Remote struct {
	URL           string            `json:"url"`
	TransportType string            `json:"transport_type,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
}

// SandboxConfig represents the configuration for running shell commands in a Docker container.
// When enabled, all shell commands run inside a sandboxed Linux container with only
// specified paths bind-mounted.
type SandboxConfig struct {
	// Image is the Docker image to use for the sandbox container.
	// Defaults to "alpine:latest" if not specified.
	Image string `json:"image,omitempty"`

	// Paths is a list of paths to bind-mount into the container.
	// Each path can optionally have a ":ro" suffix for read-only access.
	// Default is read-write (:rw) if no suffix is specified.
	// Example: [".", "/tmp", "/config:ro"]
	Paths []string `json:"paths"`
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
// It accepts either a string effort level or an integer token budget:
// - String: "minimal", "low", "medium", "high" (for OpenAI)
// - Integer: token count (for Anthropic, range 1024-32768)
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
		*t = ThinkingBudget{Effort: s}
		return nil
	}

	return nil
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
		return int(val)
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

	return fmt.Errorf("database must be a string path to a sqlite database")
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

	return "", fmt.Errorf("invalid database configuration: expected string path")
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
	PreToolUse []HookMatcherConfig `json:"pre_tool_use,omitempty" yaml:"pre_tool_use,omitempty"`

	// PostToolUse hooks run after tool execution
	PostToolUse []HookMatcherConfig `json:"post_tool_use,omitempty" yaml:"post_tool_use,omitempty"`

	// SessionStart hooks run when a session begins
	SessionStart []HookDefinition `json:"session_start,omitempty" yaml:"session_start,omitempty"`

	// SessionEnd hooks run when a session ends
	SessionEnd []HookDefinition `json:"session_end,omitempty" yaml:"session_end,omitempty"`
}

// IsEmpty returns true if no hooks are configured
func (h *HooksConfig) IsEmpty() bool {
	if h == nil {
		return true
	}
	return len(h.PreToolUse) == 0 &&
		len(h.PostToolUse) == 0 &&
		len(h.SessionStart) == 0 &&
		len(h.SessionEnd) == 0
}

// HookMatcherConfig represents a hook matcher with its hooks.
// Used for tool-related hooks (PreToolUse, PostToolUse).
type HookMatcherConfig struct {
	// Matcher is a regex pattern to match tool names (e.g., "shell|edit_file")
	// Use "*" to match all tools. Case-sensitive.
	Matcher string `json:"matcher,omitempty" yaml:"matcher,omitempty"`

	// Hooks are the hooks to execute when the matcher matches
	Hooks []HookDefinition `json:"hooks" yaml:"hooks"`
}

// HookDefinition represents a single hook configuration
type HookDefinition struct {
	// Type specifies the hook type (currently only "command" is supported)
	Type string `json:"type" yaml:"type"`

	// Command is the shell command to execute
	Command string `json:"command,omitempty" yaml:"command,omitempty"`

	// Timeout is the execution timeout in seconds (default: 60)
	Timeout int `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

// validate validates the HooksConfig
func (h *HooksConfig) validate() error {
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

	// Validate SessionStart hooks
	for i, hook := range h.SessionStart {
		if err := hook.validate("session_start", i); err != nil {
			return err
		}
	}

	// Validate SessionEnd hooks
	for i, hook := range h.SessionEnd {
		if err := hook.validate("session_end", i); err != nil {
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

	if h.Type != "command" {
		return fmt.Errorf("hooks.%s[%d]: unsupported hook type '%s' (only 'command' is supported)", prefix, index, h.Type)
	}

	if h.Command == "" {
		return fmt.Errorf("hooks.%s[%d]: command is required for command hooks", prefix, index)
	}

	return nil
}
