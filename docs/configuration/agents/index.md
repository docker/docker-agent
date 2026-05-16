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
    add_description_parameter: bool # Optional: add description to tool schema
    redact_secrets: boolean # Optional: scrub detected secrets out of tool args, outgoing chat messages, and tool output
    code_mode_tools: boolean # Optional: enable code mode tool format
    max_iterations: int # Optional: max tool-calling loops
    max_consecutive_tool_calls: int # Optional: max identical consecutive tool calls
    max_old_tool_call_tokens: int # Optional: token budget for old tool call content
    num_history_items: int # Optional: limit conversation history
    skills: boolean | [list] # Optional: enable skill discovery (true/false or list of names and/or sources)
    commands: # Optional: named prompts
      name: "prompt text"
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
```

<div class="callout callout-tip" markdown="1">
<div class="callout-title">See also
</div>
  <p>For model parameters, see <a href="{{ '/configuration/models/' | relative_url }}">Model Config</a>. For tool details, see <a href="{{ '/configuration/tools/' | relative_url }}">Tool Config</a>. For multi-agent patterns, see <a href="{{ '/concepts/multi-agent/' | relative_url }}">Multi-Agent</a>.</p>

</div>

## Properties Reference

| Property                    | Type    | Required | Description                                                                                                                                                                   |
| --------------------------- | ------- | -------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `model`                     | string  | ✓        | Model reference. Either inline (`openai/gpt-5-mini`) or a named model from the `models` section.                                                                              |
| `description`               | string  | ✓        | Brief description of the agent's purpose. Used by coordinators to decide delegation.                                                                                          |
| `instruction`               | string  | ✓        | System prompt that defines the agent's behavior, personality, and constraints.                                                                                                |
| `sub_agents`                | array   | ✗        | List of agent names or external OCI references this agent can delegate to. Supports local agents, registry references (e.g., `agentcatalog/pirate`), and named references (`name:reference`). Automatically enables the `transfer_task` tool. See [External Sub-Agents]({{ '/concepts/multi-agent/#external-sub-agents-from-registries' | relative_url }}). |
| `toolsets`                  | array   | ✗        | List of tool configurations. See [Tool Config]({{ '/configuration/tools/' | relative_url }}).                                                                                                        |
| `fallback`                  | object  | ✗        | Automatic model failover configuration.                                                                                                                                       |
| `add_date`                  | boolean | ✗        | When `true`, injects the current date into the agent's context.                                                                                                               |
| `add_environment_info`      | boolean | ✗        | When `true`, injects working directory, OS, CPU architecture, and git info into context.                                                                                      |
| `add_prompt_files`          | array   | ✗        | List of file paths whose contents are appended to the system prompt. Useful for including coding standards, guidelines, or additional context.                                |
| `add_description_parameter` | boolean | ✗        | When `true`, adds agent descriptions as a parameter in tool schemas. Helps with tool selection in multi-agent scenarios.                                                      |
| `redact_secrets`            | boolean | ✗        | When `true`, scrubs detected secrets (API keys, tokens, private keys, etc.) out of tool-call arguments, outgoing chat messages, and tool output before they reach a tool, the model, or downstream consumers. See [Redacting Secrets](#redacting-secrets) below.   |
| `code_mode_tools`           | boolean | ✗        | When `true`, formats tool responses in a code-optimized format with structured output schemas. Useful for MCP gateway and programmatic access.                                |
| `max_iterations`            | int     | ✗        | Maximum number of tool-calling loops. Default: unlimited (0). Set this to prevent infinite loops.                                                                             |
| `max_consecutive_tool_calls` | int     | ✗        | Maximum consecutive identical tool calls before the agent is terminated, preventing degenerate loops. Default: `5`.                                                          |
| `max_old_tool_call_tokens`  | int     | ✗        | Maximum number of tokens to keep from old tool call arguments and results. Older tool calls beyond this budget have their content replaced with a placeholder, saving context space. Tokens are approximated as `len/4`. Set to `-1` to disable truncation (unlimited). Default: `40000`. |
| `num_history_items`         | int     | ✗        | Limit the number of conversation history messages sent to the model. Useful for managing context window size with long conversations. Default: unlimited (all messages sent). |
| `skills`                    | bool/array | ✗     | Enable automatic skill discovery. `true` loads all discovered local skills, `false` disables them. A list can mix skill sources (`local` or `https://…` URLs) and skill names to include — see [Skills]({{ '/features/skills/' | relative_url }}).                                                     |
| `commands`                  | object  | ✗        | Named prompts that can be run with `docker agent run config.yaml /command_name`.                                                                                              |
| `welcome_message`           | string  | ✗        | Message displayed to the user when a session starts. Useful for providing context or instructions.                                                                            |
| `handoffs`                  | array   | ✗        | List of agent names this agent can hand off the conversation to. Enables the `handoff` tool. See [Handoffs Routing]({{ '/concepts/multi-agent/#handoffs-routing' | relative_url }}).                  |
| `hooks`                     | object  | ✗        | Lifecycle hooks for running commands at various points. See [Hooks]({{ '/configuration/hooks/' | relative_url }}).                                                                                   |
| `structured_output`         | object  | ✗        | Constrain agent output to match a JSON schema. See [Structured Output]({{ '/configuration/structured-output/' | relative_url }}).                                                                    |
| `cache`                     | object  | ✗        | Response cache. When the same user question is asked again, the previous answer is replayed verbatim and the model is not called. See [Response Cache](#response-cache) below.                  |

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
    model: openai/gpt-5-mini
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

## Redacting Secrets

The `redact_secrets` flag is a single agent-level switch that scrubs accidentally leaked credentials, tokens, and private keys out of an agent's I/O. It wires up three complementary defenses:

1. A `pre_tool_use` built-in hook that scrubs detected secrets from the **arguments of every tool call**, before the tool sees them.
2. A `before_llm_call` built-in hook that scrubs the same patterns from **outgoing chat messages** — message content, multi-part text content, prior reasoning content, and the JSON-encoded arguments of any tool call still in the conversation — before they reach the model provider.
3. A `tool_response_transform` built-in hook that scrubs **tool output at the source**, so the secret never reaches event consumers, the persisted session file, the `post_tool_use` hook input, or the next LLM call.

```yaml
agents:
  root:
    model: openai/gpt-5-mini
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
    model: openai/gpt-5-mini
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
        - openai/gpt-5-mini
        - google/gemini-2.5-flash
      retries: 2
      cooldown: 1m
```

## Named Commands

Define reusable prompt shortcuts:

```yaml
agents:
  root:
    model: openai/gpt-5-mini
    instruction: You are a system administrator.
    commands:
      df: "Check how much free space I have on my disk"
      logs: "Show me the last 50 lines of system logs"
      greet: "Say hello to ${env.USER}"
      deploy: "Deploy ${env.PROJECT_NAME || 'app'} to ${env.ENV || 'staging'}"
```

```bash
# Run commands from the CLI
$ docker agent run agent.yaml /df
$ docker agent run agent.yaml /greet
$ PROJECT_NAME=myapp ENV=production docker agent run agent.yaml /deploy
```

Commands use JavaScript template literal syntax for environment variable interpolation. Undefined variables expand to empty strings.

The same syntax is also expanded in agent and toolset instructions: `agents.<name>.instruction` and `toolsets[*].instruction` now support `${env.X}` placeholders (with optional `||` defaults and ternary expressions). Note that `agents.<name>.description` and `agents.<name>.welcome_message` already supported this syntax.

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
      models: [openai/gpt-5-mini]
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
    model: openai/gpt-5-mini
    description: Web researcher with memory
    instruction: Search for information and remember findings.
    toolsets:
      - type: mcp
        ref: docker:duckduckgo
      - type: memory
        path: ./research.db
```
