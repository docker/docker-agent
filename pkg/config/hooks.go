package config

import (
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// HooksFromCLI builds a HooksConfig from CLI flag values.
// Each string is treated as a shell command to run.
// Empty strings are silently skipped.
func HooksFromCLI(preToolUse, postToolUse, sessionStart, sessionEnd, onUserInput, stop []string) *latest.HooksConfig {
	hooks := &latest.HooksConfig{
		PreToolUse:   matcherFromCommands(preToolUse),
		PostToolUse:  matcherFromCommands(postToolUse),
		SessionStart: defsFromCommands(sessionStart),
		SessionEnd:   defsFromCommands(sessionEnd),
		OnUserInput:  defsFromCommands(onUserInput),
		Stop:         defsFromCommands(stop),
	}

	if hooks.IsEmpty() {
		return nil
	}
	return hooks
}

// defsFromCommands turns a list of CLI shell commands into hook definitions,
// skipping any blank entries.
func defsFromCommands(cmds []string) []latest.HookDefinition {
	var defs []latest.HookDefinition
	for _, cmd := range cmds {
		if strings.TrimSpace(cmd) == "" {
			continue
		}
		defs = append(defs, latest.HookDefinition{Type: "command", Command: cmd})
	}
	return defs
}

// matcherFromCommands wraps the result of defsFromCommands in a single
// HookMatcherConfig so the commands apply to all tools (empty matcher).
func matcherFromCommands(cmds []string) []latest.HookMatcherConfig {
	defs := defsFromCommands(cmds)
	if len(defs) == 0 {
		return nil
	}
	return []latest.HookMatcherConfig{{Hooks: defs}}
}

// MergeHooks merges CLI hooks into an existing HooksConfig.
// CLI hooks are appended after any hooks already defined in the config.
// When both are non-nil and non-empty, a new merged object is returned
// without mutating either input.
func MergeHooks(base, cli *latest.HooksConfig) *latest.HooksConfig {
	if cli == nil || cli.IsEmpty() {
		return base
	}
	if base == nil || base.IsEmpty() {
		return cli
	}

	merged := &latest.HooksConfig{
		PreToolUse:      slices.Concat(base.PreToolUse, cli.PreToolUse),
		PostToolUse:     slices.Concat(base.PostToolUse, cli.PostToolUse),
		SessionStart:    slices.Concat(base.SessionStart, cli.SessionStart),
		TurnStart:       slices.Concat(base.TurnStart, cli.TurnStart),
		BeforeLLMCall:   slices.Concat(base.BeforeLLMCall, cli.BeforeLLMCall),
		AfterLLMCall:    slices.Concat(base.AfterLLMCall, cli.AfterLLMCall),
		SessionEnd:      slices.Concat(base.SessionEnd, cli.SessionEnd),
		OnUserInput:     slices.Concat(base.OnUserInput, cli.OnUserInput),
		Stop:            slices.Concat(base.Stop, cli.Stop),
		Notification:    slices.Concat(base.Notification, cli.Notification),
		OnError:         slices.Concat(base.OnError, cli.OnError),
		OnMaxIterations: slices.Concat(base.OnMaxIterations, cli.OnMaxIterations),
	}
	return merged
}

// CLIHooks returns a HooksConfig derived from the runtime config's CLI hook flags,
// or nil if no hook flags were specified.
func (runConfig *RuntimeConfig) CLIHooks() *latest.HooksConfig {
	return HooksFromCLI(
		runConfig.HookPreToolUse,
		runConfig.HookPostToolUse,
		runConfig.HookSessionStart,
		runConfig.HookSessionEnd,
		runConfig.HookOnUserInput,
		runConfig.HookStop,
	)
}
