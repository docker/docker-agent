# docker-agent vs opencode: Migration Assessment

**Date:** 2026-05-13  
**Scope:** Can docker-agent replace our opencode GM team setup? Can it wrap CLI harnesses as agent backends?

---

## TL;DR

docker-agent is architecturally superior for multi-agent orchestration and distribution. It cannot today wrap CLI harnesses (codex, claude CLI, aider) as *model backends* — only as tools. Migration is feasible but costs 3-4 weeks without the CLI harness feature, more with it. Recommend: don't migrate the GM team now; build one greenfield docker-agent for a new use case to learn the system, then decide.

---

## 1. Feature Parity

### docker-agent has that opencode doesn't

- **OCI registry**: push/pull agent configs as OCI artifacts — `docker agent run agentcatalog/pirate`
- **serve mcp**: expose any agent as an MCP tool callable by other agents or systems
- **serve a2a**: A2A protocol server for agent-to-agent interop
- **serve api**: HTTP API server
- **background_agents**: true parallel sub-agent dispatch (non-blocking)
- **Fallback models**: automatic failover to secondary models on error
- **Rule-based routing**: route to different models based on example phrases
- **Hooks system**: before_llm_call, pre_tool_use, post_tool_use, on_agent_switch, subagent_stop — all scriptable
- **Context compaction**: automatic summarization when context fills
- **Session persistence**: SQLite, resumable sessions
- **Lifecycle management**: MCP server auto-restart with backoff
- **Deferred toolsets**: OAuth flows deferred until first user interaction
- **A2A + MCP composability**: agents can be both consumers and producers of MCP/A2A

### opencode has that docker-agent doesn't (or does worse)

- **First-class file editing primitives**: Read (with line numbers), Edit (exact-match replacement), Write (with safety guards), Grep, Glob — tuned for code. docker-agent's `filesystem` toolset is coarser.
- **Mature TUI**: opencode's TUI is purpose-built for coding workflows. docker-agent's TUI is more general.
- **CLI harness as model**: opencode's provider abstraction is more open to extension in practice. docker-agent has no subprocess/CLI model provider.
- **Implicit skill injection**: opencode's skill system auto-injects based on task context. docker-agent's skills require explicit `run_skill` tool calls.

---

## 2. Multi-Agent Orchestration

**Yes, docker-agent can replicate our GM → specialist pattern.** The primitives are actually better:

- `transfer_task`: sequential delegation, blocks parent, child gets fresh context window
- `run_background_agent`: parallel dispatch, non-blocking, coordinator polls for results
- `handoff`: linear pipeline (agent A → agent B → agent C)
- Each agent gets its own model, instruction, toolsets
- Sub-agents inherit excluded tools from parent (no recursive loops)
- `subagent_stop` hook fires on every child completion — good for logging/routing

**What maps cleanly:**
- GM agent with `sub_agents: [architect, engineer, code-reviewer, ...]` — direct equivalent
- Different model per agent — direct equivalent
- Fresh context window per delegation — direct equivalent (newSubSession builds a clean session)
- Skills as system prompt injection — direct equivalent via `run_skill` tool

**What doesn't map cleanly:**
- Our GM's "auto-load skill based on task" pattern requires explicit `run_skill` calls in docker-agent. The skill isn't auto-injected; the GM has to decide to call it.
- Our parallel wave execution (multiple subagents in parallel) maps to `run_background_agent`, but the coordinator pattern is different — the GM has to poll, not await.

---

## 3. CLI Harness Wrapping — The Key Question

**Short answer: Not possible today. Possible with ~2-4 weeks of Go work.**

### What "CLI harness as model" means

Instead of docker-agent calling `https://api.openai.com/v1/chat/completions`, it would:
1. Serialize the chat history to stdin (or a temp file)
2. Spawn `codex --exec` (or `claude`, `aider`, etc.)
3. Read the response from stdout
4. Parse tool calls from the CLI's output format
5. Return the result to the runtime loop

### Why it doesn't exist today

The model provider system (`pkg/model/provider/`) has concrete implementations for:
- OpenAI (chat completions + responses API)
- Anthropic
- Gemini
- Bedrock
- Vertex AI
- DMR (Docker Model Runner)
- Custom OpenAI-compatible (via `base_url`)

There is no `subprocess` or `cli` provider. The provider interface requires implementing a streaming chat completion contract — it's not designed for subprocess I/O.

### Workarounds available today

**Option A: `script` toolset** — expose `codex --exec "..."` as a *tool* the LLM can call. The LLM (e.g., Claude) decides when to invoke codex, gets the output back as a tool result. This is NOT the same as codex being the model — Claude is still the reasoning engine, codex is just a tool it can use.

**Option B: `base_url` custom provider** — if the CLI harness exposes an OpenAI-compatible HTTP server (e.g., `codex serve --port 8080`), point docker-agent at it. This works if the CLI supports it. Most don't out of the box.

**Option C: `serve mcp` composition** — run a separate docker-agent instance configured with the CLI harness as a tool, expose it as an MCP server, call it from the main agent. Adds latency and complexity.

### Implementation path for true CLI harness support

Add `pkg/model/provider/cli/` implementing the provider interface:

```go
type CLIProvider struct {
    Command string
    Args    []string
    StdinFormat  string // "prompt", "json", "openai"
    StdoutFormat string // "text", "json", "openai"
    Timeout time.Duration
}
```

