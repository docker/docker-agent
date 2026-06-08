---
title: "Agent Configuration"
description: "Complete reference for defining agents in your YAML configuration."
permalink: /configuration/agents/
---

# Agent Configuration

_Complete reference for defining agents in your YAML configuration._

## Full Schema

<!-- yaml-lint:skip -->
```yaml
agents:
  agent_name:
    model: string # Required: model reference
    description: string # Required: what this agent does
    instruction: string # Required: system prompt
    sub_agents: [list] # Optional: local or external sub-agent references
    toolsets: [list] # Optional: tool configurations (use `type: rag` for RAG sources)
    fallback: # Optional: fallback config
      models: [list]
      retries: 2
      cooldown: 1m
    add_date: boolean # Optional: add date to context
    add_environment_info: boolean # Optional: add env info to context
    add_prompt_files: [list] # Optional: include additional prompt files
    inject_memories: boolean # Optional: inject relevant memories at turn start
    max_inject_memories: int # Optional: cap on injected memories (default: 10)
    inject_memories_strategy: string # Optional: retrieval strategy, "local" (default)
    add_description_parameter: bool # Optional: add description to tool schema
    redact_secrets: boolean # Optional: scrub detected secrets out of tool args, outgoing chat messages, and tool output
    code_mode_tools: boolean # Optional: enable code mode tool format
    max_iterations: int # Optional: max tool-calling loops
    max_consecutive_tool_calls: int # Optional: max identical consecutive tool calls
    max_old_tool_call_tokens: int # Optional: token budget for old tool call content
    num_history_items: int # Optional: limit conversation history
    skills: boolean | [list] # Optional: enable skill discovery (true/false or list of names and/or sources)
    use_commands: [list] # Optional: names of top-level commands groups to merge into this agent
    use_skills: [list] # Optional: names of top-level skills groups to merge into this agent
    commands: # Optional: named prompts
      name: "prompt text" # or {instruction: "prompt", agent: "sub_agent_name"}
    welcome_message: string # Optional: message shown at session start
    handoffs: [list] # Optional: agent names this agent can hand off to
    hooks: # Optional: lifecycle hooks
      pre_tool_use: [list]
      tool_response_transform: [list]
      post_tool_use: [list]
      session_start: [list]
      session_end: [list]
      on_user_input: [list]
      stop: [list]
      notification: [list]
    structured_output: # Optional: constrain output format
      name: string
      schema: object
    cache: # Optional: response cache (skip the model on repeat questions)
      enabled: boolean
      case_sensitive: boolean
      trim_spaces: boolean
      path: string
    harness: # Optional: delegate to an external coding CLI (Claude Code, Codex, opencode, pi)
      type: string # Required: claude-code | codex | opencode | pi
      model: string # Optional: model override forwarded to the CLI
      effort: string # claude-code only: low | medium | high | max
      agent: string # opencode only: agent profile name
      thinking: boolean # opencode only: enable extended thinking
```

<div class="callout callout-tip" markdown="1">
<div class="callout-title">See also
</div>
  <p>For model parameters, see <a href="{{ '/configuration/models/' | relative_url }}">Model Config</a>. For tool details, see <a href="{{ '/configuration/tools/' | relative_url }}">Tool Config</a>. For multi-agent patterns, see <a href="{{ '/concepts/multi-agent/' | relative_url }}">Multi-Agent</a>.</p>

</div>

## Properties Reference

