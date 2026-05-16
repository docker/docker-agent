package hooks

import (
	"github.com/docker/docker-agent/pkg/config/latest"
)

// The persisted hooks types live next to the config schema; the
// runtime uses these short aliases. Adding a new event is a one-line
// change on [latest.HooksConfig] plus one line in compileEvents.
type (
	// Config is the hooks configuration for an agent.
	Config = latest.HooksConfig
	// Hook is a single hook entry. The Type field is one of the
	// HookType* constants below; unrecognised values are rejected by
	// the executor at registry lookup.
	Hook = latest.HookDefinition
	// MatcherConfig pairs a tool-name regex with the hooks to run when
	// it matches (used by EventPreToolUse, EventPostToolUse, and
	// EventPermissionRequest).
	MatcherConfig = latest.HookMatcherConfig
)

// HookType values populate [Hook.Type].
type HookType = string

const (
	// HookTypeCommand runs a shell command.
	HookTypeCommand HookType = "command"
	// HookTypeBuiltin dispatches to a named in-process Go function
	// registered via [Registry.RegisterBuiltin]. The name is stored in
	// [Hook.Command].
	HookTypeBuiltin HookType = "builtin"
	// HookTypeModel asks an LLM and translates the reply into the
	// hook's native [Output] shape. It is registered by the runtime
	// because it depends on the runtime's model provider stack.
	HookTypeModel HookType = "model"
)
