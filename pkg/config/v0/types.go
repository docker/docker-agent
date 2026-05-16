package v0

import (
	"errors"

	"github.com/docker/docker-agent/pkg/config/types"
)

const Version = "0"

// Toolset represents a tool configuration.
type Toolset struct {
	Type     string             `json:"type,omitempty" yaml:"type,omitempty"`
	Command  string             `json:"command,omitempty" yaml:"command,omitempty"`
	Remote   Remote             `json:"remote" yaml:"remote,omitempty"`
	Args     []string           `json:"args,omitempty" yaml:"args,omitempty"`
	Env      map[string]string  `json:"env,omitempty" yaml:"env,omitempty"`
	Envfiles types.StringOrList `json:"env_file,omitempty" yaml:"env_file,omitempty"`
	Tools    []string           `json:"tools,omitempty" yaml:"tools,omitempty"`
}

type Remote struct {
	URL           string            `json:"url" yaml:"url"`
	TransportType string            `json:"transport_type,omitempty" yaml:"transport_type,omitempty"`
	Headers       map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
}

// Ensure that either Command or Remote is set, but not both empty.
func (t *Toolset) validate() error {
	if t.Type != "mcp" {
		return nil
	}

	if t.Command == "" && t.Remote.URL == "" {
		return errors.New("either command or remote must be set")
	}
	if t.Command != "" && t.Remote.URL != "" {
		return errors.New("either command or remote must be set, but not both")
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

// TodoConfig represents todo configuration that can be either a boolean or an object
type TodoConfig struct {
	Enabled bool `json:"-" yaml:"-"`
	Shared  bool `json:"shared,omitempty" yaml:"shared,omitempty"`
}

// UnmarshalYAML implements custom unmarshaling for TodoConfig to support both boolean and object formats
func (t *TodoConfig) UnmarshalYAML(unmarshal func(any) error) error {
	type todoConfigAlias TodoConfig

	var config todoConfigAlias
	if err := unmarshal(&config); err == nil {
		*t = TodoConfig(config)
		t.Enabled = true
		return nil
	}

	var enabled bool
	if err := unmarshal(&enabled); err != nil {
		return err
	}

	t.Enabled = enabled

	return nil
}

// AgentConfig represents a single agent configuration
type AgentConfig struct {
	Name            string         `json:"name,omitempty" yaml:"name,omitempty"`
	Model           string         `json:"model,omitempty" yaml:"model,omitempty"`
	Description     string         `json:"description,omitempty" yaml:"description,omitempty"`
	Toolsets        []Toolset      `json:"toolsets,omitempty" yaml:"toolsets,omitempty"`
	Instruction     string         `json:"instruction,omitempty" yaml:"instruction,omitempty"`
	SubAgents       []string       `json:"sub_agents,omitempty" yaml:"sub_agents,omitempty"`
	AddDate         bool           `json:"add_date,omitempty" yaml:"add_date,omitempty"`
	Think           bool           `json:"think,omitempty" yaml:"think,omitempty"`
	Todo            TodoConfig     `json:"todo" yaml:"todo,omitempty"`
	MemoryConfig    MemoryConfig   `json:"memory" yaml:"memory,omitempty"`
	NumHistoryItems int            `json:"num_history_items,omitempty" yaml:"num_history_items,omitempty"`
	Commands        types.Commands `json:"commands,omitempty" yaml:"commands,omitempty"`
}

type MemoryConfig struct {
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
}

// ModelConfig represents the configuration for a model
type ModelConfig struct {
	Type              string            `json:"type,omitempty" yaml:"type,omitempty"`
	Model             string            `json:"model,omitempty" yaml:"model,omitempty"`
	Temperature       float64           `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	MaxTokens         int               `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	TopP              float64           `json:"top_p,omitempty" yaml:"top_p,omitempty"`
	FrequencyPenalty  float64           `json:"frequency_penalty,omitempty" yaml:"frequency_penalty,omitempty"`
	PresencePenalty   float64           `json:"presence_penalty,omitempty" yaml:"presence_penalty,omitempty"`
	BaseURL           string            `json:"base_url,omitempty" yaml:"base_url,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty" yaml:"parallel_tool_calls,omitempty"`
	Env               map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
}

// Config represents the entire configuration file
type Config struct {
	Version string                 `json:"version,omitempty"`
	Agents  map[string]AgentConfig `json:"agents,omitempty" yaml:"agents,omitempty"`
	Models  map[string]ModelConfig `json:"models,omitempty" yaml:"models,omitempty"`
	Env     map[string]string      `json:"env,omitempty" yaml:"env,omitempty"`
}