| Property                    | Type    | Required | Description                                                                                                                                                                   |
| --------------------------- | ------- | -------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `model`                     | string  | ✓        | Model reference. Either inline (`openai/gpt-5`) or a named model from the `models` section.                                                                              |
| `description`               | string  | ✓        | Brief description of the agent's purpose. Used by coordinators to decide delegation.                                                                                          |
| `instruction`               | string  | ✓        | System prompt that defines the agent's behavior, personality, and constraints.                                                                                                |
| `sub_agents`                | array   | ✗        | List of agent names or external OCI references this agent can delegate to. Supports local agents, registry references (e.g., `agentcatalog/pirate`), and named references (`name:reference`). Automatically enables the `transfer_task` tool. See [External Sub-Agents]({{ '/concepts/multi-agent/#external-sub-agents-from-registries' | relative_url }}). |
| `toolsets`                  | array   | ✗        | List of tool configurations. See [Tool Config]({{ '/configuration/tools/' | relative_url }}).                                                                                                        |
| `fallback`                  | object  | ✗        | Automatic model failover configuration.                                                                                                                                       |
| `add_date`                  | boolean | ✗        | When `true`, injects the current date into the agent's context.                                                                                                               |
| `add_environment_info`      | boolean | ✗        | When `true`, injects working directory, OS, CPU architecture, and git info into context.                                                                                      |
| `add_prompt_files`          | array   | ✗        | List of file paths whose contents are appended to the system prompt. Useful for including coding standards, guidelines, or additional context.                                |
| `inject_memories`           | boolean | ✗        | When `true`, the runtime fetches the most relevant stored memories at the start of every turn and injects them as a transient system message. Requires a `memory` toolset. See [Inject Memories](#inject-memories) below. |
| `max_inject_memories`       | int     | ✗        | Maximum number of memories injected per turn. Default: `10`.                                                                                                                  |
| `inject_memories_strategy`  | string  | ✗        | Retrieval strategy for `inject_memories`. `"local"` (default): in-process BM25 ranker against the user's message — cheap and deterministic. Note: an `"llm"` strategy is planned for a future release. |
| `add_description_parameter` | boolean | ✗        | When `true`, adds agent descriptions as a parameter in tool schemas. Helps with tool selection in multi-agent scenarios.                                                      |
| `redact_secrets`            | boolean | ✗        | When `true`, scrubs detected secrets (API keys, tokens, private keys, etc.) out of tool-call arguments, outgoing chat messages, and tool output before they reach a tool, the model, or downstream consumers. See [Redacting Secrets](#redacting-secrets) below.   |
| `code_mode_tools`           | boolean | ✗        | When `true`, formats tool responses in a code-optimized format with structured output schemas. Useful for MCP gateway and programmatic access.                                |
| `max_iterations`            | int     | ✗        | Maximum number of tool-calling loops. Default: unlimited (0). Set this to prevent infinite loops.                                                                             |
| `max_consecutive_tool_calls` | int     | ✗        | Maximum consecutive identical tool calls before the agent is terminated, preventing degenerate loops. Default: `5`.                                                          |
| `max_old_tool_call_tokens`  | int     | ✗        | Maximum number of tokens to keep from old tool call arguments and results. Older tool calls beyond this budget have their content replaced with a placeholder, saving context space. Tokens are approximated as `len/4`. Set to `-1` to disable truncation (unlimited). Default: `40000`. |
| `num_history_items`         | int     | ✗        | Limit the number of conversation history messages sent to the model. Useful for managing context window size with long conversations. Default: unlimited (all messages sent). |
| `skills`                    | bool/array | ✗     | Enable automatic skill discovery. `true` loads all discovered local skills, `false` disables them. A list can mix skill sources (`local` or `https://…` URLs) and skill names to include — see [Skills]({{ '/features/skills/' | relative_url }}).                                                     |
| `commands`                  | object  | ✗        | Named prompts that can be run with `docker agent run config.yaml /command_name`. Can be simple strings or objects with `instruction` and/or `agent` fields for agent switching. See [Named Commands](#named-commands) below. |
| `use_commands`              | list of string | ✗   | Names of top-level `commands` groups to merge into this agent. Inline `commands` entries take precedence on name conflicts. Default: `[]`. |
| `use_skills`                | list of string | ✗   | Names of top-level `skills` groups to merge into this agent. Inline skills are deduplicated by name against merged entries. Default: `[]`. |
| `welcome_message`           | string  | ✗        | Message displayed to the user when a session starts. Rendered as Markdown in the TUI. **Not sent to the model** — it exists purely for the user's benefit. Useful for telling users what the agent can do and what commands are available. |
| `handoffs`                  | array   | ✗        | List of agent names this agent can hand off the conversation to. Enables the `handoff` tool. See [Handoffs Routing]({{ '/concepts/multi-agent/#handoffs-routing' | relative_url }}).                  |
| `hooks`                     | object  | ✗        | Lifecycle hooks for running commands at various points. See [Hooks]({{ '/configuration/hooks/' | relative_url }}).                                                                                   |
| `structured_output`         | object  | ✗        | Constrain agent output to match a JSON schema. See [Structured Output]({{ '/configuration/structured-output/' | relative_url }}).                                                                    |
| `cache`                     | object  | ✗        | Response cache. When the same user question is asked again, the previous answer is replayed verbatim and the model is not called. See [Response Cache](#response-cache) below.                  |
| `harness`                   | object  | ✗        | Run this agent through an external coding CLI instead of a model. **Note:** Any `toolsets:` defined on the same agent are silently ignored when `harness:` is set — the external CLI brings its own tools. See [Coding Harnesses]({{ '/features/harnesses/' | relative_url }}). |

<div class="callout callout-warning" markdown="1">
<div class="callout-title">max_iterations
</div>
  <p>Default is <code>0</code> (unlimited). Always set <code>max_iterations</code> for agents with powerful tools like <code>shell</code> to prevent infinite loops. A value of 20–50 is typical for development agents.</p>

</div>

## Response Cache

The response cache short-circuits the model when the same user question is asked again. The first time a question is asked, the agent calls the model normally and stores the assistant's reply. Subsequent identical questions skip the model entirely and replay the stored reply verbatim.

```yaml
agents:
  root:
    model: openai/gpt-5
    description: Cached assistant
    instruction: You are a helpful assistant.
    cache:
      enabled: true          # required to turn the cache on
      case_sensitive: false  # default: false ("Hello" == "hello")
      trim_spaces: true      # default: false ("  hello  " == "hello")
      path: ./cache.json     # optional: persist to disk; omit for in-memory
```

| Property         | Type    | Default | Description                                                                                                                                                                                                                       |
| ---------------- | ------- | ------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `enabled`        | boolean | `false` | Master switch. When `false` (or when the `cache` section is omitted), no caching is performed.                                                                                                                                     |
| `case_sensitive` | boolean | `false` | When `true`, questions must match exactly (including case) to hit the cache.                                                                                                                                                       |
| `trim_spaces`    | boolean | `false` | When `true`, leading and trailing whitespace is stripped from the question before it is compared.                                                                                                                                  |
| `path`           | string  | _empty_ | When set, cache entries are persisted to a JSON file at the given path and reloaded on startup so the cache survives restarts. Relative paths resolve against the agent config directory. When empty, the cache lives in memory only. |

**How it works**

- The cache key is the latest user message in the session, normalized according to `case_sensitive` and `trim_spaces`.
- On a hit, the cached reply is added to the session as the assistant message and stop hooks fire normally — the rest of the agent (tools, sub-agents, the model) is bypassed.
- On a miss, the agent runs normally; the final assistant message produced by the first stop of the run is then stored under the question's key.
- Only the response to the original user question of a run is cached; follow-up turns inside the same `RunStream` are not.

**File-backed storage**

When `path` is set, every `Store` rewrites the entire cache file. Writes are **atomic**: the new content is written to a sibling temp file, `fsync`'d, and renamed over the destination, so a concurrent reader (or a process that crashes mid-write) will always see either the previous content or the new content in full — never a partially written file. The parent directory is also `fsync`'d after the rename so the rename itself is durable.

**Cross-process sharing**

Multiple processes can share the same `path:` cache file safely. Every `Store` takes an exclusive advisory lock on a sibling `<path>.lock` file (POSIX `flock(2)` on Unix, `LockFileEx` on Windows), reloads the current on-disk state under the lock, merges the new entry, and writes back atomically. Two processes that store *different* keys at the same time both see their writes preserved on disk; the lock window is short (one read + one fsync'd write).

`Lookup` watches the file's modification time and reloads the in-memory map when the file has advanced since its last load, so writes from a sibling process become visible without a restart. The `<path>.lock` sentinel file is created on first write and never deleted: removing it would let two processes lock different inodes and lose mutual exclusion.

## Inject Memories

When `inject_memories: true`, the runtime retrieves the most relevant memories from the agent's memory toolset at the start of every turn and injects them as a transient system message. The injection is **never persisted** to the session transcript — it is invisible in session replays by design, matching the behaviour of `add_date` and `add_environment_info`.

Requires a `memory` toolset on the same agent. If no memory toolset is configured, the runtime emits a warning and the hook is a no-op.

```yaml
agents:
  assistant:
    model: openai/gpt-4o-mini
    instruction: |
      You are a helpful assistant that remembers user preferences.
    toolsets:
      - type: memory
    inject_memories: true
    max_inject_memories: 5
    inject_memories_strategy: local
```

### Retrieval strategies

| Strategy | Description |
| -------- | ----------- |
| `local` (default) | In-process BM25 ranker scores all stored memories against the user's message. Cheap, deterministic, no extra model call. Uses a per-turn snapshot cache; the cache is invalidated whenever a memory is written via the agent's memory tools. |

> **Note:** An `llm` strategy is planned for a future release.

### FAQ

**Why don't I see the injected memories in my session transcript?**
Injection is transient by design — matching the behaviour of `add_date` and `add_environment_info`. The memories are visible to the model during the turn but are not saved to the session file.

**When are memories refreshed from disk?**
The `local` strategy maintains an in-process snapshot cache per agent. The cache is refreshed whenever a memory write occurs through the agent's own memory tools (`add_memory`, `update_memory`, `delete_memory`). Each refresh issues a single `GetMemories` call to SQLite.

**What if an agent has multiple memory toolsets?**
Only the first memory toolset found is used for injection. Configure a single `type: memory` toolset per agent for predictable behaviour.

## Redacting Secrets

The `redact_secrets` flag is a single agent-level switch that scrubs accidentally leaked credentials, tokens, and private keys out of an agent's I/O. It wires up three complementary defenses:

1. A `pre_tool_use` built-in hook that scrubs detected secrets from the **arguments of every tool call**, before the tool sees them.
2. A `before_llm_call` built-in hook that scrubs the same patterns from **outgoing chat messages** — message content, multi-part text content, prior reasoning content, and the JSON-encoded arguments of any tool call still in the conversation — before they reach the model provider.
3. A `tool_response_transform` built-in hook that scrubs **tool output at the source**, so the secret never reaches event consumers, the persisted session file, the `post_tool_use` hook input, or the next LLM call.

```yaml
agents:
  root:
    model: openai/gpt-5
    description: A helpful assistant that scrubs secrets before they leak
    instruction: |
      You are a helpful assistant. If the user accidentally pastes a token,
      do your best work without echoing the secret back.
    redact_secrets: true
    toolsets:
      - type: shell
```

Detection uses the [portcullis](https://github.com/docker/portcullis) ruleset, which recognises common secret patterns including:

- GitHub Personal Access Tokens (`ghp_*`, `gho_*`, `ghu_*`, `ghs_*`, `ghr_*`, fine-grained `github_pat_*`)
- AWS access keys (`AKIA*`, `ASIA*`, …) and secret access keys
- GitLab PATs (`glpat-*`), Hugging Face tokens (`hf_*`)
- Stripe (`sk_live_*`, `pk_test_*`, …), Slack (`xoxb-*`, …), Shopify, Twilio, Discord, Atlassian, Mailchimp, SendGrid, and many more
- JWTs, GCP service-account JSON, Heroku keys, Docker Hub PATs (`dckr_pat_*`)
- PEM-encoded private keys (`-----BEGIN … PRIVATE KEY-----` blocks)

Each detected span is replaced with the literal string `[REDACTED]`; the surrounding text is preserved so a redacted argument still looks like a legitimate flag (e.g. `--token=[REDACTED]`). Redaction is idempotent — applying it twice yields the same result.

<div class="callout callout-info" markdown="1">
<div class="callout-title">False positives vs. false negatives
</div>
  <p>False positives are extremely rare: every rule pairs a regex with a discriminating keyword, so plain English never trips detection. <strong>False negatives are possible</strong> — only patterns the ruleset recognises are scrubbed, so this is a defense-in-depth feature, not a substitute for keeping secrets out of the conversation in the first place. Pair it with a proper <a href="{{ '/guides/secrets/' | relative_url }}">secret manager</a> for the credentials your agent actually needs.</p>
</div>

<div class="callout callout-info" markdown="1">
<div class="callout-title">Equivalent hook entry
</div>
  <p>Setting <code>redact_secrets: true</code> on the agent is shorthand for auto-registering all three legs of the feature as hook entries. They share the <em>same</em> built-in name (<code>type: builtin</code>, <code>command: redact_secrets</code>) on <code>pre_tool_use</code>, <code>before_llm_call</code>, and <code>tool_response_transform</code> respectively — the implementation dispatches on the hook event. You can spell them out by hand to scope a leg to a subset of tools (set <code>matcher:</code> to a regex), stack them with other rewriters in a specific order, or enable just one or two legs. See <a href="https://github.com/docker/docker-agent/blob/main/examples/redact_secrets_hooks.yaml"><code>examples/redact_secrets_hooks.yaml</code></a> for a complete manual wiring and the <a href="{{ '/configuration/hooks/#available-built-ins' | relative_url }}">Hooks reference</a> for the builtin's event coverage.</p>
</div>

## Welcome Message

Display a message when users start a session:

```yaml
agents:
  assistant:
    model: openai/gpt-5
    description: Development assistant
    instruction: You are a helpful coding assistant.
    welcome_message: |
      👋 Welcome! I'm your development assistant.

      I can help you with:
      - Writing and reviewing code
      - Running tests and debugging
      - Explaining concepts

      What would you like to work on?
```

## Deferred Tool Loading

Toolsets support `defer` to load tools on-demand and speed up agent startup. See [Deferred Tool Loading]({{ '/configuration/tools/#deferred-tool-loading' | relative_url }}) for details.

```yaml
agents:
  root:
    model: anthropic/claude-sonnet-4-5
    description: Multi-purpose assistant
    instruction: You have access to many tools.
    toolsets:
      - type: mcp
        ref: docker:github-official
        defer: true
      - type: filesystem
```

## Fallback Configuration

Automatically switch to backup models when the primary fails:

| Property   | Type   | Default | Description                                                |
| ---------- | ------ | ------- | ---------------------------------------------------------- |
| `models`   | array  | `[]`    | Fallback models to try in order                            |
| `retries`  | int    | `2`     | Retries per model for 5xx errors. `-1` to disable.         |
| `cooldown` | string | `1m`    | How long to stick with a fallback after a rate limit (429) |

**Error handling:**

- **Retryable** (same model with backoff): HTTP 5xx, 408, network timeouts
- **Non-retryable** (skip to next model): HTTP 429, 4xx client errors

```yaml
agents:
  root:
    model: anthropic/claude-sonnet-4-5
    fallback:
      models:
        - openai/gpt-5
        - google/gemini-3.5-flash
      retries: 2
      cooldown: 1m
```

## Named Commands

Define reusable prompt shortcuts that can send prompts to the current agent or switch to a different sub-agent:

> **Note:** Named slash commands execute immediately, even while the agent is processing another message. Unlike regular chat messages (which are queued), slash commands interrupt or direct the agent even while it is mid-response.

```yaml
agents:
  root:
    model: openai/gpt-5
    instruction: You are a system administrator.
    commands:
      df: "Check how much free space I have on my disk"
      logs: "Show me the last 50 lines of system logs"
      greet: "Say hello to ${env.USER}"
      deploy: "Deploy ${env.PROJECT_NAME || 'app'} to ${env.ENV || 'staging'}"
      
      # Advanced format with agent switching
      plan:
        agent: planner  # Switch to the 'planner' sub-agent
        instruction: "Create a detailed plan for: $1"  # Optional: send this prompt after switching
      
      # Agent switching without instruction - forwards remaining text as prompt
      review:
        agent: reviewer  # Any text after /review is sent to the reviewer agent
```


### Command Formats

Commands support two formats:

1. **Simple string format**: The string becomes the instruction sent to the current agent
   ```yaml
   df: "Check disk space"
   ```

2. **Advanced object format**: Supports agent switching and optional instructions
   ```yaml
   plan:
     agent: planner           # Required: name of sub-agent to switch to
     instruction: "Plan: $1"  # Optional: prompt to send after switching
     description: "Switch to planning mode"  # Optional: shown in help text
   ```

When `agent` is set without `instruction`, any text typed after the slash command (e.g., `/plan build a web app`) is forwarded as a prompt to the target agent. The target agent must be listed in the current agent's `sub_agents` array.

### Agent-Switching Commands

Commands with an `agent` field switch the active agent for that command's scope. This is useful for building workflow shortcuts where `/plan`, `/review`, `/deploy` each route the user to the appropriate specialist.

```yaml
agents:
  root:
    model: openai/gpt-5
    description: Main assistant
    instruction: You are a project coordinator.
    sub_agents: [planner, reviewer]
    commands:
      # Switch to planner with a pre-filled prompt
      plan:
        agent: planner
        instruction: "Create a detailed plan for: $1"
      # Switch to reviewer; any text after /review is forwarded
      review:
        agent: reviewer
      # Simple prompt command (no switching)
      status: "Summarize what we have accomplished so far"

  planner:
    model: openai/gpt-5
    description: Planning specialist
    instruction: You create detailed project plans.

  reviewer:
    model: anthropic/claude-sonnet-4-5
    description: Code review specialist
    instruction: You review code and suggest improvements.
```

**Agent-switching vs. `handoff`**

| | Agent-switching command | `handoff` tool |
| --- | --- | --- |
| **Trigger** | User runs `/command` | Model calls `handoff()` |
| **Session** | Stays in the same session | Stays in the same session |
| **History** | Target agent sees full conversation | Target agent sees full conversation |
| **Return** | User must explicitly switch back | Target agent can chain to another agent |

**Agent-switching vs. `transfer_task`**

`transfer_task` launches a **sub-session**: the root agent sends a task, the child runs in isolation, and the result is returned to the root. The root agent stays in control and the child's work is never in the main conversation. Use `transfer_task` (via `sub_agents`) when you want delegation with a clean result; use agent-switching commands when you want to *become* a different agent for the rest of the conversation.

See [`examples/agent_switching_commands.yaml`](https://github.com/docker/docker-agent/blob/main/examples/agent_switching_commands.yaml) for a complete example.

```bash
# Run commands from the CLI
$ docker agent run agent.yaml /df
$ docker agent run agent.yaml /greet
$ PROJECT_NAME=myapp ENV=production docker agent run agent.yaml /deploy
```

Commands use JavaScript template literal syntax (`${env.VAR}`) for environment variable interpolation. Undefined variables expand to empty strings.

The same syntax is also expanded in agent and toolset instructions: `agents.<name>.instruction` and `toolsets[*].instruction` support `${env.X}` placeholders (with optional `||` defaults and ternary expressions). `agents.<name>.description` and `agents.<name>.welcome_message` also support it.

Note that path-like fields (`working_dir`, `path`) primarily use a shell-style syntax (`$VAR`, `${VAR}`, `~`), and also accept `${env.X}` as an alias (though not richer JS expressions). See [Variable Expansion in Config Fields]({{ '/configuration/overview/#variable-expansion-in-config-fields' | relative_url }}) for the full table.

## Complete Example

```yaml
models:
  claude:
    provider: anthropic
    model: claude-sonnet-4-5
    max_tokens: 64000

agents:
  root:
    model: claude
    description: Technical lead coordinating development
    instruction: |
      You are a technical lead. Analyze requests and delegate
      to the right specialist. Always review work before responding.
    welcome_message: "👋 I'm your tech lead. How can I help today?"
    sub_agents: [developer, researcher]
    add_date: true
    add_environment_info: true
    fallback:
      models: [openai/gpt-5]
    toolsets:
      - type: think
    commands:
      review: "Review all recent code changes for issues"
    hooks:
      session_start:
        - type: command
          command: "./scripts/setup.sh"

  developer:
    model: claude
    description: Expert software developer
    instruction: Write clean, tested, production-ready code.
    max_iterations: 30
    toolsets:
      - type: filesystem
      - type: shell
      - type: think
      - type: todo

  researcher:
    model: openai/gpt-5
    description: Web researcher with memory
    instruction: Search for information and remember findings.
    toolsets:
      - type: mcp
        ref: docker:duckduckgo
      - type: memory
        path: ./research.db
```
