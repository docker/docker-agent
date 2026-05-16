package runtime

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
)

// BuiltinCacheResponse is the name of the builtin stop hook that persists
// an agent's response into its configured response cache. It is
// auto-injected by [LocalRuntime.hooksExec] when the agent has a cache
// configured, mirroring the way [builtins.AddDate] et al. are
// auto-injected from agent flags via [builtins.ApplyAgentDefaults].
const BuiltinCacheResponse = "cache_response"

// applyCacheDefault appends the cache_response stop hook to cfg when a
// has a configured response cache, mirroring the role of
// [builtins.ApplyAgentDefaults] for the runtime-private cache builtin.
//
// The helper accepts (and may return) a nil cfg so callers can chain
// it after [builtins.ApplyAgentDefaults] without an extra branch.
func applyCacheDefault(cfg *hooks.Config, a *agent.Agent) *hooks.Config {
	if a.Cache() == nil {
		return cfg
	}
	if cfg == nil {
		cfg = &hooks.Config{}
	}
	cfg.Stop = append(cfg.Stop, hooks.Hook{
		Type:    hooks.HookTypeBuiltin,
		Command: BuiltinCacheResponse,
	})
	return cfg
}

// tryReplayCachedResponse looks up the latest user message in the agent's
// response cache. On a hit it replays the cached answer as the assistant
// message, fires stop hooks (which lets user-defined stop hooks run as
// they would for a real response), and returns true so the caller can
// short-circuit the run.
//
// The storage half of the cache is implemented as a builtin stop hook
// (see [LocalRuntime.cacheResponseBuiltin]); the lookup half stays here
// because no hook event currently supports short-circuiting the run with
// a synthetic response.
func (r *LocalRuntime) tryReplayCachedResponse(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	events EventSink,
) bool {
	c := a.Cache()
	if c == nil {
		return false
	}
	question := sess.GetLastUserMessageContent()
	if question == "" {
		return false
	}
	cached, ok := c.Lookup(question)
	// Treat empty stored values as misses: cache_response only stores
	// non-empty responses, so an empty entry only surfaces if the JSON
	// file was hand-edited or downgraded from a future version. Replaying
	// nothing would leave the user staring at a blank assistant message,
	// so we fall through to the model instead.
	if !ok || cached == "" {
		return false
	}

	slog.DebugContext(ctx, "Response cache hit; replaying cached answer",
		"agent", a.Name(), "session_id", sess.ID)
	modelID := a.Model(ctx).ID().String()
	events.Emit(AgentInfo(a.Name(), modelID, a.Description(), a.WelcomeMessage()))
	addAgentMessage(sess, a, &chat.Message{
		Role:      chat.MessageRoleAssistant,
		Content:   cached,
		CreatedAt: time.Now().Format(time.RFC3339),
		Model:     modelID,
	}, events)
	r.executeStopHooks(ctx, sess, a, cached, events)
	return true
}

// cacheResponseBuiltin is the stop-hook builtin that stores the
// assistant's response in the agent's response cache. It is registered
// as a closure on the runtime's hooks registry so it can resolve the
// agent (and therefore its cache instance) by name from
// [hooks.Input.AgentName].
//
// The hook is a no-op when the agent has no cache configured, when the
// dispatched input lacks a user message to key on, or when the response
// has no visible content. Storing the same answer twice is also a no-op
// (handled inside [cache.Cache.Store]), which makes the replay path —
// where [LocalRuntime.tryReplayCachedResponse] fires stop hooks for the
// cached answer — free of redundant disk writes.
func (r *LocalRuntime) cacheResponseBuiltin(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
	if in == nil || in.AgentName == "" || in.LastUserMessage == "" ||
		strings.TrimSpace(in.StopResponse) == "" {
		return nil, nil
	}
	a, err := r.team.Agent(in.AgentName)
	if err != nil || a == nil {
		slog.Debug("cache_response: agent lookup failed",
			"agent", in.AgentName, "error", err)
		return nil, nil
	}
	if c := a.Cache(); c != nil {
		c.Store(in.LastUserMessage, in.StopResponse)
	}
	return nil, nil
}
