// Package types defines the shared wire contracts for the shell tools.
package types

import (
	"encoding/json"

	"github.com/docker/docker-agent/pkg/safety"
)

const ToolNameShell = "shell"

type RunShellArgs struct {
	Cmd     string `json:"cmd" jsonschema:"Shell command"`
	Cwd     string `json:"cwd,omitempty" jsonschema:"Working directory (default \".\")"`
	Timeout int    `json:"timeout,omitempty" jsonschema:"Timeout in seconds (default 30)"`
}

// UnmarshalJSON accepts both the canonical "cmd" key and the common alias
// "command" for the shell command parameter.
//
// The advertised schema still declares "cmd" as the canonical name, but many
// models (particularly ones biased by Anthropic's built-in bash tool and other
// ecosystems that use "command") occasionally emit "command" instead. Accepting
// both prevents a wasted turn on an empty-command error while keeping the
// canonical contract unchanged.
//
// The command is resolved by [safety.CommandArg] over an exact-key map rather
// than by struct tags: encoding/json matches keys case-insensitively with
// last-wins, so {"cmd":"ls","CMD":"rm -rf x"} would run a command the runtime
// never classified. Sharing the resolver keeps the executed command identical
// to the labelled one.
func (a *RunShellArgs) UnmarshalJSON(data []byte) error {
	var raw struct {
		Cwd     string `json:"cwd"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	a.Cmd, _ = safety.CommandArg(fields)
	a.Cwd = raw.Cwd
	a.Timeout = raw.Timeout
	return nil
}
