---
title: "Configuration Overview"
description: "docker-agent uses YAML or HCL configuration files to define agents, models, tools, and their relationships."
permalink: /configuration/overview/
---

# Configuration Overview

_docker-agent uses YAML or HCL configuration files to define agents, models, tools, and their relationships._

## File Structure

A docker-agent config can be written in YAML or HCL. The examples on this page use YAML; see [HCL Configuration]({{ '/configuration/hcl/' | relative_url }}) for the block-based HCL syntax.

A docker-agent config has these main sections:

```bash
# 1. Version — configuration schema version (optional but recommended)
version: 10

# 2. Metadata — optional agent metadata for distribution
metadata:
  author: my-org
  description: My helpful agent
  version: "1.0.0"

# 3. Models — define AI models with their parameters
models:
  claude:
    provider: anthropic
    model: claude-sonnet-4-5
    max_tokens: 64000

# 4. Agents — define AI agents with their behavior
agents:
  root:
    model: claude
    description: A helpful assistant
    instruction: You are helpful.
    toolsets:
      - type: think

# 5. RAG — define retrieval-augmented generation sources (optional)
rag:
  docs:
    docs: ["./docs"]
    strategies:
      - type: chunked-embeddings
        embedding_model: openai/text-embedding-3-small

# 6. MCPs — reusable MCP server definitions (optional)
mcps:
  github:
    remote:
      url: https://api.githubcopilot.com/mcp
      transport_type: sse

# 7. Providers — optional reusable provider definitions
providers:
  my_provider:
    provider: anthropic  # or openai (default), google, amazon-bedrock, etc.
    token_key: MY_API_KEY
    max_tokens: 16384

# 8. Permissions — agent-level tool permission rules (optional)
#    For user-wide global permissions, see ~/.config/cagent/config.yaml
permissions:
  allow: ["read_*"]
  deny: ["shell:cmd=sudo*"]

# 9. Commands & Skills — reusable, named groups shared across agents (optional)
commands:
  ci:
    deploy: "Deploy the application"
skills:
  base: [local, git]
```

## Minimal Config

The simplest possible configuration — a single agent with an inline model:

```yaml
agents:
  root:
    model: openai/gpt-5
    description: A helpful assistant
    instruction: You are a helpful assistant.
```

The same config in HCL:

```hcl
agent "root" {
  model       = "openai/gpt-5"
  description = "A helpful assistant"
  instruction = "You are a helpful assistant."
}
```

## Inline vs Named Models

Models can be referenced inline or defined in the `models` section:

<div class="cards">
  <div class="card" style="cursor:default;">
    <h3>Inline</h3>
    <p>Quick and simple. Use <code>provider/model</code> syntax directly.</p>
    <pre style="margin-top:12px"><code class="language-yaml">model: openai/gpt-5</code></pre>
  </div>
  <div class="card" style="cursor:default;">
    <h3>Named</h3>
    <p>Full control over parameters. Reusable across agents.</p>
    <pre style="margin-top:12px"><code class="language-yaml">model: my_claude</code></pre>
  </div>
</div>

## Config Sections

<div class="cards">
  <a class="card" href="{{ '/configuration/hcl/' | relative_url }}">
    <div class="card-icon">🧱</div>
    <h3>HCL Configuration</h3>
    <p>Write the same agent schema in HCL using labeled blocks, heredocs, and block-based tool definitions.</p>
  </a>
  <a class="card" href="{{ '/configuration/agents/' | relative_url }}">
    <div class="card-icon">🤖</div>
    <h3>Agent Config</h3>
    <p>All agent properties: model, instruction, tools, sub-agents, hooks, and more.</p>
  </a>
  <a class="card" href="{{ '/configuration/models/' | relative_url }}">
    <div class="card-icon">🧠</div>
    <h3>Model Config</h3>
    <p>Provider setup, parameters, thinking budget, and provider-specific options.</p>
  </a>
  <a class="card" href="{{ '/configuration/tools/' | relative_url }}">
    <div class="card-icon">🔧</div>
    <h3>Tool Config</h3>
    <p>Built-in tools, MCP tools, Docker MCP, LSP, API tools, and tool filtering.</p>
  </a>
