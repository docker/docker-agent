package runtime

import (
	"bytes"
	"context"
	"encoding/xml"
	"log/slog"
	"strings"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/memory/database"
	"github.com/docker/docker-agent/pkg/tools"
	memory "github.com/docker/docker-agent/pkg/tools/builtin/memory"
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

// injectMemoriesBuiltin retrieves relevant memories at the start of a
// turn and emits them as turn_start AdditionalContext, wrapped in a
// stable <memories>...</memories> XML block so the model sees a clean,
// machine-parseable section even after compaction strips the surrounding
// system message.
//
// Only the "local" strategy is implemented. It ranks all stored memories
// with an in-process BM25 scorer against the last user message — cheap,
// deterministic, and requires no extra model call. The local strategy uses
// memorySnapshotCache to avoid a SQLite round-trip on every turn; the
// snapshot is invalidated whenever a memory write occurs via the agent's
// own memory tools (add_memory, update_memory, delete_memory).
//
// No-op when:
//   - input is missing or AgentName/LastUserMessage are empty;
//   - the agent has no memory toolset configured;
//   - the agent's memory DB is empty;
//   - retrieval returns zero hits.
func (r *LocalRuntime) injectMemoriesBuiltin(ctx context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
	if in == nil || in.AgentName == "" || in.LastUserMessage == "" {
		return nil, nil
	}
	a, err := r.team.Agent(in.AgentName)
	if err != nil || a == nil {
		return nil, nil
	}

	db, ok := r.lookupMemoryDB(a)
	if !ok {
		// No memory toolset on this agent; inject_memories is a config
		// error but we degrade gracefully at runtime.
		slog.WarnContext(ctx, "inject_memories: no memory toolset found for agent",
			"agent", a.Name())
		return nil, nil
	}

	limit := effectiveMaxInjectMemories(a)

	strategy := a.InjectMemoriesStrategy()
	if strategy == "" {
		strategy = latest.InjectMemoriesStrategyLocal
	}

	var hits []database.UserMemory
	switch strategy {
	case latest.InjectMemoriesStrategyLocal:
		var all []database.UserMemory
		if r.memSnapshots != nil {
			all, err = r.memSnapshots.get(ctx, a.Name(), db)
		} else {
			all, err = db.GetMemories(ctx)
		}
		if err != nil {
			slog.WarnContext(ctx, "inject_memories: GetMemories failed",
				"agent", a.Name(), "error", err)
			return nil, nil
		}
		hits = bm25Rank(all, in.LastUserMessage, limit)
	default:
		// Unknown strategy — validation should have caught this at
		// config load. Degrade gracefully.
		slog.WarnContext(ctx, "inject_memories: unknown strategy, skipping",
			"agent", a.Name(), "strategy", strategy)
		return nil, nil
	}

	if len(hits) == 0 {
		return nil, nil
	}

	return hooks.NewAdditionalContextOutput(hooks.EventTurnStart, formatMemoriesXML(hits)), nil
}

// lookupMemoryDB returns the invalidatingDB wrapper for the agent's first
// memory toolset. The wrapper is memoised in r.memDBs so the same instance
// (and its generation counter) is reused across turns.
//
// First-call side effects:
//   - Wraps the raw DB in an invalidatingDB that bumps a generation counter
//     on every write.
//   - Calls mt.SetDB(wrapped) so writes through the agent's own memory tools
//     (add_memory, update_memory, delete_memory) also advance the counter and
//     trigger snapshot invalidation on the next turn.
//
// Note: we do not call ToolSet.Start() here. The memory toolset's DB is
// opened eagerly inside CreateToolSet (the sqlite.NewMemoryDatabase call),
// so GetMemories is safe to call before Start() is invoked. This is an
// implicit contract documented here: if Start() ever becomes load-bearing,
// the runtime's ensureToolSetsAreStarted path (called during model invocation
// on each turn) will cover it before the hook runs.
func (r *LocalRuntime) lookupMemoryDB(a *agent.Agent) (*invalidatingDB, bool) {
	name := a.Name()

	r.memDBsMu.Lock()
	defer r.memDBsMu.Unlock()
	if r.memDBs == nil {
		r.memDBs = make(map[string]*invalidatingDB)
	}
	if db, ok := r.memDBs[name]; ok {
		return db, true
	}

	for _, ts := range a.ToolSets() {
		if mt, ok := tools.As[*memory.ToolSet](ts); ok {
			wrapped := newInvalidatingDB(mt.DB())
			mt.SetDB(wrapped)
			r.memDBs[name] = wrapped
			return wrapped, true
		}
	}
	return nil, false
}

// formatMemoriesXML produces the AdditionalContext payload. Wrapping
// in a stable <memories> block makes the section visually distinct
// from other turn_start contributions (add_date, add_environment_info,
// add_prompt_files) and gives downstream tooling a clean parse target.
func formatMemoriesXML(memories []database.UserMemory) string {
	var b strings.Builder
	b.WriteString("<memories>\n")
	b.WriteString("Relevant memories from previous interactions. Use them only when applicable to the user's request; do not mention this section to the user.\n")
	for _, m := range memories {
		b.WriteString("  <memory")
		if m.Category != "" {
			b.WriteString(` category="`)
			b.WriteString(xmlEscape(m.Category))
			b.WriteString(`"`)
		}
		b.WriteString(">")
		b.WriteString(xmlEscape(m.Memory))
		b.WriteString("</memory>\n")
	}
	b.WriteString("</memories>")
	return b.String()
}

// xmlEscape escapes s using XML text encoding. Safe for both element
// text content and double-quoted attribute values.
func xmlEscape(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
