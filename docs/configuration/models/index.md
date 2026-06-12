---
title: "Model Configuration"
description: "Complete reference for defining models with providers, parameters, and reasoning settings."
permalink: /configuration/models/
---

# Model Configuration

_Complete reference for defining models with providers, parameters, and reasoning settings._

## Full Schema

<!-- yaml-lint:skip -->
```yaml
models:
  model_name:
    first_available: [list] # Optional: candidate model refs, tried in order by available credentials.
                            # Mutually exclusive with other model settings.
    provider: string # Required unless using first_available. One of: openai, anthropic, google, amazon-bedrock,
                     # dmr, mistral, xai, nebius, minimax, requesty, azure, ollama,
                     # github-copilot, or a named provider defined under the top-level
                     # `providers:` section.
    model: string # Required: model identifier
    temperature: float # Optional: 0.0–2.0 (provider-dependent; e.g. Anthropic caps at 1.0)
    max_tokens: integer # Optional: response length limit
    top_p: float # Optional: 0.0–1.0
    frequency_penalty: float # Optional: -2.0–2.0
    presence_penalty: float # Optional: -2.0–2.0
    base_url: string # Optional: custom API endpoint
    token_key: string # Optional: env var for API token
    thinking_budget: string|int # Optional: reasoning effort
    task_budget: int|object # Optional: total task token budget (Anthropic)
    parallel_tool_calls: boolean # Optional: allow parallel tool calls
    track_usage: boolean # Optional: track token usage
    routing: [list] # Optional: rule-based model routing
    provider_opts: # Optional: provider-specific options
      key: value
```

## Properties Reference