Config would look like:
```yaml
models:
  codex-model:
    provider: cli
    model: codex
    provider_opts:
      command: codex
      args: ["--exec", "--json-output"]
      stdin_format: openai_messages
      stdout_format: openai_response
      timeout: 600s

agents:
  codex-engineer:
    model: codex-model
    description: "Codex CLI as the engineer"
    toolsets:
      - type: filesystem
```

**Complexity:** Medium. The provider interface is well-defined. The hard parts are:
1. Tool call serialization/deserialization (each CLI has its own format)
2. Streaming (most CLIs don't stream in a parseable way)
3. Error handling (exit codes, stderr, timeouts)
4. Session state (some CLIs are stateful, some aren't)

Estimate: 1-2 weeks for a basic working implementation, 3-4 weeks for production-quality with tool interop.

---

## 4. Skills System

**Highly compatible. Port is mostly mechanical.**

Our skills are markdown files. docker-agent's skills system:
- Loads markdown files from disk (`skills: true` or `skills: ["local"]`)
- Injects the skill content as the system prompt for a sub-session
- Agent calls `run_skill` tool with the skill name
- Sub-session runs with the skill as its system prompt

**What maps directly:**
- Skill markdown content — unchanged
- Skill directory structure — minor path adjustments
- Skill invocation — GM calls `run_skill("architect")` instead of `Task(subagent_type="architect")`

**What doesn't map:**
- Our skills include `<skill_files>` sections that reference scripts. docker-agent skills don't have a native "bundled resources" concept — you'd need to reference absolute paths or use the `add_prompt_files` config.
- Our skill system auto-loads based on task context. docker-agent requires explicit `run_skill` calls.
- The `skill` tool in opencode is a first-class primitive. In docker-agent, `run_skill` is a built-in tool that the GM must know to call.

---

## 5. Model Diversity

**Yes, fully supported.** Each agent gets its own model:

```yaml
agents:
  gm:
    model: anthropic/claude-opus-4-7
  engineer:
    model: openai/gpt-5.5
  code-reviewer:
    model: google/gemini-3.1-pro
  architect:
    model: anthropic/claude-opus-4-7
  data-analyst:
    model: openai/gpt-5.5
```

Inline `provider/model` shorthand works. Full `ModelConfig` with temperature, max_tokens, etc. also works. Fallback chains per agent. Rule-based routing per agent.

**Caveat:** Verify that docker-agent's OpenAI provider supports GPT-5.5's specific API parameters (reasoning effort, etc.) before committing. Provider implementations lag model releases.

---

## 6. Migration Complexity

| Workstream | Effort | Notes |
|---|---|---|
| Translate ~70 skills to docker-agent layout | 1-2 days | Mechanical. Content unchanged. |
| Build GM + specialist agent YAMLs | 2-3 days | One multi-agent YAML. Test each handoff. |
| Wire MCP toolsets (Notion, Slack, Granola, Snowflake, Opine, Chorus, BambooHR) | 2-3 days | Each MCP server needs a config entry. |
| Replace opencode's file editing primitives | 3-5 days | **Underestimated cost.** Need custom MCP server or use coarser `filesystem` toolset. |
| Test TUI workflows | 1 week | Real-use shakedown. Things will break. |
| Build CI/CD for agent configs | 2-3 days | YAML linting, OCI push. |
| **Total without CLI harness** | **3-4 weeks** | One engineer, focused. |
| **+ CLI harness model provider** | **+2-4 weeks** | If you want codex/claude CLI as model backend. |

**The underestimated cost:** opencode's Read/Edit/Write/Grep/Glob are first-class, line-number-aware, safety-guarded file editing tools tuned for code. docker-agent's `filesystem` toolset is coarser. To replicate the editing quality, you'd write a custom MCP server exposing the same primitives. That's real work.

---

## 7. Recommendation

**Don't migrate the GM team now. Revisit in 2-3 months.**

### Why not now

1. **CLI harness as model doesn't exist.** If this is the central reason to evaluate docker-agent, you're paying migration cost *plus* feature development cost. Not a win.

2. **Your current setup works.** The skill library is mature, the GM/specialist pattern is tuned, the team has muscle memory. Marginal gain from docker-agent's orchestration primitives doesn't outweigh switching costs.

3. **File editing quality gap.** The opencode file primitives are better for coding workflows today. Closing this gap requires building a custom MCP server.

### When to change this recommendation

- **You want to deploy agents to non-engineering users** (sales, finance, support). docker-agent's binary distribution and OCI packaging is much cleaner. **Migrate.**
- **You need agents callable by other systems** (API, A2A, MCP). docker-agent wins clearly. **Migrate.**
- **You're hitting opencode-specific blockers.** **Migrate to whichever unblocks fastest.**
- **Strategic alignment** — you work at Docker, you want to standardize on your own product. **Migrate, accept the cost as investment.**

### Suggested path if you do migrate

1. Build the file-tools MCP server first (the underestimated cost).
2. Port one specialist (engineer) end-to-end before doing the rest.
3. Decide on CLI harness model provider explicitly — Phase 1 or post-migration?
4. Don't try to do this and ship anything else in the same month.

### The honest one-liner

docker-agent is the better *architecture* for where multi-agent systems are heading. opencode is the better *coding tool* for where you are today. Migrate when the architecture benefit (distribution, composability, A2A) becomes load-bearing, not before.

