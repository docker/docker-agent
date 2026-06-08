package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
)

// BuiltinInjectMemories is the name of the turn_start builtin that
// retrieves relevant memories at the start of every turn and injects
// them into the conversation as a transient system message.
//
// Like cache_response (see cache.go) the builtin is registered on the
// runtime's hooks registry as a closure so it can resolve the agent
// (and therefore its memory toolset and snapshot cache) by name from
// [hooks.Input.AgentName].
const BuiltinInjectMemories = "inject_memories"

// applyInjectMemoriesDefault appends the inject_memories turn_start hook to
// cfg when the agent has inject_memories enabled. Mirrors the role of
// [applyCacheDefault] for the cache_response stop hook.
//
// The helper accepts (and may return) a nil cfg so callers can chain it
// after [builtins.ApplyAgentDefaults] without an extra branch.
func applyInjectMemoriesDefault(cfg *hooks.Config, a *agent.Agent) *hooks.Config {
	if !a.InjectMemories() {
		return cfg
	}
	if cfg == nil {
		cfg = &hooks.Config{}
	}
	cfg.TurnStart = append(cfg.TurnStart, hooks.Hook{
		Type:    hooks.HookTypeBuiltin,
		Command: BuiltinInjectMemories,
	})
	return cfg
}

// effectiveMaxInjectMemories returns the configured cap, falling back to
// [latest.DefaultMaxInjectMemories] when the agent value is zero.
func effectiveMaxInjectMemories(a *agent.Agent) int {
	if n := a.MaxInjectMemories(); n > 0 {
		return n
	}
	return latest.DefaultMaxInjectMemories
}

// injectMemoriesBuiltin is the turn_start builtin entry point.
// The actual retrieval logic lands in a later commit; this skeleton
// keeps the registration valid so applyInjectMemoriesDefault can wire
// the hook entry without LookupBuiltin failing.
func (r *LocalRuntime) injectMemoriesBuiltin(_ context.Context, _ *hooks.Input, _ []string) (*hooks.Output, error) {
	return nil, nil
}