</div>

## Advanced Configuration

<div class="cards">
  <a class="card" href="{{ '/configuration/hooks/' | relative_url }}">
    <div class="card-icon">⚡</div>
    <h3>Hooks</h3>
    <p>Run shell commands at lifecycle events like tool calls and session start/end.</p>
  </a>
  <a class="card" href="{{ '/configuration/permissions/' | relative_url }}">
    <div class="card-icon">🔐</div>
    <h3>Permissions</h3>
    <p>Control which tools auto-approve, require confirmation, or are blocked.</p>
  </a>
  <a class="card" href="{{ '/configuration/sandbox/' | relative_url }}">
    <div class="card-icon">📦</div>
    <h3>Sandbox Mode</h3>
    <p>Run agents in an isolated Docker container for security.</p>
  </a>
  <a class="card" href="{{ '/configuration/structured-output/' | relative_url }}">
    <div class="card-icon">📋</div>
    <h3>Structured Output</h3>
    <p>Constrain agent responses to match a specific JSON schema.</p>
  </a>
</div>

## Environment Variables

API keys and secrets are read from environment variables — never stored in config files. See [Managing Secrets]({{ '/guides/secrets/' | relative_url }}) for all the ways to provide credentials (env files, Docker Compose secrets, macOS Keychain, `pass`):

| Variable                   | Provider                                            |
| -------------------------- | --------------------------------------------------- |
| `OPENAI_API_KEY`           | OpenAI                                              |
| `ANTHROPIC_API_KEY`        | Anthropic                                           |
| `GOOGLE_API_KEY` / `GEMINI_API_KEY` | Google Gemini                              |
| `MISTRAL_API_KEY`          | Mistral                                             |
| `XAI_API_KEY`              | xAI                                                 |
| `NEBIUS_API_KEY`           | Nebius                                              |
| `MINIMAX_API_KEY`          | MiniMax                                             |
| `REQUESTY_API_KEY`         | Requesty                                            |
| `GITHUB_TOKEN`             | GitHub Copilot (PAT with `copilot` scope)           |
| `AZURE_API_KEY`            | Azure OpenAI (override with `token_key`)            |
| `AWS_BEARER_TOKEN_BEDROCK` | AWS Bedrock (or the standard AWS credentials chain) |

**Tool Auto-Installation:**

| Variable              | Description                                                     |
| --------------------- | --------------------------------------------------------------- |
| `DOCKER_AGENT_AUTO_INSTALL` | Set to `false` to disable automatic tool installation           |
| `DOCKER_AGENT_TOOLS_DIR`    | Override the base directory for installed tools (default: `~/.cagent/tools/`) |

**Runtime overrides:**

| Variable                            | Description                                                                                          |
| ----------------------------------- | ---------------------------------------------------------------------------------------------------- |
| `DOCKER_AGENT_DEFAULT_MODEL`        | Default model used when none is specified, in `provider/model` form (e.g. `openai/gpt-5`).      |
| `DOCKER_AGENT_MODELS_GATEWAY`       | Route model traffic through a gateway. Equivalent to the `--models-gateway` flag.                    |
| `DOCKER_AGENT_HIDE_TELEMETRY_BANNER`| Set to `1` to suppress the first-run telemetry notice.                                               |
| `DOCKER_AGENT_AUTO_UPDATE`          | Set to a truthy value (`1`, `true`, `yes`, `on`) to let standalone release binaries self-update before running. See [Optional Self-Updates]({{ '/getting-started/installation/#optional-self-updates' | relative_url }}). |

<div class="callout callout-info" markdown="1">
<div class="callout-title">Legacy <code>CAGENT_*</code> aliases
</div>
  <p>The same variables are also accepted with the legacy <code>CAGENT_</code> prefix (e.g. <code>CAGENT_DEFAULT_MODEL</code>, <code>CAGENT_MODELS_GATEWAY</code>, <code>CAGENT_HIDE_TELEMETRY_BANNER</code>) for backward compatibility. Prefer the <code>DOCKER_AGENT_*</code> form in new setups.</p>

</div>

