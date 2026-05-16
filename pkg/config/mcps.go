package config

import (
	"fmt"
	"maps"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// resolveMCPDefinitions resolves MCP definition references in agent toolsets.
// When an agent toolset of type "mcp" has a ref that matches a key in the
// top-level mcps section (rather than a "docker:" ref), the toolset is expanded
// with the definition's properties. Any properties set directly on the toolset
// override the corresponding definition properties.
func resolveMCPDefinitions(cfg *latest.Config) error {
	for name, def := range cfg.MCPs {
		if err := validateMCPDefinition(name, &def.Toolset); err != nil {
			return err
		}
	}

	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		for j := range agent.Toolsets {
			ts := &agent.Toolsets[j]
			if ts.Type != "mcp" || ts.Ref == "" || strings.HasPrefix(ts.Ref, "docker:") {
				continue
			}

			def, ok := cfg.MCPs[ts.Ref]
			if !ok {
				return fmt.Errorf("agent '%s' references non-existent MCP definition '%s'", agent.Name, ts.Ref)
			}

			applyMCPDefaults(ts, &def.Toolset)
		}
	}

	return nil
}

// validateMCPDefinition validates that a definition's ref uses the docker: prefix.
// The basic source validation (exactly one of command/remote/ref) is already handled
// by Toolset.validate() during YAML unmarshaling.
func validateMCPDefinition(name string, def *latest.Toolset) error {
	if def.Ref != "" && !strings.HasPrefix(def.Ref, "docker:") {
		return fmt.Errorf("MCP definition '%s': only docker refs are supported (e.g., 'docker:context7')", name)
	}
	return nil
}

// applyMCPDefaults fills empty fields in ts from def. Toolset values win.
// Env maps are merged (toolset values take precedence on key conflicts).
func applyMCPDefaults(ts, def *latest.Toolset) {
	// Replace the definition-name ref with the actual source.
	if def.Ref != "" {
		ts.Ref = def.Ref
	} else {
		ts.Ref = ""
	}
	if ts.Command == "" {
		ts.Command = def.Command
	}
	if ts.Remote.URL == "" {
		ts.Remote = def.Remote
	}
	if len(ts.Args) == 0 {
		ts.Args = def.Args
	}
	if ts.Version == "" {
		ts.Version = def.Version
	}
	if ts.Config == nil {
		ts.Config = def.Config
	}
	if ts.Name == "" {
		ts.Name = def.Name
	}
	if ts.Instruction == "" {
		ts.Instruction = def.Instruction
	}
	if len(ts.Tools) == 0 {
		ts.Tools = def.Tools
	}
	if ts.Defer.IsEmpty() {
		ts.Defer = def.Defer
	}
	if ts.WorkingDir == "" {
		// An empty working_dir in the referencing toolset is treated as "unset":
		// inherit the definition's value. This matches the semantics of all other
		// string fields in this function. An explicit `working_dir: ""` in YAML
		// is indistinguishable from omission and will therefore be overridden.
		ts.WorkingDir = def.WorkingDir
	}
	if len(def.Env) > 0 {
		merged := make(map[string]string, len(def.Env)+len(ts.Env))
		maps.Copy(merged, def.Env)
		if ts.Env != nil {
			maps.Copy(merged, ts.Env)
		}
		ts.Env = merged
	}
}