| Property              | Type       | Required | Description                                                                           |
| --------------------- | ---------- | -------- | ------------------------------------------------------------------------------------- |
| `first_available`     | array      | ✗        | Candidate model references tried in order; selects the first whose credentials are configured. Mutually exclusive with other model settings. |
| `provider`            | string     | ✓/✗      | Required for regular model definitions; omitted for `first_available` selectors. Provider: `openai`, `anthropic`, `google`, `amazon-bedrock`, `dmr`, `mistral`, `xai`, `nebius`, `minimax`, `requesty`, `azure`, `ollama`, `github-copilot`, or any [named provider]({{ '/providers/custom/' | relative_url }}). |
| `model`               | string     | ✓/✗      | Required for regular model definitions; omitted for `first_available` selectors. Model name (e.g., `gpt-4o`, `claude-sonnet-4-5`, `gemini-3.5-flash`) |
| `temperature`         | float      | ✗        | Sampling randomness. Range is provider-dependent — typically `0.0–2.0` (Anthropic caps at `1.0`). `0.0` is deterministic. |
| `max_tokens`          | int        | ✗        | Maximum response length in tokens                                                     |
| `top_p`               | float      | ✗        | Nucleus sampling threshold (`0.0–1.0`)                                                |
| `frequency_penalty`   | float      | ✗        | Penalize repeated tokens (`-2.0–2.0`)                                                 |
| `presence_penalty`    | float      | ✗        | Encourage topic diversity (`-2.0–2.0`)                                                |
| `base_url`            | string     | ✗        | Custom API endpoint URL (for self-hosted or proxied endpoints)                        |
| `token_key`           | string     | ✗        | Environment variable name containing the API token (overrides provider default)       |
| `thinking_budget`     | string/int | ✗        | Reasoning effort control                                                              |
| `task_budget`         | int/object | ✗        | Total token budget for an agentic task (forwarded to Anthropic; see [Task Budget](#task-budget)). |
| `parallel_tool_calls` | boolean    | ✗        | Allow model to call multiple tools at once                                            |
| `track_usage`         | boolean    | ✗        | Track and report token usage for this model                                           |
| `routing`             | array      | ✗        | Rule-based routing to different models. See [Model Routing]({{ '/configuration/routing/' | relative_url }}). |
| `provider_opts`       | object     | ✗        | Provider-specific options (see provider pages)                                        |
| `title_model`         | string     | ✗        | Model used for session-title generation. Can be a named model from the `models:` section or an inline `provider/model` string. When omitted, the agent's primary model generates titles. Cannot be combined with `first_available`. |

## Delegating Session-Title Generation

The `title_model` field lets a heavyweight primary model hand off the cheap
title-generation call to a smaller, faster model:

```yaml
model: anthropic/claude-opus-4-7
title_model: anthropic/claude-haiku-4-5
```

The value can be a named entry from the `models` stanza or an inline
`provider/model` string. When omitted, the agent's primary model generates
titles.

<div class="callout callout-warning" markdown="1">
<div class="callout-title">Constraint
</div>
  <p><code>title_model</code> cannot be combined with <code>first_available</code> model selection — the combination is rejected at validation time.</p>
</div>

## First Available Models

Use `first_available` when the same agent should work with whichever provider credentials are available in the current environment. docker-agent checks the candidates in order at load time and replaces the selector with the first candidate whose required environment variables are configured.

```yaml
models:
  smart:
    first_available:
      - anthropic/claude-sonnet-4-6
      - openai/gpt-5
      - google/gemini-3.5-flash
      - dmr/ai/qwen3 # local fallback; no API key required

agents:
  root:
    model: smart
    instruction: You are a helpful assistant.
```

Candidates can be inline `provider/model` references or names from the same `models:` section. Local providers such as `dmr` and `ollama` do not require credentials, so they are useful as final fallbacks.

If none of the candidates has credentials configured, docker-agent reports the missing environment variables grouped by candidate. You only need to configure one group of credentials, not every provider in the list.

A `first_available` model is only a selector. It cannot be combined with `provider`, `model`, `routing`, `token_key`, budgets, sampling options, or other model settings. Put those settings on named candidate models instead:

```yaml
models:
  claude:
    provider: anthropic
    model: claude-sonnet-4-6
    max_tokens: 64000

  gpt:
    provider: openai
    model: gpt-5
    thinking_budget: low

  smart:
    first_available:
      - claude
      - gpt
      - dmr/ai/qwen3
```

See [`examples/first_available.yaml`](https://github.com/docker/docker-agent/blob/main/examples/first_available.yaml) for a complete example.

## Thinking Budget

Control how much reasoning the model does before responding. This varies by provider:

### OpenAI

Uses effort levels as strings:

```yaml
models:
  gpt:
    provider: openai
    model: gpt-5
    thinking_budget: low # minimal | low | medium | high | xhigh | max | adaptive/<level>
```

### Anthropic

Uses token budgets on Claude Sonnet / older Opus models, and adaptive thinking on Claude Opus 4.7+:

```yaml
models:
  claude:
    provider: anthropic
    model: claude-sonnet-4-5
    thinking_budget: 16384 # token budget; must be < max_tokens

  opus:
    provider: anthropic
    model: claude-opus-4-8
    thinking_budget: adaptive/high # adaptive | adaptive/low | adaptive/medium | adaptive/high | adaptive/xhigh | adaptive/max
```

Claude Opus 4.7+ rejects token-based thinking requests; prefer `adaptive` or `adaptive/<level>` for those models.

### Google Gemini 2.5

Uses an integer token budget. `0` disables, `-1` lets the model decide:

```yaml
models:
  gemini:
    provider: google
    model: gemini-3.5-flash
    thinking_budget: -1 # dynamic (default)
```

### Google Gemini 3

Uses effort levels like OpenAI:

```yaml
models:
  gemini3:
    provider: google
    model: gemini-3-flash
    thinking_budget: medium # minimal | low | medium | high | xhigh | max | adaptive/<level>
```

### Disabling Thinking

Works for all providers:

```yaml
thinking_budget: none # or 0
```

## Task Budget

**Anthropic-only.**

`task_budget` caps the **total** number of tokens the model may spend across a
multi-step agentic task — combining thinking, tool calls, and final output
tokens. It lets long-running agents self-regulate effort without having to
choose a tight per-call `max_tokens`.

It is forwarded to Anthropic's
[`output_config.task_budget`](https://platform.claude.com/docs/en/about-claude/models/whats-new-claude-4-7)
request field. docker-agent automatically attaches the required
`task-budgets-2026-03-13` beta header whenever this field is set.

You can configure `task_budget` on **any** Claude model — docker-agent never
gates it by model name. At the time of writing only **Claude Opus 4.7**
actually honors the field; other Claude models will reject requests that
include it. Check the Anthropic release notes linked above for the current
list of supported models.

### Integer shorthand

```yaml
models:
  opus:
    provider: anthropic
    model: claude-opus-4-7
    task_budget: 128000 # total tokens for the whole task
    thinking_budget: adaptive # works nicely together
```

### Object form

Equivalent, and forward-compatible with future budget types:

```yaml
models:
  opus:
    provider: anthropic
    model: claude-opus-4-7
    task_budget:
      type: tokens # only "tokens" is supported today
      total: 128000
```

Setting `task_budget: 0` (or omitting the field) disables the feature — the
model falls back to the provider's default behavior.

Like other inheritable model settings, `task_budget` can also be declared on a
[provider definition]({{ '/providers/custom/' | relative_url }}) and is
inherited by every model that references that provider.

See [`examples/task_budget.yaml`](https://github.com/docker/docker-agent/blob/main/examples/task_budget.yaml) for a complete example.

## Interleaved Thinking

For Anthropic and Bedrock Claude models, interleaved thinking allows tool calls during model reasoning. It is auto-enabled whenever a thinking budget is configured:

```yaml
models:
  claude:
    provider: anthropic
    model: claude-sonnet-4-5
    thinking_budget: 8192
    # interleaved_thinking is auto-enabled when thinking_budget is set
    provider_opts:
      interleaved_thinking: false # disable if needed
```

## Thinking Display (Anthropic)

For Anthropic Claude models, `thinking_display` controls whether thinking blocks are returned in responses when thinking is enabled. Claude Opus 4.7+ hides thinking content by default (`omitted`); set this provider option to receive summarized thinking:

```yaml
models:
  opus-4-8:
    provider: anthropic
    model: claude-opus-4-8
    thinking_budget: adaptive
    provider_opts:
      thinking_display: summarized # "summarized", "display", or "omitted"
```

See the [Anthropic provider page]({{ '/providers/anthropic/#thinking-display' | relative_url }}) for details.

## Custom HTTP Headers

For OpenAI-compatible providers (`openai`, `github-copilot`, `mistral`, `xai`,
`nebius`, `minimax`, `ollama`, and any custom provider using the OpenAI API),
`provider_opts.http_headers` adds arbitrary HTTP headers to every outgoing
request:

```yaml
models:
  my_model:
    provider: openai
    model: gpt-4o
    provider_opts:
      http_headers:
        X-Request-Source: docker-agent
        X-Tenant-Id: my-team
```

Header names are matched case-insensitively. The `github-copilot` provider
automatically sets `Copilot-Integration-Id: copilot-developer-cli` — see the
[GitHub Copilot provider page]({{ '/providers/github-copilot/' | relative_url }})
for details.

## Examples by Provider

```yaml
models:
  # OpenAI
  gpt:
    provider: openai
    model: gpt-5

  # Anthropic
  claude:
    provider: anthropic
    model: claude-sonnet-4-5
    max_tokens: 64000

  # Google Gemini
  gemini:
    provider: google
    model: gemini-3.5-flash
    temperature: 0.5

  # AWS Bedrock
  bedrock:
    provider: amazon-bedrock
    model: global.anthropic.claude-sonnet-4-5-20250929-v1:0
    provider_opts:
      region: us-east-1

  # Docker Model Runner (local)
  local:
    provider: dmr
    model: ai/qwen3
    max_tokens: 8192
```

For detailed provider setup, see the [Model Providers]({{ '/providers/overview/' | relative_url }}) section.

## Custom Endpoints

Use `base_url` to point to custom or self-hosted endpoints:

```yaml
models:
  # Azure OpenAI
  azure_gpt:
    provider: openai
    model: gpt-4o
    base_url: https://my-resource.openai.azure.com/openai/deployments/gpt-4o
    token_key: AZURE_OPENAI_API_KEY

  # Self-hosted vLLM
  local_llama:
    provider: openai # vLLM is OpenAI-compatible
    model: meta-llama/Llama-3.2-3B-Instruct
    base_url: http://localhost:8000/v1

  # Proxy or gateway
  proxied:
    provider: openai
    model: gpt-4o
    base_url: https://proxy.internal.company.com/openai/v1
    token_key: INTERNAL_API_KEY
```

See [Local Models]({{ '/providers/local/' | relative_url }}) for more examples of custom endpoints.

## Inheriting from Provider Definitions

Models can reference a named provider to inherit shared defaults. Model-level settings always take precedence:

```yaml
providers:
  my_anthropic:
    provider: anthropic
    token_key: MY_ANTHROPIC_KEY
    max_tokens: 16384
    thinking_budget: high
    temperature: 0.5

models:
  claude:
    provider: my_anthropic
    model: claude-sonnet-4-5
    # Inherits max_tokens, thinking_budget, temperature from provider

  claude_fast:
    provider: my_anthropic
    model: claude-haiku-4-5
    thinking_budget: low  # Overrides provider default
```

See [Provider Definitions]({{ '/providers/custom/' | relative_url }}) for the full list of inheritable properties.
