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
version: 8

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
```

## Minimal Config

The simplest possible configuration — a single agent with an inline model:

```yaml
agents:
  root:
    model: openai/gpt-5-mini
    description: A helpful assistant
    instruction: You are a helpful assistant.
```

The same config in HCL:

```hcl
agent "root" {
  model       = "openai/gpt-5-mini"
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
    <pre style="margin-top:12px"><code class="language-yaml">model: openai/gpt-5-mini</code></pre>
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
| `DOCKER_AGENT_DEFAULT_MODEL`        | Default model used when none is specified, in `provider/model` form (e.g. `openai/gpt-5-mini`).      |
| `DOCKER_AGENT_MODELS_GATEWAY`       | Route model traffic through a gateway. Equivalent to the `--models-gateway` flag.                    |
| `DOCKER_AGENT_HIDE_TELEMETRY_BANNER`| Set to `1` to suppress the first-run telemetry notice.                                               |

<div class="callout callout-info" markdown="1">
<div class="callout-title">Legacy <code>CAGENT_*</code> aliases
</div>
  <p>The same variables are also accepted with the legacy <code>CAGENT_</code> prefix (e.g. <code>CAGENT_DEFAULT_MODEL</code>, <code>CAGENT_MODELS_GATEWAY</code>, <code>CAGENT_HIDE_TELEMETRY_BANNER</code>) for backward compatibility. Prefer the <code>DOCKER_AGENT_*</code> form in new setups.</p>

</div>

<div class="callout callout-warning" markdown="1">
<div class="callout-title">Important
</div>
  <p>Model references are case-sensitive: <code>openai/gpt-5-mini</code> is not the same as <code>openai/GPT-5-mini</code>.</p>

</div>

## Validation

docker-agent validates your configuration at startup:

- Local `sub_agents` must reference agents defined in the config (external OCI references like `agentcatalog/pirate` are pulled from registries automatically)
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

docker-agent configs are versioned. The current version is `8`. Add the version at the top of your config:

```yaml
version: 8

agents:
  root:
    model: openai/gpt-5-mini
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
    model: openai/gpt-5-mini
    toolsets:
      - type: mcp
        ref: github        # reuse the definition above
      - type: mcp
        ref: playwright
```

An `mcps` entry accepts every field a regular `type: mcp` toolset accepts (command/args/env, `remote` with `url`/`transport_type`/`headers`/`oauth`, `tools` filter, `instruction`, `defer`, …) — the `type: mcp` is implicit. See the [Tool Config]({{ '/configuration/tools/' | relative_url }}) page for all options and the [Remote MCP Servers]({{ '/features/remote-mcp/' | relative_url }}) guide for remote setups.

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
    thinking_budget: high

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
