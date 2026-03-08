package v4

import (
	"errors"
	"strings"
)

func (t *Config) UnmarshalYAML(unmarshal func(any) error) error {
	type alias Config
	var tmp alias
	if err := unmarshal(&tmp); err != nil {
		return err
	}
	*t = Config(tmp)
	return t.validate()
}

func (t *Config) validate() error {
	for i := range t.Agents {
		agent := &t.Agents[i]

		// Validate fallback config
		if err := agent.validateFallback(); err != nil {
			return err
		}

		for j := range agent.Toolsets {
			if err := agent.Toolsets[j].validate(); err != nil {
				return err
			}
		}
		if agent.Hooks != nil {
			if err := agent.Hooks.validate(); err != nil {
				return err
			}
		}
	}

	return nil
}

// validateFallback validates the fallback configuration for an agent
func (a *AgentConfig) validateFallback() error {
	if a.Fallback == nil {
		return nil
	}

	// -1 is allowed as a special value meaning "explicitly no retries"
	if a.Fallback.Retries < -1 {
		return errors.New("fallback.retries must be >= -1 (use -1 for no retries, 0 for default)")
	}
	if a.Fallback.Cooldown.Duration < 0 {
		return errors.New("fallback.cooldown must be non-negative")
	}

	for _, code := range a.Fallback.NoFallbackStatusCodes {
		if code < 400 || code > 599 {
			return errors.New("fallback.no_fallback_status_codes must contain HTTP error codes (400-599)")
		}
	}

	return nil
}

func (t *Toolset) validate() error {
	// Attributes used on the wrong toolset type.
	if len(t.Shell) > 0 && t.Type != "script" {
		return errors.New("shell can only be used with type 'script'")
	}
	if t.Path != "" && t.Type != "memory" {
		return errors.New("path can only be used with type 'memory'")
	}
	if len(t.PostEdit) > 0 && t.Type != "filesystem" {
		return errors.New("post_edit can only be used with type 'filesystem'")
	}
	if t.IgnoreVCS != nil && t.Type != "filesystem" {
		return errors.New("ignore_vcs can only be used with type 'filesystem'")
	}
	if len(t.Env) > 0 && (t.Type != "shell" && t.Type != "script" && t.Type != "mcp" && t.Type != "lsp") {
		return errors.New("env can only be used with type 'shell', 'script', 'mcp' or 'lsp'")
	}
	if t.Sandbox != nil && t.Type != "shell" {
		return errors.New("sandbox can only be used with type 'shell'")
	}
	if t.Shared && t.Type != "todo" {
		return errors.New("shared can only be used with type 'todo'")
	}
	if t.Command != "" && t.Type != "mcp" && t.Type != "lsp" {
		return errors.New("command can only be used with type 'mcp' or 'lsp'")
	}
	if len(t.Args) > 0 && t.Type != "mcp" && t.Type != "lsp" {
		return errors.New("args can only be used with type 'mcp' or 'lsp'")
	}
	if t.Ref != "" && t.Type != "mcp" {
		return errors.New("ref can only be used with type 'mcp'")
	}
	if (t.Remote.URL != "" || t.Remote.TransportType != "") && t.Type != "mcp" {
		return errors.New("remote can only be used with type 'mcp'")
	}
	if (len(t.Remote.Headers) > 0) && (t.Type != "mcp" && t.Type != "a2a") {
		return errors.New("headers can only be used with type 'mcp' or 'a2a'")
	}
	if t.Config != nil && t.Type != "mcp" {
		return errors.New("config can only be used with type 'mcp'")
	}
	if t.URL != "" && t.Type != "a2a" {
		return errors.New("url can only be used with type 'a2a'")
	}
	if t.Name != "" && (t.Type != "mcp" && t.Type != "a2a") {
		return errors.New("name can only be used with type 'mcp' or 'a2a'")
	}

	switch t.Type {
	case "shell":
		if t.Sandbox != nil && len(t.Sandbox.Paths) == 0 {
			return errors.New("sandbox requires at least one path to be set")
		}
	case "memory":
		if t.Path == "" {
			return errors.New("memory toolset requires a path to be set")
		}
	case "mcp":
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
	case "a2a":
		if t.URL == "" {
			return errors.New("a2a toolset requires a url to be set")
		}
	case "lsp":
		if t.Command == "" {
			return errors.New("lsp toolset requires a command to be set")
		}
	}

	return nil
}
