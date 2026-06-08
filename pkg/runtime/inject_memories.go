package runtime

import (
	"context"

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

// injectMemoriesBuiltin is the turn_start builtin entry point.
// The actual retrieval logic lands in a later commit; this skeleton
// keeps the registration valid so applyInjectMemoriesDefault can wire
// the hook entry without LookupBuiltin failing.
func (r *LocalRuntime) injectMemoriesBuiltin(_ context.Context, _ *hooks.Input, _ []string) (*hooks.Output, error) {
	return nil, nil
}
