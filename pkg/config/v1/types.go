package v1

import (
	"errors"
	"strings"

	"github.com/docker/docker-agent/pkg/config/types"
)

const Version = "1"

// ScriptShellToolConfig represents a custom shell tool configuration
type ScriptShellToolConfig struct {
	Cmd         string `json:"cmd" yaml:"cmd"`
	Description string `json:"description" yaml:"description"`

	// Args is directly passed as "properties" in the JSON schema
	Args map[string]any `json:"args,omitempty" yaml:"args,omitempty"`

	// Required is directly passed as "required" in the JSON schema
	Required []string `json:"required" yaml:"required"`

	Env        map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty" yaml:"working_dir,omitempty"`
}

// PostEditConfig represents a post-edit command configuration
type PostEditConfig struct {
	Path string `json:"path" yaml:"path"`
	Cmd  string `json:"cmd" yaml:"cmd"`
}

// Toolset represents a tool configuration
type Toolset struct {
	Type     string             `json:"type,omitempty" yaml:"type,omitempty"`
	Ref      string             `json:"ref,omitempty" yaml:"ref,omitempty"`
	Config   any                `json:"config,omitempty" yaml:"config,omitempty"`
	Command  string             `json:"command,omitempty" yaml:"command,omitempty"`
	Remote   Remote             `json:"remote" yaml:"remote,omitempty"`
	Args     []string           `json:"args,omitempty" yaml:"args,omitempty"`
	Env      map[string]string  `json:"env,omitempty" yaml:"env,omitempty"`
	Envfiles types.StringOrList `json:"env_file,omitempty" yaml:"env_file,omitempty"`
	Tools    []string           `json:"tools,omitempty" yaml:"tools,omitempty"`

	// For the think tool
	Shared bool `json:"shared,omitempty" yaml:"shared,omitempty"`
	// For the memory tool
	Path string `json:"path,omitempty" yaml:"path,omitempty"`

	// For the script tool
	Shell map[string]ScriptShellToolConfig `json:"shell,omitempty" yaml:"shell,omitempty"`

	// For the filesystem tool - post-edit commands
	PostEdit []PostEditConfig `json:"post_edit,omitempty" yaml:"post_edit,omitempty"`
}

type Remote struct {
	URL           string            `json:"url" yaml:"url"`
	TransportType string            `json:"transport_type,omitempty" yaml:"transport_type,omitempty"`
	Headers       map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
}

// Ensure that either Command, Remote or Ref is set, but not all empty
func (t *Toolset) validate() error {
	if len(t.Shell) > 0 && t.Type != "script" {
		return errors.New("shell can only be used with type 'script'")
	}
	if t.Type != "mcp" {
		return nil
	}

	count := 0
	if t.Command != "" {
		count++
	}
	if t.Remote.URL != "" {
		count++
	}
	if t.Ref != "" {
		count++
	}
	if count == 0 {
		return errors.New("either command, remote or ref must be set")
	}
	if count > 1 {
		return errors.New("either command, remote or ref must be set, but only one of those")
	}

	if t.Ref != "" && !strings.Contains(t.Ref, "docker:") {
		return errors.New("only docker refs are supported for MCP tools, e.g., 'docker:context7'")
	}

	return nil
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

// AgentConfig represents a single agent configuration
type AgentConfig struct {
	Model              string         `json:"model,omitempty" yaml:"model,omitempty"`
	Description        string         `json:"description,omitempty" yaml:"description,omitempty"`
	Toolsets           []Toolset      `json:"toolsets,omitempty" yaml:"toolsets,omitempty"`
	Instruction        string         `json:"instruction,omitempty" yaml:"instruction,omitempty"`
	SubAgents          []string       `json:"sub_agents,omitempty" yaml:"sub_agents,omitempty"`
	AddDate            bool           `json:"add_date,omitempty" yaml:"add_date,omitempty"`
	AddEnvironmentInfo bool           `json:"add_environment_info,omitempty" yaml:"add_environment_info,omitempty"`
	NumHistoryItems    int            `json:"num_history_items,omitempty" yaml:"num_history_items,omitempty"`
	Commands           types.Commands `json:"commands,omitempty" yaml:"commands,omitempty"`
}

// ModelConfig represents the configuration for a model
type ModelConfig struct {
	Provider          string            `json:"provider,omitempty" yaml:"provider,omitempty"`
	Model             string            `json:"model,omitempty" yaml:"model,omitempty"`
	Temperature       float64           `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	MaxTokens         int               `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	TopP              float64           `json:"top_p,omitempty" yaml:"top_p,omitempty"`
	FrequencyPenalty  float64           `json:"frequency_penalty,omitempty" yaml:"frequency_penalty,omitempty"`
	PresencePenalty   float64           `json:"presence_penalty,omitempty" yaml:"presence_penalty,omitempty"`
	BaseURL           string            `json:"base_url,omitempty" yaml:"base_url,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty" yaml:"parallel_tool_calls,omitempty"`
	Env               map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	TokenKey          string            `json:"token_key,omitempty" yaml:"token_key,omitempty"`
	// ProviderOpts allows provider-specific options. Currently used for "dmr" provider only.
	ProviderOpts map[string]any `json:"provider_opts,omitempty" yaml:"provider_opts,omitempty"`
}

// Config represents the entire configuration file
type Config struct {
	Version  string                 `json:"version,omitempty" yaml:"version,omitempty"`
	Agents   map[string]AgentConfig `json:"agents,omitempty" yaml:"agents,omitempty"`
	Models   map[string]ModelConfig `json:"models,omitempty" yaml:"models,omitempty"`
	Env      map[string]string      `json:"env,omitempty" yaml:"env,omitempty"`
	Metadata Metadata               `json:"metadata" yaml:"metadata,omitempty"`
}

type Metadata struct {
	Author  string `json:"author,omitempty" yaml:"author,omitempty"`
	License string `json:"license,omitempty" yaml:"license,omitempty"`
	Readme  string `json:"readme,omitempty" yaml:"readme,omitempty"`
}