<div class="callout callout-warning" markdown="1">
<div class="callout-title">Important
</div>
  <p>Model references are case-sensitive: <code>openai/gpt-5</code> is not the same as <code>openai/GPT-5</code>.</p>

</div>

## Variable Expansion in Config Fields

docker-agent uses **two different expansion syntaxes** depending on the field. They are not interchangeable: using the wrong syntax in a field is currently a silent no-op, so the literal string is passed through. Tracking issue: [#2615](https://github.com/docker/docker-agent/issues/2615).

### JavaScript template literals — `${env.VAR}`

Used wherever the agent prompt or HTTP traffic is templated. Backed by a JS evaluator, so you also get `||` defaults, ternaries, and tool calls (`${tool({...})}`).

Applies to:

- `agents.<name>.description`
- `agents.<name>.welcome_message`
- `agents.<name>.instruction`
- `agents.<name>.commands.*` (string form and `instruction:` field)
- `toolsets[*].instruction`
- `toolsets[*].headers` and `toolsets[*].remote.headers` (MCP, A2A, OpenAPI, fetch, API)

For `api` toolsets, `api_config.endpoint` and `api_config.headers` are also rendered through the JS expander (the same syntax applies).

```yaml
agents:
  root:
    description: "Assistant for ${env.USER || 'guest'}"
    commands:
      deploy: "Deploy ${env.PROJECT_NAME || 'app'} to ${env.ENV || 'staging'}"
    toolsets:
      - type: openapi
        url: https://api.example.com
        headers:
          Authorization: "Bearer ${env.INTERNAL_TOKEN}"
```

Undefined variables expand to the empty string.

### Shell-style — `$VAR`, `${VAR}`, `~`

Used for filesystem paths. Backed by `os.ExpandEnv` plus tilde expansion against the current user's home directory.

Applies to:

- `agents.<name>.toolsets[*].working_dir` (MCP, LSP)
- `agents.<name>.toolsets[*].path` (memory, tasks)
- `agents.<name>.toolsets[*].env` values (MCP, shell, script, LSP) — these go through `os.Expand`, not the JS evaluator, so `${env.X}` is **not** recognized here.
- The `~` prefix is also accepted in any path-like field documented as such.

```yaml
agents:
  root:
    toolsets:
      - type: memory
        path: "~/notes/${PROJECT}/memory.db"
      - type: mcp
        command: my-server
        working_dir: "$HOME/work"
```

The `working_dir` and `path` fields additionally accept the `${env.VAR}` form as an alias for `${VAR}`, so the JS-style syntax works there too. Richer JS expressions (e.g. `${env.VAR || 'default'}`) are still **not** evaluated in path fields. The `env` values fields remain shell-only.

### Quick reference

| Field                                         | `${env.X}` | `$X` / `${X}` | `~` |
| --------------------------------------------- | :--------: | :-----------: | :-: |
| `description`, `welcome_message`              |     ✓      |       ✗       |  ✗  |
| `instruction` (agent and toolset)             |     ✓      |       ✗       |  ✗  |
| `commands.*`                                  |     ✓      |       ✗       |  ✗  |
| `headers`, `remote.headers`, `api_config.headers` |     ✓      |       ✗       |  ✗  |
| `working_dir`, `path`                         |     ✓      |       ✓       |  ✓  |
| `env` values                                  |     ✗      |       ✓       |  ✗  |

When in doubt, prefer `${env.X}` for prompts and headers, and `${X}` (or `$X`) for paths.

## Validation

docker-agent validates your configuration at startup:

- Local `sub_agents` must reference agents defined in the config (external OCI references like `agentcatalog/pirate` are pulled from registries automatically; pin them to a digest with `@sha256:…` to avoid a per-run registry lookup)
- Named model references must exist in the `models` section
- Provider names must be valid (`openai`, `anthropic`, `google`, `dmr`, etc.)
- Required environment variables (API keys) must be set
- Tool-specific fields are validated (e.g., `path` is only valid for `memory`)

## JSON Schema

For YAML editor autocompletion and validation, use the [Docker Agent JSON Schema](https://github.com/docker/docker-agent/blob/main/agent-schema.json). Add this to the top of your YAML file:

```bash
# yaml-language-server: $schema=https://raw.githubusercontent.com/docker/docker-agent/main/agent-schema.json
```

## Config Versioning

docker-agent configs are versioned. The current version is `10`. Add the version at the top of your config:

```yaml
version: 10

agents:
  root:
    model: openai/gpt-5
    # ...
```

When you load an older config, docker-agent automatically migrates it to the latest schema. It's recommended to include the version to ensure consistent behavior.

## Metadata Section

Optional metadata for agent distribution via OCI registries:

```yaml
metadata:
  author: my-org
  license: Apache-2.0
  description: A helpful coding assistant
  readme: | # Displayed in registries
    This agent helps with coding tasks.
  version: "1.0.0"
```

| Field         | Description                                |
| ------------- | ------------------------------------------ |
| `author`      | Author or organization name                |
| `license`     | License identifier (e.g., Apache-2.0, MIT) |
| `description` | Short description for the agent            |
| `readme`      | Longer markdown description                |
| `version`     | Semantic version string                    |

See [Agent Distribution]({{ '/concepts/distribution/' | relative_url }}) for publishing agents to registries.

## Reusable MCP Servers (`mcps:`)

The top-level `mcps:` section defines named MCP server configurations that agents can reference with `toolsets: [{type: mcp, ref: <name>}]`. This avoids repeating the same command / URL / headers across agents and keeps credentials in one place.

```yaml
mcps:
  github:
    remote:
      url: https://api.githubcopilot.com/mcp
      transport_type: sse
  playwright:
    command: npx
    args: ["-y", "@modelcontextprotocol/server-playwright"]

agents:
  root:
    model: openai/gpt-5
    toolsets:
      - type: mcp
        ref: github        # reuse the definition above
      - type: mcp
        ref: playwright
```

An `mcps` entry accepts every field a regular `type: mcp` toolset accepts (command/args/env, `remote` with `url`/`transport_type`/`headers`/`oauth`, `tools` filter, `instruction`, `defer`, …) — the `type: mcp` is implicit. See the [Tool Config]({{ '/configuration/tools/' | relative_url }}) page for all options and the [Remote MCP Servers]({{ '/features/remote-mcp/' | relative_url }}) guide for remote setups.

## Reusable Commands & Skills (`commands:` / `skills:`)

The top-level `commands:` and `skills:` sections define named, reusable groups that agents pull in by name through `use_commands:` / `use_skills:`. This avoids repeating the same command set or skill configuration across agents. Each group value uses the exact same format as an agent's own `commands` / `skills` field.

Referenced groups are merged into the agent during config loading. An agent's own inline `commands` / `skills` entries take precedence on name conflicts.

```yaml
commands:
  ci:                       # a named command group
    deploy: "Deploy the application"
    test: "Run the test suite"
skills:
  base: [local, git]        # a named skill group

agents:
  root:
    model: openai/gpt-5
    use_commands: [ci]        # reuse the "ci" command group
    use_skills: [base]        # reuse the "base" skill group
    commands:
      lint: "Run the linter"  # inline command, merged in (wins on conflict)
  reviewer:
    model: openai/gpt-5
    use_commands: [ci]        # same group, reused without duplication
```

See [`examples/shared-commands-skills.yaml`](https://github.com/docker/docker-agent/blob/main/examples/shared-commands-skills.yaml) for a complete example.

## Custom Providers Section

Define reusable provider configurations with shared defaults. Providers can wrap any provider type — not just OpenAI-compatible endpoints:

```yaml
providers:
  # OpenAI-compatible custom endpoint
  azure:
    api_type: openai_chatcompletions
    base_url: https://my-resource.openai.azure.com/openai/deployments/gpt-4o
    token_key: AZURE_OPENAI_API_KEY

  # Anthropic with shared model defaults
  team_anthropic:
    provider: anthropic
    token_key: TEAM_ANTHROPIC_KEY
    max_tokens: 32768
    thinking_budget: 16384

models:
  azure_gpt:
    provider: azure
    model: gpt-4o

  claude:
    provider: team_anthropic
    model: claude-sonnet-4-5
    # Inherits max_tokens, thinking_budget from provider

agents:
  root:
    model: claude
```

| Field                 | Description                                                                              |
| --------------------- | ---------------------------------------------------------------------------------------- |
| `provider`            | Underlying provider type: `openai` (default), `anthropic`, `google`, `amazon-bedrock`, etc. |
| `api_type`            | API schema: `openai_chatcompletions` (default) or `openai_responses`. OpenAI-only.        |
| `base_url`            | Base URL for the API endpoint. Required for OpenAI-compatible providers.                  |
| `token_key`           | Environment variable name for the API token.                                              |
| `temperature`         | Default sampling temperature.                                                             |
| `max_tokens`          | Default maximum response tokens.                                                          |
| `thinking_budget`     | Default reasoning effort/budget.                                                          |
| `task_budget`         | Default total token budget for an agentic task (Anthropic; honored by Claude Opus 4.7 today).  |
| `top_p`               | Default top-p sampling parameter.                                                         |
| `frequency_penalty`   | Default frequency penalty.                                                                |
| `presence_penalty`    | Default presence penalty.                                                                 |
| `parallel_tool_calls` | Enable parallel tool calls by default.                                                    |
| `track_usage`         | Track token usage by default.                                                             |
| `provider_opts`       | Provider-specific options.                                                                |

See [Provider Definitions]({{ '/providers/custom/' | relative_url }}) for more details.

## Reusable YAML (anchors & aliases)

YAML anchors (`&name`), aliases (`*name`) and merge keys (`<<`) are part of the YAML spec, and Docker Agent's config parser supports them. Use them to declare a value once and reuse it elsewhere in the same file, instead of copy-pasting the same block across agents.

This complements the named-reuse sections above (`mcps:`, `commands:` / `skills:`, `providers:`). Reach for anchors when you want to share something those sections don't cover, such as an instruction string or a block of agent settings.

Reuse a value verbatim with an anchor and an alias:

```yaml
agents:
  root:
    model: anthropic/claude-sonnet-4-5
    description: Coordinator.
    instruction: &house_rules |
      You are part of the Acme engineering team.
      Cite the files you looked at and keep changes minimal.
  reviewer:
    model: anthropic/claude-sonnet-4-5
    description: Reviews code changes.
    instruction: *house_rules        # the same instruction, declared once
```

Compose a block with a merge key (`<<`), then override individual fields:

```yaml
agents:
  reviewer: &specialist
    model: anthropic/claude-sonnet-4-5
    description: Reviews code changes.
    instruction: |
      You are a meticulous software professional.
    toolsets:
      - type: filesystem
  documenter:
    <<: *specialist                   # inherit model, instruction, toolsets
    description: Writes documentation. # then override one field
```

<div class="callout callout-warning" markdown="1">
<div class="callout-title">Where anchors can live</div>
  <p>An anchor has to sit on a real value inside a known section (for example a real agent, model, or MCP entry, as above). Parking anchors in a separate top-level block such as <code>defaults:</code> or <code>prompts:</code> fails, because the parser rejects unknown top-level keys.</p>
</div>

<div class="callout callout-warning" markdown="1">
<div class="callout-title">Overriding merged keys</div>
  <p>Overriding a key that a <code>&lt;&lt;</code> merge already set works only in the <code>agents:</code> section, as shown above. Every other section (<code>models:</code>, <code>mcps:</code>, <code>providers:</code>, <code>rag:</code>) is parsed strictly and reports the override as a duplicate key. There, use <code>&lt;&lt;</code> only to add new fields, or use the named-reuse sections above when you need per-entry overrides.</p>
</div>

Anchors are for static reuse within a single file, not dynamic values or cross-file composition. For environment-specific settings, see [Variable Expansion in Config Fields](#variable-expansion-in-config-fields), which substitutes `${env.VAR}` at load time. Templating tags such as `!include` are not acted on: the tag is ignored and its argument is kept as a plain string, so no other file is loaded. Circular aliases are not detected, so keep references acyclic.

See [`examples/yaml-anchors.yaml`](https://github.com/docker/docker-agent/blob/main/examples/yaml-anchors.yaml) for a complete example.
