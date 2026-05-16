package v2

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"github.com/docker/docker-agent/pkg/config/types"
)

const Version = "2"

// Config represents the entire configuration file
type Config struct {
	Version  string                 `json:"version,omitempty"`
	Agents   map[string]AgentConfig `json:"agents,omitempty"`
	Models   map[string]ModelConfig `json:"models,omitempty"`
	RAG      map[string]RAGConfig   `json:"rag,omitempty"`
	Metadata Metadata               `json:"metadata"`
}

// AgentConfig represents a single agent configuration
type AgentConfig struct {
	Model              string            `json:"model,omitempty"`
	Description        string            `json:"description,omitempty"`
	WelcomeMessage     string            `json:"welcome_message,omitempty"`
	Toolsets           []Toolset         `json:"toolsets,omitempty"`
	Instruction        string            `json:"instruction,omitempty"`
	SubAgents          []string          `json:"sub_agents,omitempty"`
	Handoffs           []string          `json:"handoffs,omitempty"`
	RAG                []string          `json:"rag,omitempty"`
	AddDate            bool              `json:"add_date,omitempty"`
	AddEnvironmentInfo bool              `json:"add_environment_info,omitempty"`
	CodeModeTools      bool              `json:"code_mode_tools,omitempty"`
	MaxIterations      int               `json:"max_iterations,omitempty"`
	NumHistoryItems    int               `json:"num_history_items,omitempty"`
	AddPromptFiles     []string          `json:"add_prompt_files,omitempty" yaml:"add_prompt_files,omitempty"`
	Commands           types.Commands    `json:"commands,omitempty"`
	StructuredOutput   *StructuredOutput `json:"structured_output,omitempty"`
}

// ModelConfig represents the configuration for a model
type ModelConfig struct {
	Provider          string   `json:"provider,omitempty"`
	Model             string   `json:"model,omitempty"`
	Temperature       *float64 `json:"temperature,omitempty"`
	MaxTokens         int      `json:"max_tokens,omitempty"`
	TopP              *float64 `json:"top_p,omitempty"`
	FrequencyPenalty  *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty   *float64 `json:"presence_penalty,omitempty"`
	BaseURL           string   `json:"base_url,omitempty"`
	ParallelToolCalls *bool    `json:"parallel_tool_calls,omitempty"`
	TokenKey          string   `json:"token_key,omitempty"`
	// ProviderOpts allows provider-specific options. Currently used for "dmr" provider only.
	ProviderOpts map[string]any `json:"provider_opts,omitempty"`
	TrackUsage   *bool          `json:"track_usage,omitempty"`
	// ThinkingBudget controls reasoning effort/budget:
	// - For OpenAI: accepts string levels "minimal", "low", "medium", "high"
	// - For Anthropic: accepts integer token budget (1024-32000)
	// - For other providers: may be ignored
	ThinkingBudget *ThinkingBudget `json:"thinking_budget,omitempty"`
}

type Metadata struct {
	Author  string `json:"author,omitempty"`
	License string `json:"license,omitempty"`
	Readme  string `json:"readme,omitempty"`
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

	// For the `mcp` tool
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Ref     string   `json:"ref,omitempty"`
	Remote  Remote   `json:"remote"`
	Config  any      `json:"config,omitempty"`

	// For `shell`, `script` or `mcp` tools
	Env map[string]string `json:"env,omitempty"`

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

// MarshalJSON implements custom marshaling to output simple string or int format
// This ensures JSON serialization during config upgrades preserves the value correctly
func (t ThinkingBudget) MarshalJSON() ([]byte, error) {
	// If Effort string is set (non-empty), marshal as string
	if t.Effort != "" {
		return fmt.Appendf(nil, "%q", t.Effort), nil
	}

	// Otherwise marshal as integer (includes 0, -1, and positive values)
	return fmt.Appendf(nil, "%d", t.Tokens), nil
}

// UnmarshalJSON implements custom unmarshaling to accept simple string or int format
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

// RAGConfig represents a RAG (Retrieval-Augmented Generation) configuration
// Uses a unified strategies array for flexible, extensible configuration
type RAGConfig struct {
	Description string              `json:"description,omitempty"`
	Docs        []string            `json:"docs,omitempty"`       // Shared documents across all strategies
	Strategies  []RAGStrategyConfig `json:"strategies,omitempty"` // Array of strategy configurations
	Results     RAGResultsConfig    `json:"results"`
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
	// - chunked-embeddings: model, similarity_metric, threshold, vector_dimensions
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
}

// coerceToInt converts various numeric types to int
func coerceToInt(v any) int {
	switch val := v.(type) {
	case int:
		return val
	case int64:
		return int(val)
	case uint64:
		return int(val) //nolint:gosec // frozen config: value comes from validated YAML; bounds enforced by schema
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

// MarshalYAML implements custom marshaling for DatabaseConfig
func (d RAGDatabaseConfig) MarshalYAML() (any, error) {
	if d.value == nil {
		return nil, nil
	}
	return d.value, nil
}

// MarshalJSON implements custom marshaling for DatabaseConfig
func (d RAGDatabaseConfig) MarshalJSON() ([]byte, error) {
	if d.value == nil {
		return json.Marshal(nil)
	}
	return json.Marshal(d.value)
}

// UnmarshalJSON implements custom unmarshaling for DatabaseConfig
func (d *RAGDatabaseConfig) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		d.value = str
		return nil
	}
	return errors.New("database must be a string path to a sqlite database")
}

// RAGChunkingConfig represents text chunking configuration
type RAGChunkingConfig struct {
	Size                  int  `json:"size,omitempty"`
	Overlap               int  `json:"overlap,omitempty"`
	RespectWordBoundaries bool `json:"respect_word_boundaries,omitempty"`
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
	Limit             int              `json:"limit,omitempty"`               // Maximum number of results to return (top K)
	Fusion            *RAGFusionConfig `json:"fusion,omitempty"`              // How to combine results from multiple strategies
	Deduplicate       bool             `json:"deduplicate,omitempty"`         // Remove duplicate documents across strategies
	IncludeScore      bool             `json:"include_score,omitempty"`       // Include relevance scores in results
	ReturnFullContent bool             `json:"return_full_content,omitempty"` // Return full document content instead of just matched chunks
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
		Limit             int              `json:"limit,omitempty"`
		Fusion            *RAGFusionConfig `json:"fusion,omitempty"`
		Deduplicate       *bool            `json:"deduplicate,omitempty"`
		IncludeScore      *bool            `json:"include_score,omitempty"`
		ReturnFullContent *bool            `json:"return_full_content,omitempty"`
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
